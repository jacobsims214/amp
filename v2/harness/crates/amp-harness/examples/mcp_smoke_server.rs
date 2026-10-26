//! A small MCP server for the smoke test's MCP legs (#2246, H16), one per transport:
//!
//! ```sh
//! mcp_smoke_server stdio [NAME]                 # stdio (stdin/stdout)
//! mcp_smoke_server http [NAME] [--bearer-env V] # Streamable HTTP on 127.0.0.1:<free port>/mcp
//! mcp_smoke_server sse                          # legacy HTTP+SSE (2024-11-05) at 127.0.0.1:<port>/sse
//! ```
//!
//! The network modes print their URL on the first line of stdout, then serve until killed (or until
//! stdin reaches EOF, so a smoke script that dies never leaves one behind). `--bearer-env V` makes
//! the HTTP server answer 401 unless `Authorization: Bearer $V` is sent; the value is read from the
//! server's own environment and never printed.
//!
//! Tools: `echo` (`<name>: <text>`; the SSE server answers `echo: <text>`) and, on stdio and HTTP,
//! `has_env` (whether a variable reached the server, and its length, never its value).
//!
//! It is an example so that `make build` (`--all-targets`) builds it with the dev-dependencies the
//! tests already use (rmcp's server side), and the shipped binary carries no test server.

#[allow(dead_code, reason = "shared with amp-mcp's tests")]
#[path = "../../amp-mcp/tests/common/legacy_sse.rs"]
mod legacy_sse;

use std::{borrow::Cow, sync::Arc};

use axum::{
    Router,
    extract::Request,
    http::StatusCode,
    middleware::{self, Next},
    response::IntoResponse,
};
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
use tokio::io::AsyncReadExt;

#[derive(Clone)]
struct SmokeServer {
    name: String,
}

fn tool(name: &'static str, description: &'static str) -> Tool {
    let Value::Object(schema) = json!({
        "type": "object",
        "properties": {"text": {"type": "string"}, "name": {"type": "string"}},
    }) else {
        unreachable!()
    };
    Tool::new(name, description, Arc::new(schema))
}

impl ServerHandler for SmokeServer {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
            .with_server_info(Implementation::new(self.name.clone(), "1.0.0"))
            .with_instructions(format!("The {} smoke server: call echo.", self.name))
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
            tool("echo", "Echoes `text`, prefixed with the server's name."),
            tool(
                "has_env",
                "Whether the variable `name` is set (and its length).",
            ),
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

/// Serves `app` on a free loopback port, prints `http://127.0.0.1:<port><path>`, and returns when
/// stdin reaches EOF.
async fn serve(app: Router, path: &str) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind 127.0.0.1:0");
    let addr = listener.local_addr().expect("local addr");
    println!("http://{addr}{path}");
    let server = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    let mut stdin = tokio::io::stdin();
    let mut buf = [0u8; 256];
    while matches!(stdin.read(&mut buf).await, Ok(n) if n > 0) {}
    server.abort();
}

fn usage() -> ! {
    eprintln!("usage: mcp_smoke_server stdio|http|sse [NAME] [--bearer-env VAR]");
    std::process::exit(2);
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let mode = args.first().cloned().unwrap_or_else(|| usage());
    let mut name = None;
    let mut bearer_env = None;
    let mut rest = args.iter().skip(1);
    while let Some(a) = rest.next() {
        match a.as_str() {
            "--bearer-env" => bearer_env = Some(rest.next().cloned().unwrap_or_else(|| usage())),
            other if !other.starts_with('-') && name.is_none() => name = Some(other.to_owned()),
            _ => usage(),
        }
    }
    let name = name.unwrap_or_else(|| format!("smoke-{mode}"));
    let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
    rt.block_on(async move {
        match mode.as_str() {
            "stdio" => {
                let running = SmokeServer { name }
                    .serve(rmcp::transport::stdio())
                    .await
                    .expect("serve stdio");
                let _ = running.waiting().await;
            }
            "http" => {
                let service: StreamableHttpService<SmokeServer, LocalSessionManager> =
                    StreamableHttpService::new(
                        move || Ok(SmokeServer { name: name.clone() }),
                        Default::default(),
                        StreamableHttpServerConfig::default().with_json_response(true),
                    );
                let mut app = Router::new().nest_service("/mcp", service);
                if let Some(var) = bearer_env {
                    let token = std::env::var(&var).unwrap_or_else(|_| {
                        eprintln!("mcp_smoke_server: {var} is not set");
                        std::process::exit(2);
                    });
                    let wanted = Arc::new(format!("Bearer {token}"));
                    app = app.layer(middleware::from_fn(move |req: Request, next: Next| {
                        let wanted = wanted.clone();
                        async move {
                            let ok = req
                                .headers()
                                .get("authorization")
                                .and_then(|h| h.to_str().ok())
                                == Some(wanted.as_str());
                            if ok {
                                next.run(req).await
                            } else {
                                (StatusCode::UNAUTHORIZED, "missing or wrong bearer token")
                                    .into_response()
                            }
                        }
                    }));
                }
                serve(app, "/mcp").await;
            }
            "sse" => {
                let app = legacy_sse::router(legacy_sse::LegacyState::new(), "/sse");
                serve(app, "/sse").await;
            }
            _ => usage(),
        }
    });
}
