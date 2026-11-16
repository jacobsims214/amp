//! #4241: worker control end to end, in-process against the mock AMP (which stores ticket
//! comments) on faux providers (offline, no paid model). The real comment watcher
//! (`comment_watch::CommentWatcher` over `McpComments` and `SupervisorSink`) is ticked by hand so
//! every poll is deterministic; the worker's model is a faux script that decides from its own
//! context. Every repo and state dir is a throwaway under cargo's target tmp dir (never /tmp,
//! never the installed harness's state: local_harness_safety).
//!
//! Covers:
//! - a person's comment during a run reaches the worker at its next turn (never in the middle of
//!   a blocked tool), the worker acknowledges it on the ticket (author stamped with its agent
//!   id), and the acknowledgement is not steered back (the steer count stays 1);
//! - the manager stops it (abandoned run with the reason, ticket handed back), resumes it with
//!   `continue` (same session: the comment and the ack are in its context, nothing re-steered),
//!   resumes the live continued worker again in the middle of its `sleep 30` (first time, no
//!   `busy`: #4281), and restarts it (fresh session), with never two live workers on the ticket;
//!   a session lock still held past the limit is `busy` with nothing dispatched;
//! - a comment after the worker's `amp_complete_task` is refused `Closed` and never reaches the
//!   model;
//! - (wave check #4234) a steer taken while a tool is blocked, then a stop in that same step:
//!   pinned as it behaves today.

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use amp_core::{
    agent::{Runtime, RuntimeOptions},
    comment_watch::{CommentWatcher, McpComments, SupervisorSink, TickReport, WatchConfig},
    project::ProjectMarker,
    providers::StaticKeys,
    run_report::RunReporter,
    supervisor::{
        DispatchMode, ManagerContext, RunRequest, SteerError, Supervisor, SupervisorConfig,
        WorkerHooks, WorkerOutcome, WorkerReport, live_tickets,
    },
    worker_control,
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

/// The worker's agent id in AMP's form: the author of its own comments.
const AGENT: &str = "amp:amp-worker-backend";
/// The steering envelope's header, up to the comment id (comment_watch::envelope).
const MARK: &str = "[AMP ticket comment #";

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

/// A throwaway repo (project 2), the faux config under a throwaway HOME, and a runtime on a
/// fresh mock AMP that stores comments. HOME, XDG_CONFIG_HOME and XDG_STATE_HOME all point into
/// the fixture, so sessions and state never reach the installed harness's.
async fn fixture(name: &str) -> Fixture {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-worker-control")
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

    let amp = spawn(MockAmp::with_comments(), None).await;
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
    /// A launch-mode supervisor whose hooks are the real run reporter, as in the chat.
    fn supervisor(&self) -> Arc<Supervisor> {
        Supervisor::with_hooks(
            Arc::clone(&self.rt),
            ManagerContext {
                role: "amp-manager".into(),
                cwd: self.repo.clone(),
                project: ProjectMarker::find_within(&self.repo, Some(&self.dir)).unwrap(),
            },
            SupervisorConfig {
                max_parallel: 2,
                abort_grace: Duration::from_secs(5),
                dispatch: DispatchMode::Launch,
                ..SupervisorConfig::default()
            },
            Some(RunReporter::new(Arc::clone(&self.rt.mcp)) as Arc<dyn WorkerHooks>),
        )
    }

    /// The comment watcher the chat runs (comment_watch::spawn), ticked by the test.
    fn watcher(&self, sup: &Arc<Supervisor>) -> CommentWatcher<McpComments, SupervisorSink> {
        CommentWatcher::new(
            McpComments(Arc::clone(&self.rt.mcp)),
            SupervisorSink(Arc::downgrade(sup)),
            WatchConfig::default(),
        )
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

    async fn wait_for_calls(&self, tool: &str, n: usize) {
        let deadline = Instant::now() + Duration::from_secs(20);
        while self.calls(tool).len() < n {
            assert!(Instant::now() < deadline, "no {n} {tool} calls in time");
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }
}

/// One poll of the watcher.
async fn tick(w: &mut CommentWatcher<McpComments, SupervisorSink>) -> TickReport {
    w.tick(Instant::now())
        .await
        .expect("the supervisor is alive")
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

// ------------------------------------------------------------------------------------------------
// The worker's faux model.
// ------------------------------------------------------------------------------------------------

fn text_of(m: &Message) -> String {
    serde_json::to_string(m).unwrap_or_default()
}

/// The comment ids of the steering envelopes among `messages`' user messages, in order. The
/// worker's first prompt names the header too (STEERING_NOTE, `#<id>`), so only a header
/// followed by digits counts.
fn steered_ids(messages: &[Message]) -> Vec<i64> {
    let mut ids = Vec::new();
    for m in messages {
        let Message::User(_) = m else { continue };
        let text = text_of(m);
        let mut rest = text.as_str();
        while let Some(at) = rest.find(MARK) {
            rest = &rest[at + MARK.len()..];
            let digits: String = rest.chars().take_while(char::is_ascii_digit).collect();
            if let Ok(id) = digits.parse() {
                ids.push(id);
            }
        }
    }
    ids
}

/// How many `name` tool calls the assistant made in `messages`.
fn tool_calls(messages: &[Message], name: &str) -> usize {
    let needle = format!("\"{name}\"");
    messages
        .iter()
        .filter(|m| matches!(m, Message::Assistant(_)) && text_of(m).contains(&needle))
        .count()
}

fn has_tool_result(messages: &[Message]) -> bool {
    messages.iter().any(|m| matches!(m, Message::ToolResult(_)))
}

fn bash(command: String) -> rpi_ai::types::AssistantMessage {
    faux_assistant_message(
        vec![FauxBlock::tool_call("bash", json!({ "command": command }))],
        StopReason::ToolUse,
    )
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
    fn all(&self) -> Vec<Vec<Message>> {
        self.0.lock().unwrap().clone()
    }
}

/// A shell command that touches `entered`, then blocks until `gate` exists (the `Notify` of a
/// blocked tool, across the process boundary bash runs in).
fn gated(entered: &Path, gate: &Path) -> String {
    format!(
        "touch '{}'; while [ ! -f '{}' ]; do sleep 0.05; done; echo released",
        entered.display(),
        gate.display()
    )
}

/// The worker of the main test. First turn (no tool results yet): the gated command. A steered
/// comment it has not acknowledged yet: `amp_add_task_comment` on its ticket, replying to it
/// (no `author`, as STEERING_NOTE says). Otherwise: a long sleep, so it stays live until stopped.
fn steerable_worker(log: Arc<Log>, task_id: i64, entered: PathBuf, gate: PathBuf) -> FauxStep {
    FauxStep::factory(move |ctx: &Context| {
        log.0.lock().unwrap().push(ctx.messages.clone());
        let steered = steered_ids(&ctx.messages);
        let acked = tool_calls(&ctx.messages, "amp_add_task_comment");
        if !has_tool_result(&ctx.messages) {
            bash(gated(&entered, &gate))
        } else if steered.len() > acked {
            let id = steered[acked];
            faux_assistant_message(
                vec![FauxBlock::tool_call(
                    "amp_add_task_comment",
                    json!({"task_id": task_id, "body": format!("Acknowledged #{id}: will do.")}),
                )],
                StopReason::ToolUse,
            )
        } else {
            bash("sleep 30".to_owned())
        }
    })
}

// ------------------------------------------------------------------------------------------------
// The tests.
// ------------------------------------------------------------------------------------------------

/// Steer + acknowledge, then stop, resume continue and restart, never two live workers.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_steered_worker_acknowledges_then_is_stopped_resumed_and_restarted() {
    const T: i64 = 4241;
    let f = fixture("steer-stop-resume-restart").await;
    let sup = f.supervisor();
    let mut watcher = f.watcher(&sup);
    let log = Arc::new(Log::default());
    let (entered, gate) = (f.dir.join("entered"), f.dir.join("gate"));
    f.script(
        (0..40)
            .map(|_| steerable_worker(Arc::clone(&log), T, entered.clone(), gate.clone()))
            .collect(),
    );

    // Watch the live map for the whole test: never two workers on one ticket.
    let watching = CancellationToken::new();
    let live_most = tokio::spawn({
        let (sup, watching) = (Arc::clone(&sup), watching.clone());
        async move {
            let mut most = 0;
            while !watching.is_cancelled() {
                most = most.max(live_tickets(&sup).len());
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
            most
        }
    });

    sup.launch(
        RunRequest {
            task_id: T,
            agent_id: "worker-backend".into(),
            ..RunRequest::default()
        },
        CancellationToken::new(),
    )
    .await
    .expect("launched");
    f.wait_for_calls("amp_claim_task", 1).await;
    wait_until("the worker is in its blocked tool", || entered.exists()).await;

    // The watcher's first sight of the run: everything already on the ticket (the claim's
    // "picked this up") is old, nothing is steered.
    let first = tick(&mut watcher).await;
    assert_eq!(first.fetched, vec![T], "{first:?}");
    assert!(first.steered.is_empty(), "{first:?}");

    // A person comments while the worker's tool is blocked: the watcher steers it at once.
    let comment = f.amp.mock.add_comment(
        T,
        "jacob@example.com",
        "Also cover the restart path, please.",
    );
    let steer = tick(&mut watcher).await;
    assert_eq!(steer.steered, vec![(T, comment)], "{steer:?}");

    // ...but the model does not see it in the middle of the tool.
    tokio::time::sleep(Duration::from_millis(400)).await;
    assert_eq!(log.len(), 1, "no model call while the tool is blocked");
    assert!(steered_ids(&log.get(0)).is_empty());

    // Release the tool: the next request carries the envelope, after the tool's result, and the
    // worker acknowledges on its ticket, stamped with its own agent id.
    std::fs::write(&gate, "go").unwrap();
    f.wait_for_calls("amp_add_task_comment", 1).await;
    let second = log.get(1);
    assert_eq!(
        steered_ids(&second),
        vec![comment],
        "delivered at the next turn"
    );
    let result_at = second
        .iter()
        .position(|m| matches!(m, Message::ToolResult(_)))
        .expect("the gated tool's result");
    let steer_at = second
        .iter()
        .position(|m| {
            matches!(m, Message::User(_))
                && text_of(m).contains(MARK)
                && text_of(m).contains(&format!("#{comment} by jacob@example.com"))
        })
        .expect("the steer in context");
    assert!(
        steer_at > result_at,
        "the steer follows the blocked tool's result, never interrupts it"
    );
    assert!(text_of(&second[result_at]).contains("released"));
    let ack = f
        .amp
        .mock
        .comments_on(T)
        .into_iter()
        .find(|c| {
            c["body"]
                .as_str()
                .is_some_and(|b| b.starts_with("Acknowledged"))
        })
        .expect("the ack is on the ticket");
    assert_eq!(ack["author"], AGENT, "the harness stamped the ack: {ack}");
    assert_eq!(ack["body"], format!("Acknowledged #{comment}: will do."));

    // The ack does not loop: polls after it steer nothing, so the steer count stays 1.
    wait_until("the worker's turn after the ack", || log.len() >= 3).await;
    for _ in 0..3 {
        let r = tick(&mut watcher).await;
        assert_eq!(r.fetched, vec![T]);
        assert!(r.steered.is_empty(), "the ack was steered back: {r:?}");
    }

    // The manager stops it: abandoned with the reason, handed back once, never completed, and
    // no steer was left undelivered.
    let stop = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "pausing to rethink"}),
    )
    .await;
    assert_eq!(stop["status"], "stopped", "{stop}");
    assert_eq!(stop["returned"], true, "{stop}");
    assert!(live_tickets(&sup).is_empty());
    let reported = f.calls("amp_report_run");
    assert_eq!(reported.len(), 1, "{reported:?}");
    assert_eq!(reported[0]["outcome"], "abandoned");
    assert_eq!(
        reported[0]["run_note"],
        "abandoned: cancelled: stopped by manager: pausing to rethink"
    );
    let returned = f.calls("amp_return_task");
    assert_eq!(returned.len(), 1, "{returned:?}");
    assert_eq!(returned[0]["task_id"], T);
    let stopped = launch_end(&sup, T).await.expect("a report");
    assert_eq!(stopped.outcome, WorkerOutcome::Cancelled);
    assert!(
        stopped
            .notes
            .iter()
            .all(|n| !n.contains("undelivered steer")),
        "{:?}",
        stopped.notes
    );
    let first_session = stopped.session_file.clone().expect("a session file");

    // Resume continue: one new dispatch and claim, the same session (the comment and the ack
    // are in the model's context), and the old comment is not steered again.
    let before = log.len();
    // The stopped run was killed in `sleep 30`, whose session lock outlives the run's live entry
    // by up to ~1.5 s; the resume waits for it (#4281), so it succeeds first time, no retry.
    let resumed = control_tool(
        &sup,
        "amp_resume_worker",
        json!({"task_id": T, "mode": "continue", "reason": "carry on with the comment"}),
    )
    .await;
    assert_eq!(resumed["status"], "resumed", "{resumed}");
    assert_eq!(resumed["mode"], "continue", "{resumed}");
    f.wait_for_calls("amp_claim_task", 2).await;
    wait_until("the resumed worker's first turn", || log.len() > before).await;
    let continued = log.get(before);
    assert_eq!(
        steered_ids(&continued),
        vec![comment],
        "the earlier steer is in the continued session, once"
    );
    assert_eq!(tool_calls(&continued, "amp_add_task_comment"), 1);
    for _ in 0..2 {
        let r = tick(&mut watcher).await;
        assert!(r.steered.is_empty(), "re-steered after resume: {r:?}");
    }

    // #4281: continue on the LIVE continued worker while it is inside its long bash command
    // (`sleep 30`): the resume stops it, waits for its session lock, and resumes first time.
    // Its first turn (awaited above) answered `sleep 30`; give bash a moment to start it.
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        live_tickets(&sup).contains(&T),
        "the continued worker is live"
    );
    let before = log.len();
    let resumed_live = control_tool(
        &sup,
        "amp_resume_worker",
        json!({"task_id": T, "mode": "continue", "reason": "pick up the new comment"}),
    )
    .await;
    assert_eq!(resumed_live["status"], "resumed", "{resumed_live}");
    assert_eq!(resumed_live["stop"]["status"], "stopped", "{resumed_live}");
    let continued_end = launch_end(&sup, T)
        .await
        .expect("the first continued run's report");
    assert_eq!(continued_end.outcome, WorkerOutcome::Cancelled);
    assert_eq!(continued_end.session_file.as_ref(), Some(&first_session));
    f.wait_for_calls("amp_claim_task", 3).await;
    wait_until("the second continued worker's first turn", || {
        log.len() > before
    })
    .await;
    assert_eq!(steered_ids(&log.get(before)), vec![comment]);

    // Restart: the continued run is stopped first, then a fresh session (nothing from before in
    // its context).
    let before = log.len();
    let restarted = control_tool(
        &sup,
        "amp_restart_worker",
        json!({"task_id": T, "reason": "start clean"}),
    )
    .await;
    assert_eq!(restarted["status"], "restarted", "{restarted}");
    assert_eq!(restarted["stop"]["status"], "stopped", "{restarted}");
    let continued_end = launch_end(&sup, T)
        .await
        .expect("the continued run's report");
    assert_eq!(continued_end.outcome, WorkerOutcome::Cancelled);
    assert_eq!(
        continued_end.session_file.as_ref(),
        Some(&first_session),
        "continue reused the first run's session file"
    );
    f.wait_for_calls("amp_claim_task", 4).await;
    wait_until("the restarted worker's first turn", || log.len() > before).await;
    let fresh = log.get(before);
    assert!(steered_ids(&fresh).is_empty(), "a fresh session");
    assert!(!has_tool_result(&fresh), "a fresh session");
    assert_eq!(tool_calls(&fresh, "amp_add_task_comment"), 0);

    // Done: stop it. Four runs, four dispatches, each handed back, none completed, and never
    // two live workers on the ticket.
    let last = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "done testing"}),
    )
    .await;
    assert_eq!(last["status"], "stopped", "{last}");
    let restarted_end = launch_end(&sup, T)
        .await
        .expect("the restarted run's report");
    assert_ne!(
        restarted_end.session_file.as_ref(),
        Some(&first_session),
        "restart started a new session file"
    );

    // #4281: a session lock still held when the limit runs out is `busy`, and nothing is
    // dispatched. The stopped run's lock lingers a moment after its `sleep 30` was killed (the
    // very thing #4281 waits for), so take it over once it is free, then hold it.
    let session = sup.latest_session_file(T).expect("a session file");
    let deadline = Instant::now() + Duration::from_secs(10);
    let held = loop {
        match amp_core::session_lock::claim_session_lock(&session) {
            Ok(guard) => break guard,
            Err(e) => assert!(Instant::now() < deadline, "the lock never freed: {e}"),
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    };
    let limit = Duration::from_millis(300);
    let started = Instant::now();
    let busy = worker_control::relaunch(
        &sup,
        T,
        worker_control::Relaunch::Resume(amp_core::run_report::ResumeMode::Continue),
        "held lock",
        worker_control::RelaunchOptions::default(),
        limit,
        &CancellationToken::new(),
    )
    .await
    .expect("not cancelled");
    assert!(started.elapsed() >= limit, "waited out the limit");
    assert_eq!(
        busy.status,
        worker_control::RelaunchStatus::Busy,
        "{busy:?}"
    );
    assert!(busy.note.contains("still held"), "{busy:?}");
    assert_eq!(
        f.calls("amp_dispatch_task").len(),
        4,
        "a busy refusal dispatches nothing"
    );
    drop(held);

    watching.cancel();
    assert_eq!(live_most.await.unwrap(), 1, "never two live workers");
    assert_eq!(f.calls("amp_dispatch_task").len(), 4);
    assert_eq!(f.calls("amp_report_run").len(), 4);
    assert_eq!(f.calls("amp_return_task").len(), 4);
    assert!(f.calls("amp_complete_task").is_empty());
    // One steer in the whole test, acknowledged once.
    let steers: usize = log
        .all()
        .iter()
        .map(|m| steered_ids(m).len())
        .max()
        .unwrap_or(0);
    assert_eq!(steers, 1);
    assert_eq!(f.calls("amp_add_task_comment").len(), 1);
}

