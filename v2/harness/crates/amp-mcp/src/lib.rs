//! AMP MCP client built on rmcp (D2, D3, D4).
//!
//! - [`Settings`]: server URL (`--url` > `AMP_MCP_URL` > `config.toml` > hosted default), extra CA
//!   file, protocol mode and the `AMP_TOKEN` static token.
//! - [`AmpMcpClient`]: connects in [`ProtocolMode::Auto`] (stateless 2026-07-28, falling back to the
//!   legacy 2025-11-25 `initialize` handshake) or a forced era; captures the server instructions;
//!   keeps the [`Catalog`] from `tools/list` under the server's cache hint; runs every call under a
//!   120 s timeout; tracks [`Health`].
//!
//! - [`oauth`]: browser login (`login`/`logout`) and [`oauth::connect`], which picks `AMP_TOKEN`,
//!   else a stored OAuth login (refreshed as needed), else no credentials (D3).
//! - [`CredentialBackend`]: logins in the OS keychain keyed by server URL, or a 0600 file.
//!
//! This crate must never depend on rpi (D1).

mod catalog;
mod client;
mod credentials;
mod error;
mod health;
mod http;
mod keychain;
pub mod oauth;
mod protocol;
mod settings;

pub use catalog::{Catalog, MAX_CATALOG_TTL, TtlSource};
pub use client::{
    AmpMcpClient, ClientOptions, DEFAULT_CALL_TIMEOUT, DEFAULT_CONNECT_TIMEOUT,
    DEFAULT_RECONNECT_INTERVAL, RECONNECT_TIMEOUT, Reconnector, ServerSession,
    origin as url_origin,
};
pub use credentials::{
    AmpCredentialStore, CREDENTIALS_FILE_NAME, CredentialBackend, ENV_CREDENTIAL_STORE,
    ENV_KEYCHAIN_SERVICE, KEYCHAIN_SERVICE, credential_key, keychain_available, keychain_service,
};
pub use error::{FailureKind, McpError};
pub use health::Health;
pub use http::{
    DEFAULT_OAUTH_REQUEST_TIMEOUT, OAUTH_CONNECT_TIMEOUT, build_http_client,
    build_oauth_http_client, load_ca_file,
};
pub use keychain::{
    ENV_KEYCHAIN_PROMPTS, ENV_KEYCHAIN_TRACE, KEYCHAIN_REPAIR_HINT, KeychainError,
    KeychainErrorKind, keychain_delete, keychain_forget, keychain_get, keychain_prompts_allowed,
    keychain_reads, keychain_set, repair_hint,
};
pub use protocol::{Era, ProtocolMode};
pub use settings::{
    Auth, DEFAULT_MCP_URL, ENV_CA_FILE, ENV_MCP_URL, ENV_PROTOCOL, ENV_TOKEN, ENV_WEB_URL,
    FileConfig, Overrides, ServerSection, Settings, Source, StaticToken, default_config_path,
};

/// The rmcp crate this client is built on, re-exported so downstream crates use the pinned version.
pub use rmcp;
