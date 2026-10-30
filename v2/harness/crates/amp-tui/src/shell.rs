//! The chat shell's controller: one agent harness, the [`ChatView`], the slash commands.
//!
//! It is terminal-free. The terminal loop ([`crate::terminal`]) and the headless driver
//! ([`Shell::drive_until_idle`], used by tests and `amp-harness --snapshot`) both feed it the same
//! three inputs: editor submissions ([`Shell::submit`]), rpi agent events from the session's
//! [`amp_core::events::ChannelEmitter`] ([`Shell::handle_event`]) and finished runs
//! ([`Shell::finish_run`]).
//!
//! Every prompt is `prompt_text` on the same harness, so the conversation carries on between
//! prompts. A run is spawned as its own task so the screen keeps updating while it streams.
//!
//! **AMP commands (M3-4, #2108).** `/amp`, `/ticket [id|none]`, `/workers`, `/questions`, `/plan`,
//! `/project [id]` open a panel above the editor ([`crate::overlay`]) drawn from the metadata
//! poller's snapshot; `/open [id]` opens the current (or given) ticket in the browser. `/ticket`
//! and `/project` switch the poller ([`MetaHandle::set_ticket`] / [`MetaHandle::set_project`]).
//! Their AMP calls (`amp_get_plan`, `amp_answer_question`) go through [`MetaHandle::call`] on the
//! poller's connection, never through the model, in spawned tasks whose results
//! [`Shell::poll`] applies, so the screen never waits on AMP.
//!
//! **`/server` (#2164).** `/server` lists the current AMP server and the saved ones; `/server
//! <name|address>` switches the session to it without a restart, through the binary's
//! [`ServerControl`]: the session's AMP connection, the model's AMP tools and system prompt, and
//! the metadata poller (a new one replaces the old) all move to the new server. It is refused while
//! a reply or a worker is running, since their calls would be cut off.
//!
//! Answering a question is a write: Enter on a question in `/questions` asks for the answer in
//! the editor, the panel then shows exactly what will be sent, and only `y` / Enter sends it.
//!
//! The transcript gets a marker line whenever the current ticket changes (switched, claimed, or
//! its state moved), and the footer's session name becomes `manager · #2108`. A current ticket
//! that is done stops being current once the /ticket panel isn't showing it (#2187).
//!
//! **`/connect` (#2163).** `/connect` lists the model providers and their keys (and picks up keys
//! stored in another terminal); `/connect <provider>` (or `/connect <id> <base-url> [api]` for a
//! custom endpoint) asks for the key. The key is read by the shell itself, not the editor: keys
//! and pastes go to a hidden buffer ([`Shell::secret_key`], [`Shell::paste`]) that is never drawn,
//! never added to the transcript, the editor's history or the session file. Enter hands it to the
//! harness's [`ProviderConnect`], which checks it with the provider, stores it in the OS keychain
//! and rebuilds the providers; the running harness takes them with `set_models`, so the chat uses
//! the new key without a restart.
//!
//! **Configuration panels (#2245).** `/mcp`, `/agents`, `/skills`, `/config` and the per-role
//! forms of `/model` (`/model roles`, `/model <role> [provider/model] [--save]`) go to the
//! binary's [`ConfigControl`] in the background; its [`ConfigReply`] opens an
//! [`overlay::InfoPanel`], adds lines to the transcript, and (`reload: true`) runs the `/reload`
//! path at once, so a change made from a panel reaches the session exactly as a CLI change does.
//! Enter and the panel's letter keys run slash commands (`/mcp restart github`), so pressing `r`
//! and typing the command behave the same.
//!
//! Quitting ([`Shell::quit`]) never drops a worker's run report (D8): with workers live it cancels
//! them, aborts the prompt and waits, bounded by [`Shell::quit_bound`], until each worker has
//! stopped and its `amp_report_run` has returned. Without workers it waits [`ABORT_GRACE`] for the
//! aborted prompt so its session file is complete.
//!
//! **Quitting with a worker running (#2204).** While a worker of this chat is live, the first quit
//! (Ctrl+C, also mid-reply, since stopping the reply stops the worker; Ctrl+C twice on the empty
//! editor; Ctrl+D; `/quit`) only warns ([`worker_quit_warning`]): "a worker is running on #190 —
//! quit anyway? it will be returned to ready". Another quit within [`QUIT_CONFIRM_FOR`] quits.
//! Esc and `/abort` still just stop the reply, with a note that the worker stops too. A stopped or abandoned worker's ticket is handed back to ready in
//! AMP (run reporting's `on_finish`; [`Supervisor::release_unfinished`] when quitting without
//! waiting), so it is not left in progress with nobody on it. Signals still quit at once.

use std::{
    future::Future,
    pin::Pin,
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::events::{RunEvent, Totals};
use amp_core::meta::{MetaHandle, Topics};
use amp_core::models::thinking_name;
use amp_core::supervisor::{DispatchMode, Supervisor};
use amp_core::tickets::Status;
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{Model, ThinkingLevel};
use rpi_harness::agent_harness::{AgentHarness, AgentLane, HarnessRunOutcome, RunResult};
use serde_json::{Value, json};
use tokio::sync::mpsc::{UnboundedReceiver, UnboundedSender, unbounded_channel};

use crate::amp_render::{self, ApprovalNotice};
use crate::history::{PromptHistory, Recall};
use crate::newline::NewlineMode;
use crate::overlay::{self, InfoPanel, ModelRow, PanelKind, PanelState, Stage, Target};
use crate::roster::{NoticeKind, Phase, WorkerEntry};
use crate::scroll::ScrollPos;
use crate::slash::MenuKey;
use crate::view::{ChatView, FooterInfo};
use crate::workers::{SessionView, Source, session_state, stop_question};

/// Opens a URL in the browser (default: login's guarded opener; tests record instead).
pub type Opener = Arc<dyn Fn(&str) -> Result<(), String> + Send + Sync>;

/// What `/server` lists and switches to (#2164). The binary implements it over config.toml and
/// the session's AMP connection; the shell only calls it.
pub trait ServerControl: Send + Sync {
    /// The current server and the saved ones, as notice text.
    fn describe(&self) -> String;
    /// Switches the session to `target`: a saved server's name, or a server address.
    fn switch(
        self: Arc<Self>,
        target: String,
    ) -> Pin<Box<dyn Future<Output = Result<ServerSwitched, String>> + Send>>;
}

/// A finished switch: what to tell the person, and the new server's metadata poller (which
/// replaces the old one in the header and the AMP commands).
pub struct ServerSwitched {
    pub notice: String,
    pub meta: Option<MetaHandle>,
}

/// How long a command's read (`/plan`) may take.
const READ_TIMEOUT: Duration = Duration::from_secs(15);
/// How long `amp_answer_question` may take.
const WRITE_TIMEOUT: Duration = Duration::from_secs(30);

/// A command's AMP call that finished (applied by [`Shell::poll`]).
enum ShellMsg {
    Plan {
        project_id: i64,
        result: Result<Value, String>,
        /// The blocked tickets that split the plan's `blocked` counts (`None` when the plan failed).
        blocked: Option<Result<Value, String>>,
    },
    Answered {
        ticket: i64,
        result: Result<Value, String>,
    },
    /// `/connect` finished; the new providers are already on the harness.
    Connected {
        result: Result<String, String>,
        models: Option<Vec<ModelChoice>>,
    },
    Server(Result<ServerSwitched, String>),
    /// `/reload` finished (`cause`: who asked for it).
    Reloaded {
        result: Result<ReloadReply, String>,
        cause: ReloadCause,
    },
    /// A configuration command finished (#2245); `command`: what was typed, for errors.
    Config {
        result: Result<ConfigReply, String>,
        command: String,
    },
    /// The file watcher saw the configuration change: reload (not counted in `in_flight`).
    ReloadRequest,
    /// The chat's `.amp.json` changed (#2383). Not counted in `in_flight`.
    ProjectLinked(ProjectLinked),
    /// A line from a background task the shell did not start ([`Notifier`]); not counted in
    /// `in_flight`.
    Notice {
        text: String,
        error: bool,
    },
    /// A remote link's call (#2373): start a prompt or stop the run. Not counted in `in_flight`.
    Remote(crate::remote::RemoteCall),
    /// A `/remote` command's call finished (#2376).
    RemoteDone(RemoteReply),
    /// The harness started or finished compacting the context (#2388). Not counted in
    /// `in_flight`.
    Compaction {
        active: bool,
        manual: bool,
    },
}

/// The shell's watch on its harness's event bus (#2388: compaction progress). rpi's watch
/// handle doesn't unsubscribe when dropped; this does.
struct BusWatch(rpi_harness::events::WatchHandle<()>);

impl Drop for BusWatch {
    fn drop(&mut self) {
        self.0.unsubscribe();
    }
}

/// Watches `harness`'s bus for compaction progress and sends it to the shell.
fn watch_compaction(harness: &AgentHarness, tx: UnboundedSender<ShellMsg>) -> BusWatch {
    let mut handle = harness.events().watch(|| ());
    handle.start(Arc::new(move |event| {
        if let rpi_harness::events::HarnessEvent::CompactionProgress(p) = event {
            let _ = tx.send(ShellMsg::Compaction {
                active: p.active,
                manual: p.manual,
            });
        }
    }));
    BusWatch(handle)
}

/// What a `/remote` command's call returned (#2376).
enum RemoteReply {
    Enabled(Result<amp_core::remote::Enabled, amp_core::remote::RemoteError>),
    Disabled,
    Pair(Result<(String, i64), String>),
}

/// Carries the remote link's calls to the shell (#2373).
#[derive(Clone)]
pub(crate) struct RemoteSender(UnboundedSender<ShellMsg>);

impl RemoteSender {
    /// False when the shell is gone.
    pub(crate) fn send(&self, call: crate::remote::RemoteCall) -> bool {
        self.0.send(ShellMsg::Remote(call)).is_ok()
    }
}

/// Puts lines in the transcript from a task the shell did not start (#2178: the chat's retry
/// while AMP is unreachable). [`Shell::poll`] applies them.
#[derive(Clone)]
pub struct Notifier(UnboundedSender<ShellMsg>);

impl Notifier {
    pub fn notice(&self, text: impl Into<String>) {
        let _ = self.0.send(ShellMsg::Notice {
            text: text.into(),
            error: false,
        });
    }

    pub fn error(&self, text: impl Into<String>) {
        let _ = self.0.send(ShellMsg::Notice {
            text: text.into(),
            error: true,
        });
    }

    /// Asks the shell to `/reload` (#2242: the configuration files changed). Ignored when the
    /// shell has no [`ReloadControl`].
    pub fn reload(&self) {
        let _ = self.0.send(ShellMsg::ReloadRequest);
    }

    /// The chat's folder is linked to another project now (#2383: the manager wrote
    /// `.amp.json`); see [`ProjectLinked`].
    pub fn project_linked(&self, link: ProjectLinked) {
        let _ = self.0.send(ShellMsg::ProjectLinked(link));
    }
}

/// The chat's folder is linked to another project now (#2383): what changes on screen.
#[derive(Debug, Clone, Default)]
pub struct ProjectLinked {
    /// The header, status line and board follow it (`None`: no project).
    pub project_id: Option<i64>,
    /// The `/project` panel's note for a parent folder's marker (#2356).
    pub marker: Option<(i64, String)>,
    /// A notice for the transcript.
    pub notice: String,
    /// The banner's project text: (what it says now, what it says from now on).
    pub banner: Option<(String, String)>,
}

/// What `/connect` did: a message for the transcript (never a secret), and the providers and
/// models the chat has now.
pub struct ConnectReply {
    pub text: String,
    pub providers: Vec<Arc<dyn rpi_ai::Provider>>,
    pub models: Vec<ModelChoice>,
}

pub type ConnectFuture = Pin<Box<dyn Future<Output = Result<ConnectReply, String>> + Send>>;

/// The harness side of `/connect`: lists, checks and stores provider keys (amp-harness).
pub trait ProviderConnect: Send + Sync {
    /// `/connect`: the providers and where their keys come from (never a key). Also picks up
    /// keys and providers added elsewhere since the chat started.
    fn list(&self) -> ConnectFuture;
    /// `/connect <args>`: checks the arguments and returns what to ask for ("OpenRouter API key").
    /// An empty prompt means nothing needs asking (#2275: only the roles change for a provider
    /// that has its key): [`ProviderConnect::connect`] then runs at once with an empty key.
    fn prepare(&self, args: &str) -> Result<String, String>;
    /// Checks `key` with the provider, stores it, and rebuilds the providers.
    fn connect(&self, args: &str, key: String) -> ConnectFuture;
}

/// What `/reload` did (#2242): a summary for the transcript, and what the chat takes from it.
pub struct ReloadReply {
    /// One line per change (never a secret); empty when nothing changed.
    pub lines: Vec<String>,
    /// The providers the harness uses from now on.
    pub providers: Vec<Arc<dyn rpi_ai::Provider>>,
    /// What `/model` can switch to now.
    pub models: Vec<ModelChoice>,
    /// The chat's role got another model in the config: switch this chat to it
    /// (`provider/model`, one of `models`).
    pub switch_to: Option<String>,
    /// The chat's role got another model this process can't use (its provider has no key
    /// here): why the chat stays on its model (#2334), e.g. `manager moved to x/y, but x has
    /// no key here (set X_API_KEY, then /reload)`.
    pub stays: Option<String>,
    /// Lines to show as warnings (a permission change that needs a restart).
    pub warnings: Vec<String>,
}

pub type ReloadFuture = Pin<Box<dyn Future<Output = Result<ReloadReply, String>> + Send>>;

/// Who asked for a reload: it decides what the transcript says.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ReloadCause {
    /// `/reload`.
    Command,
    /// The file watcher ([`Notifier::reload`]): quiet when nothing changed.
    Watcher,
    /// A change made from a configuration panel (#2245): quiet start, "applied:" lines.
    Panel,
}

/// What a configuration command did (#2245).
#[derive(Default)]
pub struct ConfigReply {
    /// A panel to open (it replaces the open one).
    pub panel: Option<Arc<InfoPanel>>,
    /// Select the row with this key in the panel.
    pub select: Option<String>,
    /// Lines for the transcript (never a secret).
    pub lines: Vec<String>,
    /// Lines for the transcript, as errors.
    pub errors: Vec<String>,
    /// A one-line note at the bottom of the open panel (`true`: an error).
    pub note: Option<(String, bool)>,
    /// The configuration files changed: run the `/reload` path now so the session takes it.
    pub reload: bool,
}

impl ConfigReply {
    /// Opens `panel`.
    pub fn panel(panel: InfoPanel) -> Self {
        Self {
            panel: Some(Arc::new(panel)),
            ..Self::default()
        }
    }

    /// A line in the transcript and the open panel's note.
    pub fn done(line: impl Into<String>) -> Self {
        let line = line.into();
        Self {
            note: Some((line.clone(), false)),
            lines: vec![line],
            ..Self::default()
        }
    }
}

pub type ConfigFuture = Pin<Box<dyn Future<Output = Result<ConfigReply, String>> + Send>>;

/// The harness side of the configuration panels (#2245): `/mcp`, `/agents`, `/skills`,
/// `/config`, and `/model` for a role. `name` is the command without its slash, `args` the rest.
/// Writers change the same files the CLI does and ask for a reload ([`ConfigReply::reload`]).
pub trait ConfigControl: Send + Sync {
    fn run(&self, name: &str, args: &str) -> ConfigFuture;
}

/// The harness side of `/reload` (amp-harness): re-reads agents, skills, models and MCP servers
/// changed by a CLI in another terminal and brings the chat's session up to date.
pub trait ReloadControl: Send + Sync {
    fn reload(&self) -> ReloadFuture;
}

/// The block a manager turn opens with when workers ended since the last one (AMP #2857): one
/// item per end, from [`amp_core::supervisor::LaunchEnd::summary`] (its line, then its notes —
/// the #2802 hand-back notice among them — and the worker's trimmed final message, AMP #2883).
/// `None` when nothing ended, so a turn with nothing to report is the prompt as typed. The header
/// makes no claim about AMP of its own: a hand-back can fail, so each entry says where its
/// ticket stands.
fn worker_summaries(ends: &[amp_core::supervisor::LaunchEnd]) -> Option<String> {
    if ends.is_empty() {
        return None;
    }
    let mut block = String::from(
        "[worker summaries: runs you launched that have ended since your last turn. Each entry \
         says where its ticket stands: a worker that completed or blocked closed its own ticket, \
         and the notes say whether any other ticket was returned to ready or could not be. Act \
         on those notes and on any question in a final message.]",
    );
    for end in ends {
        block.push_str("\n- ");
        block.push_str(&end.summary());
    }
    Some(block)
}

/// A key being typed for `/connect` (never drawn).
struct SecretEntry {
    args: String,
    buf: String,
}

/// A finished `prompt_text` call (`Err` also covers a run task that panicked).
pub type RunDone = Result<RunResult, String>;

/// What the shell shows about the session it drives.
#[derive(Debug, Clone, Default)]
pub struct ShellInfo {
    /// The role id (`amp-manager`).
    pub role: String,
    pub provider: String,
    pub model_id: String,
    pub thinking: ThinkingLevel,
    /// What the config asked for, re-applied (clamped) when `/model` switches models.
    pub requested_thinking: Option<ThinkingLevel>,
    pub context_window: u64,
    pub cwd: String,
    pub git_branch: Option<String>,
    /// Lines of the welcome banner at the top of the transcript.
    pub banner: Vec<String>,
    /// The session file as `/usage` names it (`~/.local/state/...jsonl`); `None` keeps the
    /// transcript in memory (`--no-session`). The banner only shows its short id (#2186).
    pub session_file: Option<String>,
}

impl ShellInfo {
    /// The footer; with a current ticket its session name reads `manager · #2108`.
    fn footer(&self, ticket: Option<i64>) -> FooterInfo {
        let role = short_role(&self.role);
        FooterInfo {
            model: self.model_id.clone(),
            thinking: thinking_name(self.thinking).to_owned(),
            context_window: self.context_window,
            cwd: self.cwd.clone(),
            git_branch: self.git_branch.clone(),
            session_name: Some(match ticket {
                Some(id) => format!("{role} · #{id}"),
                None => role.to_owned(),
            }),
        }
    }

    fn model_ref(&self) -> String {
        format!("{}/{}", self.provider, self.model_id)
    }
}

/// A model `/model` can switch to: `provider/model` plus the rpi model.
#[derive(Debug, Clone)]
pub struct ModelChoice {
    pub reference: String,
    pub model: Model,
}

/// How long quitting waits for an aborted prompt with no live workers (its session file).
pub const ABORT_GRACE: Duration = Duration::from_secs(3);

/// On top of the supervisor's `abort_grace`: the run report's own timeout (`amp_report_run`, 30 s
/// in amp-core's run_report) plus a margin. With the default 10 s grace, quitting waits 45 s.
pub const REPORT_ALLOWANCE: Duration = Duration::from_secs(35);

/// How [`Shell::quit`] ended.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum QuitOutcome {
    /// Nothing was running.
    Idle,
    /// The prompt ended and every worker stopped and was reported.
    Settled,
    /// The caller skipped the wait (a second Ctrl-C).
    Skipped,
    /// The bound passed first.
    TimedOut,
}

/// The transcript line a run the person stopped ends with: dim, no error colour (#2181).
pub const STOPPED: &str = "Stopped.";

/// What the spinner line says, for [`ESC_HINT_FOR`], after Esc closed a panel, the `/` menu or a
/// worker's session during a run (#2181, #2354). With this chat's workers live it names them
/// ([`esc_hint`]).
pub const ESC_HINT: &str = "Esc again to stop the reply";

/// At most this many prompts wait for the running reply, here as on a paired device (AMP #2889).
pub use amp_core::remote::link::QUEUE_MAX;

/// The line a prompt typed during a reply leaves (AMP #2889): its place in the queue, that it
/// runs on its own once the reply ends, and that Esc drops it (as a paired device's queue).
pub fn queued_notice(position: usize, text: &str) -> String {
    let preview: String = text.replace(['\n', '\t'], " ").chars().take(80).collect();
    let more = if text.chars().count() > 80 { "…" } else { "" };
    format!(
        "queued #{position} · runs after this reply: \"{preview}{more}\" (Esc stops the reply \
         and drops the queue)"
    )
}

/// What a stopped reply says about the prompts it drops (AMP #2889).
pub fn dropped_notice(n: usize) -> String {
    if n == 1 {
        "dropped the queued prompt (↑ recalls it)".to_owned()
    } else {
        format!("dropped {n} queued prompts (↑ recalls them)")
    }
}

/// [`ESC_HINT`], naming this chat's live workers, which stopping the reply stops too (#2354).
pub fn esc_hint(tickets: &[i64]) -> String {
    match tickets.len() {
        0 => ESC_HINT.to_owned(),
        1 => format!("{ESC_HINT} and the worker on #{}", tickets[0]),
        _ => format!("{ESC_HINT} and the workers on {}", ticket_list(tickets)),
    }
}

/// How loud a launched worker's end is in the transcript (AMP #2861).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum EndLevel {
    Info,
    Warn,
    Error,
}

/// The notice for one launched worker's end (AMP #2861): a completed ticket is plain news, one
/// the worker blocked or stopped on wants the person, and a failure (or a dispatch that never
/// happened) is an error. The text is [`amp_core::supervisor::LaunchEnd::line`], as the manager
/// reads it at its next turn.
fn launch_end_notice(end: &amp_core::supervisor::LaunchEnd) -> (EndLevel, String) {
    use amp_core::supervisor::WorkerOutcome as O;
    let level = match &end.result {
        Ok(report) => match report.outcome {
            O::Completed => EndLevel::Info,
            O::Blocked | O::Stopped | O::Cancelled => EndLevel::Warn,
            O::Error | O::Timeout | O::TurnBudget => EndLevel::Error,
        },
        Err(_) => EndLevel::Error,
    };
    (level, format!("worker ended: {}", end.line()))
}

/// A launch that ended with no worker session behind it (AMP #2862): what its
/// [`amp_core::supervisor::LaunchEnd`] said.
#[derive(Debug, Clone)]
struct EndedLaunch {
    agent_id: String,
    /// The launch end's `Err` text: [`amp_core::supervisor::LAUNCH_CRASHED`] and what happened
    /// to the ticket, or why it was never dispatched.
    why: String,
    at: std::time::SystemTime,
}

/// The live workers in `sessions` with their phases, by ticket: what the roster compares to
/// decide whether it is out of date (AMP #2862).
fn live_phases(
    sessions: &[amp_core::supervisor::WorkerSessionInfo],
) -> Vec<(i64, amp_core::supervisor::WorkerPhase)> {
    let mut live: Vec<_> = sessions
        .iter()
        .filter_map(|s| s.phase.map(|p| (s.task_id, p)))
        .collect();
    live.sort_by_key(|(id, _)| *id);
    live
}

/// The roster row of a launch that ended with no session (AMP #2862): a finished worker of this
/// chat whose outcome says `crashed` or `not_dispatched`, and, for a crash, whether its ticket
/// went back to ready (the `/workers` panel shows that as for any returned worker).
fn ended_launch_entry(task_id: i64, end: &EndedLaunch) -> WorkerEntry {
    use amp_core::supervisor::LAUNCH_CRASHED;
    let crashed = end.why.starts_with(LAUNCH_CRASHED);
    let returned_to = format!("#{task_id} was returned to ready");
    let not_returned = format!("#{task_id} could not be returned to ready: ");
    let still_open = format!("#{task_id} is still in progress in AMP");
    let returned = if !crashed {
        None
    } else if end.why.contains(&returned_to) {
        Some(Ok(()))
    } else if let Some(at) = end.why.find(&not_returned) {
        Some(Err(end.why[at + not_returned.len()..].trim().to_owned()))
    } else if end.why.contains(&still_open) {
        Some(Err("still in progress in AMP".to_owned()))
    } else {
        None
    };
    WorkerEntry {
        task_id,
        agent_id: end.agent_id.clone(),
        phase: Phase::Finished,
        state: format!(
            "{} {}",
            if crashed { "crashed" } else { "not dispatched" },
            crate::roster::ago(end.at, std::time::SystemTime::now())
        ),
        outcome: Some(if crashed { "crashed" } else { "not_dispatched" }.to_owned()),
        returned,
        local: true,
        ..WorkerEntry::default()
    }
}

/// `#1, #2, #3`, or `#1, #2, #3 +2 more` past three (it has to fit the spinner line).
fn ticket_list(tickets: &[i64]) -> String {
    let shown = tickets
        .iter()
        .take(3)
        .map(|id| format!("#{id}"))
        .collect::<Vec<_>>()
        .join(", ");
    match tickets.len() {
        n if n > 3 => format!("{shown} +{} more", n - 3),
        _ => shown,
    }
}

/// How long [`ESC_HINT`] stays next to the spinner.
pub const ESC_HINT_FOR: Duration = Duration::from_secs(2);

/// What the empty editor says after a first Ctrl+C there: a second one within
/// [`QUIT_HINT_FOR`] quits (#2184).
pub const QUIT_HINT: &str = "Press Ctrl+C again to quit";

/// How long a second Ctrl+C on the empty editor still quits.
pub const QUIT_HINT_FOR: Duration = Duration::from_millis(1500);

/// How long, after [`worker_quit_warning`], the same quit again goes through (#2204).
pub const QUIT_CONFIRM_FOR: Duration = Duration::from_secs(10);

/// What the empty editor says while a quit is waiting to be confirmed.
pub const QUIT_CONFIRM_HINT: &str = "Quit again to stop the worker and leave";

/// How long quitting without waiting still gives AMP to take the abandoned tickets back.
pub const RELEASE_BOUND: Duration = Duration::from_secs(5);

/// The warning the first quit shows while workers of this chat are live (#2204).
pub fn worker_quit_warning(tickets: &[i64]) -> String {
    let ids = tickets
        .iter()
        .map(|id| format!("#{id}"))
        .collect::<Vec<_>>()
        .join(", ");
    let (who, it) = if tickets.len() == 1 {
        ("a worker is", "it")
    } else {
        ("workers are", "they")
    };
    format!(
        "{who} running on {ids} — quit anyway? {it} will be returned to ready \
         (Ctrl+C, Ctrl+D or /quit again to quit)"
    )
}

