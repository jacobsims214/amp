//! Errors from the AMP MCP client.

use std::{path::PathBuf, time::Duration};

use rmcp::{service::ClientInitializeError, service::ServiceError};

/// Everything that can go wrong talking to AMP over MCP.
///
/// The model-facing adapter (M2) turns any `Err` from [`crate::AmpMcpClient::call_tool`] into a tool
/// error the model can see; nothing here ever waits for a human.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum McpError {
    /// Settings could not be resolved (bad URL, unreadable config file, bad protocol name, ...).
    #[error("config: {0}")]
    Config(String),

    /// The extra root certificate file (`--ca-file` / `AMP_CA_FILE`) could not be used.
    #[error("CA file {path}: {reason}")]
    CaFile { path: PathBuf, reason: String },

    /// The HTTP client could not be built.
    #[error("http client: {0}")]
    Http(String),

    /// The server answered 401/403, or a stored login can no longer be refreshed: re-login needed.
    /// `challenge` is the `WWW-Authenticate` header when the server sent one; `login` starts from it.
    #[error("AMP login required: run `amp-harness login` (or set AMP_TOKEN)")]
    AuthRequired { challenge: Option<String> },

    /// A third-party MCP server (`[mcp.<name>]`) answered 401, or its stored OAuth login can no
    /// longer be refreshed. `reason` says what to do (log in, or fix the configured header or
    /// bearer variable) and never holds a secret; `challenge` is the `WWW-Authenticate` header.
    #[error("MCP server `{server}`: {reason}")]
    ServerAuth {
        server: String,
        reason: String,
        challenge: Option<String>,
    },

    /// The OAuth flow failed (discovery, registration, the browser step, the token exchange).
    #[error("OAuth: {0}")]
    OAuth(String),

    /// The keychain or credentials file could not be read or written.
    #[error("credential store: {0}")]
    CredentialStore(String),

    /// The MCP lifecycle (discover and/or initialize) failed. The message includes the causes
    /// (TLS, DNS, connection refused), which rmcp's own message leaves out.
    #[error("connect to {url}: {source}{}", causes(source.as_ref()))]
    Connect {
        url: String,
        #[source]
        source: Box<ClientInitializeError>,
    },

    /// Connecting took longer than the connect timeout.
    #[error("connect to {url}: no answer within {after:?}")]
    ConnectTimeout { url: String, after: Duration },

    /// A request did not finish within its timeout. The request was cancelled.
    #[error("{operation} timed out after {after:?}")]
    Timeout { operation: String, after: Duration },

    /// A request failed after the connection was established.
    #[error("{operation}: {source}{}", causes(source.as_ref()))]
    Service {
        operation: String,
        #[source]
        source: Box<ServiceError>,
    },

    /// The connection is closed; reconnect.
    #[error("MCP connection is closed")]
    Closed,

    /// The connection was replaced (a reconnect after `amp-harness login`, or a server switch)
    /// while this request was running, and the old one was closed under it. The request may or
    /// may not have reached the server, so it is not retried.
    #[error(
        "{operation}: the AMP connection was replaced while this call ran (re-login or server switch); it may or may not have run, so check before retrying"
    )]
    Replaced { operation: String },
}

impl McpError {
    /// True for a per-call (or connect) timeout.
    pub fn is_timeout(&self) -> bool {
        matches!(self, Self::Timeout { .. } | Self::ConnectTimeout { .. })
    }

    /// True when the server rejected our credentials (or we sent none).
    pub fn is_auth_required(&self) -> bool {
        matches!(self, Self::AuthRequired { .. } | Self::ServerAuth { .. })
    }

    /// True when the server answered the request with a JSON-RPC error: AMP's tools return one
    /// for an unknown ticket, bad arguments or a failed handler. The call failed, but the
    /// connection is fine, so it does not count against [`crate::Health`].
    pub fn is_server_error(&self) -> bool {
        matches!(self, Self::Service { source, .. } if matches!(**source, ServiceError::McpError(_)))
    }

    pub(crate) fn from_init(url: &str, error: ClientInitializeError) -> Self {
        if error.is_authorization_required() {
            return Self::AuthRequired {
                challenge: error.auth_challenge().map(str::to_owned),
            };
        }
        Self::Connect {
            url: url.to_owned(),
            source: Box::new(error),
        }
    }

    pub(crate) fn from_service(operation: &str, error: ServiceError) -> Self {
        // `ServiceError::TransportSend` doesn't expose its transport error as `source()`, so a
        // mid-session 401 is only found by starting the walk at the transport error itself.
        let start: &(dyn std::error::Error + 'static) = match &error {
            ServiceError::TransportSend(transport) => transport,
            other => other,
        };
        if let Some(challenge) = auth_challenge_in_chain(start) {
            return Self::AuthRequired { challenge };
        }
        if matches!(error, ServiceError::TransportClosed) {
            return Self::Closed;
        }
        Self::Service {
            operation: operation.to_owned(),
            source: Box::new(error),
        }
    }
}

