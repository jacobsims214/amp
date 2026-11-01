//! #2425: the chat waits for a person's plan approval after the manager asks for it, and starts
//! the manager's next turn by itself when a person approves in the web UI. The shell runs on a
//! real rpi `AgentHarness` (faux provider) with a stub metadata handle as the mock AMP: the test
//! changes the stub's `amp_check_plan` read the way a person's click on "Approve plan" changes
//! AMP's answer (the harness itself never approves). The poller's backoff is tested in amp-core
//! (meta.rs `a_long_pending_plan_is_polled_less_often`); the real pty in amp-harness's
//! tests/chat_approval_wait.rs.

use std::{
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::{
    agent::memory_session,
    events::ChannelEmitter,
    meta::{AmpMeta, MetaHandle, PlanApproval},
};
use amp_tui::{Shell, ShellInfo, Submit, chrome::sample};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use serde_json::json;
use tokio::sync::mpsc::UnboundedReceiver;

const W: usize = 120;
const H: usize = 40;
const WAIT: Duration = Duration::from_secs(20);
const RESUMED: &str = "Resumed: dispatching the approved tickets.";

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
    meta: MetaHandle,
}

/// The manager checks the plan (any `amp_*` plan tool marks the turn), asks for approval, and
/// would answer two more turns: the resume, and a second resume that must never come.
fn script() -> Vec<FauxStep> {
    vec![
        FauxStep::tool_call("amp_check_plan", json!({"project_id": 2})),
        FauxStep::text("Plan ready: approve it in AMP."),
        FauxStep::text(RESUMED),
        FauxStep::text("SECOND RESUME"),
    ]
}

async fn rig(steps: Vec<FauxStep>, role: &str) -> Rig {
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
        role: role.into(),
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
    let meta = MetaHandle::stub(
        sample::meta(false),
        Arc::new(|tool, _| Err(format!("{tool}: not in the stub"))),
    );
    shell.set_meta(meta.clone());
    Rig {
        shell,
        events,
        done,
        meta,
    }
}

/// A plan read made now: `awaiting` still wait; `approved_at` is the project's last approval.
fn read(m: &mut AmpMeta, awaiting: &[i64], approved_at: &str) {
    m.plan.value = Some(PlanApproval::from_check_plan(&json!({
        "require_plan_approval": true,
        "awaiting_approval": awaiting,
        "plan_approved_at": approved_at,
        "plan_approved_by": 7,
    })));
    m.plan.updated = Some(Instant::now());
}

const BEFORE: &str = "2026-10-26T10:00:00Z";
const APPROVED: &str = "2026-10-26T14:02:00Z";

impl Rig {
    async fn say(&mut self, text: &str) {
        assert_eq!(self.shell.submit(text).await, Submit::Continue);
        self.idle().await;
    }

    async fn idle(&mut self) {
        self.shell
            .drive_until_idle(&mut self.events, &mut self.done, WAIT)
            .await
            .expect("idle");
        self.tick().await;
    }

    /// A few terminal ticks.
    async fn tick(&mut self) {
        for _ in 0..10 {
            tokio::time::sleep(Duration::from_millis(5)).await;
            self.shell.poll();
        }
    }

    /// The poller read the plan again (`awaiting`, `approved_at`), then the terminal ticked.
    async fn poll_plan(&mut self, awaiting: &[i64], approved_at: &str) {
        // Plan reads made in the same instant as the last would look like the same read.
        tokio::time::sleep(Duration::from_millis(2)).await;
        self.meta.stub_update(|m| read(m, awaiting, approved_at));
        self.tick().await;
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(W, H).join("\n")
    }

    fn running(&self) -> bool {
        self.screen().contains("Working")
    }
}

