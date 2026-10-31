//! #2144: quitting the chat while `amp_run_ticket` runs must not exit before the abandoned
//! worker's run is reported (D8). Drives the real [`Shell`] API on a faux manager that dispatches
//! a worker through a [`Supervisor`] with the [`RunReporter`], against the mock AMP. The report is
//! slowed past the old 3 s quit grace, so a shell that only waited 3 s would miss it.
//! Repos live under cargo's target tmp dir (never /tmp: D13).

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::{
    agent::{AgentSpec, Runtime, RuntimeOptions},
    events::ChannelEmitter,
    project::ProjectMarker,
    providers::StaticKeys,
    run_report::RunReporter,
    supervisor::{
        ManagerContext, Supervisor, SupervisorConfig, WorkerHooks, WorkerInfo, WorkerReport,
    },
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use amp_tui::{Shell, ShellInfo, shell::QuitOutcome};
use common::{MockAmp, Running, spawn};
use rpi_agent::{AgentEvent, BeforeToolCall};
use rpi_ai::{
    providers::faux::{FauxBlock, FauxStep, faux_assistant_message},
    types::{Context, Message, StopReason},
};
use rpi_harness::types::HarnessTool;
use serde_json::{Value, json};
use tokio::sync::mpsc::UnboundedReceiver;

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

/// Longer than the old `ABORT_GRACE` (3 s) the TUI waited before exiting.
const SLOW_REPORT: Duration = Duration::from_secs(4);

/// The real run reporter, with `on_finish` (where `amp_report_run` is called) slowed down, like a
/// worker that takes its abort grace or an AMP that is slow to answer.
struct SlowReporter(Arc<RunReporter>);

#[async_trait::async_trait]
impl WorkerHooks for SlowReporter {
    fn before_tool_call(&self, worker: &WorkerInfo) -> Option<BeforeToolCall> {
        self.0.before_tool_call(worker)
    }

    fn extra_tools(&self, worker: &WorkerInfo) -> Vec<HarnessTool> {
        self.0.extra_tools(worker)
    }

    async fn on_finish(&self, worker: &WorkerInfo, report: &mut WorkerReport) {
        tokio::time::sleep(SLOW_REPORT).await;
        self.0.on_finish(worker, report).await;
    }

    async fn release(&self, task_id: i64, dispatch_id: &str, why: &str) -> Result<(), String> {
        self.0.release(task_id, dispatch_id, why).await
    }
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

struct Rig {
    amp: Running,
    sup: Arc<Supervisor>,
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
}

fn has_tool_result(ctx: &Context) -> bool {
    ctx.messages
        .iter()
        .any(|m| matches!(m, Message::ToolResult(_)))
}

/// A manager whose first turn dispatches ticket `task_id` to a worker that runs `sleep 30`.
async fn rig(name: &str, task_id: i64) -> Rig {
    let dir: PathBuf = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-chat-quit")
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
    let env = move |k: &str| match k {
        "HOME" => Some(home.to_string_lossy().into_owned()),
        "XDG_CONFIG_HOME" => Some(home.join(".config").to_string_lossy().into_owned()),
        "XDG_STATE_HOME" => Some(home.join(".local/state").to_string_lossy().into_owned()),
        _ => None,
    };
    let rt = Arc::new(
        Runtime::load(
            Arc::clone(&client),
            RuntimeOptions {
                repo_root: Some(repo.clone()),
                models: None,
                keys: &StaticKeys::default(),
                env: &env,
                state_dir: None,
            },
        )
        .expect("load the runtime"),
    );
    let sup = Supervisor::with_hooks(
        Arc::clone(&rt),
        ManagerContext {
            role: "amp-manager".into(),
            cwd: repo.clone(),
            project: ProjectMarker::find_within(&repo, Some(&dir)).unwrap(),
        },
        SupervisorConfig {
            abort_grace: Duration::from_secs(5),
            ..SupervisorConfig::default()
        },
        Some(Arc::new(SlowReporter(RunReporter::new(Arc::clone(&client)))) as Arc<dyn WorkerHooks>),
    );
    rt.providers.faux_script("boss").unwrap().set_responses(vec![
        FauxStep::tool_call(
            "amp_run_ticket",
            json!({"task_id": task_id, "agent_id": "amp:amp-worker-backend", "worktree": false}),
        ),
        FauxStep::text("unreachable"),
    ]);
    rt.providers
        .faux_script("wf")
        .unwrap()
        .set_responses(vec![FauxStep::factory(|ctx: &Context| {
            if has_tool_result(ctx) {
                faux_assistant_message(vec![FauxBlock::text("done")], StopReason::Stop)
            } else {
                faux_assistant_message(
                    vec![FauxBlock::tool_call(
                        "bash",
                        json!({ "command": "sleep 30" }),
                    )],
                    StopReason::ToolUse,
                )
            }
        })]);

    let (emitter, events) = ChannelEmitter::new();
    let mut spec = AgentSpec::new("amp-manager", repo.clone());
    spec.extra_tools = vec![sup.tool()];
    spec.emitter = Some(emitter.into_emitter());
    let manager = rt.build(spec).await.unwrap();
    let info = ShellInfo {
        role: "amp-manager".into(),
        ..ShellInfo::default()
    };
    let (mut shell, done) = Shell::new(Arc::clone(&manager.harness), info, Vec::new());
    shell.set_supervisor(Some(Arc::clone(&sup)));
    Rig {
        amp,
        sup,
        shell,
        events,
        done,
    }
}

impl Rig {
    /// Submits the prompt and pumps events until the worker is running its `sleep`.
    async fn dispatch(&mut self) {
        self.shell.submit("run the ticket").await;
        let deadline = Instant::now() + Duration::from_secs(20);
        loop {
            let workers = self.sup.workers();
            if workers.first().is_some_and(|w| w.totals.tool_calls > 0) {
                break;
            }
            assert!(Instant::now() < deadline, "the worker never started");
            while let Ok(event) = self.events.try_recv() {
                self.shell.handle_event(&event);
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        assert!(self.shell.is_running());
    }

    fn reports(&self) -> Vec<Value> {
        self.amp.mock.calls_to("amp_report_run")
    }

    /// The tickets handed back to ready: `amp_return_task` (AMP #2801's returned path, which the
    /// harness prefers) and `amp_set_task_state` (the fallback for a started ticket). What these
    /// tests are about is that the ticket is no longer in progress with nobody on it (#2204), not
    /// which of the two calls did it — `returned_calls` below is for the one test that reads the
    /// reason text.
    fn releases(&self) -> Vec<Value> {
        let mut all = self.amp.mock.calls_to("amp_return_task");
        all.extend(self.amp.mock.calls_to("amp_set_task_state"));
        all
    }

    /// Only `amp_return_task` calls: the returned path, with its `reason`.
    fn returned_calls(&self) -> Vec<Value> {
        self.amp.mock.calls_to("amp_return_task")
    }

    /// Only `amp_set_task_state` calls: the fallback for a ticket AMP won't return.
    fn set_state_calls(&self) -> Vec<Value> {
        self.amp.mock.calls_to("amp_set_task_state")
    }

    /// Quits (nothing skips the wait) and expects every worker to settle.
    async fn quit_and_settle(&mut self) {
        let outcome = tokio::time::timeout(
            Duration::from_secs(60),
            self.shell.quit(
                &mut self.events,
                &mut self.done,
                std::future::pending::<()>(),
                |_| {},
            ),
        )
        .await
        .expect("quit is bounded");
        assert_eq!(outcome, QuitOutcome::Settled);
    }

    /// Waits until the supervisor has no live worker (its report and release are done).
    async fn workers_gone(&self) {
        let deadline = Instant::now() + Duration::from_secs(30);
        while !self.sup.workers().is_empty() {
            assert!(Instant::now() < deadline, "the worker never finished");
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(120, 40).join("\n")
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn quitting_mid_dispatch_waits_for_the_abandoned_run_report() {
    let mut r = rig("waits", 2144).await;
    r.dispatch().await;
    assert_eq!(r.shell.live_tickets(), vec![2144]);
    assert_eq!(
        r.shell.quit_bound(),
        Duration::from_secs(5) + amp_tui::shell::REPORT_ALLOWANCE
    );

    let started = Instant::now();
    let mut repaints = 0;
    let outcome = tokio::time::timeout(
        Duration::from_secs(60),
        r.shell.quit(
            &mut r.events,
            &mut r.done,
            std::future::pending::<()>(),
            |_| repaints += 1,
        ),
    )
    .await
    .expect("quit is bounded");
    assert_eq!(outcome, QuitOutcome::Settled);
    assert!(started.elapsed() >= SLOW_REPORT, "it waited for the report");
    assert!(repaints > 0);
    assert!(!r.shell.is_running());
    assert!(r.shell.live_tickets().is_empty());

    // The abandoned run reached AMP before quit returned.
    let runs = r.reports();
    assert_eq!(runs.len(), 1, "{runs:?}");
    assert_eq!(runs[0]["task_id"], 2144);
    assert_eq!(runs[0]["outcome"], "abandoned");
    assert!(
        runs[0]["run_note"]
            .as_str()
            .unwrap_or_default()
            .starts_with("abandoned: cancelled"),
        "{}",
        runs[0]
    );
    let screen = r.screen();
    assert!(
        screen.contains("stopping 1 worker and reporting its runs"),
        "{screen}"
    );
    // #2204: and the ticket is no longer in progress with nobody on it. AMP #2801's returned path
    // is what does it now (`calls_to` gives a call's arguments, so the returned path's field is
    // `reason`, not `state`).
    assert_eq!(r.set_state_calls(), Vec::<Value>::new());
    let released = r.returned_calls();
    assert_eq!(released.len(), 1, "{released:?}");
    assert_eq!(released[0]["task_id"], 2144);
    assert!(
        released[0]["reason"]
            .as_str()
            .unwrap_or_default()
            .contains("cancelled"),
        "{}",
        released[0]
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_second_ctrl_c_skips_the_wait() {
    let mut r = rig("skip", 2145).await;
    r.dispatch().await;
    let outcome = tokio::time::timeout(
        Duration::from_secs(10),
        r.shell.quit(
            &mut r.events,
            &mut r.done,
            tokio::time::sleep(Duration::from_millis(200)),
            |_| {},
        ),
    )
    .await
    .expect("skip ends the wait");
    assert_eq!(outcome, QuitOutcome::Skipped);
    // The report is still on its way: the terminal names this ticket as unreported.
    assert_eq!(r.shell.live_tickets(), vec![2145]);
    assert!(r.reports().is_empty());
    let screen = r.screen();
    assert!(screen.contains("quitting without waiting"), "{screen}");
    // #2204: the worker's own on_finish won't get to run, so the quit handed the ticket back.
    assert!(
        screen.contains("returning #2145 to ready in AMP"),
        "{screen}"
    );
    assert_eq!(r.shell.released(), &[(2145, Ok(()))]);
    // AMP #2801's returned path is what hands it back now (the mock answers it); the fallback is
    // only for a ticket AMP refuses to return.
    assert!(r.set_state_calls().is_empty(), "{:?}", r.set_state_calls());
    let released = r.releases();
    assert_eq!(released.len(), 1, "{released:?}");
    assert_eq!(released[0]["task_id"], 2145);
    assert_eq!(r.returned_calls()[0]["task_id"], 2145);
    assert!(
        released[0]["reason"]
            .as_str()
            .unwrap()
            .contains("quit without waiting"),
        "{}",
        released[0]
    );

    // Let the worker finish its report so the test leaves nothing running.
    r.workers_gone().await;
}

/// #2204: Ctrl+C with a worker of this chat running asks before anything stops: the reply (and so
/// the worker) keeps going until the person confirms, and a confirmation that comes too late asks
/// again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn ctrl_c_with_a_worker_running_asks_before_quitting() {
    use amp_tui::shell::{CtrlC, QUIT_CONFIRM_FOR};

    let mut r = rig("confirm-ctrl-c", 2147).await;
    r.dispatch().await;
    let t0 = Instant::now();
    assert_eq!(r.shell.ctrl_c(t0).await, CtrlC::Confirm);
    let screen = r.screen();
    assert!(
        screen.contains("a worker is running on #2147 — quit anyway? it will be returned to ready"),
        "{screen}"
    );
    assert!(r.shell.is_running(), "nothing was stopped");
    assert_eq!(r.shell.live_tickets(), vec![2147]);
    assert!(r.reports().is_empty() && r.releases().is_empty());

    // Too late: it asks again.
    let late = t0 + QUIT_CONFIRM_FOR + Duration::from_millis(1);
    assert_eq!(r.shell.ctrl_c(late).await, CtrlC::Confirm);
    assert!(r.shell.is_running());
    // In time: quit. The terminal loop then calls quit, which stops and reports the worker and
    // hands its ticket back.
    assert_eq!(
        r.shell.ctrl_c(late + Duration::from_millis(500)).await,
        CtrlC::Quit
    );
    r.quit_and_settle().await;
    assert_eq!(r.reports().len(), 1);
    let released = r.releases();
    assert_eq!(released.len(), 1, "{released:?}");
    assert_eq!(released[0]["task_id"], 2147);
    assert_eq!(r.returned_calls()[0]["task_id"], 2147);
}

/// #2204: `/quit` and Ctrl+D ask the same way; either confirms the other.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn slash_quit_and_ctrl_d_with_a_worker_running_ask_first() {
    use amp_tui::{
        Submit,
        shell::{CtrlD, QUIT_CONFIRM_HINT},
    };

    let mut r = rig("confirm-quit", 2148).await;
    r.dispatch().await;
    assert_eq!(r.shell.submit("/quit").await, Submit::Continue);
    assert!(r.shell.quit_confirm_pending());
    assert_eq!(
        r.shell.view().editor_hint().as_deref(),
        Some(QUIT_CONFIRM_HINT)
    );
    assert!(
        r.screen()
            .contains("a worker is running on #2148 — quit anyway?")
    );
    assert!(r.shell.is_running());
    assert_eq!(r.shell.ctrl_d(Instant::now()).await, CtrlD::Quit);
    r.quit_and_settle().await;
    assert_eq!(r.releases()[0]["task_id"], 2148);

    // With nothing running any more, /quit quits at once.
    assert_eq!(r.shell.submit("/quit").await, Submit::Quit);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn quitting_when_idle_returns_at_once() {
    let mut r = rig("idle", 2146).await;
    let outcome = r
        .shell
        .quit(
            &mut r.events,
            &mut r.done,
            std::future::pending::<()>(),
            |_| {},
        )
        .await;
    assert_eq!(outcome, QuitOutcome::Idle);
    assert!(r.reports().is_empty());
}
