//! Streamable HTTP for any MCP server (H4, D1/D2): static headers, secret headers, a bearer token,
//! and MCP's OAuth (discovery, dynamic or pre-registered client, PKCE, refresh), on the same HTTP
//! client, OAuth code and credential store as AMP's own connection.
//!
//! Secret values arrive already fetched ([`Secret`]): from the keychain (`secret_headers`, the OAuth
//! client secret) or the process env (`bearer_env`, `${env:..}` in headers); amp-core's
//! `mcp_http::http_options` does that from a resolved `[mcp.<name>]` table. They go on the wire
//! as request headers only, marked sensitive, and are never logged or put in an error.
//!
//! Which authorization a connection uses ([`prepare`]):
//! 1. A configured `Authorization` (`bearer_env`, or an `Authorization` header) always wins; a
//!    stored OAuth login is not touched, and a 401 means the configured value is wrong.
//! 2. `auth = "none"`: headers only.
//! 3. Otherwise a stored OAuth login for the URL (keyed by [`crate::credential_key`], beside AMP's
//!    logins) goes through rmcp's `AuthClient`, which refreshes before expiry and once on a 401.
//! 4. No login: `auto` connects without one (a 401 is [`McpError::ServerAuth`], "login
//!    required"); `oauth` fails at once with that error, without a request.
//!
//! [`login`] is the browser login for one server (H9's `amp-harness mcp login <name>`).
//!
//! Legacy HTTP+SSE (H5, #2238, [`super::sse`]) shares all of this: [`connect`] uses it for
//! `type = "sse"` ([`HttpServerOptions::legacy_sse`]), and for `type = "http"` when the server
//! refuses the Streamable HTTP POST with 400, 404 or 405 ([`wants_sse_fallback`]) unless
//! `fallback_sse = false`. If the fallback finds no SSE server either, the Streamable HTTP error
//! is the one reported.

use std::{collections::HashMap, path::PathBuf, time::Duration};

use reqwest::header::{HeaderName, HeaderValue};
use rmcp::transport::{
    StreamableHttpClientTransport,
    auth::{AuthClient, OAuthClientConfig},
    streamable_http_client::StreamableHttpClientTransportConfig,
};

use super::{ExternalClient, ExternalOptions, Secret, auth_hint};
use crate::{
    CredentialBackend, McpError,
    http::{DEFAULT_OAUTH_REQUEST_TIMEOUT, build_http_client},
    oauth::{
        self, LoginOptions, LoginOutcome, LoginPrompt, OAuthClientSettings, fetch_metadata,
        new_manager, oauth_error,
    },
};

/// `auth` in `[mcp.<name>]`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum HttpAuth {
    /// Use a stored OAuth login when there is one; a 401 without one means "log in".
    #[default]
    Auto,
    /// Always OAuth: without a stored login, fail before connecting.
    OAuth,
    /// Never OAuth: headers (and `bearer_env`) only.
    None,
}

/// Where a header value came from, for `mcp show` / `doctor` and [`super::ExternalSession::auth`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum HeaderSource {
    /// Written in the config (non-secret, or expanded from `${env:..}` references).
    Config,
    /// `secret_headers`: the keychain.
    Keychain,
    /// The process environment variable named.
    Env(String),
}

/// One request header.
#[derive(Debug, Clone)]
pub struct HttpHeader {
    pub name: String,
    pub value: Secret,
    pub source: HeaderSource,
}

impl HttpHeader {
    pub fn new(name: impl Into<String>, value: impl Into<String>, source: HeaderSource) -> Self {
        Self {
            name: name.into(),
            value: Secret::new(value),
            source,
        }
    }
}

/// `Authorization: Bearer <token>` (`bearer_env`).
#[derive(Debug, Clone)]
pub struct BearerToken {
    pub token: Secret,
    /// Where it came from, e.g. `env GITHUB_TOKEN`.
    pub source: String,
}

