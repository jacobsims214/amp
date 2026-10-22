//! OAuth login (D3) against a mock authorization server: RFC 9728 + RFC 8414 discovery, dynamic
//! client registration, PKCE S256 with the loopback redirect, refresh, and the credential stores.
//!
//! The mock serves, on one loopback port, both the protected MCP resource (the rmcp test server
//! from `common`, behind a bearer check against the tokens it issued) and the authorization server.
//! A scripted "browser" follows the authorization URL and the redirect back to the listener.
//!
//! Token values are compared inside the tests but never printed: assertion messages show only
//! counts and fingerprints.

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
    Auth, ClientOptions, CredentialBackend, Health, McpError, ProtocolMode, StaticToken,
    credential_key,
    oauth::{self, LoginOptions, LoginOutcome, LoginPrompt},
};
use axum::{
    Form, Json, Router,
    extract::{Query, Request, State},
    http::{HeaderValue, StatusCode, header},
    middleware::{self, Next},
    response::{IntoResponse, Redirect, Response},
    routing::{get, post},
};
use base64::Engine as _;
use common::{ServerKind, TestServer};
use sha2::Digest as _;

// ---------------------------------------------------------------------------------------------
// The mock authorization server + protected resource.
// ---------------------------------------------------------------------------------------------

#[derive(Default)]
struct AsState {
    base: String,
    /// `expires_in` on issued access tokens.
    expires_in: AtomicU64,
    /// Answer refresh requests with `invalid_grant`.
    reject_refresh: AtomicBool,
    /// Accept this static bearer at the resource too (an `amp_` token from the UI).
    static_token: Mutex<Option<String>>,
    clients: Mutex<HashMap<String, Vec<String>>>,
    codes: Mutex<HashMap<String, PendingCode>>,
    access_tokens: Mutex<HashSet<String>>,
    refresh_tokens: Mutex<HashMap<String, String>>,
    registrations: AtomicUsize,
    code_exchanges: AtomicUsize,
    refreshes: AtomicUsize,
    rejected_at_resource: AtomicUsize,
    seq: AtomicUsize,
    /// What the client asked for in the authorization request.
    last_scope: Mutex<Option<String>>,
    /// The RFC 8414 metadata endpoint accepts the request and never answers (a stalled AS).
    stall_metadata: AtomicBool,
    /// Advertise this `authorization_endpoint` instead of our own `/authorize` (a hostile AS).
    authorization_endpoint: Mutex<Option<String>>,
}

struct PendingCode {
    client_id: String,
    redirect_uri: String,
    challenge: String,
}

impl AsState {
    fn next(&self, prefix: &str) -> String {
        format!("{prefix}-{}", self.seq.fetch_add(1, Ordering::SeqCst))
    }

    fn issue(&self, client_id: &str, scope: Option<&str>) -> serde_json::Value {
        let access = self.next("at");
        let refresh = self.next("rt");
        self.access_tokens.lock().unwrap().insert(access.clone());
        self.refresh_tokens
            .lock()
            .unwrap()
            .insert(refresh.clone(), client_id.to_owned());
        let mut body = serde_json::json!({
            "access_token": access,
            "token_type": "Bearer",
            "expires_in": self.expires_in.load(Ordering::SeqCst),
            "refresh_token": refresh,
        });
        if let Some(scope) = scope {
            body["scope"] = scope.into();
        }
        body
    }

    /// Revokes every access token (refresh tokens stay valid), like an expired or revoked session.
    fn revoke_access_tokens(&self) {
        self.access_tokens.lock().unwrap().clear();
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
        ..Default::default()
    });

    let mcp = common::router(TestServer::new(ServerKind::Dual), None).layer(
        middleware::from_fn_with_state(state.clone(), require_issued_bearer),
    );
    let app = Router::new()
        .route(
            "/.well-known/oauth-protected-resource",
            get(protected_resource),
        )
        .route(
            "/.well-known/oauth-protected-resource/mcp",
            get(protected_resource),
        )
        .route(
            "/.well-known/oauth-authorization-server",
            get(authorization_server),
        )
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
    let bearer = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .map(str::to_owned);
    let ok = bearer.is_some_and(|token| {
        state.access_tokens.lock().unwrap().contains(&token)
            || state.static_token.lock().unwrap().as_deref() == Some(token.as_str())
    });
    if ok {
        return next.run(req).await;
    }
    state.rejected_at_resource.fetch_add(1, Ordering::SeqCst);
    let mut response = (StatusCode::UNAUTHORIZED, "unauthorized").into_response();
    let challenge = format!(
        r#"Bearer resource_metadata="{}/.well-known/oauth-protected-resource/mcp", scope="amp""#,
        state.base
    );
    response.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_str(&challenge).unwrap(),
    );
    response
}

