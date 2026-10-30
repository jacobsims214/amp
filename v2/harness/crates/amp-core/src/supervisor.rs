//! The worker supervisor and the manager's `amp_run_ticket` tool (D7, M2-5).
//!
//! `amp_run_ticket(task_id, agent_id)` runs one specialist on one AMP ticket, in the foreground of
//! the tool call, and returns the worker's final message and outcome. Per call:
//!
//! 1. **Check the agent.** It must be a role [`RoleRegistry::get`] knows *and* one the manager may
//!    dispatch ([`RoleRegistry::dispatchable`]). An id the policy's `amp-*` rule would allow but
//!    the roster doesn't have is refused before anything happens in AMP.
//! 2. **Wait for a slot.** A semaphore caps running workers ([`SupervisorConfig::max_parallel`],
//!    default 4); extra calls wait, and a cancelled wait dispatches nothing.
//! 3. **`amp_dispatch_task`**, so the board shows the ticket in progress only once its worker is
//!    about to start. AMP refusing it (someone else's ticket, blocked, human) ends the call.
//! 4. **Worktree.** Roles that can edit files run in `git worktree` `<repo>/.worktrees/<ticket>`
//!    (created on branch `amp-<ticket>` off `HEAD`, or reused if it already exists); read-only
//!    roles run in the manager's directory. `worktree`, `branch` and `base` arguments override.
//!    `.worktrees/` is added to the repo's `info/exclude` if nothing ignores it yet.
//! 5. **An independent `AgentHarness`** from [`Runtime::build`]: the role's own model and thinking,
//!    its prompt + AMP instructions + `<amp_context>` with the ticket, worktree and branch,
//!    rpi-tools bound to the worker's directory, the role-filtered `amp_*` tools on the shared MCP
//!    client, and its own JSONL session under `<state>/sessions/<ticket>/`.
//! 6. **Run** until the worker stops, the wall-clock timeout ([`SupervisorConfig::timeout`],
//!    default 60 min) or the turn budget runs out, or the call is cancelled (the manager's run was
//!    aborted, or [`Supervisor::cancel_all`]). Ending a worker early aborts its harness, which
//!    cancels its in-flight tools.
//!
//! Every call that got as far as step 3 ends in a [`WorkerReport`] with a [`WorkerOutcome`], the
//! worker's usage [`Totals`] and where its work is. Run reporting ([`crate::run_report`], M2-6)
//! reports it to AMP through [`WorkerHooks`]. Errors before step 3 are tool errors for the
//! manager and touch nothing in AMP.
//!
//! **Launched (background) dispatch (AMP #2834).** With [`SupervisorConfig::dispatch`] set to
//! [`DispatchMode::Launch`], `amp_run_ticket` doesn't wait for the worker: steps 1-3 happen, then
//! the run is handed to a task the supervisor owns and the call returns a [`LaunchAck`]. The
//! worker's end is queued ([`Supervisor::take_launch_ends`]) for whatever delivers it into the
//! manager's conversation, and [`Supervisor::join_launched`] waits for them at shutdown. Nothing
//! else changes: `begin`/`prepare` are the same code path, so the semaphore, duplicate
//! prevention, AMP's dispatch, the worker's claim, the ledger and the return of every ticket that
//! doesn't close still run — only *who awaits* the run differs. Foreground stays the default, for
//! headless `run -p` and the tests.

use std::{
    collections::{BTreeMap, BTreeSet, HashMap, VecDeque},
    path::{Path, PathBuf},
    str::FromStr,
    sync::{
        Arc, Mutex, PoisonError,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::{Duration, Instant, SystemTime},
};

use rpi_agent::{
    AgentEmitter, AgentError as ToolError, AgentEvent, AgentTool, AgentToolResult, BeforeToolCall,
    ToolResultPartial,
};
use rpi_ai::types::Tool;
use rpi_harness::{
    agent_harness::{AgentLane, HarnessRunOutcome},
    types::HarnessTool,
};
use serde::Serialize;
use serde_json::{Value, json};
use tokio::{sync::Semaphore, task::JoinHandle};
use tokio_util::sync::CancellationToken;

use crate::{
    agent::{AgentSpec, Runtime, SessionKind},
    dispatch_ledger::{DispatchRecord, Ledger},
    events::{RunEvent, Totals, assistant_text},
    policy::DISPATCH_TOOL,
    project::ProjectMarker,
    roles::Role,
    tools::call_json,
    worker_session::{self, WorkerFeed},
    workspace::{self, ENV_ISOLATION, Isolation, Placement, Repo, WORK_DIR},
};

/// The AMP tools that close a ticket. A worker that ends without one of them succeeding on its
/// ticket ended abnormally (D8).
pub const COMPLETE_TOOL: &str = "amp_complete_task";
pub const BLOCK_TOOL: &str = "amp_block_task";
/// The tool a worker starts its ticket with (AMP #2801, #2802).
pub const CLAIM_TOOL: &str = "amp_claim_task";
/// The tool that hands a dispatched ticket back to ready (AMP #2801); refused once it is started.
pub const RETURN_TOOL: &str = "amp_return_task";
/// The manager's escape hatch back to ready, used when a ticket cannot be returned (it was
/// started, so `amp_return_task` refuses it).
pub const SET_STATE_TOOL: &str = "amp_set_task_state";
/// The AMP state "ready" is stored as (AMP keeps it blocked while a dependency is open).
pub const READY_STATE: &str = "backlog";
/// How long the harness's own claim may take before the run gives up (one AMP call, made before
/// the worker's first turn).
pub const CLAIM_TIMEOUT: Duration = Duration::from_secs(30);
/// How long handing tickets back at startup may take.
pub const ORPHAN_RELEASE_TIMEOUT: Duration = Duration::from_secs(20);
/// `config.toml` table with the supervisor's settings.
pub const CONFIG_TABLE: &str = "workers";

/// Limits for workers. [`SupervisorConfig::load`] reads them from config.toml `[workers]` and the
/// environment.
#[derive(Debug, Clone, PartialEq)]
pub struct SupervisorConfig {
    /// Workers running at once (`max_parallel`, `AMP_HARNESS_MAX_WORKERS`). Default 4.
    pub max_parallel: usize,
    /// Wall-clock limit per worker (`timeout_minutes`, `AMP_HARNESS_WORKER_TIMEOUT_SECS`). Default
    /// 60 minutes.
    pub timeout: Duration,
    /// Model turns per worker before it is stopped (`max_turns`, `AMP_HARNESS_WORKER_MAX_TURNS`);
    /// 0 = no limit. Default 400. rpi's own turn budget is only settable on `create_child`, so the
    /// supervisor counts turns itself.
    pub max_turns: u32,
    /// How long an aborted worker gets to wind down before it is dropped.
    pub abort_grace: Duration,
    /// Where workers that edit files work (`isolation`, `AMP_HARNESS_WORKER_ISOLATION`, #2364).
    /// Default `auto`: a git worktree in a usable repository, else a folder per ticket.
    pub isolation: Isolation,
    /// Whether `amp_run_ticket` waits for its worker (`dispatch`, `AMP_HARNESS_WORKER_DISPATCH`,
    /// AMP #2834). Default `await`: the worker runs in the foreground of the tool call, which is
    /// what headless `run -p` and every existing test rely on. `launch` returns an ack at once and
    /// runs the worker in a task the supervisor owns; the chat opts in (see
    /// [`Supervisor::launch`] and [`Supervisor::take_launch_ends`]).
    pub dispatch: DispatchMode,
}

/// Whether `amp_run_ticket` waits for its worker (AMP #2834).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum DispatchMode {
    /// The tool call awaits the worker and returns its [`WorkerReport`] (the default).
    #[default]
    Await,
    /// The tool call returns a [`LaunchAck`] and the worker runs on, in a supervisor-owned task.
    Launch,
}

impl DispatchMode {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Await => "await",
            Self::Launch => "launch",
        }
    }
}

impl FromStr for DispatchMode {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s.trim().to_ascii_lowercase().as_str() {
            "await" | "foreground" | "wait" => Ok(Self::Await),
            "launch" | "background" => Ok(Self::Launch),
            other => Err(format!("expected `await` or `launch`, not `{other}`")),
        }
    }
}

impl Default for SupervisorConfig {
    fn default() -> Self {
        Self {
            max_parallel: 4,
            timeout: Duration::from_secs(60 * 60),
            max_turns: 400,
            abort_grace: Duration::from_secs(10),
            isolation: Isolation::Auto,
            dispatch: DispatchMode::Await,
        }
    }
}

pub const ENV_MAX_WORKERS: &str = "AMP_HARNESS_MAX_WORKERS";
pub const ENV_WORKER_TIMEOUT_SECS: &str = "AMP_HARNESS_WORKER_TIMEOUT_SECS";
pub const ENV_WORKER_MAX_TURNS: &str = "AMP_HARNESS_WORKER_MAX_TURNS";
/// `launch` runs `amp_run_ticket`'s worker in the background (AMP #2834).
pub const ENV_WORKER_DISPATCH: &str = "AMP_HARNESS_WORKER_DISPATCH";

#[derive(Debug, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct WorkersTable {
    max_parallel: Option<usize>,
    timeout_minutes: Option<f64>,
    max_turns: Option<u32>,
    isolation: Option<String>,
    dispatch: Option<String>,
}

#[derive(Debug, Default, serde::Deserialize)]
struct ConfigFile {
    #[serde(default)]
    workers: Option<WorkersTable>,
}

impl SupervisorConfig {
    /// Defaults, then config.toml `[workers]` (at `config_path`, else the default location), then
    /// the environment. A bad value is an error naming where it came from.
    pub fn load(
        env: &dyn Fn(&str) -> Option<String>,
        config_path: Option<PathBuf>,
    ) -> Result<Self, String> {
        let mut cfg = Self::default();
        if let Some(path) = config_path.or_else(|| amp_mcp::default_config_path(env))
            && let Ok(text) = std::fs::read_to_string(&path)
        {
            cfg.apply_toml(&text)
                .map_err(|e| format!("{} [{CONFIG_TABLE}]: {e}", path.display()))?;
        }
        cfg.apply_env(env)?;
        Ok(cfg)
    }

    fn apply_toml(&mut self, text: &str) -> Result<(), String> {
        let file: ConfigFile = toml::from_str(text).map_err(|e| e.to_string())?;
        let Some(w) = file.workers else {
            return Ok(());
        };
        if let Some(n) = w.max_parallel {
            self.max_parallel = positive(n, "max_parallel")?;
        }
        if let Some(m) = w.timeout_minutes {
            if !(m.is_finite() && m > 0.0) {
                return Err(format!("timeout_minutes must be > 0, not {m}"));
            }
            self.timeout = Duration::from_secs_f64(m * 60.0);
        }
        if let Some(t) = w.max_turns {
            self.max_turns = t;
        }
        if let Some(i) = w.isolation {
            self.isolation = i.parse()?;
        }
        if let Some(d) = w.dispatch {
            self.dispatch = d.parse().map_err(|e| format!("dispatch: {e}"))?;
        }
        Ok(())
    }

    fn apply_env(&mut self, env: &dyn Fn(&str) -> Option<String>) -> Result<(), String> {
        let get = |k: &str| env(k).filter(|v| !v.trim().is_empty());
        let num = |k: &str, v: String| -> Result<u64, String> {
            v.trim()
                .parse::<u64>()
                .map_err(|_| format!("{k}={v}: expected a whole number"))
        };
        if let Some(v) = get(ENV_MAX_WORKERS) {
            self.max_parallel = positive(num(ENV_MAX_WORKERS, v)? as usize, ENV_MAX_WORKERS)?;
        }
        if let Some(v) = get(ENV_WORKER_TIMEOUT_SECS) {
            let secs = num(ENV_WORKER_TIMEOUT_SECS, v)?;
            self.timeout =
                Duration::from_secs(positive(secs as usize, ENV_WORKER_TIMEOUT_SECS)? as u64);
        }
        if let Some(v) = get(ENV_WORKER_MAX_TURNS) {
            self.max_turns = num(ENV_WORKER_MAX_TURNS, v)? as u32;
        }
        if let Some(v) = get(ENV_ISOLATION) {
            self.isolation = v.parse().map_err(|e| format!("{ENV_ISOLATION}: {e}"))?;
        }
        if let Some(v) = get(ENV_WORKER_DISPATCH) {
            self.dispatch = v
                .parse()
                .map_err(|e| format!("{ENV_WORKER_DISPATCH}: {e}"))?;
        }
        Ok(())
    }
}

fn positive(n: usize, what: &str) -> Result<usize, String> {
    if n == 0 {
        Err(format!("{what} must be at least 1"))
    } else {
        Ok(n)
    }
}

/// Where the manager runs: workers' worktrees go inside this repo.
#[derive(Debug, Clone)]
pub struct ManagerContext {
    /// The manager's role id.
    pub role: String,
    /// The manager's working directory (inside the repo it manages).
    pub cwd: PathBuf,
    /// The manager's `.amp.json`, passed on to workers.
    pub project: Option<ProjectMarker>,
}

/// One `amp_run_ticket` call.
#[derive(Debug, Clone, PartialEq, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RunRequest {
    pub task_id: i64,
    pub agent_id: String,
    /// Extra instructions from the manager, appended to the worker's prompt.
    #[serde(default)]
    pub instructions: Option<String>,
    /// Force a worktree on (`true`) or off (`false`). Default: when the role can edit files.
    #[serde(default)]
    pub worktree: Option<bool>,
    /// The worktree's branch. Default `amp-<task_id>`.
    #[serde(default)]
    pub branch: Option<String>,
    /// Where a new branch starts. Default `HEAD` of the manager's repo.
    #[serde(default)]
    pub base: Option<String>,
}

