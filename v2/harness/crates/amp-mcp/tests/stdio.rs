//! Stdio MCP servers (#2236): start, list, call, stderr log, env, process-group cleanup, timeouts.
//!
//! This file owns `main` (`harness = false` in Cargo.toml) because the test binary is also the
//! server under test: `--stdio-server <mode>` runs a tiny rmcp server on stdin/stdout, and
//! `--fake-harness <pid file>` plays a harness that connects to one and then gets SIGKILLed. A
//! libtest binary would print "running N tests" on stdout, which is the JSON-RPC channel.
//!
//! Run one case: `cargo test -p amp-mcp --test stdio -- <name substring>`. Every wait is bounded.

use std::{
    borrow::Cow,
    collections::BTreeMap,
    future::Future,
    path::{Path, PathBuf},
    pin::Pin,
    process::Stdio,
    sync::Arc,
    time::{Duration, Instant},
};

use amp_mcp::{
    ExternalClient, ExternalOptions, McpError, StdioSpec,
    external::{SHUTDOWN_GRACE, STDERR_LOG_MAX_BYTES},
    rmcp::model::{CallToolResult, ContentBlock},
};
use rmcp::{
    ErrorData, RoleServer, ServerHandler, ServiceExt,
    model::{
        CallToolRequestParams, CallToolResponse, Implementation, ListToolsResult,
        PaginatedRequestParams, ProtocolVersion, ServerCapabilities, ServerConfig, Tool,
    },
    service::RequestContext,
};

const SERVER_FLAG: &str = "--stdio-server";
const HARNESS_FLAG: &str = "--fake-harness";
const INSTRUCTIONS: &str = "stdio test server: call echo first.";

// ---------------------------------------------------------------------------------------------
// The server side

#[derive(Clone, Copy, PartialEq, Eq)]
enum Mode {
    /// A normal server that exits on stdin EOF.
    Normal,
    /// Answers normally but keeps running after stdin EOF (shutdown must kill it).
    IgnoreEof,
    /// Never answers `initialize` (startup timeout). Logs its pid to stderr.
    Hang,
    /// Writes to stderr and exits before the handshake.
    Exit,
}

#[derive(Clone)]
struct StdioTestServer;

fn tool(name: &'static str) -> Tool {
    let serde_json::Value::Object(schema) = serde_json::json!({"type": "object", "properties": {}})
    else {
        unreachable!()
    };
    Tool::new(name, name, Arc::new(schema))
}

fn text(s: impl Into<String>) -> Result<CallToolResponse, ErrorData> {
    Ok(CallToolResult::success(vec![ContentBlock::text(s.into())]).into())
}

impl ServerHandler for StdioTestServer {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
            .with_server_info(Implementation::new("stdio-test", "1.2.3"))
            .with_instructions(INSTRUCTIONS)
    }

    fn supported_protocol_versions(&self) -> Cow<'static, [ProtocolVersion]> {
        Cow::Borrowed(ProtocolVersion::KNOWN_VERSIONS)
    }

    async fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        _context: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        Ok(ListToolsResult::with_all_items(
            [
                "echo",
                "env_names",
                "env_value",
                "cwd",
                "pid",
                "stderr",
                "stderr_bulk",
                "spawn_grandchild",
                "sleep_ms",
                "exit_now",
            ]
            .into_iter()
            .map(tool)
            .collect(),
        ))
    }

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        _context: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        let args = request.arguments.clone().unwrap_or_default();
        let arg = |k: &str| args.get(k).and_then(|v| v.as_str()).unwrap_or_default();
        let num = |k: &str| args.get(k).and_then(|v| v.as_u64()).unwrap_or_default();
        match request.name.as_ref() {
            "echo" => text(arg("text")),
            "env_names" => {
                let mut names: Vec<String> = std::env::vars_os()
                    .map(|(k, _)| k.to_string_lossy().into_owned())
                    .collect();
                names.sort();
                text(names.join("\n"))
            }
            "env_value" => text(std::env::var(arg("name")).unwrap_or_else(|_| "<unset>".into())),
            "cwd" => text(std::env::current_dir().unwrap().display().to_string()),
            "pid" => text(std::process::id().to_string()),
            "stderr" => {
                eprintln!("{}", arg("text"));
                text("ok")
            }
            "stderr_bulk" => {
                use std::io::Write as _;
                let line = "x".repeat(99) + "\n";
                let mut err = std::io::stderr().lock();
                for _ in 0..num("bytes").div_ceil(100) {
                    let _ = err.write_all(line.as_bytes());
                }
                text("ok")
            }
            "spawn_grandchild" => {
                let child = std::process::Command::new("sleep")
                    .arg("300")
                    .stdin(Stdio::null())
                    .stdout(Stdio::null())
                    .stderr(Stdio::null())
                    .spawn()
                    .map_err(|e| ErrorData::internal_error(e.to_string(), None))?;
                text(child.id().to_string())
            }
            "sleep_ms" => {
                tokio::time::sleep(Duration::from_millis(num("ms"))).await;
                text("slept")
            }
            "exit_now" => std::process::exit(0),
            other => Err(ErrorData::invalid_params(
                format!("unknown tool {other}"),
                None,
            )),
        }
    }
}

