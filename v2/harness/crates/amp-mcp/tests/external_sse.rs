//! Third-party MCP servers over legacy HTTP+SSE (#2238, H5): `type = "sse"` explicitly, and
//! `type = "http"` falling back to it when the Streamable HTTP POST is refused, against a mock
//! 2024-11-05 server (endpoint event, POSTed messages, replies on the stream).
//!
//! Secret values are compared inside the mock but never printed: assertions show names, counts and
//! the absence of a planted marker.

mod common;

use std::{
    path::{Path, PathBuf},
    sync::{Arc, atomic::Ordering},
    time::Duration,
};

use amp_mcp::{
    CredentialBackend, Era, ExternalClient, HeaderSource, HttpHeader, HttpServerOptions, McpError,
    ProtocolMode,
    external::{
        http::{self, wants_sse_fallback},
        sse,
    },
    rmcp::model::ContentBlock,
};
use axum::{
    Router,
    extract::Request,
    http::{HeaderValue, StatusCode, header},
    middleware::{self, Next},
    response::IntoResponse,
    routing::get,
};
use common::legacy_sse::{self, Endpoint, LegacyState};

/// Planted in every secret value, so a leak into an error or Debug output is caught.
const MARK: &str = "S3CRET-MARK";

struct Spawned {
    base: String,
    task: tokio::task::JoinHandle<()>,
}

impl Spawned {
    fn url(&self, path: &str) -> String {
        format!("{}{path}", self.base)
    }
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
        base: format!("http://{addr}"),
        task,
    }
}

