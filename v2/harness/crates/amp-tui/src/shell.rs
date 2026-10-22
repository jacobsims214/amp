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
//! Quitting ([`Shell::quit`]) never drops a worker's run report (D8): with workers live it cancels
//! them, aborts the prompt and waits, bounded by [`Shell::quit_bound`], until each worker has
//! stopped and its `amp_report_run` has returned. Without workers it waits [`ABORT_GRACE`] for the
//! aborted prompt so its session file is complete.

use std::{
    future::Future,
    pin::Pin,
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::events::{RunEvent, Totals};
use amp_core::meta::MetaHandle;
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
use crate::overlay::{self, PanelKind, PanelState, Stage, Target};
use crate::slash::MenuKey;
use crate::view::{ChatView, FooterInfo};

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
    fn prepare(&self, args: &str) -> Result<String, String>;
    /// Checks `key` with the provider, stores it, and rebuilds the providers.
    fn connect(&self, args: &str, key: String) -> ConnectFuture;
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

/// What the spinner line says, for [`ESC_HINT_FOR`], after Esc closed a panel during a run.
pub const ESC_HINT: &str = "Esc again to stop the reply";

/// How long [`ESC_HINT`] stays next to the spinner.
pub const ESC_HINT_FOR: Duration = Duration::from_secs(2);

/// What the empty editor says after a first Ctrl+C there: a second one within
/// [`QUIT_HINT_FOR`] quits (#2184).
pub const QUIT_HINT: &str = "Press Ctrl+C again to quit";

/// How long a second Ctrl+C on the empty editor still quits.
pub const QUIT_HINT_FOR: Duration = Duration::from_millis(1500);

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
    running: bool,
    done_tx: UnboundedSender<RunDone>,
    /// The AMP metadata poller (#2104), if the caller started one.
    meta: Option<MetaHandle>,
    opener: Opener,
    msgs_tx: UnboundedSender<ShellMsg>,
    msgs_rx: UnboundedReceiver<ShellMsg>,
    /// Command calls still running.
    in_flight: usize,
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
    /// Until when the spinner line shows [`ESC_HINT`].
    esc_hint_until: Option<Instant>,
    /// Until when a second Ctrl+C on the empty editor quits ([`QUIT_HINT`]).
    quit_hint_until: Option<Instant>,
    /// Until when Ctrl+D at the latest output doesn't quit ([`CTRL_D_SETTLE`]).
    ctrl_d_settle_until: Option<Instant>,
    /// Whether the terminal reports the mouse to us (wheel scrolling, the pill's click). Off,
    /// the terminal's own click-drag selection works without a modifier (`[tui] mouse`, #2185).
    mouse_capture: bool,
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
                running: false,
                done_tx,
                meta: None,
                opener: Arc::new(|url: &str| amp_mcp::oauth::open_browser(url)),
                msgs_tx,
                msgs_rx,
                in_flight: 0,
                seen_revision: 0,
                marked: None,
                footer_ticket: None,
                cleared_done: None,
                supervisor: None,
                connect: None,
                secret: None,
                server: None,
                switching: false,
                esc_hint_until: None,
                quit_hint_until: None,
                ctrl_d_settle_until: None,
                mouse_capture: true,
            },
            done_rx,
        )
    }

    pub fn view(&self) -> &ChatView {
        &self.view
    }

    /// Whether the terminal loop turns mouse reporting on (`[tui] mouse`, default on, #2185).
    pub fn set_mouse_capture(&mut self, on: bool) {
        self.mouse_capture = on;
    }

    pub fn mouse_capture(&self) -> bool {
        self.mouse_capture
    }

    pub fn info(&self) -> &ShellInfo {
        &self.info
    }

    /// How the transcript shows thinking (`[tui] show_thinking`, #2177).
    pub fn set_show_thinking(&mut self, display: crate::ThinkingDisplay) {
        self.view.set_show_thinking(display);
    }

    /// Ctrl+T: expands or collapses the last thinking section. Returns whether there was one.
    pub fn toggle_thinking(&mut self) -> bool {
        self.view.toggle_thinking().is_some()
    }

    /// Ctrl+O: shows or hides every tool call's arguments and result (#2183, pi's
    /// `app.tools.expand`). Returns whether they are now shown.
    pub fn toggle_tools(&mut self) -> bool {
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
        self.abort().await;
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
                    repaint(self);
                    return QuitOutcome::Skipped;
                }
                () = tokio::time::sleep_until(deadline) => {
                    self.view.add_notice("gave up waiting");
                    repaint(self);
                    return QuitOutcome::TimedOut;
                }
                _ = ticker.tick() => {}
            }
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
            && let Some(refusal) =
                amp_core::approval::refusal(&amp_core::events::tool_result_text(result))
        {
            let snapshot = self.meta.as_ref().map(|m| m.snapshot());
            self.view
                .add_to_transcript(Arc::new(ApprovalNotice::new(refusal, snapshot.as_deref())));
        }
    }

    /// Handles one editor submission: a slash command or a prompt to the agent.
    pub async fn submit(&mut self, text: &str) -> Submit {
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

    fn start_run(&mut self, prompt: String) {
        self.running = true;
        self.prompts += 1;
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
        match result {
            Ok(r) => match r.outcome {
                HarnessRunOutcome::Completed { .. } => {}
                HarnessRunOutcome::Aborted { .. } => self.view.add_notice(STOPPED),
                // The provider may report a stopped request as an error ("Request was
                // aborted"): the person asked for it, so it is not one.
                HarnessRunOutcome::Failed { .. } if stopping => self.view.add_notice(STOPPED),
                HarnessRunOutcome::Failed { error, .. } => self
                    .view
                    .add_error(&format!("run failed: {}: {}", error.code, error.message)),
                HarnessRunOutcome::Suspended { .. } => self
                    .view
                    .add_notice("the provider deferred its response (run suspended)"),
            },
            Err(_) if stopping => self.view.add_notice(STOPPED),
            Err(e) => self.view.add_error(&format!("run failed: {e}")),
        }
    }

    /// A key the editor doesn't get first: the `/` menu's keys ([`crate::slash::CommandMenu`]),
    /// then an open AMP panel's keys ([`Shell::panel_key`]), then the transcript's scroll keys
    /// ([`Shell::scroll_key`]), then Esc during a run, which stops the reply. Returns true when it was used; otherwise the caller gives it to the editor
    /// (Enter on the menu puts the chosen command in the editor and returns false, so the editor
    /// submits it).
    ///
    /// Esc that closes a panel during a run doesn't stop it: the spinner line says
    /// [`ESC_HINT`] for [`ESC_HINT_FOR`], and the next Esc stops the reply (#2181).
    pub async fn key(&mut self, key: &KeyEvent, editor_empty: bool) -> bool {
        let is_esc = key.code == KeyCode::Esc
            && !key
                .modifiers
                .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT);
        let confirming = matches!(
            self.view.panel().state().map(|s| s.stage),
            Some(Stage::Confirm { .. } | Stage::Sending { .. })
        );
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
        let panel_open = self.view.panel().is_open();
        if self.panel_key(key, editor_empty) {
            if is_esc && self.running && panel_open && !self.view.panel().is_open() {
                self.show_esc_hint();
            }
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
        if self.running {
            self.abort().await;
            return CtrlD::Stopped;
        }
        CtrlD::Quit
    }

    fn show_esc_hint(&mut self) {
        self.esc_hint_until = Some(Instant::now() + ESC_HINT_FOR);
        self.view.set_working_message(ESC_HINT);
    }

    fn clear_esc_hint(&mut self) {
        if self.esc_hint_until.take().is_some() {
            self.view.set_working_message(WORKING);
        }
    }

    /// Ctrl+C at `now` (#2184): stops a running reply, else clears a non-empty editor, else
    /// shows [`QUIT_HINT`] in the empty editor; a second Ctrl+C there within [`QUIT_HINT_FOR`]
    /// returns [`CtrlC::Quit`]. `/quit`, Ctrl+D and signals still quit at once, and Ctrl+C
    /// during the "stopping N workers" wait skips it ([`Shell::quit`]).
    pub async fn ctrl_c(&mut self, now: Instant) -> CtrlC {
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
        if self.quit_hint_until.is_some_and(|until| now < until) {
            self.clear_quit_hint();
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
        changed
    }

    async fn command(&mut self, name: &str, arg: &str) -> Submit {
        match name {
            "quit" | "exit" | "q" => return Submit::Quit,
            "help" | "?" => self
                .view
                .add_to_transcript(Arc::new(crate::slash::HelpBlock::new())),
            "usage" => self.view.add_notice(&self.usage_text()),
            "model" if arg.is_empty() => self.view.add_notice(&self.model_text()),
            "model" => self.switch_model(arg).await,
            "connect" => self.connect_command(arg),
            "abort" => self.abort().await,
            "amp" => self.open_panel("amp", PanelKind::Amp),
            "workers" => self.open_panel("workers", PanelKind::Workers),
            "questions" => self.open_panel("questions", PanelKind::Questions),
            "ticket" => self.ticket_command(arg),
            "project" => self.project_command(arg),
            "plan" => self.plan_command(),
            "open" => self.open_command(arg),
            "server" => self.server_command(arg),
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
                if !editor_empty {
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
                        Some(target) => self.open_target(&target),
                        None => {}
                    },
                    KeyCode::Char('o') => {
                        if let Some(target) = panel.selected() {
                            self.open_target(&target);
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
    pub fn press(&mut self, name: &str) -> Result<bool, String> {
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
        let code = match name {
            "enter" => KeyCode::Enter,
            "esc" => KeyCode::Esc,
            "up" => KeyCode::Up,
            "down" => KeyCode::Down,
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
        Ok(self.panel_key(&key, true))
    }

    fn open_target(&mut self, target: &Target) {
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
                ShellMsg::Notice { .. } => {}
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
        format!(
            "usage this session: {} prompt(s), {} turn(s), {} tool call(s) ({} failed)\n\
             tokens: input {} · output {} · cache read {} · cache write {} · cost ${:.4}\n\
             {}\n\
             model: {} • thinking {}\n\
             session: {}",
            self.prompts,
            t.turns,
            t.tool_calls,
            t.tool_errors,
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

    fn model_text(&self) -> String {
        let mut text = format!(
            "model: {} • thinking {} (role {})",
            self.info.model_ref(),
            thinking_name(self.info.thinking),
            short_role(&self.info.role),
        );
        if !self.models.is_empty() {
            text.push_str("\navailable: ");
            text.push_str(
                &self
                    .models
                    .iter()
                    .map(|m| m.reference.as_str())
                    .collect::<Vec<_>>()
                    .join(", "),
            );
            text.push_str("\nswitch with /model provider/model");
        }
        text
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
                "unknown model `{reference}` (/model lists the available ones)"
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
