//! #2252: opening a worker's session from the chat, switching between workers, stopping one, and
//! reading a finished one. Drives the real [`Shell`] on a faux manager that dispatches two
//! tickets at once through a [`Supervisor`] with the real [`RunReporter`], against the mock AMP.
//! Workers run `sleep 30` until stopped. Repos live under cargo's target tmp dir (never /tmp:
//! D13).

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use amp_core::{
    agent::{AgentSpec, Runtime, RuntimeOptions},
    events::ChannelEmitter,
    meta::MetaHandle,
    project::ProjectMarker,
    providers::StaticKeys,
    run_report::RunReporter,
    supervisor::{ManagerContext, Supervisor, SupervisorConfig, WorkerHooks, WorkerOutcome},
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use amp_tui::{
    ESC_HINT, ESC_HINT_FOR, Shell, ShellInfo, chrome::sample, esc_hint, workers::READ_ONLY_KEYS,
};
use common::{MockAmp, Running, spawn};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    providers::faux::{FauxBlock, FauxStep, faux_assistant_message},
    types::{Content, Context, Message, StopReason},
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
const B: i64 = 9002;
const PERSON: &str = "jo@example.com";

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
    rt: Arc<Runtime>,
    client: Arc<AmpMcpClient>,
    repo: PathBuf,
    dir: PathBuf,
    sup: Arc<Supervisor>,
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
    /// What the manager saw as amp_run_ticket's results, once both workers ended.
    manager_saw: Arc<Mutex<Vec<String>>>,
}

fn has_tool_result(ctx: &Context) -> bool {
    ctx.messages
        .iter()
        .any(|m| matches!(m, Message::ToolResult(_)))
}

fn tool_results(ctx: &Context) -> Vec<String> {
    ctx.messages
        .iter()
        .filter_map(|m| match m {
            Message::ToolResult(r) => Some(
                r.content
                    .iter()
                    .filter_map(|c| match c {
                        Content::Text(t) => Some(t.text.clone()),
                        _ => None,
                    })
                    .collect::<Vec<_>>()
                    .join("\n"),
            ),
            _ => None,
        })
        .collect()
}

fn sleeper() -> FauxStep {
    FauxStep::factory(|ctx: &Context| {
        if has_tool_result(ctx) {
            faux_assistant_message(vec![FauxBlock::text("done")], StopReason::Stop)
        } else {
            faux_assistant_message(
                vec![
                    FauxBlock::thinking("I should look around first."),
                    FauxBlock::text("Looking around."),
                    FauxBlock::tool_call("bash", json!({ "command": "sleep 30" })),
                ],
                StopReason::ToolUse,
            )
        }
    })
}

/// A manager whose first turn dispatches tickets A and B in one message; its second turn records
/// the results it got back.
async fn rig(name: &str) -> Rig {
    let dir: PathBuf = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-chat-workers")
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
    let sup = supervisor(&rt, &client, &repo, &dir);
    let manager_saw: Arc<Mutex<Vec<String>>> = Arc::default();
    let saw = Arc::clone(&manager_saw);
    rt.providers.faux_script("boss").unwrap().set_responses(vec![
        FauxStep::factory(|_ctx: &Context| {
            let call = |id: i64| {
                FauxBlock::tool_call(
                    "amp_run_ticket",
                    json!({"task_id": id, "agent_id": "amp:amp-worker-backend", "worktree": false}),
                )
            };
            faux_assistant_message(vec![call(A), call(B)], StopReason::ToolUse)
        }),
        FauxStep::factory(move |ctx: &Context| {
            *saw.lock().unwrap() = tool_results(ctx);
            faux_assistant_message(vec![FauxBlock::text("noted")], StopReason::Stop)
        }),
    ]);
    rt.providers
        .faux_script("wf")
        .unwrap()
        .set_responses((0..6).map(|_| sleeper()).collect());

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
    shell.set_person(PERSON);
    Rig {
        amp,
        rt,
        client,
        repo,
        dir,
        sup,
        shell,
        events,
        done,
        manager_saw,
    }
}