/// The note when stopping the reply also stops this chat's workers (#2204).
pub fn worker_stop_notice(tickets: &[i64]) -> String {
    let ids = tickets
        .iter()
        .map(|id| format!("#{id}"))
        .collect::<Vec<_>>()
        .join(", ");
    if tickets.len() == 1 {
        format!("stopping the worker on {ids} too; its ticket goes back to ready")
    } else {
        format!("stopping the workers on {ids} too; their tickets go back to ready")
    }
}

/// What stopping the reply does to this chat's live workers (AMP #2858), for
/// [`reply_stop_notice`] and the Esc hint.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ReplyStop {
    /// Every live worker of this chat.
    pub live: Vec<i64>,
    /// Launched during the reply being stopped and still waiting for a slot (not dispatched):
    /// the reply's tool-call signal still cancels these. Launch mode only.
    pub cancelled: Vec<i64>,
    /// Live, not cancelled, and still waiting for a slot: they stay queued. Launch mode only.
    pub queued: Vec<i64>,
}

/// The note when Esc, `/abort`, `/stop` or a remote Stop stops the reply (AMP #2858), or `None`
/// when no worker of this chat is live.
///
/// - [`DispatchMode::Await`]: the workers run inside the reply's `amp_run_ticket` calls, so
///   stopping the reply stops them: [`worker_stop_notice`], as before.
/// - [`DispatchMode::Launch`]: a dispatched worker is the supervisor's and keeps running (only
///   `/workers` or quitting stops it); a launch from this very reply that is still waiting for a
///   slot is cancelled before AMP ever hears of it.
pub fn reply_stop_notice(mode: DispatchMode, stop: &ReplyStop) -> Option<String> {
    if stop.live.is_empty() {
        return None;
    }
    if mode == DispatchMode::Await {
        return Some(worker_stop_notice(&stop.live));
    }
    let mut parts = vec!["stopping the reply".to_owned()];
    match stop.cancelled.as_slice() {
        [] => {}
        [one] => parts.push(format!(
            "#{one} was still waiting for a slot, so its launch is cancelled and it is not \
             dispatched"
        )),
        many => parts.push(format!(
            "{} were still waiting for a slot, so their launches are cancelled and they are not \
             dispatched",
            hash_ids(many)
        )),
    }
    let kept: Vec<i64> = stop
        .live
        .iter()
        .copied()
        .filter(|id| !stop.cancelled.contains(id))
        .collect();
    match kept.as_slice() {
        [] => {}
        [one] => {
            let what = if stop.queued.contains(one) {
                "stays queued for a slot"
            } else {
                "keeps running"
            };
            parts.push(format!("the worker on #{one} {what} (/workers to stop it)"));
        }
        many => {
            let waiting: Vec<i64> = many
                .iter()
                .copied()
                .filter(|id| stop.queued.contains(id))
                .collect();
            let mut line = format!(
                "{} workers keep running on {} (/workers to stop one)",
                many.len(),
                hash_ids(many)
            );
            match waiting.as_slice() {
                [] => {}
                [one] => line.push_str(&format!("; #{one} is still queued for a slot")),
                w => line.push_str(&format!("; {} are still queued for a slot", hash_ids(w))),
            }
            parts.push(line);
        }
    }
    Some(parts.join("; "))
}

fn hash_ids(ids: &[i64]) -> String {
    ids.iter()
        .map(|id| format!("#{id}"))
        .collect::<Vec<_>>()
        .join(", ")
}

/// After Ctrl+D scrolled the transcript (or was held at its end), Ctrl+D on the empty editor
/// doesn't quit until it has been left alone this long, so scrolling down with it (or holding
/// it) stops at the latest output instead of quitting (#2185).
pub const CTRL_D_SETTLE: Duration = Duration::from_secs(1);

/// What Ctrl+D on the empty editor did ([`Shell::ctrl_d`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CtrlD {
    /// The transcript was scrolled up: it went down half a page.
    Scrolled,
    /// At the latest output within [`CTRL_D_SETTLE`] of scrolling with Ctrl+D: nothing.
    Held,
    /// A worker of this chat is live: the quit only warned ([`worker_quit_warning`]).
    Confirm,
    /// A run was going: it was asked to stop.
    Stopped,
    /// Quit.
    Quit,
}

/// What Ctrl+C did ([`Shell::ctrl_c`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CtrlC {
    /// A run was going: it was asked to stop.
    Stopped,
    /// The editor had text: it was cleared.
    Cleared,
    /// The editor was empty: it now says [`QUIT_HINT`] for [`QUIT_HINT_FOR`].
    Hint,
    /// A second Ctrl+C on the empty editor within [`QUIT_HINT_FOR`], with a worker of this chat
    /// live: the quit only warned ([`worker_quit_warning`]); one more Ctrl+C quits.
    Confirm,
    /// A second Ctrl+C on the empty editor within [`QUIT_HINT_FOR`]: quit.
    Quit,
}

/// What stopping the wait for plan approval says (#2425).
pub const STOPPED_WAITING: &str = "Stopped waiting for plan approval. The manager carries on when \
                                   you reply.";

/// The spinner's own message while a run is going.
const WORKING: &str = "Working…";

/// What the caller does after a submission.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Submit {
    Continue,
    Quit,
}

pub struct Shell {
    view: ChatView,
    harness: Arc<AgentHarness>,
    info: ShellInfo,
    models: Vec<ModelChoice>,
    totals: Totals,
    prompts: u32,
    /// A model chosen in the `/model` picker, switched to by [`Shell::key`] / [`Shell::press`]
    /// (switching is async; the panel keys are not).
    pending_model: Option<String>,
    running: bool,
    done_tx: UnboundedSender<RunDone>,
    /// The AMP metadata poller (#2104), if the caller started one.
    meta: Option<MetaHandle>,
    opener: Opener,
    /// The clipboard and browser for `/remote`'s pairing link (#2423): off until the terminal
    /// loop turns the real one on ([`Shell::attach_terminal`]); tests set recording stand-ins.
    desktop: crate::clipboard::Desktop,
    msgs_tx: UnboundedSender<ShellMsg>,
    msgs_rx: UnboundedReceiver<ShellMsg>,
    /// Command calls still running.
    in_flight: usize,
    /// The `task_id` argument of AMP tool calls still running, by call id (#2226): a refusal's
    /// ticket is the call's own argument, not a number parsed out of the error's text.
    call_tickets: std::collections::HashMap<String, i64>,
    /// The snapshot revision last looked at, and the ticket (id, state) last marked.
    seen_revision: u64,
    marked: Option<(i64, Option<Status>)>,
    footer_ticket: Option<i64>,
    /// The completed ticket this shell last stopped tracking (so it asks the poller once).
    cleared_done: Option<i64>,
    /// The manager's worker supervisor, so quitting can cancel and wait for its workers.
    supervisor: Option<Arc<Supervisor>>,
    /// Copies of the supervisor's launched ends as they land (AMP #2861), shown as notices on the
    /// tick. Display only: the queue itself is drained at the next turn's start (#2857).
    launch_watch: Option<tokio::sync::broadcast::Receiver<amp_core::supervisor::LaunchEnd>>,
    /// The supervisor's live workers and their phases as the roster last showed them (AMP
    /// #2862): an agent event refreshes the roster when they changed, whatever tool ended.
    roster_live: Vec<(i64, amp_core::supervisor::WorkerPhase)>,
    /// Launches that ended with no session behind them (AMP #2862): the worker task crashed
    /// (#2885), or the launch was never dispatched. The supervisor keeps no finished session for
    /// these, so the roster keeps them from their launch end.
    ended_launches: std::collections::BTreeMap<i64, EndedLaunch>,
    /// Tests: the worker sessions the roster reads, in place of a supervisor's.
    #[cfg(test)]
    test_sessions: Option<Vec<amp_core::supervisor::WorkerSessionInfo>>,
    /// Tickets with a live worker when the running reply started (AMP #2858): not this reply's
    /// launches, so stopping it leaves them alone.
    turn_live: Vec<i64>,
    /// `/connect`'s harness side, if the caller gave one.
    connect: Option<Arc<dyn ProviderConnect>>,
    /// A key being typed for `/connect`.
    secret: Option<SecretEntry>,
    /// `/server` (#2164), if the caller gave one.
    server: Option<Arc<dyn ServerControl>>,
    /// A `/server` switch is running.
    switching: bool,
    /// `/reload` (#2242), if the caller gave one.
    reload: Option<Arc<dyn ReloadControl>>,
    /// A reload is running; `reload_again` asks for one more when it ends.
    reloading: bool,
    reload_again: Option<ReloadCause>,
    /// The configuration panels (#2245), if the caller gave them.
    config: Option<Arc<dyn ConfigControl>>,
    /// A slash command chosen in a configuration panel (Enter or a letter key), run by
    /// [`Shell::key`] / [`Shell::press`] (commands are async; the panel keys are not).
    pending_command: Option<String>,
    /// Until when the spinner line shows [`ESC_HINT`].
    esc_hint_until: Option<Instant>,
    /// Until when a second Ctrl+C on the empty editor quits ([`QUIT_HINT`]).
    quit_hint_until: Option<Instant>,
    /// Until when Ctrl+D at the latest output doesn't quit ([`CTRL_D_SETTLE`]).
    ctrl_d_settle_until: Option<Instant>,
    /// Until when a quit goes through although workers are live ([`QUIT_CONFIRM_FOR`]).
    quit_confirm_until: Option<Instant>,
    /// What quitting without waiting did with the abandoned tickets ([`Shell::released`]).
    released: Vec<(i64, Result<(), String>)>,
    /// Whether the terminal reports the mouse to us (wheel scrolling, the pill's click). Off,
    /// the terminal's own click-drag selection works without a modifier (`[tui] mouse`, #2185).
    mouse_capture: bool,
    /// How this terminal starts a new line (#2349): the empty editor's hint and `/help` name the
    /// key that works.
    newline: NewlineMode,
    /// Worker sessions opened from the chat (#2252), by ticket.
    sessions: std::collections::BTreeMap<i64, SessionView>,
    /// The worker session shown instead of the manager's transcript.
    viewing: Option<i64>,
    /// A stop waiting for its `y` (#2252).
    stop_confirm: Option<i64>,
    /// The person clicked into the editor while a panel was open (#2344): the editor has the
    /// keys (↑↓ and Enter included) until the panel closes or something is sent.
    editor_clicked: bool,
    /// Who stops workers from this chat ([`Shell::set_person`]); else the AMP user's email.
    person: Option<String>,
    /// The manager's scroll position while a worker's session is shown (#2347).
    scroll_back: Option<ScrollPos>,
    /// The workers the transcript has a notice for, and whether each was live then (#2347).
    noticed: std::collections::BTreeMap<i64, bool>,
    /// Workers seen running (AMP has their dispatch): a new one refreshes the board, so the
    /// status line's Ready count follows the dispatch rather than the next poll (#2355).
    running_seen: std::collections::BTreeSet<i64>,
    /// The screen's size, for Ctrl+T to find the thinking in sight ([`Shell::set_screen`]).
    screen: Option<(usize, usize)>,
    /// The prompts sent in this chat, for ↑↓ in the editor (#2348).
    history: PromptHistory,
    /// The running prompt's run id in the manager feed (#2370).
    run_id: Option<String>,
    /// Prompts typed here while a reply runs (AMP #2889), oldest first: each runs as its own
    /// turn when the reply before it ends, as the remote link's follow-up queue does
    /// ([`amp_core::remote::link::QUEUE_MAX`] at most). A stopped reply drops them.
    prompt_queue: std::collections::VecDeque<String>,
    /// The worker chips last sent to the manager feed (#2370).
    fed_workers: Option<amp_core::chat_event::ChatEvent>,
    /// The status the remote link reports (#2373), refreshed on every poll once it exists.
    remote_status: Option<Arc<std::sync::Mutex<amp_core::remote::HostStatus>>>,
    /// `/remote` (#2376), once the caller gave the chat its remote link.
    remote: Option<crate::remote_ui::RemoteUi>,
    /// Compaction progress from the harness (#2388).
    _compaction: BusWatch,
    /// The tool call the model is writing right now (#2384), for the spinner line.
    streaming: Option<StreamingCall>,
    /// `[manager] auto_resume_on_approval` (#2425): wait for a person's plan approval after a
    /// manager turn that asked for it, and start the next turn when it comes.
    auto_resume: bool,
    /// This turn planned or looked at the plan ([`amp_core::approval_wait::touches_plan`]).
    turn_plan: bool,
    /// This turn's plan-approval refusals.
    turn_refusals: Vec<amp_core::approval::Refusal>,
    /// The wait for a person's plan approval (#2425), between turns.
    approval_wait: Option<amp_core::approval_wait::ApprovalWait>,
}

/// A tool call whose arguments are still streaming (#2384).
struct StreamingCall {
    index: usize,
    name: String,
    /// The arguments so far (partial JSON).
    json: String,
    /// When the spinner line last changed for it: re-parsing the arguments is throttled.
    shown_at: Instant,
}

/// How often the spinner line follows a streaming tool call (parsing its partial arguments).
const STREAMING_EVERY: Duration = Duration::from_millis(250);

impl Shell {
    /// The shell and the receiver its runs finish on (select on it next to the agent events).
    pub fn new(
        harness: Arc<AgentHarness>,
        info: ShellInfo,
        models: Vec<ModelChoice>,
    ) -> (Self, UnboundedReceiver<RunDone>) {
        let (done_tx, done_rx) = unbounded_channel();
        let (msgs_tx, msgs_rx) = unbounded_channel();
        let compaction = watch_compaction(&harness, msgs_tx.clone());
        let view = ChatView::new();
        view.set_footer(&info.footer(None));
        view.add_banner(&info.banner);
        (
            Self {
                view,
                harness,
                info,
                models,
                totals: Totals::default(),
                prompts: 0,
                pending_model: None,
                running: false,
                prompt_queue: std::collections::VecDeque::new(),
                done_tx,
                meta: None,
                opener: Arc::new(|url: &str| amp_mcp::oauth::open_browser(url)),
                desktop: crate::clipboard::Desktop::off(),
                msgs_tx,
                msgs_rx,
                in_flight: 0,
                call_tickets: Default::default(),
                seen_revision: 0,
                marked: None,
                footer_ticket: None,
                cleared_done: None,
                supervisor: None,
                launch_watch: None,
                roster_live: Vec::new(),
                ended_launches: Default::default(),
                #[cfg(test)]
                test_sessions: None,
                turn_live: Vec::new(),
                connect: None,
                secret: None,
                server: None,
                switching: false,
                reload: None,
                reloading: false,
                reload_again: None,
                config: None,
                pending_command: None,
                esc_hint_until: None,
                quit_hint_until: None,
                ctrl_d_settle_until: None,
                quit_confirm_until: None,
                released: Vec::new(),
                mouse_capture: true,
                newline: NewlineMode::default(),
                sessions: Default::default(),
                viewing: None,
                stop_confirm: None,
                editor_clicked: false,
                person: None,
                scroll_back: None,
                noticed: Default::default(),
                running_seen: Default::default(),
                screen: None,
                history: PromptHistory::new(),
                run_id: None,
                fed_workers: None,
                remote_status: None,
                remote: None,
                _compaction: compaction,
                streaming: None,
                auto_resume: true,
                turn_plan: false,
                turn_refusals: Vec::new(),
                approval_wait: None,
            },
            done_rx,
        )
    }

    /// Sends the chat to the manager's feed (#2370): prompts with their origin, run starts and
    /// outcomes, every notice line, approval refusals and the worker chips. The session's rpi
    /// events reach the feed through its fan-out emitter, not through the shell.
    pub fn set_feed(&mut self, feed: Option<Arc<amp_core::manager_feed::ManagerFeed>>) {
        self.view.set_feed(feed);
        self.fed_workers = None;
    }

    /// Sends a prompt to the agent as a user turn, never as a slash command (#2370: the remote
    /// link's way in). False, and nothing is sent, while a run or a server switch is going on.
    pub fn send_prompt(&mut self, text: &str, origin: amp_core::chat_event::PromptOrigin) -> bool {
        self.send_prompt_as(text, origin, None)
    }

    /// [`Shell::send_prompt`] with the prompt id the feed's `user_message` gets (#2373: the
    /// remote link's ids, so a queued prompt keeps the id its `ack` gave it); `None`: `p_N`.
    /// A remote prompt reaches the model tagged `[remote: <device name>]` (R-M16); the feed and
    /// the transcript show the plain text.
    pub fn send_prompt_as(
        &mut self,
        text: &str,
        origin: amp_core::chat_event::PromptOrigin,
        prompt_id: Option<String>,
    ) -> bool {
        let text = text.trim();
        if text.is_empty() || self.running || self.switching {
            return false;
        }
        // #2376: a prompt from a paired device says so above its bubble. #2391 (L2): its text
        // came over the network, so escape sequences and invisible or bidi characters are
        // dropped from what shows here; the model gets the text as sent.
        match &origin {
            amp_core::chat_event::PromptOrigin::Remote { device_name, .. } => {
                use amp_core::remote::pairing::{
                    sanitize_for_terminal, sanitize_text_for_terminal,
                };
                let tag = format!("[remote: {}]", sanitize_for_terminal(device_name, 64));
                self.view
                    .add_user_tagged(&sanitize_text_for_terminal(text), Some(tag));
            }
            amp_core::chat_event::PromptOrigin::Local => self.view.add_user_tagged(text, None),
        }
        self.start_run_as(text.to_owned(), origin, prompt_id);
        true
    }

    /// The remote link's way in (#2373): a [`amp_core::remote::RemoteHost`] whose calls this
    /// shell applies in [`Shell::poll`].
    pub fn remote_host(&mut self) -> Arc<dyn amp_core::remote::RemoteHost> {
        let status = Arc::clone(
            self.remote_status
                .get_or_insert_with(|| Arc::new(std::sync::Mutex::new(Default::default()))),
        );
        self.refresh_remote_status();
        Arc::new(crate::remote::ShellRemote::new(
            RemoteSender(self.msgs_tx.clone()),
            status,
        ))
    }

    /// The status the remote link reports: model, busy, context, cost, project, ticket.
    fn refresh_remote_status(&self) {
        let Some(cell) = &self.remote_status else {
            return;
        };
        let meta = self.meta.as_ref().map(|m| m.snapshot());
        let window = self.info.context_window.max(1) as i64;
        let ctx = self.totals.context_tokens;
        let status = amp_core::remote::HostStatus {
            model: self.info.model_ref(),
            busy: self.running,
            context_pct: (ctx > 0).then(|| (ctx * 100 / window).clamp(0, 100)),
            cost_usd_micros: amp_core::chat_event::usd_micros(self.totals.usage.cost_usd),
            project: meta.as_ref().and_then(|m| {
                let id = m.project_id?;
                let name = m
                    .project
                    .get()
                    .filter(|p| p.id == id)
                    .map(|p| p.name.clone())
                    .unwrap_or_default();
                Some((id, name))
            }),
            ticket_id: meta.as_ref().and_then(|m| m.ticket_id),
            branch: self.info.git_branch.clone(),
            workers_running: self
                .supervisor
                .as_ref()
                .map_or(0, |s| s.workers().len() as i64),
            ..remote_status_more(meta.as_deref(), &self.info, &self.totals)
        };
        *cell.lock().unwrap_or_else(|e| e.into_inner()) = status;
    }

    /// Applies one remote call (#2373).
    fn remote_call(&mut self, call: crate::remote::RemoteCall) {
        match call {
            crate::remote::RemoteCall::Start {
                prompt_id,
                text,
                origin,
                reply,
            } => {
                let started = self.send_prompt_as(&text, origin, Some(prompt_id));
                let _ = reply.send(started);
            }
            crate::remote::RemoteCall::Stop { reply } => {
                // As Esc / `/abort`: the reply stops; what that does to the workers depends on
                // the dispatch mode (AMP #2858).
                if self.running {
                    self.warn_reply_stop();
                    self.view.set_stopping(true);
                    self.clear_esc_hint();
                    let harness = Arc::clone(&self.harness);
                    tokio::spawn(async move {
                        let _ = harness.abort().await;
                    });
                } else {
                    // #2425: Stop from a paired device also stops waiting for plan approval.
                    self.stop_approval_wait(Some(STOPPED_WAITING));
                }
                let _ = reply.send(());
            }
        }
        self.refresh_remote_status();
    }

    /// Gives the chat its remote link for `/remote` (#2376), or why remote control can't be used
    /// from here (`/remote` says so). `warning`: shown each time it is turned on (the loopback dev
    /// override). Without a call, `/remote` says it isn't available in this session.
    pub fn set_remote(
        &mut self,
        link: Result<amp_core::remote::RemoteLink, String>,
        warning: Option<String>,
    ) {
        self.remote = Some(crate::remote_ui::RemoteUi::new(link, warning));
    }

    /// A pairing question is waiting for its local `y`/`N` (#2376).
    pub fn pairing_pending(&self) -> bool {
        self.remote.as_ref().is_some_and(|r| r.pending.is_some())
    }

    /// Answers the pairing question (#2376): `allow` only from a `y` typed on this keyboard.
    pub fn answer_pairing(&mut self, allow: bool) {
        let Some(prompt) = self.remote.as_mut().and_then(|r| r.pending.take()) else {
            return;
        };
        let name = prompt.device_name.clone();
        let delivered = prompt.answer.send(allow).is_ok();
        let line = match (allow, delivered) {
            (true, true) => format!(
                "Allowed \"{name}\": it can now send prompts and Stop to this chat. /remote \
                 devices lists paired devices and revokes them."
            ),
            (true, false) => {
                format!("Too late: the pairing request from \"{name}\" had already ended.")
            }
            (false, _) => format!("Refused \"{name}\": it was not paired."),
        };
        self.view.add_notice(&line);
    }

    /// The screen's size for the QR code: the last one seen, else the terminal's, else 80x24.
    fn screen_size(&self) -> (usize, usize) {
        self.screen
            .or_else(|| {
                crossterm::terminal::size()
                    .ok()
                    .map(|(w, h)| (usize::from(w), usize::from(h)))
            })
            .unwrap_or((80, 24))
    }

    /// `/remote [on|off|pair|copy|open|devices [revoke <n|all>]|status]` (#2376, #2423).
    fn remote_command(&mut self, arg: &str) {
        const USAGE: &str = "/remote: use /remote (turn it on), /remote off, /remote pair, \
                             /remote copy, /remote open, /remote devices, \
                             /remote devices revoke <n|all>";
        let Some(ui) = &self.remote else {
            self.view.add_notice(
                "/remote: not available in this session (remote control needs the interactive \
                 chat in a terminal)",
            );
            return;
        };
        let link = match &ui.link {
            Ok(link) => link.clone(),
            Err(why) => {
                self.view.add_error(&format!("/remote: {why}"));
                return;
            }
        };
        let busy = ui.busy;
        let warning = ui.warning.clone();
        let mut words = arg.split_whitespace();
        let verb = words.next().unwrap_or("on");
        let rest: Vec<&str> = words.collect();
        let now = amp_core::remote::relay::now_ms();
        if busy && matches!(verb, "on" | "off" | "pair") {
            self.view
                .add_notice("/remote: still working on the last /remote; try again in a moment");
            return;
        }
        match (verb, rest.as_slice()) {
            ("on", []) if link.is_on() => self.remote_status(&link, now),
            ("on", []) => {
                if let Some(w) = warning {
                    self.view.add_warning(&w);
                }
                self.view
                    .add_notice("Turning remote control on: registering this chat with AMP...");
                self.remote_job(async move { RemoteReply::Enabled(link.enable().await) });
            }
            ("status", []) => self.remote_status(&link, now),
            ("copy", []) | ("open", []) => {
                let open_link = match link.current() {
                    amp_core::remote::RemoteState::On(on) => on
                        .link
                        .filter(|_| on.link_expires_at_ms.is_some_and(|until| until > now)),
                    _ => {
                        self.view.add_notice(&format!(
                            "/remote {verb}: remote control is off; /remote turns it on and \
                             makes a pairing link."
                        ));
                        return;
                    }
                };
                match open_link {
                    None => self.view.add_notice(&format!(
                        "/remote {verb}: no pairing link is open (each one works once); \
                         /remote pair makes a new one."
                    )),
                    Some(url) if verb == "copy" => self.copy_pairing_link(&url, true),
                    Some(url) => match self.desktop.open(&url) {
                        Ok(_) => self.view.add_notice(
                            "Opened the pairing link in this computer's browser: sign in to AMP \
                             there if asked, then answer y here.",
                        ),
                        Err(e) => self.view.add_error(&format!(
                            "/remote open: couldn't open the browser ({e}); /remote copy copies \
                             the link instead."
                        )),
                    },
                }
            }
            ("off", []) if !link.is_on() => self.view.add_notice("Remote control is already off."),
            ("off", []) => {
                self.remote_job(async move {
                    link.disable().await;
                    RemoteReply::Disabled
                });
            }
            ("pair", []) if !link.is_on() => self
                .view
                .add_notice("/remote pair: remote control is off; /remote turns it on first."),
            ("pair", []) => {
                self.remote_job(async move { RemoteReply::Pair(link.new_pairing_link().await) });
            }
            ("devices", []) => match link.devices() {
                Ok(devices) => {
                    let devices = crate::remote_ui::sorted(&devices);
                    for line in crate::remote_ui::device_lines(&devices, now) {
                        self.view.add_notice(&line);
                    }
                }
                Err(e) => self.view.add_error(&format!("/remote devices: {e}")),
            },
            ("devices", ["revoke", which]) | ("revoke", [which]) => {
                self.remote_revoke(&link, which)
            }
            ("devices", ["revoke"]) | ("revoke", []) => self.view.add_notice(
                "/remote devices revoke: which one? A number from /remote devices, or all",
            ),
            _ => self.view.add_notice(USAGE),
        }
    }