/// Asks for approval and confirms the chat waits for it (status line and transcript).
async fn waiting(rig: &mut Rig) {
    rig.say("plan the work").await;
    assert!(rig.screen().contains("Plan ready: approve it in AMP."));
    // The read from before the turn ended doesn't count: nothing is said yet.
    assert!(!rig.screen().contains("Waiting for plan approval in AMP"));
    rig.poll_plan(&[2108, 2109], BEFORE).await;
    let screen = rig.screen();
    assert!(rig.shell.waiting_for_approval());
    assert!(
        screen.contains("⏸ Waiting for plan approval in AMP (#2108, #2109)."),
        "{screen}"
    );
    assert!(
        screen.contains("⏸ waiting for plan approval in AMP"),
        "status line: {screen}"
    );
    assert_eq!(
        rig.shell.view().chrome().approval_wait().as_deref(),
        Some("https://amp.simstech.cloud/project/2/my-work?approve=all")
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_approval_resumes_the_manager_exactly_once() {
    let mut rig = rig(script(), "amp-manager").await;
    waiting(&mut rig).await;
    // The person approves #2108 in the web UI (#2109 left for later).
    rig.poll_plan(&[2109], APPROVED).await;
    assert!(!rig.shell.waiting_for_approval());
    rig.idle().await;
    let screen = rig.screen();
    assert!(
        screen.contains("✓ Plan approved in AMP by ada@example.com at"),
        "{screen}"
    );
    assert!(
        screen.contains("(#2108). #2109 still waiting. The manager carries on."),
        "{screen}"
    );
    assert!(
        screen.contains("[amp-harness] The plan was approved in AMP by ada@example.com: #2108"),
        "{screen}"
    );
    assert!(screen.contains(RESUMED), "{screen}");
    assert!(
        rig.shell.view().chrome().approval_wait().is_none(),
        "the waiting segment goes"
    );
    // More reads (the approval again, then #2109 too) start nothing: the wait ended at the resume.
    rig.poll_plan(&[2109], APPROVED).await;
    rig.poll_plan(&[], "2026-10-26T15:00:00Z").await;
    assert!(!rig.running());
    let screen = rig.screen();
    assert!(!screen.contains("SECOND RESUME"), "{screen}");
    assert_eq!(screen.matches("✓ Plan approved in AMP").count(), 1);
    // The harness made no AMP call of its own to approve.
    assert!(
        rig.meta
            .stub_calls()
            .iter()
            .all(|(t, _)| t != "amp_approve_plan")
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn no_resume_without_an_approval() {
    let mut rig = rig(script(), "amp-manager").await;
    waiting(&mut rig).await;
    for _ in 0..5 {
        rig.poll_plan(&[2108, 2109], BEFORE).await;
    }
    // Someone else's approval elsewhere in the project moved the project's last approval, but
    // none of these tickets: still waiting.
    rig.poll_plan(&[2108, 2109], APPROVED).await;
    assert!(rig.shell.waiting_for_approval());
    assert!(!rig.running());
    assert!(!rig.screen().contains(RESUMED));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn esc_or_stop_cancels_the_wait() {
    for how in ["esc", "/stop"] {
        let mut rig = rig(script(), "amp-manager").await;
        waiting(&mut rig).await;
        if how == "esc" {
            let esc = KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE);
            assert!(rig.shell.key(&esc, true).await, "Esc is used");
        } else {
            rig.say("/stop").await;
        }
        assert!(!rig.shell.waiting_for_approval(), "{how}");
        let screen = rig.screen();
        assert!(
            screen.contains("Stopped waiting for plan approval."),
            "{how}: {screen}"
        );
        assert!(rig.shell.view().chrome().approval_wait().is_none());
        rig.poll_plan(&[], APPROVED).await;
        assert!(!rig.running(), "{how}");
        assert!(!rig.screen().contains(RESUMED), "{how}");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_new_prompt_or_the_setting_off_means_no_wait() {
    // Typing a new prompt: that turn decides for itself (here it doesn't touch the plan).
    let mut r = rig(
        vec![
            FauxStep::tool_call("amp_check_plan", json!({"project_id": 2})),
            FauxStep::text("Plan ready: approve it in AMP."),
            FauxStep::text("Something else."),
            FauxStep::text(RESUMED),
        ],
        "amp-manager",
    )
    .await;
    waiting(&mut r).await;
    r.say("what else?").await;
    assert!(!r.shell.waiting_for_approval());
    r.poll_plan(&[], APPROVED).await;
    assert!(!r.screen().contains(RESUMED));

    // auto_resume_on_approval = false.
    let mut r = rig_off().await;
    r.say("plan the work").await;
    r.poll_plan(&[2108, 2109], BEFORE).await;
    assert!(!r.shell.waiting_for_approval());
    assert!(!r.screen().contains("Waiting for plan approval in AMP"));
    r.poll_plan(&[], APPROVED).await;
    assert!(!r.screen().contains(RESUMED));

    // Not the manager: no wait.
    let mut r = rig(script(), "amp-reviewer").await;
    r.say("plan the work").await;
    r.poll_plan(&[2108, 2109], BEFORE).await;
    assert!(!r.shell.waiting_for_approval());
}

async fn rig_off() -> Rig {
    let mut r = rig(script(), "amp-manager").await;
    r.shell.set_auto_resume(false);
    r
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn tickets_that_stop_waiting_without_an_approval_end_the_wait() {
    let mut rig = rig(script(), "amp-manager").await;
    waiting(&mut rig).await;
    // Both tickets deleted in AMP: no approval recorded.
    rig.poll_plan(&[], BEFORE).await;
    assert!(!rig.shell.waiting_for_approval());
    let screen = rig.screen();
    assert!(
        screen.contains("#2108, #2109 no longer wait for approval"),
        "{screen}"
    );
    assert!(!rig.running());
    assert!(!screen.contains(RESUMED));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn nothing_awaiting_means_no_wait() {
    let mut rig = rig(script(), "amp-manager").await;
    rig.say("plan the work").await;
    rig.poll_plan(&[], BEFORE).await;
    assert!(!rig.shell.waiting_for_approval());
    assert!(!rig.screen().contains("Waiting for plan approval"));
}

/// Remote control (#2425 on remote-m1): a paired browser follows the chat's feed, so it sees the
/// wait and the resumed turn; its Stop stops the wait like Esc.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn remote_viewers_see_the_wait_and_the_resume_and_stop_ends_it() {
    use amp_core::{chat_event::ChatEvent, manager_feed::ManagerFeed};
    let texts = |feed: &ManagerFeed| -> Vec<String> {
        feed.flush();
        feed.since(0)
            .unwrap()
            .into_iter()
            .filter_map(|e| match e.event {
                ChatEvent::Notice { text, .. } => Some(text),
                ChatEvent::UserMessage { text, .. } => Some(format!("user: {text}")),
                _ => None,
            })
            .collect()
    };

    // The wait and the resume reach the feed.
    let mut r = rig(script(), "amp-manager").await;
    let feed = ManagerFeed::new();
    r.shell.set_feed(Some(Arc::clone(&feed)));
    waiting(&mut r).await;
    assert!(
        texts(&feed)
            .iter()
            .any(|t| t.starts_with("⏸ Waiting for plan approval in AMP (#2108, #2109).")),
        "{:?}",
        texts(&feed)
    );
    r.poll_plan(&[], APPROVED).await;
    r.idle().await;
    let seen = texts(&feed);
    assert!(
        seen.iter()
            .any(|t| t.starts_with("✓ Plan approved in AMP by ada@example.com")),
        "{seen:?}"
    );
    assert!(
        seen.iter()
            .any(|t| t.starts_with("user: [amp-harness] The plan was approved in AMP")),
        "{seen:?}"
    );

    // Stop from the browser while waiting: the wait ends, an approval then starts nothing.
    let mut r = rig(script(), "amp-manager").await;
    let feed = ManagerFeed::new();
    r.shell.set_feed(Some(Arc::clone(&feed)));
    waiting(&mut r).await;
    let host = r.shell.remote_host();
    let stop = tokio::spawn(async move { host.stop().await });
    for _ in 0..200 {
        r.shell.poll();
        if stop.is_finished() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    stop.await.unwrap();
    assert!(!r.shell.waiting_for_approval());
    assert!(
        texts(&feed)
            .iter()
            .any(|t| t.starts_with("Stopped waiting for plan approval.")),
        "{:?}",
        texts(&feed)
    );
    r.poll_plan(&[], APPROVED).await;
    assert!(!r.screen().contains(RESUMED));
}
