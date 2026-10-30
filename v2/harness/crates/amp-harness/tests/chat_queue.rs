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
//! until the test stops it (or, for AMP #2891's wake tests, until the test opens its gate).
//! Repos and state live under cargo's target tmp dir (never /tmp: D13).
//!
//! AMP #2891: a launched worker that ends while the chat is idle starts one manager turn by
//! itself ([`amp_tui::WAKE_PROMPT`]), carrying the summaries of every end since the last turn;
//! never while a reply runs or a prompt waits (that turn carries them); and a loop guard stops
//! a run of automatic turns nobody asked for.

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicI64, Ordering},
    },
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
use amp_tui::{Shell, ShellInfo, WAKE_LIMIT, WAKE_PROMPT};
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
/// Two more tickets for the wake tests: workers that end when their gate opens.
const B: i64 = 9002;
const C: i64 = 9003;
/// The loop test's tickets start here, one per automatic turn; their gates are open.
const LOOP: i64 = 9100;
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

/// The manager: a prompt with "run ticket" in it launches ticket A and then says so; "run
/// tickets" launches B and C in one message; any other prompt is answered at once. In loop mode
/// (the #2891 loop guard test), every prompt — the automatic ones included — launches the next
/// [`LOOP`] ticket, whose worker ends at once. Every turn records the prompt it answered (with
/// any worker summaries above it), in order.
fn manager(turns: &Arc<Mutex<Vec<String>>>, looping: &Arc<AtomicBool>) -> FauxStep {
    let turns = Arc::clone(turns);
    let looping = Arc::clone(looping);
    let next = Arc::new(AtomicI64::new(LOOP));
    FauxStep::factory(move |ctx: &Context| {
        let user = last_user(ctx);
        if user.contains("think") {
            // A reply still being written when Esc arrives (the faux stream checks the stop
            // signal once this returns).
            std::thread::sleep(Duration::from_millis(1500));
        }
        let launch = |task_id: i64| {
            FauxBlock::tool_call(
                "amp_run_ticket",
                json!({"task_id": task_id, "agent_id": "amp:amp-worker-backend", "worktree": false}),
            )
        };
        if !after_tool(ctx) {
            let calls = if looping.load(Ordering::SeqCst) {
                vec![launch(next.fetch_add(1, Ordering::SeqCst))]
            } else if user.contains("run tickets") {
                vec![launch(B), launch(C)]
            } else if user.contains("run ticket") {
                vec![launch(A)]
            } else {
                Vec::new()
            };
            if !calls.is_empty() {
                return faux_assistant_message(calls, StopReason::ToolUse);
            }
        }
        turns.lock().unwrap().push(user.clone());
        let reply = if after_tool(ctx) {
            "Launched; it runs in the background.".to_owned()
        } else {
            "On it.".to_owned()
        };
        faux_assistant_message(vec![FauxBlock::text(reply)], StopReason::Stop)
    })
}

