//! #2185: the transcript scrolls from the keyboard. PageUp/PageDown move a page, Ctrl+U/Ctrl+D
//! half a page (empty editor), End jumps back to the latest output, and while it is scrolled up
//! new output shows a "↓ new output" pill on its last row. Keys go through `Shell::key` (and
//! `Shell::ctrl_d`) as terminal.rs sends them. Real rpi `AgentHarness` on a slow faux provider;
//! no terminal.

use std::{
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{CTRL_D_SETTLE, CtrlD, KEYS, NEW_OUTPUT, Shell, ShellInfo, Submit};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use tokio::sync::mpsc::UnboundedReceiver;

const W: usize = 100;
const H: usize = 30;
const WAIT: Duration = Duration::from_secs(20);
/// Transcript lines added by `fill`.
const LINES: usize = 120;

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
}

async fn rig() -> Rig {
    let words: Vec<String> = (0..600).map(|i| format!("word{i}")).collect();
    let script = FauxScript::new().with_tokens_per_second(40.0);
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
    let (shell, done) = Shell::new(Arc::new(harness), info, Vec::new());
    Rig {
        shell,
        events,
        done,
    }
}

fn key(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::NONE)
}

fn ctrl(c: char) -> KeyEvent {
    KeyEvent::new(KeyCode::Char(c), KeyModifiers::CONTROL)
}

impl Rig {
    /// `LINES` numbered notices, then a frame so the scroll view knows its sizes.
    fn fill(&self) {
        for n in 1..=LINES {
            self.shell.view().add_notice(&format!("line {n:03}"));
        }
        self.screen();
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(W, H).join("\n")
    }

    /// The numbered lines on screen, in order.
    fn shown(&self) -> Vec<usize> {
        self.screen()
            .lines()
            .filter_map(|l| l.trim().strip_prefix("line ")?.parse().ok())
            .collect()
    }

    /// Presses `k` the way terminal.rs does: `Shell::key` first, else the editor.
    async fn press(&mut self, k: KeyEvent) -> bool {
        let empty = self.shell.view().editor().get_text().is_empty();
        let used = self.shell.key(&k, empty).await;
        if !used {
            self.shell.view().editor().handle_key(k);
        }
        self.screen();
        used
    }

    fn at_end(&self) -> bool {
        self.shell.view().scroll().at_end()
    }

    fn top(&self) -> usize {
        self.shell.view().scroll().top()
    }

