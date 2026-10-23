//! #2234 (decision 13, D11): `amp-harness agents ...` and `skills ...` at both scopes (`--user`,
//! `--repo`), and a new agent made with `agents new` that the manager dispatches and that runs on a
//! model resolved through the fallback chain (faux providers, mock AMP, offline).
//!
//! Every HOME, config dir and repo is under cargo's target tmp dir (never /tmp, and never the
//! person's real ~/.config/amp-harness: D13).

mod common;

use std::{
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    time::Duration,
};

use amp_core::{
    agent::{AgentSpec, Runtime, RuntimeOptions},
    faux::StepSpec,
    project::ProjectMarker,
    providers::StaticKeys,
    supervisor::{
        ManagerContext, Supervisor, SupervisorConfig, WorkerHooks, WorkerInfo, WorkerOutcome,
        WorkerReport,
    },
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use common::{MockAmp, Output, spawn};
use rpi_ai::providers::faux::FauxStep;
use rpi_harness::agent_harness::{AgentLane, HarnessRunOutcome};
use serde_json::{Value, json};

/// The manager on `boss`, worker-backend and reviewer on `wf`/`wr`: a new agent's model shows
/// which fallback it took.
const CONFIG: &str = r#"
[providers.boss]
api = "faux"
[providers.boss.models."boss-1"]

[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]
[providers.wf.models."wf-2"]

[providers.wr]
api = "faux"
[providers.wr.models."wr-1"]

[roles.manager]
model = "boss/boss-1"
[roles.worker-backend]
model = "wf/wf-1"
[roles.reviewer]
model = "wr/wr-1"
"#;

const NO_AMP: &str = "http://127.0.0.1:9/mcp";

/// A work dir with HOME (config.toml = [`CONFIG`]) and a project (`.amp.json`) that is a git repo.
fn setup(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-agents")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(dir.join("home/.config/amp-harness")).unwrap();
    std::fs::create_dir_all(dir.join("project")).unwrap();
    std::fs::write(dir.join("home/.config/amp-harness/config.toml"), CONFIG).unwrap();
    std::fs::write(
        dir.join("project/.amp.json"),
        json!({"project_id": 2, "project_name": "AMP v2"}).to_string(),
    )
    .unwrap();
    dir
}

fn user_dir(dir: &Path) -> PathBuf {
    dir.join("home/.config/amp-harness")
}

fn repo_dir(dir: &Path) -> PathBuf {
    dir.join("project/.amp/harness")
}

async fn cli(dir: &Path, args: &[&str]) -> Output {
    cli_env(dir, args, &[]).await
}

async fn cli_env(dir: &Path, args: &[&str], env: &[(&str, &str)]) -> Output {
    let mut all = vec![("EDITOR", "false"), ("VISUAL", "")];
    all.extend_from_slice(env);
    common::run(dir, NO_AMP, args, &all).await
}

fn ok(out: &Output, what: &str) {
    assert_eq!(
        out.code, 0,
        "{what}\nstdout:\n{}\nstderr:\n{}",
        out.stdout, out.stderr
    );
}

fn agent<'a>(list: &'a Value, id: &str) -> &'a Value {
    list["agents"]
        .as_array()
        .unwrap()
        .iter()
        .find(|a| a["id"] == id)
        .unwrap_or_else(|| panic!("no {id} in {list:#}"))
}

/// An "editor" script: `sed` expression applied to the file in place.
fn editor(dir: &Path, name: &str, sed: &str) -> String {
    let path = dir.join(format!("{name}.sh"));
    std::fs::write(
        &path,
        format!("#!/bin/sh\nsed -i.bak '{sed}' \"$1\" && rm -f \"$1.bak\"\n"),
    )
    .unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
    }
    path.to_string_lossy().into_owned()
}

