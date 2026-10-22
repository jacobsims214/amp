//! The AMP MCP client: connect (D2), tool catalog, per-call timeouts, health.

use std::{
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use rmcp::{
    ClientHandler, RoleClient,
    model::{
        CacheScope, CallToolRequestParams, CallToolResult, ClientCapabilities, ClientConfig,
        Implementation, JsonObject, PaginatedRequestParams, ProtocolVersion,
    },
    service::{
        ClientCacheConfig, ClientInitializeError, ClientServiceExt, NotificationContext, Peer,
        RunningService,
    },
    transport::{
        StreamableHttpClientTransport,
        streamable_http_client::{StreamableHttpClient, StreamableHttpClientTransportConfig},
    },
};
use tokio::sync::{Mutex, watch};

use crate::{
    Auth, Catalog, Era, Health, McpError, ProtocolMode, Settings, health::HealthCell,
    http::build_http_client,
};

/// Per-call timeout (D2). A call that runs longer is cancelled and returns [`McpError::Timeout`].
pub const DEFAULT_CALL_TIMEOUT: Duration = Duration::from_secs(120);
/// Upper bound on connecting. Auto's discover probe alone may take 10 s before it falls back.
pub const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(30);

/// Everything [`AmpMcpClient::connect`] needs.
#[derive(Debug, Clone)]
pub struct ClientOptions {
    pub url: String,
    pub ca_file: Option<PathBuf>,
    pub auth: Auth,
    pub protocol: ProtocolMode,
    pub call_timeout: Duration,
    pub connect_timeout: Duration,
    /// Catalog TTL when the server sends no `ttlMs` hint. Zero = refetch whenever asked.
    pub catalog_default_ttl: Duration,
    /// `clientInfo` sent to the server.
    pub client_name: String,
    pub client_version: String,
}

impl ClientOptions {
    pub fn new(url: impl Into<String>) -> Self {
        Self {
            url: url.into(),
            ca_file: None,
            auth: Auth::None,
            protocol: ProtocolMode::Auto,
            call_timeout: DEFAULT_CALL_TIMEOUT,
            connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            catalog_default_ttl: Duration::ZERO,
            client_name: "amp-harness".to_owned(),
            client_version: env!("CARGO_PKG_VERSION").to_owned(),
        }
    }

    pub fn from_settings(settings: &Settings) -> Self {
        Self {
            ca_file: settings.ca_file.clone(),
            auth: settings.auth.clone(),
            protocol: settings.protocol,
            ..Self::new(settings.url.clone())
        }
    }

    pub fn with_protocol(mut self, protocol: ProtocolMode) -> Self {
        self.protocol = protocol;
        self
    }

    pub fn with_auth(mut self, auth: Auth) -> Self {
        self.auth = auth;
        self
    }

    pub fn with_ca_file(mut self, ca_file: impl Into<PathBuf>) -> Self {
        self.ca_file = Some(ca_file.into());
        self
    }

    pub fn with_call_timeout(mut self, timeout: Duration) -> Self {
        self.call_timeout = timeout;
        self
    }

    pub fn with_connect_timeout(mut self, timeout: Duration) -> Self {
        self.connect_timeout = timeout;
        self
    }

    pub fn with_catalog_default_ttl(mut self, ttl: Duration) -> Self {
        self.catalog_default_ttl = ttl;
        self
    }
}

/// What the server told us when we connected.
#[derive(Debug, Clone)]
pub struct ServerSession {
    pub url: String,
    /// The mode we asked for.
    pub mode: ProtocolMode,
    /// The version actually negotiated.
    pub protocol_version: ProtocolVersion,
    pub era: Era,
    /// `Auto` found a legacy-only server and used `initialize`.
    pub fell_back: bool,
    pub server_name: Option<String>,
    pub server_version: Option<String>,
    /// AMP's server instructions (from `server/discover`, or `initialize` on the legacy path).
    pub instructions: Option<String>,
    /// How we authenticated, for `doctor` (e.g. `none`, `static token (env AMP_TOKEN)`,
    /// `oauth (macOS Keychain ...)`). Never contains a secret.
    pub auth: String,
}

/// The rmcp client-side handler: identifies us and notices `tools/list_changed`.
#[derive(Debug, Clone)]
struct AmpClientHandler {
    info: ClientConfig,
    tools_changed: Arc<AtomicBool>,
}

impl ClientHandler for AmpClientHandler {
    fn get_info(&self) -> ClientConfig {
        self.info.clone()
    }

    async fn on_tool_list_changed(&self, _context: NotificationContext<RoleClient>) {
        self.tools_changed.store(true, Ordering::Release);
    }
}

/// A connected AMP MCP client. Share it behind an `Arc`; every method takes `&self`.
pub struct AmpMcpClient {
    service: RunningService<RoleClient, AmpClientHandler>,
    session: ServerSession,
    call_timeout: Duration,
    catalog_default_ttl: Duration,
    catalog: Mutex<Option<Catalog>>,
    tools_changed: Arc<AtomicBool>,
    health: HealthCell,
}

impl std::fmt::Debug for AmpMcpClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("AmpMcpClient")
            .field("session", &self.session)
            .field("call_timeout", &self.call_timeout)
            .field("health", &self.health.get())
            .finish_non_exhaustive()
    }
}