    /// Runs one `/remote` call in the background; [`Shell::poll`] applies its reply.
    fn remote_job(&mut self, job: impl Future<Output = RemoteReply> + Send + 'static) {
        if let Some(ui) = &mut self.remote {
            ui.busy = true;
        }
        self.in_flight += 1;
        let tx = self.msgs_tx.clone();
        tokio::spawn(async move {
            let _ = tx.send(ShellMsg::RemoteDone(job.await));
        });
    }

    /// `/remote devices revoke <n|id|all>`: unpins on this laptop (the device must pair again).
    fn remote_revoke(&mut self, link: &amp_core::remote::RemoteLink, which: &str) {
        if which == "all" {
            match link.unpin_all() {
                Ok(0) => self.view.add_notice("No paired devices to revoke."),
                Ok(n) => self.view.add_notice(&format!(
                    "Revoked all {n} paired device{}: each must pair again (/remote pair).",
                    if n == 1 { "" } else { "s" }
                )),
                Err(e) => self.view.add_error(&format!("/remote devices revoke: {e}")),
            }
            return;
        }
        let devices = match link.devices() {
            Ok(d) => crate::remote_ui::sorted(&d),
            Err(e) => {
                self.view.add_error(&format!("/remote devices revoke: {e}"));
                return;
            }
        };
        let pin = match crate::remote_ui::pick(&devices, which) {
            Ok(pin) => pin.clone(),
            Err(e) => {
                self.view
                    .add_notice(&format!("/remote devices revoke: {e}"));
                return;
            }
        };
        match link.unpin(&pin.device_id) {
            Ok(_) => self.view.add_notice(&format!(
                "Revoked \"{}\": its commands are refused from now on; it must pair again.",
                pin.name
            )),
            Err(e) => self.view.add_error(&format!("/remote devices revoke: {e}")),
        }
    }

    /// `/remote` while it is on: who is watching, until when, and the open pairing link.
    fn remote_status(&mut self, link: &amp_core::remote::RemoteLink, now: i64) {
        use crate::remote_ui::{clock, span};
        let amp_core::remote::RemoteState::On(on) = link.current() else {
            self.view
                .add_notice("Remote control is starting; /remote again shows its state.");
            return;
        };
        let connection = if on.online {
            "connected"
        } else {
            "offline, reconnecting"
        };
        // #2424: no limits unless the organization set them.
        let limits = match (on.expires_at_ms, on.idle_deadline_ms) {
            (None, None) => "It stays on, through sleep and network changes, until you end it: \
                             /remote off, quitting this chat, or Disconnect in AMP."
                .to_owned(),
            (Some(exp), None) => format!(
                "It turns off at {} (your organization's limit). /remote off ends it.",
                clock(exp, now)
            ),
            (None, Some(idle)) => format!(
                "It turns off after {} without remote activity (now at {}; your \
                 organization's limit). /remote off ends it.",
                span(idle - now),
                clock(idle, now)
            ),
            (Some(exp), Some(idle)) => format!(
                "It turns off at {} or after {} without remote activity (now at {}; your \
                 organization's limits). /remote off ends it.",
                clock(exp, now),
                span(idle - now),
                clock(idle, now)
            ),
        };
        self.view.add_notice(&format!(
            "Remote control is on ({connection}): {} device{} watching, {} paired. {limits}",
            on.viewers,
            if on.viewers == 1 { "" } else { "s" },
            on.devices,
        ));
        match (on.link, on.link_expires_at_ms) {
            (Some(url), Some(until)) if until > now => self.show_pairing_link(&url, until, now),
            _ => self
                .view
                .add_notice("No pairing link is open: /remote pair makes one."),
        }
    }

    /// The pairing link, its QR code when the window has room, and how long it works.
    fn show_pairing_link(&mut self, url: &str, until_ms: i64, now: i64) {
        use crate::remote_ui::{QrBlock, clock, qr_fits, qr_size, span};
        self.view.add_notice(&format!(
            "Pair a phone or another computer: open this link there, signed in to AMP as you, \
             then check the code and answer y here. It works once, for {} (until {}).",
            span(until_ms - now),
            clock(until_ms, now)
        ));
        self.view
            .add_to_transcript(Arc::new(crate::remote_ui::LinkBlock::new(url)));
        // #2423: how else to get the link out of the chat.
        self.view.add_notice(if self.mouse_capture {
            "/remote copy copies the link again, /remote open opens it in this computer's \
             browser. To select it yourself: Option-drag (iTerm2) or Fn-drag (Terminal.app)."
        } else {
            "/remote copy copies the link again, /remote open opens it in this computer's \
             browser, or select it with the mouse."
        });
        let (w, h) = self.screen_size();
        if qr_fits(url, w, h)
            && let Some(qr) = QrBlock::new(url)
        {
            self.view.add_to_transcript(Arc::new(qr));
        } else if let Some((cols, rows)) = qr_size(url) {
            self.view.add_notice(&format!(
                "(No QR code: it needs a window of about {}x{}; this one is {w}x{h}. Use the link.)",
                cols + 4,
                rows + 12
            ));
        }
        self.copy_pairing_link(url, false);
    }

    /// Puts the pairing link on the clipboard (#2423) and says so, never showing or logging it
    /// again. `asked`: `/remote copy` (so a chat with no clipboard says why nothing happened).
    fn copy_pairing_link(&mut self, url: &str, asked: bool) {
        let copied = self.desktop.copy(url);
        let failed = copied
            .tool_error
            .as_deref()
            .map(|e| format!(" ({e})"))
            .unwrap_or_default();
        if copied.tool.is_some() {
            self.view
                .add_notice("Pairing link copied to the clipboard.");
        } else if copied.osc52 {
            self.view.add_notice(&format!(
                "Pairing link sent to the terminal's clipboard (OSC 52){failed}. If pasting gives \
                 nothing, your terminal doesn't allow it: /remote open, or select the link."
            ));
        } else if asked || copied.tool_error.is_some() {
            self.view.add_notice(&format!(
                "/remote copy: no clipboard here{failed}; /remote open, or select the link."
            ));
        }
    }

    /// Applies a `/remote` call's reply.
    fn remote_done(&mut self, reply: RemoteReply) {
        use crate::remote_ui::{clock, span};
        if let Some(ui) = &mut self.remote {
            ui.busy = false;
        }
        let now = amp_core::remote::relay::now_ms();
        match reply {
            RemoteReply::Enabled(Ok(on)) => {
                let feed = self.view.feed();
                if let Some(ui) = &mut self.remote {
                    ui.watch_queue(feed.as_ref());
                }
                let idle = match self.remote.as_ref().map(|r| r.current()) {
                    Some(amp_core::remote::RemoteState::On(amp_core::remote::RemoteOn {
                        idle_deadline_ms: Some(idle),
                        ..
                    })) => format!(", or after {} without remote activity", span(idle - now)),
                    _ => String::new(),
                };
                // #2424: by default a session has no lifetime and no idle limit.
                let text = match on.expires_at_ms {
                    Some(exp) => format!(
                        "Remote control is on until {} ({}){idle} (your organization's limit). \
                         /remote off ends it; this laptop's key is {}.",
                        clock(exp, now),
                        span(exp - now),
                        on.fingerprint
                    ),
                    None if !idle.is_empty() => format!(
                        "Remote control is on until you end it{idle} (your organization's \
                         limit). /remote off ends it; this laptop's key is {}.",
                        on.fingerprint
                    ),
                    None => format!(
                        "Remote control is on until you end it: it reconnects by itself after \
                         sleep or a network change. /remote off ends it; this laptop's key is {}.",
                        on.fingerprint
                    ),
                };
                self.view.add_notice(&text);
                self.show_pairing_link(&on.link, on.link_expires_at_ms, now);
            }
            RemoteReply::Enabled(Err(e)) => self.view.add_error(&format!("/remote: {}", e.message)),
            // The link says "Remote control is off." itself.
            RemoteReply::Disabled => {}
            RemoteReply::Pair(Ok((url, until))) => self.show_pairing_link(&url, until, now),
            RemoteReply::Pair(Err(e)) => self.view.add_error(&format!("/remote pair: {e}")),
        }
    }

    /// The link's news since the last poll (#2376): notices, the pairing question, the badge
    /// and queued remote prompts. Returns whether anything changed.
    fn poll_remote(&mut self) -> bool {
        let Some(ui) = &mut self.remote else {
            return false;
        };
        let link = ui.link.as_ref().ok().cloned();
        let devices = move || {
            link.as_ref()
                .and_then(|l| l.devices().ok())
                .unwrap_or_default()
        };
        let now = amp_core::remote::relay::now_ms();
        let events = ui.drain(now, &devices);
        let changed = !events.is_empty();
        for event in events {
            match event {
                crate::remote_ui::UiEvent::Line(level, text) => match level {
                    amp_core::chat_event::NoticeLevel::Info => self.view.add_notice(&text),
                    amp_core::chat_event::NoticeLevel::Warn => self.view.add_warning(&text),
                    amp_core::chat_event::NoticeLevel::Error => self.view.add_error(&text),
                },
                crate::remote_ui::UiEvent::Pairing => self.show_pairing_question(now),
                crate::remote_ui::UiEvent::Badge(badge) => self.view.footer().set_badge(badge),
            }
        }
        changed
    }

    /// The pairing question, on this terminal only.
    fn show_pairing_question(&mut self, now: i64) {
        let Some(p) = self.remote.as_ref().and_then(|r| r.pending.as_ref()) else {
            return;
        };
        let question = p.question();
        let mut hint = format!(
            "Press y to allow it, any other key to refuse. Only this keyboard can answer; with no \
             answer by {} it is refused.",
            crate::remote_ui::clock(p.expires_at_ms, now)
        );
        if !p.user_agent.is_empty() {
            hint.push_str(&format!(
                " Browser: {}",
                crate::chrome::shorten(&p.user_agent, 60)
            ));
        }
        self.view.add_local_line(&question, true);
        self.view.add_local_line(&hint, false);
        self.view.scroll().to_end();
    }

    pub fn view(&self) -> &ChatView {
        &self.view
    }

    /// Columns of space left and right of the chat (`[tui] padding`, #2251).
    pub fn set_padding(&mut self, columns: usize) {
        self.view.set_padding(columns);
    }

    /// Whether the input has the keys (#2344), so the terminal shows its cursor in the editor.
    /// It doesn't while:
    /// - a `/connect` key is being typed (hidden), or a worker stop waits for its `y`;
    /// - a worker's session is shown in the editor's place;
    /// - `/questions` shows an answer for confirmation, or sends it;
    /// - a panel is open with an empty editor (↑↓, Enter and a configuration panel's letter
    ///   keys go to it), unless the person clicked into the editor.
    pub fn input_focused(&self) -> bool {
        if self.secret.is_some()
            || self.stop_confirm.is_some()
            || self.pairing_pending()
            || self.viewing.is_some()
            || self.view.strip().is_focused()
        {
            return false;
        }
        match self.view.panel().state().map(|s| s.stage) {
            None | Some(Stage::Answer { .. }) => true,
            Some(Stage::Confirm { .. } | Stage::Sending { .. }) => false,
            Some(Stage::Browse) => self.editor_clicked || !self.view.editor().get_text().is_empty(),
        }
    }

    /// Hands [`Shell::input_focused`] to the view, which shows or hides the cursor. The terminal
    /// loop calls it before every repaint.
    pub fn sync_focus(&mut self) {
        if !self.view.panel().is_open() {
            self.editor_clicked = false;
        }
        self.view.set_input_focus(self.input_focused());
    }

    /// A left click at `(row, column)` (0-based) on a `width` x `height` screen (#2344). Inside
    /// the editor box it gives the editor the keys and puts the insertion point where it landed.
    /// Returns whether it did.
    ///
    /// On a thinking header in the transcript it opens or closes that section (#2345).
    ///
    /// Anywhere else, a click on a worker (#2347) opens its session: a chip above the editor, a
    /// `/workers` row, a worker notice or dispatch line, or any `#ticket` mention of a ticket
    /// with a worker session (the mention under the click, else the row's first such ticket).
    pub fn click(&mut self, width: usize, height: usize, row: usize, column: usize) -> bool {
        self.screen = Some((width, height));
        if self.view.click_thinking(width, height, row).is_some() {
            return true;
        }
        if self.secret.is_some() || self.stop_confirm.is_some() {
            return false;
        }
        if self.viewing.is_some() {
            return self.click_worker(width, height, row, column);
        }
        let Some((line, byte)) = self.view.editor_hit(width, height, row, column) else {
            return self.click_worker(width, height, row, column);
        };
        if matches!(
            self.view.panel().state().map(|s| s.stage),
            Some(Stage::Confirm { .. } | Stage::Sending { .. })
        ) {
            return false;
        }
        self.editor_clicked = self.view.panel().is_open();
        self.view.editor().set_cursor(line, byte);
        self.sync_focus();
        true
    }

    /// Whether the terminal loop turns mouse reporting on (`[tui] mouse`, default on, #2185).
    pub fn set_mouse_capture(&mut self, on: bool) {
        self.mouse_capture = on;
        self.view.set_mouse(on);
    }

    /// The terminal's size (the terminal loop sets it before Ctrl+T; `--snapshot` once), so
    /// Ctrl+T opens the thinking in sight ([`Shell::toggle_thinking`]).
    pub fn set_screen(&mut self, width: usize, height: usize) {
        self.screen = Some((width, height));
    }

    pub fn mouse_capture(&self) -> bool {
        self.mouse_capture
    }

    /// This terminal's new-line keys (#2349; the terminal loop sets it once the kitty keyboard
    /// protocol is negotiated). Without Shift+Enter the empty editor's hint names Ctrl+J
    /// ([`crate::LEGACY_PLACEHOLDER`]) and `/help` says Shift+Enter sends.
    pub fn set_newline_mode(&mut self, mode: NewlineMode) {
        self.newline = mode;
        self.view.set_placeholder(if mode.shift_enter() {
            crate::PLACEHOLDER
        } else {
            crate::LEGACY_PLACEHOLDER
        });
    }

    pub fn newline_mode(&self) -> NewlineMode {
        self.newline
    }

    /// A key for the editor, once nothing else took it ([`Shell::key`] returned false): Enter
    /// right after a `\` starts a new line instead of sending ([`crate::newline::continue_line`],
    /// #2349); anything else goes to rpi-tui's editor (Shift+Enter and Ctrl+J insert a new line,
    /// Enter sends).
    pub fn editor_key(&mut self, key: KeyEvent) {
        let editor = self.view.editor().clone();
        if !crate::newline::continue_line(&editor, &key) {
            editor.handle_key(key);
        }
    }

    pub fn info(&self) -> &ShellInfo {
        &self.info
    }

    /// How the transcript shows thinking (`[tui] show_thinking`, #2177); `/config thinking`
    /// changes it for the messages already shown too, worker sessions included (#2345).
    pub fn set_show_thinking(&mut self, display: crate::ThinkingDisplay) {
        self.view.set_show_thinking(display);
        for session in self.sessions.values_mut() {
            session.set_show_thinking(display);
        }
    }

    /// `/config thinking [collapsed|full|hidden]` (#2345): sets how this chat shows thinking;
    /// without a value it goes to the next one (collapsed, full, hidden). Saving it is
    /// `[tui] show_thinking` in config.toml.
    fn thinking_command(&mut self, arg: &str) {
        use crate::ThinkingDisplay as D;
        let next = if arg.trim().is_empty() {
            Ok(match self.view.show_thinking() {
                D::Collapsed => D::Full,
                D::Full => D::Hidden,
                D::Hidden => D::Collapsed,
            })
        } else {
            D::parse(arg)
        };
        let command = format!("/config thinking {arg}").trim_end().to_owned();
        match next {
            Ok(display) => {
                self.set_show_thinking(display);
                let line = format!(
                    "thinking: {} for this chat ([tui] show_thinking in config.toml keeps it)",
                    display.word()
                );
                self.apply_config(Ok(ConfigReply::done(line)), &command);
            }
            Err(e) => self.apply_config(Err(e), &command),
        }
    }

    /// Ctrl+T: expands or collapses the lowest thinking header in sight, else the last thinking
    /// section (#2177, #2345). Returns whether there was one.
    pub fn toggle_thinking(&mut self) -> bool {
        if let Some((w, h)) = self.screen
            && self.view.toggle_visible_thinking(w, h).is_some()
        {
            return true;
        }
        if let Some(s) = self.shown_session() {
            return s.view().toggle_thinking().is_some();
        }
        self.view.toggle_thinking().is_some()
    }

    /// Ctrl+O: shows or hides every tool call's arguments and result (#2183, pi's
    /// `app.tools.expand`). Returns whether they are now shown.
    pub fn toggle_tools(&mut self) -> bool {
        if let Some(s) = self.shown_session() {
            return s.view().toggle_tools();
        }
        self.view.toggle_tools()
    }

    pub fn is_running(&self) -> bool {
        self.running
    }

    /// Gives the shell the session's worker supervisor (the manager's; `None` for a worker role).
    pub fn set_supervisor(&mut self, supervisor: Option<Arc<Supervisor>>) {
        self.launch_watch = supervisor.as_ref().map(|s| s.watch_launch_ends());
        self.supervisor = supervisor;
    }