/// Everything [`connect`] and [`login`] need for one server.
#[derive(Clone)]
pub struct HttpServerOptions {
    /// The configured server name (`[mcp.<name>]`), used in messages.
    pub name: String,
    /// Timeouts, protocol mode (`Auto` by default for HTTP) and client info.
    pub external: ExternalOptions,
    pub url: String,
    /// Every header to send, secret or not, in order.
    pub headers: Vec<HttpHeader>,
    pub bearer: Option<BearerToken>,
    pub auth: HttpAuth,
    /// Pre-registered client, scopes, callback port, metadata URL.
    pub oauth: OAuthClientSettings,
    /// Extra root certificates (PEM bundle) for the server and its authorization server.
    pub ca_file: Option<PathBuf>,
    /// Connect through this unix socket instead of TCP (unix only).
    pub socket: Option<PathBuf>,
    /// Retry as legacy HTTP+SSE when Streamable HTTP is refused (H5).
    pub fallback_sse: bool,
    /// `type = "sse"`: connect over legacy HTTP+SSE only (`url` is the event stream's URL).
    pub legacy_sse: bool,
    /// Bound on each OAuth request (discovery, refresh).
    pub oauth_request_timeout: Duration,
    /// Where the OAuth login is kept ([`crate::scoped_credential_key`]): `None` for the person's
    /// own servers (the URL alone, beside AMP's login), a repo server's own scope otherwise, so
    /// it never uses a login the person made for the same URL (#2308).
    pub credential_scope: Option<String>,
}

impl HttpServerOptions {
    pub fn new(server: impl Into<String>, url: impl Into<String>) -> Self {
        Self {
            name: server.into(),
            external: ExternalOptions {
                protocol: crate::ProtocolMode::Auto,
                ..ExternalOptions::default()
            },
            url: url.into(),
            headers: Vec::new(),
            bearer: None,
            auth: HttpAuth::Auto,
            oauth: OAuthClientSettings::default(),
            ca_file: None,
            socket: None,
            fallback_sse: true,
            legacy_sse: false,
            oauth_request_timeout: DEFAULT_OAUTH_REQUEST_TIMEOUT,
            credential_scope: None,
        }
    }

    pub fn with_header(mut self, header: HttpHeader) -> Self {
        self.headers.push(header);
        self
    }

    pub fn with_bearer(mut self, token: impl Into<String>, source: impl Into<String>) -> Self {
        self.bearer = Some(BearerToken {
            token: Secret::new(token),
            source: source.into(),
        });
        self
    }

    pub fn with_auth(mut self, auth: HttpAuth) -> Self {
        self.auth = auth;
        self
    }

    pub fn with_oauth(mut self, oauth: OAuthClientSettings) -> Self {
        self.oauth = oauth;
        self
    }

    pub fn with_ca_file(mut self, ca_file: impl Into<PathBuf>) -> Self {
        self.ca_file = Some(ca_file.into());
        self
    }

    pub fn with_socket(mut self, socket: impl Into<PathBuf>) -> Self {
        self.socket = Some(socket.into());
        self
    }

    /// `type = "sse"`: legacy HTTP+SSE only, no Streamable HTTP attempt.
    pub fn with_legacy_sse(mut self) -> Self {
        self.legacy_sse = true;
        self
    }

    pub fn server(&self) -> &str {
        &self.name
    }

    /// Whether the config itself supplies `Authorization` (bearer or header).
    pub fn has_static_authorization(&self) -> bool {
        self.bearer.is_some()
            || self
                .headers
                .iter()
                .any(|h| h.name.eq_ignore_ascii_case("authorization"))
    }

    /// The key this server's OAuth login is stored under.
    pub fn credential_key(&self) -> String {
        crate::scoped_credential_key(&self.url, self.credential_scope.as_deref())
    }

    /// The [`LoginOptions`] for this server's OAuth login.
    pub fn login_options(&self, backend: CredentialBackend) -> LoginOptions {
        LoginOptions::new(self.url.clone(), backend)
            .with_credential_scope(self.credential_scope.clone())
            .with_ca_file(self.ca_file.clone())
            .with_request_timeout(self.oauth_request_timeout)
            .with_client(self.oauth.clone())
            .with_mcp_server(self.name.clone())
    }
}

// Header values may be secrets: names only, URL as origin.
impl std::fmt::Debug for HttpServerOptions {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HttpServerOptions")
            .field("server", &self.name)
            .field("url", &crate::url_origin(&self.url))
            .field(
                "headers",
                &self.headers.iter().map(|h| &h.name).collect::<Vec<_>>(),
            )
            .field("bearer", &self.bearer.as_ref().map(|b| &b.source))
            .field("auth", &self.auth)
            .field("oauth", &self.oauth)
            .field("socket", &self.socket)
            .field("fallback_sse", &self.fallback_sse)
            .field("legacy_sse", &self.legacy_sse)
            .finish_non_exhaustive()
    }
}

