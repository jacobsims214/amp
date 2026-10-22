//! AMP MCP client built on rmcp (D2, D3, D4).
//!
//! - [`Settings`]: server URL (`--url` > `AMP_MCP_URL` > `config.toml` > hosted default), extra CA
//!   file, protocol mode and the `AMP_TOKEN` static token.
//! - [`AmpMcpClient`]: connects in [`ProtocolMode::Auto`] (stateless 2026-07-28, falling back to the
//!   legacy 2025-11-25 `initialize` handshake) or a forced era; captures the server instructions;
//!   keeps the [`Catalog`] from `tools/list` under the server's cache hint; runs every call under a
//!   120 s timeout; tracks [`Health`].
//!
//! M1-3 adds OAuth and the credential store. This crate must never depend on rpi (D1).

mod catalog;
mod client;
mod error;
mod health;
mod http;
mod protocol;
mod settings;

pub use catalog::{Catalog, MAX_CATALOG_TTL, TtlSource};
pub use client::{
    AmpMcpClient, ClientOptions, DEFAULT_CALL_TIMEOUT, DEFAULT_CONNECT_TIMEOUT, ServerSession,
};
pub use error::McpError;
pub use health::Health;
pub use http::{build_http_client, load_ca_file};
pub use protocol::{Era, ProtocolMode};
pub use settings::{
    Auth, DEFAULT_MCP_URL, ENV_CA_FILE, ENV_MCP_URL, ENV_PROTOCOL, ENV_TOKEN, FileConfig,
    Overrides, ServerSection, Settings, Source, StaticToken, default_config_path,
};

/// The rmcp crate this client is built on, re-exported so downstream crates use the pinned version.
pub use rmcp;
