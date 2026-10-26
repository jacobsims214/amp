//! #2349: new lines without the kitty keyboard protocol. Keys go the way the terminal loop sends
//! them: the reader's [`amp_tui::terminal::read_key`], then `Shell::key`, then
//! `Shell::editor_key`. Real rpi `AgentHarness` on the faux provider; no terminal. (crossterm's
//! own decoding of `CSI 27;2;13~` is tested in vendor/crossterm, and end to end in a pty.)

use std::{sync::Arc, time::Duration};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{
    LEGACY_PLACEHOLDER, LEGACY_SHIFT_ENTER, NewlineMode, PLACEHOLDER, Shell, ShellInfo, Submit,
    terminal::read_key,
};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use tokio::sync::mpsc::UnboundedReceiver;

const W: usize = 100;
const H: usize = 40;

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
    submitted: Arc<std::sync::Mutex<Vec<String>>>,
    kitty: bool,
}

async fn rig(mode: NewlineMode) -> Rig {
    let script = FauxScript::new();
    script.set_responses(vec![FauxStep::text("ok"), FauxStep::text("ok again")]);
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
        git_branch: Some("harness-m4".into()),
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, done) = Shell::new(Arc::new(harness), info, Vec::new());
    shell.set_newline_mode(mode);
    let submitted = Arc::new(std::sync::Mutex::new(Vec::new()));
    let sink = submitted.clone();
    shell.view().editor().on_submit(Arc::new(move |text: &str| {
        sink.lock().unwrap().push(text.to_owned())
    }));
    Rig {
        shell,
        events,
        done,
        submitted,
        kitty: mode == NewlineMode::Kitty,
    }
}

impl Rig {
    /// One key as the terminal loop handles it.
    async fn press_mod(&mut self, code: KeyCode, mods: KeyModifiers) {
        let key = read_key(KeyEvent::new(code, mods), self.kitty);
        let empty = self.shell.view().editor().get_text().is_empty();
        if !self.shell.key(&key, empty).await {
            self.shell.editor_key(key);
        }
    }

    async fn press(&mut self, code: KeyCode) {
        self.press_mod(code, KeyModifiers::NONE).await;
    }

    async fn type_text(&mut self, text: &str) {
        for c in text.chars() {
            self.press(KeyCode::Char(c)).await;
        }
    }

    fn text(&self) -> String {
        self.shell.view().editor().get_text()
    }

    fn sent(&self) -> Vec<String> {
        self.submitted.lock().unwrap().clone()
    }

    fn screen(&self, width: usize) -> String {
        self.shell.view().render_plain(width, H).join("\n")
    }

    /// What the terminal loop does with a submission, then waits for the reply.
    async fn submit_sent(&mut self) {
        let sent = self.submitted.lock().unwrap().pop().expect("submitted");
        assert_eq!(self.shell.submit(&sent).await, Submit::Continue);
        self.shell.view().editor().clear();
        self.shell
            .drive_until_idle(&mut self.events, &mut self.done, Duration::from_secs(20))
            .await
            .expect("idle");
    }
}

