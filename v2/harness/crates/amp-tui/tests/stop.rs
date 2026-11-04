//! #2181: stopping a reply. Esc during a run stops it, and the transcript gets one dim
//! "Stopped." line (no error colour, no emoji) below what the reply had streamed. Esc that
//! closes an AMP panel during a run doesn't stop it: the spinner line says "Esc again to stop
//! the reply" for a moment, and the next Esc stops it. Real rpi `AgentHarness`, a slow faux
//! provider, a stub metadata handle for the panel; no terminal.

use std::{
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::{agent::memory_session, events::ChannelEmitter, meta::MetaHandle};
use amp_tui::{ESC_HINT, ESC_HINT_FOR, STOPPED, Shell, ShellInfo, Submit, chrome::sample};
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
const WAIT: Duration = Duration::from_secs(20);

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
}

/// A reply that streams slowly (a few words a second), so a test can stop it part way.
async fn rig() -> Rig {
    let words: Vec<String> = (0..400).map(|i| format!("word{i}")).collect();
    let script = FauxScript::new().with_tokens_per_second(20.0);
    script.set_responses(vec![FauxStep::text(format!("alpha {}", words.join(" ")))]);
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
    shell.set_meta(MetaHandle::stub(
        sample::meta(true),
        Arc::new(|tool, _| Err(format!("{tool}: not in the stub"))),
    ));
    Rig {
        shell,
        events,
        done,
    }
}

fn esc() -> KeyEvent {
    KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE)
}

impl Rig {
    /// Sends the prompt and feeds events until the reply has started streaming.
    async fn start(&mut self) {
        assert_eq!(self.shell.submit("go").await, Submit::Continue);
        assert!(self.shell.is_running());
        let deadline = Instant::now() + WAIT;
        while !self.screen().contains("alpha") {
            assert!(Instant::now() < deadline, "the reply never started");
            if let Ok(Some(event)) =
                tokio::time::timeout(Duration::from_millis(200), self.events.recv()).await
            {
                self.shell.handle_event(&event);
            }
        }
    }

    async fn finish(&mut self) {
        self.shell
            .drive_until_idle(&mut self.events, &mut self.done, WAIT)
            .await
            .expect("idle");
    }

    /// The screen, the boxes' side edges as blanks (#2882).
    fn screen(&self) -> String {
        self.shell
            .view()
            .render_plain(W, H)
            .iter()
            .map(|l| l.replace('│', " ").trim_end().to_owned())
            .collect::<Vec<_>>()
            .join("\n")
    }

    /// The styled frame line that holds `text`.
    fn styled_line(&self, text: &str) -> String {
        self.shell
            .view()
            .render_frame(W, H)
            .into_iter()
            .find(|l| rpi_tui::strip_ansi(l).contains(text))
            .unwrap_or_else(|| panic!("{text:?} not on screen\n{}", self.screen()))
    }
}

/// The transcript says the run stopped exactly once, dim, with no error styling or wording,
/// and the partial reply is kept.
fn assert_stopped_once(rig: &Rig) {
    let screen = rig.screen();
    assert!(!rig.shell.is_running());
    assert_eq!(
        screen.lines().filter(|l| l.contains("Stopped")).count(),
        1,
        "{screen}"
    );
    assert!(
        screen.lines().any(|l| l.trim() == STOPPED),
        "a line of its own\n{screen}"
    );
    let lower = screen.to_lowercase();
    for unwanted in ["aborted", "aborting", "❌", "error:", "run failed"] {
        assert!(!lower.contains(unwanted), "{unwanted:?}\n{screen}");
    }
    assert!(
        screen.contains("alpha"),
        "the partial reply is kept\n{screen}"
    );
    let colors = rpi_tui::theme().colors;
    let line = rig.styled_line(STOPPED);
    assert!(line.contains(&colors.dim.fg(STOPPED)), "dim: {line:?}");
    assert!(
        !line.contains(&colors.error.to_fg()),
        "not error-coloured: {line:?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn esc_stops_the_reply_with_one_dim_stopped_line() {
    let mut rig = rig().await;
    rig.start().await;
    assert!(
        rig.shell.key(&esc(), true).await,
        "Esc during a run is used"
    );
    rig.finish().await;
    assert_stopped_once(&rig);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn abort_command_says_stopped_once_too() {
    let mut rig = rig().await;
    rig.start().await;
    assert_eq!(rig.shell.submit("/abort").await, Submit::Continue);
    rig.finish().await;
    assert_stopped_once(&rig);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn esc_closing_a_panel_mid_run_hints_and_the_next_esc_stops() {
    let mut rig = rig().await;
    rig.start().await;
    assert_eq!(rig.shell.submit("/amp").await, Submit::Continue);
    assert!(rig.shell.view().panel().is_open());

    // The first Esc closes the panel, the run goes on, and the spinner line hints.
    assert!(rig.shell.key(&esc(), true).await);
    assert!(!rig.shell.view().panel().is_open());
    assert!(
        rig.shell.is_running(),
        "closing the panel doesn't stop the run"
    );
    assert_eq!(
        rig.shell.view().working_message().as_deref(),
        Some(ESC_HINT)
    );
    assert!(rig.screen().contains(ESC_HINT), "{}", rig.screen());
    assert!(!rig.screen().contains("Stopped"));

    // The hint goes after ESC_HINT_FOR (the tick calls this through Shell::poll).
    assert!(!rig.shell.expire_hints(Instant::now()));
    assert!(rig.shell.expire_hints(Instant::now() + ESC_HINT_FOR));
    assert_eq!(
        rig.shell.view().working_message().as_deref(),
        Some(amp_tui::view::REPLYING)
    );

    // Open it and close it again: the hint is back; the second Esc stops the reply.
    assert_eq!(rig.shell.submit("/amp").await, Submit::Continue);
    assert!(rig.shell.key(&esc(), true).await);
    assert_eq!(
        rig.shell.view().working_message().as_deref(),
        Some(ESC_HINT)
    );
    assert!(rig.shell.key(&esc(), true).await);
    rig.finish().await;
    assert_stopped_once(&rig);
    assert!(rig.shell.view().working_message().is_none());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn esc_with_nothing_running_and_no_panel_goes_to_the_editor() {
    let mut rig = rig().await;
    assert!(!rig.shell.key(&esc(), true).await);
    assert!(!rig.screen().contains("Stopped"));
}