/// How a worker's run ended. M2-6 reports a run for each; every outcome but `Completed` and
/// `Blocked` is an abnormal end (D8: "abandoned: <reason>").
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerOutcome {
    /// The worker completed its ticket (`amp_complete_task` succeeded).
    Completed,
    /// The worker blocked its ticket with a question (`amp_block_task` succeeded).
    Blocked,
    /// The worker stopped on its own without completing or blocking its ticket, or the repeat
    /// guard stopped its model (#2225; `error` then says so).
    Stopped,
    /// The worker could not be set up (worktree, model key, ...) or its run failed (model error).
    Error,
    /// The wall-clock timeout ran out.
    Timeout,
    /// The manager's run was aborted, or every worker was cancelled.
    Cancelled,
    /// The turn budget ran out.
    TurnBudget,
}

impl WorkerOutcome {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Completed => "completed",
            Self::Blocked => "blocked",
            Self::Stopped => "stopped",
            Self::Error => "error",
            Self::Timeout => "timeout",
            Self::Cancelled => "cancelled",
            Self::TurnBudget => "turn_budget",
        }
    }

    /// The worker closed its ticket itself.
    pub fn closed_ticket(self) -> bool {
        matches!(self, Self::Completed | Self::Blocked)
    }
}

/// What `amp_dispatch_task` answered (AMP #2801's result, read for what the harness needs).
/// Every field is optional: an older AMP returns only `task_id` and `dispatched_to`.
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Deserialize)]
pub struct DispatchResult {
    #[serde(default)]
    pub task_id: i64,
    /// The agent AMP recorded (AMP's own answer, not what we asked for).
    #[serde(default)]
    pub agent_id: Option<String>,
    #[serde(default)]
    pub dispatched_to: Option<String>,
    /// The token the worker presents to start and to close the ticket; empty on an old server.
    #[serde(default)]
    pub dispatch_id: String,
    /// `backlog`, `in_progress`, ... as AMP recorded it.
    #[serde(default)]
    pub state: Option<String>,
    /// `not_started` | `started`.
    #[serde(default)]
    pub phase: Option<String>,
    /// When AMP returns it if nobody starts it (RFC3339), when the watchdog is on.
    #[serde(default)]
    pub start_by: Option<String>,
    /// A ready-made prompt for the worker subagent, when the server sends one.
    #[serde(default)]
    pub worker_prompt: Option<String>,
    /// What the dispatching model is told to do next (AMP #2801).
    #[serde(default)]
    pub next: Option<String>,
    /// The project's flag timeout in minutes, omitted when 0 (off).
    #[serde(default)]
    pub flag_after_minutes: Option<i64>,
}

impl DispatchResult {
    /// The agent AMP says this dispatch is for.
    pub fn agent(&self) -> Option<&str> {
        self.agent_id
            .as_deref()
            .or(self.dispatched_to.as_deref())
            .filter(|a| !a.is_empty())
    }
}

/// What [`Supervisor::release_orphans`] did with one ticket a dead process left dispatched.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OrphanRelease {
    pub record: DispatchRecord,
    /// `Ok`: AMP has the ticket back in ready. `Err`: AMP refused, and why.
    pub result: Result<(), String>,
}

impl OrphanRelease {
    /// What the person is told, in one line.
    pub fn notice(&self) -> String {
        orphan_notice(&self.record, &self.result)
    }
}

/// The reason an orphaned ticket is handed back with: it names the ticket, the agent and what
/// happened, so the history reads as the incident it was.
pub fn orphan_reason(rec: &DispatchRecord) -> String {
    format!(
        "the chat that dispatched it to {} is gone (harness process {} ended with it in progress)",
        rec.agent_id, rec.pid
    )
}

/// The line the person at the chat is shown for an orphaned ticket, whatever the outcome.
pub fn orphan_notice(rec: &DispatchRecord, result: &Result<(), String>) -> String {
    match result {
        Ok(()) => format!(
            "#{0} was returned to ready: the chat that dispatched it to {1} ended before its worker \
             finished. Re-dispatch it, or ask your user.",
            rec.task_id, rec.agent_id
        ),
        Err(e) => format!(
            "#{0} was dispatched to {1} by a chat that ended before its worker finished, and AMP \
             would not take it back: {2}. It may still be in progress; check the ticket.",
            rec.task_id, rec.agent_id, e
        ),
    }
}

/// Whether AMP's refusal means the record is spent and can be dropped: the ticket isn't in
/// progress any more (completed or blocked by someone since), so there is nothing to hand back.
fn record_is_stale(err: &Result<(), String>) -> bool {
    let Err(e) = err else {
        return false;
    };
    let e = e.to_ascii_lowercase();
    e.contains("not in progress") || e.contains("not found") || e.contains("unknown task")
}

/// What a worker is, known before it starts (hooks get this).
#[derive(Debug, Clone, Serialize)]
pub struct WorkerInfo {
    pub task_id: i64,
    /// AMP's form: `amp:amp-worker-backend`.
    pub agent_id: String,
    /// The role id: `amp-worker-backend`.
    pub role: String,
    /// The dispatch token AMP gave this worker (AMP #2801/#2802): what starts the ticket and what
    /// makes its completion the worker's own rather than a closeout. Empty on a server without
    /// one.
    pub dispatch_id: String,
    /// `provider/model`.
    pub model: String,
    pub cwd: PathBuf,
    pub worktree: Option<PathBuf>,
    pub branch: Option<String>,
    /// Where it works (#2364): its own worktree, its own folder `amp-work/<ticket>/`, or the
    /// manager's folder (`shared`, also for read-only roles).
    pub workspace: Placement,
    /// The manager's folder is in a git repository (usable for worktrees or not).
    pub git: bool,
    /// The role can edit files.
    pub edits: bool,
    /// The manager's folder (the person's files).
    pub manager_cwd: PathBuf,
    /// The worker's live usage totals, updated as each event is emitted (so a before-tool hook on
    /// `amp_complete_task` sees every assistant message before that call).
    #[serde(skip)]
    pub totals: Arc<Mutex<Totals>>,
}

/// How a worker's run ended, returned to the manager as `amp_run_ticket`'s result.
#[derive(Debug, Clone, Serialize)]
pub struct WorkerReport {
    pub task_id: i64,
    pub agent_id: String,
    pub role: String,
    pub outcome: WorkerOutcome,
    /// The dispatch token this run worked under (AMP #2801/#2802); empty on an old server.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub dispatch_id: String,
    /// Whether the ticket was started for this run (the harness claimed it, or AMP's fallback did
    /// on the worker's first write). A worker that ended before that leaves a ticket AMP can still
    /// *return* rather than one that has to be set back by hand.
    #[serde(default)]
    pub started: bool,
    /// What handing the ticket back did, once run reporting tried: `Some(Ok(()))` when AMP has it
    /// back in ready, `Some(Err(why))` when AMP refused. `None` when the worker closed its ticket
    /// (nothing to hand back) or no run reporting is in this process (AMP #2802). The `/workers`
    /// panel shows it, so a person looking at a finished worker sees where its ticket went.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub returned: Option<Result<(), String>>,
    /// The worker's last assistant text.
    pub final_message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    /// `provider/model` (empty when the worker could not be built).
    pub model: String,
    pub cwd: PathBuf,
    pub worktree: Option<PathBuf>,
    pub branch: Option<String>,
    /// `worktree`, `folder` or `shared` (#2364).
    pub workspace: Placement,
    pub session_file: Option<PathBuf>,
    pub turns: u32,
    pub tool_calls: u32,
    pub tool_errors: u32,
    pub usage: crate::events::UsageJson,
    pub duration_ms: u64,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub notes: Vec<String>,
    #[serde(skip)]
    pub totals: Totals,
}

/// What a launched `amp_run_ticket` tells the manager at once (AMP #2834), in place of the
/// [`WorkerReport`] a foreground call would have waited for: the ticket is AMP's, the worker is
/// the supervisor's, and its end will come later through [`Supervisor::take_launch_ends`].
///
/// `Serialize`, because it is the tool result's `details`; the chat's tool row reads the same
/// fields.
#[derive(Debug, Clone, Serialize)]
pub struct LaunchAck {
    pub task_id: i64,
    /// AMP's form: `amp:amp-worker-backend`.
    pub agent_id: String,
    /// The role id.
    pub role: String,
    /// `provider/model`, or empty when the role has no model configured (the worker then fails to
    /// build and reports it, exactly as a foreground run does).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    /// Where it works when it starts (#2364): worktree, folder, or the person's own folder.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub workspace: Option<Placement>,
    /// Workers allowed at once ([`SupervisorConfig::max_parallel`]): what the manager may count on.
    pub limit: usize,
    /// Every slot was taken, so this worker waits its turn before it dispatches in AMP.
    pub queued: bool,
    /// What the manager is told, in one line.
    pub message: String,
}

impl LaunchAck {
    fn new(
        task_id: i64,
        agent_id: String,
        role: String,
        model: String,
        workspace: Option<Placement>,
        limit: usize,
        queued: bool,
    ) -> Self {
        let message = format!(
            "Launched {agent_id} on #{task_id}: it is running in the background{}. You can carry on \
             in this chat; its result arrives as a notice when it ends. Up to {limit} workers run \
             at once; stop it from the /workers panel.",
            if queued {
                " (every worker slot was taken, so it waits its turn)"
            } else {
                ""
            }
        );
        Self {
            task_id,
            agent_id,
            role,
            model,
            workspace,
            limit,
            queued,
            message,
        }
    }
}

/// How a launched worker ended (AMP #2834): the run's report, or the reason it never got as far
/// as a dispatch. Queued by [`Supervisor::launch`], read by [`Supervisor::take_launch_ends`] —
/// the seam completion delivery sits on.
#[derive(Debug, Clone)]
pub struct LaunchEnd {
    pub task_id: i64,
    pub agent_id: String,
    /// `Ok`: the run ended and its [`WorkerHooks::on_finish`] ran (the ticket was closed, or
    /// returned). `Err`: nothing was dispatched — the slot was cancelled before it came up, or
    /// AMP refused the dispatch — and nothing in AMP needs undoing.
    pub result: Result<WorkerReport, String>,
}

/// The queue of launched ends (AMP #2834, #2835): one producer — [`Supervisor::record_launch_end`],
/// fed by [`Supervisor::launch`] — and two ways to consume it. Draining is what makes a summary
/// arrive exactly once: an end leaves the queue on the call that returns it, so a later drain or
/// wait never sees it again, whether it was a success or a failure.
///
/// Watching ([`LaunchEnds::watch`], AMP #2861) is separate from consuming: every pushed end is
/// also broadcast to the watchers, so the chat can show it the moment it lands without taking it
/// from the queue the next manager turn reads.
#[derive(Debug)]
pub struct LaunchEnds {
    queue: Mutex<VecDeque<LaunchEnd>>,
    notify: tokio::sync::Notify,
    watchers: tokio::sync::broadcast::Sender<LaunchEnd>,
}

/// How many ends a watcher can fall behind before it misses some (it is told how many).
const LAUNCH_END_WATCH_CAPACITY: usize = 64;

impl Default for LaunchEnds {
    fn default() -> Self {
        Self {
            queue: Mutex::default(),
            notify: tokio::sync::Notify::new(),
            watchers: tokio::sync::broadcast::channel(LAUNCH_END_WATCH_CAPACITY).0,
        }
    }
}

impl LaunchEnds {
    /// Queues one end, wakes whoever is waiting for it, and shows it to the watchers.
    pub fn push(&self, end: LaunchEnd) {
        // No watchers is fine: the queue is the delivery, the broadcast is only for display.
        let _ = self.watchers.send(end.clone());
        self.queue
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .push_back(end);
        self.notify.notify_waiters();
    }

    /// A copy of every end pushed from now on, for display (AMP #2861). Receiving from it never
    /// touches the queue, so [`LaunchEnds::drain`] still returns each end exactly once.
    pub fn watch(&self) -> tokio::sync::broadcast::Receiver<LaunchEnd> {
        self.watchers.subscribe()
    }

    /// Every queued end, oldest first, leaving the queue empty.
    pub fn drain(&self) -> Vec<LaunchEnd> {
        let mut queue = self.queue.lock().unwrap_or_else(PoisonError::into_inner);
        queue.drain(..).collect()
    }

    /// The oldest end, or waits for the next one. Returns at once when one is already queued.
    pub async fn next(&self) -> LaunchEnd {
        loop {
            let notified = self.notify.notified();
            tokio::pin!(notified);
            // Subscribe before looking, so an end recorded in between is not missed.
            notified.as_mut().enable();
            if let Some(end) = self
                .queue
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .pop_front()
            {
                return end;
            }
            notified.await;
        }
    }

