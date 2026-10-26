//! #2345: thinking sections open and close one at a time. A click on a thinking header (the
//! row `Shell::click` maps it to) opens or closes that section; Ctrl+T opens or closes the lowest
//! header in sight, so scrolling to an older one and pressing Ctrl+T opens that one; the
//! transcript doesn't jump when one opens or closes; `/config thinking` changes the mode for the
//! sections already shown. Real rpi `AgentHarness` on the faux provider; no terminal.

use std::{sync::Arc, time::Duration};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{Shell, ShellInfo, Submit};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxBlock, FauxProvider, FauxScript, FauxStep, faux_assistant_message},
    types::StopReason,
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

/// `n` numbered lines: `{tag} 01`, `{tag} 02`, …
fn lines(tag: &str, n: usize) -> String {
    (1..=n)
        .map(|i| format!("{tag} {i:02}"))
        .collect::<Vec<_>>()
        .join("\n\n")
}

fn step(thinking: String, text: String) -> FauxStep {
    FauxStep::message(faux_assistant_message(
        vec![FauxBlock::Thinking(thinking), FauxBlock::Text(text)],
        StopReason::Stop,
    ))
}

/// Two turns: the first answer is long enough to push its thinking header out of sight.
async fn rig() -> Rig {
    let script = FauxScript::new();
    script.set_responses(vec![
        step(lines("why-one", 4), lines("answer-one", 30)),
        step(lines("why-two", 40), "answer-two".into()),
    ]);
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
    shell.set_screen(W, H);
    let mut rig = Rig {
        shell,
        events,
        done,
    };
    for prompt in ["one", "two"] {
        assert_eq!(rig.shell.submit(prompt).await, Submit::Continue);
        rig.shell
            .drive_until_idle(&mut rig.events, &mut rig.done, WAIT)
            .await
            .expect("idle");
    }
    rig.screen();
    rig
}

impl Rig {
    fn rows(&self) -> Vec<String> {
        self.shell.view().render_plain(W, H)
    }

    fn screen(&self) -> String {
        self.rows().join("\n")
    }

    /// The screen rows holding a thinking header, with the header (left-trimmed).
    fn headers(&self) -> Vec<(usize, String)> {
        self.rows()
            .into_iter()
            .enumerate()
            .filter_map(|(i, r)| {
                let t = r.trim_start();
                (t.starts_with("▸ Thought for ") || t.starts_with("▾ Thought for "))
                    .then(|| (i, t.to_owned()))
            })
            .collect()
    }

    fn top(&self) -> usize {
        self.shell.view().scroll().top()
    }

    async fn page_up(&mut self) {
        let k = KeyEvent::new(KeyCode::PageUp, KeyModifiers::NONE);
        assert!(self.shell.key(&k, true).await);
        self.screen();
    }

    fn ctrl_t(&mut self) -> bool {
        let used = self.shell.toggle_thinking();
        self.screen();
        used
    }

    fn click(&mut self, row: usize) -> bool {
        let used = self.shell.click(W, H, row, 8);
        self.screen();
        used
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn collapsed_by_default_with_thought_for_and_the_hint_on_the_last() {
    let r = rig().await;
    let screen = r.screen();
    // The first turn's header is above the viewport; the last one is in sight with its hint.
    let headers = r.headers();
    assert_eq!(headers.len(), 1, "{screen}");
    assert!(
        headers[0].1.starts_with("▸ Thought for ")
            && headers[0].1.ends_with("s · Ctrl+T or click to open"),
        "{screen}"
    );
    assert!(!screen.contains("why-two"), "{screen}");
    assert!(screen.contains("answer-two"), "{screen}");
}

#[tokio::test(flavor = "multi_thread")]
async fn ctrl_t_opens_the_header_in_sight_and_keeps_it_on_screen() {
    let mut r = rig().await;
    assert!(r.shell.view().scroll().at_end());
    let (row, _) = r.headers()[0];
    // At the end: the last header opens. Its 40 rows would push it off the top, so the view
    // scrolls to it and it reads from its first line.
    assert!(r.ctrl_t());
    let screen = r.screen();
    let headers = r.headers();
    assert_eq!(headers.len(), 1, "{screen}");
    assert!(headers[0].1.starts_with("▾ Thought for "), "{screen}");
    let rows = r.rows();
    assert!(
        rows[headers[0].0 + 1].contains("why-two 01"),
        "the first line under the header:\n{screen}"
    );
    assert!(headers[0].0 <= row, "{screen}");
    // Pressed again it closes.
    assert!(r.ctrl_t());
    assert!(!r.screen().contains("why-two"), "{}", r.screen());

    // Scrolled up to the first turn: Ctrl+T opens that one, and nothing above it moves.
    while r.top() > 0 {
        r.page_up().await;
    }
    let before = r.headers();
    assert_eq!(before.len(), 1, "{}", r.screen());
    assert!(r.ctrl_t());
    let after = r.headers();
    assert_eq!(after[0].0, before[0].0, "{}", r.screen());
    assert!(after[0].1.starts_with("▾"), "{}", r.screen());
    assert_eq!(r.top(), 0);
    let screen = r.screen();
    assert!(
        screen.contains("why-one 01") && screen.contains("why-one 04"),
        "{screen}"
    );
    assert!(
        !screen.contains("why-two"),
        "only that one opened:\n{screen}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_click_on_a_header_opens_and_closes_that_section() {
    let mut r = rig().await;
    while r.top() > 0 {
        r.page_up().await;
    }
    let (row, header) = r.headers()[0].clone();
    assert!(header.starts_with("▸"), "{}", r.screen());
    let top = r.top();

    assert!(r.click(row));
    let rows = r.rows();
    assert!(
        rows[row].trim_start().starts_with("▾ Thought for "),
        "{}",
        r.screen()
    );
    assert!(rows[row + 1].contains("│ why-one 01"), "{}", r.screen());
    assert_eq!(r.top(), top, "the transcript doesn't move");

    assert!(r.click(row));
    assert!(r.rows()[row].trim_start().starts_with("▸ Thought for "));
    assert!(!r.screen().contains("why-one"), "{}", r.screen());

    // A click elsewhere in the transcript does nothing.
    let answer = r
        .rows()
        .iter()
        .position(|l| l.contains("answer-one 01"))
        .expect("an answer row");
    assert!(!r.click(answer));
    assert!(!r.screen().contains("why-one"));
}

#[tokio::test(flavor = "multi_thread")]
async fn config_thinking_changes_the_sections_already_shown() {
    let mut r = rig().await;
    assert_eq!(
        r.shell.submit("/config thinking full").await,
        Submit::Continue
    );
    let screen = r.screen();
    assert!(screen.contains("thinking: full for this chat"), "{screen}");
    while r.top() > 0 {
        r.page_up().await;
    }
    let screen = r.screen();
    assert!(
        screen.contains("▾ Thought for") && screen.contains("│ why-one 01"),
        "{screen}"
    );

    // Without a value it moves on: full -> hidden -> collapsed.
    r.shell.submit("/config thinking").await;
    r.shell.view().scroll().to_end();
    let screen = r.screen();
    assert!(
        screen.contains("thinking: hidden for this chat"),
        "{screen}"
    );
    assert!(!screen.contains("Thought for"), "{screen}");
    r.shell.submit("/config thinking").await;
    assert!(r.screen().contains("thinking: collapsed for this chat"));
    assert!(r.screen().contains("▸ Thought for"), "{}", r.screen());

    r.shell.submit("/config thinking loud").await;
    assert!(
        r.screen().contains("unknown show_thinking `loud`"),
        "{}",
        r.screen()
    );
}
