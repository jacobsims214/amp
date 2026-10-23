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
//! Answering a question is a write: Enter on a question in `/questions` asks for the answer in
//! the editor, the panel then shows exactly what will be sent, and only `y` / Enter sends it.
//!
//! The transcript gets a marker line whenever the current ticket changes (switched, claimed, or
//! its state moved), and the footer's session name becomes `manager · #2108`.
//!
//! Quitting ([`Shell::quit`]) never drops a worker's run report (D8): with workers live it cancels
//! them, aborts the prompt and waits, bounded by [`Shell::quit_bound`], until each worker has
//! stopped and its `amp_report_run` has returned. Without workers it waits [`ABORT_GRACE`] for the
//! aborted prompt so its session file is complete.

use std::{future::Future, sync::Arc, time::Duration};

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

use crate::amp_render;
use crate::overlay::{self, PanelKind, PanelState, Stage, Target};
use crate::view::{ChatView, FooterInfo};

/// Opens a URL in the browser (default: login's guarded opener; tests record instead).
pub type Opener = Arc<dyn Fn(&str) -> Result<(), String> + Send + Sync>;

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

/// What the caller does after a submission.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Submit {
    Continue,
    Quit,
}

pub const HELP: &str = "AMP: /amp  /ticket [id|none]  /workers  /questions  /plan  /project [id]  /open [id]\n\
commands: /model [provider/model]  /usage  /help  /quit\n\
keys: Enter send · Shift+Enter new line · Esc abort the run (or close a panel) · Ctrl+C abort, clear, or quit\n\
panels: open focused (↑↓ select, Enter act on the row) · typing goes to the editor and unfocuses · Tab switches";

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
    /// The manager's worker supervisor, so quitting can cancel and wait for its workers.
    supervisor: Option<Arc<Supervisor>>,
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
                supervisor: None,
            },
            done_rx,
        )
    }

    pub fn view(&self) -> &ChatView {
        &self.view
    }

    pub fn info(&self) -> &ShellInfo {
        &self.info
    }

    pub fn is_running(&self) -> bool {
        self.running
    }

    /// Gives the shell the session's worker supervisor (the manager's; `None` for a worker role).
    pub fn set_supervisor(&mut self, supervisor: Option<Arc<Supervisor>>) {
        self.supervisor = supervisor;
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
        // A prompt hands the keys back to the editor; a docked panel stays, unfocused.
        self.view.panel().update(|s| s.focused = false);
        if self.running {
            self.view
                .add_notice("still working: wait for the reply, or press Esc to abort it");
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

    /// Asks the running prompt to stop; it then finishes as aborted.
    pub async fn abort(&mut self) {
        if !self.running {
            return;
        }
        self.view.add_notice("aborting…");
        if let Err(e) = self.harness.abort().await {
            self.view.add_error(&format!("abort: {e}"));
        }
    }

    /// Records a finished run. Drain the agent events first: they were all sent before it ended.
    pub fn finish_run(&mut self, result: RunDone) {
        self.running = false;
        self.view.set_working(false);
        match result {
            Ok(r) => match r.outcome {
                HarnessRunOutcome::Completed { .. } => {}
                HarnessRunOutcome::Aborted { .. } => self.view.add_notice("aborted"),
                HarnessRunOutcome::Failed { error, .. } => self
                    .view
                    .add_error(&format!("run failed: {}: {}", error.code, error.message)),
                HarnessRunOutcome::Suspended { .. } => self
                    .view
                    .add_notice("the provider deferred its response (run suspended)"),
            },
            Err(e) => self.view.add_error(&format!("run failed: {e}")),
        }
    }

    async fn command(&mut self, name: &str, arg: &str) -> Submit {
        match name {
            "quit" | "exit" | "q" => return Submit::Quit,
            "help" | "?" => self.view.add_notice(HELP),
            "usage" => self.view.add_notice(&self.usage_text()),
            "model" if arg.is_empty() => self.view.add_notice(&self.model_text()),
            "model" => self.switch_model(arg).await,
            "abort" => self.abort().await,
            "amp" => self.open_panel("amp", PanelKind::Amp),
            "workers" => self.open_panel("workers", PanelKind::Workers),
            "questions" => self.open_panel("questions", PanelKind::Questions),
            "ticket" => self.ticket_command(arg),
            "project" => self.project_command(arg),
            "plan" => self.plan_command(),
            "open" => self.open_command(arg),
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

    /// Routes a key (not Ctrl+C / Ctrl+D, which the terminal loop handles) to an open panel or
    /// the editor. Returns false only for an Esc that no panel took while a prompt runs: the
    /// caller aborts the run with it.
    pub fn key(&mut self, key: KeyEvent) -> bool {
        let editor = self.view.editor().clone();
        if self.panel_key(&key, editor.get_text().is_empty()) {
            return true;
        }
        if key.code == KeyCode::Esc && self.running {
            return false;
        }
        editor.handle_key(key);
        true
    }

    /// A paste goes to the editor and hands the keys back to it.
    pub fn paste(&mut self, text: &str) {
        self.unfocus_panel();
        self.view.editor().handle_paste(text);
    }

    fn unfocus_panel(&self) {
        self.view.panel().update(|s| s.focused = false);
    }

    /// A key while a panel is open. Returns true when the panel used it (the editor must not).
    ///
    /// Esc closes the panel (or backs out of an answer). A printable key always goes to the
    /// editor, and unfocuses the panel (#2161: typing "ok…" must never open a browser). While
    /// the panel is focused and the editor empty, ↑/↓/PgUp/PgDn select and Enter acts on the
    /// selection (answers a question; otherwise opens it). Tab, with an empty editor, switches
    /// focus between the panel and the editor. While an answer waits for confirmation,
    /// `y`/Enter sends it and `n`/Esc cancels; other keys are swallowed.
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
                        s.focused = true;
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
                            s.focused = true;
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
                if key.code == KeyCode::Tab && editor_empty {
                    panel.update(|s| s.focused = !s.focused);
                    return true;
                }
                // Typing: the key is the editor's, and so are the keys after it.
                if matches!(key.code, KeyCode::Char(_)) || !editor_empty {
                    if state.focused {
                        self.unfocus_panel();
                    }
                    return false;
                }
                if !state.focused {
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
                    _ => return false,
                }
                true
            }
        }
    }

    /// [`Self::panel_key`] by name, with an empty editor (`--snapshot`'s `--input key:<name>`):
    /// `enter`, `esc`, `up`, `down`, `tab`, or one character.
    pub fn press(&mut self, name: &str) -> Result<bool, String> {
        let code = match name {
            "enter" => KeyCode::Enter,
            "esc" => KeyCode::Esc,
            "tab" => KeyCode::Tab,
            "up" => KeyCode::Up,
            "down" => KeyCode::Down,
            c if c.chars().count() == 1 => KeyCode::Char(c.chars().next().unwrap_or(' ')),
            other => {
                return Err(format!(
                    "key `{other}`: not a key (enter, esc, up, down, tab, or one character)"
                ));
            }
        };
        Ok(self.panel_key(&KeyEvent::new(code, KeyModifiers::NONE), true))
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
        let mut changed = false;
        while let Ok(msg) = self.msgs_rx.try_recv() {
            self.in_flight = self.in_flight.saturating_sub(1);
            changed = true;
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
                ShellMsg::Answered { ticket, result } => match result {
                    Ok(_) => {
                        self.view.panel().update(|s| {
                            s.stage = Stage::Browse;
                            s.focused = true;
                            s.note = Some((format!("answered #{ticket}"), false));
                        });
                        self.view
                            .add_notice(&format!("answered #{ticket} (amp_answer_question)"));
                    }
                    Err(e) => {
                        let e = crate::chrome::clean(&e);
                        self.view.panel().update(|s| {
                            s.stage = Stage::Browse;
                            s.focused = true;
                            s.note = Some((format!("not answered: {e}"), true));
                        });
                        self.view
                            .add_error(&format!("answer to #{ticket} failed: {e}"));
                    }
                },
            }
        }
        changed | self.sync_meta()
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
             model: {} • thinking {}",
            self.prompts,
            t.turns,
            t.tool_calls,
            t.tool_errors,
            u.input_tokens,
            u.output_tokens,
            u.cache_read_tokens,
            u.cache_write_tokens,
            u.cost_usd,
            self.info.model_ref(),
            thinking_name(self.info.thinking),
        )
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
