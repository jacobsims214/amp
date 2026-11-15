//! #4265: lifecycle safety of the manager's worker-control tools (`amp_stop_worker`,
//! `amp_restart_worker`, `amp_resume_worker`): they never dispatch a ticket twice, never complete
//! a ticket, and never replay a side effect. KB discoveries/worker-lifecycle-control-tools.md
//! section 4, cases 1-7. #4241 (tests/worker_control.rs) covers the steering end to end path;
//! replay of an open operation (Never tools get the stand-in, Safe tools re-run once, the
//! attempt limit) is in amp-core's supervisor::tests::resume (#4239).
//!
//! Runs in-process against the mock AMP with a new dispatch id per dispatch
//! (`MockAmp::with_fresh_dispatches`), so a test can tell a run's dispatch from the one before
//! it. Workers run on faux providers (offline, no paid model). Every repo, HOME and state dir is
//! a throwaway under cargo's target tmp dir (never /tmp, never the installed harness's state:
//! local_harness_safety).

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use amp_core::{
    agent::{Runtime, RuntimeOptions},
    project::ProjectMarker,
    providers::StaticKeys,
    run_report::RunReporter,
    supervisor::{
        DispatchMode, ManagerContext, RunRequest, Supervisor, SupervisorConfig, WorkerHooks,
        WorkerInfo, WorkerOutcome, WorkerReport, live_tickets,
    },
    worker_control,
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use common::{MockAmp, Running, spawn};
use rpi_agent::BeforeToolCall;
use rpi_ai::{
    providers::faux::{FauxBlock, FauxStep, faux_assistant_message},
    types::{Context, Message, StopReason},
};
use rpi_harness::types::HarnessTool;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

const CONFIG: &str = r#"
[providers.boss]
api = "faux"
[providers.boss.models."boss-1"]

[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]

[roles.manager]
model = "boss/boss-1"
[roles.worker-backend]
model = "wf/wf-1"
"#;

/// How long [`SlowFinish`] holds a run's end: the old run stays live (in the live map) this long
/// after its abort, as a worker taking its abort grace or a slow AMP would.
const SLOW_FINISH: Duration = Duration::from_millis(1500);

// ------------------------------------------------------------------------------------------------
// Fixture.
// ------------------------------------------------------------------------------------------------

struct Fixture {
    dir: PathBuf,
    repo: PathBuf,
    amp: Running,
    rt: Arc<Runtime>,
}

fn git(dir: &Path, args: &[&str]) {
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
}

/// A throwaway repo (project 2), the faux config under a throwaway HOME, and a runtime on a fresh
/// mock AMP that issues a new dispatch id per dispatch. HOME and every XDG dir point into the
/// fixture, so sessions and state never reach the installed harness's.
async fn fixture(name: &str) -> Fixture {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-worker-lifecycle")
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
    let dir = std::fs::canonicalize(&dir).unwrap();

    let amp = spawn(MockAmp::with_fresh_dispatches(), None).await;
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
            "XDG_CACHE_HOME" => Some(home.join(".cache").to_string_lossy().into_owned()),
            "XDG_DATA_HOME" => Some(home.join(".local/share").to_string_lossy().into_owned()),
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
    Fixture {
        dir,
        repo,
        amp,
        rt: Arc::new(rt),
    }
}

impl Fixture {
    fn reporter(&self) -> Arc<RunReporter> {
        RunReporter::new(Arc::clone(&self.rt.mcp))
    }

    /// A supervisor in `dispatch` mode whose hooks are `hooks` (the real run reporter, as in the
    /// chat, unless a test wraps it).
    fn supervisor_with(
        &self,
        dispatch: DispatchMode,
        max_parallel: usize,
        hooks: Arc<dyn WorkerHooks>,
    ) -> Arc<Supervisor> {
        Supervisor::with_hooks(
            Arc::clone(&self.rt),
            ManagerContext {
                role: "amp-manager".into(),
                cwd: self.repo.clone(),
                project: ProjectMarker::find_within(&self.repo, Some(&self.dir)).unwrap(),
            },
            SupervisorConfig {
                max_parallel,
                abort_grace: Duration::from_secs(5),
                dispatch,
                ..SupervisorConfig::default()
            },
            Some(hooks),
        )
    }

    /// A launch-mode supervisor (the chat's default) with the real run reporter.
    fn launcher(&self, max_parallel: usize) -> Arc<Supervisor> {
        self.supervisor_with(DispatchMode::Launch, max_parallel, self.reporter())
    }

    fn script(&self, steps: Vec<FauxStep>) {
        self.rt
            .providers
            .faux_script("wf")
            .expect("the faux worker provider")
            .set_responses(steps);
    }

    fn calls(&self, tool: &str) -> Vec<Value> {
        self.amp.mock.calls_to(tool)
    }

    fn count(&self, tool: &str) -> usize {
        self.calls(tool).len()
    }

    /// Every AMP call so far, by name, in order.
    fn all_calls(&self) -> Vec<String> {
        self.amp
            .mock
            .calls
            .lock()
            .unwrap()
            .iter()
            .map(|(name, _)| name.clone())
            .collect()
    }

    async fn wait_for_calls(&self, tool: &str, n: usize) {
        let deadline = Instant::now() + Duration::from_secs(20);
        while self.count(tool) < n {
            assert!(Instant::now() < deadline, "no {n} {tool} calls in time");
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }
}

fn req(task_id: i64) -> RunRequest {
    RunRequest {
        task_id,
        agent_id: "worker-backend".into(),
        ..RunRequest::default()
    }
}

/// The mock's `n`th dispatch id of `task_id` (1-based).
fn dispatch_id(task_id: i64, n: u32) -> String {
    format!("d_mock{task_id}-{n}")
}

/// Calls one of the manager's worker-control tools the way its model would.
async fn control_tool(sup: &Arc<Supervisor>, name: &str, args: Value) -> Value {
    let tools = worker_control::tools(sup);
    let tool = tools
        .iter()
        .find(|t| t.tool.schema().name == name)
        .unwrap_or_else(|| panic!("{name} is registered"));
    tool.tool
        .execute("call-1", args, CancellationToken::new(), Arc::new(|_| {}))
        .await
        .unwrap_or_else(|e| panic!("{name} answers: {e:?}"))
        .details
}

async fn wait_until(what: &str, mut ok: impl FnMut() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(20);
    while !ok() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

/// The next launch end for `task_id` (others are skipped).
async fn launch_end(sup: &Supervisor, task_id: i64) -> Result<WorkerReport, String> {
    loop {
        let end = tokio::time::timeout(Duration::from_secs(20), sup.next_launch_end())
            .await
            .expect("a launch end in time");
        if end.task_id == task_id {
            return end.result;
        }
    }
}

/// Samples the live map until the returned token is cancelled; the task answers the most entries
/// `task_id` ever had at once.
fn watch_live(
    sup: &Arc<Supervisor>,
    task_id: i64,
) -> (CancellationToken, tokio::task::JoinHandle<usize>) {
    let watching = CancellationToken::new();
    let task = tokio::spawn({
        let (sup, watching) = (Arc::clone(sup), watching.clone());
        async move {
            let mut most = 0;
            while !watching.is_cancelled() {
                let n = live_tickets(&sup).iter().filter(|t| **t == task_id).count();
                most = most.max(n);
                tokio::time::sleep(Duration::from_millis(2)).await;
            }
            most
        }
    });
    (watching, task)
}

// ------------------------------------------------------------------------------------------------
// Workers' faux models.
// ------------------------------------------------------------------------------------------------

fn text_of(m: &Message) -> String {
    serde_json::to_string(m).unwrap_or_default()
}

fn has_tool_result(ctx: &Context) -> bool {
    ctx.messages
        .iter()
        .any(|m| matches!(m, Message::ToolResult(_)))
}

fn bash(command: String) -> rpi_ai::types::AssistantMessage {
    faux_assistant_message(
        vec![FauxBlock::tool_call("bash", json!({ "command": command }))],
        StopReason::ToolUse,
    )
}

/// The first turn runs `sleep 30` (live until stopped); a later turn says "done".
fn sleeper() -> FauxStep {
    FauxStep::factory(|ctx: &Context| {
        if has_tool_result(ctx) {
            faux_assistant_message(vec![FauxBlock::text("done")], StopReason::Stop)
        } else {
            bash("sleep 30".to_owned())
        }
    })
}

fn sleepers(n: usize) -> Vec<FauxStep> {
    (0..n).map(|_| sleeper()).collect()
}

/// Every request the worker's model got, with what was in its context.
#[derive(Default)]
struct Log(Mutex<Vec<Vec<Message>>>);

impl Log {
    fn len(&self) -> usize {
        self.0.lock().unwrap().len()
    }
    fn get(&self, i: usize) -> Vec<Message> {
        self.0.lock().unwrap()[i].clone()
    }
}

// ------------------------------------------------------------------------------------------------
// A run reporter whose end is slow.
// ------------------------------------------------------------------------------------------------

/// The real run reporter with `on_finish` held for [`SLOW_FINISH`] first. A run leaves the live
/// map only after `on_finish` returns, so the stopped run stays live through the hold. Each
/// `on_finish` records `(task_id, dispatches of it so far)` right before it returns, which is
/// the last moment the run is live.
struct SlowFinish {
    inner: Arc<RunReporter>,
    mock: MockAmp,
    /// `(task_id, amp_dispatch_task calls for it)` at the end of each `on_finish`.
    ends: Mutex<Vec<(i64, usize)>>,
}

impl SlowFinish {
    fn dispatches_of(&self, task_id: i64) -> usize {
        self.mock
            .calls_to("amp_dispatch_task")
            .iter()
            .filter(|c| c["task_id"] == task_id)
            .count()
    }
}

#[async_trait::async_trait]
impl WorkerHooks for SlowFinish {
    fn before_tool_call(&self, worker: &WorkerInfo) -> Option<BeforeToolCall> {
        self.inner.before_tool_call(worker)
    }

    fn extra_tools(&self, worker: &WorkerInfo) -> Vec<HarnessTool> {
        self.inner.extra_tools(worker)
    }

    async fn on_finish(&self, worker: &WorkerInfo, report: &mut WorkerReport) {
        tokio::time::sleep(SLOW_FINISH).await;
        self.inner.on_finish(worker, report).await;
        let n = self.dispatches_of(worker.task_id);
        self.ends.lock().unwrap().push((worker.task_id, n));
    }

    async fn release(&self, task_id: i64, dispatch_id: &str, why: &str) -> Result<(), String> {
        self.inner.release(task_id, dispatch_id, why).await
    }

    async fn release_with_token(
        &self,
        task_id: i64,
        dispatch_id: &str,
        why: &str,
    ) -> Result<(), String> {
        self.inner
            .release_with_token(task_id, dispatch_id, why)
            .await
    }
}

// ------------------------------------------------------------------------------------------------
// Cases 1 and 2: stop, and stop again.
// ------------------------------------------------------------------------------------------------

/// Case 1: the manager stops a running worker. Exactly one `amp_report_run` (abandoned, the
/// reason in its note, no person-at-chat wording), exactly one `amp_return_task` with this run's
/// dispatch id, and no `amp_complete_task` (nor `amp_block_task`).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn case1_a_stopped_worker_is_reported_abandoned_and_returned_once() {
    const T: i64 = 4651;
    let f = fixture("case1-stop").await;
    let sup = f.launcher(2);
    f.script(sleepers(4));
    sup.launch(req(T), CancellationToken::new())
        .await
        .expect("launched");
    f.wait_for_calls("amp_claim_task", 1).await;
    assert_eq!(
        f.calls("amp_claim_task")[0]["dispatch_id"],
        dispatch_id(T, 1)
    );

    let stop = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "the plan changed"}),
    )
    .await;
    assert_eq!(stop["status"], "stopped", "{stop}");
    assert_eq!(stop["outcome"], "cancelled", "{stop}");
    assert_eq!(stop["returned"], true, "{stop}");
    assert!(live_tickets(&sup).is_empty());

    let reported = f.calls("amp_report_run");
    assert_eq!(reported.len(), 1, "{reported:?}");
    assert_eq!(reported[0]["task_id"], T);
    assert_eq!(reported[0]["outcome"], "abandoned");
    let note = reported[0]["run_note"].as_str().unwrap_or_default();
    assert!(note.contains("the plan changed"), "{note}");
    assert!(!note.contains("from the chat"), "{note}");

    let returned = f.calls("amp_return_task");
    assert_eq!(returned.len(), 1, "{returned:?}");
    assert_eq!(returned[0]["task_id"], T);
    assert_eq!(
        returned[0]["dispatch_id"],
        dispatch_id(T, 1),
        "{returned:?}"
    );

    assert_eq!(f.count("amp_complete_task"), 0);
    assert_eq!(f.count("amp_block_task"), 0);
    assert_eq!(f.count("amp_dispatch_task"), 1);
    assert_eq!(f.count("amp_set_task_state"), 0, "no fallback was needed");

    let end = launch_end(&sup, T).await.expect("a report");
    assert_eq!(end.outcome, WorkerOutcome::Cancelled);
    assert!(
        end.notes.iter().all(|n| !n.contains("from the chat")),
        "{:?}",
        end.notes
    );
}