/// A comment that arrives after the worker's `amp_complete_task` is refused `Closed`: the watcher
/// stops watching the run and the model never gets another turn for it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_comment_after_the_worker_closed_its_ticket_is_refused_closed() {
    const T: i64 = 5101;
    let f = fixture("steer-after-close").await;
    let sup = f.supervisor();
    let mut watcher = f.watcher(&sup);
    let log = Arc::new(Log::default());
    let (entered, gate) = (f.dir.join("entered"), f.dir.join("gate"));
    let step = {
        let (log, entered, gate) = (Arc::clone(&log), entered.clone(), gate.clone());
        move || {
            let (log, entered, gate) = (Arc::clone(&log), entered.clone(), gate.clone());
            FauxStep::factory(move |ctx: &Context| {
                log.0.lock().unwrap().push(ctx.messages.clone());
                match tool_calls(&ctx.messages, "bash") {
                    _ if tool_calls(&ctx.messages, "amp_complete_task") == 0 => {
                        faux_assistant_message(
                            vec![FauxBlock::tool_call(
                                "amp_complete_task",
                                json!({"task_id": T}),
                            )],
                            StopReason::ToolUse,
                        )
                    }
                    0 => bash(gated(&entered, &gate)),
                    _ => {
                        faux_assistant_message(vec![FauxBlock::text("Finished.")], StopReason::Stop)
                    }
                }
            })
        }
    };
    f.script((0..6).map(|_| step()).collect());

    sup.launch(
        RunRequest {
            task_id: T,
            agent_id: "worker-backend".into(),
            ..RunRequest::default()
        },
        CancellationToken::new(),
    )
    .await
    .expect("launched");
    f.wait_for_calls("amp_complete_task", 1).await;
    wait_until("the worker is in its blocked tool", || entered.exists()).await;

    let baseline = tick(&mut watcher).await;
    assert!(baseline.steered.is_empty(), "{baseline:?}");
    f.amp
        .mock
        .add_comment(T, "jacob@example.com", "One more thing after all.");
    let late = tick(&mut watcher).await;
    assert!(late.steered.is_empty(), "steered a closed ticket: {late:?}");
    // The supervisor's own answer, as the watcher got it.
    assert_eq!(
        sup.steer_worker(T, "late", "test").await,
        Err(SteerError::Closed(T))
    );
    // The watcher has given up on this run: it does not even read the ticket again.
    let after = tick(&mut watcher).await;
    assert!(after.fetched.is_empty(), "{after:?}");

    std::fs::write(&gate, "go").unwrap();
    let report = launch_end(&sup, T).await.expect("a report");
    assert_eq!(report.outcome, WorkerOutcome::Completed, "{report:?}");
    assert!(
        report
            .notes
            .iter()
            .all(|n| !n.contains("undelivered steer")),
        "{:?}",
        report.notes
    );
    assert!(
        log.all().iter().all(|m| steered_ids(m).is_empty()),
        "the late comment never reached the model"
    );
    assert!(
        f.calls("amp_return_task").is_empty(),
        "a closed ticket stays closed"
    );
}

