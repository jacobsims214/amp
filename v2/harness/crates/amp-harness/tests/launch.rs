//! AMP #2834 (story 444): launched — background — dispatch. A chat's `amp_run_ticket` returns an
//! acknowledgement while the worker keeps running in a task the supervisor owns, so the manager
//! can take other work and dispatches from separate turns overlap instead of serialising.
//!
//! What must not change is tested here too: the four-worker cap, duplicate prevention, AMP's
//! dispatch and the worker's claim, the #2802 ledger and the hand-back of every ticket a worker
//! does not close, an explicit stop from the chat, and shutdown. Foreground dispatch stays the
//! default — `tests/supervisor.rs` is that suite, unchanged.
//!
//! In-process against the mock AMP on faux providers; throwaway repos under cargo's target tmp
//! dir (never /tmp: D13).

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use amp_core::{
    agent::{Runtime, RuntimeOptions},
    dispatch_ledger::Ledger,
    faux::StepSpec,
    project::ProjectMarker,
    providers::StaticKeys,
    run_report::RunReporter,
    supervisor::{
        DispatchMode, ManagerContext, RunRequest, RunTicketError, Supervisor, SupervisorConfig,
        WorkerHooks, WorkerInfo, WorkerOutcome, WorkerReport, live_tickets,
    },
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use common::{MockAmp, Running, spawn};
use rpi_ai::{
    providers::faux::{FauxBlock, FauxStep, faux_assistant_message},
    types::{Context, Message, StopReason},
};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

const CONFIG: &str = r#"
[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]

[roles.manager]
model = "wf/wf-1"
[roles.worker-backend]
model = "wf/wf-1"
[roles.researcher]
model = "wf/wf-1"
"#;

struct Fixture {
    dir: PathBuf,
    repo: PathBuf,
    amp: Running,
    rt: Arc<Runtime>,
    ledger: Arc<Ledger>,
}

fn git(dir: &Path, args: &[&str]) -> String {
    let out = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args(args)
        .env("GIT_AUTHOR_NAME", "t")
        .env("GIT_AUTHOR_EMAIL", "t@example.com")
        .env("GIT_COMMITTER_NAME", "t")
        .env("GIT_COMMITTER_EMAIL", "t@example.com")
        .output()
        .expect("run git");
    assert!(
        out.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8_lossy(&out.stdout).trim().to_owned()
}

async fn fixture(name: &str) -> Fixture {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-launch")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    let repo = dir.join("repo");
    std::fs::create_dir_all(&repo).unwrap();
    std::fs::create_dir_all(dir.join("home/.config/amp-harness")).unwrap();
    std::fs::write(dir.join("home/.config/amp-harness/config.toml"), CONFIG).unwrap();
    std::fs::write(
        repo.join(".amp.json"),
        json!({"project_id": 2, "project_name": "AMP v2"}).to_string(),
    )
    .unwrap();
    std::fs::write(repo.join("README.md"), "test repo\n").unwrap();
    git(&repo, &["init", "-q", "-b", "main"]);
    git(&repo, &["add", "."]);
    git(&repo, &["commit", "-q", "-m", "init"]);
    let repo = std::fs::canonicalize(&repo).unwrap();

    let amp = spawn(MockAmp::new(), None).await;
    let client = Arc::new(
        AmpMcpClient::connect(ClientOptions::new(amp.url.clone()))
            .await
            .expect("connect to the mock AMP"),
    );
    let home = dir.join("home");
    let env = {
        let home = home.clone();
        move |k: &str| match k {
            "HOME" => Some(home.to_string_lossy().into_owned()),
            "XDG_CONFIG_HOME" => Some(home.join(".config").to_string_lossy().into_owned()),
            "XDG_STATE_HOME" => Some(home.join(".local/state").to_string_lossy().into_owned()),
            _ => None,
        }
    };
    let rt = Runtime::load(
        client,
        RuntimeOptions {
            repo_root: Some(repo.clone()),
            models: None,
            keys: &StaticKeys::default(),
            env: &env,
            state_dir: None,
        },
    )
    .expect("load the runtime");
    let ledger = Arc::new(Ledger::open(&rt.state_dir));
    Fixture {
        dir,
        repo,
        amp,
        rt: Arc::new(rt),
        ledger,
    }
}

impl Fixture {
    /// A supervisor in the launched mode the chat uses, with the real run reporter (so #2802's
    /// claim, ledger and hand-back are exercised, not stubbed) recording every finished worker.
    fn supervisor(
        &self,
        config: SupervisorConfig,
        hooks: Option<Arc<Recorder>>,
    ) -> Arc<Supervisor> {
        let reporter =
            RunReporter::new(Arc::clone(&self.rt.mcp)).with_ledger(Arc::clone(&self.ledger));
        let hooks: Arc<dyn WorkerHooks> = match hooks {
            Some(recorder) => Arc::new(Recording {
                reporter,
                finished: recorder,
            }),
            None => reporter as Arc<dyn WorkerHooks>,
        };
        self.supervisor_with(config, hooks)
    }

    /// A launched-mode supervisor with exactly these hooks.
    fn supervisor_with(
        &self,
        config: SupervisorConfig,
        hooks: Arc<dyn WorkerHooks>,
    ) -> Arc<Supervisor> {
        Supervisor::with_hooks_and_ledger(
            Arc::clone(&self.rt),
            ManagerContext {
                role: "amp-manager".into(),
                cwd: self.repo.clone(),
                project: ProjectMarker::find_within(&self.repo, Some(&self.dir)).unwrap(),
            },
            config,
            Some(hooks),
            Arc::clone(&self.ledger),
        )
    }

    fn script(&self, steps: Vec<FauxStep>) {
        self.rt
            .providers
            .faux_script("wf")
            .expect("a faux provider")
            .set_responses(steps);
    }

    fn calls(&self, tool: &str) -> Vec<Value> {
        self.amp.mock.calls_to(tool)
    }
}

/// Records every finished worker, in the order they ended.
#[derive(Default)]
struct Recorder {
    finished: Mutex<Vec<WorkerReport>>,
}

impl Recorder {
    fn reports(&self) -> Vec<WorkerReport> {
        self.finished.lock().unwrap().clone()
    }
}

/// The real run reporter, with a copy of every report kept for the test: #2802's hand-back is
/// what these tests check, so it must be the production one, not a stub.
struct Recording {
    reporter: Arc<RunReporter>,
    finished: Arc<Recorder>,
}

#[async_trait::async_trait]
impl WorkerHooks for Recording {
    fn before_tool_call(&self, worker: &WorkerInfo) -> Option<rpi_agent::BeforeToolCall> {
        self.reporter.before_tool_call(worker)
    }

    fn extra_tools(&self, worker: &WorkerInfo) -> Vec<rpi_harness::types::HarnessTool> {
        self.reporter.extra_tools(worker)
    }

    async fn on_finish(&self, worker: &WorkerInfo, report: &mut WorkerReport) {
        self.reporter.on_finish(worker, report).await;
        self.finished.finished.lock().unwrap().push(report.clone());
    }

    async fn release(&self, task_id: i64, why: &str) -> Result<(), String> {
        self.reporter.release(task_id, why).await
    }

    fn notice(&self, text: &str) {
        self.reporter.notice(text);
    }
}

fn launch_config(max_parallel: usize) -> SupervisorConfig {
    SupervisorConfig {
        max_parallel,
        abort_grace: Duration::from_secs(5),
        dispatch: DispatchMode::Launch,
        ..SupervisorConfig::default()
    }
}

fn req(task_id: i64, agent: &str) -> RunRequest {
    RunRequest {
        task_id,
        agent_id: agent.into(),
        ..RunRequest::default()
    }
}

fn has_tool_result(ctx: &Context) -> bool {
    ctx.messages
        .iter()
        .any(|m| matches!(m, Message::ToolResult(_)))
}

/// A worker that runs `command` on its first turn and answers on its second. Each worker decides
/// from its own context, so the shared faux queue serves them in any order.
fn bash_then_done(command: &'static str) -> FauxStep {
    FauxStep::factory(move |ctx: &Context| {
        if has_tool_result(ctx) {
            faux_assistant_message(vec![FauxBlock::text("done")], StopReason::Stop)
        } else {
            faux_assistant_message(
                vec![FauxBlock::tool_call("bash", json!({ "command": command }))],
                StopReason::ToolUse,
            )
        }
    })
}

fn step(text: &str, tool_calls: Value) -> FauxStep {
    let spec: StepSpec =
        serde_json::from_value(json!({"text": text, "tool_calls": tool_calls})).unwrap();
    spec.to_step()
}

/// Waits for `n` launched ends (each with the bound the test gives it), returning them.
async fn ends(sup: &Supervisor, n: usize, bound: Duration) -> Vec<amp_core::supervisor::LaunchEnd> {
    let mut out = Vec::new();
    for _ in 0..n {
        let end = tokio::time::timeout(bound, sup.next_launch_end())
            .await
            .expect("a launched worker's end");
        out.push(end);
    }
    out
}

/// 1. The chat's launch returns while its worker is still running: the ack names the ticket, the
/// agent, the model and the cap, the worker has not finished, and its end arrives afterwards with
/// the report — the dispatch, the claim and its outcome all intact.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_launch_returns_before_its_worker_finishes_and_the_end_carries_the_report() {
    let f = fixture("returns-early").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(launch_config(4), Some(Arc::clone(&hooks)));
    // Two turns of work: the first sleeps long enough that a foreground call would block here.
    // (One faux step is consumed per model request: a factory answers turn by turn, so two.)
    f.script(vec![bash_then_done("sleep 2"), bash_then_done("sleep 2")]);

    let started = Instant::now();
    let ack = sup
        .launch(req(3010, "worker-backend"), CancellationToken::new())
        .await
        .expect("launched");
    let ack_after = started.elapsed();

    // The call returned with the worker's work still ahead of it, not after it: the sleep alone
    // is 2 s, and a foreground call would have waited for the second turn too.
    assert!(
        ack_after < Duration::from_millis(1500),
        "the launch waited for its worker: {ack_after:?}"
    );
    assert!(
        hooks.reports().is_empty(),
        "the worker finished before the ack"
    );
    assert_eq!(
        live_tickets(&sup),
        [3010].into_iter().collect(),
        "it is live"
    );
    assert_eq!(ack.task_id, 3010);
    assert_eq!(ack.agent_id, "amp:amp-worker-backend");
    assert_eq!(ack.role, "amp-worker-backend");
    assert_eq!(ack.model, "wf/wf-1");
    assert_eq!(ack.limit, 4);
    assert!(!ack.queued, "a free slot was there");
    assert!(ack.workspace.is_some(), "the ack says where it works");
    assert!(ack.message.contains("#3010"), "{}", ack.message);
    assert!(ack.message.contains("background"), "{}", ack.message);
    assert_eq!(
        serde_json::to_value(&ack).unwrap()["task_id"],
        3010,
        "the ack is what the tool result carries"
    );

    // Then it ends, once, with the report a foreground call would have returned.
    let end = ends(&sup, 1, Duration::from_secs(30)).await.remove(0);
    assert_eq!(end.task_id, 3010);
    assert_eq!(end.agent_id, "amp:amp-worker-backend");
    let line = end.line();
    let report = end.result.expect("a report");
    assert_eq!(report.outcome, WorkerOutcome::Stopped, "{report:?}");
    assert_eq!(report.dispatch_id, "d_mock3010");
    assert!(report.started, "the harness claimed it: {report:?}");
    assert_eq!(report.turns, 2);
    assert!(line.contains("#3010 stopped"), "{line}");

    // AMP #2802, unchanged by launching: dispatched, claimed with the token, handed back, and the
    // ledger let go of it.
    assert_eq!(f.calls("amp_dispatch_task")[0]["task_id"], 3010);
    assert_eq!(f.calls("amp_claim_task")[0]["dispatch_id"], "d_mock3010");
    assert_eq!(f.calls("amp_return_task")[0]["task_id"], 3010);
    assert!(
        f.ledger.records().iter().all(|r| r.task_id != 3010),
        "{:?}",
        f.ledger.records()
    );
    assert_eq!(hooks.reports().len(), 1);
    assert!(live_tickets(&sup).is_empty(), "the slot was released");
    assert!(sup.take_launch_ends().is_empty(), "read once, then gone");
}

/// 2. Workers launched from separate manager turns overlap without passing four (AMP #2834's
/// second acceptance criterion): six tickets launched from six turns of one chat — more than the
/// cap, so the cap has to bind — reach exactly four at once, never five, and all six get through
/// their dispatch, their run and their end.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn launches_from_separate_turns_overlap_up_to_four_and_never_past() {
    let f = fixture("overlap").await;
    let hooks = Arc::new(Recorder::default());
    // The shipped default cap, not a number of the test's choosing: four is what the chat gets.
    let sup = f.supervisor(launch_config(4), Some(Arc::clone(&hooks)));
    // Six workers, two turns each, from a shared queue any of them can take a step from.
    f.script((0..12).map(|_| bash_then_done("sleep 1")).collect());

    // Six launches, each awaited to its ack before the next: separate turns, as the model makes
    // them. Every ack comes back at once, so they overlap instead of serialising.
    let started = Instant::now();
    let mut acks = Vec::new();
    for id in 3021..=3024 {
        acks.push(
            sup.launch(req(id, "worker-backend"), CancellationToken::new())
                .await
                .expect("launched"),
        );
    }
    // Wait until all four hold their slots (claimed in AMP, so they are really running before the
    // next turns ask), which is also what makes the `queued` ack below deterministic.
    let deadline = Instant::now() + Duration::from_secs(20);
    while f.calls("amp_claim_task").len() < 4 {
        assert!(
            Instant::now() < deadline,
            "the first four never all started"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    // Two more turns, with the cap full: they are accepted (nothing is refused) and told they wait
    // their turn instead of being made to wait for it here.
    for id in 3025..=3026 {
        acks.push(
            sup.launch(req(id, "worker-backend"), CancellationToken::new())
                .await
                .expect("launched"),
        );
    }
    assert!(
        started.elapsed() < Duration::from_millis(3000),
        "six launches took {:?}: they did not overlap",
        started.elapsed()
    );
    // The ack tells the manager the cap it may count on, and (with the four slots taken) that the
    // rest are waiting their turn rather than adding a fifth worker.
    assert!(acks.iter().all(|a| a.limit == 4), "{acks:?}");
    assert!(
        !acks[0].queued && !acks[3].queued,
        "the first four got a slot: {acks:?}"
    );
    assert!(
        acks[4].queued && acks[5].queued,
        "the last two with the cap full said they wait: {acks:?}"
    );
    assert!(
        acks[4].message.contains("waits its turn"),
        "{}",
        acks[4].message
    );

    let ends = ends(&sup, 6, Duration::from_secs(60)).await;
    assert_eq!(ends.iter().map(|e| e.task_id).collect::<Vec<_>>().len(), 6);
    for end in &ends {
        assert_eq!(
            end.result.as_ref().expect("a report").outcome,
            WorkerOutcome::Stopped,
            "{end:?}"
        );
    }
    // Four at once, never five: the cap is the supervisor's semaphore, held by the background
    // task, so it binds however many turns asked.
    assert_eq!(sup.peak_running(), 4, "the cap was reached and held");
    assert!(sup.peak_running() <= 4, "never more than four");
    assert_eq!(f.calls("amp_dispatch_task").len(), 6, "all six dispatched");
    assert!(live_tickets(&sup).is_empty());
    assert_eq!(hooks.reports().len(), 6, "all six ran and were reported");
}

/// 3. Duplicate prevention is unchanged: a ticket with a launched worker refuses a second launch
/// and a foreground run until its worker ends, then accepts one.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_ticket_with_a_launched_worker_cannot_be_launched_twice() {
    let f = fixture("duplicates").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(launch_config(4), Some(Arc::clone(&hooks)));
    f.script(vec![bash_then_done("sleep 1"), bash_then_done("sleep 1")]);
    sup.launch(req(3031, "worker-backend"), CancellationToken::new())
        .await
        .expect("launched");
    let again = sup
        .launch(req(3031, "worker-backend"), CancellationToken::new())
        .await
        .unwrap_err();
    assert!(
        matches!(again, RunTicketError::AlreadyRunning(3031)),
        "{again}"
    );
    let foreground = sup
        .run_ticket(req(3031, "worker-backend"), CancellationToken::new())
        .await
        .unwrap_err();
    assert!(
        matches!(foreground, RunTicketError::AlreadyRunning(3031)),
        "{foreground}"
    );
    // The dispatch happens in the background task: wait for it before counting.
    let deadline = Instant::now() + Duration::from_secs(20);
    while f.calls("amp_claim_task").is_empty() {
        assert!(Instant::now() < deadline, "the worker never started");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    assert_eq!(f.calls("amp_dispatch_task").len(), 1, "dispatched once");

    ends(&sup, 1, Duration::from_secs(30)).await;
    // Once it has ended the ticket can run again (a foreground run this time).
    f.script(vec![step("Nothing to do.", json!([]))]);
    let r = sup
        .run_ticket(req(3031, "worker-backend"), CancellationToken::new())
        .await
        .expect("a second run");
    assert_eq!(r.outcome, WorkerOutcome::Stopped);
    assert_eq!(f.calls("amp_dispatch_task").len(), 2);
}

/// 4. An explicit stop from the chat still ends a launched worker, as `cancelled`, and its ticket
/// is handed back (AMP #2802) — the manager's Esc is what *doesn't* (that's the shell's, #2836).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_launched_worker_stops_when_the_person_stops_it_and_its_ticket_comes_back() {
    let f = fixture("stopped").await;
    let sup = f.supervisor(launch_config(4), None);
    f.script(vec![bash_then_done("sleep 30"), bash_then_done("sleep 30")]);

    sup.launch(req(3041, "worker-backend"), CancellationToken::new())
        .await
        .expect("launched");
    // Wait until it is really running (dispatched), so the stop lands on a live worker.
    let deadline = Instant::now() + Duration::from_secs(20);
    while f.calls("amp_claim_task").is_empty() {
        assert!(Instant::now() < deadline, "the worker never started");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    sup.stop_worker(3041, "jo@example.com").expect("stopped");

    let end = ends(&sup, 1, Duration::from_secs(30)).await.remove(0);
    let report = end.result.expect("a report");
    assert_eq!(report.outcome, WorkerOutcome::Cancelled, "{report:?}");
    assert!(
        report
            .error
            .as_deref()
            .unwrap_or_default()
            .contains("jo@example.com"),
        "it says who stopped it: {report:?}"
    );
    // #2802: the ticket is not left in progress with nobody on it.
    assert_eq!(f.calls("amp_return_task")[0]["task_id"], 3041);
    assert_eq!(report.returned, Some(Ok(())));
    assert_eq!(
        sup.session(3041).map(|s| s.outcome),
        Some(Some(WorkerOutcome::Cancelled)),
        "the /workers panel sees it"
    );
}

/// 5. A tool call cancelled while its launch waits for a slot dispatches nothing: the signal still
/// stops a worker that has not reached AMP, so `run -p`'s abort semantics survive for the queued
/// case (the KB's rule for #2834).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_cancelled_waiting_launch_never_dispatches() {
    let f = fixture("cancelled-wait").await;
    let sup = f.supervisor(launch_config(1), None);
    f.script(vec![
        bash_then_done("sleep 3"),
        bash_then_done("sleep 3"),
        bash_then_done("sleep 3"),
        bash_then_done("sleep 3"),
    ]);

    // The one slot goes to the first worker.
    sup.launch(req(3051, "worker-backend"), CancellationToken::new())
        .await
        .expect("launched");
    let deadline = Instant::now() + Duration::from_secs(20);
    while f.calls("amp_claim_task").is_empty() {
        assert!(Instant::now() < deadline, "the first worker never started");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }

    // The second waits for the slot; the manager's run is cancelled before it gets one.
    let signal = CancellationToken::new();
    let ack = sup
        .launch(req(3052, "worker-backend"), signal.clone())
        .await
        .expect("launched");
    assert!(ack.queued, "no slot was free: {}", ack.message);
    assert!(ack.message.contains("waits its turn"), "{}", ack.message);
    signal.cancel();

    let mut ends = ends(&sup, 2, Duration::from_secs(40)).await;
    ends.sort_by_key(|e| e.task_id);
    let cancelled = ends
        .into_iter()
        .find(|e| e.task_id == 3052)
        .expect("the cancelled launch's end");
    let why = cancelled
        .result
        .as_ref()
        .expect_err("nothing was dispatched");
    assert!(why.contains("cancelled"), "{why}");
    assert!(
        cancelled.line().contains("was not dispatched"),
        "{}",
        cancelled.line()
    );
    // Only the first ticket ever reached AMP.
    assert_eq!(f.calls("amp_dispatch_task").len(), 1);
    assert_eq!(f.calls("amp_dispatch_task")[0]["task_id"], 3051);
}

/// 6. Shutdown: `cancel_all` ends launched workers, and `join_launched` is what a quitting chat
/// waits on before it hands the leftovers back with `release_unfinished`.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn cancel_all_and_join_launched_end_every_background_worker() {
    let f = fixture("shutdown").await;
    let sup = f.supervisor(launch_config(4), None);
    f.script((0..6).map(|_| bash_then_done("sleep 30")).collect());

    for id in 3061..=3063 {
        sup.launch(req(id, "worker-backend"), CancellationToken::new())
            .await
            .expect("launched");
    }
    assert_eq!(sup.launched_running(), 3);
    // All three are really running (claimed in AMP) before the quit, so the quit ends live
    // workers rather than catching one still waiting for its slot.
    let deadline = Instant::now() + Duration::from_secs(20);
    while f.calls("amp_claim_task").len() < 3 {
        assert!(Instant::now() < deadline, "the workers never all started");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    // Nothing to undo while they are still working.
    assert!(
        !sup.unfinished().is_empty(),
        "their tickets are in progress"
    );

    sup.cancel_all();
    let ended = sup.join_launched(Duration::from_secs(30)).await;
    assert_eq!(ended, 3, "every launched task ended");
    assert_eq!(sup.launched_running(), 0);
    assert!(live_tickets(&sup).is_empty());

    let mut ends = ends(&sup, 3, Duration::from_secs(10)).await;
    ends.sort_by_key(|e| e.task_id);
    for end in &ends {
        let report = end.result.as_ref().expect("a report");
        assert_eq!(report.outcome, WorkerOutcome::Cancelled, "{report:?}");
        assert_eq!(report.returned, Some(Ok(())), "its ticket was handed back");
    }
    assert_eq!(f.calls("amp_return_task").len(), 3);
    assert!(sup.unfinished().is_empty());
}

/// 7. The mode itself: `launch` is opt-in, and the tool description matches it — so the manager is
/// told what its call will do.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn dispatch_mode_is_opt_in_from_config_and_the_tool_says_so() {
    // The default is the foreground, for `run -p` and every existing caller. (The config and
    // environment parsing is unit-tested in `amp-core`'s `supervisor` module.)
    let default = SupervisorConfig::default();
    assert_eq!(default.dispatch, DispatchMode::Await);
    assert_eq!(default.dispatch.as_str(), "await");
    assert_eq!(DispatchMode::Launch.as_str(), "launch");

    let f = fixture("description").await;
    let awaiting = f.supervisor(SupervisorConfig::default(), None);
    let launched = f.supervisor(launch_config(4), None);
    let awaiting_desc = awaiting.tool().tool.schema().description.clone();
    let launched_desc = launched.tool().tool.schema().description.clone();
    assert!(
        awaiting_desc.contains("wait for it to finish"),
        "{awaiting_desc}"
    );
    assert!(
        launched_desc.contains("in the background") && launched_desc.contains("acknowledgement"),
        "{launched_desc}"
    );
    assert!(
        !launched_desc.contains("wait for it to finish"),
        "{launched_desc}"
    );
    assert!(
        launched_desc.contains("At most 4 run at once"),
        "{launched_desc}"
    );
}