    /// How many ends are waiting; what a caller checks instead of draining to look.
    pub fn len(&self) -> usize {
        self.queue
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

impl LaunchEnd {
    /// Whether this end is one the person has to hear about: a run that closed its ticket says
    /// nothing new, one that ended any other way carries its own notes.
    pub fn needs_attention(&self) -> bool {
        match &self.result {
            Ok(report) => !report.outcome.closed_ticket(),
            Err(_) => false,
        }
    }

    /// The one line completion delivery shows: `■ #123 completed by amp:amp-worker-backend`.
    pub fn line(&self) -> String {
        match &self.result {
            Ok(report) => {
                let mut line = format!(
                    "#{} {} by {}",
                    self.task_id,
                    report.outcome.as_str(),
                    self.agent_id
                );
                if let Some(err) = report.error.as_deref().filter(|e| !e.trim().is_empty()) {
                    line.push_str(&format!(": {err}"));
                }
                line
            }
            Err(why) => format!(
                "#{} was not dispatched to {}: {why}",
                self.task_id, self.agent_id
            ),
        }
    }
}

/// The supervisor's extension points; run reporting ([`crate::run_report::RunReporter`], M2-6)
/// plugs in here.
#[async_trait::async_trait]
pub trait WorkerHooks: Send + Sync {
    /// A before-tool hook for this worker. It runs after the role's policy allowed a call
    /// ([`AgentSpec::before_tool_call`]); run reporting fills run fields into
    /// `amp_complete_task` / `amp_block_task` here.
    fn before_tool_call(&self, _worker: &WorkerInfo) -> Option<BeforeToolCall> {
        None
    }

    /// Extra tools for this worker (run reporting: `amp_session_usage`). The role's policy
    /// filters them like any other tool.
    fn extra_tools(&self, _worker: &WorkerInfo) -> Vec<HarnessTool> {
        Vec::new()
    }

    /// Every dispatched worker ends here exactly once, whatever the outcome, before the report
    /// goes back to the manager. Run reporting calls `amp_report_run` here when
    /// `!report.outcome.closed_ticket()` and adds a note to the report.
    async fn on_finish(&self, _worker: &WorkerInfo, _report: &mut WorkerReport) {}

    /// Hands ticket `task_id` back after this process abandoned its worker (#2204): AMP moves it
    /// from in progress to ready, so it isn't left in progress with nobody on it. `why` is one
    /// line for the ticket's history. Run reporting calls it from `on_finish` for a `cancelled`
    /// worker (the chat stopped or quit), and [`Supervisor::release_unfinished`] calls it when
    /// the chat quits without waiting for `on_finish`.
    async fn release(&self, _task_id: i64, _why: &str) -> Result<(), String> {
        Err("no run reporting in this process".to_owned())
    }

    /// Tells the person at the manager's chat something the harness noticed about a ticket, above
    /// the editor (`⚠ #123 was returned: worker amp-x timed out after 20 min`). The default does
    /// nothing; a process with a chat prints it. AMP #2802: a returned ticket whose manager was
    /// never told is the bug this whole ticket exists to fix.
    fn notice(&self, _text: &str) {}
}

/// Why `amp_run_ticket` refused or failed before a worker was dispatched.
#[derive(Debug, thiserror::Error)]
pub enum RunTicketError {
    #[error("unknown agent `{agent}`. Agents you can run: {available}")]
    UnknownAgent { agent: String, available: String },
    #[error("`{agent}` can't be dispatched by {manager}. Agents you can run: {available}")]
    NotDispatchable {
        agent: String,
        manager: String,
        available: String,
    },
    #[error("ticket #{0} already has a worker running in this session")]
    AlreadyRunning(i64),
    #[error("AMP refused to dispatch ticket #{task_id}: {message}")]
    Dispatch { task_id: i64, message: String },
    #[error("cancelled before the worker started")]
    Cancelled,
    #[error("bad arguments: {0}")]
    BadArguments(String),
    /// `[workers] isolation = "worktree"` in a folder without a usable git repository (#2364).
    #[error("{0}; nothing was dispatched")]
    Workspace(String),
    /// The person stopped the worker from the chat while it waited for a slot (#2252).
    #[error(
        "{by} stopped the worker on #{task_id} from the chat before it started; nothing was dispatched. Ask them before running it again"
    )]
    Stopped { task_id: i64, by: String },
}

/// Why [`Supervisor::stop_worker`] did nothing.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum StopError {
    #[error("#{0} has no worker in this chat")]
    NotFound(i64),
    #[error("the worker on #{0} has already finished")]
    Finished(i64),
    #[error("the worker on #{0} is already stopping")]
    AlreadyStopping(i64),
}

/// A worker's session as the chat lists it (#2252): live (running or queued) or finished in this
/// process.
#[derive(Debug, Clone)]
pub struct WorkerSessionInfo {
    pub task_id: i64,
    pub agent_id: String,
    /// `Some` while the worker is live; `None` once it has finished.
    pub phase: Option<WorkerPhase>,
    pub model: Option<String>,
    pub cwd: Option<PathBuf>,
    /// When it started running (or was queued).
    pub since: SystemTime,
    /// When it finished.
    pub ended: Option<SystemTime>,
    pub totals: Totals,
    /// How it ended (finished workers).
    pub outcome: Option<WorkerOutcome>,
    pub error: Option<String>,
    /// Who stopped it from the chat, once someone has asked it to stop.
    pub stopped_by: Option<String>,
    /// What handing its ticket back did (`None`: it closed the ticket, or nothing was reported).
    /// The `/workers` panel says so under the finished worker (AMP #2802).
    pub returned: Option<Result<(), String>>,
    pub session_file: Option<PathBuf>,
    /// The worktree's branch, once the worker has one (#2347).
    pub branch: Option<String>,
    /// The worker model's context window in tokens, when known (#2347: the chat's `ctx N%`).
    pub context_window: Option<u64>,
}

impl WorkerSessionInfo {
    pub fn is_live(&self) -> bool {
        self.phase.is_some()
    }
}

/// A worker that finished in this process: its feed stays viewable.
struct FinishedWorker {
    info: WorkerSessionInfo,
    feed: Arc<WorkerFeed>,
}

/// What a stop from the chat says (#2252): the worker's error, the abandoned run's note
/// (`abandoned: cancelled: stopped by ... from the chat`) and the reason the ticket is handed back.
pub fn stopped_from_chat(by: &str) -> String {
    format!("stopped by {by} from the chat")
}

/// A worker's phase, for a `/workers` view.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerPhase {
    /// Waiting for a free slot.
    Queued,
    Running,
}

/// A live worker, for a `/workers` view.
#[derive(Debug, Clone, Serialize)]
pub struct WorkerStatus {
    pub task_id: i64,
    pub agent_id: String,
    pub phase: WorkerPhase,
    pub model: Option<String>,
    pub cwd: Option<PathBuf>,
    pub since: SystemTime,
    pub totals: Totals,
}

struct LiveWorker {
    agent_id: String,
    /// AMP has the dispatch (the ticket is in progress).
    dispatched: bool,
    /// Whether the worker has closed its ticket yet (set once it is running).
    watch: Option<Arc<WorkerWatch>>,
    phase: WorkerPhase,
    model: Option<String>,
    cwd: Option<PathBuf>,
    since: SystemTime,
    totals: Option<Arc<Mutex<Totals>>>,
    /// Every event the worker emitted, for the chat's session view (#2252).
    feed: Arc<WorkerFeed>,
    /// Cancelled by [`Supervisor::stop_worker`]; `stopped_by` says who.
    stop: CancellationToken,
    stopped_by: Arc<Mutex<Option<String>>>,
    branch: Option<String>,
    context_window: Option<u64>,
}

/// Runs workers for one manager. Share it as `Arc<Supervisor>`; [`Supervisor::tool`] is the
/// manager's `amp_run_ticket`.
pub struct Supervisor {
    rt: Arc<Runtime>,
    manager: ManagerContext,
    /// The manager's `.amp.json` now: [`ManagerContext::project`] until the chat's folder is
    /// linked to another project ([`Supervisor::set_project`], #2383).
    project: Mutex<Option<ProjectMarker>>,
    config: SupervisorConfig,
    slots: Arc<Semaphore>,
    cancel: CancellationToken,
    hooks: Option<Arc<dyn WorkerHooks>>,
    live: Mutex<BTreeMap<i64, LiveWorker>>,
    /// Workers that finished in this process, by ticket (the latest run of each).
    finished: Mutex<BTreeMap<i64, FinishedWorker>>,
    running: AtomicUsize,
    peak: AtomicUsize,
    /// Serializes `git worktree` changes (git's own lock fails rather than waits).
    git_lock: tokio::sync::Mutex<()>,
    /// Held by a worker that edits files in the manager's own folder under `isolation =
    /// "shared"`, so two of them never write there at once (#2364).
    writer: Arc<tokio::sync::Mutex<()>>,
    /// The tickets this machine dispatched (AMP #2802): what the next startup hands back if this
    /// process dies with one in progress.
    ledger: Arc<Ledger>,
    /// The tasks of the workers launched in the background (AMP #2834), kept so
    /// [`Supervisor::join_launched`] can wait for them at shutdown.
    launched: Mutex<Vec<JoinHandle<()>>>,
    /// The ends of those launches, for completion delivery to drain.
    launch_ends: LaunchEnds,
}

impl std::fmt::Debug for Supervisor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Supervisor")
            .field("manager", &self.manager.role)
            .field("config", &self.config)
            .finish_non_exhaustive()
    }
}

impl Supervisor {
    pub fn new(rt: Arc<Runtime>, manager: ManagerContext, config: SupervisorConfig) -> Arc<Self> {
        Self::with_hooks(rt, manager, config, None)
    }

    pub fn with_hooks(
        rt: Arc<Runtime>,
        manager: ManagerContext,
        config: SupervisorConfig,
        hooks: Option<Arc<dyn WorkerHooks>>,
    ) -> Arc<Self> {
        let ledger = Arc::new(Ledger::open(&rt.state_dir));
        Self::with_hooks_and_ledger(rt, manager, config, hooks, ledger)
    }

    /// [`Supervisor::with_hooks`] on a ledger the caller made (one per process, so every chat of
    /// it shares the liveness lock and the records).
    pub fn with_hooks_and_ledger(
        rt: Arc<Runtime>,
        manager: ManagerContext,
        config: SupervisorConfig,
        hooks: Option<Arc<dyn WorkerHooks>>,
        ledger: Arc<Ledger>,
    ) -> Arc<Self> {
        Arc::new(Self {
            slots: Arc::new(Semaphore::new(config.max_parallel.max(1))),
            rt,
            project: Mutex::new(manager.project.clone()),
            manager,
            config,
            cancel: CancellationToken::new(),
            hooks,
            live: Mutex::new(BTreeMap::new()),
            finished: Mutex::new(BTreeMap::new()),
            running: AtomicUsize::new(0),
            peak: AtomicUsize::new(0),
            git_lock: tokio::sync::Mutex::new(()),
            writer: Arc::new(tokio::sync::Mutex::new(())),
            ledger,
            launched: Mutex::new(Vec::new()),
            launch_ends: LaunchEnds::default(),
        })
    }

    /// The dispatch ledger this supervisor shares.
    pub fn ledger(&self) -> &Arc<Ledger> {
        &self.ledger
    }

    pub fn config(&self) -> &SupervisorConfig {
        &self.config
    }

    /// The project marker workers get from now on (#2383: the manager wrote `.amp.json` in the
    /// chat). Running workers keep the one they were started with.
    pub fn set_project(&self, project: Option<ProjectMarker>) {
        *self.project.lock().unwrap_or_else(PoisonError::into_inner) = project;
    }

    /// The project marker new workers get.
    pub fn project(&self) -> Option<ProjectMarker> {
        self.project
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()
    }

    /// The manager's `amp_run_ticket` tool, for [`AgentSpec::extra_tools`].
    pub fn tool(self: &Arc<Self>) -> HarnessTool {
        HarnessTool::new(Arc::new(RunTicketTool::new(Arc::clone(self))))
    }

    /// Cancels every queued and running worker (the TUI's Ctrl-C, or shutdown).
    pub fn cancel_all(&self) {
        self.cancel.cancel();
    }

    /// Starts ticket `task_id` in AMP on `agent_id`'s behalf with `dispatch_id`, the token
    /// `amp_dispatch_task` gave it (AMP #2802). This is the option the ticket asks for: the
    /// harness claims for its worker, right before the worker's first turn, rather than trusting
    /// the model to make `amp_claim_task` its first call.
    ///
    /// Why here: the harness controls this point exactly (the worker exists, its worktree and
    /// harness are built, it is about to prompt), a worker that never gets this far is still
    /// *returnable* by AMP (`amp_return_task` only refuses a started ticket), and the claim is
    /// idempotent (`already_started`), so nothing breaks if the worker also calls it.
    async fn claim_for_worker(
        &self,
        task_id: i64,
        dispatch_id: &str,
        agent_id: &str,
    ) -> Result<(), String> {
        let Value::Object(args) = json!({"task_id": task_id, "dispatch_id": dispatch_id}) else {
            unreachable!()
        };
        let out = self
            .rt
            .mcp
            .call_tool_with_timeout(CLAIM_TOOL, Some(args), CLAIM_TIMEOUT)
            .await
            .map_err(|e| e.to_string())?;
        if out.is_error == Some(true) {
            return Err(crate::tools::result_text(&out).trim().to_owned());
        }
        tracing::debug!(ticket = task_id, agent = agent_id, "started the ticket");
        Ok(())
    }

    /// Live workers, by ticket.
    pub fn workers(&self) -> Vec<WorkerStatus> {
        let live = self.live.lock().unwrap();
        live.iter()
            .map(|(id, w)| WorkerStatus {
                task_id: *id,
                agent_id: w.agent_id.clone(),
                phase: w.phase,
                model: w.model.clone(),
                cwd: w.cwd.clone(),
                since: w.since,
                totals: w
                    .totals
                    .as_ref()
                    .map(|t| t.lock().unwrap().clone())
                    .unwrap_or_default(),
            })
            .collect()
    }

    /// Tickets AMP has as dispatched to a live worker of this process that hasn't closed its
    /// ticket (completed or blocked) yet: what quitting now would leave in progress.
    pub fn unfinished(&self) -> Vec<i64> {
        let live = self.live.lock().unwrap();
        live.iter()
            .filter(|(_, w)| {
                w.dispatched
                    && w.watch
                        .as_ref()
                        .is_none_or(|watch| watch.closed.lock().unwrap().is_none())
            })
            .map(|(id, _)| *id)
            .collect()
    }

    /// Returns every [`Supervisor::unfinished`] ticket to ready through the hooks
    /// ([`WorkerHooks::release`]), all at once, within `bound`. Quitting without waiting calls
    /// it, because the workers' own `on_finish` will not get to run (#2204). Each ticket comes
    /// back with its result; one still going when `bound` passes says so.
    pub async fn release_unfinished(
        &self,
        why: &str,
        bound: Duration,
    ) -> Vec<(i64, Result<(), String>)> {
        let ids = self.unfinished();
        let Some(hooks) = self.hooks.clone() else {
            return ids
                .into_iter()
                .map(|id| (id, Err("no run reporting in this process".to_owned())))
                .collect();
        };
        let mut set = tokio::task::JoinSet::new();
        for id in &ids {
            let (hooks, id, why) = (Arc::clone(&hooks), *id, why.to_owned());
            set.spawn(async move { (id, hooks.release(id, &why).await) });
        }
        let mut out = Vec::new();
        let _ = tokio::time::timeout(bound, async {
            while let Some(done) = set.join_next().await {
                if let Ok(r) = done {
                    out.push(r);
                }
            }
        })
        .await;
        for id in ids {
            if !out.iter().any(|(d, _)| *d == id) {
                out.push((
                    id,
                    Err(format!(
                        "AMP did not answer within {}",
                        human_duration(bound)
                    )),
                ));
            }
        }
        out.sort_by_key(|(id, _)| *id);
        out
    }

