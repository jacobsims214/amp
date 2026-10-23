//! OAuth login for AMP (D3), on rmcp's `auth` state machine.
//!
//! `login`: 401 challenge → RFC 9728 protected-resource metadata → RFC 8414 authorization-server
//! metadata → dynamic client registration (AMP's amp-mcpoauth broker) → PKCE S256 authorization in
//! the browser with a loopback redirect on `127.0.0.1:<random port>` → token exchange (with
//! `offline_access`, so there is a refresh token) → saved to the [`CredentialBackend`] under the
//! server URL.
//!
//! [`connect`]: `AMP_TOKEN` wins; otherwise a stored login is used through rmcp's `AuthClient`,
//! which refreshes the access token before it expires and once more if the server rejects it.
//! When refreshing is impossible the call fails with [`McpError::AuthRequired`] ("re-login needed").
//!
//! Token values are never logged, printed or put in an error. Don't turn on `debug` tracing for
//! `rmcp::transport::auth`: rmcp logs the authorization code and token response there.

use std::{path::PathBuf, time::Duration};

use oauth2::TokenResponse as _;

use rmcp::transport::auth::{
    AuthClient, AuthError, AuthorizationManager, AuthorizationMetadata,
    AuthorizationMetadataSource, AuthorizationRequest, AuthorizationSession, OAuthTokenResponse,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
};

use crate::{
    AmpMcpClient, Auth, ClientOptions, CredentialBackend, McpError,
    external::Secret,
    http::{DEFAULT_OAUTH_REQUEST_TIMEOUT, build_http_client, build_oauth_http_client},
};

/// How long `login` waits for the browser to come back to the loopback listener.
pub const DEFAULT_LOGIN_TIMEOUT: Duration = Duration::from_secs(300);
/// The path of the loopback redirect URI.
pub const CALLBACK_PATH: &str = "/callback";
/// `client_name` sent in dynamic client registration.
pub const DEFAULT_CLIENT_NAME: &str = "amp-harness";

/// What [`login`] needs.
#[derive(Debug, Clone)]
pub struct LoginOptions {
    pub url: String,
    pub ca_file: Option<PathBuf>,
    pub backend: CredentialBackend,
    pub client_name: String,
    /// Upper bound on waiting for the browser.
    pub timeout: Duration,
    /// Upper bound on each HTTP request of the flow (the 401 probe, discovery, client
    /// registration, token exchange).
    pub request_timeout: Duration,
    /// A pre-registered client, scopes, a fixed callback port or a metadata URL (`[mcp.<name>]
    /// oauth = {..}`). The default (AMP's case) registers dynamically and lets rmcp pick scopes.
    pub client: OAuthClientSettings,
    /// The login's scope ([`crate::scoped_credential_key`]): `None` for AMP and the person's own
    /// servers; a repo server's own scope otherwise (#2308).
    pub credential_scope: Option<String>,
}

/// OAuth settings beyond discovery + dynamic registration, for third-party servers (D2).
#[derive(Clone, Default)]
pub struct OAuthClientSettings {
    /// A client registered out of band: no dynamic registration.
    pub client_id: Option<String>,
    /// That client's secret (a confidential client), from the keychain. Needs `client_id`.
    pub client_secret: Option<Secret>,
    /// Scopes to request; empty lets rmcp choose from the server's metadata and challenge.
    pub scopes: Vec<String>,
    /// The loopback redirect port; 0 picks a free one. A pre-registered client usually needs a
    /// fixed one, since its redirect URI is registered.
    pub callback_port: u16,
    /// Authorization server metadata (RFC 8414) to use instead of discovery.
    pub auth_server_metadata_url: Option<String>,
}

impl std::fmt::Debug for OAuthClientSettings {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("OAuthClientSettings")
            .field("client_id", &self.client_id)
            .field(
                "client_secret",
                &self.client_secret.as_ref().map(|_| "<redacted>"),
            )
            .field("scopes", &self.scopes)
            .field("callback_port", &self.callback_port)
            .field("auth_server_metadata_url", &self.auth_server_metadata_url)
            .finish()
    }
}

impl LoginOptions {
    pub fn new(url: impl Into<String>, backend: CredentialBackend) -> Self {
        Self {
            url: url.into(),
            ca_file: None,
            backend,
            client_name: DEFAULT_CLIENT_NAME.to_owned(),
            timeout: DEFAULT_LOGIN_TIMEOUT,
            request_timeout: DEFAULT_OAUTH_REQUEST_TIMEOUT,
            client: OAuthClientSettings::default(),
            credential_scope: None,
        }
    }