async fn protected_resource(State(state): State<Arc<AsState>>) -> Json<serde_json::Value> {
    Json(serde_json::json!({
        "resource": format!("{}/mcp", state.base),
        "authorization_servers": [state.base],
        "scopes_supported": ["amp", "offline_access"],
        "bearer_methods_supported": ["header"],
    }))
}

async fn authorization_server(State(state): State<Arc<AsState>>) -> Json<serde_json::Value> {
    if state.stall_metadata.load(Ordering::SeqCst) {
        std::future::pending::<()>().await;
    }
    // Shaped like AMP's amp-mcpoauth metadata (v2/amp-api/cmd/mcpoauth/main.go).
    Json(serde_json::json!({
        "issuer": state.base,
        "authorization_endpoint": state
            .authorization_endpoint
            .lock()
            .unwrap()
            .clone()
            .unwrap_or_else(|| format!("{}/authorize", state.base)),
        "token_endpoint": format!("{}/token", state.base),
        "registration_endpoint": format!("{}/register", state.base),
        "response_types_supported": ["code"],
        "grant_types_supported": ["authorization_code", "refresh_token"],
        "code_challenge_methods_supported": ["S256"],
        "token_endpoint_auth_methods_supported": ["none"],
        "scopes_supported": ["amp", "offline_access"],
        "authorization_response_iss_parameter_supported": false,
    }))
}

async fn register(
    State(state): State<Arc<AsState>>,
    Json(body): Json<serde_json::Value>,
) -> Response {
    state.registrations.fetch_add(1, Ordering::SeqCst);
    let redirect_uris: Vec<String> = body["redirect_uris"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(str::to_owned))
                .collect()
        })
        .unwrap_or_default();
    // A public native client with a loopback redirect, as amp-mcpoauth expects.
    if body["token_endpoint_auth_method"] != "none"
        || redirect_uris.is_empty()
        || !redirect_uris
            .iter()
            .all(|u| u.starts_with("http://127.0.0.1:"))
    {
        return (
            StatusCode::BAD_REQUEST,
            Json(serde_json::json!({"error": "invalid_client_metadata"})),
        )
            .into_response();
    }
    let client_id = state.next("client");
    state
        .clients
        .lock()
        .unwrap()
        .insert(client_id.clone(), redirect_uris.clone());
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
    let client_id = get("client_id");
    let redirect_uri = get("redirect_uri");
    let registered = state
        .clients
        .lock()
        .unwrap()
        .get(&client_id)
        .is_some_and(|uris| uris.contains(&redirect_uri));
    if get("response_type") != "code"
        || !registered
        || get("code_challenge_method") != "S256"
        || get("code_challenge").is_empty()
        || get("state").is_empty()
    {
        return (StatusCode::BAD_REQUEST, "bad authorization request").into_response();
    }
    *state.last_scope.lock().unwrap() = q.get("scope").cloned();
    let code = state.next("code");
    state.codes.lock().unwrap().insert(
        code.clone(),
        PendingCode {
            client_id,
            redirect_uri: redirect_uri.clone(),
            challenge: get("code_challenge"),
        },
    );
    Redirect::to(&format!(
        "{redirect_uri}?code={code}&state={}",
        urlencode(&get("state"))
    ))
    .into_response()
}