/// A server's HTTP client, headers and authorization, ready for a transport: Streamable HTTP
/// here, legacy SSE in [`super::sse`].
#[derive(Clone)]
pub struct PreparedHttp {
    pub url: String,
    /// The plain client (CA file, same-origin redirects, unix socket).
    pub http: reqwest::Client,
    /// Every configured header (an `Authorization` header included), values marked sensitive.
    pub headers: HashMap<HeaderName, HeaderValue>,
    /// `Authorization: Bearer` from `bearer_env`.
    pub bearer: Option<Secret>,
    /// A stored OAuth login, through rmcp's refreshing client.
    pub oauth: Option<AuthClient<reqwest::Client>>,
    /// How we authenticate, for [`super::ExternalSession::auth`]. No secrets.
    pub auth: String,
    /// `http` or `http (unix socket)`.
    pub transport: &'static str,
}

impl std::fmt::Debug for PreparedHttp {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PreparedHttp")
            .field("url", &crate::url_origin(&self.url))
            .field("headers", &self.headers.keys().collect::<Vec<_>>())
            .field("auth", &self.auth)
            .field("transport", &self.transport)
            .finish_non_exhaustive()
    }
}

/// Headers rmcp sets itself and refuses as custom headers.
const RESERVED_HEADERS: [&str; 4] = [
    "accept",
    "mcp-session-id",
    "mcp-protocol-version",
    "last-event-id",
];

/// Checks the options, builds the client and headers, and picks the authorization (see the
/// module docs). Loads the stored OAuth login, if any, through `backend`.
pub async fn prepare(
    options: &HttpServerOptions,
    backend: &CredentialBackend,
) -> Result<PreparedHttp, McpError> {
    let server = options.server();
    let config_error = |msg: String| McpError::Config(format!("MCP server `{server}`: {msg}"));
    let lower = options.url.to_ascii_lowercase();
    if !(lower.starts_with("https://") || lower.starts_with("http://"))
        || reqwest::Url::parse(&options.url).is_err()
    {
        return Err(config_error(
            "url must be an http:// or https:// URL".into(),
        ));
    }

    let mut headers = HashMap::new();
    for header in &options.headers {
        let name = HeaderName::from_bytes(header.name.as_bytes())
            .map_err(|_| config_error(format!("header `{}`: not a valid name", header.name)))?;
        if RESERVED_HEADERS.contains(&name.as_str()) {
            return Err(config_error(format!(
                "header `{}` is set by the MCP transport itself",
                header.name
            )));
        }
        let mut value = HeaderValue::from_str(header.value.expose()).map_err(|_| {
            // Never echo the value: it may be a secret.
            config_error(format!(
                "header `{}`: the value has characters a header cannot carry",
                header.name
            ))
        })?;
        value.set_sensitive(true);
        if headers.insert(name, value).is_some() {
            return Err(config_error(format!(
                "header `{}` is given twice",
                header.name
            )));
        }
    }
    let static_authorization = options.has_static_authorization();
    if options.bearer.is_some() && headers.contains_key(&reqwest::header::AUTHORIZATION) {
        return Err(config_error(
            "bearer_env and an Authorization header both set Authorization; keep one".into(),
        ));
    }
    if static_authorization && options.auth == HttpAuth::OAuth {
        return Err(config_error(
            "auth = \"oauth\" conflicts with a configured Authorization (bearer_env or header)"
                .into(),
        ));
    }

    let (http, transport) = match &options.socket {
        None => (build_http_client(options.ca_file.as_deref())?, "http"),
        #[cfg(unix)]
        Some(socket) => (
            crate::http::build_unix_socket_http_client(options.ca_file.as_deref(), socket)?,
            "http (unix socket)",
        ),
        #[cfg(not(unix))]
        Some(_) => {
            return Err(config_error(
                "socket is only supported on unix systems".into(),
            ));
        }
    };

    let mut auth_parts: Vec<String> = options
        .headers
        .iter()
        .filter_map(|h| match &h.source {
            HeaderSource::Config => None,
            HeaderSource::Keychain => Some(format!("header {} (keychain)", h.name)),
            HeaderSource::Env(var) => Some(format!("header {} (env {var})", h.name)),
        })
        .collect();
    if let Some(bearer) = &options.bearer {
        auth_parts.push(format!("bearer ({})", bearer.source));
    }

    let oauth = if static_authorization || options.auth == HttpAuth::None {
        None
    } else {
        stored_login(options, backend).await?
    };
    if let Some(_client) = &oauth {
        auth_parts.push(format!("oauth ({})", backend.describe()));
    } else if options.auth == HttpAuth::OAuth {
        return Err(McpError::ServerAuth {
            server: server.to_owned(),
            reason: auth_hint(server, "oauth"),
            challenge: None,
        });
    }

    Ok(PreparedHttp {
        url: options.url.clone(),
        http,
        headers,
        bearer: options.bearer.as_ref().map(|b| b.token.clone()),
        oauth,
        auth: if auth_parts.is_empty() {
            "none".to_owned()
        } else {
            auth_parts.join(", ")
        },
        transport,
    })
}

