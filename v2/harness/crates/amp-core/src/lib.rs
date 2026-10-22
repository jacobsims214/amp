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
//! M2-3 (#2091):
//! - [`models`]: providers, models and the model each role runs on (shipped `models.toml` + the
//!   user's `config.toml`; opencode-style, several providers at once, no default provider).
//! - [`providers`]: live rpi providers from that config: keys (env, then keychain), the stall
//!   guard (header / stream-idle timeouts), and the faux provider for tests.
//!
//! M2-2 (#2093):
//! - [`roles`]: the role registry (prompt, preloaded skills, model key, policy) built from the assets.
//! - [`policy`]: each role's opencode `permission` rules, enforced in an rpi before-tool hook; a
//!   denied call is a tool error the model sees, nothing ever prompts, and children get neither
//!   the question tool nor the dispatch tool.
//!
//! Depends on amp-mcp and rpi-ai/agent/tools/harness, never on amp-tui.

pub mod assets;
pub mod models;
pub mod policy;
pub mod project;
pub mod providers;
pub mod roles;
pub mod tickets;
pub mod tools;