    /// Releases the tickets this machine dispatched whose harness process is gone (AMP #2802):
    /// the chat crashed, was killed, or the machine rebooted between a dispatch and its worker
    /// closing the ticket. Called once at startup, before any worker runs.
    ///
    /// Each orphan is handed back with the reason naming what happened, and every outcome is told
    /// to the person through [`WorkerHooks::notice`] — a ticket silently reset is almost as bad
    /// as one stuck in progress. A record whose ticket AMP won't take back (already completed,
    /// someone else's run since) is dropped only when AMP says it isn't in progress any more;
    /// otherwise it is kept, so the next startup tries again.
    pub async fn release_orphans(&self) -> Vec<OrphanRelease> {
        let orphans = self.ledger.orphans();
        if orphans.is_empty() {
            return Vec::new();
        }
        let mut out = Vec::new();
        for rec in orphans {
            let why = orphan_reason(&rec);
            let result = match self.hooks.clone() {
                Some(hooks) => hooks.release(rec.task_id, &why).await,
                None => Err("no run reporting in this process".to_owned()),
            };
            if result.is_ok() || record_is_stale(&result) {
                self.ledger.clear(rec.task_id);
            }
            if let Some(hooks) = &self.hooks {
                hooks.notice(&orphan_notice(&rec, &result));
            }
            out.push(OrphanRelease {
                record: rec,
                result,
            });
        }
        out
    }

    /// Releases the tickets this machine dispatched whose harness process is gone, within `bound`.
    /// `release_orphans` with a deadline, for a startup that must not hang on AMP.
    pub async fn release_orphans_within(&self, bound: Duration) -> Vec<OrphanRelease> {
        tokio::time::timeout(bound, self.release_orphans())
            .await
            .unwrap_or_default()
    }

    /// The most workers that ran at the same time.
    pub fn peak_running(&self) -> usize {
        self.peak.load(Ordering::SeqCst)
    }

    /// Every worker session of this chat: the live ones (by ticket), then the finished ones
    /// (by ticket) that have no live run (#2252).
    pub fn sessions(&self) -> Vec<WorkerSessionInfo> {
        let mut out: Vec<WorkerSessionInfo> = {
            let live = self.live.lock().unwrap();
            live.iter().map(|(id, w)| live_info(*id, w)).collect()
        };
        let finished = self.finished.lock().unwrap();
        for (id, f) in finished.iter() {
            if !out.iter().any(|s| s.task_id == *id) {
                out.push(f.info.clone());
            }
        }
        out
    }

    /// One worker session of this chat, live or finished.
    pub fn session(&self, task_id: i64) -> Option<WorkerSessionInfo> {
        if let Some(w) = self.live.lock().unwrap().get(&task_id) {
            return Some(live_info(task_id, w));
        }
        self.finished
            .lock()
            .unwrap()
            .get(&task_id)
            .map(|f| f.info.clone())
    }

    /// The events of ticket `task_id`'s worker in this chat (live, or the last finished run).
    pub fn feed(&self, task_id: i64) -> Option<Arc<WorkerFeed>> {
        if let Some(w) = self.live.lock().unwrap().get(&task_id) {
            return Some(Arc::clone(&w.feed));
        }
        self.finished
            .lock()
            .unwrap()
            .get(&task_id)
            .map(|f| Arc::clone(&f.feed))
    }

    /// The newest session file a worker on `task_id` wrote, in this process or an earlier one.
    pub fn latest_session_file(&self, task_id: i64) -> Option<PathBuf> {
        worker_session::latest_session(&self.rt.state_dir, task_id)
    }

    /// Stops the live worker on `task_id` because `by` (the person at the chat) asked to
    /// (#2252). Its run ends as `cancelled` with the error [`stopped_from_chat`]: run reporting
    /// records it abandoned and hands the ticket back to ready (#2204), and the manager's
    /// `amp_run_ticket` returns with a note saying who stopped it. A worker still waiting for a
    /// slot is never dispatched.
    pub fn stop_worker(&self, task_id: i64, by: &str) -> Result<(), StopError> {
        let live = self.live.lock().unwrap();
        let Some(w) = live.get(&task_id) else {
            return Err(if self.finished.lock().unwrap().contains_key(&task_id) {
                StopError::Finished(task_id)
            } else {
                StopError::NotFound(task_id)
            });
        };
        if w.stop.is_cancelled() {
            return Err(StopError::AlreadyStopping(task_id));
        }
        let by = by.trim();
        *w.stopped_by.lock().unwrap() = Some(if by.is_empty() {
            "the person".to_owned()
        } else {
            by.to_owned()
        });
        w.stop.cancel();
        Ok(())
    }

    /// Keeps a finished worker's session viewable, and forgets it from the live map's point of
    /// view (the live guard removes it right after).
    fn record_finished(&self, report: &WorkerReport) {
        let (feed, since, stopped_by, context_window) = {
            let live = self.live.lock().unwrap();
            match live.get(&report.task_id) {
                Some(w) => (
                    Arc::clone(&w.feed),
                    w.since,
                    w.stopped_by.lock().unwrap().clone(),
                    w.context_window,
                ),
                None => (WorkerFeed::new(), SystemTime::now(), None, None),
            }
        };
        let info = WorkerSessionInfo {
            task_id: report.task_id,
            agent_id: report.agent_id.clone(),
            phase: None,
            model: Some(report.model.clone()).filter(|m| !m.is_empty()),
            cwd: Some(report.cwd.clone()),
            since,
            ended: Some(SystemTime::now()),
            totals: report.totals.clone(),
            outcome: Some(report.outcome),
            error: report.error.clone(),
            stopped_by,
            returned: report.returned.clone(),
            session_file: report.session_file.clone(),
            branch: report.branch.clone(),
            context_window,
        };
        self.finished
            .lock()
            .unwrap()
            .insert(report.task_id, FinishedWorker { info, feed });
    }

    /// The roles this manager may dispatch, from the roster as it is now (a `/reload` may have
    /// changed it, #2242).
    pub fn dispatchable(&self) -> Vec<Role> {
        let catalog = self.rt.catalog();
        match catalog.roles.get(&self.manager.role) {
            Some(m) => catalog.roles.dispatchable(m).into_iter().cloned().collect(),
            None => Vec::new(),
        }
    }

    fn available(&self) -> String {
        let ids: Vec<String> = self
            .dispatchable()
            .iter()
            .map(|r| format!("amp:{}", r.id))
            .collect();
        if ids.is_empty() {
            "none".to_owned()
        } else {
            ids.join(", ")
        }
    }

    /// `amp_run_ticket`'s description: what it does, plus every dispatchable role with its
    /// description.
    pub fn tool_description(&self) -> String {
        let (opening, returns) = match self.config.dispatch {
            // The default: the tool call waits for the worker (headless, and what `run -p` does).
            DispatchMode::Await => (
                "Run a specialist agent on one AMP ticket and wait for it to finish.",
                "returns its final message and outcome",
            ),
            // AMP #2834: the chat. The call returns at once and the worker runs on, so the
            // manager can keep talking while up to `max_parallel` workers run.
            DispatchMode::Launch => (
                "Start a specialist agent on one AMP ticket in the background: it runs while you \
                 carry on with other work in this chat, and its result arrives as a notice when it \
                 ends. Don't wait for it, and don't run the same ticket twice.",
                "returns an acknowledgement at once; the worker's last message and outcome arrive \
                 when it ends",
            ),
        };
        let mut text = format!(
            "{opening} Marks the ticket dispatched in AMP (amp_dispatch_task), gives a worker whose \
             role edits files its own place to work ({}), runs it on its own model, and {returns} \
             (completed, blocked, stopped, error, timeout, cancelled or turn_budget) as JSON. At \
             most {} run at once, and calls beyond that wait for a free slot; each worker has {} of \
             wall-clock time. agent_id is the ticket's assigned_to.\n\nAgents:",
            self.place_description(),
            self.config.max_parallel,
            human_duration(self.config.timeout),
        );
        for role in self.dispatchable() {
            let desc = role
                .description
                .split_whitespace()
                .collect::<Vec<_>>()
                .join(" ");
            text.push_str(&format!("\n- amp:{}: {desc}", role.id));
        }
        text
    }

    /// `amp_run_ticket`'s words for where editing workers work, for this folder now (#2364).
    fn place_description(&self) -> String {
        let repo = Repo::probe(&self.manager.cwd);
        match workspace::placement(self.config.isolation, &repo, &self.manager.cwd, false) {
            Ok(Placement::Worktree) => {
                "a git worktree at .worktrees/<task_id> on branch amp-<task_id>".to_owned()
            }
            Ok(Placement::Folder) => {
                format!("a folder {WORK_DIR}/<task_id>/; no git branch, workers don't commit")
            }
            Ok(Placement::Shared) => {
                "this folder, one editing worker at a time; workers don't commit".to_owned()
            }
            Err(e) => format!("none: {e}"),
        }
    }

    fn resolve_role(&self, agent: &str) -> Result<Role, RunTicketError> {
        let role = self
            .rt
            .role(agent)
            .map_err(|_| RunTicketError::UnknownAgent {
                agent: agent.to_owned(),
                available: self.available(),
            })?;
        if !self.dispatchable().iter().any(|r| r.id == role.id) {
            return Err(RunTicketError::NotDispatchable {
                agent: agent.to_owned(),
                manager: self.manager.role.clone(),
                available: self.available(),
            });
        }
        Ok(role)
    }

    /// Runs one ticket. `Err` means nothing was dispatched; once AMP has the dispatch, every end
    /// is an `Ok(WorkerReport)` (and [`WorkerHooks::on_finish`] has run).
    ///
    /// This is the foreground half of [`Supervisor::launch`]'s pair: both call
    /// [`Supervisor::prepare`] and [`Supervisor::run_prepared`], so a launched worker is the same
    /// run, only awaited by the supervisor's task instead of by the tool call (AMP #2834).
    pub async fn run_ticket(
        self: &Arc<Self>,
        req: RunRequest,
        signal: CancellationToken,
    ) -> Result<WorkerReport, RunTicketError> {
        let prepared = self.prepare(req, signal).await?;
        self.run_prepared(prepared, false).await
    }

    /// Validating everything that can refuse a run, and reserving its place, before it starts
    /// (AMP #2834): the agent, the live-map slot (which is what turns a second call for the same
    /// ticket into [`RunTicketError::AlreadyRunning`]) and where the worker will work. `Err` here
    /// means nothing was dispatched and nothing is left in this supervisor's live map.
    async fn prepare(
        self: &Arc<Self>,
        req: RunRequest,
        signal: CancellationToken,
    ) -> Result<Prepared, RunTicketError> {
        if req.task_id <= 0 {
            return Err(RunTicketError::BadArguments(format!(
                "task_id must be a ticket id, not {}",
                req.task_id
            )));
        }
        let role = self.resolve_role(&req.agent_id)?;
        let agent_id = format!("amp:{}", role.id);
        let slot = self.claim(req.task_id, &agent_id)?;
        let stops = self.stop_handle(req.task_id, signal);
        let model = role
            .resolve_model(&self.rt.current_models())
            .ok()
            .map(|r| r.model_ref());

        // #2364: where it will work, decided before anything happens in AMP.
        let edits = role.tool_policy(&self.manager.cwd).exposes("edit");
        let repo = self.probe().await;
        let place = if req.worktree.unwrap_or(edits) {
            Some(
                workspace::placement(
                    self.config.isolation,
                    &repo,
                    &self.manager.cwd,
                    req.base.is_some(),
                )
                .map_err(RunTicketError::Workspace)?,
            )
        } else {
            None
        };
        Ok(Prepared {
            req,
            role,
            agent_id,
            model,
            at: Where { repo, place, edits },
            stops,
            slot,
        })
    }