/// The ticket's repro: `aaa`, Shift+Enter (xterm's `CSI 27;2;13~`, which the patched crossterm
/// reads as Shift+Enter), `bbb`, Alt+Enter (`ESC CR`: Enter with Alt), `ccc`, Ctrl+J, `ddd`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn legacy_keys_each_start_a_new_line_and_send_nothing() {
    let mut rig = rig(NewlineMode::Legacy).await;
    rig.type_text("aaa").await;
    rig.press_mod(KeyCode::Enter, KeyModifiers::SHIFT).await;
    rig.type_text("bbb").await;
    rig.press_mod(KeyCode::Enter, KeyModifiers::ALT).await;
    rig.type_text("ccc").await;
    rig.press_mod(KeyCode::Char('j'), KeyModifiers::CONTROL)
        .await;
    rig.type_text("ddd").await;
    assert_eq!(rig.text(), "aaa\nbbb\nccc\nddd");
    assert!(rig.sent().is_empty(), "{:?}", rig.sent());

    // Enter still sends the whole thing.
    rig.press(KeyCode::Enter).await;
    assert_eq!(rig.sent(), vec!["aaa\nbbb\nccc\nddd".to_owned()]);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn backslash_enter_starts_a_new_line_in_every_mode() {
    for mode in [
        NewlineMode::Legacy,
        NewlineMode::Kitty,
        NewlineMode::AppleTerminal,
    ] {
        let mut rig = rig(mode).await;
        rig.type_text("first\\").await;
        rig.press(KeyCode::Enter).await;
        assert_eq!(rig.text(), "first\n", "{mode:?}");
        assert!(rig.sent().is_empty(), "{mode:?}: {:?}", rig.sent());
        rig.type_text("second").await;
        rig.press(KeyCode::Enter).await;
        assert_eq!(rig.sent(), vec!["first\nsecond".to_owned()], "{mode:?}");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn kitty_keys_are_unchanged() {
    let mut rig = rig(NewlineMode::Kitty).await;
    rig.type_text("one").await;
    rig.press_mod(KeyCode::Enter, KeyModifiers::SHIFT).await;
    rig.type_text("two").await;
    rig.press_mod(KeyCode::Char('j'), KeyModifiers::CONTROL)
        .await;
    rig.type_text("three").await;
    assert_eq!(rig.text(), "one\ntwo\nthree");
    rig.press(KeyCode::Enter).await;
    assert_eq!(rig.sent(), vec!["one\ntwo\nthree".to_owned()]);
    // The empty editor's hint is the usual one.
    assert_eq!(rig.shell.view().placeholder(), PLACEHOLDER);
}

/// #2348 still holds for drafts made with the legacy keys: ↑ in a multi-line draft moves the
/// cursor, and in an empty editor it recalls the last prompt (with its new lines).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn history_keeps_working_with_legacy_new_lines() {
    let mut rig = rig(NewlineMode::Legacy).await;
    rig.type_text("a").await;
    rig.press_mod(KeyCode::Enter, KeyModifiers::ALT).await;
    rig.type_text("b").await;
    rig.press(KeyCode::Enter).await;
    rig.submit_sent().await;
    assert_eq!(rig.text(), "");

    rig.type_text("x").await;
    rig.press_mod(KeyCode::Char('j'), KeyModifiers::CONTROL)
        .await;
    rig.type_text("y").await;
    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "x\ny", "↑ moved the cursor, no recall");
    assert_eq!(rig.shell.view().editor().cursor_position().0, 0);

    rig.shell.view().editor().clear();
    rig.press(KeyCode::Up).await;
    assert_eq!(
        rig.text(),
        "a\nb",
        "↑ in the empty editor recalls the prompt"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_legacy_hint_names_ctrl_j_and_falls_back_when_narrow() {
    let rig = rig(NewlineMode::Legacy).await;
    let screen = rig.screen(W);
    assert!(screen.contains(LEGACY_PLACEHOLDER), "{screen}");
    // Too narrow for the long hint: the short one.
    let narrow = rig.screen(30);
    assert!(!narrow.contains(LEGACY_PLACEHOLDER), "{narrow}");
    assert!(narrow.contains(PLACEHOLDER), "{narrow}");

    // Terminal.app's Shift+Enter works (rpi-tui asks macOS), so its hint is the usual one.
    let apple = rig_screen(NewlineMode::AppleTerminal).await;
    assert!(!apple.contains(LEGACY_PLACEHOLDER), "{apple}");
    assert!(apple.contains(PLACEHOLDER), "{apple}");
}

async fn rig_screen(mode: NewlineMode) -> String {
    rig(mode).await.screen(W)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn help_says_shift_enter_sends_without_the_kitty_protocol() {
    let mut legacy = rig(NewlineMode::Legacy).await;
    assert_eq!(legacy.shell.submit("/help").await, Submit::Continue);
    let screen = legacy.screen(W);
    assert!(
        screen.contains("no kitty keyboard protocol here"),
        "{screen}"
    );
    assert!(screen.contains("Ctrl+J"), "{screen}");
    assert!(screen.contains("Alt+Enter"), "{screen}");
    assert!(screen.contains("\\ Enter"), "{screen}");

    let mut kitty = rig(NewlineMode::Kitty).await;
    assert_eq!(kitty.shell.submit("/help").await, Submit::Continue);
    let screen = kitty.screen(W);
    assert!(!screen.contains(LEGACY_SHIFT_ENTER.1), "{screen}");
    assert!(screen.contains("Ctrl+J"), "{screen}");
}