    /// Shows every launched worker that ended since the last tick (AMP #2861), whether the
    /// manager is idle or mid-reply: no dispatch tool result has to come back first. It reads
    /// the supervisor's broadcast, never its queue, so the next turn still gets each end once
    /// ([`Shell::take_worker_summaries`]). Returns whether anything was shown.
    fn show_launch_ends(&mut self) -> bool {
        use tokio::sync::broadcast::error::TryRecvError;
        let Some(watch) = self.launch_watch.as_mut() else {
            return false;
        };
        let mut shown = false;
        loop {
            match watch.try_recv() {
                Ok(end) => {
                    if let Err(why) = &end.result {
                        self.ended_launches.insert(
                            end.task_id,
                            EndedLaunch {
                                agent_id: end.agent_id.clone(),
                                why: why.clone(),
                                at: std::time::SystemTime::now(),
                            },
                        );
                    }
                    let (level, text) = launch_end_notice(&end);
                    match level {
                        EndLevel::Info => self.view.add_notice(&text),
                        EndLevel::Warn => self.view.add_warning(&text),
                        EndLevel::Error => self.view.add_error(&text),
                    }
                    shown = true;
                }
                Err(TryRecvError::Lagged(missed)) => {
                    self.view.add_warning(&format!(
                        "{missed} worker ends were not shown here; /workers lists them"
                    ));
                    shown = true;
                }
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Closed) => {
                    self.launch_watch = None;
                    break;
                }
            }
        }
        // AMP #2862: an end is news for the roster too (the worker is finished, crashed or never
        // dispatched), not only when a dispatch tool call returns.
        if shown {
            self.build_roster();
        }
        shown
    }

    /// Gives the shell `/connect` (amp-harness: keychain, provider checks, config).
    pub fn set_connect(&mut self, connect: Arc<dyn ProviderConnect>) {
        self.connect = Some(connect);
    }

    /// The models `/model` can switch to.
    pub fn models(&self) -> &[ModelChoice] {
        &self.models
    }

    /// Whether a `/connect` key is being typed (keys and pastes go to it, not the editor).
    pub fn secret_active(&self) -> bool {
        self.secret.is_some()
    }

    /// A key while a `/connect` key is being typed. Returns true when it was used. Enter sends
    /// the key; Esc or Ctrl-C cancels; Backspace and Ctrl-U edit. Nothing is ever drawn.
    pub fn secret_key(&mut self, key: &KeyEvent) -> bool {
        let Some(entry) = self.secret.as_mut() else {
            return false;
        };
        let ctrl = key.modifiers.contains(KeyModifiers::CONTROL);
        match key.code {
            KeyCode::Enter => {
                let entry = self.secret.take().expect("checked above");
                self.send_secret(entry);
            }
            KeyCode::Esc => self.cancel_secret(),
            KeyCode::Char('c') | KeyCode::Char('d') if ctrl => self.cancel_secret(),
            KeyCode::Char('u') if ctrl => entry.buf.clear(),
            KeyCode::Backspace => {
                entry.buf.pop();
            }
            KeyCode::Char(c) if !ctrl => entry.buf.push(c),
            _ => {}
        }
        true
    }

    /// Pasted text: into the `/connect` key when one is being typed, else the editor, which then
    /// has the keys while a panel is docked (#2161).
    pub fn paste(&mut self, text: &str) {
        match self.secret.as_mut() {
            Some(entry) => entry.buf.push_str(text.trim_end_matches(['\r', '\n'])),
            None => {
                self.editor_clicked = self.view.panel().is_open();
                self.view.editor().handle_paste(text);
            }
        }
    }

    fn cancel_secret(&mut self) {
        if self.secret.take().is_some() {
            self.view
                .add_notice("/connect cancelled: nothing was stored");
        }
    }

    fn send_secret(&mut self, entry: SecretEntry) {
        let key = entry.buf.trim().to_owned();
        if key.is_empty() {
            self.view
                .add_notice("/connect cancelled: no key was entered");
            return;
        }
        let Some(connect) = self.connect.clone() else {
            return;
        };
        self.view.add_notice("checking the key with the provider…");
        let job = connect.connect(&entry.args, key);
        self.spawn_connect(job);
    }

    /// Runs a `/connect` job; on success the harness takes the new providers before the result
    /// is applied ([`Shell::poll`]).
    fn spawn_connect(&mut self, job: ConnectFuture) {
        let harness = Arc::clone(&self.harness);
        let tx = self.msgs_tx.clone();
        self.in_flight += 1;
        tokio::spawn(async move {
            let msg = match job.await {
                Ok(reply) => match harness.set_models(reply.providers).await {
                    Ok(()) => ShellMsg::Connected {
                        result: Ok(reply.text),
                        models: Some(reply.models),
                    },
                    Err(e) => ShellMsg::Connected {
                        result: Err(format!("the chat could not take the new providers: {e}")),
                        models: None,
                    },
                },
                Err(e) => ShellMsg::Connected {
                    result: Err(e),
                    models: None,
                },
            };
            let _ = tx.send(msg);
        });
    }

    /// `/connect` lists the providers; `/connect <provider>` asks for its key (hidden).
    fn connect_command(&mut self, arg: &str) {
        let Some(connect) = self.connect.clone() else {
            self.view.add_notice(
                "/connect: not available here; run `amp-harness connect` in a terminal",
            );
            return;
        };
        if arg.is_empty() {
            let job = connect.list();
            self.spawn_connect(job);
            return;
        }
        match connect.prepare(arg) {
            Ok(prompt) if prompt.is_empty() => {
                self.view.panel().close();
                let job = connect.connect(arg, String::new());
                self.spawn_connect(job);
            }
            Ok(prompt) => {
                self.view.panel().close();
                self.secret = Some(SecretEntry {
                    args: arg.to_owned(),
                    buf: String::new(),
                });
                self.view.add_notice(&format!(
                    "{prompt}: paste or type it now and press Enter. It is not shown, and it is \
                     never written to the transcript or the session file. Esc cancels."
                ));
            }
            Err(e) => self
                .view
                .add_error(&format!("/connect: {}", crate::chrome::clean(&e))),
        }
    }

    /// A [`Notifier`] for background tasks' transcript lines.
    pub fn notifier(&self) -> Notifier {
        Notifier(self.msgs_tx.clone())
    }

    /// Gives the shell `/server` (#2164).
    pub fn set_server_control(&mut self, control: Arc<dyn ServerControl>) {
        self.server = Some(control);
    }

    /// Gives the shell `/reload` (#2242).
    pub fn set_reload(&mut self, control: Arc<dyn ReloadControl>) {
        self.reload = Some(control);
    }

    /// Gives the shell the configuration panels (#2245).
    pub fn set_config(&mut self, control: Arc<dyn ConfigControl>) {
        self.config = Some(control);
    }

    /// `/mcp`, `/agents`, `/skills`, `/config`, `/model <role> …` (#2245): runs in the
    /// background; [`Shell::poll`] applies the reply.
    fn config_command(&mut self, name: &str, arg: &str) {
        let Some(control) = self.config.clone() else {
            self.view.add_notice(&format!(
                "/{name}: not available in this session (`amp-harness {}` in a terminal)",
                match name {
                    "model" => "models",
                    other => other,
                }
            ));
            return;
        };
        let command = if arg.is_empty() {
            format!("/{name}")
        } else {
            format!("/{name} {arg}")
        };
        self.in_flight += 1;
        let tx = self.msgs_tx.clone();
        let job = control.run(name, arg);
        tokio::spawn(async move {
            // Its own task, so a panic in the command still reports back.
            let result = match tokio::spawn(job).await {
                Ok(result) => result,
                Err(e) => Err(format!("the command crashed: {e}")),
            };
            let _ = tx.send(ShellMsg::Config { result, command });
        });
    }

    /// A configuration command's reply: the panel, the transcript, the note, then the reload.
    fn apply_config(&mut self, result: Result<ConfigReply, String>, command: &str) {
        let reply = match result {
            Ok(reply) => reply,
            Err(e) => {
                let e = crate::chrome::clean(&e);
                self.view.panel().update(|s| {
                    if s.kind == PanelKind::Info {
                        s.note = Some((e.clone(), true));
                    }
                });
                self.view.add_error(&format!("{command}: {e}"));
                return;
            }
        };
        if let Some(panel) = reply.panel {
            // The same panel again (a refresh) keeps its selection.
            let keep = self.view.panel().state().and_then(|s| {
                (s.kind == PanelKind::Info
                    && s.info.as_ref().is_some_and(|i| i.title == panel.title))
                .then_some(s.selected)
            });
            let mut state = PanelState::info(panel, reply.select.as_deref());
            if let (Some(at), None) = (keep, reply.select.as_deref()) {
                state.selected = at;
            }
            self.view.panel().open(state);
        }
        if let Some(note) = reply.note {
            self.view.panel().update(|s| {
                if s.kind == PanelKind::Info {
                    s.note = Some(note);
                }
            });
        }
        for line in &reply.lines {
            self.view.add_notice(line);
        }
        for line in &reply.errors {
            self.view.add_error(line);
        }
        if reply.reload {
            self.reload_command(ReloadCause::Panel);
        }
    }

    /// `/reload` (or the file watcher, or a panel's change): re-reads the configuration in the
    /// background; the result is applied by [`Shell::poll`]. A reload asked for while one runs
    /// follows it.
    fn reload_command(&mut self, cause: ReloadCause) {
        let Some(control) = self.reload.clone() else {
            if cause == ReloadCause::Command {
                self.view
                    .add_notice("/reload: not available in this session");
            }
            return;
        };
        if self.reloading {
            // A panel's or a person's request outranks the watcher's (it says what changed).
            if self.reload_again != Some(ReloadCause::Command) {
                self.reload_again = match (self.reload_again, cause) {
                    (Some(ReloadCause::Panel), ReloadCause::Watcher) => Some(ReloadCause::Panel),
                    _ => Some(cause),
                };
            }
            if cause == ReloadCause::Command {
                self.view
                    .add_notice("/reload: already reloading; once more when it is done");
            }
            return;
        }
        self.reloading = true;
        self.in_flight += 1;
        if cause == ReloadCause::Command {
            self.view
                .add_notice("reloading agents, skills, models and MCP servers…");
        }
        let harness = Arc::clone(&self.harness);
        let tx = self.msgs_tx.clone();
        tokio::spawn(async move {
            // Its own task, so a panic in the reload still ends `reloading`.
            let result = match tokio::spawn(control.reload()).await {
                Ok(result) => result,
                Err(e) => Err(format!("the reload crashed: {e}")),
            };
            let result = match result {
                Ok(reply) => match harness.set_models(reply.providers.clone()).await {
                    Ok(()) => Ok(reply),
                    Err(e) => Err(format!("the chat could not take the new providers: {e}")),
                },
                Err(e) => Err(e),
            };
            let _ = tx.send(ShellMsg::Reloaded { result, cause });
        });
    }

    /// The supervisor's dispatch mode ([`DispatchMode::Await`] without one).
    pub fn dispatch_mode(&self) -> DispatchMode {
        self.supervisor
            .as_ref()
            .map_or(DispatchMode::Await, |s| s.config().dispatch)
    }

    /// What stopping the running reply would do to this chat's workers now (AMP #2858). In
    /// Launch mode a worker is cancelled only when this reply launched it (it wasn't live when
    /// the turn started, so its tool-call signal is this reply's) and it is still waiting for a
    /// slot (queued, and AMP has no dispatch for it).
    pub fn reply_stop(&self) -> ReplyStop {
        let Some(sup) = self.supervisor.as_ref() else {
            return ReplyStop::default();
        };
        let workers = sup.workers();
        let live = workers.iter().map(|w| w.task_id).collect();
        if sup.config().dispatch == DispatchMode::Await {
            return ReplyStop {
                live,
                ..ReplyStop::default()
            };
        }
        let dispatched = sup.unfinished();
        let waiting = |w: &&amp_core::supervisor::WorkerStatus| {
            w.phase == amp_core::supervisor::WorkerPhase::Queued && !dispatched.contains(&w.task_id)
        };
        let (cancelled, queued) = workers
            .iter()
            .filter(waiting)
            .map(|w| w.task_id)
            .partition(|id| !self.turn_live.contains(id));
        ReplyStop {
            live,
            cancelled,
            queued,
        }
    }

    /// Warns what stopping the reply does to this chat's workers, if any are live (AMP #2858).
    fn warn_reply_stop(&mut self) {
        if let Some(note) = reply_stop_notice(self.dispatch_mode(), &self.reply_stop()) {
            self.view.add_warning(&note);
        }
    }

    /// Tickets with a live (queued, running or reporting) worker.
    pub fn live_tickets(&self) -> Vec<i64> {
        self.supervisor
            .as_ref()
            .map(|s| s.workers().iter().map(|w| w.task_id).collect())
            .unwrap_or_default()
    }

    /// How long [`Shell::quit`] waits once workers are live: the supervisor's abort grace plus
    /// [`REPORT_ALLOWANCE`].
    pub fn quit_bound(&self) -> Duration {
        self.supervisor
            .as_ref()
            .map_or(ABORT_GRACE, |s| s.config().abort_grace + REPORT_ALLOWANCE)
    }

    fn settled(&self) -> bool {
        !self.running && self.live_tickets().is_empty()
    }

    /// Winds the session down before the process exits. With workers live: cancel them all,
    /// abort the prompt, say so, and wait (up to [`Shell::quit_bound`]) until the prompt has ended
    /// and every worker has stopped and been reported. Otherwise a running prompt is aborted and
    /// gets [`ABORT_GRACE`]. `skip` ends the wait early (a second Ctrl-C); `repaint` runs after
    /// each change and on a 120 ms tick so the caller can redraw.
    pub async fn quit<S, R>(
        &mut self,
        events: &mut UnboundedReceiver<AgentEvent>,
        done: &mut UnboundedReceiver<RunDone>,
        skip: S,
        mut repaint: R,
    ) -> QuitOutcome
    where
        S: Future<Output = ()>,
        R: FnMut(&Self),
    {
        // Quitting: nothing queued may start once the reply ends.
        self.prompt_queue.clear();
        if self.settled() {
            return QuitOutcome::Idle;
        }
        let start = tokio::time::Instant::now();
        let mut deadline = start + ABORT_GRACE;
        let mut waiting_for_workers = false;
        if self.running
            && let Some(sup) = &self.supervisor
        {
            // The manager may be starting a worker right now: nothing new may start.
            sup.cancel_all();
        }
        self.abort_quietly().await;
        tokio::pin!(skip);
        let mut ticker = tokio::time::interval(Duration::from_millis(120));
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            let live = self.live_tickets().len();
            if live > 0 && !waiting_for_workers {
                waiting_for_workers = true;
                deadline = start + self.quit_bound();
                if let Some(sup) = &self.supervisor {
                    sup.cancel_all();
                }
                self.view.add_notice(&format!(
                    "stopping {live} worker{} and reporting {} runs… (Ctrl+C again to quit now)",
                    if live == 1 { "" } else { "s" },
                    if live == 1 { "its" } else { "their" },
                ));
            }
            if self.settled() {
                while let Ok(event) = events.try_recv() {
                    self.handle_event(&event);
                }
                repaint(self);
                return QuitOutcome::Settled;
            }
            repaint(self);
            tokio::select! {
                biased;
                Some(event) = events.recv() => self.handle_event(&event),
                Some(result) = done.recv() => {
                    while let Ok(event) = events.try_recv() {
                        self.handle_event(&event);
                    }
                    self.finish_run(result);
                }
                () = &mut skip => {
                    self.view.add_notice("quitting without waiting");
                    self.release_unfinished("the chat quit without waiting for its worker", &mut repaint).await;
                    return QuitOutcome::Skipped;
                }
                () = tokio::time::sleep_until(deadline) => {
                    self.view.add_notice("gave up waiting");
                    self.release_unfinished("the chat quit before its worker stopped", &mut repaint).await;
                    return QuitOutcome::TimedOut;
                }
                _ = ticker.tick() => {}
            }
        }
    }

    /// Quitting without waiting: the workers' own `on_finish` won't run, so hand their tickets
    /// back to ready now, within [`RELEASE_BOUND`] (#2204).
    async fn release_unfinished<R: FnMut(&Self)>(&mut self, why: &str, repaint: &mut R) {
        let Some(sup) = self.supervisor.clone() else {
            repaint(self);
            return;
        };
        let ids = sup.unfinished();
        if ids.is_empty() {
            repaint(self);
            return;
        }
        let list = ids
            .iter()
            .map(|id| format!("#{id}"))
            .collect::<Vec<_>>()
            .join(", ");
        self.view
            .add_notice(&format!("returning {list} to ready in AMP…"));
        repaint(self);
        self.released = sup.release_unfinished(why, RELEASE_BOUND).await;
        repaint(self);
    }

    /// What quitting without waiting did with each abandoned ticket: `Ok` when AMP has it back
    /// in ready. Empty when the workers settled (their `on_finish` handed them back).
    pub fn released(&self) -> &[(i64, Result<(), String>)] {
        &self.released
    }

    /// Whether a quit may go ahead at `now` (#2204). With no worker of this chat live, always.
    /// Otherwise the first quit shows [`worker_quit_warning`] and returns false; another quit
    /// within [`QUIT_CONFIRM_FOR`] returns true.
    pub fn confirm_quit(&mut self, now: Instant) -> bool {
        let live = self.live_tickets();
        if live.is_empty() {
            self.clear_quit_confirm();
            return true;
        }
        if self.quit_confirm_until.is_some_and(|until| now < until) {
            self.clear_quit_confirm();
            return true;
        }
        self.quit_confirm_until = Some(now + QUIT_CONFIRM_FOR);
        self.view.add_warning(&worker_quit_warning(&live));
        self.view.set_editor_hint(Some(QUIT_CONFIRM_HINT));
        false
    }

    /// A quit is waiting to be confirmed ([`Shell::confirm_quit`]).
    pub fn quit_confirm_pending(&self) -> bool {
        self.quit_confirm_until.is_some()
    }

    fn clear_quit_confirm(&mut self) {
        if self.quit_confirm_until.take().is_some() {
            self.view.set_editor_hint(None);
        }
    }

    /// Usage and counts over every run of this session.
    pub fn totals(&self) -> &Totals {
        &self.totals
    }

    /// Attaches the AMP metadata poller: from now on every `amp_*` tool call that ends without
    /// error refreshes what it touched ([`MetaHandle::observe`]).
    /// The header and status line show its snapshot.
    pub fn set_meta(&mut self, meta: MetaHandle) {
        self.view.chrome().set_meta(meta.clone());
        self.meta = Some(meta);
        // Another server's plan: the wait was for this one's.
        self.stop_approval_wait(None);
    }

    /// `[manager] auto_resume_on_approval` (#2425, default on): after a manager turn that asked
    /// for plan approval, wait for a person to approve it in AMP and start the next turn then.
    /// Off: the chat waits for the person to reply, as before.
    pub fn set_auto_resume(&mut self, on: bool) {
        self.auto_resume = on;
        if !on {
            self.stop_approval_wait(None);
        }
    }

    /// Whether the chat waits for a person's plan approval (#2425).
    pub fn waiting_for_approval(&self) -> bool {
        self.approval_wait.is_some()
    }

    /// Ends the wait for plan approval, if there is one, saying `why` (`None`: quietly).
    /// Returns whether there was one.
    fn stop_approval_wait(&mut self, why: Option<&str>) -> bool {
        self.view.chrome().set_approval_wait(None);
        if self.approval_wait.take().is_none() {
            return false;
        }
        if let Some(why) = why {
            self.view.add_notice(why);
        }
        true
    }

    /// Starts waiting for a person's plan approval after a manager turn (#2425), when the turn
    /// asked for it: it planned or looked at the plan, or AMP refused a step for want of
    /// approval. A fresh plan read then decides ([`Shell::watch_approval`]): with nothing of the
    /// person's awaiting, there is nothing to wait for.
    fn begin_approval_wait(&mut self) {
        let turn_plan = std::mem::take(&mut self.turn_plan);
        let refusals = std::mem::take(&mut self.turn_refusals);
        if !self.auto_resume || short_role(&self.info.role) != "manager" {
            return;
        }
        if !turn_plan && refusals.is_empty() {
            return;
        }
        let Some(meta) = &self.meta else {
            return;
        };
        let snapshot = meta.snapshot();
        let Some(project_id) = snapshot.project_id else {
            return;
        };
        self.approval_wait = Some(amp_core::approval_wait::ApprovalWait::new(
            project_id,
            &refusals,
            &snapshot,
            Instant::now(),
        ));
        meta.refresh(Topics::PLAN);
    }

    /// Follows the wait for plan approval on each poll (#2425): says it is waiting once AMP's
    /// plan confirms tickets await the person, and when a person approves, says so and starts
    /// the manager's next turn (once: the wait ends there). Tickets that stop waiting without an
    /// approval, approval turned off or another project end the wait with a note, no turn.
    /// Never while a reply runs or a server switch is going on.
    fn watch_approval(&mut self) -> bool {
        use amp_core::approval_wait::{self as aw, WaitCheck};
        if self.running || self.switching {
            return false;
        }
        let (Some(meta), Some(wait)) = (&self.meta, self.approval_wait.as_mut()) else {
            return false;
        };
        let snapshot = meta.snapshot();
        let link = snapshot.approve_url(wait.project_id);
        match wait.check(&snapshot) {
            WaitCheck::Unknown | WaitCheck::Waiting => false,
            WaitCheck::Nothing => {
                self.stop_approval_wait(None);
                false
            }
            WaitCheck::Armed(tickets) => {
                self.view
                    .add_warning(&aw::waiting_line(&tickets, link.as_deref()));
                self.view
                    .chrome()
                    .set_approval_wait(Some(link.unwrap_or_default()));
                true
            }
            WaitCheck::Changed(why) => {
                self.stop_approval_wait(Some(&why));
                true
            }
            WaitCheck::Approved(approval) => {
                self.stop_approval_wait(None);
                let me = snapshot.user.get();
                self.view.add_notice(&aw::approved_line(&approval, me));
                let prompt = aw::resume_prompt(&approval, me);
                self.view.add_user(&prompt);
                self.start_run(prompt);
                true
            }
        }
    }

    /// The AMP metadata poller, for the header and status line (M3-3).
    pub fn meta(&self) -> Option<&MetaHandle> {
        self.meta.as_ref()
    }

    /// Replaces the browser opener (tests record the URL instead of opening it).
    pub fn set_opener(&mut self, opener: Opener) {
        self.opener = opener;
    }

    /// The clipboard and browser `/remote` copies and opens its pairing link with (#2423).
    pub fn set_desktop(&mut self, desktop: crate::clipboard::Desktop) {
        self.desktop = desktop;
    }

    /// The terminal loop started: unless a test set one, the system's clipboard tools and
    /// browser; OSC 52 goes through `write`.
    pub fn attach_terminal(&mut self, write: crate::clipboard::TerminalWrite) {
        let desktop = if self.desktop.is_on() {
            self.desktop.clone()
        } else {
            crate::clipboard::Desktop::system()
        };
        self.desktop = desktop.with_terminal(write);
    }

    pub fn handle_event(&mut self, event: &AgentEvent) {
        if let Some(e) = RunEvent::from_agent(event) {
            self.totals.observe(&e);
        }
        if let Some(meta) = &self.meta {
            meta.observe(event);
        }
        self.view.apply(event);
        self.follow_streaming(event);
        // AMP #2862: the roster follows the supervisor's live status, not the dispatch tool call.
        // In Launch mode `amp_run_ticket` returns at once, while its worker is still queued or
        // running; in Await mode (#2355) the worker has ended by then, so the strip stops
        // saying running and its finished notice lands now, above the manager's reply. Either
        // way, a change in who is live is what refreshes it. Streaming deltas are skipped: the
        // tick catches anything they would.
        if !matches!(event, AgentEvent::MessageUpdate { .. }) && self.live_changed() {
            self.build_roster();
        }
        if let AgentEvent::ToolExecutionStart {
            tool_call_id,
            tool_name,
            args,
        } = event
            && tool_name.starts_with("amp_")
        {
            if amp_core::approval_wait::touches_plan(tool_name) {
                self.turn_plan = true;
            }
            if let Some(id) = amp_core::approval::task_id_arg(args) {
                self.call_tickets.insert(tool_call_id.clone(), id);
            }
        }
        let call_ticket = match event {
            AgentEvent::ToolExecutionEnd { tool_call_id, .. } => {
                self.call_tickets.remove(tool_call_id)
            }
            _ => None,
        };
        // #2171: AMP refused a step only a person takes in the web UI (approving the plan, a
        // person's ticket). Say so plainly under the tool call, with the link; the harness ends
        // the manager's turn.
        if let AgentEvent::ToolExecutionEnd {
            tool_name,
            result,
            is_error: true,
            ..
        } = event
            && tool_name.starts_with("amp_")
            && let Some(mut refusal) =
                amp_core::approval::refusal(&amp_core::events::tool_result_text(result))
        {
            if call_ticket.is_some() {
                refusal.ticket_id = call_ticket;
            }
            if amp_core::approval_wait::waits_for_approval(&refusal) {
                self.turn_refusals.push(refusal.clone());
            }
            let snapshot = self.meta.as_ref().map(|m| m.snapshot());
            let line = match &refusal.link {
                Some(link) => format!("{} ({link})", refusal.message),
                None => refusal.message.clone(),
            };
            self.view.feed_notice(
                amp_core::chat_event::NoticeLevel::Warn,
                Some("approval_refused"),
                &line,
            );
            self.view
                .add_to_transcript(Arc::new(ApprovalNotice::new(refusal, snapshot.as_deref())));
        }
        // #2225: the repeat guard stopped a model stuck on one tool call; say so in one sentence.
        if let AgentEvent::ToolExecutionEnd {
            result,
            is_error: true,
            ..
        } = event
            && let Some(stop) =
                amp_core::repeat::stopped(&amp_core::events::tool_result_text(result))
        {
            self.view.add_warning(&stop.user_line("/model"));
        }
    }

    /// #2384: while the model writes a tool call's arguments (a plan's `amp_create_tasks` can
    /// stream for 40 s), the spinner line says what it is writing and how far it got
    /// ([`amp_render::streaming_line`]) instead of only "Working…". Back to "Working…" once the
    /// call is complete.
    fn follow_streaming(&mut self, event: &AgentEvent) {
        use rpi_ai::types::{AssistantMessageEvent as E, Content};
        let now = Instant::now();
        let streamed = match event {
            AgentEvent::MessageUpdate {
                assistant_message_event:
                    E::ToolCallStart {
                        content_index,
                        partial,
                    },
                ..
            } => Some((*content_index, partial, "")),
            AgentEvent::MessageUpdate {
                assistant_message_event:
                    E::ToolCallDelta {
                        content_index,
                        partial,
                        delta,
                    },
                ..
            } => Some((*content_index, partial, delta.as_str())),
            _ => None,
        };
        if let Some((index, partial, delta)) = streamed {
            match &mut self.streaming {
                Some(call) if call.index == index => {
                    call.json.push_str(delta);
                    // Some providers name the call after its first delta.
                    if call.name.is_empty()
                        && let Some(Content::ToolCall(tc)) = partial.content.get(index)
                    {
                        call.name = tc.name.clone();
                    }
                    if now.duration_since(call.shown_at) >= STREAMING_EVERY {
                        call.shown_at = now;
                        self.show_working();
                    }
                }
                _ => {
                    if let Some(Content::ToolCall(call)) = partial.content.get(index) {
                        self.streaming = Some(StreamingCall {
                            index,
                            name: call.name.clone(),
                            json: delta.to_owned(),
                            shown_at: now,
                        });
                        self.show_working();
                    }
                }
            }
            return;
        }
        let complete = matches!(
            event,
            AgentEvent::MessageUpdate {
                assistant_message_event: E::ToolCallEnd { .. },
                ..
            } | AgentEvent::MessageEnd { .. }
                | AgentEvent::ToolExecutionStart { .. }
                | AgentEvent::AgentEnd { .. }
        );
        if complete && self.streaming.take().is_some() {
            self.show_working();
        }
    }

    /// The spinner's message when no hint is shown: what the model is writing, else "Working…".
    fn working_text(&self) -> String {
        let text = match &self.streaming {
            Some(call) => amp_render::streaming_line(&call.name, &call.json),
            None => WORKING.to_owned(),
        };
        match self.prompt_queue.len() {
            0 => text,
            n => format!("{text} · {n} queued"),
        }
    }

    /// Puts [`Self::working_text`] on the spinner line, unless the Esc hint is up.
    fn show_working(&self) {
        if self.esc_hint_until.is_none() {
            self.view.set_working_message(&self.working_text());
        }
    }

    /// Handles one editor submission: a slash command or a prompt to the agent.
    pub async fn submit(&mut self, text: &str) -> Submit {
        self.editor_clicked = false;
        let trimmed = text.trim();
        if trimmed.is_empty() {
            return Submit::Continue;
        }
        // Typing an answer to a question (`/questions`, Enter on one): the submission is the
        // answer, shown for confirmation; nothing is sent yet.
        if !trimmed.starts_with('/')
            && let Some(PanelState {
                stage: Stage::Answer { ticket, question },
                ..
            }) = self.view.panel().state()
        {
            let answer = clean_answer(text);
            if answer.is_empty() {
                self.view
                    .panel()
                    .update(|s| s.note = Some(("the answer is empty".to_owned(), true)));
            } else {
                self.view.panel().update(|s| {
                    s.stage = Stage::Confirm {
                        ticket,
                        question,
                        answer,
                    };
                    s.note = None;
                });
            }
            return Submit::Continue;
        }
        // ↑ recalls it (#2348); answers above stay out (they belong to their question).
        self.history.record(text);
        if let Some(rest) = trimmed.strip_prefix('/') {
            let (name, arg) = match rest.split_once(char::is_whitespace) {
                Some((name, arg)) => (name, arg.trim()),
                None => (rest, ""),
            };
            return self.command(name, arg).await;
        }
        // A prompt hands the keys back to the editor; a docked panel stays, unfocused.
        // #2161: a sent prompt hands the keys back to the editor; a docked panel stays.
        self.editor_clicked = self.view.panel().is_open();
        if self.running {
            self.queue_prompt(trimmed);
            return Submit::Continue;
        }
        if self.switching {
            self.view
                .add_notice("switching AMP servers: send it again once the switch is done");
            return Submit::Continue;
        }
        self.view.add_user(trimmed);
        self.start_run(trimmed.to_owned());
        Submit::Continue
    }

    /// A prompt typed while a reply runs (AMP #2889): it waits its turn instead of being refused,
    /// and the transcript and the spinner line say so. At most [`QUEUE_MAX`] wait, as on a paired
    /// device; a stopped reply drops them ([`Shell::finish_run`]).
    fn queue_prompt(&mut self, text: &str) {
        if self.prompt_queue.len() >= QUEUE_MAX {
            self.view.add_notice(&format!(
                "{QUEUE_MAX} prompts are already queued: send it once one has run (↑ recalls it)"
            ));
            return;
        }
        self.prompt_queue.push_back(text.to_owned());
        self.view
            .add_notice(&queued_notice(self.prompt_queue.len(), text));
        if self.esc_hint_until.is_none() {
            self.show_working();
        }
    }

    /// The prompts typed here that wait for the running reply to end (AMP #2889), oldest first.
    pub fn queued_prompts(&self) -> Vec<String> {
        self.prompt_queue.iter().cloned().collect()
    }

    /// After a reply ended (AMP #2889): a stopped one drops the queue and says so; otherwise the
    /// oldest queued prompt runs now, as its own turn. Prompts from a paired device wait in the
    /// remote link's queue and run after these, one per reply ([`amp_core::remote::link`]).
    fn after_run(&mut self, stopped: bool) {
        if stopped {
            let n = self.prompt_queue.len();
            self.prompt_queue.clear();
            if n > 0 {
                self.view.add_notice(&dropped_notice(n));
            }
            return;
        }
        if self.switching {
            return;
        }
        if let Some(next) = self.prompt_queue.pop_front() {
            self.view.add_user(&next);
            self.start_run(next);
        }
    }

    /// While any ticket waits for approval, re-reads the plan now (#2209, #2230): before the manager's
    /// turn, and after `/open plan`, so an approval made in the web UI shows without waiting for
    /// the next poll.
    fn refresh_pending_plan(&self) {
        if let Some(meta) = &self.meta
            && meta.snapshot().plan_awaiting()
        {
            meta.refresh(Topics::PLAN);
        }
    }

    fn start_run(&mut self, prompt: String) {
        self.start_run_from(prompt, amp_core::chat_event::PromptOrigin::Local);
    }

    fn start_run_from(&mut self, prompt: String, origin: amp_core::chat_event::PromptOrigin) {
        self.start_run_as(prompt, origin, None);
    }

    fn start_run_as(
        &mut self,
        prompt: String,
        origin: amp_core::chat_event::PromptOrigin,
        prompt_id: Option<String>,
    ) {
        self.refresh_pending_plan();
        // A new turn: it says for itself whether it waits for approval when it ends (#2425).
        self.stop_approval_wait(None);
        self.turn_plan = false;
        self.turn_refusals.clear();
        self.running = true;
        // AMP #2858: workers already live now were not launched by this reply, so stopping it
        // can't cancel them.
        self.turn_live = self.live_tickets();
        self.prompts += 1;
        // R-M16: the model sees where a remote turn came from; the feed shows the plain text.
        let model_text = match &origin {
            amp_core::chat_event::PromptOrigin::Remote { device_name, .. } => {
                format!("[remote: {device_name}] {prompt}")
            }
            amp_core::chat_event::PromptOrigin::Local => prompt.clone(),
        };
        // #2857: workers that ended since the last turn are delivered here, at the top of the turn
        // that starts now — and only here. Draining takes each end out of the supervisor's queue,
        // so the next turn never sees it again, and an end that arrives while this run is going
        // stays queued for the turn after it (#2835).
        let model_text = match self.take_worker_summaries() {
            Some(summaries) => format!("{summaries}\n\n{model_text}"),
            None => model_text,
        };
        if self.view.feed().is_some() {
            let prompt_id = prompt_id.unwrap_or_else(|| format!("p_{}", self.prompts));
            let run_id = format!("run_{}", self.prompts);
            self.view
                .feed_push(amp_core::chat_event::ChatEvent::user_message(
                    prompt_id.clone(),
                    &prompt,
                    &origin,
                ));
            self.view
                .feed_push(amp_core::chat_event::ChatEvent::RunStart {
                    run_id: run_id.clone(),
                    prompt_id,
                });
            self.run_id = Some(run_id);
        }
        self.view.take_error_shown();
        self.view.set_working(true);
        let harness = Arc::clone(&self.harness);
        let done = self.done_tx.clone();
        tokio::spawn(async move {
            // The run gets its own task so a panic in it still reports back here.
            let run =
                tokio::spawn(async move { harness.prompt_text(&model_text, Vec::new()).await });
            let result = match run.await {
                Ok(Ok(r)) => Ok(r),
                Ok(Err(e)) => Err(e.to_string()),
                Err(e) => Err(format!("the run crashed: {e}")),
            };
            let _ = done.send(result);
        });
    }

    /// The ends of this chat's workers that finished since the last turn, as a block the model
    /// reads at the top of the turn starting now (AMP #2835, #2857). `None` when nothing is
    /// waiting. Consuming them is what makes them arrive once: a later turn's drain is empty.
    fn take_worker_summaries(&self) -> Option<String> {
        let ends = self.supervisor.as_ref()?.take_launch_ends();
        worker_summaries(&ends)
    }

    /// Asks the running prompt to stop; it then finishes as aborted, and the transcript says
    /// "Stopped." once, below whatever the reply had streamed ([`Shell::finish_run`]).
    ///
    /// It stops only the reply: it never calls [`Supervisor::cancel_all`] (AMP #2858). In Await
    /// mode the reply's `amp_run_ticket` calls end, and their workers with them; in Launch mode
    /// dispatched workers keep running and only this reply's still-queued launches end
    /// ([`reply_stop_notice`] says which). `/workers` stops one worker; quitting stops them all.
    pub async fn abort(&mut self) {
        if self.running {
            self.warn_reply_stop();
        }
        self.abort_quietly().await;
    }

    /// [`Shell::abort`] without the note about workers ([`Shell::quit`] says its own).
    async fn abort_quietly(&mut self) {
        if !self.running {
            return;
        }
        self.view.set_stopping(true);
        self.clear_esc_hint();
        if let Err(e) = self.harness.abort().await {
            self.view.add_error(&format!("abort: {e}"));
        }
    }

    /// Records a finished run. Drain the agent events first: they were all sent before it ended.
    pub fn finish_run(&mut self, result: RunDone) {
        self.running = false;
        self.esc_hint_until = None;
        self.streaming = None;
        self.view.set_working(false);
        let stopping = self.view.stopping();
        self.view.set_stopping(false);
        // The reply's error line already says what went wrong: say it once (#2207).
        let error_shown = self.view.take_error_shown();
        // AMP #2889: the run ended stopped exactly when its `run_end` says `aborted`, which is
        // what makes the remote link drop its queue: the chat's queue goes with it, so the two
        // never disagree. A reply that had already ended when Esc reached it ended `completed`
        // (or `failed`), and both queues run on.
        let stopped = match &result {
            Ok(r) => match &r.outcome {
                HarnessRunOutcome::Aborted { .. } => true,
                HarnessRunOutcome::Failed { .. } => stopping,
                HarnessRunOutcome::Completed { .. } | HarnessRunOutcome::Suspended { .. } => false,
            },
            Err(_) => stopping,
        };
        if let Some(run_id) = self.run_id.take() {
            use amp_core::chat_event::{ChatEvent, RunOutcome};
            let (outcome, error) = match &result {
                Ok(r) => match &r.outcome {
                    HarnessRunOutcome::Completed { .. } => (RunOutcome::Completed, None),
                    HarnessRunOutcome::Aborted { .. } => (RunOutcome::Aborted, None),
                    HarnessRunOutcome::Failed { .. } if stopping => (RunOutcome::Aborted, None),
                    HarnessRunOutcome::Failed {
                        error,
                        final_message,
                        ..
                    } => (
                        RunOutcome::Failed,
                        Some(amp_core::provider_error::run_failure(
                            &error.code,
                            &error.message,
                            final_message.as_ref().map_or("", |m| m.provider.as_str()),
                        )),
                    ),
                    HarnessRunOutcome::Suspended { .. } => (RunOutcome::Suspended, None),
                },
                Err(_) if stopping => (RunOutcome::Aborted, None),
                Err(e) => (
                    RunOutcome::Failed,
                    Some(amp_core::provider_error::summarize("", e)),
                ),
            };
            self.view.feed_push(ChatEvent::RunEnd {
                run_id,
                outcome,
                error,
            });
        }
        match result {
            Ok(r) => match r.outcome {
                HarnessRunOutcome::Completed { .. } => self.begin_approval_wait(),
                HarnessRunOutcome::Aborted { .. } => self.view.add_notice(STOPPED),
                // The provider may report a stopped request as an error ("Request was
                // aborted"): the person asked for it, so it is not one.
                HarnessRunOutcome::Failed { .. } if stopping => self.view.add_notice(STOPPED),
                HarnessRunOutcome::Failed { .. } if error_shown => {}
                HarnessRunOutcome::Failed {
                    error,
                    final_message,
                    ..
                } => self.view.add_error(&format!(
                    "✗ {}",
                    amp_core::provider_error::run_failure(
                        &error.code,
                        &error.message,
                        final_message.as_ref().map_or("", |m| m.provider.as_str()),
                    )
                )),
                HarnessRunOutcome::Suspended { .. } => self
                    .view
                    .add_notice("the provider deferred its response (run suspended)"),
            },
            Err(_) if stopping => self.view.add_notice(STOPPED),
            Err(e) => self.view.add_error(&format!(
                "✗ run failed: {}",
                amp_core::provider_error::summarize("", &e)
            )),
        }
        self.after_run(stopped);
    }

    /// A key the editor doesn't get first: the `/` menu's keys ([`crate::slash::CommandMenu`]),
    /// then an open AMP panel's keys ([`Shell::panel_key`]), then the transcript's scroll keys
    /// ([`Shell::scroll_key`]), then Esc during a run, which stops the reply. Returns true when it was used; otherwise the caller gives it to the editor
    /// (Enter on the menu puts the chosen command in the editor and returns false, so the editor
    /// submits it).
    ///
    /// Esc that closes a panel (or a worker's session, [`Shell::session_key`]) during a run
    /// doesn't stop it: the spinner line says [`ESC_HINT`] for [`ESC_HINT_FOR`], naming this
    /// chat's live workers ([`esc_hint`]), and the next Esc stops the reply (#2181, #2354).
    pub async fn key(&mut self, key: &KeyEvent, editor_empty: bool) -> bool {
        // #2376: a pairing question takes the next key: `y` allows, anything else refuses.
        if self.pairing_pending() {
            let plain = !key
                .modifiers
                .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT);
            let allow = plain && matches!(key.code, KeyCode::Char('y') | KeyCode::Char('Y'));
            self.answer_pairing(allow);
            return true;
        }
        // #2252: a stop waiting for its answer takes the next key; a worker's session takes
        // every key while it is shown (the editor is hidden).
        if self.stop_confirm.is_some() {
            match key.code {
                KeyCode::Char('y') | KeyCode::Char('Y') | KeyCode::Enter => {
                    let _ = self.confirm_stop();
                }
                _ => self.cancel_stop(),
            }
            return true;
        }
        // #2347: the worker strip, while Ctrl+W gave it the keys.
        if self.view.strip().is_focused() && self.strip_key(key) {
            return true;
        }
        if self.viewing.is_some() {
            return self.session_key(key);
        }
        let is_esc = key.code == KeyCode::Esc
            && !key
                .modifiers
                .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT);
        let confirming = matches!(
            self.view.panel().state().map(|s| s.stage),
            Some(Stage::Confirm { .. } | Stage::Sending { .. })
        );
        // ↑↓ recall earlier prompts (#2348): before the `/` menu while a recalled prompt is
        // shown (a recalled `/usage` opens the menu), after it otherwise.
        if self.history_key(key, true) {
            return true;
        }
        if !confirming {
            match self.view.menu().key(key) {
                MenuKey::Pass => {}
                MenuKey::Run => return false,
                MenuKey::Used => {
                    if is_esc && self.running {
                        self.show_esc_hint();
                    }
                    return true;
                }
            }
        }
        if self.history_key(key, false) {
            return true;
        }
        let panel_open = self.view.panel().is_open();
        if self.panel_key(key, editor_empty) {
            if is_esc && self.running && panel_open && !self.view.panel().is_open() {
                self.show_esc_hint();
            }
            self.switch_pending_model().await;
            self.run_pending_command().await;
            return true;
        }
        if self.scroll_key(key, editor_empty) {
            return true;
        }
        if is_esc && self.running {
            self.abort().await;
            return true;
        }
        // #2425: Esc stops waiting for plan approval (the manager then waits for a reply).
        if is_esc && self.stop_approval_wait(Some(STOPPED_WAITING)) {
            return true;
        }
        false
    }

    /// Plain ↑ / ↓ as prompt history ([`crate::history`], #2348), while no panel is open (panels
    /// keep their ↑↓). `browsing_only`: only while a recalled prompt is shown unchanged (checked
    /// before the `/` menu, which otherwise keeps its ↑↓). Returns whether the key was used.
    fn history_key(&mut self, key: &KeyEvent, browsing_only: bool) -> bool {
        let recall = match key.code {
            KeyCode::Up => Recall::Older,
            KeyCode::Down => Recall::Newer,
            _ => return false,
        };
        if !key.modifiers.is_empty() || self.view.panel().is_open() {
            return false;
        }
        let editor = self.view.editor().clone();
        if browsing_only && !self.history.is_browsing(&editor.get_text()) {
            return false;
        }
        self.history.apply(recall, &editor)
    }

    /// The prompts recorded for ↑↓ (#2348), oldest first.
    pub fn history(&self) -> &[String] {
        self.history.entries()
    }

    /// The transcript's scroll keys, when no panel took the key (#2185):
    /// - PageUp / PageDown: a page up or down (the editor's own page keys are Ctrl+PageUp/Down);
    /// - Ctrl+U: half a page up, with an empty editor (with text, the editor deletes to the line
    ///   start); Ctrl+D half a page down is [`Shell::ctrl_d`];
    /// - End: back to the latest output, with an empty editor (with text it moves the cursor to
    ///   the line end); Ctrl+End always.
    ///
    /// Returns whether the key was one of these.
    pub fn scroll_key(&mut self, key: &KeyEvent, editor_empty: bool) -> bool {
        let scroll = self.view.scroll();
        let mods = key.modifiers;
        let plain = !mods.intersects(KeyModifiers::CONTROL | KeyModifiers::ALT);
        match key.code {
            KeyCode::PageUp if plain => scroll.page_by(-1),
            KeyCode::PageDown if plain => scroll.page_by(1),
            KeyCode::Char('u') if mods == KeyModifiers::CONTROL && editor_empty => {
                scroll.half_page_by(-1)
            }
            KeyCode::End if mods == KeyModifiers::CONTROL || (plain && editor_empty) => {
                scroll.to_end()
            }
            _ => return false,
        }
        true
    }

    /// Ctrl+D on the empty editor at `now` (#2185): scrolled up, it scrolls half a page down;
    /// at the latest output it stops a running reply or quits, as before, unless Ctrl+D scrolled
    /// within [`CTRL_D_SETTLE`] (then nothing, so scrolling down with it never quits by
    /// overshooting). With text in the editor, Ctrl+D is the editor's (delete forward).
    pub async fn ctrl_d(&mut self, now: Instant) -> CtrlD {
        let scroll = self.view.scroll();
        if !scroll.at_end() {
            scroll.half_page_by(1);
            self.ctrl_d_settle_until = Some(now + CTRL_D_SETTLE);
            return CtrlD::Scrolled;
        }
        if self.ctrl_d_settle_until.is_some_and(|until| now < until) {
            self.ctrl_d_settle_until = Some(now + CTRL_D_SETTLE);
            return CtrlD::Held;
        }
        self.ctrl_d_settle_until = None;
        if self.running && self.live_tickets().is_empty() {
            self.abort().await;
            return CtrlD::Stopped;
        }
        // With a worker of this chat live (in this reply or still reporting), ask first (#2204).
        if !self.confirm_quit(now) {
            return CtrlD::Confirm;
        }
        CtrlD::Quit
    }

    fn show_esc_hint(&mut self) {
        self.esc_hint_until = Some(Instant::now() + ESC_HINT_FOR);
        // AMP #2858: name only the workers the next Esc would stop (in Launch mode, this
        // reply's launches still waiting for a slot).
        let stopped = match self.dispatch_mode() {
            DispatchMode::Await => self.live_tickets(),
            DispatchMode::Launch => self.reply_stop().cancelled,
        };
        let mut hint = esc_hint(&stopped);
        // AMP #2889: stopping the reply drops what was queued behind it.
        match self.prompt_queue.len() {
            0 => {}
            1 => hint.push_str(" (drops the queued prompt)"),
            n => hint.push_str(&format!(" (drops {n} queued prompts)")),
        }
        self.view.set_working_message(&hint);
    }

    fn clear_esc_hint(&mut self) {
        if self.esc_hint_until.take().is_some() {
            self.show_working();
        }
    }

    /// Ctrl+C at `now` (#2184): stops a running reply, else clears a non-empty editor, else
    /// shows [`QUIT_HINT`] in the empty editor; a second Ctrl+C there within [`QUIT_HINT_FOR`]
    /// returns [`CtrlC::Quit`]. With a worker of this chat live, Ctrl+C never stops the reply
    /// (and the worker) or quits silently: it warns first ([`CtrlC::Confirm`],
    /// [`Shell::confirm_quit`]) and the next Ctrl+C quits (#2204). Signals still quit
    /// at once, and Ctrl+C during the "stopping N workers" wait skips it ([`Shell::quit`]).
    pub async fn ctrl_c(&mut self, now: Instant) -> CtrlC {
        if self.running && !self.live_tickets().is_empty() {
            // A worker of this chat is running inside this reply: stopping the reply would stop
            // it too, which is what the person pressing Ctrl+C (to quit) may not know. Ask first;
            // the next Ctrl+C quits (the quit stops and reports the workers). Esc still only
            // stops the reply.
            self.clear_quit_hint();
            return if self.confirm_quit(now) {
                CtrlC::Quit
            } else {
                CtrlC::Confirm
            };
        }
        if self.running {
            self.clear_quit_hint();
            self.abort().await;
            return CtrlC::Stopped;
        }
        if !self.view.editor().get_text().is_empty() {
            self.clear_quit_hint();
            self.view.editor().clear();
            return CtrlC::Cleared;
        }
        if self.quit_confirm_until.is_some_and(|until| now < until) {
            // The worker warning is up: this Ctrl+C confirms it.
            self.clear_quit_confirm();
            return CtrlC::Quit;
        }
        if self.quit_hint_until.is_some_and(|until| now < until) {
            self.clear_quit_hint();
            if !self.confirm_quit(now) {
                return CtrlC::Confirm;
            }
            return CtrlC::Quit;
        }
        self.quit_hint_until = Some(now + QUIT_HINT_FOR);
        self.view.set_editor_hint(Some(QUIT_HINT));
        CtrlC::Hint
    }

    /// Takes [`QUIT_HINT`] away: the next Ctrl+C on the empty editor only hints again. The
    /// terminal loop calls it for every other key, so the window is two Ctrl+Cs in a row.
    pub fn clear_quit_hint(&mut self) {
        if self.quit_hint_until.take().is_some() {
            self.view.set_editor_hint(None);
        }
    }

    /// Puts the spinner line back to "Working…" once [`ESC_HINT`] has been shown for
    /// [`ESC_HINT_FOR`] at `now`, and the empty editor back to its placeholder once
    /// [`QUIT_HINT_FOR`] has passed. Returns true when either changed. [`Shell::poll`] calls it.
    pub fn expire_hints(&mut self, now: Instant) -> bool {
        let mut changed = false;
        if self.esc_hint_until.is_some_and(|until| now >= until) {
            self.clear_esc_hint();
            changed = true;
        }
        if self.quit_hint_until.is_some_and(|until| now >= until) {
            self.clear_quit_hint();
            changed = true;
        }
        if self.quit_confirm_until.is_some_and(|until| now >= until) {
            self.clear_quit_confirm();
            changed = true;
        }
        changed
    }

    async fn command(&mut self, name: &str, arg: &str) -> Submit {
        match name {
            "quit" | "exit" | "q" => {
                if self.confirm_quit(Instant::now()) {
                    return Submit::Quit;
                }
            }
            "help" | "?" => self.view.add_to_transcript(Arc::new(
                crate::slash::HelpBlock::new()
                    .mouse_capture(self.mouse_capture)
                    .newline(self.newline),
            )),
            "usage" => self.view.add_notice(&self.usage_text()),
            "model" if arg.is_empty() => self.model_picker(),
            // `/model provider/model` switches this chat; `/model roles`, `/model <role> …` are a
            // role's model (#2245).
            "model" if !is_model_ref(arg) => self.config_command("model", arg),
            "model" => self.switch_model(arg).await,
            // `/config thinking …` is the chat's own (#2345).
            "config" if arg.split_whitespace().next() == Some("thinking") => {
                self.thinking_command(arg.trim_start().trim_start_matches("thinking"))
            }
            "mcp" | "agents" | "skills" | "config" => self.config_command(name, arg),
            "connect" => self.connect_command(arg),
            "abort" => self.abort().await,
            // #2425: stops the reply, or the wait for plan approval.
            "stop" => {
                if self.running {
                    self.abort().await;
                } else if !self.stop_approval_wait(Some(STOPPED_WAITING)) {
                    self.view.add_notice("/stop: nothing to stop");
                }
            }
            "amp" => self.open_panel("amp", PanelKind::Amp),
            "workers" => self.open_panel("workers", PanelKind::Workers),
            "worker" => self.worker_command(arg),
            "questions" => self.open_panel("questions", PanelKind::Questions),
            "ticket" => self.ticket_command(arg),
            "project" => self.project_command(arg),
            "plan" => self.plan_command(),
            "open" => self.open_command(arg),
            "server" => self.server_command(arg),
            "reload" => self.reload_command(ReloadCause::Command),
            "remote" => self.remote_command(arg),
            "close" => self.view.panel().close(),
            "" => self.view.add_notice("empty command (try /help)"),
            other => self
                .view
                .add_notice(&format!("unknown command: /{other} (try /help)")),
        }
        Submit::Continue
    }

    /// The metadata poller, or a notice that the chat has none (`--snapshot`, no AMP).
    fn need_meta(&self, command: &str) -> Option<MetaHandle> {
        if self.meta.is_none() {
            self.view.add_notice(&format!(
                "/{command}: no AMP metadata in this session (the metadata poller is not running)"
            ));
        }
        self.meta.clone()
    }

    fn open_panel(&mut self, command: &str, kind: PanelKind) {
        if self.need_meta(command).is_some() {
            self.view.panel().open(PanelState::new(kind));
        }
    }

    /// `/ticket` shows the current ticket; `/ticket 2108` makes #2108 current; `/ticket none`
    /// clears it.
    fn ticket_command(&mut self, arg: &str) {
        let Some(meta) = self.need_meta("ticket") else {
            return;
        };
        match arg {
            "" => {}
            "none" | "clear" | "-" => {
                meta.set_ticket(None);
                self.view.panel().close();
                self.view.add_notice("no current ticket");
                return;
            }
            _ => match parse_id(arg) {
                Some(id) => meta.set_ticket(Some(id)),
                None => {
                    self.view.add_error(&format!(
                        "/ticket: `{}` is not a ticket id (try /ticket 2108, or /ticket none)",
                        crate::chrome::clean(arg)
                    ));
                    return;
                }
            },
        }
        self.view.panel().open(PanelState::new(PanelKind::Ticket));
    }

    /// `/project` shows the project; `/project 2` switches the header and board to project 2.
    fn project_command(&mut self, arg: &str) {
        let Some(meta) = self.need_meta("project") else {
            return;
        };
        if !arg.is_empty() {
            match parse_id(arg) {
                Some(id) => meta.set_project(Some(id)),
                None => {
                    self.view.add_error(&format!(
                        "/project: `{}` is not a project id (try /project 2)",
                        crate::chrome::clean(arg)
                    ));
                    return;
                }
            }
        }
        self.view.panel().open(PanelState::new(PanelKind::Project));
    }

    /// `/server` lists the servers; `/server <name|address>` switches to one (#2164), in the
    /// background, refused while a reply or a worker runs.
    fn server_command(&mut self, arg: &str) {
        let Some(control) = self.server.clone() else {
            self.view
                .add_notice("/server: not available in this session (no AMP connection to switch)");
            return;
        };
        if arg.is_empty() {
            self.view.add_notice(&control.describe());
            return;
        }
        if self.switching {
            self.view
                .add_notice("/server: already switching; wait for it to finish");
            return;
        }
        if self.running {
            self.view.add_notice(
                "/server: wait for the reply (or press Esc to abort it), then switch: its AMP calls \
                 would be cut off",
            );
            return;
        }
        let live = self.live_tickets().len();
        if live > 0 {
            self.view.add_notice(&format!(
                "/server: {live} worker{} still running on this server (/workers); switch once {} done",
                if live == 1 { " is" } else { "s are" },
                if live == 1 { "it is" } else { "they are" },
            ));
            return;
        }
        self.switching = true;
        self.in_flight += 1;
        self.view
            .add_notice(&format!("switching to {}…", crate::chrome::clean(arg)));
        let tx = self.msgs_tx.clone();
        let target = arg.to_owned();
        tokio::spawn(async move {
            // Its own task, so a panic in the switch still ends `switching`.
            let result = match tokio::spawn(control.switch(target)).await {
                Ok(result) => result,
                Err(e) => Err(format!("the switch crashed: {e}")),
            };
            let _ = tx.send(ShellMsg::Server(result));
        });
    }

    /// `/plan`: plan approval from the snapshot, and the plan tree from `amp_get_plan` (read on
    /// the poller's connection, in the background). The plan counts stored states only, so the
    /// blocked tickets are read too (`amp_list_tasks(owner=any, state=blocked, format=full)`), to
    /// show questions, scheduled and waiting tickets apart like the web UI.
    fn plan_command(&mut self) {
        let Some(meta) = self.need_meta("plan") else {
            return;
        };
        let mut state = PanelState::new(PanelKind::Plan);
        if let Some(project_id) = meta.snapshot().project_id {
            let tx = self.msgs_tx.clone();
            self.in_flight += 1;
            tokio::spawn(async move {
                let result = meta
                    .call(
                        "amp_get_plan",
                        json!({"project_id": project_id}),
                        READ_TIMEOUT,
                    )
                    .await;
                let blocked = match &result {
                    Ok(_) => Some(
                        meta.call(
                            "amp_list_tasks",
                            json!({"project_id": project_id, "owner": "any", "state": "blocked", "format": "full"}),
                            READ_TIMEOUT,
                        )
                        .await,
                    ),
                    Err(_) => None,
                };
                let _ = tx.send(ShellMsg::Plan {
                    project_id,
                    result,
                    blocked,
                });
            });
        } else {
            state.plan = Some(Err("no project".to_owned()));
        }
        self.view.panel().open(state);
    }

    /// `/open` opens the current ticket (or `/open 2108`, or the project's Mission page without a
    /// ticket) in the browser. Only http(s) URLs from the instance's URL helpers are opened.
    fn open_command(&mut self, arg: &str) {
        let Some(meta) = self.need_meta("open") else {
            return;
        };
        let m = meta.snapshot();
        // `/open plan`: where a person approves the plan (#2171): the project's My work tab with
        // the Approve view open (#2495; Flight plan on an AMP older than My work).
        if arg.eq_ignore_ascii_case("plan") {
            match (&m.instance, m.project_id) {
                (Some(_), Some(pid)) => {
                    let url = m.approve_url(pid).unwrap_or_default();
                    self.open_url(&url);
                    self.refresh_pending_plan();
                }
                (None, _) => self
                    .view
                    .add_error("/open: not connected to AMP yet (no instance to link to)"),
                (Some(_), None) => self
                    .view
                    .add_error("/open plan: no project yet (/project <id>)"),
            }
            return;
        }
        let ticket = if arg.is_empty() {
            m.ticket_id
        } else {
            match parse_id(arg) {
                Some(id) => Some(id),
                None => {
                    self.view.add_error(&format!(
                        "/open: `{}` is not a ticket id",
                        crate::chrome::clean(arg)
                    ));
                    return;
                }
            }
        };
        let Some(inst) = &m.instance else {
            self.view
                .add_error("/open: not connected to AMP yet (no instance to link to)");
            return;
        };
        let url = match (ticket, m.project_id) {
            (Some(id), Some(pid)) => inst.ticket_url(pid, id),
            (None, Some(pid)) => inst.project_url(pid),
            (Some(_), None) => {
                self.view.add_error(
                    "/open: no project, so no board to open the ticket on (/project <id>)",
                );
                return;
            }
            (None, None) => inst.web_base.clone(),
        };
        self.open_url(&url);
    }

    fn open_url(&mut self, url: &str) {
        if !overlay::openable(url) {
            self.view
                .add_error("/open: not a plain http(s) URL; refusing to open it");
            return;
        }
        match (self.opener)(url) {
            Ok(()) => self.view.add_notice(&format!("opened {url}")),
            Err(e) => self.view.add_error(&format!("/open {url}: {e}")),
        }
    }

    /// A key while a panel is open. Returns true when the panel used it (the editor must not).
    ///
    /// Esc closes the panel (or backs out of an answer). While the editor is empty and has not
    /// been given the keys, ↑/↓/PgUp/PgDn select and Enter acts on the selection (answers a
    /// question; otherwise opens it); a configuration panel's letters are commands. Any other
    /// printable key is typing: it goes to the editor, which keeps the keys (#2161: typing "ok…"
    /// must never open a browser). Tab, with an empty editor, switches the keys between the panel
    /// and the editor. While an answer waits for confirmation, `y`/Enter sends it and `n`/Esc
    /// cancels; other keys are swallowed.
    ///
    /// #2395: the /workers panel takes no printable key at all, so typing always reaches the
    /// editor while it is open. Its actions are chords instead ([`overlay::WORKERS_STOP_KEY`],
    /// [`overlay::WORKERS_OPEN_KEY`]), which act on the selected row whatever the editor holds.
    pub fn panel_key(&mut self, key: &KeyEvent, editor_empty: bool) -> bool {
        let panel = self.view.panel().clone();
        let Some(state) = panel.state() else {
            return false;
        };
        if state.kind == PanelKind::Workers
            && state.stage == Stage::Browse
            && key.modifiers == KeyModifiers::CONTROL
        {
            match key.code {
                KeyCode::Char('x') => {
                    if let Some(Target::Ticket { id, .. }) = panel.selected() {
                        self.ask_stop(id);
                    }
                    return true;
                }
                KeyCode::Char('g') => {
                    if let Some(target) = panel.selected() {
                        self.open_target(&target);
                    }
                    return true;
                }
                _ => {}
            }
        }
        if key
            .modifiers
            .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT)
        {
            return false;
        }
        match state.stage {
            Stage::Answer { .. } => {
                if key.code == KeyCode::Esc {
                    panel.update(|s| {
                        s.stage = Stage::Browse;
                        s.note = Some(("not sent".to_owned(), false));
                    });
                    return true;
                }
                false
            }
            Stage::Confirm { ticket, answer, .. } => {
                match key.code {
                    KeyCode::Char('y') | KeyCode::Char('Y') | KeyCode::Enter => {
                        self.send_answer(ticket, answer)
                    }
                    KeyCode::Char('n') | KeyCode::Char('N') | KeyCode::Esc => {
                        panel.update(|s| {
                            s.stage = Stage::Browse;
                            s.note = Some(("not sent".to_owned(), false));
                        });
                    }
                    _ => {}
                }
                true
            }
            Stage::Sending { .. } => true,
            Stage::Browse => {
                if key.code == KeyCode::Esc {
                    panel.close();
                    return true;
                }
                // #2161: Tab (with an empty editor) hands the keys to the editor and back.
                if key.code == KeyCode::Tab && editor_empty {
                    self.editor_clicked = !self.editor_clicked;
                    return true;
                }
                // #2344: a click into the editor (or typing, Tab, a paste) gave it the keys.
                if !editor_empty || self.editor_clicked {
                    return false;
                }
                match key.code {
                    KeyCode::Up => panel.move_selection(-1),
                    KeyCode::Down => panel.move_selection(1),
                    KeyCode::PageUp => panel.move_selection(-5),
                    KeyCode::PageDown => panel.move_selection(5),
                    KeyCode::Enter => match panel.selected() {
                        Some(Target::Ticket {
                            id,
                            question: Some(question),
                        }) if state.kind == PanelKind::Questions => {
                            panel.update(|s| {
                                s.stage = Stage::Answer {
                                    ticket: id,
                                    question,
                                };
                                s.note = None;
                            });
                        }
                        Some(Target::Model(reference)) => {
                            panel.close();
                            self.pending_model = Some(reference);
                        }
                        Some(Target::Item(_)) => {
                            if let Some(command) = overlay::info_enter(&state) {
                                self.pending_command = Some(command);
                            }
                        }
                        // #2252: Enter on a worker opens its session (o still opens AMP).
                        Some(Target::Ticket { id, .. })
                            if state.kind == PanelKind::Workers && self.has_session(id) =>
                        {
                            if let Err(e) = self.open_worker(id) {
                                panel.update(|s| s.note = Some((e, true)));
                            }
                        }
                        Some(target) => self.open_target(&target),
                        None => {}
                    },
                    // A configuration panel's letter keys (#2245).
                    KeyCode::Char(c)
                        if state.kind == PanelKind::Info
                            && overlay::info_command(&state, c).is_some() =>
                    {
                        self.pending_command = overlay::info_command(&state, c);
                    }
                    // #2161: any other printable key is typing. It goes to the editor, and so do
                    // the keys after it, so a prompt starting "ok" never opens a browser. A
                    // configuration panel keeps the keys: its letters are commands, and one that
                    // doesn't apply to the row does nothing (#2245). #2395: /workers has no letter
                    // commands (Ctrl+X stops, Ctrl+G opens), so `x` and `o` are typing too.
                    KeyCode::Char(_) if state.kind != PanelKind::Info => {
                        self.editor_clicked = true;
                        return false;
                    }
                    _ => return false,
                }
                true
            }
        }
    }

    /// [`Self::panel_key`] by name, with an empty editor (`--snapshot`'s `--input key:<name>`):
    /// `enter`, `esc`, `up`, `down`, `tab`, or one character. `ctrl+t` toggles the last thinking,
    /// `ctrl+o` the tool calls' details.
    pub async fn press(&mut self, name: &str) -> Result<bool, String> {
        if let Some(text) = name.strip_prefix("paste:") {
            self.paste(text);
            return Ok(true);
        }
        if name.eq_ignore_ascii_case("ctrl+t") {
            return Ok(self.toggle_thinking());
        }
        if name.eq_ignore_ascii_case("ctrl+o") {
            self.toggle_tools();
            return Ok(true);
        }
        if name.eq_ignore_ascii_case("ctrl+w") {
            return Ok(self.toggle_workers_focus());
        }
        let code = match name {
            "enter" => KeyCode::Enter,
            "esc" => KeyCode::Esc,
            "up" => KeyCode::Up,
            "down" => KeyCode::Down,
            "tab" => KeyCode::Tab,
            "backtab" => KeyCode::BackTab,
            "left" => KeyCode::Left,
            "right" => KeyCode::Right,
            c if c.chars().count() == 1 => KeyCode::Char(c.chars().next().unwrap_or(' ')),
            other => {
                return Err(format!(
                    "key `{other}`: not a key (enter, esc, up, down, tab, or one character)"
                ));
            }
        };
        let key = KeyEvent::new(code, KeyModifiers::NONE);
        if self.secret_key(&key) {
            return Ok(true);
        }
        if self.stop_confirm.is_some()
            || self.pairing_pending()
            || self.viewing.is_some()
            || self.view.strip().is_focused()
        {
            return Ok(self.key(&key, true).await);
        }
        let used = self.panel_key(&key, true);
        self.switch_pending_model().await;
        self.run_pending_command().await;
        Ok(used)
    }

    /// Runs the slash command a configuration panel's key chose, if one did.
    async fn run_pending_command(&mut self) {
        if let Some(command) = self.pending_command.take() {
            // Boxed: `submit` reaches `key` only through the terminal, never back here.
            Box::pin(self.submit(&command)).await;
        }
    }

    fn open_target(&mut self, target: &Target) {
        if matches!(target, Target::Model(_) | Target::Item(_)) {
            return;
        }
        let Some(meta) = &self.meta else { return };
        match overlay::target_url(&meta.snapshot(), target) {
            Some(url) => self.open_url(&url),
            None => self
                .view
                .add_error("/open: no AMP instance or project to link to yet"),
        }
    }

    /// Sends a confirmed answer with `amp_answer_question` on the poller's connection.
    fn send_answer(&mut self, ticket: i64, answer: String) {
        let Some(meta) = self.meta.clone() else {
            return;
        };
        self.view.panel().update(|s| {
            s.stage = Stage::Sending { ticket };
            s.note = None;
        });
        let tx = self.msgs_tx.clone();
        self.in_flight += 1;
        tokio::spawn(async move {
            let result = meta
                .call(
                    "amp_answer_question",
                    json!({"task_id": ticket, "answer": answer}),
                    WRITE_TIMEOUT,
                )
                .await;
            let _ = tx.send(ShellMsg::Answered { ticket, result });
        });
    }

    /// Applies finished command calls and follows the metadata (the ticket marker, the footer).
    /// Returns whether anything changed. The terminal loop calls it on its tick.
    pub fn poll(&mut self) -> bool {
        let mut changed = self.expire_hints(Instant::now());
        changed |= self.poll_remote();
        changed |= self.build_roster();
        changed |= self.show_launch_ends();
        changed |= self.pump_sessions();
        while let Ok(msg) = self.msgs_rx.try_recv() {
            changed = true;
            if let ShellMsg::Notice { text, error } = &msg {
                if *error {
                    self.view.add_error(text);
                } else {
                    self.view.add_notice(text);
                }
                continue;
            }
            if let ShellMsg::Compaction { active, manual } = &msg {
                self.view.set_compaction(*active, *manual);
                continue;
            }
            if let ShellMsg::ReloadRequest = &msg {
                self.reload_command(ReloadCause::Watcher);
                continue;
            }
            if let ShellMsg::ProjectLinked(link) = msg {
                if let Some(meta) = &self.meta {
                    meta.link_project(link.project_id, link.marker);
                }
                if let Some((from, to)) = &link.banner {
                    self.view.replace_in_banner(from, to);
                }
                self.view.add_notice(&link.notice);
                continue;
            }
            let msg = match msg {
                ShellMsg::Remote(call) => {
                    self.remote_call(call);
                    continue;
                }
                other => other,
            };
            self.in_flight = self.in_flight.saturating_sub(1);
            match msg {
                ShellMsg::Plan {
                    project_id,
                    result,
                    blocked,
                } => {
                    let current = self.meta.as_ref().and_then(|m| m.snapshot().project_id);
                    if current == Some(project_id) {
                        self.view.panel().update(|s| {
                            if s.kind == PanelKind::Plan {
                                s.plan = Some(result);
                                s.plan_blocked = blocked;
                            }
                        });
                    }
                }
                ShellMsg::Connected { result, models } => {
                    if let Some(models) = models {
                        self.models = models;
                    }
                    match result {
                        Ok(text) => self.view.add_notice(&text),
                        Err(e) => self
                            .view
                            .add_error(&format!("/connect: {}", crate::chrome::clean(&e))),
                    }
                }
                ShellMsg::Server(result) => {
                    self.switching = false;
                    match result {
                        Ok(switched) => {
                            if let Some(meta) = switched.meta {
                                self.set_meta(meta);
                            }
                            self.view.add_notice(&switched.notice);
                        }
                        Err(e) => self.view.add_error(&format!("/server: {e}")),
                    }
                }
                ShellMsg::Reloaded { result, cause } => {
                    self.reloading = false;
                    self.apply_reload(result, cause);
                    if let Some(again) = self.reload_again.take() {
                        self.reload_command(again);
                    }
                }
                ShellMsg::Config { result, command } => self.apply_config(result, &command),
                ShellMsg::RemoteDone(reply) => self.remote_done(reply),
                ShellMsg::Notice { .. }
                | ShellMsg::ReloadRequest
                | ShellMsg::Remote(_)
                | ShellMsg::ProjectLinked(_)
                | ShellMsg::Compaction { .. } => {}
                ShellMsg::Answered { ticket, result } => match result {
                    Ok(_) => {
                        self.view.panel().update(|s| {
                            s.stage = Stage::Browse;
                            s.note = Some((format!("answered #{ticket}"), false));
                        });
                        self.view
                            .add_notice(&format!("answered #{ticket} (amp_answer_question)"));
                    }
                    Err(e) => {
                        let e = crate::chrome::clean(&e);
                        self.view.panel().update(|s| {
                            s.stage = Stage::Browse;
                            s.note = Some((format!("not answered: {e}"), true));
                        });
                        self.view
                            .add_error(&format!("answer to #{ticket} failed: {e}"));
                    }
                },
            }
        }
        self.refresh_remote_status();
        // The Done marker first (sync_meta), then let the finished ticket go.
        let synced = self.sync_meta();
        let watched = self.watch_approval();
        changed | synced | watched | self.clear_done_ticket()
    }

    /// A current ticket stops being current once it is done (UX review #2175 finding 12,
    /// #2187), so a "■ Done #id" line doesn't stay pinned under the header for the rest of the
    /// session. It goes once its snapshot says Done and the /ticket panel isn't showing it: a
    /// ticket completed mid-session goes straight away; one picked with `/ticket <id>` that was
    /// already done stays while its panel is open and goes when the panel closes. The Done
    /// marker in the transcript has already said so, so no "no current ticket" marker follows.
    fn clear_done_ticket(&mut self) -> bool {
        let Some(meta) = &self.meta else {
            return false;
        };
        let m = meta.snapshot();
        let Some(id) = m.ticket_id else {
            self.cleared_done = None;
            return false;
        };
        let done = m
            .ticket
            .get()
            .is_some_and(|t| t.id == id && t.status == Status::Done);
        let shown = self
            .view
            .panel()
            .state()
            .is_some_and(|s| s.kind == PanelKind::Ticket);
        if !done || shown || self.cleared_done == Some(id) {
            return false;
        }
        self.cleared_done = Some(id);
        meta.set_ticket(None);
        if self.marked == Some((id, Some(Status::Done))) {
            self.marked = None;
        }
        true
    }

    /// Waits (at most `timeout`) until every command call has finished, applying them. The
    /// headless driver's counterpart of the terminal tick.
    pub async fn settle(&mut self, timeout: Duration) -> Result<(), String> {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            self.poll();
            if self.in_flight == 0 {
                return Ok(());
            }
            if tokio::time::Instant::now() >= deadline {
                return Err(format!("AMP command calls still running after {timeout:?}"));
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }

    /// Draws the ticket marker when the current ticket or its state changed, and keeps the
    /// footer's `#ticket` in step.
    fn sync_meta(&mut self) -> bool {
        let Some(meta) = &self.meta else {
            return false;
        };
        let m = meta.snapshot();
        if m.revision == self.seen_revision {
            return false;
        }
        self.seen_revision = m.revision;
        let mut changed = false;
        if m.ticket_id != self.footer_ticket {
            self.footer_ticket = m.ticket_id;
            self.view.set_footer(&self.info.footer(self.footer_ticket));
            changed = true;
        }
        // Wait for a new ticket to load (or fail) before marking it.
        if let Some(key) = amp_render::marker_for(&m)
            && key != self.marked
        {
            self.marked = key;
            self.view
                .add_to_transcript(amp_render::marker_component(&m));
            changed = true;
        }
        changed
    }

    fn usage_text(&self) -> String {
        let t = &self.totals;
        let u = &t.usage;
        let failed = if t.tool_errors > 0 {
            format!(" ({} failed)", t.tool_errors)
        } else {
            String::new()
        };
        format!(
            "usage this session: {}, {}, {}{failed}\n\
             tokens: input {} · output {} · cache read {} · cache write {} · cost ${:.4}\n\
             {}\n\
             model: {} • thinking {}\n\
             session: {}",
            count(self.prompts as u64, "prompt", "prompts"),
            count(t.turns as u64, "turn", "turns"),
            count(t.tool_calls as u64, "tool call", "tool calls"),
            u.input_tokens,
            u.output_tokens,
            u.cache_read_tokens,
            u.cache_write_tokens,
            u.cost_usd,
            self.context_text(),
            self.info.model_ref(),
            thinking_name(self.info.thinking),
            self.info
                .session_file
                .as_deref()
                .unwrap_or("in memory (not saved)"),
        )
    }

    /// `/usage`'s context line: what the footer's `ctx 4%` stands for, and the cache hit rate.
    fn context_text(&self) -> String {
        let footer = self.view.footer();
        let mut text = match footer.context() {
            Some(c) => format!(
                "context: {} of {} tokens ({})",
                crate::footer::format_tokens(c.tokens),
                crate::footer::format_tokens(c.window),
                crate::footer::context_label(c.percent),
            ),
            None => "context: not reported yet".to_owned(),
        };
        if let Some(rate) = footer.cache_hit_rate() {
            text.push_str(&format!(" · cache hit {rate:.1}%"));
        }
        text
    }

    /// `/model` with no argument: the picker panel over the models this chat can switch to,
    /// the current one selected (#2188). With none to pick from, the model line as a notice.
    fn model_picker(&mut self) {
        if self.models.is_empty() {
            self.view.add_notice(&self.model_text());
            return;
        }
        let current = self.info.model_ref();
        let rows = self
            .models
            .iter()
            .map(|m| ModelRow {
                reference: m.reference.clone(),
                current: m.reference == current,
                context_window: m.model.context_window,
                reasoning: m.model.reasoning,
            })
            .collect();
        let mut state = PanelState::models(rows);
        state.note = Some((
            format!(
                "now {} • thinking {} (role {}); /model roles: every role's model",
                current,
                thinking_name(self.info.thinking),
                short_role(&self.info.role)
            ),
            false,
        ));
        self.view.panel().open(state);
    }

    /// Switches to the model chosen in the picker, if one was.
    async fn switch_pending_model(&mut self) {
        if let Some(reference) = self.pending_model.take() {
            self.switch_model(&reference).await;
        }
    }

    fn model_text(&self) -> String {
        let mut text = format!(
            "model: {} • thinking {} (role {})",
            self.info.model_ref(),
            thinking_name(self.info.thinking),
            short_role(&self.info.role),
        );
        if self.models.is_empty() {
            text.push_str("\nno other models: add a provider with /connect");
        }
        text
    }

    /// A finished reload: the transcript says what changed; `/model` gets the new choices; the
    /// chat's role's new model (from the config) is switched to like `/model` would, from the
    /// next run.
    fn apply_reload(&mut self, result: Result<ReloadReply, String>, cause: ReloadCause) {
        let reply = match result {
            Ok(reply) => reply,
            Err(e) => {
                let what = match cause {
                    ReloadCause::Watcher => "auto-reload",
                    ReloadCause::Command => "/reload",
                    ReloadCause::Panel => "applying the change",
                };
                self.view
                    .add_error(&format!("{what}: {}", crate::chrome::clean(&e)));
                return;
            }
        };
        self.models = reply.models;
        let mut lines = reply.lines;
        let mut stays = reply.stays;
        let choice = reply.switch_to.and_then(|reference| {
            let found = self
                .models
                .iter()
                .find(|m| m.reference == reference)
                .cloned();
            if found.is_none() {
                stays.get_or_insert(format!("{reference} isn't available here"));
            }
            found.map(|choice| (reference, choice))
        });
        if let Some(why) = stays {
            lines.push(format!(
                "this chat stays on {}: {why}; /model picks another for this chat",
                self.info.model_ref()
            ));
        }
        if let Some((reference, choice)) = choice {
            let thinking = if choice.model.reasoning {
                choice.model.clamp_thinking_level(
                    self.info
                        .requested_thinking
                        .unwrap_or(ThinkingLevel::Medium),
                )
            } else {
                ThinkingLevel::Off
            };
            let (provider, model_id) = reference.split_once('/').unwrap_or(("", &reference));
            self.info.provider = provider.to_owned();
            self.info.model_id = model_id.to_owned();
            self.info.thinking = thinking;
            self.info.context_window = choice.model.context_window;
            self.view.set_footer(&self.info.footer(self.footer_ticket));
            lines.push(format!(
                "this chat now runs on {} • thinking {} (from the next reply)",
                self.info.model_ref(),
                thinking_name(thinking)
            ));
            let harness = Arc::clone(&self.harness);
            let model = choice.model.clone();
            let tx = self.msgs_tx.clone();
            tokio::spawn(async move {
                let mut failed = harness.set_model(model).await.err().map(|e| e.to_string());
                if failed.is_none() {
                    failed = harness
                        .set_thinking_level(thinking)
                        .await
                        .err()
                        .map(|e| format!("thinking: {e}"));
                }
                if let Some(e) = failed {
                    let _ = tx.send(ShellMsg::Notice {
                        text: format!("/reload: could not switch the model: {e}"),
                        error: true,
                    });
                }
            });
        }
        if lines.is_empty() {
            if cause == ReloadCause::Command {
                self.view
                    .add_notice("reloaded: nothing changed since the last load");
            }
        } else {
            let head = match cause {
                ReloadCause::Watcher => {
                    "configuration changed elsewhere; reloaded (workers already running keep theirs):"
                }
                ReloadCause::Command => "reloaded (workers already running keep theirs):",
                ReloadCause::Panel => "applied (workers already running keep theirs):",
            };
            self.view
                .add_notice(&format!("{head}\n  {}", lines.join("\n  ")));
        }
        for w in reply.warnings {
            self.view.add_error(&w);
        }
    }

    async fn switch_model(&mut self, reference: &str) {
        if self.running {
            self.view
                .add_notice("can't switch models during a run (Esc aborts it)");
            return;
        }
        let Some(choice) = self
            .models
            .iter()
            .find(|m| m.reference == reference)
            .cloned()
        else {
            self.view.add_error(&format!(
                "unknown model `{reference}` (/model picks from the available ones)"
            ));
            return;
        };
        let thinking = if choice.model.reasoning {
            choice.model.clamp_thinking_level(
                self.info
                    .requested_thinking
                    .unwrap_or(ThinkingLevel::Medium),
            )
        } else {
            ThinkingLevel::Off
        };
        let model = choice.model.clone();
        if let Err(e) = self.harness.set_model(model).await {
            self.view.add_error(&format!("/model: {e}"));
            return;
        }
        if let Err(e) = self.harness.set_thinking_level(thinking).await {
            self.view.add_error(&format!("/model: thinking: {e}"));
        }
        let (provider, model_id) = reference.split_once('/').unwrap_or(("", reference));
        self.info.provider = provider.to_owned();
        self.info.model_id = model_id.to_owned();
        self.info.thinking = thinking;
        self.info.context_window = choice.model.context_window;
        self.view.set_footer(&self.info.footer(self.footer_ticket));
        self.view.add_notice(&format!(
            "model: {} • thinking {} (this session only)",
            self.info.model_ref(),
            thinking_name(thinking)
        ));
    }

    /// Runs until no prompt is running (or `timeout`), applying events and finished runs.
    /// The headless driver for tests and `--snapshot`; the terminal loop does the same in its
    /// `select!`.
    pub async fn drive_until_idle(
        &mut self,
        events: &mut UnboundedReceiver<AgentEvent>,
        done: &mut UnboundedReceiver<RunDone>,
        timeout: Duration,
    ) -> Result<(), String> {
        let deadline = tokio::time::Instant::now() + timeout;
        while self.running {
            tokio::select! {
                biased;
                Some(event) = events.recv() => self.handle_event(&event),
                Some(result) = done.recv() => {
                    while let Ok(event) = events.try_recv() {
                        self.handle_event(&event);
                    }
                    self.finish_run(result);
                }
                _ = tokio::time::sleep_until(deadline) => {
                    return Err(format!("the run did not finish within {timeout:?}"));
                }
            }
        }
        while let Ok(event) = events.try_recv() {
            self.handle_event(&event);
        }
        Ok(())
    }
}