/// A worker that waits (30 s at most, until the test stops it) for its ticket's gate file in
/// `gates` to appear, then says done. Ticket A's gate never opens: it runs until stopped.
fn gated(gates: &Path) -> FauxStep {
    let gates = gates.to_path_buf();
    FauxStep::factory(move |ctx: &Context| {
        if after_tool(ctx) {
            return faux_assistant_message(vec![FauxBlock::text("done")], StopReason::Stop);
        }
        let first = ctx
            .messages
            .iter()
            .find_map(|m| match m {
                Message::User(u) => Some(match &u.content {
                    UserContent::Text(t) => t.clone(),
                    UserContent::Blocks(b) => texts(b),
                }),
                _ => None,
            })
            .unwrap_or_default();
        let ticket: String = first
            .split_once("ticket #")
            .map(|(_, rest)| rest.chars().take_while(char::is_ascii_digit).collect())
            .unwrap_or_default();
        let gate = gates.join(ticket);
        faux_assistant_message(
            vec![FauxBlock::tool_call(
                "bash",
                json!({ "command": format!(
                    "for i in $(seq 600); do [ -e '{}' ] && exit 0; sleep 0.05; done",
                    gate.display()
                )}),
            )],
            StopReason::ToolUse,
        )
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
    /// The manager launches a worker on every turn ([`manager`]).
    looping: Arc<AtomicBool>,
    /// The workers' gate files ([`gated`]).
    gates: PathBuf,
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
    let looping: Arc<AtomicBool> = Arc::default();
    let gates = dir.join("gates");
    std::fs::create_dir_all(&gates).unwrap();
    // One factory per step: a step is popped per model call, whoever asks.
    let boss = manager(&turns, &looping);
    rt.providers
        .faux_script("boss")
        .unwrap()
        .set_responses((0..60).map(|_| boss.clone()).collect());
    rt.providers
        .faux_script("wf")
        .unwrap()
        .set_responses((0..60).map(|_| gated(&gates)).collect());

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
        looping,
        gates,
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

    /// Lets ticket `id`'s worker end.
    fn open_gate(&self, id: i64) {
        std::fs::write(self.gates.join(id.to_string()), "").unwrap();
    }

    /// The manager's turns that the chat started by itself (AMP #2891).
    fn wakes(&self) -> usize {
        self.turns()
            .iter()
            .filter(|t| t.contains(WAKE_PROMPT))
            .count()
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

/// AMP #2891: with the chat idle, two launched workers that end close together start exactly one
/// manager turn, by themselves. It is marked as automatic in the transcript, its prompt carries
/// both summaries, and nothing is left for a later turn: the person's next prompt goes as typed.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn an_idle_chat_wakes_the_manager_once_for_workers_that_end_together() {
    let mut r = rig("wake-idle").await;
    r.shell.submit("run tickets 9002 and 9003").await;
    r.until("the launch reply to end", |r| {
        !r.shell.is_running() && r.turns().len() == 1 && r.sup.session(C).is_some()
    })
    .await;
    // Idle with the workers running: nothing ended, so nothing wakes.
    r.settle(Duration::from_millis(800)).await;
    assert_eq!(r.turns().len(), 1, "{:?}", r.turns());

    r.open_gate(B);
    r.open_gate(C);
    r.until("the automatic turn to end", |r| {
        r.wakes() == 1 && !r.shell.is_running()
    })
    .await;
    // Long enough for a second wake, if one were coming.
    r.settle(Duration::from_millis(1500)).await;
    let turns = r.turns();
    assert_eq!(turns.len(), 2, "one automatic turn: {turns:?}");
    let wake = &turns[1];
    assert!(wake.starts_with("[worker summaries"), "{wake}");
    assert!(wake.contains(&format!("#{B} ")), "{wake}");
    assert!(wake.contains(&format!("#{C} ")), "{wake}");
    assert!(wake.contains(WAKE_PROMPT), "{wake}");
    assert_eq!(r.sup.pending_launch_ends(), 0);
    let screen = r.screen();
    assert!(
        screen.contains("2 workers you launched ended while the chat was idle"),
        "the transcript says what started the turn\n{screen}"
    );
    assert!(screen.contains("not a prompt from you"), "{screen}");

    // Each end reached exactly one turn: the person's next prompt has none.
    r.shell.submit("anything else?").await;
    r.until("the person's turn", |r| {
        r.turns().len() == 3 && !r.shell.is_running()
    })
    .await;
    let turns = r.turns();
    assert!(turns[2].starts_with("anything else?"), "{turns:?}");
}

/// AMP #2891: no automatic turn while a reply runs or a prompt waits — the end goes to the
/// queued prompt's turn instead — nor with `wake_on_worker_end` off, when the end waits for the
/// person. Turned back on, the idle chat wakes for it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn no_wake_while_busy_or_queued_or_switched_off() {
    let mut r = rig("wake-busy").await;
    r.shell.submit("run tickets 9002 and 9003").await;
    r.until("the launch reply to end", |r| {
        !r.shell.is_running() && r.turns().len() == 1 && r.sup.session(C).is_some()
    })
    .await;

    // A slow reply runs, a prompt is queued behind it, and a worker ends meanwhile.
    r.shell.submit("think it over").await;
    r.shell.submit("and then?").await;
    r.open_gate(B);
    r.until_holding("the end to show mid-reply", |r| {
        r.screen().contains(&format!("worker ended: #{B}"))
    })
    .await;
    assert!(r.shell.is_running());
    r.until("the queued prompt's turn", |r| {
        r.turns().len() == 3 && !r.shell.is_running()
    })
    .await;
    r.settle(Duration::from_millis(1500)).await;
    let turns = r.turns();
    assert_eq!(turns.len(), 3, "no automatic turn: {turns:?}");
    assert_eq!(r.wakes(), 0, "{turns:?}");
    assert!(turns[1].starts_with("think it over"), "{turns:?}");
    assert!(
        turns[2].starts_with("[worker summaries") && turns[2].contains(&format!("#{B} ")),
        "the queued prompt's turn carries the end: {turns:?}"
    );
    assert!(turns[2].contains("and then?"), "{turns:?}");

    // Switched off: the end waits for the person.
    r.shell.set_wake_on_worker_end(false);
    r.open_gate(C);
    r.until("the second end", |r| r.sup.pending_launch_ends() == 1)
        .await;
    r.settle(Duration::from_millis(1500)).await;
    assert_eq!(r.turns().len(), 3, "{:?}", r.turns());
    assert_eq!(r.sup.pending_launch_ends(), 1);

    // Back on: the idle chat wakes for it.
    r.shell.set_wake_on_worker_end(true);
    r.until("the automatic turn", |r| {
        r.wakes() == 1 && !r.shell.is_running()
    })
    .await;
    let turns = r.turns();
    assert!(turns[3].contains(&format!("#{C} ")), "{turns:?}");
    assert_eq!(r.sup.pending_launch_ends(), 0);
}

/// AMP #2891: a paired device's prompt queued in the remote link carries the end; the chat does
/// not start an automatic turn ahead of it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_remote_prompt_in_the_queue_takes_the_end_instead_of_a_wake() {
    let mut r = rig("wake-remote").await;
    let mock = MockRelay::start(MockOptions::default()).await;
    let (link, device) = r.pair(&mock).await;
    // As `amp-harness chat` does: the shell reads the link's state (its queue among it).
    r.shell.set_remote(Ok(link.clone()), None);
    let mut seq = now_ms();
    r.shell.submit("run tickets 9002 and 9003").await;
    r.until("the launch reply to end", |r| {
        !r.shell.is_running() && r.turns().len() == 1 && r.sup.session(C).is_some()
    })
    .await;

    r.shell.submit("think it over").await;
    seq += 1;
    assert!(mock.send_command(&prompt(&mock, &device, seq, "status from the phone")));
    r.until_holding("the phone's prompt to queue", |_| {
        count(&mock, |e| {
            e["op"] == "ack" && e["body"]["result"] == "queued"
        }) == 1
    })
    .await;
    r.open_gate(B);
    r.until_holding("the end to show mid-reply", |r| {
        r.screen().contains(&format!("worker ended: #{B}"))
    })
    .await;
    r.until("the phone's turn", |r| {
        r.turns().len() == 3 && !r.shell.is_running()
    })
    .await;
    r.settle(Duration::from_millis(1500)).await;
    let turns = r.turns();
    assert_eq!(turns.len(), 3, "no automatic turn: {turns:?}");
    assert!(
        turns[2].starts_with("[worker summaries") && turns[2].contains(&format!("#{B} ")),
        "{turns:?}"
    );
    assert!(
        turns[2].contains("[remote: Safari on iPhone] status from the phone"),
        "{turns:?}"
    );
    r.open_gate(C);
    r.until("the automatic turn for the last end", |r| {
        r.wakes() == 1 && !r.shell.is_running()
    })
    .await;
    link.disable().await;
}

/// AMP #2891's loop guard: a manager that launches another worker on every turn, each ending at
/// once, gets WAKE_LIMIT automatic turns and then a warning, not an endless loop. The last end
/// waits for the person, whose prompt carries it and lets automatic turns start again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_loop_guard_stops_automatic_turns_until_the_person_speaks() {
    let mut r = rig("wake-loop").await;
    for id in LOOP..LOOP + 20 {
        r.open_gate(id);
    }
    r.looping.store(true, Ordering::SeqCst);
    r.shell.submit("keep going").await;
    r.until("the loop guard to trip", |r| {
        r.shell.wake_paused() && !r.shell.is_running()
    })
    .await;
    r.until("the last launched worker's end", |r| {
        r.sup.pending_launch_ends() == 1
    })
    .await;
    r.settle(Duration::from_millis(1500)).await;
    assert_eq!(r.wakes(), WAKE_LIMIT, "{:?}", r.turns());
    assert_eq!(r.turns().len(), 1 + WAKE_LIMIT);
    // Joined, so a wrapped line reads as one.
    let screen = r.screen().split_whitespace().collect::<Vec<_>>().join(" ");
    assert!(
        screen.contains(&format!(
            "The manager started {WAKE_LIMIT} turns by itself in the last 10 minutes"
        )),
        "{screen}"
    );
    assert!(
        screen.contains("One ended worker waits for your next prompt"),
        "{screen}"
    );
    assert_eq!(r.sup.pending_launch_ends(), 1, "the end waits");

    // The person speaks: their turn carries the waiting end, and the guard resets.
    r.looping.store(false, Ordering::SeqCst);
    r.shell.submit("stop launching").await;
    assert!(!r.shell.wake_paused());
    r.until("the person's turn", |r| {
        r.turns().len() == 2 + WAKE_LIMIT && !r.shell.is_running()
    })
    .await;
    let turns = r.turns();
    let last = turns.last().unwrap();
    assert!(last.starts_with("[worker summaries"), "{last}");
    assert!(last.contains("stop launching"), "{last}");
    assert_eq!(r.sup.pending_launch_ends(), 0);
}