#[tokio::test]
async fn agents_crud_at_user_and_repo_scope() {
    let dir = setup("agents-crud");

    // new: a template at user scope, a copy at repo scope, one with its own model.
    let out = cli(
        &dir,
        &["agents", "new", "sql-expert", "--description", "Writes SQL"],
    )
    .await;
    ok(&out, "new sql-expert");
    assert!(
        out.stdout
            .contains("model:    wf/wf-1, from based_on worker-backend"),
        "{}",
        out.stdout
    );
    assert!(user_dir(&dir).join("agents/sql-expert.md").is_file());

    let out = cli(
        &dir,
        &["agents", "new", "rev2", "--from", "amp-reviewer", "--repo"],
    )
    .await;
    ok(&out, "new rev2 --repo");
    let rev2 = std::fs::read_to_string(repo_dir(&dir).join("agents/rev2.md")).unwrap();
    assert!(
        rev2.contains("based_on: \"amp-reviewer\"") && rev2.contains("dispatch: true"),
        "{rev2}"
    );
    assert!(
        !rev2.contains("\nmodel:"),
        "the copy drops opencode's model: {rev2}"
    );

    let out = cli(
        &dir,
        &[
            "agents",
            "new",
            "fast",
            "--from",
            "blank",
            "--model",
            "wf/wf-2",
            "--thinking",
            "low",
            "--repo",
        ],
    )
    .await;
    ok(&out, "new fast --model --repo");
    let repo_cfg = std::fs::read_to_string(repo_dir(&dir).join("config.toml")).unwrap();
    assert!(
        repo_cfg.contains("[roles.fast]\nmodel = \"wf/wf-2\"\nthinking = \"low\""),
        "{repo_cfg}"
    );

    // Refusals: an existing id, a bad id, an unknown model, --repo outside a project.
    for (args, want) in [
        (vec!["agents", "new", "sql-expert"], "already exists"),
        (vec!["agents", "new", "Bad_Id"], "lowercase"),
        (
            vec!["agents", "new", "x", "--model", "nope/m"],
            "not a configured provider/model",
        ),
        (
            vec!["agents", "new", "x", "--from", "nobody"],
            "not a template",
        ),
    ] {
        let out = cli(&dir, &args).await;
        assert_eq!(out.code, 2, "{args:?}: {}", out.stderr);
        assert!(out.stderr.contains(want), "{args:?}: {}", out.stderr);
    }

    // list --json: origin, model and its source, dispatchable, at both scopes.
    let out = cli(&dir, &["agents", "list", "--json"]).await;
    ok(&out, "list");
    let list = out.json();
    let a = agent(&list, "sql-expert");
    assert_eq!(a["origin"], "user");
    assert_eq!(a["model"], "wf/wf-1");
    assert_eq!(a["dispatchable"], true);
    assert_eq!(a["skills"], json!(["amp-execution"]));
    let a = agent(&list, "rev2");
    assert_eq!(a["origin"], "repo");
    assert_eq!(a["model"], "wr/wr-1");
    assert_eq!(a["skills"], json!(["amp-review", "code-reviewer"]));
    let a = agent(&list, "fast");
    // (faux models don't reason, so the thinking sent is off; the file holds "low".)
    assert_eq!(a["model"], "wf/wf-2");
    assert!(
        a["model_source"].as_str().unwrap().starts_with("repo "),
        "{a}"
    );
    let a = agent(&list, "amp-worker-backend");
    assert_eq!(
        (a["origin"].as_str(), a["dispatchable"].as_bool()),
        (Some("built-in"), Some(true))
    );
    assert_eq!(agent(&list, "amp-manager")["mode"], "primary");

    // show (text and JSON).
    let out = cli(&dir, &["agents", "show", "rev2"]).await;
    ok(&out, "show");
    assert!(
        out.stdout.contains("rev2  (repo)")
            && out.stdout.contains("dispatch:    yes (dispatch: true)"),
        "{}",
        out.stdout
    );
    let out = cli(&dir, &["agents", "show", "amp:fast", "--json"]).await;
    ok(&out, "show --json");
    assert_eq!(out.json()["id"], "fast");

    // agents skills add/remove (user and repo).
    ok(
        &cli(
            &dir,
            &["agents", "skills", "sql-expert", "add", "go-engineer"],
        )
        .await,
        "skills add",
    );
    ok(
        &cli(
            &dir,
            &[
                "agents",
                "skills",
                "rev2",
                "remove",
                "code-reviewer",
                "--repo",
            ],
        )
        .await,
        "skills remove --repo",
    );
    let out = cli(&dir, &["agents", "skills", "sql-expert", "add", "nope"]).await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    let list = cli(&dir, &["agents", "list", "--json"]).await.json();
    assert_eq!(
        agent(&list, "sql-expert")["skills"],
        json!(["amp-execution", "go-engineer"])
    );
    assert_eq!(agent(&list, "rev2")["skills"], json!(["amp-review"]));

    // edit: a built-in agent gets an override in the chosen layer; an invalid edit saves nothing.
    let good = editor(
        &dir,
        "good",
        "s/^description: .*/description: Docs, edited/",
    );
    let out = cli_env(
        &dir,
        &["agents", "edit", "amp-worker-docs", "--repo"],
        &[("EDITOR", &good)],
    )
    .await;
    ok(&out, "edit --repo");
    assert!(repo_dir(&dir).join("agents/amp-worker-docs.md").is_file());
    let list = cli(&dir, &["agents", "list", "--json"]).await.json();
    let a = agent(&list, "amp-worker-docs");
    assert_eq!(
        (a["origin"].as_str(), a["description"].as_str()),
        (Some("repo"), Some("Docs, edited"))
    );
    assert_eq!(a["overrides"], json!(["built-in"]));
    let bad = editor(&dir, "bad", "s/^description: .*/description:/");
    let before = std::fs::read_to_string(user_dir(&dir).join("agents/sql-expert.md")).unwrap();
    let out = cli_env(&dir, &["agents", "edit", "sql-expert"], &[("EDITOR", &bad)]).await;
    assert_eq!(out.code, 1, "{}", out.stderr);
    assert!(
        out.stderr
            .contains("non-empty `description`; nothing was saved"),
        "{}",
        out.stderr
    );
    assert_eq!(
        std::fs::read_to_string(user_dir(&dir).join("agents/sql-expert.md")).unwrap(),
        before
    );
    let out = cli_env(
        &dir,
        &["agents", "edit", "sql-expert"],
        &[("EDITOR", "true")],
    )
    .await;
    ok(&out, "edit unchanged");
    assert!(out.stdout.contains("No changes"), "{}", out.stdout);
    assert!(
        !user_dir(&dir).join(".edit").exists(),
        "scratch files are cleaned up"
    );

    // validate.
    let out = cli(&dir, &["agents", "validate", "--json"]).await;
    ok(&out, "validate");
    assert_eq!(out.json()["ok"], true);

    // remove / reset, at the right scope only.
    let out = cli(&dir, &["agents", "remove", "rev2"]).await;
    assert_eq!(out.code, 2);
    assert!(
        out.stderr.contains("it is in the repo layer"),
        "{}",
        out.stderr
    );
    let out = cli(&dir, &["agents", "remove", "amp-worker-backend"]).await;
    assert!(
        out.code == 2 && out.stderr.contains("agents reset"),
        "{}",
        out.stderr
    );
    ok(
        &cli(&dir, &["agents", "remove", "rev2", "--repo"]).await,
        "remove --repo",
    );
    ok(
        &cli(&dir, &["agents", "remove", "fast", "--repo"]).await,
        "remove fast --repo",
    );
    let repo_cfg = std::fs::read_to_string(repo_dir(&dir).join("config.toml")).unwrap();
    assert!(!repo_cfg.contains("[roles.fast]"), "{repo_cfg}");
    ok(
        &cli(&dir, &["agents", "remove", "sql-expert", "--user"]).await,
        "remove --user",
    );
    let out = cli(&dir, &["agents", "reset", "amp-worker-docs"]).await;
    ok(&out, "reset");
    assert!(
        out.stdout.contains("now the built-in one"),
        "{}",
        out.stdout
    );
    let list = cli(&dir, &["agents", "list", "--json"]).await.json();
    assert_eq!(list["agents"].as_array().unwrap().len(), 11);
    assert!(
        list["agents"]
            .as_array()
            .unwrap()
            .iter()
            .all(|a| a["origin"] == "built-in")
    );

    // --repo outside a project.
    std::fs::remove_file(dir.join("project/.amp.json")).unwrap();
    let out = cli(&dir, &["agents", "new", "x", "--repo"]).await;
    assert!(
        out.code == 2 && out.stderr.contains("not in an AMP project"),
        "{}",
        out.stderr
    );
}