/// Worker sessions (#2252): open one, follow it, switch, go back, stop.
impl Shell {
    /// Who the chat says stopped a worker (`stopped by <person> from the chat`). Without it, the
    /// AMP user's email, else `$USER`.
    pub fn set_person(&mut self, name: &str) {
        self.person = Some(name.trim().to_owned()).filter(|n| !n.is_empty());
    }

    fn person(&self) -> String {
        if let Some(p) = &self.person {
            return p.clone();
        }
        if let Some(meta) = &self.meta
            && let Some(user) = meta.snapshot().user.get()
            && !user.email.trim().is_empty()
        {
            return user.email.trim().to_owned();
        }
        std::env::var("USER")
            .ok()
            .filter(|u| !u.trim().is_empty())
            .unwrap_or_else(|| "the person".to_owned())
    }

    /// The ticket whose worker session is shown.
    pub fn viewing(&self) -> Option<i64> {
        self.viewing
    }

    /// A worker session opened in this chat.
    pub fn session(&self, task_id: i64) -> Option<&SessionView> {
        self.sessions.get(&task_id)
    }

    fn shown_session(&self) -> Option<&SessionView> {
        self.viewing.and_then(|id| self.sessions.get(&id))
    }

    /// The ticket whose stop is waiting for `y`.
    pub fn stop_pending(&self) -> Option<i64> {
        self.stop_confirm
    }

