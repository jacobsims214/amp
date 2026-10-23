//! The AMP metadata poller (D10, #2104): what the chat TUI shows about AMP, fetched by the harness
//! itself on its own MCP client, never through the model, so it costs no tokens and never enters
//! the model's context.
//!
//! [`MetaPoller::spawn`] starts one background task and returns a [`MetaHandle`]. The task keeps
//! an [`AmpMeta`] snapshot up to date in a `tokio::sync::watch` channel; the TUI reads it with
//! [`MetaHandle::snapshot`] or waits on [`MetaHandle::subscribe`]. The task stops when the last
//! handle is dropped.
//!
//! What it fetches, and when (`research/harness-tui-theme-and-metadata.md` §2):
//!
//! | Item ([`Topics`]) | MCP call | Polled | Also refreshed |
//! |---|---|---|---|
//! | user (`WHOAMI`) | `amp_whoami` | on connect/reconnect; doubles as the idle health probe | after the model calls `amp_whoami` |
//! | project (`PROJECT`) | `amp_get_project` | on start and on [`MetaHandle::set_project`] | after `amp_update_project_settings` / `amp_approve_plan` |
//! | board counts + questions + in-progress tickets (`BOARD`) | `amp_list_tasks(owner=any, compact)` | every 30 s with a project | after any ticket-changing `amp_*` call |
//! | current ticket (`TICKET`) + its epic/story names (`TREE`, `amp_get_plan`, cached) | `amp_get_task` | every 30 s while the ticket is not done | after an `amp_*` call naming it |
//! | plan approval (`PLAN`) | `amp_check_plan` | every 60 s, only when the project requires approval | after `amp_create_tasks` / `amp_approve_plan` / `amp_set_task_dependencies` / … |
//! | the ticket's artifacts (`ARTIFACTS`) | `amp_list_artifacts(task_id)` | every 60 s while one is in review | after `amp_publish_artifact` / `amp_review_artifact` |
//! | MCP health | every call above | `amp_whoami` after 60 s without any call | — |
//!
//! Immediate refresh: feed the session's agent events to [`MetaHandle::observe`] (the chat shell
//! does it in `Shell::handle_event`). When an `amp_*` tool call ends without error, the items it
//! touched ([`topics_for_tool`]) are refetched right away. A successful `amp_claim_task` makes that
//! ticket the current one.
//!
//! The current ticket is whatever [`MetaHandle::set_ticket`] or the last `amp_claim_task` names.
//! Dispatching or running a ticket for a worker (`amp_dispatch_task`, `amp_run_ticket`) does not
//! change it: those tickets are workers' (the supervisor's `/workers` view), not this session's.
//!
//! **Health vs. item errors (#2108).** Only transport, timeout and auth failures change
//! [`MetaHealth::connection`]. When AMP answers a call with an error (a tool `isError` result, or
//! a JSON-RPC error such as "task 99999 not found"), the connection stays healthy and only that
//! item records the error ([`Fetched::error`]: it keeps its last value, shown stale, or shows as
//! unavailable).
//!
//! The slash commands' own calls (`/plan`'s `amp_get_plan`, `/questions`' `amp_answer_question`)
//! go through [`MetaHandle::call`], on the poller's connection; a successful write refreshes what
//! it touched like a model's call does. [`MetaHandle::stub`] is a handle without AMP, for tests.

use std::{
    collections::HashMap,
    fmt,
    future::Future,
    pin::Pin,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use amp_mcp::{AmpMcpClient, Health, McpError};
use rpi_agent::AgentEvent;
use serde::Deserialize;
use serde_json::{Value, json};
use tokio::sync::{
    mpsc::{UnboundedReceiver, UnboundedSender, unbounded_channel},
    oneshot, watch,
};

use crate::tickets::Status;
use crate::tools::{result_json, result_text};

// ---------------------------------------------------------------------------------------------
// Topics and the tool → topics map
// ---------------------------------------------------------------------------------------------

/// A set of metadata items to (re)fetch.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
pub struct Topics(u16);

impl Topics {
    pub const NONE: Self = Self(0);
    /// `amp_whoami`: the user and their orgs.
    pub const WHOAMI: Self = Self(1);
    /// `amp_get_project`.
    pub const PROJECT: Self = Self(1 << 1);
    /// `amp_list_tasks(owner=any)`: counts, questions, in-progress tickets.
    pub const BOARD: Self = Self(1 << 2);
    /// `amp_get_task` on the current ticket.
    pub const TICKET: Self = Self(1 << 3);
    /// `amp_check_plan`: plan approval.
    pub const PLAN: Self = Self(1 << 4);
    /// `amp_list_artifacts` for the current ticket.
    pub const ARTIFACTS: Self = Self(1 << 5);
    /// A health probe (`amp_whoami`), only when nothing else ran.
    pub const PING: Self = Self(1 << 6);
    /// `amp_get_plan`: epic and story names (and epic order, for the cat colours).
    pub const TREE: Self = Self(1 << 7);
    /// Everything fetched on start and after a reconnect.
    pub const ALL: Self = Self(
        Self::WHOAMI.0
            | Self::PROJECT.0
            | Self::BOARD.0
            | Self::TICKET.0
            | Self::PLAN.0
            | Self::ARTIFACTS.0
            | Self::TREE.0,
    );

    pub fn contains(self, other: Self) -> bool {
        other.0 != 0 && self.0 & other.0 == other.0
    }

    pub fn is_empty(self) -> bool {
        self.0 == 0
    }

    pub fn names(self) -> Vec<&'static str> {
        [
            (Self::WHOAMI, "whoami"),
            (Self::PROJECT, "project"),
            (Self::TREE, "tree"),
            (Self::BOARD, "board"),
            (Self::TICKET, "ticket"),
            (Self::PLAN, "plan"),
            (Self::ARTIFACTS, "artifacts"),
            (Self::PING, "ping"),
        ]
        .into_iter()
        .filter(|(t, _)| self.contains(*t))
        .map(|(_, n)| n)
        .collect()
    }
}

impl std::ops::BitOr for Topics {
    type Output = Self;
    fn bitor(self, rhs: Self) -> Self {
        Self(self.0 | rhs.0)
    }
}

impl std::ops::BitOrAssign for Topics {
    fn bitor_assign(&mut self, rhs: Self) {
        self.0 |= rhs.0;
    }
}

impl std::ops::Sub for Topics {
    type Output = Self;
    fn sub(self, rhs: Self) -> Self {
        Self(self.0 & !rhs.0)
    }
}

impl fmt::Display for Topics {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.names().join("|"))
    }
}

/// A ticket id from tool arguments: a number, or a string like `"2104"` / `"#2104"`.
fn id_of(v: &Value) -> Option<i64> {
    match v {
        Value::Number(n) => n.as_i64(),
        Value::String(s) => s.trim().trim_start_matches('#').parse().ok(),
        _ => None,
    }
}

/// Whether a call's arguments name ticket `ticket` (`task_id`, or one of `task_ids`).
fn names_ticket(args: &Value, ticket: Option<i64>) -> bool {
    let Some(ticket) = ticket else { return false };
    if id_of(&args["task_id"]) == Some(ticket) {
        return true;
    }
    args["task_ids"]
        .as_array()
        .is_some_and(|ids| ids.iter().any(|v| id_of(v) == Some(ticket)))
}

/// Tools that only read: they change nothing the TUI shows.
fn is_read_only(name: &str) -> bool {
    const PREFIXES: &[&str] = &["amp_get_", "amp_list_", "amp_kb_", "amp_find_"];
    const NAMES: &[&str] = &[
        "amp_guide",
        "amp_trace",
        "amp_velocity",
        "amp_audit_log",
        "amp_human_impact",
        "amp_design_context",
        "amp_session_usage",
        "amp_day_report",
        "amp_project_day_reports",
    ];
    PREFIXES.iter().any(|p| name.starts_with(p)) || NAMES.contains(&name)
}

/// The items an `amp_*` tool call may have changed, so they are refetched as soon as it ends.
/// `ticket` is the current ticket. Non-AMP tools and read-only AMP tools map to nothing.
pub fn topics_for_tool(name: &str, args: &Value, ticket: Option<i64>) -> Topics {
    if !name.starts_with("amp_") || is_read_only(name) {
        return Topics::NONE;
    }
    let names_current = names_ticket(args, ticket);
    let this_ticket = if names_current {
        Topics::TICKET
    } else {
        Topics::NONE
    };
    match name {
        "amp_whoami" => Topics::WHOAMI,
        "amp_update_project_settings" => Topics::PROJECT | Topics::PLAN,
        "amp_approve_plan" => Topics::PROJECT | Topics::PLAN | Topics::BOARD,
        "amp_check_plan" => Topics::PLAN | Topics::BOARD,
        "amp_create_tasks"
        | "amp_set_task_dependencies"
        | "amp_delete_task"
        | "amp_restore_tasks"
        | "amp_move_task"
        | "amp_update_task" => Topics::BOARD | Topics::PLAN | this_ticket,
        "amp_create_epic" | "amp_update_epic" | "amp_delete_epic" | "amp_create_story"
        | "amp_update_story" | "amp_delete_story" | "amp_move_story" => {
            // Names may have changed; the ticket line re-reads them from the new tree.
            let ticket = if ticket.is_some() {
                Topics::TICKET
            } else {
                Topics::NONE
            };
            Topics::TREE | Topics::BOARD | Topics::PLAN | ticket
        }
        "amp_publish_artifact" | "amp_review_artifact" | "amp_keep_artifact" => Topics::ARTIFACTS,
        "amp_add_task_comment" => this_ticket,
        // Ticket changes: dispatch, complete, block, claim, answer, state, report, run_ticket, …
        // Unknown future tools land here too, which errs on the side of a fresh board.
        _ => {
            let without_target = args.get("task_id").is_none() && args.get("task_ids").is_none();
            let ticket = if names_current || (without_target && ticket.is_some()) {
                Topics::TICKET
            } else {
                Topics::NONE
            };
            Topics::BOARD | ticket
        }
    }
}

