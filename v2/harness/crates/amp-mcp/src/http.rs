//! The reqwest clients the MCP transport and the OAuth flow use.

use std::{path::Path, time::Duration};

use crate::McpError;

/// Maximum redirects followed for one request (same as rmcp's default client).
const MAX_REDIRECTS: usize = 10;

/// Builds the HTTP client: rustls with the platform's roots, plus every certificate in `ca_file`
/// (`--ca-file` / `AMP_CA_FILE`, e.g. the docker-desktop cluster's self-signed CA).
///
/// Mirrors rmcp's default transport client: no idle pooling, and only same-origin 307/308 redirects
/// are followed, so a POST body or the session id never goes to another host.
///
/// It has no overall request timeout on purpose: legacy-era SSE streams are long-lived. Calls are
/// bounded by the client's own connect and call timeouts instead.
pub fn build_http_client(ca_file: Option<&Path>) -> Result<reqwest::Client, McpError> {
    base_builder(ca_file)?
        .build()
        .map_err(|e| McpError::Http(e.to_string()))
}

/// Default bound on one OAuth HTTP request (discovery, registration, token exchange, refresh):
/// rmcp's own default OAuth client uses the same 30 s.
pub const DEFAULT_OAUTH_REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
/// Bound on establishing the TCP/TLS connection of an OAuth request.
pub const OAUTH_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

/// Builds the client for rmcp's OAuth `AuthorizationManager`: the same roots (incl. `ca_file`) and
/// same-origin redirect policy as [`build_http_client`], plus a per-request `timeout` and a connect
/// timeout (the smaller of `timeout` and [`OAUTH_CONNECT_TIMEOUT`]). rmcp ignores per-request
/// timeouts on a custom client, so without these a stalled authorization server hangs forever.
pub fn build_oauth_http_client(
    ca_file: Option<&Path>,
    timeout: Duration,
) -> Result<reqwest::Client, McpError> {
    base_builder(ca_file)?
        .timeout(timeout)
        .connect_timeout(timeout.min(OAUTH_CONNECT_TIMEOUT))
        .build()
        .map_err(|e| McpError::Http(e.to_string()))
}

fn base_builder(ca_file: Option<&Path>) -> Result<reqwest::ClientBuilder, McpError> {
    let mut builder = reqwest::Client::builder()
        .pool_max_idle_per_host(0)
        .redirect(same_origin_redirect_policy());
    if let Some(path) = ca_file {
        builder = builder.tls_certs_merge(load_ca_file(path)?);
    }
    Ok(builder)
}

/// Reads a PEM bundle; at least one certificate is required.
pub fn load_ca_file(path: &Path) -> Result<Vec<reqwest::Certificate>, McpError> {
    let ca_error = |reason: String| McpError::CaFile {
        path: path.to_owned(),
        reason,
    };
    let pem = std::fs::read(path).map_err(|e| ca_error(e.to_string()))?;
    let certs = reqwest::Certificate::from_pem_bundle(&pem)
        .map_err(|e| ca_error(format!("not a PEM certificate bundle: {e}")))?;
    if certs.is_empty() {
        return Err(ca_error("no PEM certificates found".to_owned()));
    }
    Ok(certs)
}

fn same_origin_redirect_policy() -> reqwest::redirect::Policy {
    reqwest::redirect::Policy::custom(|attempt| {
        let preserves_body = matches!(
            attempt.status(),
            reqwest::StatusCode::TEMPORARY_REDIRECT | reqwest::StatusCode::PERMANENT_REDIRECT
        );
        let same_origin = attempt
            .previous()
            .first()
            .is_some_and(|original| original.origin() == attempt.url().origin());
        if !preserves_body || !same_origin {
            attempt.stop()
        } else if attempt.previous().len() > MAX_REDIRECTS {
            attempt.error(format!("exceeded {MAX_REDIRECTS} redirects"))
        } else {
            attempt.follow()
        }
    })
}