#[tokio::test]
async fn skills_crud_at_user_and_repo_scope() {
    let dir = setup("skills-crud");

    // new from the template (user), import a Claude Code style dir (repo), new --from a dir.
    let out = cli(
        &dir,
        &["skills", "new", "my-skill", "--description", "Does things"],
    )
    .await;
    ok(&out, "skills new");
    let md = std::fs::read_to_string(user_dir(&dir).join("skills/my-skill/SKILL.md")).unwrap();
    assert!(
        md.starts_with("---\nname: \"my-skill\"\ndescription: \"Does things\"\n---"),
        "{md}"
    );

    let src = dir.join("claude-skills/pdf-tools");
    std::fs::create_dir_all(src.join("scripts")).unwrap();
    std::fs::write(
        src.join("SKILL.md"),
        "---\nname: pdf-tools\ndescription: Work with PDFs\n---\n\nUse scripts/split.sh.\n",
    )
    .unwrap();
    std::fs::write(src.join("scripts/split.sh"), "#!/bin/sh\n").unwrap();
    std::fs::write(src.join(".DS_Store"), "x").unwrap();
    let src_s = src.to_string_lossy().into_owned();
    let out = cli(&dir, &["skills", "import", &src_s, "--repo"]).await;
    ok(&out, "import --repo");
    assert!(
        repo_dir(&dir)
            .join("skills/pdf-tools/scripts/split.sh")
            .is_file()
    );
    assert!(!repo_dir(&dir).join("skills/pdf-tools/.DS_Store").exists());
    let out = cli(&dir, &["skills", "import", &src_s]).await;
    assert!(
        out.code == 2 && out.stderr.contains("already exists"),
        "{}",
        out.stderr
    );
    ok(
        &cli(&dir, &["skills", "import", &src_s, "--name", "pdf-two"]).await,
        "import --name",
    );
    ok(
        &cli(
            &dir,
            &["skills", "new", "pdf-three", "--from", &src_s, "--repo"],
        )
        .await,
        "new --from",
    );
    let md = std::fs::read_to_string(repo_dir(&dir).join("skills/pdf-three/SKILL.md")).unwrap();
    assert!(md.contains("name: \"pdf-three\""), "{md}");

    // list / show.
    let out = cli(&dir, &["skills", "list", "--json"]).await;
    ok(&out, "list");
    let list = out.json();
    let find = |name: &str| {
        list["skills"]
            .as_array()
            .unwrap()
            .iter()
            .find(|s| s["name"] == name)
            .cloned()
            .unwrap_or_else(|| panic!("no {name}"))
    };
    assert_eq!(find("my-skill")["origin"], "user");
    assert_eq!(find("pdf-tools")["origin"], "repo");
    assert_eq!(find("amp-execution")["origin"], "built-in");
    assert!(
        find("amp-execution")["preloaded_by"]
            .as_array()
            .unwrap()
            .iter()
            .any(|a| a == "amp-worker-backend")
    );
    let out = cli(&dir, &["skills", "show", "pdf-tools", "--json"]).await;
    ok(&out, "show");
    let show = out.json();
    assert_eq!(show["files"].as_array().unwrap().len(), 2);
    assert!(show["content"].as_str().unwrap().contains("split.sh"));

    // edit a built-in skill's SKILL.md in the user layer: only that file is overridden.
    let good = editor(&dir, "good", "s/^description: .*/description: Go, edited/");
    ok(
        &cli_env(
            &dir,
            &["skills", "edit", "go-engineer"],
            &[("EDITOR", &good)],
        )
        .await,
        "edit",
    );
    let show = cli(&dir, &["skills", "show", "go-engineer", "--json"])
        .await
        .json();
    assert_eq!(show["description"], "Go, edited");
    assert_eq!(show["layers"], json!(["built-in", "user"]));
    let bad = editor(&dir, "bad", "s/^name: .*/name: other-name/");
    let out = cli_env(&dir, &["skills", "edit", "my-skill"], &[("EDITOR", &bad)]).await;
    assert!(
        out.code == 1 && out.stderr.contains("must match its directory"),
        "{}",
        out.stderr
    );

    // A preloaded skill can't be removed until no agent preloads it.
    ok(
        &cli(&dir, &["agents", "new", "helper", "--skills", "pdf-tools"]).await,
        "agent preloading pdf-tools",
    );
    let out = cli(&dir, &["skills", "remove", "pdf-tools", "--repo"]).await;
    assert!(
        out.code == 1
            && out
                .stderr
                .contains("agents skills <agent> remove pdf-tools"),
        "{}",
        out.stderr
    );
    assert!(
        repo_dir(&dir).join("skills/pdf-tools/SKILL.md").is_file(),
        "restored"
    );
    ok(
        &cli(&dir, &["agents", "skills", "helper", "remove", "pdf-tools"]).await,
        "unpreload",
    );
    ok(
        &cli(&dir, &["skills", "remove", "pdf-tools", "--repo"]).await,
        "remove --repo",
    );
    let out = cli(&dir, &["skills", "remove", "my-skill", "--repo"]).await;
    assert!(
        out.code == 2 && out.stderr.contains("it is in the user layer"),
        "{}",
        out.stderr
    );
    ok(
        &cli(&dir, &["skills", "remove", "my-skill"]).await,
        "remove --user",
    );
    let out = cli(&dir, &["skills", "remove", "amp-kb", "--repo"]).await;
    assert!(
        out.code == 2 && out.stderr.contains("skills reset"),
        "{}",
        out.stderr
    );
    ok(
        &cli(&dir, &["skills", "reset", "go-engineer"]).await,
        "reset",
    );
    let show = cli(&dir, &["skills", "show", "go-engineer", "--json"])
        .await
        .json();
    assert_eq!(show["origin"], "built-in");

    let out = cli(&dir, &["skills", "validate", "--json"]).await;
    ok(&out, "validate");
    assert_eq!(out.json()["ok"], true);
}

