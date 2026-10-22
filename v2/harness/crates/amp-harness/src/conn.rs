//! Connection flags shared by the commands that talk to AMP, and the CLI's error type.

use std::{fmt, path::PathBuf, sync::Arc};

use amp_core::tools::ToolCallError;
use amp_mcp::{
    AmpMcpClient, Auth, ClientOptions, CredentialBackend, McpError, Overrides, ProtocolMode,
    Reconnector, Settings, oauth,
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

/// An error the CLI prints on stderr, with its exit code.
#[derive(Debug)]
pub struct CliError {
    pub message: String,
    pub code: u8,
}

impl CliError {
    pub fn new(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            code: 1,
        }
    }

    /// Bad arguments (exit 2, like clap's own usage errors).
    pub fn usage(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            code: 2,
        }
    }

    /// Already reported (doctor's failed checks): exit non-zero without another message.
    pub fn silent(code: u8) -> Self {
        Self {
            message: String::new(),
            code,
        }
    }
}

impl fmt::Display for CliError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl From<McpError> for CliError {
    fn from(e: McpError) -> Self {
        Self::new(e.to_string())
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

    /// #2159: a stored login (or none yet) reconnects after `amp-harness login`; AMP_TOKEN never
    /// does, so a rejected token stays rejected until the person sets a new one.
    #[test]
    fn only_a_session_without_a_static_token_reconnects() {
        assert!(reconnector(&settings(None)).is_some());
        assert!(reconnector(&settings(Some("amp_dummy_value"))).is_none());
    }
}