/// Case 2: a stop while the first stop is still ending is `already_stopping`, and a stop after it
/// ended is `already_finished`; neither makes another AMP call.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn case2_a_second_stop_makes_no_more_amp_calls() {
    const T: i64 = 4652;
    let f = fixture("case2-stop-again").await;
    let hooks = Arc::new(SlowFinish {
        inner: f.reporter(),
        mock: f.amp.mock.clone(),
        ends: Mutex::default(),
    });
    let sup = f.supervisor_with(DispatchMode::Launch, 2, hooks);
    f.script(sleepers(4));
    sup.launch(req(T), CancellationToken::new())
        .await
        .expect("launched");
    f.wait_for_calls("amp_claim_task", 1).await;

    // The first stop: its run is held in its end for SLOW_FINISH.
    let first = tokio::spawn({
        let sup = Arc::clone(&sup);
        async move {
            control_tool(
                &sup,
                "amp_stop_worker",
                json!({"task_id": T, "reason": "first stop"}),
            )
            .await
        }
    });
    wait_until("the first stop took", || {
        sup.session(T).is_some_and(|s| s.stopped_by.is_some())
    })
    .await;
    let calls_before = f.all_calls();

    // Again while it is ending: already_stopping, nothing in AMP.
    assert!(sup.session(T).is_some_and(|s| s.is_live()), "still ending");
    let again = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "second stop"}),
    )
    .await;
    assert_eq!(again["status"], "already_stopping", "{again}");
    assert_eq!(f.all_calls(), calls_before, "already_stopping called AMP");

    let first = first.await.unwrap();
    assert_eq!(first["status"], "stopped", "{first}");
    assert_eq!(f.count("amp_report_run"), 1);
    assert_eq!(f.count("amp_return_task"), 1);
    let calls_after_first = f.all_calls();

    // After it ended: already_finished (with the end), nothing in AMP.
    let late = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "third stop"}),
    )
    .await;
    assert_eq!(late["status"], "already_finished", "{late}");
    assert_eq!(late["outcome"], "cancelled", "{late}");
    assert_eq!(late["returned"], true, "{late}");
    assert_eq!(
        f.all_calls(),
        calls_after_first,
        "already_finished called AMP"
    );

    // The run's note is the first stop's reason only.
    let note = f.calls("amp_report_run")[0]["run_note"]
        .as_str()
        .unwrap_or_default()
        .to_owned();
    assert!(note.contains("first stop"), "{note}");
    assert!(
        !note.contains("second stop") && !note.contains("third stop"),
        "{note}"
    );
    assert_eq!(f.count("amp_complete_task"), 0);
    assert_eq!(f.count("amp_dispatch_task"), 1);
}

