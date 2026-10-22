//! Connection flags shared by the commands that talk to AMP, and the CLI's error type.

use std::{
    fmt,
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};

use amp_core::tools::ToolCallError;
use amp_mcp::{
    AmpMcpClient, Auth, ClientOptions, CredentialBackend, FailureKind, McpError, Overrides,
    ProtocolMode, Reconnector, Settings, oauth,
};
use clap::Args;

/// `--url`, `--ca-file`, `--protocol`, `--config`, `--token-env` (D4; precedence in amp-mcp).
#[derive(Args, Debug, Clone, Default)]
pub struct ConnArgs {
    /// MCP server URL (default: AMP_MCP_URL, then config.toml, then hosted AMP).
    #[arg(long)]
    pub url: Option<String>,
    /// Extra root certificate (PEM), e.g. the docker-desktop cluster's amp-local-ca.crt.
    #[arg(long)]
    pub ca_file: Option<PathBuf>,
    /// MCP protocol: auto (default), 2026-07-28 or 2025-11-25.
    #[arg(long)]
    pub protocol: Option<ProtocolMode>,
    /// Read this config.toml instead of ~/.config/amp-harness/config.toml.
    #[arg(long)]
    pub config: Option<PathBuf>,
    /// Read the static AMP token from this environment variable instead of AMP_TOKEN.
    #[arg(long)]
    pub token_env: Option<String>,
}

impl ConnArgs {
    pub fn settings(&self) -> Result<Settings, McpError> {
        Settings::resolve(&Overrides {
            url: self.url.clone(),
            ca_file: self.ca_file.clone(),
            protocol: self.protocol,
            config_file: self.config.clone(),
            token_env: self.token_env.clone(),
        })
    }
}

/// Connects with D3's credential order: a static token, else the stored login, else none.
/// The credential store is only consulted (and the keychain only probed) without a static token.
pub async fn connect(settings: &Settings) -> Result<AmpMcpClient, McpError> {
    let options = ClientOptions::from_settings(settings);
    if matches!(settings.auth, Auth::Static { .. }) {
        return AmpMcpClient::connect(options).await;
    }
    let backend = CredentialBackend::resolve()?;
    oauth::connect(options, &backend).await
}

/// [`connect`] for the one-shot commands (`whoami`, `tickets`, `mcp`): a failure is one plain
/// sentence naming the server, with what to do next ([`connect_message`], #2178).
pub async fn connect_cli(settings: &Settings) -> Result<AmpMcpClient, CliError> {
    connect(settings).await.map_err(|e| {
        CliError::new(connect_message_verbose(
            &settings.url,
            Some(&settings.auth),
            &e,
        ))
    })
}

/// The session's own connection (the model's AMP tools, run reporting, the worker supervisor):
/// [`connect`], plus the automatic reconnect, so `amp-harness login` in another terminal heals
/// it within 15 s without restarting the chat (#2159). A static token gets no reconnect: a rejected
/// AMP_TOKEN stays rejected until the person sets a new one and restarts.
pub async fn connect_session(settings: &Settings) -> Result<AmpMcpClient, McpError> {
    let client = connect(settings).await?;
    client.set_reconnector(reconnector(settings));
    Ok(client)
}

/// A fresh [`connect`] with `settings`, for [`AmpMcpClient::set_reconnector`]; `None` for a
/// static token. #2164's `/server` builds one for the new server's settings and calls
/// [`AmpMcpClient::reconnect`].
pub fn reconnector(settings: &Settings) -> Option<Reconnector> {
    if matches!(settings.auth, Auth::Static { .. }) {
        return None;
    }
    let settings = settings.clone();
    Some(Arc::new(move || {
        let settings = settings.clone();
        Box::pin(async move {
            // A reconnect follows "login required": read the stored login afresh, since keychain
            // reads are cached per process (#2165) and the new login was written by another one.
            if let Ok(backend) = CredentialBackend::resolve() {
                backend.forget_cached(&settings.url);
            }
            connect(&settings).await
        })
    }))
}

