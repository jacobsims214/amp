//! `/sessions` (AMP #3010, artifact #28): the picker opens on this directory's saved chats,
//! browse → confirm → resume, and cancelling changes nothing.
//!
//! The shell runs on a real rpi `AgentHarness` (faux provider) over an in-memory session, with a
//! [`SessionControl`] stand-in for the binary: its `list` returns the rows a scan of the state dir
//! would (this file builds them; `amp-core`'s `session_index` tests cover the scan itself), and its
//! `resume` records the request it was given. So what is tested here is the picker's own
//! contract — registry, stages, keys, the busy refusal, and that a browse or a cancel never
//! changes the active session — not the switch transaction, which is #3012's.

use std::{
    path::PathBuf,
    sync::{Arc, Mutex},
    time::{Duration, SystemTime},
};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{
    ResumeFuture, ResumeRequest, Resumed, SessionControl, SessionList, SessionListFuture,
    SessionRow, Shell, ShellInfo, Stage, Submit,
};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use rpi_tui::visible_width;

const W: usize = 110;
const H: usize = 34;
const WAIT: Duration = Duration::from_secs(20);

/// One saved chat in the state dir, as the scan would list it.
fn row(id: &str, age_secs: u64, role: &str, preview: &str) -> SessionRow {
    SessionRow {
        path: PathBuf::from(format!(
            "/state/amp-harness/sessions/{}/{age_secs}-{id}.jsonl",
            if role.is_empty() { "amp-manager" } else { role }
        )),
        id: id.to_owned(),
        role: role.to_owned(),
        modified_at: SystemTime::now() - Duration::from_secs(age_secs),
        preview: Some(preview.to_owned()),
        problem: None,
    }
}

/// What the binary would report for this chat: three saved sessions, one of them the current one.
fn listing(current: Option<PathBuf>) -> SessionList {
    SessionList {
        scope: "/work/amp".to_owned(),
        rows: vec![
            row(
                "8f3d…a1c2",
                2 * 3600,
                "",
                "…the changelog groups the tickets by epic",
            ),
            row(
                "b2c4…7f09",
                0,
                "",
                "Two tickets in AMP Harness › M4 are ready",
            ),
            row("5e21…9b77", 86_400, "", "…the publish ticket stays blocked"),
        ],
        current,
    }
}

/// The binary's side, recorded: what it was asked to list, what it was asked to resume.
struct Recorded {
    listed: Vec<PathBuf>,
    resumed: Vec<ResumeRequest>,
    /// What `resume` answers. `None`: the switch succeeds.
    refuse: Option<String>,
    /// The transcript a successful switch hands back (AMP #3012). Only #3012's reset-list tests
    /// fill it: the picker's own tests pass `None` and never look at the conversation.
    transcript: Option<Vec<(Option<SystemTime>, rpi_agent::AgentEvent)>>,
}

#[derive(Clone)]
struct Control(Arc<Mutex<Recorded>>);

impl Control {
    fn new(refuse: Option<String>) -> Self {
        Self(Arc::new(Mutex::new(Recorded {
            listed: Vec::new(),
            resumed: Vec::new(),
            refuse,
            transcript: None,
        })))
    }

    /// A control whose switch also hands back a transcript (AMP #3012's reset list).
    fn with_transcript(transcript: Vec<(Option<SystemTime>, rpi_agent::AgentEvent)>) -> Self {
        let control = Self::new(None);
        control.0.lock().unwrap().transcript = Some(transcript);
        control
    }

    fn listed(&self) -> Vec<PathBuf> {
        self.0.lock().unwrap().listed.clone()
    }

    fn resumed(&self) -> Vec<ResumeRequest> {
        self.0.lock().unwrap().resumed.clone()
    }
}

impl SessionControl for Control {
    fn list(&self) -> SessionListFuture {
        let control = self.clone();
        Box::pin(async move {
            control
                .0
                .lock()
                .unwrap()
                .listed
                .push(PathBuf::from("/work/amp"));
            Ok(listing(None))
        })
    }