// ------------------------------------------------------------------------------------------------
// A new agent, dispatched by the manager on faux models.
// ------------------------------------------------------------------------------------------------

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

fn template(text: &str, tool_calls: Value) -> FauxStep {
    let spec: StepSpec =
        serde_json::from_value(json!({"text": text, "tool_calls": tool_calls})).unwrap();
    spec.to_step()
}

/// The acceptance test: `agents new` (no `amp-` prefix, no `[roles]`) makes an agent the manager
/// lists in `amp_run_ticket`, may call it for (the policy hook allows it), and that runs on
/// worker-backend's model through the fallback chain; a repo-scope copy of the reviewer runs on
/// the reviewer's model.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_new_agent_is_dispatched_by_the_manager_on_a_resolved_model() {
    let dir = setup("dispatch");
    let repo = dir.join("project");
    std::fs::write(repo.join("README.md"), "test repo\n").unwrap();
    git(&repo, &["init", "-q", "-b", "main"]);
    git(&repo, &["add", "."]);
    git(&repo, &["commit", "-q", "-m", "init"]);
    ok(
        &cli(
            &dir,
            &["agents", "new", "sql-expert", "--description", "Writes SQL"],
        )
        .await,
        "new",
    );
    ok(
        &cli(
            &dir,
            &["agents", "new", "checker", "--from", "reviewer", "--repo"],
        )
        .await,
        "new --repo",
    );
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
    let rt = Arc::new(
        Runtime::load(
            client,
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
    let hooks = Arc::new(Recorder::default());
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
        Some(Arc::clone(&hooks) as Arc<dyn WorkerHooks>),
    );
    let tool = sup.tool();
    let description = tool.tool.schema().description.clone();
    assert!(
        description.contains("- amp:sql-expert: Writes SQL"),
        "{description}"
    );
    assert!(description.contains("- amp:checker: "), "{description}");

    rt.providers
        .faux_script("boss")
        .unwrap()
        .set_responses(vec![
            FauxStep::tool_call(
                "amp_run_ticket",
                json!({"task_id": 4242, "agent_id": "amp:sql-expert"}),
            ),
            FauxStep::tool_call(
                "amp_run_ticket",
                json!({"task_id": 4243, "agent_id": "checker"}),
            ),
            template("Done: {{last_tool_result}}", json!([])),
        ]);
    rt.providers.faux_script("wf").unwrap().set_responses(vec![
        template(
            "SQL written.",
            json!([{"name": "amp_complete_task", "arguments": {"task_id": 4242}}]),
        ),
        template("Finished #4242.", json!([])),
    ]);
    rt.providers
        .faux_script("wr")
        .unwrap()
        .set_responses(vec![template("Reviewed #4243.", json!([]))]);

    let mut spec = AgentSpec::new("amp-manager", repo.clone());
    spec.project = ProjectMarker::find_within(&repo, Some(&dir)).unwrap();
    spec.extra_tools = vec![tool];
    let manager = rt.build(spec).await.expect("build the manager");
    let result = tokio::time::timeout(
        Duration::from_secs(60),
        manager
            .harness
            .prompt_text("run #4242 then #4243", Vec::new()),
    )
    .await
    .expect("the manager finished")
    .expect("the run");
    assert!(
        matches!(result.outcome, HarnessRunOutcome::Completed { .. }),
        "{:?}",
        result.outcome
    );

    assert_eq!(
        amp.mock.calls_to("amp_dispatch_task"),
        vec![
            json!({"task_id": 4242, "agent_id": "amp:sql-expert"}),
            json!({"task_id": 4243, "agent_id": "amp:checker"}),
        ]
    );
    let finished = hooks.finished.lock().unwrap().clone();
    assert_eq!(finished.len(), 2, "{finished:?}");
    let (info, report) = &finished[0];
    assert_eq!(report.outcome, WorkerOutcome::Completed, "{report:?}");
    assert_eq!(
        (info.role.as_str(), info.model.as_str()),
        ("sql-expert", "wf/wf-1")
    );
    assert_eq!(report.final_message, "Finished #4242.");
    assert_eq!(
        info.worktree.as_deref(),
        Some(repo.join(".worktrees/4242").as_path()),
        "a worker-template agent edits, so it gets a worktree"
    );
    let (info, report) = &finished[1];
    assert_eq!(
        (info.role.as_str(), info.model.as_str()),
        ("checker", "wr/wr-1")
    );
    assert_eq!(report.final_message, "Reviewed #4243.");
    assert!(
        info.worktree.is_none(),
        "a reviewer can't edit: no worktree"
    );
}
