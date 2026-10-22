//! M3-4 (#2108): the AMP slash commands, their panels, answering a question (confirmed before
//! anything is sent), `/open`'s URL guard, `amp_*` tool titles and the ticket marker. The shell
//! runs on a real rpi `AgentHarness` (faux provider) with a stub metadata handle
//! (`MetaHandle::stub`) holding the sample snapshot, so no AMP is needed; the poller's own call
//! path is tested against a mock AMP in amp-core's tests/meta.rs.

use std::{
    sync::{Arc, Mutex},
    time::Duration,
};

use amp_core::{agent::memory_session, events::ChannelEmitter, meta::MetaHandle};
use amp_tui::{
    PanelKind, Shell, ShellInfo, Stage, Submit,
    chrome::{plain, sample},
};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use rpi_tui::visible_width;
use serde_json::{Value, json};
use tokio::sync::mpsc::UnboundedReceiver;

const W: usize = 110;
const H: usize = 40;
const WAIT: Duration = Duration::from_secs(20);

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
    meta: MetaHandle,
    opened: Arc<Mutex<Vec<String>>>,
}

fn plan_json() -> Value {
    json!({"project_id": 2, "epics": [
        {"id": 150, "name": "Earlier", "tickets": {"completed": 12}, "stories": []},
        {"id": 161, "name": "AMP Harness", "tickets": {"in_progress": 2, "backlog": 3, "blocked": 3, "completed": 9},
         "stories": [{"id": 393, "name": "M3 theme + metadata", "tickets": {"in_progress": 2, "backlog": 1, "blocked": 3}}]}
    ]})
}

/// `amp_list_tasks(owner=any, state=blocked, format=full)`: story 393 has a question, a waiting
/// ticket and a scheduled one (AMP stores all three as `blocked`).
fn blocked_json() -> Value {
    let t = |id: i64, reason: Option<&str>| json!({"id": id, "epic_id": 161, "story_id": 393, "state": "blocked", "block_reason": reason});
    json!({
        "ready_to_dispatch": [], "in_progress": [], "completed": [],
        "blocked": [t(2107, Some("Dark or light?")), t(2111, None)],
        "scheduled": [t(2112, Some("scheduled: 2026-11-01"))],
        "questions": [t(2107, Some("Dark or light?"))]
    })
}

async fn rig(steps: Vec<FauxStep>, with_meta: bool) -> Rig {
    let script = FauxScript::new();
    script.set_responses(steps);
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
        git_branch: Some("harness-m3".into()),
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, done) = Shell::new(Arc::new(harness), info, Vec::new());
    let opened: Arc<Mutex<Vec<String>>> = Arc::default();
    let log = Arc::clone(&opened);
    shell.set_opener(Arc::new(move |url: &str| {
        log.lock().unwrap().push(url.to_owned());
        Ok(())
    }));
    let meta = MetaHandle::stub(
        sample::meta(true),
        Arc::new(|tool, args| match tool {
            "amp_get_plan" => Ok(plan_json()),
            "amp_list_tasks" if args["format"] == "full" && args["state"] == "blocked" => {
                Ok(blocked_json())
            }
            "amp_answer_question" if args["task_id"] == 2110 => Err(
                "tools/call amp_answer_question: Mcp error: -32603: task 2110 is not blocked"
                    .into(),
            ),
            "amp_answer_question" => Ok(json!({"ok": true, "task_id": args["task_id"]})),
            other => Err(format!("{other}: not in the stub")),
        }),
    );
    if with_meta {
        shell.set_meta(meta.clone());
        let workers = sample::data(true).workers;
        shell
            .view()
            .chrome()
            .set_workers(Arc::new(move || workers.clone()));
    }
    Rig {
        shell,
        events,
        done,
        meta,
        opened,
    }
}

fn key(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::NONE)
}

impl Rig {
    async fn say(&mut self, text: &str) {
        assert_eq!(self.shell.submit(text).await, Submit::Continue);
        self.shell
            .drive_until_idle(&mut self.events, &mut self.done, WAIT)
            .await
            .expect("idle");
        self.settle().await;
    }

    async fn settle(&mut self) {
        // Stub commands (set_ticket, ...) land on the stub's task; give it a moment, bounded.
        for _ in 0..20 {
            tokio::time::sleep(Duration::from_millis(5)).await;
            self.shell.poll();
        }
        self.shell.settle(WAIT).await.expect("settled");
    }

