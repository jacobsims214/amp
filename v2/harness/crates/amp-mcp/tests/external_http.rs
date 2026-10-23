//! Third-party MCP servers over Streamable HTTP (#2237, H4): static and secret headers, a bearer
//! token, OAuth with dynamic registration and with a pre-registered confidential client, a
//! metadata URL instead of discovery, every protocol version from 2024-11-05 to 2026-07-28, and a
//! unix socket.
//!
//! Secret values are compared inside the mocks but never printed: assertions show names, counts
//! and the absence of planted markers.

mod common;

use std::{
    collections::{HashMap, HashSet},
    path::{Path, PathBuf},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
    },
    time::Duration,
};

use amp_mcp::{
    CredentialBackend, Era, ExternalClient, HeaderSource, Health, HttpAuth, HttpHeader,
    HttpServerOptions, McpError, ProtocolMode, Secret,
    external::http::{self, wants_sse_fallback},
    oauth::{CALLBACK_PATH, LoginPrompt, OAuthClientSettings},
    rmcp::model::{ContentBlock, ProtocolVersion},
};
use axum::{
    Form, Json, Router,
    extract::{Query, Request, State},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    middleware::{self, Next},
    response::{IntoResponse, Redirect, Response},
    routing::{get, post},
};
use base64::Engine as _;
use common::{ServerKind, TestServer};
use sha2::Digest as _;

/// Planted in every secret value, so a leak into an error or Debug output is caught.
const MARK: &str = "S3CRET-MARK";

// ---------------------------------------------------------------------------------------------
// Servers
// ---------------------------------------------------------------------------------------------