    /// The run itself, on a [`Prepared`] ticket: waits its turn (the shared-folder writer lock,
    /// then a worker slot), dispatches in AMP, records the ledger, runs the worker, and ends in a
    /// report — or in `Err` when it never got to dispatch.
    ///
    /// `detached` is AMP #2834's launch rule: the manager's tool-call signal still cancels a
    /// worker that has not been dispatched yet (nothing is left in AMP), but once AMP has the
    /// dispatch the run is the supervisor's, and only [`Supervisor::stop_worker`] (the person, at
    /// the chat) or [`Supervisor::cancel_all`] (quit) end it. An Esc in the chat stops the reply,
    /// not the workers.
    async fn run_prepared(
        &self,
        prepared: Prepared,
        detached: bool,
    ) -> Result<WorkerReport, RunTicketError> {
        let Prepared {
            req,
            role,
            agent_id,
            at,
            stops,
            slot,
            ..
        } = prepared;
        // Held until this returns, however it returns: the ticket leaves the live map (so it can
        // be run again) only when its run is really over.
        let _slot = slot;
        let execution = if detached {
            StopHandle {
                signal: CancellationToken::new(),
                ..stops.clone()
            }
        } else {
            stops.clone()
        };
        let signal = stops.signal.clone();

        // Under `isolation = "shared"`, workers that edit files take turns in the manager's
        // folder. (An explicit `worktree: false` is the manager's own call and doesn't wait.)
        let _writer = if at.edits && at.place == Some(Placement::Shared) {
            Some(tokio::select! {
                biased;
                _ = signal.cancelled() => return Err(RunTicketError::Cancelled),
                _ = self.cancel.cancelled() => return Err(RunTicketError::Cancelled),
                _ = stops.stop.cancelled() => {
                    let by = stops.stopped_by.lock().unwrap().clone().unwrap_or_default();
                    return Err(RunTicketError::Stopped { task_id: req.task_id, by });
                }
                g = Arc::clone(&self.writer).lock_owned() => g,
            })
        } else {
            None
        };

        // Wait for a slot; a cancelled wait dispatches nothing.
        let permit = tokio::select! {
            biased;
            _ = signal.cancelled() => return Err(RunTicketError::Cancelled),
            _ = self.cancel.cancelled() => return Err(RunTicketError::Cancelled),
            _ = stops.stop.cancelled() => {
                let by = stops.stopped_by.lock().unwrap().clone().unwrap_or_default();
                return Err(RunTicketError::Stopped { task_id: req.task_id, by });
            }
            p = Arc::clone(&self.slots).acquire_owned() => p.map_err(|_| RunTicketError::Cancelled)?,
        };
        let _running = Running::start(self);

        let dispatched = call_json(
            &self.rt.mcp,
            "amp_dispatch_task",
            json!({"task_id": req.task_id, "agent_id": agent_id}),
        )
        .await
        .map_err(|e| {
            // #2226: AMP's refusals link the web UI; without a public URL the link is relative.
            // Same rewrite as every other AMP tool result (#2205), so the model and the TUI see
            // the web UI's absolute link, not `/project/71/my-work?approve=all`.
            let session = self.rt.mcp.session();
            RunTicketError::Dispatch {
                task_id: req.task_id,
                message: crate::links::web_links(
                    &e.to_string(),
                    &session.web_base(),
                    &session.mcp_origin(),
                )
                .into_owned(),
            }
        })?;
        if let Some(w) = self.live.lock().unwrap().get_mut(&req.task_id) {
            w.dispatched = true;
        }
        // AMP #2801's result says what the dispatch gave us: the token the worker presents to
        // start and close the ticket, and the agent AMP recorded. An older server sends neither;
        // then the worker starts by fallback (its first write) exactly as before.
        let dispatched =
            serde_json::from_value::<DispatchResult>(dispatched).unwrap_or_else(|_| {
                DispatchResult {
                    task_id: req.task_id,
                    ..DispatchResult::default()
                }
            });
        let dispatch = Dispatch::new(&dispatched.dispatch_id);
        // The ledger is what the next startup sweeps: this ticket is ours from here until its
        // worker closes it or it is handed back.
        if let Err(e) = self.ledger.record(
            req.task_id,
            &dispatched.dispatch_id,
            dispatched.agent().unwrap_or(&agent_id),
        ) {
            tracing::warn!(ticket = req.task_id, error = %e, "could not record the dispatch");
        }

        let report = self
            .run_dispatched(&req, &role, &agent_id, &dispatch, at, execution)
            .await;
        drop(permit);
        Ok(report)
    }

    /// Launches one ticket in the background (AMP #2834): everything that can refuse a run
    /// happens here, and the caller gets a [`LaunchAck`] as soon as the worker is the
    /// supervisor's — the worker itself runs on in a task of the supervisor's, its ticket
    /// dispatched, claimed, in the ledger and returned exactly as a foreground run's would be.
    ///
    /// The manager's tool call is what calls this, so a chat is free for other work while up to
    /// [`SupervisorConfig::max_parallel`] workers run; dispatches from separate turns overlap
    /// instead of serialising (the cap is the same semaphore, held by the background task).
    ///
    /// **Cancellation.** `signal` (the tool call's) cancels a worker that has not been dispatched
    /// yet: nothing is left in AMP and the end says so. Once AMP has the dispatch, only
    /// [`Supervisor::stop_worker`] — the person at the chat — or [`Supervisor::cancel_all`] — quit
    /// — end the run, because stopping the manager's reply must not silently kill work it
    /// started (the KB's split from #2832).
    ///
    /// Every launched end lands in [`Supervisor::take_launch_ends`], for whoever delivers it into
    /// the manager's conversation; [`Supervisor::join_launched`] waits for the tasks at shutdown.
    pub async fn launch(
        self: &Arc<Self>,
        req: RunRequest,
        signal: CancellationToken,
    ) -> Result<LaunchAck, RunTicketError> {
        let prepared = self.prepare(req, signal).await?;
        // Best effort at the ack: with every slot taken this run waits its turn, which the
        // manager is told instead of being made to wait for it.
        let ack = LaunchAck::new(
            prepared.req.task_id,
            prepared.agent_id.clone(),
            prepared.role.id.clone(),
            prepared.model.clone().unwrap_or_default(),
            prepared.at.place,
            self.config.max_parallel,
            self.slots.available_permits() == 0,
        );
        let sup = Arc::clone(self);
        let (task_id, agent_id) = (ack.task_id, ack.agent_id.clone());
        let handle = tokio::spawn(async move {
            let result = sup.run_prepared(prepared, true).await;
            sup.record_launch_end(LaunchEnd {
                task_id,
                agent_id,
                result: result.map_err(|e| e.to_string()),
            });
        });
        let mut launched = self.launched.lock().unwrap_or_else(PoisonError::into_inner);
        // Finished tasks are already accounted for by their end: drop their handles so a long
        // chat's launches don't pile up.
        launched.retain(|h| !h.is_finished());
        launched.push(handle);
        drop(launched);
        Ok(ack)
    }

    /// The launched workers' ends, oldest first, drained (AMP #2834): each is a finished run or a
    /// run that never got as far as dispatching. Completion delivery is the chat's job; this is
    /// the queue it reads.
    pub fn take_launch_ends(&self) -> Vec<LaunchEnd> {
        self.launch_ends.drain()
    }

    /// Every launched end from now on, as it lands, without consuming it (AMP #2861): what the
    /// chat shows; [`Supervisor::take_launch_ends`] still delivers each one to the manager once.
    pub fn watch_launch_ends(&self) -> tokio::sync::broadcast::Receiver<LaunchEnd> {
        self.launch_ends.watch()
    }

    /// Waits for the next launched end (AMP #2834). Returns at once when one is already queued.
    pub async fn next_launch_end(&self) -> LaunchEnd {
        self.launch_ends.next().await
    }

    /// How many launched workers are still on this supervisor's books.
    pub fn launched_running(&self) -> usize {
        self.launched
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .iter()
            .filter(|h| !h.is_finished())
            .count()
    }

    /// Waits up to `bound` for the launched workers to end (AMP #2834: `cancel_all` first, then
    /// this, then [`Supervisor::release_unfinished`] for anything still open). Returns how many
    /// ended within the bound; the ones that didn't are left running — a caller that means to
    /// stop them cancels first rather than having them aborted mid-run here.
    pub async fn join_launched(&self, bound: Duration) -> usize {
        let handles: Vec<JoinHandle<()>> =
            std::mem::take(&mut *self.launched.lock().unwrap_or_else(PoisonError::into_inner));
        let deadline = Instant::now() + bound;
        let mut ended = 0;
        for handle in handles {
            let left = deadline.saturating_duration_since(Instant::now());
            if tokio::time::timeout(left, handle).await.is_ok() {
                ended += 1;
            }
        }
        ended
    }

    fn record_launch_end(&self, end: LaunchEnd) {
        self.launch_ends.push(end);
    }

    fn stop_handle(&self, task_id: i64, signal: CancellationToken) -> StopHandle {
        let live = self.live.lock().unwrap();
        let (stop, stopped_by) = match live.get(&task_id) {
            Some(w) => (w.stop.clone(), Arc::clone(&w.stopped_by)),
            None => (CancellationToken::new(), Arc::default()),
        };
        StopHandle {
            signal,
            stop,
            stopped_by,
        }
    }

    fn claim(self: &Arc<Self>, task_id: i64, agent_id: &str) -> Result<LiveSlot, RunTicketError> {
        let mut live = self.live.lock().unwrap();
        if live.contains_key(&task_id) {
            return Err(RunTicketError::AlreadyRunning(task_id));
        }
        live.insert(
            task_id,
            LiveWorker {
                agent_id: agent_id.to_owned(),
                dispatched: false,
                watch: None,
                phase: WorkerPhase::Queued,
                model: None,
                cwd: None,
                since: SystemTime::now(),
                totals: None,
                feed: WorkerFeed::new(),
                stop: CancellationToken::new(),
                stopped_by: Arc::default(),
                branch: None,
                context_window: None,
            },
        );
        Ok(LiveSlot(Arc::clone(self), task_id))
    }