fn supervisor(
    rt: &Arc<Runtime>,
    client: &Arc<AmpMcpClient>,
    repo: &Path,
    dir: &Path,
) -> Arc<Supervisor> {
    Supervisor::with_hooks(
        Arc::clone(rt),
        ManagerContext {
            role: "amp-manager".into(),
            cwd: repo.to_path_buf(),
            project: ProjectMarker::find_within(repo, Some(dir)).unwrap(),
        },
        SupervisorConfig {
            abort_grace: Duration::from_secs(5),
            ..SupervisorConfig::default()
        },
        Some(RunReporter::new(Arc::clone(client)) as Arc<dyn WorkerHooks>),
    )
}

fn key(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::NONE)
}

impl Rig {
    fn pump(&mut self) {
        while let Ok(event) = self.events.try_recv() {
            self.shell.handle_event(&event);
        }
        while let Ok(result) = self.done.try_recv() {
            self.shell.finish_run(result);
        }
        self.shell.poll();
    }

    /// Pumps until `ok` holds (20 s at most).
    async fn until(&mut self, what: &str, mut ok: impl FnMut(&mut Self) -> bool) {
        let deadline = Instant::now() + Duration::from_secs(20);
        loop {
            self.pump();
            if ok(self) {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "timed out waiting for {what}\n{}",
                self.screen()
            );
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(150, 40).join("\n")
    }

    async fn press(&mut self, code: KeyCode) -> bool {
        self.shell.key(&key(code), true).await
    }

    fn calls(&self, tool: &str, task: i64) -> Vec<Value> {
        self.amp
            .mock
            .calls_to(tool)
            .into_iter()
            .filter(|c| c["task_id"] == task)
            .collect()
    }

    /// Whether `task`'s worker has started its run and its first bash call (the `sleep 30`).
    fn in_bash(&self, task: i64) -> bool {
        self.sup.feed(task).is_some_and(|feed| {
            feed.since(0).0.iter().any(|(_, e)| {
                matches!(e, AgentEvent::ToolExecutionStart { tool_name, .. } if tool_name == "bash")
            })
        })
    }

    fn finished(&self, task: i64) -> bool {
        self.sup.session(task).is_some_and(|s| !s.is_live())
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn open_follow_switch_stop_and_read_a_finished_worker() {
    let mut r = rig("open-switch-stop").await;
    r.shell.submit("run both tickets").await;
    // Opened as soon as the worker exists: the view then follows it live.
    r.until("both workers", |r| r.sup.sessions().len() == 2)
        .await;
    // #2347: the manager's transcript scrolled up stays where it was across a session.
    for n in 0..80 {
        r.shell.view().add_notice(&format!("filler line {n}"));
    }
    r.screen();
    r.shell.view().scroll().page_by(-1);
    r.screen();
    let before = r.shell.view().scroll().position();
    assert!(!before.at_end, "{before:?}");
    r.shell.submit(&format!("/worker {A}")).await;
    assert_eq!(r.shell.viewing(), Some(A));
    r.until("the worker's bash call in its session", |r| {
        r.screen().contains("sleep 30")
    })
    .await;
    let screen = r.screen();
    assert!(screen.contains(&format!("worker #{A}")), "{screen}");
    assert!(screen.contains("amp:amp-worker-backend"), "{screen}");
    assert!(screen.contains("wf/wf-1"), "{screen}");
    assert!(screen.contains("running"), "{screen}");
    assert!(
        screen.contains(&format!("You are working AMP ticket #{A}")),
        "the worker's prompt: {screen}"
    );
    assert!(screen.contains("Looking around."), "{screen}");
    assert!(
        !screen.contains("I should look around first."),
        "thinking is collapsed as in the main chat: {screen}"
    );
    assert!(screen.contains("Esc back to the chat"), "{screen}");
    // #2251: the session's bar, transcript and keys line sit inside the shared padding.
    for row in screen.lines().filter(|l| !l.trim().is_empty()) {
        assert!(
            row.starts_with("  ") && row.chars().count() <= 148,
            "{row:?}"
        );
    }
    assert!(
        !screen.contains("run both tickets"),
        "the manager's transcript is not shown: {screen}"
    );

    // #2347: Esc restores the manager's scroll position; the session opens again.
    assert!(r.press(KeyCode::Esc).await);
    r.screen();
    let after = r.shell.view().scroll().position();
    assert_eq!(after.top, before.top, "{before:?} {after:?}");
    assert!(!after.at_end);
    r.shell.view().scroll().to_end();
    r.shell.submit(&format!("/worker {A}")).await;
    assert_eq!(r.shell.viewing(), Some(A));

    // Switch to the other worker and back.
    assert!(r.press(KeyCode::Tab).await);
    assert_eq!(r.shell.viewing(), Some(B));
    r.until("B's session", |r| {
        let s = r.screen();
        s.contains(&format!("worker #{B}")) && s.contains("sleep 30")
    })
    .await;
    assert!(r.press(KeyCode::BackTab).await);
    assert_eq!(r.shell.viewing(), Some(A));

    // x asks; n keeps it running.
    assert!(r.press(KeyCode::Char('x')).await);
    assert_eq!(r.shell.stop_pending(), Some(A));
    assert!(
        r.screen().contains(&format!(
            "stop the worker on #{A}? its run is reported abandoned"
        )),
        "{}",
        r.screen()
    );
    assert!(r.press(KeyCode::Char('n')).await);
    assert_eq!(r.shell.stop_pending(), None);
    assert!(r.sup.session(A).unwrap().is_live());
    assert!(r.calls("amp_report_run", A).is_empty());

    // x then y stops it.
    assert!(r.press(KeyCode::Char('x')).await);
    assert!(r.press(KeyCode::Char('y')).await);
    r.until("A to finish", |r| r.finished(A)).await;
    let a = r.sup.session(A).unwrap();
    assert_eq!(a.outcome, Some(WorkerOutcome::Cancelled));
    assert_eq!(a.stopped_by.as_deref(), Some(PERSON));
    assert!(r.sup.session(B).unwrap().is_live(), "B keeps running");
    // The run is reported abandoned with who stopped it, and the ticket goes back to ready.
    let runs = r.calls("amp_report_run", A);
    assert_eq!(runs.len(), 1, "{runs:?}");
    assert_eq!(runs[0]["outcome"], "abandoned");
    let note = runs[0]["run_note"].as_str().unwrap_or_default();
    assert!(
        note.contains(&format!("stopped by {PERSON} from the chat")),
        "{note}"
    );
    let released = r.calls("amp_set_task_state", A);
    assert_eq!(released.len(), 1, "{released:?}");
    assert_eq!(released[0]["state"], "backlog");
    assert!(
        released[0]["reason"]
            .as_str()
            .unwrap_or_default()
            .contains(&format!("stopped by {PERSON} from the chat")),
        "{}",
        released[0]
    );
    // The finished session stays open, read-only, and says how it ended.
    r.until("the bar to say it stopped", |r| {
        r.screen()
            .contains(&format!("stopped by {PERSON} from the chat after"))
    })
    .await;
    assert!(r.screen().contains("read-only"), "{}", r.screen());
    assert!(r.press(KeyCode::Char('x')).await);
    assert_eq!(r.shell.stop_pending(), None, "nothing to stop");
    assert!(r.screen().contains("has finished: nothing to stop"));

    // Esc goes back to the manager, whose transcript says what happened.
    assert!(r.press(KeyCode::Esc).await);
    assert_eq!(r.shell.viewing(), None);
    let screen = r.screen();
    assert!(
        screen.contains("filler line 79"),
        "the manager's transcript: {screen}"
    );
    assert!(
        screen.contains(&format!(
            "stopping the worker on #{A} (stopped by {PERSON} from the chat)"
        )),
        "{screen}"
    );
    assert!(
        r.shell.is_running(),
        "Esc in a session never stops the manager"
    );

    // Stop B from the /workers panel: Enter watches, Ctrl+X stops (#2395: x is typing).
    r.shell.set_meta(MetaHandle::stub(
        sample::meta(false),
        Arc::new(|_, _| Ok(json!({}))),
    ));
    let sup = Arc::clone(&r.sup);
    r.shell
        .view()
        .chrome()
        .set_workers(Arc::new(move || sup.workers()));
    // #2347: o in a session opens its ticket in AMP's web UI.
    let opened: Arc<Mutex<Vec<String>>> = Arc::default();
    let sink = Arc::clone(&opened);
    r.shell.set_opener(Arc::new(move |url: &str| {
        sink.lock().unwrap().push(url.to_owned());
        Ok(())
    }));
    r.shell.submit(&format!("/worker {B}")).await;
    assert!(r.press(KeyCode::Char('o')).await);
    assert_eq!(
        *opened.lock().unwrap(),
        vec![format!(
            "https://amp.simstech.cloud/project/2/board?task={B}"
        )]
    );
    assert_eq!(r.shell.viewing(), Some(B), "o keeps the session open");
    assert!(r.press(KeyCode::Esc).await);
    r.shell.submit("/workers").await;
    assert!(
        r.screen()
            .contains("Enter/click watch · Ctrl+X stop · Ctrl+G open in AMP"),
        "{}",
        r.screen()
    );
    assert!(r.press(KeyCode::Enter).await);
    assert_eq!(r.shell.viewing(), Some(B), "Enter on the row watches it");
    assert!(r.press(KeyCode::Esc).await);
    r.shell.submit("/workers").await;
    assert!(
        !r.press(KeyCode::Char('x')).await,
        "x in /workers is the editor's"
    );
    assert_eq!(r.shell.stop_pending(), None);
    assert!(
        r.shell
            .key(
                &KeyEvent::new(KeyCode::Char('x'), KeyModifiers::CONTROL),
                true
            )
            .await
    );
    assert_eq!(r.shell.stop_pending(), Some(B));
    assert!(r.press(KeyCode::Enter).await, "Enter confirms too");
    r.until("B to finish", |r| r.finished(B)).await;
    assert_eq!(r.calls("amp_set_task_state", B).len(), 1);

    // Both results went back to the manager, which was told who stopped them.
    r.until("the manager's reply", |r| !r.shell.is_running())
        .await;
    let saw = r.manager_saw.lock().unwrap().join("\n");
    assert!(saw.contains("\"outcome\": \"cancelled\""), "{saw}");
    assert!(
        saw.contains(&format!("stopped by {PERSON} from the chat")),
        "{saw}"
    );
    assert!(
        saw.contains(&format!(
            "{PERSON} watched this worker and stopped it from the chat; ask them before running #{A} again"
        )),
        "{saw}"
    );
    assert!(
        saw.contains(&format!("returned #{A} to ready in AMP")),
        "{saw}"
    );

    // A finished worker of an earlier chat: read from its session file.
    let fresh = supervisor(&r.rt, &r.client, &r.repo, &r.dir);
    assert!(fresh.session(A).is_none());
    let file = fresh.latest_session_file(A).expect("A's session file");
    assert!(file.starts_with(r.rt.state_dir.join("sessions").join(A.to_string())));
    let (mut later, _done) = Shell::new(
        r.rt.build(AgentSpec::new("amp-manager", r.repo.clone()))
            .await
            .unwrap()
            .harness,
        ShellInfo::default(),
        Vec::new(),
    );
    later.set_supervisor(Some(fresh));
    later.submit(&format!("/worker {A}")).await;
    assert_eq!(later.viewing(), Some(A));
    let screen = later.view().render_plain(150, 40).join("\n");
    assert!(screen.contains(&format!("worker #{A}")), "{screen}");
    assert!(screen.contains("from "), "{screen}");
    assert!(
        screen.contains(&format!("You are working AMP ticket #{A}")),
        "{screen}"
    );
    assert!(screen.contains("sleep 30"), "{screen}");
    assert!(screen.contains(READ_ONLY_KEYS.split(" · ").next().unwrap()));
    assert!(later.session(A).is_some_and(|s| !s.is_live()));
    // A ticket with no session anywhere says so.
    later.submit("/worker 123456").await;
    assert_eq!(later.viewing(), Some(A));
    later.key(&key(KeyCode::Esc), true).await;
    let screen = later.view().render_plain(150, 40).join("\n");
    assert!(
        screen.contains("/worker: #123456 has no worker session in this chat or on disk"),
        "{screen}"
    );
}

/// #2354: Esc that leaves a worker's session during the reply doesn't make the next Esc stop the
/// reply (and every worker) unannounced: the spinner line says so first, naming the workers, as
/// it does after Esc closes a panel; the Esc after that stops them, and the transcript says so.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn esc_leaving_a_session_hints_before_the_next_esc_stops_the_workers() {
    let mut r = rig("esc-after-session").await;
    r.shell.submit("run both tickets").await;
    // Both in their `sleep 30`, as when a person watches a busy worker. Claimed is not enough:
    // a worker stopped while it still waits to start (repo probe, slot) is dropped without a
    // finished session (nothing was dispatched yet), so `finished` below would never hold.
    r.until("both workers in their sleep 30", |r| {
        r.sup.workers().len() == 2 && r.in_bash(A) && r.in_bash(B)
    })
    .await;
    let live = r.shell.live_tickets();
    assert_eq!(live.len(), 2, "{live:?}");
    let hint = esc_hint(&live);
    assert_eq!(
        hint,
        format!("{ESC_HINT} and the workers on #{}, #{}", live[0], live[1])
    );

    r.shell.submit(&format!("/worker {A}")).await;
    assert_eq!(r.shell.viewing(), Some(A));
    assert!(r.press(KeyCode::Esc).await);
    assert_eq!(r.shell.viewing(), None);
    assert!(
        r.shell.is_running(),
        "leaving the session doesn't stop the reply"
    );
    assert_eq!(
        r.shell.view().working_message().as_deref(),
        Some(hint.as_str())
    );
    assert!(r.screen().contains(&hint), "{}", r.screen());

    // The hint goes after ESC_HINT_FOR; until then nothing has stopped.
    r.pump();
    assert_eq!(r.sup.workers().len(), 2);
    assert!(r.shell.expire_hints(Instant::now() + ESC_HINT_FOR));
    assert_eq!(
        r.shell.view().working_message().as_deref(),
        Some("Working…")
    );

    // Back in, out again: the hint is back, and the next Esc stops the reply and its workers,
    // naming them.
    r.shell.submit(&format!("/worker {B}")).await;
    assert!(r.press(KeyCode::Esc).await);
    assert_eq!(
        r.shell.view().working_message().as_deref(),
        Some(hint.as_str())
    );
    assert!(r.press(KeyCode::Esc).await);
    assert!(
        r.screen().contains(&format!(
            "stopping the workers on #{}, #{} too; their tickets go back to ready",
            live[0], live[1]
        )),
        "{}",
        r.screen()
    );
    r.until("the reply and both workers to stop", |r| {
        !r.shell.is_running() && r.finished(A) && r.finished(B)
    })
    .await;
}

#[test]
fn esc_hint_names_one_worker_or_caps_many() {
    assert_eq!(esc_hint(&[]), ESC_HINT);
    assert_eq!(
        esc_hint(&[222]),
        format!("{ESC_HINT} and the worker on #222")
    );
    assert_eq!(
        esc_hint(&[1, 2, 3, 4, 5]),
        format!("{ESC_HINT} and the workers on #1, #2, #3 +2 more")
    );
}