// ---------------------------------------------------------------------------------------------
// The snapshot
// ---------------------------------------------------------------------------------------------

/// One fetched item: the last good value (kept when a later fetch fails), when it was fetched,
/// and the last fetch's error.
#[derive(Debug, Clone)]
pub struct Fetched<T> {
    pub value: Option<T>,
    pub updated: Option<Instant>,
    pub error: Option<String>,
}

impl<T> Default for Fetched<T> {
    fn default() -> Self {
        Self {
            value: None,
            updated: None,
            error: None,
        }
    }
}

impl<T> Fetched<T> {
    pub fn get(&self) -> Option<&T> {
        self.value.as_ref()
    }

    fn set(&mut self, result: Result<T, String>) {
        match result {
            Ok(v) => {
                self.value = Some(v);
                self.updated = Some(Instant::now());
                self.error = None;
            }
            Err(e) => self.error = Some(e),
        }
    }
}

/// The AMP server this harness talks to, and links into its web UI.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Instance {
    /// The MCP endpoint (`https://amp.simstech.cloud/mcp`).
    pub mcp_url: String,
    /// The web UI origin (`https://amp.simstech.cloud`): the configured web URL's origin, else the
    /// MCP URL without its path.
    pub web_base: String,
    /// The negotiated MCP protocol version (`2026-07-28`).
    pub protocol: String,
    pub server_name: Option<String>,
    pub server_version: Option<String>,
    /// How the harness authenticated (never a secret).
    pub auth: String,
}

impl Instance {
    /// `web_url` overrides the web UI origin (compose serves the UI on another port than MCP).
    fn of(client: &AmpMcpClient, web_url: Option<&str>) -> Self {
        let s = client.session();
        Self {
            mcp_url: s.url.clone(),
            web_base: web_base(web_url.unwrap_or(&s.url)),
            protocol: s.protocol_version.to_string(),
            server_name: s.server_name.clone(),
            server_version: s.server_version.clone(),
            auth: s.auth.clone(),
        }
    }

    /// Host (and port) for the header: `amp.simstech.cloud`.
    pub fn host(&self) -> &str {
        self.web_base
            .split_once("://")
            .map_or(self.web_base.as_str(), |(_, h)| h)
    }

    /// The board with the ticket's drawer open (`Board.tsx` reads `?task=`).
    pub fn ticket_url(&self, project_id: i64, ticket_id: i64) -> String {
        format!(
            "{}/project/{project_id}/board?task={ticket_id}",
            self.web_base
        )
    }

    /// The project's Mission page.
    pub fn project_url(&self, project_id: i64) -> String {
        format!("{}/project/{project_id}", self.web_base)
    }

    pub fn artifact_url(&self, artifact_id: i64) -> String {
        format!("{}/artifacts/{artifact_id}", self.web_base)
    }

    /// The Flight plan page, where a person approves the plan (`approve_url` in AMP).
    pub fn plan_url(&self, project_id: i64) -> String {
        format!("{}/project/{project_id}/plan", self.web_base)
    }

    pub fn kb_url(&self, project_id: i64) -> String {
        format!("{}/project/{project_id}/kb", self.web_base)
    }
}

/// `scheme://host[:port]` of a URL (everything before the path).
pub fn web_base(url: &str) -> String {
    amp_mcp::url_origin(url)
}

/// The MCP connection as the header shows it (§2 item 2).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Connection {
    /// No client yet (the poller is connecting).
    Connecting,
    /// The last call succeeded quickly.
    Ok,
    /// The last call succeeded but took longer than [`Intervals::slow_after`].
    Slow { latency: Duration },
    /// The last call failed or timed out; the connection is still open.
    Degraded { reason: String },
    /// The server rejected our credentials: re-login needed.
    AuthRequired,
    /// The connection is closed (reconnecting if the poller has a connector).
    Down { reason: String },
}

/// The three header colours: success, warning, error.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HealthLevel {
    Ok,
    Degraded,
    Down,
}

impl Connection {
    pub fn level(&self) -> HealthLevel {
        match self {
            Self::Ok => HealthLevel::Ok,
            Self::Connecting | Self::Slow { .. } | Self::Degraded { .. } => HealthLevel::Degraded,
            Self::AuthRequired | Self::Down { .. } => HealthLevel::Down,
        }
    }

    fn after_call(health: Health, latency: Duration, slow_after: Duration) -> Self {
        match health {
            Health::Connected if latency > slow_after => Self::Slow { latency },
            Health::Connected => Self::Ok,
            Health::Degraded { reason } => Self::Degraded { reason },
            Health::AuthRequired => Self::AuthRequired,
            Health::Disconnected { reason } => Self::Down { reason },
        }
    }
}

impl fmt::Display for Connection {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Connecting => f.write_str("connecting"),
            Self::Ok => f.write_str("connected"),
            Self::Slow { latency } => write!(f, "slow ({} ms)", latency.as_millis()),
            Self::Degraded { reason } => write!(f, "degraded: {reason}"),
            Self::AuthRequired => f.write_str("re-login needed"),
            Self::Down { reason } => write!(f, "disconnected: {reason}"),
        }
    }
}

/// MCP health from the poller's own calls.
#[derive(Debug, Clone)]
pub struct MetaHealth {
    pub connection: Connection,
    /// The last call's round trip.
    pub latency: Option<Duration>,
    /// When a call last succeeded (the server answered, even with a tool error).
    pub last_ok: Option<Instant>,
    /// When the poller last called AMP.
    pub last_call: Option<Instant>,
    /// Calls made so far (all of them, failed ones too).
    pub calls: u64,
}

impl Default for MetaHealth {
    fn default() -> Self {
        Self {
            connection: Connection::Connecting,
            latency: None,
            last_ok: None,
            last_call: None,
            calls: 0,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Org {
    pub id: i64,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub slug: String,
}

/// `amp_whoami`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct User {
    #[serde(default)]
    pub user_id: i64,
    #[serde(default)]
    pub email: String,
    /// `human` or `agent`.
    #[serde(default)]
    pub account: String,
    #[serde(default)]
    pub via_api_token: bool,
    #[serde(default)]
    pub global_admin: bool,
    #[serde(default)]
    pub orgs: Vec<Org>,
}

/// `amp_get_project`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Project {
    pub id: i64,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub code: String,
    #[serde(default)]
    pub state: String,
    #[serde(default)]
    pub org_id: Option<i64>,
    #[serde(default)]
    pub require_plan_approval: bool,
}

/// An epic or story name, for `Epic › Story`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Named {
    pub id: i64,
    pub name: String,
}

/// The current ticket (§2 item 4).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Ticket {
    pub id: i64,
    pub project_id: i64,
    pub name: String,
    /// AMP's stored state (`backlog`, `in_progress`, `blocked`, `completed`).
    pub state: String,
    pub block_reason: Option<String>,
    /// The web UI's display state.
    pub status: Status,
    pub assigned_to: Option<String>,
    pub dispatched_at: Option<String>,
    pub epic: Option<Named>,
    pub story: Option<Named>,
    /// The epic's position in `amp_get_plan` (0-based): the cat-N colour is `epic_index % 6`.
    pub epic_index: Option<usize>,
}

/// A ticket waiting on a person's answer.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Question {
    pub id: i64,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub question: String,
}

/// An in-progress ticket on the board (who runs it).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BoardTicket {
    pub id: i64,
    pub name: String,
    /// The agent profile running it (`amp:amp-worker-backend`).
    pub assigned_to: Option<String>,
    pub epic: String,
}

/// Board counts in the web UI's words (§2 items 5, 8, 10), from `amp_list_tasks(owner=any)`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Board {
    pub in_progress: u32,
    pub ready: u32,
    pub question: u32,
    pub scheduled: u32,
    pub waiting: u32,
    pub done: u32,
    pub questions: Vec<Question>,
    pub in_progress_tickets: Vec<BoardTicket>,
    /// In progress but silent for a while (usually a dead worker).
    pub quiet: Vec<i64>,
    /// Open tickets a person does (never dispatched).
    pub waiting_on_people: u32,
}

impl Board {
    pub fn count(&self, status: Status) -> u32 {
        match status {
            Status::InProgress => self.in_progress,
            Status::Ready => self.ready,
            Status::Question => self.question,
            Status::Scheduled => self.scheduled,
            Status::Waiting => self.waiting,
            Status::Done => self.done,
        }
    }