    /// Whether ticket `task_id` has a worker session to show: one of this chat's, or a session
    /// file from an earlier one.
    pub fn has_session(&self, task_id: i64) -> bool {
        self.sessions.contains_key(&task_id)
            || self.supervisor.as_ref().is_some_and(|s| {
                s.session(task_id).is_some() || s.latest_session_file(task_id).is_some()
            })
    }

    /// Shows the session of ticket `task_id`'s worker in place of the manager's transcript:
    /// live (it follows the worker) for a worker of this chat, else read-only from its newest
    /// session file. Esc goes back ([`Shell::close_worker`]).
    pub fn open_worker(&mut self, task_id: i64) -> Result<(), String> {
        let Some(sup) = self.supervisor.clone() else {
            return Err("this chat runs no workers".to_owned());
        };
        let feed = sup.feed(task_id);
        let fresh = match (self.sessions.get(&task_id), &feed) {
            (Some(v), Some(f)) => !v.feed_is(f),
            (Some(_), None) => false,
            (None, _) => true,
        };
        if fresh {
            let thinking = self.view.show_thinking();
            let session = match feed {
                Some(feed) => SessionView::new(task_id, Source::Chat, feed, thinking),
                None => {
                    let file = sup.latest_session_file(task_id).ok_or_else(|| {
                        format!("#{task_id} has no worker session in this chat or on disk")
                    })?;
                    let events = amp_core::worker_session::replay(&file)?;
                    let name = file
                        .file_name()
                        .map(|n| n.to_string_lossy().into_owned())
                        .unwrap_or_default();
                    SessionView::new(
                        task_id,
                        Source::File(name),
                        amp_core::worker_session::WorkerFeed::from_events(events),
                        thinking,
                    )
                }
            };
            self.sessions.insert(task_id, session);
        }
        if self.stop_confirm.is_some_and(|id| id != task_id) {
            self.stop_confirm = None;
        }
        if self.viewing.is_none() {
            self.scroll_back = Some(self.view.scroll().position());
        }
        self.viewing = Some(task_id);
        self.view.strip().blur();
        self.view.strip().set_viewing(Some(task_id));
        self.view.panel().close();
        self.refresh_session(task_id);
        if let Some(s) = self.sessions.get(&task_id) {
            self.view.show_session(Some(s.parts()));
        }
        Ok(())
    }

    /// Back to the manager's transcript and editor.
    pub fn close_worker(&mut self) {
        if self.viewing.take().is_some() {
            self.stop_confirm = None;
            self.view.show_session(None);
            self.view.strip().set_viewing(None);
            // #2347: the manager's transcript where the person left it.
            if let Some(pos) = self.scroll_back.take() {
                self.view.scroll().restore(pos);
            }
        }
    }

    /// The order Tab goes through: this chat's worker sessions (live first), plus the one shown
    /// when it was read from disk.
    fn session_order(&self) -> Vec<i64> {
        let mut ids: Vec<i64> = self
            .supervisor
            .as_ref()
            .map(|s| s.sessions().iter().map(|i| i.task_id).collect())
            .unwrap_or_default();
        if let Some(v) = self.viewing
            && !ids.contains(&v)
        {
            ids.push(v);
        }
        ids
    }

