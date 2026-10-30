//! AMP #2889: a prompt sent while the manager's reply runs waits in a queue and runs, in order, as
//! its own turn once that reply ends. That holds for a prompt typed in the chat (the TUI's queue)
//! and for one from a paired device (the remote link's queue), and also when the reply launched a
//! worker: since #2837 the chat launches workers in the background, so the turn ends and the queue
//! moves on while the worker keeps running. Esc (or a remote `stop`) stops the reply and drops both
//! queues, and the workers keep running (#2858).
//!
//! Drives the real [`Shell`] with the manager feed and the real [`RemoteLink`] against #2373's
//! in-process mock relay, on a faux manager that launches a worker through a [`Supervisor`] in
//! Launch mode with the real [`RunReporter`], against the mock AMP. The worker runs `sleep 30`
//! until the test stops it. Repos and state live under cargo's target tmp dir (never /tmp: D13).

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use amp_core::{
    agent::{AgentSpec, Runtime, RuntimeOptions},
    events::{ChannelEmitter, FanOutEmitter},
    manager_feed::ManagerFeed,
    project::ProjectMarker,
    providers::StaticKeys,
    remote::{
        FixedToken, LinkTuning, LocalEvent, RemoteDeps, RemoteLink, RemoteSetup,
        crypto::{self, KeyPair, KeyStore},
        envelope,
        mock_relay::{MockOptions, MockRelay, is_chat},
        pairing,
        redact::Redactor,
        relay::now_ms,
    },
    run_report::RunReporter,
    supervisor::{DispatchMode, ManagerContext, Supervisor, SupervisorConfig, WorkerHooks},
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use amp_tui::{Shell, ShellInfo};
use common::{MockAmp, Running, spawn};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    providers::faux::{FauxBlock, FauxStep, faux_assistant_message},
    types::{Content, Context, Message, StopReason, UserContent},
};
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

const A: i64 = 9001;
const PERSON: &str = "jo@example.com";
const USER: i64 = 1001;
const WAIT: Duration = Duration::from_secs(20);

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

