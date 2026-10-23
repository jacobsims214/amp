//! Connection flags shared by the commands that talk to AMP, and the CLI's error type.

use std::{fmt, path::PathBuf};

use amp_core::tools::ToolCallError;
use amp_mcp::{
    AmpMcpClient, Auth, ClientOptions, CredentialBackend, McpError, Overrides, ProtocolMode,
    Settings, oauth,
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