    /// Shows the next (`step` 1) or previous (-1) worker session; `None` when there is no other.
    pub fn switch_worker(&mut self, step: isize) -> Option<i64> {
        let ids = self.session_order();
        if ids.len() < 2 {
            return None;
        }
        let at = self
            .viewing
            .and_then(|v| ids.iter().position(|id| *id == v))
            .unwrap_or(0) as isize;
        let next = ids[(at + step).rem_euclid(ids.len() as isize) as usize];
        self.open_worker(next).ok()?;
        Some(next)
    }

    /// The keys of a shown session: Esc back, Tab/→ and Shift+Tab/← switch, x stop, and the
    /// transcript's scroll keys. Every other key is swallowed (the editor is hidden).
    pub fn session_key(&mut self, key: &KeyEvent) -> bool {
        if self.scroll_key(key, true) {
            return true;
        }
        if key
            .modifiers
            .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT)
        {
            return true;
        }
        match key.code {
            KeyCode::Esc => {
                self.close_worker();
                // #2354: like closing a panel, leaving a session never makes the next Esc stop
                // the reply (and every worker) unannounced.
                if self.running {
                    self.show_esc_hint();
                }
            }
            KeyCode::Tab | KeyCode::Right => {
                self.switch_worker(1);
            }
            KeyCode::BackTab | KeyCode::Left => {
                self.switch_worker(-1);
            }
            KeyCode::Char('x') | KeyCode::Char('X') => {
                if let Some(id) = self.viewing {
                    self.ask_stop(id);
                }
            }
            // #2347: the ticket in AMP's web UI.
            KeyCode::Char('o') | KeyCode::Char('O') => {
                if let Some(id) = self.viewing {
                    self.open_ticket(id);
                }
            }
            _ => {}
        }
        true
    }

    /// Opens ticket `id` in AMP's web UI (o in a session or the worker strip).
    fn open_ticket(&mut self, id: i64) {
        if self.meta.is_none() {
            let text = format!("#{id}: no AMP connection in this chat to open it in");
            if let Some(s) = self.shown_session() {
                s.keys().set(&text, true);
            } else {
                self.view.add_notice(&text);
            }
            return;
        }
        self.open_target(&Target::Ticket { id, question: None });
    }

    /// Ctrl+W (#2347): gives the worker strip the keys, or takes them back. False when there is
    /// no worker to choose (the key then does what it did before).
    pub fn toggle_workers_focus(&mut self) -> bool {
        let strip = self.view.strip().clone();
        if strip.is_focused() {
            strip.blur();
            return true;
        }
        if self.stop_confirm.is_some() || self.secret.is_some() {
            return false;
        }
        self.build_roster();
        strip.focus()
    }

    /// The worker strip's keys (#2347): ←/→ (and ↑/↓, Tab) choose, Enter watches, o opens the
    /// ticket in AMP, x stops it, Esc or Ctrl+W gives the keys back. Any other key gives them
    /// back too and goes on to the editor (returns false), unless a session is shown.
    pub fn strip_key(&mut self, key: &KeyEvent) -> bool {
        let strip = self.view.strip().clone();
        if key.modifiers == KeyModifiers::CONTROL && key.code == KeyCode::Char('w') {
            strip.blur();
            return true;
        }
        let plain = !key
            .modifiers
            .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT);
        match key.code {
            KeyCode::Left | KeyCode::Up | KeyCode::BackTab => strip.move_by(-1),
            KeyCode::Right | KeyCode::Down | KeyCode::Tab => strip.move_by(1),
            KeyCode::Esc => strip.blur(),
            KeyCode::Enter => {
                strip.blur();
                if let Some(id) = strip.selected()
                    && let Err(e) = self.open_worker(id)
                {
                    self.view.add_notice(&format!("#{id}: {e}"));
                }
            }
            KeyCode::Char('o') | KeyCode::Char('O') if plain => {
                if let Some(id) = strip.selected() {
                    self.open_ticket(id);
                }
            }
            KeyCode::Char('x') | KeyCode::Char('X') if plain => {
                strip.blur();
                if let Some(id) = strip.selected() {
                    self.ask_stop(id);
                }
            }
            _ => {
                strip.blur();
                return self.viewing.is_some();
            }
        }
        true
    }

    /// A click outside the editor (#2347): opens the session of the worker the clicked row
    /// names. Returns whether it did.
    fn click_worker(&mut self, width: usize, height: usize, row: usize, column: usize) -> bool {
        let lines = self.view.render_plain(width, height);
        let Some(text) = lines.get(row) else {
            return false;
        };
        let has = |id: i64| self.view.roster().get(id).is_some_and(|e| e.viewable);
        let Some(id) = crate::roster::ticket_at(text, column, has) else {
            return false;
        };
        if self.viewing == Some(id) || !self.has_session(id) {
            return false;
        }
        self.view.strip().blur();
        if let Err(e) = self.open_worker(id) {
            self.view.add_notice(&format!("#{id}: {e}"));
        }
        true
    }

    /// Rebuilds the roster (#2347): this chat's workers from the supervisor, sessions read from
    /// disk, and tickets in progress elsewhere, joined with the metadata poller's tickets. Tells
    /// the poller which tickets to follow, and adds a transcript notice when a worker of this
    /// chat starts or finishes. Returns whether anything shown changed (elapsed times tick once a
    /// second while workers are live).
    fn build_roster(&mut self) -> bool {
        let now = std::time::SystemTime::now();
        let snap = self.meta.as_ref().map(|m| m.snapshot());
        let sessions = self.worker_sessions();
        self.roster_live = live_phases(&sessions);
        let mut entries: std::collections::BTreeMap<i64, WorkerEntry> = Default::default();
        for i in &sessions {
            let model = i.model.clone().unwrap_or_default();
            let window = i
                .context_window
                .unwrap_or_else(|| self.context_window_of(&model));
            let phase = match i.phase {
                Some(_) if i.stopped_by.is_some() => Phase::Stopping,
                Some(amp_core::supervisor::WorkerPhase::Queued) => Phase::Queued,
                Some(amp_core::supervisor::WorkerPhase::Running) => Phase::Running,
                None => Phase::Finished,
            };
            entries.insert(
                i.task_id,
                WorkerEntry {
                    task_id: i.task_id,
                    agent_id: i.agent_id.clone(),
                    model,
                    phase,
                    state: session_state(i, now),
                    outcome: i.outcome.map(|o| o.as_str().to_owned()),
                    usage: i.totals.usage,
                    context_tokens: i.totals.context_tokens,
                    context_window: window,
                    branch: i.branch.clone(),
                    returned: i.returned.clone(),
                    viewable: true,
                    local: true,
                    ..WorkerEntry::default()
                },
            );
        }
        // AMP #2862: launches that ended with no session (crashed, or never dispatched) stay
        // identifiable as this chat's finished workers until the ticket runs again.
        for (id, end) in &self.ended_launches {
            entries
                .entry(*id)
                .or_insert_with(|| ended_launch_entry(*id, end));
        }
        // Sessions read from disk (an earlier chat's workers).
        for (id, s) in &self.sessions {
            if entries.contains_key(id) || s.source == Source::Chat {
                continue;
            }
            let model = s.model().to_owned();
            entries.insert(
                *id,
                WorkerEntry {
                    task_id: *id,
                    context_window: self.context_window_of(&model),
                    model,
                    phase: Phase::Finished,
                    state: match &s.source {
                        Source::File(name) => format!("from {name}"),
                        Source::Chat => "finished".to_owned(),
                    },
                    usage: s.totals().usage,
                    context_tokens: s.totals().context_tokens,
                    viewable: true,
                    ..WorkerEntry::default()
                },
            );
        }
        if let Some(m) = &snap {
            if let Some(b) = m.board.get() {
                for t in &b.in_progress_tickets {
                    entries.entry(t.id).or_insert_with(|| WorkerEntry {
                        task_id: t.id,
                        name: t.name.clone(),
                        epic: t.epic.clone(),
                        agent_id: t.assigned_to.clone().unwrap_or_default(),
                        phase: Phase::Elsewhere,
                        state: "in progress".to_owned(),
                        ..WorkerEntry::default()
                    });
                }
            }
            for e in entries.values_mut() {
                fill_from_meta(e, m, now);
            }
            // Follow live workers here first, then elsewhere, then finished ones.
            let mut ids: Vec<&WorkerEntry> = entries.values().collect();
            ids.sort_by_key(|e| (!e.is_live(), e.phase == Phase::Finished, e.task_id));
            let ids: Vec<i64> = ids.into_iter().map(|e| e.task_id).collect();
            if let Some(meta) = &self.meta {
                meta.set_workers(&ids);
            }
        }
        for e in entries.values_mut() {
            if e.name.is_empty() {
                e.name = self.ticket_name(e.task_id);
            }
        }
        // #2370: the chips in the manager feed, when they changed.
        if self.view.feed().is_some() {
            // #2388: with what the chip shows besides: epic › story, the ticket's state, the
            // model's window (tokens and cost come from the supervisor).
            let chips = amp_core::chat_event::ChatEvent::worker_status(&sessions, |id| {
                let e = entries.get(&id);
                let ticket = snap
                    .as_ref()
                    .and_then(|m| m.workers.get(&id))
                    .and_then(|f| f.get())
                    .map(|w| &w.ticket);
                amp_core::chat_event::ChipMeta {
                    name: e.map(|e| e.name.clone()),
                    epic: e.map(|e| e.epic.clone()),
                    story: e.map(|e| e.story.clone()),
                    ticket_state: ticket.map(|t| t.status.word().to_owned()),
                    context_window: e
                        .map(|e| e.context_window)
                        .and_then(|w| i64::try_from(w).ok()),
                }
            });
            if self.fed_workers.as_ref() != Some(&chips) {
                let empty_first = self.fed_workers.is_none()
                    && matches!(&chips, amp_core::chat_event::ChatEvent::WorkerStatus { workers } if workers.is_empty());
                if !empty_first {
                    self.view.feed_push(chips.clone());
                }
                self.fed_workers = Some(chips);
            }
        }
        // Started and finished notices in the manager's transcript.
        for i in &sessions {
            let live = i.is_live();
            match self.noticed.get(&i.task_id) {
                Some(was) if *was == live => {}
                Some(false) | None if live => {
                    self.view.add_worker_notice(i.task_id, NoticeKind::Started);
                }
                _ => self.view.add_worker_notice(i.task_id, NoticeKind::Finished),
            }
            self.noticed.insert(i.task_id, live);
        }
        // A live worker that left the supervisor with no session (its task crashed, AMP #2885)
        // is not live any more; its launch end's notice already said how it ended (#2862).
        for (id, live) in self.noticed.iter_mut() {
            if *live && !sessions.iter().any(|i| i.task_id == *id) {
                *live = false;
            }
        }
        // A worker that just started running was just dispatched: AMP's ready count changed.
        let mut dispatched = false;
        for i in &sessions {
            if i.phase == Some(amp_core::supervisor::WorkerPhase::Running) {
                dispatched |= self.running_seen.insert(i.task_id);
            } else if !i.is_live() {
                self.running_seen.remove(&i.task_id);
            }
        }
        if dispatched && let Some(meta) = &self.meta {
            meta.refresh(Topics::BOARD);
        }
        if self.view.strip().is_focused() && self.view.strip().shown().is_empty() {
            self.view.strip().blur();
        }
        self.view.roster().set(entries)
    }

    /// This chat's worker sessions, live and finished, from the supervisor.
    fn worker_sessions(&self) -> Vec<amp_core::supervisor::WorkerSessionInfo> {
        #[cfg(test)]
        if let Some(sessions) = &self.test_sessions {
            return sessions.clone();
        }
        self.supervisor
            .as_ref()
            .map(|s| s.sessions())
            .unwrap_or_default()
    }

    /// Whether the supervisor's live workers (tickets and phases) differ from what the roster
    /// last showed (AMP #2862).
    fn live_changed(&self) -> bool {
        #[cfg(test)]
        let has_source = self.supervisor.is_some() || self.test_sessions.is_some();
        #[cfg(not(test))]
        let has_source = self.supervisor.is_some();
        if !has_source {
            return false;
        }
        live_phases(&self.worker_sessions()) != self.roster_live
    }

    /// The context window of `model` (`provider/model`) from the chat's model list; 0 unknown.
    fn context_window_of(&self, model: &str) -> u64 {
        self.models
            .iter()
            .find(|m| m.reference == model)
            .map(|m| m.model.context_window)
            .unwrap_or(0)
    }

    /// `/worker [ticket]`: opens that ticket's worker session, or with no ticket the first live
    /// worker of this chat (else the last one that finished).
    fn worker_command(&mut self, arg: &str) {
        let id = if arg.is_empty() {
            self.supervisor.as_ref().and_then(|s| {
                let all = s.sessions();
                all.iter()
                    .find(|i| i.is_live())
                    .or(all.last())
                    .map(|i| i.task_id)
            })
        } else {
            parse_id(arg)
        };
        match id {
            None if arg.is_empty() => self.view.add_notice(
                "/worker: no worker has run in this chat yet (/worker <ticket> reads an earlier \
                 session from disk)",
            ),
            None => self
                .view
                .add_notice(&format!("/worker {arg}: not a ticket id")),
            Some(id) => {
                if let Err(e) = self.open_worker(id) {
                    self.view.add_notice(&format!("/worker: {e}"));
                }
            }
        }
    }

    /// Asks before stopping the live worker on `task_id` (x in its session or in /workers).
    /// Returns whether the question is up.
    pub fn ask_stop(&mut self, task_id: i64) -> bool {
        let why = match self.supervisor.as_ref().and_then(|s| s.session(task_id)) {
            Some(i) if i.is_live() && i.stopped_by.is_none() => None,
            Some(i) if i.is_live() => Some(format!("the worker on #{task_id} is already stopping")),
            Some(_) => Some(format!(
                "the worker on #{task_id} has finished: nothing to stop"
            )),
            None => Some(format!("#{task_id} has no worker running in this chat")),
        };
        if let Some(why) = why {
            self.say_about_stop(&why, false);
            return false;
        }
        self.stop_confirm = Some(task_id);
        self.say_about_stop(&stop_question(task_id), true);
        true
    }

    /// Where the stop's question and answer show: the session's keys line, else the /workers
    /// panel's note, else the transcript.
    fn say_about_stop(&mut self, text: &str, question: bool) {
        if let Some(s) = self.shown_session() {
            s.keys().set(text, true);
        } else if self.view.panel().is_open() {
            self.view
                .panel()
                .update(|s| s.note = Some((text.to_owned(), question)));
        } else {
            self.view.add_notice(text);
        }
    }

    /// The `y` to [`Shell::ask_stop`]: stops the worker as the person (run reporting records the
    /// run abandoned, `stopped by <person> from the chat`, and returns the ticket to ready; the
    /// manager's `amp_run_ticket` returns saying so).
    pub fn confirm_stop(&mut self) -> Result<i64, String> {
        let id = self
            .stop_confirm
            .take()
            .ok_or_else(|| "no stop is waiting".to_owned())?;
        let by = self.person();
        let result = match &self.supervisor {
            Some(sup) => sup.stop_worker(id, &by).map_err(|e| e.to_string()),
            None => Err("this chat runs no workers".to_owned()),
        };
        match &result {
            Ok(()) => {
                let line = format!(
                    "stopping the worker on #{id} ({}): its run is reported abandoned, the \
                     ticket goes back to ready, and the manager is told",
                    amp_core::supervisor::stopped_from_chat(&by)
                );
                self.view.add_warning(&line);
                if let Some(s) = self.sessions.get(&id) {
                    s.view().add_warning(&line);
                }
                if self.viewing.is_none() && self.view.panel().is_open() {
                    self.view
                        .panel()
                        .update(|s| s.note = Some((format!("stopping #{id}"), false)));
                }
            }
            Err(e) => self.say_about_stop(e, false),
        }
        if self.viewing == Some(id) {
            self.refresh_session(id);
        }
        result.map(|()| id)
    }

    /// Any other key than `y` to [`Shell::ask_stop`]: the worker keeps going.
    fn cancel_stop(&mut self) {
        if let Some(id) = self.stop_confirm.take() {
            if self.viewing == Some(id) {
                self.refresh_session(id);
            } else if self.view.panel().is_open() {
                self.view
                    .panel()
                    .update(|s| s.note = Some((format!("#{id} keeps running"), false)));
            }
        }
    }

    /// The ticket's name, when the board knows it.
    fn ticket_name(&self, task_id: i64) -> String {
        let Some(meta) = &self.meta else {
            return String::new();
        };
        let snap = meta.snapshot();
        if let Some(t) = snap.ticket.get()
            && t.id == task_id
        {
            return t.name.clone();
        }
        snap.board
            .get()
            .and_then(|b| {
                b.in_progress_tickets
                    .iter()
                    .find(|t| t.id == task_id)
                    .map(|t| t.name.clone())
                    .or_else(|| {
                        b.questions
                            .iter()
                            .find(|q| q.id == task_id)
                            .map(|q| q.name.clone())
                    })
            })
            .unwrap_or_default()
    }

    /// Brings one session up to date: new events, the bar, the keys line. Returns (new events,
    /// still live).
    fn refresh_session(&mut self, task_id: i64) -> (bool, bool) {
        let info = self.supervisor.as_ref().and_then(|s| s.session(task_id));
        let name = self.ticket_name(task_id);
        let confirm = self.stop_confirm == Some(task_id);
        let Some(s) = self.sessions.get_mut(&task_id) else {
            return (false, false);
        };
        let info = if s.source == Source::Chat { info } else { None };
        let got = s.pump();
        let entry = self.view.roster().get(task_id).unwrap_or(WorkerEntry {
            task_id,
            name,
            ..WorkerEntry::default()
        });
        let live = s.update(info.as_ref(), entry);
        if confirm {
            s.keys().set(&stop_question(task_id), true);
        } else {
            s.reset_keys();
        }
        (got, live)
    }

    /// Follows every opened session; true when the shown one changed (or is live: its elapsed
    /// time moves).
    fn pump_sessions(&mut self) -> bool {
        let ids: Vec<i64> = self.sessions.keys().copied().collect();
        let mut changed = false;
        for id in ids {
            let (got, live) = self.refresh_session(id);
            if self.viewing == Some(id) && (got || live) {
                changed = true;
                if got {
                    self.view.scroll().touch();
                }
            }
        }
        changed
    }
}

/// Adds what the metadata poller knows about `e`'s ticket (#2347): name, story › epic,
/// assignee, what it waits on, and for a worker elsewhere the last run AMP recorded.
fn fill_from_meta(e: &mut WorkerEntry, m: &amp_core::meta::AmpMeta, now: std::time::SystemTime) {
    if let Some(b) = m.board.get()
        && e.name.is_empty()
        && let Some(t) = b.in_progress_tickets.iter().find(|t| t.id == e.task_id)
    {
        e.name = t.name.clone();
        e.epic = t.epic.clone();
    }
    let Some(w) = m.workers.get(&e.task_id).and_then(|f| f.get()) else {
        return;
    };
    let t = &w.ticket;
    if !t.name.is_empty() {
        e.name = t.name.clone();
    }
    if let Some(epic) = &t.epic {
        e.epic = epic.name.clone();
    }
    if let Some(story) = &t.story {
        e.story = story.name.clone();
    }
    if e.agent_id.is_empty() {
        e.agent_id = t.assigned_to.clone().unwrap_or_default();
    }
    e.waiting_on = w.waiting_on.clone();
    if e.phase == Phase::Elsewhere {
        if let Some(since) = t.dispatched_at.as_deref().and_then(overlay::parse_rfc3339) {
            let d = now.duration_since(since).unwrap_or_default();
            e.state = format!("in progress {}", crate::roster::duration(d));
        }
        if let Some(run) = &w.last_run {
            if run.has_usage() {
                e.usage = amp_core::events::UsageJson {
                    input_tokens: run.input_tokens,
                    output_tokens: run.output_tokens,
                    cache_read_tokens: run.cache_read_tokens,
                    cache_write_tokens: run.cache_write_tokens,
                    cost_usd: run.cost_usd.unwrap_or(0.0),
                };
                e.usage_recorded = run.recorded_at;
            }
            if e.model.is_empty() {
                e.model = run.model.clone();
            }
        }
    }
    if e.branch.is_none()
        && let Some(run) = &w.last_run
        && !run.branch.is_empty()
    {
        e.branch = Some(run.branch.clone());
    }
}

/// Whether `/model`'s argument is this chat's model (`provider/model`), not a role (#2245):
/// the first word holds a `/`.
fn is_model_ref(arg: &str) -> bool {
    arg.split_whitespace()
        .next()
        .is_some_and(|w| w.contains('/'))
}

/// `2108` or `#2108`.
fn parse_id(arg: &str) -> Option<i64> {
    arg.trim()
        .trim_start_matches('#')
        .parse()
        .ok()
        .filter(|id: &i64| *id > 0)
}

/// The answer as it will be sent: each line cleaned of control characters (so what the panel
/// shows is exactly what is sent), blank lines at the ends dropped.
fn clean_answer(text: &str) -> String {
    text.lines()
        .map(crate::chrome::clean)
        .collect::<Vec<_>>()
        .join("\n")
        .trim()
        .to_owned()
}

/// `amp-manager` -> `manager`, `amp:amp-worker-backend` -> `worker-backend`.
/// #2388: what the remote link's status carries besides the basics: the rest of the TUI's
/// header (ticket name, state, epic › story, the AMP connection), footer (thinking, role, cwd,
/// session tokens) and status line (board counts, plan approval).
fn remote_status_more(
    meta: Option<&amp_core::meta::AmpMeta>,
    info: &ShellInfo,
    totals: &Totals,
) -> amp_core::remote::HostStatus {
    use amp_core::meta::Connection;
    use amp_core::remote::{AmpConnection, BoardCounts, HostStatus, SessionTokens};
    let ticket = meta.and_then(|m| {
        let id = m.ticket_id?;
        m.ticket.get().filter(|t| t.id == id)
    });
    let u = &totals.usage;
    let used = u.input_tokens + u.output_tokens + u.cache_read_tokens + u.cache_write_tokens;
    HostStatus {
        ticket_name: ticket
            .map(|t| t.name.clone())
            .filter(|n| !n.trim().is_empty()),
        ticket_state: ticket.map(|t| t.status.word().to_owned()),
        epic: ticket
            .and_then(|t| t.epic.as_ref())
            .map(|e| e.name.clone())
            .filter(|n| !n.trim().is_empty()),
        story: ticket
            .and_then(|t| t.story.as_ref())
            .map(|s| s.name.clone())
            .filter(|n| !n.trim().is_empty()),
        usage: (used > 0).then_some(SessionTokens {
            input: u.input_tokens,
            output: u.output_tokens,
            cache_read: u.cache_read_tokens,
            cache_write: u.cache_write_tokens,
        }),
        thinking: Some(thinking_name(info.thinking).to_owned()),
        role: Some(short_role(&info.role).to_owned()).filter(|r| !r.is_empty()),
        cwd: Some(info.cwd.clone()).filter(|c| !c.is_empty()),
        board: meta.and_then(|m| m.board.get()).map(|b| BoardCounts {
            in_progress: i64::from(b.count(Status::InProgress)),
            ready: i64::from(b.count(Status::Ready)),
            question: i64::from(b.count(Status::Question)),
            scheduled: i64::from(b.count(Status::Scheduled)),
        }),
        plan_approval: meta
            .filter(|m| m.plan_required())
            .and_then(|m| m.plan.get())
            .map(|p| (p.yours().len() as i64, p.others().len() as i64)),
        amp_connection: meta.map(|m| {
            let (state, latency_ms) = match &m.health.connection {
                Connection::Connecting => ("connecting", None),
                Connection::Ok => ("connected", None),
                Connection::Slow { latency } => ("slow", i64::try_from(latency.as_millis()).ok()),
                Connection::Degraded { .. } => ("degraded", None),
                Connection::AuthRequired => ("login_needed", None),
                Connection::Down { .. } => ("disconnected", None),
            };
            AmpConnection {
                state: state.to_owned(),
                latency_ms,
            }
        }),
        ..HostStatus::default()
    }
}

pub fn short_role(role: &str) -> &str {
    let role = role.rsplit(':').next().unwrap_or(role);
    role.strip_prefix("amp-").unwrap_or(role)
}

/// `1 prompt`, `3 prompts`.
fn count(n: u64, one: &str, many: &str) -> String {
    format!("{n} {}", if n == 1 { one } else { many })
}

#[cfg(test)]
mod tests {
    use super::{clean_answer, parse_id, short_role, worker_summaries};
    use crate::roster::{Phase, WorkerEntry};
    use std::sync::Arc;

