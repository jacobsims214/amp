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
        session_file: None,
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
    // #2213: the dispatch time is relative like the rest of the chrome, not raw RFC 3339.
    let amp_row = screen
        .lines()
        .find(|l| l.contains("AMP      state in_progress"))
        .unwrap_or_else(|| panic!("AMP row\n{screen}"));
    assert!(
        amp_row.contains("· dispatched ") && amp_row.trim_end().ends_with(" ago"),
        "{amp_row}"
    );
    assert!(!amp_row.contains("2026-10-22T"), "{amp_row}");
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

/// UX review #2175 finding 12 (#2187): a current ticket that completes stops being current (no
/// "■ Done #id" pinned under the header for the rest of the session), and the /ticket panel
/// leaves empty fields out instead of printing "-".
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_done_ticket_stops_being_current() {
    let mut rig = rig(vec![], true).await;
    rig.settle().await;
    assert_eq!(rig.meta.snapshot().ticket_id, Some(2106));
    assert!(rig.screen().contains("manager · #2106"), "{}", rig.screen());

    // The ticket completes mid-session (a poll finds it done, no assignee or dispatch time).
    rig.meta.stub_update(|m| {
        let t = m.ticket.value.as_mut().unwrap();
        t.state = "completed".into();
        t.status = amp_core::tickets::Status::Done;
        t.assigned_to = None;
        t.dispatched_at = None;
    });
    rig.settle().await;
    assert_eq!(rig.meta.snapshot().ticket_id, None, "cleared on completion");
    let screen = rig.screen();
    assert!(
        screen.contains("── ■ Done #2106 M3-3 Header and AMP status line ──"),
        "the Done marker says what happened\n{screen}"
    );
    assert!(
        !screen.contains("no current ticket"),
        "no second marker\n{screen}"
    );
    assert!(
        !screen.contains("#2106\n") && !screen.contains("· #2106"),
        "{screen}"
    );

    // `/ticket 2106` on the done ticket: it stays current while its panel is open, the AMP row
    // has no "-" placeholders, and it goes when the panel closes.
    rig.say("/ticket 2106").await;
    assert_eq!(rig.meta.snapshot().ticket_id, Some(2106));
    let screen = rig.screen();
    assert!(screen.contains("› ■ Done #2106"), "{screen}");
    let amp_row = screen
        .lines()
        .find(|l| l.contains("AMP      state completed"))
        .unwrap_or_else(|| panic!("AMP row\n{screen}"));
    assert!(!amp_row.contains(" -"), "{amp_row}");
    assert!(
        !amp_row.contains("assigned") && !amp_row.contains("dispatched"),
        "{amp_row}"
    );
    assert!(rig.press(KeyCode::Esc).await);
    assert_eq!(
        rig.meta.snapshot().ticket_id,
        None,
        "cleared once the panel closed"
    );
    assert_fits(&rig);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn workers_lists_local_and_remote_workers() {
    let mut rig = rig(vec![], true).await;
    rig.say("/workers").await;
    let screen = rig.screen();
    assert!(screen.contains("AMP · workers"), "{screen}");
    // Every row leads with the palette state mark (the web word), then #id name, the
    // assignee exactly as AMP stores it, and where it runs.
    assert!(
        screen.contains(
            "› ■ In progress running #2105 M3-5 Inline ticket chips · amp:amp-worker-backend · local"
        ),
        "{screen}"
    );
    assert!(
        screen.contains("    claude-opus-5-5 · "),
        "the run's detail below it\n{screen}"
    );
    assert!(
        screen.contains("□ Ready queued #2111 · amp:amp-worker-backend · local"),
        "{screen}"
    );
    assert!(
        screen.contains(
            "■ In progress #2104 M3-2 AMP metadata poller · amp:amp-worker-backend · elsewhere"
        ),
        "{screen}"
    );
    assert!(screen.contains("■ In progress #2106"), "{screen}");
    assert!(
        screen.lines().filter(|l| l.contains("#2105")).count() == 1,
        "a local worker is not listed twice\n{screen}"
    );
    assert!(
        !screen.contains(" worker-backend"),
        "assignee unmodified\n{screen}"
    );
    // The location is dim; the assignee is not.
    let c = rpi_tui::theme().colors;
    let frame = rig.shell.view().render_frame(W, H).join("\n");
    let local_row = frame
        .lines()
        .find(|l| l.contains("#2105"))
        .unwrap_or_default();
    assert!(local_row.contains(&c.dim.fg("· local")), "{local_row:?}");
    assert!(frame.contains(&c.dim.fg("· elsewhere")), "{frame:?}");
    assert!(
        frame.contains(&c.muted.fg("amp:amp-worker-backend")),
        "{frame:?}"
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
    // #2171: the person approves, in the web UI; the row and its hint link the Flight plan page.
    assert!(
        screen.contains("approval 2 waiting for your approval #2108 #2109"),
        "{screen}"
    );
    assert!(
        screen.contains(
            "approve the plan in AMP: https://amp.simstech.cloud/project/2/plan (o or /open plan)"
        ),
        "{screen}"
    );
    let frame = rig.shell.view().render_frame(W, H).join("\n");
    assert!(
        frame.contains("\x1b]8;;https://amp.simstech.cloud/project/2/plan\x07"),
        "the plan link is an OSC 8 hyperlink"
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
    // The first row is the approval: Enter opens the Flight plan page.
    rig.press(KeyCode::Enter).await;
    assert_eq!(
        rig.opened(),
        vec!["https://amp.simstech.cloud/project/2/plan".to_owned()]
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
    // `/open plan`: the Flight plan page, where a person approves (#2171).
    rig.say("/open plan").await;
    assert_eq!(
        rig.opened().last().unwrap(),
        "https://amp.simstech.cloud/project/2/plan"
    );
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
    let screen = rig.screen();
    for usage in ["/questions", "/plan", "/project [id]", "/open [id|plan]"] {
        assert!(screen.contains(usage), "{usage}\n{screen}");
    }
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
        screen.contains("✗ complete_task #2078 all done"),
        "{screen}"
    );
    // #2183: one line per call, and the two calls in a row share one block (no blank between).
    let rows: Vec<&str> = screen.lines().collect();
    let at = rows
        .iter()
        .position(|r| r.contains("✗ complete_task #2078"))
        .unwrap();
    assert!(
        rows[at + 1..]
            .iter()
            .take(6)
            .any(|r| r.starts_with("   ✗ read README.md")),
        "{screen}"
    );
    assert!(
        rows[at + 1..]
            .iter()
            .take_while(|r| !r.contains("read README.md"))
            .all(|r| !r.trim().is_empty()),
        "{screen}"
    );
    assert!(!screen.contains("amp_complete_task {"), "{screen}");
    // Other tools keep pi's rendering.
    assert!(screen.contains("read README.md"), "{screen}");
}

/// #2171: AMP refused a step only a person takes. The transcript says so plainly, with the link
/// built from the instance (not the server's text), under the refused call.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_refusal_shows_waiting_for_your_approval_with_the_link() {
    let mut rig = rig(vec![], true).await;
    let refused = |name: &str, text: &str| AgentEvent::ToolExecutionEnd {
        tool_call_id: format!("call-{name}"),
        tool_name: name.into(),
        result: rpi_agent::AgentToolResult::text(text),
        is_error: true,
    };
    rig.shell.handle_event(&refused(
        "amp_dispatch_task",
        "ticket 2108 was planned after the plan was last approved; this project needs the plan \
         approved before agents start new tickets. A person approves it in the AMP web UI (Approve \
         plan): http://localhost:5173/project/2/plan. Agents can't approve: stop and ask your \
         person to approve it there.\n[amp-harness] This needs a person in the AMP web UI.",
    ));
    let screen = rig.screen();
    assert!(screen.contains("⏸ Waiting for your approval"), "{screen}");
    // #2183: the notice stays right under the refused call's line and its error, prominent.
    let rows: Vec<&str> = screen.lines().collect();
    let call = rows
        .iter()
        .position(|r| r.starts_with("   ✗ dispatch_task"))
        .expect(&screen);
    let notice = rows
        .iter()
        .position(|r| r.contains("⏸ Waiting for your approval"))
        .unwrap();
    assert!(notice > call && notice - call <= 7, "{screen}");
    assert!(
        screen.contains(
            "Approve it in AMP: https://amp.simstech.cloud/project/2/plan   (/open plan)"
        ),
        "{screen}"
    );
    assert!(
        screen.contains("The manager stopped here; nothing else was run."),
        "{screen}"
    );
    // The notice quotes the server's sentence, not the harness's note to the model.
    let notice = &screen[screen.find("⏸").unwrap()..];
    assert!(!notice.contains("[amp-harness]"), "{notice}");
    let frame = rig.shell.view().render_frame(W, H).join("\n");
    assert!(frame.contains("\x1b]8;;https://amp.simstech.cloud/project/2/plan\x07"));

    // A person's ticket: the board drawer for that ticket.
    rig.shell.handle_event(&refused(
        "amp_complete_task",
        "Marking ticket 2110 (a person's ticket) done is done by a person in the AMP web UI, not \
         by an agent or over MCP: http://localhost:5173/project/2/board?task=2110. Stop and ask \
         your person to do it there.",
    ));
    let screen = rig.screen();
    assert!(
        screen.contains("⏸ Waiting for you in the AMP web UI"),
        "{screen}"
    );
    assert!(
        screen.contains("Do it in AMP: https://amp.simstech.cloud/project/2/board?task=2110"),
        "{screen}"
    );

    // Other errors are just errors.
    let before = rig.screen().matches("⏸").count();
    rig.shell.handle_event(&refused(
        "amp_complete_task",
        "ticket 5 is backlog: it was never dispatched, so an agent can't complete it",
    ));
    assert_eq!(rig.screen().matches("⏸").count(), before);
    assert_fits(&rig);
}

/// #2201: per-ticket approval (#2198). `amp_check_plan` says whose each awaiting ticket is.
fn per_ticket_plan(m: &mut amp_core::meta::AmpMeta) {
    let plan = amp_core::meta::PlanApproval::from_check_plan(&json!({
        "require_plan_approval": true,
        "awaiting_approval": [2108, 2109, 2203],
        "awaiting_approval_tickets": [
            {"task_id": 2108, "owner_user_id": 7, "yours": true},
            {"task_id": 2109, "owner_user_id": 7, "yours": true},
            {"task_id": 2203, "owner_user_id": 9, "yours": false}
        ],
        "awaiting_yours": [2108, 2109],
        "approve_url": "http://localhost:5173/project/2/plan"
    }));
    m.plan.value = Some(plan);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn plan_shows_yours_apart_from_others() {
    let mut rig = rig(vec![], true).await;
    rig.meta.stub_update(per_ticket_plan);
    rig.say("/plan").await;
    let screen = rig.screen();
    assert!(
        screen.contains("approval 2 of yours waiting for your approval #2108 #2109"),
        "{screen}"
    );
    assert!(
        screen.contains(
            "approve yours in AMP: https://amp.simstech.cloud/project/2/plan (o or /open plan)"
        ),
        "{screen}"
    );
    assert!(
        screen.contains("others' 1 waiting for their owners' approval #2203"),
        "{screen}"
    );
    assert!(
        screen.contains("their owners approve them in AMP (a project lead may, on their behalf)"),
        "{screen}"
    );
    // The status line counts the person's own (the others' segment goes first when narrow; see
    // tests/chrome.rs).
    assert!(
        screen.contains("│ plan: 2 of yours waiting for your approval"),
        "{screen}"
    );
    assert_fits(&rig);

    // None of the person's own: the panel says so, the chip names only others'.
    rig.meta.stub_update(|m| {
        let plan = m.plan.value.as_mut().unwrap();
        for t in plan.tickets.as_mut().unwrap() {
            t.yours = false;
        }
    });
    rig.say("/plan").await;
    let screen = rig.screen();
    assert!(
        screen.contains("approval none of yours waiting"),
        "{screen}"
    );
    assert!(
        screen.contains("others' 3 waiting for their owners' approval #2108 #2109 #2203"),
        "{screen}"
    );
    assert!(!screen.contains("waiting for your approval"), "{screen}");
}

/// #2201: a per-ticket refusal (#2198) says whose approval the ticket waits for.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_per_ticket_refusal_says_whose_approval() {
    let mut rig = rig(vec![], true).await;
    rig.meta.stub_update(per_ticket_plan);
    let refused = |id: i64| AgentEvent::ToolExecutionEnd {
        tool_call_id: format!("call-{id}"),
        tool_name: "amp_dispatch_task".into(),
        result: rpi_agent::AgentToolResult::text(format!(
            "tool error: amp_dispatch_task: tools/call amp_dispatch_task: Mcp error: -32603: ticket \
             {id} isn't approved: nobody has approved it since it was planned or its scope last \
             changed, and this project needs each ticket approved before agents start it. Its owner \
             approves it in the AMP web UI (Approve plan; a project lead may approve it on their \
             behalf): http://localhost:5173/project/2/plan. Agents can't approve: stop and ask your \
             person to approve it there.\n[amp-harness] This needs a person in the AMP web UI."
        )),
        is_error: true,
    };
    rig.shell.handle_event(&refused(2108));
    let screen = rig.screen();
    assert!(
        screen.contains("⏸ Waiting for your approval: #2108 is yours"),
        "{screen}"
    );
    assert!(
        screen.contains(
            "Approve it in AMP: https://amp.simstech.cloud/project/2/plan   (/open plan)"
        ),
        "{screen}"
    );

    rig.shell.handle_event(&refused(2203));
    let screen = rig.screen();
    assert!(
        screen.contains("⏸ Waiting for its owner's approval: #2203 is someone else's"),
        "{screen}"
    );
    assert!(
        screen.contains("Flight plan: https://amp.simstech.cloud/project/2/plan"),
        "{screen}"
    );

    // Not in the last plan check: its owner's, possibly the person's own.
    rig.shell.handle_event(&refused(4242));
    let screen = rig.screen();
    assert!(
        screen.contains("⏸ Waiting for #4242's owner to approve it"),
        "{screen}"
    );
    assert_fits(&rig);
}

/// #2226: `amp_run_ticket` wraps the dispatch refusal ("AMP refused to dispatch ticket #N: …");
/// the notice still says whose ticket it is (from the call's own `task_id`), shows the server's
/// sentence once without the transport's prefix, and no relative link.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_run_ticket_refusal_says_whose_ticket() {
    let mut rig = rig(vec![], true).await;
    rig.meta.stub_update(per_ticket_plan);
    // `named` is the ticket the error's text names; the call's own `task_id` wins.
    let mut refuse = |id: i64, named: i64| {
        let call = format!("run-{id}");
        rig.shell.handle_event(&AgentEvent::ToolExecutionStart {
            tool_call_id: call.clone(),
            tool_name: "amp_run_ticket".into(),
            args: serde_json::json!({"task_id": id, "agent_id": "amp-worker-backend"}),
        });
        rig.shell.handle_event(&AgentEvent::ToolExecutionEnd {
            tool_call_id: call,
            tool_name: "amp_run_ticket".into(),
            result: rpi_agent::AgentToolResult::text(format!(
                "tool error: AMP refused to dispatch ticket #{named}: tools/call amp_dispatch_task: \
                 Mcp error: -32603: ticket {named} isn't approved: nobody has approved it since it \
                 was planned or its scope last changed, and this project needs each ticket \
                 approved before agents start it. Its owner approves it in the AMP web UI \
                 (Approve plan; a project lead may approve it on their behalf): /project/2/plan. \
                 Agents can't approve: stop and ask your person to approve it there.\n\
                 [amp-harness] This needs a person in the AMP web UI."
            )),
            is_error: true,
        });
    };
    refuse(2108, 2108);
    // Structured over parsed: a text naming another ticket still reads as the call's.
    refuse(2203, 4242);
    // Words, not rows: the sentence wraps.
    let screen = rig
        .screen()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    assert!(
        screen.contains("⏸ Waiting for your approval: #2108 is yours"),
        "{screen}"
    );
    assert!(
        screen.contains("⏸ Waiting for its owner's approval: #2203 is someone else's"),
        "{screen}"
    );
    assert!(!screen.contains("#4242"), "{screen}");
    for noise in [
        "tool error",
        "tools/call",
        "Mcp error",
        "-32603",
        "refused to dispatch",
    ] {
        assert!(!screen.contains(noise), "{noise}: {screen}");
    }
    assert!(!screen.contains(": /project/"), "{screen}");
    assert!(!screen.contains(" /project/"), "{screen}");
    // The sentence once per refusal (the tool rows point at the notice instead).
    assert_eq!(
        screen.matches("isn't approved: nobody").count(),
        2,
        "{screen}"
    );
    assert!(screen.contains("on their behalf). Agents"), "{screen}");
    assert!(
        screen.contains("Approve it in AMP: https://amp.simstech.cloud/project/2/plan"),
        "{screen}"
    );
    assert_fits(&rig);
}