fn texts(content: &[Content]) -> String {
    content
        .iter()
        .filter_map(|c| match c {
            Content::Text(t) => Some(t.text.clone()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// The newest user message the manager was sent.
fn last_user(ctx: &Context) -> String {
    ctx.messages
        .iter()
        .rev()
        .find_map(|m| match m {
            Message::User(u) => Some(match &u.content {
                UserContent::Text(t) => t.clone(),
                UserContent::Blocks(b) => texts(b),
            }),
            _ => None,
        })
        .unwrap_or_default()
}

/// Whether the newest message is a tool result (the manager's second call in a turn).
fn after_tool(ctx: &Context) -> bool {
    matches!(ctx.messages.last(), Some(Message::ToolResult(_)))
}

/// The manager: a prompt with "run ticket" in it launches ticket A and then says so; any other
/// prompt is answered at once. Every turn records the prompt it answered, in order.
fn manager(turns: &Arc<Mutex<Vec<String>>>) -> FauxStep {
    let turns = Arc::clone(turns);
    FauxStep::factory(move |ctx: &Context| {
        let user = last_user(ctx);
        if user.contains("think") {
            // A reply still being written when Esc arrives (the faux stream checks the stop
            // signal once this returns).
            std::thread::sleep(Duration::from_millis(1500));
        }
        if user.contains("run ticket") && !after_tool(ctx) {
            return faux_assistant_message(
                vec![FauxBlock::tool_call(
                    "amp_run_ticket",
                    json!({"task_id": A, "agent_id": "amp:amp-worker-backend", "worktree": false}),
                )],
                StopReason::ToolUse,
            );
        }
        turns.lock().unwrap().push(user.clone());
        let reply = if after_tool(ctx) {
            format!("Launched #{A}; it runs in the background.")
        } else {
            "On it.".to_owned()
        };
        faux_assistant_message(vec![FauxBlock::text(reply)], StopReason::Stop)
    })
}

/// A worker that runs `sleep 30` (until the test stops it), then says done.
fn sleeper() -> FauxStep {
    FauxStep::factory(|ctx: &Context| {
        if after_tool(ctx) {
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
    })
}

struct Rig {
    amp: Running,
    sup: Arc<Supervisor>,
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
    feed: Arc<ManagerFeed>,
    /// The prompts the manager answered, in order.
    turns: Arc<Mutex<Vec<String>>>,
    _dir: PathBuf,
}

async fn rig(name: &str) -> Rig {
    rig_with(name, DispatchMode::Launch).await
}

async fn rig_with(name: &str, dispatch: DispatchMode) -> Rig {
    let dir: PathBuf = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-chat-queue")
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
            dispatch,
            ..SupervisorConfig::default()
        },
        Some(RunReporter::new(Arc::clone(&client)) as Arc<dyn WorkerHooks>),
    );
    let turns: Arc<Mutex<Vec<String>>> = Arc::default();
    rt.providers
        .faux_script("boss")
        .unwrap()
        .set_responses((0..12).map(|_| manager(&turns)).collect());
    rt.providers
        .faux_script("wf")
        .unwrap()
        .set_responses((0..4).map(|_| sleeper()).collect());

    // The manager's events go to the shell and to the manager feed, as in `amp-harness chat`.
    let (tui, events) = ChannelEmitter::new();
    let fanout = FanOutEmitter::new();
    fanout.add_sink(tui.into_emitter());
    let feed = ManagerFeed::new();
    fanout.add_sink(feed.emitter());
    let mut spec = AgentSpec::new("amp-manager", repo.clone());
    spec.extra_tools = vec![sup.tool()];
    spec.emitter = Some(fanout.into_emitter());
    let manager = rt.build(spec).await.unwrap();
    let info = ShellInfo {
        role: "amp-manager".into(),
        ..ShellInfo::default()
    };
    let (mut shell, done) = Shell::new(Arc::clone(&manager.harness), info, Vec::new());
    shell.set_supervisor(Some(Arc::clone(&sup)));
    shell.set_person(PERSON);
    shell.set_feed(Some(Arc::clone(&feed)));
    std::mem::forget(feed.spawn_flusher());
    Rig {
        amp,
        sup,
        shell,
        events,
        done,
        feed,
        turns,
        _dir: dir,
    }
}

fn key(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::NONE)
}

impl Rig {
    /// What the terminal loop does each tick.
    fn pump(&mut self) {
        while let Ok(event) = self.events.try_recv() {
            self.shell.handle_event(&event);
        }
        while let Ok(result) = self.done.try_recv() {
            while let Ok(event) = self.events.try_recv() {
                self.shell.handle_event(&event);
            }
            self.shell.finish_run(result);
        }
        self.shell.poll();
    }

    /// Pumps until `ok` holds (20 s at most).
    async fn until(&mut self, what: &str, mut ok: impl FnMut(&mut Self) -> bool) {
        let deadline = Instant::now() + WAIT;
        loop {
            self.pump();
            if ok(self) {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "timed out waiting for {what}\nturns: {:?}\n{}",
                self.turns(),
                self.screen()
            );
            tokio::time::sleep(Duration::from_millis(30)).await;
        }
    }

    /// Like [`Rig::until`], but holds back the end of the running reply (as if it were still
    /// streaming): agent events and the shell's poll run, a finished run is not applied.
    async fn until_holding(&mut self, what: &str, mut ok: impl FnMut(&mut Self) -> bool) {
        let deadline = Instant::now() + WAIT;
        loop {
            while let Ok(event) = self.events.try_recv() {
                self.shell.handle_event(&event);
            }
            self.shell.poll();
            if ok(self) {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "timed out waiting for {what}\n{}",
                self.screen()
            );
            tokio::time::sleep(Duration::from_millis(30)).await;
        }
    }

    /// Pumps for `d` without waiting for anything.
    async fn settle(&mut self, d: Duration) {
        let end = Instant::now() + d;
        while Instant::now() < end {
            self.pump();
            tokio::time::sleep(Duration::from_millis(30)).await;
        }
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(160, 60).join("\n")
    }

    fn turns(&self) -> Vec<String> {
        self.turns.lock().unwrap().clone()
    }

    fn worker_live(&self) -> bool {
        self.sup.session(A).is_some_and(|s| s.is_live())
    }

    /// Stops the worker and waits for it to end, so no `sleep 30` outlives the test.
    async fn stop_worker(&mut self) {
        self.sup.cancel_all();
        self.until("the worker to stop", |r| !r.worker_live()).await;
        assert!(!self.amp.mock.calls_to("amp_dispatch_task").is_empty());
    }

    /// Starts the remote link and pairs "Safari on iPhone". Returns the link and the device.
    async fn pair(&mut self, mock: &MockRelay) -> (RemoteLink, KeyPair) {
        let dir = self._dir.join("remote");
        std::fs::create_dir_all(&dir).unwrap();
        let link = RemoteLink::new(RemoteDeps {
            setup: RemoteSetup {
                api_base: mock.base.clone(),
                web_origin: "https://amp.example.test".into(),
                ca_file: None,
                project_id: Some(2),
                host: "test-host".into(),
                os: "macos".into(),
                version: "0.0.0-test".into(),
            },
            tokens: Arc::new(FixedToken(MockOptions::default().oauth_token, USER)),
            host: self.shell.remote_host(),
            feed: Arc::clone(&self.feed),
            // Never the real keychain in tests (the Mac may be locked).
            keys: KeyStore::memory(),
            pins: Some(dir.join("remote-devices.json")),
            action_log: Some(dir.join("remote-actions.log")),
            pending_ends: Some(dir.join("remote-pending-ends.json")),
            redactor: Redactor::new(),
            switches: Arc::new(|| Ok(())),
            tuning: LinkTuning {
                batch_every: Duration::from_millis(30),
                tick: Duration::from_millis(50),
                status_min: Duration::from_millis(200),
                ..LinkTuning::default()
            },
        });
        let mut local = link.take_local_events().expect("local events");
        let enabled = link.enable().await.expect("enable");
        self.until("hello", |_| {
            mock.events().iter().any(|e| e["op"] == "hello")
        })
        .await;
        let hid = harness_id(mock);
        let (secret, _) = pairing::parse_link_fragment(&enabled.link).unwrap();
        let device = KeyPair::generate();
        let msg = pairing::build_pair_request(
            &secret,
            &mock.session_id().unwrap(),
            &hid,
            &device,
            "Safari on iPhone",
            now_ms(),
        );
        assert!(mock.send_pair_request(msg, USER, "jsims@example.test"));
        let prompt = loop {
            self.pump();
            match tokio::time::timeout(Duration::from_millis(50), local.recv()).await {
                Ok(Some(LocalEvent::Pairing(p))) => break p,
                Ok(Some(_)) | Err(_) => {}
                Ok(None) => panic!("the link's local events ended"),
            }
        };
        prompt.answer.send(true).unwrap();
        let dev = device.id().to_owned();
        self.until("pair_result accepted", |_| {
            mock.events().iter().any(|e| {
                e["op"] == "pair_result"
                    && e["device_id"] == dev.as_str()
                    && e["body"]["result"] == "accepted"
            })
        })
        .await;
        // Keep the local events drained in the background (notices nobody reads here).
        tokio::spawn(async move { while local.recv().await.is_some() {} });
        (link, device)
    }
}

fn harness_id(mock: &MockRelay) -> String {
    mock.events()
        .into_iter()
        .find(|e| e["op"] == "hello")
        .expect("hello")["harness_id"]
        .as_str()
        .unwrap()
        .to_owned()
}

/// A signed `prompt` from `device`.
fn prompt(mock: &MockRelay, device: &KeyPair, seq: i64, text: &str) -> String {
    command(
        mock,
        device,
        seq,
        "prompt",
        json!({"text": text, "client_msg_id": format!("m-{seq}")}),
    )
}

fn command(mock: &MockRelay, device: &KeyPair, seq: i64, op: &str, body: Value) -> String {
    envelope::sign_command(
        device,
        &mock.session_id().unwrap(),
        &harness_id(mock),
        seq,
        now_ms(),
        &crypto::new_nonce(),
        op,
        body,
    )
}

fn count(mock: &MockRelay, pred: impl Fn(&Value) -> bool) -> usize {
    mock.events().iter().filter(|e| pred(e)).count()
}

/// The user's report: "messages just say they queue up but the manager never does anything with
/// them". A prompt typed while the manager's reply runs is queued, visibly, and runs as the next
/// turn as soon as that reply ends — here a reply that launched a worker, which keeps running.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_prompt_typed_during_a_reply_that_launched_a_worker_runs_right_after_it() {
    let mut r = rig("local").await;
    r.shell.submit("run ticket 9001").await;
    assert!(r.shell.is_running());
    r.shell.submit("what is left on the board?").await;
    r.shell.submit("and the plan?").await;
    assert_eq!(
        r.shell.queued_prompts(),
        ["what is left on the board?", "and the plan?"]
    );
    let screen = r.screen();
    assert!(
        screen.contains("queued #1 · runs after this reply: \"what is left on the board?\""),
        "{screen}"
    );
    assert!(
        screen.contains("queued #2 · runs after this reply"),
        "{screen}"
    );
    assert!(
        screen.contains("2 queued"),
        "the count is on the spinner line\n{screen}"
    );
    assert!(!screen.contains("still working"), "{screen}");

    r.until("both queued prompts to run", |r| r.turns().len() == 3)
        .await;
    r.until("the last reply to end", |r| !r.shell.is_running())
        .await;
    let turns = r.turns();
    assert!(turns[0].contains("run ticket 9001"), "{turns:?}");
    assert!(turns[1].contains("what is left on the board?"), "{turns:?}");
    assert!(turns[2].contains("and the plan?"), "{turns:?}");
    assert!(
        r.worker_live(),
        "the queue ran without waiting for the launched worker"
    );
    assert!(r.shell.queued_prompts().is_empty());
    let screen = r.screen();
    assert!(
        !screen.contains("2 queued"),
        "the count went with the queue\n{screen}"
    );
    r.stop_worker().await;
}

/// A paired device's prompt sent during a reply that launched a worker is acked `queued` and
/// runs once that reply ends, while the worker keeps running. With a prompt typed on the laptop
/// queued behind the same reply, both run, one turn each: the laptop's first, then the device's.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_remote_prompt_queued_during_a_reply_that_launched_a_worker_runs_after_it() {
    let mut r = rig("remote").await;
    let mock = MockRelay::start(MockOptions::default()).await;
    let (link, device) = r.pair(&mock).await;
    let mut seq = now_ms();

    seq += 1;
    assert!(mock.send_command(&prompt(&mock, &device, seq, "run ticket 9001")));
    // The reply's end is held back (as a slow reply would be) until both prompts are queued.
    r.until_holding("the remote prompt to start", |r| r.shell.is_running())
        .await;
    seq += 1;
    assert!(mock.send_command(&prompt(&mock, &device, seq, "status from the phone")));
    r.until_holding("the queued ack", |_| {
        count(&mock, |e| {
            e["op"] == "ack" && e["body"]["result"] == "queued"
        }) == 1
    })
    .await;
    let ack = mock
        .events()
        .into_iter()
        .find(|e| e["op"] == "ack" && e["body"]["result"] == "queued")
        .unwrap();
    assert_eq!(ack["body"]["queue_position"], 1);
    r.shell.submit("status from the laptop").await;
    assert_eq!(r.shell.queued_prompts(), ["status from the laptop"]);
    let screen = r.screen();
    assert!(
        screen.contains("queued #1 · runs after this reply: \"status from the laptop\""),
        "{screen}"
    );

    r.until("the queued prompts to run", |r| r.turns().len() == 3)
        .await;
    r.until("the last reply to end", |r| {
        !r.shell.is_running() && count(&mock, |e| is_chat(e, "run_end")) == 3
    })
    .await;
    let turns = r.turns();
    assert!(turns[0].contains("run ticket 9001"), "{turns:?}");
    assert!(
        turns[1].contains("status from the laptop"),
        "laptop first: {turns:?}"
    );
    assert!(
        turns[2].contains("[remote: Safari on iPhone] status from the phone"),
        "then the phone: {turns:?}"
    );
    assert!(
        r.worker_live(),
        "the remote queue ran without waiting for the launched worker"
    );
    let queue = mock
        .events()
        .into_iter()
        .rev()
        .find(|e| is_chat(e, "queue"))
        .expect("a queue event");
    assert_eq!(
        queue["body"]["items"],
        json!([]),
        "the remote queue drained"
    );
    r.stop_worker().await;
    link.disable().await;
}