/// 8. The same thing through the seam the chat actually goes through: the manager's tool call
/// (`amp_run_ticket`'s `execute`), not `Supervisor::launch` directly. In `launch` mode the call
/// returns a JSON ack with the worker still running, and — the KB's rule for this ticket — the
/// *tool call's* signal no longer reaches a worker once AMP has its dispatch. The person's stop
/// (`stop_worker`) is what ends it. Esc stops the reply, not the workers.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_tool_call_acks_then_survives_its_own_signal_and_still_reports() {
    use amp_core::supervisor::RunTicketTool;

    let f = fixture("tool-call").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(launch_config(4), Some(Arc::clone(&hooks)));
    f.script(vec![bash_then_done("sleep 30"), bash_then_done("sleep 30")]);
    let tool = RunTicketTool::new(Arc::clone(&sup));
    let signal = CancellationToken::new();

    // What one manager turn does: call the tool. It answers at once, with the ack's JSON.
    let started = Instant::now();
    let result = tokio::time::timeout(
        Duration::from_secs(10),
        rpi_agent::AgentTool::execute(
            &tool,
            "call-1",
            json!({"task_id": 3071, "agent_id": "amp:amp-worker-backend"}),
            signal.clone(),
            Arc::new(|_| {}),
        ),
    )
    .await
    .expect("the tool call returned without waiting for the worker")
    .expect("the tool call succeeded");
    let acked = started.elapsed();
    assert!(
        acked < Duration::from_secs(5),
        "the tool call waited for its worker: {acked:?}"
    );
    // The tool result is the ack: the manager reads a launched ticket, not a finished one.
    assert_eq!(result.details["task_id"], 3071, "{:?}", result.details);
    assert_eq!(result.details["agent_id"], "amp:amp-worker-backend");
    assert_eq!(result.details["limit"], 4);
    assert!(
        result.details.get("outcome").is_none(),
        "an ack has no outcome yet: {:?}",
        result.details
    );
    assert!(
        rpi_tui_text(&result).contains("background"),
        "the model is told it runs on: {:?}",
        result.content
    );

    // The worker reached AMP (dispatched and claimed) before the turn ended...
    let deadline = Instant::now() + Duration::from_secs(20);
    while f.calls("amp_claim_task").is_empty() {
        assert!(Instant::now() < deadline, "the worker never started");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    // ...so the tool call's signal — the chat stopping the manager's reply — must not kill it.
    signal.cancel();
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        hooks.reports().is_empty(),
        "the cancelled turn ended the worker it had launched: {:?}",
        hooks.reports()
    );
    assert!(
        live_tickets(&sup).contains(&3071),
        "the worker is still the supervisor's after its turn ended"
    );

    // The person at the chat stops it, and the end carries what happened.
    sup.stop_worker(3071, "jo@example.com").expect("stopped");
    let end = ends(&sup, 1, Duration::from_secs(30)).await.remove(0);
    let report = end.result.expect("a report");
    assert_eq!(report.outcome, WorkerOutcome::Cancelled, "{report:?}");
    assert_eq!(report.returned, Some(Ok(())), "its ticket was handed back");
    assert_eq!(f.calls("amp_return_task")[0]["task_id"], 3071);
    assert!(live_tickets(&sup).is_empty());
}

