//! #2184: Ctrl+C on the empty editor doesn't quit at once. The first one shows "Press Ctrl+C
//! again to quit" in the editor for 1.5 s; a second one inside that window quits. Ctrl+C still
//! stops a running reply and clears a non-empty editor first. Real rpi `AgentHarness` on a slow
//! faux provider; no terminal.

use std::{
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{CtrlC, PLACEHOLDER, QUIT_HINT, QUIT_HINT_FOR, Shell, ShellInfo, Submit};
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

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
}

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
    let (shell, done) = Shell::new(Arc::new(harness), info, Vec::new());
    Rig {
        shell,
        events,
        done,
    }
}

impl Rig {
    fn screen(&self) -> String {
        self.shell.view().render_plain(W, H).join("\n")
    }

    fn hint_shown(&self) -> bool {
        self.screen().contains(QUIT_HINT)
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_second_ctrl_c_inside_the_window_quits() {
    let mut r = rig().await;
    let t0 = Instant::now();
    assert!(r.screen().contains(PLACEHOLDER));
    assert_eq!(r.shell.ctrl_c(t0).await, CtrlC::Hint);
    assert!(r.hint_shown(), "the hint is in the editor\n{}", r.screen());
    assert!(!r.screen().contains(PLACEHOLDER));
    assert_eq!(
        r.shell.ctrl_c(t0 + Duration::from_millis(900)).await,
        CtrlC::Quit
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn the_hint_expires_and_a_late_ctrl_c_only_hints_again() {
    let mut r = rig().await;
    let t0 = Instant::now();
    assert_eq!(r.shell.ctrl_c(t0).await, CtrlC::Hint);
    // Still inside the window: nothing changes yet.
    assert!(
        !r.shell
            .expire_hints(t0 + QUIT_HINT_FOR - Duration::from_millis(10))
    );
    assert!(r.hint_shown());
    // Past it: the placeholder is back.
    assert!(r.shell.expire_hints(t0 + QUIT_HINT_FOR));
    assert!(!r.hint_shown());
    assert!(r.screen().contains(PLACEHOLDER));
    assert_eq!(r.shell.view().editor_hint(), None);
    // A Ctrl+C after the window (even without a tick in between) starts over.
    let late = t0 + QUIT_HINT_FOR + Duration::from_millis(100);
    assert_eq!(r.shell.ctrl_c(late).await, CtrlC::Hint);
    let mut r2 = rig().await;
    assert_eq!(r2.shell.ctrl_c(t0).await, CtrlC::Hint);
    assert_eq!(
        r2.shell.ctrl_c(t0 + QUIT_HINT_FOR).await,
        CtrlC::Hint,
        "the window is 1.5 s"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn another_key_between_the_two_cancels_the_quit() {
    let mut r = rig().await;
    let t0 = Instant::now();
    assert_eq!(r.shell.ctrl_c(t0).await, CtrlC::Hint);
    r.shell.clear_quit_hint();
    assert!(!r.hint_shown());
    assert_eq!(
        r.shell.ctrl_c(t0 + Duration::from_millis(200)).await,
        CtrlC::Hint
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn ctrl_c_clears_a_non_empty_editor_before_hinting() {
    let mut r = rig().await;
    let t0 = Instant::now();
    r.shell.view().editor().set_text("half a thought");
    assert_eq!(r.shell.ctrl_c(t0).await, CtrlC::Cleared);
    assert!(r.shell.view().editor().get_text().is_empty());
    assert!(!r.hint_shown());
    assert_eq!(
        r.shell.ctrl_c(t0 + Duration::from_millis(100)).await,
        CtrlC::Hint
    );
    assert_eq!(
        r.shell.ctrl_c(t0 + Duration::from_millis(200)).await,
        CtrlC::Quit
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn ctrl_c_during_a_reply_stops_it_and_does_not_count_toward_quitting() {
    let mut r = rig().await;
    assert_eq!(r.shell.submit("go").await, Submit::Continue);
    assert!(r.shell.is_running());
    let deadline = Instant::now() + WAIT;
    while !r.screen().contains("alpha") {
        assert!(Instant::now() < deadline, "the reply never started");
        if let Ok(Some(event)) =
            tokio::time::timeout(Duration::from_millis(200), r.events.recv()).await
        {
            r.shell.handle_event(&event);
        }
    }
    let t0 = Instant::now();
    assert_eq!(r.shell.ctrl_c(t0).await, CtrlC::Stopped);
    r.shell
        .drive_until_idle(&mut r.events, &mut r.done, WAIT)
        .await
        .expect("idle");
    assert!(!r.shell.is_running());
    // The next Ctrl+C on the empty editor only hints.
    assert_eq!(
        r.shell.ctrl_c(t0 + Duration::from_millis(300)).await,
        CtrlC::Hint
    );
}
