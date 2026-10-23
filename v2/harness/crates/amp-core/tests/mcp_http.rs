//! `[mcp.<name>]` http servers end to end (#2237, H4): config -> resolve -> `mcp_http::http_options`
//! (secrets from a test-only keychain service and the env) -> `external::http::connect` -> tools.
//!
//! The keychain test uses service `amp-harness-test-h4-<pid>`, deletes its item in a Drop guard,
//! and is skipped where there is no keychain (Linux CI) or AMP_TEST_SKIP_KEYCHAIN is set.

use std::{path::Path, sync::Arc, time::Duration};

/// amp-mcp's mock legacy HTTP+SSE server (#2238).
#[allow(dead_code, reason = "shared with amp-mcp's tests")]
#[path = "../../amp-mcp/tests/common/legacy_sse.rs"]
mod legacy_sse;

use amp_core::{
    mcp_config::{ExpandContext, McpConfig, Resolved, header_account},
    mcp_http::http_options,
};
use amp_mcp::{
    CredentialBackend,
    external::http,
    rmcp::{
        ErrorData, RoleServer, ServerHandler,
        model::{
            CallToolRequestParams, CallToolResponse, CallToolResult, ContentBlock, Implementation,
            ListToolsResult, PaginatedRequestParams, ServerCapabilities, ServerConfig, Tool,
        },
        service::RequestContext,
        transport::streamable_http_server::{
            StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
        },
    },
};
use axum::{
    extract::Request,
    http::StatusCode,
    middleware::{self, Next},
    response::IntoResponse,
};

/// Planted in every secret, so a leak into an error or Debug output is caught.
const MARK: &str = "S3CRET-MARK";

#[derive(Clone)]
struct Pong;

impl ServerHandler for Pong {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
            .with_server_info(Implementation::new("pong", "1.0.0"))
    }

    async fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        _context: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        let serde_json::Value::Object(schema) = serde_json::json!({"type": "object"}) else {
            unreachable!()
        };
        Ok(ListToolsResult::with_all_items(vec![Tool::new(
            "ping",
            "Answers pong.",
            Arc::new(schema),
        )]))
    }

    async fn call_tool(
        &self,
        _request: CallToolRequestParams,
        _context: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        Ok(CallToolResult::success(vec![ContentBlock::text("pong")]).into())
    }
}