/// The tool result's text, as the manager reads it.
fn rpi_tui_text(result: &rpi_agent::AgentToolResult) -> String {
    result
        .content
        .iter()
        .map(|c| match c {
            rpi_agent::TextContentOrImage::Text(t) => t.text.clone(),
            rpi_agent::TextContentOrImage::Image(_) => String::new(),
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// Launches two workers that each answer in one turn and end on their own, then waits for both
/// background tasks to finish without consuming a single end, so the test starts from "both are
/// done, nobody has read anything" every time.
async fn two_workers_end_unread(f: &Fixture, sup: &Arc<Supervisor>, ids: [i64; 2]) {
    f.script(vec![
        step("Nothing to do.", json!([])),
        step("Nothing to do.", json!([])),
    ]);
    for id in ids {
        sup.launch(req(id, "worker-backend"), CancellationToken::new())
            .await
            .expect("launched");
    }
    // `join_launched` waits on the background tasks themselves, and each task records its end
    // before it returns: once both have joined, both ends are queued and nothing has read them.
    assert_eq!(
        sup.join_launched(Duration::from_secs(30)).await,
        2,
        "both launched workers ended"
    );
    assert_eq!(sup.launched_running(), 0);
    assert!(live_tickets(sup).is_empty());
}

fn sorted_ids(ends: &[amp_core::supervisor::LaunchEnd]) -> Vec<i64> {
    let mut ids: Vec<i64> = ends.iter().map(|e| e.task_id).collect();
    ids.sort_unstable();
    ids
}

/// 9. AMP #2865: a busy manager turn does not lose worker results. Two background workers end
/// before anything consumes the queue; both ends are retained, one drain (the manager's next turn,
/// `Shell::take_worker_summaries` → `take_launch_ends`, #2857) returns both, and a second drain —
/// or a wait for the next end — gets nothing: no duplicate summaries.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn two_ends_before_any_consumption_are_kept_and_drained_exactly_once() {
    let f = fixture("retained-two").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(launch_config(4), Some(Arc::clone(&hooks)));

    two_workers_end_unread(&f, &sup, [3081, 3082]).await;
    // Both really finished — reported through on_finish — before anything was consumed.
    assert_eq!(hooks.reports().len(), 2, "{:?}", hooks.reports());

    // The first drain returns both, each with its report.
    let first = sup.take_launch_ends();
    assert_eq!(sorted_ids(&first), [3081, 3082], "{first:?}");
    for end in &first {
        let report = end.result.as_ref().expect("a report");
        assert_eq!(report.task_id, end.task_id);
        assert_eq!(report.outcome, WorkerOutcome::Stopped, "{report:?}");
        assert!(
            end.line().contains(&format!("#{}", end.task_id)),
            "{}",
            end.line()
        );
    }

    // A second drain is empty, and nothing is waiting to be handed out again.
    assert!(sup.take_launch_ends().is_empty(), "a drained end came back");
    assert!(
        tokio::time::timeout(Duration::from_millis(200), sup.next_launch_end())
            .await
            .is_err(),
        "a drained end was handed out again"
    );
    assert!(sup.take_launch_ends().is_empty());
}

/// 10. AMP #2865 with the #2861 display broadcast in play: a watcher sees a copy of each end, and
/// reading those copies (or never reading them) does not take anything out of the delivery queue.
/// The queue still holds both, one drain returns both, a second drain is empty, and the watcher
/// gets nothing new from the drain.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_display_broadcast_does_not_change_what_the_queue_retains() {
    let f = fixture("retained-watched").await;
    let sup = f.supervisor(launch_config(4), None);
    // Two watchers, subscribed before the workers end: one reads everything (the chat's poll),
    // one never reads at all.
    let mut shown = sup.watch_launch_ends();
    let _ignored = sup.watch_launch_ends();

    two_workers_end_unread(&f, &sup, [3091, 3092]).await;

    // The watcher got a copy of each end...
    let mut copies = Vec::new();
    while let Ok(end) = shown.try_recv() {
        copies.push(end);
    }
    assert_eq!(sorted_ids(&copies), [3091, 3092], "{copies:?}");

    // ...and the queue still delivers both, exactly once.
    let first = sup.take_launch_ends();
    assert_eq!(sorted_ids(&first), [3091, 3092], "{first:?}");
    assert!(first.iter().all(|e| e.result.is_ok()), "{first:?}");
    assert!(sup.take_launch_ends().is_empty(), "a drained end came back");

    // Draining shows nothing new to the watcher, and a watcher subscribed late sees no history.
    assert!(
        matches!(
            shown.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ),
        "the drain re-broadcast an end"
    );
    let mut late = sup.watch_launch_ends();
    assert!(matches!(
        late.try_recv(),
        Err(tokio::sync::broadcast::error::TryRecvError::Empty)
    ));
}

/// Hooks around the real run reporter whose `on_finish` panics before the reporter sees the run:
/// a launched worker task that dies mid-finish, so nothing on the normal path ends it or returns
/// its ticket (AMP #2885). Every `release` is kept for the test.
struct PanicsInFinish {
    reporter: Arc<RunReporter>,
    released: Mutex<Vec<(i64, String)>>,
}

#[async_trait::async_trait]
impl WorkerHooks for PanicsInFinish {
    fn before_tool_call(&self, worker: &WorkerInfo) -> Option<rpi_agent::BeforeToolCall> {
        self.reporter.before_tool_call(worker)
    }

    async fn on_finish(&self, _worker: &WorkerInfo, _report: &mut WorkerReport) {
        panic!("boom in on_finish");
    }

    async fn release(&self, task_id: i64, why: &str) -> Result<(), String> {
        self.released
            .lock()
            .unwrap()
            .push((task_id, why.to_owned()));
        self.reporter.release(task_id, why).await
    }
}

/// 11. AMP #2885: a launched worker task that panics after its ticket was dispatched still ends
/// exactly once — an `Err` end naming the crash, on the delivery queue and on the display
/// broadcast — and its ticket is handed back through `WorkerHooks::release` (#2802), the live
/// slot and the worker slot both let go. Before the fix the panic was swallowed: no end, the
/// ticket left in progress, and quit's `release_unfinished` blind to it (its live entry gone).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_launched_worker_that_panics_still_ends_once_and_returns_its_ticket() {
    let f = fixture("panics").await;
    let hooks = Arc::new(PanicsInFinish {
        reporter: RunReporter::new(Arc::clone(&f.rt.mcp)).with_ledger(Arc::clone(&f.ledger)),
        released: Mutex::default(),
    });
    let sup = f.supervisor_with(launch_config(1), Arc::clone(&hooks) as Arc<dyn WorkerHooks>);
    let mut shown = sup.watch_launch_ends();
    f.script(vec![step("Nothing to do.", json!([]))]);

    sup.launch(req(3101, "worker-backend"), CancellationToken::new())
        .await
        .expect("launched");
    assert_eq!(
        sup.join_launched(Duration::from_secs(30)).await,
        1,
        "the launched task ended"
    );

    // Exactly one end, an error naming the crash and where the ticket went.
    let ends = sup.take_launch_ends();
    assert_eq!(ends.len(), 1, "{ends:?}");
    let end = &ends[0];
    assert_eq!(end.task_id, 3101);
    let why = end.result.as_ref().expect_err("a crash is an Err end");
    assert!(why.contains("crashed"), "{why}");
    assert!(
        why.contains("boom in on_finish"),
        "it names the panic: {why}"
    );
    assert!(why.contains("#3101 was returned"), "{why}");
    let line = end.line();
    assert!(line.contains("#3101") && line.contains("crashed"), "{line}");
    assert!(
        !line.contains("not dispatched"),
        "it was dispatched: {line}"
    );
    assert_eq!(end.summary(), line);
    assert!(sup.take_launch_ends().is_empty(), "delivered once");

    // The display broadcast showed it too.
    let copy = shown.try_recv().expect("the chat was shown the crash");
    assert_eq!(copy.task_id, 3101);
    assert!(copy.line().contains("crashed"), "{}", copy.line());

    // #2802: handed back through the hooks, once, with the reason; the ledger let go of it.
    let released = hooks.released.lock().unwrap().clone();
    assert_eq!(released.len(), 1, "{released:?}");
    assert_eq!(released[0].0, 3101);
    assert!(released[0].1.contains("worker crashed"), "{released:?}");
    let returns = f.calls("amp_return_task");
    assert_eq!(returns.len(), 1, "{returns:?}");
    assert_eq!(returns[0]["task_id"], 3101);
    assert!(
        f.ledger.records().iter().all(|r| r.task_id != 3101),
        "{:?}",
        f.ledger.records()
    );

    // The live entry and the worker slot are free: the ticket can run again.
    assert!(live_tickets(&sup).is_empty());
    assert!(sup.unfinished().is_empty());
    assert_eq!(sup.launched_running(), 0);
    f.script(vec![step("Nothing to do.", json!([]))]);
    let again = Supervisor::launch(&sup, req(3102, "worker-backend"), CancellationToken::new())
        .await
        .expect("the single slot is free again");
    assert!(!again.queued, "the crashed run's permit was released");
    sup.join_launched(Duration::from_secs(30)).await;
}