    fn pill_shown(&self) -> bool {
        self.screen().contains(NEW_OUTPUT.trim())
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn page_up_page_down_and_end_scroll_the_transcript() {
    let mut r = rig().await;
    r.fill();
    assert!(r.at_end());
    assert_eq!(r.shown().last(), Some(&LINES), "{}", r.screen());
    let bottom = r.top();
    let page = r.shell.view().scroll().page() as usize;
    assert!(page >= 5, "a page is most of the viewport: {page}");

    assert!(r.press(key(KeyCode::PageUp)).await);
    assert!(!r.at_end());
    assert_eq!(r.top(), bottom - page);
    assert!(!r.shown().contains(&LINES), "{}", r.screen());

    assert!(r.press(key(KeyCode::PageUp)).await);
    assert_eq!(r.top(), bottom - 2 * page);
    assert!(r.press(key(KeyCode::PageDown)).await);
    assert_eq!(r.top(), bottom - page);
    assert!(r.press(key(KeyCode::PageDown)).await);
    assert!(r.at_end(), "back at the bottom it follows again");
    assert_eq!(r.shown().last(), Some(&LINES));

    // End from anywhere.
    for _ in 0..5 {
        r.press(key(KeyCode::PageUp)).await;
    }
    assert!(!r.shown().contains(&LINES));
    assert!(r.press(key(KeyCode::End)).await);
    assert!(r.at_end());
    assert_eq!(r.shown().last(), Some(&LINES), "{}", r.screen());
    // PageUp at the very top stays at the top.
    for _ in 0..40 {
        r.press(key(KeyCode::PageUp)).await;
    }
    assert_eq!(r.top(), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn ctrl_u_and_ctrl_d_scroll_half_a_page_with_an_empty_editor() {
    let mut r = rig().await;
    r.fill();
    let bottom = r.top();
    let half = r.shell.view().scroll().half_page() as usize;

    assert!(r.press(ctrl('u')).await);
    assert_eq!(r.top(), bottom - half);
    assert!(r.press(ctrl('u')).await);
    assert_eq!(r.top(), bottom - 2 * half);

    let t0 = Instant::now();
    assert_eq!(r.shell.ctrl_d(t0).await, CtrlD::Scrolled);
    r.screen();
    assert_eq!(r.top(), bottom - half);
    assert_eq!(r.shell.ctrl_d(t0).await, CtrlD::Scrolled);
    r.screen();
    assert!(r.at_end());
    // Holding Ctrl+D (or pressing it again right away) stops at the bottom instead of quitting.
    assert_eq!(
        r.shell.ctrl_d(t0 + Duration::from_millis(300)).await,
        CtrlD::Held
    );
    assert_eq!(
        r.shell.ctrl_d(t0 + Duration::from_millis(900)).await,
        CtrlD::Held
    );
    // Left alone for CTRL_D_SETTLE, Ctrl+D at the bottom quits as before.
    let later = t0 + Duration::from_millis(900) + CTRL_D_SETTLE + Duration::from_millis(1);
    assert_eq!(r.shell.ctrl_d(later).await, CtrlD::Quit);
}

#[tokio::test(flavor = "multi_thread")]
async fn ctrl_d_at_the_bottom_quits_at_once_as_before() {
    let mut r = rig().await;
    r.fill();
    assert_eq!(r.shell.ctrl_d(Instant::now()).await, CtrlD::Quit);
}

#[tokio::test(flavor = "multi_thread")]
async fn with_text_in_the_editor_ctrl_u_and_end_are_the_editors() {
    let mut r = rig().await;
    r.fill();
    let bottom = r.top();
    r.shell.view().editor().set_text("hello world");
    assert!(
        !r.press(key(KeyCode::End)).await,
        "End moves the editor's cursor"
    );
    assert_eq!(r.top(), bottom);
    assert!(
        !r.press(ctrl('u')).await,
        "the editor deletes to the line start"
    );
    assert_eq!(r.top(), bottom);
    assert_eq!(r.shell.view().editor().get_text(), "");

    r.press(key(KeyCode::PageUp)).await;
    assert!(!r.at_end(), "PageUp scrolls even while typing");
    r.shell.view().editor().set_text("hi");
    assert!(
        !r.press(key(KeyCode::End)).await,
        "End moves the editor's cursor"
    );
    assert!(!r.at_end());
    let ctrl_end = KeyEvent::new(KeyCode::End, KeyModifiers::CONTROL);
    assert!(
        r.press(ctrl_end).await,
        "Ctrl+End always jumps to the latest"
    );
    assert!(r.at_end());
    assert_eq!(r.shell.view().editor().get_text(), "hi");
}

#[tokio::test(flavor = "multi_thread")]
async fn the_new_output_pill_shows_only_while_scrolled_up_with_output_below() {
    let mut r = rig().await;
    r.fill();
    assert!(!r.pill_shown());
    r.press(key(KeyCode::PageUp)).await;
    assert!(!r.pill_shown(), "nothing new yet:\n{}", r.screen());
    let top = r.top();

    r.shell.view().add_notice("line 999");
    assert!(r.pill_shown(), "{}", r.screen());
    assert_eq!(r.top(), top, "new output doesn't move a scrolled-up view");
    // On the transcript's last row, centred, above the editor.
    let screen = r.screen();
    let row = screen
        .lines()
        .position(|l| l.contains(NEW_OUTPUT.trim()))
        .unwrap();
    let editor_top = screen
        .lines()
        .position(|l| l.trim_start().starts_with('─'))
        .unwrap();
    assert!(row < editor_top, "{screen}");
    let styled = r.shell.view().render_frame(W, H);
    assert!(
        styled[row].contains("\x1b[7m"),
        "inverted: {:?}",
        styled[row]
    );

    // Scrolling down to the bottom by hand takes it away, as End does.
    for _ in 0..10 {
        r.press(key(KeyCode::PageDown)).await;
    }
    assert!(r.at_end());
    assert!(!r.pill_shown());
    r.press(key(KeyCode::PageUp)).await;
    r.shell.view().add_notice("line 998");
    assert!(r.pill_shown());
    r.press(key(KeyCode::End)).await;
    assert!(!r.pill_shown());
    assert!(r.screen().contains("line 998"));
}

#[tokio::test(flavor = "multi_thread")]
async fn a_streaming_reply_below_a_scrolled_up_view_shows_the_pill() {
    let mut r = rig().await;
    r.fill();
    assert_eq!(r.shell.submit("go").await, Submit::Continue);
    assert!(r.shell.is_running());
    // The prompt jumped to the end; scroll up while the reply streams.
    r.press(key(KeyCode::PageUp)).await;
    r.press(key(KeyCode::PageUp)).await;
    assert!(!r.at_end());
    let top = r.top();
    let deadline = Instant::now() + WAIT;
    while !r.pill_shown() {
        assert!(Instant::now() < deadline, "no pill\n{}", r.screen());
        if let Ok(Some(event)) =
            tokio::time::timeout(Duration::from_millis(200), r.events.recv()).await
        {
            r.shell.handle_event(&event);
        }
    }
    assert_eq!(
        r.top(),
        top,
        "streaming doesn't pull a scrolled-up view down"
    );
    assert!(!r.screen().contains("alpha"), "{}", r.screen());
    r.press(key(KeyCode::End)).await;
    assert!(r.at_end());
    assert!(!r.pill_shown());
    // Following again: the stream shows as it arrives.
    while !r.screen().contains("alpha") {
        assert!(
            Instant::now() < deadline,
            "the reply never showed\n{}",
            r.screen()
        );
        if let Ok(Some(event)) =
            tokio::time::timeout(Duration::from_millis(200), r.events.recv()).await
        {
            r.shell.handle_event(&event);
        }
    }
    assert!(r.at_end());
    assert!(!r.pill_shown());
    r.shell.abort().await;
    r.shell
        .drive_until_idle(&mut r.events, &mut r.done, WAIT)
        .await
        .expect("idle");
}

#[tokio::test(flavor = "multi_thread")]
async fn sending_a_prompt_jumps_back_to_the_latest_output() {
    let mut r = rig().await;
    r.fill();
    r.press(key(KeyCode::PageUp)).await;
    assert!(!r.at_end());
    assert_eq!(r.shell.submit("go").await, Submit::Continue);
    r.screen();
    assert!(r.at_end());
    // Stop it once it has started (an abort before the run starts is lost).
    let deadline = Instant::now() + WAIT;
    while !r.screen().contains("alpha") {
        assert!(Instant::now() < deadline, "the reply never started");
        if let Ok(Some(event)) =
            tokio::time::timeout(Duration::from_millis(200), r.events.recv()).await
        {
            r.shell.handle_event(&event);
        }
    }
    assert!(r.at_end(), "following the reply");
    r.shell.abort().await;
    r.shell
        .drive_until_idle(&mut r.events, &mut r.done, WAIT)
        .await
        .expect("idle");
}

#[test]
fn help_lists_the_scroll_keys_and_selection() {
    let keys: Vec<&str> = KEYS.iter().map(|(k, _)| *k).collect();
    for want in ["PgUp/PgDn", "Ctrl+U/D", "End"] {
        assert!(keys.contains(&want), "{keys:?}");
    }
    assert!(
        KEYS.iter()
            .any(|(_, about)| about.contains("Terminal.app") && about.contains("iTerm2")),
        "{KEYS:?}"
    );
}
