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
    AuthClient, AuthError, AuthorizationManager, AuthorizationMetadataSource, AuthorizationRequest,
    AuthorizationSession, OAuthTokenResponse,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
};

use crate::{
    AmpMcpClient, Auth, ClientOptions, CredentialBackend, McpError,
    credentials::credential_key,
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
        }
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

    // 2. Loopback redirect listener on a random port (RFC 8252 §7.3).
    let listener = TcpListener::bind(("127.0.0.1", 0))
        .await
        .map_err(|e| McpError::OAuth(format!("bind the loopback redirect listener: {e}")))?;
    let port = listener
        .local_addr()
        .map_err(|e| McpError::OAuth(format!("loopback listener: {e}")))?
        .port();
    let redirect_uri = format!("http://127.0.0.1:{port}{CALLBACK_PATH}");

    // 3. Discovery + dynamic client registration + the PKCE authorization URL.
    let store = options.backend.store_for(&options.url);
    let mut manager = new_manager(
        &options.url,
        options.ca_file.as_deref(),
        options.request_timeout,
    )
    .await?;
    manager.set_credential_store(store.clone());
    let resolution = manager
        .resolve_metadata_from_challenge(challenge.as_deref())
        .await
        .map_err(oauth_error)?;
    if resolution.source == AuthorizationMetadataSource::LegacyEndpointFallback {
        return Err(McpError::OAuth(format!(
            "{} publishes no OAuth metadata (RFC 9728 / RFC 8414); use AMP_TOKEN instead",
            options.url
        )));
    }
    manager.set_metadata(resolution.metadata);
    let request = AuthorizationRequest::new(redirect_uri.clone())
        .with_client_name(options.client_name.clone());
    let session = AuthorizationSession::new(manager, request)
        .await
        .map_err(|(_, e)| oauth_error(e))?;
    let authorization_url = session.get_authorization_url().to_owned();
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

    Ok(outcome(&options, client_id, &token))
}

fn outcome(options: &LoginOptions, client_id: String, token: &OAuthTokenResponse) -> LoginOutcome {
    LoginOutcome {
        key: credential_key(&options.url),
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
            operation: format!("OAuth discovery for {}", options.url),
            after: options.connect_timeout,
        })??;
    let auth = format!("oauth ({})", backend.describe());
    let mut client =
        AmpMcpClient::connect_with_http_client(options, AuthClient::new(http, manager)).await?;
    client.set_auth_description(auth);
    Ok(client)
}

async fn new_manager(
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
/// anyway, so a failed open only means the user copies it.
pub fn open_browser(url: &str) -> Result<(), String> {
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
