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
//! M2-4 (#2095):
//! - [`agent`]: [`agent::Runtime`] (what every session shares) and [`agent::Runtime::build`], one
//!   rpi `AgentHarness` per role: model, rpi-tools + AMP tools filtered by the role's policy, the
//!   system prompt (role + AMP server instructions + `.amp.json` context + preloaded skills +
//!   AGENTS.md), and an in-memory or JSONL session.
//! - [`mcp_tools`]: every AMP MCP tool as a native rpi tool under AMP's own name.
//! - [`events`]: rpi agent events as `run --json` events, the channel emitter and usage totals.
//! - [`faux`]: faux-provider scripts from a JSON file (`AMP_HARNESS_FAUX_SCRIPT`).
//!
//! M2-5 (#2096):
//! - [`supervisor`]: the worker supervisor behind the manager's `amp_run_ticket` (D7): dispatch in
//!   AMP, a git worktree per ticket, an independent harness per worker on its own model, a cap on
//!   parallel workers, a wall-clock timeout, a turn budget and cancellation, ending in a
//!   [`supervisor::WorkerReport`] with a [`supervisor::WorkerOutcome`].
//!
//! M2-6 (#2098):
//! - [`run_report`]: automatic run reporting (D8). [`run_report::RunReporter`] fills the model,
//!   tokens, cost and git fields a worker left out of `amp_complete_task` / `amp_block_task`, and
//!   reports a worker that ended without closing its ticket with `amp_report_run` ("abandoned:
//!   <reason>"); [`run_report::SessionUsageTool`] is the native `amp_session_usage`.
//!
//! M3-2 (#2104):
//! - [`meta`]: the AMP metadata poller behind the chat TUI's header and status line (D10): its own
//!   MCP client, never the model; polled on #2078 §2's intervals and refreshed right after any
//!   `amp_*` tool call; the TUI reads one shared [`meta::AmpMeta`] snapshot.
//!
//! Depends on amp-mcp and rpi-ai/agent/tools/harness, never on amp-tui.

pub mod agent;
pub mod approval;
pub mod assets;
pub mod events;
pub mod faux;
pub mod links;
pub mod mcp_tools;
pub mod meta;
pub mod models;
pub mod policy;
pub mod project;
pub mod provider_error;
pub mod providers;
pub mod repo_config;
pub mod request_time;
pub mod roles;
pub mod run_report;
pub mod supervisor;
pub mod text_tool_calls;
pub mod tickets;
pub mod tools;