// ------------------------------------------------------------------------------------------------
// Case 3: stop while queued.
// ------------------------------------------------------------------------------------------------

/// Case 3: a worker still waiting for a slot is stopped before AMP hears of it: `not_dispatched`,
/// no `amp_dispatch_task` for its ticket, and nothing reported or returned for it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn case3_a_queued_worker_stopped_is_never_dispatched() {
    const RUNNING: i64 = 4653;
    const QUEUED: i64 = 4654;
    let f = fixture("case3-stop-queued").await;
    let sup = f.launcher(1);
    f.script(sleepers(4));
    sup.launch(req(RUNNING), CancellationToken::new())
        .await
        .expect("first launched");
    f.wait_for_calls("amp_claim_task", 1).await;
    sup.launch(req(QUEUED), CancellationToken::new())
        .await
        .expect("second launched (queued for the one slot)");

    let out = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": QUEUED, "reason": "not needed"}),
    )
    .await;
    assert_eq!(out["status"], "not_dispatched", "{out}");
    let end = launch_end(&sup, QUEUED).await;
    assert!(end.is_err(), "nothing was dispatched: {end:?}");

    let of_queued = |tool: &str| {
        f.calls(tool)
            .iter()
            .filter(|c| c["task_id"] == QUEUED)
            .count()
    };
    for tool in [
        "amp_dispatch_task",
        "amp_claim_task",
        "amp_report_run",
        "amp_return_task",
        "amp_set_task_state",
        "amp_complete_task",
    ] {
        assert_eq!(of_queued(tool), 0, "{tool} for the queued ticket");
    }
    assert_eq!(f.count("amp_dispatch_task"), 1, "only the running one");

    let first = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": RUNNING, "reason": "done testing"}),
    )
    .await;
    assert_eq!(first["status"], "stopped", "{first}");
}

