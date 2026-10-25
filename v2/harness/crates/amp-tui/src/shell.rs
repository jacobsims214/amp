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
use amp_core::supervisor::Supervisor;
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
    /// A line from a background task the shell did not start ([`Notifier`]); not counted in
    /// `in_flight`.
    Notice {
        text: String,
        error: bool,
    },
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

/// [`ESC_HINT`], naming this chat's live workers, which stopping the reply stops too (#2354).
pub fn esc_hint(tickets: &[i64]) -> String {
    match tickets.len() {
        0 => ESC_HINT.to_owned(),
        1 => format!("{ESC_HINT} and the worker on #{}", tickets[0]),
        _ => format!("{ESC_HINT} and the workers on {}", ticket_list(tickets)),
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
}

impl Shell {
    /// The shell and the receiver its runs finish on (select on it next to the agent events).
    pub fn new(
        harness: Arc<AgentHarness>,
        info: ShellInfo,
        models: Vec<ModelChoice>,
    ) -> (Self, UnboundedReceiver<RunDone>) {
        let (done_tx, done_rx) = unbounded_channel();
        let (msgs_tx, msgs_rx) = unbounded_channel();
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
                done_tx,
                meta: None,
                opener: Arc::new(|url: &str| amp_mcp::oauth::open_browser(url)),
                msgs_tx,
                msgs_rx,
                in_flight: 0,
                call_tickets: Default::default(),
                seen_revision: 0,
                marked: None,
                footer_ticket: None,
                cleared_done: None,
                supervisor: None,
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
            },
            done_rx,
        )
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
        self.supervisor = supervisor;
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

    /// Pasted text: into the `/connect` key when one is being typed, else the editor.
    pub fn paste(&mut self, text: &str) {
        match self.secret.as_mut() {
            Some(entry) => entry.buf.push_str(text.trim_end_matches(['\r', '\n'])),
            None => self.view.editor().handle_paste(text),
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
    }

    /// The AMP metadata poller, for the header and status line (M3-3).
    pub fn meta(&self) -> Option<&MetaHandle> {
        self.meta.as_ref()
    }

    /// Replaces the browser opener (tests record the URL instead of opening it).
    pub fn set_opener(&mut self, opener: Opener) {
        self.opener = opener;
    }

    pub fn handle_event(&mut self, event: &AgentEvent) {
        if let Some(e) = RunEvent::from_agent(event) {
            self.totals.observe(&e);
        }
        if let Some(meta) = &self.meta {
            meta.observe(event);
        }
        self.view.apply(event);
        // #2355: a worker's run has ended (the supervisor recorded it) by the time its
        // `amp_run_ticket` returns: the strip stops saying running, and its finished notice lands
        // now, before the manager's reply, not on the next tick.
        if let AgentEvent::ToolExecutionEnd { tool_name, .. } = event
            && tool_name == amp_core::policy::DISPATCH_TOOL
        {
            self.build_roster();
        }
        if let AgentEvent::ToolExecutionStart {
            tool_call_id,
            tool_name,
            args,
        } = event
            && tool_name.starts_with("amp_")
            && let Some(id) = amp_core::approval::task_id_arg(args)
        {
            self.call_tickets.insert(tool_call_id.clone(), id);
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
            let snapshot = self.meta.as_ref().map(|m| m.snapshot());
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
        if self.running {
            self.view
                .add_notice("still working: wait for the reply, or press Esc to abort it");
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
        self.refresh_pending_plan();
        self.running = true;
        self.prompts += 1;
        self.view.take_error_shown();
        self.view.set_working(true);
        let harness = Arc::clone(&self.harness);
        let done = self.done_tx.clone();
        tokio::spawn(async move {
            // The run gets its own task so a panic in it still reports back here.
            let run = tokio::spawn(async move { harness.prompt_text(&prompt, Vec::new()).await });
            let result = match run.await {
                Ok(Ok(r)) => Ok(r),
                Ok(Err(e)) => Err(e.to_string()),
                Err(e) => Err(format!("the run crashed: {e}")),
            };
            let _ = done.send(result);
        });
    }

    /// Asks the running prompt to stop; it then finishes as aborted, and the transcript says
    /// "Stopped." once, below whatever the reply had streamed ([`Shell::finish_run`]).
    pub async fn abort(&mut self) {
        if self.running {
            let live = self.live_tickets();
            if !live.is_empty() {
                // Stopping the reply cancels the manager's amp_run_ticket, and so its workers.
                self.view.add_warning(&worker_stop_notice(&live));
            }
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
        self.view.set_working(false);
        let stopping = self.view.stopping();
        self.view.set_stopping(false);
        // The reply's error line already says what went wrong: say it once (#2207).
        let error_shown = self.view.take_error_shown();
        match result {
            Ok(r) => match r.outcome {
                HarnessRunOutcome::Completed { .. } => {}
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
        self.view
            .set_working_message(&esc_hint(&self.live_tickets()));
    }

    fn clear_esc_hint(&mut self) {
        if self.esc_hint_until.take().is_some() {
            self.view.set_working_message(WORKING);
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
        // `/open plan`: the Flight plan page, where a person approves the plan (#2171).
        if arg.eq_ignore_ascii_case("plan") {
            match (&m.instance, m.project_id) {
                (Some(inst), Some(pid)) => {
                    let url = inst.plan_url(pid);
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
    /// Esc closes the panel (or backs out of an answer). With an empty editor, ↑/↓ select,
    /// Enter acts on the selection (answers a question; otherwise opens it) and `o` opens it.
    /// While an answer waits for confirmation, `y`/Enter sends it and `n`/Esc cancels; other keys
    /// are swallowed.
    pub fn panel_key(&mut self, key: &KeyEvent, editor_empty: bool) -> bool {
        let panel = self.view.panel().clone();
        let Some(state) = panel.state() else {
            return false;
        };
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
                // #2344: a click into the editor gave it the keys.
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
                    // A configuration panel's letter keys (#2245): before `o`, which they may use.
                    KeyCode::Char(c)
                        if state.kind == PanelKind::Info
                            && overlay::info_command(&state, c).is_some() =>
                    {
                        self.pending_command = overlay::info_command(&state, c);
                    }
                    KeyCode::Char('o') => {
                        if let Some(target) = panel.selected() {
                            self.open_target(&target);
                        }
                    }
                    KeyCode::Char('x') if state.kind == PanelKind::Workers => {
                        if let Some(Target::Ticket { id, .. }) = panel.selected() {
                            self.ask_stop(id);
                        }
                    }
                    _ => return false,
                }
                true
            }
        }
    }

    /// [`Self::panel_key`] by name, with an empty editor (`--snapshot`'s `--input key:<name>`):
    /// `enter`, `esc`, `up`, `down`, or one character. `ctrl+t` toggles the last thinking,
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
                    "key `{other}`: not a key (enter, esc, up, down, or one character)"
                ));
            }
        };
        let key = KeyEvent::new(code, KeyModifiers::NONE);
        if self.secret_key(&key) {
            return Ok(true);
        }
        if self.stop_confirm.is_some() || self.viewing.is_some() || self.view.strip().is_focused() {
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
        changed |= self.build_roster();
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
            if let ShellMsg::ReloadRequest = &msg {
                self.reload_command(ReloadCause::Watcher);
                continue;
            }
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
                ShellMsg::Notice { .. } | ShellMsg::ReloadRequest => {}
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
        // The Done marker first (sync_meta), then let the finished ticket go.
        let synced = self.sync_meta();
        changed | synced | self.clear_done_ticket()
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
        let sessions = self
            .supervisor
            .as_ref()
            .map(|s| s.sessions())
            .unwrap_or_default();
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
                    viewable: true,
                    local: true,
                    ..WorkerEntry::default()
                },
            );
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
    use super::{clean_answer, parse_id, short_role};

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

    #[test]
    fn short_role_drops_the_prefixes() {
        assert_eq!(short_role("amp-manager"), "manager");
        assert_eq!(short_role("amp:amp-worker-backend"), "worker-backend");
        assert_eq!(short_role("reviewer"), "reviewer");
    }
}