    async fn run_dispatched(
        &self,
        req: &RunRequest,
        role: &Role,
        agent_id: &str,
        dispatch: &Dispatch,
        at: Where,
        stops: StopHandle,
    ) -> WorkerReport {
        let StopHandle {
            signal,
            stop,
            stopped_by,
        } = stops;
        let started = Instant::now();
        let feed = self
            .live
            .lock()
            .unwrap()
            .get(&req.task_id)
            .map(|w| Arc::clone(&w.feed))
            .unwrap_or_default();
        let mut notes = Vec::new();
        let totals = Arc::new(Mutex::new(Totals::default()));
        let resolved = role.resolve_model(&self.rt.current_models()).ok();
        let model = resolved.as_ref().map(|r| r.model_ref()).unwrap_or_default();
        let context_window = resolved
            .as_ref()
            .map(|r| r.model.context_window)
            .filter(|w| *w > 0);

        // Where the worker runs (#2364: decided in run_ticket).
        let mut info = WorkerInfo {
            task_id: req.task_id,
            agent_id: agent_id.to_owned(),
            role: role.id.clone(),
            dispatch_id: dispatch.id.clone(),
            model,
            cwd: self.manager.cwd.clone(),
            worktree: None,
            branch: None,
            workspace: at.place.unwrap_or(Placement::Shared),
            git: at.repo.is_git(),
            edits: at.edits,
            manager_cwd: self.manager.cwd.clone(),
            totals: Arc::clone(&totals),
        };
        match at.place {
            Some(Placement::Worktree) => match self.worktree(req, &at.repo).await {
                Ok(wt) => {
                    if wt.reused {
                        notes.push(format!(
                            "reused the existing worktree {}",
                            wt.path.display()
                        ));
                    }
                    info.cwd = wt.path.clone();
                    info.worktree = Some(wt.path);
                    info.branch = wt.branch;
                }
                Err(e) => {
                    return self
                        .finish(
                            &info,
                            started,
                            notes,
                            Finish::error(format!("worktree: {e}")),
                            dispatch,
                        )
                        .await;
                }
            },
            Some(Placement::Folder) => match ticket_folder(&self.manager.cwd, req.task_id) {
                Ok((path, reused)) => {
                    if reused {
                        notes.push(format!("reused its folder {}", path.display()));
                    }
                    if self.config.isolation == Isolation::Auto
                        && let Some(why) = at.repo.why_not(&self.manager.cwd)
                    {
                        notes.push(no_worktree_note(&self.manager.cwd, &why, &path, &at.repo));
                    }
                    info.cwd = path;
                }
                Err(e) => {
                    return self
                        .finish(
                            &info,
                            started,
                            notes,
                            Finish::error(format!("folder: {e}")),
                            dispatch,
                        )
                        .await;
                }
            },
            Some(Placement::Shared) | None => {}
        }
        {
            let mut live = self.live.lock().unwrap();
            if let Some(w) = live.get_mut(&req.task_id) {
                w.phase = WorkerPhase::Running;
                w.model = Some(info.model.clone());
                w.cwd = Some(info.cwd.clone());
                w.since = SystemTime::now();
                w.totals = Some(Arc::clone(&totals));
                w.branch = info.branch.clone();
                w.context_window = context_window;
            }
        }

        // Build the worker's own harness.
        let budget = CancellationToken::new();
        let watch = Arc::new(WorkerWatch {
            task_id: req.task_id,
            totals: Arc::clone(&totals),
            max_turns: self.config.max_turns,
            budget: budget.clone(),
            closing: Mutex::new(HashMap::new()),
            closed: Mutex::new(None),
            repeat_stop: Mutex::new(None),
            feed,
        });
        if let Some(w) = self.live.lock().unwrap().get_mut(&req.task_id) {
            w.watch = Some(Arc::clone(&watch));
        }
        let mut spec = AgentSpec::new(role.id.clone(), info.cwd.clone());
        spec.project = self.project();
        spec.extra_context = Some(worker_context(&info, &self.manager.role));
        spec.session = SessionKind::Jsonl {
            dir: self
                .rt
                .state_dir
                .join("sessions")
                .join(req.task_id.to_string()),
        };
        spec.emitter = Some(Arc::new(WorkerEmitter {
            watch: Arc::clone(&watch),
        }));
        if let Some(hooks) = &self.hooks {
            spec.before_tool_call = hooks.before_tool_call(&info);
            spec.extra_tools.extend(hooks.extra_tools(&info));
        }
        // #2239: a worker waits for shared MCP servers still on their first attempt (a no-op once
        // they are up) and gets its own instances of worktree-scoped ones, released in finish().
        let key = req.task_id.to_string();
        self.rt.mcp_hub.wait_started(true).await;
        if let Some(worktree) = &info.worktree {
            spec.mcp.worktree = self.rt.mcp_hub.start_worktree(&key, worktree).await;
        }
        spec.mcp.output_key = Some(key);
        let built = match self.rt.build(spec).await {
            Ok(b) => b,
            Err(e) => {
                return self
                    .finish(
                        &info,
                        started,
                        notes,
                        Finish::error(format!("build the worker: {e}")),
                        dispatch,
                    )
                    .await;
            }
        };
        info.model = built.resolved.model_ref();
        let session_file = built.session_file.clone();
        let harness = Arc::clone(&built.harness);
        // AMP #2802: the ticket is started by the harness, on its worker's behalf, at the moment
        // the worker actually exists and is about to run its first turn. Not by the model's first
        // tool call (a model that never makes one would leave the ticket unstarted and let AMP's
        // watchdog return it under a live worker), and not before the worker is built (a build
        // failure must leave the ticket *returnable*: AMP returns a dispatched-but-unstarted
        // ticket and refuses a started one). The claim is idempotent and posts "{agent} picked
        // this up" on the ticket, which is what makes the run visible on the board.
        if !dispatch.id.is_empty() {
            match self
                .claim_for_worker(req.task_id, &dispatch.id, agent_id)
                .await
            {
                Ok(()) => {
                    dispatch.claimed.store(true, Ordering::SeqCst);
                    notes.push(format!(
                        "started #{0} in AMP (amp_claim_task with its dispatch_id)",
                        req.task_id
                    ));
                }
                Err(e) => {
                    // A stale token means this dispatch is over (AMP returned the ticket and
                    // dispatched it again): the run must not go on as if it held the ticket.
                    notes.push(format!("could not start #{0} in AMP: {e}", req.task_id));
                    return self
                        .finish(
                            &info,
                            started,
                            notes,
                            Finish::error(format!("the claim was refused: {e}")),
                            dispatch,
                        )
                        .await;
                }
            }
        }
        let prompt = worker_prompt(&info, req, self.project().as_ref());
        // The chat's session view starts with the prompt (#2252); rpi doesn't emit it.
        watch.feed.push(&AgentEvent::MessageStart {
            message: rpi_agent::AgentMessage::User(rpi_ai::types::UserMessage {
                role: Default::default(),
                content: rpi_ai::types::UserContent::Text(prompt.clone()),
                timestamp: SystemTime::now()
                    .duration_since(SystemTime::UNIX_EPOCH)
                    .map_or(0, |d| d.as_millis() as i64),
            }),
        });

        // Run it in the foreground of this call.
        let run = harness.prompt_text(&prompt, Vec::new());
        tokio::pin!(run);
        let deadline = tokio::time::sleep(self.config.timeout);
        tokio::pin!(deadline);
        let stop = tokio::select! {
            biased;
            r = &mut run => Stop::Returned(r),
            _ = signal.cancelled() => Stop::Ended(WorkerOutcome::Cancelled, "the manager's run was cancelled".to_owned()),
            _ = self.cancel.cancelled() => Stop::Ended(WorkerOutcome::Cancelled, "every worker was cancelled".to_owned()),
            _ = &mut deadline => Stop::Ended(WorkerOutcome::Timeout, format!("no result after {}", human_duration(self.config.timeout))),
            _ = budget.cancelled() => Stop::Ended(WorkerOutcome::TurnBudget, format!("turn budget of {} turns used up", self.config.max_turns)),
            _ = stop.cancelled() => {
                let by = stopped_by.lock().unwrap().clone().unwrap_or_default();
                Stop::Ended(WorkerOutcome::Cancelled, stopped_from_chat(&by))
            }
        };
        let result = match stop {
            Stop::Returned(r) => Some(r),
            Stop::Ended(outcome, why) => {
                let _ = harness.abort().await;
                let settled = tokio::time::timeout(self.config.abort_grace, &mut run).await;
                if settled.is_err() {
                    notes.push(format!(
                        "the worker did not stop within {} of the abort and was dropped",
                        human_duration(self.config.abort_grace)
                    ));
                }
                let last = totals.lock().unwrap().last_text.clone();
                let mut finish = Finish {
                    outcome,
                    error: Some(why),
                    text: last,
                    session_file,
                };
                if let Ok(Ok(r)) = settled
                    && let Some(text) = outcome_text(&r.outcome)
                    && !text.is_empty()
                {
                    finish.text = text;
                }
                // Read after the abort settled: a closing call can end during the grace period.
                let closed = *watch.closed.lock().unwrap();
                if closed.is_none()
                    && let Some(by) = stopped_by.lock().unwrap().clone()
                {
                    // #2252: the manager reads this in amp_run_ticket's result.
                    notes.push(format!(
                        "{by} watched this worker and stopped it from the chat; ask them \
                         before running #{} again",
                        req.task_id
                    ));
                }
                let finish = finish.after_closing(closed, &mut notes);
                return self.finish(&info, started, notes, finish, dispatch).await;
            }
        };

        let closed = *watch.closed.lock().unwrap();
        let last = totals.lock().unwrap().last_text.clone();
        let mut finish = match result {
            Some(Ok(r)) => match r.outcome {
                HarnessRunOutcome::Completed { final_message, .. } => Finish {
                    outcome: closed.unwrap_or(WorkerOutcome::Stopped),
                    // #2225: say why it stopped (the abandoned run's note carries it).
                    error: closed
                        .is_none()
                        .then(|| {
                            watch
                                .repeat_stop
                                .lock()
                                .unwrap()
                                .as_ref()
                                .map(|s| s.reason())
                        })
                        .flatten(),
                    text: assistant_text(&final_message),
                    session_file: None,
                },
                HarnessRunOutcome::Aborted { final_message, .. } => Finish {
                    outcome: WorkerOutcome::Cancelled,
                    error: Some("the worker's run was aborted".to_owned()),
                    text: assistant_text(&final_message),
                    session_file: None,
                },
                HarnessRunOutcome::Failed {
                    error,
                    final_message,
                    ..
                } => Finish {
                    outcome: WorkerOutcome::Error,
                    // One line, no response body (#2207): the manager reads this as a tool result.
                    error: Some(crate::provider_error::run_failure(
                        &error.code,
                        &error.message,
                        final_message.as_ref().map_or("", |m| m.provider.as_str()),
                    )),
                    text: final_message
                        .as_ref()
                        .map(assistant_text)
                        .unwrap_or_default(),
                    session_file: None,
                },
                HarnessRunOutcome::Suspended { .. } => Finish::error(
                    "the provider deferred its response (suspended runs are not supported)",
                ),
            },
            Some(Err(e)) => Finish::error(format!("{e}")),
            None => Finish::error("no result"),
        };
        if finish.text.is_empty() {
            finish.text = last;
        }
        finish.session_file = session_file;
        let finish = finish.after_closing(closed, &mut notes);
        self.finish(&info, started, notes, finish, dispatch).await
    }

    async fn finish(
        &self,
        info: &WorkerInfo,
        started: Instant,
        notes: Vec<String>,
        finish: Finish,
        dispatch: &Dispatch,
    ) -> WorkerReport {
        let totals = info.totals.lock().unwrap().clone();
        let mut report = WorkerReport {
            task_id: info.task_id,
            agent_id: info.agent_id.clone(),
            role: info.role.clone(),
            outcome: finish.outcome,
            dispatch_id: dispatch.id.clone(),
            started: dispatch.started(),
            returned: None,
            final_message: finish.text,
            error: finish.error,
            model: info.model.clone(),
            cwd: info.cwd.clone(),
            worktree: info.worktree.clone(),
            branch: info.branch.clone(),
            workspace: info.workspace,
            session_file: finish.session_file,
            turns: totals.turns,
            tool_calls: totals.tool_calls,
            tool_errors: totals.tool_errors,
            usage: totals.usage,
            duration_ms: started.elapsed().as_millis() as u64,
            notes,
            totals,
        };
        // #2239: the worker's worktree-scoped MCP server instances end with it.
        self.rt
            .mcp_hub
            .release_worktree(&info.task_id.to_string())
            .await;
        if let Some(hooks) = &self.hooks {
            hooks.on_finish(info, &mut report).await;
        }
        self.record_finished(&report);
        report
    }

    /// Git about the manager's folder, asked off the async threads.
    async fn probe(&self) -> Repo {
        let cwd = self.manager.cwd.clone();
        tokio::task::spawn_blocking(move || Repo::probe(&cwd))
            .await
            .unwrap_or(Repo::NotGit)
    }

    /// One line on where this manager's workers work, for its `<amp_context>` (#2364).
    pub fn workspace_context(&self) -> String {
        let repo = Repo::probe(&self.manager.cwd);
        workspace::manager_context(self.config.isolation, &self.manager.cwd, &repo)
    }

    /// The chat banner's line about where workers work, when it isn't git worktrees (#2364).
    pub fn workspace_banner(&self) -> Option<String> {
        let repo = Repo::probe(&self.manager.cwd);
        workspace::banner_line(self.config.isolation, &self.manager.cwd, &repo)
    }

    /// Creates or reuses `<repo>/.worktrees/<task_id>` in `repo` (usable, or with no commit when
    /// the call names a base: [`workspace::placement`] decided that).
    async fn worktree(&self, req: &RunRequest, repo: &Repo) -> Result<Worktree, String> {
        let _lock = self.git_lock.lock().await;
        let top = match repo {
            Repo::Usable { top } | Repo::NoCommit { top } => top.clone(),
            other => {
                return Err(other
                    .why_not(&self.manager.cwd)
                    .unwrap_or_else(|| "no git repository".to_owned()));
            }
        };
        let path = top.join(".worktrees").join(req.task_id.to_string());
        exclude_worktrees_dir(&top).await;

        if path.exists() {
            let inner = git(&path, &["rev-parse", "--show-toplevel"])
                .await
                .map_err(|e| format!("{} exists but is not a git worktree: {e}", path.display()))?;
            if !same_path(Path::new(&inner), &path) {
                return Err(format!(
                    "{} exists but is not a git worktree (it belongs to {inner})",
                    path.display()
                ));
            }
            let branch = git(&path, &["branch", "--show-current"])
                .await
                .ok()
                .filter(|b| !b.is_empty());
            return Ok(Worktree {
                path,
                branch,
                reused: true,
            });
        }

        let branch = req
            .branch
            .clone()
            .filter(|b| !b.trim().is_empty())
            .unwrap_or_else(|| format!("amp-{}", req.task_id));
        let path_str = path.to_string_lossy().into_owned();
        let exists = git(
            &top,
            &[
                "rev-parse",
                "--verify",
                "--quiet",
                &format!("refs/heads/{branch}"),
            ],
        )
        .await
        .is_ok();
        if exists {
            git(&top, &["worktree", "add", &path_str, &branch]).await?;
        } else {
            let base = req.base.clone().unwrap_or_else(|| "HEAD".to_owned());
            git(&top, &["worktree", "add", "-b", &branch, &path_str, &base]).await?;
        }
        Ok(Worktree {
            path,
            branch: Some(branch),
            reused: false,
        })
    }
}

/// Where run_ticket decided the worker works (#2364).
/// How a run can be stopped: the manager's run being cancelled (`signal`), and the person asking
/// this worker to stop (`stop`, with who asked in `stopped_by`). Passed as one thing — they are
/// only ever used together, and separately they made `run_dispatched` a seven-argument function.
#[derive(Clone)]
struct StopHandle {
    signal: CancellationToken,
    stop: CancellationToken,
    stopped_by: Arc<Mutex<Option<String>>>,
}

struct Where {
    repo: Repo,
    /// `None`: the manager's folder (read-only roles, or `worktree: false`).
    place: Option<Placement>,
    /// The role can edit files.
    edits: bool,
}

/// One ticket checked and reserved by [`Supervisor::prepare`], waiting for its run (AMP #2834):
/// everything that can refuse a dispatch has been decided, and the ticket is in the live map so
/// nothing else can start it. Dropping it without running releases the slot.
struct Prepared {
    req: RunRequest,
    role: Role,
    /// AMP's form: `amp:amp-worker-backend`.
    agent_id: String,
    /// `provider/model` when the role's model resolves (the ack says it before the worker is
    /// built).
    model: Option<String>,
    at: Where,
    stops: StopHandle,
    slot: LiveSlot,
}

/// What a dispatch gave this run (AMP #2801/#2802): the token its worker starts and closes the
/// ticket with. Empty on a server that doesn't issue tokens, which skips the claim and lets the
/// worker's first write start the ticket (AMP's fallback).
#[derive(Debug, Clone, Default)]
struct Dispatch {
    /// `d_…`.
    id: String,
    /// Whether the ticket was started for this run: the harness's claim, or AMP's fallback on the
    /// worker's first write. Shared, because the worker's hooks see the same run.
    claimed: Arc<AtomicBool>,
}

impl Dispatch {
    fn new(id: &str) -> Self {
        Self {
            id: id.to_owned(),
            claimed: Arc::default(),
        }
    }

    fn started(&self) -> bool {
        self.claimed.load(Ordering::SeqCst)
    }
}

/// Creates (or reuses) the worker's own folder `<cwd>/amp-work/<task_id>` (#2364).
fn ticket_folder(cwd: &Path, task_id: i64) -> Result<(PathBuf, bool), String> {
    let path = cwd.join(WORK_DIR).join(task_id.to_string());
    if path.is_dir() {
        return Ok((path, true));
    }
    std::fs::create_dir_all(&path).map_err(|e| format!("create {}: {e}", path.display()))?;
    Ok((path, false))
}

struct Worktree {
    path: PathBuf,
    branch: Option<String>,
    reused: bool,
}

enum Stop<R> {
    Returned(R),
    Ended(WorkerOutcome, String),
}

struct Finish {
    outcome: WorkerOutcome,
    error: Option<String>,
    text: String,
    session_file: Option<PathBuf>,
}

impl Finish {
    fn error(message: impl Into<String>) -> Self {
        Self {
            outcome: WorkerOutcome::Error,
            error: Some(message.into()),
            text: String::new(),
            session_file: None,
        }
    }

    /// A worker whose `amp_complete_task` / `amp_block_task` on its own ticket already succeeded
    /// keeps that outcome, however its run then ended (cancelled, timed out, over its turn budget,
    /// aborted or failed on a later turn). The ticket is closed and its run recorded with the
    /// closing call, so this is not an abandoned run: reporting one would count the work as rework
    /// and its tokens twice. The reason it stopped becomes a note, not an error.
    fn after_closing(self, closed: Option<WorkerOutcome>, notes: &mut Vec<String>) -> Self {
        let Some(closed) = closed else {
            return self;
        };
        if self.outcome.closed_ticket() {
            return self;
        }
        let mut why = self.outcome.as_str().to_owned();
        if let Some(err) = self
            .error
            .as_deref()
            .map(str::trim)
            .filter(|e| !e.is_empty())
        {
            why.push_str(": ");
            why.push_str(err);
        }
        notes.push(format!("stopped after closing its ticket: {why}"));
        Self {
            outcome: closed,
            error: None,
            ..self
        }
    }
}