// ------------------------------------------------------------------------------------------------
// Case 4: restart while the old run is still ending.
// ------------------------------------------------------------------------------------------------

/// Case 4: a restart while the old run is still ending (its end held for SLOW_FINISH after the
/// abort) waits: no second `amp_dispatch_task` until the old run has left the live map (it was
/// still 1 at the old run's last live moment), the live map never holds the ticket twice, and
/// the new run claims with the new dispatch id (the old one is reported and returned with its
/// own).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn case4_restart_waits_for_the_old_run_and_claims_with_the_new_dispatch() {
    const T: i64 = 4655;
    let f = fixture("case4-restart-in-grace").await;
    let hooks = Arc::new(SlowFinish {
        inner: f.reporter(),
        mock: f.amp.mock.clone(),
        ends: Mutex::default(),
    });
    let sup = f.supervisor_with(DispatchMode::Launch, 2, Arc::clone(&hooks) as _);
    f.script(sleepers(6));
    sup.launch(req(T), CancellationToken::new())
        .await
        .expect("launched");
    f.wait_for_calls("amp_claim_task", 1).await;
    let (watching, most) = watch_live(&sup, T);

    let started = Instant::now();
    let restarted = control_tool(
        &sup,
        "amp_restart_worker",
        json!({"task_id": T, "reason": "start over"}),
    )
    .await;
    assert_eq!(restarted["status"], "restarted", "{restarted}");
    assert_eq!(restarted["stop"]["status"], "stopped", "{restarted}");
    assert!(
        started.elapsed() >= SLOW_FINISH,
        "the restart waited out the old run's end ({:?})",
        started.elapsed()
    );

    // The old run's last live moment saw one dispatch: the restart's came after it left.
    assert_eq!(
        hooks.ends.lock().unwrap().first().copied(),
        Some((T, 1)),
        "no second dispatch while the old run was live"
    );
    let old = launch_end(&sup, T).await.expect("the old run's report");
    assert_eq!(old.outcome, WorkerOutcome::Cancelled);

    // The new run claims with its own dispatch; the old one was reported and returned with its.
    f.wait_for_calls("amp_claim_task", 2).await;
    assert_eq!(f.count("amp_dispatch_task"), 2, "exactly one new dispatch");
    let claims = f.calls("amp_claim_task");
    assert_eq!(claims[0]["dispatch_id"], dispatch_id(T, 1));
    assert_eq!(claims[1]["dispatch_id"], dispatch_id(T, 2), "{claims:?}");
    let returned = f.calls("amp_return_task");
    assert_eq!(returned.len(), 1, "{returned:?}");
    assert_eq!(returned[0]["dispatch_id"], dispatch_id(T, 1));
    assert_eq!(f.count("amp_report_run"), 1);
    assert_eq!(f.count("amp_complete_task"), 0);
    // A stale claim would be refused by the mock and end the run; it is live.
    assert!(sup.session(T).is_some_and(|s| s.is_live()));

    let last = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "done testing"}),
    )
    .await;
    assert_eq!(last["status"], "stopped", "{last}");
    watching.cancel();
    assert_eq!(most.await.unwrap(), 1, "the ticket was never live twice");
    let returned = f.calls("amp_return_task");
    assert_eq!(returned.len(), 2);
    assert_eq!(returned[1]["dispatch_id"], dispatch_id(T, 2));
    assert_eq!(f.count("amp_dispatch_task"), 2);
    assert_eq!(f.count("amp_complete_task"), 0);
}

