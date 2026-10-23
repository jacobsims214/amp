//! In-process MCP servers for the amp-mcp tests: rmcp's own Streamable HTTP server under axum.
#![allow(dead_code, reason = "each test binary uses a different subset")]

use std::{
    borrow::Cow,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use axum::{
    extract::Request,
    http::{HeaderValue, StatusCode, header},
    middleware::{self, Next},
    response::{IntoResponse, Response},
};
use rmcp::{
    ErrorData, RoleServer, ServerHandler,
    model::{
        CacheScope, CallToolRequestParams, CallToolResponse, CallToolResult, ContentBlock,
        DiscoverResult, ErrorCode, Implementation, ListToolsResult, PaginatedRequestParams,
        ProtocolVersion, ServerCapabilities, ServerConfig, Tool,
    },
    service::{MaybeSendFuture, RequestContext},
    transport::streamable_http_server::{
        StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
    },
};

pub const INSTRUCTIONS: &str = "AMP test server instructions: read the ticket first.";

/// Which protocol eras the test server speaks.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ServerKind {
    /// Like current AMP: `server/discover` (2026-07-28) and legacy `initialize`, side by side.
    Dual,
    /// An older AMP: `server/discover` is "method not found" (rmcp's own Auto fallback path).
    LegacyOnly,
    /// A server whose SDK knows `server/discover` but supports only versions up to 2025-11-25: it
    /// answers the probe with "unsupported protocol version" (amp-mcp's extra fallback path).
    NoModernVersion,
}

#[derive(Debug, Default)]
pub struct Counters {
    pub list_tools: AtomicUsize,
    pub call_tool: AtomicUsize,
}

impl Counters {
    pub fn list_tools(&self) -> usize {
        self.list_tools.load(Ordering::SeqCst)
    }
}

#[derive(Debug, Clone)]
pub struct TestServer {
    pub kind: ServerKind,
    /// `ttlMs` (+ private scope) on `tools/list`; `None` = no hint.
    pub tools_ttl_ms: Option<u64>,
    pub counters: Arc<Counters>,
}

impl TestServer {
    pub fn new(kind: ServerKind) -> Self {
        Self {
            kind,
            tools_ttl_ms: Some(300_000),
            counters: Arc::default(),
        }
    }

    pub fn with_tools_ttl_ms(mut self, ttl: Option<u64>) -> Self {
        self.tools_ttl_ms = ttl;
        self
    }
}

fn tool(name: &'static str, description: &'static str) -> Tool {
    let schema = serde_json::json!({"type": "object", "properties": {}});
    let serde_json::Value::Object(schema) = schema else {
        unreachable!()
    };
    Tool::new(name, description, Arc::new(schema))
}

impl ServerHandler for TestServer {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
            .with_server_info(Implementation::new("amp-test", "9.9.9"))
            .with_instructions(INSTRUCTIONS)
    }

    fn supported_protocol_versions(&self) -> Cow<'static, [ProtocolVersion]> {
        match self.kind {
            ServerKind::Dual | ServerKind::LegacyOnly => {
                Cow::Borrowed(ProtocolVersion::KNOWN_VERSIONS)
            }
            ServerKind::NoModernVersion => {
                Cow::Borrowed(ProtocolVersion::known_up_to(&ProtocolVersion::V_2025_11_25))
            }
        }
    }

    fn discover(
        &self,
        _context: RequestContext<RoleServer>,
    ) -> impl Future<Output = Result<DiscoverResult, ErrorData>> + MaybeSendFuture + '_ {
        std::future::ready(match self.kind {
            ServerKind::Dual | ServerKind::NoModernVersion => Ok(DiscoverResult::from_server_info(
                self.supported_protocol_versions().into_owned(),
                self.get_info(),
            )),
            ServerKind::LegacyOnly => Err(ErrorData::new(
                ErrorCode::METHOD_NOT_FOUND,
                "Method not found",
                None,
            )),
        })
    }

    async fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        _context: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        self.counters.list_tools.fetch_add(1, Ordering::SeqCst);
        let mut result = ListToolsResult::with_all_items(vec![
            tool("amp_whoami", "Who you are acting as in AMP."),
            tool("amp_echo", "Echoes its arguments."),
            tool("amp_sleep", "Sleeps for `ms` milliseconds."),
        ]);
        if let Some(ttl) = self.tools_ttl_ms {
            result = result
                .with_ttl_ms(ttl)
                .with_cache_scope(CacheScope::Private);
        }
        Ok(result)
    }

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        _context: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        self.counters.call_tool.fetch_add(1, Ordering::SeqCst);
        let args = serde_json::Value::Object(request.arguments.clone().unwrap_or_default());
        match request.name.as_ref() {
            "amp_whoami" => {
                Ok(CallToolResult::success(vec![ContentBlock::text("local-dev")]).into())
            }
            "amp_echo" => {
                Ok(CallToolResult::success(vec![ContentBlock::text(args.to_string())]).into())
            }
            "amp_sleep" => {
                let ms = args.get("ms").and_then(|v| v.as_u64()).unwrap_or(0);
                tokio::time::sleep(Duration::from_millis(ms)).await;
                Ok(CallToolResult::success(vec![ContentBlock::text("slept")]).into())
            }
            "amp_fail" => Ok(CallToolResult::error(vec![ContentBlock::text("tool failed")]).into()),
            other => Err(ErrorData::invalid_params(
                format!("unknown tool {other}"),
                None,
            )),
        }
    }
}

/// The axum router serving `server` at `/mcp`, optionally behind a bearer check.
pub fn router(server: TestServer, bearer: Option<&'static str>) -> axum::Router {
    let service: StreamableHttpService<TestServer, LocalSessionManager> =
        StreamableHttpService::new(
            move || Ok(server.clone()),
            Default::default(),
            StreamableHttpServerConfig::default().with_json_response(true),
        );
    let router = axum::Router::new().nest_service("/mcp", service);
    match bearer {
        None => router,
        Some(token) => router.layer(middleware::from_fn(move |req: Request, next: Next| {
            require_bearer(token, req, next)
        })),
    }
}

async fn require_bearer(token: &'static str, req: Request, next: Next) -> Response {
    let ok = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v == format!("Bearer {token}"));
    if ok {
        return next.run(req).await;
    }
    let mut response = (StatusCode::UNAUTHORIZED, "unauthorized").into_response();
    response.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_static(
            r#"Bearer resource_metadata="http://127.0.0.1/.well-known/oauth-protected-resource""#,
        ),
    );
    response
}

/// A server running on a loopback port; stops when dropped.
pub struct Running {
    pub url: String,
    pub counters: Arc<Counters>,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Running {
    fn drop(&mut self) {
        self.task.abort();
    }
}

pub async fn spawn(server: TestServer) -> Running {
    spawn_with_bearer(server, None).await
}

pub async fn spawn_with_bearer(server: TestServer, bearer: Option<&'static str>) -> Running {
    let counters = server.counters.clone();
    let app = router(server, bearer);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Running {
        url: format!("http://{addr}/mcp"),
        counters,
        task,
    }
}