/// `--verbose` (or a non-empty `RUST_LOG`): connection errors also print their full cause chain.
static VERBOSE: AtomicBool = AtomicBool::new(false);

/// Turns on the full cause chains (`--verbose`); `RUST_LOG` does it too.
pub fn set_verbose(on: bool) {
    let env = std::env::var("RUST_LOG").is_ok_and(|v| !v.trim().is_empty());
    VERBOSE.store(on || env, Ordering::Relaxed);
}

pub fn verbose() -> bool {
    VERBOSE.load(Ordering::Relaxed)
}

/// The URL a connect error names, if it names one.
fn error_url(e: &McpError) -> Option<&str> {
    match e {
        McpError::Connect { url, .. } | McpError::ConnectTimeout { url, .. } => Some(url),
        _ => None,
    }
}

/// One plain sentence for a failure to reach AMP, with what to do next (#2178): "Can't reach AMP
/// at localhost:18999 (connection refused). Is the server running? …". No Rust type names: the
/// full chain is added on its own line only with `--verbose` / `RUST_LOG`
/// ([`connect_message_verbose`]). `url` is the server's MCP URL; `auth` says whether a static
/// token (its variable's name, never its value) was refused.
pub fn connect_message(url: &str, auth: Option<&Auth>, e: &McpError) -> String {
    let host = crate::setup::host_of(url);
    let switch = "or switch servers with `amp-harness server set <url>` (or --url)";
    match e.kind() {
        FailureKind::Refused => format!(
            "Can't reach AMP at {host} (connection refused). Is the server running? Check with \
             `amp-harness doctor`, {switch}."
        ),
        FailureKind::HostNotFound => format!(
            "Can't reach AMP at {host} (host not found). Check the address with `amp-harness \
             server`, {switch}."
        ),
        FailureKind::Tls => format!(
            "Can't connect securely to AMP at {host} (its certificate is not trusted, or it does \
             not speak HTTPS). For a private CA save it with `amp-harness server set {url} \
             --ca-file <PEM>`; `amp-harness doctor` shows the certificate problem."
        ),
        FailureKind::LoginRequired => match auth {
            Some(Auth::Static { env, .. }) => format!(
                "AMP at {host} refused the token in {env} (401). Set a new token in {env}, or \
                 unset it and run `amp-harness login --url {url}`."
            ),
            _ => format!(
                "AMP at {host} needs you to log in (401). Run `amp-harness login --url {url}`, \
                 then try again."
            ),
        },
        FailureKind::NotFound => format!(
            "AMP at {host} has no MCP endpoint at {url} (HTTP 404). Check the address (it \
             usually ends in /mcp) with `amp-harness server`, {switch}."
        ),
        FailureKind::NotMcp => format!(
            "The server at {host} did not answer as AMP ({}). Check the address (it usually ends \
             in /mcp) with `amp-harness server`, {switch}.",
            e.reason()
        ),
        FailureKind::Timeout => format!(
            "AMP at {host} did not answer ({}). Check the server and your network with \
             `amp-harness doctor`, {switch}.",
            e.reason()
        ),
        FailureKind::Closed => format!(
            "The connection to AMP at {host} was closed. Try again; `amp-harness doctor` checks \
             the server."
        ),
        FailureKind::Config => format!(
            "The AMP server settings are not usable ({}). See them with `amp-harness server`.",
            e.reason()
        ),
        _ => format!(
            "Can't connect to AMP at {host} ({}). Check with `amp-harness doctor`, {switch}.",
            e.reason()
        ),
    }
}

/// [`connect_message`], plus the full cause chain on the next line when `--verbose` / `RUST_LOG`.
pub fn connect_message_verbose(url: &str, auth: Option<&Auth>, e: &McpError) -> String {
    let message = connect_message(url, auth, e);
    if verbose() {
        format!("{message}\n  cause: {e}")
    } else {
        message
    }
}