// ------------------------------------------------------------------------------------------------
// Case 5: the old run's hand-back failed.
// ------------------------------------------------------------------------------------------------

/// Case 5: when the stopped run's hand-back fails (the mock refuses `amp_return_task` for 4048,
/// and its `amp_set_task_state` fallback too), restart and resume are refused `busy` and dispatch
/// nothing while AMP still has the ticket under the old dispatch. #4304: each retry re-reads the
/// ticket (`amp_get_task`, its only AMP call), so once a person returns it in AMP the next
/// restart dispatches exactly once; and a new run's own failed hand-back is `busy` again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn case5_restart_and_resume_are_busy_when_the_hand_back_failed() {
    // The mock refuses both hand-back paths for this ticket.
    const T: i64 = 4048;
    let f = fixture("case5-hand-back-failed").await;
    let sup = f.launcher(2);
    f.script(sleepers(4));
    sup.launch(req(T), CancellationToken::new())
        .await
        .expect("launched");
    f.wait_for_calls("amp_claim_task", 1).await;

    // Restart of the live worker: its stop's hand-back fails, so no new dispatch.
    let restart = control_tool(
        &sup,
        "amp_restart_worker",
        json!({"task_id": T, "reason": "try again"}),
    )
    .await;
    assert_eq!(restart["status"], "busy", "{restart}");
    assert_eq!(
        restart["stop"]["status"], "stopped_not_returned",
        "{restart}"
    );
    assert_eq!(restart["stop"]["returned"], false, "{restart}");
    assert!(restart.get("run").is_none(), "{restart}");
    assert_eq!(
        f.count("amp_dispatch_task"),
        1,
        "the restart dispatched nothing"
    );
    assert_eq!(f.count("amp_report_run"), 1);
    assert_eq!(f.count("amp_return_task"), 1);
    assert_eq!(
        f.calls("amp_return_task")[0]["dispatch_id"],
        dispatch_id(T, 1)
    );
    assert_eq!(f.count("amp_set_task_state"), 1, "the fallback was tried");
    let calls = f.all_calls();

    // Now finished with a failed hand-back and AMP still has the ticket in progress under the
    // old dispatch: restart and both resume modes are busy. Each re-reads the ticket in AMP
    // (#4304) and makes no other AMP call.
    for (tool, args) in [
        (
            "amp_restart_worker",
            json!({"task_id": T, "reason": "again"}),
        ),
        (
            "amp_resume_worker",
            json!({"task_id": T, "mode": "continue", "reason": "again"}),
        ),
        (
            "amp_resume_worker",
            json!({"task_id": T, "mode": "replay", "reason": "again"}),
        ),
    ] {
        let out = control_tool(&sup, tool, args.clone()).await;
        assert_eq!(out["status"], "busy", "{tool} {args}: {out}");
        assert!(out.get("run").is_none(), "{out}");
        let note = out["note"].as_str().unwrap_or_default();
        assert!(note.contains("could not hand its ticket back"), "{out}");
        assert!(
            note.contains(&format!(
                "still has it in progress under the old dispatch ({})",
                dispatch_id(T, 1)
            )),
            "{out}"
        );
    }
    let mut expected = calls.clone();
    expected.extend(std::iter::repeat_n("amp_get_task".to_owned(), 3));
    assert_eq!(
        f.all_calls(),
        expected,
        "busy refusals only read the ticket"
    );
    assert_eq!(f.count("amp_dispatch_task"), 1);
    assert!(live_tickets(&sup).is_empty());

    // A person returns the ticket in AMP: the next restart goes ahead, through exactly one new
    // dispatch that the new run claims.
    f.amp.mock.return_in_ui(T);
    let restart = control_tool(
        &sup,
        "amp_restart_worker",
        json!({"task_id": T, "reason": "returned in AMP"}),
    )
    .await;
    assert_eq!(restart["status"], "restarted", "{restart}");
    assert!(restart.get("stop").is_none(), "{restart}");
    f.wait_for_calls("amp_claim_task", 2).await;
    assert_eq!(f.count("amp_dispatch_task"), 2, "exactly one new dispatch");
    assert_eq!(
        f.calls("amp_claim_task")[1]["dispatch_id"],
        dispatch_id(T, 2)
    );
    assert!(live_tickets(&sup).contains(&T));

    // The new run's hand-back fails too, and the ticket was not returned since: busy again, with
    // the new run's dispatch named, and nothing dispatched.
    let stop = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "stop again"}),
    )
    .await;
    assert_eq!(stop["status"], "stopped_not_returned", "{stop}");
    let out = control_tool(
        &sup,
        "amp_restart_worker",
        json!({"task_id": T, "reason": "not returned"}),
    )
    .await;
    assert_eq!(out["status"], "busy", "{out}");
    assert!(out.get("run").is_none(), "{out}");
    assert!(
        out["note"]
            .as_str()
            .is_some_and(|n| n.contains(&format!("under the old dispatch ({})", dispatch_id(T, 2)))),
        "{out}"
    );
    assert_eq!(
        f.count("amp_dispatch_task"),
        2,
        "the busy refusal dispatched nothing"
    );
    assert_eq!(f.count("amp_complete_task"), 0);
    assert!(live_tickets(&sup).is_empty());
}