    pub fn with_credential_scope(mut self, scope: Option<String>) -> Self {
        self.credential_scope = scope;
        self
    }

    /// The key the login is stored under.
    pub fn credential_key(&self) -> String {
        crate::scoped_credential_key(&self.url, self.credential_scope.as_deref())
    }

    pub fn with_client(mut self, client: OAuthClientSettings) -> Self {
        self.client = client;
        self
    }

    pub fn with_ca_file(mut self, ca_file: Option<PathBuf>) -> Self {
        self.ca_file = ca_file;
        self
    }

    pub fn with_timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    pub fn with_request_timeout(mut self, timeout: Duration) -> Self {
        self.request_timeout = timeout;
        self
    }
}

/// Handed to the caller once the authorization URL exists and the loopback listener is waiting:
/// open `authorization_url` in a browser (or print it).
#[derive(Debug, Clone)]
pub struct LoginPrompt {
    pub authorization_url: String,
    pub redirect_uri: String,
}

/// The result of a login. Holds no secrets.
#[derive(Debug, Clone)]
pub struct LoginOutcome {
    /// The normalised server URL the login is stored under.
    pub key: String,
    pub client_id: String,
    pub scopes: Vec<String>,
    pub expires_in: Option<Duration>,
    pub has_refresh_token: bool,
    /// Where it was stored ([`CredentialBackend::describe`]).
    pub stored_in: String,
}

/// Runs the browser login and stores the result. `prompt` is called once, after client
/// registration, with the URL to open; the function then waits (up to `options.timeout`) for the
/// redirect to the loopback listener and exchanges the code.
pub async fn login<F>(options: LoginOptions, prompt: F) -> Result<LoginOutcome, McpError>
where
    F: FnOnce(&LoginPrompt),
{
    // 1. The server's 401 challenge starts discovery (RFC 9728 `resource_metadata`). A server that
    //    lets us in without credentials (compose: auth off) needs no login.
    let probe = ClientOptions::new(options.url.clone())
        .with_protocol(crate::ProtocolMode::Auto)
        .with_connect_timeout(options.request_timeout);
    let probe = ClientOptions {
        ca_file: options.ca_file.clone(),
        ..probe
    };
    let challenge = match AmpMcpClient::connect(probe).await {
        Ok(client) => {
            client.close().await;
            return Err(McpError::OAuth(format!(
                "{} accepts requests without credentials (auth off), so there is nothing to log in to",
                options.url
            )));
        }
        Err(McpError::AuthRequired { challenge }) => challenge,
        Err(e) => return Err(e),
    };
    login_from_challenge(&options, challenge, prompt).await
}