/// Esc stops the reply and drops the queue (as a remote `stop` does): the transcript says so, the
/// dropped prompts never reach the manager, and the launched worker keeps running (#2858).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn esc_stops_the_reply_drops_both_queues_and_leaves_the_worker_running() {
    let mut r = rig("esc").await;
    let mock = MockRelay::start(MockOptions::default()).await;
    let (link, device) = r.pair(&mock).await;
    let mut seq = now_ms();

    // A reply that launched the worker ends; the worker runs on.
    r.shell.submit("run ticket 9001").await;
    r.until("the launch reply to end", |r| {
        !r.shell.is_running() && r.worker_live() && r.turns().len() == 1
    })
    .await;

    // A new reply, with a prompt queued behind it here and on the phone; then Esc.
    r.shell.submit("think about it").await;
    r.shell.submit("never sent from the laptop").await;
    seq += 1;
    assert!(mock.send_command(&prompt(&mock, &device, seq, "never sent from the phone")));
    r.until_holding("the phone's prompt to queue", |_| {
        count(&mock, |e| {
            e["op"] == "ack" && e["body"]["result"] == "queued"
        }) == 1
    })
    .await;
    assert!(r.shell.is_running());
    let screen = r.screen();
    assert!(screen.contains("1 queued"), "{screen}");
    assert!(r.shell.key(&key(KeyCode::Esc), true).await);
    r.until("the reply to stop", |r| {
        !r.shell.is_running()
            && count(&mock, |e| {
                is_chat(e, "run_end") && e["body"]["outcome"] == "aborted"
            }) == 1
    })
    .await;
    let screen = r.screen();
    assert!(screen.contains("Stopped."), "{screen}");
    assert!(
        screen.contains("dropped the queued prompt (↑ recalls it)"),
        "{screen}"
    );
    assert!(r.shell.queued_prompts().is_empty());
    let queue = mock
        .events()
        .into_iter()
        .rev()
        .find(|e| is_chat(e, "queue"))
        .expect("a queue event");
    assert_eq!(
        queue["body"]["items"],
        json!([]),
        "Esc drops the remote queue"
    );
    // Nothing queued starts afterwards.
    r.settle(Duration::from_millis(800)).await;
    let turns = r.turns();
    assert!(!turns.iter().any(|t| t.contains("never sent")), "{turns:?}");
    assert_eq!(count(&mock, |e| is_chat(e, "run_start")), 2);
    assert!(r.worker_live(), "Esc left the launched worker running");
    r.stop_worker().await;
    link.disable().await;
}