// ------------------------------------------------------------------------------------------------
// Case 6: resume continue does not replay a side effect.
// ------------------------------------------------------------------------------------------------

/// The side-effect command's marker: in its command line and in its output.
const SIDE: &str = "SIDE-EFFECT-DONE";

/// The worker of case 6. Until its context has the side-effect call: append one line to `side`
/// and print [`SIDE`]. After it: append one line to `gcount`, touch `entered`, then wait for a
/// `gate` that never comes (a short-polling tool, live until stopped).
fn side_effect_worker(log: Arc<Log>, side: PathBuf, gcount: PathBuf, entered: PathBuf) -> FauxStep {
    FauxStep::factory(move |ctx: &Context| {
        log.0.lock().unwrap().push(ctx.messages.clone());
        let did_side = ctx
            .messages
            .iter()
            .any(|m| matches!(m, Message::Assistant(_)) && text_of(m).contains(SIDE));
        if !did_side {
            bash(format!("echo once >> '{}'; echo {SIDE}", side.display()))
        } else {
            let gate = entered.with_file_name("gate-never");
            bash(format!(
                "echo g >> '{}'; touch '{}'; while [ ! -f '{}' ]; do sleep 0.05; done",
                gcount.display(),
                entered.display(),
                gate.display()
            ))
        }
    })
}

