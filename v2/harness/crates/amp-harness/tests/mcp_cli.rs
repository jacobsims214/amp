//! `amp-harness mcp …` CRUD (#2243, H9) end to end with the built binary: every subcommand against
//! stdio, Streamable HTTP and legacy HTTP+SSE servers, at user and repo scope; OAuth `login` /
//! `logout` against a mock authorization server; secrets refused on the command line and never
//! written to a file or printed; and (opt-in, macOS: `AMP_TEST_KEYCHAIN=1`) secret headers and env
//! vars asked into the real keychain under a test-only service.
//!
//! This file owns `main` (`harness = false`) because the test binary is also the stdio MCP server
//! (`--stdio-server <mode>`), like tests/mcp_hub.rs. HOMEs, configs and state live under cargo's
//! target dir (D13); the credential store is the file one, so CI needs no keychain.
//!
//! Run one case: `cargo test -p amp-harness --test mcp_cli -- <name substring>`.

mod common;

/// amp-mcp's mock legacy HTTP+SSE server (#2238).
#[allow(dead_code, reason = "shared with amp-mcp's tests")]
#[path = "../../amp-mcp/tests/common/legacy_sse.rs"]
mod legacy_sse;

use std::{
    borrow::Cow,
    collections::{HashMap, HashSet},
    path::{Path, PathBuf},
    process::Stdio,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};

use axum::{
    Form, Json, Router,
    extract::{Query, Request, State},
    http::{HeaderValue, StatusCode, header},
    middleware::{self, Next},
    response::{IntoResponse, Redirect, Response},
    routing::{get, post},
};
use common::{Output, workdir};
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
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

const SERVER_FLAG: &str = "--stdio-server";
/// A secret-shaped value planted in tests; it must never show up in a file or an output.
const MARK: &str = "sk-mcpcli-planted-0123456789abcdef";
/// The bearer token the http mock wants (sent through `${env:..}` / bearer_env only).
const BEARER: &str = "mcpcli-bearer-7f3a9c";

// ---------------------------------------------------------------------------------------------
// The MCP server (stdio and Streamable HTTP)

#[derive(Clone)]
struct CliTestServer {
    name: &'static str,
}

fn tool(name: &'static str) -> Tool {
    let Value::Object(schema) = json!({"type": "object", "properties": {}}) else {
        unreachable!()
    };
    Tool::new(name, format!("test tool {name}"), Arc::new(schema))
}

impl ServerHandler for CliTestServer {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
            .with_server_info(Implementation::new(self.name, "1.0.0"))
    }

    fn supported_protocol_versions(&self) -> Cow<'static, [ProtocolVersion]> {
        Cow::Borrowed(ProtocolVersion::KNOWN_VERSIONS)
    }

    async fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        _context: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        Ok(ListToolsResult::with_all_items(vec![
            tool("echo"),
            tool("has_env"),
        ]))
    }

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        _context: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        let args = request.arguments.clone().unwrap_or_default();
        let arg = |k: &str| {
            args.get(k)
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned()
        };
        let text = match request.name.as_ref() {
            "echo" => format!("{}: {}", self.name, arg("text")),
            // Says whether a variable reached the server (and its length), never its value.
            "has_env" => match std::env::var(arg("name")) {
                Ok(v) => format!("set {}", v.len()),
                Err(_) => "unset".to_owned(),
            },
            other => {
                return Err(ErrorData::invalid_params(
                    format!("unknown tool {other}"),
                    None,
                ));
            }
        };
        Ok(CallToolResult::success(vec![ContentBlock::text(text)]).into())
    }
}

fn run_stdio_server() {
    let rt = tokio::runtime::Runtime::new().unwrap();
    rt.block_on(async {
        let running = CliTestServer { name: "cli-stdio" }
            .serve(rmcp::transport::stdio())
            .await
            .expect("serve");
        let _ = running.waiting().await;
    });
}