async fn token(
    State(state): State<Arc<AsState>>,
    Form(form): Form<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| form.get(k).cloned().unwrap_or_default();
    let invalid_grant = || {
        (
            StatusCode::BAD_REQUEST,
            Json(serde_json::json!({"error": "invalid_grant"})),
        )
            .into_response()
    };
    match get("grant_type").as_str() {
        "authorization_code" => {
            state.code_exchanges.fetch_add(1, Ordering::SeqCst);
            let Some(pending) = state.codes.lock().unwrap().remove(&get("code")) else {
                return invalid_grant();
            };
            let verifier_hash = base64::engine::general_purpose::URL_SAFE_NO_PAD
                .encode(sha2::Sha256::digest(get("code_verifier").as_bytes()));
            if verifier_hash != pending.challenge
                || pending.redirect_uri != get("redirect_uri")
                || pending.client_id != get("client_id")
            {
                return invalid_grant();
            }
            let scope = state.last_scope.lock().unwrap().clone();
            Json(state.issue(&pending.client_id, scope.as_deref())).into_response()
        }
        "refresh_token" => {
            state.refreshes.fetch_add(1, Ordering::SeqCst);
            if state.reject_refresh.load(Ordering::SeqCst) {
                return invalid_grant();
            }
            // Rotating refresh tokens: the old one is spent.
            let Some(client_id) = state
                .refresh_tokens
                .lock()
                .unwrap()
                .remove(&get("refresh_token"))
            else {
                return invalid_grant();
            };
            if client_id != get("client_id") {
                return invalid_grant();
            }
            Json(state.issue(&client_id, None)).into_response()
        }
        _ => (
            StatusCode::BAD_REQUEST,
            Json(serde_json::json!({"error": "unsupported_grant_type"})),
        )
            .into_response(),
    }
}

fn urlencode(s: &str) -> String {
    s.bytes()
        .map(|b| match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                (b as char).to_string()
            }
            _ => format!("%{b:02X}"),
        })
        .collect()
}

// ---------------------------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------------------------

/// A per-test directory under cargo's target dir (never /tmp: D13).
fn scratch(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-mcp-oauth")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

fn file_backend(name: &str) -> CredentialBackend {
    CredentialBackend::file(scratch(name).join("amp-harness").join("credentials.json"))
}

/// The scripted browser: follows the authorization URL to the AS, then the AS's redirect back to
/// the loopback listener. `tamper` may rewrite the redirect (wrong state, an error response).
fn browser(tamper: Option<fn(String) -> Vec<String>>) -> impl FnOnce(&LoginPrompt) {
    move |prompt: &LoginPrompt| {
        assert!(prompt.redirect_uri.starts_with("http://127.0.0.1:"));
        assert!(prompt.redirect_uri.ends_with(oauth::CALLBACK_PATH));
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
            let visits = match tamper {
                Some(tamper) => tamper(location),
                None => vec![location],
            };
            for visit in visits {
                let _ = http.get(&visit).send().await;
            }
        });
    }
}

async fn login(mock: &MockAs, backend: &CredentialBackend) -> Result<LoginOutcome, McpError> {
    oauth::login(
        LoginOptions::new(&mock.url, backend.clone()).with_timeout(Duration::from_secs(20)),
        browser(None),
    )
    .await
}

fn options(url: &str) -> ClientOptions {
    ClientOptions::new(url).with_protocol(ProtocolMode::Auto)
}

/// The access token currently stored (compared, never printed).
async fn stored_access_token(backend: &CredentialBackend, url: &str) -> String {
    use oauth2::TokenResponse as _;
    let creds = backend
        .store_for(url)
        .load_credentials()
        .await
        .unwrap()
        .expect("stored login");
    creds
        .token_response
        .expect("token")
        .access_token()
        .secret()
        .clone()
}

fn assert_no_token_values(text: &str) {
    for marker in ["at-", "rt-", "code-"] {
        assert!(
            !text.contains(marker),
            "a token value leaked into output ({marker}…)"
        );
    }
}

// ---------------------------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------------------------