    /// The status line's counts, in its order, without the zero ones.
    pub fn nonzero(&self) -> Vec<(Status, u32)> {
        [
            Status::InProgress,
            Status::Ready,
            Status::Question,
            Status::Scheduled,
        ]
        .into_iter()
        .map(|s| (s, self.count(s)))
        .filter(|(_, n)| *n > 0)
        .collect()
    }

    /// Parses AMP's compact `amp_list_tasks` result.
    pub fn from_compact(v: &Value) -> Result<Self, String> {
        if !v.is_object() || v.get("ready_to_dispatch").is_none() {
            return Err("not a compact amp_list_tasks result".to_owned());
        }
        let len = |k: &str| v[k].as_array().map_or(0, |a| a.len() as u32);
        let num = |k: &str| v[k].as_u64().unwrap_or(0) as u32;
        let questions: Vec<Question> =
            serde_json::from_value(v["questions"].clone()).unwrap_or_default();
        let mut in_progress_tickets = Vec::new();
        if let Some(epics) = v["by_epic"].as_object() {
            for (epic, lines) in epics {
                for line in lines.as_array().into_iter().flatten() {
                    if line["state"] == "in_progress" {
                        in_progress_tickets.push(BoardTicket {
                            id: line["id"].as_i64().unwrap_or(0),
                            name: line["name"].as_str().unwrap_or("").to_owned(),
                            assigned_to: line["assigned_to"].as_str().map(str::to_owned),
                            epic: epic.clone(),
                        });
                    }
                }
            }
        }
        in_progress_tickets.sort_by_key(|t| t.id);
        Ok(Self {
            in_progress: len("in_progress"),
            ready: len("ready_to_dispatch"),
            question: questions.len() as u32,
            scheduled: num("scheduled"),
            waiting: num("waiting"),
            done: num("completed"),
            questions,
            in_progress_tickets,
            quiet: v["quiet"]
                .as_array()
                .into_iter()
                .flatten()
                .filter_map(|q| q["id"].as_i64())
                .collect(),
            waiting_on_people: len("waiting_on_people"),
        })
    }
}

/// Plan approval (§2 item 9), from `amp_check_plan`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct PlanApproval {
    pub require_plan_approval: bool,
    /// Open tickets awaiting approval, anyone's (project-wide, board order).
    pub awaiting_approval: Vec<i64>,
    pub plan_approved_at: Option<String>,
    /// The Flight plan page AMP gave (`approve_url`, #2167) while tickets await approval. Untrusted
    /// text: links are built with [`Instance::plan_url`].
    pub approve_url: Option<String>,
    /// Per-ticket approval (#2198): each awaiting ticket with its owner and whether it is the
    /// caller's own (`awaiting_approval_tickets`). `None` from servers before #2198, which have
    /// one project-wide approval: then every awaiting ticket is shown as the person's to approve.
    pub tickets: Option<Vec<AwaitingTicket>>,
}

/// A ticket awaiting approval, from `amp_check_plan`'s `awaiting_approval_tickets` (#2198).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AwaitingTicket {
    pub task_id: i64,
    /// The ticket's owner (`None`: unassigned, approved by whoever planned it).
    pub owner_user_id: Option<i64>,
    /// The caller's own: their click approves it. Others' wait for their owners (or a project
    /// lead, on their behalf).
    pub yours: bool,
}

impl PlanApproval {
    /// From an `amp_check_plan` result. Works with servers before and after #2198.
    pub fn from_check_plan(v: &Value) -> Self {
        let ids = |key: &str| -> Vec<i64> {
            v[key]
                .as_array()
                .into_iter()
                .flatten()
                .filter_map(Value::as_i64)
                .collect()
        };
        let awaiting_approval = ids("awaiting_approval");
        let tickets = v["awaiting_approval_tickets"].as_array().map(|list| {
            let yours_ids = v["awaiting_yours"]
                .as_array()
                .map(|_| ids("awaiting_yours"));
            list.iter()
                .filter_map(|t| {
                    let task_id = t["task_id"].as_i64()?;
                    let yours = match t["yours"].as_bool() {
                        Some(y) => y,
                        None => yours_ids.as_ref().is_some_and(|y| y.contains(&task_id)),
                    };
                    Some(AwaitingTicket {
                        task_id,
                        owner_user_id: t["owner_user_id"].as_i64(),
                        yours,
                    })
                })
                .collect()
        });
        PlanApproval {
            require_plan_approval: v["require_plan_approval"].as_bool().unwrap_or(false),
            awaiting_approval,
            plan_approved_at: v["plan_approved_at"].as_str().map(str::to_owned),
            approve_url: v["approve_url"].as_str().map(str::to_owned),
            tickets,
        }
    }

    /// Whether the server approves per ticket (#2198) and said whose each ticket is.
    pub fn per_ticket(&self) -> bool {
        self.tickets.is_some()
    }

    /// The awaiting tickets the person approves themselves. Before #2198 that is every awaiting
    /// ticket (one click approved the whole plan).
    pub fn yours(&self) -> Vec<i64> {
        match &self.tickets {
            Some(t) => t.iter().filter(|t| t.yours).map(|t| t.task_id).collect(),
            None => self.awaiting_approval.clone(),
        }
    }

    /// The awaiting tickets that are someone else's: their owners approve them (or a project
    /// lead, on their behalf). Always empty before #2198.
    pub fn others(&self) -> Vec<i64> {
        match &self.tickets {
            Some(t) => t.iter().filter(|t| !t.yours).map(|t| t.task_id).collect(),
            None => Vec::new(),
        }
    }

    /// Whose approval ticket `id` waits for, when AMP said (#2198).
    pub fn whose(&self, id: i64) -> Option<&AwaitingTicket> {
        self.tickets.as_ref()?.iter().find(|t| t.task_id == id)
    }
}

/// An artifact of the current ticket (§2 item 11).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Artifact {
    pub id: i64,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub kind: String,
    /// `in_review`, `approved` or `changes_requested`.
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub current_version: i64,
    #[serde(default)]
    pub url: Option<String>,
}

impl Artifact {
    /// Waiting on a person: in review, or changes asked for.
    pub fn in_review(&self) -> bool {
        self.status == "in_review" || self.status == "changes_requested"
    }
}

/// Everything the TUI shows about AMP. Cheap to clone out of the handle (`Arc`).
#[derive(Debug, Clone, Default)]
pub struct AmpMeta {
    /// Bumped on every change, so a reader can tell a new snapshot from an old one.
    pub revision: u64,
    /// `None` until the poller has a client.
    pub instance: Option<Instance>,
    pub health: MetaHealth,
    pub user: Fetched<User>,
    pub project_id: Option<i64>,
    pub project: Fetched<Project>,
    pub board: Fetched<Board>,
    pub ticket_id: Option<i64>,
    pub ticket: Fetched<Ticket>,
    pub plan: Fetched<PlanApproval>,
    pub artifacts: Fetched<Vec<Artifact>>,
}

impl AmpMeta {
    /// The project's org name (from `amp_whoami`'s orgs).
    pub fn org_name(&self) -> Option<&str> {
        let org_id = self.project.get()?.org_id?;
        self.user
            .get()?
            .orgs
            .iter()
            .find(|o| o.id == org_id)
            .map(|o| o.name.as_str())
    }

    /// Artifacts of the current ticket that wait on a person.
    pub fn artifacts_in_review(&self) -> Vec<&Artifact> {
        self.artifacts
            .get()
            .into_iter()
            .flatten()
            .filter(|a| a.in_review())
            .collect()
    }

    /// Whether plan approval matters: the project requires it.
    pub fn plan_required(&self) -> bool {
        self.project.get().is_some_and(|p| p.require_plan_approval)
    }

    /// Whether tickets wait for this person's approval (the status line's "waiting for your
    /// approval" chip is up). Since per-ticket approval (#2198) only their own count.
    pub fn plan_pending(&self) -> bool {
        self.plan_required() && self.plan.get().is_some_and(|p| !p.yours().is_empty())
    }

    /// Whether any ticket waits for approval, the person's own or others' (either plan chip is
    /// up). The plan is polled fast while this holds (#2209, #2230): a lead may approve others'
    /// tickets on their behalf, and the others' chip must clear as soon as the person's own.
    pub fn plan_awaiting(&self) -> bool {
        self.plan_required()
            && self
                .plan
                .get()
                .is_some_and(|p| !p.awaiting_approval.is_empty())
    }

    fn ticket_open(&self) -> bool {
        self.ticket_id.is_some() && self.ticket.get().is_none_or(|t| t.status != Status::Done)
    }
}

// ---------------------------------------------------------------------------------------------
// Intervals and the schedule
// ---------------------------------------------------------------------------------------------