    fn resume(&self, request: ResumeRequest) -> ResumeFuture {
        let control = self.clone();
        Box::pin(async move {
            control.0.lock().unwrap().resumed.push(request.clone());
            let recorded = control.0.lock().unwrap();
            match recorded.refuse.clone() {
                Some(why) => Err(why),
                None => Ok(Resumed {
                    path: request.path,
                    messages: 38,
                    model: Some("openrouter/xiaomi/mimo-v2.6-pro".to_owned()),
                    transcript: recorded.transcript.clone().unwrap_or_default(),
                }),
            }
        })
    }
}

struct Rig {
    shell: Shell,
    control: Control,
    /// A run's events, kept so `drive_until_idle` has something to drain.
    events: tokio::sync::mpsc::UnboundedReceiver<rpi_agent::AgentEvent>,
    done: tokio::sync::mpsc::UnboundedReceiver<amp_tui::RunDone>,
}

async fn rig(control: Control) -> Rig {
    rig_with(control, FauxScript::new()).await
}

async fn rig_with(control: Control, script: FauxScript) -> Rig {
    let provider = FauxProvider::new(script);
    let model = provider.default_model().clone();
    let (emitter, events) = ChannelEmitter::new();
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(emitter.into_emitter()),
        ..Default::default()
    })
    .await
    .expect("harness");
    let info = ShellInfo {
        role: "amp-manager".into(),
        provider: model.provider.to_string(),
        model_id: model.id.clone(),
        thinking: ThinkingLevel::Off,
        requested_thinking: None,
        context_window: 200_000,
        cwd: "/work/amp".into(),
        git_branch: None,
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, done) = Shell::new(Arc::new(harness), info, Vec::new());
    shell.set_session_control(Arc::new(control.clone()) as Arc<dyn SessionControl>);
    Rig {
        shell,
        control,
        events,
        done,
    }
}

impl Rig {
    /// Runs `text` as a slash command and lets its background work land.
    async fn say(&mut self, text: &str) {
        assert_eq!(self.shell.submit(text).await, Submit::Continue);
        self.shell
            .drive_until_idle(&mut self.events, &mut self.done, WAIT)
            .await
            .expect("idle");
        self.settle().await;
    }

    async fn settle(&mut self) {
        for _ in 0..40 {
            tokio::time::sleep(Duration::from_millis(5)).await;
            self.shell.poll();
        }
        self.shell.settle(WAIT).await.expect("settled");
    }

    /// A key as the terminal loop routes it: to the open panel, else the editor.
    async fn key(&mut self, code: KeyCode) {
        self.key_with(code, KeyModifiers::NONE).await;
    }

    async fn key_with(&mut self, code: KeyCode, modifiers: KeyModifiers) {
        let key = KeyEvent::new(code, modifiers);
        let empty = self.shell.view().editor().get_text().is_empty();
        if !self.shell.key(&key, empty).await {
            self.shell.editor_key(key);
        }
        self.settle().await;
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(W, H).join("\n")
    }

    /// Everything the conversation holds, not just what fits the frame: the transcript is taller
    /// than the screen after a restore, and the point of these tests is what is *in* it.
    fn transcript(&self) -> String {
        self.shell
            .view()
            .transcript_component()
            .render(W)
            .join("\n")
    }