impl AmpMcpClient {
    /// Connects with the built-in HTTP client (rustls + `ca_file`) and `options.auth`.
    pub async fn connect(options: ClientOptions) -> Result<Self, McpError> {
        let http = build_http_client(options.ca_file.as_deref())?;
        Self::connect_with_http_client(options, http).await
    }

    /// Connects over any rmcp Streamable HTTP client. M1-3 passes rmcp's OAuth `AuthClient` here
    /// (built on [`crate::build_http_client`]) with `options.auth = Auth::None`.
    pub async fn connect_with_http_client<C>(
        options: ClientOptions,
        http: C,
    ) -> Result<Self, McpError>
    where
        C: StreamableHttpClient + Send + Sync + 'static,
    {
        let mut config = StreamableHttpClientTransportConfig::with_uri(options.url.clone())
            // Legacy sessions: transparently re-initialize if the server forgot ours (restart).
            .reinit_on_expired_session(true);
        if let Auth::Static { token, .. } = &options.auth {
            config = config.auth_header(token.expose());
        }

        let tools_changed = Arc::new(AtomicBool::new(false));
        let handler = AmpClientHandler {
            info: ClientConfig::new(
                ClientCapabilities::default(),
                Implementation::new(options.client_name.clone(), options.client_version.clone()),
            )
            .with_protocol_version(options.protocol.initialize_version()),
            tools_changed: tools_changed.clone(),
        };

        let handshake = |mode: ProtocolMode| {
            let transport =
                StreamableHttpClientTransport::with_client(http.clone(), config.clone());
            handler
                .clone()
                .serve_with_lifecycle(transport, mode.lifecycle())
        };
        let connect = async {
            match handshake(options.protocol).await {
                // rmcp's Auto falls back only when `server/discover` is rejected as unknown. A server
                // that knows the method but lists only pre-2026 versions is legacy too: retry with
                // `initialize` on a fresh transport.
                Err(ClientInitializeError::NoCompatibleProtocolVersion {
                    server_supported,
                    ..
                }) if options.protocol == ProtocolMode::Auto
                    && server_supported.iter().any(ProtocolVersion::has_initialize) =>
                {
                    tracing::debug!(
                        ?server_supported,
                        "no 2026-07-28 on the server; using initialize"
                    );
                    handshake(ProtocolMode::Legacy).await
                }
                other => other,
            }
        };
        let service = tokio::time::timeout(options.connect_timeout, connect)
            .await
            .map_err(|_| McpError::ConnectTimeout {
                url: options.url.clone(),
                after: options.connect_timeout,
            })?
            .map_err(|e| McpError::from_init(&options.url, e))?;

        // The catalog below is the one cache, so a refresh or list_changed always reaches the server.
        service
            .peer()
            .set_response_cache_config(ClientCacheConfig::disabled())
            .await;

        let info = service.peer_info().ok_or_else(|| McpError::Connect {
            url: options.url.clone(),
            source: Box::new(ClientInitializeError::ConnectionClosed(
                "no server info after the handshake".to_owned(),
            )),
        })?;
        let era = Era::of(&info.protocol_version);
        let session = ServerSession {
            url: options.url.clone(),
            mode: options.protocol,
            protocol_version: info.protocol_version.clone(),
            era,
            fell_back: options.protocol == ProtocolMode::Auto && era == Era::Legacy,
            server_name: info.server_info.as_ref().map(|s| s.name.clone()),
            server_version: info.server_info.as_ref().map(|s| s.version.clone()),
            instructions: info.instructions.clone(),
            auth: options.auth.describe(),
        };
        tracing::debug!(
            url = %session.url,
            protocol = %session.protocol_version,
            era = %session.era,
            fell_back = session.fell_back,
            "connected to AMP MCP"
        );

        Ok(Self {
            service,
            session,
            call_timeout: options.call_timeout,
            catalog_default_ttl: options.catalog_default_ttl,
            catalog: Mutex::new(None),
            tools_changed,
            health: HealthCell::new(),
        })
    }

    pub fn session(&self) -> &ServerSession {
        &self.session
    }

    pub(crate) fn set_auth_description(&mut self, auth: String) {
        self.session.auth = auth;
    }

    /// AMP's server instructions, for the system prompt.
    pub fn instructions(&self) -> Option<&str> {
        self.session.instructions.as_deref()
    }

    pub fn protocol_version(&self) -> &ProtocolVersion {
        &self.session.protocol_version
    }

