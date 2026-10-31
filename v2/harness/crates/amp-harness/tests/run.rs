//! M2-4 (#2095): `amp-harness run -p` end to end against the in-process mock AMP, on the faux
//! provider with a scripted model (offline, deterministic). Covers: an `amp_*` tool called
//! natively under AMP's own name, AMP's server instructions in the system prompt the model
//! receives, `.amp.json` project context, AGENTS.md, preloaded skills, the manager's tool policy,
//! AMP tool errors, the `--json` event contract and a clear error for a missing provider key.

mod common;

use std::path::Path;

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const FAUX_CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
[roles.worker-backend]
model = "faux/faux-1"
"#;

/// A work dir with `.amp.json` (project 2), the faux config and a faux script.
fn setup(name: &str, script: Value) -> std::path::PathBuf {
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), FAUX_CONFIG).unwrap();
    std::fs::write(dir.join("script.json"), script.to_string()).unwrap();
    dir
}

async fn harness(dir: &Path, url: &str, args: &[&str]) -> Output {
    let script = dir.join("script.json");
    let state = dir.join("home/.local/state");
    run(
        dir,
        url,
        args,
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await
}

fn json_lines(out: &Output) -> Vec<Value> {
    out.stdout
        .lines()
        .map(|l| serde_json::from_str(l).unwrap_or_else(|e| panic!("not JSON ({e}): {l}")))
        .collect()
}

fn of_type<'a>(events: &'a [Value], ty: &str) -> Vec<&'a Value> {
    events.iter().filter(|e| e["type"] == ty).collect()
}

#[tokio::test]
async fn run_calls_an_amp_tool_natively_and_reports_json_events() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(
        "run-json",
        json!([
            {"text": "Checking who I am.", "tool_calls": [{"name": "amp_whoami", "id": "call-1"}]},
            {"text": "You are: {{last_tool_result}}"}
        ]),
    );

    let out = harness(&dir, &amp.url, &["run", "-p", "who am I?", "--json"]).await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let events = json_lines(&out);

    // The session: the manager on the faux model, AMP's tools under AMP's names.
    let session = &events[0];
    assert_eq!(session["type"], "session");
    assert_eq!(session["role"], "amp-manager");
    assert_eq!(session["provider"], "faux");
    assert_eq!(session["model"], "faux-1");
    assert_eq!(session["project_id"], 2);
    assert_eq!(session["mcp"]["url"], amp.url.as_str());
    assert_eq!(session["mcp"]["server"], "amp-mock");
    assert_eq!(session["mcp"]["instructions"], true);
    assert_eq!(session["faux_scripted"], json!(["faux"]));
    // #2367: the manager preloads no skill; it reads amp-planning when it plans.
    assert_eq!(session["skills"], json!([]));
    let tools: Vec<&str> = session["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t.as_str().unwrap())
        .collect();
    // M2-5: the manager gets the native dispatch tool.
    for name in [
        "amp_whoami",
        "amp_get_task",
        "amp_kb_write",
        "read",
        "bash",
        "amp_run_ticket",
    ] {
        assert!(tools.contains(&name), "{name} missing from {tools:?}");
    }
    // The manager gets edit/write only for `.amp.json` (#2363, the amp-init skill); the policy
    // denies every other path. No tool is double-prefixed.
    assert!(
        tools.contains(&"edit") && tools.contains(&"write"),
        "{tools:?}"
    );
    assert!(
        tools.iter().all(|t| !t.starts_with("amp_amp_")),
        "{tools:?}"
    );
    let session_file = session["session_file"].as_str().unwrap();
    assert!(
        session_file.contains("/amp-harness/sessions/amp-manager/")
            && Path::new(session_file).is_file(),
        "{session_file}"
    );

    // The model called amp_whoami; AMP got the call; the result came back to the model.
    assert_eq!(amp.mock.calls_to("amp_whoami"), vec![json!({})]);
    let starts = of_type(&events, "tool_start");
    assert_eq!(starts.len(), 1);
    assert_eq!(
        *starts[0],
        json!({"type": "tool_start", "id": "call-1", "name": "amp_whoami", "arguments": {}})
    );
    let ends = of_type(&events, "tool_end");
    assert_eq!(ends[0]["is_error"], false);
    assert!(
        ends[0]["text"]
            .as_str()
            .unwrap()
            .contains("local-dev@example.com")
    );
    assert_eq!(of_type(&events, "turn_start").len(), 2);
    let assistants = of_type(&events, "assistant");
    assert_eq!(assistants[0]["stop_reason"], "tool_use");
    assert_eq!(assistants[0]["tool_calls"][0]["name"], "amp_whoami");
    assert!(!of_type(&events, "text_delta").is_empty());

    let result = events.last().unwrap();
    assert_eq!(result["type"], "result");
    assert_eq!(result["outcome"], "completed", "{result}");
    assert!(result["error"].is_null());
    assert!(
        result["text"].as_str().unwrap().starts_with("You are: {")
            && result["text"]
                .as_str()
                .unwrap()
                .contains("local-dev@example.com"),
        "{result}"
    );
    assert_eq!(result["turns"], 2);
    assert_eq!(result["tool_calls"], 1);
    assert_eq!(result["tool_errors"], 0);
    assert_eq!(result["tools_used"], json!(["amp_whoami"]));
    assert_eq!(result["model"], "faux/faux-1");
    assert!(
        result["usage"]["input_tokens"].as_i64().unwrap() > 0,
        "{result}"
    );
    assert!(
        result["usage"]["output_tokens"].as_i64().unwrap() > 0,
        "{result}"
    );
}