/// A `/server` switch that waits until the test lets it fail.
struct HeldSwitch(Mutex<Option<tokio::sync::oneshot::Receiver<()>>>);

impl amp_tui::ServerControl for HeldSwitch {
    fn describe(&self) -> String {
        "test server".into()
    }
    fn switch(
        self: Arc<Self>,
        _target: String,
    ) -> std::pin::Pin<
        Box<dyn std::future::Future<Output = Result<amp_tui::ServerSwitched, String>> + Send>,
    > {
        let rx = self.0.lock().unwrap().take();
        Box::pin(async move {
            if let Some(rx) = rx {
                let _ = rx.await;
            }
            Err("the test let the switch fail".to_owned())
        })
    }
}

/// AMP #2891: no automatic turn while a `/server` switch runs; once it is over, the idle chat
/// wakes for the end that waited.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn no_wake_during_a_server_switch() {
    let mut r = rig("wake-switch").await;
    let (release, held) = tokio::sync::oneshot::channel();
    r.shell
        .set_server_control(Arc::new(HeldSwitch(Mutex::new(Some(held)))));
    r.shell.submit("run tickets 9002 and 9003").await;
    r.until("the launch reply to end", |r| {
        !r.shell.is_running() && r.turns().len() == 1 && r.sup.session(C).is_some()
    })
    .await;
    // Both end with waking off (`/server` waits for live workers), then the switch starts.
    r.shell.set_wake_on_worker_end(false);
    r.open_gate(B);
    r.open_gate(C);
    r.until("both ends", |r| {
        r.sup.pending_launch_ends() == 2 && r.sup.launched_running() == 0
    })
    .await;
    r.shell.submit("/server elsewhere").await;
    r.until("the switch to start", |r| {
        r.screen().contains("switching to elsewhere")
    })
    .await;
    r.shell.set_wake_on_worker_end(true);
    r.settle(Duration::from_millis(1500)).await;
    assert_eq!(r.wakes(), 0, "{:?}", r.turns());
    assert_eq!(r.sup.pending_launch_ends(), 2);

    release.send(()).unwrap();
    r.until("the automatic turn after the switch", |r| {
        r.wakes() == 1 && !r.shell.is_running()
    })
    .await;
    assert_eq!(r.sup.pending_launch_ends(), 0);
}