    /// #2857: a turn opens with the ends the manager has not seen, once. The block is `None` with
    /// nothing queued (the prompt goes as typed), and a second look at the same drained ends has
    /// nothing left to say.
    #[test]
    fn worker_summaries_are_one_block_that_a_turn_uses_once() {
        use amp_core::supervisor::{LaunchEnd, WorkerOutcome, WorkerReport};

        assert_eq!(
            worker_summaries(&[]),
            None,
            "nothing ended: the prompt as typed"
        );

        let report = |outcome, error: Option<&str>| WorkerReport {
            task_id: 2857,
            agent_id: "amp:amp-worker-backend".into(),
            role: "amp-worker-backend".into(),
            outcome,
            dispatch_id: String::new(),
            started: true,
            returned: None,
            final_message: String::new(),
            error: error.map(str::to_owned),
            model: "wf/wf-1".into(),
            cwd: std::path::PathBuf::from("/r"),
            worktree: None,
            branch: None,
            workspace: amp_core::workspace::Placement::Shared,
            session_file: None,
            turns: 0,
            tool_calls: 0,
            tool_errors: 0,
            usage: amp_core::events::UsageJson::default(),
            duration_ms: 0,
            notes: Vec::new(),
            totals: amp_core::events::Totals::default(),
        };
        let mut failed = report(WorkerOutcome::Error, Some("the model failed"));
        failed.returned = Some(Err("503".into()));
        failed.notes = vec![
            "#2855: worker amp:amp-worker-frontend failed: the model failed, and it could not be \
             returned: 503. It may still be in progress; check the ticket."
                .into(),
        ];
        let mut asked = report(WorkerOutcome::Blocked, None);
        asked.final_message = format!("Per-user cache or shared?\n{}", "x".repeat(900));
        let ends = vec![
            LaunchEnd {
                task_id: 2854,
                agent_id: "amp:amp-worker-backend".into(),
                result: Ok(report(WorkerOutcome::Completed, None)),
            },
            LaunchEnd {
                task_id: 2855,
                agent_id: "amp:amp-worker-frontend".into(),
                result: Ok(failed),
            },
            LaunchEnd {
                task_id: 2856,
                agent_id: "amp:amp-worker-backend".into(),
                result: Ok(asked),
            },
        ];

        let block = worker_summaries(&ends).expect("two ends, so a block");
        assert!(
            block.contains("#2854 completed by amp:amp-worker-backend"),
            "{block}"
        );
        assert!(
            block.contains("#2855 error by amp:amp-worker-frontend: the model failed"),
            "{block}"
        );
        // #2883: the failed hand-back reaches the model, and the header no longer claims every
        // ticket is already updated.
        assert!(
            block.contains(
                "    note: #2855: worker amp:amp-worker-frontend failed: the model \
                 failed, and it could not be returned: 503"
            ),
            "{block}"
        );
        assert!(!block.contains("already updated in AMP"), "{block}");
        // A blocked worker's question arrives, trimmed.
        assert!(
            block.contains(
                "#2856 blocked by amp:amp-worker-backend\n    final message: \
                 Per-user cache or shared?"
            ),
            "{block}"
        );
        assert!(block.contains("[trimmed;"), "{block}");
        assert!(!block.contains(&"x".repeat(700)), "{block}");
        // One item per end, under one header: the model reads where each run got to.
        assert_eq!(
            block.lines().filter(|l| l.starts_with("- ")).count(),
            3,
            "{block}"
        );
    }

    #[test]
    fn ids_and_answers() {
        assert_eq!(parse_id("2108"), Some(2108));
        assert_eq!(parse_id("#2108"), Some(2108));
        assert_eq!(parse_id("0"), None);
        assert_eq!(parse_id("x"), None);
        assert_eq!(
            clean_answer("  dark\x1b[31m  please \n\nok\n"),
            "dark[31m please\n\nok"
        );
    }

    /// #2388: the remote status carries the TUI's header, footer and status line facts.
    #[test]
    fn remote_status_has_what_the_chrome_shows() {
        use amp_core::remote::{AmpConnection, BoardCounts, SessionTokens};
        let meta = crate::chrome::sample::meta(true);
        let info = super::ShellInfo {
            role: "amp-manager".into(),
            provider: "anthropic".into(),
            model_id: "claude".into(),
            thinking: rpi_ai::ThinkingLevel::High,
            requested_thinking: None,
            context_window: 200_000,
            cwd: "/Users/ada/dev/amp".into(),
            git_branch: None,
            banner: vec![],
            session_file: None,
        };
        let mut totals = amp_core::events::Totals::default();
        totals.usage.input_tokens = 1200;
        totals.usage.cache_read_tokens = 9000;
        let s = super::remote_status_more(Some(&meta), &info, &totals);
        assert_eq!(
            s.ticket_name.as_deref(),
            Some("M3-3 Header and AMP status line")
        );
        assert_eq!(s.ticket_state.as_deref(), Some("in_progress"));
        assert_eq!(s.epic.as_deref(), Some("AMP Harness"));
        assert_eq!(s.story.as_deref(), Some("M3 theme + metadata"));
        assert_eq!(s.thinking.as_deref(), Some("high"));
        assert_eq!(s.role.as_deref(), Some("manager"));
        assert_eq!(
            s.usage,
            Some(SessionTokens {
                input: 1200,
                output: 0,
                cache_read: 9000,
                cache_write: 0
            })
        );
        assert_eq!(
            s.board,
            Some(BoardCounts {
                in_progress: 3,
                ready: 5,
                question: 2,
                scheduled: 1
            })
        );
        assert_eq!(s.plan_approval, Some((2, 0)));
        assert_eq!(
            s.amp_connection,
            Some(AmpConnection {
                state: "connected".into(),
                latency_ms: None
            })
        );
        // The link sends only the folder's name.
        assert_eq!(
            amp_core::remote::cwd_basename(s.cwd.as_deref().unwrap()).as_deref(),
            Some("amp")
        );
        // Without the poller: no AMP facts, and no usage before the first reply.
        let s = super::remote_status_more(None, &info, &Default::default());
        assert_eq!(
            (s.ticket_name, s.board, s.amp_connection, s.usage),
            (None, None, None, None)
        );
    }

    #[test]
    fn short_role_drops_the_prefixes() {
        assert_eq!(short_role("amp-manager"), "manager");
        assert_eq!(short_role("amp:amp-worker-backend"), "worker-backend");
        assert_eq!(short_role("reviewer"), "reviewer");
    }

    fn stop(live: &[i64], cancelled: &[i64], queued: &[i64]) -> super::ReplyStop {
        super::ReplyStop {
            live: live.to_vec(),
            cancelled: cancelled.to_vec(),
            queued: queued.to_vec(),
        }
    }

    /// AMP #2858: in Await mode stopping the reply stops its workers, and says so as before.
    #[test]
    fn await_mode_keeps_the_old_stop_notice() {
        use super::{DispatchMode::Await, reply_stop_notice, worker_stop_notice};
        assert_eq!(reply_stop_notice(Await, &stop(&[], &[], &[])), None);
        assert_eq!(
            reply_stop_notice(Await, &stop(&[7], &[], &[])).as_deref(),
            Some("stopping the worker on #7 too; its ticket goes back to ready")
        );
        assert_eq!(
            reply_stop_notice(Await, &stop(&[7, 8], &[], &[])),
            Some(worker_stop_notice(&[7, 8]))
        );
    }

    /// AMP #2858: in Launch mode stopping the reply leaves dispatched workers running, and the
    /// notice never says they stop or that their tickets go back to ready.
    #[test]
    fn launch_mode_says_the_workers_keep_running() {
        use super::{DispatchMode::Launch, reply_stop_notice};
        assert_eq!(reply_stop_notice(Launch, &stop(&[], &[], &[])), None);
        let one = reply_stop_notice(Launch, &stop(&[7], &[], &[])).unwrap();
        assert_eq!(
            one,
            "stopping the reply; the worker on #7 keeps running (/workers to stop it)"
        );
        let two = reply_stop_notice(Launch, &stop(&[7, 8], &[], &[])).unwrap();
        assert_eq!(
            two,
            "stopping the reply; 2 workers keep running on #7, #8 (/workers to stop one)"
        );
        for note in [&one, &two] {
            assert!(!note.contains("too"), "{note}");
            assert!(!note.contains("back to ready"), "{note}");
        }
    }

    /// AMP #2858: a launch from the stopped reply that still waits for a slot is cancelled and
    /// never dispatched; one queued by an earlier reply stays queued; both are named apart from
    /// the workers that keep running.
    #[test]
    fn launch_mode_names_cancelled_and_still_queued_launches() {
        use super::{DispatchMode::Launch, reply_stop_notice};
        assert_eq!(
            reply_stop_notice(Launch, &stop(&[9], &[9], &[])).as_deref(),
            Some(
                "stopping the reply; #9 was still waiting for a slot, so its launch is \
                 cancelled and it is not dispatched"
            )
        );
        assert_eq!(
            reply_stop_notice(Launch, &stop(&[7, 9, 10], &[9, 10], &[])).as_deref(),
            Some(
                "stopping the reply; #9, #10 were still waiting for a slot, so their launches \
                 are cancelled and they are not dispatched; the worker on #7 keeps running \
                 (/workers to stop it)"
            )
        );
        assert_eq!(
            reply_stop_notice(Launch, &stop(&[11], &[], &[11])).as_deref(),
            Some(
                "stopping the reply; the worker on #11 stays queued for a slot (/workers to stop it)"
            )
        );
        assert_eq!(
            reply_stop_notice(Launch, &stop(&[7, 8, 9, 11], &[9], &[11])).as_deref(),
            Some(
                "stopping the reply; #9 was still waiting for a slot, so its launch is \
                 cancelled and it is not dispatched; 3 workers keep running on #7, #8, #11 \
                 (/workers to stop one); #11 is still queued for a slot"
            )
        );
    }

    /// AMP #2858: with no supervisor (a worker role's chat) nothing is live, the mode is Await,
    /// and stopping the reply has nothing to say about workers.
    #[tokio::test]
    async fn no_supervisor_means_await_and_no_worker_note() {
        let ends = amp_core::supervisor::LaunchEnds::default();
        let (shell, _feed) = watching_shell(&ends).await;
        assert_eq!(shell.dispatch_mode(), super::DispatchMode::Await);
        assert_eq!(shell.reply_stop(), super::ReplyStop::default());
    }

    /// A worker report for the #2861 tests: `task_id` ended with `outcome`.
    fn ended(
        task_id: i64,
        outcome: amp_core::supervisor::WorkerOutcome,
        error: Option<&str>,
    ) -> amp_core::supervisor::LaunchEnd {
        use amp_core::supervisor::{LaunchEnd, WorkerReport};
        LaunchEnd {
            task_id,
            agent_id: "amp:amp-worker-backend".into(),
            result: Ok(WorkerReport {
                task_id,
                agent_id: "amp:amp-worker-backend".into(),
                role: "amp-worker-backend".into(),
                outcome,
                dispatch_id: String::new(),
                started: true,
                returned: None,
                final_message: String::new(),
                error: error.map(str::to_owned),
                model: "wf/wf-1".into(),
                cwd: std::path::PathBuf::from("/r"),
                worktree: None,
                branch: None,
                workspace: amp_core::workspace::Placement::Shared,
                session_file: None,
                turns: 0,
                tool_calls: 0,
                tool_errors: 0,
                usage: amp_core::events::UsageJson::default(),
                duration_ms: 0,
                notes: Vec::new(),
                totals: amp_core::events::Totals::default(),
            }),
        }
    }

    /// A manager shell on a scripted model with a manager feed, watching `ends` the way
    /// [`super::Shell::set_supervisor`] watches the supervisor's queue (#2861).
    /// AMP #2889: a prompt sent during a reply is queued, not refused: the transcript says where
    /// it is and that it runs after this reply, the spinner line counts the queue, a sixth is
    /// refused as on a paired device, and a stopped reply drops them all and says so.
    #[tokio::test]
    async fn prompts_sent_during_a_reply_queue_up_to_five_and_a_stop_drops_them() {
        let ends = amp_core::supervisor::LaunchEnds::default();
        let (mut shell, _feed) = watching_shell(&ends).await;
        shell.running = true;
        shell.view.set_working(true);
        for i in 1..=6 {
            shell.submit(&format!("follow-up {i}")).await;
        }
        assert_eq!(
            shell.queued_prompts(),
            (1..=5)
                .map(|i| format!("follow-up {i}"))
                .collect::<Vec<_>>()
        );
        let screen = shell.view().render_plain(160, 60).join("\n");
        assert!(
            screen.contains(
                "queued #1 · runs after this reply: \"follow-up 1\" (Esc stops the reply and \
                 drops the queue)"
            ),
            "{screen}"
        );
        assert!(
            screen.contains("queued #5 · runs after this reply"),
            "{screen}"
        );
        assert!(
            screen.contains("5 prompts are already queued: send it once one has run"),
            "{screen}"
        );
        assert!(!screen.contains("follow-up 6\""), "{screen}");
        assert!(!screen.contains("still working"), "{screen}");
        assert_eq!(shell.working_text(), "Working… · 5 queued");
        shell.running = false;
        shell.after_run(true);
        assert!(shell.queued_prompts().is_empty());
        let screen = shell.view().render_plain(160, 60).join("\n");
        assert!(
            screen.contains("dropped 5 queued prompts (↑ recalls them)"),
            "{screen}"
        );
        assert_eq!(shell.working_text(), "Working…");
        assert_eq!(
            super::dropped_notice(1),
            "dropped the queued prompt (↑ recalls it)"
        );
    }

    /// AMP #2889: a long or multi-line prompt shows as one line of at most 80 characters.
    #[test]
    fn the_queued_line_is_one_short_line() {
        let long = format!("line one\n{}", "x".repeat(200));
        let line = super::queued_notice(2, &long);
        assert!(line.starts_with("queued #2 · runs after this reply: \"line one xxx"));
        assert!(line.contains("…\" (Esc stops the reply"));
        assert!(!line.contains('\n'));
    }

    async fn watching_shell(
        ends: &amp_core::supervisor::LaunchEnds,
    ) -> (super::Shell, Arc<amp_core::manager_feed::ManagerFeed>) {
        use rpi_ai::providers::faux::{FauxProvider, FauxScript};
        use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
        let provider = FauxProvider::new(FauxScript::new());
        let model = provider.default_model().clone();
        let harness = AgentHarness::create(AgentHarnessOptions {
            model: model.clone(),
            session: amp_core::agent::memory_session(),
            models: vec![provider],
            ..Default::default()
        })
        .await
        .expect("harness");
        let info = super::ShellInfo {
            role: "amp-manager".into(),
            provider: model.provider.to_string(),
            model_id: model.id.clone(),
            thinking: rpi_ai::ThinkingLevel::Off,
            requested_thinking: None,
            context_window: 200_000,
            cwd: "/work/amp".into(),
            git_branch: None,
            session_file: None,
            banner: vec!["AMP harness · manager".into()],
        };
        let (mut shell, _done) = super::Shell::new(Arc::new(harness), info, vec![]);
        let feed = amp_core::manager_feed::ManagerFeed::new();
        shell.set_feed(Some(Arc::clone(&feed)));
        shell.launch_watch = Some(ends.watch());
        (shell, feed)
    }

    fn feed_notices(
        feed: &amp_core::manager_feed::ManagerFeed,
    ) -> Vec<(amp_core::chat_event::NoticeLevel, String)> {
        use amp_core::chat_event::ChatEvent;
        feed.since(0)
            .expect("feed")
            .into_iter()
            .filter_map(|e| match e.event {
                ChatEvent::Notice { level, text, .. } => Some((level, text)),
                _ => None,
            })
            .collect()
    }

    /// #2861: a launched worker's end shows on the tick — with the manager idle and mid-reply —
    /// as a transcript line and a feed notice, success as news and failure as an error. Nothing
    /// here is a dispatch tool result: the shell only polls.
    #[tokio::test]
    async fn launched_ends_show_on_the_tick_idle_or_mid_reply() {
        use amp_core::chat_event::NoticeLevel;
        use amp_core::supervisor::{LaunchEnd, LaunchEnds, WorkerOutcome};

        let ends = LaunchEnds::default();
        let (mut shell, feed) = watching_shell(&ends).await;
        assert!(!shell.show_launch_ends(), "nothing ended yet");

        // Idle.
        ends.push(ended(2854, WorkerOutcome::Completed, None));
        assert!(!shell.is_running());
        assert!(shell.poll(), "the tick shows the end");
        let screen = shell.view().render_plain(120, 40).join("\n");
        assert!(
            screen.contains("worker ended: #2854 completed by amp:amp-worker-backend"),
            "{screen}"
        );

        // Mid-reply: a run is going, and its tool calls have nothing to do with these workers.
        shell.running = true;
        ends.push(ended(2855, WorkerOutcome::Error, Some("the model failed")));
        ends.push(LaunchEnd {
            task_id: 2856,
            agent_id: "amp:amp-worker-frontend".into(),
            result: Err("AMP refused the dispatch".into()),
        });
        assert!(shell.show_launch_ends());
        let screen = shell.view().render_plain(120, 40).join("\n");
        assert!(
            screen
                .contains("worker ended: #2855 error by amp:amp-worker-backend: the model failed"),
            "{screen}"
        );
        assert!(
            screen.contains("#2856 was not dispatched to amp:amp-worker-frontend"),
            "{screen}"
        );
        shell.running = false;

        let notices = feed_notices(&feed);
        let level_of = |needle: &str| {
            notices
                .iter()
                .find(|(_, t)| t.contains(needle))
                .map(|(l, _)| *l)
                .unwrap_or_else(|| panic!("no notice for {needle}: {notices:?}"))
        };
        assert_eq!(level_of("#2854 completed"), NoticeLevel::Info);
        assert_eq!(level_of("#2855 error"), NoticeLevel::Error);
        assert_eq!(level_of("#2856 was not dispatched"), NoticeLevel::Error);
        assert!(!shell.show_launch_ends(), "each end is shown once");
    }

    /// #2861: showing an end leaves it in the queue, so the next manager turn still gets every
    /// end exactly once (#2857's drain), and the notices never repeat what the turn delivers.
    #[tokio::test]
    async fn showing_ends_leaves_the_next_turns_delivery_whole_and_once() {
        use amp_core::supervisor::{LaunchEnds, WorkerOutcome};

        let ends = LaunchEnds::default();
        let (mut shell, _feed) = watching_shell(&ends).await;
        ends.push(ended(2854, WorkerOutcome::Completed, None));
        ends.push(ended(2855, WorkerOutcome::Error, Some("the model failed")));
        assert!(shell.show_launch_ends());
        assert_eq!(ends.len(), 2, "display does not consume the queue");

        // What `Shell::take_worker_summaries` does at the next turn's start.
        let block = worker_summaries(&ends.drain()).expect("both ends reach the turn");
        assert_eq!(
            block.lines().filter(|l| l.starts_with("- ")).count(),
            2,
            "{block}"
        );
        assert!(block.contains("#2854 completed"), "{block}");
        assert!(block.contains("#2855 error"), "{block}");
        assert_eq!(worker_summaries(&ends.drain()), None, "and only once");

        // Draining sends nothing new to the watcher: the transcript does not show them again.
        assert!(!shell.show_launch_ends());
        let screen = shell.view().render_plain(120, 40).join("\n");
        assert_eq!(screen.matches("worker ended: #2854").count(), 1, "{screen}");
    }

    /// A worker session for the #2862 roster tests: live in `phase`, or finished with `outcome`.
    fn session(
        task_id: i64,
        phase: Option<amp_core::supervisor::WorkerPhase>,
        outcome: Option<amp_core::supervisor::WorkerOutcome>,
        returned: Option<Result<(), String>>,
    ) -> amp_core::supervisor::WorkerSessionInfo {
        let now = std::time::SystemTime::now();
        amp_core::supervisor::WorkerSessionInfo {
            task_id,
            agent_id: "amp:amp-worker-backend".into(),
            phase,
            model: Some("wf/wf-1".into()),
            cwd: None,
            since: now,
            ended: phase.is_none().then_some(now),
            totals: amp_core::events::Totals::default(),
            outcome,
            error: None,
            stopped_by: None,
            returned,
            session_file: None,
            branch: None,
            context_window: None,
        }
    }

    /// A tool call ending in the manager's run.
    fn tool_end(tool_name: &str) -> rpi_agent::AgentEvent {
        rpi_agent::AgentEvent::ToolExecutionEnd {
            tool_call_id: format!("call-{tool_name}"),
            tool_name: tool_name.into(),
            result: rpi_agent::AgentToolResult::text("ok"),
            is_error: false,
        }
    }

    fn roster_entry(shell: &super::Shell, task_id: i64) -> Option<WorkerEntry> {
        shell
            .view()
            .roster()
            .all()
            .into_iter()
            .find(|e| e.task_id == task_id)
    }

    /// AMP #2862: in Launch mode `amp_run_ticket` returns while its worker is queued, so the
    /// tool call's end says nothing about the worker. The roster follows the supervisor: the
    /// launch shows at once, the running worker stays running after the dispatch tool ended, and
    /// the finish shows from the launch end, with no tool event at all.
    #[tokio::test]
    async fn the_roster_follows_a_launched_worker_from_start_to_finish() {
        use amp_core::supervisor::{LaunchEnds, WorkerOutcome as O, WorkerPhase as P};

        let ends = LaunchEnds::default();
        let (mut shell, _feed) = watching_shell(&ends).await;
        shell.test_sessions = Some(Vec::new());
        shell.build_roster();
        assert!(roster_entry(&shell, 2901).is_none());

        // Background start: the launch is queued when the dispatch tool returns its ack.
        shell.test_sessions = Some(vec![session(2901, Some(P::Queued), None, None)]);
        shell.handle_event(&tool_end(amp_core::policy::DISPATCH_TOOL));
        let e = roster_entry(&shell, 2901).expect("the launch shows at once");
        assert_eq!(e.phase, Phase::Queued);
        assert!(e.local);

        // It gets a slot while the manager does something else.
        shell.test_sessions = Some(vec![session(2901, Some(P::Running), None, None)]);
        shell.handle_event(&tool_end("amp_get_task"));
        assert_eq!(roster_entry(&shell, 2901).unwrap().phase, Phase::Running);
        assert_eq!(shell.view().strip().shown().len(), 1, "the strip shows it");

        // A later dispatch tool end with no change in who is live leaves it running.
        shell.handle_event(&tool_end(amp_core::policy::DISPATCH_TOOL));
        assert_eq!(roster_entry(&shell, 2901).unwrap().phase, Phase::Running);

        // The finish: only the supervisor's state and its launch end, no tool call.
        shell.test_sessions = Some(vec![session(2901, None, Some(O::Completed), None)]);
        ends.push(ended(2901, O::Completed, None));
        assert!(shell.show_launch_ends());
        let e = roster_entry(&shell, 2901).expect("a finished worker stays listed");
        assert_eq!(e.phase, Phase::Finished);
        assert_eq!(e.outcome.as_deref(), Some("completed"));
        assert!(shell.view().strip().shown().is_empty(), "nothing is live");
    }

    /// AMP #2862: Await mode keeps #2355's timing. The worker has ended by the time its tool
    /// call returns, and the roster sees that from the supervisor at that event, so the finished
    /// notice comes before the manager's reply. The tool's name plays no part.
    #[tokio::test]
    async fn await_mode_refreshes_when_the_worker_ends_inside_the_tool_call() {
        use amp_core::supervisor::{LaunchEnds, WorkerOutcome as O, WorkerPhase as P};

        let ends = LaunchEnds::default();
        let (mut shell, _feed) = watching_shell(&ends).await;
        shell.test_sessions = Some(vec![session(2902, Some(P::Running), None, None)]);
        shell.handle_event(&tool_end("bash"));
        assert_eq!(roster_entry(&shell, 2902).unwrap().phase, Phase::Running);

        shell.test_sessions = Some(vec![session(2902, None, Some(O::Completed), None)]);
        shell.handle_event(&tool_end(amp_core::policy::DISPATCH_TOOL));
        let e = roster_entry(&shell, 2902).unwrap();
        assert_eq!(
            e.phase,
            Phase::Finished,
            "refreshed at the event, not the next tick"
        );
        assert!(!shell.live_changed());
    }

    /// AMP #2862: workers that ended badly stay identifiable in the roster after they finish:
    /// an error whose ticket was returned, a stop whose return failed, a crashed worker task
    /// (no session behind it, #2885) with either hand-back result, and a launch never
    /// dispatched. They survive later rebuilds, and the panel says where each ticket went.
    #[tokio::test]
    async fn returned_failed_and_crashed_workers_stay_in_the_roster() {
        use amp_core::supervisor::{LAUNCH_CRASHED, LaunchEnd, LaunchEnds, WorkerOutcome as O};

        let ends = LaunchEnds::default();
        let (mut shell, _feed) = watching_shell(&ends).await;
        shell.test_sessions = Some(vec![
            session(2911, None, Some(O::Error), Some(Ok(()))),
            session(2912, None, Some(O::Cancelled), Some(Err("503".into()))),
            session(2913, None, Some(O::Completed), None),
        ]);
        let crash = |id: i64, tail: &str| LaunchEnd {
            task_id: id,
            agent_id: "amp:amp-worker-frontend".into(),
            result: Err(format!("{LAUNCH_CRASHED}: boom; #{id} {tail}")),
        };
        ends.push(crash(2914, "was returned to ready"));
        ends.push(crash(
            2915,
            "could not be returned to ready: 502 Bad Gateway",
        ));
        ends.push(LaunchEnd {
            task_id: 2916,
            agent_id: "amp:amp-worker-backend".into(),
            result: Err("AMP refused the dispatch".into()),
        });
        assert!(shell.show_launch_ends());
        // Later ticks rebuild the roster; nothing ended is lost.
        shell.build_roster();
        shell.poll();

        let get = |id| roster_entry(&shell, id).unwrap_or_else(|| panic!("#{id} is listed"));
        let e = get(2911);
        assert_eq!(
            (e.phase, e.outcome.as_deref()),
            (Phase::Finished, Some("error"))
        );
        assert_eq!(e.returned, Some(Ok(())));
        let e = get(2912);
        assert_eq!(e.outcome.as_deref(), Some("cancelled"));
        assert_eq!(e.returned, Some(Err("503".into())));
        assert_eq!(get(2913).outcome.as_deref(), Some("completed"));
        let e = get(2914);
        assert_eq!(
            (e.phase, e.outcome.as_deref()),
            (Phase::Finished, Some("crashed"))
        );
        assert_eq!(e.agent_id, "amp:amp-worker-frontend");
        assert!(e.state.starts_with("crashed"), "{}", e.state);
        assert_eq!(e.returned, Some(Ok(())));
        let e = get(2915);
        assert_eq!(e.outcome.as_deref(), Some("crashed"));
        assert_eq!(e.returned, Some(Err("502 Bad Gateway".into())));
        let e = get(2916);
        assert_eq!(e.outcome.as_deref(), Some("not_dispatched"));
        assert_eq!(e.returned, None);
        assert!(
            shell.view().roster().local().len() == 6,
            "all six are this chat's workers"
        );

        // A crashed ticket that runs again shows its new session, not the crash.
        shell.test_sessions.as_mut().unwrap().push(session(
            2914,
            Some(amp_core::supervisor::WorkerPhase::Running),
            None,
            None,
        ));
        shell.handle_event(&tool_end(amp_core::policy::DISPATCH_TOOL));
        assert_eq!(get_phase(&shell, 2914), Phase::Running);
    }

    fn get_phase(shell: &super::Shell, task_id: i64) -> Phase {
        roster_entry(shell, task_id).expect("listed").phase
    }
}
