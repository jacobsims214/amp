//! Clients for third-party MCP servers (D1 of decisions/amp-harness-configurability.md).
//!
//! One [`ExternalClient`] per configured `[mcp.<name>]` server. It shares rmcp, the error type and
//! the protocol modes with [`crate::AmpMcpClient`], but none of AMP's own extras (catalog TTL
//! hints, the health poller, reconnects): the hub (H6) owns restarts and tool lists.
//!
//! Transports:
//! - [`stdio`]: a local child process ([`ExternalClient::connect_stdio`], #2236).
//! - Streamable HTTP (#2237) and legacy SSE (H5) connect through [`ExternalClient::handshake`].
//!
//! The client never sees a configuration file: amp-core resolves `[mcp.<name>]` (env, secrets,
//! paths) into a spec and hands it here, so this crate stays free of rpi and of config rules.

pub mod stdio;

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
        JsonObject, ProtocolVersion, Tool,
    },
    service::{
        ClientCacheConfig, ClientInitializeError, ClientServiceExt, NotificationContext, Peer,
        RunningService,
    },
    transport::IntoTransport,
};

pub use stdio::{
    SHUTDOWN_GRACE, STDERR_LOG_MAX_BYTES, StdioSpec, rotate_log_if_full, stderr_log_path,
};

use crate::{McpError, ProtocolMode};

/// Connect and call limits plus what we tell the server about ourselves.
#[derive(Debug, Clone)]
pub struct ExternalOptions {
    /// Spawn (stdio) or connect, plus the MCP handshake (`startup_timeout_secs`).
    pub connect_timeout: Duration,
    /// Each `tools/list` page and `tools/call` (`call_timeout_secs`).
    pub call_timeout: Duration,
    /// `Legacy` (`initialize`) is the stdio default: every stdio server in the wild speaks it.
    pub protocol: ProtocolMode,
    pub client_name: String,
    pub client_version: String,
}

impl Default for ExternalOptions {
    fn default() -> Self {
        Self {
            connect_timeout: Duration::from_secs(30),
            call_timeout: crate::DEFAULT_CALL_TIMEOUT,
            protocol: ProtocolMode::Legacy,
            client_name: "amp-harness".to_owned(),
            client_version: env!("CARGO_PKG_VERSION").to_owned(),
        }
    }
}

/// What the server told us in the handshake.
#[derive(Debug, Clone)]
pub struct ExternalInfo {
    /// `stdio`, `http` or `sse`.
    pub transport: &'static str,
    pub protocol_version: ProtocolVersion,
    pub server_name: Option<String>,
    pub server_version: Option<String>,
    /// The server's `instructions` (D8 puts them in the prompt).
    pub instructions: Option<String>,
}

/// The rmcp client handler: identifies us and notices `notifications/tools/list_changed`.
#[derive(Debug, Clone)]
pub(crate) struct ExternalHandler {
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

/// A connected third-party MCP server. Share it behind an `Arc`; calls take `&self`.
///
/// Dropping it closes the connection; for a stdio server it also kills the server's whole process
/// group at once. [`Self::shutdown`] is the graceful way (close stdin, wait, then kill).
pub struct ExternalClient {
    name: String,
    service: Option<RunningService<RoleClient, ExternalHandler>>,
    info: ExternalInfo,
    call_timeout: Duration,
    tools_changed: Arc<AtomicBool>,
    /// The stdio server's process (group) and its stderr pump. `None` for remote servers.
    child: Option<stdio::ChildGuard>,
}

impl std::fmt::Debug for ExternalClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ExternalClient")
            .field("name", &self.name)
            .field("info", &self.info)
            .field("call_timeout", &self.call_timeout)
            .field("pid", &self.pid())
            .finish_non_exhaustive()
    }
}

impl ExternalClient {
    /// Runs the MCP handshake over `transport` under `options.connect_timeout`. `target` names the
    /// server in errors (a URL origin, or `stdio server <name>`); it must not hold a secret.
    pub(crate) async fn handshake<T, E, A>(
        name: &str,
        target: &str,
        transport_kind: &'static str,
        transport: T,
        options: &ExternalOptions,
    ) -> Result<Self, McpError>
    where
        T: IntoTransport<RoleClient, E, A>,
        E: std::error::Error + Send + Sync + 'static,
    {
        let tools_changed = Arc::new(AtomicBool::new(false));
        let handler = ExternalHandler {
            info: ClientConfig::new(
                ClientCapabilities::default(),
                Implementation::new(options.client_name.clone(), options.client_version.clone()),
            )
            .with_protocol_version(ProtocolMode::LEGACY_VERSION),
            tools_changed: tools_changed.clone(),
        };
        let service = tokio::time::timeout(
            options.connect_timeout,
            handler.serve_with_lifecycle(transport, options.protocol.lifecycle()),
        )
        .await
        .map_err(|_| McpError::ConnectTimeout {
            url: target.to_owned(),
            after: options.connect_timeout,
        })?
        .map_err(|e| McpError::from_init(target, e))?;
        // No response cache: the hub decides when to refetch (list_changed, /mcp restart).
        service
            .peer()
            .set_response_cache_config(ClientCacheConfig::disabled())
            .await;
        let peer_info = service.peer_info().ok_or_else(|| McpError::Connect {
            url: target.to_owned(),
            source: Box::new(ClientInitializeError::ConnectionClosed(
                "no server info after the handshake".to_owned(),
            )),
        })?;
        let info = ExternalInfo {
            transport: transport_kind,
            protocol_version: peer_info.protocol_version.clone(),
            server_name: peer_info.server_info.as_ref().map(|s| s.name.clone()),
            server_version: peer_info.server_info.as_ref().map(|s| s.version.clone()),
            instructions: peer_info.instructions.clone(),
        };
        Ok(Self {
            name: name.to_owned(),
            service: Some(service),
            info,
            call_timeout: options.call_timeout,
            tools_changed,
            child: None,
        })
    }