struct Served {
    url: String,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Served {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn serve(app: Router, path: &str) -> Served {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Served {
        url: format!("http://{addr}{path}"),
        task,
    }
}

/// Every request must carry these headers (exact values), else 401.
fn requiring(app: Router, wanted: Vec<(&'static str, String)>) -> Router {
    let wanted = Arc::new(wanted);
    app.layer(middleware::from_fn(move |req: Request, next: Next| {
        let wanted = wanted.clone();
        async move {
            let ok = wanted.iter().all(|(k, v)| {
                req.headers().get(*k).and_then(|h| h.to_str().ok()) == Some(v.as_str())
            });
            if ok {
                next.run(req).await
            } else {
                (StatusCode::UNAUTHORIZED, "missing or wrong header").into_response()
            }
        }
    }))
}

fn http_app() -> Router {
    let service: StreamableHttpService<CliTestServer, LocalSessionManager> =
        StreamableHttpService::new(
            || Ok(CliTestServer { name: "cli-http" }),
            Default::default(),
            StreamableHttpServerConfig::default().with_json_response(true),
        );
    Router::new().nest_service("/mcp", service)
}

// ---------------------------------------------------------------------------------------------
// A mock OAuth authorization server with its protected resource at /mcp (http or legacy sse)

#[derive(Default)]
struct AsState {
    base: String,
    seq: AtomicUsize,
    clients: Mutex<HashMap<String, Vec<String>>>,
    codes: Mutex<HashMap<String, String>>,
    tokens: Mutex<HashSet<String>>,
}

impl AsState {
    fn next(&self, prefix: &str) -> String {
        format!("{prefix}-{}", self.seq.fetch_add(1, Ordering::SeqCst))
    }
}

async fn spawn_oauth(sse: bool) -> (Served, Arc<AsState>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    let state = Arc::new(AsState {
        base: base.clone(),
        ..Default::default()
    });
    let resource = if sse {
        legacy_sse::router(legacy_sse::LegacyState::new(), "/mcp")
    } else {
        http_app()
    };
    let resource = resource.layer(middleware::from_fn_with_state(state.clone(), bearer_gate));
    let app = Router::new()
        .route("/.well-known/oauth-protected-resource/mcp", get(prm))
        .route("/.well-known/oauth-authorization-server", get(as_metadata))
        .route("/register", post(register))
        .route("/authorize", get(authorize))
        .route("/token", post(token))
        .with_state(state.clone())
        .merge(resource);
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    (
        Served {
            url: format!("{base}/mcp"),
            task,
        },
        state,
    )
}

async fn bearer_gate(State(s): State<Arc<AsState>>, req: Request, next: Next) -> Response {
    let ok = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .is_some_and(|t| s.tokens.lock().unwrap().contains(t));
    if ok {
        return next.run(req).await;
    }
    let mut response = (StatusCode::UNAUTHORIZED, "unauthorized").into_response();
    let challenge = format!(
        r#"Bearer resource_metadata="{}/.well-known/oauth-protected-resource/mcp", scope="read""#,
        s.base
    );
    response.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_str(&challenge).unwrap(),
    );
    response
}

async fn prm(State(s): State<Arc<AsState>>) -> Json<Value> {
    Json(json!({
        "resource": format!("{}/mcp", s.base),
        "authorization_servers": [s.base],
        "scopes_supported": ["read"],
        "bearer_methods_supported": ["header"],
    }))
}

async fn as_metadata(State(s): State<Arc<AsState>>) -> Json<Value> {
    Json(json!({
        "issuer": s.base,
        "authorization_endpoint": format!("{}/authorize", s.base),
        "token_endpoint": format!("{}/token", s.base),
        "registration_endpoint": format!("{}/register", s.base),
        "response_types_supported": ["code"],
        "grant_types_supported": ["authorization_code", "refresh_token"],
        "code_challenge_methods_supported": ["S256"],
        "token_endpoint_auth_methods_supported": ["none"],
        "scopes_supported": ["read"],
    }))
}

async fn register(State(s): State<Arc<AsState>>, Json(body): Json<Value>) -> Response {
    let uris: Vec<String> = body["redirect_uris"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(str::to_owned))
                .collect()
        })
        .unwrap_or_default();
    let id = s.next("client");
    s.clients.lock().unwrap().insert(id.clone(), uris.clone());
    (
        StatusCode::CREATED,
        Json(json!({"client_id": id, "redirect_uris": uris, "token_endpoint_auth_method": "none"})),
    )
        .into_response()
}

async fn authorize(
    State(s): State<Arc<AsState>>,
    Query(q): Query<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| q.get(k).cloned().unwrap_or_default();
    let known = s
        .clients
        .lock()
        .unwrap()
        .get(&get("client_id"))
        .is_some_and(|uris| uris.contains(&get("redirect_uri")));
    if !known || get("response_type") != "code" || get("state").is_empty() {
        return (StatusCode::BAD_REQUEST, "bad authorization request").into_response();
    }
    let code = s.next("code");
    s.codes
        .lock()
        .unwrap()
        .insert(code.clone(), get("client_id"));
    Redirect::to(&format!(
        "{}?code={code}&state={}",
        get("redirect_uri"),
        get("state")
    ))
    .into_response()
}

async fn token(
    State(s): State<Arc<AsState>>,
    Form(form): Form<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| form.get(k).cloned().unwrap_or_default();
    let ok = match get("grant_type").as_str() {
        "authorization_code" => s.codes.lock().unwrap().remove(&get("code")).is_some(),
        "refresh_token" => true,
        _ => false,
    };
    if !ok {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error": "invalid_grant"})),
        )
            .into_response();
    }
    let access = s.next("at");
    s.tokens.lock().unwrap().insert(access.clone());
    Json(json!({
        "access_token": access, "token_type": "Bearer", "expires_in": 3600,
        "refresh_token": s.next("rt"),
    }))
    .into_response()
}

// ---------------------------------------------------------------------------------------------
// The binary

fn me() -> String {
    std::env::current_exe().unwrap().display().to_string()
}

struct T {
    dir: PathBuf,
    /// Extra environment for every run.
    env: Vec<(String, String)>,
    /// Use the real keychain (opt-in test) instead of the file store.
    keychain: Option<String>,
}

impl T {
    fn new(name: &str) -> Self {
        let dir = workdir(&format!("mcp-cli-{name}"), Some(2));
        std::fs::create_dir_all(dir.join("home/.config/amp-harness")).unwrap();
        // An editor that applies $EDIT_FROM -> $EDIT_TO to the file it is given.
        let editor = dir.join("editor.sh");
        std::fs::write(
            &editor,
            "#!/bin/sh\nsed -i.orig \"s|$EDIT_FROM|$EDIT_TO|\" \"$1\" && rm -f \"$1.orig\"\n",
        )
        .unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&editor, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        Self {
            dir,
            env: Vec::new(),
            keychain: None,
        }
    }