/// #2205 (QA pass 1): compose's amp-api has no APP_PUBLIC_URL, so `approve_url` is relative and
/// the manager used to hand out `http://localhost:18000/project/N/plan` (the MCP port). With the
/// web UI configured (`AMP_WEB_URL`, as setup saves `[server] web_url`), the model gets the web
/// UI link in the tool result and the web UI origin in its context.
#[tokio::test]
async fn a_planning_turn_gets_the_web_ui_link_not_the_mcp_origin() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(
        "run-web-links",
        json!([
            {"tool_calls": [{"name": "amp_check_plan", "arguments": {"project_id": 2}, "id": "c-1"}]},
            {"text": "Approve it here: {{last_tool_result}}\n---\n{{system_prompt}}"}
        ]),
    );
    let state = dir.join("home/.local/state");
    let script = dir.join("script.json");
    let out = run(
        &dir,
        &amp.url,
        &["run", "-p", "plan it", "--json", "--no-session"],
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
            ("AMP_WEB_URL", "http://localhost:5173"),
        ],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let events = json_lines(&out);
    let text = events.last().unwrap()["text"].as_str().unwrap();
    let mcp_origin = amp.url.trim_end_matches("/mcp");
    assert!(!text.contains(&format!("{mcp_origin}/project")), "{text}");

    let (result, prompt) = text.split_once("\n---\n").unwrap();
    assert!(
        result.contains(r#""approve_url":"http://localhost:5173/project/2/my-work?approve=all""#),
        "{result}"
    );
    // The manager's context names the web UI origin.
    assert!(
        prompt.contains("\namp_web_ui: http://localhost:5173 (AMP's web UI: every link you give a person is on this origin, e.g. http://localhost:5173/project/<id>/my-work?approve=all"),
        "no amp_web_ui line"
    );
}

