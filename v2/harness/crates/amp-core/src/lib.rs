//! AMP harness core: the glue between amp-mcp and the rpi agent crates (D1, D5-D9).
//!
//! M1-4:
//! - [`project`]: the `.amp.json` project marker (D4; its stale `amp_api` field is ignored).
//! - [`tickets`]: AMP task records with the web UI's state words (`Ready`, `In progress`, `Waiting`,
//!   `Question`, `Scheduled`, `Done`).
//! - [`tools`]: the tool contract (D11) and calling AMP tools for JSON from harness code.
//!
//! Depends on amp-mcp and rpi-ai/agent/tools/harness, never on amp-tui.

pub mod project;
pub mod tickets;
pub mod tools;