/// How often each item is polled (§2's refresh column), plus call limits.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Intervals {
    /// Board counts and questions (30 s).
    pub board: Duration,
    /// The current ticket while it is not done (30 s).
    pub ticket: Duration,
    /// Plan approval, when the project requires it (60 s).
    pub plan: Duration,
    /// Plan approval while any ticket waits for approval (5 s, #2209, #2230): the person (or a lead
    /// on an owner's behalf) approves in the web UI, and the status line's plan chips must clear
    /// soon after.
    pub plan_pending: Duration,
    /// The ticket's artifacts while one is in review (60 s).
    pub artifacts: Duration,
    /// A health probe after this long without any call (60 s).
    pub idle_ping: Duration,
    /// Per-call timeout for the poller's own calls (10 s; the model's tools use 120 s).
    pub call_timeout: Duration,
    /// A successful call slower than this shows as degraded (3 s).
    pub slow_after: Duration,
    /// Wait between reconnect attempts (15 s).
    pub reconnect: Duration,
}

impl Default for Intervals {
    fn default() -> Self {
        Self {
            board: Duration::from_secs(30),
            ticket: Duration::from_secs(30),
            plan: Duration::from_secs(60),
            plan_pending: Duration::from_secs(5),
            artifacts: Duration::from_secs(60),
            idle_ping: Duration::from_secs(60),
            call_timeout: Duration::from_secs(10),
            slow_after: Duration::from_secs(3),
            reconnect: Duration::from_secs(15),
        }
    }
}

/// Which periodic items apply right now (from the current snapshot).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Conditions {
    pub project: bool,
    pub ticket_open: bool,
    pub plan_required: bool,
    /// Tickets wait for approval, anyone's (polled every `plan_pending`; `AmpMeta::plan_awaiting`).
    pub plan_pending: bool,
    pub artifacts_in_review: bool,
}

impl Conditions {
    fn of(meta: &AmpMeta) -> Self {
        Self {
            project: meta.project_id.is_some(),
            ticket_open: meta.ticket_open(),
            plan_required: meta.plan_required(),
            plan_pending: meta.plan_awaiting(),
            artifacts_in_review: !meta.artifacts_in_review().is_empty(),
        }
    }
}

/// When each periodic item last ran. Pure, so the intervals are unit-tested without a clock.
#[derive(Debug, Clone, Default)]
pub struct Schedule {
    last: HashMap<u16, Instant>,
    last_call: Option<Instant>,
}

impl Schedule {
    const PERIODIC: [Topics; 4] = [
        Topics::BOARD,
        Topics::TICKET,
        Topics::PLAN,
        Topics::ARTIFACTS,
    ];

    fn interval(iv: &Intervals, t: Topics, c: Conditions) -> Option<Duration> {
        match t {
            Topics::BOARD if c.project => Some(iv.board),
            Topics::TICKET if c.ticket_open => Some(iv.ticket),
            Topics::PLAN if c.plan_required && c.plan_pending => Some(iv.plan_pending),
            Topics::PLAN if c.plan_required => Some(iv.plan),
            Topics::ARTIFACTS if c.artifacts_in_review => Some(iv.artifacts),
            _ => None,
        }
    }

    /// Records that `topics` ran at `at`.
    pub fn ran(&mut self, topics: Topics, at: Instant) {
        for t in Self::PERIODIC {
            if topics.contains(t) {
                self.last.insert(t.0, at);
            }
        }
    }

    /// Records a call to AMP (any call keeps the connection checked).
    pub fn called(&mut self, at: Instant) {
        self.last_call = Some(at);
    }

    /// The items due at `now`. An item that never ran is due as soon as it applies.
    pub fn due(&self, iv: &Intervals, c: Conditions, now: Instant) -> Topics {
        let mut due = Topics::NONE;
        for t in Self::PERIODIC {
            if let Some(every) = Self::interval(iv, t, c) {
                match self.last.get(&t.0) {
                    Some(at) if now.saturating_duration_since(*at) < every => {}
                    _ => due |= t,
                }
            }
        }
        if due.is_empty()
            && self
                .last_call
                .is_none_or(|at| now.saturating_duration_since(at) >= iv.idle_ping)
        {
            due |= Topics::PING;
        }
        due
    }

    /// When the next item falls due (at most `idle_ping` after the last call).
    pub fn next_wake(&self, iv: &Intervals, c: Conditions, now: Instant) -> Instant {
        let mut next = self.last_call.map_or(now, |at| at + iv.idle_ping);
        for t in Self::PERIODIC {
            if let Some(every) = Self::interval(iv, t, c) {
                let at = self.last.get(&t.0).map_or(now, |at| *at + every);
                next = next.min(at);
            }
        }
        next.max(now)
    }
}

// ---------------------------------------------------------------------------------------------
// The handle
// ---------------------------------------------------------------------------------------------

/// Connects a fresh client (first connect when [`MetaPoller::spawn`] got none, and reconnects).
pub type Connector = Arc<
    dyn Fn() -> Pin<Box<dyn Future<Output = Result<AmpMcpClient, McpError>> + Send>> + Send + Sync,
>;

/// What the poller starts with.
#[derive(Clone, Default)]
pub struct MetaOptions {
    /// The project from `.amp.json`.
    pub project_id: Option<i64>,
    /// A ticket to show from the start (a worker session's own ticket).
    pub ticket_id: Option<i64>,
    pub intervals: Intervals,
    /// Connects (or reconnects) the poller's own client. Without one, a closed client stays down.
    pub connector: Option<Connector>,
    /// The web UI origin for links (`AMP_WEB_URL`, `[server] web_url`). `None`: the MCP URL's
    /// origin, which is right when the UI and MCP share a host (hosted AMP, the cluster).
    pub web_url: Option<String>,
}

enum Command {
    Refresh(Topics),
    SetProject(Option<i64>),
    SetTicket(Option<i64>),
    Call(Call),
}

/// A tool call asked for by the TUI (a slash command), run on the poller's connection.
struct Call {
    tool: String,
    args: Value,
    timeout: Duration,
    reply: oneshot::Sender<Result<Value, String>>,
}

/// Answers [`MetaHandle::stub`]'s calls: `(tool, arguments) -> result`.
pub type StubResponder = Arc<dyn Fn(&str, &Value) -> Result<Value, String> + Send + Sync>;

struct Inner {
    commands: UnboundedSender<Command>,
    meta: watch::Receiver<Arc<AmpMeta>>,
    /// `amp_*` tool calls in flight: call id → (name, arguments), from `ToolExecutionStart`.
    started: Mutex<HashMap<String, (String, Value)>>,
    /// [`MetaHandle::stub`] only: the calls it answered.
    stub_calls: Option<CallLog>,
    /// [`MetaHandle::stub`] only: its snapshot's sender, for [`MetaHandle::stub_update`].
    stub_meta: Option<Arc<watch::Sender<Arc<AmpMeta>>>>,
}

/// The calls a [`MetaHandle::stub`] answered: (tool, arguments).
type CallLog = Arc<Mutex<Vec<(String, Value)>>>;

/// How long [`MetaHandle::call`] waits beyond its own timeout for the poller to get to the call
/// (it may be in the middle of a refresh, whose calls have their own 10 s timeouts).
pub const CALL_QUEUE_GRACE: Duration = Duration::from_secs(30);

/// The TUI's side of the poller. Clone it freely; the poller stops when the last clone drops.
#[derive(Clone)]
pub struct MetaHandle(Arc<Inner>);

impl fmt::Debug for MetaHandle {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("MetaHandle")
            .field("revision", &self.0.meta.borrow().revision)
            .finish()
    }
}

impl MetaHandle {
    /// The current snapshot.
    pub fn snapshot(&self) -> Arc<AmpMeta> {
        self.0.meta.borrow().clone()
    }

    /// A receiver that wakes on every change (`changed().await`, then `borrow()`).
    pub fn subscribe(&self) -> watch::Receiver<Arc<AmpMeta>> {
        self.0.meta.clone()
    }

    /// Refetches `topics` now.
    pub fn refresh(&self, topics: Topics) {
        let _ = self.0.commands.send(Command::Refresh(topics));
    }

    /// Switches project (`/project`): clears the project's items and refetches them.
    pub fn set_project(&self, project_id: Option<i64>) {
        let _ = self.0.commands.send(Command::SetProject(project_id));
    }

    /// Switches the current ticket (`/ticket`, a claim): clears its items and refetches them.
    pub fn set_ticket(&self, ticket_id: Option<i64>) {
        let _ = self.0.commands.send(Command::SetTicket(ticket_id));
    }

    /// Calls an AMP tool on the poller's own connection (a slash command's read or write; never
    /// the model's). `timeout` bounds the call itself; the whole wait, including a refresh the
    /// poller may be in the middle of, is bounded by `timeout` + [`CALL_QUEUE_GRACE`]. When a
    /// write succeeds, what it touched is refetched ([`topics_for_tool`]).
    pub async fn call(&self, tool: &str, args: Value, timeout: Duration) -> Result<Value, String> {
        let (reply, answer) = oneshot::channel();
        self.0
            .commands
            .send(Command::Call(Call {
                tool: tool.to_owned(),
                args,
                timeout,
                reply,
            }))
            .map_err(|_| "the AMP metadata poller has stopped".to_owned())?;
        match tokio::time::timeout(timeout + CALL_QUEUE_GRACE, answer).await {
            Ok(Ok(result)) => result,
            Ok(Err(_)) => Err("the AMP metadata poller has stopped".to_owned()),
            Err(_) => Err(format!(
                "{tool}: no answer within {:?}",
                timeout + CALL_QUEUE_GRACE
            )),
        }
    }