#[tokio::test]
async fn the_model_receives_amp_instructions_and_project_context() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("run-prompt", json!([{"text": "{{system_prompt}}"}]));
    std::fs::write(
        dir.join("project/AGENTS.md"),
        "Repo rule: run make check before committing.",
    )
    .unwrap();

    // What the model was actually sent (the faux step echoes its system prompt).
    let out = harness(
        &dir,
        &amp.url,
        &["run", "-p", "hi", "--json", "--no-session"],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let events = json_lines(&out);
    assert!(events[0]["session_file"].is_null());
    let prompt = events.last().unwrap()["text"].as_str().unwrap().to_owned();

    // 1. The role prompt.
    assert!(prompt.starts_with("# AMP Manager"), "{}", &prompt[..80]);
    // 2. AMP's server instructions, verbatim.
    assert!(
        prompt.contains(
            "<amp_server_instructions server=\"amp-mock 1.2.3\">\nAMP mock instructions.\n</amp_server_instructions>"
        ),
        "no AMP instructions in the system prompt"
    );
    // 3. The project from .amp.json (its stale amp_api is ignored).
    let project_dir = std::fs::canonicalize(dir.join("project")).unwrap();
    // #2205: the web UI line; without a web URL it is the MCP URL's origin.
    let origin = amp.url.trim_end_matches("/mcp");
    assert!(prompt.contains(&format!(
        "<amp_context>\nrole: amp-manager\namp_server: {}\namp_web_ui: {origin} (AMP's web UI: every link you give a person is on this origin, e.g. {origin}/project/<id>/my-work?approve=all to approve a plan; never build links from amp_server)\nproject_id: 2\nproject_name: AMP v2\nproject_code: amp-v2\nproject_marker: {}\nworking_directory: {}\nworkers_workspace: ",
        amp.url,
        project_dir.join(".amp.json").display(),
        project_dir.display(),
    )), "no amp_context block");
    // #2364: the manager's `workers_workspace:` line (this folder is untracked in the checkout
    // the tests run in, so its workers get folders), then the clock line.
    assert!(
        prompt.contains("amp-work/<ticket>/ and nothing is committed"),
        "no workers_workspace line"
    );
    assert!(prompt.contains(&format!(
        "\n{}\n</amp_context>",
        amp_core::request_time::CONTEXT_LINE
    )));
    assert!(!prompt.contains("stale.invalid"));
    // 4. The manager preloads no skill (#2367): its prompt says to read amp-planning to plan.
    assert!(!prompt.contains("<preloaded_skills>"));
    assert!(prompt.contains("Read the **amp-planning** skill before you plan"));
    // 5. AGENTS.md.
    assert!(prompt.contains("<project_context>"));
    assert!(prompt.contains("Repo rule: run make check before committing."));
    // 6. rpi's listing of every skill, with paths the model can read.
    assert!(prompt.contains("<available_skills>"));
    assert!(prompt.contains("<name>amp-execution</name>"));
    let order = [
        "# AMP Manager",
        "<amp_server_instructions",
        "<amp_context>",
        "<project_context>",
        "<available_skills>",
    ];
    let at: Vec<usize> = order.iter().map(|s| prompt.find(s).unwrap()).collect();
    assert!(
        at.windows(2).all(|w| w[0] < w[1]),
        "sections out of order: {at:?}"
    );
    let skill_path = prompt
        .split("<location>")
        .nth(1)
        .and_then(|s| s.split("</location>").next())
        .unwrap();
    assert!(
        Path::new(skill_path).is_file(),
        "skill not materialized: {skill_path}"
    );

    // --print-system-prompt prints the same prompt without calling a model.
    let out = harness(&dir, &amp.url, &["run", "--print-system-prompt"]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(out.stdout.trim_end(), prompt.trim_end());

    // #2367: --print-tools prints the tools as one JSON array, built-in schemas compacted.
    let out = harness(&dir, &amp.url, &["run", "--print-tools"]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let tools: Value = serde_json::from_str(out.stdout.trim()).unwrap();
    let names: Vec<&str> = tools
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["name"].as_str().unwrap())
        .collect();
    assert!(
        names.contains(&"amp_run_ticket") && names.contains(&"amp_whoami"),
        "{names:?}"
    );
    // The manager's policy keeps amp_dispatch_task out (amp_run_ticket dispatches).
    assert!(!names.contains(&"amp_dispatch_task"), "{names:?}");
    let read = tools
        .as_array()
        .unwrap()
        .iter()
        .find(|t| t["name"] == "read")
        .unwrap();
    assert!(read["parameters"].get("$schema").is_none(), "{read}");
    assert_eq!(read["parameters"]["required"], json!(["path"]));
}

#[tokio::test]
async fn policy_denials_and_amp_tool_errors_reach_the_model_and_the_run_goes_on() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(
        "run-policy",
        json!([
            {"tool_calls": [
                {"name": "bash", "arguments": {"command": "rm -rf build"}, "id": "c1"},
                {"name": "amp_fail", "id": "c2"},
                {"name": "write", "arguments": {"path": "x.txt", "content": "x"}, "id": "c3"}
            ]},
            {"text": "Recovered."}
        ]),
    );
    let out = harness(&dir, &amp.url, &["run", "-p", "try things", "--json"]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let events = json_lines(&out);
    let ends = of_type(&events, "tool_end");
    let end = |id: &str| ends.iter().find(|e| e["id"] == id).unwrap();
    assert_eq!(end("c1")["is_error"], true);
    assert!(
        end("c1")["text"]
            .as_str()
            .unwrap()
            .contains("Denied by the amp-manager tool policy"),
        "{}",
        end("c1")
    );
    assert_eq!(end("c2")["is_error"], true);
    assert!(end("c2")["text"].as_str().unwrap().contains("it broke"));
    // The manager may write `.amp.json` only (#2363): any other file is denied.
    assert_eq!(end("c3")["is_error"], true);
    assert!(
        end("c3")["text"]
            .as_str()
            .unwrap()
            .contains("Denied by the amp-manager tool policy"),
        "{}",
        end("c3")
    );
    assert!(!dir.join("project/x.txt").exists());
    let result = events.last().unwrap();
    assert_eq!(result["outcome"], "completed");
    assert_eq!(result["tool_errors"], 3);
    assert_eq!(result["text"], "Recovered.");
}

#[tokio::test]
async fn text_output_streams_the_answer_and_logs_tools_on_stderr() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(
        "run-text",
        json!([
            {"tool_calls": [{"name": "amp_get_project", "arguments": {"project_id": 2}}]},
            {"text": "Project: {{last_tool_result}}"}
        ]),
    );
    let out = harness(&dir, &amp.url, &["run", "-p", "which project?"]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert!(out.stdout.starts_with("Project: {"), "{}", out.stdout);
    assert!(out.stdout.contains("AMP v2") && out.stdout.ends_with('\n'));
    assert!(
        out.stderr
            .contains("[tool] amp_get_project {\"project_id\":2}"),
        "{}",
        out.stderr
    );
    assert_eq!(
        amp.mock.calls_to("amp_get_project"),
        vec![json!({"project_id": 2})]
    );
}

#[tokio::test]
async fn a_worker_role_runs_too_and_bad_input_fails_clearly() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("run-worker", json!([{"text": "worker here"}]));
    let out = harness(
        &dir,
        &amp.url,
        &[
            "run",
            "-p",
            "hi",
            "--role",
            "worker-backend",
            "--json",
            "--no-session",
        ],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let events = json_lines(&out);
    assert_eq!(events[0]["role"], "amp-worker-backend");
    assert_eq!(events[0]["skills"], json!(["amp-execution"]));
    let tools = events[0]["tools"].as_array().unwrap();
    assert!(tools.contains(&json!("edit")) && tools.contains(&json!("write")));
    assert!(!tools.contains(&json!("amp_run_ticket")));

    let out = harness(&dir, &amp.url, &["run", "-p", "hi", "--role", "nobody"]).await;
    assert_eq!(out.code, 2);
    assert!(
        out.stderr.contains("unknown role `nobody`"),
        "{}",
        out.stderr
    );

    // A script that runs out fails the run (exit 1) instead of hanging it.
    std::fs::write(dir.join("script.json"), "[]").unwrap();
    let out = harness(
        &dir,
        &amp.url,
        &["run", "-p", "hi", "--json", "--no-session"],
    )
    .await;
    assert_eq!(out.code, 1, "{}", out.stdout);
    let result = json_lines(&out).pop().unwrap();
    assert_eq!(result["type"], "result");
    assert_ne!(result["outcome"], "completed");
}

#[tokio::test]
async fn a_role_without_a_provider_key_says_which_key_to_set() {
    let amp = spawn(MockAmp::new(), None).await;
    // No config.toml: the shipped defaults put the manager on OpenRouter, and the runner removes
    // OPENROUTER_API_KEY from the environment.
    let dir = workdir("run-nokey", Some(2));
    let state = dir.join("home/.local/state");
    let out = run(
        &dir,
        &amp.url,
        &["run", "-p", "hi"],
        &[
            ("AMP_CREDENTIAL_STORE", "file"),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await;
    assert_eq!(out.code, 1, "{}", out.stdout);
    assert!(out.stderr.contains("OPENROUTER_API_KEY"), "{}", out.stderr);
    assert!(amp.mock.calls_to("amp_whoami").is_empty());
}

/// M2-6 (#2098): every session has the native `amp_session_usage` over its own live usage.
#[tokio::test]
async fn the_manager_has_a_native_amp_session_usage_tool() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(
        "run-session-usage",
        json!([
            {"text": "Checking my usage.", "tool_calls": [{"name": "amp_session_usage", "id": "u-1"}]},
            {"text": "Usage: {{last_tool_result}}"}
        ]),
    );
    let out = harness(&dir, &amp.url, &["run", "-p", "usage?", "--json"]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let events = json_lines(&out);
    let tools = &events[0]["tools"];
    assert!(
        tools
            .as_array()
            .unwrap()
            .iter()
            .any(|t| t == "amp_session_usage"),
        "{tools}"
    );
    let ends = of_type(&events, "tool_end");
    assert_eq!(ends.len(), 1);
    assert_eq!(ends[0]["is_error"], false, "{}", ends[0]);
    let usage: Value = serde_json::from_str(ends[0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(usage["model"], "faux/faux-1");
    // It already counts the message that called it.
    assert!(usage["input_tokens"].as_i64().unwrap() > 0, "{usage}");
    assert!(usage["output_tokens"].as_i64().unwrap() > 0, "{usage}");
    assert!(
        usage["cost_usd"].is_null(),
        "no pricing configured: {usage}"
    );
    assert_eq!(usage["turns"], 1);
    assert_eq!(usage["tool_calls"], 1);
    assert!(
        usage.get("note").is_none(),
        "the manager fills its own fields"
    );
    // It is native: AMP never saw the call.
    assert!(amp.mock.calls_to("amp_session_usage").is_empty());
}

/// AMP #2884: `run -p` ends when the manager's reply ends, so it must never launch a worker in
/// the background. With `[workers] dispatch = "launch"` in config.toml and
/// AMP_HARNESS_WORKER_DISPATCH=launch, `amp_run_ticket` still waits: its result is the worker's
/// report (not a launch ack), and the worker closed its ticket before the process exited.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn run_waits_for_its_worker_even_when_config_and_env_say_launch() {
    let amp = spawn(MockAmp::new(), None).await;
    // One shared faux queue, popped in order because the manager waits: manager, worker, worker,
    // manager.
    let dir = setup(
        "run-headless-await",
        json!([
            {"tool_calls": [{"name": "amp_run_ticket", "id": "call-1",
                "arguments": {"task_id": 4242, "agent_id": "amp-worker-backend"}}]},
            {"text": "Done.", "tool_calls": [{"name": "amp_complete_task", "arguments": {"task_id": 4242}}]},
            {"text": "Finished #4242."},
            {"text": "Worker said: {{last_tool_result}}"}
        ]),
    );
    let config = dir.join("home/.config/amp-harness/config.toml");
    let mut toml = std::fs::read_to_string(&config).unwrap();
    toml.push_str("\n[workers]\ndispatch = \"launch\"\n");
    std::fs::write(&config, toml).unwrap();

    let script = dir.join("script.json");
    let state = dir.join("home/.local/state");
    let out = run(
        &dir,
        &amp.url,
        &["run", "-p", "run #4242", "--json"],
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
            ("AMP_HARNESS_WORKER_DISPATCH", "launch"),
        ],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let events = json_lines(&out);

    let end = of_type(&events, "tool_end")
        .into_iter()
        .find(|e| e["id"] == "call-1")
        .unwrap_or_else(|| panic!("no amp_run_ticket result: {}", out.stdout));
    assert_eq!(end["is_error"], false, "{end}");
    let text = end["text"].as_str().unwrap();
    let report: Value = serde_json::from_str(text.trim())
        .unwrap_or_else(|e| panic!("amp_run_ticket result is not a report ({e}): {text}"));
    assert_eq!(report["outcome"], "completed", "{report}");
    assert_eq!(report["task_id"], 4242, "{report}");
    assert!(
        report.get("queued").is_none(),
        "a launch ack, not a report: {report}"
    );
    // The worker closed its ticket while the run was still alive.
    assert_eq!(
        amp.mock.calls_to("amp_complete_task").len(),
        1,
        "{}",
        out.stderr
    );
    let result = events.last().unwrap();
    assert_eq!(result["type"], "result");
    assert_eq!(result["outcome"], "completed", "{result}");
}

/// Writes one dispatch-ledger record the way the supervisor writes it.
fn ledger_record(dir: &Path, task_id: i64, dispatch_id: &str, pid: u32) {
    use amp_core::dispatch_ledger::DispatchRecord;
    std::fs::write(
        dir.join(format!("t{task_id}.json")),
        serde_json::to_string(&DispatchRecord {
            task_id,
            dispatch_id: dispatch_id.into(),
            agent_id: "amp:amp-worker-backend".into(),
            pid,
            at_ms: 1,
        })
        .unwrap(),
    )
    .unwrap();
}

/// AMP #2910: `run -p` sweeps the dispatch ledger at startup, as the chat does. A ticket whose
/// dispatching harness process is gone (pid 0 holds no liveness lock) is handed back with its
/// dispatch_id before the prompt runs, and said on stderr and as an `orphan_released` event; a
/// record whose process still holds its lock is somebody's running worker and is left alone.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn run_releases_a_dead_harness_dispatch_at_startup_and_leaves_a_live_one() {
    use amp_core::dispatch_ledger::LEDGER_DIR;
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("run-orphans", json!([{"text": "Nothing else to do."}]));
    let ledger = dir.join("home/.local/state/amp-harness").join(LEDGER_DIR);
    std::fs::create_dir_all(&ledger).unwrap();
    ledger_record(&ledger, 2150, "d_dead2150", 0);
    // A live process: this test holds a lock under pid 4243's name, the way a running harness
    // holds its own.
    let held = std::fs::File::options()
        .create(true)
        .truncate(false)
        .write(true)
        .open(ledger.join("live-4243.lock"))
        .unwrap();
    held.try_lock().unwrap();
    ledger_record(&ledger, 2151, "d_live2151", 4243);

    let out = harness(&dir, &amp.url, &["run", "-p", "anything?", "--json"]).await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );

    // AMP got one return: the dead run's ticket, with that run's dispatch_id (#2909).
    let returned = amp.mock.calls_to("amp_return_task");
    assert_eq!(returned.len(), 1, "{returned:?}");
    assert_eq!(returned[0]["task_id"], 2150, "{returned:?}");
    assert_eq!(returned[0]["dispatch_id"], "d_dead2150", "{returned:?}");

    // A CI log shows it on stderr.
    assert!(
        out.stderr
            .lines()
            .any(|l| l.starts_with("amp-harness: #2150 was returned to ready")),
        "{}",
        out.stderr
    );
    assert!(!out.stderr.contains("#2151"), "{}", out.stderr);

    // --json: `session` stays first, then one orphan_released event, before the run's events.
    let events = json_lines(&out);
    assert_eq!(events[0]["type"], "session", "{}", out.stdout);
    assert_eq!(events[1]["type"], "orphan_released", "{}", out.stdout);
    let released = of_type(&events, "orphan_released");
    assert_eq!(released.len(), 1, "{}", out.stdout);
    assert_eq!(released[0]["task_id"], 2150);
    assert_eq!(released[0]["dispatch_id"], "d_dead2150");
    assert_eq!(released[0]["ok"], true);
    assert!(
        released[0]["text"]
            .as_str()
            .unwrap()
            .contains("#2150 was returned to ready"),
        "{}",
        released[0]
    );
    assert_eq!(of_type(&events, "result")[0]["outcome"], "completed");

    // The dead run's record is gone; the live one's is still somebody's.
    assert!(!ledger.join("t2150.json").exists());
    assert!(ledger.join("t2151.json").exists());
    drop(held);
}

/// AMP #2910: in text mode the release is one `amp-harness: ` stderr line, and the answer still
/// streams to stdout.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn run_text_mode_says_the_startup_release_on_stderr() {
    use amp_core::dispatch_ledger::LEDGER_DIR;
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("run-orphans-text", json!([{"text": "All quiet."}]));
    let ledger = dir.join("home/.local/state/amp-harness").join(LEDGER_DIR);
    std::fs::create_dir_all(&ledger).unwrap();
    ledger_record(&ledger, 2152, "d_dead2152", 0);

    let out = harness(&dir, &amp.url, &["run", "-p", "anything?"]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert_eq!(
        amp.mock.calls_to("amp_return_task")[0]["dispatch_id"],
        "d_dead2152"
    );
    assert!(
        out.stderr
            .lines()
            .any(|l| l.starts_with("amp-harness: #2152 was returned to ready")),
        "{}",
        out.stderr
    );
    assert!(out.stdout.contains("All quiet."), "{}", out.stdout);
    assert!(!out.stdout.contains("#2152"), "{}", out.stdout);
}
