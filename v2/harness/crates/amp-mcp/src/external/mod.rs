//! Clients for third-party MCP servers (story 397, D1): one [`ExternalClient`] per configured
//! `[mcp.<name>]` server.
//!
//! [`ExternalClient`] is transport-agnostic: rmcp's `RunningService` hides the transport, so a stdio
//! child (H3), Streamable HTTP ([`http`], H4) and legacy HTTP+SSE (H5) all end up as the same type.
//! A transport module builds its rmcp transport and hands a factory for it to
//! [`ExternalClient::connect_with`], which runs the MCP lifecycle in the configured
//! [`ProtocolMode`] and keeps what the server said.
//!
//! Unlike [`crate::AmpMcpClient`], an external client never swaps its connection: the hub (H6)
//! drops a failed client and connects a new one with backoff. AMP-only behaviour (catalog TTL
//! hints, the metadata poller, reconnect after `amp-harness login`) stays on `AmpMcpClient`.
//!
//! Sampling, elicitation and roots are not advertised (D8): the client's capabilities are empty.

pub mod http;

use std::{
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use rmcp::{
    ClientHandler, RoleClient,
    model::{
        CallToolRequestParams, CallToolResult, ClientCapabilities, ClientConfig, Implementation,
        JsonObject, PaginatedRequestParams, ProtocolVersion, Tool,
    },
    service::{
        ClientCacheConfig, ClientInitializeError, ClientServiceExt, NotificationContext, Peer,
        RunningService,
    },
    transport::IntoTransport,
};
use tokio::sync::watch;

use crate::{
    DEFAULT_CALL_TIMEOUT, DEFAULT_CONNECT_TIMEOUT, Era, Health, McpError, ProtocolMode,
    health::HealthCell,
};

/// A secret value (a header value, a bearer token, an OAuth client secret). `Debug` never shows
/// it; [`Self::expose`] is the one way to read it, at the point it goes on the wire.
#[derive(Clone, PartialEq, Eq)]
pub struct Secret(String);

impl Secret {
    pub fn new(value: impl Into<String>) -> Self {
        Self(value.into())
    }

    pub fn expose(&self) -> &str {
        &self.0
    }
}

impl std::fmt::Debug for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Secret(<redacted>)")
    }
}

/// What every transport shares when connecting.
#[derive(Debug, Clone)]
pub struct ExternalOptions {
    /// The configured server name (`[mcp.<name>]`), used in messages.
    pub server: String,
    pub protocol: ProtocolMode,
    /// Bound on the whole MCP lifecycle (and, for HTTP, OAuth discovery before it).
    pub connect_timeout: Duration,
    /// Default bound on one request ([`ExternalClient::call_tool`], `tools/list`).
    pub call_timeout: Duration,
    /// `clientInfo` sent to the server.
    pub client_name: String,
    pub client_version: String,
}

impl ExternalOptions {
    pub fn new(server: impl Into<String>) -> Self {
        Self {
            server: server.into(),
            protocol: ProtocolMode::Auto,
            connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            call_timeout: DEFAULT_CALL_TIMEOUT,
            client_name: "amp-harness".to_owned(),
            client_version: env!("CARGO_PKG_VERSION").to_owned(),
        }
    }

    pub fn with_protocol(mut self, protocol: ProtocolMode) -> Self {
        self.protocol = protocol;
        self
    }

    pub fn with_connect_timeout(mut self, timeout: Duration) -> Self {
        self.connect_timeout = timeout;
        self
    }

    pub fn with_call_timeout(mut self, timeout: Duration) -> Self {
        self.call_timeout = timeout;
        self
    }
}

/// What a server told us when we connected. Holds no secrets.
#[derive(Debug, Clone)]
pub struct ExternalSession {
    /// The configured server name.
    pub server: String,
    /// `stdio`, `http`, `http (unix socket)`, `sse`.
    pub transport: String,
    /// Where it is: the URL's origin for remote servers, the command for stdio.
    pub endpoint: String,
    pub mode: ProtocolMode,
    pub protocol_version: ProtocolVersion,
    pub era: Era,
    /// `Auto` found a server without 2026-07-28 and used `initialize`.
    pub fell_back: bool,
    pub server_name: Option<String>,
    pub server_version: Option<String>,
    /// The server's instructions (D8 puts them in the prompt, capped).
    pub instructions: Option<String>,
    /// How we authenticated, e.g. `none`, `header X-Api-Key (keychain)`, `bearer (env GITHUB_TOKEN)`,
    /// `oauth (macOS Keychain (service amp-harness))`. Never contains a secret.
    pub auth: String,
}

/// The rmcp handler: identifies us and notices `tools/list_changed`.
#[derive(Debug, Clone)]
pub struct ExternalHandler {
    info: ClientConfig,
    tools_changed: Arc<AtomicBool>,
}

impl ClientHandler for ExternalHandler {
    fn get_info(&self) -> ClientConfig {
        self.info.clone()
    }