#[tokio::test]
async fn login_registers_runs_pkce_and_stores_in_the_file() {
    let mock = spawn_mock_as(3600).await;
    let backend = file_backend("login-file");
    let outcome = login(&mock, &backend).await.expect("login");

    assert_eq!(mock.state.registrations.load(Ordering::SeqCst), 1);
    assert_eq!(mock.state.code_exchanges.load(Ordering::SeqCst), 1);
    assert_eq!(outcome.key, credential_key(&mock.url));
    assert!(outcome.client_id.starts_with("client-"));
    assert!(outcome.has_refresh_token, "offline_access → refresh token");
    assert_eq!(outcome.expires_in, Some(Duration::from_secs(3600)));
    let asked = mock
        .state
        .last_scope
        .lock()
        .unwrap()
        .clone()
        .unwrap_or_default();
    assert!(
        asked.split(' ').any(|s| s == "offline_access"),
        "scope asked: {asked}"
    );
    assert!(outcome.stored_in.starts_with("file "));
    assert_no_token_values(&format!("{outcome:?}"));

    // The fallback file: one entry under the server URL, 0600, in a 0700 dir.
    let CredentialBackend::File { path } = &backend else {
        unreachable!()
    };
    let file: serde_json::Value = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    let entry = &file["servers"][credential_key(&mock.url)];
    assert_eq!(entry["client_id"], outcome.client_id.as_str());
    assert!(entry["token_response"]["refresh_token"].is_string());
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "credentials file mode {mode:o}");
        let dir_mode = std::fs::metadata(path.parent().unwrap())
            .unwrap()
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(dir_mode, 0o700, "credentials dir mode {dir_mode:o}");
    }

    // The stored login connects and calls tools.
    let client = oauth::connect(options(&mock.url), &backend)
        .await
        .expect("connect with the stored login");
    assert!(
        client.session().auth.starts_with("oauth (file "),
        "{}",
        client.session().auth
    );
    client.call_tool("amp_whoami", None).await.expect("call");
    assert_eq!(client.health(), Health::Connected);
    assert_eq!(mock.state.refreshes.load(Ordering::SeqCst), 0);
    client.close().await;
}

#[tokio::test]
async fn token_close_to_expiry_is_refreshed_before_use() {
    // expires_in below rmcp's 30 s refresh buffer: every use refreshes first.
    let mock = spawn_mock_as(5).await;
    let backend = file_backend("refresh-expiry");
    login(&mock, &backend).await.expect("login");
    let first = stored_access_token(&backend, &mock.url).await;

    let client = oauth::connect(options(&mock.url), &backend)
        .await
        .expect("connect");
    client.call_tool("amp_whoami", None).await.expect("call");
    assert!(mock.state.refreshes.load(Ordering::SeqCst) >= 1);
    // The refreshed token was saved back to the store.
    assert_ne!(stored_access_token(&backend, &mock.url).await, first);
    client.close().await;
}

#[tokio::test]
async fn rejected_token_mid_session_refreshes_and_retries() {
    let mock = spawn_mock_as(3600).await;
    let backend = file_backend("refresh-401");
    login(&mock, &backend).await.expect("login");
    let client = oauth::connect(options(&mock.url), &backend)
        .await
        .expect("connect");
    client
        .call_tool("amp_whoami", None)
        .await
        .expect("first call");
    assert_eq!(mock.state.refreshes.load(Ordering::SeqCst), 0);

    mock.state.revoke_access_tokens();
    let result = client.call_tool("amp_whoami", None).await;
    assert!(result.is_ok(), "call after revocation: {:?}", result.err());
    assert_eq!(mock.state.refreshes.load(Ordering::SeqCst), 1);
    assert!(mock.state.rejected_at_resource.load(Ordering::SeqCst) >= 1);
    assert_eq!(client.health(), Health::Connected);
    client.close().await;
}

#[tokio::test]
async fn refresh_rejected_mid_session_means_relogin() {
    let mock = spawn_mock_as(3600).await;
    let backend = file_backend("relogin");
    login(&mock, &backend).await.expect("login");
    let client = oauth::connect(options(&mock.url), &backend)
        .await
        .expect("connect");
    client
        .call_tool("amp_whoami", None)
        .await
        .expect("first call");

    mock.state.revoke_access_tokens();
    mock.state.reject_refresh.store(true, Ordering::SeqCst);
    let err = client
        .call_tool("amp_whoami", None)
        .await
        .expect_err("refresh rejected");
    assert!(err.is_auth_required(), "{err:?}");
    assert!(err.to_string().contains("amp-harness login"), "{err}");
    assert_eq!(client.health(), Health::AuthRequired);
    assert_eq!(client.health().to_string(), "re-login needed");
    assert_no_token_values(&format!("{err:?}"));
    client.close().await;
}