/// Why talking to AMP failed, in the terms a person can act on (#2178). [`McpError::kind`] reads it
/// from the error and its whole cause chain, so callers never show rmcp's or reqwest's own text
/// (type names, `Send message error Transport [...]`) unless asked for it (`--verbose`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[non_exhaustive]
pub enum FailureKind {
    /// Nothing listens on that host and port.
    Refused,
    /// The host name does not resolve.
    HostNotFound,
    /// TLS failed: an untrusted or wrong certificate, or TLS spoken to a plain HTTP port.
    Tls,
    /// The server rejected our credentials (401/403), or we sent none.
    LoginRequired,
    /// The server answered, but that path is not an MCP endpoint (404).
    NotFound,
    /// The server answered with something that is not MCP (an HTTP error page, HTML, a non
    /// JSON-RPC body): most likely the wrong address.
    NotMcp,
    /// No answer in time.
    Timeout,
    /// The connection was closed (the server went away mid-session).
    Closed,
    /// The settings themselves are wrong (a bad URL, an unreadable CA file).
    Config,
    /// Anything else; [`McpError::reason`] says what.
    Other,
}

impl McpError {
    /// What went wrong, read from the error and its cause chain (#2178).
    pub fn kind(&self) -> FailureKind {
        match self {
            Self::AuthRequired { .. } | Self::ServerAuth { .. } => {
                return FailureKind::LoginRequired;
            }
            Self::ConnectTimeout { .. } | Self::Timeout { .. } => return FailureKind::Timeout,
            Self::Closed | Self::Replaced { .. } => return FailureKind::Closed,
            Self::Config(_) | Self::CaFile { .. } => return FailureKind::Config,
            Self::Http(_) | Self::OAuth(_) | Self::CredentialStore(_) => return FailureKind::Other,
            Self::Connect { .. } | Self::Service { .. } => {}
        }
        if self.is_server_error() {
            return FailureKind::Other;
        }
        let mut current: Option<&(dyn std::error::Error + 'static)> = Some(self);
        let mut text = String::new();
        while let Some(e) = current {
            if let Some(io) = e.downcast_ref::<std::io::Error>() {
                match io.kind() {
                    std::io::ErrorKind::ConnectionRefused => return FailureKind::Refused,
                    std::io::ErrorKind::TimedOut => return FailureKind::Timeout,
                    _ => {}
                }
            }
            if let Some(re) = e.downcast_ref::<reqwest::Error>() {
                if re.is_timeout() {
                    return FailureKind::Timeout;
                }
                if re.status() == Some(reqwest::StatusCode::NOT_FOUND) {
                    return FailureKind::NotFound;
                }
            }
            text.push_str(&e.to_string().to_lowercase());
            text.push('\n');
            current = next_cause(e);
        }
        classify_text(&text)
    }

    /// One short phrase for [`Self::kind`] with no Rust type names, e.g. "connection refused",
    /// "host not found", "no answer within 30s". The full chain stays in `Display`
    /// (`to_string()`), for `--verbose` and logs.
    pub fn reason(&self) -> String {
        match self.kind() {
            FailureKind::Refused => "connection refused".to_owned(),
            FailureKind::HostNotFound => "host not found".to_owned(),
            FailureKind::Tls => {
                "TLS failed: the server's certificate is not trusted, or it does not speak HTTPS"
                    .to_owned()
            }
            FailureKind::LoginRequired => "login required".to_owned(),
            FailureKind::NotFound => "no MCP endpoint at that path (HTTP 404)".to_owned(),
            FailureKind::NotMcp => match self.http_status() {
                Some(status) => format!("it answered HTTP {status}, not MCP"),
                None => "it did not answer as an MCP server".to_owned(),
            },
            FailureKind::Timeout => match self {
                Self::ConnectTimeout { after, .. } | Self::Timeout { after, .. } => {
                    format!("no answer within {}s", after.as_secs().max(1))
                }
                _ => "no answer in time".to_owned(),
            },
            FailureKind::Closed => "the connection was closed".to_owned(),
            FailureKind::Config => match self {
                Self::Config(msg) => msg.clone(),
                other => other.to_string(),
            },
            FailureKind::Other => match self {
                // The server's own error message is meant for people; rmcp's wrappers are not.
                Self::Service { source, .. } => match &**source {
                    ServiceError::McpError(data) => data.message.to_string(),
                    _ => "the request failed".to_owned(),
                },
                Self::Connect { .. } => "the server did not complete the MCP handshake".to_owned(),
                other => other.to_string(),
            },
        }
    }
}

impl McpError {
    /// The HTTP status in rmcp's "unexpected server response: HTTP 501 Not Implemented: …", if any.
    pub(crate) fn http_status(&self) -> Option<String> {
        let mut current: Option<&(dyn std::error::Error + 'static)> = Some(self);
        while let Some(e) = current {
            let text = e.to_string();
            if let Some(at) = text.find("HTTP ") {
                let code: String = text[at + 5..].chars().take(3).collect();
                if code.len() == 3 && code.chars().all(|c| c.is_ascii_digit()) {
                    return Some(code);
                }
            }
            current = next_cause(e);
        }
        None
    }
}

/// [`McpError::kind`] from the lowercased messages of the whole chain, for the causes that are
/// only visible as text (hyper's DNS error, rustls' certificate errors, rmcp's `HTTP 404`).
fn classify_text(text: &str) -> FailureKind {
    let has = |needles: &[&str]| needles.iter().any(|n| text.contains(n));
    if has(&[
        "connection refused",
        "os error 61",
        "os error 111",
        "os error 10061",
    ]) {
        FailureKind::Refused
    } else if has(&[
        "dns error",
        "failed to lookup address",
        "nodename nor servname",
        "name or service not known",
        "no such host",
        "name resolution",
    ]) {
        FailureKind::HostNotFound
    } else if has(&[
        "certificate",
        "invalid peer",
        "tls handshake",
        "handshake failure",
        "invalidcontenttype",
        "corrupt message",
        "received corrupt",
        "unknownissuer",
        "unknown issuer",
    ]) {
        FailureKind::Tls
    } else if has(&["http 401", "http 403", "401 unauthorized", "403 forbidden"]) {
        FailureKind::LoginRequired
    } else if has(&["http 404", "404 not found"]) {
        FailureKind::NotFound
    } else if has(&[
        "unexpected server response",
        "unexpected content type",
        "deserialize error",
        "http 4",
        "http 5",
    ]) {
        FailureKind::NotMcp
    } else if has(&["timed out", "deadline has elapsed", "operation timed out"]) {
        FailureKind::Timeout
    } else if has(&[
        "connection reset",
        "connection closed",
        "broken pipe",
        "transport closed",
        "channel closed",
        "unexpected end of stream",
    ]) {
        FailureKind::Closed
    } else {
        FailureKind::Other
    }
}

/// The error's source chain as `: cause: deeper cause`, skipping messages already shown.
///
/// rmcp's transport errors only say "error sending request for url (...)"; the reason (an untrusted
/// certificate, a refused connection, a DNS failure) is further down, behind links rmcp doesn't
/// expose through `source()` (see [`next_cause`]).
fn causes(error: &(dyn std::error::Error + 'static)) -> String {
    let mut seen = error.to_string();
    let mut out = String::new();
    let mut current = next_cause(error);
    while let Some(e) = current {
        let msg = e.to_string();
        if !msg.is_empty() && !seen.contains(&msg) {
            out.push_str(": ");
            out.push_str(&msg);
            seen.push_str(&msg);
        }
        current = next_cause(e);
    }
    out
}

/// `source()`, plus the links rmcp 3.6.0 leaves out of it: `ClientInitializeError::TransportError`
/// and `LegacyFallbackFailed`,
/// `ServiceError::TransportSend` and `StreamableHttpError::Client` hold their cause in a field that
/// is not marked `#[source]`.
fn next_cause<'a>(
    error: &'a (dyn std::error::Error + 'static),
) -> Option<&'a (dyn std::error::Error + 'static)> {
    use rmcp::transport::streamable_http_client::StreamableHttpError;
    if let Some(ClientInitializeError::TransportError { error, .. }) = error.downcast_ref() {
        return Some(error);
    }
    // Auto's fallback: discover failed, then so did `initialize`; the latter says why (#2178).
    if let Some(ClientInitializeError::LegacyFallbackFailed { fallback, .. }) = error.downcast_ref()
    {
        return Some(&**fallback);
    }
    if let Some(ServiceError::TransportSend(transport)) = error.downcast_ref() {
        return Some(transport);
    }
    if let Some(StreamableHttpError::<reqwest::Error>::Client(client)) = error.downcast_ref() {
        return Some(client);
    }
    error.source()
}

/// Walks an error's source chain for the transport's 401/403 errors.
/// `Some(None)` = authorization required without a challenge header.
fn auth_challenge_in_chain(error: &(dyn std::error::Error + 'static)) -> Option<Option<String>> {
    use rmcp::transport::{
        auth::AuthError,
        streamable_http_client::{AuthRequiredError, InsufficientScopeError},
    };
    let mut current = Some(error);
    while let Some(e) = current {
        if let Some(auth) = e.downcast_ref::<AuthRequiredError>() {
            return Some(Some(auth.www_authenticate_header.clone()));
        }
        if let Some(scope) = e.downcast_ref::<InsufficientScopeError>() {
            return Some(Some(scope.www_authenticate_header.clone()));
        }
        if matches!(
            e.downcast_ref::<AuthError>(),
            Some(AuthError::AuthorizationRequired)
        ) {
            return Some(None);
        }
        current = e.source();
    }
    None
}