/// The stored OAuth login for the server's URL, ready to use, or `None` when there is none.
/// A login the authorization server no longer matches (issuer changed) is a
/// [`McpError::ServerAuth`]: log in again.
async fn stored_login(
    options: &HttpServerOptions,
    backend: &CredentialBackend,
) -> Result<Option<AuthClient<reqwest::Client>>, McpError> {
    let store = backend
        .store_for_key(options.credential_key())
        .for_mcp_server(options.server());
    let Some(stored) = store.load_credentials().await? else {
        return Ok(None);
    };
    if stored.token_response.is_none() {
        return Ok(None);
    }
    let server = options.server();
    let relogin = || McpError::ServerAuth {
        server: server.to_owned(),
        reason: auth_hint(server, "oauth"),
        challenge: None,
    };
    let mut manager = new_manager(
        &options.url,
        options.ca_file.as_deref(),
        options.oauth_request_timeout,
    )
    .await?;
    manager.set_credential_store(store);
    if let Some(url) = &options.oauth.auth_server_metadata_url {
        manager.set_metadata(
            fetch_metadata(
                url,
                options.ca_file.as_deref(),
                options.oauth_request_timeout,
            )
            .await?,
        );
    }
    if !manager
        .initialize_from_store()
        .await
        .map_err(|e| match oauth_error(e) {
            McpError::AuthRequired { .. } => relogin(),
            other => other,
        })?
    {
        return Err(relogin());
    }
    // A confidential pre-registered client needs its secret to refresh. rmcp reloads only the
    // client id from the store, so configure the secret again when the ids match.
    if let (Some(id), Some(secret)) = (&options.oauth.client_id, &options.oauth.client_secret)
        && *id == stored.client_id
    {
        manager
            .configure_client(
                OAuthClientConfig::new(id.clone(), options.url.clone())
                    .with_client_secret(secret.expose().to_owned()),
            )
            .map_err(oauth_error)?;
    }
    let http = build_http_client(options.ca_file.as_deref())?;
    Ok(Some(AuthClient::new(http, manager)))
}

/// [`prepare`] under the connect timeout (a stalled OAuth discovery must not hang a connect).
pub(crate) async fn prepare_bounded(
    options: &HttpServerOptions,
    backend: &CredentialBackend,
) -> Result<PreparedHttp, McpError> {
    tokio::time::timeout(options.external.connect_timeout, prepare(options, backend))
        .await
        .map_err(|_| McpError::Timeout {
            operation: format!("MCP server `{}`: OAuth discovery", options.server()),
            after: options.external.connect_timeout,
        })?
}

/// Connects to a remote server and runs the MCP lifecycle in `options.external.protocol`
/// (2026-07-28 `server/discover`, or `initialize` at any version from 2024-11-05 to 2025-11-25
/// that the server picks).
///
/// - `legacy_sse` (`type = "sse"`): legacy HTTP+SSE only ([`super::sse::connect`]).
/// - Otherwise Streamable HTTP; when the server refuses its POST with 400, 404 or 405 and
///   `fallback_sse` is on, legacy HTTP+SSE at the same URL with the same headers and
///   credentials. If that server doesn't speak SSE either, the Streamable HTTP error is returned;
///   if it does but fails (a rejection, a broken handshake), that error is.
pub async fn connect(
    options: &HttpServerOptions,
    backend: &CredentialBackend,
) -> Result<ExternalClient, McpError> {
    let prepared = prepare_bounded(options, backend).await?;
    let name = options.server();
    if options.legacy_sse {
        return super::sse::connect_prepared(name, &options.external, &prepared).await;
    }
    match connect_prepared(name, &options.external, &prepared).await {
        Err(e) if options.fallback_sse && wants_sse_fallback(&e) => {
            tracing::debug!(
                server = name,
                status = e.http_status().as_deref().unwrap_or(""),
                "Streamable HTTP refused; trying legacy HTTP+SSE"
            );
            match super::sse::attempt(name, &options.external, &prepared).await {
                Ok(client) => Ok(client),
                Err(failure) if failure.not_sse => Err(e),
                Err(failure) => Err(failure.error),
            }
        }
        other => other,
    }
}