#[tokio::test]
async fn amp_token_takes_precedence_over_a_stored_login() {
    let mock = spawn_mock_as(5).await; // a stored login would refresh on use
    let backend = file_backend("amp-token");
    login(&mock, &backend).await.expect("login");
    *mock.state.static_token.lock().unwrap() = Some("amp_static_token".to_owned());

    let token = |t: &str| Auth::Static {
        token: StaticToken::new(t).unwrap(),
        env: "AMP_TOKEN".into(),
    };
    let client = oauth::connect(
        options(&mock.url).with_auth(token("amp_static_token")),
        &backend,
    )
    .await
    .expect("connect with AMP_TOKEN");
    assert_eq!(client.session().auth, "static token (env AMP_TOKEN)");
    client.call_tool("amp_whoami", None).await.expect("call");
    // The stored login was not used (no refresh even though its token is about to expire).
    assert_eq!(mock.state.refreshes.load(Ordering::SeqCst), 0);
    client.close().await;

    // A bad AMP_TOKEN is not rescued by the stored login.
    let err = oauth::connect(options(&mock.url).with_auth(token("amp_wrong")), &backend)
        .await
        .expect_err("wrong AMP_TOKEN");
    assert!(err.is_auth_required(), "{err:?}");
}

#[tokio::test]
async fn without_a_login_connect_asks_for_one() {
    let mock = spawn_mock_as(3600).await;
    let backend = file_backend("no-login");
    let err = oauth::connect(options(&mock.url), &backend)
        .await
        .expect_err("no login");
    match &err {
        McpError::AuthRequired { challenge } => assert!(
            challenge
                .as_deref()
                .is_some_and(|c| c.contains("resource_metadata")),
            "{challenge:?}"
        ),
        other => panic!("expected AuthRequired, got {other:?}"),
    }
    assert!(!oauth::has_login(&mock.url, &backend).await.unwrap());
}

#[tokio::test]
async fn logins_are_keyed_by_server_url_and_logout_removes_one() {
    let a = spawn_mock_as(3600).await;
    let b = spawn_mock_as(3600).await;
    let backend = file_backend("keyed");
    login(&a, &backend).await.expect("login a");
    login(&b, &backend).await.expect("login b");
    assert!(oauth::has_login(&a.url, &backend).await.unwrap());
    assert!(oauth::has_login(&b.url, &backend).await.unwrap());

    assert!(oauth::logout(&a.url, &backend).await.unwrap());
    assert!(
        !oauth::logout(&a.url, &backend).await.unwrap(),
        "second logout"
    );
    assert!(!oauth::has_login(&a.url, &backend).await.unwrap());
    assert!(oauth::has_login(&b.url, &backend).await.unwrap(), "b kept");
    let err = oauth::connect(options(&a.url), &backend)
        .await
        .expect_err("logged out");
    assert!(err.is_auth_required(), "{err:?}");
    let client = oauth::connect(options(&b.url), &backend)
        .await
        .expect("b still logged in");
    client.close().await;
}

#[tokio::test]
async fn login_ignores_a_foreign_state_and_reports_a_denial() {
    let mock = spawn_mock_as(3600).await;
    let backend = file_backend("denied");
    // The browser first hits the listener with someone else's state (ignored), then the AS
    // reports access_denied for ours.
    let tamper: fn(String) -> Vec<String> = |location| {
        let (base, query) = location.split_once('?').unwrap();
        let state = query
            .split('&')
            .find_map(|kv| kv.strip_prefix("state="))
            .unwrap()
            .to_owned();
        vec![
            format!("{base}?code=stolen&state=not-ours"),
            format!("{base}/../favicon.ico"),
            format!("{base}?error=access_denied&state={state}"),
        ]
    };
    let err = oauth::login(
        LoginOptions::new(&mock.url, backend.clone()).with_timeout(Duration::from_secs(20)),
        browser(Some(tamper)),
    )
    .await
    .expect_err("denied");
    assert!(
        matches!(&err, McpError::OAuth(m) if m.contains("access_denied")),
        "{err:?}"
    );
    assert_eq!(mock.state.code_exchanges.load(Ordering::SeqCst), 0);
    assert!(!oauth::has_login(&mock.url, &backend).await.unwrap());
}

