//! M2-6 (#2098): automatic run reporting (D8), in-process against the mock AMP on faux providers.
//! Workers run through the supervisor with [`RunReporter`] as its hooks, in throwaway git repos
//! (and a bare "remote") under cargo's target tmp dir (never /tmp: D13).
//!
//! Covers: a completed run carries model, tokens and git (pushed) without overwriting what
//! the model set; a blocked run on an unpushed commit; calls on another ticket are left alone;
//! `amp_session_usage` for a worker; abandoned runs (error, turn budget) reported with
//! `amp_report_run` and a note; AMP refusing that report; a read-only worker reports no git.

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::Arc,
    time::Duration,
};

use amp_core::{
    agent::{Runtime, RuntimeOptions},
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
use rpi_ai::{
    providers::faux::{FauxBlock, FauxStep, faux_assistant_message},
    types::{Context, StopReason},
};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

/// Workers on faux provider `wf`. rpi's faux provider estimates tokens but always reports a cost
/// of 0 (the real providers price usage from models.toml), so these runs carry no `cost_usd`: the
/// harness only passes on numbers the provider reported. The unit tests in amp-core's run_report
/// cover the cost field.
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

/// A throwaway repo with `.amp.json` and one commit; with `remote`, a bare `origin` it is pushed
/// to. Plus the faux config and a runtime on a fresh mock AMP.
async fn fixture(name: &str, remote: bool) -> Fixture {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-run-report")
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
    if remote {
        let bare = dir.join("remote.git");
        git(&dir, &["init", "-q", "--bare", bare.to_str().unwrap()]);
        git(&repo, &["remote", "add", "origin", bare.to_str().unwrap()]);
        git(&repo, &["push", "-q", "-u", "origin", "main"]);
    }
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
    fn supervisor(&self, config: SupervisorConfig) -> Arc<Supervisor> {
        let reporter = RunReporter::new(Arc::clone(&self.rt.mcp));
        Supervisor::with_hooks(
            Arc::clone(&self.rt),
            ManagerContext {
                role: "amp-manager".into(),
                cwd: self.repo.clone(),
                project: ProjectMarker::find_within(&self.repo, Some(&self.dir)).unwrap(),
            },
            config,
            Some(reporter as Arc<dyn WorkerHooks>),
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

    fn worktree(&self, task_id: i64) -> PathBuf {
        self.repo.join(".worktrees").join(task_id.to_string())
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

fn template(text: &str, tool_calls: Value) -> FauxStep {
    let spec: StepSpec =
        serde_json::from_value(json!({"text": text, "tool_calls": tool_calls})).unwrap();
    spec.to_step()
}

/// A step that first does some git work in `dir` (what a worker's bash would do), then calls
/// `tool` with `args`.
fn git_then_call(
    dir: PathBuf,
    git_steps: Vec<Vec<&'static str>>,
    tool: &'static str,
    args: Value,
) -> FauxStep {
    FauxStep::factory(move |_ctx: &Context| {
        for step in &git_steps {
            if step[0] == "WRITE" {
                std::fs::write(dir.join(step[1]), step[2]).unwrap();
            } else {
                git(&dir, step);
            }
        }
        faux_assistant_message(
            vec![
                FauxBlock::text("Working."),
                FauxBlock::tool_call(tool, args.clone()),
            ],
            StopReason::ToolUse,
        )
    })
}

fn num(v: &Value) -> i64 {
    v.as_i64().unwrap_or_else(|| panic!("not a number: {v}"))
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_completed_run_carries_model_tokens_cost_and_pushed_git_without_overwriting() {
    let f = fixture("completed", true).await;
    let sup = f.supervisor(config());
    let wt = f.worktree(2085);
    f.script(vec![
        // Commit and push the ticket branch, then ask for the session's usage.
        git_then_call(
            wt.clone(),
            vec![
                vec!["WRITE", "work.txt", "done\n"],
                vec!["add", "work.txt"],
                vec!["commit", "-q", "-m", "work (#2085)"],
                vec!["push", "-q", "-u", "origin", "amp-2085"],
            ],
            "amp_session_usage",
            json!({}),
        ),
        // The model reports its own model name and a PR, and nothing else.
        template(
            "usage: {{last_tool_result}}",
            json!([{"name": "amp_complete_task", "arguments": {
                "task_id": 2085, "model": "claude-x", "pr_url": "https://example.com/pr/1"}}]),
        ),
        template("Finished #2085.", json!([])),
    ]);

    let r = sup
        .run_ticket(req(2085, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Completed, "{r:?}");

    let complete = f.calls("amp_complete_task");
    assert_eq!(complete.len(), 1, "{complete:?}");
    let c = &complete[0];
    assert_eq!(c["task_id"], 2085);
    assert_eq!(c["model"], "claude-x", "the model's own value is kept");
    assert_eq!(c["pr_url"], "https://example.com/pr/1");
    // Usage up to and including the message that made the call: more than nothing, no more than
    // the whole run (the final "Finished" message comes after).
    let (input, output) = (num(&c["input_tokens"]), num(&c["output_tokens"]));
    assert!(input > 0 && output > 0, "{c}");
    assert!(
        input <= r.usage.input_tokens && output < r.usage.output_tokens,
        "{c} vs {r:?}"
    );
    assert!(
        c["cache_read_tokens"].is_i64() && c["cache_write_tokens"].is_i64(),
        "{c}"
    );
    assert!(c.get("cost_usd").is_none(), "faux reports no cost: {c}");
    // Git from the worktree: its branch, the commit just made, and it is on its upstream.
    assert_eq!(c["branch"], "amp-2085");
    assert_eq!(c["commit"], git(&wt, &["rev-parse", "HEAD"]));
    assert_eq!(c["pushed"], true);

    // A closed ticket needs no amp_report_run.
    assert!(f.calls("amp_report_run").is_empty());
    assert!(r.notes.is_empty(), "{:?}", r.notes);

    // The worker's amp_session_usage answered with its configured model and live usage.
    let transcript = std::fs::read_to_string(r.session_file.as_ref().unwrap()).unwrap();
    assert!(
        transcript.contains(r#"\"model\":\"wf/wf-1\""#),
        "{transcript}"
    );
    assert!(
        transcript.contains("fills these"),
        "the worker is told it's automatic"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_blocked_run_on_an_unpushed_commit_and_other_tickets_are_left_alone() {
    let f = fixture("blocked", true).await;
    let sup = f.supervisor(config());
    let wt = f.worktree(2085);
    f.script(vec![
        // A local commit only, then a call about some other ticket.
        git_then_call(
            wt.clone(),
            vec![
                vec!["WRITE", "wip.txt", "wip\n"],
                vec!["add", "wip.txt"],
                vec!["commit", "-q", "-m", "wip"],
            ],
            "amp_complete_task",
            json!({"task_id": 9999}),
        ),
        template(
            "Need an answer.",
            json!([{"name": "amp_block_task", "arguments": {
                "task_id": "#2085", "reason": "A or B?", "input_tokens": 5, "branch": ""}}]),
        ),
        template("Asked.", json!([])),
    ]);

    let r = sup
        .run_ticket(req(2085, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Blocked, "{r:?}");

    assert_eq!(
        f.calls("amp_complete_task"),
        vec![json!({"task_id": 9999})],
        "a call on another ticket is not filled in"
    );
    let block = f.calls("amp_block_task");
    assert_eq!(block.len(), 1);
    let b = &block[0];
    assert_eq!(b["reason"], "A or B?");
    assert_eq!(b["input_tokens"], 5, "the model's number is kept");
    assert!(num(&b["output_tokens"]) > 0, "{b}");
    assert_eq!(b["model"], "wf/wf-1", "the configured provider/model");
    assert_eq!(b["branch"], "amp-2085", "an empty string is filled");
    assert_eq!(b["commit"], git(&wt, &["rev-parse", "HEAD"]));
    assert_eq!(b["pushed"], false, "the commit is only local");
    assert!(f.calls("amp_report_run").is_empty());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn abandoned_runs_are_reported_with_usage_git_and_a_note() {
    let f = fixture("abandoned", false).await;

    // 1. The script runs out after one tool call: the run fails (outcome error).
    let sup = f.supervisor(config());
    f.script(vec![template(
        "Looking.",
        json!([{"name": "bash", "arguments": {"command": "true"}}]),
    )]);
    let r = sup
        .run_ticket(req(2085, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Error, "{r:?}");
    let runs = f.calls("amp_report_run");
    assert_eq!(runs.len(), 1, "{runs:?}");
    let run = &runs[0];
    assert_eq!(run["task_id"], 2085);
    assert_eq!(run["outcome"], "abandoned");
    assert_eq!(run["agent_id"], "amp:amp-worker-backend");
    assert_eq!(run["model"], "wf/wf-1");
    let note = run["run_note"].as_str().unwrap();
    assert!(
        note.starts_with("abandoned: error: ") && !note.starts_with("abandoned: error: error"),
        "{note}"
    );
    assert!(!note.contains('\n'), "{note}");
    assert_eq!(num(&run["input_tokens"]), r.usage.input_tokens);
    assert_eq!(num(&run["output_tokens"]), r.usage.output_tokens);
    assert!(run.get("cost_usd").is_none(), "faux reports no cost: {run}");
    let wt = f.worktree(2085);
    assert_eq!(run["branch"], "amp-2085");
    assert_eq!(run["commit"], git(&wt, &["rev-parse", "HEAD"]));
    assert_eq!(run["pushed"], false, "no remote at all");
    assert!(
        r.notes
            .iter()
            .any(|n| n.contains("reported the run to AMP")),
        "{:?}",
        r.notes
    );
    assert!(f.calls("amp_complete_task").is_empty());

    // 2. The turn budget runs out.
    let sup = f.supervisor(SupervisorConfig {
        max_turns: 1,
        ..config()
    });
    f.script(vec![
        template(
            "1",
            json!([{"name": "bash", "arguments": {"command": "true"}}]),
        ),
        template(
            "2",
            json!([{"name": "bash", "arguments": {"command": "true"}}]),
        ),
        template("3", json!([])),
    ]);
    let r = sup
        .run_ticket(req(2086, "worker-backend"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::TurnBudget, "{r:?}");
    let runs = f.calls("amp_report_run");
    assert_eq!(runs.len(), 2);
    assert_eq!(runs[1]["task_id"], 2086);
    assert_eq!(runs[1]["outcome"], "abandoned");
    assert!(
        runs[1]["run_note"]
            .as_str()
            .unwrap()
            .starts_with("abandoned: turn_budget: turn budget of 1 turns"),
        "{}",
        runs[1]
    );

    // 3. A read-only worker stops without closing its ticket: reported, but with no git fields
    //    (it ran in the manager's checkout, which is not its work).
    f.script(vec![template("Nothing to do.", json!([]))]);
    let r = sup
        .run_ticket(req(7, "researcher"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Stopped);
    let runs = f.calls("amp_report_run");
    assert_eq!(runs.len(), 3);
    assert_eq!(runs[2]["run_note"], "abandoned: stopped");
    assert_eq!(runs[2]["agent_id"], "amp:amp-researcher");
    for field in ["branch", "commit", "pushed"] {
        assert!(runs[2].get(field).is_none(), "{field}: {}", runs[2]);
    }

    // 4. AMP refuses the report: the worker's report says so, nothing fails.
    f.script(vec![template("Nothing.", json!([]))]);
    let r = sup
        .run_ticket(req(4041, "researcher"), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(r.outcome, WorkerOutcome::Stopped);
    assert!(
        r.notes
            .iter()
            .any(|n| n.contains("could not report the abandoned run") && n.contains("4041")),
        "{:?}",
        r.notes
    );
}