/// AMP #2891: quitting starts nothing: the workers it stops end, and no automatic turn follows.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn no_wake_once_quitting() {
    let mut r = rig("wake-quit").await;
    r.shell.submit("run tickets 9002 and 9003").await;
    r.until("the launch reply to end", |r| {
        !r.shell.is_running() && r.turns().len() == 1 && r.sup.session(C).is_some()
    })
    .await;
    let outcome = tokio::time::timeout(
        Duration::from_secs(30),
        r.shell
            .quit(&mut r.events, &mut r.done, std::future::pending(), |_| {}),
    )
    .await
    .expect("quit settles");
    assert_eq!(outcome, amp_tui::shell::QuitOutcome::Settled);
    r.until("both stopped workers' ends", |r| {
        r.sup.pending_launch_ends() == 2
    })
    .await;
    r.settle(Duration::from_millis(1500)).await;
    assert_eq!(r.wakes(), 0, "{:?}", r.turns());
    assert!(!r.shell.is_running());
}

/// The newest `status` the link sent: (`busy`, `workers_running`).
fn last_status(mock: &MockRelay) -> Option<(bool, i64)> {
    mock.events()
        .into_iter()
        .rev()
        .find(|e| e["op"] == "status")
        .map(|e| {
            (
                e["body"]["busy"].as_bool().unwrap_or(false),
                e["body"]["workers_running"].as_i64().unwrap_or(-1),
            )
        })
}

