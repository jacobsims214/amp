//! From a resolved `[mcp.<name>]` http (or sse) table to amp-mcp's [`HttpServerOptions`] (H4).
//!
//! This is where secret values are read: `secret_headers` and the OAuth client secret from the
//! keychain, `bearer_env` from the process env. They go straight into [`amp_mcp::Secret`]s on the
//! options (request headers only, never logged) and errors name the item, never the value.
//!
//! ```ignore
//! let (Resolved::Http(remote) | Resolved::Sse(remote)) = server.resolve(&ctx)? else { .. };
//! let options = mcp_http::http_options(server, &remote, Some(&amp_mcp::keychain_service()), env)?;
//! let client = amp_mcp::external::http::connect(&options, &backend).await?;
//! ```
//!
//! `type = "sse"` sets [`HttpServerOptions::legacy_sse`], so the same `http::connect` speaks
//! legacy HTTP+SSE to it; `type = "http"` falls back to SSE unless `fallback_sse = false` (#2238).

use amp_mcp::{
    ExternalOptions, HeaderSource, HttpAuth, HttpHeader, HttpServerOptions, Secret,
    oauth::OAuthClientSettings,
};

use crate::mcp_config::{
    AuthMode, McpServer, ResolvedRemote, SecretSource, SecretTarget, TransportKind,
};

/// Builds the connect options for `server` (whose resolved http/sse table is `remote`), fetching
/// its secrets. `keychain` is the keychain service ([`amp_mcp::keychain_service`]; a test-only
/// one in tests), or `None` when the keychain is off (`AMP_CREDENTIAL_STORE=file`), in which case
/// a server that keeps a secret there is an error. A secret that is not there is an error naming
/// where it was looked for.
pub fn http_options(
    server: &McpServer,
    remote: &ResolvedRemote,
    keychain: Option<&str>,
    env: &dyn Fn(&str) -> Option<String>,
) -> Result<HttpServerOptions, String> {
    let name = &server.name;
    let mut options = HttpServerOptions::new(name.clone(), remote.url.clone());
    options.external = ExternalOptions {
        protocol: remote.protocol,
        connect_timeout: server.startup_timeout,
        call_timeout: server.call_timeout,
        ..ExternalOptions::default()
    };
    options.headers = remote
        .headers
        .iter()
        .map(|(k, v)| HttpHeader::new(k.clone(), v.clone(), HeaderSource::Config))
        .collect();
    options.auth = match remote.auth {
        AuthMode::Auto => HttpAuth::Auto,
        AuthMode::Oauth => HttpAuth::OAuth,
        AuthMode::None => HttpAuth::None,
    };
    if let Some(spec) = &remote.oauth {
        options.oauth = OAuthClientSettings {
            client_id: spec.client_id.clone(),
            client_secret: None,
            scopes: spec.scopes.clone(),
            callback_port: spec.callback_port,
            auth_server_metadata_url: spec.auth_server_metadata_url.clone(),
        };
    }
    options.ca_file = remote.ca_file.clone();
    options.socket = remote.socket.clone();
    options.fallback_sse = remote.fallback_sse;
    options.legacy_sse = server.transport.kind() == TransportKind::Sse;

    for secret in &remote.secrets {
        let value = match (&secret.source, keychain) {
            (SecretSource::Keychain { account }, None) => {
                return Err(format!(
                    "MCP server `{name}`: keychain {account} can't be read because the keychain \
                     is off (AMP_CREDENTIAL_STORE=file); use bearer_env or ${{env:..}} instead"
                ));
            }
            (_, service) => secret
                .fetch(service.unwrap_or_default(), env)
                .map_err(|e| format!("MCP server `{name}`: {e}"))?,
        };
        let Some(value) = value else {
            return Err(match &secret.source {
                SecretSource::Keychain { account } => format!(
                    "MCP server `{name}`: keychain {account} is missing; store it with \
                     `amp-harness mcp add` (or remove it from the config)"
                ),
                SecretSource::Env { var } => {
                    format!("MCP server `{name}`: environment variable {var} is not set")
                }
            });
        };
        let source = || match &secret.source {
            SecretSource::Keychain { .. } => HeaderSource::Keychain,
            SecretSource::Env { var } => HeaderSource::Env(var.clone()),
        };
        match &secret.target {
            SecretTarget::Header(header) => {
                options
                    .headers
                    .push(HttpHeader::new(header.clone(), value, source()));
            }
            SecretTarget::Bearer => options = options.with_bearer(value, secret.describe()),
            SecretTarget::OAuthClientSecret => {
                options.oauth.client_secret = Some(Secret::new(value));
            }
            // Stdio only; resolve never puts one on a remote server.
            SecretTarget::Env(_) => {}
        }
    }
    Ok(options)
}
