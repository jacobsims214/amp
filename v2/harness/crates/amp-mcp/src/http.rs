//! The reqwest client the MCP transport (and, in M1-3, the OAuth flow) uses.

use std::path::Path;

use crate::McpError;

/// Maximum redirects followed for one request (same as rmcp's default client).
const MAX_REDIRECTS: usize = 10;

/// Builds the HTTP client: rustls with the platform's roots, plus every certificate in `ca_file`
/// (`--ca-file` / `AMP_CA_FILE`, e.g. the docker-desktop cluster's self-signed CA).
///
/// Mirrors rmcp's default transport client: no idle pooling, and only same-origin 307/308 redirects
/// are followed, so a POST body or the session id never goes to another host.
pub fn build_http_client(ca_file: Option<&Path>) -> Result<reqwest::Client, McpError> {
    let mut builder = reqwest::Client::builder()
        .pool_max_idle_per_host(0)
        .redirect(same_origin_redirect_policy());
    if let Some(path) = ca_file {
        builder = builder.tls_certs_merge(load_ca_file(path)?);
    }
    builder.build().map_err(|e| McpError::Http(e.to_string()))
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