    async fn on_tool_list_changed(&self, _context: NotificationContext<RoleClient>) {
        self.tools_changed.store(true, Ordering::Release);
    }
}

/// A connected third-party MCP server. Share it behind an `Arc`; every method takes `&self`.
pub struct ExternalClient {
    service: RunningService<RoleClient, ExternalHandler>,
    session: ExternalSession,
    call_timeout: Duration,
    tools_changed: Arc<AtomicBool>,
    health: HealthCell,
}

impl std::fmt::Debug for ExternalClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ExternalClient")
            .field("session", &self.session)
            .field("call_timeout", &self.call_timeout)
            .field("health", &self.health.get())
            .finish_non_exhaustive()
    }
}

/// How a transport describes itself to [`ExternalClient::connect_with`].
#[derive(Debug, Clone)]
pub struct TransportInfo {
    /// [`ExternalSession::transport`].
    pub kind: String,
    /// [`ExternalSession::endpoint`].
    pub endpoint: String,
    /// [`ExternalSession::auth`].
    pub auth: String,
}

impl ExternalClient {
    /// Runs the MCP lifecycle over transports from `make_transport`, in `options.protocol`.
    ///
    /// `make_transport` is called once, and once more when `Auto` meets a server that knows
    /// `server/discover` but offers only pre-2026 versions (the retry needs a fresh transport). An
    /// `Err` from it is returned as is. A 401 during the lifecycle becomes
    /// [`McpError::ServerAuth`] (with the server's challenge, for `login`).
    pub async fn connect_with<T, E, A, F>(
        options: &ExternalOptions,
        info: TransportInfo,
        mut make_transport: F,
    ) -> Result<Self, McpError>
    where
        T: IntoTransport<RoleClient, E, A>,
        E: std::error::Error + Send + Sync + 'static,
        F: FnMut() -> Result<T, McpError> + Send,
    {
        let tools_changed = Arc::new(AtomicBool::new(false));
        let handler = ExternalHandler {
            info: ClientConfig::new(
                ClientCapabilities::default(),
                Implementation::new(options.client_name.clone(), options.client_version.clone()),
            )
            .with_protocol_version(options.protocol.initialize_version()),
            tools_changed: tools_changed.clone(),
        };
        let map = |e: ClientInitializeError| {
            server_auth(
                McpError::from_init(&info.endpoint, e),
                &options.server,
                &info.auth,
            )
        };

        let connect = async {
            let first = handler
                .clone()
                .serve_with_lifecycle(make_transport()?, options.protocol.lifecycle())
                .await;
            match first {
                // As in AmpMcpClient: rmcp's Auto falls back only when `server/discover` is
                // unknown. A server that knows it but lists only initialize-era versions is legacy.
                Err(ClientInitializeError::NoCompatibleProtocolVersion {
                    server_supported,
                    ..
                }) if options.protocol == ProtocolMode::Auto
                    && server_supported.iter().any(ProtocolVersion::has_initialize) =>
                {
                    handler
                        .clone()
                        .serve_with_lifecycle(make_transport()?, ProtocolMode::Legacy.lifecycle())
                        .await
                        .map_err(map)
                }
                other => other.map_err(map),
            }
        };
        let service = tokio::time::timeout(options.connect_timeout, connect)
            .await
            .map_err(|_| McpError::ConnectTimeout {
                url: info.endpoint.clone(),
                after: options.connect_timeout,
            })??;
        // No response cache: list_changed and refreshes always reach the server.
        service
            .peer()
            .set_response_cache_config(ClientCacheConfig::disabled())
            .await;

        let peer_info = service.peer_info().ok_or_else(|| McpError::Connect {
            url: info.endpoint.clone(),
            source: Box::new(ClientInitializeError::ConnectionClosed(
                "no server info after the handshake".to_owned(),
            )),
        })?;
        let era = Era::of(&peer_info.protocol_version);
        let session = ExternalSession {
            server: options.server.clone(),
            transport: info.kind,
            endpoint: info.endpoint,
            mode: options.protocol,
            protocol_version: peer_info.protocol_version.clone(),
            era,
            fell_back: options.protocol == ProtocolMode::Auto && era == Era::Legacy,
            server_name: peer_info.server_info.as_ref().map(|s| s.name.clone()),
            server_version: peer_info.server_info.as_ref().map(|s| s.version.clone()),
            instructions: peer_info.instructions.clone(),
            auth: info.auth,
        };
        tracing::debug!(
            server = %session.server,
            transport = %session.transport,
            protocol = %session.protocol_version,
            "connected to MCP server"
        );
        Ok(Self {
            service,
            session,
            call_timeout: options.call_timeout,
            tools_changed,
            health: HealthCell::new(),
        })
    }

    /// What the server said when we connected.
    pub fn session(&self) -> &ExternalSession {
        &self.session
    }

    /// The server's instructions, if any.
    pub fn instructions(&self) -> Option<&str> {
        self.session.instructions.as_deref()
    }

