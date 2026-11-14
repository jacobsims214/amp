//! The chat's configuration panels (#2245, H14) end to end, the way a person uses them: the built
//! binary's chat in `--snapshot` mode (the TUI's own headless render), real MCP servers (this test
//! binary as a stdio server, an in-process Streamable HTTP server), faux model providers, temp
//! HOMEs and repos under cargo's target dir (D13), the file credential store (no keychain).
//!
//! - `/mcp`: health per server, live; keys and typed commands: enable/disable (written to the user
//!   config, applied through the reload path), trust/untrust of a repo server, restart, test,
//!   details, `/mcp add <name> <url>`; stdio add/edit name the terminal command.
//! - `/model roles` and `/model <role> …`: a role's model for this chat only (kept by `/reload`),
//!   saved to config.toml with `--save` or `s` in the picker; the chat's own role switches the chat.
//! - `/agents`, `/skills`, `/config` (with sources; a provider key shows its source, never itself).
//! - `/help` lists them; the AMP status line shows `mcp 1/1`.
//! - `/mcp login amp` (#4250): AMP's own browser login against a mock OAuth server (this test
//!   binary plays the browser), then the session reconnects (offline and online chats); a denied
//!   login changes nothing; AMP_TOKEN is refused.
//!
//! Panels with no machine-specific paths are compared with `tests/snapshots/chat-*.txt`;
//! regenerate with `UPDATE_SNAPSHOTS=1 cargo test -p amp-harness --test chat_panels`.
//!
//! This file owns `main` (`harness = false`) because the test binary is also the stdio MCP server
//! (`--stdio-server`), like tests/mcp_hub.rs and tests/mcp_cli.rs.

mod common;

use std::{
    borrow::Cow,
    path::{Path, PathBuf},
    sync::Arc,
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
use common::{MockAmp, Output, spawn};
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
/// The test binary as the chat's browser (`AMP_HARNESS_SNAPSHOT_BROWSER`): approves the login.
const BROWSER_FLAG: &str = "--browser";
/// The same, but the person denies the login.
const DENY_FLAG: &str = "--browser-deny";
const NO_AMP: &str = "http://127.0.0.1:9/mcp";
/// A provider key planted in the environment: `/config` names its source, never the value.
const PLANTED: &str = "sk-or-v1-chatpanels-planted-0123456789abcdef";

// ---------------------------------------------------------------------------------------------
// The MCP server (stdio and Streamable HTTP)

#[derive(Clone)]
struct PanelServer {
    name: &'static str,
}

fn tool(name: &'static str) -> Tool {
    let Value::Object(schema) = json!({"type": "object", "properties": {}}) else {
        unreachable!()
    };
    Tool::new(name, format!("test tool {name}"), Arc::new(schema))
}

impl ServerHandler for PanelServer {
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
            tool("ping"),
        ]))
    }

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        _context: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        Ok(CallToolResult::success(vec![ContentBlock::text(format!(
            "{}: {}",
            self.name, request.name
        ))])
        .into())
    }
}

