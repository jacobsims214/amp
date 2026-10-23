//! The MCP hub (#2239, H6) end to end: external stdio and Streamable HTTP servers' tools called by
//! the manager (`run -p`) and by a supervised worker, on a faux model; the hub's lifecycle
//! (restart with backoff, giving up, restart on request, needs-login, lazy start, call timeout,
//! shutdown killing the process group), tools/list_changed reaching a live session, the output
//! cap with its spill file, and the servers' instructions in the system prompt.
//!
//! This file owns `main` (`harness = false` in Cargo.toml) because the test binary is also the
//! stdio MCP server under test: `--stdio-server <mode>` runs a small rmcp server on stdin/stdout
//! (libtest would print "running N tests" on stdout, the JSON-RPC channel). The HTTP server is the
//! same handler under rmcp's Streamable HTTP service in-process.
//!
//! Run one case: `cargo test -p amp-harness --test mcp_hub -- <name substring>`. Every case is
//! bounded (90 s); scratch dirs are under cargo's target dir (never /tmp: D13).

mod common;

use std::{
    borrow::Cow,
    path::{Path, PathBuf},
    process::Command,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use amp_core::{
    agent::{AgentSpec, Runtime, RuntimeOptions},
    mcp_config::McpConfig,
    mcp_hub::{Backoff, HubEvent, HubOptions, McpHub, McpReload, ServerStatus},
    providers::StaticKeys,
};
use amp_mcp::{AmpMcpClient, ClientOptions, ConfigPaths};
use axum::{
    extract::Request,
    http::{StatusCode, header},
    middleware::{self, Next},
    response::IntoResponse,
};
use common::{MockAmp, Output, spawn, workdir};
use rmcp::{
    ErrorData, RoleServer, ServerHandler, ServiceExt,
    model::{
        CallToolRequestParams, CallToolResponse, CallToolResult, ContentBlock, Implementation,
        ListToolsResult, PaginatedRequestParams, ProtocolVersion, ServerCapabilities, ServerConfig,
        Tool,
    },
    service::RequestContext,
    transport::streamable_http_server::{
        StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
    },
};
use serde_json::{Value, json};

const SERVER_FLAG: &str = "--stdio-server";
const INSTRUCTIONS: &str = "hub test server: call echo first.";

// ---------------------------------------------------------------------------------------------
// The server (stdio and HTTP)

/// Set by `add_tool`: `extra` is listed from then on (one server process = one connection).
static EXTRA: AtomicBool = AtomicBool::new(false);

#[derive(Clone)]
struct HubTestServer {
    name: &'static str,
}

fn tool(name: &'static str) -> Tool {
    let Value::Object(schema) = json!({"type": "object", "properties": {}}) else {
        unreachable!()
    };
    Tool::new(name, format!("test tool {name}"), Arc::new(schema))
}

fn text(s: impl Into<String>) -> Result<CallToolResponse, ErrorData> {
    Ok(CallToolResult::success(vec![ContentBlock::text(s.into())]).into())
}

impl ServerHandler for HubTestServer {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
            .with_server_info(Implementation::new(self.name, "1.0.0"))
            .with_instructions(format!("{INSTRUCTIONS} ({})", self.name))
    }

    fn supported_protocol_versions(&self) -> Cow<'static, [ProtocolVersion]> {
        Cow::Borrowed(ProtocolVersion::KNOWN_VERSIONS)
    }

    async fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        _context: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        let mut tools: Vec<Tool> = ["echo", "big", "whereami", "sleep_ms", "add_tool"]
            .into_iter()
            .map(tool)
            .collect();
        if EXTRA.load(Ordering::SeqCst) {
            tools.push(tool("extra"));
        }
        Ok(ListToolsResult::with_all_items(tools))
    }

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        context: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        let args = request.arguments.clone().unwrap_or_default();
        let arg = |k: &str| args.get(k).and_then(Value::as_str).unwrap_or_default();
        let num = |k: &str| args.get(k).and_then(Value::as_u64).unwrap_or_default();
        match request.name.as_ref() {
            "echo" => text(format!("{}: {}", self.name, arg("text"))),
            "big" => text("y".repeat(num("n") as usize)),
            "whereami" => text(format!(
                "cwd={} pid={}",
                std::env::current_dir().unwrap().display(),
                std::process::id()
            )),
            "sleep_ms" => {
                tokio::time::sleep(Duration::from_millis(num("ms"))).await;
                text("slept")
            }
            "add_tool" => {
                EXTRA.store(true, Ordering::SeqCst);
                let _ = context.peer.notify_tool_list_changed().await;
                text("added")
            }
            "extra" => text("extra!"),
            other => Err(ErrorData::invalid_params(
                format!("unknown tool {other}"),
                None,
            )),
        }
    }
}

fn run_server(mode: &str) {
    let rt = tokio::runtime::Runtime::new().unwrap();
    rt.block_on(async {
        match mode {
            "exit" => {
                eprintln!("boom: exiting before the handshake");
                std::process::exit(3);
            }
            "hang" => tokio::time::sleep(Duration::from_secs(300)).await,
            _ => {
                let running = HubTestServer { name: "hub-stdio" }
                    .serve(rmcp::transport::stdio())
                    .await
                    .expect("serve");
                let _ = running.waiting().await;
            }
        }
    });
}