fn outcome_text(outcome: &HarnessRunOutcome) -> Option<String> {
    match outcome {
        HarnessRunOutcome::Completed { final_message, .. }
        | HarnessRunOutcome::Aborted { final_message, .. } => Some(assistant_text(final_message)),
        HarnessRunOutcome::Failed { final_message, .. } => {
            final_message.as_ref().map(assistant_text)
        }
        HarnessRunOutcome::Suspended { .. } => None,
    }
}

/// Keeps a ticket in the live map while its run exists, and takes it out when the run ends,
/// however it ends (AMP #2834: owned rather than borrowed, so a launched run's task can hold it).
struct LiveSlot(Arc<Supervisor>, i64);

impl Drop for LiveSlot {
    fn drop(&mut self) {
        if let Ok(mut live) = self.0.live.lock() {
            live.remove(&self.1);
        }
    }
}

/// Counts running workers and records the peak.
struct Running<'a>(&'a Supervisor);

impl<'a> Running<'a> {
    fn start(sup: &'a Supervisor) -> Self {
        let now = sup.running.fetch_add(1, Ordering::SeqCst) + 1;
        sup.peak.fetch_max(now, Ordering::SeqCst);
        Self(sup)
    }
}

impl Drop for Running<'_> {
    fn drop(&mut self) {
        self.0.running.fetch_sub(1, Ordering::SeqCst);
    }
}

/// Watches a worker's events as they are emitted: usage totals, the turn budget, and whether
/// the worker closed its ticket.
struct WorkerWatch {
    task_id: i64,
    totals: Arc<Mutex<Totals>>,
    max_turns: u32,
    budget: CancellationToken,
    /// Tool call id -> the outcome it means, for calls that close this ticket.
    closing: Mutex<HashMap<String, WorkerOutcome>>,
    closed: Mutex<Option<WorkerOutcome>>,
    /// #2225: the repeat guard stopped the worker's model.
    repeat_stop: Mutex<Option<crate::repeat::RepeatStop>>,
    /// The chat's session view reads the worker's events from here (#2252).
    feed: Arc<WorkerFeed>,
}

impl WorkerWatch {
    fn observe(&self, event: &AgentEvent) {
        self.feed.push(event);
        let Some(e) = RunEvent::from_agent(event) else {
            return;
        };
        let turns = {
            let mut t = self.totals.lock().unwrap();
            t.observe(&e);
            t.turns
        };
        match &e {
            RunEvent::TurnStart if self.max_turns > 0 && turns > self.max_turns => {
                self.budget.cancel();
            }
            RunEvent::ToolStart {
                id,
                name,
                arguments,
            } => {
                let outcome = match name.as_str() {
                    COMPLETE_TOOL => Some(WorkerOutcome::Completed),
                    BLOCK_TOOL => Some(WorkerOutcome::Blocked),
                    _ => None,
                };
                if let Some(outcome) = outcome
                    && task_id_arg(arguments) == Some(self.task_id)
                {
                    self.closing.lock().unwrap().insert(id.clone(), outcome);
                }
            }
            RunEvent::ToolEnd {
                id, is_error, text, ..
            } => {
                if let Some(outcome) = self.closing.lock().unwrap().remove(id)
                    && !is_error
                {
                    *self.closed.lock().unwrap() = Some(outcome);
                }
                if *is_error && let Some(stop) = crate::repeat::stopped(text) {
                    *self.repeat_stop.lock().unwrap() = Some(stop);
                }
            }
            _ => {}
        }
    }
}

fn live_info(task_id: i64, w: &LiveWorker) -> WorkerSessionInfo {
    WorkerSessionInfo {
        task_id,
        agent_id: w.agent_id.clone(),
        phase: Some(w.phase),
        model: w.model.clone(),
        cwd: w.cwd.clone(),
        since: w.since,
        ended: None,
        totals: w
            .totals
            .as_ref()
            .map(|t| t.lock().unwrap().clone())
            .unwrap_or_default(),
        outcome: None,
        error: None,
        stopped_by: w.stopped_by.lock().unwrap().clone(),
        returned: None,
        session_file: None,
        branch: w.branch.clone(),
        context_window: w.context_window,
    }
}

/// A tool call's `task_id` argument: a number, or a string such as `"#12"`.
pub(crate) fn task_id_arg(args: &Value) -> Option<i64> {
    match &args["task_id"] {
        Value::Number(n) => n.as_i64().or_else(|| n.as_f64().map(|f| f as i64)),
        Value::String(s) => s.trim().trim_start_matches('#').parse().ok(),
        _ => None,
    }
}

/// The worker's `agent_emitter`: updates the watch synchronously, so totals are current when the
/// next tool hook runs.
struct WorkerEmitter {
    watch: Arc<WorkerWatch>,
}

impl AgentEmitter for WorkerEmitter {
    fn emit(
        &self,
        event: AgentEvent,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send + 'static>> {
        self.watch.observe(&event);
        Box::pin(std::future::ready(()))
    }

    fn try_emit(&self, event: AgentEvent) {
        self.watch.observe(&event);
    }
}

/// What a worker outside its own git worktree is told about git (#2364): nothing to commit.
const NO_GIT_STEPS: &str = "don't commit, branch, push or open a pull request, and leave branch, \
     commit, pushed and pr_url out of amp_complete_task";

fn worker_context(info: &WorkerInfo, manager: &str) -> String {
    let mut lines = vec![
        format!("ticket: #{}", info.task_id),
        format!("agent_id: {}", info.agent_id),
        format!("dispatched_by: {manager} (amp_run_ticket)"),
    ];
    // AMP #2801/#2802: the token is what makes this run's completion the worker's own rather
    // than a closeout. The harness already started the ticket with it and fills it into
    // amp_complete_task/amp_block_task; saying so keeps the worker from passing something else.
    if !info.dispatch_id.is_empty() {
        lines.push(format!("dispatch_id: {}", info.dispatch_id));
        lines.push(
            "started: amp-harness called amp_claim_task for you with that dispatch_id; pass it on \
             amp_complete_task and amp_block_task (the harness fills it in when you leave it out)"
                .to_owned(),
        );
    }
    match (&info.worktree, &info.branch) {
        (Some(wt), branch) => {
            lines.push("workspace: worktree".to_owned());
            lines.push(format!("worktree: {}", wt.display()));
            if let Some(b) = branch {
                lines.push(format!("branch: {b}"));
            }
            lines.push("git: yes (commit your work on your branch)".to_owned());
        }
        (None, _) => {
            lines.push(match info.workspace {
                Placement::Folder => format!(
                    "workspace: folder {} (your own, for this ticket; the person's files are in {})",
                    info.cwd.display(),
                    info.manager_cwd.display()
                ),
                _ if info.edits => format!(
                    "workspace: shared {} (the person's folder; workers that edit files take turns in it)",
                    info.cwd.display()
                ),
                _ => format!("workspace: the manager's folder {}", info.cwd.display()),
            });
            lines.push("worktree: none".to_owned());
            if !info.git {
                lines.push(format!("git: none (not a git repository): {NO_GIT_STEPS}"));
            } else if info.edits {
                lines.push(format!(
                    "git: not yours here (no branch of your own): {NO_GIT_STEPS}"
                ));
            }
        }
    }
    lines.join("\n")
}

fn worker_prompt(info: &WorkerInfo, req: &RunRequest, project: Option<&ProjectMarker>) -> String {
    let mut text = format!(
        "You are working AMP ticket #{}{}. Read it with amp_get_task (include comments): its \
         description and acceptance criteria are the spec. Follow your execution protocol, and \
         finish by completing the ticket (amp_complete_task) or blocking it with a question \
         (amp_block_task).",
        info.task_id,
        project
            .map(|p| format!(" in project {}", p.project_id))
            .unwrap_or_default(),
    );
    // AMP #2802 item 6: the dispatch result's own worker_prompt tells a worker to claim first.
    // The harness has already claimed (it controls the moment the worker starts, so no model
    // turn can be lost between the two), so the prompt says what is true rather than asking for
    // a call that would only report already_started.
    if !info.dispatch_id.is_empty() {
        text.push_str(&format!(
            "\n\nThe harness started this ticket for you (amp_claim_task with dispatch_id \
             \"{}\"). Pass that dispatch_id to amp_complete_task and amp_block_task: it is what \
             makes the completion yours rather than a closeout. The harness fills it in if you \
             leave it out.",
            info.dispatch_id
        ));
    }
    match (&info.worktree, &info.branch) {
        (Some(wt), Some(branch)) => text.push_str(&format!(
            "\n\nYour working directory is the git worktree {} on branch {branch}; commit your work there.",
            wt.display()
        )),
        (Some(wt), None) => text.push_str(&format!(
            "\n\nYour working directory is the git worktree {}.",
            wt.display()
        )),
        (None, _) => {
            match info.workspace {
                Placement::Folder => text.push_str(&format!(
                    "\n\nYour working directory is {}, a folder of your own for this ticket inside \
                     {}. Put everything you make for this ticket in it. Read any file in {} you \
                     need, but don't change files outside your folder: other workers may be \
                     running at the same time.",
                    info.cwd.display(),
                    info.manager_cwd.display(),
                    info.manager_cwd.display()
                )),
                _ if info.edits => text.push_str(&format!(
                    "\n\nYour working directory is {}, the person's own folder. Workers that edit \
                     files take turns in it, so you are the only one editing now.",
                    info.cwd.display()
                )),
                _ => text.push_str(&format!(
                    "\n\nYour working directory is {}.",
                    info.cwd.display()
                )),
            }
            if info.edits {
                text.push_str(&format!(
                    " This work is not on a git branch of yours: {NO_GIT_STEPS}. In your \
                     completion comment, list the files you made or changed, with their paths."
                ));
            } else if !info.git {
                text.push_str(" It is not a git repository.");
            }
        }
    }
    if let Some(extra) = req
        .instructions
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty())
    {
        text.push_str("\n\nFrom the manager:\n");
        text.push_str(extra);
    }
    text
}

fn human_duration(d: Duration) -> String {
    let secs = d.as_secs();
    if secs >= 60 && secs.is_multiple_of(60) {
        format!("{} min", secs / 60)
    } else if secs >= 1 {
        format!("{secs} s")
    } else {
        format!("{} ms", d.as_millis())
    }
}

fn same_path(a: &Path, b: &Path) -> bool {
    match (std::fs::canonicalize(a), std::fs::canonicalize(b)) {
        (Ok(a), Ok(b)) => a == b,
        _ => a == b,
    }
}

/// The note a worker's report carries when `auto` gave it a folder instead of a worktree:
/// `why` is the reason (#2356, #2364).
fn no_worktree_note(cwd: &Path, why: &str, folder: &Path, repo: &Repo) -> String {
    format!(
        "{why}; the worker worked in its own folder {} (no branch, nothing committed). To give \
         workers their own git worktrees, {} in {}",
        folder.display(),
        repo.fix().replace(" here", ""),
        cwd.display()
    )
}

/// Runs git in `dir`; stdout trimmed, or stderr as the error.
async fn git(dir: &Path, args: &[&str]) -> Result<String, String> {
    let out = tokio::time::timeout(
        Duration::from_secs(120),
        tokio::process::Command::new("git")
            .arg("-C")
            .arg(dir)
            .args(args)
            .env("GIT_TERMINAL_PROMPT", "0")
            .kill_on_drop(true)
            .output(),
    )
    .await
    .map_err(|_| format!("git {} timed out", args.join(" ")))?
    .map_err(|e| format!("run git: {e}"))?;
    if out.status.success() {
        Ok(String::from_utf8_lossy(&out.stdout).trim().to_owned())
    } else {
        Err(format!(
            "git {}: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr).trim()
        ))
    }
}

/// Adds `.worktrees/` to the repo's `info/exclude` unless something already ignores it, so the
/// workers' checkouts never show up as untracked files. Best effort.
async fn exclude_worktrees_dir(top: &Path) {
    if git(top, &["check-ignore", "-q", ".worktrees/x"])
        .await
        .is_ok()
    {
        return;
    }
    let Ok(common) = git(top, &["rev-parse", "--git-common-dir"]).await else {
        return;
    };
    let common = if Path::new(&common).is_absolute() {
        PathBuf::from(common)
    } else {
        top.join(common)
    };
    let file = common.join("info").join("exclude");
    let mut text = std::fs::read_to_string(&file).unwrap_or_default();
    if !text.is_empty() && !text.ends_with('\n') {
        text.push('\n');
    }
    text.push_str("# amp-harness worker worktrees\n.worktrees/\n");
    let _ = std::fs::create_dir_all(common.join("info"));
    let _ = std::fs::write(&file, text);
}

/// The manager's `amp_run_ticket` tool.
pub struct RunTicketTool {
    sup: Arc<Supervisor>,
    schema: Tool,
}

impl RunTicketTool {
    pub fn new(sup: Arc<Supervisor>) -> Self {
        let schema = Tool {
            name: DISPATCH_TOOL.to_owned(),
            description: sup.tool_description(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "task_id": {"type": "integer", "description": "REQUIRED. The ticket to run."},
                    "agent_id": {"type": "string", "description": "REQUIRED. The agent to run it, e.g. amp:amp-worker-backend (the ticket's assigned_to)."},
                    "instructions": {"type": "string", "description": "Optional. Extra instructions for the worker, added to its prompt (the ticket itself is the spec)."},
                    "worktree": {"type": "boolean", "description": "Optional. Force a git worktree on or off. Default: on for agents that edit files."},
                    "branch": {"type": "string", "description": "Optional. Branch for a new worktree. Default amp-<task_id>."},
                    "base": {"type": "string", "description": "Optional. Where a new branch starts. Default HEAD."}
                },
                "required": ["task_id", "agent_id"],
                "additionalProperties": false
            })
            .into(),
            constrained_sampling: None,
        };
        Self { sup, schema }
    }
}

