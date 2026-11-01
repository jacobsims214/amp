//! #2348: ↑↓ in the editor recall the prompts sent earlier in the chat. Real rpi
//! `AgentHarness` on the faux provider; keys go through `Shell::key` and then the editor, the way
//! the terminal loop sends them; no terminal.

use std::{sync::Arc, time::Duration};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{KEYS, ModelChoice, Shell, ShellInfo, Submit};
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
    /// What the editor submitted (its on_submit callback), as the terminal loop would get it.
    submitted: Arc<std::sync::Mutex<Vec<String>>>,
}

async fn rig() -> Rig {
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
    let mut alt = model.clone();
    alt.id = "faux-2".into();
    let models = vec![
        ModelChoice {
            reference: format!("{}/{}", model.provider, model.id),
            model: model.clone(),
        },
        ModelChoice {
            reference: format!("{}/faux-2", model.provider),
            model: alt,
        },
    ];
    let (shell, done) = Shell::new(Arc::new(harness), info, models);
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
    }
}

fn key(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::NONE)
}

impl Rig {
    /// One key as the terminal loop sends it: the shell first, then the editor.
    async fn press(&mut self, code: KeyCode) {
        let editor = self.shell.view().editor().clone();
        let empty = editor.get_text().is_empty();
        let k = key(code);
        if !self.shell.key(&k, empty).await {
            editor.handle_key(k);
        }
    }

    async fn type_text(&mut self, text: &str) {
        for c in text.chars() {
            self.press(KeyCode::Char(c)).await;
        }
    }

    fn text(&self) -> String {
        self.shell.view().editor().get_text()
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(W, H).join("\n")
    }

    async fn finish(&mut self) {
        self.shell
            .drive_until_idle(&mut self.events, &mut self.done, Duration::from_secs(20))
            .await
            .expect("idle");
    }
}

impl Rig {
    async fn press_mod(&mut self, code: KeyCode, mods: KeyModifiers) {
        let editor = self.shell.view().editor().clone();
        let empty = editor.get_text().is_empty();
        let k = KeyEvent::new(code, mods);
        if !self.shell.key(&k, empty).await {
            editor.handle_key(k);
        }
    }

    /// Enter, then what the terminal loop does with the submission (submit, clear the editor).
    async fn send(&mut self, text: &str) {
        self.type_text(text).await;
        self.press(KeyCode::Enter).await;
        let sent = self.submitted.lock().unwrap().pop().expect("submitted");
        assert_eq!(sent, text);
        assert_eq!(self.shell.submit(&sent).await, Submit::Continue);
        self.shell.view().editor().clear();
        self.finish().await;
    }

    fn cursor(&self) -> (usize, usize) {
        self.shell.view().editor().cursor_position()
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn up_recalls_earlier_prompts_and_down_comes_back_to_the_draft() {
    let mut rig = rig().await;
    // Nothing sent yet: ↑ does nothing.
    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "");

    rig.send("first message").await;
    rig.send("second message").await;
    assert_eq!(rig.shell.history(), ["first message", "second message"]);
    let _ = rig.screen();

    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "second message");
    assert_eq!(
        rig.cursor(),
        (0, "second message".len()),
        "cursor at the end"
    );
    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "first message");
    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "first message", "the oldest stays");
    rig.press(KeyCode::Down).await;
    assert_eq!(rig.text(), "second message");
    rig.press(KeyCode::Down).await;
    assert_eq!(rig.text(), "", "past the newest: the empty draft");
    rig.press(KeyCode::Down).await;
    assert_eq!(rig.text(), "");

    // A one-line draft comes back too.
    rig.type_text("half").await;
    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "second message");
    rig.press(KeyCode::Down).await;
    assert_eq!(rig.text(), "half");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn up_and_down_move_the_cursor_in_a_multi_line_draft() {
    let mut rig = rig().await;
    rig.send("earlier").await;

    rig.type_text("line one").await;
    rig.press_mod(KeyCode::Enter, KeyModifiers::SHIFT).await;
    rig.type_text("line two").await;
    assert_eq!(rig.text(), "line one\nline two");
    assert_eq!(rig.cursor().0, 1);
    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "line one\nline two", "the draft stays");
    assert_eq!(rig.cursor().0, 0, "↑ moved the cursor");
    rig.press(KeyCode::Up).await;
    assert_eq!(
        rig.text(),
        "line one\nline two",
        "first row of a multi-line draft: still the editor's"
    );
    rig.press(KeyCode::Down).await;
    assert_eq!(rig.cursor().0, 1, "↓ moved the cursor");
    assert_eq!(rig.text(), "line one\nline two");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_slash_menu_keeps_its_arrows_and_recalled_commands_do_not_trap_up() {
    let mut rig = rig().await;
    rig.send("hello").await;

    // Typing `/` opens the menu: ↓ moves its selection, not the history.
    rig.type_text("/").await;
    assert!(rig.shell.view().menu().is_open());
    let first = rig.shell.view().menu().selected().expect("selected").name;
    rig.press(KeyCode::Down).await;
    let second = rig.shell.view().menu().selected().expect("selected").name;
    assert_ne!(first, second);
    assert_eq!(rig.text(), "/");
    rig.press(KeyCode::Up).await;
    assert_eq!(
        rig.shell.view().menu().selected().expect("selected").name,
        first
    );
    assert_eq!(rig.text(), "/");
    rig.shell.view().editor().clear();

    // A slash command is recorded; recalled, ↑ goes on past it although the menu shows.
    rig.send("/usage").await;
    assert_eq!(rig.shell.history(), ["hello", "/usage"]);
    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "/usage");
    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "hello");
    rig.press(KeyCode::Down).await;
    assert_eq!(rig.text(), "/usage");
    rig.press(KeyCode::Down).await;
    assert_eq!(rig.text(), "");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn panels_keep_their_arrows() {
    let mut rig = rig().await;
    rig.send("hello").await;
    // `/model` opens a picker panel; ↑ there is the panel's, the editor stays empty.
    rig.send("/model").await;
    assert!(rig.shell.view().panel().is_open());
    let selected = |rig: &Rig| {
        rig.shell
            .view()
            .render_plain(W, H)
            .into_iter()
            .find(|l| l.contains("› "))
            .unwrap_or_default()
    };
    let before = selected(&rig);
    rig.press(KeyCode::Down).await;
    assert_eq!(rig.text(), "");
    assert_ne!(selected(&rig), before, "↓ moved the picker");
    rig.press(KeyCode::Up).await;
    assert_eq!(rig.text(), "");
    assert_eq!(selected(&rig), before, "↑ moved it back");
}

#[test]
fn help_lists_the_history_keys() {
    assert!(
        KEYS.iter()
            .any(|(k, about)| *k == "↑/↓" && about.contains("earlier prompts")),
        "{KEYS:?}"
    );
}