fn lines(path: &Path) -> usize {
    std::fs::read_to_string(path)
        .map(|s| s.lines().count())
        .unwrap_or(0)
}

/// Case 6: resume `continue` after a stop. The side-effect bash call of the prior session is not
/// executed again (its file has one line), nor is the call the stop interrupted (its counter
/// goes from 1 to 2 only through the resumed model's own new call); the resumed model sees the
/// prior result in its context; and the session continued is the latest one from before the
/// build, not a new file and not an older one.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn case6_resume_continue_never_replays_a_side_effect() {
    const T: i64 = 4656;
    let f = fixture("case6-continue-no-replay").await;
    let sup = f.launcher(2);
    let log = Arc::new(Log::default());
    let (side, gcount, entered) = (
        f.dir.join("side"),
        f.dir.join("gcount"),
        f.dir.join("entered"),
    );
    f.script(
        (0..12)
            .map(|_| {
                side_effect_worker(
                    Arc::clone(&log),
                    side.clone(),
                    gcount.clone(),
                    entered.clone(),
                )
            })
            .collect(),
    );
    sup.launch(req(T), CancellationToken::new())
        .await
        .expect("launched");
    f.wait_for_calls("amp_claim_task", 1).await;
    // The side effect has finished (its result is in the session) before the stop: the worker is
    // in its second, short-polling call.
    wait_until("the worker is past its side effect", || entered.exists()).await;
    assert_eq!(lines(&side), 1);
    assert_eq!(lines(&gcount), 1);

    let stop = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "pause"}),
    )
    .await;
    assert_eq!(stop["status"], "stopped", "{stop}");
    let first = launch_end(&sup, T).await.expect("a report");
    let session = first.session_file.clone().expect("a session file");
    let sessions_dir = session.parent().unwrap().to_owned();
    // An older session of the same ticket, named to sort first: never the one continued.
    let decoy = sessions_dir.join("0000000000001-decoy.jsonl");
    std::fs::write(&decoy, "").unwrap();
    std::fs::remove_file(&entered).unwrap();

    let before = log.len();
    // The stopped run's session lock can outlive its live entry for a moment; the resume waits
    // for it (#4281), so it is never `busy` here.
    let resumed = control_tool(
        &sup,
        "amp_resume_worker",
        json!({"task_id": T, "mode": "continue", "reason": "carry on"}),
    )
    .await;
    assert_eq!(resumed["status"], "resumed", "{resumed}");
    assert_eq!(resumed["mode"], "continue", "{resumed}");
    f.wait_for_calls("amp_claim_task", 2).await;
    assert_eq!(
        f.calls("amp_claim_task")[1]["dispatch_id"],
        dispatch_id(T, 2)
    );
    wait_until("the resumed worker's own call", || entered.exists()).await;

    // Nothing from the prior session ran again.
    assert_eq!(lines(&side), 1, "the side effect ran again");
    assert_eq!(
        lines(&gcount),
        2,
        "only the resumed model's new call ran (the interrupted one was not replayed)"
    );
    // The resumed model has the prior side effect's result in its context.
    let continued = log.get(before);
    assert!(
        continued
            .iter()
            .any(|m| matches!(m, Message::ToolResult(_)) && text_of(m).contains(SIDE)),
        "the continued session carries the side effect's result"
    );

    let last = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "done testing"}),
    )
    .await;
    assert_eq!(last["status"], "stopped", "{last}");
    let resumed_end = launch_end(&sup, T).await.expect("the resumed run's report");
    assert_eq!(
        resumed_end.session_file.as_ref(),
        Some(&session),
        "continue used the latest session from before the build"
    );
    assert_ne!(resumed_end.session_file.as_ref(), Some(&decoy));
    assert_eq!(lines(&side), 1);
    assert_eq!(f.count("amp_dispatch_task"), 2);
    assert_eq!(f.count("amp_report_run"), 2);
    assert_eq!(f.count("amp_return_task"), 2);
    assert_eq!(f.count("amp_complete_task"), 0);
}