#[tokio::test]
async fn login_times_out_when_the_browser_never_returns() {
    let mock = spawn_mock_as(3600).await;
    let backend = file_backend("timeout");
    let err = oauth::login(
        LoginOptions::new(&mock.url, backend.clone()).with_timeout(Duration::from_millis(300)),
        |_prompt: &LoginPrompt| {},
    )
    .await
    .expect_err("timeout");
    assert!(
        matches!(&err, McpError::OAuth(m) if m.contains("no answer")),
        "{err:?}"
    );
}

/// How long a stalled-AS test may take before we call it a hang.
const HANG: Duration = Duration::from_secs(10);

#[tokio::test]
async fn connect_with_a_login_fails_fast_when_the_as_metadata_stalls() {
    let mock = spawn_mock_as(3600).await;
    let backend = file_backend("stalled-connect");
    login(&mock, &backend).await.expect("login");
    mock.state.stall_metadata.store(true, Ordering::SeqCst);

    let bound = Duration::from_millis(500);
    let started = std::time::Instant::now();
    let result = tokio::time::timeout(
        HANG,
        oauth::connect(options(&mock.url).with_connect_timeout(bound), &backend),
    )
    .await
    .expect("oauth::connect hung on a stalled authorization server");
    let Err(err) = result else {
        panic!("a stalled AS cannot connect")
    };
    assert!(err.is_timeout(), "{err:?}");
    assert!(
        started.elapsed() < bound * 4,
        "took {:?}",
        started.elapsed()
    );
    assert_no_token_values(&err.to_string());
}

#[tokio::test]
async fn login_fails_fast_when_the_as_metadata_stalls() {
    let mock = spawn_mock_as(3600).await;
    mock.state.stall_metadata.store(true, Ordering::SeqCst);
    let backend = file_backend("stalled-login");

    let bound = Duration::from_millis(500);
    let started = std::time::Instant::now();
    let result = tokio::time::timeout(
        HANG,
        oauth::login(
            LoginOptions::new(&mock.url, backend.clone()).with_request_timeout(bound),
            |_prompt: &LoginPrompt| panic!("no browser when discovery stalls"),
        ),
    )
    .await
    .expect("login hung on a stalled authorization server");
    let err = result.expect_err("a stalled AS cannot log in");
    assert!(matches!(err, McpError::OAuth(_)), "{err:?}");
    assert!(
        started.elapsed() < bound * 4,
        "took {:?}",
        started.elapsed()
    );
    assert_eq!(mock.state.registrations.load(Ordering::SeqCst), 0);
    assert!(!oauth::has_login(&mock.url, &backend).await.unwrap());
}

#[tokio::test]
async fn login_refuses_a_non_http_authorization_endpoint() {
    for endpoint in ["file:///etc/passwd", "amp-evil://authorize"] {
        let mock = spawn_mock_as(3600).await;
        *mock.state.authorization_endpoint.lock().unwrap() = Some(endpoint.to_owned());
        let backend = file_backend("hostile-endpoint");
        let prompted = Arc::new(AtomicBool::new(false));
        let seen = prompted.clone();
        let err = oauth::login(
            LoginOptions::new(&mock.url, backend.clone()).with_timeout(Duration::from_secs(5)),
            move |_prompt: &LoginPrompt| seen.store(true, Ordering::SeqCst),
        )
        .await
        .expect_err(endpoint);
        assert!(
            matches!(&err, McpError::OAuth(m) if m.contains("is not http(s); refusing to open it")),
            "{endpoint}: {err:?}"
        );
        assert!(
            !prompted.load(Ordering::SeqCst),
            "{endpoint}: the prompt (browser) must never be called"
        );
        assert!(!oauth::has_login(&mock.url, &backend).await.unwrap());
    }
}

#[tokio::test]
async fn login_refuses_plain_http_to_a_remote_authorization_endpoint() {
    let mock = spawn_mock_as(3600).await;
    *mock.state.authorization_endpoint.lock().unwrap() =
        Some("http://auth.example.com/authorize".to_owned());
    let backend = file_backend("plain-http-endpoint");
    let err = oauth::login(
        LoginOptions::new(&mock.url, backend.clone()).with_timeout(Duration::from_secs(5)),
        |_prompt: &LoginPrompt| panic!("no browser for a plain-http remote endpoint"),
    )
    .await
    .expect_err("plain http remote");
    assert!(
        matches!(&err, McpError::OAuth(m) if m.contains("refusing to open it")),
        "{err:?}"
    );
}