/// `app` behind a check that every request (GET and POST alike) carries `X-Api-Key` and the
/// bearer token; a 401 otherwise, with a challenge.
fn requiring_credentials(app: Router, key: String, bearer: String) -> Router {
    let (key, bearer) = (Arc::new(key), Arc::new(format!("Bearer {bearer}")));
    app.layer(middleware::from_fn(move |req: Request, next: Next| {
        let (key, bearer) = (key.clone(), bearer.clone());
        async move {
            let ok = {
                let header = |name| req.headers().get(name).and_then(|v| v.to_str().ok());
                header("x-api-key") == Some(key.as_str())
                    && header("authorization") == Some(bearer.as_str())
                    && header("x-org") == Some("simstech")
            };
            if ok {
                return next.run(req).await;
            }
            let mut response = (StatusCode::UNAUTHORIZED, "no").into_response();
            response.headers_mut().insert(
                header::WWW_AUTHENTICATE,
                HeaderValue::from_static(r#"Bearer realm="legacy""#),
            );
            response
        }
    }))
}

fn options(url: &str) -> HttpServerOptions {
    let mut options = HttpServerOptions::new("legacy", url);
    options.external.connect_timeout = Duration::from_secs(15);
    options.external.call_timeout = Duration::from_secs(15);
    options
}

fn with_credentials(options: HttpServerOptions, key: &str, bearer: &str) -> HttpServerOptions {
    options
        .with_header(HttpHeader::new("X-Org", "simstech", HeaderSource::Config))
        .with_header(HttpHeader::new("X-Api-Key", key, HeaderSource::Keychain))
        .with_bearer(bearer, "env LEGACY_TOKEN")
}

/// A per-test directory under cargo's target dir (never /tmp: D13).
fn scratch(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-mcp-external-sse")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

fn backend(name: &str) -> CredentialBackend {
    CredentialBackend::file(scratch(name).join("credentials.json"))
}

async fn echo(client: &ExternalClient) -> String {
    let tools = client.list_tools().await.expect("tools/list");
    let names: Vec<&str> = tools.iter().map(|t| t.name.as_ref()).collect();
    assert_eq!(names, ["echo"]);
    let mut args = serde_json::Map::new();
    args.insert("text".into(), "hello".into());
    let result = client
        .call_tool("echo", Some(args))
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

fn assert_legacy(client: &ExternalClient, fell_back: bool) {
    let info = client.info();
    assert_eq!(info.transport, "sse");
    assert_eq!(info.protocol_version.as_str(), "2024-11-05");
    assert_eq!(info.era(), Era::Legacy);
    assert_eq!(info.server_name.as_deref(), Some("legacy-sse"));
    assert_eq!(info.server_version.as_deref(), Some("0.9.0"));
    assert_eq!(info.instructions.as_deref(), Some("A legacy SSE server."));
    assert_eq!(info.fell_back, fell_back);
}

async fn wait_until(what: &str, mut done: impl FnMut() -> bool) {
    for _ in 0..100 {
        if done() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    panic!("timed out waiting for {what}");
}

#[tokio::test]
async fn explicit_sse_with_headers_and_bearer() {
    let state = LegacyState::new();
    let key = format!("key-{MARK}");
    let token = format!("tok-{MARK}");
    let server = serve(requiring_credentials(
        legacy_sse::router(state.clone(), "/sse"),
        key.clone(),
        token.clone(),
    ))
    .await;
    let opts = with_credentials(options(&server.url("/sse")), &key, &token).with_legacy_sse();
    assert_no_secret(&format!("{opts:?}"));

    let client = http::connect(&opts, &backend("explicit"))
        .await
        .expect("connect");
    assert_legacy(&client, true);
    assert_eq!(
        client.info().auth,
        "header X-Api-Key (keychain), bearer (env LEGACY_TOKEN)"
    );
    assert_no_secret(&format!("{client:?}"));
    assert_eq!(echo(&client).await, "echo: hello");
    // The Streamable HTTP POST was never tried: one GET, and every POST reached the session.
    assert_eq!(state.gets.load(Ordering::SeqCst), 1);
    assert_eq!(
        *state.methods.lock().unwrap(),
        [
            "initialize",
            "notifications/initialized",
            "tools/list",
            "tools/call"
        ]
    );

    // Server-sent notifications arrive on the stream.
    assert!(!client.take_tools_changed());
    state.notify("notifications/tools/list_changed");
    wait_until("tools/list_changed", || client.take_tools_changed()).await;

    // An unknown tool is the server's JSON-RPC error; the connection stays up.
    let err = client.call_tool("nope", None).await.expect_err("unknown");
    assert!(err.is_server_error(), "{err}");
    assert!(!client.is_closed());

    client.shutdown().await;

    // `protocol = "legacy"` (and sse::connect directly) work the same way.
    let mut legacy = opts.clone();
    legacy.external.protocol = ProtocolMode::Legacy;
    let client = sse::connect(&legacy, &backend("explicit-legacy"))
        .await
        .expect("sse::connect");
    assert_legacy(&client, false);
    client.shutdown().await;
}

#[tokio::test]
async fn http_falls_back_to_sse_when_the_post_is_refused() {
    let state = LegacyState::new();
    let key = format!("key-{MARK}");
    let token = format!("tok-{MARK}");
    let server = serve(requiring_credentials(
        legacy_sse::router(state.clone(), "/sse"),
        key.clone(),
        token.clone(),
    ))
    .await;
    // type = "http" at the SSE URL: POST /sse is 405, so the client retries as legacy SSE with the
    // same headers and bearer.
    let opts = with_credentials(options(&server.url("/sse")), &key, &token);
    assert!(opts.fallback_sse && !opts.legacy_sse);
    let client = http::connect(&opts, &backend("fallback"))
        .await
        .expect("fallback");
    assert_legacy(&client, true);
    assert_eq!(echo(&client).await, "echo: hello");
    client.shutdown().await;

    // fallback_sse = false: the Streamable HTTP error stands and SSE is never tried.
    let gets = state.gets.load(Ordering::SeqCst);
    let mut no_fallback = opts.clone();
    no_fallback.fallback_sse = false;
    let err = http::connect(&no_fallback, &backend("no-fallback"))
        .await
        .expect_err("no fallback");
    assert!(wants_sse_fallback(&err), "{err}");
    assert_eq!(state.gets.load(Ordering::SeqCst), gets);
    assert_no_secret(&err.to_string());
}

#[tokio::test]
async fn a_server_that_speaks_neither_reports_the_streamable_http_error() {
    // POST is 405 and GET is plain text: not SSE either.
    let app = Router::new().route("/mcp", get(|| async { "hello" }));
    let server = serve(app).await;
    let err = http::connect(&options(&server.url("/mcp")), &backend("neither"))
        .await
        .expect_err("neither");
    assert!(wants_sse_fallback(&err), "the Streamable HTTP error: {err}");
    assert!(!err.to_string().contains("legacy SSE"), "{err}");

    // Asked for SSE explicitly, the SSE error says what was wrong.
    let err = http::connect(
        &options(&server.url("/mcp")).with_legacy_sse(),
        &backend("neither-sse"),
    )
    .await
    .expect_err("not sse");
    assert!(err.to_string().contains("not an event stream"), "{err}");

    // A 404 for both.
    let err = http::connect(
        &options(&server.url("/missing")).with_legacy_sse(),
        &backend("missing"),
    )
    .await
    .expect_err("404");
    assert!(err.to_string().contains("HTTP 404"), "{err}");
}

#[tokio::test]
async fn rejections_are_server_auth_with_the_challenge() {
    let state = LegacyState::new();
    let server = serve(requiring_credentials(
        legacy_sse::router(state.clone(), "/sse"),
        format!("key-{MARK}"),
        format!("tok-{MARK}"),
    ))
    .await;
    let wrong = with_credentials(
        options(&server.url("/sse")),
        &format!("wrong-{MARK}"),
        &format!("tok-{MARK}"),
    );
    for opts in [wrong.clone().with_legacy_sse(), wrong] {
        let err = http::connect(&opts, &backend("rejected"))
            .await
            .expect_err("rejected");
        let McpError::ServerAuth {
            server, challenge, ..
        } = &err
        else {
            panic!("{err:?}");
        };
        assert_eq!(server, "legacy");
        assert_eq!(challenge.as_deref(), Some(r#"Bearer realm="legacy""#));
        assert!(
            err.to_string().contains(
                "rejected the configured credentials (header X-Api-Key (keychain), bearer (env LEGACY_TOKEN))"
            ),
            "{err}"
        );
        assert_no_secret(&err.to_string());
        assert_no_secret(&format!("{err:?}"));
    }
    assert_eq!(state.gets.load(Ordering::SeqCst), 0);

    // No credentials at all: "log in".
    let err = http::connect(
        &options(&server.url("/sse")).with_legacy_sse(),
        &backend("anonymous"),
    )
    .await
    .expect_err("anonymous");
    assert!(
        err.to_string().contains("amp-harness mcp login legacy"),
        "{err}"
    );
}

#[tokio::test]
async fn the_endpoint_must_be_on_the_same_origin() {
    let state = LegacyState::new();
    let server = serve(legacy_sse::router(state.clone(), "/sse")).await;
    state.set_endpoint(Endpoint::Literal(
        "http://evil.example/messages?sessionId=x".into(),
    ));
    let opts = options(&server.url("/sse"));
    for opts in [opts.clone().with_legacy_sse(), opts] {
        let err = http::connect(&opts, &backend("foreign"))
            .await
            .expect_err("foreign endpoint");
        assert!(err.to_string().contains("another origin"), "{err}");
    }
    assert_eq!(state.posts.load(Ordering::SeqCst), 0);

    // An absolute URL on the same origin is fine.
    state.set_endpoint(Endpoint::Literal(server.url("/messages?sessionId=s9")));
    // (s9 is not a session the mock knows: the POST is 404, which ends the handshake.)
    let err = http::connect(
        &options(&server.url("/sse")).with_legacy_sse(),
        &backend("same-origin"),
    )
    .await
    .expect_err("unknown session");
    assert!(err.to_string().contains("HTTP 404"), "{err}");
    assert_eq!(state.posts.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn a_stream_without_an_endpoint_event_times_out() {
    let state = LegacyState::new();
    state.set_endpoint(Endpoint::Never);
    let server = serve(legacy_sse::router(state.clone(), "/sse")).await;
    let mut opts = options(&server.url("/sse")).with_legacy_sse();
    opts.external.connect_timeout = Duration::from_secs(2);
    let started = std::time::Instant::now();
    let err = http::connect(&opts, &backend("silent"))
        .await
        .expect_err("silent");
    assert!(err.is_timeout(), "{err}");
    assert!(started.elapsed() < Duration::from_secs(10));
}

#[tokio::test]
async fn the_client_closes_when_the_stream_ends() {
    let state = LegacyState::new();
    let server = serve(legacy_sse::router(state.clone(), "/sse")).await;
    let client = http::connect(&options(&server.url("/sse")), &backend("closing"))
        .await
        .expect("connect");
    assert_eq!(echo(&client).await, "echo: hello");
    state.close_all();
    wait_until("the client to notice", || client.is_closed()).await;
    let err = client.list_tools().await.expect_err("closed");
    assert!(matches!(err, McpError::Closed), "{err:?}");
}