/// Steps 2-5 of [`login`], after a probe got the server's 401 (`challenge` is its
/// `WWW-Authenticate` header, if any). [`crate::external::http::login`] probes a third-party
/// server its own way (with its headers) and continues here.
pub(crate) async fn login_from_challenge<F>(
    options: &LoginOptions,
    challenge: Option<String>,
    prompt: F,
) -> Result<LoginOutcome, McpError>
where
    F: FnOnce(&LoginPrompt),
{
    let client = &options.client;
    if client.client_secret.is_some() && client.client_id.is_none() {
        return Err(McpError::Config(
            "an OAuth client secret needs a client_id".to_owned(),
        ));
    }

    // 2. Loopback redirect listener (RFC 8252 §7.3), on a random port unless one is configured.
    let listener = TcpListener::bind(("127.0.0.1", client.callback_port))
        .await
        .map_err(|e| {
            McpError::OAuth(format!(
                "bind the loopback redirect listener on 127.0.0.1:{}: {e}",
                client.callback_port
            ))
        })?;
    let port = listener
        .local_addr()
        .map_err(|e| McpError::OAuth(format!("loopback listener: {e}")))?
        .port();
    let redirect_uri = format!("http://127.0.0.1:{port}{CALLBACK_PATH}");

    // 3. Discovery + dynamic client registration + the PKCE authorization URL.
    let store = options.backend.store_for_key(options.credential_key());
    let mut manager = new_manager(
        &options.url,
        options.ca_file.as_deref(),
        options.request_timeout,
    )
    .await?;
    manager.set_credential_store(store.clone());
    if let Some(metadata_url) = &client.auth_server_metadata_url {
        let metadata = fetch_metadata(
            metadata_url,
            options.ca_file.as_deref(),
            options.request_timeout,
        )
        .await?;
        manager.set_metadata(metadata);
    } else {
        let resolution = manager
            .resolve_metadata_from_challenge(challenge.as_deref())
            .await
            .map_err(oauth_error)?;
        if resolution.source == AuthorizationMetadataSource::LegacyEndpointFallback {
            return Err(McpError::OAuth(format!(
                "{} publishes no OAuth metadata (RFC 9728 / RFC 8414); use AMP_TOKEN instead                  (for an [mcp] server: bearer_env, secret_headers or oauth.auth_server_metadata_url)",
                options.url
            )));
        }
        manager.set_metadata(resolution.metadata);
    }
    let mut request = AuthorizationRequest::new(redirect_uri.clone())
        .with_client_name(options.client_name.clone());
    if let Some(client_id) = &client.client_id {
        request = request.with_preregistered_client(client_id.clone());
    }
    if let Some(secret) = &client.client_secret {
        request = request.with_client_secret(secret.expose().to_owned());
    }
    if !client.scopes.is_empty() {
        request = request.with_scopes(client.scopes.iter().cloned());
    }
    let session = AuthorizationSession::new(manager, request)
        .await
        .map_err(|(_, e)| oauth_error(e))?;
    let authorization_url = session.get_authorization_url().to_owned();
    // The URL comes from the server's `authorization_endpoint` metadata, which rmcp only parses:
    // never let a malicious server make us launch `file://` or a custom scheme handler.
    check_authorization_url(&authorization_url).map_err(McpError::OAuth)?;
    let expected_state = query_param(&authorization_url, "state")
        .ok_or_else(|| McpError::OAuth("authorization URL has no state parameter".to_owned()))?;

    // 4. The browser.
    prompt(&LoginPrompt {
        authorization_url,
        redirect_uri: redirect_uri.clone(),
    });

    // 5. Wait for the redirect, then exchange the code (rmcp saves the tokens to the store).
    let callback = tokio::time::timeout(
        options.timeout,
        wait_for_callback(&listener, port, &expected_state),
    )
    .await
    .map_err(|_| {
        McpError::OAuth(format!(
            "no answer from the browser within {:?}; run login again",
            options.timeout
        ))
    })??;
    drop(listener);
    let token = session
        .handle_callback_url(&callback)
        .await
        .map_err(oauth_error)?;
    let (client_id, _) = session.get_credentials().await.map_err(oauth_error)?;

    Ok(outcome(options, client_id, &token))
}

/// Reads RFC 8414 authorization server metadata from `url` (`oauth.auth_server_metadata_url`),
/// with the OAuth client's roots, redirect policy and timeouts.
pub(crate) async fn fetch_metadata(
    url: &str,
    ca_file: Option<&std::path::Path>,
    request_timeout: Duration,
) -> Result<AuthorizationMetadata, McpError> {
    let http = build_oauth_http_client(ca_file, request_timeout)?;
    let fail = |reason: String| {
        McpError::OAuth(format!(
            "authorization server metadata at {}: {reason}",
            crate::url_origin(url)
        ))
    };
    let response = http
        .get(url)
        .header(reqwest::header::ACCEPT, "application/json")
        .send()
        .await
        .map_err(|e| fail(e.without_url().to_string()))?;
    if !response.status().is_success() {
        return Err(fail(format!("HTTP {}", response.status())));
    }
    response
        .json::<AuthorizationMetadata>()
        .await
        .map_err(|e| fail(format!("not OAuth metadata: {}", e.without_url())))
}

fn outcome(options: &LoginOptions, client_id: String, token: &OAuthTokenResponse) -> LoginOutcome {
    LoginOutcome {
        key: options.credential_key(),
        client_id,
        scopes: token_scopes(token),
        expires_in: token_expires_in(token),
        has_refresh_token: token_has_refresh(token),
        stored_in: options.backend.describe(),
    }
}

/// Removes the stored login for `url`. `Ok(true)` if there was one. (AMP's broker has no
/// revocation endpoint in v1; the refresh token simply stops being used.)
pub async fn logout(url: &str, backend: &CredentialBackend) -> Result<bool, McpError> {
    backend.store_for(url).clear_credentials().await
}

