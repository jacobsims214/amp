//! AMP MCP client built on rmcp (D2, D3).
//!
//! Scaffold only (M1-1). M1-2 adds connect (Auto lifecycle), the tool catalog, per-call timeouts and
//! health; M1-3 adds OAuth and the credential store. This crate must never depend on rpi (D1).

/// The rmcp crate this client is built on, re-exported so downstream crates use the pinned version.
pub use rmcp;