struct Spawned {
    url: String,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Spawned {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn serve(app: Router) -> Spawned {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Spawned {
        url: format!("http://{addr}/mcp"),
        task,
    }
}

/// The test MCP server behind a check that every `(name, value)` header is present.
fn requiring_headers(kind: ServerKind, required: Vec<(&'static str, String)>) -> Router {
    let required = Arc::new(required);
    common::router(TestServer::new(kind), None).layer(middleware::from_fn(
        move |req: Request, next: Next| {
            let required = required.clone();
            async move {
                let ok = required.iter().all(|(name, value)| {
                    req.headers().get(*name).and_then(|v| v.to_str().ok()) == Some(value.as_str())
                });
                if ok {
                    next.run(req).await
                } else {
                    (StatusCode::UNAUTHORIZED, "missing or wrong header").into_response()
                }
            }
        },
    ))
}

fn options(url: &str) -> HttpServerOptions {
    let mut options = HttpServerOptions::new("acme", url);
    options.external = options
        .external
        .clone()
        .with_connect_timeout(Duration::from_secs(15))
        .with_call_timeout(Duration::from_secs(15));
    options
}

/// A per-test directory under cargo's target dir (never /tmp: D13).
fn scratch(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-mcp-external-http")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

fn file_backend(name: &str) -> CredentialBackend {
    CredentialBackend::file(scratch(name).join("credentials.json"))
}

async fn echo(client: &ExternalClient) -> String {
    let tools = client.list_tools().await.expect("tools/list");
    let names: Vec<&str> = tools.iter().map(|t| t.name.as_ref()).collect();
    assert!(names.contains(&"amp_echo"), "{names:?}");
    let mut args = serde_json::Map::new();
    args.insert("hello".into(), "world".into());
    let result = client
        .call_tool("amp_echo", Some(args))
        .await
        .expect("tools/call");
    match &result.content[0] {
        ContentBlock::Text(text) => text.text.clone(),
        other => panic!("unexpected content {other:?}"),
    }
}

fn assert_no_secret(text: &str) {
    assert!(!text.contains(MARK), "a secret leaked: {text}");
}

// ---------------------------------------------------------------------------------------------
// Headers and bearer
// ---------------------------------------------------------------------------------------------

#[tokio::test]
async fn static_and_secret_headers_are_sent_and_never_shown() {
    let api_key = format!("{MARK}-key");
    let server = serve(requiring_headers(
        ServerKind::Dual,
        vec![("x-org", "simstech".into()), ("x-api-key", api_key.clone())],
    ))
    .await;
    let backend = file_backend("headers");

    let opts = options(&server.url)
        .with_header(HttpHeader::new("X-Org", "simstech", HeaderSource::Config))
        .with_header(HttpHeader::new(
            "X-Api-Key",
            api_key.clone(),
            HeaderSource::Keychain,
        ));
    assert_no_secret(&format!("{opts:?}"));
    let client = http::connect(&opts, &backend).await.expect("connect");
    assert!(echo(&client).await.contains("world"));
    let session = client.session();
    assert_eq!(session.server, "acme");
    assert_eq!(session.transport, "http");
    assert_eq!(session.auth, "header X-Api-Key (keychain)");
    assert_eq!(client.health(), Health::Connected);
    assert_no_secret(&format!("{client:?}"));
    client.close().await;

    // Without the secret header the server refuses, and the error says what was used, not its value.
    let opts = options(&server.url)
        .with_header(HttpHeader::new("X-Org", "simstech", HeaderSource::Config))
        .with_header(HttpHeader::new(
            "X-Api-Key",
            format!("{MARK}-wrong"),
            HeaderSource::Keychain,
        ));
    let err = http::connect(&opts, &backend).await.expect_err("wrong key");
    assert!(matches!(err, McpError::ServerAuth { .. }), "{err:?}");
    let text = err.to_string();
    assert!(text.contains("`acme`"), "{text}");
    assert!(
        text.contains("rejected the configured credentials"),
        "{text}"
    );
    assert!(text.contains("X-Api-Key"), "{text}");
    assert_no_secret(&text);
    assert_no_secret(&format!("{err:?}"));
}

#[tokio::test]
async fn bearer_token_wins_and_a_wrong_one_names_its_variable() {
    const TOKEN: &str = "S3CRET-MARK-bearer";
    let server = common::spawn_with_bearer(TestServer::new(ServerKind::Dual), Some(TOKEN)).await;
    let backend = file_backend("bearer");

    let client = http::connect(
        &options(&server.url).with_bearer(TOKEN, "env ACME_TOKEN"),
        &backend,
    )
    .await
    .expect("connect with bearer");
    assert!(echo(&client).await.contains("world"));
    assert_eq!(client.session().auth, "bearer (env ACME_TOKEN)");
    client.close().await;

    let err = http::connect(
        &options(&server.url).with_bearer(format!("{MARK}-nope"), "env ACME_TOKEN"),
        &backend,
    )
    .await
    .expect_err("wrong bearer");
    assert!(err.is_auth_required(), "{err:?}");
    let text = err.to_string();
    assert!(text.contains("env ACME_TOKEN"), "{text}");
    assert!(
        !text.contains("mcp login"),
        "a bearer is not fixed by a login: {text}"
    );
    assert_no_secret(&text);
}

#[tokio::test]
async fn an_authorization_header_works_like_a_bearer() {
    const TOKEN: &str = "S3CRET-MARK-hdr";
    let server = common::spawn_with_bearer(TestServer::new(ServerKind::Dual), Some(TOKEN)).await;
    let client = http::connect(
        &options(&server.url).with_header(HttpHeader::new(
            "Authorization",
            format!("Bearer {TOKEN}"),
            HeaderSource::Keychain,
        )),
        &file_backend("authz-header"),
    )
    .await
    .expect("connect");
    assert!(echo(&client).await.contains("world"));
    client.close().await;
}

#[tokio::test]
async fn bad_header_settings_are_config_errors_without_values() {
    let backend = file_backend("bad-headers");
    let cases: Vec<(HttpServerOptions, &str)> = vec![
        (
            options("http://127.0.0.1:1/mcp").with_header(HttpHeader::new(
                "Accept",
                "x",
                HeaderSource::Config,
            )),
            "set by the MCP transport",
        ),
        (
            options("http://127.0.0.1:1/mcp").with_header(HttpHeader::new(
                "Bad Name",
                "x",
                HeaderSource::Config,
            )),
            "not a valid name",
        ),
        (
            options("http://127.0.0.1:1/mcp").with_header(HttpHeader::new(
                "X-Key",
                format!("{MARK}\nsplit"),
                HeaderSource::Keychain,
            )),
            "cannot carry",
        ),
        (
            options("http://127.0.0.1:1/mcp")
                .with_header(HttpHeader::new(
                    "Authorization",
                    format!("Bearer {MARK}"),
                    HeaderSource::Keychain,
                ))
                .with_bearer(MARK, "env T"),
            "keep one",
        ),
        (
            options("http://127.0.0.1:1/mcp")
                .with_bearer(MARK, "env T")
                .with_auth(HttpAuth::OAuth),
            "conflicts",
        ),
        (options("ftp://example.com/mcp"), "http:// or https://"),
    ];
    for (opts, want) in cases {
        let err = http::prepare(&opts, &backend).await.expect_err(want);
        let text = err.to_string();
        assert!(matches!(err, McpError::Config(_)), "{text}");
        assert!(text.contains(want), "{want}: {text}");
        assert_no_secret(&text);
    }
}

// ---------------------------------------------------------------------------------------------
// Protocol versions
// ---------------------------------------------------------------------------------------------

#[tokio::test]
async fn every_protocol_version_from_2024_11_05_to_2026_07_28() {
    let backend = file_backend("protocols");
    let cases = [
        (
            ServerKind::Dual,
            ProtocolMode::Auto,
            ProtocolVersion::V_2026_07_28,
            false,
        ),
        (
            ServerKind::Dual,
            ProtocolMode::Modern,
            ProtocolVersion::V_2026_07_28,
            false,
        ),
        (
            ServerKind::Dual,
            ProtocolMode::Legacy,
            ProtocolVersion::V_2025_11_25,
            false,
        ),
        (
            ServerKind::LegacyOnly,
            ProtocolMode::Auto,
            ProtocolVersion::V_2025_11_25,
            true,
        ),
        (
            ServerKind::NoModernVersion,
            ProtocolMode::Auto,
            ProtocolVersion::V_2025_11_25,
            true,
        ),
        (
            ServerKind::Only20241105,
            ProtocolMode::Auto,
            ProtocolVersion::V_2024_11_05,
            true,
        ),
        (
            ServerKind::Only20241105,
            ProtocolMode::Legacy,
            ProtocolVersion::V_2024_11_05,
            false,
        ),
    ];
    for (kind, mode, want, fell_back) in cases {
        let server = common::spawn(TestServer::new(kind)).await;
        let mut opts = options(&server.url);
        opts.external = opts.external.clone().with_protocol(mode);
        let client = http::connect(&opts, &backend)
            .await
            .unwrap_or_else(|e| panic!("{kind:?} {mode}: {e}"));
        let session = client.session();
        assert_eq!(session.protocol_version, want, "{kind:?} {mode}");
        assert_eq!(session.fell_back, fell_back, "{kind:?} {mode}");
        assert_eq!(
            session.era,
            if want == ProtocolVersion::V_2026_07_28 {
                Era::Stateless
            } else {
                Era::Legacy
            }
        );
        assert_eq!(session.server_name.as_deref(), Some("amp-test"));
        assert_eq!(session.instructions.as_deref(), Some(common::INSTRUCTIONS));
        assert!(echo(&client).await.contains("world"), "{kind:?} {mode}");
        client.close().await;
    }

    // Forcing 2026-07-28 on a server without it is an error, not a silent downgrade.
    let server = common::spawn(TestServer::new(ServerKind::Only20241105)).await;
    let mut opts = options(&server.url);
    opts.external = opts.external.clone().with_protocol(ProtocolMode::Modern);
    assert!(http::connect(&opts, &backend).await.is_err());
}

#[tokio::test]
async fn a_refused_post_is_an_sse_fallback_candidate() {
    // A plain HTTP server with no MCP endpoint: POST /mcp answers 405 like an old SSE-only server.
    let app = Router::new().route("/mcp", get(|| async { "sse lives here" }));
    let server = serve(app).await;
    let err = http::connect(&options(&server.url), &file_backend("sse-candidate"))
        .await
        .expect_err("not Streamable HTTP");
    assert!(wants_sse_fallback(&err), "{err}");

    let refused = http::connect(&options("http://127.0.0.1:1/mcp"), &file_backend("refused"))
        .await
        .expect_err("refused");
    assert!(!wants_sse_fallback(&refused), "{refused}");
}

#[tokio::test]
async fn tool_errors_and_timeouts() {
    let server = common::spawn(TestServer::new(ServerKind::Dual)).await;
    let client = http::connect(&options(&server.url), &file_backend("timeouts"))
        .await
        .unwrap();
    let mut args = serde_json::Map::new();
    args.insert("ms".into(), 2_000.into());
    let err = client
        .call_tool_with_timeout("amp_sleep", Some(args), Duration::from_millis(200))
        .await
        .expect_err("timeout");
    assert!(err.is_timeout(), "{err}");
    assert!(err.to_string().contains("acme"), "{err}");
    let failed = client.call_tool("amp_fail", None).await.expect("answered");
    assert_eq!(failed.is_error, Some(true));
    assert!(!client.take_tools_changed());
    client.close().await;
}

#[cfg(unix)]
#[tokio::test]
async fn over_a_unix_socket() {
    // Kept short: unix socket paths are limited to about 104 bytes.
    let dir = scratch("sock");
    let path = dir.join("s");
    let path = if path.as_os_str().len() < 100 {
        path
    } else {
        eprintln!("skipped: scratch path too long for a unix socket");
        return;
    };
    let listener = tokio::net::UnixListener::bind(&path).unwrap();
    let app = common::router(TestServer::new(ServerKind::Dual), None);
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    let client = http::connect(
        &options("http://mcp.local/mcp").with_socket(&path),
        &file_backend("sock-creds"),
    )
    .await
    .expect("connect over the socket");
    assert_eq!(client.session().transport, "http (unix socket)");
    assert!(echo(&client).await.contains("world"));
    client.close().await;
    task.abort();
}

// ---------------------------------------------------------------------------------------------
// OAuth: a mock authorization server + protected resource on one port
// ---------------------------------------------------------------------------------------------

/// A registered client: its redirect URIs and, for a confidential one, its secret.
type Client = (Vec<String>, Option<String>);

#[derive(Default)]
struct AsState {
    base: String,
    expires_in: AtomicU64,
    /// Dynamic registration allowed.
    dcr: AtomicBool,
    /// The RFC 8414 well-known answers 404 (only the custom metadata URL works).
    hide_well_known: AtomicBool,
    /// client_id -> (redirect URIs, secret).
    clients: Mutex<HashMap<String, Client>>,
    codes: Mutex<HashMap<String, (String, String, String)>>,
    access_tokens: Mutex<HashSet<String>>,
    refresh_tokens: Mutex<HashMap<String, String>>,
    registrations: AtomicUsize,
    refreshes: AtomicUsize,
    /// Token requests from a confidential client that carried the right secret.
    authenticated_token_requests: AtomicUsize,
    resource_requests: AtomicUsize,
    seq: AtomicUsize,
}

impl AsState {
    fn next(&self, prefix: &str) -> String {
        format!("{prefix}-{}", self.seq.fetch_add(1, Ordering::SeqCst))
    }

    fn issue(&self, client_id: &str) -> serde_json::Value {
        let access = self.next("at");
        let refresh = self.next("rt");
        self.access_tokens.lock().unwrap().insert(access.clone());
        self.refresh_tokens
            .lock()
            .unwrap()
            .insert(refresh.clone(), client_id.to_owned());
        serde_json::json!({
            "access_token": access,
            "token_type": "Bearer",
            "expires_in": self.expires_in.load(Ordering::SeqCst),
            "refresh_token": refresh,
        })
    }

    fn metadata(&self) -> serde_json::Value {
        serde_json::json!({
            "issuer": self.base,
            "authorization_endpoint": format!("{}/authorize", self.base),
            "token_endpoint": format!("{}/token", self.base),
            "registration_endpoint": format!("{}/register", self.base),
            "response_types_supported": ["code"],
            "grant_types_supported": ["authorization_code", "refresh_token"],
            "code_challenge_methods_supported": ["S256"],
            "token_endpoint_auth_methods_supported": ["none", "client_secret_basic"],
            "scopes_supported": ["read", "write", "offline_access"],
        })
    }
}

struct MockAs {
    url: String,
    state: Arc<AsState>,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for MockAs {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn spawn_mock_as(expires_in: u64) -> MockAs {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    let state = Arc::new(AsState {
        base: base.clone(),
        expires_in: AtomicU64::new(expires_in),
        dcr: AtomicBool::new(true),
        ..Default::default()
    });
    let mcp = requiring_headers(ServerKind::Dual, vec![("x-org", "simstech".into())]).layer(
        middleware::from_fn_with_state(state.clone(), require_issued_bearer),
    );
    let app = Router::new()
        .route("/.well-known/oauth-protected-resource/mcp", get(prm))
        .route("/.well-known/oauth-authorization-server", get(well_known))
        .route("/custom/as-metadata", get(custom_metadata))
        .route("/register", post(register))
        .route("/authorize", get(authorize))
        .route("/token", post(token))
        .with_state(state.clone())
        .merge(mcp);
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    MockAs {
        url: format!("{base}/mcp"),
        state,
        task,
    }
}

async fn require_issued_bearer(
    State(state): State<Arc<AsState>>,
    req: Request,
    next: Next,
) -> Response {
    state.resource_requests.fetch_add(1, Ordering::SeqCst);
    let ok = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .is_some_and(|t| state.access_tokens.lock().unwrap().contains(t));
    if ok {
        return next.run(req).await;
    }
    let mut response = (StatusCode::UNAUTHORIZED, "unauthorized").into_response();
    let challenge = format!(
        r#"Bearer resource_metadata="{}/.well-known/oauth-protected-resource/mcp", scope="read""#,
        state.base
    );
    response.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_str(&challenge).unwrap(),
    );
    response
}

async fn prm(State(state): State<Arc<AsState>>) -> Json<serde_json::Value> {
    Json(serde_json::json!({
        "resource": format!("{}/mcp", state.base),
        "authorization_servers": [state.base],
        "scopes_supported": ["read", "write", "offline_access"],
        "bearer_methods_supported": ["header"],
    }))
}

async fn well_known(State(state): State<Arc<AsState>>) -> Response {
    if state.hide_well_known.load(Ordering::SeqCst) {
        return StatusCode::NOT_FOUND.into_response();
    }
    Json(state.metadata()).into_response()
}

async fn custom_metadata(State(state): State<Arc<AsState>>) -> Json<serde_json::Value> {
    Json(state.metadata())
}

async fn register(
    State(state): State<Arc<AsState>>,
    Json(body): Json<serde_json::Value>,
) -> Response {
    if !state.dcr.load(Ordering::SeqCst) {
        return StatusCode::NOT_FOUND.into_response();
    }
    state.registrations.fetch_add(1, Ordering::SeqCst);
    let redirect_uris: Vec<String> = body["redirect_uris"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(str::to_owned))
                .collect()
        })
        .unwrap_or_default();
    let client_id = state.next("client");
    state
        .clients
        .lock()
        .unwrap()
        .insert(client_id.clone(), (redirect_uris.clone(), None));
    (
        StatusCode::CREATED,
        Json(serde_json::json!({
            "client_id": client_id,
            "redirect_uris": redirect_uris,
            "token_endpoint_auth_method": "none",
        })),
    )
        .into_response()
}

async fn authorize(
    State(state): State<Arc<AsState>>,
    Query(q): Query<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| q.get(k).cloned().unwrap_or_default();
    let (client_id, redirect_uri) = (get("client_id"), get("redirect_uri"));
    let registered = state
        .clients
        .lock()
        .unwrap()
        .get(&client_id)
        .is_some_and(|(uris, _)| uris.contains(&redirect_uri));
    if get("response_type") != "code"
        || !registered
        || get("code_challenge_method") != "S256"
        || get("state").is_empty()
    {
        return (StatusCode::BAD_REQUEST, "bad authorization request").into_response();
    }
    let code = state.next("code");
    state.codes.lock().unwrap().insert(
        code.clone(),
        (client_id, redirect_uri.clone(), get("code_challenge")),
    );
    Redirect::to(&format!(
        "{redirect_uri}?code={code}&state={}",
        get("state")
    ))
    .into_response()
}

/// The client id from `client_secret_basic` (checked against the registered secret) or the form.
/// `None`: a confidential client without its secret.
fn token_client(
    state: &AsState,
    headers: &HeaderMap,
    form: &HashMap<String, String>,
) -> Option<String> {
    let basic = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Basic "))
        .and_then(|b| base64::engine::general_purpose::STANDARD.decode(b).ok())
        .and_then(|b| String::from_utf8(b).ok());
    let clients = state.clients.lock().unwrap();
    if let Some(pair) = basic {
        let (id, secret) = pair.split_once(':')?;
        let (id, secret) = (urldecode(id), urldecode(secret));
        let (_, registered) = clients.get(&id)?;
        if registered.as_deref() == Some(secret.as_str()) {
            state
                .authenticated_token_requests
                .fetch_add(1, Ordering::SeqCst);
            return Some(id);
        }
        return None;
    }
    let id = form.get("client_id")?.clone();
    match clients.get(&id)? {
        (_, None) => Some(id),
        (_, Some(_)) => None,
    }
}

async fn token(
    State(state): State<Arc<AsState>>,
    headers: HeaderMap,
    Form(form): Form<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| form.get(k).cloned().unwrap_or_default();
    let invalid = |error: &str| {
        (
            StatusCode::BAD_REQUEST,
            Json(serde_json::json!({ "error": error })),
        )
            .into_response()
    };
    let Some(client_id) = token_client(&state, &headers, &form) else {
        return invalid("invalid_client");
    };
    match get("grant_type").as_str() {
        "authorization_code" => {
            let Some((code_client, redirect_uri, challenge)) =
                state.codes.lock().unwrap().remove(&get("code"))
            else {
                return invalid("invalid_grant");
            };
            let verifier = base64::engine::general_purpose::URL_SAFE_NO_PAD
                .encode(sha2::Sha256::digest(get("code_verifier").as_bytes()));
            if verifier != challenge
                || redirect_uri != get("redirect_uri")
                || code_client != client_id
            {
                return invalid("invalid_grant");
            }
            Json(state.issue(&client_id)).into_response()
        }
        "refresh_token" => {
            state.refreshes.fetch_add(1, Ordering::SeqCst);
            let owner = state
                .refresh_tokens
                .lock()
                .unwrap()
                .remove(&get("refresh_token"));
            if owner.as_deref() != Some(client_id.as_str()) {
                return invalid("invalid_grant");
            }
            Json(state.issue(&client_id)).into_response()
        }
        _ => invalid("unsupported_grant_type"),
    }
}

fn urldecode(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'%' if i + 2 < bytes.len() => {
                let hex = std::str::from_utf8(&bytes[i + 1..i + 3]).unwrap();
                out.push(u8::from_str_radix(hex, 16).unwrap());
                i += 3;
            }
            b'+' => {
                out.push(b' ');
                i += 1;
            }
            b => {
                out.push(b);
                i += 1;
            }
        }
    }
    String::from_utf8(out).unwrap()
}

/// The scripted browser: follows the authorization URL and the AS's redirect back to the listener.
fn browser(prompt: &LoginPrompt) {
    assert!(prompt.redirect_uri.starts_with("http://127.0.0.1:"));
    assert!(prompt.redirect_uri.ends_with(CALLBACK_PATH));
    let url = prompt.authorization_url.clone();
    tokio::spawn(async move {
        let http = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .unwrap();
        let response = http.get(&url).send().await.unwrap();
        assert_eq!(response.status(), StatusCode::SEE_OTHER, "authorize");
        let location = response.headers()[header::LOCATION]
            .to_str()
            .unwrap()
            .to_owned();
        let _ = http.get(&location).send().await;
    });
}

fn oauth_options(mock: &MockAs) -> HttpServerOptions {
    options(&mock.url).with_header(HttpHeader::new("X-Org", "simstech", HeaderSource::Config))
}

fn free_port() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

#[tokio::test]
async fn oauth_with_dynamic_registration_logs_in_connects_and_refreshes() {
    let mock = spawn_mock_as(3600).await;
    let backend = file_backend("oauth-dcr");
    let opts = oauth_options(&mock);

    // auto without a login: one probe, then "login required" with the challenge for login.
    let err = http::connect(&opts, &backend).await.expect_err("no login");
    let McpError::ServerAuth { challenge, .. } = &err else {
        panic!("{err:?}");
    };
    assert!(
        challenge
            .as_deref()
            .is_some_and(|c| c.contains("resource_metadata"))
    );
    assert!(
        err.to_string().contains("amp-harness mcp login acme"),
        "{err}"
    );

    // oauth without a login fails before any request.
    let before = mock.state.resource_requests.load(Ordering::SeqCst);
    let err = http::connect(&opts.clone().with_auth(HttpAuth::OAuth), &backend)
        .await
        .expect_err("oauth, no login");
    assert!(matches!(err, McpError::ServerAuth { .. }), "{err:?}");
    assert_eq!(mock.state.resource_requests.load(Ordering::SeqCst), before);

    // Login: discovery from the challenge, DCR, PKCE, code exchange; the probe sends our headers.
    let outcome = http::login(&opts, backend.clone(), Duration::from_secs(20), browser)
        .await
        .expect("login");
    assert_eq!(mock.state.registrations.load(Ordering::SeqCst), 1);
    assert!(outcome.client_id.starts_with("client-"));
    assert!(outcome.has_refresh_token);

    let client = http::connect(&opts, &backend)
        .await
        .expect("connect with the login");
    assert!(
        client.session().auth.starts_with("oauth ("),
        "{}",
        client.session().auth
    );
    assert!(echo(&client).await.contains("world"));
    client.close().await;

    // auth = "none" ignores the stored login.
    let err = http::connect(&opts.clone().with_auth(HttpAuth::None), &backend)
        .await
        .expect_err("none");
    assert!(err.is_auth_required());

    // Tokens revoked server-side: the refresh token gets a new one on the 401, no new login.
    mock.state.access_tokens.lock().unwrap().clear();
    let client = http::connect(&opts, &backend)
        .await
        .expect("connect after revoke");
    assert!(echo(&client).await.contains("world"));
    assert!(mock.state.refreshes.load(Ordering::SeqCst) >= 1);
    assert_eq!(mock.state.registrations.load(Ordering::SeqCst), 1);
    client.close().await;

    // Logout removes it.
    assert!(http::logout(&opts, &backend).await.unwrap());
    assert!(
        http::connect(&opts, &backend)
            .await
            .unwrap_err()
            .is_auth_required()
    );
}

#[tokio::test]
async fn oauth_with_a_pre_registered_confidential_client_and_a_metadata_url() {
    let mock = spawn_mock_as(1).await;
    // No dynamic registration and no RFC 8414 well-known: only the configured client and URL work.
    mock.state.dcr.store(false, Ordering::SeqCst);
    mock.state.hide_well_known.store(true, Ordering::SeqCst);
    let port = free_port();
    let secret = format!("{MARK}-client-secret");
    mock.state.clients.lock().unwrap().insert(
        "pre-registered".into(),
        (
            vec![format!("http://127.0.0.1:{port}{CALLBACK_PATH}")],
            Some(secret.clone()),
        ),
    );
    let settings = OAuthClientSettings {
        client_id: Some("pre-registered".into()),
        client_secret: Some(Secret::new(secret.clone())),
        scopes: vec!["read".into()],
        callback_port: port,
        auth_server_metadata_url: Some(format!("{}/custom/as-metadata", mock.state.base)),
    };
    let opts = oauth_options(&mock).with_oauth(settings.clone());
    assert_no_secret(&format!("{opts:?}"));
    let backend = file_backend("oauth-pre");

    let outcome = http::login(&opts, backend.clone(), Duration::from_secs(20), browser)
        .await
        .expect("login with the pre-registered client");
    assert_eq!(outcome.client_id, "pre-registered");
    assert_eq!(mock.state.registrations.load(Ordering::SeqCst), 0);
    assert_eq!(
        mock.state
            .authenticated_token_requests
            .load(Ordering::SeqCst),
        1
    );

    // expires_in = 1: every use refreshes, and each refresh must carry the client secret.
    let client = http::connect(&opts, &backend).await.expect("connect");
    assert!(echo(&client).await.contains("world"));
    let refreshes = mock.state.refreshes.load(Ordering::SeqCst);
    assert!(refreshes >= 1, "expected a refresh");
    assert_eq!(
        mock.state
            .authenticated_token_requests
            .load(Ordering::SeqCst),
        1 + refreshes,
        "every refresh authenticated with the client secret"
    );
    client.close().await;

    // Without the secret the AS refuses the refresh as invalid_client (a config problem, not a
    // login one), and nothing secret is shown.
    let mut no_secret = opts.clone();
    no_secret.oauth.client_secret = None;
    let err = match http::connect(&no_secret, &backend).await {
        Err(e) => e,
        Ok(client) => client
            .list_tools()
            .await
            .expect_err("refresh without secret"),
    };
    assert!(err.to_string().contains("invalid_client"), "{err}");
    assert_no_secret(&format!("{err} {err:?}"));
}

#[tokio::test]
async fn login_is_refused_where_it_cannot_apply() {
    let backend = file_backend("login-refused");
    let server = common::spawn(TestServer::new(ServerKind::Dual)).await;
    let err = http::login(
        &options(&server.url),
        backend.clone(),
        Duration::from_secs(5),
        browser,
    )
    .await
    .expect_err("open server");
    assert!(err.to_string().contains("nothing to log in to"), "{err}");

    let err = http::login(
        &options(&server.url).with_bearer(MARK, "env T"),
        backend.clone(),
        Duration::from_secs(5),
        browser,
    )
    .await
    .expect_err("bearer configured");
    assert!(err.to_string().contains("always wins"), "{err}");
    assert_no_secret(&err.to_string());

    let err = http::login(
        &options(&server.url).with_auth(HttpAuth::None),
        backend,
        Duration::from_secs(5),
        browser,
    )
    .await
    .expect_err("auth none");
    assert!(err.to_string().contains("auth = \"none\""), "{err}");
}