fn run_server(mode: Mode) {
    let rt = tokio::runtime::Runtime::new().unwrap();
    rt.block_on(async {
        match mode {
            Mode::Exit => {
                eprintln!("boom: exiting before the handshake");
                std::process::exit(3);
            }
            Mode::Hang => {
                eprintln!("hang pid {}", std::process::id());
                tokio::time::sleep(Duration::from_secs(300)).await;
            }
            Mode::Normal | Mode::IgnoreEof => {
                let running = StdioTestServer
                    .serve(rmcp::transport::stdio())
                    .await
                    .expect("serve");
                let _ = running.waiting().await;
                if mode == Mode::IgnoreEof {
                    eprintln!("stdin closed; ignoring it");
                    tokio::time::sleep(Duration::from_secs(300)).await;
                }
            }
        }
    });
}

// ---------------------------------------------------------------------------------------------
// Helpers

fn scratch(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../target/test-scratch/amp-mcp-stdio")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

fn me() -> String {
    std::env::current_exe().unwrap().display().to_string()
}

fn spec(dir: &Path, mode: &str) -> StdioSpec {
    StdioSpec {
        name: "probe".to_owned(),
        command: me(),
        args: vec![SERVER_FLAG.to_owned(), mode.to_owned()],
        env: BTreeMap::from([
            ("PATH".to_owned(), std::env::var("PATH").unwrap_or_default()),
            ("PLAIN".to_owned(), "visible".to_owned()),
        ]),
        cwd: dir.to_path_buf(),
        stderr_log: dir.join("mcp-logs/probe.log"),
        options: ExternalOptions {
            connect_timeout: Duration::from_secs(20),
            call_timeout: Duration::from_secs(20),
            ..ExternalOptions::default()
        },
    }
}

async fn call(client: &ExternalClient, tool: &str, args: serde_json::Value) -> String {
    let serde_json::Value::Object(args) = args else {
        panic!("args must be an object")
    };
    let result = client.call_tool(tool, Some(args)).await.expect(tool);
    result
        .content
        .iter()
        .filter_map(|c| c.as_text().map(|t| t.text.clone()))
        .collect::<Vec<_>>()
        .join("")
}

fn ps(pid: u32, field: &str) -> Option<String> {
    let out = std::process::Command::new("ps")
        .args(["-o", &format!("{field}="), "-p", &pid.to_string()])
        .output()
        .ok()?;
    let s = String::from_utf8_lossy(&out.stdout).trim().to_owned();
    (!s.is_empty()).then_some(s)
}

/// Running and not a zombie.
fn alive(pid: u32) -> bool {
    ps(pid, "stat").is_some_and(|stat| !stat.starts_with('Z'))
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

fn kill9(pid: u32) {
    let _ = std::process::Command::new("kill")
        .args(["-9", &pid.to_string()])
        .status();
}

async fn wait_for_file_text(path: &Path, needle: &str, within: Duration) -> String {
    let deadline = Instant::now() + within;
    loop {
        let s = std::fs::read_to_string(path).unwrap_or_default();
        if s.contains(needle) || Instant::now() >= deadline {
            return s;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

// ---------------------------------------------------------------------------------------------
// Cases

async fn starts_lists_and_calls() {
    let dir = scratch("basic");
    let client = ExternalClient::connect_stdio(spec(&dir, "normal"))
        .await
        .unwrap();
    assert_eq!(client.name(), "probe");
    let info = client.info();
    assert_eq!(info.transport, "stdio");
    assert_eq!(info.server_name.as_deref(), Some("stdio-test"));
    assert_eq!(info.server_version.as_deref(), Some("1.2.3"));
    assert_eq!(info.instructions.as_deref(), Some(INSTRUCTIONS));
    assert!(
        info.protocol_version.has_initialize(),
        "stdio uses initialize"
    );

    let tools: Vec<String> = client
        .list_tools()
        .await
        .unwrap()
        .into_iter()
        .map(|t| t.name.to_string())
        .collect();
    assert!(tools.contains(&"echo".to_owned()), "{tools:?}");
    assert_eq!(tools.len(), 10);

    assert_eq!(
        call(&client, "echo", serde_json::json!({"text": "hi"})).await,
        "hi"
    );
    let cwd = call(&client, "cwd", serde_json::json!({})).await;
    assert_eq!(
        Path::new(&cwd).canonicalize().unwrap(),
        dir.canonicalize().unwrap()
    );
    let pid: u32 = call(&client, "pid", serde_json::json!({}))
        .await
        .parse()
        .unwrap();
    assert_eq!(client.pid(), Some(pid));
    // Its own session and group (setsid): pgid == pid.
    assert_eq!(ps(pid, "pgid").as_deref(), Some(pid.to_string().as_str()));
    assert!(!client.is_closed());
    client.shutdown().await;
    assert!(wait_dead(pid, Duration::from_secs(5)).await);
}

async fn child_env_is_exactly_the_spec() {
    // The parent (this test process) has HOME, USER, CARGO_*, maybe AMP_TOKEN and provider keys;
    // none of it may reach the child. Only the spec's variables do.
    let dir = scratch("env");
    let mut s = spec(&dir, "normal");
    s.env
        .insert("SECRET_FROM_KEYCHAIN".into(), "dummy-value-123".into());
    let client = ExternalClient::connect_stdio(s).await.unwrap();
    let names = call(&client, "env_names", serde_json::json!({})).await;
    let names: Vec<&str> = names
        .lines()
        // macOS may add its own per-process variable at exec; it is not ours.
        .filter(|n| !n.starts_with("__CF") && !n.starts_with("__CFBundle"))
        .collect();
    assert_eq!(names, ["PATH", "PLAIN", "SECRET_FROM_KEYCHAIN"]);
    assert!(std::env::var_os("HOME").is_some(), "the parent has HOME");
    assert_eq!(
        call(&client, "env_value", serde_json::json!({"name": "PLAIN"})).await,
        "visible"
    );
    client.shutdown().await;
}

async fn stderr_goes_to_the_log_file() {
    let dir = scratch("stderr");
    let s = spec(&dir, "normal");
    let log = s.stderr_log.clone();
    let client = ExternalClient::connect_stdio(s).await.unwrap();
    assert_eq!(client.stderr_log(), Some(log.as_path()));
    call(
        &client,
        "stderr",
        serde_json::json!({"text": "marker-7731"}),
    )
    .await;
    let text = wait_for_file_text(&log, "marker-7731", Duration::from_secs(5)).await;
    assert!(text.contains("marker-7731"), "{text}");
    assert!(text.contains("--- amp-harness: started `"), "{text}");
    assert!(!text.contains(SERVER_FLAG), "args are never logged: {text}");
    client.shutdown().await;

    // A second start appends to the same file.
    let client = ExternalClient::connect_stdio(spec(&dir, "normal"))
        .await
        .unwrap();
    client.shutdown().await;
    let text = std::fs::read_to_string(&log).unwrap();
    assert_eq!(
        text.matches("--- amp-harness: started").count(),
        2,
        "{text}"
    );
}

async fn stderr_log_rotates() {
    let dir = scratch("rotate");
    let s = spec(&dir, "normal");
    let log = s.stderr_log.clone();
    std::fs::create_dir_all(log.parent().unwrap()).unwrap();
    // Just under the limit: not rotated at start, rotated once the server writes.
    std::fs::write(&log, vec![b'o'; (STDERR_LOG_MAX_BYTES - 10) as usize]).unwrap();
    let client = ExternalClient::connect_stdio(s).await.unwrap();
    call(&client, "stderr_bulk", serde_json::json!({"bytes": 1000})).await;
    call(
        &client,
        "stderr",
        serde_json::json!({"text": "after-rotate"}),
    )
    .await;
    let text = wait_for_file_text(&log, "after-rotate", Duration::from_secs(5)).await;
    assert!(text.contains("after-rotate"));
    let old = dir.join("mcp-logs/probe.log.1");
    let old_len = std::fs::metadata(&old).unwrap().len();
    assert!(old_len >= STDERR_LOG_MAX_BYTES, "{old_len}");
    assert!((text.len() as u64) < STDERR_LOG_MAX_BYTES);
    client.shutdown().await;
}

async fn group_dies(drop_instead_of_shutdown: bool, mode: &str) {
    let dir = scratch(&format!("group-{mode}-{drop_instead_of_shutdown}"));
    let client = ExternalClient::connect_stdio(spec(&dir, mode))
        .await
        .unwrap();
    let pid = client.pid().unwrap();
    let grandchild: u32 = call(&client, "spawn_grandchild", serde_json::json!({}))
        .await
        .parse()
        .unwrap();
    assert!(alive(grandchild));
    assert_eq!(
        ps(grandchild, "pgid").as_deref(),
        Some(pid.to_string().as_str()),
        "the grandchild is in the server's group"
    );
    let started = Instant::now();
    if drop_instead_of_shutdown {
        drop(client);
    } else {
        client.shutdown().await;
        assert!(
            started.elapsed() < SHUTDOWN_GRACE * 2 + Duration::from_secs(2),
            "{:?}",
            started.elapsed()
        );
    }
    let server_dead = wait_dead(pid, Duration::from_secs(10)).await;
    let grandchild_dead = wait_dead(grandchild, Duration::from_secs(10)).await;
    if !server_dead {
        kill9(pid);
    }
    if !grandchild_dead {
        kill9(grandchild);
    }
    assert!(server_dead, "server {pid} outlived the client ({mode})");
    assert!(
        grandchild_dead,
        "grandchild {grandchild} outlived the client ({mode})"
    );
}

async fn shutdown_kills_the_process_group() {
    group_dies(false, "normal").await;
}

async fn drop_kills_the_process_group() {
    group_dies(true, "normal").await;
}

async fn shutdown_kills_a_server_that_ignores_eof() {
    group_dies(false, "ignore-eof").await;
}

async fn dies_with_the_harness() {
    // A harness process connects to a server and is then SIGKILLed: no destructor runs, so the
    // server must end on its own when stdin closes.
    let dir = scratch("harness");
    let pid_file = dir.join("server.pid");
    let mut harness = tokio::process::Command::new(me())
        .args([HARNESS_FLAG, &pid_file.display().to_string()])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .kill_on_drop(true)
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(20);
    let server_pid = loop {
        if let Some(pid) = std::fs::read_to_string(&pid_file)
            .ok()
            .and_then(|s| s.trim().parse::<u32>().ok())
        {
            break pid;
        }
        assert!(Instant::now() < deadline, "fake harness never connected");
        tokio::time::sleep(Duration::from_millis(50)).await;
    };
    assert!(alive(server_pid));
    harness.start_kill().unwrap();
    let _ = tokio::time::timeout(Duration::from_secs(5), harness.wait()).await;
    let dead = wait_dead(server_pid, Duration::from_secs(10)).await;
    if !dead {
        kill9(server_pid);
    }
    assert!(dead, "server {server_pid} outlived a SIGKILLed harness");
}

fn run_fake_harness(pid_file: &str) {
    let rt = tokio::runtime::Runtime::new().unwrap();
    rt.block_on(async {
        let dir = Path::new(pid_file).parent().unwrap().to_path_buf();
        let client = ExternalClient::connect_stdio(spec(&dir, "normal"))
            .await
            .unwrap();
        // Tools work, then sit until killed.
        call(&client, "echo", serde_json::json!({"text": "x"})).await;
        std::fs::write(pid_file, client.pid().unwrap().to_string()).unwrap();
        tokio::time::sleep(Duration::from_secs(120)).await;
        drop(client);
    });
}

async fn startup_timeout_kills_the_server() {
    let dir = scratch("timeout");
    let mut s = spec(&dir, "hang");
    s.options.connect_timeout = Duration::from_millis(800);
    let log = s.stderr_log.clone();
    let started = Instant::now();
    let err = ExternalClient::connect_stdio(s).await.unwrap_err();
    assert!(started.elapsed() < Duration::from_secs(5));
    assert!(matches!(err, McpError::ConnectTimeout { .. }), "{err}");
    assert!(err.to_string().contains("stdio server `probe`"), "{err}");
    assert!(err.to_string().contains("probe.log"), "{err}");
    let text = wait_for_file_text(&log, "hang pid ", Duration::from_secs(5)).await;
    let pid: u32 = text
        .split("hang pid ")
        .nth(1)
        .and_then(|s| s.split_whitespace().next())
        .and_then(|s| s.parse().ok())
        .unwrap_or_else(|| panic!("no pid in log: {text}"));
    let dead = wait_dead(pid, Duration::from_secs(10)).await;
    if !dead {
        kill9(pid);
    }
    assert!(dead, "a server that timed out at startup was left running");
}

async fn early_exit_points_at_the_log() {
    let dir = scratch("early-exit");
    let s = spec(&dir, "exit");
    let log = s.stderr_log.clone();
    let err = ExternalClient::connect_stdio(s).await.unwrap_err();
    assert!(matches!(err, McpError::Connect { .. }), "{err:?}");
    assert!(
        err.to_string().contains(&log.display().to_string()),
        "{err}"
    );
    let text = wait_for_file_text(&log, "boom", Duration::from_secs(5)).await;
    assert!(
        text.contains("boom: exiting before the handshake"),
        "{text}"
    );
}

async fn spawn_errors_name_the_problem() {
    let dir = scratch("spawn");
    let mut s = spec(&dir, "normal");
    s.command = "amp-no-such-command-2236".into();
    let err = ExternalClient::connect_stdio(s).await.unwrap_err();
    assert!(matches!(err, McpError::Spawn { .. }), "{err:?}");
    assert!(err.to_string().contains("command not found"), "{err}");
    assert!(err.to_string().contains("`probe`"), "{err}");

    let mut s = spec(&dir, "normal");
    s.cwd = dir.join("missing");
    let err = ExternalClient::connect_stdio(s).await.unwrap_err();
    assert!(err.to_string().contains("does not exist"), "{err}");

    // The secret value of an env var never shows in an error.
    let mut s = spec(&dir, "normal");
    s.command = "amp-no-such-command-2236".into();
    s.env.insert("TOKEN".into(), "sekrit-2236".into());
    s.args.push("--key=sekrit-2236".into());
    let err = ExternalClient::connect_stdio(s).await.unwrap_err();
    assert!(!format!("{err} {err:?}").contains("sekrit"), "{err:?}");
}

async fn call_timeout_and_closed_server() {
    let dir = scratch("calls");
    let client = ExternalClient::connect_stdio(spec(&dir, "normal"))
        .await
        .unwrap();
    let err = client
        .call_tool_with_timeout(
            "sleep_ms",
            serde_json::json!({"ms": 5000}).as_object().cloned(),
            Duration::from_millis(200),
        )
        .await
        .unwrap_err();
    assert!(err.is_timeout(), "{err}");
    // Still usable after a timed-out call.
    assert_eq!(
        call(&client, "echo", serde_json::json!({"text": "again"})).await,
        "again"
    );
    // An unknown tool is a server error, not a dead connection.
    let err = client.call_tool("nope", None).await.unwrap_err();
    assert!(err.is_server_error(), "{err}");

    let pid = client.pid().unwrap();
    let _ = client.call_tool("exit_now", None).await;
    let deadline = Instant::now() + Duration::from_secs(5);
    while !client.is_closed() && Instant::now() < deadline {
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert!(client.is_closed(), "the server exited");
    assert!(client.call_tool("echo", None).await.is_err());
    client.shutdown().await;
    assert!(wait_dead(pid, Duration::from_secs(5)).await);
}

// ---------------------------------------------------------------------------------------------
// Runner

type Case = fn() -> Pin<Box<dyn Future<Output = ()>>>;

macro_rules! cases {
    ($($name:ident),* $(,)?) => {
        vec![$((stringify!($name), (|| Box::pin($name()) as Pin<Box<dyn Future<Output = ()>>>) as Case)),*]
    };
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if let Some(i) = args.iter().position(|a| a == SERVER_FLAG) {
        let mode = match args.get(i + 1).map(String::as_str) {
            Some("normal") => Mode::Normal,
            Some("ignore-eof") => Mode::IgnoreEof,
            Some("hang") => Mode::Hang,
            Some("exit") => Mode::Exit,
            other => panic!("unknown server mode {other:?}"),
        };
        return run_server(mode);
    }
    if let Some(i) = args.iter().position(|a| a == HARNESS_FLAG) {
        return run_fake_harness(&args[i + 1]);
    }
    if args.iter().any(|a| a == "--list") {
        return;
    }

    let filter: Vec<&String> = args.iter().filter(|a| !a.starts_with("--")).collect();
    let all: Vec<(&str, Case)> = cases![
        starts_lists_and_calls,
        child_env_is_exactly_the_spec,
        stderr_goes_to_the_log_file,
        stderr_log_rotates,
        shutdown_kills_the_process_group,
        drop_kills_the_process_group,
        shutdown_kills_a_server_that_ignores_eof,
        dies_with_the_harness,
        startup_timeout_kills_the_server,
        early_exit_points_at_the_log,
        spawn_errors_name_the_problem,
        call_timeout_and_closed_server,
    ];
    let rt = tokio::runtime::Runtime::new().unwrap();
    let (mut passed, mut failed) = (0, Vec::new());
    for (name, case) in all {
        if !filter.is_empty() && !filter.iter().any(|f| name.contains(f.as_str())) {
            continue;
        }
        let started = Instant::now();
        // Each case is bounded so a hang fails the run instead of stalling CI.
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