    /// A handle on a fixed snapshot with no AMP connection, for tests and render checks.
    /// [`Self::set_project`] / [`Self::set_ticket`] change the snapshot's ids (keeping the other
    /// items), [`Self::call`] is answered by `responder`, and refreshes do nothing. Every call is
    /// also recorded in [`Self::stub_calls`]. Must be called inside a tokio runtime.
    pub fn stub(meta: AmpMeta, responder: StubResponder) -> Self {
        let (tx, rx) = watch::channel(Arc::new(meta));
        let tx = Arc::new(tx);
        let stub_meta = Arc::clone(&tx);
        let (commands, mut command_rx) = unbounded_channel::<Command>();
        let calls: CallLog = Arc::default();
        let recorded = Arc::clone(&calls);
        tokio::spawn(async move {
            while let Some(cmd) = command_rx.recv().await {
                let bump = |f: &dyn Fn(&mut AmpMeta)| {
                    tx.send_modify(|arc| {
                        let m = Arc::make_mut(arc);
                        f(m);
                        m.revision += 1;
                    })
                };
                match cmd {
                    Command::Refresh(_) => {}
                    Command::SetProject(id) => bump(&|m| m.project_id = id),
                    Command::SetTicket(id) => bump(&|m| m.ticket_id = id),
                    Command::Call(c) => {
                        if let Ok(mut log) = recorded.lock() {
                            log.push((c.tool.clone(), c.args.clone()));
                        }
                        let _ = c.reply.send(responder(&c.tool, &c.args));
                    }
                }
            }
        });
        Self(Arc::new(Inner {
            commands,
            meta: rx,
            started: Mutex::new(HashMap::new()),
            stub_calls: Some(calls),
            stub_meta: Some(stub_meta),
        }))
    }

    /// [`Self::stub`] only: changes the snapshot (bumping its revision), as a poll that found a
    /// change would. Does nothing on a real poller's handle.
    pub fn stub_update(&self, f: impl FnOnce(&mut AmpMeta)) {
        if let Some(tx) = &self.0.stub_meta {
            tx.send_modify(|arc| {
                let m = Arc::make_mut(arc);
                f(m);
                m.revision += 1;
            });
        }
    }

    /// The calls a [`Self::stub`] handle answered, in order (empty for a real poller).
    pub fn stub_calls(&self) -> Vec<(String, Value)> {
        self.0
            .stub_calls
            .as_ref()
            .and_then(|c| c.lock().ok().map(|c| c.clone()))
            .unwrap_or_default()
    }

    /// Reports a finished tool call by name and arguments. Errors change nothing, so pass only
    /// calls that succeeded.
    pub fn tool_finished(&self, name: &str, args: &Value) {
        if name == "amp_claim_task"
            && let Some(id) = id_of(&args["task_id"])
            && self.0.meta.borrow().ticket_id != Some(id)
        {
            self.set_ticket(Some(id));
            return;
        }
        let ticket = self.0.meta.borrow().ticket_id;
        let topics = topics_for_tool(name, args, ticket);
        if !topics.is_empty() {
            self.refresh(topics);
        }
    }

    /// Feeds one agent event: `amp_*` tool calls that end without error refresh what they touched.
    pub fn observe(&self, event: &AgentEvent) {
        match event {
            AgentEvent::ToolExecutionStart {
                tool_call_id,
                tool_name,
                args,
            } if tool_name.starts_with("amp_") => {
                if let Ok(mut started) = self.0.started.lock() {
                    started.insert(tool_call_id.clone(), (tool_name.clone(), args.clone()));
                }
            }
            AgentEvent::ToolExecutionEnd {
                tool_call_id,
                tool_name,
                is_error,
                result,
            } if tool_name.starts_with("amp_") => {
                let started = self
                    .0
                    .started
                    .lock()
                    .ok()
                    .and_then(|mut s| s.remove(tool_call_id));
                if *is_error {
                    // A refusal that needs a person (#2171): show the approval state as it is now.
                    if crate::approval::refusal(&agent_result_text(result)).is_some() {
                        self.refresh(Topics::PROJECT | Topics::PLAN | Topics::BOARD);
                    }
                    return;
                }
                let args = started.map(|(_, a)| a).unwrap_or(Value::Null);
                self.tool_finished(tool_name, &args);
            }
            _ => {}
        }
    }
}

/// The text blocks of an agent tool result.
fn agent_result_text(result: &rpi_agent::AgentToolResult) -> String {
    result
        .content
        .iter()
        .filter_map(|c| match c {
            rpi_agent::TextContentOrImage::Text(t) => Some(t.text.as_str()),
            rpi_agent::TextContentOrImage::Image(_) => None,
        })
        .collect::<Vec<_>>()
        .join("\n")
}

// ---------------------------------------------------------------------------------------------
// The poller task
// ---------------------------------------------------------------------------------------------

/// The background poller.
pub struct MetaPoller {
    tx: watch::Sender<Arc<AmpMeta>>,
    client: Option<Arc<AmpMcpClient>>,
    connector: Option<Connector>,
    web_url: Option<String>,
    iv: Intervals,
    schedule: Schedule,
    /// `amp_get_plan` cache: (project id, epics as (id, name, stories as (id, name))).
    tree: Option<(i64, Vec<EpicNode>)>,
    next_reconnect: Option<Instant>,
    /// The last call on the client failed with 401/403 and none has been answered since; cleared
    /// by an answered call or a new client (#2156).
    auth_required: bool,
    /// Slash-command calls waiting to run ([`MetaHandle::call`]).
    calls: Vec<Call>,
}

#[derive(Debug, Clone, Deserialize)]
struct StoryNode {
    id: i64,
    #[serde(default)]
    name: String,
}

#[derive(Debug, Clone, Deserialize)]
struct EpicNode {
    id: i64,
    #[serde(default)]
    name: String,
    #[serde(default)]
    stories: Vec<StoryNode>,
}

#[derive(Debug, Deserialize)]
struct TaskRecord {
    id: i64,
    #[serde(default)]
    project_id: i64,
    #[serde(default)]
    name: String,
    #[serde(default)]
    state: String,
    #[serde(default)]
    block_reason: Option<String>,
    #[serde(default)]
    assigned_to: Option<String>,
    #[serde(default)]
    dispatched_at: Option<String>,
    #[serde(default)]
    epic_id: Option<i64>,
    #[serde(default)]
    story_id: Option<i64>,
}

impl MetaPoller {
    /// Starts the poller on `client` (its own connection; never the model's tools). With no
    /// client, it connects with `options.connector` in the background, so the TUI starts at once.
    /// Must be called inside a tokio runtime.
    pub fn spawn(client: Option<Arc<AmpMcpClient>>, options: MetaOptions) -> MetaHandle {
        let meta = AmpMeta {
            project_id: options.project_id,
            ticket_id: options.ticket_id,
            instance: client
                .as_deref()
                .map(|c| Instance::of(c, options.web_url.as_deref())),
            ..AmpMeta::default()
        };
        let (tx, rx) = watch::channel(Arc::new(meta));
        let (commands, command_rx) = unbounded_channel();
        let poller = Self {
            tx,
            client,
            connector: options.connector,
            web_url: options.web_url,
            iv: options.intervals,
            schedule: Schedule::default(),
            tree: None,
            next_reconnect: None,
            auth_required: false,
            calls: Vec::new(),
        };
        tokio::spawn(poller.run(command_rx));
        MetaHandle(Arc::new(Inner {
            commands,
            meta: rx,
            started: Mutex::new(HashMap::new()),
            stub_calls: None,
            stub_meta: None,
        }))
    }

    fn meta(&self) -> Arc<AmpMeta> {
        self.tx.borrow().clone()
    }

    fn update(&self, f: impl FnOnce(&mut AmpMeta)) {
        self.tx.send_modify(|arc| {
            let m = Arc::make_mut(arc);
            f(m);
            m.revision += 1;
        });
    }