    fn stage(&self) -> Stage {
        self.shell
            .view()
            .panel()
            .state()
            .expect("a panel is open")
            .stage
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sessions_is_a_listed_command_that_opens_the_picker() {
    // The registry and the shell agree: the command exists in both.
    let spec = amp_tui::COMMANDS
        .iter()
        .find(|c| c.name == "sessions")
        .expect("/sessions is in COMMANDS");
    assert_eq!(spec.usage(), "/sessions");
    assert!(spec.about.contains("this directory"), "{}", spec.about);

    let mut rig = rig(Control::new(None)).await;
    rig.say("/sessions").await;
    assert!(!rig.control.listed().is_empty(), "the scan was asked for");

    let screen = rig.screen();
    // The box, its scope line and the rows (artifact #28).
    assert!(screen.contains("Sessions · this directory · 3"), "{screen}");
    assert!(
        screen.contains("scope /work/amp · exact match on the session's directory"),
        "{screen}"
    );
    assert!(screen.contains("8f3d…a1c2"), "{screen}");
    assert!(
        screen.contains("…the changelog groups the tickets by epic"),
        "{screen}"
    );
    assert!(
        screen.contains("↑↓ select · Enter resume · Esc close"),
        "{screen}"
    );
    assert!(rig.shell.view().panel().is_open());

    // Every rendered row fits the frame at the widths the harness checks.
    for w in [40, 60, 80, 140] {
        for line in rig.shell.view().render_frame(w, H) {
            assert!(visible_width(&line) <= w, "{w}: {line:?}");
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn browsing_and_cancelling_never_resumes_or_changes_the_session() {
    let mut rig = rig(Control::new(None)).await;
    rig.say("/sessions").await;

    // ↑/↓ and Ctrl+P / Ctrl+N move the cursor among the three rows; PageUp/PageDown too.
    rig.key(KeyCode::Down).await;
    assert_eq!(rig.stage(), Stage::Browse);
    rig.key_with(KeyCode::Char('n'), KeyModifiers::CONTROL)
        .await;
    rig.key_with(KeyCode::Char('p'), KeyModifiers::CONTROL)
        .await;
    rig.key(KeyCode::Up).await;
    rig.key(KeyCode::PageDown).await;
    rig.key(KeyCode::PageUp).await;

    // Esc closes the picker with nothing asked.
    rig.key(KeyCode::Esc).await;
    assert!(!rig.shell.view().panel().is_open(), "Esc closed it");
    assert!(rig.control.resumed().is_empty(), "nothing was resumed");

    // `q` closes it too, from the list (artifact #28's keyboard table).
    rig.say("/sessions").await;
    rig.key(KeyCode::Char('q')).await;
    assert!(!rig.shell.view().panel().is_open(), "q closed it");
    assert!(rig.control.resumed().is_empty());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn enter_asks_once_and_then_resumes_the_picked_file() {
    let mut rig = rig(Control::new(None)).await;
    rig.say("/sessions").await;

    // Enter on the first row asks in place; nothing is loaded yet.
    rig.key(KeyCode::Enter).await;
    match rig.stage() {
        Stage::Resume { path } => assert!(path.to_string_lossy().contains("8f3d…a1c2"), "{path:?}"),
        other => panic!("{other:?}"),
    }
    assert!(
        rig.control.resumed().is_empty(),
        "Enter on the list only asks"
    );
    let screen = rig.screen();
    assert!(screen.contains("Resume 8f3d…a1c2"), "{screen}");
    assert!(screen.contains("stays saved as this chat"), "{screen}");
    assert!(
        screen.contains("does not re-run its tool calls"),
        "{screen}"
    );
    assert!(screen.contains("Enter resume · Esc back"), "{screen}");

    // Esc goes back to the list, cursor where it was, still nothing resumed.
    rig.key(KeyCode::Esc).await;
    assert_eq!(rig.stage(), Stage::Browse);
    assert!(rig.control.resumed().is_empty());

    // Enter, then Enter again: one request for exactly that file, and the picker closes.
    rig.key(KeyCode::Enter).await;
    rig.key(KeyCode::Enter).await;
    let resumed = rig.control.resumed();
    assert_eq!(resumed.len(), 1, "{resumed:?}");
    assert!(resumed[0].path.to_string_lossy().contains("8f3d…a1c2"));
    assert_eq!(resumed[0].id, "8f3d…a1c2");
    assert!(!rig.shell.view().panel().is_open(), "the picker closed");
    let screen = rig.screen();
    assert!(
        screen.contains("✓ resumed 8f3d…a1c2")
            && screen.contains("38 messages")
            && screen.contains("continues in the original file"),
        "{screen}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_refused_resume_keeps_the_chat_and_says_why() {
    let mut rig = rig(Control::new(Some(
        "session /state/…/8f3d…a1c2.jsonl is owned by another live process".to_owned(),
    )))
    .await;
    rig.say("/sessions").await;
    rig.key(KeyCode::Enter).await;
    rig.key(KeyCode::Enter).await;

    assert_eq!(rig.control.resumed().len(), 1, "it was asked");
    let screen = rig.screen();
    assert!(screen.contains("was not resumed"), "{screen}");
    assert!(screen.contains("owned by another live process"), "{screen}");
    // The picker comes back on the list so another row can be picked.
    assert!(rig.shell.view().panel().is_open());
    assert_eq!(rig.stage(), Stage::Browse);
    assert!(screen.contains("5e21…9b77"), "the list is back: {screen}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn while_a_reply_runs_the_picker_never_opens() {
    // A slow reply, so the picker is asked for while it is still streaming (as a person would).
    let script = FauxScript::new().with_tokens_per_second(20.0);
    let words: Vec<String> = (0..400).map(|i| format!("word{i}")).collect();
    script.set_responses(vec![FauxStep::text(words.join(" "))]);
    let mut rig = rig_with(Control::new(None), script).await;
    assert_eq!(rig.shell.submit("go").await, Submit::Continue);
    assert!(rig.shell.is_running(), "a reply is in flight");
    // Asked for mid-reply: `poll` applies the transcript lines; the run itself is not waited for
    // (it streams for 20 s, and the point is that `/sessions` never touched it).
    assert_eq!(rig.shell.submit("/sessions").await, Submit::Continue);
    for _ in 0..20 {
        tokio::time::sleep(Duration::from_millis(5)).await;
        rig.shell.poll();
    }
    assert!(rig.shell.is_running(), "still replying, and still fine");

    assert!(!rig.shell.view().panel().is_open(), "no picker");
    assert!(rig.control.listed().is_empty(), "no scan either");
    // The notice is one wrapped line in the Conversation box: compare it with the box's edges
    // and wrapping removed.
    let screen = rig.screen();
    let flat = screen
        .lines()
        .map(|l| l.trim_matches(|c| c == '│' || c == ' ').trim_matches('┌'))
        .collect::<Vec<_>>()
        .join(" ")
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    assert!(
        flat.contains("/sessions is unavailable: the manager is replying"),
        "{screen}"
    );
    assert!(flat.contains("Your chat is unchanged"), "{screen}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_current_session_is_marked_and_cannot_be_picked() {
    struct Current(PathBuf);
    impl SessionControl for Current {
        fn list(&self) -> SessionListFuture {
            let current = self.0.clone();
            Box::pin(async move { Ok(listing(Some(current))) })
        }
        fn resume(&self, _request: ResumeRequest) -> ResumeFuture {
            Box::pin(async move { Err("the current session is not selectable".to_owned()) })
        }
    }

    // The chat's own file is the second row of the listing.
    let current = listing(None).rows[1].path.clone();
    let provider = FauxProvider::new(rpi_ai::providers::faux::FauxScript::new());
    let model = provider.default_model().clone();
    let (emitter, mut events) = ChannelEmitter::new();
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(emitter.into_emitter()),
        ..Default::default()
    })
    .await
    .expect("harness");
    let info = ShellInfo {
        role: "amp-manager".into(),
        provider: model.provider.to_string(),
        model_id: model.id.clone(),
        thinking: ThinkingLevel::Off,
        requested_thinking: None,
        context_window: 200_000,
        cwd: "/work/amp".into(),
        git_branch: None,
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, mut done) = Shell::new(Arc::new(harness), info, Vec::new());
    shell.set_session_control(Arc::new(Current(current.clone())));
    assert_eq!(shell.submit("/sessions").await, Submit::Continue);
    shell
        .drive_until_idle(&mut events, &mut done, WAIT)
        .await
        .expect("idle");
    for _ in 0..40 {
        tokio::time::sleep(Duration::from_millis(5)).await;
        shell.poll();
    }
    shell.settle(WAIT).await.expect("settled");

    let screen = shell.view().render_plain(W, H).join("\n");
    assert!(screen.contains("· current"), "{screen}");
    // Three clean rows: the count alone (artifact #28's list screen). The current session is one of
    // them — present and well-formed, just not something to pick.
    assert!(screen.contains("Sessions · this directory · 3"), "{screen}");

    // ↑/↓ visit only the two other rows: the current one is not a target.
    let panel = shell.view().panel();
    let targets = panel.targets();
    assert_eq!(targets.len(), 2, "{targets:?}");
    assert!(
        !targets
            .iter()
            .any(|t| matches!(t, amp_tui::Target::Session(path) if path == &current)),
        "{targets:?}"
    );
    assert_eq!(
        panel.selected(),
        Some(amp_tui::Target::Session(listing(None).rows[0].path.clone()))
    );
}

/// The confirming state is not the editor's: keys go to the panel, so typing cannot leak into a
/// prompt while a resume waits for its Enter.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_confirm_stage_keeps_the_keys() {
    let mut rig = rig(Control::new(None)).await;
    rig.say("/sessions").await;
    rig.key(KeyCode::Enter).await;
    assert!(matches!(rig.stage(), Stage::Resume { .. }));
    assert!(
        !rig.shell.input_focused(),
        "the editor does not have the keys while confirming"
    );

    // Typing goes nowhere: the editor stays empty (the panel has the keys).
    rig.key(KeyCode::Char('x')).await;
    assert_eq!(rig.shell.view().editor().get_text(), "");
    assert!(matches!(rig.stage(), Stage::Resume { .. }));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn without_a_session_store_the_command_says_so() {
    // No control: `--no-session`, or a harness built without the binary's side.
    let provider = FauxProvider::new(rpi_ai::providers::faux::FauxScript::new());
    let model = provider.default_model().clone();
    let (emitter, mut events) = ChannelEmitter::new();
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(emitter.into_emitter()),
        ..Default::default()
    })
    .await
    .expect("harness");
    let info = ShellInfo {
        role: "amp-manager".into(),
        provider: model.provider.to_string(),
        model_id: model.id.clone(),
        thinking: ThinkingLevel::Off,
        requested_thinking: None,
        context_window: 200_000,
        cwd: "/work/amp".into(),
        git_branch: None,
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, mut done) = Shell::new(Arc::new(harness), info, Vec::new());
    assert_eq!(shell.submit("/sessions").await, Submit::Continue);
    shell
        .drive_until_idle(&mut events, &mut done, WAIT)
        .await
        .expect("idle");
    let screen = shell.view().render_plain(W, H).join("\n");
    assert!(
        screen.contains("/sessions: not available in this session"),
        "{screen}"
    );
    assert!(!shell.view().panel().is_open());
}

// -- AMP #3012: the reset list ----------------------------------------------------------------

/// What a worker saved: a prompt and its reply, in the shape `worker_session::replay` produces.
fn restored_transcript() -> Vec<(Option<SystemTime>, rpi_agent::AgentEvent)> {
    use rpi_agent::{AgentEvent, AgentMessage};
    use rpi_ai::{
        providers::faux::{FauxBlock, faux_assistant_message},
        types::{StopReason, UserContent, UserMessage},
    };

    // `replay` emits a user message as MessageStart (the prompt is what the person said, "before"
    // the run), and the assistant's MessageEnd is the reply.
    let prompt = AgentEvent::MessageStart {
        message: AgentMessage::User(UserMessage {
            role: Default::default(),
            content: UserContent::Text("what did we decide about the lock order?".into()),
            timestamp: 0,
        }),
    };
    let reply = AgentEvent::MessageEnd {
        message: AgentMessage::Assistant(Box::new(faux_assistant_message(
            FauxBlock::text("the loader claims the file before it replays it"),
            StopReason::Stop,
        ))),
    };
    let at = SystemTime::now();
    vec![(Some(at), prompt), (Some(at), reply)]
}

/// The acceptance criterion's first half: after a switch the person sees the resumed
/// conversation — not the one they left, and not an empty screen — and the per-session display
/// state the old conversation owned is gone with it (discovery §6.3).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_resume_shows_the_resumed_transcript_and_resets_the_per_session_state() {
    let mut rig = rig(Control::with_transcript(restored_transcript())).await;
    // Talk to *this* session first, so there is something to clear: a prompt in history and a
    // reply in the transcript that the resumed conversation never had.
    rig.say("a prompt only the old session saw").await;
    assert!(
        rig.shell
            .history()
            .iter()
            .any(|p| p.contains("old session")),
        "the prompt is in this chat's history: {:?}",
        rig.shell.history()
    );
    let old = rig.transcript();
    assert!(old.contains("a prompt only the old session saw"), "{old}");

    // Resume. The picker's confirm is Enter on the list then Enter on the row.
    rig.say("/sessions").await;
    rig.key(KeyCode::Enter).await;
    rig.key(KeyCode::Enter).await;
    assert_eq!(rig.control.resumed().len(), 1);

    let screen = rig.transcript();
    // The resumed conversation is what is shown, with its own prompt and reply.
    assert!(
        screen.contains("what did we decide about the lock order?"),
        "the resumed prompt is in the transcript: {screen}"
    );
    assert!(
        screen.contains("the loader claims the file before it replays it"),
        "and its reply: {screen}"
    );
    // The old conversation is not, and neither is the resumed line's own incantation of the old id.
    assert!(
        !screen.contains("a prompt only the old session saw"),
        "the old conversation was replaced, not appended to: {screen}"
    );
    // The resumed line still says where the continuation went.
    assert!(screen.contains("✓ resumed 8f3d…a1c2"), "{screen}");

    // Per-session display state: history reset (recalling would re-send a prompt into a
    // conversation that never saw it)…
    assert!(
        rig.shell.history().is_empty(),
        "prompt history was reset: {:?}",
        rig.shell.history()
    );
    // …and the totals start empty: the old session's spend is not this one's.
    assert_eq!(rig.shell.totals().turns, 0, "run numbering restarted");
    assert_eq!(rig.shell.totals().tool_calls, 0);
}

/// A switch that fails leaves the conversation exactly as it was — same transcript, same history,
/// same session — and says why. This is the "busy/error paths preserve old session" criterion at
/// the shell's level; the harness-level halves are in `session_ownership.rs`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_failed_switch_leaves_the_conversation_and_its_history_untouched() {
    let mut rig = rig(Control::new(Some(
        "its transcript could not be read (permission denied); the session already open is \
         unchanged and nothing was written"
            .to_owned(),
    )))
    .await;
    rig.say("a prompt that must survive a failed switch").await;
    let before = rig.transcript();

    rig.say("/sessions").await;
    rig.key(KeyCode::Enter).await;
    rig.key(KeyCode::Enter).await;
    assert_eq!(rig.control.resumed().len(), 1, "it was asked");

    let after = rig.transcript();
    assert!(
        after.contains("a prompt that must survive a failed switch"),
        "the old conversation is still shown: {after}"
    );
    assert!(
        after.contains("its transcript could not be read"),
        "and the reason is given: {after}"
    );
    assert!(
        rig.shell
            .history()
            .iter()
            .any(|p| p.contains("must survive a failed switch")),
        "history was not reset by a failed switch: {:?}",
        rig.shell.history()
    );
    assert!(
        !before.contains("its transcript could not be read"),
        "the failure line is new, not something that was already there"
    );
}

/// The refusals, one per state, each naming what to do next and cancelling nothing.
///
/// There are two gates and they say different things, deliberately: `/sessions` does not even
/// *open* while the chat is not at rest (the picker's own guard, #3010), and the transaction
/// itself re-checks every state before a switch is asked for (`Shell::switch_blocker`, #3012) —
/// the second gate is what covers a state that appeared while the picker was open.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_busy_chat_refuses_the_picker_and_says_why() {
    // A slow reply, so a prompt can be queued behind it while it streams.
    let script = FauxScript::new().with_tokens_per_second(20.0);
    let words: Vec<String> = (0..400).map(|i| format!("word{i}")).collect();
    script.set_responses(vec![FauxStep::text(words.join(" "))]);
    let mut rig = rig_with(Control::new(None), script).await;

    assert_eq!(rig.shell.submit("go").await, Submit::Continue);
    assert!(rig.shell.is_running());
    // Typed while it runs: queued as a later turn (AMP #2889), not refused.
    assert_eq!(rig.shell.submit("sent later").await, Submit::Continue);
    assert_eq!(
        rig.shell.queued_prompts(),
        vec!["sent later".to_owned()],
        "it is queued here"
    );

    // `/sessions` while replying: no scan, no picker, and a line saying when to try again.
    assert_eq!(rig.shell.submit("/sessions").await, Submit::Continue);
    for _ in 0..20 {
        tokio::time::sleep(Duration::from_millis(5)).await;
        rig.shell.poll();
    }
    assert!(rig.control.listed().is_empty(), "no scan while busy");
    assert!(!rig.shell.view().panel().is_open(), "no picker while busy");
    let flat = rig
        .screen()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    assert!(
        flat.contains("/sessions is unavailable: the manager is replying"),
        "{}",
        rig.screen()
    );
    assert!(flat.contains("Your chat is unchanged"), "{}", rig.screen());
    assert!(
        rig.shell.is_running(),
        "nothing was cancelled to make room for a switch"
    );
    assert_eq!(
        rig.shell.queued_prompts(),
        vec!["sent later".to_owned()],
        "and the queued prompt is still queued"
    );
}
