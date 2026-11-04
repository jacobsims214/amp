//! amp-mcp against an in-process rmcp server, in both MCP protocol eras.

mod common;

use std::time::Duration;

use amp_mcp::{
    AmpMcpClient, Auth, ClientOptions, Era, Health, McpError, ProtocolMode, StaticToken, TtlSource,
    rmcp::model::{CacheScope, ProtocolVersion},
};
use common::{INSTRUCTIONS, ServerKind, TestServer, spawn, spawn_with_bearer};

fn args(value: serde_json::Value) -> Option<serde_json::Map<String, serde_json::Value>> {
    match value {
        serde_json::Value::Object(map) => Some(map),
        _ => None,
    }
}

fn text(result: &amp_mcp::rmcp::model::CallToolResult) -> String {
    result
        .content
        .iter()
        .filter_map(|c| c.as_text().map(|t| t.text.clone()))
        .collect()
}

/// Connects, then checks instructions, the catalog and a tool call: the same surface in every era.
async fn connect_and_exercise(
    kind: ServerKind,
    mode: ProtocolMode,
) -> Result<(AmpMcpClient, common::Running), McpError> {
    let server = spawn(TestServer::new(kind)).await;
    let client = AmpMcpClient::connect(ClientOptions::new(&server.url).with_protocol(mode)).await?;

    let session = client.session();
    assert_eq!(session.mode, mode);
    assert_eq!(
        client.instructions().as_deref(),
        Some(INSTRUCTIONS),
        "{kind:?} {mode}"
    );
    assert_eq!(session.server_name.as_deref(), Some("amp-test"));
    assert_eq!(session.server_version.as_deref(), Some("9.9.9"));

    let catalog = client.catalog().await.expect("catalog");
    assert_eq!(
        catalog.names().collect::<Vec<_>>(),
        ["amp_whoami", "amp_echo", "amp_sleep"]
    );
    let result = client
        .call_tool("amp_echo", args(serde_json::json!({"x": 1})))
        .await
        .expect("call");
    assert_eq!(text(&result), r#"{"x":1}"#);
    assert_eq!(client.health(), Health::Connected);
    Ok((client, server))
}

#[tokio::test]
async fn auto_uses_the_stateless_era_on_a_dual_server() {
    let (client, _server) = connect_and_exercise(ServerKind::Dual, ProtocolMode::Auto)
        .await
        .unwrap();
    let s = client.session();
    assert_eq!(s.protocol_version, ProtocolVersion::V_2026_07_28);
    assert_eq!(s.era, Era::Stateless);
    assert!(!s.fell_back);
    client.close().await;
}

#[tokio::test]
async fn forced_2026_07_28_on_a_dual_server() {
    let (client, _server) = connect_and_exercise(ServerKind::Dual, ProtocolMode::Modern)
        .await
        .unwrap();
    assert_eq!(client.protocol_version(), ProtocolVersion::V_2026_07_28);
    assert_eq!(client.session().era, Era::Stateless);
    client.close().await;
}

#[tokio::test]
async fn forced_legacy_on_a_dual_server() {
    let (client, _server) = connect_and_exercise(ServerKind::Dual, ProtocolMode::Legacy)
        .await
        .unwrap();
    let s = client.session();
    assert_eq!(s.protocol_version, ProtocolVersion::V_2025_11_25);
    assert_eq!(s.era, Era::Legacy);
    assert!(!s.fell_back, "forced legacy is not a fallback");
    client.close().await;
}

#[tokio::test]
async fn auto_falls_back_to_legacy_on_an_old_server() {
    let (client, _server) = connect_and_exercise(ServerKind::LegacyOnly, ProtocolMode::Auto)
        .await
        .unwrap();
    let s = client.session();
    assert_eq!(s.protocol_version, ProtocolVersion::V_2025_11_25);
    assert_eq!(s.era, Era::Legacy);
    assert!(s.fell_back);
    client.close().await;
}

#[tokio::test]
async fn auto_falls_back_when_the_server_lists_no_2026_version() {
    let (client, _server) = connect_and_exercise(ServerKind::NoModernVersion, ProtocolMode::Auto)
        .await
        .unwrap();
    let s = client.session();
    assert_eq!(s.protocol_version, ProtocolVersion::V_2025_11_25);
    assert_eq!(s.era, Era::Legacy);
    assert!(s.fell_back);
    client.close().await;
}

#[tokio::test]
async fn forced_legacy_on_an_old_server() {
    let (client, _server) = connect_and_exercise(ServerKind::LegacyOnly, ProtocolMode::Legacy)
        .await
        .unwrap();
    assert_eq!(client.session().era, Era::Legacy);
    client.close().await;
}

#[tokio::test]
async fn forced_2026_07_28_fails_on_an_old_server() {
    for kind in [ServerKind::LegacyOnly, ServerKind::NoModernVersion] {
        let server = spawn(TestServer::new(kind)).await;
        let err = AmpMcpClient::connect(
            ClientOptions::new(&server.url).with_protocol(ProtocolMode::Modern),
        )
        .await
        .expect_err("an old server cannot speak 2026-07-28");
        assert!(matches!(err, McpError::Connect { .. }), "{kind:?}: {err:?}");
    }
}

#[tokio::test]
async fn catalog_honours_the_cache_hint() {
    // 5-minute hint (what AMP sends): the second read is served from the catalog.
    let server = spawn(TestServer::new(ServerKind::Dual)).await;
    let client = AmpMcpClient::connect(ClientOptions::new(&server.url))
        .await
        .unwrap();
    let first = client.catalog().await.unwrap();
    assert_eq!(first.ttl(), Duration::from_secs(300));
    assert_eq!(first.ttl_source(), TtlSource::Server);
    assert_eq!(first.cache_scope(), Some(CacheScope::Private));
    assert!(first.get("amp_whoami").is_some());
    client.catalog().await.unwrap();
    assert_eq!(server.counters.list_tools(), 1);
    // A forced refresh ignores the hint.
    client.refresh_catalog().await.unwrap();
    assert_eq!(server.counters.list_tools(), 2);
    client.close().await;

    // A short hint expires.
    let server = spawn(TestServer::new(ServerKind::Dual).with_tools_ttl_ms(Some(100))).await;
    let client = AmpMcpClient::connect(ClientOptions::new(&server.url))
        .await
        .unwrap();
    client.catalog().await.unwrap();
    client.catalog().await.unwrap();
    assert_eq!(server.counters.list_tools(), 1);
    tokio::time::sleep(Duration::from_millis(150)).await;
    client.catalog().await.unwrap();
    assert_eq!(server.counters.list_tools(), 2);
    client.close().await;
}

#[tokio::test]
async fn catalog_without_a_hint_uses_the_default_ttl() {
    // 2026-07-28 requires `ttlMs`; rmcp's server sends 0 when the handler sets none: always stale.
    let server = spawn(TestServer::new(ServerKind::Dual).with_tools_ttl_ms(None)).await;
    let client = AmpMcpClient::connect(ClientOptions::new(&server.url))
        .await
        .unwrap();
    let catalog = client.catalog().await.unwrap();
    assert_eq!(catalog.ttl(), Duration::ZERO);
    client.catalog().await.unwrap();
    assert_eq!(server.counters.list_tools(), 2);
    client.close().await;

    // The legacy era carries no hint: the client default applies. Zero: every read refetches.
    let server = spawn(TestServer::new(ServerKind::Dual).with_tools_ttl_ms(None)).await;
    let legacy = || ClientOptions::new(&server.url).with_protocol(ProtocolMode::Legacy);
    let client = AmpMcpClient::connect(legacy()).await.unwrap();
    let catalog = client.catalog().await.unwrap();
    assert_eq!(catalog.ttl_source(), TtlSource::Default);
    client.catalog().await.unwrap();
    assert_eq!(server.counters.list_tools(), 2);
    client.close().await;

    // A configured default is honoured.
    let client = AmpMcpClient::connect(legacy().with_catalog_default_ttl(Duration::from_secs(60)))
        .await
        .unwrap();
    let catalog = client.catalog().await.unwrap();
    assert_eq!(catalog.ttl_source(), TtlSource::Default);
    assert_eq!(catalog.ttl(), Duration::from_secs(60));
    client.catalog().await.unwrap();
    assert_eq!(server.counters.list_tools(), 3);
    client.close().await;
}

#[tokio::test]
async fn call_timeout_is_an_error_and_degrades_health() {
    for mode in [ProtocolMode::Auto, ProtocolMode::Legacy] {
        let server = spawn(TestServer::new(ServerKind::Dual)).await;
        let client = AmpMcpClient::connect(
            ClientOptions::new(&server.url)
                .with_protocol(mode)
                .with_call_timeout(Duration::from_millis(200)),
        )
        .await
        .unwrap();
        let mut health = client.watch_health();

        let started = std::time::Instant::now();
        let err = client
            .call_tool("amp_sleep", args(serde_json::json!({"ms": 5_000})))
            .await
            .expect_err("must time out");
        assert!(started.elapsed() < Duration::from_secs(3), "{mode}");
        assert!(err.is_timeout(), "{mode}: {err:?}");
        assert!(
            matches!(&err, McpError::Timeout { operation, after }
                if operation == "tools/call amp_sleep" && *after == Duration::from_millis(200)),
            "{err:?}"
        );
        assert!(matches!(client.health(), Health::Degraded { .. }), "{mode}");
        assert!(health.has_changed().unwrap());
        health.borrow_and_update();

        // The connection still works, and health recovers.
        let ok = client
            .call_tool("amp_sleep", args(serde_json::json!({"ms": 10})))
            .await
            .unwrap();
        assert_eq!(text(&ok), "slept");
        assert_eq!(client.health(), Health::Connected);
        assert!(health.has_changed().unwrap());
        client.close().await;
    }
}

#[tokio::test]
async fn default_call_timeout_is_120s() {
    let opts = ClientOptions::new("http://127.0.0.1:1/mcp");
    assert_eq!(opts.call_timeout, Duration::from_secs(120));
    assert_eq!(amp_mcp::DEFAULT_CALL_TIMEOUT, Duration::from_secs(120));
}

#[tokio::test]
async fn tool_errors_are_results_not_failures() {
    let server = spawn(TestServer::new(ServerKind::Dual)).await;
    let client = AmpMcpClient::connect(ClientOptions::new(&server.url))
        .await
        .unwrap();
    let result = client.call_tool("amp_fail", None).await.unwrap();
    assert_eq!(result.is_error, Some(true));
    assert_eq!(client.health(), Health::Connected);
    // A JSON-RPC error from the server is an Err, but the connection is fine afterwards.
    let err = client.call_tool("amp_nope", None).await.unwrap_err();
    assert!(matches!(err, McpError::Service { .. }), "{err:?}");
    assert!(client.call_tool("amp_whoami", None).await.is_ok());
    client.close().await;
}

#[tokio::test]
async fn static_bearer_token() {
    for mode in [ProtocolMode::Auto, ProtocolMode::Legacy] {
        let server =
            spawn_with_bearer(TestServer::new(ServerKind::Dual), Some("amp_test_token")).await;

        // No token: authorization required, with the challenge for the OAuth flow.
        let err = AmpMcpClient::connect(ClientOptions::new(&server.url).with_protocol(mode))
            .await
            .expect_err("no token");
        match &err {
            McpError::AuthRequired { challenge } => assert!(
                challenge
                    .as_deref()
                    .is_some_and(|c| c.contains("resource_metadata")),
                "{mode}: {challenge:?}"
            ),
            other => panic!("{mode}: expected AuthRequired, got {other:?}"),
        }

        // Wrong token: same.
        let wrong = Auth::Static {
            token: StaticToken::new("amp_wrong").unwrap(),
            env: "AMP_TOKEN".into(),
        };
        let err = AmpMcpClient::connect(
            ClientOptions::new(&server.url)
                .with_protocol(mode)
                .with_auth(wrong),
        )
        .await
        .expect_err("wrong token");
        assert!(err.is_auth_required(), "{mode}: {err:?}");

        // Right token: connected, and every request carries it.
        let right = Auth::Static {
            token: StaticToken::new("amp_test_token").unwrap(),
            env: "AMP_TOKEN".into(),
        };
        let client = AmpMcpClient::connect(
            ClientOptions::new(&server.url)
                .with_protocol(mode)
                .with_auth(right),
        )
        .await
        .unwrap();
        assert_eq!(client.catalog().await.unwrap().len(), 3);
        assert!(client.call_tool("amp_whoami", None).await.is_ok());
        assert!(!format!("{client:?}").contains("amp_test_token"));
        client.close().await;
    }
}

#[tokio::test]
async fn unreachable_server_is_a_connect_error() {
    // Bind then drop, so nothing listens on the port.
    let port = {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        l.local_addr().unwrap().port()
    };
    let err = AmpMcpClient::connect(
        ClientOptions::new(format!("http://127.0.0.1:{port}/mcp"))
            .with_connect_timeout(Duration::from_secs(5)),
    )
    .await
    .expect_err("nothing listening");
    assert!(
        matches!(
            err,
            McpError::Connect { .. } | McpError::ConnectTimeout { .. }
        ),
        "{err:?}"
    );
}

#[tokio::test]
async fn closed_client_reports_disconnected() {
    let server = spawn(TestServer::new(ServerKind::Dual)).await;
    let client = AmpMcpClient::connect(ClientOptions::new(&server.url))
        .await
        .unwrap();
    let peer = client.peer().clone();
    client.close().await;
    assert!(peer.is_transport_closed());
}