    async fn press(&mut self, code: KeyCode) -> bool {
        let used = self.shell.panel_key(&key(code), true);
        self.settle().await;
        used
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(W, H).join("\n")
    }

    fn opened(&self) -> Vec<String> {
        self.opened.lock().unwrap().clone()
    }
}

fn assert_fits(rig: &Rig) {
    for w in [30, 60, 80, 140] {
        let lines = rig.shell.view().render_frame(w, H);
        for l in &lines {
            assert!(visible_width(l) <= w, "{w}: {:?}", plain(l));
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn amp_shows_the_connection_and_esc_closes_it() {
    let mut rig = rig(vec![], true).await;
    rig.say("/amp").await;
    let screen = rig.screen();
    for want in [
        "AMP · connection",
        "instance amp.simstech.cloud (https://amp.simstech.cloud/mcp)",
        "server   amp 2.0.0 · protocol 2026-07-28 · auth oauth",
        "user     ada@example.com · human",
        "health   ● connected",
        "project  simstech › AMP #2",
        "Enter / o open in browser · Esc close",
    ] {
        assert!(screen.contains(want), "{want}\n{screen}");
    }
    assert_fits(&rig);
    // Enter opens the selected row (the instance).
    assert!(rig.press(KeyCode::Enter).await);
    assert_eq!(rig.opened(), vec!["https://amp.simstech.cloud".to_owned()]);
    assert!(rig.press(KeyCode::Esc).await);
    assert!(!rig.shell.view().panel().is_open());
    assert!(!rig.screen().contains("AMP · connection"));
    // No panel: keys go to the editor.
    assert!(!rig.press(KeyCode::Down).await);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn ticket_shows_and_switches_the_current_ticket() {
    let mut rig = rig(vec![], true).await;
    rig.say("/ticket").await;
    let screen = rig.screen();
    assert!(screen.contains("AMP · ticket"), "{screen}");
    assert!(
        screen.contains("› ■ In progress #2106 M3-3 Header and AMP status line"),
        "{screen}"
    );
    assert!(
        screen.contains("plan     ▌ AMP Harness › M3 theme + metadata"),
        "{screen}"
    );
    assert!(
        screen.contains("state in_progress · assigned amp:amp-worker-backend"),
        "{screen}"
    );
    assert!(
        screen.contains("this session has not reported a run"),
        "{screen}"
    );
    // The marker for the first ticket, and the footer's #ticket.
    assert!(
        screen.contains("── ■ In progress #2106 M3-3 Header and AMP status line ──"),
        "{screen}"
    );
    assert!(
        screen.contains("/work/amp (harness-m3) • manager · #2106"),
        "{screen}"
    );

    // o opens the ticket's board drawer.
    assert!(rig.press(KeyCode::Char('o')).await);
    assert_eq!(
        rig.opened(),
        vec!["https://amp.simstech.cloud/project/2/board?task=2106".to_owned()]
    );

    // /ticket 2107 switches the poller; the panel follows (the stub has no 2107 data).
    rig.say("/ticket #2107").await;
    assert_eq!(rig.meta.snapshot().ticket_id, Some(2107));
    let screen = rig.screen();
    assert!(screen.contains("#2107 loading…"), "{screen}");
    assert!(screen.contains("manager · #2107"), "{screen}");

    rig.say("/ticket none").await;
    assert_eq!(rig.meta.snapshot().ticket_id, None);
    let screen = rig.screen();
    assert!(screen.contains("no current ticket"), "{screen}");
    assert!(!rig.shell.view().panel().is_open());
    assert!(
        screen.contains("• manager\n") || screen.contains("• manager"),
        "{screen}"
    );

    rig.say("/ticket abc").await;
    assert!(rig.screen().contains("/ticket: `abc` is not a ticket id"));
    assert_fits(&rig);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn workers_lists_local_and_remote_workers() {
    let mut rig = rig(vec![], true).await;
    rig.say("/workers").await;
    let screen = rig.screen();
    assert!(screen.contains("AMP · workers"), "{screen}");
    assert!(
        screen.contains(
            "› ■ running #2105 M3-5 Inline ticket chips worker-backend · claude-opus-5-5"
        ),
        "{screen}"
    );
    assert!(screen.contains("□ queued  #2111"), "{screen}");
    assert!(
        screen.contains("■ elsewhere #2104 M3-2 AMP metadata poller worker-backend"),
        "{screen}"
    );
    assert!(screen.contains("■ elsewhere #2106"), "{screen}");
    assert!(
        !screen.contains("elsewhere #2105"),
        "a local worker is not listed twice"
    );
    // ↓ then o: the second worker's ticket.
    rig.press(KeyCode::Down).await;
    rig.press(KeyCode::Char('o')).await;
    assert_eq!(
        rig.opened(),
        vec!["https://amp.simstech.cloud/project/2/board?task=2111".to_owned()]
    );
    assert_fits(&rig);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn questions_are_answered_only_after_confirmation() {
    let mut rig = rig(vec![], true).await;
    rig.say("/questions").await;
    let screen = rig.screen();
    assert!(screen.contains("AMP · questions"), "{screen}");
    assert!(
        screen.contains("› ■  QUESTION  #2107 M3-4 Slash commands"),
        "{screen}"
    );
    assert!(screen.contains("Overlay or inline?"), "{screen}");
    assert!(screen.contains("#2110 M3-6 Docs"), "{screen}");
    assert!(screen.contains("Enter answer"), "{screen}");

    // Enter: the answer is typed in the editor.
    assert!(rig.press(KeyCode::Enter).await);
    let state = rig.shell.view().panel().state().unwrap();
    assert!(
        matches!(state.stage, Stage::Answer { ticket: 2107, .. }),
        "{state:?}"
    );
    assert!(rig.screen().contains("type the answer in the editor below"));
    // Keys other than Esc belong to the editor while answering.
    assert!(!rig.shell.panel_key(&key(KeyCode::Char('y')), true));

    // Submitting shows exactly what will be sent; nothing is sent yet.
    rig.say("Overlay,\x1b[31m please.\nDocked above the editor.")
        .await;
    let screen = rig.screen();
    assert!(
        screen.contains("send this answer to #2107 M3-4 Slash commands with amp_answer_question?"),
        "{screen}"
    );
    assert!(screen.contains("Overlay,[31m please."), "{screen}");
    assert!(screen.contains("Docked above the editor."), "{screen}");
    assert!(
        screen.contains("y / Enter send · n / Esc cancel"),
        "{screen}"
    );
    assert!(
        rig.meta.stub_calls().is_empty(),
        "not sent before confirming"
    );
    // Other keys are swallowed while confirming.
    assert!(rig.press(KeyCode::Char('x')).await);
    assert!(rig.meta.stub_calls().is_empty());

    // n cancels: still nothing sent.
    assert!(rig.press(KeyCode::Char('n')).await);
    assert!(rig.screen().contains("not sent"));
    assert!(rig.meta.stub_calls().is_empty());

    // Again, and y sends it, cleaned the same way it was shown.
    rig.press(KeyCode::Enter).await;
    rig.say("Overlay, docked.").await;
    assert!(rig.press(KeyCode::Char('y')).await);
    assert_eq!(
        rig.meta.stub_calls(),
        vec![(
            "amp_answer_question".to_owned(),
            json!({"task_id": 2107, "answer": "Overlay, docked."})
        )]
    );
    let screen = rig.screen();
    assert!(
        screen.contains("answered #2107 (amp_answer_question)"),
        "{screen}"
    );
    assert!(screen.contains("answered #2107"), "{screen}");

    // A failed answer says so.
    rig.press(KeyCode::Down).await;
    rig.press(KeyCode::Enter).await;
    rig.say("README").await;
    rig.press(KeyCode::Enter).await;
    let screen = rig.screen();
    assert!(screen.contains("answer to #2110 failed:"), "{screen}");
    assert!(screen.contains("task 2110 is not blocked"), "{screen}");
    assert_eq!(rig.meta.stub_calls().len(), 2);

    // An empty answer is not taken.
    rig.press(KeyCode::Enter).await;
    rig.say("   \x1b").await;
    assert!(rig.screen().contains("the answer is empty"));
    assert!(rig.press(KeyCode::Esc).await);
    assert_eq!(
        rig.shell.view().panel().state().unwrap().stage,
        Stage::Browse
    );
    assert_fits(&rig);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn plan_shows_the_tree_and_approval() {
    let mut rig = rig(vec![], true).await;
    rig.say("/plan").await;
    let screen = rig.screen();
    assert!(screen.contains("AMP · plan"), "{screen}");
    assert!(
        screen.contains("approval 2 awaiting approval #2108 #2109"),
        "{screen}"
    );
    assert!(screen.contains("▌ Earlier ■ 12 Done"), "{screen}");
    // The six web words with state marks: the story's three blocked tickets are a question, a
    // waiting ticket and a scheduled one, not "3 blocked".
    assert!(
        screen.contains(
            "M3 theme + metadata ■ 2 In progress · □ 1 Ready · ■ 1 Question · ■ 1 Scheduled · □ 1 Waiting"
        ),
        "{screen}"
    );
    assert!(
        screen.contains("▌ AMP Harness ■ 2 In progress · □ 3 Ready · ■ 1 Question · ■ 1 Scheduled"),
        "{screen}"
    );
    for word in ["open", "blocked"] {
        assert!(!screen.contains(&format!("1 {word}")), "{screen}");
    }
    let calls = rig.meta.stub_calls();
    assert_eq!(calls[0].1, json!({"project_id": 2}));
    assert_eq!(
        calls[1],
        (
            "amp_list_tasks".to_owned(),
            json!({"project_id": 2, "owner": "any", "state": "blocked", "format": "full"})
        )
    );
    rig.press(KeyCode::Enter).await;
    assert_eq!(
        rig.opened(),
        vec!["https://amp.simstech.cloud/project/2".to_owned()]
    );
    assert_fits(&rig);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn project_shows_and_switches_the_project() {
    let mut rig = rig(vec![], true).await;
    rig.say("/project").await;
    let screen = rig.screen();
    assert!(screen.contains("AMP · project"), "{screen}");
    assert!(screen.contains("project  simstech › AMP #2"), "{screen}");
    assert!(
        screen.contains("code amp · active · plan approval required"),
        "{screen}"
    );
    assert!(
        screen.contains("board    ■ 3 In progress · □ 5 Ready · ■ 2 Question · ■ 1 Scheduled"),
        "{screen}"
    );
    rig.say("/project 5").await;
    assert_eq!(rig.meta.snapshot().project_id, Some(5));
    assert!(rig.shell.view().panel().state().unwrap().kind == PanelKind::Project);
    rig.say("/project x").await;
    assert!(rig.screen().contains("/project: `x` is not a project id"));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn open_opens_only_urls_from_the_helpers() {
    let mut rig = rig(vec![], true).await;
    rig.say("/open").await;
    rig.say("/open 2109").await;
    assert_eq!(
        rig.opened(),
        vec![
            "https://amp.simstech.cloud/project/2/board?task=2106".to_owned(),
            "https://amp.simstech.cloud/project/2/board?task=2109".to_owned(),
        ]
    );
    assert!(
        rig.screen()
            .contains("opened https://amp.simstech.cloud/project/2/board?task=2109")
    );
    rig.say("/open nope").await;
    assert!(rig.screen().contains("/open: `nope` is not a ticket id"));
    // No current ticket: the project's Mission page.
    rig.say("/ticket none").await;
    rig.say("/open").await;
    assert_eq!(
        rig.opened().last().unwrap(),
        "https://amp.simstech.cloud/project/2"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn without_metadata_the_commands_say_so() {
    let mut rig = rig(vec![], false).await;
    for cmd in [
        "amp",
        "ticket",
        "workers",
        "questions",
        "plan",
        "project",
        "open",
    ] {
        rig.say(&format!("/{cmd}")).await;
        assert!(
            rig.screen()
                .contains(&format!("/{cmd}: no AMP metadata in this session")),
            "{cmd}\n{}",
            rig.screen()
        );
    }
    assert!(!rig.shell.view().panel().is_open());
    assert!(rig.opened().is_empty());
    rig.say("/help").await;
    assert!(
        rig.screen()
            .contains("/questions  /plan  /project [id]  /open [id]")
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn amp_tool_calls_are_titled_with_their_ticket() {
    let mut rig = rig(
        vec![
            FauxStep::tool_call(
                "amp_complete_task",
                json!({"task_id": 2078, "note": "all done"}),
            ),
            FauxStep::tool_call("read", json!({"path": "README.md"})),
            FauxStep::text("reported"),
        ],
        false,
    )
    .await;
    rig.say("finish it").await;
    let screen = rig.screen();
    assert!(
        screen.contains("✗ AMP · complete_task #2078 all done"),
        "{screen}"
    );
    assert!(!screen.contains("amp_complete_task {"), "{screen}");
    // Other tools keep pi's rendering.
    assert!(screen.contains("read README.md"), "{screen}");
}