    pub fn call_timeout(&self) -> Duration {
        self.call_timeout
    }

    /// The rmcp peer, for requests this type does not wrap (resources, prompts: H15).
    pub fn peer(&self) -> Peer<RoleClient> {
        self.service.peer().clone()
    }

    pub fn is_closed(&self) -> bool {
        self.service.is_closed() || self.service.peer().is_transport_closed()
    }

    /// Current health. A closed transport always reads as disconnected.
    pub fn health(&self) -> Health {
        if self.is_closed() {
            return Health::Disconnected {
                reason: "connection closed".to_owned(),
            };
        }
        self.health.get()
    }

    pub fn watch_health(&self) -> watch::Receiver<Health> {
        self.health.subscribe()
    }

    /// True once after the server sent `notifications/tools/list_changed`: refetch with
    /// [`Self::list_tools`].
    pub fn take_tools_changed(&self) -> bool {
        self.tools_changed.swap(false, Ordering::AcqRel)
    }

    /// Every tool, all pages, under the call timeout.
    pub async fn list_tools(&self) -> Result<Vec<Tool>, McpError> {
        // Cleared first, so a list_changed that lands mid-fetch is not lost.
        self.tools_changed.store(false, Ordering::Release);
        let peer = self.peer();
        let fetch = async move {
            let mut tools = Vec::new();
            let mut cursor = None;
            loop {
                let mut params = PaginatedRequestParams::default();
                params.cursor = cursor;
                let page = peer.list_tools(Some(params)).await?;
                tools.extend(page.tools);
                cursor = page.next_cursor;
                if cursor.is_none() {
                    break;
                }
            }
            Ok::<_, rmcp::ServiceError>(tools)
        };
        self.with_timeout("tools/list", self.call_timeout, fetch)
            .await
    }

    /// Calls a tool (its original, unprefixed name) under the default call timeout. `Ok` includes
    /// results the tool flagged `isError`; `Err` means the call did not complete.
    pub async fn call_tool(
        &self,
        name: &str,
        arguments: Option<JsonObject>,
    ) -> Result<CallToolResult, McpError> {
        self.call_tool_with_timeout(name, arguments, self.call_timeout)
            .await
    }

    /// [`Self::call_tool`] with an explicit timeout. Dropping the future (or the timeout) sends
    /// `notifications/cancelled`.
    pub async fn call_tool_with_timeout(
        &self,
        name: &str,
        arguments: Option<JsonObject>,
        timeout: Duration,
    ) -> Result<CallToolResult, McpError> {
        let mut params = CallToolRequestParams::new(name.to_owned());
        if let Some(arguments) = arguments {
            params = params.with_arguments(arguments);
        }
        let peer = self.peer();
        self.with_timeout(
            &format!("tools/call {name}"),
            timeout,
            peer.call_tool(params),
        )
        .await
    }

    async fn with_timeout<T>(
        &self,
        operation: &str,
        timeout: Duration,
        request: impl Future<Output = Result<T, rmcp::ServiceError>>,
    ) -> Result<T, McpError> {
        if self.is_closed() {
            let e = McpError::Closed;
            self.health.set(Health::after(Err(&e)));
            return Err(e);
        }
        let result = match tokio::time::timeout(timeout, request).await {
            Err(_) => Err(McpError::Timeout {
                operation: format!("{}: {operation}", self.session.server),
                after: timeout,
            }),
            Ok(Err(e)) => Err(self.map_service_error(operation, e)),
            Ok(Ok(value)) => Ok(value),
        };
        self.health.set(Health::after(result.as_ref().map(|_| ())));
        result
    }

    fn map_service_error(&self, operation: &str, error: rmcp::ServiceError) -> McpError {
        server_auth(
            McpError::from_service(&format!("{}: {operation}", self.session.server), error),
            &self.session.server,
            &self.session.auth,
        )
    }

    /// Closes the connection (for stdio, the transport ends the child).
    pub async fn close(self) {
        let _ = self.service.cancel().await;
    }
}

/// A rejection (rmcp's auth error with the server's challenge, or a bare HTTP 401/403 that many
/// API-key servers send without `WWW-Authenticate`) as [`McpError::ServerAuth`]; anything else as
/// is.
fn server_auth(error: McpError, server: &str, auth: &str) -> McpError {
    let challenge = match error {
        McpError::AuthRequired { challenge } => challenge,
        e if e.kind() == crate::FailureKind::LoginRequired && !e.is_server_error() => None,
        other => return other,
    };
    McpError::ServerAuth {
        server: server.to_owned(),
        reason: auth_hint(server, auth),
        challenge,
    }
}

/// What to tell a person whose server rejected us, given how we authenticated.
pub(crate) fn auth_hint(server: &str, auth: &str) -> String {
    if auth.starts_with("oauth") || auth == "none" {
        format!("login required: run `amp-harness mcp login {server}`")
    } else {
        format!("the server rejected the configured credentials ({auth})")
    }
}