#[test]
fn open_browser_refuses_file_and_custom_scheme_urls() {
    for url in ["file:///etc/passwd", "amp-evil://authorize"] {
        let err = oauth::open_browser(url).expect_err(url);
        assert!(err.contains("refusing to open it"), "{url}: {err}");
    }
}

#[tokio::test]
async fn login_to_a_server_without_auth_says_so() {
    let server = common::spawn(TestServer::new(ServerKind::Dual)).await;
    let backend = file_backend("auth-off");
    let err = oauth::login(
        LoginOptions::new(&server.url, backend.clone()).with_timeout(Duration::from_secs(5)),
        |_prompt: &LoginPrompt| panic!("no browser for an auth-off server"),
    )
    .await
    .expect_err("auth off");
    assert!(
        matches!(&err, McpError::OAuth(m) if m.contains("auth off")),
        "{err:?}"
    );
    // And connect without a login just works there (compose).
    let client = oauth::connect(options(&server.url), &backend)
        .await
        .expect("connect");
    assert_eq!(client.session().auth, "none");
    client.close().await;
}

#[tokio::test]
async fn a_loosened_credentials_file_is_put_back_to_0600() {
    let mock = spawn_mock_as(3600).await;
    let backend = file_backend("chmod");
    login(&mock, &backend).await.expect("login");
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let CredentialBackend::File { path } = &backend else {
            unreachable!()
        };
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert!(oauth::has_login(&mock.url, &backend).await.unwrap());
        let mode = std::fs::metadata(path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
    }
}

/// The real OS keychain, under a test-only service name; the entry is deleted afterwards even if
/// the test fails. Skipped (with a note) where there is no keychain, e.g. Linux CI without a
/// Secret Service, or when AMP_TEST_SKIP_KEYCHAIN is set.
#[tokio::test]
async fn keychain_store_round_trip_and_login() {
    if std::env::var_os("AMP_TEST_SKIP_KEYCHAIN").is_some() {
        eprintln!("skipped: AMP_TEST_SKIP_KEYCHAIN is set");
        return;
    }
    let service = format!("amp-harness-test-{}", std::process::id());
    if let Err(reason) = amp_mcp::keychain_available(&service) {
        eprintln!("skipped: no usable keychain ({reason})");
        return;
    }
    let backend = CredentialBackend::keychain(service.clone());

    let mock = spawn_mock_as(3600).await;
    struct Cleanup(CredentialBackend, String);
    impl Drop for Cleanup {
        fn drop(&mut self) {
            if let CredentialBackend::Keychain { service } = &self.0
                && let Ok(entry) = keyring::Entry::new(service, &credential_key(&self.1))
            {
                let _ = entry.delete_credential();
            }
        }
    }
    let _cleanup = Cleanup(backend.clone(), mock.url.clone());

    let outcome = login(&mock, &backend)
        .await
        .expect("login into the keychain");
    assert!(
        outcome.stored_in.contains(&service),
        "{}",
        outcome.stored_in
    );

    // Stored in the keychain under (service, server URL).
    let entry = keyring::Entry::new(&service, &credential_key(&mock.url)).unwrap();
    let json = entry.get_password().expect("keychain entry exists");
    let stored: serde_json::Value = serde_json::from_str(&json).unwrap();
    assert_eq!(stored["client_id"], outcome.client_id.as_str());

    let client = oauth::connect(options(&mock.url), &backend)
        .await
        .expect("connect with the keychain login");
    assert!(
        client.session().auth.contains("oauth"),
        "{}",
        client.session().auth
    );
    client.call_tool("amp_whoami", None).await.expect("call");
    client.close().await;

    assert!(oauth::logout(&mock.url, &backend).await.unwrap());
    assert!(matches!(entry.get_password(), Err(keyring::Error::NoEntry)));
    assert!(!oauth::logout(&mock.url, &backend).await.unwrap());
}
