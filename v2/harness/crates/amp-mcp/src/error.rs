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
}

impl McpError {
    /// True for a per-call (or connect) timeout.
    pub fn is_timeout(&self) -> bool {
        matches!(self, Self::Timeout { .. } | Self::ConnectTimeout { .. })
    }

    /// True when the server rejected our credentials (or we sent none).
    pub fn is_auth_required(&self) -> bool {
        matches!(self, Self::AuthRequired { .. })
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

/// `source()`, plus the links rmcp 3.6.0 leaves out of it: `ClientInitializeError::TransportError`,
/// `ServiceError::TransportSend` and `StreamableHttpError::Client` hold their cause in a field that
/// is not marked `#[source]`.
fn next_cause<'a>(
    error: &'a (dyn std::error::Error + 'static),
) -> Option<&'a (dyn std::error::Error + 'static)> {
    use rmcp::transport::streamable_http_client::StreamableHttpError;
    if let Some(ClientInitializeError::TransportError { error, .. }) = error.downcast_ref() {
        return Some(error);
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