    async fn run(mut self, mut commands: UnboundedReceiver<Command>) {
        let mut pending = Topics::ALL;
        loop {
            if self.ensure_client().await {
                pending |= Topics::ALL;
            }
            pending |= self.run_calls().await;
            // While re-login is needed and a reconnect is possible, the old client is not polled:
            // the reconnect attempt (every `intervals.reconnect`) is the only probe (#2156).
            let auth_wait = self.auth_wait();
            if self.client.is_some() && !auth_wait {
                let now = Instant::now();
                pending |= self
                    .schedule
                    .due(&self.iv, Conditions::of(&self.meta()), now);
                if !pending.is_empty() {
                    let topics = std::mem::take(&mut pending);
                    self.refresh(topics).await;
                }
            }
            // Commands that arrived during the refresh run before sleeping.
            loop {
                match commands.try_recv() {
                    Ok(cmd) => pending |= self.apply(cmd),
                    Err(tokio::sync::mpsc::error::TryRecvError::Empty) => break,
                    Err(tokio::sync::mpsc::error::TryRecvError::Disconnected) => return,
                }
            }
            let now = Instant::now();
            let wake = if self.client.is_none() {
                // Nothing to poll until a connect succeeds; pending items wait for it.
                match (&self.connector, self.next_reconnect) {
                    (Some(_), Some(at)) => at,
                    (Some(_), None) => now,
                    (None, _) => now + Duration::from_secs(24 * 3600),
                }
            } else if auth_wait {
                // Pending topics wait for the reconnect, which refetches everything anyway.
                self.next_reconnect.unwrap_or(now)
            } else if !pending.is_empty() || !self.calls.is_empty() {
                continue;
            } else {
                let mut wake = self
                    .schedule
                    .next_wake(&self.iv, Conditions::of(&self.meta()), now);
                if self.connector.is_some() && self.client.as_ref().is_some_and(|c| c.is_closed()) {
                    wake = wake.min(self.next_reconnect.unwrap_or(now));
                }
                wake
            };
            tokio::select! {
                cmd = commands.recv() => match cmd {
                    Some(cmd) => pending |= self.apply(cmd),
                    None => return,
                },
                _ = tokio::time::sleep_until(tokio::time::Instant::from_std(wake)) => {}
            }
        }
    }

    fn apply(&mut self, cmd: Command) -> Topics {
        match cmd {
            Command::Refresh(t) => t,
            Command::Call(call) => {
                self.calls.push(call);
                Topics::NONE
            }
            Command::SetProject(id) => {
                if self.meta().project_id == id {
                    return Topics::NONE;
                }
                self.tree = None;
                self.update(|m| {
                    m.project_id = id;
                    m.project = Fetched::default();
                    m.board = Fetched::default();
                    m.plan = Fetched::default();
                });
                Topics::PROJECT | Topics::BOARD | Topics::PLAN | Topics::TREE
            }
            Command::SetTicket(id) => {
                if self.meta().ticket_id == id {
                    return Topics::TICKET;
                }
                self.update(|m| {
                    m.ticket_id = id;
                    m.ticket = Fetched::default();
                    m.artifacts = Fetched::default();
                });
                Topics::TICKET | Topics::ARTIFACTS
            }
        }
    }

    /// True when the open client needs a re-login and the connector can replace it (#2156).
    ///
    /// `amp-harness login` registers a new OAuth client (DCR) each time. The open client's
    /// rmcp `AuthorizationManager` reloads the stored tokens on each request, so it may work
    /// again right after a login, but it keeps the old client id and refuses to refresh the new
    /// client's tokens ("stored credentials belong to a different client"). Only a fresh connect
    /// (`initialize_from_store`) picks up the new client id, so the poller reconnects instead.
    ///
    /// It is the poller's own flag rather than the snapshot's `Connection`: after a 401 the rmcp
    /// transport may briefly read as closed, which would flip the snapshot to Down and back.
    fn auth_wait(&self) -> bool {
        self.auth_required && self.connector.is_some() && self.client.is_some()
    }

    /// Makes sure there is an open, usable client, connecting or reconnecting with the connector
    /// when it is time to: when there is no client, when it is closed, or when it needs a
    /// re-login ([`Self::auth_wait`]). Returns true when a new client was connected (refetch
    /// everything).
    async fn ensure_client(&mut self) -> bool {
        let closed = self.client.as_ref().is_none_or(|c| c.is_closed());
        let auth_wait = self.auth_wait();
        if !closed && !auth_wait {
            return false;
        }
        if !auth_wait && let Some(client) = &self.client {
            let reason = match client.health() {
                Health::Disconnected { reason } => reason,
                _ => "connection closed".to_owned(),
            };
            self.update(|m| m.health.connection = Connection::Down { reason });
        }
        let Some(connector) = self.connector.clone() else {
            return false;
        };
        let now = Instant::now();
        if self.next_reconnect.is_some_and(|at| now < at) {
            return false;
        }
        self.next_reconnect = Some(now + self.iv.reconnect);
        match connector().await {
            Ok(client) => {
                let instance = Instance::of(&client, self.web_url.as_deref());
                self.client = Some(Arc::new(client));
                self.next_reconnect = None;
                self.auth_required = false;
                self.update(|m| {
                    m.instance = Some(instance);
                    m.health.connection = Connection::Ok;
                });
                true
            }
            // The open client stays (slash commands may still use it) and so does its
            // AuthRequired: a failed attempt, for whatever reason, does not change what it says.
            Err(_) if auth_wait => false,
            Err(e) => {
                let connection = if e.is_auth_required() {
                    Connection::AuthRequired
                } else {
                    // A plain reason for `/amp` (#2178), never rmcp's type names.
                    Connection::Down { reason: e.reason() }
                };
                self.update(|m| m.health.connection = connection);
                false
            }
        }
    }

    /// Runs the queued slash-command calls. Without a client they fail at once (rather than wait
    /// for a reconnect). Returns what successful writes touched.
    async fn run_calls(&mut self) -> Topics {
        let mut touched = Topics::NONE;
        for c in std::mem::take(&mut self.calls) {
            let open = self.client.as_ref().is_some_and(|c| !c.is_closed());
            let result = if open {
                self.call_with(&c.tool, c.args.clone(), c.timeout).await
            } else {
                Err(format!("{}: not connected to AMP", c.tool))
            };
            if result.is_ok() {
                touched |= topics_for_tool(&c.tool, &c.args, self.meta().ticket_id);
            }
            let _ = c.reply.send(result);
        }
        touched
    }

    /// One AMP call on the poller's client: JSON out, health in.
    async fn call(&mut self, tool: &str, args: Value) -> Result<Value, String> {
        let timeout = self.iv.call_timeout;
        self.call_with(tool, args, timeout).await
    }

    /// [`Self::call`] with its own timeout. The connection's health follows transport, timeout
    /// and auth failures only: an error AMP answered with (`isError`, or a JSON-RPC error such
    /// as an unknown ticket) is the caller's item error, not the connection's.
    async fn call_with(
        &mut self,
        tool: &str,
        args: Value,
        timeout: Duration,
    ) -> Result<Value, String> {
        let Some(client) = self.client.clone() else {
            return Err("not connected".to_owned());
        };
        let arguments = match args {
            Value::Object(map) => Some(map),
            _ => None,
        };
        let started = Instant::now();
        let result = client
            .call_tool_with_timeout(tool, arguments, timeout)
            .await;
        let latency = started.elapsed();
        let now = Instant::now();
        self.schedule.called(now);
        let connection = Connection::after_call(client.health(), latency, self.iv.slow_after);
        let answered = result
            .as_ref()
            .map_or_else(McpError::is_server_error, |_| true);
        if answered {
            self.auth_required = false;
        } else if connection == Connection::AuthRequired {
            self.auth_required = true;
        }
        self.update(|m| {
            m.health.connection = connection;
            m.health.latency = Some(latency);
            m.health.last_call = Some(now);
            m.health.calls += 1;
            if answered {
                m.health.last_ok = Some(now);
            }
        });
        let result = result.map_err(|e| e.to_string())?;
        if result.is_error == Some(true) {
            let text = result_text(&result);
            return Err(format!(
                "{tool}: {}",
                if text.trim().is_empty() {
                    "the tool reported an error"
                } else {
                    text.trim()
                }
            ));
        }
        result_json(&result).ok_or_else(|| format!("{tool}: no JSON in the result"))
    }

    async fn refresh(&mut self, topics: Topics) {
        let now = Instant::now();
        let mut ran = Topics::NONE;
        if topics.contains(Topics::WHOAMI) || topics.contains(Topics::PING) {
            let r = self.call("amp_whoami", json!({})).await;
            let r = r.and_then(|v| serde_json::from_value::<User>(v).map_err(|e| e.to_string()));
            self.update(|m| m.user.set(r));
            ran |= Topics::WHOAMI;
        }
        let project_id = self.meta().project_id;
        if topics.contains(Topics::PROJECT)
            && let Some(pid) = project_id
        {
            let r = self
                .call("amp_get_project", json!({"project_id": pid}))
                .await;
            let r = r.and_then(|v| {
                serde_json::from_value::<Project>(v["project"].clone()).map_err(|e| e.to_string())
            });
            self.update(|m| m.project.set(r));
        }
        if topics.contains(Topics::TREE) {
            self.tree = None;
        }
        if topics.contains(Topics::BOARD)
            && let Some(pid) = project_id
        {
            let r = self
                .call(
                    "amp_list_tasks",
                    json!({"project_id": pid, "owner": "any", "format": "compact"}),
                )
                .await;
            let r = r.and_then(|v| Board::from_compact(&v));
            self.update(|m| m.board.set(r));
            ran |= Topics::BOARD;
        }
        let ticket_id = self.meta().ticket_id;
        if topics.contains(Topics::TICKET)
            && let Some(tid) = ticket_id
        {
            let r = self.fetch_ticket(tid).await;
            self.update(|m| {
                if m.ticket_id == Some(tid) {
                    m.ticket.set(r);
                }
            });
            ran |= Topics::TICKET;
        }
        if topics.contains(Topics::PLAN)
            && let Some(pid) = project_id
            && self.meta().plan_required()
        {
            let r = self
                .call("amp_check_plan", json!({"project_id": pid}))
                .await;
            let r = r.map(|v| PlanApproval::from_check_plan(&v));
            self.update(|m| m.plan.set(r));
            ran |= Topics::PLAN;
        }
        if topics.contains(Topics::ARTIFACTS)
            && let Some(tid) = ticket_id
        {
            let r = self
                .call("amp_list_artifacts", json!({"task_id": tid}))
                .await;
            let r = r.and_then(|v| {
                serde_json::from_value::<Vec<Artifact>>(v["artifacts"].clone())
                    .map_err(|e| e.to_string())
            });
            self.update(|m| {
                if m.ticket_id == Some(tid) {
                    m.artifacts.set(r);
                }
            });
            ran |= Topics::ARTIFACTS;
        }
        // Items that did not apply count as run, so they wait a full interval before retrying.
        self.schedule.ran(topics | ran, now);
    }