/// The root cause of the report, kept as a regression marker: in Await mode (the chat's default
/// before #2837) the reply that dispatched a worker lasted as long as the worker, so a queued
/// prompt said "queued" and then sat there for up to an hour; and stopping the reply to get to it
/// dropped it. The queue itself always drained at the reply's end: here it runs the moment the
/// worker (and so the reply) ends.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn in_await_mode_the_queue_waits_for_the_worker_because_the_reply_does() {
    let mut r = rig_with("await", DispatchMode::Await).await;
    let mock = MockRelay::start(MockOptions::default()).await;
    let (link, device) = r.pair(&mock).await;
    let mut seq = now_ms();
    seq += 1;
    assert!(mock.send_command(&prompt(&mock, &device, seq, "run ticket 9001")));
    r.until("the worker to run", |r| r.worker_live()).await;
    seq += 1;
    assert!(mock.send_command(&prompt(&mock, &device, seq, "status from the phone")));
    r.until("the queued ack", |_| {
        count(&mock, |e| {
            e["op"] == "ack" && e["body"]["result"] == "queued"
        }) == 1
    })
    .await;
    r.settle(Duration::from_millis(1500)).await;
    assert!(r.shell.is_running(), "the reply waits for its worker");
    assert!(r.turns().is_empty(), "{:?}", r.turns());
    // The worker ends (stopped here; finishing does the same): the reply ends, the queue runs.
    r.sup.cancel_all();
    r.until("the queued prompt to run", |r| {
        r.turns()
            .iter()
            .any(|t| t.contains("status from the phone"))
    })
    .await;
    assert!(!r.worker_live());
    link.disable().await;
}