impl std::fmt::Debug for RunTicketTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RunTicketTool").finish_non_exhaustive()
    }
}

/// The arguments as the model sends them: `task_id` may come as a string.
fn parse_request(params: Value) -> Result<RunRequest, String> {
    let mut params = match params {
        Value::Object(map) => map,
        other => return Err(format!("arguments must be a JSON object, not {other}")),
    };
    if let Some(v) = params.get("task_id").cloned() {
        let id = task_id_arg(&json!({ "task_id": v }))
            .ok_or_else(|| format!("task_id must be a number, not {v}"))?;
        params.insert("task_id".into(), json!(id));
    }
    serde_json::from_value(Value::Object(params)).map_err(|e| e.to_string())
}

#[async_trait::async_trait]
impl AgentTool for RunTicketTool {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        "Run ticket"
    }

    async fn execute(
        &self,
        _tool_call_id: &str,
        params: Value,
        signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, ToolError> {
        let req = parse_request(params).map_err(ToolError::Validation)?;
        // AMP #2834: `[workers] dispatch = "launch"` (the chat) gets the ack; the default
        // foreground run awaits its worker and returns the report, which is what `run -p` and
        // every test rely on.
        let out = match self.sup.config().dispatch {
            DispatchMode::Await => self
                .sup
                .run_ticket(req, signal)
                .await
                .map(|r| Outcome::Report(Box::new(r))),
            DispatchMode::Launch => self.sup.launch(req, signal).await.map(Outcome::Launched),
        };
        match out {
            Ok(out) => {
                let value = match &out {
                    Outcome::Report(report) => serde_json::to_value(report),
                    Outcome::Launched(ack) => serde_json::to_value(ack),
                }
                .unwrap_or(Value::Null);
                let mut result =
                    AgentToolResult::text(serde_json::to_string_pretty(&value).unwrap_or_default());
                result.details = value;
                Ok(result)
            }
            Err(RunTicketError::Cancelled) => Err(ToolError::Abort),
            Err(e) => Err(ToolError::Tool(e.to_string())),
        }
    }
}

/// What `amp_run_ticket` returns: a foreground run's report, or a launched run's ack.
enum Outcome {
    /// Boxed: a report is much larger than an ack, and this enum is built once per call.
    Report(Box<WorkerReport>),
    Launched(LaunchAck),
}

/// Tickets with a worker in this process (for tests and diagnostics).
pub fn live_tickets(sup: &Supervisor) -> BTreeSet<i64> {
    sup.live.lock().unwrap().keys().copied().collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_layers_defaults_toml_and_env() {
        let mut c = SupervisorConfig::default();
        assert_eq!(c.max_parallel, 4);
        assert_eq!(c.timeout, Duration::from_secs(3600));
        c.apply_toml("[server]\nurl = \"x\"\n[workers]\nmax_parallel = 2\ntimeout_minutes = 0.5\nmax_turns = 9\n")
            .unwrap();
        assert_eq!(
            (c.max_parallel, c.timeout, c.max_turns),
            (2, Duration::from_secs(30), 9)
        );
        let env = |k: &str| match k {
            ENV_MAX_WORKERS => Some("6".to_owned()),
            ENV_WORKER_TIMEOUT_SECS => Some("5".to_owned()),
            _ => None,
        };
        c.apply_env(&env).unwrap();
        assert_eq!((c.max_parallel, c.timeout), (6, Duration::from_secs(5)));

        assert!(c.apply_toml("[workers]\nmax_parallel = 0\n").is_err());
        assert!(c.apply_toml("[workers]\nmaxparallel = 3\n").is_err());
        let bad = |k: &str| (k == ENV_MAX_WORKERS).then(|| "four".to_owned());
        assert!(c.apply_env(&bad).unwrap_err().contains(ENV_MAX_WORKERS));
    }

    /// AMP #2834: launched dispatch is opt-in. The default is the foreground, so `run -p` and
    /// every existing caller are unchanged; the chat asks for `launch` in config or the
    /// environment, and a typo is refused by name rather than silently doing the other thing.
    #[test]
    fn launched_dispatch_is_opt_in_and_a_bad_value_is_refused() {
        assert_eq!(SupervisorConfig::default().dispatch, DispatchMode::Await);
        assert_eq!(DispatchMode::Await.as_str(), "await");
        assert_eq!(DispatchMode::Launch.as_str(), "launch");

        let mut c = SupervisorConfig::default();
        c.apply_toml("[workers]\ndispatch = \"launch\"\n").unwrap();
        assert_eq!(c.dispatch, DispatchMode::Launch);
        // The environment wins over config.toml, as every other worker setting does.
        let env = |k: &str| (k == ENV_WORKER_DISPATCH).then(|| "await".to_owned());
        c.apply_env(&env).unwrap();
        assert_eq!(c.dispatch, DispatchMode::Await);

        let e = SupervisorConfig::default()
            .apply_toml("[workers]\ndispatch = \"eventually\"\n")
            .unwrap_err();
        assert!(e.contains("expected `await` or `launch`"), "{e}");
        let bad = |k: &str| (k == ENV_WORKER_DISPATCH).then(|| "later".to_owned());
        let e = SupervisorConfig::default().apply_env(&bad).unwrap_err();
        assert!(e.contains(ENV_WORKER_DISPATCH), "{e}");
        assert!(e.contains("expected `await` or `launch`"), "{e}");
    }

    /// A launched end's one line, and whether it needs the person's attention: a closed ticket
    /// doesn't, a run that ended any other way does, and a dispatch that never happened says so.
    #[test]
    fn a_launched_end_reads_as_one_line() {
        let report = |outcome, error: Option<&str>| WorkerReport {
            task_id: 12,
            agent_id: "amp:amp-worker-backend".into(),
            role: "amp-worker-backend".into(),
            outcome,
            dispatch_id: String::new(),
            started: true,
            returned: None,
            final_message: String::new(),
            error: error.map(str::to_owned),
            model: "wf/wf-1".into(),
            cwd: PathBuf::from("/r"),
            worktree: None,
            branch: None,
            workspace: Placement::Shared,
            session_file: None,
            turns: 0,
            tool_calls: 0,
            tool_errors: 0,
            usage: crate::events::UsageJson::default(),
            duration_ms: 0,
            notes: Vec::new(),
            totals: Totals::default(),
        };
        let end = |outcome, error: Option<&str>| LaunchEnd {
            task_id: 12,
            agent_id: "amp:amp-worker-backend".into(),
            result: Ok(report(outcome, error)),
        };

        let done = end(WorkerOutcome::Completed, None);
        assert_eq!(done.line(), "#12 completed by amp:amp-worker-backend");
        assert!(!done.needs_attention());
        let blocked = end(WorkerOutcome::Blocked, None);
        assert!(!blocked.needs_attention(), "a question is the worker's own");
        let failed = end(WorkerOutcome::Error, Some("the model failed"));
        assert!(failed.needs_attention());
        assert_eq!(
            failed.line(),
            "#12 error by amp:amp-worker-backend: the model failed"
        );
        let refused = LaunchEnd {
            task_id: 12,
            agent_id: "amp:amp-worker-backend".into(),
            result: Err("cancelled before the worker started".into()),
        };
        assert!(!refused.needs_attention(), "nothing was dispatched");
        assert_eq!(
            refused.line(),
            "#12 was not dispatched to amp:amp-worker-backend: cancelled before the worker started"
        );
    }

    /// AMP #2835: the completion queue holds successes and failures alike, and consuming an end
    /// takes it out of the queue for good — a second drain or wait never sees it again.
    #[tokio::test]
    async fn launch_ends_queue_successes_and_failures_once() {
        let ends = LaunchEnds::default();
        assert!(ends.is_empty());
        let end = |task_id, result| LaunchEnd {
            task_id,
            agent_id: "amp:amp-worker-backend".into(),
            result,
        };
        let ok = || {
            Ok(WorkerReport {
                task_id: 0,
                agent_id: "amp:amp-worker-backend".into(),
                role: "amp-worker-backend".into(),
                outcome: WorkerOutcome::Completed,
                dispatch_id: String::new(),
                started: true,
                returned: None,
                final_message: String::new(),
                error: None,
                model: "wf/wf-1".into(),
                cwd: PathBuf::from("/r"),
                worktree: None,
                branch: None,
                workspace: Placement::Shared,
                session_file: None,
                turns: 0,
                tool_calls: 0,
                tool_errors: 0,
                usage: crate::events::UsageJson::default(),
                duration_ms: 0,
                notes: Vec::new(),
                totals: Totals::default(),
            })
        };

        // One queue carries both a success and a failure, in the order they ended.
        ends.push(end(1, ok()));
        ends.push(end(2, Err("the slot was cancelled".into())));
        assert_eq!(ends.len(), 2);

        let drained = ends.drain();
        assert_eq!(
            drained.iter().map(|e| e.task_id).collect::<Vec<_>>(),
            vec![1, 2]
        );
        assert!(drained[0].result.is_ok(), "a completed run");
        assert!(drained[1].result.is_err(), "a dispatch that never ran");

        // Consumed exactly once: the queue is empty and stays that way.
        assert!(ends.is_empty());
        assert!(ends.drain().is_empty(), "a second drain sees nothing");

        // A wait returns what is queued and removes it too, so the two ways of consuming cannot
        // both hand out the same end.
        ends.push(end(3, ok()));
        assert_eq!(ends.next().await.task_id, 3);
        assert!(ends.is_empty(), "the wait took it out of the queue");
    }

    #[test]
    fn requests_accept_string_ids_and_reject_unknown_fields() {
        let r =
            parse_request(json!({"task_id": "#12", "agent_id": "amp:amp-worker-docs"})).unwrap();
        assert_eq!(r.task_id, 12);
        assert!(parse_request(json!({"task_id": 1, "agent_id": "x", "agnet": 1})).is_err());
        assert!(parse_request(json!({"agent_id": "x"})).is_err());
        assert!(parse_request(json!([1])).is_err());
    }

    #[test]
    fn closing_calls_only_count_for_this_ticket_and_only_when_they_succeed() {
        let watch = WorkerWatch {
            task_id: 7,
            totals: Arc::default(),
            max_turns: 2,
            budget: CancellationToken::new(),
            closing: Mutex::default(),
            closed: Mutex::default(),
            repeat_stop: Mutex::default(),
            feed: WorkerFeed::new(),
        };
        let start = |id: &str, name: &str, task: i64| AgentEvent::ToolExecutionStart {
            tool_call_id: id.into(),
            tool_name: name.into(),
            args: json!({"task_id": task}),
        };
        let end = |id: &str, name: &str, is_error: bool| AgentEvent::ToolExecutionEnd {
            tool_call_id: id.into(),
            tool_name: name.into(),
            result: AgentToolResult::text("x"),
            is_error,
        };
        watch.observe(&start("a", COMPLETE_TOOL, 8));
        watch.observe(&end("a", COMPLETE_TOOL, false));
        assert_eq!(*watch.closed.lock().unwrap(), None, "another ticket");
        watch.observe(&start("b", COMPLETE_TOOL, 7));
        watch.observe(&end("b", COMPLETE_TOOL, true));
        assert_eq!(*watch.closed.lock().unwrap(), None, "a failed call");
        watch.observe(&start("c", BLOCK_TOOL, 7));
        watch.observe(&end("c", BLOCK_TOOL, false));
        assert_eq!(*watch.closed.lock().unwrap(), Some(WorkerOutcome::Blocked));

        for _ in 0..2 {
            watch.observe(&AgentEvent::TurnStart);
        }
        assert!(!watch.budget.is_cancelled());
        watch.observe(&AgentEvent::TurnStart);
        assert!(watch.budget.is_cancelled(), "the third turn is over budget");
    }

    #[test]
    fn outcomes_serialize_as_snake_case_words() {
        for o in [
            WorkerOutcome::Completed,
            WorkerOutcome::Blocked,
            WorkerOutcome::Stopped,
            WorkerOutcome::Error,
            WorkerOutcome::Timeout,
            WorkerOutcome::Cancelled,
            WorkerOutcome::TurnBudget,
        ] {
            assert_eq!(serde_json::to_value(o).unwrap(), json!(o.as_str()));
        }
        assert!(WorkerOutcome::Blocked.closed_ticket());
        assert!(!WorkerOutcome::Stopped.closed_ticket());
    }

    #[test]
    fn a_closed_ticket_outlives_how_the_run_ended() {
        let stopped = |outcome, error: Option<&str>| Finish {
            outcome,
            error: error.map(str::to_owned),
            text: String::new(),
            session_file: None,
        };
        // Closed, then budget-stopped: keeps the closing outcome; the reason is a note.
        let mut notes = Vec::new();
        let f = stopped(
            WorkerOutcome::TurnBudget,
            Some("turn budget of 1 turns used up"),
        )
        .after_closing(Some(WorkerOutcome::Completed), &mut notes);
        assert_eq!((f.outcome, f.error), (WorkerOutcome::Completed, None));
        assert_eq!(
            notes,
            vec!["stopped after closing its ticket: turn_budget: turn budget of 1 turns used up"]
        );
        // Never closed: unchanged.
        let mut notes = Vec::new();
        let f = stopped(WorkerOutcome::Cancelled, Some("every worker was cancelled"))
            .after_closing(None, &mut notes);
        assert_eq!(f.outcome, WorkerOutcome::Cancelled);
        assert!(f.error.is_some() && notes.is_empty());
        // Already the closing outcome: no note.
        let f = stopped(WorkerOutcome::Blocked, None)
            .after_closing(Some(WorkerOutcome::Blocked), &mut notes);
        assert_eq!(f.outcome, WorkerOutcome::Blocked);
        assert!(notes.is_empty());
    }

    #[test]
    fn durations_read_naturally() {
        assert_eq!(human_duration(Duration::from_secs(3600)), "60 min");
        assert_eq!(human_duration(Duration::from_secs(90)), "90 s");
        assert_eq!(human_duration(Duration::from_millis(300)), "300 ms");
    }
}