/// The Streamable HTTP lifecycle on an already prepared client.
pub async fn connect_prepared(
    name: &str,
    external: &ExternalOptions,
    prepared: &PreparedHttp,
) -> Result<ExternalClient, McpError> {
    let mut config = StreamableHttpClientTransportConfig::with_uri(prepared.url.clone())
        // Legacy sessions: transparently re-initialize if the server forgot ours (restart).
        .reinit_on_expired_session(true)
        .custom_headers(prepared.headers.clone());
    if let Some(token) = &prepared.bearer {
        config = config.auth_header(token.expose());
    }
    let target = crate::url_origin(&prepared.url);
    let (kind, auth) = (prepared.transport, prepared.auth.as_str());
    match &prepared.oauth {
        Some(client) => {
            ExternalClient::handshake_with(name, &target, kind, auth, external, || {
                Ok(StreamableHttpClientTransport::with_client(
                    client.clone(),
                    config.clone(),
                ))
            })
            .await
        }
        None => {
            ExternalClient::handshake_with(name, &target, kind, auth, external, || {
                Ok(StreamableHttpClientTransport::with_client(
                    prepared.http.clone(),
                    config.clone(),
                ))
            })
            .await
        }
    }
}

/// Whether a failed Streamable HTTP connect should be retried as legacy HTTP+SSE: the spec's
/// backwards-compatibility rule is a POST `initialize` answered 400, 404 or 405.
pub fn wants_sse_fallback(error: &McpError) -> bool {
    matches!(error, McpError::Connect { .. })
        && matches!(error.http_status().as_deref(), Some("400" | "404" | "405"))
}

/// The browser login for one server: probes it without OAuth (with its headers) for the 401
/// challenge, then runs discovery, registration (or the pre-registered client), PKCE and the
/// token exchange, and stores the login under the server URL in `backend`.
pub async fn login<F>(
    options: &HttpServerOptions,
    backend: CredentialBackend,
    timeout: Duration,
    prompt: F,
) -> Result<LoginOutcome, McpError>
where
    F: FnOnce(&LoginPrompt),
{
    let server = options.server();
    if options.auth == HttpAuth::None {
        return Err(McpError::Config(format!(
            "MCP server `{server}` has auth = \"none\"; there is nothing to log in to"
        )));
    }
    if options.has_static_authorization() {
        return Err(McpError::Config(format!(
            "MCP server `{server}` sends a configured Authorization (bearer_env or header), which \
             always wins over a login; remove it to use OAuth"
        )));
    }
    let login_options = options.login_options(backend).with_timeout(timeout);
    // The probe: the server's headers, no credentials, no stored login.
    let probe = HttpServerOptions {
        auth: HttpAuth::None,
        external: ExternalOptions {
            connect_timeout: options.oauth_request_timeout,
            ..options.external.clone()
        },
        ..options.clone()
    };
    let challenge = match connect(&probe, &login_options.backend).await {
        Ok(client) => {
            client.shutdown().await;
            return Err(McpError::OAuth(format!(
                "MCP server `{server}` accepts requests without credentials, so there is nothing \
                 to log in to"
            )));
        }
        Err(McpError::ServerAuth { challenge, .. }) => challenge,
        Err(e) => return Err(e),
    };
    oauth::login_from_challenge(&login_options, challenge, prompt).await
}

/// Removes the stored login for the server. `Ok(true)` if there was one.
pub async fn logout(
    options: &HttpServerOptions,
    backend: &CredentialBackend,
) -> Result<bool, McpError> {
    backend
        .store_for_key(options.credential_key())
        .for_mcp_server(options.server())
        .clear_credentials()
        .await
}