/// The same server over Streamable HTTP; with `bearer`, other requests get a 401.
struct Http {
    url: String,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Http {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn serve_http(bearer: Option<&'static str>) -> Http {
    let service: StreamableHttpService<HubTestServer, LocalSessionManager> =
        StreamableHttpService::new(
            || Ok(HubTestServer { name: "hub-http" }),
            Default::default(),
            StreamableHttpServerConfig::default().with_json_response(true),
        );
    let mut app = axum::Router::new().nest_service("/mcp", service);
    if let Some(token) = bearer {
        app = app.layer(middleware::from_fn(
            move |req: Request, next: Next| async move {
                let ok = req
                    .headers()
                    .get(header::AUTHORIZATION)
                    .and_then(|v| v.to_str().ok())
                    .is_some_and(|v| v == format!("Bearer {token}"));
                if ok {
                    next.run(req).await
                } else {
                    (StatusCode::UNAUTHORIZED, "unauthorized").into_response()
                }
            },
        ));
    }
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Http {
        url: format!("http://{addr}/mcp"),
        task,
    }
}

// ---------------------------------------------------------------------------------------------
// Helpers

fn me() -> String {
    std::env::current_exe().unwrap().display().to_string()
}

/// A `[mcp.<name>]` stdio table running this binary as the server.
fn stdio_table(name: &str, mode: &str, extra: &str) -> String {
    format!(
        "[mcp.{name}]\ntype = \"stdio\"\ncommand = {:?}\nargs = [{SERVER_FLAG:?}, {mode:?}]\n{extra}\n",
        me()
    )
}

const FAUX: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
[roles.worker-backend]
model = "faux/faux-1"
"#;

/// A work dir (project 2) with `config.toml` = the faux models + `mcp`, and a faux script.
fn setup(name: &str, mcp: &str, script: Value) -> PathBuf {
    let dir = workdir(&format!("mcp-hub-{name}"), Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), format!("{FAUX}\n{mcp}")).unwrap();
    std::fs::write(dir.join("script.json"), script.to_string()).unwrap();
    dir
}

async fn harness(dir: &Path, url: &str, args: &[&str], env: &[(&str, &str)]) -> Output {
    let script = dir.join("script.json");
    let state = dir.join("home/.local/state");
    let mut all = vec![
        ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
        ("XDG_STATE_HOME", state.to_str().unwrap()),
    ];
    all.extend_from_slice(env);
    common::run(dir, url, args, &all).await
}

fn state_dir(dir: &Path) -> PathBuf {
    dir.join("home/.local/state/amp-harness")
}

fn events(out: &Output) -> Vec<Value> {
    out.stdout
        .lines()
        .map(|l| serde_json::from_str(l).unwrap_or_else(|e| panic!("not JSON ({e}): {l}")))
        .collect()
}

fn tool_ends(events: &[Value]) -> Vec<&Value> {
    events.iter().filter(|e| e["type"] == "tool_end").collect()
}

fn alive(pid: u32) -> bool {
    let out = Command::new("ps")
        .args(["-o", "stat=", "-p", &pid.to_string()])
        .output()
        .unwrap();
    let stat = String::from_utf8_lossy(&out.stdout).trim().to_owned();
    !stat.is_empty() && !stat.starts_with('Z')
}

async fn wait_dead(pid: u32, within: Duration) -> bool {
    let deadline = Instant::now() + within;
    while Instant::now() < deadline {
        if !alive(pid) {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    !alive(pid)
}

fn pid_in(text: &str) -> u32 {
    text.split("pid=")
        .nth(1)
        .and_then(|s| s.split(|c: char| !c.is_ascii_digit()).next())
        .and_then(|s| s.parse().ok())
        .unwrap_or_else(|| panic!("no pid in {text:?}"))
}

fn scratch(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-mcp-hub")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// A hub over `toml` (a user config file) with fast backoff, not started.
fn hub(name: &str, toml_text: &str, backoff: Backoff) -> (McpHub, PathBuf) {
    let dir = scratch(name);
    let file = dir.join("config.toml");
    std::fs::write(&file, toml_text).unwrap();
    let paths = ConfigPaths {
        user_file: Some(file),
        user_dir: Some(dir.clone()),
        ..ConfigPaths::default()
    };
    let config = McpConfig::load(&paths);
    assert!(config.problems.is_empty(), "{:?}", config.problems);
    let mut opts = HubOptions::new(paths, dir.join("state"), dir.clone());
    opts.keychain = None;
    opts.backoff = backoff;
    opts.poll = Duration::from_millis(50);
    (McpHub::new(config, opts), dir)
}

fn fast() -> Backoff {
    Backoff {
        initial: Duration::from_millis(100),
        max: Duration::from_millis(400),
        give_up_after: 3,
        window: Duration::from_secs(60),
    }
}

fn status(hub: &McpHub, name: &str) -> ServerStatus {
    hub.health()
        .into_iter()
        .find(|h| h.name == name)
        .unwrap_or_else(|| panic!("no server {name}"))
        .status
}

async fn wait_status(hub: &McpHub, name: &str, want: &str, within: Duration) -> ServerStatus {
    let deadline = Instant::now() + within;
    loop {
        let s = status(hub, name);
        if s.word() == want || Instant::now() >= deadline {
            return s;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
}

/// Calls `mcp__<server>__<tool>` through the hub's own adapter, as a session would.
async fn call(hub: &McpHub, name: &str, args: Value) -> Result<String, String> {
    let view = hub.session_view("amp-worker-backend", None, "test");
    let tool = view
        .tools
        .iter()
        .find(|t| t.tool.schema().name == name)
        .unwrap_or_else(|| panic!("{name} not in the session view"));
    tool.tool
        .execute(
            "call-1",
            args,
            tokio_util::sync::CancellationToken::new(),
            Arc::new(|_| {}),
        )
        .await
        .map(|r| {
            r.content
                .iter()
                .filter_map(|c| match c {
                    rpi_agent::TextContentOrImage::Text(t) => Some(t.text.clone()),
                    _ => None,
                })
                .collect::<Vec<_>>()
                .join("")
        })
        .map_err(|e| e.to_string())
}

// ---------------------------------------------------------------------------------------------
// Cases: run -p and workers on a faux model

async fn manager_calls_stdio_and_http_tools_end_to_end() {
    let amp = spawn(MockAmp::new(), None).await;
    let web = serve_http(Some("web-secret")).await;
    let mcp = format!(
        "{}\n[mcp.web]\ntype = \"http\"\nurl = {:?}\nbearer_env = \"WEB_TOKEN\"\n",
        stdio_table("fs", "normal", ""),
        web.url
    );
    let dir = setup(
        "e2e",
        &mcp,
        json!([
            {"tool_calls": [{"name": "mcp__fs__echo", "arguments": {"text": "hi"}, "id": "c1"}]},
            {"tool_calls": [{"name": "mcp__web__echo", "arguments": {"text": "there"}, "id": "c2"}]},
            {"tool_calls": [{"name": "mcp__fs__whereami", "id": "c3"}]},
            {"text": "done: {{last_tool_result}}"}
        ]),
    );
    let out = harness(
        &dir,
        &amp.url,
        &["run", "-p", "use the servers", "--json"],
        &[("WEB_TOKEN", "web-secret")],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let ev = events(&out);
    let session = &ev[0];
    let tools: Vec<&str> = session["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t.as_str().unwrap())
        .collect();
    for name in [
        "amp_whoami",
        "mcp__fs__echo",
        "mcp__fs__big",
        "mcp__web__echo",
    ] {
        assert!(tools.contains(&name), "{name} missing from {tools:?}");
    }
    let servers = session["mcp_servers"].as_array().unwrap();
    assert_eq!(servers.len(), 2, "{servers:?}");
    for s in servers {
        assert_eq!(s["status"]["state"], "ready", "{s}");
        assert_eq!(s["tools"], 5, "{s}");
    }
    assert_eq!(servers[0]["transport"], "stdio");
    assert_eq!(servers[1]["transport"], "http");
    let ends = tool_ends(&ev);
    assert_eq!(ends.len(), 3, "{ends:?}");
    assert!(ends.iter().all(|e| e["is_error"] == false), "{ends:?}");
    assert_eq!(ends[0]["text"], "hub-stdio: hi");
    assert_eq!(ends[1]["text"], "hub-http: there");
    // The stdio server ran in the repo root and is gone once the command exited.
    let whereami = ends[2]["text"].as_str().unwrap();
    let project = std::fs::canonicalize(dir.join("project")).unwrap();
    assert!(
        whereami.starts_with(&format!("cwd={} ", project.display())),
        "{whereami}"
    );
    assert!(wait_dead(pid_in(whereami), Duration::from_secs(5)).await);
    // Its stderr went to the log, never to our stderr.
    assert!(state_dir(&dir).join("mcp-logs/fs.log").exists());
    assert!(!out.stderr.contains("amp-harness: MCP"), "{}", out.stderr);
    // The bearer value never shows anywhere.
    assert!(!out.stdout.contains("web-secret") && !out.stderr.contains("web-secret"));
}

async fn instructions_are_in_the_system_prompt_after_amps() {
    let amp = spawn(MockAmp::new(), None).await;
    let web = serve_http(None).await;
    let mcp = format!(
        "{}\n[mcp.web]\nurl = {:?}\ninstructions = false\n",
        stdio_table("fs", "normal", ""),
        web.url
    );
    let dir = setup("prompt", &mcp, json!([]));
    let out = harness(&dir, &amp.url, &["run", "--print-system-prompt"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let p = &out.stdout;
    let block = format!(
        "<mcp_server_instructions server=\"fs\">\n{INSTRUCTIONS} (hub-stdio)\n</mcp_server_instructions>\n\n<amp_context>"
    );
    assert!(p.contains(&block), "{p}");
    let amp_at = p.find("<amp_server_instructions").unwrap();
    assert!(amp_at < p.find("<mcp_server_instructions").unwrap());
    // `instructions = false` keeps the web server's out.
    assert!(!p.contains("(hub-http)"), "{p}");
    assert_eq!(p.matches("<mcp_server_instructions").count(), 1);
}

async fn output_over_the_cap_is_saved_to_a_file() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(
        "cap",
        &stdio_table("fs", "normal", "max_output_tokens = 10"),
        json!([
            {"tool_calls": [{"name": "mcp__fs__big", "arguments": {"n": 100}, "id": "big-1"}]},
            {"text": "ok"}
        ]),
    );
    let out = harness(&dir, &amp.url, &["run", "-p", "big", "--json"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let ev = events(&out);
    let text = tool_ends(&ev)[0]["text"].as_str().unwrap().to_owned();
    assert!(
        text.starts_with(&format!(
            "{}\n\n[truncated: 25 tokens, over the 10-token limit; full output at ",
            "y".repeat(40)
        )),
        "{text}"
    );
    let path = text
        .split("full output at ")
        .nth(1)
        .unwrap()
        .trim_end_matches(" (read it with read)]");
    assert!(
        path.starts_with(
            state_dir(&dir)
                .join("mcp-output/amp-manager-")
                .to_str()
                .unwrap()
        ),
        "{path}"
    );
    assert!(path.ends_with("/big-1.txt"), "{path}");
    assert_eq!(std::fs::read_to_string(path).unwrap(), "y".repeat(100));
}

async fn a_server_that_does_not_start_is_a_warning_not_a_failure() {
    let amp = spawn(MockAmp::new(), None).await;
    let mcp = format!(
        "{}\n{}",
        stdio_table("slow", "hang", "startup_timeout_secs = 1"),
        stdio_table("fs", "normal", "")
    );
    let dir = setup(
        "slow",
        &mcp,
        json!([
            {"tool_calls": [{"name": "mcp__fs__echo", "arguments": {"text": "still works"}}]},
            {"text": "ok"}
        ]),
    );
    let started = Instant::now();
    let out = harness(&dir, &amp.url, &["run", "-p", "go"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(
        out.stderr
            .contains("amp-harness: MCP server `slow`: failed: connect to stdio server"),
        "{}",
        out.stderr
    );
    assert!(
        out.stderr.contains("[tool] mcp__fs__echo"),
        "{}",
        out.stderr
    );
    assert!(started.elapsed() < Duration::from_secs(20));
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
        .unwrap();
    assert!(out.status.success(), "git {args:?}: {out:?}");
}

async fn a_worker_gets_shared_tools_and_its_own_worktree_instance() {
    let amp = spawn(MockAmp::new(), None).await;
    let mcp = format!(
        "{}\n{}",
        stdio_table("fs", "normal", ""),
        stdio_table("wt", "normal", "cwd = \"${worktree}\"")
    );
    // One faux queue, used in order: the manager dispatches, the worker calls both servers and
    // reports where the worktree server ran, then the manager answers.
    let dir = setup(
        "worker",
        &mcp,
        json!([
            {"tool_calls": [{"name": "amp_run_ticket", "arguments": {"task_id": 77, "agent_id": "amp:amp-worker-backend"}}]},
            {"tool_calls": [{"name": "mcp__fs__echo", "arguments": {"text": "from the worker"}}]},
            {"tool_calls": [{"name": "mcp__wt__whereami"}]},
            {"text": "{{last_tool_result}}"},
            {"text": "manager: {{last_tool_result}}"}
        ]),
    );
    let project = dir.join("project");
    std::fs::write(project.join("README.md"), "x\n").unwrap();
    git(&project, &["init", "-q", "-b", "main"]);
    git(&project, &["add", "."]);
    git(&project, &["commit", "-q", "-m", "init"]);

    let out = harness(&dir, &amp.url, &["run", "-p", "run #77", "--json"], &[]).await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let ev = events(&out);
    let tools = ev[0]["tools"].as_array().unwrap();
    // The manager has the shared server's tools, not the per-worktree one's.
    assert!(tools.iter().any(|t| t == "mcp__fs__echo"), "{tools:?}");
    assert!(
        !tools
            .iter()
            .any(|t| t.as_str().unwrap().starts_with("mcp__wt__")),
        "{tools:?}"
    );
    let report: Value = serde_json::from_str(tool_ends(&ev)[0]["text"].as_str().unwrap()).unwrap();
    let worktree = std::fs::canonicalize(project.join(".worktrees/77")).unwrap();
    let message = report["final_message"].as_str().unwrap();
    assert!(
        message.starts_with(&format!("cwd={} ", worktree.display())),
        "{report}"
    );
    // The worker's instance logged under its own name and ended with the worker.
    assert!(state_dir(&dir).join("mcp-logs/wt-77.log").exists());
    assert!(wait_dead(pid_in(message), Duration::from_secs(5)).await);
    // The worker's call to the shared server went through.
    let session_dir = state_dir(&dir).join("sessions/77");
    let hits = common::provider::files_containing(&session_dir, "hub-stdio: from the worker");
    assert!(
        !hits.is_empty(),
        "no worker transcript mentions the shared server's answer"
    );
}

// ---------------------------------------------------------------------------------------------
// Cases: the hub's lifecycle, in-process

async fn restarts_with_backoff_after_the_server_dies() {
    let (hub, _dir) = hub("restart", &stdio_table("fs", "normal", ""), fast());
    let mut events = hub.subscribe();
    hub.start();
    assert!(hub.wait_started(true).await.is_empty());
    assert_eq!(status(&hub, "fs"), ServerStatus::Ready);
    assert_eq!(hub.summary().to_string(), "mcp 1/1");
    let first = pid_in(&call(&hub, "mcp__fs__whereami", json!({})).await.unwrap());
    assert_eq!(hub.health()[0].pid, Some(first));

    Command::new("kill")
        .args(["-9", &first.to_string()])
        .status()
        .unwrap();
    // Failed (with a retry delay), then ready again on a new process.
    let mut saw_failed = false;
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        let event = tokio::time::timeout_at(deadline.into(), events.recv())
            .await
            .expect("hub events within 20 s")
            .unwrap();
        match event {
            HubEvent::Failed { status, .. } => {
                assert_eq!(status.word(), "failed", "{status}");
                saw_failed = true;
            }
            HubEvent::Ready { .. } if saw_failed => break,
            _ => {}
        }
    }
    let health = &hub.health()[0];
    assert_eq!(health.status, ServerStatus::Ready);
    assert!(health.restarts >= 1);
    let second = pid_in(&call(&hub, "mcp__fs__whereami", json!({})).await.unwrap());
    assert_ne!(first, second);
    hub.shutdown().await;
    assert!(wait_dead(second, Duration::from_secs(5)).await);
}

async fn gives_up_after_repeated_failures_until_restarted() {
    let (hub, dir) = hub("give-up", &stdio_table("bad", "exit", ""), fast());
    hub.start();
    // The first attempt fails; run -p would stop waiting here.
    assert!(hub.wait_started(true).await.is_empty());
    let s = wait_status(&hub, "bad", "stopped", Duration::from_secs(20)).await;
    let ServerStatus::Stopped { reason } = &s else {
        panic!("{s:?}")
    };
    assert!(reason.contains("gave up after 3 failures"), "{reason}");
    assert!(reason.contains("/mcp restart bad"), "{reason}");
    assert!(
        reason.contains(&format!("{}", dir.join("state/mcp-logs/bad.log").display())),
        "the error points at the log: {reason}"
    );
    assert_eq!(hub.summary().to_string(), "mcp 0/1");
    // It stays stopped (no retries)...
    let restarts = hub.health()[0].restarts;
    tokio::time::sleep(Duration::from_millis(600)).await;
    assert_eq!(hub.health()[0].restarts, restarts);
    // ...until restarted, which starts the count again.
    hub.restart("bad").unwrap();
    let again = wait_status(&hub, "bad", "failed", Duration::from_secs(10)).await;
    assert_eq!(again.word(), "failed", "{again:?}");
    assert!(
        hub.restart("nope")
            .unwrap_err()
            .contains("no MCP server named `nope`")
    );
    hub.shutdown().await;
}

async fn needs_login_is_not_retried() {
    let web = serve_http(Some("right")).await;
    let toml_text = format!("[mcp.web]\nurl = {:?}\nauth = \"none\"\n", web.url);
    let (hub, _dir) = hub("login", &toml_text, fast());
    hub.start();
    hub.wait_started(true).await;
    let s = status(&hub, "web");
    assert_eq!(s.word(), "needs-login", "{s:?}");
    tokio::time::sleep(Duration::from_millis(500)).await;
    assert_eq!(hub.health()[0].restarts, 0);
    assert!(hub.session_view("amp-manager", None, "k").tools.is_empty());
    hub.shutdown().await;
}

async fn lazy_servers_wait_for_a_restart_and_calls_time_out() {
    let (hub, _dir) = hub(
        "lazy",
        &stdio_table("fs", "normal", "startup = \"lazy\"\ncall_timeout_secs = 1"),
        fast(),
    );
    hub.start();
    assert!(hub.wait_started(true).await.is_empty());
    assert_eq!(status(&hub, "fs"), ServerStatus::Idle);
    assert_eq!(hub.summary().to_string(), "mcp 0/0");
    hub.restart("fs").unwrap();
    assert_eq!(
        wait_status(&hub, "fs", "ready", Duration::from_secs(20)).await,
        ServerStatus::Ready
    );
    let err = call(&hub, "mcp__fs__sleep_ms", json!({"ms": 3000}))
        .await
        .unwrap_err();
    assert!(err.contains("timed out after 1s"), "{err}");
    // The server is still fine afterwards.
    assert_eq!(
        call(&hub, "mcp__fs__echo", json!({"text": "after"}))
            .await
            .unwrap(),
        "hub-stdio: after"
    );
    hub.shutdown().await;
}

async fn the_role_filter_and_disabled_servers() {
    let toml_text = format!(
        "{}\n{}",
        stdio_table("fs", "normal", "roles = [\"worker-*\"]"),
        stdio_table("off", "normal", "enabled = false")
    );
    let (hub, _dir) = hub("roles", &toml_text, fast());
    hub.start();
    hub.wait_started(true).await;
    assert!(hub.session_view("amp-manager", None, "k").tools.is_empty());
    assert_eq!(
        hub.session_view("amp-worker-backend", None, "k")
            .tools
            .len(),
        5
    );
    assert_eq!(status(&hub, "off"), ServerStatus::Disabled);
    assert_eq!(hub.summary().to_string(), "mcp 1/1");
    hub.shutdown().await;
}

/// A live session (built before the server connected) gets its tools and instructions once it
/// connects, and a tool added later (tools/list_changed), without being rebuilt.
async fn live_sessions_follow_connects_and_tool_list_changes() {
    let dir = scratch("live");
    let amp = spawn(MockAmp::new(), None).await;
    let home = dir.join("home");
    let config = home.join(".config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(
        config.join("config.toml"),
        format!("{FAUX}\n{}", stdio_table("fs", "normal", "")),
    )
    .unwrap();
    let client = Arc::new(
        AmpMcpClient::connect(ClientOptions::new(amp.url.clone()))
            .await
            .unwrap(),
    );
    let home_s = home.display().to_string();
    let env = move |k: &str| match k {
        "HOME" => Some(home_s.clone()),
        "XDG_CONFIG_HOME" => Some(format!("{home_s}/.config")),
        "XDG_STATE_HOME" => Some(format!("{home_s}/.local/state")),
        _ => None,
    };
    let rt = Runtime::load(
        client,
        RuntimeOptions {
            repo_root: None,
            models: None,
            keys: &StaticKeys::default(),
            env: &env,
            state_dir: None,
        },
    )
    .unwrap();
    let built = rt
        .build(AgentSpec::new("worker-backend", dir.clone()))
        .await
        .unwrap();
    let names = |tools: Vec<rpi_harness::types::HarnessTool>| -> Vec<String> {
        tools.iter().map(|t| t.tool.schema().name.clone()).collect()
    };
    assert!(
        !names(built.harness.get_tools().await.unwrap())
            .iter()
            .any(|n| n.starts_with("mcp__"))
    );
    assert!(!built.system_prompt.contains("<mcp_server_instructions"));

    rt.mcp_hub.start();
    rt.mcp_hub.wait_started(true).await;
    let deadline = Instant::now() + Duration::from_secs(10);
    let has = |want: &'static str| {
        let h = Arc::clone(&built.harness);
        async move {
            names(h.get_tools().await.unwrap())
                .iter()
                .any(|n| n == want)
        }
    };
    while !has("mcp__fs__echo").await && Instant::now() < deadline {
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(has("mcp__fs__echo").await);
    let prompt = built.harness.get_system_prompt().await.unwrap().unwrap();
    assert!(
        prompt.contains("<mcp_server_instructions server=\"fs\">"),
        "{prompt}"
    );
    // Built-in and AMP tools are kept.
    let all = names(built.harness.get_tools().await.unwrap());
    assert!(all.contains(&"read".to_owned()) && all.contains(&"amp_whoami".to_owned()));

    assert!(!has("mcp__fs__extra").await);
    assert_eq!(
        call(&rt.mcp_hub, "mcp__fs__add_tool", json!({}))
            .await
            .unwrap(),
        "added"
    );
    let deadline = Instant::now() + Duration::from_secs(10);
    while !has("mcp__fs__extra").await && Instant::now() < deadline {
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(
        has("mcp__fs__extra").await,
        "list_changed did not reach the session"
    );
    assert_eq!(rt.mcp_hub.health()[0].tools, 6);
    rt.mcp_hub.shutdown().await;
}

// ---------------------------------------------------------------------------------------------
// Reload (#2242)

/// The hub's paths for a [`hub`] built in `dir`.
fn hub_paths(dir: &Path) -> ConfigPaths {
    ConfigPaths {
        user_file: Some(dir.join("config.toml")),
        user_dir: Some(dir.to_path_buf()),
        ..ConfigPaths::default()
    }
}

/// Rewrites a [`hub`]'s config.toml and reloads it.
async fn rewrite(hub: &McpHub, dir: &Path, toml_text: &str) -> McpReload {
    std::fs::write(dir.join("config.toml"), toml_text).unwrap();
    hub.reload(McpConfig::load(&hub_paths(dir))).await
}

async fn wait_gone(hub: &McpHub, name: &str, within: Duration) -> bool {
    let deadline = Instant::now() + within;
    while Instant::now() < deadline {
        if !hub.health().iter().any(|h| h.name == name) && !hub.retired().contains(&name.into()) {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    false
}

/// `/reload` after `[mcp.*]` changed: only what changed restarts. An unchanged server keeps its
/// process; a changed one gets a new process and its old one stops; a removed one keeps serving
/// the session that still holds its tools (a running worker's snapshot) and stops once that is
/// dropped; a new one starts.
async fn reload_restarts_only_changed_servers() {
    let tables = |b_extra: &str, with_c: bool, with_d: bool| {
        let mut t = stdio_table("a", "normal", "");
        t.push_str(&stdio_table("b", "normal", b_extra));
        if with_c {
            t.push_str(&stdio_table("c", "normal", ""));
        }
        if with_d {
            t.push_str(&stdio_table("d", "normal", ""));
        }
        t
    };
    let (hub, dir) = hub("reload", &tables("", true, false), fast());
    hub.start();
    assert!(hub.wait_started(true).await.is_empty());
    let pid = |name: &'static str| {
        let hub = &hub;
        async move {
            pid_in(
                &call(hub, &format!("mcp__{name}__whereami"), json!({}))
                    .await
                    .unwrap(),
            )
        }
    };
    let (a1, b1, c1) = (pid("a").await, pid("b").await, pid("c").await);
    // A running worker's tools for c (its snapshot).
    let held: Vec<_> = hub
        .session_view("amp-worker-backend", None, "worker")
        .tools
        .into_iter()
        .filter(|t| t.tool.schema().name.starts_with("mcp__c__"))
        .collect();
    assert_eq!(held.len(), 5);

    // Nothing changed: nothing restarts.
    let same = rewrite(&hub, &dir, &tables("", true, false)).await;
    assert!(same.is_empty(), "{same:?}");
    assert_eq!(pid("a").await, a1);

    let report = rewrite(
        &hub,
        &dir,
        &tables("env = { RELOADED = \"1\" }", false, true),
    )
    .await;
    assert_eq!(report.added, vec!["d"]);
    assert_eq!(report.removed, vec!["c"]);
    assert_eq!(report.changed, vec!["b"]);
    assert!(report.retried.is_empty() && report.problems.is_empty());
    for name in ["b", "d"] {
        assert_eq!(
            wait_status(&hub, name, "ready", Duration::from_secs(20)).await,
            ServerStatus::Ready,
            "{name}"
        );
    }
    assert_eq!(pid("a").await, a1, "an unchanged server keeps its process");
    let b2 = pid("b").await;
    assert_ne!(b2, b1, "a changed server restarts");
    assert!(
        wait_dead(b1, Duration::from_secs(10)).await,
        "the old b stops: nobody holds it"
    );
    pid("d").await;
    // c is gone from the hub but still serves the worker that holds its tools...
    assert!(!hub.health().iter().any(|h| h.name == "c"));
    assert!(
        hub.session_view("amp-worker-backend", None, "new")
            .tools
            .iter()
            .all(|t| !t.tool.schema().name.starts_with("mcp__c__"))
    );
    assert!(alive(c1), "c stops only once no session holds it");
    let echo = held
        .iter()
        .find(|t| t.tool.schema().name == "mcp__c__echo")
        .unwrap();
    let out = echo
        .tool
        .execute(
            "c1",
            json!({"text": "still here"}),
            tokio_util::sync::CancellationToken::new(),
            Arc::new(|_| {}),
        )
        .await
        .unwrap();
    assert!(format!("{:?}", out.content).contains("still here"));
    // ...until the worker is done with it.
    drop(held);
    assert!(
        wait_dead(c1, Duration::from_secs(10)).await,
        "c stops once released"
    );
    assert!(wait_gone(&hub, "c", Duration::from_secs(5)).await);
    hub.shutdown().await;
    for p in [a1, b2] {
        assert!(wait_dead(p, Duration::from_secs(5)).await);
    }
}

/// An HTTP server that answers 401 until `open` is set (a login done elsewhere).
async fn serve_gated(open: Arc<AtomicBool>) -> Http {
    let service: StreamableHttpService<HubTestServer, LocalSessionManager> =
        StreamableHttpService::new(
            || Ok(HubTestServer { name: "hub-http" }),
            Default::default(),
            StreamableHttpServerConfig::default().with_json_response(true),
        );
    let app = axum::Router::new()
        .nest_service("/mcp", service)
        .layer(middleware::from_fn(move |req: Request, next: Next| {
            let open = Arc::clone(&open);
            async move {
                if open.load(Ordering::SeqCst) {
                    next.run(req).await
                } else {
                    (StatusCode::UNAUTHORIZED, "unauthorized").into_response()
                }
            }
        }));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Http {
        url: format!("http://{addr}/mcp"),
        task,
    }
}

/// A server that needed a login (never retried on its own) is tried again by a reload, so a
/// login done in another terminal takes effect after `/reload` without restarting the chat.
async fn reload_retries_a_server_that_needed_a_login() {
    let open = Arc::new(AtomicBool::new(false));
    let web = serve_gated(Arc::clone(&open)).await;
    let toml_text = format!("[mcp.web]\nurl = {:?}\nauth = \"none\"\n", web.url);
    let (hub, dir) = hub("reload-login", &toml_text, fast());
    hub.start();
    hub.wait_started(true).await;
    assert_eq!(status(&hub, "web").word(), "needs-login");
    open.store(true, Ordering::SeqCst);
    let report = rewrite(&hub, &dir, &toml_text).await;
    assert_eq!(report.retried, vec!["web"], "{report:?}");
    assert_eq!(
        wait_status(&hub, "web", "ready", Duration::from_secs(20)).await,
        ServerStatus::Ready
    );
    // A broken table is reported, and its server left out.
    let broken = format!("{toml_text}[mcp.bad]\ntype = \"stdio\"\n");
    let report = rewrite(&hub, &dir, &broken).await;
    assert_eq!(report.added, vec!["bad"]);
    assert!(
        report.problems.iter().any(|p| p.contains("mcp.bad")),
        "{report:?}"
    );
    assert_eq!(status(&hub, "web"), ServerStatus::Ready);
    hub.shutdown().await;
}

/// `Runtime::reload` with a live session: a server added to config.toml reaches the session
/// (tools and instructions) without rebuilding it, even though the session was built when no
/// server was configured; removing it takes its tools away and stops it.
async fn runtime_reload_adds_and_removes_servers_for_a_live_session() {
    let dir = scratch("runtime-reload");
    let amp = spawn(MockAmp::new(), None).await;
    let home = dir.join("home");
    let config = home.join(".config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), FAUX).unwrap();
    let client = Arc::new(
        AmpMcpClient::connect(ClientOptions::new(amp.url.clone()))
            .await
            .unwrap(),
    );
    let home_s = home.display().to_string();
    let env = move |k: &str| match k {
        "HOME" => Some(home_s.clone()),
        "XDG_CONFIG_HOME" => Some(format!("{home_s}/.config")),
        "XDG_STATE_HOME" => Some(format!("{home_s}/.local/state")),
        _ => None,
    };
    let rt = Runtime::load(
        client,
        RuntimeOptions {
            repo_root: None,
            models: None,
            keys: &StaticKeys::default(),
            env: &env,
            state_dir: None,
        },
    )
    .unwrap();
    rt.mcp_hub.start();
    let built = rt
        .build(AgentSpec::new("amp-manager", dir.clone()))
        .await
        .unwrap();
    let names = |h: Arc<rpi_harness::agent_harness::AgentHarness>| async move {
        h.get_tools()
            .await
            .unwrap()
            .iter()
            .map(|t| t.tool.schema().name.clone())
            .collect::<Vec<_>>()
    };
    assert!(
        !names(Arc::clone(&built.harness))
            .await
            .iter()
            .any(|n| n.starts_with("mcp__"))
    );

    std::fs::write(
        config.join("config.toml"),
        format!("{FAUX}\n{}", stdio_table("fs", "normal", "")),
    )
    .unwrap();
    let report = rt.reload(&StaticKeys::default(), &env).await.unwrap();
    assert_eq!(report.mcp.added, vec!["fs"]);
    let deadline = Instant::now() + Duration::from_secs(20);
    while !names(Arc::clone(&built.harness))
        .await
        .contains(&"mcp__fs__echo".to_owned())
        && Instant::now() < deadline
    {
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(
        names(Arc::clone(&built.harness))
            .await
            .contains(&"mcp__fs__echo".to_owned())
    );
    let prompt = built.harness.get_system_prompt().await.unwrap().unwrap();
    assert!(
        prompt.contains("<mcp_server_instructions server=\"fs\">"),
        "{prompt}"
    );
    let pid = rt.mcp_hub.health()[0].pid.expect("a stdio pid");

    std::fs::write(config.join("config.toml"), FAUX).unwrap();
    let report = rt.reload(&StaticKeys::default(), &env).await.unwrap();
    assert_eq!(report.mcp.removed, vec!["fs"]);
    assert!(
        !names(Arc::clone(&built.harness))
            .await
            .iter()
            .any(|n| n.starts_with("mcp__"))
    );
    let prompt = built.harness.get_system_prompt().await.unwrap().unwrap();
    assert!(!prompt.contains("<mcp_server_instructions"), "{prompt}");
    assert!(
        wait_dead(pid, Duration::from_secs(10)).await,
        "the removed server stops"
    );
    rt.mcp_hub.shutdown().await;
}

// ---------------------------------------------------------------------------------------------
// Runner

type Case = fn() -> std::pin::Pin<Box<dyn std::future::Future<Output = ()>>>;

macro_rules! cases {
    ($($name:ident),* $(,)?) => {
        vec![$((stringify!($name), (|| Box::pin($name()) as std::pin::Pin<Box<dyn std::future::Future<Output = ()>>>) as Case)),*]
    };
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if let Some(i) = args.iter().position(|a| a == SERVER_FLAG) {
        return run_server(args.get(i + 1).map_or("normal", String::as_str));
    }
    if args.iter().any(|a| a == "--list") {
        return;
    }
    let filter: Vec<&String> = args.iter().filter(|a| !a.starts_with("--")).collect();
    let all: Vec<(&str, Case)> = cases![
        manager_calls_stdio_and_http_tools_end_to_end,
        instructions_are_in_the_system_prompt_after_amps,
        output_over_the_cap_is_saved_to_a_file,
        a_server_that_does_not_start_is_a_warning_not_a_failure,
        a_worker_gets_shared_tools_and_its_own_worktree_instance,
        restarts_with_backoff_after_the_server_dies,
        gives_up_after_repeated_failures_until_restarted,
        needs_login_is_not_retried,
        lazy_servers_wait_for_a_restart_and_calls_time_out,
        the_role_filter_and_disabled_servers,
        live_sessions_follow_connects_and_tool_list_changes,
        reload_restarts_only_changed_servers,
        reload_retries_a_server_that_needed_a_login,
        runtime_reload_adds_and_removes_servers_for_a_live_session,
    ];
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .unwrap();
    let (mut passed, mut failed) = (0, Vec::new());
    for (name, case) in all {
        if !filter.is_empty() && !filter.iter().any(|f| name.contains(f.as_str())) {
            continue;
        }
        let started = Instant::now();
        let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            rt.block_on(async {
                tokio::time::timeout(Duration::from_secs(90), case())
                    .await
                    .unwrap_or_else(|_| panic!("{name} timed out"))
            })
        }));
        match outcome {
            Ok(()) => {
                passed += 1;
                println!(
                    "test {name} ... ok ({:.1}s)",
                    started.elapsed().as_secs_f64()
                );
            }
            Err(_) => {
                failed.push(name);
                println!("test {name} ... FAILED");
            }
        }
    }
    println!(
        "\ntest result: {}. {passed} passed; {} failed",
        if failed.is_empty() { "ok" } else { "FAILED" },
        failed.len()
    );
    if !failed.is_empty() {
        println!("failures: {}", failed.join(", "));
        std::process::exit(1);
    }
}