    async fn fetch_ticket(&mut self, id: i64) -> Result<Ticket, String> {
        let v = self.call("amp_get_task", json!({"task_id": id})).await?;
        let t: TaskRecord =
            serde_json::from_value(v["task"].clone()).map_err(|e| format!("amp_get_task: {e}"))?;
        let mut ticket = Ticket {
            id: t.id,
            project_id: t.project_id,
            status: Status::of(&t.state, t.block_reason.as_deref()),
            name: t.name,
            state: t.state,
            block_reason: t.block_reason.filter(|r| !r.is_empty()),
            assigned_to: t.assigned_to.filter(|a| !a.is_empty()),
            dispatched_at: t.dispatched_at,
            epic: None,
            story: None,
            epic_index: None,
        };
        if let Some(epic_id) = t.epic_id {
            let known = self.tree.as_ref().is_some_and(|(pid, epics)| {
                *pid == t.project_id && epics.iter().any(|e| e.id == epic_id)
            });
            if !known && t.project_id != 0 {
                // A failed plan read leaves the names out; the ticket still shows.
                if let Ok(v) = self
                    .call("amp_get_plan", json!({"project_id": t.project_id}))
                    .await
                    && let Ok(epics) = serde_json::from_value::<Vec<EpicNode>>(v["epics"].clone())
                {
                    self.tree = Some((t.project_id, epics));
                }
            }
            if let Some((_, epics)) = &self.tree
                && let Some((index, epic)) = epics.iter().enumerate().find(|(_, e)| e.id == epic_id)
            {
                ticket.epic_index = Some(index);
                ticket.epic = Some(Named {
                    id: epic.id,
                    name: epic.name.clone(),
                });
                ticket.story = t.story_id.and_then(|sid| {
                    epic.stories.iter().find(|s| s.id == sid).map(|s| Named {
                        id: s.id,
                        name: s.name.clone(),
                    })
                });
            }
        }
        Ok(ticket)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn iv() -> Intervals {
        Intervals::default()
    }

    #[test]
    fn web_base_is_the_origin() {
        assert_eq!(
            web_base("https://amp.simstech.cloud/mcp"),
            "https://amp.simstech.cloud"
        );
        assert_eq!(
            web_base("http://localhost:18000/mcp"),
            "http://localhost:18000"
        );
        assert_eq!(web_base("https://amp.local"), "https://amp.local");
        let i = Instance {
            mcp_url: "https://amp.simstech.cloud/mcp".into(),
            web_base: "https://amp.simstech.cloud".into(),
            protocol: "2026-07-28".into(),
            server_name: None,
            server_version: None,
            auth: "none".into(),
        };
        assert_eq!(i.host(), "amp.simstech.cloud");
        assert_eq!(
            i.ticket_url(2, 2104),
            "https://amp.simstech.cloud/project/2/board?task=2104"
        );
        assert_eq!(i.project_url(2), "https://amp.simstech.cloud/project/2");
        assert_eq!(i.artifact_url(3), "https://amp.simstech.cloud/artifacts/3");
        assert_eq!(i.kb_url(2), "https://amp.simstech.cloud/project/2/kb");
    }

    #[test]
    fn tools_map_to_what_they_touch() {
        let t = Some(2104);
        let on = |n: &str, a: Value| topics_for_tool(n, &a, t);
        // Not AMP, or read-only: nothing.
        assert_eq!(on("bash", json!({})), Topics::NONE);
        assert_eq!(on("amp_get_task", json!({"task_id": 2104})), Topics::NONE);
        assert_eq!(on("amp_list_tasks", json!({})), Topics::NONE);
        assert_eq!(on("amp_kb_search", json!({})), Topics::NONE);
        assert_eq!(on("amp_session_usage", json!({})), Topics::NONE);
        // Ticket changes: the board, plus the ticket when it is the current one.
        assert_eq!(
            on("amp_complete_task", json!({"task_id": 2104})),
            Topics::BOARD | Topics::TICKET
        );
        assert_eq!(
            on("amp_complete_task", json!({"task_id": "#2104"})),
            Topics::BOARD | Topics::TICKET
        );
        assert_eq!(
            on("amp_dispatch_task", json!({"task_id": 9})),
            Topics::BOARD
        );
        assert_eq!(on("amp_run_ticket", json!({"task_id": 9})), Topics::BOARD);
        assert_eq!(
            on("amp_handover_tasks", json!({"task_ids": [1, 2104]})),
            Topics::BOARD | Topics::TICKET
        );
        assert_eq!(
            on("amp_create_tasks", json!({"tasks": []})),
            Topics::BOARD | Topics::PLAN
        );
        assert_eq!(
            on("amp_approve_plan", json!({})),
            Topics::PROJECT | Topics::PLAN | Topics::BOARD
        );
        assert_eq!(
            on("amp_publish_artifact", json!({"task_id": 2104})),
            Topics::ARTIFACTS
        );
        assert_eq!(
            on("amp_add_task_comment", json!({"task_id": 2104})),
            Topics::TICKET
        );
        assert_eq!(
            on("amp_add_task_comment", json!({"task_id": 1})),
            Topics::NONE
        );
        assert_eq!(on("amp_whoami", json!({})), Topics::WHOAMI);
        assert!(on("amp_update_story", json!({"story_id": 1})).contains(Topics::TREE));
        // No current ticket: the ticket is never refreshed.
        assert_eq!(
            topics_for_tool("amp_complete_task", &json!({"task_id": 2104}), None),
            Topics::BOARD
        );
    }

    #[test]
    fn schedule_runs_each_item_on_its_interval() {
        let iv = iv();
        let t0 = Instant::now();
        let mut s = Schedule::default();
        let c = Conditions {
            project: true,
            ticket_open: true,
            plan_required: false,
            plan_pending: false,
            artifacts_in_review: false,
        };
        // Nothing ran yet: board and ticket are due (plan and artifacts don't apply).
        assert_eq!(s.due(&iv, c, t0), Topics::BOARD | Topics::TICKET);
        s.ran(Topics::BOARD | Topics::TICKET, t0);
        s.called(t0);
        assert_eq!(s.due(&iv, c, t0 + Duration::from_secs(29)), Topics::NONE);
        assert_eq!(s.next_wake(&iv, c, t0), t0 + Duration::from_secs(30));
        assert_eq!(
            s.due(&iv, c, t0 + Duration::from_secs(30)),
            Topics::BOARD | Topics::TICKET
        );
        // Plan approval applies: every 60 s.
        let c = Conditions {
            plan_required: true,
            ..c
        };
        s.ran(Topics::PLAN, t0);
        assert_eq!(
            s.due(&iv, c, t0 + Duration::from_secs(59)) - Topics::BOARD - Topics::TICKET,
            Topics::NONE
        );
        assert!(
            s.due(&iv, c, t0 + Duration::from_secs(60))
                .contains(Topics::PLAN)
        );
        // A done ticket stops polling.
        let c = Conditions {
            ticket_open: false,
            plan_required: false,
            ..c
        };
        assert_eq!(s.due(&iv, c, t0 + Duration::from_secs(45)), Topics::BOARD);
    }

    /// #2209: a person approves the plan in the web UI; the poller must notice soon. A faux clock
    /// steps the pure schedule in 100 ms ticks, running whatever is due (the "server" answers the
    /// plan with the approval state at that instant), for approvals landing at every offset of
    /// a poll cycle. The chip (AmpMeta::plan_pending) clears within 10 s of the approval, and
    /// with nothing pending the plan goes back to the 60 s poll.
    #[test]
    fn a_pending_plan_is_polled_fast_so_the_chip_clears_within_10s_of_approval() {
        let iv = iv();
        assert!(iv.plan_pending <= Duration::from_secs(5));
        let project = Project {
            id: 2,
            name: "AMP".into(),
            code: "amp".into(),
            state: "active".into(),
            org_id: None,
            require_plan_approval: true,
        };
        let plan = |awaiting: Vec<i64>| PlanApproval {
            require_plan_approval: true,
            awaiting_approval: awaiting,
            ..PlanApproval::default()
        };
        let tick = Duration::from_millis(100);
        let t0 = Instant::now();
        for approve_at_ms in (0..=65_000u64).step_by(700) {
            let approve_at = t0 + Duration::from_secs(1) + Duration::from_millis(approve_at_ms);
            let mut meta = AmpMeta {
                project_id: Some(2),
                ..AmpMeta::default()
            };
            meta.project.set(Ok(project.clone()));
            meta.plan.set(Ok(plan(vec![2105])));
            let mut s = Schedule::default();
            s.ran(Topics::ALL, t0);
            s.called(t0);
            assert!(meta.plan_pending(), "the chip is up");
            let mut now = t0;
            let cleared = loop {
                now += tick;
                assert!(
                    now < approve_at + Duration::from_secs(120),
                    "never cleared (approval at +{approve_at_ms} ms)"
                );
                let due = s.due(&iv, Conditions::of(&meta), now);
                if due.contains(Topics::PLAN) {
                    let awaiting = if now >= approve_at {
                        vec![]
                    } else {
                        vec![2105]
                    };
                    meta.plan.set(Ok(plan(awaiting)));
                }
                s.ran(due, now);
                s.called(now);
                if !meta.plan_pending() {
                    break now;
                }
            };
            let lag = cleared.saturating_duration_since(approve_at);
            assert!(
                lag <= Duration::from_secs(10),
                "cleared {lag:?} after an approval at +{approve_at_ms} ms"
            );
        }
        // Nothing pending: the plan is back on its 60 s poll.
        let c = Conditions {
            project: true,
            plan_required: true,
            ..Conditions::default()
        };
        let mut s = Schedule::default();
        s.ran(Topics::PLAN, t0);
        assert!(
            !s.due(&iv, c, t0 + Duration::from_secs(59))
                .contains(Topics::PLAN)
        );
        assert!(
            s.due(&iv, c, t0 + Duration::from_secs(60))
                .contains(Topics::PLAN)
        );
        let pending = Conditions {
            plan_pending: true,
            ..c
        };
        assert!(
            s.due(&iv, pending, t0 + Duration::from_secs(5))
                .contains(Topics::PLAN)
        );
        assert!(s.next_wake(&iv, pending, t0) <= t0 + Duration::from_secs(5));
        // Pending but the project doesn't require approval: the plan isn't polled at all.
        let off = Conditions {
            plan_required: false,
            ..pending
        };
        assert!(
            !s.due(&iv, off, t0 + Duration::from_secs(600))
                .contains(Topics::PLAN)
        );
    }

    /// #2230: only others' tickets wait (a lead then approves them on their owners' behalf).
    /// They keep the plan on the fast poll too, so the others' chip clears within the fast-poll
    /// interval rather than after the 60 s plan poll.
    #[test]
    fn others_waiting_keep_the_plan_on_the_fast_poll() {
        let iv = iv();
        let mut meta = AmpMeta {
            project_id: Some(2),
            ..AmpMeta::default()
        };
        meta.project.set(Ok(Project {
            id: 2,
            name: "AMP".into(),
            code: "amp".into(),
            state: "active".into(),
            org_id: None,
            require_plan_approval: true,
        }));
        let others = |ids: Vec<i64>| PlanApproval {
            require_plan_approval: true,
            awaiting_approval: ids.clone(),
            tickets: Some(
                ids.iter()
                    .map(|&task_id| AwaitingTicket {
                        task_id,
                        owner_user_id: Some(9),
                        yours: false,
                    })
                    .collect(),
            ),
            ..PlanApproval::default()
        };
        meta.plan.set(Ok(others(vec![196])));
        assert!(!meta.plan_pending(), "none of yours wait");
        assert!(meta.plan_awaiting(), "others' wait");
        let c = Conditions::of(&meta);
        assert!(c.plan_pending);
        let t0 = Instant::now();
        let mut s = Schedule::default();
        s.ran(Topics::ALL, t0);
        assert!(s.due(&iv, c, t0 + iv.plan_pending).contains(Topics::PLAN));
        // Approved on behalf: nothing waits, back to the 60 s poll.
        meta.plan.set(Ok(others(vec![])));
        assert!(!meta.plan_awaiting());
        assert!(
            !s.due(&iv, Conditions::of(&meta), t0 + Duration::from_secs(59))
                .contains(Topics::PLAN)
        );
    }

    #[test]
    fn idle_ping_only_when_nothing_else_runs() {
        let iv = iv();
        let t0 = Instant::now();
        let mut s = Schedule::default();
        let none = Conditions::default();
        assert_eq!(s.due(&iv, none, t0), Topics::PING);
        s.called(t0);
        assert_eq!(s.due(&iv, none, t0 + Duration::from_secs(59)), Topics::NONE);
        assert_eq!(s.next_wake(&iv, none, t0), t0 + Duration::from_secs(60));
        assert_eq!(s.due(&iv, none, t0 + Duration::from_secs(60)), Topics::PING);
        // With a project, the board poll is the health check.
        let c = Conditions {
            project: true,
            ..none
        };
        assert_eq!(s.due(&iv, c, t0 + Duration::from_secs(90)), Topics::BOARD);
    }

    #[test]
    fn board_from_compact_list_tasks() {
        let v = json!({
            "ready_to_dispatch": [2086, 2087], "in_progress": [2085],
            "questions": [{"id": 2084, "name": "M1-3 OAuth", "question": "Keychain or file?"}],
            "waiting": 3, "scheduled": 1, "completed": 9,
            "quiet": [{"id": 2085, "name": "x", "agent_id": "a", "silent_minutes": 20}],
            "waiting_on_people": [{"id": 2092}],
            "by_epic": {"Harness": [
                {"id": 2085, "name": "M1-4 CLI", "state": "in_progress", "assigned_to": "amp:amp-worker-backend"},
                {"id": 2086, "name": "M1-5", "state": "ready"}
            ]},
            "count": 16, "owner_filter": "any"
        });
        let b = Board::from_compact(&v).unwrap();
        assert_eq!(
            (
                b.in_progress,
                b.ready,
                b.question,
                b.scheduled,
                b.waiting,
                b.done
            ),
            (1, 2, 1, 1, 3, 9)
        );
        assert_eq!(b.questions[0].question, "Keychain or file?");
        assert_eq!(b.in_progress_tickets.len(), 1);
        assert_eq!(
            b.in_progress_tickets[0].assigned_to.as_deref(),
            Some("amp:amp-worker-backend")
        );
        assert_eq!(b.quiet, vec![2085]);
        assert_eq!(b.waiting_on_people, 1);
        assert_eq!(
            b.nonzero(),
            vec![
                (Status::InProgress, 1),
                (Status::Ready, 2),
                (Status::Question, 1),
                (Status::Scheduled, 1)
            ]
        );
        assert!(Board::from_compact(&json!({"ready_to_dispatch": []})).is_ok());
        assert!(Board::from_compact(&json!({"in_progress": []})).is_err());
    }

    #[test]
    fn connection_levels() {
        let slow = Duration::from_secs(3);
        assert_eq!(
            Connection::after_call(Health::Connected, Duration::from_millis(10), slow).level(),
            HealthLevel::Ok
        );
        assert_eq!(
            Connection::after_call(Health::Connected, Duration::from_secs(4), slow).level(),
            HealthLevel::Degraded
        );
        assert_eq!(
            Connection::after_call(Health::AuthRequired, Duration::ZERO, slow),
            Connection::AuthRequired
        );
        assert_eq!(Connection::AuthRequired.level(), HealthLevel::Down);
        assert_eq!(Connection::Connecting.level(), HealthLevel::Degraded);
    }

    #[test]
    fn plan_approval_reads_old_and_per_ticket_servers() {
        // Before #2198: one project-wide approval; every awaiting ticket is the person's to approve.
        let old = PlanApproval::from_check_plan(&json!({
            "require_plan_approval": true, "awaiting_approval": [3, 4],
            "approve_url": "/project/2/plan"
        }));
        assert!(!old.per_ticket());
        assert_eq!((old.yours(), old.others()), (vec![3, 4], vec![]));
        assert_eq!(old.whose(3), None);

        // #2198: owners and `yours` per ticket.
        let new = PlanApproval::from_check_plan(&json!({
            "require_plan_approval": true, "awaiting_approval": [3, 4, 5],
            "awaiting_approval_tickets": [
                {"task_id": 3, "owner_user_id": 7, "yours": true},
                {"task_id": 4, "owner_user_id": 9, "yours": false},
                {"task_id": 5, "owner_user_id": null}
            ],
            "awaiting_yours": [3, 5]
        }));
        assert!(new.per_ticket());
        // `yours` missing on an item falls back to awaiting_yours.
        assert_eq!((new.yours(), new.others()), (vec![3, 5], vec![4]));
        assert_eq!(
            new.whose(4).map(|t| (t.owner_user_id, t.yours)),
            Some((Some(9), false))
        );
        assert_eq!(new.whose(5).map(|t| t.owner_user_id), Some(None));

        // Nothing awaiting on a per-ticket server.
        let none = PlanApproval::from_check_plan(&json!({
            "awaiting_approval": [], "awaiting_approval_tickets": [], "awaiting_yours": []
        }));
        assert!(none.per_ticket() && none.yours().is_empty() && none.others().is_empty());
    }
}