/// Wave check #4234, pinned as it behaves today: a steer taken while the worker's tool is blocked,
/// then the run stopped in that same step. rpi drains the queued steer into the transcript as the
/// aborted step settles (it rides in one last model request whose answer the abort throws away),
/// the supervisor counts that echo as delivery, and so the run's report has no `undelivered
/// steer` note, although the worker never acted on the comment (no ack). If this changes (the
/// note appears), update this test and the steering KB doc.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_steer_taken_then_stopped_in_the_same_step_is_counted_delivered_today() {
    const T: i64 = 5102;
    let f = fixture("steer-then-stop").await;
    let sup = f.supervisor();
    let mut watcher = f.watcher(&sup);
    let log = Arc::new(Log::default());
    let (entered, gate) = (f.dir.join("entered"), f.dir.join("gate-never"));
    f.script(
        (0..6)
            .map(|_| steerable_worker(Arc::clone(&log), T, entered.clone(), gate.clone()))
            .collect(),
    );

    sup.launch(
        RunRequest {
            task_id: T,
            agent_id: "worker-backend".into(),
            ..RunRequest::default()
        },
        CancellationToken::new(),
    )
    .await
    .expect("launched");
    f.wait_for_calls("amp_claim_task", 1).await;
    wait_until("the worker is in its blocked tool", || entered.exists()).await;
    tick(&mut watcher).await;
    let comment = f
        .amp
        .mock
        .add_comment(T, "jacob@example.com", "Stop and think first.");
    assert_eq!(tick(&mut watcher).await.steered, vec![(T, comment)]);

    let stop = control_tool(
        &sup,
        "amp_stop_worker",
        json!({"task_id": T, "reason": "stop now"}),
    )
    .await;
    assert_eq!(stop["status"], "stopped", "{stop}");
    let report = launch_end(&sup, T).await.expect("a report");
    assert_eq!(report.outcome, WorkerOutcome::Cancelled);
    // rpi drained the queued steer after the aborted tool's result and built one more model
    // request with it (the faux provider answers regardless of the abort; the run discards the
    // answer): the steer is in the transcript, so the supervisor saw it echoed and counts it as
    // delivered...
    let requests = log.all();
    let last = requests.last().expect("model requests");
    assert_eq!(
        steered_ids(last),
        vec![comment],
        "drained into the aborted step"
    );
    assert!(
        requests[..requests.len() - 1]
            .iter()
            .all(|m| steered_ids(m).is_empty()),
        "only the aborted step's request carries it"
    );
    // ...so the report has no `undelivered steer` note, although the worker never acted on it.
    assert!(
        report
            .notes
            .iter()
            .all(|n| !n.contains("undelivered steer")),
        "today: counted as delivered, no undelivered note: {:?}",
        report.notes
    );
    assert!(
        f.calls("amp_add_task_comment").is_empty(),
        "never acknowledged"
    );
}