struct Server {
    url: String,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Server {
    fn drop(&mut self) {
        self.task.abort();
    }
}

/// The pong server, answering 401 unless every `(header, value)` is present.
async fn spawn(required: Vec<(&'static str, String)>) -> Server {
    let service: StreamableHttpService<Pong, LocalSessionManager> = StreamableHttpService::new(
        || Ok(Pong),
        Default::default(),
        StreamableHttpServerConfig::default().with_json_response(true),
    );
    let required = Arc::new(required);
    let app = axum::Router::new()
        .nest_service("/mcp", service)
        .layer(middleware::from_fn(move |req: Request, next: Next| {
            let required = required.clone();
            async move {
                let ok = required.iter().all(|(name, value)| {
                    req.headers().get(*name).and_then(|v| v.to_str().ok()) == Some(value.as_str())
                });
                if ok {
                    next.run(req).await
                } else {
                    (StatusCode::UNAUTHORIZED, "no").into_response()
                }
            }
        }));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Server {
        url: format!("http://{addr}/mcp"),
        task,
    }
}

const USER_FILE: &str = "/home/u/.config/amp-harness/config.toml";

/// `[mcp.acme]` from `body`, resolved.
fn resolve(
    body: &str,
    env: &dyn Fn(&str) -> Option<String>,
) -> (
    amp_core::mcp_config::McpServer,
    amp_core::mcp_config::ResolvedRemote,
) {
    let table: toml::Table = toml::from_str(&format!("[acme]\n{body}")).unwrap();
    let config = McpConfig::from_tables(Some((&table, Path::new(USER_FILE))), None);
    assert!(config.errors().next().is_none(), "{:?}", config.problems);
    let server = config.servers["acme"].clone();
    let ctx = ExpandContext {
        env,
        repo: None,
        worktree: None,
        home: None,
        config_dir: None,
    };
    match server.resolve(&ctx).unwrap() {
        Resolved::Http(remote) | Resolved::Sse(remote) => (server, remote),
        other => panic!("{other:?}"),
    }
}

fn scratch_backend(name: &str) -> CredentialBackend {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-core-mcp-http")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    CredentialBackend::file(dir.join("credentials.json"))
}

async fn ping(options: &amp_mcp::HttpServerOptions, backend: &CredentialBackend) -> String {
    let client = http::connect(options, backend).await.expect("connect");
    let tools = client.list_tools().await.unwrap();
    assert_eq!(tools[0].name, "ping");
    let result = client.call_tool("ping", None).await.unwrap();
    client.shutdown().await;
    match &result.content[0] {
        ContentBlock::Text(t) => t.text.clone(),
        other => panic!("{other:?}"),
    }
}

#[tokio::test]
async fn bearer_env_and_env_headers_connect_and_call() {
    let token = format!("{MARK}-bearer");
    let trace = "trace-1".to_owned();
    let server = spawn(vec![
        ("authorization", format!("Bearer {token}")),
        ("x-trace", trace.clone()),
        ("x-org", "simstech".into()),
    ])
    .await;
    let env = {
        let (token, trace) = (token.clone(), trace.clone());
        move |k: &str| match k {
            "ACME_TOKEN" => Some(token.clone()),
            "TRACE_ID" => Some(trace.clone()),
            _ => None,
        }
    };
    let (server_cfg, remote) = resolve(
        &format!(
            "url = \"{}\"\nheaders = {{ \"X-Org\" = \"simstech\", \"X-Trace\" = \"${{env:TRACE_ID}}\" }}\nbearer_env = \"ACME_TOKEN\"\nstartup_timeout_secs = 10\ncall_timeout_secs = 10\nprotocol = \"2025-11-25\"",
            server.url
        ),
        &env,
    );
    // Keychain off: fine, nothing here lives there.
    let options = http_options(&server_cfg, &remote, None, &env).expect("options");
    assert_eq!(options.external.connect_timeout, Duration::from_secs(10));
    assert_eq!(options.external.protocol, amp_mcp::ProtocolMode::Legacy);
    assert!(!format!("{options:?}").contains(MARK));
    assert_eq!(ping(&options, &scratch_backend("env")).await, "pong");

    // Unset bearer variable: an error naming the variable.
    let (server_cfg, remote) = resolve(
        &format!("url = \"{}\"\nbearer_env = \"ACME_TOKEN\"", server.url),
        &env,
    );
    let err = http_options(&server_cfg, &remote, None, &|_: &str| None).unwrap_err();
    assert!(err.contains("ACME_TOKEN is not set"), "{err}");
}

#[tokio::test]
async fn keychain_secrets_need_the_keychain() {
    let (server, remote) = resolve(
        "url = \"https://mcp.example.com/mcp\"\nsecret_headers = [\"X-Api-Key\"]\noauth = { client_id = \"abc\", client_secret = true }",
        &|_: &str| None,
    );
    let err = http_options(&server, &remote, None, &|_: &str| None).unwrap_err();
    assert!(err.contains("AMP_CREDENTIAL_STORE=file"), "{err}");
    assert!(err.contains("mcp:acme:header:x-api-key"), "{err}");
}

/// Removes the test keychain items even when the test fails.
struct KeychainGuard {
    service: String,
    accounts: Vec<String>,
}

impl Drop for KeychainGuard {
    fn drop(&mut self) {
        for account in &self.accounts {
            let _ = amp_mcp::keychain_delete(&self.service, account);
        }
    }
}

#[tokio::test]
async fn secret_headers_come_from_the_keychain() {
    if std::env::var_os("AMP_TEST_SKIP_KEYCHAIN").is_some() {
        eprintln!("skipped: AMP_TEST_SKIP_KEYCHAIN is set");
        return;
    }
    let service = format!("amp-harness-test-h4-{}", std::process::id());
    if let Err(reason) = amp_mcp::keychain_available(&service) {
        eprintln!("skipped: no usable keychain ({reason})");
        return;
    }
    let account = header_account("acme", "X-Api-Key");
    let _guard = KeychainGuard {
        service: service.clone(),
        accounts: vec![account.clone()],
    };
    let key = format!("{MARK}-api-key");
    let server = spawn(vec![("x-api-key", key.clone())]).await;
    let (server_cfg, remote) = resolve(
        &format!(
            "url = \"{}\"\nsecret_headers = [\"X-Api-Key\"]\nstartup_timeout_secs = 10",
            server.url
        ),
        &|_: &str| None,
    );

    // Not stored yet: an error naming the item.
    let err = http_options(&server_cfg, &remote, Some(&service), &|_: &str| None).unwrap_err();
    assert!(err.contains(&account) && err.contains("missing"), "{err}");

    amp_mcp::keychain_set(&service, &account, &key).expect("store the test secret");
    let options =
        http_options(&server_cfg, &remote, Some(&service), &|_: &str| None).expect("options");
    assert!(!format!("{options:?}").contains(MARK));
    assert_eq!(ping(&options, &scratch_backend("keychain")).await, "pong");
}

#[tokio::test]
async fn sse_servers_connect_explicitly_and_by_fallback() {
    // #2238: a legacy HTTP+SSE server at /sse, behind a bearer.
    let state = legacy_sse::LegacyState::new();
    let token = format!("{MARK}-sse");
    let required = Arc::new(format!("Bearer {token}"));
    let app = legacy_sse::router(state.clone(), "/sse").layer(middleware::from_fn(
        move |req: Request, next: Next| {
            let required = required.clone();
            async move {
                let ok = req
                    .headers()
                    .get("authorization")
                    .and_then(|v| v.to_str().ok())
                    == Some(required.as_str());
                if ok {
                    next.run(req).await
                } else {
                    (StatusCode::UNAUTHORIZED, "no").into_response()
                }
            }
        },
    ));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/sse", listener.local_addr().unwrap());
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    let _server = Server {
        url: url.clone(),
        task,
    };
    let env = {
        let token = token.clone();
        move |k: &str| (k == "SSE_TOKEN").then(|| token.clone())
    };
    let connect = |body: String| {
        let env = env.clone();
        async move {
            let (server_cfg, remote) = resolve(&body, &env);
            let options = http_options(&server_cfg, &remote, None, &env).expect("options");
            assert!(!format!("{options:?}").contains(MARK));
            http::connect(&options, &scratch_backend("sse")).await
        }
    };
    let common = format!("url = \"{url}\"\nbearer_env = \"SSE_TOKEN\"\nstartup_timeout_secs = 10");

    // type = "sse", and type = "http" falling back to it.
    for body in [
        format!("type = \"sse\"\n{common}"),
        format!("type = \"http\"\n{common}"),
    ] {
        let client = connect(body).await.expect("connect");
        assert_eq!(client.info().transport, "sse");
        assert_eq!(client.info().protocol_version.as_str(), "2024-11-05");
        assert_eq!(client.info().auth, "bearer (env SSE_TOKEN)");
        assert_eq!(client.list_tools().await.unwrap()[0].name, "echo");
        client.shutdown().await;
    }
    assert_eq!(state.gets.load(std::sync::atomic::Ordering::SeqCst), 2);

    // fallback_sse = false: the Streamable HTTP refusal stands.
    let err = connect(format!("type = \"http\"\nfallback_sse = false\n{common}"))
        .await
        .expect_err("no fallback");
    assert!(http::wants_sse_fallback(&err), "{err}");
    assert!(!err.to_string().contains(MARK));
    assert_eq!(state.gets.load(std::sync::atomic::Ordering::SeqCst), 2);
}