    pub fn call_timeout(&self) -> Duration {
        self.call_timeout
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

    /// Watch health changes (the TUI header).
    pub fn watch_health(&self) -> watch::Receiver<Health> {
        self.health.subscribe()
    }

    pub fn is_closed(&self) -> bool {
        self.service.is_closed() || self.service.peer().is_transport_closed()
    }

    /// The rmcp peer, for requests this crate does not wrap.
    pub fn peer(&self) -> &Peer<RoleClient> {
        self.service.peer()
    }

    /// The tool catalog, refetched only when the cached one is stale (cache hint, list_changed).
    /// If a refetch fails and an older list exists, the older list is returned (SEP-2549 allows
    /// serving stale on error) and health shows the failure.
    pub async fn catalog(&self) -> Result<Catalog, McpError> {
        let mut slot = self.catalog.lock().await;
        let changed = self.tools_changed.load(Ordering::Acquire);
        if let Some(catalog) = slot.as_ref()
            && !changed
            && catalog.is_fresh()
        {
            return Ok(catalog.clone());
        }
        match self.fetch_catalog().await {
            Ok(catalog) => {
                *slot = Some(catalog.clone());
                Ok(catalog)
            }
            Err(e) => {
                // Keep a pending list_changed for the next attempt.
                if changed {
                    self.tools_changed.store(true, Ordering::Release);
                }
                match slot.as_ref() {
                    Some(stale) if !e.is_auth_required() => {
                        tracing::warn!(error = %e, "tools/list failed; serving the previous catalog");
                        Ok(stale.clone())
                    }
                    _ => Err(e),
                }
            }
        }
    }

    /// Refetches the catalog now, ignoring the cache hint.
    pub async fn refresh_catalog(&self) -> Result<Catalog, McpError> {
        let mut slot = self.catalog.lock().await;
        let catalog = self.fetch_catalog().await?;
        *slot = Some(catalog.clone());
        Ok(catalog)
    }

    async fn fetch_catalog(&self) -> Result<Catalog, McpError> {
        // Cleared before the request, so a list_changed that lands mid-fetch is not lost.
        self.tools_changed.store(false, Ordering::Release);
        let peer = self.service.peer().clone();
        let fetch = async move {
            let fetched_at = Instant::now();
            let mut tools = Vec::new();
            let mut ttl_ms: Option<u64> = None;
            let mut scope: Option<CacheScope> = None;
            let mut cursor = None;
            loop {
                let mut params = PaginatedRequestParams::default();
                params.cursor = cursor;
                let page = peer.list_tools(Some(params)).await?;
                tools.extend(page.tools);
                if let Some(ms) = page.ttl_ms {
                    ttl_ms = Some(ttl_ms.map_or(ms, |cur| cur.min(ms)));
                }
                // Private wins: one private page makes the whole list private.
                scope = match (scope, page.cache_scope) {
                    (Some(CacheScope::Private), _) | (_, Some(CacheScope::Private)) => {
                        Some(CacheScope::Private)
                    }
                    (a, b) => b.or(a),
                };
                cursor = page.next_cursor;
                if cursor.is_none() {
                    break;
                }
            }
            Ok::<_, rmcp::ServiceError>((tools, fetched_at, ttl_ms, scope))
        };
        let result = self
            .with_timeout("tools/list", self.call_timeout, fetch)
            .await;
        result.map(|(tools, fetched_at, ttl_ms, scope)| {
            Catalog::new(tools, fetched_at, ttl_ms, scope, self.catalog_default_ttl)
        })
    }

    /// Calls an AMP tool with the default per-call timeout (120 s).
    ///
    /// `Ok` includes results the tool itself flagged `isError`; `Err` means the call did not
    /// complete (timeout, transport, auth). Either way the M2 adapter shows the model a tool result.
    pub async fn call_tool(
        &self,
        name: &str,
        arguments: Option<JsonObject>,
    ) -> Result<CallToolResult, McpError> {
        self.call_tool_with_timeout(name, arguments, self.call_timeout)
            .await
    }

    /// [`Self::call_tool`] with an explicit timeout.
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
        let peer = self.service.peer();
        self.with_timeout(
            &format!("tools/call {name}"),
            timeout,
            peer.call_tool(params),
        )
        .await
    }

    /// Runs one request under `timeout`, maps its error and records health. Dropping the rmcp
    /// future on timeout sends `notifications/cancelled` for the request.
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
                operation: operation.to_owned(),
                after: timeout,
            }),
            Ok(Err(e)) => Err(McpError::from_service(operation, e)),
            Ok(Ok(value)) => Ok(value),
        };
        self.health.set(Health::after(result.as_ref().map(|_| ())));
        result
    }

    /// Closes the connection.
    pub async fn close(self) {
        let _ = self.service.cancel().await;
    }
}
