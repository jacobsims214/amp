//! The AMP MCP client: connect (D2), tool catalog, per-call timeouts, health.

use std::{
    path::PathBuf,
    pin::Pin,
    sync::{
        Arc, Mutex as StdMutex, PoisonError, RwLock,
        atomic::{AtomicBool, AtomicU64, Ordering},
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

/// What a reconnect runs: a fresh connect with the session's settings, which picks up a new
/// `amp-harness login` (#2159), or connects to another server (#2164's `/server`). The same shape as
/// amp-core's metadata poller `Connector`, so one closure can serve both.
pub type Reconnector = Arc<
    dyn Fn() -> Pin<Box<dyn Future<Output = Result<AmpMcpClient, McpError>> + Send>> + Send + Sync,
>;

/// Least time between two automatic reconnect attempts (the poller's `intervals.reconnect`).
pub const DEFAULT_RECONNECT_INTERVAL: Duration = Duration::from_secs(15);
/// Upper bound on one reconnect attempt (OAuth discovery plus the handshake, each bounded too).
pub const RECONNECT_TIMEOUT: Duration = Duration::from_secs(60);

/// One MCP connection: the rmcp service and what belongs to it. [`AmpMcpClient`] holds the current
/// one and swaps it on a reconnect; a call keeps the connection it started on.
struct Connection {
    /// `None` for the placeholder of a client that started offline ([`AmpMcpClient::offline`]):
    /// it reads as closed, so the first call (or [`AmpMcpClient::reconnect`]) connects.
    service: Option<RunningService<RoleClient, AmpClientHandler>>,
    session: Arc<ServerSession>,
    catalog: Mutex<Option<Catalog>>,
    tools_changed: Arc<AtomicBool>,
}

impl Connection {
    fn is_closed(&self) -> bool {
        match &self.service {
            Some(service) => service.is_closed() || service.peer().is_transport_closed(),
            None => true,
        }
    }

    fn peer(&self) -> Result<Peer<RoleClient>, McpError> {
        self.service
            .as_ref()
            .map(|s| s.peer().clone())
            .ok_or(McpError::Closed)
    }

    fn cancel(&self) {
        if let Some(service) = &self.service {
            service.cancellation_token().cancel();
        }
    }
}

/// The automatic reconnect: off until [`AmpMcpClient::set_reconnector`].
struct ReconnectConfig {
    connector: Option<Reconnector>,
    interval: Duration,
}

/// A connected AMP MCP client. Share it behind an `Arc`; every method takes `&self`.
///
/// The connection inside is replaceable (#2159): with a [`Reconnector`] set, a call the server
/// rejects as "login required" (or a call on a closed connection) reconnects, at most once per
/// [`DEFAULT_RECONNECT_INTERVAL`], and on success retries on the new connection (a 401 means the
/// call never ran). Everyone holding the `Arc` (the model's tools, run reporting, the supervisor)
/// then uses the new connection. [`Self::replace`] and [`Self::reconnect`] do the same swap on
/// demand, e.g. to switch servers. A call still running on the old connection fails at once with
/// [`McpError::Replaced`]; it is never retried, since it may have run.
pub struct AmpMcpClient {
    conn: RwLock<Arc<Connection>>,
    call_timeout: Duration,
    catalog_default_ttl: Duration,
    /// One cell for every connection, so health watchers survive a swap.
    health: HealthCell,
    reconnect: StdMutex<ReconnectConfig>,
    /// Serialises reconnect attempts and holds when the last automatic one started.
    last_attempt: Mutex<Option<Instant>>,
    /// Bumped on every swap.
    generation: AtomicU64,
}

impl std::fmt::Debug for AmpMcpClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("AmpMcpClient")
            .field("session", &self.session())
            .field("call_timeout", &self.call_timeout)
            .field("health", &self.health.get())
            .field("generation", &self.generation())
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
            conn: RwLock::new(Arc::new(Connection {
                service: Some(service),
                session: Arc::new(session),
                catalog: Mutex::new(None),
                tools_changed,
            })),
            call_timeout: options.call_timeout,
            catalog_default_ttl: options.catalog_default_ttl,
            health: HealthCell::new(),
            reconnect: StdMutex::new(ReconnectConfig {
                connector: None,
                interval: DEFAULT_RECONNECT_INTERVAL,
            }),
            last_attempt: Mutex::new(None),
            generation: AtomicU64::new(0),
        })
    }

    /// A client that is not connected yet (#2178: the chat opens while AMP is unreachable). It
    /// reads as closed ([`Health::Disconnected`] with `reason`); [`Self::session`] has the URL and
    /// nothing from the server. Set a [`Reconnector`] and the first call connects, or connect
    /// elsewhere and [`Self::replace`]. Calls fail with [`McpError::Closed`] until then.
    pub fn offline(options: &ClientOptions, reason: impl Into<String>) -> Self {
        let health = HealthCell::new();
        health.set(Health::Disconnected {
            reason: reason.into(),
        });
        Self {
            conn: RwLock::new(Arc::new(Connection {
                service: None,
                session: Arc::new(ServerSession {
                    url: options.url.clone(),
                    mode: options.protocol,
                    protocol_version: ProtocolVersion::default(),
                    era: Era::of(&ProtocolVersion::default()),
                    fell_back: false,
                    server_name: None,
                    server_version: None,
                    instructions: None,
                    auth: options.auth.describe(),
                }),
                catalog: Mutex::new(None),
                tools_changed: Arc::new(AtomicBool::new(false)),
            })),
            call_timeout: options.call_timeout,
            catalog_default_ttl: options.catalog_default_ttl,
            health,
            reconnect: StdMutex::new(ReconnectConfig {
                connector: None,
                interval: DEFAULT_RECONNECT_INTERVAL,
            }),
            last_attempt: Mutex::new(None),
            generation: AtomicU64::new(0),
        }
    }

    /// False until a client made with [`Self::offline`] has connected.
    pub fn is_online(&self) -> bool {
        self.current().service.is_some()
    }

    /// The current connection. A call holds on to it until it ends, even across a swap.
    fn current(&self) -> Arc<Connection> {
        Arc::clone(&self.conn.read().unwrap_or_else(PoisonError::into_inner))
    }

    fn is_current(&self, conn: &Arc<Connection>) -> bool {
        Arc::ptr_eq(
            conn,
            &self.conn.read().unwrap_or_else(PoisonError::into_inner),
        )
    }

    /// What the server said when the current connection was made.
    pub fn session(&self) -> Arc<ServerSession> {
        Arc::clone(&self.current().session)
    }

    pub(crate) fn set_auth_description(&mut self, auth: String) {
        let conn = self.conn.get_mut().unwrap_or_else(PoisonError::into_inner);
        match Arc::get_mut(conn) {
            Some(conn) => Arc::make_mut(&mut conn.session).auth = auth,
            None => tracing::debug!("connection already shared; auth description not set"),
        }
    }

    /// AMP's server instructions, for the system prompt.
    pub fn instructions(&self) -> Option<String> {
        self.current().session.instructions.clone()
    }

    pub fn protocol_version(&self) -> ProtocolVersion {
        self.current().session.protocol_version.clone()
    }

    pub fn call_timeout(&self) -> Duration {
        self.call_timeout
    }

    /// Current health. A closed transport always reads as disconnected.
    pub fn health(&self) -> Health {
        if !self.is_online() {
            return match self.health.get() {
                h @ Health::Disconnected { .. } => h,
                _ => Health::Disconnected {
                    reason: "not connected yet".to_owned(),
                },
            };
        }
        if self.is_closed() {
            return Health::Disconnected {
                reason: "connection closed".to_owned(),
            };
        }
        self.health.get()
    }

    /// Watch health changes (the TUI header). The same receiver keeps working across swaps.
    pub fn watch_health(&self) -> watch::Receiver<Health> {
        self.health.subscribe()
    }

    pub fn is_closed(&self) -> bool {
        self.current().is_closed()
    }

    /// The rmcp peer of the current connection, for requests this crate does not wrap.
    ///
    /// # Panics
    ///
    /// On a client made with [`Self::offline`] that has not connected yet ([`Self::is_online`]).
    pub fn peer(&self) -> Peer<RoleClient> {
        self.current()
            .peer()
            .expect("peer() on an offline AMP client; check is_online() first")
    }

    /// How many times the connection has been replaced.
    pub fn generation(&self) -> u64 {
        self.generation.load(Ordering::Acquire)
    }

    /// Turns the automatic reconnect on (`Some`) or off (`None`). Leave it off for a static token
    /// (`AMP_TOKEN`): reconnecting can't fix a rejected token, the person sets a new one.
    pub fn set_reconnector(&self, connector: Option<Reconnector>) {
        self.reconnect_config().connector = connector;
    }

    /// The least time between two automatic reconnect attempts (default 15 s).
    pub fn set_reconnect_interval(&self, interval: Duration) {
        self.reconnect_config().interval = interval;
    }

    /// Whether a [`Reconnector`] is set.
    pub fn can_reconnect(&self) -> bool {
        self.reconnect_config().connector.is_some()
    }

    fn reconnect_config(&self) -> std::sync::MutexGuard<'_, ReconnectConfig> {
        self.reconnect
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
    }

    /// Reconnects now through the [`Reconnector`], ignoring the rate limit (after an explicit
    /// login, or after [`Self::set_reconnector`] pointed it at another server). On failure the
    /// current connection stays.
    pub async fn reconnect(&self) -> Result<(), McpError> {
        let connector = self.reconnect_config().connector.clone().ok_or_else(|| {
            McpError::Config("this connection has no reconnector (static token)".to_owned())
        })?;
        let mut last = self.last_attempt.lock().await;
        *last = Some(Instant::now());
        let client = run_connector(&connector).await?;
        self.adopt(client);
        Ok(())
    }

    /// Replaces the connection with `client`'s (made elsewhere, e.g. for another server). The
    /// timeouts, health watchers and reconnector stay this client's; `client`'s are dropped.
    pub fn replace(&self, client: AmpMcpClient) {
        self.adopt(client);
    }

    fn adopt(&self, client: AmpMcpClient) {
        let new = client
            .conn
            .into_inner()
            .unwrap_or_else(PoisonError::into_inner);
        let old = std::mem::replace(
            &mut *self.conn.write().unwrap_or_else(PoisonError::into_inner),
            new,
        );
        let generation = self.generation.fetch_add(1, Ordering::AcqRel) + 1;
        self.health.set(Health::Connected);
        tracing::debug!(
            generation,
            url = %self.session().url,
            "AMP MCP connection replaced"
        );
        // Calls still running on the old connection fail now (TransportClosed) instead of waiting
        // out their timeout; the service ends when the last of them lets go of it.
        old.cancel();
    }

    /// After `failed` could not be used (login required, or closed before a request was sent):
    /// true when a newer connection is now current, so the call may be sent again on it. Runs
    /// the reconnector if one is set and the last automatic attempt is old enough.
    async fn recover(&self, failed: &Arc<Connection>) -> bool {
        if !self.is_current(failed) {
            return true;
        }
        let Some((connector, interval)) = ({
            let config = self.reconnect_config();
            config.connector.clone().map(|c| (c, config.interval))
        }) else {
            return false;
        };
        // Single flight: callers that failed together wait for one attempt.
        let mut last = self.last_attempt.lock().await;
        if !self.is_current(failed) {
            return true;
        }
        if last.is_some_and(|at| at.elapsed() < interval) {
            return false;
        }
        *last = Some(Instant::now());
        match run_connector(&connector).await {
            Ok(client) => {
                self.adopt(client);
                true
            }
            Err(e) => {
                tracing::debug!(error = %e, "AMP reconnect failed");
                false
            }
        }
    }

    /// Runs `request` on the current connection. If it is closed, or the server says login
    /// required, [`Self::recover`] may swap in a new connection; then the request runs once more
    /// there. Neither case can have run the request on the server.
    async fn recovering<T, F, Fut>(&self, request: F) -> Result<T, McpError>
    where
        F: Fn(Arc<Connection>) -> Fut,
        Fut: Future<Output = Result<T, McpError>>,
    {
        let mut conn = self.current();
        if conn.is_closed() {
            if self.recover(&conn).await {
                conn = self.current();
            }
            if conn.is_closed() {
                let e = McpError::Closed;
                self.health.set(Health::after(Err(&e)));
                return Err(e);
            }
        }
        match request(Arc::clone(&conn)).await {
            Err(e) if e.is_auth_required() && self.recover(&conn).await => {
                request(self.current()).await
            }
            other => other,
        }
    }

    /// The tool catalog, refetched only when the cached one is stale (cache hint, list_changed).
    /// If a refetch fails and an older list exists, the older list is returned (SEP-2549 allows
    /// serving stale on error) and health shows the failure.
    pub async fn catalog(&self) -> Result<Catalog, McpError> {
        self.recovering(|conn| self.catalog_on(conn)).await
    }

    async fn catalog_on(&self, conn: Arc<Connection>) -> Result<Catalog, McpError> {
        let mut slot = conn.catalog.lock().await;
        let changed = conn.tools_changed.load(Ordering::Acquire);
        if let Some(catalog) = slot.as_ref()
            && !changed
            && catalog.is_fresh()
        {
            return Ok(catalog.clone());
        }
        match self.fetch_catalog(&conn).await {
            Ok(catalog) => {
                *slot = Some(catalog.clone());
                Ok(catalog)
            }
            Err(e) => {
                // Keep a pending list_changed for the next attempt.
                if changed {
                    conn.tools_changed.store(true, Ordering::Release);
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
        self.recovering(|conn| async move {
            let mut slot = conn.catalog.lock().await;
            let catalog = self.fetch_catalog(&conn).await?;
            *slot = Some(catalog.clone());
            Ok(catalog)
        })
        .await
    }

    async fn fetch_catalog(&self, conn: &Arc<Connection>) -> Result<Catalog, McpError> {
        // Cleared before the request, so a list_changed that lands mid-fetch is not lost.
        conn.tools_changed.store(false, Ordering::Release);
        let peer = conn.peer()?;
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
            .with_timeout(conn, "tools/list", self.call_timeout, fetch)
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
        let operation = format!("tools/call {name}");
        self.recovering(|conn| {
            let mut params = CallToolRequestParams::new(name.to_owned());
            if let Some(arguments) = arguments.clone() {
                params = params.with_arguments(arguments);
            }
            let operation = &operation;
            async move {
                let peer = conn.peer()?;
                self.with_timeout(&conn, operation, timeout, peer.call_tool(params))
                    .await
            }
        })
        .await
    }

    /// Runs one request on `conn` under `timeout`, maps its error and records health. Dropping the
    /// rmcp future on timeout sends `notifications/cancelled` for the request. Only the current
    /// connection's outcomes change the health; a call cut off by a swap fails as
    /// [`McpError::Replaced`].
    async fn with_timeout<T>(
        &self,
        conn: &Arc<Connection>,
        operation: &str,
        timeout: Duration,
        request: impl Future<Output = Result<T, rmcp::ServiceError>>,
    ) -> Result<T, McpError> {
        if conn.is_closed() {
            let e = McpError::Closed;
            if self.is_current(conn) {
                self.health.set(Health::after(Err(&e)));
            }
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
        if self.is_current(conn) {
            self.health.set(Health::after(result.as_ref().map(|_| ())));
            return result;
        }
        match result {
            Err(e) if !e.is_server_error() && !e.is_auth_required() => Err(McpError::Replaced {
                operation: operation.to_owned(),
            }),
            other => other,
        }
    }

    /// Closes the connection.
    pub async fn close(self) {
        let conn = self
            .conn
            .into_inner()
            .unwrap_or_else(PoisonError::into_inner);
        match Arc::try_unwrap(conn) {
            Ok(Connection {
                service: Some(service),
                ..
            }) => {
                let _ = service.cancel().await;
            }
            Ok(_) => {}
            Err(shared) => shared.cancel(),
        }
    }
}

/// One reconnect attempt, bounded by [`RECONNECT_TIMEOUT`].
async fn run_connector(connector: &Reconnector) -> Result<AmpMcpClient, McpError> {
    tokio::time::timeout(RECONNECT_TIMEOUT, connector())
        .await
        .map_err(|_| McpError::Timeout {
            operation: "reconnect".to_owned(),
            after: RECONNECT_TIMEOUT,
        })?
}