    /// The configured server name (`[mcp.<name>]`).
    pub fn name(&self) -> &str {
        &self.name
    }

    pub fn info(&self) -> &ExternalInfo {
        &self.info
    }

    pub fn call_timeout(&self) -> Duration {
        self.call_timeout
    }

    /// The stdio server's process id (also its process group and session id). `None` for remote
    /// servers.
    pub fn pid(&self) -> Option<u32> {
        self.child.as_ref().map(|c| c.pid)
    }

    /// Where the stdio server's stderr goes. `None` for remote servers.
    pub fn stderr_log(&self) -> Option<&std::path::Path> {
        self.child.as_ref().map(|c| c.log.as_path())
    }

    /// The connection is gone (the server exited, or the transport failed).
    pub fn is_closed(&self) -> bool {
        match &self.service {
            Some(s) => s.is_closed() || s.peer().is_transport_closed(),
            None => true,
        }
    }

    /// rmcp's peer, for requests this type does not wrap (resources, prompts).
    pub fn peer(&self) -> Result<Peer<RoleClient>, McpError> {
        self.service
            .as_ref()
            .map(|s| s.peer().clone())
            .ok_or(McpError::Closed)
    }

    /// True once after the server sent `notifications/tools/list_changed`.
    pub fn take_tools_changed(&self) -> bool {
        self.tools_changed.swap(false, Ordering::AcqRel)
    }

    /// Every tool, following `nextCursor`, under the call timeout.
    pub async fn list_tools(&self) -> Result<Vec<Tool>, McpError> {
        let peer = self.live_peer()?;
        self.bounded("tools/list", self.call_timeout, peer.list_all_tools())
            .await
    }

    /// Calls a tool under the call timeout. `Ok` includes results the tool flagged `isError`;
    /// `Err` means the call did not complete. On timeout the request is dropped, which makes rmcp
    /// send `notifications/cancelled`.
    pub async fn call_tool(
        &self,
        name: &str,
        arguments: Option<JsonObject>,
    ) -> Result<CallToolResult, McpError> {
        self.call_tool_with_timeout(name, arguments, self.call_timeout)
            .await
    }

    pub async fn call_tool_with_timeout(
        &self,
        name: &str,
        arguments: Option<JsonObject>,
        timeout: Duration,
    ) -> Result<CallToolResult, McpError> {
        let peer = self.live_peer()?;
        let mut params = CallToolRequestParams::new(name.to_owned());
        if let Some(arguments) = arguments {
            params = params.with_arguments(arguments);
        }
        self.bounded(
            &format!("tools/call {name}"),
            timeout,
            peer.call_tool(params),
        )
        .await
    }

    fn live_peer(&self) -> Result<Peer<RoleClient>, McpError> {
        if self.is_closed() {
            return Err(McpError::Closed);
        }
        self.peer()
    }

    async fn bounded<T>(
        &self,
        operation: &str,
        timeout: Duration,
        request: impl Future<Output = Result<T, rmcp::ServiceError>>,
    ) -> Result<T, McpError> {
        match tokio::time::timeout(timeout, request).await {
            Err(_) => Err(McpError::Timeout {
                operation: operation.to_owned(),
                after: timeout,
            }),
            Ok(Err(e)) => Err(McpError::from_service(operation, e)),
            Ok(Ok(value)) => Ok(value),
        }
    }

    /// Closes the connection. A stdio server gets EOF on stdin and [`SHUTDOWN_GRACE`] to exit;
    /// then its whole process group is killed (so `npx`/`uvx` trees do not leak) and reaped.
    pub async fn shutdown(mut self) {
        if let Some(service) = self.service.take() {
            // Closes the transport: for stdio that is the child's stdin.
            let _ = tokio::time::timeout(SHUTDOWN_GRACE, service.cancel()).await;
        }
        if let Some(mut child) = self.child.take() {
            child.shutdown(SHUTDOWN_GRACE).await;
        }
    }
}