/// An error the CLI prints on stderr, with its exit code.
#[derive(Debug)]
pub struct CliError {
    pub message: String,
    pub code: u8,
    /// The chat's first run could not reach AMP: the chat opens offline instead (#2178).
    pub offline: bool,
}

impl CliError {
    pub fn new(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            code: 1,
            offline: false,
        }
    }

    /// Bad arguments (exit 2, like clap's own usage errors).
    pub fn usage(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            code: 2,
            offline: false,
        }
    }

    /// Already reported (doctor's failed checks): exit non-zero without another message.
    pub fn silent(code: u8) -> Self {
        Self {
            message: String::new(),
            code,
            offline: false,
        }
    }
}

impl fmt::Display for CliError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl From<McpError> for CliError {
    /// Connection failures read as one plain sentence with a next step ([`connect_message`]);
    /// other errors (bad settings, a tool's own error) keep their message.
    fn from(e: McpError) -> Self {
        if let Some(url) = error_url(&e) {
            return Self::new(connect_message_verbose(url, None, &e));
        }
        match &e {
            // rmcp's text for a failed request names its own types: say what failed, plainly.
            McpError::Service { operation, .. } if !e.is_server_error() => {
                let message = format!(
                    "AMP {operation} failed ({}); `amp-harness doctor` checks the connection.",
                    e.reason()
                );
                Self::new(if verbose() {
                    format!("{message}\n  cause: {e}")
                } else {
                    message
                })
            }
            _ => Self::new(e.to_string()),
        }
    }
}

impl From<ToolCallError> for CliError {
    fn from(e: ToolCallError) -> Self {
        Self::new(e.to_string())
    }
}

impl From<amp_core::project::MarkerError> for CliError {
    fn from(e: amp_core::project::MarkerError) -> Self {
        Self::new(format!(".amp.json: {e}"))
    }
}

/// Prints `value` as pretty JSON followed by a newline (the `--json` output of every command).
pub fn print_json<T: serde::Serialize>(value: &T) -> Result<(), CliError> {
    let text = serde_json::to_string_pretty(value)
        .map_err(|e| CliError::new(format!("encode JSON: {e}")))?;
    println!("{text}");
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn settings(token: Option<&str>) -> Settings {
        let token = token.map(str::to_owned);
        let env = move |key: &str| match key {
            "AMP_TOKEN" => token.clone(),
            "XDG_CONFIG_HOME" => Some("/nonexistent/amp-harness-conn-test".to_owned()),
            _ => None,
        };
        Settings::resolve_with_env(
            &Overrides {
                url: Some("http://127.0.0.1:9/mcp".to_owned()),
                ..Overrides::default()
            },
            &env,
        )
        .unwrap()
    }

    /// #2178: a server that never answers reads as one plain sentence with a next step.
    #[test]
    fn a_connect_timeout_is_one_plain_sentence() {
        let e = McpError::ConnectTimeout {
            url: "http://localhost:18999/mcp".to_owned(),
            after: std::time::Duration::from_secs(30),
        };
        let message = connect_message("http://localhost:18999/mcp", None, &e);
        assert_eq!(
            message,
            "AMP at localhost:18999 did not answer (no answer within 30s). Check the server and \
             your network with `amp-harness doctor`, or switch servers with `amp-harness server \
             set <url>` (or --url)."
        );
        assert_eq!(CliError::from(e).message, message);
    }

    /// #2159: a stored login (or none yet) reconnects after `amp-harness login`; AMP_TOKEN never
    /// does, so a rejected token stays rejected until the person sets a new one.
    #[test]
    fn only_a_session_without_a_static_token_reconnects() {
        assert!(reconnector(&settings(None)).is_some());
        assert!(reconnector(&settings(Some("amp_dummy_value"))).is_none());
    }
}
