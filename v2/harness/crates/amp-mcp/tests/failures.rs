//! #2178: every way of failing to reach AMP has a [`FailureKind`] and a short plain reason with no
//! Rust type names (rmcp's own text is `Send message error Transport [rmcp::…<reqwest::…>]`).

mod common;

use std::time::Duration;

use amp_mcp::{AmpMcpClient, ClientOptions, FailureKind, McpError};
use common::{ServerKind, TestServer, spawn_with_bearer};

async fn connect(url: &str) -> McpError {
    connect_with(ClientOptions::new(url)).await
}

async fn connect_with(options: ClientOptions) -> McpError {
    match AmpMcpClient::connect(options).await {
        Ok(_) => panic!("connected, but it should have failed"),
        Err(e) => e,
    }
}

/// A loopback port nothing listens on.
async fn closed_port() -> u16 {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    listener.local_addr().unwrap().port()
}

/// Serves `app` on a loopback port; returns its base URL.
async fn serve(app: axum::Router) -> String {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    format!("http://{addr}")
}

fn assert_plain(e: &McpError, kind: FailureKind, reason: &str) {
    assert_eq!(e.kind(), kind, "{e}");
    let plain = e.reason();
    assert!(plain.contains(reason), "{plain:?} lacks {reason:?} ({e})");
    for raw in ["::", "rmcp", "reqwest", "Transport [", "os error"] {
        assert!(!plain.contains(raw), "{plain:?} contains {raw:?}");
    }
}

#[tokio::test]
async fn a_closed_port_is_connection_refused() {
    let port = closed_port().await;
    let e = connect(&format!("http://127.0.0.1:{port}/mcp")).await;
    assert_plain(&e, FailureKind::Refused, "connection refused");
    // The chain is still there for --verbose.
    assert!(e.to_string().to_lowercase().contains("refused"), "{e}");
}

#[tokio::test]
async fn an_unknown_host_is_host_not_found() {
    let e = connect("http://amp-2178.invalid/mcp").await;
    assert_plain(&e, FailureKind::HostNotFound, "host not found");
}

#[tokio::test]
async fn a_401_is_login_required() {
    let server = spawn_with_bearer(TestServer::new(ServerKind::Dual), Some("secret")).await;
    let e = connect(&server.url).await;
    assert_plain(&e, FailureKind::LoginRequired, "login required");
}

#[tokio::test]
async fn a_wrong_path_is_not_found() {
    let base = serve(axum::Router::new().route("/", axum::routing::get(|| async { "hi" }))).await;
    let e = connect(&format!("{base}/nope")).await;
    assert_plain(&e, FailureKind::NotFound, "HTTP 404");
}

#[tokio::test]
async fn a_server_that_is_not_mcp_says_so() {
    let base = serve(axum::Router::new().route(
        "/mcp",
        axum::routing::post(|| async {
            (
                axum::http::StatusCode::NOT_IMPLEMENTED,
                "<html>not here</html>",
            )
        }),
    ))
    .await;
    let e = connect(&format!("{base}/mcp")).await;
    assert_plain(&e, FailureKind::NotMcp, "HTTP 501");
}

#[tokio::test]
async fn tls_to_a_plain_http_port_is_tls() {
    let server = spawn_with_bearer(TestServer::new(ServerKind::Dual), None).await;
    let url = server.url.replace("http://", "https://");
    let e = connect(&url).await;
    assert_plain(&e, FailureKind::Tls, "TLS failed");
}

#[tokio::test]
async fn a_server_that_never_answers_times_out() {
    // Accepts connections and never answers.
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        let mut held = Vec::new();
        while let Ok((socket, _)) = listener.accept().await {
            held.push(socket);
        }
    });
    let e = connect_with(
        ClientOptions::new(format!("http://{addr}/mcp"))
            .with_connect_timeout(Duration::from_millis(300)),
    )
    .await;
    assert_plain(&e, FailureKind::Timeout, "no answer within");
}

/// The chat's offline start: the client reads as disconnected with the reason, calls fail as
/// Closed (no hang), and `replace` brings it online.
#[tokio::test]
async fn an_offline_client_fails_fast_and_can_be_brought_online() {
    let server = spawn_with_bearer(TestServer::new(ServerKind::Dual), None).await;
    let options = ClientOptions::new(&server.url);
    let client = AmpMcpClient::offline(&options, "connection refused");
    assert!(!client.is_online());
    assert!(client.is_closed());
    assert_eq!(
        client.health().to_string(),
        "disconnected: connection refused"
    );
    assert_eq!(client.session().url, server.url);
    let e = tokio::time::timeout(Duration::from_secs(5), client.catalog())
        .await
        .expect("an offline call fails at once")
        .unwrap_err();
    assert!(matches!(e, McpError::Closed), "{e}");

    client.replace(AmpMcpClient::connect(options).await.unwrap());
    assert!(client.is_online());
    assert!(client.health().is_connected());
    assert!(client.catalog().await.unwrap().names().count() > 0);
}