    fn user_file(&self) -> PathBuf {
        self.dir.join("home/.config/amp-harness/config.toml")
    }

    fn repo_file(&self) -> PathBuf {
        self.dir.join("project/.amp/harness/config.toml")
    }

    fn read(path: &Path) -> String {
        std::fs::read_to_string(path).unwrap_or_default()
    }

    fn trust_file(&self) -> PathBuf {
        self.dir.join("home/.local/state/amp-harness/trust.toml")
    }

    async fn run(&self, args: &[&str]) -> Output {
        self.run_with(args, &[], None).await
    }

    async fn run_with(&self, args: &[&str], env: &[(&str, &str)], stdin: Option<&str>) -> Output {
        let mut cmd = self.command(args, env);
        cmd.stdin(if stdin.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
        let mut child = cmd.spawn().expect("spawn amp-harness");
        if let Some(input) = stdin {
            let mut pipe = child.stdin.take().unwrap();
            pipe.write_all(input.as_bytes()).await.unwrap();
            drop(pipe);
        }
        let out = tokio::time::timeout(Duration::from_secs(90), child.wait_with_output())
            .await
            .unwrap_or_else(|_| panic!("amp-harness {args:?} finished within 90 s"))
            .unwrap();
        Output {
            code: out.status.code().unwrap_or(-1),
            stdout: String::from_utf8(out.stdout).unwrap(),
            stderr: String::from_utf8(out.stderr).unwrap(),
        }
    }

    fn command(&self, args: &[&str], env: &[(&str, &str)]) -> tokio::process::Command {
        let dir = &self.dir;
        let mut cmd = tokio::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"));
        cmd.args(args)
            .current_dir(dir.join("project"))
            .env("XDG_CONFIG_HOME", dir.join("home/.config"))
            .env("XDG_STATE_HOME", dir.join("home/.local/state"))
            .env("AMP_MCP_URL", "http://127.0.0.1:9/mcp")
            .env("AMP_HARNESS_CEILING_DIR", dir)
            .env("AMP_HARNESS_KEYCHAIN_PROMPTS", "never")
            .env("EDITOR", dir.join("editor.sh"))
            .env_remove("VISUAL")
            .env_remove("AMP_HARNESS_PROMPTS")
            .env_remove("AMP_HARNESS_TRUST_REPO_MCP")
            .env_remove("AMP_HARNESS_CONFIG")
            .env_remove("AMP_HARNESS_CONFIG_DIR")
            .env_remove("AMP_TOKEN")
            .env_remove("RUST_LOG")
            .kill_on_drop(true);
        match &self.keychain {
            // The login keychain lives under the real HOME; config and state stay under target/.
            Some(service) => {
                cmd.env_remove("AMP_CREDENTIAL_STORE")
                    .env("AMP_HARNESS_KEYCHAIN_SERVICE", service);
            }
            None => {
                cmd.env("HOME", dir.join("home"))
                    .env("AMP_CREDENTIAL_STORE", "file")
                    .env_remove("AMP_HARNESS_KEYCHAIN_SERVICE");
            }
        }
        for (k, v) in &self.env {
            cmd.env(k, v);
        }
        for (k, v) in env {
            cmd.env(k, v);
        }
        cmd
    }

    /// `mcp login <name> --no-browser`, driving the "browser" (the authorize URL it prints, then
    /// the redirect back to its loopback listener).
    async fn login(&self, name: &str) -> Output {
        let mut cmd = self.command(&["mcp", "login", name, "--no-browser"], &[]);
        cmd.stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        let mut child = cmd.spawn().unwrap();
        let mut lines = BufReader::new(child.stderr.take().unwrap()).lines();
        let mut stderr = String::new();
        let deadline = Instant::now() + Duration::from_secs(30);
        let url = loop {
            let line = tokio::time::timeout_at(deadline.into(), lines.next_line())
                .await
                .expect("the login URL within 30 s")
                .unwrap()
                .unwrap_or_else(|| panic!("login ended early:\n{stderr}"));
            stderr.push_str(&line);
            stderr.push('\n');
            let t = line.trim();
            if t.starts_with("http") && t.contains("/authorize") {
                break t.to_owned();
            }
        };
        let http = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .unwrap();
        let r = http.get(&url).send().await.unwrap();
        assert_eq!(r.status(), StatusCode::SEE_OTHER, "authorize");
        let back = r.headers()[header::LOCATION].to_str().unwrap().to_owned();
        let _ = http.get(&back).send().await;
        let rest = tokio::spawn(async move {
            let mut more = String::new();
            while let Ok(Some(l)) = lines.next_line().await {
                more.push_str(&l);
                more.push('\n');
            }
            more
        });
        let out = tokio::time::timeout(Duration::from_secs(30), child.wait_with_output())
            .await
            .expect("login finished")
            .unwrap();
        stderr.push_str(&rest.await.unwrap());
        Output {
            code: out.status.code().unwrap_or(-1),
            stdout: String::from_utf8(out.stdout).unwrap(),
            stderr,
        }
    }

    /// No file under the test dir holds `secret`.
    fn assert_no_file_has(&self, secret: &str) {
        fn walk(dir: &Path, secret: &str) {
            for e in std::fs::read_dir(dir).unwrap().flatten() {
                let p = e.path();
                if p.is_dir() {
                    walk(&p, secret);
                } else if let Ok(bytes) = std::fs::read(&p) {
                    assert!(
                        !String::from_utf8_lossy(&bytes).contains(secret),
                        "{} holds a secret",
                        p.display()
                    );
                }
            }
        }
        walk(&self.dir, secret);
    }
}

fn ok(out: &Output) -> &Output {
    assert_eq!(
        out.code, 0,
        "stdout:\n{}\nstderr:\n{}",
        out.stdout, out.stderr
    );
    out
}

fn no_secret(out: &Output, secret: &str) {
    assert!(
        !out.stdout.contains(secret) && !out.stderr.contains(secret),
        "a secret was printed:\n{}\n{}",
        out.stdout,
        out.stderr
    );
}

/// The `mcp list --json` row for `name`.
fn row<'a>(list: &'a Value, name: &str) -> &'a Value {
    list.as_array()
        .unwrap()
        .iter()
        .find(|r| r["name"] == name)
        .unwrap_or_else(|| panic!("no {name} in {list}"))
}

