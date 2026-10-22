//! AMP harness core: the glue between amp-mcp and the rpi agent crates (D1, D5-D9).
//!
//! M1-4:
//! - [`project`]: the `.amp.json` project marker (D4; its stale `amp_api` field is ignored).
//! - [`tickets`]: AMP task records with the web UI's state words (`Ready`, `In progress`, `Waiting`,
//!   `Question`, `Scheduled`, `Done`).
//! - [`tools`]: the tool contract (D11) and calling AMP tools for JSON from harness code.
//!
//! M2-1:
//! - [`assets`]: the agent roster and skills generated from `v2/.opencode` (D5), embedded, with
//!   user (`~/.config/amp-harness/`) and repo (`.amp/harness/`) per-file overrides.
//!
//! Depends on amp-mcp and rpi-ai/agent/tools/harness, never on amp-tui.

pub mod assets;
pub mod project;
pub mod tickets;
pub mod tools;