fn run_stdio_server() {
    let rt = tokio::runtime::Runtime::new().unwrap();
    rt.block_on(async {
        let running = PanelServer {
            name: "panels-stdio",
        }
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

async fn serve_http() -> Served {
    let service: StreamableHttpService<PanelServer, LocalSessionManager> =
        StreamableHttpService::new(
            || {
                Ok(PanelServer {
                    name: "panels-http",
                })
            },
            Default::default(),
            StreamableHttpServerConfig::default().with_json_response(true),
        );
    let app = Router::new().nest_service("/mcp", service);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Served {
        url: format!("http://{addr}/mcp"),
        task,
    }
}

// ---------------------------------------------------------------------------------------------
// A mock AMP behind a mock OAuth authorization server (#4250), like tests/mcp_cli.rs's

#[derive(Default)]
struct AsState {
    /// Set once the server listens ([`spawn_oauth_amp`]).
    base: std::sync::OnceLock<String>,
    seq: std::sync::atomic::AtomicUsize,
    clients: std::sync::Mutex<std::collections::HashMap<String, Vec<String>>>,
    codes: std::sync::Mutex<std::collections::HashMap<String, String>>,
    tokens: std::sync::Mutex<std::collections::HashSet<String>>,
}

impl AsState {
    fn next(&self, prefix: &str) -> String {
        let n = self.seq.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        format!("{prefix}-{n}")
    }

    fn base(&self) -> &str {
        self.base.get().expect("listening")
    }

    fn registered(&self) -> usize {
        self.clients.lock().unwrap().len()
    }
}

/// [`MockAmp`] at `/mcp`, which wants a bearer token the authorization server issued.
async fn spawn_oauth_amp() -> (common::Running, Arc<AsState>) {
    let state = Arc::new(AsState::default());
    let mock = MockAmp::new();
    let resource =
        common::app(&mock, None).layer(middleware::from_fn_with_state(state.clone(), bearer_gate));
    let app = Router::new()
        .route("/.well-known/oauth-protected-resource/mcp", get(prm))
        .route("/.well-known/oauth-authorization-server", get(as_metadata))
        .route("/register", post(register))
        .route("/authorize", get(authorize))
        .route("/token", post(token))
        .with_state(state.clone())
        .merge(resource);
    let running = common::serve(app, mock).await;
    let base = running.url.strip_suffix("/mcp").unwrap().to_owned();
    state.base.set(base).unwrap();
    (running, state)
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
        r#"Bearer resource_metadata="{}/.well-known/oauth-protected-resource/mcp""#,
        s.base()
    );
    response.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_str(&challenge).unwrap(),
    );
    response
}

async fn prm(State(s): State<Arc<AsState>>) -> Json<Value> {
    Json(json!({
        "resource": format!("{}/mcp", s.base()),
        "authorization_servers": [s.base()],
        "bearer_methods_supported": ["header"],
    }))
}

async fn as_metadata(State(s): State<Arc<AsState>>) -> Json<Value> {
    Json(json!({
        "issuer": s.base(),
        "authorization_endpoint": format!("{}/authorize", s.base()),
        "token_endpoint": format!("{}/token", s.base()),
        "registration_endpoint": format!("{}/register", s.base()),
        "response_types_supported": ["code"],
        "grant_types_supported": ["authorization_code", "refresh_token"],
        "code_challenge_methods_supported": ["S256"],
        "token_endpoint_auth_methods_supported": ["none"],
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
    Query(q): Query<std::collections::HashMap<String, String>>,
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
    Form(form): Form<std::collections::HashMap<String, String>>,
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

/// `--browser <url>` / `--browser-deny <url>`: what a person's browser does with a login URL.
/// Approve: follow the authorization server's redirect back to the chat's loopback listener.
/// Deny: send the listener `error=access_denied` with the request's state.
fn run_browser(deny: bool, url: &str) {
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    rt.block_on(async {
        let http = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .timeout(Duration::from_secs(20))
            .build()
            .unwrap();
        let back = if deny {
            let url = reqwest::Url::parse(url).unwrap();
            let q: std::collections::HashMap<String, String> =
                url.query_pairs().into_owned().collect();
            format!(
                "{}?error=access_denied&state={}",
                q["redirect_uri"], q["state"]
            )
        } else {
            let r = http.get(url).send().await.unwrap();
            r.headers()[header::LOCATION].to_str().unwrap().to_owned()
        };
        let _ = http.get(&back).send().await;
    });
}

/// A "browser" program for `AMP_HARNESS_SNAPSHOT_BROWSER`: this test binary with `flag`.
fn browser_script(dir: &Path, flag: &str) -> String {
    let path = dir.join(format!("browser{flag}.sh"));
    std::fs::write(
        &path,
        format!("#!/bin/sh\nexec {} {flag} \"$1\"\n", toml_str(&exe())),
    )
    .unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
    }
    path.display().to_string()
}

// ---------------------------------------------------------------------------------------------
// Setup

fn exe() -> String {
    std::env::current_exe()
        .unwrap()
        .to_string_lossy()
        .into_owned()
}

/// Faux providers; every role inherits `wf/wf-1` except the manager (`boss/boss-1`).
fn models_toml() -> String {
    r#"
[models]
profile = "inherit"
inherit = "wf/wf-1"

[providers.boss]
api = "faux"
[providers.boss.models."boss-1"]

[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]
[providers.wf.models."wf-2"]

[roles.manager]
model = "boss/boss-1"
"#
    .to_owned()
}

fn stdio_table(name: &str, extra: &str) -> String {
    format!(
        "\n[mcp.{name}]\ntype = \"stdio\"\ncommand = {}\nargs = [\"{SERVER_FLAG}\"]\n{extra}",
        toml_str(&exe())
    )
}

fn toml_str(s: &str) -> String {
    format!("\"{}\"", s.replace('\\', "\\\\").replace('"', "\\\""))
}

/// HOME with the config above plus `user_mcp`, and a project (`.amp.json`) whose
/// `.amp/harness/config.toml` holds `repo_mcp`.
fn setup(name: &str, user_mcp: &str, repo_mcp: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-chat-panels")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(dir.join("home/.config/amp-harness")).unwrap();
    std::fs::create_dir_all(dir.join("project/.amp/harness")).unwrap();
    std::fs::write(
        dir.join("home/.config/amp-harness/config.toml"),
        models_toml() + user_mcp,
    )
    .unwrap();
    std::fs::write(
        dir.join("project/.amp.json"),
        json!({"project_id": 2, "project_name": "AMP v2"}).to_string(),
    )
    .unwrap();
    if !repo_mcp.is_empty() {
        std::fs::write(dir.join("project/.amp/harness/config.toml"), repo_mcp).unwrap();
    }
    dir
}

fn user_config(dir: &Path) -> String {
    std::fs::read_to_string(dir.join("home/.config/amp-harness/config.toml")).unwrap()
}

/// The chat at 110x60 with `inputs`, against `amp` (or no AMP at all).
async fn chat(dir: &Path, amp: Option<&str>, inputs: &[&str], env: &[(&str, &str)]) -> Output {
    let mut args = vec!["--snapshot", "110x60", "--no-setup"];
    if amp.is_some() {
        args.push("--meta");
    }
    for i in inputs {
        args.push("--input");
        args.push(i);
    }
    let state = dir.join("home/.local/state");
    let mut envs: Vec<(&str, &str)> = vec![
        ("XDG_STATE_HOME", state.to_str().unwrap()),
        ("AMP_HARNESS_AUTO_RELOAD", "0"),
    ];
    envs.extend_from_slice(env);
    let out = common::run(dir, amp.unwrap_or(NO_AMP), &args, &envs).await;
    assert_eq!(
        out.code, 0,
        "the chat failed\nstdout:\n{}\nstderr:\n{}",
        out.stdout, out.stderr
    );
    out
}

/// The screen's words on one line, without the boxes' side edges (#2882).
fn flat(s: &str) -> String {
    s.split_whitespace()
        .filter(|w| *w != "│")
        .collect::<Vec<_>>()
        .join(" ")
}

/// The open panel as drawn (#2882: its box, title in the top edge to the key hint in the bottom
/// one), without the box's margin. Rows end without the right edge, so a row's last words read
/// as its end.
fn panel(out: &Output) -> Vec<String> {
    let rows: Vec<String> = out.stdout.lines().map(str::to_owned).collect();
    // The box after the Conversation box's bottom edge.
    let Some(start) = rows
        .iter()
        .enumerate()
        .skip_while(|(_, l)| !l.trim_start().starts_with('└'))
        .find(|(_, l)| l.trim_start().starts_with("┌─ "))
        .map(|(i, _)| i)
    else {
        panic!("no panel open:\n{}", out.stdout);
    };
    let margin = rows[start].len() - rows[start].trim_start().len();
    let mut out = Vec::new();
    for l in &rows[start..] {
        let l = l.get(margin..).unwrap_or("").trim_end();
        let l = match l.strip_prefix('│') {
            Some(_) => l.strip_suffix('│').unwrap_or(l).trim_end().to_owned(),
            None => l.to_owned(),
        };
        let last = l.starts_with('└');
        out.push(l);
        if last {
            break;
        }
    }
    out
}

fn snapshot(name: &str, rows: &[String]) {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests/snapshots")
        .join(format!("{name}.txt"));
    let got = rows.join("\n") + "\n";
    if std::env::var_os("UPDATE_SNAPSHOTS").is_some() {
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, &got).unwrap();
        return;
    }
    let want = std::fs::read_to_string(&path).unwrap_or_else(|e| {
        panic!(
            "{}: {e} (run with UPDATE_SNAPSHOTS=1 to create it)",
            path.display()
        )
    });
    assert_eq!(got, want, "{name} differs (UPDATE_SNAPSHOTS=1 to accept)");
}

fn row<'a>(rows: &'a [String], start: &str) -> &'a str {
    rows.iter()
        .map(String::as_str)
        .find(|r| r.trim_start_matches(['│', '›', ' ']).starts_with(start))
        .unwrap_or_else(|| panic!("no row {start:?}:\n{}", rows.join("\n")))
}

// ---------------------------------------------------------------------------------------------
// Cases

/// #2246: a role with no model of its own says, in `/model roles` and `/config`, which fallback it
/// runs on, and the panel names the fallback model.
async fn model_roles_show_the_fallback() {
    let dir = setup("fallback", "", "");
    std::fs::write(
        dir.join("home/.config/amp-harness/config.toml"),
        "[providers.boss]\napi = \"faux\"\n[providers.boss.models.\"boss-1\"]\n\
         [providers.wf]\napi = \"faux\"\n[providers.wf.models.\"wf-1\"]\n\
         [roles.manager]\nmodel = \"boss/boss-1\"\n[roles.worker-backend]\nmodel = \"wf/wf-1\"\n",
    )
    .unwrap();
    let agents = dir.join("home/.config/amp-harness/agents");
    std::fs::create_dir_all(&agents).unwrap();
    std::fs::write(
        agents.join("smoke-helper.md"),
        "---\ndescription: test\nmode: subagent\ndispatch: true\nskills: []\n---\n\nHelp.\n",
    )
    .unwrap();
    let out = chat(&dir, None, &["/model roles"], &[]).await;
    let rows = panel(&out);
    let helper = row(&rows, "smoke-helper");
    assert!(
        helper.contains("wf/wf-1  ") && helper.ends_with("fallback: worker-backend's model"),
        "{rows:#?}"
    );
    assert!(
        !row(&rows, "worker-backend").contains("fallback"),
        "{rows:#?}"
    );
    assert!(
        rows.iter().any(|r| r.contains(
            "fallback: wf/wf-1 (worker-backend's model, default) for roles with no model"
        )),
        "{rows:#?}"
    );
    // /config lists it among the roles, with the fallback as its source.
    let mut seen = String::new();
    for downs in [0, 14, 28] {
        let mut inputs = vec!["/config"];
        inputs.extend(std::iter::repeat_n("key:down", downs));
        let out = chat(&dir, None, &inputs, &[]).await;
        seen.push_str(&panel(&out).join("\n"));
        seen.push('\n');
    }
    let seen = flat(&seen);
    assert!(
        seen.contains("smoke-helper wf/wf-1 · off (fallback worker-backend (config config.toml))"),
        "{seen}"
    );
}

/// `/mcp` lists every server with its health (a ready user server, a disabled one, an untrusted
/// repo one) and only the keys that apply to the selected one; the status line says `mcp 1/1`.
async fn mcp_list_shows_health_and_the_status_line() {
    let dir = setup(
        "list",
        &(stdio_table("fs", "startup = \"eager\"\n") + &stdio_table("off", "enabled = false\n")),
        &stdio_table("shared", ""),
    );
    let amp = spawn(MockAmp::new(), None).await;
    let out = chat(&dir, Some(&amp.url), &["/mcp", "wait:mcp"], &[]).await;
    let rows = panel(&out);
    snapshot("chat-mcp-list", &rows);
    assert!(row(&rows, "●").contains("fs"), "{rows:#?}");
    let fs = row(&rows, "● fs");
    assert!(
        fs.contains("stdio · user") && fs.contains("ready · 2 tools"),
        "{fs}"
    );
    let off = row(&rows, "○ off");
    assert!(off.contains("disabled"), "{off}");
    let shared = row(&rows, "● shared");
    assert!(
        shared.contains("stdio · repo") && shared.contains("not trusted"),
        "{shared}"
    );
    assert!(
        rows.iter()
            .any(|r| r.contains("↑↓ select · Enter details · r restart · t test · d disable")),
        "{rows:#?}"
    );
    // The AMP status line: one server should run, and it is ready.
    assert!(out.stdout.contains("mcp 1/1"), "{}", out.stdout);
}

/// Keys and typed commands change the config through the CLI's code and apply at once through
/// the reload path: `e` enables `off` (it starts), `/mcp disable fs` stops `fs`.
async fn mcp_enable_disable_apply_through_reload() {
    let dir = setup(
        "enable",
        &(stdio_table("fs", "startup = \"eager\"\n") + &stdio_table("off", "enabled = false\n")),
        "",
    );
    let out = chat(
        &dir,
        None,
        &[
            "/mcp",
            "key:down",
            "key:e",
            "wait:mcp",
            "/mcp disable fs",
            "wait:mcp",
            "/mcp",
        ],
        &[],
    )
    .await;
    let screen = flat(&out.stdout);
    assert!(
        screen.contains("MCP server `off` enabled"),
        "{}",
        out.stdout
    );
    assert!(
        screen.contains("MCP server `fs` disabled"),
        "{}",
        out.stdout
    );
    assert!(
        screen.contains("applied (workers already running keep theirs): mcp:"),
        "{}",
        out.stdout
    );
    let rows = panel(&out);
    assert!(row(&rows, "● off").contains("ready · 2 tools"), "{rows:#?}");
    assert!(row(&rows, "○ fs").contains("disabled"), "{rows:#?}");
    let config = user_config(&dir);
    let off = config.split("[mcp.off]").nth(1).unwrap();
    assert!(off.contains("enabled = true"), "{config}");
    let fs = config.split("[mcp.fs]").nth(1).unwrap();
    assert!(fs.contains("enabled = false"), "{config}");
    // The CLI's backup rule holds for the chat's writes too.
    assert!(
        dir.join("home/.config/amp-harness/config.toml.bak")
            .exists()
    );
}

/// A repo server stays off until trusted: `/mcp trust` records it outside the repo and starts it;
/// `U` (untrust) on it stops it again.
async fn mcp_trust_and_untrust_a_repo_server() {
    let dir = setup("trust", "", &stdio_table("shared", ""));
    let out = chat(&dir, None, &["/mcp trust shared", "wait:mcp", "/mcp"], &[]).await;
    assert!(
        flat(&out.stdout).contains("trusted `shared`: runs"),
        "{}",
        out.stdout
    );
    let rows = panel(&out);
    let shared = row(&rows, "● shared");
    assert!(shared.contains("ready · 2 tools"), "{rows:#?}");
    let store = dir.join("home/.local/state/amp-harness/trust.toml");
    let text = std::fs::read_to_string(&store).expect("trust.toml");
    assert!(text.contains("shared"), "{text}");
    assert!(
        !dir.join("project/.amp/harness/trust.toml").exists(),
        "never in the repo"
    );
    // A user-level server is not the repo's to trust.
    let dir2 = setup("trust-user", &stdio_table("mine", ""), "");
    let out = chat(&dir2, None, &["/mcp trust mine"], &[]).await;
    assert!(
        flat(&out.stdout).contains("those are always trusted"),
        "{}",
        out.stdout
    );

    // Untrust with the key on the selected row (the only server).
    let out = chat(&dir, None, &["/mcp", "key:U", "wait:mcp", "/mcp"], &[]).await;
    assert!(
        flat(&out.stdout).contains("untrusted `shared`"),
        "{}",
        out.stdout
    );
    let rows = panel(&out);
    assert!(row(&rows, "● shared").contains("not trusted"), "{rows:#?}");
    let text = std::fs::read_to_string(&store).unwrap();
    assert!(!text.contains("[repos."), "{text}");
}

/// `/mcp add <name> <url>` adds an HTTP server to the user config and connects it; `/mcp <name>`
/// shows it in detail with its tools; `/mcp test` and `/mcp restart` act on the live hub; what the
/// chat leaves to the terminal is named as a command to copy.
async fn mcp_add_details_test_and_restart() {
    let dir = setup("add", &stdio_table("fs", "startup = \"eager\"\n"), "");
    let http = serve_http().await;
    let add = format!("/mcp add web {}", http.url);
    let out = chat(
        &dir,
        None,
        &[
            &add,
            "wait:mcp",
            "/mcp test web",
            "/mcp restart fs",
            "wait:mcp",
            "/mcp test fs",
            "/mcp login fs",
            "/mcp edit fs",
            "/mcp add local -- npx -y some-server",
            "/mcp web",
        ],
        &[],
    )
    .await;
    let screen = flat(&out.stdout);
    assert!(
        screen.contains("added http MCP server `web` to"),
        "{}",
        out.stdout
    );
    assert!(
        screen.contains("MCP server `web`: ready, 2 tools: echo, ping (server panels-http 1.0.0)"),
        "{}",
        out.stdout
    );
    assert!(
        screen.contains("MCP server `fs`: restarting"),
        "{}",
        out.stdout
    );
    assert!(
        screen.contains("MCP server `fs`: ready, 2 tools: echo, ping (server panels-stdio 1.0.0)"),
        "{}",
        out.stdout
    );
    assert!(
        screen.contains("is a stdio server: it has no login"),
        "{}",
        out.stdout
    );
    assert!(
        screen.contains("/mcp edit runs in a terminal (opens $EDITOR): amp-harness mcp edit fs"),
        "{}",
        out.stdout
    );
    assert!(
        screen.contains("amp-harness mcp add local -- npx -y some-server"),
        "{}",
        out.stdout
    );
    let config = user_config(&dir);
    let web = config.split("[mcp.web]").nth(1).expect("[mcp.web] written");
    assert!(web.contains("type = \"http\""), "{config}");
    assert!(web.contains(&http.url), "{config}");

    let rows = panel(&out);
    assert!(rows[0].starts_with("┌─ MCP · web"), "{rows:#?}");
    assert!(row(&rows, "● web").contains("ready · 2 tools"), "{rows:#?}");
    assert!(row(&rows, "type").contains("http"), "{rows:#?}");
    assert!(row(&rows, "url").contains(&http.url), "{rows:#?}");
    assert!(
        row(&rows, "from").contains("user config · ~/.config/amp-harness/config.toml"),
        "{rows:#?}"
    );
    assert!(row(&rows, "auth").contains("auto"), "{rows:#?}");
    assert!(
        row(&rows, "process").contains("server panels-http 1.0.0"),
        "{rows:#?}"
    );
    assert!(
        rows.iter()
            .any(|r| r.trim_start_matches('│').trim() == "echo, ping"),
        "{rows:#?}"
    );
}

/// #4250: `/mcp login amp` logs in to AMP from inside the chat (the browser prompt in the
/// transcript, never stderr) and the session reconnects with the new login:
/// - a chat that started without a usable login connects, and its header heals;
/// - an online chat swaps its connection;
/// - a denied login changes nothing (stored login, connection);
/// - AMP_TOKEN is refused (a browser login would not replace it).
///
/// The mock authorization server and the file credential store live in this test's temp HOME;
/// no real browser, keychain or AMP login is touched.
async fn mcp_login_amp_logs_in_and_reconnects() {
    let dir = setup("amp-login", "", "");
    let (amp, auth) = spawn_oauth_amp().await;
    let approve = browser_script(&dir, BROWSER_FLAG);
    let deny = browser_script(&dir, DENY_FLAG);
    let creds = dir.join(format!(
        "home/.config/amp-harness/{}",
        amp_mcp::CREDENTIALS_FILE_NAME
    ));

    // 1. No login yet: the chat starts offline and the header says re-login needed.
    let out = chat(
        &dir,
        Some(&amp.url),
        &["/mcp login amp", "exec:sleep 1"],
        &[("AMP_HARNESS_SNAPSHOT_BROWSER", &approve)],
    )
    .await;
    let screen = flat(&out.stdout);
    assert!(
        screen.contains("AMP at 127.0.0.1") && screen.contains("Opened your browser to log in"),
        "{}",
        out.stdout
    );
    assert!(screen.contains("logged in to AMP at"), "{}", out.stdout);
    assert!(
        screen.contains("Connected to AMP at") && screen.contains("AMP tools now"),
        "the offline session connected: {}",
        out.stdout
    );
    assert!(!screen.contains("re-login needed"), "{}", out.stdout);
    assert!(
        !out.stderr.contains("authorize"),
        "nothing on stderr: {}",
        out.stderr
    );
    assert!(
        !amp.mock.calls_to("amp_whoami").is_empty(),
        "the poller reconnected and polled"
    );
    let stored = std::fs::read_to_string(&creds).expect("the login is in the file store");
    assert_eq!(auth.registered(), 1);

    // 2. Online with that login: a new login swaps the connection.
    let out = chat(
        &dir,
        Some(&amp.url),
        &["/mcp login amp"],
        &[("AMP_HARNESS_SNAPSHOT_BROWSER", &approve)],
    )
    .await;
    let screen = flat(&out.stdout);
    assert!(
        screen.contains("logged in to AMP at")
            && screen.contains("the session reconnected with it"),
        "{}",
        out.stdout
    );
    assert!(!screen.contains("re-login needed"), "{}", out.stdout);
    assert_eq!(auth.registered(), 2);
    let stored = {
        let now = std::fs::read_to_string(&creds).unwrap();
        assert_ne!(now, stored, "the new login replaced the old one");
        now
    };

    // 3. Denied in the browser: an error, and nothing changed.
    let out = chat(
        &dir,
        Some(&amp.url),
        &["/mcp login amp"],
        &[("AMP_HARNESS_SNAPSHOT_BROWSER", &deny)],
    )
    .await;
    let screen = flat(&out.stdout);
    assert!(
        screen.contains("/mcp login amp: OAuth: authorization failed: access_denied"),
        "{}",
        out.stdout
    );
    assert!(!screen.contains("logged in to AMP"), "{}", out.stdout);
    assert!(
        !screen.contains("re-login needed"),
        "still connected: {}",
        out.stdout
    );
    assert_eq!(std::fs::read_to_string(&creds).unwrap(), stored);

    // 4. AMP_TOKEN: refused before any browser.
    let registered = auth.registered();
    let out = chat(
        &dir,
        Some(&amp.url),
        &["/mcp login amp"],
        &[
            ("AMP_TOKEN", "planted-static-token"),
            ("AMP_HARNESS_SNAPSHOT_BROWSER", &approve),
        ],
    )
    .await;
    let screen = flat(&out.stdout);
    assert!(
        screen.contains("set a new token in AMP_TOKEN and restart the chat"),
        "{}",
        out.stdout
    );
    assert_eq!(auth.registered(), registered, "no login was started");
    assert_eq!(std::fs::read_to_string(&creds).unwrap(), stored);
}

/// `/model roles` lists every role's model and source; `/model <role> <p/m>` sets it for this
/// chat only (kept by `/reload`, not written); `s` in the picker saves it to config.toml; the
/// chat's own role switches the chat.
async fn model_per_role() {
    let dir = setup("roles", "", "");
    let out = chat(&dir, None, &["/model roles"], &[]).await;
    let rows = panel(&out);
    snapshot("chat-model-roles", &rows);
    assert!(row(&rows, "● manager").contains("boss/boss-1"), "{rows:#?}");
    assert!(
        row(&rows, "worker-backend").contains("wf/wf-1"),
        "{rows:#?}"
    );

    let before = user_config(&dir);
    let out = chat(
        &dir,
        None,
        &[
            "/model worker-backend wf/wf-2 high",
            "/reload",
            "/model roles",
        ],
        &[],
    )
    .await;
    let screen = flat(&out.stdout);
    assert!(
        screen.contains("worker-backend: wf/wf-2 • thinking off for this chat"),
        "{}",
        out.stdout
    );
    assert!(
        screen.contains("model amp-worker-backend: wf/wf-1"),
        "the reload path applied it: {}",
        out.stdout
    );
    let rows = panel(&out);
    let wb = row(&rows, "worker-backend");
    assert!(
        wb.contains("wf/wf-2") && wb.contains("this chat only"),
        "{wb}"
    );
    assert_eq!(user_config(&dir), before, "nothing written");

    // The picker: the current model is selected; ↓ then `s` saves the next one.
    let out = chat(
        &dir,
        None,
        &["/model worker-backend", "key:down", "key:s", "/model roles"],
        &[],
    )
    .await;
    assert!(
        flat(&out.stdout).contains("worker-backend: wf/wf-2 saved in"),
        "{}",
        out.stdout
    );
    let config = user_config(&dir);
    let wb = config
        .split("[roles.worker-backend]")
        .nth(1)
        .expect("[roles.worker-backend] written");
    assert!(wb.contains("model = \"wf/wf-2\""), "{config}");
    let rows = panel(&out);
    let wb = row(&rows, "worker-backend");
    assert!(
        wb.contains("wf/wf-2") && !wb.contains("this chat only"),
        "{wb}"
    );

    // The chat's own role: the chat switches (footer), as /reload does.
    let out = chat(&dir, None, &["/model manager wf/wf-2"], &[]).await;
    assert!(
        flat(&out.stdout).contains("this chat now runs on wf/wf-2"),
        "{}",
        out.stdout
    );
    // Mistakes are refused with the way out.
    let out = chat(
        &dir,
        None,
        &[
            "/model worker-backend nope/x",
            "/model nobody wf/wf-1",
            "/model reviewer --reset",
        ],
        &[],
    )
    .await;
    let screen = flat(&out.stdout);
    assert!(
        screen.contains("`nope/x` is not a configured model"),
        "{screen}"
    );
    assert!(screen.contains("no role `nobody`"), "{screen}");
    assert!(
        screen.contains("reviewer has no model chosen in this chat"),
        "{screen}"
    );
}

/// `/agents`, `/agents <id>`, `/skills`, `/skills <name>` show the session's catalog with the
/// layer each file comes from; a repo agent is marked `repo`.
async fn agents_and_skills() {
    let dir = setup("agents", "", "");
    std::fs::create_dir_all(dir.join("project/.amp/harness/agents")).unwrap();
    std::fs::write(
        dir.join("project/.amp/harness/agents/sql-expert.md"),
        "---\ndescription: Writes SQL for this repo\nmode: subagent\ndispatch: true\n---\nYou write SQL.\n",
    )
    .unwrap();
    // #2310: a repo agent is used once trusted.
    let state = dir.join("home/.local/state");
    let out = common::run(
        &dir,
        NO_AMP,
        &["trust", "agent:sql-expert"],
        &[("XDG_STATE_HOME", state.to_str().unwrap())],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let out = chat(&dir, None, &["/agents"], &[]).await;
    let rows = panel(&out);
    snapshot("chat-agents", &rows);
    let sql = row(&rows, "sql-expert");
    assert!(
        sql.contains("worker") && sql.contains("repo") && sql.contains("wf/wf-1"),
        "{sql}"
    );
    assert!(row(&rows, "manager").contains("primary"), "{rows:#?}");

    let out = chat(&dir, None, &["/agents", "key:down"], &[]).await;
    assert!(
        panel(&out).iter().any(|r| r.starts_with("│ ›")),
        "selection moves"
    );
    let out = chat(&dir, None, &["/agents worker-backend"], &[]).await;
    let rows = panel(&out);
    assert!(
        rows[0].starts_with("┌─ agent · worker-backend"),
        "{rows:#?}"
    );
    assert!(row(&rows, "mode").contains("subagent"), "{rows:#?}");
    assert!(
        row(&rows, "model").contains("wf/wf-1 · thinking"),
        "{rows:#?}"
    );
    assert!(row(&rows, "skills").contains("amp-execution"), "{rows:#?}");
    assert!(row(&rows, "dispatch").contains("yes"), "{rows:#?}");
    assert!(rows.iter().any(|r| r.contains("permissions")), "{rows:#?}");

    let out = chat(&dir, None, &["/agents sql-expert"], &[]).await;
    let rows = panel(&out);
    assert!(
        row(&rows, "file").contains("<repo>/.amp/harness/agents/sql-expert.md"),
        "{rows:#?}"
    );
    assert!(
        row(&rows, "dispatch").contains("dispatch: true"),
        "{rows:#?}"
    );

    let out = chat(&dir, None, &["/skills"], &[]).await;
    let rows = panel(&out);
    assert!(row(&rows, "skill").contains("from"), "{rows:#?}");
    assert!(
        rows.iter().any(|r| r.contains("amp-execution")),
        "{rows:#?}"
    );
    let out = chat(&dir, None, &["/skills amp-execution"], &[]).await;
    let rows = panel(&out);
    assert!(rows[0].starts_with("┌─ skill · amp-execution"), "{rows:#?}");
    assert!(
        row(&rows, "preloaded").contains("worker-backend"),
        "{rows:#?}"
    );
    assert!(rows.iter().any(|r| r.contains("SKILL.md")), "{rows:#?}");
}

/// `/config`: every section with where each value comes from; an env setting says `env VAR`, a
/// provider key its source, and the key itself appears nowhere. Read by scrolling the panel.
async fn config_overview_with_sources() {
    let dir = setup("config", &stdio_table("fs", ""), "");
    let mut seen = String::new();
    for downs in [0, 14, 28, 42, 56] {
        let mut inputs = vec!["/config"];
        inputs.extend(std::iter::repeat_n("key:down", downs));
        let out = chat(
            &dir,
            None,
            &inputs,
            &[
                ("OPENROUTER_API_KEY", PLANTED),
                ("AMP_HARNESS_PADDING", "2"),
            ],
        )
        .await;
        assert!(!out.stdout.contains(PLANTED) && !out.stderr.contains(PLANTED));
        seen.push_str(&panel(&out).join("\n"));
        seen.push('\n');
    }
    let seen = flat(&seen);
    for want in [
        "files",
        "user config ~/.config/amp-harness/config.toml",
        "repo config <repo>/.amp/harness/config.toml",
        "AMP server",
        "url http://127.0.0.1:9/mcp (env AMP_MCP_URL)",
        "inherit wf/wf-1",
        "providers (keys: where they come from)",
        "key: env OPENROUTER_API_KEY",
        "roles (/model roles changes them)",
        "manager boss/boss-1 · off (config config.toml)",
        "MCP servers (/mcp)",
        "fs stdio",
        "chat [tui]",
        "padding 2 (env AMP_HARNESS_PADDING)",
        "mouse true (default)",
        "tools, workers, secrets",
        "max workers 4",
    ] {
        assert!(seen.contains(want), "{want:?} missing from:\n{seen}");
    }
}

/// #4227 (T14): `/config` lists `[compaction]` enabled / reserve_tokens / keep_recent_tokens, each
/// with the layer that set it, the chat role's effective window, and the load warnings (W3 for a
/// model with no window; W2 stays doctor's).
async fn config_shows_compaction_with_sources() {
    let extra = "\n[compaction]\nreserve_tokens = 9000\n\n[providers.oc]\napi = \"openai-completions\"\n\
                 base_url = \"http://127.0.0.1:9/v1\"\nkey_env = \"OC_KEY\"\n[providers.oc.models.\"oc-1\"]\n";
    let dir = setup("config-compaction", extra, "");
    let state = dir.join("home/.local/state");
    let cache = dir.join("home/.cache");
    let config = dir.join("home/.config/amp-harness/config.toml");
    let mut seen = String::new();
    for downs in [0, 14, 28, 42, 56, 70] {
        let mut inputs = vec!["/config"];
        inputs.extend(std::iter::repeat_n("key:down", downs));
        let out = chat(
            &dir,
            None,
            &inputs,
            &[
                ("XDG_STATE_HOME", state.to_str().unwrap()),
                ("XDG_CACHE_HOME", cache.to_str().unwrap()),
                ("AMP_HARNESS_CONFIG", config.to_str().unwrap()),
            ],
        )
        .await;
        seen.push_str(&panel(&out).join("\n"));
        seen.push('\n');
    }
    let seen = flat(&seen);
    for want in [
        "compaction [compaction]",
        "enabled true (default)",
        "reserve_tokens 9000 (config ~/.config/amp-harness/config.toml)",
        "keep_recent_tokens 20000 (default)",
        "chat window boss/boss-1 · window 128000",
        "warning: context_window not set for oc/oc-1; assuming 128000",
    ] {
        assert!(seen.contains(want), "{want:?} missing from:\n{seen}");
    }
    assert!(
        !seen.contains("is above [compaction] reserve_tokens"),
        "W2 is doctor-only:\n{seen}"
    );
}

/// `/help` lists the new commands in its Config group.
async fn help_lists_the_panels() {
    let dir = setup("help", "", "");
    let out = chat(&dir, None, &["/help"], &[]).await;
    for want in [
        "Config",
        "/model [p/m|roles|role]",
        "/mcp [name|action name]",
        "/agents [id]",
        "/skills [name]",
        "/config",
    ] {
        assert!(out.stdout.contains(want), "{want}:\n{}", out.stdout);
    }
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
    if let [flag, url] = args.as_slice()
        && (flag == BROWSER_FLAG || flag == DENY_FLAG)
    {
        return run_browser(flag == DENY_FLAG, url);
    }
    if args.iter().any(|a| a == "--list") {
        return;
    }
    let filter: Vec<&String> = args.iter().filter(|a| !a.starts_with("--")).collect();
    let all: Vec<(&str, Case)> = cases![
        mcp_list_shows_health_and_the_status_line,
        mcp_enable_disable_apply_through_reload,
        mcp_trust_and_untrust_a_repo_server,
        mcp_add_details_test_and_restart,
        mcp_login_amp_logs_in_and_reconnects,
        model_per_role,
        model_roles_show_the_fallback,
        agents_and_skills,
        config_overview_with_sources,
        config_shows_compaction_with_sources,
        help_lists_the_panels,
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
