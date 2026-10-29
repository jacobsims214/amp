//! AMP #2802 (story 444, dispatch integrity), harness side: the supervisor starts its worker's
//! ticket, every end that doesn't close it hands it back, a chat that died is swept at startup,
//! and the person is told.
//!
//! Runs in-process against the mock AMP on faux providers, in throwaway repos under cargo's
//! target tmp dir (never /tmp: D13).

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    time::Duration,
};

use amp_core::{
    agent::{Runtime, RuntimeOptions},
    dispatch_ledger::{DispatchRecord, LEDGER_DIR, Ledger},
    faux::StepSpec,
    project::ProjectMarker,
    providers::StaticKeys,
    run_report::RunReporter,
    supervisor::{
        ManagerContext, RunRequest, Supervisor, SupervisorConfig, WorkerHooks, WorkerOutcome,
    },
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use common::{MockAmp, Running, spawn};
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
        .join("amp-harness-dispatch-integrity")
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
    // One ledger per process, as every real process has: the fixture's state dir.
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
    fn report(&self) -> (Arc<RunReporter>, Arc<Notices>) {
        let notices = Arc::new(Notices::default());
        let seen = Arc::clone(&notices);
        let reporter =
            RunReporter::new(Arc::clone(&self.rt.mcp)).with_ledger(Arc::clone(&self.ledger));
        reporter.set_notice(Arc::new(move |text: &str| {
            seen.0.lock().unwrap().push(text.to_owned())
        }));
        (reporter, notices)
    }

    fn supervisor(&self, config: SupervisorConfig) -> Arc<Supervisor> {
        let (reporter, _) = self.report();
        Supervisor::with_hooks_and_ledger(
            Arc::clone(&self.rt),
            ManagerContext {
                role: "amp-manager".into(),
                cwd: self.repo.clone(),
                project: ProjectMarker::find_within(&self.repo, Some(&self.dir)).unwrap(),
            },
            config,
            Some(reporter as Arc<dyn WorkerHooks>),
            Arc::clone(&self.ledger),
        )
    }

    fn supervisor_with_notices(&self, config: SupervisorConfig) -> (Arc<Supervisor>, Arc<Notices>) {
        let (reporter, notices) = self.report();
        let sup = Supervisor::with_hooks_and_ledger(
            Arc::clone(&self.rt),
            ManagerContext {
                role: "amp-manager".into(),
                cwd: self.repo.clone(),
                project: ProjectMarker::find_within(&self.repo, Some(&self.dir)).unwrap(),
            },
            config,
            Some(reporter as Arc<dyn WorkerHooks>),
            Arc::clone(&self.ledger),
        );
        (sup, notices)
    }

    fn script(&self, steps: Vec<rpi_ai::providers::faux::FauxStep>) {
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

/// What the person at the chat was told (`WorkerHooks::notice`).
#[derive(Default)]
struct Notices(Mutex<Vec<String>>);

impl Notices {
    fn lines(&self) -> Vec<String> {
        self.0.lock().unwrap().clone()
    }

    fn any(&self, needle: &str) -> bool {
        self.lines().iter().any(|l| l.contains(needle))
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

fn step(text: &str, tool_calls: Value) -> rpi_ai::providers::faux::FauxStep {
    let spec: StepSpec =
        serde_json::from_value(json!({"text": text, "tool_calls": tool_calls})).unwrap();
    spec.to_step()
}

/// 1. The harness starts the ticket for its worker, with the dispatch_id AMP gave it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_harness_starts_the_ticket_for_its_worker_with_the_dispatch_id() {
    let f = fixture("claim").await;
    let sup = f.supervisor(config());
    f.script(vec![step(
        "I'm done looking.",
        json!([{"name": "bash", "arguments": {"command": "true"}}]),
    )]);
    let r = sup
        .run_ticket(req(2101, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();

    // The claim is the harness's own, made with the token the dispatch answered.
    let claims = f.calls("amp_claim_task");
    assert_eq!(claims.len(), 1, "{claims:?}");
    assert_eq!(claims[0]["task_id"], 2101);
    assert_eq!(claims[0]["dispatch_id"], "d_mock2101");
    // And the run reports it as started and says which dispatch it worked under.
    assert!(r.started, "{r:?}");
    assert_eq!(r.dispatch_id, "d_mock2101");
    assert!(
        r.notes.iter().any(|n| n.contains("started #2101 in AMP")),
        "{:?}",
        r.notes
    );
}

/// 1b. The ticket's own record says a dispatch happened, and the ticket is cleared from the
/// ledger once the run is over and handed back.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_ledger_holds_a_live_dispatch_and_lets_go_when_the_ticket_comes_back() {
    let f = fixture("ledger").await;
    let sup = f.supervisor(config());
    // The ticket is recorded the moment AMP has the dispatch...
    f.script(vec![step(
        "Nothing to do.",
        json!([{"name": "bash", "arguments": {"command": "true"}}]),
    )]);
    let _ = sup
        .run_ticket(req(2102, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    // ...and the run ended without closing it, so it was returned and forgotten: nothing is left
    // for the next startup to sweep.
    assert_eq!(f.calls("amp_return_task")[0]["task_id"], 2102);
    assert!(
        f.ledger.records().iter().all(|r| r.task_id != 2102),
        "{:?}",
        f.ledger.records()
    );
}

/// 2. Every outcome that doesn't close the ticket hands it back, and the manager is told — in
/// `amp_run_ticket`'s result, which is the manager's conversation.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn every_end_that_does_not_close_the_ticket_returns_it_and_tells_the_manager() {
    let f = fixture("returns").await;

    // (a) The model's run fails.
    let (sup, notices) = f.supervisor_with_notices(config());
    f.script(vec![step(
        "Looking.",
        json!([{"name": "bash", "arguments": {"command": "true"}}]),
    )]);
    let r = sup
        .run_ticket(req(2110, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Error, "{r:?}");
    assert!(
        r.notes.iter().any(|n| n.contains("#2110 was returned")
            && n.contains("failed")
            && n.contains("re-dispatch it or ask your user")),
        "the manager is told what happened and what to do: {:?}",
        r.notes
    );
    // Not a second transcript paragraph per run: the `■ worker finished` line is already there,
    // and the duplicate pushed the conversation off a small screen (#2347's 80x24 chat).
    assert!(notices.lines().is_empty(), "{:?}", notices.lines());

    // (b) The wall-clock timeout runs out.
    let (sup, notices) = f.supervisor_with_notices(SupervisorConfig {
        timeout: Duration::from_millis(1),
        ..config()
    });
    f.script(vec![step(
        "Slow.",
        json!([{"name": "bash", "arguments": {"command": "sleep 3"}}]),
    )]);
    let r = sup
        .run_ticket(req(2111, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Timeout, "{r:?}");
    assert!(
        r.notes
            .iter()
            .any(|n| n.contains("#2111 was returned") && n.contains("timed out")),
        "{:?}",
        r.notes
    );
    assert!(notices.lines().is_empty(), "{:?}", notices.lines());

    // (c) The chat stopped it.
    let (sup, notices) = f.supervisor_with_notices(config());
    let signal = CancellationToken::new();
    let stops = signal.clone();
    f.script(vec![rpi_ai::providers::faux::FauxStep::factory(
        move |_| {
            stops.cancel();
            rpi_ai::providers::faux::faux_assistant_message(
                vec![rpi_ai::providers::faux::FauxBlock::text("Stopping.")],
                rpi_ai::types::StopReason::Stop,
            )
        },
    )]);
    let r = sup
        .run_ticket(req(2112, "worker-backend"), signal)
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Cancelled, "{r:?}");
    assert!(
        r.notes
            .iter()
            .any(|n| n.contains("#2112 was returned") && n.contains("was stopped")),
        "{:?}",
        r.notes
    );
    assert!(notices.lines().is_empty(), "{:?}", notices.lines());

    // Every one of them went back through AMP's returned path, in ticket order.
    let returned: Vec<i64> = f
        .calls("amp_return_task")
        .iter()
        .filter_map(|c| c["task_id"].as_i64())
        .collect();
    assert_eq!(returned, vec![2110, 2111, 2112], "{returned:?}");
}

/// 3. A ticket AMP refuses to return because it was started is set back to ready instead — the
/// documented fallback, and the run says so rather than pretending it was returned.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_started_ticket_amp_will_not_return_is_set_back_to_ready_and_the_run_says_so() {
    let f = fixture("fallback").await;
    let (sup, notices) = f.supervisor_with_notices(config());
    f.script(vec![
        step(
            "Nothing.",
            json!([{"name": "bash", "arguments": {"command": "true"}}]),
        ),
        step("Stopping here.", json!([])),
    ]);
    // 4046: the mock's ticket that amp_return_task refuses with "was started".
    let r = sup
        .run_ticket(req(4046, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Stopped, "{r:?}");
    let returned = f.calls("amp_return_task");
    assert_eq!(returned[0]["task_id"], 4046);
    let states = f.calls("amp_set_task_state");
    assert_eq!(states.len(), 1, "{states:?}");
    assert_eq!(states[0]["task_id"], 4046);
    assert_eq!(states[0]["state"], "backlog");
    // The ticket IS back in ready, so the note says it was returned and names both calls: the
    // refusal is not hidden by the fallback, and the person is not told a re-dispatchable ticket
    // is stuck in progress.
    assert!(
        r.notes
            .iter()
            .any(|n| n.starts_with("returned #4046 to ready")
                && n.contains("already started")
                && n.contains("amp_set_task_state")),
        "{:?}",
        r.notes
    );
    assert_eq!(r.returned, Some(Ok(())), "it is in ready");
    // The manager is told it came back, and that it came back by the fallback, so nobody hunts
    // for it under AMP's `returned` list.
    assert!(
        r.notes.iter().any(
            |n| n.contains("#4046 was returned") && n.contains("set back rather than returned")
        ) && !r.notes.iter().any(|n| n.contains("could not be returned")),
        "{:?}",
        r.notes
    );
    assert!(notices.lines().is_empty(), "{:?}", notices.lines());
}

/// 3b. Both refusals are reported, and nothing is claimed to have happened.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn when_amp_refuses_both_paths_the_note_names_both() {
    let f = fixture("both-refuse").await;
    let sup = f.supervisor(config());
    f.script(vec![
        step(
            "Nothing.",
            json!([{"name": "bash", "arguments": {"command": "true"}}]),
        ),
        step("Stopping here.", json!([])),
    ]);
    let r = sup
        .run_ticket(req(4048, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert!(
        r.notes.iter().any(|n| {
            n.starts_with("could not return #4048")
                && n.contains("amp_return_task")
                && n.contains("amp_set_task_state")
        }),
        "{:?}",
        r.notes
    );
    // The ledger keeps it: the ticket may still be in progress, so the next startup tries again.
    assert!(
        f.ledger.records().iter().any(|r| r.task_id == 4048),
        "{:?}",
        f.ledger.records()
    );
}

/// 4. On startup, tickets a dead harness process dispatched are handed back, and the person is
/// told — including the one AMP won't take back.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn startup_releases_the_tickets_of_a_dead_chat_and_tells_the_person() {
    let f = fixture("orphans").await;
    let dir = f.rt.state_dir.join(LEDGER_DIR);
    std::fs::create_dir_all(&dir).unwrap();
    // Two records from a process that cannot be running (pid 0 has no harness liveness lock),
    // written the way the supervisor writes them.
    for (task_id, agent) in [
        (2120, "amp:amp-worker-backend"),
        (2121, "amp:amp-worker-docs"),
    ] {
        std::fs::write(
            dir.join(format!("t{task_id}.json")),
            serde_json::to_string(&DispatchRecord {
                task_id,
                dispatch_id: format!("d_dead{task_id}"),
                agent_id: agent.into(),
                pid: 0,
                at_ms: 1,
            })
            .unwrap(),
        )
        .unwrap();
    }
    let (sup, notices) = f.supervisor_with_notices(config());
    let releases = sup.release_orphans().await;
    assert_eq!(releases.len(), 2, "{releases:?}");
    assert!(releases.iter().all(|r| r.result.is_ok()), "{releases:?}");

    // AMP got a return per ticket, with a reason naming what happened.
    let returned = f.calls("amp_return_task");
    let ids: Vec<i64> = returned
        .iter()
        .filter_map(|c| c["task_id"].as_i64())
        .collect();
    assert_eq!(ids, vec![2120, 2121], "{returned:?}");
    assert!(
        returned[0]["reason"]
            .as_str()
            .unwrap()
            .contains("the chat that dispatched it to amp:amp-worker-backend is gone"),
        "{}",
        returned[0]
    );
    // The person was told, in words that say what to do next.
    assert!(
        notices.any("#2120 was returned to ready") && notices.any("Re-dispatch it"),
        "{:?}",
        notices.lines()
    );
    // Nothing is left for the next startup.
    assert!(f.ledger.records().is_empty(), "{:?}", f.ledger.records());
}

/// 4b. A record whose live process still holds its lock is left alone: another chat's worker is
/// running, not orphaned.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn startup_leaves_a_live_chats_dispatch_alone() {
    let f = fixture("orphans-live").await;
    let dir = f.rt.state_dir.join(LEDGER_DIR);
    std::fs::create_dir_all(&dir).unwrap();
    // A live process: this one holds a lock under that pid's name, exactly as another harness
    // holds its own while it runs.
    let held = std::fs::File::options()
        .create(true)
        .truncate(false)
        .write(true)
        .open(dir.join("live-4242.lock"))
        .unwrap();
    held.try_lock().unwrap();
    std::fs::write(
        dir.join("t2130.json"),
        serde_json::to_string(&DispatchRecord {
            task_id: 2130,
            dispatch_id: "d_live".into(),
            agent_id: "amp:amp-worker-backend".into(),
            pid: 4242,
            at_ms: 1,
        })
        .unwrap(),
    )
    .unwrap();
    let sup = f.supervisor(config());
    assert!(sup.release_orphans().await.is_empty());
    assert!(f.calls("amp_return_task").is_empty());
    assert_eq!(f.ledger.records().len(), 1, "still somebody's");
}

/// 4c. A ticket closed by its worker leaves nothing for the sweep, and is not returned twice.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_closed_ticket_is_not_swept_at_the_next_startup() {
    let f = fixture("closed").await;
    let sup = f.supervisor(config());
    f.script(vec![step(
        "Done.",
        json!([{"name": "amp_complete_task", "arguments": {"task_id": 2140}}]),
    )]);
    let r = sup
        .run_ticket(req(2140, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Completed, "{r:?}");
    assert!(f.calls("amp_return_task").is_empty());
    assert!(f.ledger.records().is_empty(), "{:?}", f.ledger.records());
    // The completion carried the dispatch_id: it is the worker's own, not a closeout.
    let completed = f.calls("amp_complete_task");
    assert_eq!(completed[0]["dispatch_id"], "d_mock2140", "{completed:?}");

    let sup = f.supervisor(config());
    assert!(sup.release_orphans().await.is_empty());
    assert!(f.calls("amp_return_task").is_empty());
}

/// 5. A server that predates dispatch tokens is still supported: no claim, no dispatch_id, and
/// the ticket is handed back the way that server understands.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_server_without_dispatch_tokens_still_runs_and_returns() {
    let f = fixture("old-server").await;
    let sup = f.supervisor(config());
    f.script(vec![
        step(
            "Nothing to do.",
            json!([{"name": "bash", "arguments": {"command": "true"}}]),
        ),
        step("Stopping here.", json!([])),
    ]);
    // 4043: the mock's answer shaped like an AMP before #2801 (no dispatch_id at all).
    let r = sup
        .run_ticket(req(4043, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Stopped, "{r:?}");
    assert_eq!(r.dispatch_id, "");
    assert!(!r.started, "{r:?}");
    assert!(
        f.calls("amp_claim_task").is_empty(),
        "nothing to claim with"
    );
    // Still handed back: the returned path is the same call.
    assert_eq!(f.calls("amp_return_task")[0]["task_id"], 4043);
    // And the completion the model makes carries no dispatch_id field at all.
    assert!(
        f.calls("amp_complete_task")
            .iter()
            .all(|c| c.get("dispatch_id").is_none())
    );
}

/// 6. A claim AMP refuses ends the run instead of letting the worker work a dispatch that is no
/// longer current.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_refused_claim_ends_the_run_before_the_worker_does_anything() {
    let f = fixture("claim-refused").await;
    let sup = f.supervisor(config());
    f.script(vec![step("Never reached.", json!([]))]);
    // 4045: the mock refuses the claim ("not dispatched: there is nothing to start").
    let r = sup
        .run_ticket(req(4045, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Error, "{r:?}");
    assert!(
        r.error.as_deref().unwrap_or_default().contains("claim"),
        "{r:?}"
    );
    assert!(
        r.notes.iter().any(|n| n.contains("could not start #4045")),
        "{:?}",
        r.notes
    );
    assert_eq!(r.turns, 0, "the model never ran: {r:?}");
    assert!(f.calls("amp_complete_task").is_empty());
}

/// 7. `is_refused_started` reads AMP's own refusal, and nothing else, as the started case.
#[test]
fn only_amps_started_refusal_takes_the_fallback_path() {
    use amp_core::run_report::is_refused_started;
    assert!(is_refused_started(
        "task 4046 was started (by amp:amp-worker-backend): returning it would pull it out from \
         under a worker that is running. Block it with a question, or close it out with \
         closeout_reason"
    ));
    assert!(!is_refused_started("task 4042 is owned by someone else"));
    assert!(!is_refused_started("task 4047 not found"));
    assert!(!is_refused_started(
        "task 5 is blocked — waiting on task IDs: [4]"
    ));
}

/// The notice says what to do next, and reads as a person's line, not a model's.
#[test]
fn the_notice_names_the_ticket_the_worker_the_outcome_and_the_next_step() {
    use amp_core::run_report::{Released, returned_notice};
    use amp_core::supervisor::{WorkerInfo, WorkerReport};
    use amp_core::workspace::Placement;

    let worker = WorkerInfo {
        task_id: 7,
        agent_id: "amp:amp-worker-backend".into(),
        role: "amp-worker-backend".into(),
        dispatch_id: "d_x".into(),
        model: "wf/wf-1".into(),
        cwd: PathBuf::from("."),
        worktree: None,
        branch: None,
        workspace: Placement::Shared,
        git: false,
        edits: false,
        manager_cwd: PathBuf::from("."),
        totals: Arc::default(),
    };
    let mut report = WorkerReport {
        task_id: 7,
        agent_id: "amp:amp-worker-backend".into(),
        role: "amp-worker-backend".into(),
        outcome: WorkerOutcome::Timeout,
        dispatch_id: "d_x".into(),
        started: true,
        returned: None,
        final_message: String::new(),
        error: None,
        model: "wf/wf-1".into(),
        cwd: PathBuf::from("."),
        worktree: None,
        branch: None,
        workspace: Placement::Shared,
        session_file: None,
        turns: 0,
        tool_calls: 0,
        tool_errors: 0,
        usage: Default::default(),
        duration_ms: 20 * 60 * 1000,
        notes: Vec::new(),
        totals: Default::default(),
    };
    let line = returned_notice(&worker, &report, &Ok(Released::Returned));
    assert!(line.contains("#7"), "{line}");
    assert!(line.contains("amp:amp-worker-backend"), "{line}");
    assert!(line.contains("timed out after 20 min"), "{line}");
    assert!(line.contains("re-dispatch it"), "{line}");

    report.outcome = WorkerOutcome::Error;
    report.error = Some("provider_error: boom".into());
    let line = returned_notice(&worker, &report, &Ok(Released::Returned));
    assert!(line.contains("failed: provider_error: boom"), "{line}");

    // The fallback is also a ticket back in ready: same instruction, and it says which way, so
    // nobody looks for it under AMP's `returned` list and fails to find it.
    let line = returned_notice(&worker, &report, &Ok(Released::SetBack));
    assert!(line.contains("#7 was returned"), "{line}");
    assert!(line.contains("set back rather than returned"), "{line}");
    assert!(line.contains("re-dispatch it"), "{line}");
    assert!(!line.contains("could not be returned"), "{line}");

    let line = returned_notice(&worker, &report, &Err("amp_return_task: nope".into()));
    assert!(line.contains("could not be returned"), "{line}");
    assert!(line.contains("nope"), "{line}");
}