/// Removes the login stored under `key` ([`crate::scoped_credential_key`]). `Ok(true)` if there
/// was one.
pub async fn logout_key(key: &str, backend: &CredentialBackend) -> Result<bool, McpError> {
    backend
        .store_for_key(key.to_owned())
        .clear_credentials()
        .await
}

/// Copies the login stored under `from` to `to` (keys from [`crate::scoped_credential_key`]),
/// keeping `from`. `Ok(false)` when there is none under `from`, or `to` already has one. For
/// `amp-harness mcp secret --adopt` (#2308): a person giving a repo server the login that every
/// server of that URL shared before logins were scoped.
pub async fn copy_login(
    from: &str,
    to: &str,
    backend: &CredentialBackend,
) -> Result<bool, McpError> {
    let target = backend.store_for_key(to.to_owned());
    if target.load_credentials().await?.is_some() {
        return Ok(false);
    }
    match backend
        .store_for_key(from.to_owned())
        .load_credentials()
        .await?
    {
        Some(creds) if creds.token_response.is_some() => {
            target.save_credentials(creds).await?;
            Ok(true)
        }
        _ => Ok(false),
    }
}

/// Whether a login with a token is stored under `key` ([`crate::scoped_credential_key`]).
pub async fn has_login_key(key: &str, backend: &CredentialBackend) -> Result<bool, McpError> {
    Ok(backend
        .store_for_key(key.to_owned())
        .load_credentials()
        .await?
        .is_some_and(|c| c.token_response.is_some()))
}

/// Whether a login with a token is stored for `url`.
pub async fn has_login(url: &str, backend: &CredentialBackend) -> Result<bool, McpError> {
    Ok(backend
        .store_for(url)
        .load_credentials()
        .await?
        .is_some_and(|c| c.token_response.is_some()))
}

/// Connects with the best credentials available, in D3's order:
/// 1. `options.auth` is a static token (`AMP_TOKEN`): use it; stored logins are not touched.
/// 2. A stored login for `options.url`: OAuth through rmcp's `AuthClient` (refreshes as needed).
/// 3. Nothing: connect without credentials (compose); an auth server answers
///    [`McpError::AuthRequired`] and the caller says "run `amp-harness login`".
pub async fn connect(
    options: ClientOptions,
    backend: &CredentialBackend,
) -> Result<AmpMcpClient, McpError> {
    if matches!(options.auth, Auth::Static { .. }) {
        return AmpMcpClient::connect(options).await;
    }
    let store = backend.store_for(&options.url);
    let stored = store.load_credentials().await?;
    if !stored.is_some_and(|c| c.token_response.is_some()) {
        return AmpMcpClient::connect(options).await;
    }

    let http = build_http_client(options.ca_file.as_deref())?;
    // Loads the client id and re-discovers the authorization server; a login made against a
    // different issuer is dropped (rmcp clears it), which means logging in again. This runs before
    // the MCP handshake, so it gets its own bound (the same connect timeout); each OAuth request
    // is also bounded by the manager's client.
    let prepare = async {
        let mut manager = new_manager(
            &options.url,
            options.ca_file.as_deref(),
            DEFAULT_OAUTH_REQUEST_TIMEOUT,
        )
        .await?;
        manager.set_credential_store(store);
        if !manager.initialize_from_store().await.map_err(oauth_error)? {
            return Err(McpError::AuthRequired { challenge: None });
        }
        Ok(manager)
    };
    let manager = tokio::time::timeout(options.connect_timeout, prepare)
        .await
        .map_err(|_| McpError::Timeout {
            operation: format!("OAuth discovery for {}", crate::redact_url(&options.url)),
            after: options.connect_timeout,
        })??;
    let auth = format!("oauth ({})", backend.describe());
    let mut client =
        AmpMcpClient::connect_with_http_client(options, AuthClient::new(http, manager)).await?;
    client.set_auth_description(auth);
    Ok(client)
}

pub(crate) async fn new_manager(
    url: &str,
    ca_file: Option<&std::path::Path>,
    request_timeout: Duration,
) -> Result<AuthorizationManager, McpError> {
    let mut manager = AuthorizationManager::new(url).await.map_err(oauth_error)?;
    // Same TLS roots (incl. --ca-file) and same-origin-only redirects as the MCP transport, plus
    // request and connect timeouts (rmcp's default OAuth client has them; ours must too).
    manager
        .with_client(build_oauth_http_client(ca_file, request_timeout)?)
        .map_err(oauth_error)?;
    Ok(manager)
}