fn tools_of(v: &Value) -> Vec<String> {
    v.as_array()
        .unwrap()
        .iter()
        .map(|t| t["name"].as_str().unwrap().to_owned())
        .collect()
}

// ---------------------------------------------------------------------------------------------
// Cases

/// stdio: add (with a test), list with health, get, test, tools/call --server, disable/enable,
/// edit in $EDITOR (and a refused edit), edit --roles, duplicates, remove; then the same server
/// at repo scope (trusted for the person who added it; a hand change makes it untrusted), the
/// user switch for a repo server, a name in both layers, and `config check` at the end.
async fn stdio_at_user_and_repo_scope() {
    let t = T::new("stdio");
    let me = me();
    let out = t
        .run(&[
            "mcp",
            "add",
            "fs",
            "--json",
            "--",
            &me,
            SERVER_FLAG,
            "normal",
        ])
        .await;
    let v = ok(&out).json();
    assert_eq!(v["type"], "stdio");
    assert_eq!(v["scope"], "user");
    assert_eq!(v["test"]["ok"], true, "{v}");
    assert_eq!(v["test"]["tools"], json!(["echo", "has_env"]));
    let text = T::read(&t.user_file());
    assert!(
        text.contains("[mcp.fs]\n# Added by `amp-harness mcp add`.\ntype = \"stdio\""),
        "{text}"
    );

    let list = ok(&t.run(&["mcp", "list", "--json"]).await).json();
    assert_eq!(row(&list, "fs")["status"]["state"], "ready");
    assert_eq!(row(&list, "fs")["tools"], 2);
    let out = ok(&t.run(&["mcp", "list"]).await).stdout.clone();
    assert!(
        out.contains(&format!(
            "fs (stdio, user): {me} --stdio-server normal - ready, 2 tools"
        )),
        "{out}"
    );

    let v = ok(&t.run(&["mcp", "get", "fs", "--json"]).await).json();
    assert_eq!(v["command"], me);
    assert_eq!(v["status"]["ok"], true);
    let out = t.run(&["mcp", "test", "fs"]).await;
    assert!(ok(&out).stdout.contains("ready in"), "{}", out.stdout);
    let v = ok(&t.run(&["mcp", "tools", "--server", "fs", "--json"]).await).json();
    assert_eq!(tools_of(&v), ["echo", "has_env"]);
    let out = t
        .run(&["mcp", "call", "--server", "fs", "echo", r#"{"text":"hi"}"#])
        .await;
    assert_eq!(ok(&out).stdout.trim(), "cli-stdio: hi");

    ok(&t.run(&["mcp", "disable", "fs"]).await);
    let list = ok(&t.run(&["mcp", "list", "--json"]).await).json();
    assert_eq!(row(&list, "fs")["status"]["state"], "disabled");
    assert!(T::read(&t.user_file()).contains("enabled = false"));
    ok(&t.run(&["mcp", "enable", "fs"]).await);
    let list = ok(&t.run(&["mcp", "list", "--json"]).await).json();
    assert_eq!(row(&list, "fs")["status"]["state"], "ready");

    // $EDITOR: a valid change is saved; an invalid one (no terminal to ask again) is not.
    let out = t
        .run_with(
            &["mcp", "edit", "fs"],
            &[("EDIT_FROM", "\"normal\""), ("EDIT_TO", "\"edited\"")],
            None,
        )
        .await;
    assert!(ok(&out).stdout.contains("Saved `fs`"), "{}", out.stdout);
    let v = ok(&t.run(&["mcp", "get", "fs", "--json", "--no-health"]).await).json();
    assert_eq!(v["args"], json!([SERVER_FLAG, "edited"]));
    let before = T::read(&t.user_file());
    let out = t
        .run_with(
            &["mcp", "edit", "fs"],
            &[
                ("EDIT_FROM", "type = \"stdio\""),
                ("EDIT_TO", "type = \"bogus\""),
            ],
            None,
        )
        .await;
    assert_eq!(out.code, 1, "{}", out.stderr);
    assert!(out.stderr.contains("nothing was saved"), "{}", out.stderr);
    assert_eq!(T::read(&t.user_file()), before);
    ok(&t
        .run(&["mcp", "edit", "fs", "--roles", "worker-*,reviewer"])
        .await);
    let v = ok(&t.run(&["mcp", "get", "fs", "--json", "--no-health"]).await).json();
    assert_eq!(v["roles"], json!(["worker-*", "reviewer"]));
    assert!(T::read(&t.user_file()).contains("# Added by `amp-harness mcp add`."));

    let out = t.run(&["mcp", "add", "fs", "--", &me]).await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(out.stderr.contains("already exists"), "{}", out.stderr);

    // Repo scope: written to the repo file and trusted for this person as configured now.
    let out = t
        .run(&[
            "mcp",
            "add",
            "rfs",
            "--scope",
            "repo",
            "--json",
            "--",
            &me,
            SERVER_FLAG,
            "normal",
        ])
        .await;
    let v = ok(&out).json();
    assert_eq!(v["scope"], "repo");
    assert_eq!(v["trusted"], true);
    assert_eq!(v["test"]["ok"], true, "{v}");
    assert!(T::read(&t.repo_file()).contains("[mcp.rfs]"));
    assert!(T::read(&t.trust_file()).contains("rfs"));
    let v = ok(&t.run(&["mcp", "get", "rfs", "--json", "--no-health"]).await).json();
    assert_eq!(v["layer"], "repo");
    assert_eq!(v["trust"], "trusted");
    // A change someone else makes to the repo file is not trusted.
    std::fs::write(
        t.repo_file(),
        T::read(&t.repo_file()).replace("\"normal\"", "\"changed\""),
    )
    .unwrap();
    let out = t.run(&["mcp", "test", "rfs", "--json"]).await;
    assert_eq!(out.code, 1, "{}", out.stdout);
    assert_eq!(out.json()["status"]["state"], "untrusted");
    let list = ok(&t.run(&["mcp", "list", "--json"]).await).json();
    assert_eq!(row(&list, "rfs")["status"]["state"], "untrusted");
    assert_eq!(row(&list, "rfs")["trust"], "changed");

    // Disabling a repo server writes a switch in the user file; enabling drops it again.
    let repo_before = T::read(&t.repo_file());
    ok(&t.run(&["mcp", "disable", "rfs"]).await);
    assert!(T::read(&t.user_file()).contains("[mcp.rfs]\nenabled = false"));
    assert_eq!(T::read(&t.repo_file()), repo_before);
    ok(&t.run(&["mcp", "enable", "rfs"]).await);
    assert!(!T::read(&t.user_file()).contains("[mcp.rfs]"));

    // The same name in both layers: remove has to be told which.
    ok(&t.run(&["mcp", "add", "both", "--no-test", "--", &me]).await);
    ok(&t
        .run(&["mcp", "add", "both", "--no-test", "--repo", "--", &me])
        .await);
    let out = t.run(&["mcp", "remove", "both"]).await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(out.stderr.contains("--scope"), "{}", out.stderr);
    let out = ok(&t.run(&["mcp", "remove", "both", "--scope", "repo"]).await)
        .stdout
        .clone();
    assert!(out.contains("still defined in the user config"), "{out}");

    let out = ok(&t.run(&["mcp", "remove", "rfs", "--json"]).await).json();
    assert_eq!(out["scope"], "repo");
    assert!(!T::read(&t.repo_file()).contains("rfs"));
    assert!(!T::read(&t.trust_file()).contains("rfs"));
    ok(&t.run(&["mcp", "rm", "fs"]).await);
    ok(&t.run(&["mcp", "remove", "both"]).await);
    assert!(!T::read(&t.user_file()).contains("[mcp."));
    assert!(t.user_file().with_extension("toml.bak").exists());
    let out = t.run(&["mcp", "get", "fs"]).await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    ok(&t.run(&["config", "check"]).await);
}

/// Streamable HTTP behind a bearer (bearer_env, or an `Authorization: Bearer ${env:..}` header)
/// and a plain header, at user and repo scope; a missing variable is a clear failure; the token
/// never reaches a file or an output.
async fn http_at_user_and_repo_scope() {
    let t = T::new("http");
    let server = serve(
        requiring(
            http_app(),
            vec![
                ("authorization", format!("Bearer {BEARER}")),
                ("x-org", "s".into()),
            ],
        ),
        "/mcp",
    )
    .await;
    // #2308: the repo server below reads its own variable; a repo server may never read one
    // the person's own servers use.
    let env = [("GH_TOKEN", BEARER), ("RGH_TOKEN", BEARER)];
    let out = t
        .run_with(
            &[
                "mcp",
                "add",
                "gh",
                &server.url,
                "-H",
                "X-Org: s",
                "--bearer-env",
                "GH_TOKEN",
                "--json",
            ],
            &env,
            None,
        )
        .await;
    let v = ok(&out).json();
    assert_eq!(v["type"], "http");
    assert_eq!(v["test"]["ok"], true, "{v}");
    assert_eq!(v["test"]["tools"], json!(["echo", "has_env"]));

    // Without the variable: a configuration failure naming it.
    let out = t.run(&["mcp", "test", "gh", "--json"]).await;
    assert_eq!(out.code, 1);
    let v = out.json();
    assert_eq!(v["status"]["state"], "stopped", "{v}");
    assert!(
        v["status"]["reason"].as_str().unwrap().contains("GH_TOKEN"),
        "{v}"
    );

    let list = ok(&t.run_with(&["mcp", "list", "--json"], &env, None).await).json();
    assert_eq!(row(&list, "gh")["status"]["state"], "ready");
    let v = ok(&t
        .run_with(&["mcp", "get", "gh", "--json"], &env, None)
        .await)
    .json();
    assert_eq!(v["url"], server.url.as_str());
    assert_eq!(v["headers"]["X-Org"], "s");
    assert_eq!(v["secrets"][0]["source"], "env GH_TOKEN");
    assert_eq!(v["secrets"][0]["state"], "set");
    let out = t
        .run_with(
            &["mcp", "call", "--server", "gh", "echo", r#"{"text":"yo"}"#],
            &env,
            None,
        )
        .await;
    assert_eq!(ok(&out).stdout.trim(), "cli-http: yo");
    let v = ok(&t
        .run_with(&["mcp", "tools", "--server", "gh", "--json"], &env, None)
        .await)
    .json();
    assert_eq!(tools_of(&v), ["echo", "has_env"]);

    // Repo scope with Claude Code's `${VAR}` in a header: stored as `${env:VAR}`.
    let out = t
        .run_with(
            &[
                "mcp",
                "add",
                "rgh",
                "--url",
                &server.url,
                "--scope",
                "project",
                "-H",
                "Authorization: Bearer ${RGH_TOKEN}",
                "-H",
                "X-Org=s",
                "--json",
            ],
            &env,
            None,
        )
        .await;
    let v = ok(&out).json();
    assert_eq!(v["trusted"], true);
    assert_eq!(v["test"]["ok"], true, "{v}");
    assert!(
        T::read(&t.repo_file()).contains("Authorization = \"Bearer ${env:RGH_TOKEN}\""),
        "{}",
        T::read(&t.repo_file())
    );
    let out = t.run_with(&["mcp", "test", "rgh"], &env, None).await;
    assert!(ok(&out).stdout.contains("ready"), "{}", out.stdout);
    ok(&t.run(&["mcp", "remove", "rgh"]).await);
    ok(&t.run(&["mcp", "remove", "gh"]).await);
    t.assert_no_file_has(BEARER);
}

/// Legacy HTTP+SSE: explicit `--transport sse`, an `http` server that falls back to SSE, one that
/// may not fall back, and the repo scope.
async fn sse_at_user_and_repo_scope() {
    let t = T::new("sse");
    let state = legacy_sse::LegacyState::new();
    let server = serve(
        requiring(
            legacy_sse::router(state.clone(), "/sse"),
            vec![("x-org", "s".into())],
        ),
        "/sse",
    )
    .await;
    let v = ok(&t
        .run(&[
            "mcp",
            "add",
            "old",
            "--transport",
            "sse",
            &server.url,
            "-H",
            "X-Org: s",
            "--json",
        ])
        .await)
    .json();
    assert_eq!(v["type"], "sse");
    assert_eq!(v["test"]["ok"], true, "{v}");
    assert_eq!(v["test"]["protocol"], "2024-11-05", "{v}");
    assert_eq!(v["test"]["tools"], json!(["echo"]));
    let v = ok(&t
        .run(&["mcp", "add", "fb", &server.url, "-H", "X-Org: s", "--json"])
        .await)
    .json();
    assert_eq!(v["type"], "http");
    assert_eq!(v["test"]["ok"], true, "fell back to SSE: {v}");
    ok(&t
        .run(&[
            "mcp",
            "add",
            "nofb",
            &server.url,
            "-H",
            "X-Org: s",
            "--no-sse-fallback",
            "--no-test",
        ])
        .await);
    let out = t.run(&["mcp", "test", "nofb"]).await;
    assert_eq!(out.code, 1, "{}", out.stdout);

    let v = ok(&t
        .run(&[
            "mcp",
            "add",
            "rold",
            "-t",
            "sse",
            "--url",
            &server.url,
            "-H",
            "X-Org: s",
            "--repo",
            "--json",
        ])
        .await)
    .json();
    assert_eq!(v["test"]["ok"], true, "{v}");
    let out = t
        .run(&[
            "mcp",
            "call",
            "--server",
            "rold",
            "echo",
            r#"{"text":"sse"}"#,
        ])
        .await;
    assert!(ok(&out).stdout.contains("sse"), "{}", out.stdout);
    let v = ok(&t.run(&["mcp", "tools", "--server", "rold", "--json"]).await).json();
    assert_eq!(tools_of(&v), ["echo"]);
    let list = ok(&t.run(&["mcp", "list", "--json"]).await).json();
    for name in ["old", "fb", "rold"] {
        assert_eq!(
            row(&list, name)["status"]["state"],
            "ready",
            "{name}: {list}"
        );
    }
    assert_ne!(row(&list, "nofb")["status"]["state"], "ready");
    for name in ["old", "fb", "nofb", "rold"] {
        ok(&t.run(&["mcp", "remove", name]).await);
    }
    ok(&t.run(&["config", "check"]).await);
}

/// OAuth for an http server (user scope) and an sse server (repo scope): needs-login before,
/// `mcp login` in the "browser", ready after, `get` shows the login, `logout` forgets it, and
/// `remove` takes a remaining login with it.
async fn oauth_login_and_logout_for_http_and_sse() {
    let t = T::new("oauth");
    for (name, sse, scope) in [("ohttp", false, "user"), ("osse", true, "repo")] {
        let (server, _state) = spawn_oauth(sse).await;
        let mut args = vec![
            "mcp",
            "add",
            name,
            "--url",
            &server.url,
            "--scope",
            scope,
            "--no-test",
        ];
        if sse {
            args.extend(["--transport", "sse"]);
        }
        ok(&t.run(&args).await);
        let out = t.run(&["mcp", "test", name, "--json"]).await;
        assert_eq!(out.code, 1, "{name}: {}", out.stdout);
        assert_eq!(out.json()["status"]["state"], "needs-login", "{name}");
        let v = ok(&t.run(&["mcp", "get", name, "--json", "--no-health"]).await).json();
        assert_eq!(v["login"], false, "{name}");

        let out = t.login(name).await;
        assert!(
            ok(&out).stdout.contains(&format!("Logged in to `{name}`")),
            "{}",
            out.stdout
        );
        let out = t.run(&["mcp", "test", name, "--json"]).await;
        let v = ok(&out).json();
        assert_eq!(v["ok"], true, "{name}: {v}");
        let v = ok(&t.run(&["mcp", "get", name, "--json", "--no-health"]).await).json();
        assert_eq!(v["login"], true, "{name}");

        let out = t.run(&["mcp", "logout", name]).await;
        assert!(ok(&out).stdout.contains("Logged out"), "{}", out.stdout);
        let out = t.run(&["mcp", "test", name, "--json"]).await;
        assert_eq!(out.json()["status"]["state"], "needs-login", "{name}");

        ok(&t.login(name).await);
        let v = ok(&t.run(&["mcp", "remove", name, "--json"]).await).json();
        assert_eq!(v["logged_out"], true, "{name}: {v}");
        let creds = T::read(&t.dir.join("home/.config/amp-harness/credentials.json"));
        assert!(!creds.contains(&server.url.replace("/mcp", "")), "{creds}");
    }
    // stdio servers have no login.
    ok(&t
        .run(&["mcp", "add", "local", "--no-test", "--", &me()])
        .await);
    let out = t.run(&["mcp", "login", "local"]).await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(out.stderr.contains("stdio server"), "{}", out.stderr);
}

/// Secret values are refused on the command line (and never echoed), a secret header/env needs
/// the keychain (CI has none: refused before anything is written), `add-json` moves literal
/// secrets aside, Claude Code's `${VAR}` becomes `${env:VAR}`, bad names and misplaced flags are
/// usage errors, and nothing secret is in any file.
async fn secrets_stay_out_of_files_argv_and_output() {
    let t = T::new("secrets");
    let me = me();
    let url = "https://mcp.example.com/mcp";
    let cases: Vec<Vec<String>> = vec![
        vec![
            "-H".into(),
            format!("Authorization: Bearer {MARK}"),
            url.into(),
        ],
        vec!["-H".into(), format!("X-Api-Key: {MARK}"), url.into()],
        vec![
            "-e".into(),
            format!("API_TOKEN={MARK}"),
            "--".into(),
            me.clone(),
        ],
        vec!["--".into(), me.clone(), MARK.into()],
    ];
    for extra in &cases {
        let mut args = vec!["mcp", "add", "leak", "--no-test"];
        args.extend(extra.iter().map(String::as_str));
        let out = t.run(&args).await;
        assert_eq!(out.code, 2, "{args:?}: {}", out.stderr);
        assert!(out.stderr.contains("nothing was written"), "{}", out.stderr);
        no_secret(&out, MARK);
    }
    // Secret flags need the keychain; with the file store nothing is asked, stored or written.
    let out = t
        .run_with(
            &[
                "mcp",
                "add",
                "k",
                url,
                "--secret-header",
                "X-Api-Key",
                "--secret-stdin",
            ],
            &[],
            Some(&format!("{MARK}\n")),
        )
        .await;
    assert_eq!(out.code, 1, "{}", out.stderr);
    assert!(out.stderr.contains("keychain"), "{}", out.stderr);
    no_secret(&out, MARK);
    let json_cfg = json!({"type": "http", "url": url, "headers": {"X-Api-Key": MARK}}).to_string();
    let out = t
        .run_with(&["mcp", "add-json", "k", "-"], &[], Some(&json_cfg))
        .await;
    assert_eq!(out.code, 1, "{}", out.stderr);
    no_secret(&out, MARK);
    assert!(!t.user_file().exists(), "{}", T::read(&t.user_file()));

    // add-json without secrets; `${VAR}` becomes a reference.
    let json_cfg =
        json!({"command": me, "args": [SERVER_FLAG, "normal", "${HOME}"], "env": {"LOG": "1"}})
            .to_string();
    let v = ok(&t
        .run_with(
            &["mcp", "add-json", "j", "-", "--json"],
            &[],
            Some(&json_cfg),
        )
        .await)
    .json();
    assert_eq!(v["test"]["ok"], true, "{v}");
    assert!(T::read(&t.user_file()).contains("\"${env:HOME}\""));
    let out = t.run(&["mcp", "secret", "j"]).await;
    assert_eq!(out.code, 2, "{}", out.stderr);

    for (args, needle) in [
        (vec!["mcp", "add", "GitHub", url], "github"),
        (vec!["mcp", "add", "x", url, "-e", "A=1"], "stdio"),
        (vec!["mcp", "add", "x", "--", "cmd", "-H", "A: b"], ""),
        (
            vec!["mcp", "add", "x", "--bearer-env", "T", "--", "cmd"],
            "http/sse",
        ),
        (vec!["mcp", "add", "x", "-t", "sse"], "URL"),
    ] {
        let out = t.run(&args).await;
        if needle.is_empty() {
            // `-H` after the command is the command's own argument (as in Claude Code).
            ok(&out);
            ok(&t.run(&["mcp", "remove", "x"]).await);
            continue;
        }
        assert_eq!(out.code, 2, "{args:?}: {}", out.stderr);
        assert!(out.stderr.contains(needle), "{args:?}: {}", out.stderr);
    }
    t.assert_no_file_has(MARK);
}

/// Opt-in (macOS, `AMP_TEST_KEYCHAIN=1`): secret headers and env vars are read from stdin into
/// the real keychain under a test-only service, reach the server, show as "stored" (never the
/// value), can be replaced with `mcp secret`, and `remove` deletes them.
async fn keychain_secrets_for_http_and_stdio() {
    if !(cfg!(target_os = "macos") && std::env::var("AMP_TEST_KEYCHAIN").is_ok_and(|v| v == "1")) {
        println!("  skipped: set AMP_TEST_KEYCHAIN=1 to run the real-keychain case (macOS)");
        return;
    }
    let service = format!("amp-harness-test-mcp-cli-{}", std::process::id());
    struct Cleanup(String);
    impl Drop for Cleanup {
        fn drop(&mut self) {
            for account in ["mcp:k:header:x-api-key", "mcp:s:env:BRAVE_API_KEY"] {
                let _ = std::process::Command::new("security")
                    .args(["delete-generic-password", "-s", &self.0, "-a", account])
                    .output();
            }
        }
    }
    let _cleanup = Cleanup(service.clone());
    let mut t = T::new("keychain");
    t.keychain = Some(service.clone());
    let key = format!("{MARK}-kc");
    let server = serve(
        requiring(http_app(), vec![("x-api-key", key.clone())]),
        "/mcp",
    )
    .await;
    let out = t
        .run_with(
            &[
                "mcp",
                "add",
                "k",
                &server.url,
                "--secret-header",
                "X-Api-Key",
                "--secret-stdin",
                "--json",
            ],
            &[],
            Some(&format!("{key}\n")),
        )
        .await;
    let v = ok(&out).json();
    assert_eq!(v["secrets_stored"], json!(["header X-Api-Key"]));
    assert_eq!(v["test"]["ok"], true, "{v}");
    no_secret(&out, &key);
    let v = ok(&t.run(&["mcp", "get", "k", "--json", "--no-health"]).await).json();
    assert_eq!(v["secrets"][0]["state"], "stored", "{v}");
    // A wrong value is rejected by the server; `mcp secret` replaces it.
    ok(&t
        .run_with(
            &["mcp", "secret", "k", "--secret-stdin"],
            &[],
            Some("wrong\n"),
        )
        .await);
    let out = t.run(&["mcp", "test", "k", "--json"]).await;
    assert_eq!(out.code, 1, "{}", out.stdout);
    ok(&t
        .run_with(
            &["mcp", "secret", "k", "X-Api-Key", "--secret-stdin"],
            &[],
            Some(&format!("{key}\n")),
        )
        .await);
    ok(&t.run(&["mcp", "test", "k"]).await);

    let out = t
        .run_with(
            &[
                "mcp",
                "add",
                "s",
                "--secret-env",
                "BRAVE_API_KEY",
                "--secret-stdin",
                "--",
                &me(),
                SERVER_FLAG,
                "normal",
            ],
            &[],
            Some(&format!("{key}\n")),
        )
        .await;
    ok(&out);
    let out = t
        .run(&[
            "mcp",
            "call",
            "--server",
            "s",
            "has_env",
            r#"{"name":"BRAVE_API_KEY"}"#,
        ])
        .await;
    assert_eq!(ok(&out).stdout.trim(), format!("set {}", key.len()));
    for name in ["k", "s"] {
        let v = ok(&t.run(&["mcp", "remove", name, "--json"]).await).json();
        assert_eq!(
            v["secrets_removed"].as_array().map(Vec::len),
            Some(1),
            "{v}"
        );
    }
    for account in ["mcp:k:header:x-api-key", "mcp:s:env:BRAVE_API_KEY"] {
        let found = std::process::Command::new("security")
            .args(["find-generic-password", "-s", &service, "-a", account])
            .output()
            .unwrap();
        assert!(!found.status.success(), "{account} was not deleted");
    }
    t.assert_no_file_has(&key);
}

// ---------------------------------------------------------------------------------------------

type Case = fn() -> std::pin::Pin<Box<dyn std::future::Future<Output = ()>>>;

macro_rules! cases {
    ($($name:ident),* $(,)?) => {
        vec![$((stringify!($name), (|| Box::pin($name()) as std::pin::Pin<Box<dyn std::future::Future<Output = ()>>>) as Case)),*]
    };
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.iter().any(|a| a == SERVER_FLAG) {
        return run_stdio_server();
    }
    if args.iter().any(|a| a == "--list") {
        return;
    }
    let filter: Vec<&String> = args.iter().filter(|a| !a.starts_with("--")).collect();
    let all: Vec<(&str, Case)> = cases![
        stdio_at_user_and_repo_scope,
        http_at_user_and_repo_scope,
        sse_at_user_and_repo_scope,
        oauth_login_and_logout_for_http_and_sse,
        secrets_stay_out_of_files_argv_and_output,
        keychain_secrets_for_http_and_stdio,
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
                tokio::time::timeout(Duration::from_secs(240), case())
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
