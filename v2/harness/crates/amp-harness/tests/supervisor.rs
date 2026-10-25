//! M2-5 (#2096): the worker supervisor and `amp_run_ticket`, in-process against the mock AMP, on
//! faux providers (offline, deterministic). The manager runs on one faux provider and the workers
//! on another, so each side's model is visible. Every repo is a throwaway git repo under cargo's
//! target tmp dir (never /tmp: D13).
//!
//! Covers: dispatch → worktree → worker on its own model → result back to the manager; unknown
//! and non-dispatchable agents and AMP refusals; the parallel cap; the wall-clock timeout; the
//! turn budget; cancelling the manager cancels its worker; `cancel_all` cancels running and
//! queued workers.

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use amp_core::{
    agent::{AgentSpec, Runtime, RuntimeOptions},
    events::{ChannelEmitter, RunEvent},
    faux::StepSpec,
    project::ProjectMarker,
    providers::StaticKeys,
    supervisor::{
        ManagerContext, RunRequest, RunTicketError, Supervisor, SupervisorConfig, WorkerHooks,
        WorkerInfo, WorkerOutcome, WorkerReport, live_tickets,
    },
    workspace::{Isolation, Placement},
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use common::{MockAmp, Running, spawn};
use rpi_ai::{
    providers::faux::{FauxBlock, FauxStep, faux_assistant_message},
    types::{Context, Message, StopReason},
};
use rpi_harness::agent_harness::{AgentLane, HarnessRunOutcome};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

/// The manager on provider `boss`, workers on provider `wf`: two faux providers, two models.
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
[roles.researcher]
model = "wf/wf-1"
"#;

struct Fixture {
    dir: PathBuf,
    repo: PathBuf,
    amp: Running,
    rt: Arc<Runtime>,
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

/// A throwaway git repo with `.amp.json` (project 2) and one commit, the faux config, and a
/// runtime connected to a fresh mock AMP.
async fn fixture(name: &str) -> Fixture {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-supervisor")
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
    Fixture {
        dir,
        repo,
        amp,
        rt: Arc::new(rt),
    }
}

impl Fixture {
    fn manager(&self) -> ManagerContext {
        ManagerContext {
            role: "amp-manager".into(),
            cwd: self.repo.clone(),
            project: ProjectMarker::find_within(&self.repo, Some(&self.dir)).unwrap(),
        }
    }

    fn supervisor(&self, config: SupervisorConfig, hooks: Arc<Recorder>) -> Arc<Supervisor> {
        Supervisor::with_hooks(
            Arc::clone(&self.rt),
            self.manager(),
            config,
            Some(hooks as Arc<dyn WorkerHooks>),
        )
    }

    fn script(&self, provider: &str, steps: Vec<FauxStep>) {
        self.rt
            .providers
            .faux_script(provider)
            .expect("a faux provider")
            .set_responses(steps);
    }

    fn dispatched(&self) -> Vec<Value> {
        self.amp.mock.calls_to("amp_dispatch_task")
    }
}

/// Records every finished worker (the M2-6 seam).
#[derive(Default)]
struct Recorder {
    finished: Mutex<Vec<(WorkerInfo, WorkerReport)>>,
}

#[async_trait::async_trait]
impl WorkerHooks for Recorder {
    async fn on_finish(&self, worker: &WorkerInfo, report: &mut WorkerReport) {
        self.finished
            .lock()
            .unwrap()
            .push((worker.clone(), report.clone()));
    }
}

impl Recorder {
    fn reports(&self) -> Vec<WorkerReport> {
        self.finished
            .lock()
            .unwrap()
            .iter()
            .map(|(_, r)| r.clone())
            .collect()
    }
}

fn config() -> SupervisorConfig {
    SupervisorConfig {
        abort_grace: Duration::from_secs(5),
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

/// A worker step for a shared queue: the first turn runs `command`, later turns say "done". Each
/// worker decides from its own context, so any worker can take any step.
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

fn template(text: &str, tool_calls: Value) -> FauxStep {
    let spec: StepSpec =
        serde_json::from_value(json!({"text": text, "tool_calls": tool_calls})).unwrap();
    spec.to_step()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn manager_dispatches_a_worker_on_its_own_model_in_its_own_worktree() {
    let f = fixture("dispatch").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(config(), Arc::clone(&hooks));

    // The tool lists every agent the manager may run, with its description.
    let tool = sup.tool();
    let description = tool.tool.schema().description.clone();
    let dispatchable = sup.dispatchable();
    assert_eq!(dispatchable.len(), 10, "{description}");
    for role in &dispatchable {
        assert!(
            description.contains(&format!("- amp:{}: ", role.id)),
            "{} missing:\n{description}",
            role.id
        );
    }
    assert!(!description.contains("amp:amp-manager"), "{description}");

    f.script(
        "boss",
        vec![
            FauxStep::tool_call(
                "amp_run_ticket",
                json!({"task_id": 2085, "agent_id": "amp:amp-worker-backend"}),
            ),
            template("Worker result: {{last_tool_result}}", json!([])),
        ],
    );
    f.script(
        "wf",
        vec![
            template(
                "Checking where I am.",
                json!([{"name": "bash", "arguments": {"command": "pwd && git branch --show-current"}}]),
            ),
            template(
                "I am in {{last_tool_result}}",
                json!([{"name": "amp_complete_task", "arguments": {"task_id": 2085}}]),
            ),
            template("Finished #2085.", json!([])),
        ],
    );

    let (emitter, mut rx) = ChannelEmitter::new();
    let mut spec = AgentSpec::new("amp-manager", f.repo.clone());
    spec.project = f.manager().project;
    spec.extra_tools = vec![tool];
    spec.emitter = Some(emitter.into_emitter());
    let manager = f.rt.build(spec).await.expect("build the manager");
    assert!(manager.tool_names.iter().any(|t| t == "amp_run_ticket"));

    let result = tokio::time::timeout(
        Duration::from_secs(60),
        manager.harness.prompt_text("run #2085", Vec::new()),
    )
    .await
    .expect("the manager finished")
    .expect("the run");
    let HarnessRunOutcome::Completed { final_message, .. } = result.outcome else {
        panic!("{:?}", result.outcome)
    };
    let text = amp_core::events::assistant_text(&final_message);

    // AMP got the dispatch, in AMP's agent id form.
    assert_eq!(
        f.dispatched(),
        vec![json!({"task_id": 2085, "agent_id": "amp:amp-worker-backend"})]
    );
    assert_eq!(f.amp.mock.calls_to("amp_complete_task").len(), 1);

    // The worker ran in its own worktree, on its own branch, inside the manager's repo.
    let worktree = f.repo.join(".worktrees/2085");
    assert!(worktree.is_dir());
    assert_eq!(git(&worktree, &["branch", "--show-current"]), "amp-2085");
    assert_eq!(
        git(&f.repo, &["status", "--porcelain"]),
        "",
        "worktrees are excluded"
    );

    let reports = hooks.reports();
    assert_eq!(reports.len(), 1);
    let r = &reports[0];
    assert_eq!(r.outcome, WorkerOutcome::Completed, "{r:?}");
    assert_eq!(r.model, "wf/wf-1");
    assert_eq!(r.role, "amp-worker-backend");
    assert_eq!(r.agent_id, "amp:amp-worker-backend");
    assert_eq!(r.worktree.as_deref(), Some(worktree.as_path()));
    assert_eq!(r.cwd, worktree);
    assert_eq!(r.branch.as_deref(), Some("amp-2085"));
    assert_eq!(r.final_message, "Finished #2085.");
    assert_eq!(r.turns, 3);
    assert_eq!(r.tool_calls, 2);
    assert!(r.usage.output_tokens > 0, "{:?}", r.usage);
    let session = r.session_file.as_ref().expect("a JSONL session");
    assert!(
        session.starts_with(f.dir.join("home/.local/state/amp-harness/sessions/2085")),
        "{}",
        session.display()
    );
    // The worker's bash ran in the worktree: its own transcript shows it.
    let transcript = std::fs::read_to_string(session).unwrap();
    assert!(
        transcript.contains(&format!("I am in {}\\namp-2085", worktree.display())),
        "{transcript}"
    );
    assert!(transcript.contains("wf-1"), "the worker's own model");

    // The manager ran on its model and got the worker's report as the tool result.
    assert!(text.contains("\"outcome\": \"completed\""), "{text}");
    assert!(text.contains("Finished #2085."), "{text}");
    let mut manager_models = Vec::new();
    while let Ok(ev) = rx.try_recv() {
        // (rpi's faux provider names itself "faux" in messages, so compare the model.)
        if let Some(RunEvent::Assistant { model, .. }) = RunEvent::from_agent(&ev) {
            manager_models.push(model);
        }
    }
    assert_eq!(manager_models, ["boss-1", "boss-1"]);
    assert!(live_tickets(&sup).is_empty());

    // Running the ticket again reuses its worktree.
    f.script("wf", vec![template("Again.", json!([]))]);
    let again = sup
        .run_ticket(req(2085, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(again.outcome, WorkerOutcome::Stopped);
    assert_eq!(again.cwd, worktree);
    assert!(
        again.notes.iter().any(|n| n.contains("reused")),
        "{again:?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn unknown_and_undispatchable_agents_and_refused_tickets_start_nothing() {
    let f = fixture("refusals").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(config(), Arc::clone(&hooks));
    let none = CancellationToken::new();

    // amp-* matches the manager's policy, but no such role exists.
    let e = sup
        .run_ticket(req(1, "amp:amp-ghost"), none.clone())
        .await
        .unwrap_err();
    assert!(matches!(e, RunTicketError::UnknownAgent { .. }), "{e}");
    assert!(e.to_string().contains("amp:amp-worker-backend"), "{e}");
    // A role that exists but is not a child the manager may dispatch.
    let e = sup
        .run_ticket(req(1, "amp:amp-manager"), none.clone())
        .await
        .unwrap_err();
    assert!(matches!(e, RunTicketError::NotDispatchable { .. }), "{e}");
    assert!(f.dispatched().is_empty());

    // AMP refuses the dispatch: no worker, no worktree.
    let e = sup
        .run_ticket(req(4040, "amp:amp-worker-backend"), none.clone())
        .await
        .unwrap_err();
    assert!(
        matches!(e, RunTicketError::Dispatch { task_id: 4040, .. }),
        "{e}"
    );
    assert!(e.to_string().contains("owned by someone else"), "{e}");
    assert!(!f.repo.join(".worktrees/4040").exists());
    assert!(hooks.reports().is_empty());

    // Read-only roles run in the manager's directory, without a worktree.
    f.script("wf", vec![template("Nothing to change.", json!([]))]);
    let r = sup
        .run_ticket(req(7, "researcher"), none.clone())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Stopped);
    assert_eq!(r.cwd, f.repo);
    assert!(r.worktree.is_none());
    assert!(!f.repo.join(".worktrees/7").exists());

    // A failure after dispatch still ends in a report (M2-6 records it as abandoned).
    f.script("wf", vec![]);
    let r = sup
        .run_ticket(
            RunRequest {
                worktree: Some(false),
                ..req(8, "amp:amp-worker-backend")
            },
            none,
        )
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Error, "{r:?}");
    assert!(
        r.error.as_deref().unwrap_or_default().contains("faux"),
        "{r:?}"
    );
    assert_eq!(hooks.reports().len(), 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn at_most_max_parallel_workers_run_at_once() {
    let f = fixture("parallel").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(
        SupervisorConfig {
            max_parallel: 2,
            // #2364: a folder each (no worktrees to make); `worktree: false` would make editing
            // workers take turns in the manager's folder.
            isolation: Isolation::Folder,
            ..config()
        },
        Arc::clone(&hooks),
    );
    f.script("wf", (0..20).map(|_| bash_then_done("sleep 0.4")).collect());

    let started = Instant::now();
    let mut set = tokio::task::JoinSet::new();
    for id in 101..=105 {
        let sup = Arc::clone(&sup);
        set.spawn(async move {
            sup.run_ticket(req(id, "amp:amp-worker-backend"), CancellationToken::new())
                .await
        });
    }
    let mut outcomes = Vec::new();
    while let Some(r) = set.join_next().await {
        outcomes.push(r.unwrap().unwrap().outcome);
    }
    let elapsed = started.elapsed();

    assert_eq!(outcomes, vec![WorkerOutcome::Stopped; 5]);
    assert_eq!(sup.peak_running(), 2, "the cap held");
    // 5 workers, 2 at a time, ~0.4 s each: at least 3 rounds.
    assert!(elapsed >= Duration::from_millis(1200), "{elapsed:?}");
    assert_eq!(f.dispatched().len(), 5);
    assert!(sup.workers().is_empty());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_worker_past_its_timeout_is_stopped() {
    let f = fixture("timeout").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(
        SupervisorConfig {
            timeout: Duration::from_millis(800),
            ..config()
        },
        Arc::clone(&hooks),
    );
    f.script(
        "wf",
        vec![bash_then_done("sleep 30"), bash_then_done("sleep 30")],
    );

    let started = Instant::now();
    let r = sup
        .run_ticket(
            RunRequest {
                worktree: Some(false),
                ..req(201, "worker-backend")
            },
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Timeout, "{r:?}");
    assert!(r.error.as_deref().unwrap().contains("800 ms"), "{r:?}");
    assert!(
        started.elapsed() < Duration::from_secs(10),
        "the sleep was killed"
    );
    assert_eq!(hooks.reports().len(), 1);
    assert!(live_tickets(&sup).is_empty());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_worker_over_its_turn_budget_is_stopped() {
    let f = fixture("turns").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(
        SupervisorConfig {
            max_turns: 3,
            ..config()
        },
        Arc::clone(&hooks),
    );
    // A worker that never stops calling tools.
    let looping = || {
        FauxStep::factory(|_: &Context| {
            faux_assistant_message(
                vec![FauxBlock::tool_call("bash", json!({"command": "true"}))],
                StopReason::ToolUse,
            )
        })
    };
    f.script("wf", (0..20).map(|_| looping()).collect());
    let r = sup
        .run_ticket(
            RunRequest {
                worktree: Some(false),
                ..req(301, "worker-backend")
            },
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::TurnBudget, "{r:?}");
    assert!(r.turns >= 4 && r.turns <= 5, "{r:?}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn aborting_the_manager_cancels_its_worker() {
    let f = fixture("cancel-manager").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(config(), Arc::clone(&hooks));
    f.script(
        "boss",
        vec![
            FauxStep::tool_call(
                "amp_run_ticket",
                json!({"task_id": 401, "agent_id": "amp:amp-worker-backend", "worktree": false}),
            ),
            FauxStep::text("unreachable"),
        ],
    );
    f.script(
        "wf",
        vec![bash_then_done("sleep 30"), bash_then_done("sleep 30")],
    );

    let mut spec = AgentSpec::new("amp-manager", f.repo.clone());
    spec.extra_tools = vec![sup.tool()];
    let manager = f.rt.build(spec).await.unwrap();
    let harness = Arc::clone(&manager.harness);
    let run = tokio::spawn(async move { harness.prompt_text("run #401", Vec::new()).await });

    // Wait until the worker is running, then abort the manager.
    let deadline = Instant::now() + Duration::from_secs(20);
    while sup.workers().is_empty() || sup.workers()[0].totals.tool_calls == 0 {
        assert!(Instant::now() < deadline, "the worker never started");
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let started = Instant::now();
    manager.harness.abort().await.unwrap();
    let result = tokio::time::timeout(Duration::from_secs(20), run)
        .await
        .expect("the manager stopped")
        .unwrap()
        .unwrap();
    // rpi ends an aborted run during tool execution as Aborted or as Completed on the tool-use
    // message; either way the manager never took another turn.
    assert!(
        !matches!(result.outcome, HarnessRunOutcome::Failed { .. }),
        "{:?}",
        result.outcome
    );
    assert_eq!(
        f.rt.providers.faux_script("boss").unwrap().pending_count(),
        1,
        "the manager stopped after the abort"
    );
    assert!(started.elapsed() < Duration::from_secs(10));

    // The worker's run ended as cancelled (the tool call may finish just after the manager).
    let deadline = Instant::now() + Duration::from_secs(10);
    while hooks.reports().is_empty() {
        assert!(Instant::now() < deadline, "no report for the worker");
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let r = &hooks.reports()[0];
    assert_eq!(r.outcome, WorkerOutcome::Cancelled, "{r:?}");
    assert_eq!(r.task_id, 401);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn cancel_all_stops_running_workers_and_queued_ones_never_dispatch() {
    let f = fixture("cancel-all").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(
        SupervisorConfig {
            max_parallel: 1,
            ..config()
        },
        Arc::clone(&hooks),
    );
    f.script("wf", (0..4).map(|_| bash_then_done("sleep 30")).collect());

    let spawn_run = |id: i64| {
        let sup = Arc::clone(&sup);
        tokio::spawn(async move {
            sup.run_ticket(
                RunRequest {
                    worktree: Some(false),
                    ..req(id, "worker-backend")
                },
                CancellationToken::new(),
            )
            .await
        })
    };
    let running = spawn_run(501);
    let deadline = Instant::now() + Duration::from_secs(20);
    while f.dispatched().is_empty() {
        assert!(Instant::now() < deadline, "the first worker never started");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let queued = spawn_run(502);
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert_eq!(live_tickets(&sup).len(), 2, "one running, one queued");

    sup.cancel_all();
    let r = tokio::time::timeout(Duration::from_secs(20), running)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Cancelled);
    let q = tokio::time::timeout(Duration::from_secs(20), queued)
        .await
        .unwrap()
        .unwrap();
    assert!(matches!(q, Err(RunTicketError::Cancelled)), "{q:?}");
    assert_eq!(
        f.dispatched().len(),
        1,
        "the queued ticket was never dispatched"
    );
    assert!(live_tickets(&sup).is_empty());
}

/// #2250: the manager's and a supervised worker's requests each carry the current time (the
/// scripts echo the newest stamp they were sent), and neither transcript keeps the stamp.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn manager_and_worker_requests_carry_the_current_time_but_transcripts_do_not() {
    let f = fixture("clock").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(config(), Arc::clone(&hooks));
    f.script(
        "boss",
        vec![
            template(
                "m1={{current_time}}",
                json!([{"name": "amp_run_ticket", "arguments": {"task_id": 2085, "agent_id": "amp:amp-worker-backend"}}]),
            ),
            template("m2={{current_time}}", json!([])),
        ],
    );
    f.script(
        "wf",
        vec![
            template(
                "w1={{current_time}}",
                json!([{"name": "bash", "arguments": {"command": "true"}}]),
            ),
            template("w2={{current_time}}", json!([])),
        ],
    );

    let (emitter, mut rx) = ChannelEmitter::new();
    let mut spec = AgentSpec::new("amp-manager", f.repo.clone());
    spec.project = f.manager().project;
    spec.extra_tools = vec![sup.tool()];
    spec.emitter = Some(emitter.into_emitter());
    let manager = f.rt.build(spec).await.expect("build the manager");
    let result = tokio::time::timeout(
        Duration::from_secs(60),
        manager.harness.prompt_text("run #2085", Vec::new()),
    )
    .await
    .expect("the manager finished")
    .expect("the run");
    assert!(matches!(
        result.outcome,
        HarnessRunOutcome::Completed { .. }
    ));

    let mut manager_text = String::new();
    while let Ok(ev) = rx.try_recv() {
        if let Some(RunEvent::Assistant { text, .. }) = RunEvent::from_agent(&ev) {
            manager_text.push_str(&text);
            manager_text.push('\n');
        }
    }
    let reports = hooks.reports();
    assert_eq!(reports.len(), 1, "{manager_text}");
    let worker = std::fs::read_to_string(reports[0].session_file.as_ref().unwrap()).unwrap();
    // Each turn saw a stamp (the prompt's, then the tool result's): never "none".
    for (label, text) in [
        ("m1=", &manager_text),
        ("m2=", &manager_text),
        ("w1=", &worker),
        ("w2=", &worker),
    ] {
        // The last occurrence: the whole message (the JSONL also has the streamed deltas).
        let at = text
            .rfind(label)
            .unwrap_or_else(|| panic!("no {label} in {text}"));
        let stamp = &text[at + label.len()..];
        assert!(
            !stamp.starts_with("none") && stamp[..80.min(stamp.len())].contains(" UTC)"),
            "{label}: {stamp}"
        );
    }
    assert!(!worker.contains("<current_time>"), "{worker}");
    assert!(!manager_text.contains("<current_time>"), "{manager_text}");
}

/// #2383: the chat started with no project and the manager then wrote `.amp.json`; the chat
/// moves the supervisor onto it ([`Supervisor::set_project`]), so a worker dispatched afterwards
/// gets the project in its prompt and its `<amp_context>`.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn workers_dispatched_after_a_relink_get_the_new_project() {
    let f = fixture("relink").await;
    let hooks = Arc::new(Recorder::default());
    let sup = Supervisor::with_hooks(
        Arc::clone(&f.rt),
        ManagerContext {
            project: None,
            ..f.manager()
        },
        config(),
        Some(Arc::clone(&hooks) as Arc<dyn WorkerHooks>),
    );
    assert_eq!(sup.project(), None);
    sup.set_project(f.manager().project);
    assert_eq!(sup.project().map(|p| p.project_id), Some(2));
    f.script(
        "boss",
        vec![
            template(
                "",
                json!([{"name": "amp_run_ticket", "arguments": {"task_id": 2085, "agent_id": "amp:amp-worker-backend"}}]),
            ),
            template("done", json!([])),
        ],
    );
    f.script(
        "wf",
        vec![template(
            "wp={{context:project_id}} wn={{context:project_name}}",
            json!([]),
        )],
    );
    let mut spec = AgentSpec::new("amp-manager", f.repo.clone());
    spec.extra_tools = vec![sup.tool()];
    let manager = f.rt.build(spec).await.expect("build the manager");
    tokio::time::timeout(
        Duration::from_secs(60),
        manager.harness.prompt_text("run #2085", Vec::new()),
    )
    .await
    .expect("the manager finished")
    .expect("the run");
    let reports = hooks.reports();
    assert_eq!(reports.len(), 1);
    let worker = std::fs::read_to_string(reports[0].session_file.as_ref().unwrap()).unwrap();
    assert!(worker.contains("wp=2 wn=AMP v2"), "{worker}");
    assert!(worker.contains("#2085 in project 2."), "{worker}");
}

/// #2356: a manager in a new folder that the surrounding repository doesn't track, or in a
/// repository with no commit yet, can't give a worker a useful worktree (it would be a checkout of
/// the other repository, or there is nothing to branch from). The worker runs in the folder
/// itself, and its report says why and how to get worktrees. After a commit, worktrees work.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn workers_run_in_place_without_a_usable_repository_and_say_why() {
    let f = fixture("in-place").await;
    let hooks = Arc::new(Recorder::default());
    let child = f.repo.join("new-folder");
    std::fs::create_dir_all(&child).unwrap();
    let sup = Supervisor::with_hooks(
        Arc::clone(&f.rt),
        ManagerContext {
            role: "amp-manager".into(),
            cwd: child.clone(),
            project: f.manager().project,
        },
        config(),
        Some(Arc::clone(&hooks) as Arc<dyn WorkerHooks>),
    );
    let run = |task_id: i64| {
        f.script("wf", vec![template("Done here.", json!([]))]);
        sup.run_ticket(
            req(task_id, "amp:amp-worker-backend"),
            CancellationToken::new(),
        )
    };

    // Untracked by the parent repository: not a worktree of the parent, a folder of its own
    // (#2364: `auto` outside a usable repository).
    let r = run(11).await.unwrap();
    assert_eq!(r.cwd, child.join("amp-work/11"), "{r:?}");
    assert_eq!(r.workspace, Placement::Folder, "{r:?}");
    assert!(r.worktree.is_none(), "{r:?}");
    assert!(!f.repo.join(".worktrees/11").exists());
    assert!(
        r.notes.iter().any(|n| n.contains(&format!(
            "{} is inside the git repository {} but none of its files are in it",
            child.display(),
            f.repo.display()
        )) && n.contains("run `git init` and commit once in")),
        "{r:?}"
    );

    // Its own repository, no commit yet.
    git(&child, &["init", "-q", "-b", "main"]);
    let r = run(12).await.unwrap();
    assert_eq!(r.cwd, child.join("amp-work/12"), "{r:?}");
    assert!(r.worktree.is_none(), "{r:?}");
    assert!(
        r.notes
            .iter()
            .any(|n| n.contains("has no commit yet, so there is nothing to branch from")),
        "{r:?}"
    );

    // After one commit: a worktree of the folder's own repository.
    std::fs::write(child.join("README.md"), "child\n").unwrap();
    git(&child, &["add", "."]);
    git(&child, &["commit", "-q", "-m", "init"]);
    let r = run(13).await.unwrap();
    assert_eq!(
        r.worktree.as_deref(),
        Some(child.join(".worktrees/13").as_path())
    );
    assert!(r.notes.is_empty(), "{r:?}");
}

/// The ticket a worker works, from its prompt ("You are working AMP ticket #N").
fn ticket_of(ctx: &Context) -> String {
    let all = serde_json::to_string(&ctx.messages).unwrap();
    let at = all.find("AMP ticket #").expect("a worker prompt") + "AMP ticket #".len();
    all[at..].chars().take_while(char::is_ascii_digit).collect()
}

/// #2364: a worker step for a shared queue that writes `out.md` (relative to its working
/// directory) naming its ticket, waits a moment so the other worker overlaps, then completes.
/// Every system prompt and first prompt it saw is kept in `seen`.
fn write_then_done(seen: Arc<Mutex<Vec<String>>>) -> FauxStep {
    FauxStep::factory(move |ctx: &Context| {
        let ticket = ticket_of(ctx);
        if has_tool_result(ctx) {
            faux_assistant_message(vec![FauxBlock::text("done")], StopReason::Stop)
        } else {
            seen.lock().unwrap().push(format!(
                "{}\n{}",
                ctx.system_prompt.clone().unwrap_or_default(),
                serde_json::to_string(&ctx.messages).unwrap()
            ));
            faux_assistant_message(
                vec![
                    FauxBlock::tool_call(
                        "write",
                        json!({"path": "out.md", "content": format!("ticket {ticket}\n")}),
                    ),
                    FauxBlock::tool_call("bash", json!({"command": "sleep 1"})),
                ],
                StopReason::ToolUse,
            )
        }
    })
}

/// #2364: `isolation = "folder"` (what `auto` gives outside a usable git repository): two
/// workers that edit files run at once, each in its own folder `amp-work/<ticket>/`, so the same
/// relative file never collides, and neither is told to commit; the manager is told not to plan
/// git steps. `shared` instead runs editing workers one at a time in the manager's folder.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn outside_git_worktrees_parallel_workers_get_their_own_folders_and_no_git_steps() {
    let f = fixture("folders").await;
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(
        SupervisorConfig {
            isolation: Isolation::Folder,
            ..config()
        },
        Arc::clone(&hooks),
    );
    let ctx = sup.workspace_context();
    assert!(
        ctx.starts_with("workers_workspace: isolation = folder"),
        "{ctx}"
    );
    assert!(ctx.contains("don't plan those steps"), "{ctx}");
    assert!(sup.workspace_banner().is_some());
    let desc = sup.tool().tool.schema().description.clone();
    assert!(desc.contains("a folder amp-work/<task_id>/"), "{desc}");

    let seen = Arc::new(Mutex::new(Vec::new()));
    f.script(
        "wf",
        (0..4).map(|_| write_then_done(Arc::clone(&seen))).collect(),
    );
    let none = CancellationToken::new();
    let (a, b) = tokio::join!(
        sup.run_ticket(req(21, "amp:amp-worker-backend"), none.clone()),
        sup.run_ticket(req(22, "amp:amp-worker-backend"), none.clone()),
    );
    let (a, b) = (a.unwrap(), b.unwrap());
    assert_eq!(sup.peak_running(), 2, "they ran at the same time");
    for (r, id) in [(&a, 21), (&b, 22)] {
        let folder = f.repo.join(format!("amp-work/{id}"));
        assert_eq!(r.cwd, folder, "{r:?}");
        assert_eq!(r.workspace, Placement::Folder);
        assert!(r.worktree.is_none() && r.branch.is_none(), "{r:?}");
        assert_eq!(
            std::fs::read_to_string(folder.join("out.md")).unwrap(),
            format!("ticket {id}\n"),
            "each worker's file is its own"
        );
    }
    assert!(!f.repo.join("out.md").exists());
    assert!(!f.repo.join(".worktrees").exists());
    let seen = seen.lock().unwrap().clone();
    assert_eq!(seen.len(), 2);
    for prompt in &seen {
        assert!(prompt.contains("workspace: folder "), "{prompt}");
        assert!(
            prompt.contains("don't commit, branch, push or open a pull request"),
            "{prompt}"
        );
        assert!(!prompt.contains("commit your work"), "{prompt}");
    }

    // Shared: the manager's folder, editing workers one at a time.
    let hooks = Arc::new(Recorder::default());
    let sup = f.supervisor(
        SupervisorConfig {
            isolation: Isolation::Shared,
            ..config()
        },
        Arc::clone(&hooks),
    );
    let seen = Arc::new(Mutex::new(Vec::new()));
    f.script(
        "wf",
        (0..4).map(|_| write_then_done(Arc::clone(&seen))).collect(),
    );
    let (a, b) = tokio::join!(
        sup.run_ticket(req(23, "amp:amp-worker-backend"), none.clone()),
        sup.run_ticket(req(24, "amp:amp-worker-backend"), none.clone()),
    );
    let (a, b) = (a.unwrap(), b.unwrap());
    assert_eq!(sup.peak_running(), 1, "editing workers took turns");
    for r in [&a, &b] {
        assert_eq!(r.cwd, f.repo, "{r:?}");
        assert_eq!(r.workspace, Placement::Shared);
    }
    let seen = seen.lock().unwrap().clone();
    assert!(
        seen.iter()
            .all(|p| p.contains("workspace: shared ") && p.contains("git: not yours here")),
        "{seen:?}"
    );
}

/// #2364: `isolation = "worktree"` refuses a folder without a usable repository before anything
/// happens in AMP, and says how to fix it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn worktree_isolation_refuses_a_folder_without_a_usable_repository() {
    let f = fixture("strict").await;
    let hooks = Arc::new(Recorder::default());
    let child = f.repo.join("untracked");
    std::fs::create_dir_all(&child).unwrap();
    let sup = Supervisor::with_hooks(
        Arc::clone(&f.rt),
        ManagerContext {
            role: "amp-manager".into(),
            cwd: child.clone(),
            project: f.manager().project,
        },
        SupervisorConfig {
            isolation: Isolation::Worktree,
            ..config()
        },
        Some(Arc::clone(&hooks) as Arc<dyn WorkerHooks>),
    );
    let e = sup
        .run_ticket(req(31, "amp:amp-worker-backend"), CancellationToken::new())
        .await
        .unwrap_err();
    assert!(matches!(e, RunTicketError::Workspace(_)), "{e}");
    let text = e.to_string();
    assert!(
        text.contains("isolation = \"worktree\" needs a git repository"),
        "{text}"
    );
    assert!(text.contains("nothing was dispatched"), "{text}");
    assert!(f.dispatched().is_empty());
    assert!(!child.join("amp-work").exists());
    // A read-only role still runs (it needs no place of its own).
    f.script("wf", vec![template("Read it.", json!([]))]);
    let r = sup
        .run_ticket(req(32, "researcher"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.cwd, child);
}