// ------------------------------------------------------------------------------------------------
// Case 7: replay after a normal stop.
// ------------------------------------------------------------------------------------------------

/// Case 7: resume `replay` after a normal stop is refused `nothing_to_replay`. A normal abort
/// leaves no open operation, so there is nothing to replay. The refusal comes after the
/// dispatch and before the claim (Await mode, so the tool's own result says so): the new
/// dispatch is never claimed, the ticket is handed back with that dispatch, the worker's model
/// is not called again, and the stopped run's bash call does not run again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn case7_replay_after_a_normal_stop_is_nothing_to_replay() {
    const T: i64 = 4657;
    let f = fixture("case7-replay-refused").await;
    let sup = f.supervisor_with(DispatchMode::Await, 2, f.reporter());
    let log = Arc::new(Log::default());
    let ran = f.dir.join("ran");
    f.script(
        (0..6)
            .map(|_| {
                let (log, ran) = (Arc::clone(&log), ran.clone());
                FauxStep::factory(move |ctx: &Context| {
                    log.0.lock().unwrap().push(ctx.messages.clone());
                    bash(format!("echo r >> '{}'; sleep 30", ran.display()))
                })
            })
            .collect(),
    );
    let run = tokio::spawn({
        let sup = Arc::clone(&sup);
        async move { sup.run_ticket(req(T), CancellationToken::new()).await }
    });
    f.wait_for_calls("amp_claim_task", 1).await;
    wait_until("the worker's bash call ran", || lines(&ran) == 1).await;

    let stop = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "pause"}),
    )
    .await;
    assert_eq!(stop["status"], "stopped", "{stop}");
    let report = run.await.unwrap().expect("a report");
    assert_eq!(report.outcome, WorkerOutcome::Cancelled);
    let model_calls = log.len();

    // The killed `sleep 30` leaves the session locked for a moment after the run; the resume
    // waits for it (#4281), so it is never `busy` here.
    let replay = control_tool(
        &sup,
        "amp_resume_worker",
        json!({"task_id": T, "mode": "replay", "reason": "replay it"}),
    )
    .await;
    assert_eq!(replay["status"], "nothing_to_replay", "{replay}");
    assert_eq!(replay["mode"], "replay", "{replay}");

    // Dispatched once more, never claimed, handed back with the new dispatch.
    assert_eq!(f.count("amp_dispatch_task"), 2);
    assert_eq!(
        f.count("amp_claim_task"),
        1,
        "the refused replay never started"
    );
    let returned = f.calls("amp_return_task");
    assert_eq!(
        returned.last().unwrap()["dispatch_id"],
        dispatch_id(T, 2),
        "{returned:?}"
    );
    assert_eq!(returned.len(), 2, "{returned:?}");
    assert_eq!(f.count("amp_complete_task"), 0);
    // No replay of anything: no model call and the bash call did not run again.
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert_eq!(log.len(), model_calls, "the model was called again");
    assert_eq!(lines(&ran), 1, "the stopped run's bash call ran again");
    assert!(live_tickets(&sup).is_empty());
}