/// Maps rmcp's OAuth errors. "Log in again" cases become [`McpError::AuthRequired`].
pub(crate) fn oauth_error(e: AuthError) -> McpError {
    match e {
        AuthError::AuthorizationRequired | AuthError::TokenRefreshRejected(_) => {
            McpError::AuthRequired { challenge: None }
        }
        AuthError::CredentialStoreError(msg) => McpError::CredentialStore(msg),
        other => McpError::OAuth(other.to_string()),
    }
}

fn token_scopes(token: &OAuthTokenResponse) -> Vec<String> {
    token
        .scopes()
        .map(|s| s.iter().map(|s| s.to_string()).collect())
        .unwrap_or_default()
}

fn token_expires_in(token: &OAuthTokenResponse) -> Option<Duration> {
    token.expires_in()
}

fn token_has_refresh(token: &OAuthTokenResponse) -> bool {
    token.refresh_token().is_some()
}

fn query_param(url: &str, name: &str) -> Option<String> {
    let url = reqwest::Url::parse(url).ok()?;
    url.query_pairs()
        .find(|(k, _)| k == name)
        .map(|(_, v)| v.into_owned())
}

/// An authorization URL we are willing to open in the browser: `https`, or `http` to a loopback
/// host (127.0.0.0/8, ::1, localhost), which a local or test authorization server uses. The error
/// names only the scheme and host, never the query (it carries the PKCE challenge and state).
fn check_authorization_url(url: &str) -> Result<(), String> {
    let parsed = reqwest::Url::parse(url)
        .map_err(|_| "authorization endpoint is not a valid URL; refusing to open it".to_owned())?;
    match parsed.scheme() {
        "https" => Ok(()),
        "http" if is_loopback_host(&parsed) => Ok(()),
        "http" => Err(format!(
            "authorization endpoint http://{}… is plain http to a non-loopback host; refusing to open it",
            parsed.host_str().unwrap_or_default()
        )),
        scheme => Err(format!(
            "authorization endpoint {scheme}://… is not http(s); refusing to open it"
        )),
    }
}

fn is_loopback_host(url: &reqwest::Url) -> bool {
    // `host_str` is already lowercased; an IPv6 host comes bracketed.
    let Some(host) = url.host_str() else {
        return false;
    };
    host == "localhost"
        || host
            .trim_start_matches('[')
            .trim_end_matches(']')
            .parse::<std::net::IpAddr>()
            .is_ok_and(|ip| ip.is_loopback())
}

const MAX_REQUEST_HEAD: usize = 16 * 1024;
const PER_CONNECTION_TIMEOUT: Duration = Duration::from_secs(10);

/// Serves the loopback listener until the browser arrives at [`CALLBACK_PATH`] with our `state`.
/// Anything else (favicon, a stale tab, a request with another state) gets an error page and the
/// wait goes on. Returns the full callback URL.
async fn wait_for_callback(
    listener: &TcpListener,
    port: u16,
    expected_state: &str,
) -> Result<String, McpError> {
    loop {
        let (mut stream, _) = listener
            .accept()
            .await
            .map_err(|e| McpError::OAuth(format!("loopback listener: {e}")))?;
        let target =
            match tokio::time::timeout(PER_CONNECTION_TIMEOUT, read_target(&mut stream)).await {
                Ok(Some(target)) => target,
                _ => continue,
            };
        let url = format!("http://127.0.0.1:{port}{target}");
        let Ok(parsed) = reqwest::Url::parse(&url) else {
            respond(&mut stream, 400, "Bad request.").await;
            continue;
        };
        if parsed.path() != CALLBACK_PATH {
            respond(&mut stream, 404, "Not found.").await;
            continue;
        }
        let param = |name: &str| {
            parsed
                .query_pairs()
                .find(|(k, _)| k == name)
                .map(|(_, v)| v.into_owned())
        };
        if param("state").as_deref() != Some(expected_state) {
            respond(
                &mut stream,
                400,
                "This sign-in link is not the one amp-harness is waiting for. Run `amp-harness login` again if needed.",
            )
            .await;
            continue;
        }
        if let Some(error) = param("error") {
            let description = param("error_description").unwrap_or_default();
            respond(
                &mut stream,
                400,
                "AMP sign-in failed. Return to the terminal for details.",
            )
            .await;
            return Err(McpError::OAuth(format!(
                "authorization failed: {error} {description}"
            )));
        }
        if param("code").is_none() {
            respond(&mut stream, 400, "The sign-in response has no code.").await;
            continue;
        }
        respond(
            &mut stream,
            200,
            "AMP sign-in complete. You can close this tab and return to the terminal.",
        )
        .await;
        return Ok(url);
    }
}

