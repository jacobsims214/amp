//! #2242 (H13): CLI changes made while a chat runs take effect after `/reload`, without a
//! restart: a new agent is dispatchable (the roster), a new skill is listed, a role's new model is
//! used (the chat's own and new workers'), and workers already built keep what they were built
//! with. A broken agent file changes nothing. The MCP side (only changed servers restart) is in
//! tests/mcp_hub.rs, which owns the test MCP server.
//!
//! Faux providers, the in-process mock AMP, temp HOMEs and repos under cargo's target tmp dir
//! (never /tmp: D13). The CLI runs as the real binary.

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::Arc,
};

use amp_core::{
    agent::{AgentSpec, LiveSession, Runtime, RuntimeOptions},
    project::ProjectMarker,
    providers::StaticKeys,
    supervisor::{ManagerContext, Supervisor, SupervisorConfig},
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use common::{MockAmp, Output, spawn};
use serde_json::json;

/// The manager on `boss` (or `boss2`), worker-backend on `wf`.
const CONFIG: &str = r#"
[providers.boss]
api = "faux"
[providers.boss.models."boss-1"]

[providers.boss2]
api = "faux"
[providers.boss2.models."boss2-1"]

[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]
[providers.wf.models."wf-2"]

[roles.manager]
model = "boss/boss-1"
[roles.worker-backend]
model = "wf/wf-1"
"#;

const NO_AMP: &str = "http://127.0.0.1:9/mcp";

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

/// HOME with config.toml = [`CONFIG`], and a project (`.amp.json`, project 2) that is a git repo.
fn setup(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-reload")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(dir.join("home/.config/amp-harness")).unwrap();
    std::fs::create_dir_all(dir.join("project")).unwrap();
    std::fs::write(dir.join("home/.config/amp-harness/config.toml"), CONFIG).unwrap();
    let repo = dir.join("project");
    std::fs::write(
        repo.join(".amp.json"),
        json!({"project_id": 2, "project_name": "AMP v2"}).to_string(),
    )
    .unwrap();
    std::fs::write(repo.join(".gitignore"), ".worktrees/\n").unwrap();
    git(&repo, &["init", "-q", "-b", "main"]);
    git(&repo, &["add", "."]);
    git(&repo, &["commit", "-q", "-m", "init"]);
    dir
}

async fn cli(dir: &Path, args: &[&str]) -> Output {
    common::run(dir, NO_AMP, args, &[("EDITOR", "false"), ("VISUAL", "")]).await
}

fn ok(out: &Output, what: &str) {
    assert_eq!(
        out.code, 0,
        "{what}\nstdout:\n{}\nstderr:\n{}",
        out.stdout, out.stderr
    );
}

/// `sh -c` text for `--input exec:`: the binary with `args`, quoted.
fn exec(args: &[&str]) -> String {
    let quote = |s: &str| format!("'{}'", s.replace('\'', r"'\''"));
    let mut parts = vec![quote(env!("CARGO_BIN_EXE_amp-harness"))];
    parts.extend(args.iter().map(|a| quote(a)));
    format!("exec:{}", parts.join(" "))
}

/// The acceptance test, in the chat as a person uses it: the manager can't dispatch an agent that
/// doesn't exist yet; `agents new` and `models set manager …` run in "another terminal" (`exec:`)
/// while the chat runs; after `/reload` the manager runs on its new model and dispatches the new
/// agent, which runs on worker-backend's model through the fallback chain.
#[tokio::test]
async fn the_chat_takes_cli_changes_on_reload() {
    let dir = setup("chat");
    let amp = spawn(MockAmp::new(), None).await;
    let script = dir.join("script.json");
    let run_ticket = |id: &str| {
        json!({"tool_calls": [{"name": "amp_run_ticket", "id": id,
                               "arguments": {"task_id": 4242, "agent_id": "amp:sql-expert"}}]})
    };
    std::fs::write(
        &script,
        json!({
            "boss": [run_ticket("c1"), {"text": "before: {{last_tool_result}}"}],
            "boss2": [run_ticket("c2"), {"text": "after reload: {{last_tool_result}}"}],
            "wf": [{"text": "Finished #4242."}]
        })
        .to_string(),
    )
    .unwrap();
    let state = dir.join("home/.local/state");
    let new_agent = exec(&["agents", "new", "sql-expert", "--description", "Writes SQL"]);
    let set_model = exec(&["models", "set", "manager", "boss2/boss2-1", "--no-check"]);
    let out = common::run(
        &dir,
        &amp.url,
        &[
            "--snapshot",
            "160x200",
            "--no-setup",
            "--input",
            "run #4242 with sql-expert",
            "--input",
            &new_agent,
            "--input",
            &set_model,
            "--input",
            "/reload",
            "--input",
            "try again",
        ],
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await;
    ok(&out, "the chat");
    let screen = &out.stdout;
    let flat = screen.split_whitespace().collect::<Vec<_>>().join(" ");

    // Before: the roster had no sql-expert, so nothing was dispatched.
    assert!(flat.contains("before:"), "{screen}");
    assert!(flat.contains("sql-expert"), "{screen}");
    // The reload says what it took.
    assert!(
        flat.contains("reloaded (workers already running keep theirs)"),
        "{screen}"
    );
    assert!(flat.contains("agents: +sql-expert"), "{screen}");
    assert!(
        flat.contains(
            "model amp-manager: boss/boss-1 (thinking off) → boss2/boss2-1 (thinking off)"
        ),
        "{screen}"
    );
    assert!(
        flat.contains("this chat now runs on boss2/boss2-1"),
        "{screen}"
    );
    // After: the manager (on boss2) dispatched it once, and the worker finished.
    assert_eq!(
        amp.mock.calls_to("amp_dispatch_task"),
        vec![json!({"task_id": 4242, "agent_id": "amp:sql-expert"})],
        "{screen}"
    );
    assert!(flat.contains("after reload:"), "{screen}");
    assert!(flat.contains("Finished #4242."), "{screen}");
    // The footer names the new model.
    assert!(screen.contains("boss2-1"), "{screen}");
}

/// #2334 (QA pass 3 finding 4): the manager role moves (in another terminal) to a provider that
/// has no key in this process. The chat stays on its model and says why, with the way out,
/// instead of the footer and `/model roles` silently disagreeing.
#[tokio::test]
async fn a_role_moved_to_a_provider_without_a_key_here_says_why_the_chat_stays() {
    let dir = setup("stays");
    let config = dir.join("home/.config/amp-harness/config.toml");
    let mut text = std::fs::read_to_string(&config).unwrap();
    text.push_str(
        "\n[providers.example]\napi = \"openai-completions\"\nbase_url = \"http://127.0.0.1:9/v1\"\n\
         key_env = \"AMP_TEST_2334_EXAMPLE_KEY\"\n[providers.example.models.\"orchestrator\"]\n",
    );
    std::fs::write(&config, text).unwrap();
    let set_model = exec(&[
        "models",
        "set",
        "manager",
        "example/orchestrator",
        "--no-check",
    ]);
    let state = dir.join("home/.local/state");
    let out = common::run(
        &dir,
        NO_AMP,
        &[
            "--snapshot",
            "160x60",
            "--no-setup",
            "--input",
            &set_model,
            "--input",
            "/reload",
        ],
        &[("XDG_STATE_HOME", state.to_str().unwrap())],
    )
    .await;
    ok(&out, "the chat");
    let screen = &out.stdout;
    let flat = screen.split_whitespace().collect::<Vec<_>>().join(" ");
    assert!(
        flat.contains(
            "this chat stays on boss/boss-1: amp-manager moved to example/orchestrator, but \
             example has no key here (set AMP_TEST_2334_EXAMPLE_KEY"
        ),
        "{screen}"
    );
    assert!(
        flat.contains("/model picks another for this chat"),
        "{screen}"
    );
    assert!(!flat.contains("this chat now runs on"), "{screen}");
}

/// A runtime over `dir`'s HOME and project, with the mock AMP.
async fn runtime(
    dir: &Path,
    url: &str,
) -> (
    Arc<Runtime>,
    PathBuf,
    impl Fn(&str) -> Option<String> + Sync,
) {
    let repo = std::fs::canonicalize(dir.join("project")).unwrap();
    let client = Arc::new(
        AmpMcpClient::connect(ClientOptions::new(url.to_owned()))
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
    (Arc::new(rt), repo, env)
}

fn skill_names(skills: Option<Vec<rpi_harness::types::Skill>>) -> Vec<String> {
    skills
        .unwrap_or_default()
        .into_iter()
        .map(|s| s.name)
        .collect()
}

/// In process: what `Runtime::reload` reports and puts in place, what `refresh_session` changes
/// in the live manager, and that a worker session built before keeps its snapshot.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn reload_reaches_new_sessions_and_the_live_manager_but_not_running_workers() {
    let dir = setup("runtime");
    let amp = spawn(MockAmp::new(), None).await;
    let (rt, repo, env) = runtime(&dir, &amp.url).await;
    let project = ProjectMarker::find_within(&repo, Some(&dir)).unwrap();
    let sup = Supervisor::with_hooks(
        Arc::clone(&rt),
        ManagerContext {
            role: "amp-manager".into(),
            cwd: repo.clone(),
            project: project.clone(),
        },
        SupervisorConfig::default(),
        None,
    );
    let mut spec = AgentSpec::new("amp-manager", repo.clone());
    spec.project = project.clone();
    spec.extra_tools = vec![sup.tool()];
    let manager = rt.build(spec).await.expect("build the manager");
    let worker = rt
        .build(AgentSpec::new("worker-backend", repo.clone()))
        .await
        .expect("build a worker");
    let worker_prompt = worker.harness.get_system_prompt().await.unwrap();
    let run_ticket_description = |tools: Vec<rpi_harness::types::HarnessTool>| {
        tools
            .iter()
            .find(|t| t.tool.schema().name == "amp_run_ticket")
            .map(|t| t.tool.schema().description.clone())
            .expect("amp_run_ticket")
    };
    assert!(
        !run_ticket_description(manager.harness.get_tools().await.unwrap()).contains("sql-expert")
    );

    // Another terminal.
    ok(
        &cli(
            &dir,
            &["agents", "new", "sql-expert", "--description", "Writes SQL"],
        )
        .await,
        "agents new",
    );
    ok(
        &cli(
            &dir,
            &["skills", "new", "pdf-tools", "--description", "Read PDFs"],
        )
        .await,
        "skills new",
    );
    ok(
        &cli(
            &dir,
            &["models", "set", "worker-backend", "wf/wf-2", "--no-check"],
        )
        .await,
        "models set",
    );
    // Nothing moves until the reload.
    assert!(rt.role("sql-expert").is_err());

    let report = rt
        .reload(&StaticKeys::default(), &env)
        .await
        .expect("reload");
    assert_eq!(report.agents.added, vec!["sql-expert"]);
    assert_eq!(report.skills.added, vec!["pdf-tools"]);
    let wb = report
        .model_change("amp-worker-backend")
        .unwrap_or_else(|| panic!("{report:#?}"));
    assert_eq!(wb.from.as_deref(), Some("wf/wf-1 (thinking off)"));
    assert_eq!(wb.to.as_deref(), Some("wf/wf-2 (thinking off)"));
    assert!(report.model_change("amp-manager").is_none());
    assert!(report.mcp.is_empty(), "{:?}", report.mcp);
    let lines = report.lines().join("\n");
    assert!(lines.contains("agents: +sql-expert"), "{lines}");
    assert!(lines.contains("skills: +pdf-tools"), "{lines}");

    // New sessions: the new agent resolves (worker-backend's new model, via its template's
    // based_on), new workers list the skill.
    let sql = rt
        .role("sql-expert")
        .unwrap()
        .resolve_model(&rt.current_models())
        .unwrap();
    assert_eq!(sql.model_ref(), "wf/wf-2");
    let fresh = rt
        .build(AgentSpec::new("worker-backend", repo.clone()))
        .await
        .unwrap();
    assert_eq!(fresh.resolved.model_ref(), "wf/wf-2");
    assert!(
        skill_names(fresh.harness.get_resources().await.unwrap().skills)
            .contains(&"pdf-tools".to_owned())
    );
    let roster: Vec<String> = sup.dispatchable().into_iter().map(|r| r.id).collect();
    assert!(roster.contains(&"sql-expert".to_owned()), "{roster:?}");

    // The worker built before keeps its snapshot.
    assert_eq!(
        worker.harness.get_system_prompt().await.unwrap(),
        worker_prompt
    );
    assert!(
        !skill_names(worker.harness.get_resources().await.unwrap().skills)
            .contains(&"pdf-tools".to_owned())
    );

    // The live manager, refreshed: the new roster in amp_run_ticket, the new skill listed.
    let refreshed = rt
        .refresh_session(LiveSession {
            harness: &manager.harness,
            role: &manager.role.id,
            cwd: &repo,
            project: project.as_ref(),
            extra_context: None,
            policy: &manager.policy,
            replace_tools: vec![sup.tool()],
        })
        .await
        .expect("refresh");
    assert!(refreshed.skills_changed);
    // A `dispatch: true` agent is a new `task` rule in the manager's policy: in force now.
    assert!(refreshed.policy_changed);
    assert!(
        manager
            .policy
            .check(
                "amp_run_ticket",
                &json!({"task_id": 1, "agent_id": "amp:sql-expert"})
            )
            .is_ok()
    );
    assert_eq!(refreshed.resolved.unwrap().model_ref(), "boss/boss-1");
    let description = run_ticket_description(manager.harness.get_tools().await.unwrap());
    assert!(
        description.contains("- amp:sql-expert: Writes SQL"),
        "{description}"
    );
    assert!(
        skill_names(manager.harness.get_resources().await.unwrap().skills)
            .contains(&"pdf-tools".to_owned())
    );
    let tools = manager.harness.get_tools().await.unwrap();
    assert_eq!(
        tools
            .iter()
            .filter(|t| t.tool.schema().name == "amp_run_ticket")
            .count(),
        1,
        "replaced, not added"
    );

    // A second reload with nothing changed reports nothing.
    let again = rt.reload(&StaticKeys::default(), &env).await.unwrap();
    assert!(again.is_empty(), "{again:#?}");
    rt.mcp_hub.shutdown().await;
}

/// A broken agent file (it preloads a skill that doesn't exist) fails the reload with what to
/// fix, and changes nothing, not even the models changed at the same time.
#[tokio::test]
async fn a_broken_agent_file_changes_nothing() {
    let dir = setup("broken");
    let amp = spawn(MockAmp::new(), None).await;
    let (rt, _repo, env) = runtime(&dir, &amp.url).await;
    ok(
        &cli(
            &dir,
            &["models", "set", "worker-backend", "wf/wf-2", "--no-check"],
        )
        .await,
        "models set",
    );
    let agents = dir.join("home/.config/amp-harness/agents");
    std::fs::create_dir_all(&agents).unwrap();
    std::fs::write(
        agents.join("bad.md"),
        "---\ndescription: broken\nmode: subagent\nskills: [no-such-skill]\n---\nHello.\n",
    )
    .unwrap();
    let err = rt
        .reload(&StaticKeys::default(), &env)
        .await
        .expect_err("a broken agent fails the reload")
        .to_string();
    assert!(err.contains("no-such-skill"), "{err}");
    assert!(rt.role("bad").is_err());
    let wb = rt
        .role("worker-backend")
        .unwrap()
        .resolve_model(&rt.current_models())
        .unwrap();
    assert_eq!(wb.model_ref(), "wf/wf-1", "models stay as they were");
    // Fixed: the next reload takes both.
    std::fs::remove_file(agents.join("bad.md")).unwrap();
    let report = rt.reload(&StaticKeys::default(), &env).await.unwrap();
    assert!(
        report.model_change("amp-worker-backend").is_some(),
        "{report:#?}"
    );
}