/// AMP #2838: a paired device sees `busy` while a manager reply runs (an automatic wake turn is
/// one) and never because workers run: an idle manager with a live worker reports not busy, with
/// the worker in `workers_running`, and takes the device's prompt at once. A prompt sent during
/// the wake turn queues, and starts as soon as the turn ends.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn remote_busy_is_the_managers_reply_not_its_workers() {
    let mut r = rig("remote-busy").await;
    let mock = MockRelay::start(MockOptions::default()).await;
    let (link, device) = r.pair(&mock).await;
    r.shell.set_remote(Ok(link.clone()), None);
    let mut seq = now_ms();
    let acks = |result: &str| {
        count(&mock, |e| {
            e["op"] == "ack" && e["body"]["op"] == "prompt" && e["body"]["result"] == result
        })
    };

    // An active reply is busy (its end held back, as a slow reply's would be).
    seq += 1;
    assert!(mock.send_command(&prompt(&mock, &device, seq, "run ticket 9001")));
    r.until_holding("the launch reply, busy", |r| {
        r.shell.is_running()
            && r.turns().len() == 1
            && r.worker_live()
            && last_status(&mock).is_some_and(|(busy, _)| busy)
    })
    .await;

    // The reply ends, the worker runs on: not busy, the worker counted apart.
    r.until("idle with the worker running, not busy", |r| {
        !r.shell.is_running() && r.worker_live() && last_status(&mock) == Some((false, 1))
    })
    .await;
    seq += 1;
    assert!(mock.send_command(&prompt(&mock, &device, seq, "status from the phone")));
    r.until("the phone's prompt to start at once", |r| {
        acks("started") == 2 && r.turns().len() == 2 && !r.shell.is_running()
    })
    .await;
    assert_eq!(acks("queued"), 0, "a running worker queued the prompt");
    assert!(r.worker_live());

    // The worker ends: the automatic wake turn is a reply, so busy.
    r.until("not busy again", |_| {
        last_status(&mock).is_some_and(|(busy, _)| !busy)
    })
    .await;
    r.open_gate(A);
    r.until_holding("the wake turn, busy", |r| {
        r.wakes() == 1 && last_status(&mock).is_some_and(|(busy, _)| busy)
    })
    .await;
    assert!(r.shell.is_running());
    seq += 1;
    assert!(mock.send_command(&prompt(&mock, &device, seq, "and next?")));
    r.until_holding("the prompt to queue behind the wake turn", |_| {
        acks("queued") == 1
    })
    .await;

    // The wake turn ends: busy clears and the queue starts the next prompt.
    r.until("the queued prompt's turn", |r| {
        r.turns().len() == 4 && !r.shell.is_running()
    })
    .await;
    let turns = r.turns();
    assert!(turns[2].contains(WAKE_PROMPT), "{turns:?}");
    assert!(
        turns[3].contains("[remote: Safari on iPhone] and next?"),
        "{turns:?}"
    );
    r.until("not busy, no workers", |_| {
        last_status(&mock) == Some((false, 0))
    })
    .await;
    link.disable().await;
}