/// Reads an HTTP/1.x request head and returns the request target of a `GET`.
async fn read_target(stream: &mut TcpStream) -> Option<String> {
    let mut buf = Vec::with_capacity(1024);
    let mut chunk = [0u8; 1024];
    while !buf.windows(4).any(|w| w == b"\r\n\r\n") {
        if buf.len() > MAX_REQUEST_HEAD {
            return None;
        }
        let n = stream.read(&mut chunk).await.ok()?;
        if n == 0 {
            break;
        }
        buf.extend_from_slice(&chunk[..n]);
    }
    let head = String::from_utf8_lossy(&buf);
    let mut parts = head.lines().next()?.split_whitespace();
    let method = parts.next()?;
    let target = parts.next()?;
    (method == "GET" && target.starts_with('/')).then(|| target.to_owned())
}

async fn respond(stream: &mut TcpStream, status: u16, message: &str) {
    let reason = match status {
        200 => "OK",
        404 => "Not Found",
        _ => "Bad Request",
    };
    let body = format!(
        "<!doctype html><html><head><meta charset=\"utf-8\"><title>AMP sign-in</title></head>\
         <body style=\"font-family:system-ui,sans-serif;margin:3rem\"><p>{message}</p></body></html>"
    );
    let response = format!(
        "HTTP/1.1 {status} {reason}\r\nContent-Type: text/html; charset=utf-8\r\n\
         Content-Length: {}\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    let _ = stream.write_all(response.as_bytes()).await;
    let _ = stream.shutdown().await;
}

/// Opens `url` in the user's browser. Errors are for the caller to print; the URL is printed
/// anyway, so a failed open only means the user copies it. Only `http`/`https` URLs are opened:
/// anything else (`file://`, a custom scheme handler) is refused before a process is spawned.
pub fn open_browser(url: &str) -> Result<(), String> {
    let scheme = reqwest::Url::parse(url)
        .map(|u| u.scheme().to_owned())
        .map_err(|_| "not a valid URL; refusing to open it".to_owned())?;
    if scheme != "http" && scheme != "https" {
        return Err(format!(
            "{scheme}:// URLs are not http(s); refusing to open it"
        ));
    }
    #[cfg(target_os = "macos")]
    let mut command = std::process::Command::new("open");
    #[cfg(target_os = "windows")]
    let mut command = {
        let mut c = std::process::Command::new("rundll32");
        c.arg("url.dll,FileProtocolHandler");
        c
    };
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    let mut command = std::process::Command::new("xdg-open");
    command
        .arg(url)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    match command.status() {
        Ok(status) if status.success() => Ok(()),
        Ok(status) => Err(format!("browser opener exited with {status}")),
        Err(e) => Err(e.to_string()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn authorization_url_must_be_https_or_loopback_http() {
        for ok in [
            "https://auth.example.com/authorize?state=s",
            "http://127.0.0.1:8080/authorize",
            "http://127.0.0.2/authorize",
            "http://[::1]:9000/authorize",
            "http://localhost/authorize",
            "http://LOCALHOST:1/authorize",
        ] {
            assert!(check_authorization_url(ok).is_ok(), "{ok}");
        }
        for bad in [
            "file:///etc/passwd",
            "amp-evil://run?cmd=x",
            "javascript:alert(1)",
            "http://auth.example.com/authorize",
            "http://localhost.evil.com/authorize",
            "not a url",
        ] {
            let err = check_authorization_url(bad).expect_err(bad);
            assert!(err.contains("refusing to open it"), "{bad}: {err}");
        }
    }

    #[test]
    fn rejection_never_echoes_the_query() {
        let err =
            check_authorization_url("file:///x?state=SECRETSTATE&code_challenge=CC").unwrap_err();
        assert!(!err.contains("SECRETSTATE") && !err.contains("CC"), "{err}");
    }

    #[test]
    fn open_browser_refuses_non_http_urls_without_spawning() {
        // These return before a Command is built, so no opener process is ever launched.
        for bad in [
            "file:///etc/passwd",
            "amp-evil://run",
            "smb://host/share",
            "not a url",
        ] {
            let err = open_browser(bad).expect_err(bad);
            assert!(err.contains("refusing to open it"), "{bad}: {err}");
        }
    }
}
