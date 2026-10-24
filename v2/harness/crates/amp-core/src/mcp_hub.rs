//! The MCP connection hub (#2239, H6): every configured `[mcp.<name>]` server, connected once per
//! process and shared by the manager and every worker (D6, D8, D9 of
//! decisions/amp-harness-configurability.md).
//!
//! # Lifecycle (D6)
//!
//! [`McpHub`] lives in [`crate::agent::Runtime`] (`rt.mcp_hub`). [`McpHub::start`] gives every
//! enabled shared server one background task (a *cell*) that owns its [`ExternalClient`]:
//! connect, list tools, then watch the connection (every [`HubOptions::poll`]) for a closed
//! transport (the stdio server exited, the HTTP session broke) and for
//! `notifications/tools/list_changed`.
//! - `startup = "background"` (default): connected in parallel. The chat opens at once; `run -p`
//!   and workers wait for the first attempt ([`McpHub::wait_started`]), bounded by the server's
//!   `startup_timeout_secs`. A timeout is a warning, not a failure.
//! - `startup = "eager"`: the chat waits too. `startup = "lazy"`: idle until [`McpHub::restart`]
//!   (`/mcp test`, the CLI) starts it.
//! - A failure (spawn, exit, transport, timeout) moves the server to `failed` and retries after
//!   1, 2, 4 … 60 s ([`Backoff`]); after 5 failures in 10 minutes it stops until
//!   [`McpHub::restart`]. A rejected login is `needs-login` and a configuration problem (a missing
//!   secret, an unset variable) is `stopped`: neither fixes itself, so neither is retried.
//! - `scope = "worktree"` servers get one instance per worker worktree:
//!   [`McpHub::start_worktree`] when the supervisor builds the worker, [`McpHub::release_worktree`]
//!   when it finishes. Their stderr logs are `<server>-<ticket>.log`.
//! - [`McpHub::shutdown`] closes every server gracefully (a stdio server gets EOF, then its process
//!   group is killed). Call it before the process exits; dropping the clients also kills stdio
//!   process groups, but without the grace period.
//!
//! # Tools (D4, D9)
//!
//! Each server tool becomes an [`ExternalTool`] named `mcp__<server>__<tool>`
//! ([`crate::mcp_config::ToolNames`]), next to AMP's `amp_*` tools. A tool holds its server's cell,
//! not a client, so a reconnect is invisible to the session. A call is bounded by the server's
//! `call_timeout_secs`; a cancelled run drops it (rmcp sends `notifications/cancelled`).
//! Text beyond `max_output_tokens` (4 characters per token) is cut: the model gets the head plus a
//! line naming the file under `<state>/mcp-output/<ticket or session>/` that holds all of it
//! ([`cap_output`]). Those files are private (0600, in 0700 directories) and pruned by age and
//! total size ([`OutputRetention`], [`prune_output`]) when the hub starts and every hour after
//! (#2309).
//!
//! # Sessions
//!
//! [`crate::agent::Runtime::build`] takes [`McpHub::session_view`] for the role (tools, the
//! servers' instructions) and then [`McpHub::attach`]es the harness. When a server becomes ready,
//! changes its tool list or stops, the hub *reconciles* every attached harness: its `mcp__*` tools
//! are replaced (`set_tools`) and its `<mcp_server_instructions>` blocks are swapped
//! ([`with_mcp_instructions`]), effective from the next run. Workers built later see the current
//! state.
//!
//! # Reload (#2242)
//!
//! [`McpHub::reload`] puts a new configuration in place and restarts only what changed: unchanged
//! servers keep their connection, new and changed ones get a fresh cell, and removed, disabled or
//! changed old cells are *retired*. A retired cell keeps serving the sessions that still hold its
//! tools (a running worker keeps its snapshot) and stops once none does. Unchanged servers that
//! were stopped or needed a login are tried again.
//!
//! # Trust (#2241)
//!
//! A server from a repo's `.amp/harness/config.toml` runs only once trusted
//! ([`crate::mcp_trust`]): [`McpHub::new`] reads the trust store named in
//! [`HubOptions::trust_store`] and every enabled repo server that is not trusted at its current
//! fingerprint (nor covered by [`HubOptions::trust_all`]) is [`ServerStatus::Untrusted`]: never
//! spawned or connected, no worktree instances, no tools. [`McpHub::trust`] lets one start (the
//! chat calls it after asking; recording it in the store is the caller's job).
//!
//! # Health
//!
//! [`McpHub::health`] (one [`ServerHealth`] per server, including disabled and broken ones),
//! [`McpHub::summary`] (`mcp 3/4` for the status line) and [`McpHub::subscribe`] (one
//! [`HubEvent`] per change, for transcript lines such as `github: 12 tools`).

use std::{
    collections::{BTreeMap, BTreeSet, VecDeque},
    path::{Path, PathBuf},
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use amp_mcp::{
    ConfigPaths, CredentialBackend, ExternalClient, McpError,
    external::http,
    rmcp::model::{JsonObject, Tool as McpToolDef},
};
use rpi_agent::{AgentError, AgentTool, AgentToolResult, ToolResultPartial};
use rpi_ai::types::Tool;
use rpi_harness::{
    agent_harness::AgentHarness,
    types::{HarnessTool, ToolReplay},
};
use serde::Serialize;
use serde_json::Value;
use tokio::{
    sync::{Notify, broadcast, watch},
    task::JoinHandle,
};
use tokio_util::sync::CancellationToken;

use crate::{
    mcp_config::{
        DEFAULT_MAX_OUTPUT_TOKENS, ExpandContext, McpConfig, McpServer, Resolved, Scope, SecretRef,
        Startup, TOOL_PREFIX, ToolNames,
    },
    mcp_http, mcp_stdio,
    mcp_tools::model_text,
    meta::HealthLevel,
    policy::ToolPolicy,
};

/// A server's instructions longer than this are cut (D8).
pub const MAX_INSTRUCTIONS_BYTES: usize = 4096;
/// Overrides `[tools] mcp_max_output_tokens` (the default for servers that set none).
pub const ENV_MAX_OUTPUT_TOKENS: &str = "AMP_HARNESS_MCP_MAX_OUTPUT_TOKENS";
/// Characters per token when estimating output size (D9).
pub const CHARS_PER_TOKEN: u64 = 4;
/// How often a started hub prunes `<state>/mcp-output/` again ([`prune_output`]).
pub const OUTPUT_PRUNE_EVERY: Duration = Duration::from_secs(3600);
/// Empty `<state>/mcp-output/<key>/` directories are removed once they are this old, so one a
/// session has just created is not taken from under it.
const EMPTY_DIR_GRACE: Duration = Duration::from_secs(3600);
/// The tag around each server's instructions in the system prompt.
pub const INSTRUCTIONS_TAG: &str = "mcp_server_instructions";

const CONTEXT_OPEN: &str = "<amp_context>";

// ---------------------------------------------------------------------------------------------
// Options

/// Reconnect delays after failures (D6): `initial`, doubling up to `max`; after `give_up_after`
/// failures within `window` the server stops until restarted.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Backoff {
    pub initial: Duration,
    pub max: Duration,
    pub give_up_after: usize,
    pub window: Duration,
}

impl Default for Backoff {
    fn default() -> Self {
        Self {
            initial: Duration::from_secs(1),
            max: Duration::from_secs(60),
            give_up_after: 5,
            window: Duration::from_secs(600),
        }
    }
}

impl Backoff {
    /// The wait after the `failures`-th failure in the window (1-based): 1, 2, 4 … `max`.
    pub fn delay(&self, failures: usize) -> Duration {
        let exp = failures.saturating_sub(1).min(20) as u32;
        self.initial.saturating_mul(1u32 << exp).min(self.max)
    }
}

/// What the hub needs from the runtime.
#[derive(Debug, Clone)]
pub struct HubOptions {
    /// The configuration paths (for `${repo}`, `${config_dir}` and relative paths).
    pub paths: ConfigPaths,
    /// `<state>/mcp-logs/` and `<state>/mcp-output/` live here.
    pub state_dir: PathBuf,
    /// A stdio server's working directory when it sets no `cwd` (the repo root, else the
    /// harness's own); a worktree instance uses its worktree.
    pub default_cwd: PathBuf,
    /// The keychain service for secrets, `None` when the keychain is off
    /// (`AMP_CREDENTIAL_STORE=file`).
    pub keychain: Option<String>,
    pub backoff: Backoff,
    /// How often a connected server is checked for a closed connection or a changed tool list.
    pub poll: Duration,
    /// `[tools] mcp_max_output_tokens` / [`ENV_MAX_OUTPUT_TOKENS`]: the cap for servers that keep
    /// the default `max_output_tokens`.
    pub default_max_output_tokens: Option<u64>,
    /// The repo-server trust store (`<state>/trust.toml`, #2241). `None`: no store, so no repo
    /// server is trusted unless `trust_all`.
    pub trust_store: Option<PathBuf>,
    /// `--trust-repo-mcp` / `AMP_HARNESS_TRUST_REPO_MCP`: every repo server may start.
    pub trust_all: bool,
    /// How long and how much spilled output `<state>/mcp-output/` keeps (#2309): pruned at
    /// [`McpHub::start`] and every [`OUTPUT_PRUNE_EVERY`]. `None` (the default here; a session's
    /// runtime sets the configured one) never prunes, so a short-lived CLI hub cannot apply other
    /// limits than the person's.
    pub output_retention: Option<OutputRetention>,
}

/// How much spilled MCP output (`<state>/mcp-output/`) is kept (#2309): `[tools]
/// mcp_output_keep_days` (default 7) and `mcp_output_max_mb` (default 512). `None` keeps files
/// regardless of age or of total size.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct OutputRetention {
    pub max_age: Option<Duration>,
    pub max_bytes: Option<u64>,
}

impl OutputRetention {
    pub const DEFAULT_KEEP_DAYS: u64 = 7;
    pub const DEFAULT_MAX_MB: u64 = 512;

    /// From `[tools] mcp_output_keep_days` / `mcp_output_max_mb`; 0 turns that limit off.
    pub fn from_config(keep_days: Option<u64>, max_mb: Option<u64>) -> Self {
        let days = keep_days.unwrap_or(Self::DEFAULT_KEEP_DAYS);
        let mb = max_mb.unwrap_or(Self::DEFAULT_MAX_MB);
        Self {
            max_age: (days > 0).then(|| Duration::from_secs(days.saturating_mul(86_400))),
            max_bytes: (mb > 0).then(|| mb.saturating_mul(1024 * 1024)),
        }
    }
}

impl Default for OutputRetention {
    fn default() -> Self {
        Self::from_config(None, None)
    }
}

impl HubOptions {
    /// The production options: the real keychain unless `AMP_CREDENTIAL_STORE=file`.
    pub fn new(paths: ConfigPaths, state_dir: PathBuf, default_cwd: PathBuf) -> Self {
        let file_store = std::env::var(amp_mcp::ENV_CREDENTIAL_STORE)
            .is_ok_and(|v| v.trim().eq_ignore_ascii_case("file"));
        Self {
            paths,
            default_cwd,
            keychain: (!file_store).then(amp_mcp::keychain_service),
            backoff: Backoff::default(),
            poll: Duration::from_millis(500),
            default_max_output_tokens: None,
            trust_store: Some(crate::mcp_trust::TrustStore::path_in(&state_dir)),
            trust_all: crate::mcp_trust::trust_all_from(&|k| std::env::var(k).ok()),
            output_retention: None,
            state_dir,
        }
    }
}

// ---------------------------------------------------------------------------------------------
// Status and health

/// Where one server (instance) stands.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(tag = "state", rename_all = "kebab-case")]
pub enum ServerStatus {
    /// `startup = "lazy"`, not started yet.
    Idle,
    Starting,
    Ready,
    /// The last attempt failed; the next one is due in `retry_in_ms`.
    Failed {
        reason: String,
        retry_in_ms: u64,
    },
    /// Not retried until restarted: a configuration problem, or too many failures.
    Stopped {
        reason: String,
    },
    /// The server rejected our credentials; log in (or fix them) and restart.
    NeedsLogin {
        reason: String,
    },
    /// `enabled = false`.
    Disabled,
    /// A repo server the person has not trusted (or whose config changed since): never started
    /// (#2241). `reason` says how to trust it.
    Untrusted {
        reason: String,
    },
}

impl ServerStatus {
    /// One word: idle, starting, ready, failed, stopped, needs-login, disabled.
    pub fn word(&self) -> &'static str {
        match self {
            Self::Idle => "idle",
            Self::Starting => "starting",
            Self::Ready => "ready",
            Self::Failed { .. } => "failed",
            Self::Stopped { .. } => "stopped",
            Self::NeedsLogin { .. } => "needs-login",
            Self::Disabled => "disabled",
            Self::Untrusted { .. } => "untrusted",
        }
    }

    /// The header dot: ready/idle/disabled ok, starting/failed degraded, stopped/login down.
    pub fn level(&self) -> HealthLevel {
        match self {
            Self::Ready | Self::Idle | Self::Disabled => HealthLevel::Ok,
            Self::Starting | Self::Failed { .. } | Self::Untrusted { .. } => HealthLevel::Degraded,
            Self::Stopped { .. } | Self::NeedsLogin { .. } => HealthLevel::Down,
        }
    }

    pub fn reason(&self) -> Option<&str> {
        match self {
            Self::Failed { reason, .. }
            | Self::Stopped { reason }
            | Self::NeedsLogin { reason }
            | Self::Untrusted { reason } => Some(reason),
            _ => None,
        }
    }

    /// Whether its tools stay registered: a transient state keeps them (a call then says the
    /// server is reconnecting); stopped, needs-login and disabled drop them.
    fn keeps_tools(&self) -> bool {
        matches!(self, Self::Ready | Self::Starting | Self::Failed { .. })
    }
}

impl std::fmt::Display for ServerStatus {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Failed {
                reason,
                retry_in_ms,
            } => write!(
                f,
                "failed: {reason} (retrying in {})",
                human_ms(*retry_in_ms)
            ),
            Self::Stopped { reason } => write!(f, "stopped: {reason}"),
            Self::NeedsLogin { reason } => write!(f, "needs login: {reason}"),
            Self::Untrusted { reason } => write!(f, "not started, not trusted: {reason}"),
            other => f.write_str(other.word()),
        }
    }
}

fn human_ms(ms: u64) -> String {
    if ms >= 1000 && ms.is_multiple_of(1000) {
        format!("{} s", ms / 1000)
    } else {
        format!("{ms} ms")
    }
}

/// One server (or one worktree instance of it) for `/mcp`, `doctor` and the status line.
#[derive(Debug, Clone, Serialize)]
pub struct ServerHealth {
    pub name: String,
    /// stdio, http or sse.
    pub transport: &'static str,
    /// shared or worktree.
    pub scope: &'static str,
    /// The worktree instance's key (the ticket id); `None` for a shared server.
    pub instance: Option<String>,
    pub status: ServerStatus,
    pub tools: usize,
    /// `name version` from the handshake.
    pub server: Option<String>,
    pub protocol: Option<String>,
    pub pid: Option<u32>,
    pub stderr_log: Option<PathBuf>,
    /// Reconnects so far.
    pub restarts: u32,
}

/// Ready servers out of those that should run (`mcp 3/4`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize)]
pub struct HubSummary {
    pub ready: usize,
    pub total: usize,
    pub level: HubLevel,
}

/// [`HealthLevel`] for serialization.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum HubLevel {
    #[default]
    Ok,
    Degraded,
    Down,
}

impl From<HealthLevel> for HubLevel {
    fn from(l: HealthLevel) -> Self {
        match l {
            HealthLevel::Ok => Self::Ok,
            HealthLevel::Degraded => Self::Degraded,
            HealthLevel::Down => Self::Down,
        }
    }
}

impl std::fmt::Display for HubSummary {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "mcp {}/{}", self.ready, self.total)
    }
}

/// One change, for a transcript line.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum HubEvent {
    Ready {
        server: String,
        instance: Option<String>,
        tools: usize,
    },
    ToolsChanged {
        server: String,
        instance: Option<String>,
        tools: usize,
    },
    Failed {
        server: String,
        instance: Option<String>,
        status: ServerStatus,
    },
}

impl HubEvent {
    /// `github: 12 tools`, `github: tools changed (13 tools)`, `github: failed: ...`.
    pub fn line(&self) -> String {
        let label = |s: &str, i: &Option<String>| match i {
            Some(i) => format!("{s} (#{i})"),
            None => s.to_owned(),
        };
        match self {
            Self::Ready {
                server,
                instance,
                tools,
            } => format!("{}: {}", label(server, instance), count(*tools, "tool")),
            Self::ToolsChanged {
                server,
                instance,
                tools,
            } => format!(
                "{}: tools changed ({})",
                label(server, instance),
                count(*tools, "tool")
            ),
            Self::Failed {
                server,
                instance,
                status,
            } => {
                // The reason often names the server itself (`MCP server `x`: …`): once is enough
                // (#2334).
                let reason = status.reason().map(|r| without_server_prefix(r, server));
                let status = match (status, reason) {
                    (ServerStatus::Failed { retry_in_ms, .. }, Some(r)) => {
                        format!("failed: {r} (retrying in {})", human_ms(*retry_in_ms))
                    }
                    (ServerStatus::Stopped { .. }, Some(r)) => format!("stopped: {r}"),
                    (ServerStatus::NeedsLogin { .. }, Some(r)) => format!("needs login: {r}"),
                    (other, _) => other.to_string(),
                };
                let next = if is_unset_variable(&status) {
                    "; set it where you start amp-harness, then /reload"
                } else {
                    ""
                };
                format!("MCP server {}: {status}{next}", label(server, instance))
            }
        }
    }

    pub fn is_error(&self) -> bool {
        matches!(self, Self::Failed { .. })
    }
}

/// `reason` without a leading ``MCP server `name`: `` (the connect errors start with it).
fn without_server_prefix<'a>(reason: &'a str, server: &str) -> &'a str {
    reason
        .strip_prefix(&format!("MCP server `{server}`: "))
        .unwrap_or(reason)
}

/// Whether a failure is an environment variable that isn't set (the person sets it where the
/// harness starts; nothing else fixes it).
fn is_unset_variable(status: &str) -> bool {
    status.contains("environment variable ") && status.contains(" is not set")
}

/// Which servers' failures a transcript has shown (#2334): a reload retries every stopped server,
/// and a failing one fails again the same way, so the transcript says it once and again only when
/// the failure changes (or after the server was ready in between). Transient retries' "retrying
/// in N s" doesn't count as a change.
#[derive(Debug, Default)]
pub struct FailureLog {
    shown: BTreeMap<(String, Option<String>), String>,
}

impl FailureLog {
    /// The transcript line for `event`, or `None` when it would repeat a failure already shown.
    pub fn line(&mut self, event: &HubEvent) -> Option<String> {
        match event {
            HubEvent::Failed {
                server,
                instance,
                status,
            } => {
                let seen = format!("{} {}", status.word(), status.reason().unwrap_or_default());
                let key = (server.clone(), instance.clone());
                if self.shown.get(&key) == Some(&seen) {
                    return None;
                }
                self.shown.insert(key, seen);
            }
            HubEvent::Ready {
                server, instance, ..
            } => {
                self.shown.remove(&(server.clone(), instance.clone()));
            }
            HubEvent::ToolsChanged { .. } => {}
        }
        Some(event.line())
    }
}

/// What [`McpHub::reload`] changed (#2242), by server name.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct McpReload {
    /// Servers that are new in the configuration.
    pub added: Vec<String>,
    /// Servers no longer configured (their tables are gone).
    pub removed: Vec<String>,
    /// Servers whose table changed (enabled, command, url, ...), or whose keychain secret or
    /// env_file contents changed (#2311): restarted with the new one.
    pub changed: Vec<String>,
    /// Unchanged servers that were stopped or needed a login, tried again (a secret stored or a
    /// login done in another terminal).
    pub retried: Vec<String>,
    /// The configuration's errors now (`mcp.x.url: ...`); those servers are left out.
    pub problems: Vec<String>,
    /// Repo servers that may not start until trusted (#2241): `amp-harness mcp trust <name>`,
    /// then reload.
    pub untrusted: Vec<String>,
}

impl McpReload {
    fn diff(old: &McpConfig, new: &McpConfig) -> Self {
        let names = |c: &McpConfig| -> BTreeSet<String> {
            c.servers.keys().chain(c.broken.iter()).cloned().collect()
        };
        let (before, after) = (names(old), names(new));
        Self {
            added: after.difference(&before).cloned().collect(),
            removed: before.difference(&after).cloned().collect(),
            changed: after
                .intersection(&before)
                .filter(|n| old.servers.get(*n) != new.servers.get(*n))
                .cloned()
                .collect(),
            retried: Vec::new(),
            problems: new.errors().map(|p| p.to_string()).collect(),
            untrusted: Vec::new(),
        }
    }

    /// Nothing changed and nothing is wrong.
    pub fn is_empty(&self) -> bool {
        self.added.is_empty()
            && self.removed.is_empty()
            && self.changed.is_empty()
            && self.retried.is_empty()
            && self.problems.is_empty()
            && self.untrusted.is_empty()
    }
}

fn count(n: usize, what: &str) -> String {
    if n == 1 {
        format!("1 {what}")
    } else {
        format!("{n} {what}s")
    }
}

// ---------------------------------------------------------------------------------------------
// Cells

/// One running server (instance) and its state.
struct Cell {
    server: McpServer,
    /// The worktree instance's key; `None` for a shared server.
    instance: Option<String>,
    worktree: Option<PathBuf>,
    log_name: String,
    st: Mutex<CellState>,
    /// A restart request.
    wake: Notify,
    task_running: AtomicBool,
    /// [`crate::mcp_stamp::server_stamp`] when the cell was made (shared cells; empty for
    /// worktree ones): a reload restarts the server when it differs, though the table is the same.
    stamp: String,
    /// Stops this cell alone (a shared server removed or changed by a reload, #2242). A child of
    /// the hub's token, so [`McpHub::shutdown`] stops it too. Worktree cells use their set's.
    stop: CancellationToken,
}

struct CellState {
    status: ServerStatus,
    client: Option<Arc<ExternalClient>>,
    tools: Vec<McpToolDef>,
    info: Option<amp_mcp::ExternalInfo>,
    pid: Option<u32>,
    stderr_log: Option<PathBuf>,
    restarts: u32,
    /// Finished attempts (the first one ends the startup wait).
    attempts: u32,
    failures: VecDeque<Instant>,
    /// When the current first attempt started (for the startup wait's deadline).
    first_attempt: Option<Instant>,
}

impl Cell {
    fn new(server: McpServer, instance: Option<String>, worktree: Option<PathBuf>) -> Self {
        let log_name = match &instance {
            Some(i) => format!("{}-{i}", server.name),
            None => server.name.clone(),
        };
        let status = if server.startup == Startup::Lazy {
            ServerStatus::Idle
        } else {
            ServerStatus::Starting
        };
        Self {
            server,
            instance,
            worktree,
            log_name,
            st: Mutex::new(CellState {
                status,
                client: None,
                tools: Vec::new(),
                info: None,
                pid: None,
                stderr_log: None,
                restarts: 0,
                attempts: 0,
                failures: VecDeque::new(),
                first_attempt: None,
            }),
            wake: Notify::new(),
            task_running: AtomicBool::new(false),
            stamp: String::new(),
            stop: CancellationToken::new(),
        }
    }

    /// A shared server's cell, stoppable on its own under `hub` (the hub's token).
    fn shared(server: McpServer, hub: &CancellationToken, stamp: String) -> Self {
        let mut cell = Self::new(server, None, None);
        cell.stop = hub.child_token();
        cell.stamp = stamp;
        cell
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, CellState> {
        self.st.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The live client and its tools, or why there is none (naming the server).
    fn connection(&self) -> Result<(Arc<ExternalClient>, Vec<McpToolDef>), String> {
        let client = self
            .client()
            .map_err(|e| format!("MCP server `{}` {e}", self.server.name))?;
        Ok((client, self.lock().tools.clone()))
    }

    fn client(&self) -> Result<Arc<ExternalClient>, String> {
        let st = self.lock();
        match &st.client {
            Some(c) if !c.is_closed() => Ok(Arc::clone(c)),
            _ => Err(match &st.status {
                ServerStatus::Ready | ServerStatus::Starting => {
                    "is reconnecting; try again shortly".to_owned()
                }
                other => format!("is not connected ({other})"),
            }),
        }
    }

    fn health(&self) -> ServerHealth {
        let st = self.lock();
        ServerHealth {
            name: self.server.name.clone(),
            transport: self.server.transport.kind().as_str(),
            scope: scope_word(self.server.scope),
            instance: self.instance.clone(),
            status: st.status.clone(),
            tools: if st.status.keeps_tools() {
                st.tools.len()
            } else {
                0
            },
            server: st.info.as_ref().and_then(|i| {
                i.server_name
                    .as_ref()
                    .map(|n| format!("{n} {}", i.server_version.as_deref().unwrap_or("")))
                    .map(|s| s.trim().to_owned())
            }),
            protocol: st.info.as_ref().map(|i| i.protocol_version.to_string()),
            pid: st.pid,
            stderr_log: st.stderr_log.clone(),
            restarts: st.restarts,
        }
    }

    fn take_client(&self) -> Option<Arc<ExternalClient>> {
        let mut st = self.lock();
        st.pid = None;
        st.client.take()
    }
}

fn scope_word(scope: Scope) -> &'static str {
    match scope {
        Scope::Shared => "shared",
        Scope::Worktree => "worktree",
    }
}

/// Closes a client gracefully when nobody else holds it (an in-flight call does); otherwise the
/// last holder's drop closes it (and kills a stdio group).
async fn close(client: Arc<ExternalClient>) {
    if let Ok(client) = Arc::try_unwrap(client) {
        client.shutdown().await;
    }
}

/// How an attempt failed.
enum Fail {
    /// Retried with backoff.
    Transient(String),
    /// Not retried: log in (or fix the credentials), then restart.
    Auth(String),
    /// Not retried: fix the configuration, then restart.
    Config(String),
}

fn classify(e: McpError) -> Fail {
    if e.is_auth_required() {
        return Fail::Auth(e.to_string());
    }
    match e {
        McpError::Config(_) | McpError::CaFile { .. } => Fail::Config(e.to_string()),
        other => Fail::Transient(other.to_string()),
    }
}

/// The servers that run as one shared instance: enabled, `scope = "shared"`.
fn shared_servers(config: &McpConfig) -> impl Iterator<Item = &McpServer> {
    config.enabled().filter(|s| s.scope == Scope::Shared)
}

// ---------------------------------------------------------------------------------------------
// The hub

/// The process-wide MCP hub. See the module docs.
pub struct McpHub {
    inner: Arc<Inner>,
}

struct Inner {
    opts: HubOptions,
    /// The configuration in effect; [`McpHub::reload`] replaces it (#2242).
    config: Mutex<Arc<McpConfig>>,
    /// Names never inherited by stdio servers ([`mcp_stdio::strip_set`]); updated when the
    /// runtime learns another secret name (`--token-env`).
    strip: Mutex<BTreeSet<String>>,
    /// The person's own credential variables, which no repo server may read (#2308,
    /// [`crate::mcp_trust::check_repo_env`]).
    protected: Mutex<BTreeSet<String>>,
    /// The shared servers by name; [`McpHub::reload`] adds, replaces and removes cells.
    shared: Mutex<BTreeMap<String, Arc<Cell>>>,
    /// Shared cells a reload removed or replaced, still running until no session holds their
    /// tools (a running worker keeps its snapshot, #2242). [`sweep_retired`] stops them.
    retired: Mutex<Vec<Arc<Cell>>>,
    /// A [`sweep_retired`] task is running.
    sweeping: AtomicBool,
    worktrees: Mutex<BTreeMap<String, Arc<WorktreeSet>>>,
    attached: Mutex<Vec<Arc<Attached>>>,
    /// Serializes reconciles, so two changes never interleave `get_tools`/`set_tools`.
    apply: tokio::sync::Mutex<()>,
    rev: watch::Sender<u64>,
    events: broadcast::Sender<HubEvent>,
    cancel: CancellationToken,
    tasks: Mutex<Vec<JoinHandle<()>>>,
    started: AtomicBool,
    /// Repo servers that may not start (#2241), by name, until [`McpHub::trust`].
    untrusted: Mutex<BTreeMap<String, crate::mcp_trust::Untrusted>>,
}

/// The worktree instances of one worker (one per `scope = "worktree"` server).
pub struct WorktreeSet {
    key: String,
    cells: BTreeMap<String, Arc<Cell>>,
    cancel: CancellationToken,
    tasks: Mutex<Vec<JoinHandle<()>>>,
}

impl WorktreeSet {
    pub fn key(&self) -> &str {
        &self.key
    }

    pub fn health(&self) -> Vec<ServerHealth> {
        self.cells.values().map(|c| c.health()).collect()
    }

    /// This set's instance of `name`: its client and tools ([`McpHub::connection`]).
    pub fn connection(&self, name: &str) -> Result<(Arc<ExternalClient>, Vec<McpToolDef>), String> {
        self.cells
            .get(name)
            .ok_or_else(|| format!("MCP server `{name}` has no instance here"))?
            .connection()
    }
}

impl std::fmt::Debug for WorktreeSet {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("WorktreeSet")
            .field("key", &self.key)
            .field("servers", &self.cells.keys().collect::<Vec<_>>())
            .finish()
    }
}

/// A live session the hub keeps in step.
struct Attached {
    harness: Weak<AgentHarness>,
    role: String,
    policy: Arc<ToolPolicy>,
    output_key: String,
    worktree: Option<Arc<WorktreeSet>>,
    /// The view last applied, to skip no-op updates.
    applied: Mutex<String>,
}

/// What a session gets from the hub now: its `mcp__*` tools and the servers' instruction blocks.
pub struct SessionView {
    pub tools: Vec<HarnessTool>,
    /// Each tool's model-facing name and its `mcp` policy subject `<server>/<tool>` (#2240), for
    /// [`ToolPolicy::learn_mcp_subjects`] before the tools are filtered.
    pub subjects: Vec<(String, String)>,
    /// `<mcp_server_instructions server="name">…</mcp_server_instructions>`, one per server.
    pub instructions: Vec<String>,
    /// Identifies this view (tool names and instructions), to skip no-op updates.
    pub signature: String,
}

impl std::fmt::Debug for McpHub {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("McpHub")
            .field("servers", &self.inner.shared_names())
            .finish_non_exhaustive()
    }
}

impl Drop for McpHub {
    fn drop(&mut self) {
        // The cell tasks close their clients; a client dropped with the runtime kills its group.
        self.inner.cancel.cancel();
    }
}

impl McpHub {
    /// A hub for `config`'s enabled servers. Nothing runs until [`Self::start`].
    pub fn new(config: McpConfig, opts: HubOptions) -> Self {
        let cancel = CancellationToken::new();
        let untrusted = untrusted_in(&config, &opts);
        let stamps = Stamper::new(&opts);
        let shared = shared_servers(&config)
            .map(|s| {
                let cell = Cell::shared(s.clone(), &cancel, stamps.of(s));
                if let Some(u) = untrusted.get(&s.name) {
                    cell.lock().status = ServerStatus::Untrusted { reason: u.reason() };
                }
                (s.name.clone(), Arc::new(cell))
            })
            .collect();
        let (rev, _) = watch::channel(0);
        let (events, _) = broadcast::channel(64);
        Self {
            inner: Arc::new(Inner {
                opts,
                config: Mutex::new(Arc::new(config)),
                strip: Mutex::new(BTreeSet::new()),
                protected: Mutex::new(BTreeSet::new()),
                shared: Mutex::new(shared),
                retired: Mutex::new(Vec::new()),
                sweeping: AtomicBool::new(false),
                worktrees: Mutex::new(BTreeMap::new()),
                attached: Mutex::new(Vec::new()),
                apply: tokio::sync::Mutex::new(()),
                rev,
                events,
                cancel,
                tasks: Mutex::new(Vec::new()),
                started: AtomicBool::new(false),
                untrusted: Mutex::new(untrusted),
            }),
        }
    }

    /// The enabled repo servers that may not start yet (#2241), for the chat's question and
    /// warnings.
    pub fn untrusted(&self) -> Vec<crate::mcp_trust::Untrusted> {
        self.inner.untrusted_map().values().cloned().collect()
    }

    /// Lets the repo server `name` start (the person trusted it): it starts now if the hub is
    /// started and it is not lazy, and worktree instances include it from now on. Recording the
    /// decision in the trust store is the caller's job. A server that was not untrusted is fine.
    pub fn trust(&self, name: &str) -> Result<(), String> {
        let was = self
            .inner
            .untrusted
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .remove(name);
        if was.is_none() {
            return match self.inner.config().servers.get(name) {
                Some(_) => Ok(()),
                None => Err(self.unknown(name)),
            };
        }
        if let Some(cell) = self.inner.shared_cell(name) {
            let lazy = cell.server.startup == Startup::Lazy;
            cell.lock().status = if lazy {
                ServerStatus::Idle
            } else {
                ServerStatus::Starting
            };
            if self.inner.started.load(Ordering::Acquire) && !lazy {
                self.spawn_cell(&cell, cell.stop.clone(), None);
            }
        }
        self.inner.bump();
        Ok(())
    }

    /// [`Self::trust`], but only if the server in effect now is exactly `shown` (the one the
    /// person was asked about, #2310, review #2248 finding 15). When a reload replaced it in
    /// between, nothing changes and this returns `Ok(false)`: the new config was never shown, so
    /// it stays untrusted (its record, made from `shown`, does not match it either).
    pub fn trust_as_shown(&self, shown: &McpServer) -> Result<bool, String> {
        let same = self
            .inner
            .config()
            .servers
            .get(&shown.name)
            .is_some_and(|now| {
                now == shown
                    && crate::mcp_trust::fingerprint(now) == crate::mcp_trust::fingerprint(shown)
            });
        if !same {
            return Ok(false);
        }
        self.trust(&shown.name).map(|()| true)
    }

    /// The configuration in effect (problems included): the one it was built from, or the last
    /// one [`Self::reload`] put in place.
    pub fn config(&self) -> Arc<McpConfig> {
        self.inner.config()
    }

    /// Sets the variable names stdio servers never inherit (the runtime's secrets plus
    /// `pass_to_bash`, [`mcp_stdio::strip_set`]).
    pub fn set_strip(&self, names: BTreeSet<String>) {
        *self.inner.strip.lock().unwrap_or_else(|e| e.into_inner()) = names;
    }

    /// Adds variable names that hold the person's own credentials (provider keys, AMP_TOKEN,
    /// `--token-env`, `pass_to_bash`, what their own MCP servers read): a repo server that reads
    /// one is stopped with a config error instead of connecting (#2308). `AMP_TOKEN` is always
    /// protected.
    pub fn protect_env(&self, names: impl IntoIterator<Item = String>) {
        self.inner
            .protected
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .extend(names);
    }

    /// Whether any server is configured to run (shared or per worktree).
    pub fn has_servers(&self) -> bool {
        self.inner.config().enabled().next().is_some()
    }

    /// Starts every enabled shared server except lazy ones, in the background. Needs a tokio
    /// runtime. Calling it again does nothing.
    pub fn start(&self) {
        if self.inner.started.swap(true, Ordering::AcqRel) {
            return;
        }
        self.spawn_output_pruner();
        let untrusted = self.inner.untrusted_map();
        for cell in self.inner.shared_cells() {
            if cell.server.startup != Startup::Lazy && !untrusted.contains_key(&cell.server.name) {
                self.spawn_cell(&cell, cell.stop.clone(), None);
            }
        }
    }

    /// Prunes `<state>/mcp-output/` now and every [`OUTPUT_PRUNE_EVERY`] until shutdown (#2309).
    fn spawn_output_pruner(&self) {
        let Some(retention) = self.inner.opts.output_retention else {
            return;
        };
        let root = self.inner.opts.state_dir.join("mcp-output");
        let cancel = self.inner.cancel.clone();
        let task = tokio::spawn(async move {
            loop {
                let dir = root.clone();
                let pruned = tokio::task::spawn_blocking(move || {
                    prune_output(&dir, retention, std::time::SystemTime::now())
                })
                .await;
                if let Ok(p) = pruned
                    && p.removed > 0
                {
                    tracing::debug!(removed = p.removed, kept = p.kept, "pruned mcp-output");
                }
                tokio::select! {
                    _ = cancel.cancelled() => return,
                    _ = tokio::time::sleep(OUTPUT_PRUNE_EVERY) => {}
                }
            }
        });
        push_task(&self.inner.tasks, task);
    }

    fn spawn_cell(
        &self,
        cell: &Arc<Cell>,
        cancel: CancellationToken,
        into: Option<&Mutex<Vec<JoinHandle<()>>>>,
    ) {
        if cell.task_running.swap(true, Ordering::AcqRel) {
            return;
        }
        {
            let mut st = cell.lock();
            st.status = ServerStatus::Starting;
            st.first_attempt = Some(Instant::now());
        }
        let (hub, owned) = (Arc::downgrade(&self.inner), Arc::clone(cell));
        let task = tokio::spawn(async move {
            // Cleared however the task ends (return, cancel, panic or abort), so a later restart
            // spawns a new task instead of waking one that is gone (#2311).
            let _running = TaskRunning(Arc::downgrade(&owned));
            run_cell(hub, owned, cancel).await;
        });
        push_task(into.unwrap_or(&self.inner.tasks), task);
        self.inner.bump();
    }

    /// Waits until the first connection attempt of every eager server (and, with `background`,
    /// every background one) has finished, each bounded by its `startup_timeout_secs`. Returns the
    /// names still starting when the wait ended (a warning for the caller to show).
    pub async fn wait_started(&self, background: bool) -> Vec<String> {
        let cells: Vec<Arc<Cell>> = self
            .inner
            .shared_cells()
            .into_iter()
            .filter(|c| {
                c.server.startup == Startup::Eager
                    || (background && c.server.startup == Startup::Background)
            })
            .collect();
        wait_first_attempts(&self.inner, &cells).await
    }

    /// Restarts a shared server now: a lazy one starts, a stopped or needs-login one tries again
    /// with its failure count reset, a connected one reconnects.
    pub fn restart(&self, name: &str) -> Result<(), String> {
        let cell = self
            .inner
            .shared_cell(name)
            .ok_or_else(|| self.unknown(name))?;
        if let Some(u) = self.inner.untrusted_map().get(name) {
            return Err(format!(
                "MCP server `{name}` is not trusted: {}",
                u.reason()
            ));
        }
        self.wake_or_spawn(&cell);
        Ok(())
    }

    /// Wakes a shared cell's task (a restart), or starts it when it has none.
    fn wake_or_spawn(&self, cell: &Arc<Cell>) {
        if cell.task_running.load(Ordering::Acquire) {
            cell.wake.notify_one();
        } else {
            self.spawn_cell(cell, cell.stop.clone(), None);
        }
    }

    fn unknown(&self, name: &str) -> String {
        let config = self.inner.config();
        match config.servers.get(name) {
            Some(s) if !s.enabled => format!("MCP server `{name}` is disabled"),
            Some(_) => format!("MCP server `{name}` runs one instance per worker worktree"),
            None if config.broken.contains(name) => {
                format!("MCP server `{name}` has configuration errors (amp-harness config check)")
            }
            None => format!("no MCP server named `{name}`"),
        }
    }

    /// Every server: shared ones, running worktree instances, worktree-scoped servers with no
    /// instance (shown idle), disabled ones and broken ones (stopped, with the problem).
    /// A connected shared server's client and the tools it listed, for the CLI (`amp-harness mcp
    /// test`, `mcp call --server`, `mcp tools --server`, #2243). Sessions never use this: their
    /// tools go through the cell, so a reconnect is invisible to them.
    pub fn connection(&self, name: &str) -> Result<(Arc<ExternalClient>, Vec<McpToolDef>), String> {
        self.inner
            .shared_cell(name)
            .ok_or_else(|| format!("MCP server `{name}` is not running in this hub"))?
            .connection()
    }

    pub fn health(&self) -> Vec<ServerHealth> {
        let config = self.inner.config();
        let mut out: Vec<ServerHealth> = self
            .inner
            .shared_cells()
            .iter()
            .map(|c| c.health())
            .collect();
        let worktrees = self
            .inner
            .worktrees
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .cloned()
            .collect::<Vec<_>>();
        for set in &worktrees {
            out.extend(set.health());
        }
        for server in config.servers.values() {
            let listed = out.iter().any(|h| h.name == server.name);
            if listed {
                continue;
            }
            let untrusted = self.inner.untrusted_map();
            let status = match untrusted.get(&server.name) {
                Some(u) if server.enabled => ServerStatus::Untrusted { reason: u.reason() },
                _ if server.enabled => ServerStatus::Idle,
                _ => ServerStatus::Disabled,
            };
            out.push(idle_health(server, status));
        }
        for name in &config.broken {
            let reason = config
                .errors()
                .find(|p| {
                    p.key == format!("mcp.{name}") || p.key.starts_with(&format!("mcp.{name}."))
                })
                .map(|p| p.to_string())
                .unwrap_or_else(|| "configuration error".to_owned());
            out.push(ServerHealth {
                name: name.clone(),
                transport: "unknown",
                scope: "shared",
                instance: None,
                status: ServerStatus::Stopped { reason },
                tools: 0,
                server: None,
                protocol: None,
                pid: None,
                stderr_log: None,
                restarts: 0,
            });
        }
        out.sort_by(|a, b| (&a.name, &a.instance).cmp(&(&b.name, &b.instance)));
        out
    }

    /// `mcp ready/total` over the shared servers that should run (not idle or disabled), with the
    /// worst level among them.
    pub fn summary(&self) -> HubSummary {
        let mut s = HubSummary::default();
        let mut worst = HealthLevel::Ok;
        for cell in self.inner.shared_cells() {
            let st = cell.lock();
            if matches!(
                st.status,
                ServerStatus::Idle | ServerStatus::Disabled | ServerStatus::Untrusted { .. }
            ) {
                continue;
            }
            s.total += 1;
            if st.status == ServerStatus::Ready {
                s.ready += 1;
            }
            worst = worse(worst, st.status.level());
        }
        s.level = worst.into();
        s
    }

    /// One [`HubEvent`] per change from now on.
    pub fn subscribe(&self) -> broadcast::Receiver<HubEvent> {
        self.inner.events.subscribe()
    }

    /// Bumped on every change.
    pub fn watch(&self) -> watch::Receiver<u64> {
        self.inner.rev.subscribe()
    }

    /// Starts this worker's instances of every `scope = "worktree"` server in `worktree` (`key`:
    /// the ticket id) and waits for their first attempts, each bounded by its startup timeout.
    /// `None` when no such server is configured. The supervisor releases them with
    /// [`Self::release_worktree`].
    pub async fn start_worktree(&self, key: &str, worktree: &Path) -> Option<Arc<WorktreeSet>> {
        // #2241: an untrusted repo server gets no instance (workers never ask).
        let untrusted = self.inner.untrusted_map();
        let servers: Vec<McpServer> = self
            .inner
            .config()
            .enabled()
            .filter(|s| s.scope == Scope::Worktree && !untrusted.contains_key(&s.name))
            .cloned()
            .collect();
        if servers.is_empty() {
            return None;
        }
        let set = Arc::new(WorktreeSet {
            key: key.to_owned(),
            cells: servers
                .into_iter()
                .map(|s| {
                    let name = s.name.clone();
                    (
                        name,
                        Arc::new(Cell::new(
                            s,
                            Some(key.to_owned()),
                            Some(worktree.to_path_buf()),
                        )),
                    )
                })
                .collect(),
            cancel: self.inner.cancel.child_token(),
            tasks: Mutex::new(Vec::new()),
        });
        let previous = self
            .inner
            .worktrees
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(key.to_owned(), Arc::clone(&set));
        if let Some(old) = previous {
            stop_set(&old).await;
        }
        for cell in set.cells.values() {
            self.spawn_cell(cell, set.cancel.clone(), Some(&set.tasks));
        }
        let cells: Vec<Arc<Cell>> = set.cells.values().cloned().collect();
        wait_first_attempts(&self.inner, &cells).await;
        Some(set)
    }

    /// Shuts down the worktree instances started for `key` (its worker finished).
    pub async fn release_worktree(&self, key: &str) {
        let set = self
            .inner
            .worktrees
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .remove(key);
        if let Some(set) = set {
            stop_set(&set).await;
            self.inner.bump();
        }
    }

    /// What a session of `role` gets now: the tools of every server that allows the role (shared
    /// ones, plus `worktree`'s instances), named `mcp__<server>__<tool>`, and their instructions.
    /// `output_key` names the session's spill directory (`<state>/mcp-output/<key>/`).
    pub fn session_view(
        &self,
        role: &str,
        worktree: Option<&Arc<WorktreeSet>>,
        output_key: &str,
    ) -> SessionView {
        session_view(&self.inner, role, worktree, output_key)
    }

    /// Keeps `harness` in step with the hub (tools and instructions) from now on. `applied` is the
    /// signature of the view the session was built with. A change that happened in between is
    /// applied at once.
    pub async fn attach(
        &self,
        harness: &Arc<AgentHarness>,
        role: &str,
        policy: Arc<ToolPolicy>,
        output_key: String,
        worktree: Option<Arc<WorktreeSet>>,
        applied: String,
    ) {
        let attached = Arc::new(Attached {
            harness: Arc::downgrade(harness),
            role: role.to_owned(),
            policy,
            output_key,
            worktree,
            applied: Mutex::new(applied),
        });
        {
            let mut list = self
                .inner
                .attached
                .lock()
                .unwrap_or_else(|e| e.into_inner());
            list.retain(|a| a.harness.strong_count() > 0);
            list.push(attached);
        }
        reconcile(&self.inner).await;
    }

    /// Brings every attached session up to date now (normally the cell tasks do this).
    pub async fn reconcile(&self) {
        reconcile(&self.inner).await;
    }

    /// Puts `config` in place without restarting what didn't change (#2242, `/reload` after
    /// `[mcp.*]` was edited in another terminal):
    /// - a shared server whose table is unchanged keeps its connection (and its process); one
    ///   that was stopped or needed a login is tried again;
    /// - a new or changed one gets a fresh cell, started now (unless lazy) when the hub runs;
    /// - a removed, disabled or changed one's old cell is *retired*: it keeps serving the sessions
    ///   that still hold its tools (a running worker keeps its snapshot) and stops once none
    ///   does.
    ///
    /// Attached sessions get the new tools and instructions from their next run. Worktree
    /// instances already running keep theirs; workers built from now on use the new
    /// configuration.
    /// Files outside the configuration whose change should reload it (#2311): the secret
    /// versions file and every configured stdio server's env_file. The chat's watcher looks at
    /// them with the config files.
    pub fn watched_files(&self) -> Vec<PathBuf> {
        let config = self.inner.config();
        let env = |k: &str| std::env::var(k).ok();
        let ctx = ExpandContext::from_paths(&self.inner.opts.paths, &env, None);
        let mut files = vec![crate::mcp_stamp::versions_path(&self.inner.opts.state_dir)];
        files.extend(
            config
                .servers
                .values()
                .filter_map(|s| crate::mcp_stamp::env_file_path(s, &ctx)),
        );
        files.sort();
        files.dedup();
        files
    }

    pub async fn reload(&self, config: McpConfig) -> McpReload {
        let mut report = McpReload::diff(&self.inner.config(), &config);
        // #2241: trust as the store has it now (`mcp trust` in another terminal counts too).
        let untrusted = untrusted_in(&config, &self.inner.opts);
        let was_untrusted = self.inner.untrusted_map();
        let mut start = Vec::new();
        let mut retry = Vec::new();
        let mut retire = Vec::new();
        let stamps = Stamper::new(&self.inner.opts);
        {
            let mut shared = self.inner.shared.lock().unwrap_or_else(|e| e.into_inner());
            let wanted: BTreeMap<String, McpServer> = shared_servers(&config)
                .map(|s| (s.name.clone(), s.clone()))
                .collect();
            let gone: Vec<String> = shared
                .keys()
                .filter(|n| !wanted.contains_key(*n))
                .cloned()
                .collect();
            for name in gone {
                retire.extend(shared.remove(&name));
            }
            for (name, server) in wanted {
                let trust_same = untrusted.contains_key(&name) == was_untrusted.contains_key(&name);
                let stamp = stamps.of(&server);
                let same = shared
                    .get(&name)
                    .map(|c| c.server == server && c.stamp == stamp && trust_same);
                if let Some(c) = shared.get(&name)
                    && c.server == server
                    && c.stamp != stamp
                    && !report.changed.contains(&name)
                {
                    // #2311: `mcp secret` rotated a value, or the env_file was edited.
                    report.changed.push(name.clone());
                }
                if same == Some(true) {
                    let cell = Arc::clone(&shared[&name]);
                    let status = cell.lock().status.clone();
                    if matches!(
                        status,
                        ServerStatus::Stopped { .. } | ServerStatus::NeedsLogin { .. }
                    ) {
                        retry.push(cell);
                    }
                    continue;
                }
                let cell = Cell::shared(server, &self.inner.cancel, stamp);
                if let Some(u) = untrusted.get(&name) {
                    cell.lock().status = ServerStatus::Untrusted { reason: u.reason() };
                }
                let cell = Arc::new(cell);
                retire.extend(shared.insert(name, Arc::clone(&cell)));
                start.push(cell);
            }
        }
        *self.inner.config.lock().unwrap_or_else(|e| e.into_inner()) = Arc::new(config);
        // Named only when that is news: new to the config, changed, or no longer trusted.
        report.untrusted = untrusted
            .keys()
            .filter(|n| {
                report.added.contains(n)
                    || report.changed.contains(n)
                    || !was_untrusted.contains_key(*n)
            })
            .cloned()
            .collect();
        *self
            .inner
            .untrusted
            .lock()
            .unwrap_or_else(|e| e.into_inner()) = untrusted.clone();
        if self.inner.started.load(Ordering::Acquire) {
            for cell in &start {
                if cell.server.startup != Startup::Lazy
                    && !untrusted.contains_key(&cell.server.name)
                {
                    self.spawn_cell(cell, cell.stop.clone(), None);
                }
            }
            for cell in &retry {
                report.retried.push(cell.server.name.clone());
                self.wake_or_spawn(cell);
            }
        }
        if !retire.is_empty() {
            self.inner
                .retired
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .extend(retire);
            self.spawn_sweeper();
        }
        self.inner.bump();
        reconcile(&self.inner).await;
        report
    }

    /// Stops retired cells as soon as no session holds their tools (checked every
    /// [`HubOptions::poll`]); ends when none is left or the hub shuts down.
    fn spawn_sweeper(&self) {
        if self.inner.sweeping.swap(true, Ordering::AcqRel) {
            return;
        }
        let hub = Arc::downgrade(&self.inner);
        let cancel = self.inner.cancel.clone();
        let poll = self.inner.opts.poll;
        tokio::spawn(async move {
            loop {
                let Some(inner) = hub.upgrade() else {
                    return;
                };
                if sweep_retired(&inner) {
                    inner.sweeping.store(false, Ordering::Release);
                    // A reload may have retired another cell after the sweep and before the flag
                    // went down; then this task carries on.
                    let empty = inner
                        .retired
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .is_empty();
                    if empty || inner.sweeping.swap(true, Ordering::AcqRel) {
                        return;
                    }
                }
                drop(inner);
                tokio::select! {
                    _ = cancel.cancelled() => return,
                    _ = tokio::time::sleep(poll) => {}
                }
            }
        });
    }

    /// Applies the hub's view to `harness` again even when it looks unchanged: its policy's rules
    /// were replaced (#2242), so tools it filtered out before may be allowed now, or the reverse.
    pub async fn reapply(&self, harness: &Arc<AgentHarness>) {
        {
            let list = self
                .inner
                .attached
                .lock()
                .unwrap_or_else(|e| e.into_inner());
            for a in list.iter() {
                if a.harness
                    .upgrade()
                    .is_some_and(|h| Arc::ptr_eq(&h, harness))
                {
                    a.applied
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .push_str("\n(reapply)");
                }
            }
        }
        reconcile(&self.inner).await;
    }

    /// Shared servers a reload retired that still run (some session holds their tools).
    pub fn retired(&self) -> Vec<String> {
        self.inner
            .retired
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .map(|c| c.server.name.clone())
            .collect()
    }

    /// Replaces `harness`'s system prompt with `base` (a prompt [`crate::agent::Runtime`] built,
    /// without MCP blocks) plus the `<mcp_server_instructions>` blocks the session has now
    /// (#2242: `/reload` rebuilt the role's prompt). Serialized with the hub's own updates, so a
    /// server connecting meanwhile can't lose its block. Returns the prompt set.
    pub async fn set_session_prompt(
        &self,
        harness: &AgentHarness,
        base: &str,
    ) -> Result<String, String> {
        let _serial = self.inner.apply.lock().await;
        let current = harness
            .get_system_prompt()
            .await
            .map_err(|e| e.to_string())?
            .unwrap_or_default();
        let prompt = with_mcp_instructions(base, &mcp_instruction_blocks(&current));
        harness
            .set_system_prompt(Some(prompt.clone()))
            .await
            .map_err(|e| e.to_string())?;
        Ok(prompt)
    }

    /// Closes every server (shared and worktree instances) gracefully, bounded by
    /// [`amp_mcp::external::SHUTDOWN_GRACE`] plus a second. Call it before the process exits.
    pub async fn shutdown(&self) {
        self.inner.cancel.cancel();
        let sets: Vec<Arc<WorktreeSet>> = std::mem::take(
            &mut *self
                .inner
                .worktrees
                .lock()
                .unwrap_or_else(|e| e.into_inner()),
        )
        .into_values()
        .collect();
        let mut tasks: Vec<JoinHandle<()>> =
            std::mem::take(&mut *self.inner.tasks.lock().unwrap_or_else(|e| e.into_inner()));
        for set in &sets {
            tasks.extend(std::mem::take(
                &mut *set.tasks.lock().unwrap_or_else(|e| e.into_inner()),
            ));
        }
        join_bounded(tasks).await;
    }
}

fn idle_health(server: &McpServer, status: ServerStatus) -> ServerHealth {
    ServerHealth {
        name: server.name.clone(),
        transport: server.transport.kind().as_str(),
        scope: scope_word(server.scope),
        instance: None,
        status,
        tools: 0,
        server: None,
        protocol: None,
        pid: None,
        stderr_log: None,
        restarts: 0,
    }
}

fn worse(a: HealthLevel, b: HealthLevel) -> HealthLevel {
    let rank = |l: HealthLevel| match l {
        HealthLevel::Ok => 0,
        HealthLevel::Degraded => 1,
        HealthLevel::Down => 2,
    };
    if rank(b) > rank(a) { b } else { a }
}

/// Takes [`crate::mcp_stamp::server_stamp`]s with one read of the versions file.
struct Stamper<'a> {
    opts: &'a HubOptions,
    versions: BTreeMap<String, String>,
}

impl<'a> Stamper<'a> {
    fn new(opts: &'a HubOptions) -> Self {
        Self {
            opts,
            versions: crate::mcp_stamp::load(&opts.state_dir),
        }
    }

    fn of(&self, server: &McpServer) -> String {
        let env = |k: &str| std::env::var(k).ok();
        let ctx = ExpandContext::from_paths(&self.opts.paths, &env, None);
        crate::mcp_stamp::server_stamp(server, &self.versions, &ctx)
    }
}

/// Adds a task handle, dropping the handles of tasks that have finished, so the list holds only
/// live tasks however often servers restart or reload (#2311).
fn push_task(list: &Mutex<Vec<JoinHandle<()>>>, task: JoinHandle<()>) {
    let mut list = list.lock().unwrap_or_else(|e| e.into_inner());
    list.retain(|t| !t.is_finished());
    list.push(task);
}

/// Resets [`Cell::task_running`] when a cell's task ends. Weak, so the task still holds exactly
/// one strong reference, which [`sweep_retired`] counts.
struct TaskRunning(Weak<Cell>);

impl Drop for TaskRunning {
    fn drop(&mut self) {
        if let Some(cell) = self.0.upgrade() {
            cell.task_running.store(false, Ordering::Release);
        }
    }
}

async fn stop_set(set: &WorktreeSet) {
    set.cancel.cancel();
    let tasks = std::mem::take(&mut *set.tasks.lock().unwrap_or_else(|e| e.into_inner()));
    join_bounded(tasks).await;
}

/// Stops each retired cell no session holds any more (only the retired list and its own task
/// have it). Returns whether none is left.
fn sweep_retired(inner: &Inner) -> bool {
    let mut list = inner.retired.lock().unwrap_or_else(|e| e.into_inner());
    list.retain(|cell| {
        let task = usize::from(cell.task_running.load(Ordering::Acquire));
        let held = Arc::strong_count(cell) > 1 + task;
        if !held {
            cell.lock().status = ServerStatus::Stopped {
                reason: "removed or replaced by a reload".to_owned(),
            };
            cell.stop.cancel();
        }
        held
    });
    list.is_empty()
}

async fn join_bounded(tasks: Vec<JoinHandle<()>>) {
    let bound = amp_mcp::external::SHUTDOWN_GRACE + Duration::from_secs(2);
    let all = futures_join_all(tasks);
    let _ = tokio::time::timeout(bound, all).await;
}

async fn futures_join_all(tasks: Vec<JoinHandle<()>>) {
    for t in tasks {
        let _ = t.await;
    }
}

async fn wait_first_attempts(inner: &Arc<Inner>, cells: &[Arc<Cell>]) -> Vec<String> {
    let mut rx = inner.rev.subscribe();
    loop {
        let now = Instant::now();
        let mut pending = Vec::new();
        let mut next_deadline: Option<Instant> = None;
        for cell in cells {
            let st = cell.lock();
            // Not started (or a lazy one), or its first attempt is over.
            let Some(started) = st.first_attempt else {
                continue;
            };
            if st.attempts > 0 || st.status != ServerStatus::Starting {
                continue;
            }
            // The connect is bounded by the startup timeout; the grace covers listing tools.
            let deadline = started + cell.server.startup_timeout + Duration::from_secs(2);
            if deadline > now {
                pending.push(cell.server.name.clone());
                next_deadline = Some(next_deadline.map_or(deadline, |d| d.min(deadline)));
            } else {
                pending.push(cell.server.name.clone());
            }
        }
        let Some(deadline) = next_deadline else {
            return pending;
        };
        tokio::select! {
            r = rx.changed() => if r.is_err() { return pending; },
            _ = tokio::time::sleep_until(deadline.into()) => {}
        }
    }
}

/// The enabled repo servers `opts` does not let start (#2241). An unreadable trust store trusts
/// nothing (logged).
fn untrusted_in(
    config: &McpConfig,
    opts: &HubOptions,
) -> BTreeMap<String, crate::mcp_trust::Untrusted> {
    use crate::mcp_trust::{TrustStore, untrusted};
    if !config.enabled().any(McpServer::is_repo) {
        return BTreeMap::new();
    }
    let store = match opts.trust_store.as_deref().map(TrustStore::load) {
        Some(Ok(s)) => Some(s),
        Some(Err(e)) => {
            tracing::warn!("MCP trust store: {e}");
            None
        }
        None => None,
    };
    untrusted(config, store.as_ref(), opts.trust_all)
}

impl Inner {
    fn config(&self) -> Arc<McpConfig> {
        Arc::clone(&self.config.lock().unwrap_or_else(|e| e.into_inner()))
    }

    fn shared_cells(&self) -> Vec<Arc<Cell>> {
        self.shared
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .cloned()
            .collect()
    }

    fn shared_cell(&self, name: &str) -> Option<Arc<Cell>> {
        self.shared
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(name)
            .cloned()
    }

    fn shared_names(&self) -> Vec<String> {
        self.shared
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .keys()
            .cloned()
            .collect()
    }

    fn untrusted_map(&self) -> BTreeMap<String, crate::mcp_trust::Untrusted> {
        self.untrusted
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    fn bump(&self) {
        self.rev.send_modify(|r| *r += 1);
    }

    fn emit(&self, event: HubEvent) {
        let _ = self.events.send(event);
    }

    fn max_output_tokens(&self, server: &McpServer) -> u64 {
        if server.max_output_tokens == DEFAULT_MAX_OUTPUT_TOKENS {
            self.opts
                .default_max_output_tokens
                .unwrap_or(DEFAULT_MAX_OUTPUT_TOKENS)
        } else {
            server.max_output_tokens
        }
    }
}

// ---------------------------------------------------------------------------------------------
// One server's task

async fn run_cell(hub: Weak<Inner>, cell: Arc<Cell>, cancel: CancellationToken) {
    let changed = |event: Option<HubEvent>| {
        let hub = hub.clone();
        async move {
            if let Some(inner) = hub.upgrade() {
                inner.bump();
                if let Some(e) = event {
                    inner.emit(e);
                }
                reconcile(&inner).await;
            }
        }
    };
    let (server, instance) = (cell.server.name.clone(), cell.instance.clone());
    loop {
        {
            let mut st = cell.lock();
            st.status = ServerStatus::Starting;
        }
        changed(None).await;
        let Some(ctx) = hub.upgrade().map(|h| ConnectCtx::from(&*h)) else {
            return;
        };
        let bound = cell.server.startup_timeout + Duration::from_secs(2);
        let attempt = tokio::select! {
            biased;
            _ = cancel.cancelled() => return,
            r = tokio::time::timeout(bound, connect_cell(&cell, &ctx)) => r.unwrap_or_else(|_| {
                Err(Fail::Transient(format!("no answer within {:?}", cell.server.startup_timeout)))
            }),
        };
        drop(ctx);
        let fail = match attempt {
            Ok((client, tools)) => {
                let client = Arc::new(client);
                let n = tools.len();
                {
                    let mut st = cell.lock();
                    st.attempts += 1;
                    st.status = ServerStatus::Ready;
                    st.info = Some(client.info().clone());
                    st.pid = client.pid();
                    st.stderr_log = client.stderr_log().map(Path::to_path_buf);
                    st.tools = tools;
                    st.client = Some(Arc::clone(&client));
                }
                changed(Some(HubEvent::Ready {
                    server: server.clone(),
                    instance: instance.clone(),
                    tools: n,
                }))
                .await;
                let poll = hub
                    .upgrade()
                    .map(|h| h.opts.poll)
                    .unwrap_or(Duration::from_millis(500));
                let why = loop {
                    tokio::select! {
                        biased;
                        _ = cancel.cancelled() => {
                            if let Some(c) = cell.take_client() {
                                close(c).await;
                            }
                            return;
                        }
                        _ = cell.wake.notified() => break None,
                        _ = tokio::time::sleep(poll) => {
                            if client.is_closed() {
                                break Some("the connection closed (the server exited or the transport failed)".to_owned());
                            }
                            if client.take_tools_changed() {
                                match client.list_tools().await {
                                    Ok(tools) => {
                                        let n = tools.len();
                                        cell.lock().tools = tools;
                                        changed(Some(HubEvent::ToolsChanged {
                                            server: server.clone(),
                                            instance: instance.clone(),
                                            tools: n,
                                        }))
                                        .await;
                                    }
                                    Err(e) if client.is_closed() => break Some(e.to_string()),
                                    Err(e) => tracing::warn!(server = %server, "tools/list after list_changed: {e}"),
                                }
                            }
                        }
                    }
                };
                drop(client);
                if let Some(c) = cell.take_client() {
                    close(c).await;
                }
                match why {
                    // A restart request: reconnect at once, failures forgiven.
                    None => {
                        let mut st = cell.lock();
                        st.failures.clear();
                        st.restarts += 1;
                        continue;
                    }
                    Some(reason) => Fail::Transient(reason),
                }
            }
            Err(f) => {
                cell.lock().attempts += 1;
                f
            }
        };

        let (status, retry) = match fail {
            Fail::Auth(reason) => (ServerStatus::NeedsLogin { reason }, None),
            Fail::Config(reason) => (ServerStatus::Stopped { reason }, None),
            Fail::Transient(reason) => {
                let backoff = hub.upgrade().map(|h| h.opts.backoff).unwrap_or_default();
                let mut st = cell.lock();
                let now = Instant::now();
                st.failures.push_back(now);
                while st
                    .failures
                    .front()
                    .is_some_and(|t| now.duration_since(*t) > backoff.window)
                {
                    st.failures.pop_front();
                }
                let n = st.failures.len();
                if n >= backoff.give_up_after {
                    (
                        ServerStatus::Stopped {
                            reason: format!(
                                "{reason}; gave up after {n} failures in {} min (restart it with `/mcp restart {server}`)",
                                backoff.window.as_secs().div_ceil(60)
                            ),
                        },
                        None,
                    )
                } else {
                    let delay = backoff.delay(n);
                    (
                        ServerStatus::Failed {
                            reason,
                            retry_in_ms: delay.as_millis() as u64,
                        },
                        Some(delay),
                    )
                }
            }
        };
        {
            let mut st = cell.lock();
            if !status.keeps_tools() {
                st.tools.clear();
            }
            st.status = status.clone();
        }
        tracing::warn!(server = %server, "MCP server: {status}");
        changed(Some(HubEvent::Failed {
            server: server.clone(),
            instance: instance.clone(),
            status,
        }))
        .await;
        match retry {
            Some(delay) => {
                tokio::select! {
                    biased;
                    _ = cancel.cancelled() => return,
                    _ = cell.wake.notified() => cell.lock().failures.clear(),
                    _ = tokio::time::sleep(delay) => {}
                }
            }
            None => {
                tokio::select! {
                    biased;
                    _ = cancel.cancelled() => return,
                    _ = cell.wake.notified() => cell.lock().failures.clear(),
                }
            }
        }
        cell.lock().restarts += 1;
    }
}

/// What a connect attempt needs, copied out of the hub so the attempt holds no lock.
struct ConnectCtx {
    paths: ConfigPaths,
    state_dir: PathBuf,
    default_cwd: PathBuf,
    keychain: Option<String>,
    strip: BTreeSet<String>,
    protected: BTreeSet<String>,
}

impl From<&Inner> for ConnectCtx {
    fn from(h: &Inner) -> Self {
        Self {
            paths: h.opts.paths.clone(),
            state_dir: h.opts.state_dir.clone(),
            default_cwd: h.opts.default_cwd.clone(),
            keychain: h.opts.keychain.clone(),
            strip: h.strip.lock().unwrap_or_else(|e| e.into_inner()).clone(),
            protected: h
                .protected
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .clone(),
        }
    }
}

/// A server resolved for one attempt: expanded, secrets read, ready to connect.
enum Prepared {
    Stdio(amp_mcp::StdioSpec),
    /// Streamable HTTP or legacy SSE (`HttpServerOptions::legacy_sse`).
    Remote(amp_mcp::HttpServerOptions),
}

/// The environment a server's config may read (#2308): everything for a user server; for a repo
/// server only the variables it names ([`McpServer::env_reads`], approved when it was trusted)
/// plus `HOME` (for `${home}`), never one in `protected`.
fn config_env(
    server: &McpServer,
    protected: &BTreeSet<String>,
) -> impl Fn(&str) -> Option<String> + use<> {
    let reads = server.env_reads();
    let repo = server.is_repo();
    let protected = protected.clone();
    move |k: &str| {
        if repo
            && (protected.contains(k)
                || k == amp_mcp::ENV_TOKEN
                || (k != "HOME" && !reads.contains(k)))
        {
            return None;
        }
        std::env::var(k).ok()
    }
}

/// The values of the variables `server` reads, to scrub from its error messages: a URL with
/// `${env:KEY}` in its path or query must not show up in a status line, the transcript or
/// `run -p`'s stderr (#2308).
fn env_values(server: &McpServer, env: &dyn Fn(&str) -> Option<String>) -> Vec<String> {
    server
        .env_reads()
        .iter()
        .filter_map(|k| env(k))
        .filter(|v| !v.trim().is_empty())
        .collect()
}

impl Fail {
    /// The same failure with URLs redacted and `values` replaced (#2308).
    fn scrubbed(self, values: &[String]) -> Self {
        let clean = |m: String| amp_mcp::scrub_values(&amp_mcp::redact_urls(&m), values);
        match self {
            Self::Transient(m) => Self::Transient(clean(m)),
            Self::Auth(m) => Self::Auth(clean(m)),
            Self::Config(m) => Self::Config(clean(m)),
        }
    }
}

/// Expands `${...}`, reads env files and secrets (synchronously: nothing here is held across an
/// await).
fn prepare(cell: &Cell, ctx: &ConnectCtx) -> Result<Prepared, Fail> {
    // #2308: a repo server never reads the person's own credentials, and only the variables it
    // was trusted with.
    crate::mcp_trust::check_repo_env(&cell.server, &ctx.protected).map_err(Fail::Config)?;
    let env = config_env(&cell.server, &ctx.protected);
    let expand = ExpandContext::from_paths(&ctx.paths, &env, cell.worktree.as_deref());
    let resolved = cell
        .server
        .resolve(&expand)
        .map_err(|p| Fail::Config(p.to_string()))?;
    let remote = |r| {
        mcp_http::http_options(&cell.server, r, ctx.keychain.as_deref(), &env).map_err(Fail::Config)
    };
    Ok(match resolved {
        Resolved::Stdio(r) => {
            let service = ctx.keychain.clone().unwrap_or_default();
            let env_ref = &env;
            let fetch = move |s: &SecretRef| s.fetch(&service, env_ref);
            let default_cwd = cell.worktree.clone().unwrap_or(ctx.default_cwd.clone());
            let mut sctx = mcp_stdio::StdioContext::from_process(
                &ctx.strip,
                &fetch,
                &ctx.state_dir,
                &default_cwd,
            );
            sctx.log_name = Some(&cell.log_name);
            Prepared::Stdio(
                mcp_stdio::stdio_spec(&cell.server, r, &sctx)
                    .map_err(|p| Fail::Config(p.to_string()))?,
            )
        }
        Resolved::Http(r) | Resolved::Sse(r) => Prepared::Remote(remote(&r)?),
    })
}

/// Connects with the server's transport, then lists its tools.
async fn connect_cell(
    cell: &Cell,
    ctx: &ConnectCtx,
) -> Result<(ExternalClient, Vec<McpToolDef>), Fail> {
    let values = env_values(&cell.server, &config_env(&cell.server, &ctx.protected));
    connect_cell_inner(cell, ctx)
        .await
        .map_err(|f| f.scrubbed(&values))
}

async fn connect_cell_inner(
    cell: &Cell,
    ctx: &ConnectCtx,
) -> Result<(ExternalClient, Vec<McpToolDef>), Fail> {
    let client = match prepare(cell, ctx)? {
        Prepared::Stdio(spec) => ExternalClient::connect_stdio(spec).await,
        // `type = "sse"` has `legacy_sse` set by `http_options`; `http::connect` speaks legacy
        // HTTP+SSE to it, and falls back to it for `type = "http"` (#2238).
        Prepared::Remote(options) => {
            let backend = CredentialBackend::resolve().map_err(classify)?;
            http::connect(&options, &backend).await
        }
    }
    .map_err(classify)?;
    let tools = client.list_tools().await.map_err(classify)?;
    Ok((client, tools))
}

// ---------------------------------------------------------------------------------------------
// Sessions

fn session_view(
    inner: &Inner,
    role: &str,
    worktree: Option<&Arc<WorktreeSet>>,
    output_key: &str,
) -> SessionView {
    let mut cells: Vec<Arc<Cell>> = inner.shared_cells();
    if let Some(set) = worktree {
        cells.extend(set.cells.values().cloned());
    }
    cells.sort_by(|a, b| a.server.name.cmp(&b.server.name));
    let spill_dir = inner
        .opts
        .state_dir
        .join("mcp-output")
        .join(file_component(output_key));
    let mut names = ToolNames::new();
    let mut tools = Vec::new();
    let mut subjects = Vec::new();
    let mut instructions = Vec::new();
    let mut signature = String::new();
    for cell in cells {
        if !cell.server.allows_role(role) {
            continue;
        }
        let (defs, text) = {
            let st = cell.lock();
            if !st.status.keeps_tools() {
                continue;
            }
            let text = st
                .info
                .as_ref()
                .and_then(|i| i.instructions.clone())
                .filter(|t| cell.server.instructions && !t.trim().is_empty());
            (st.tools.clone(), text)
        };
        for def in &defs {
            let name = match names.insert(&cell.server.name, &def.name) {
                Ok(n) => n,
                Err(e) => {
                    tracing::warn!("MCP: {e}; tool skipped");
                    continue;
                }
            };
            signature.push_str(&name);
            signature.push('\n');
            subjects.push((name.clone(), format!("{}/{}", cell.server.name, def.name)));
            tools.push(
                ExternalTool::new(
                    def,
                    name,
                    Arc::clone(&cell),
                    inner.max_output_tokens(&cell.server),
                    spill_dir.clone(),
                )
                .into_harness_tool(),
            );
        }
        if let Some(text) = text {
            let block = instructions_block(&cell.server.name, &text);
            signature.push_str(&block);
            instructions.push(block);
        }
    }
    SessionView {
        tools,
        subjects,
        instructions,
        signature,
    }
}

async fn reconcile(inner: &Arc<Inner>) {
    let _serial = inner.apply.lock().await;
    let attached: Vec<Arc<Attached>> = {
        let mut list = inner.attached.lock().unwrap_or_else(|e| e.into_inner());
        list.retain(|a| a.harness.strong_count() > 0);
        list.clone()
    };
    for a in attached {
        let Some(harness) = a.harness.upgrade() else {
            continue;
        };
        let view = session_view(inner, &a.role, a.worktree.as_ref(), &a.output_key);
        if *a.applied.lock().unwrap_or_else(|e| e.into_inner()) == view.signature {
            continue;
        }
        let signature = view.signature.clone();
        if let Err(e) = apply_view(&harness, &a.policy, view).await {
            tracing::warn!("MCP: could not update a session: {e}");
            continue;
        }
        *a.applied.lock().unwrap_or_else(|e| e.into_inner()) = signature;
    }
}

/// Replaces `harness`'s `mcp__*` tools with the view's (after `policy`, which learns their `mcp`
/// subjects first) and its instruction blocks with the view's. Everything else is kept. Effective
/// from the next run.
async fn apply_view(
    harness: &AgentHarness,
    policy: &ToolPolicy,
    view: SessionView,
) -> Result<(), String> {
    policy.learn_mcp_subjects(view.subjects.iter().map(|(n, s)| (n.as_str(), s.as_str())));
    let instructions = &view.instructions;
    let current = harness.get_tools().await.map_err(|e| e.to_string())?;
    let mut next: Vec<HarnessTool> = current
        .into_iter()
        .filter(|t| !t.tool.schema().name.starts_with(TOOL_PREFIX))
        .collect();
    next.extend(crate::agent::filter_harness_tools(policy, view.tools));
    let names: Vec<String> = next.iter().map(|t| t.tool.schema().name.clone()).collect();
    harness
        .set_tools(next, Some(names))
        .await
        .map_err(|e| e.to_string())?;
    if let Some(prompt) = harness
        .get_system_prompt()
        .await
        .map_err(|e| e.to_string())?
    {
        let updated = with_mcp_instructions(&prompt, instructions);
        if updated != prompt {
            harness
                .set_system_prompt(Some(updated))
                .await
                .map_err(|e| e.to_string())?;
        }
    }
    Ok(())
}

/// `<mcp_server_instructions server="name">…</mcp_server_instructions>`, the text cut at
/// [`MAX_INSTRUCTIONS_BYTES`].
pub fn instructions_block(server: &str, text: &str) -> String {
    let text = text.trim();
    let text = if text.len() > MAX_INSTRUCTIONS_BYTES {
        let mut cut = MAX_INSTRUCTIONS_BYTES;
        while !text.is_char_boundary(cut) {
            cut -= 1;
        }
        format!("{}\n[instructions truncated at 4 KB]", &text[..cut])
    } else {
        text.to_owned()
    };
    format!("<{INSTRUCTIONS_TAG} server=\"{server}\">\n{text}\n</{INSTRUCTIONS_TAG}>")
}

/// `prompt` with its `<mcp_server_instructions>` blocks replaced by `blocks`: they go before
/// `<amp_context>` (so after AMP's own instructions), or at the end when there is none.
pub fn with_mcp_instructions(prompt: &str, blocks: &[String]) -> String {
    let open = format!("<{INSTRUCTIONS_TAG} ");
    let close = format!("</{INSTRUCTIONS_TAG}>");
    let mut out = prompt.to_owned();
    while let Some(start) = out.find(&open) {
        let Some(end) = out[start..].find(&close).map(|i| start + i + close.len()) else {
            break;
        };
        let end = if out[end..].starts_with("\n\n") {
            end + 2
        } else {
            end
        };
        out.replace_range(start..end, "");
    }
    if blocks.is_empty() {
        return out;
    }
    let joined = blocks.join("\n\n");
    match out.find(CONTEXT_OPEN) {
        Some(at) => out.insert_str(at, &format!("{joined}\n\n")),
        None => {
            if !out.is_empty() {
                out.push_str("\n\n");
            }
            out.push_str(&joined);
        }
    }
    out
}

/// The `<mcp_server_instructions>` blocks in `prompt`, in order.
pub fn mcp_instruction_blocks(prompt: &str) -> Vec<String> {
    let open = format!("<{INSTRUCTIONS_TAG} ");
    let close = format!("</{INSTRUCTIONS_TAG}>");
    let mut out = Vec::new();
    let mut rest = prompt;
    while let Some(start) = rest.find(&open) {
        let Some(end) = rest[start..].find(&close).map(|i| start + i + close.len()) else {
            break;
        };
        out.push(rest[start..end].to_owned());
        rest = &rest[end..];
    }
    out
}

// ---------------------------------------------------------------------------------------------
// The tool adapter

/// One tool of an external MCP server, as a native agent tool named `mcp__<server>__<tool>`.
pub struct ExternalTool {
    schema: Tool,
    label: String,
    tool: String,
    read_only: bool,
    cell: Arc<Cell>,
    max_output_tokens: u64,
    spill_dir: PathBuf,
}

impl std::fmt::Debug for ExternalTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ExternalTool")
            .field("name", &self.schema.name)
            .field("server", &self.cell.server.name)
            .finish_non_exhaustive()
    }
}

impl ExternalTool {
    fn new(
        def: &McpToolDef,
        name: String,
        cell: Arc<Cell>,
        max_output_tokens: u64,
        spill_dir: PathBuf,
    ) -> Self {
        let description = def
            .description
            .as_deref()
            .map(str::to_owned)
            .unwrap_or_else(|| def.name.to_string());
        let label = def
            .title
            .clone()
            .or_else(|| def.annotations.as_ref().and_then(|a| a.title.clone()))
            .unwrap_or_else(|| def.name.to_string());
        let read_only = def
            .annotations
            .as_ref()
            .and_then(|a| a.read_only_hint)
            .unwrap_or(false);
        Self {
            schema: Tool {
                name,
                description,
                parameters: Value::Object((*def.input_schema).clone()).into(),
                constrained_sampling: None,
            },
            label,
            tool: def.name.to_string(),
            read_only,
            cell,
            max_output_tokens,
            spill_dir,
        }
    }

    fn into_harness_tool(self) -> HarnessTool {
        let replay = if self.read_only {
            ToolReplay::Safe
        } else {
            ToolReplay::Never
        };
        HarnessTool::new(Arc::new(self)).with_replay(replay)
    }
}

fn arguments(params: Value) -> Result<Option<JsonObject>, AgentError> {
    match params {
        Value::Object(map) => Ok(Some(map)),
        Value::Null => Ok(None),
        other => Err(AgentError::Validation(format!(
            "arguments must be a JSON object, not {other}"
        ))),
    }
}

#[async_trait::async_trait]
impl AgentTool for ExternalTool {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        &self.label
    }

    async fn execute(
        &self,
        tool_call_id: &str,
        params: Value,
        signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, AgentError> {
        let args = arguments(params)?;
        let server = &self.cell.server.name;
        let client = self
            .cell
            .client()
            .map_err(|why| AgentError::Tool(format!("MCP server `{server}` {why}")))?;
        let call = client.call_tool(&self.tool, args);
        let result = tokio::select! {
            biased;
            _ = signal.cancelled() => return Err(AgentError::Abort),
            r = call => r,
        };
        drop(client);
        let result = result.map_err(|e| AgentError::Tool(format!("{}: {e}", self.schema.name)))?;
        let text = cap_output(
            &model_text(&result),
            self.max_output_tokens,
            &self.spill_dir,
            tool_call_id,
        );
        if result.is_error == Some(true) {
            let message = if text.trim().is_empty() {
                "the tool reported an error".to_owned()
            } else {
                text.trim().to_owned()
            };
            return Err(AgentError::Tool(message));
        }
        let mut out = AgentToolResult::text(text);
        if let Some(structured) = &result.structured_content {
            out.details = structured.clone();
        }
        Ok(out)
    }
}

/// `text` if it fits in `max_tokens` (at [`CHARS_PER_TOKEN`]); otherwise the full text goes to
/// `<dir>/<call id>.txt` and the result is its head plus a line saying where the rest is (D9).
pub fn cap_output(text: &str, max_tokens: u64, dir: &Path, call_id: &str) -> String {
    let limit = max_tokens.saturating_mul(CHARS_PER_TOKEN) as usize;
    let chars = text.chars().count();
    if chars <= limit {
        return text.to_owned();
    }
    let tokens = (chars as u64).div_ceil(CHARS_PER_TOKEN);
    let head: String = text.chars().take(limit).collect();
    let id = match file_component(call_id) {
        id if id.is_empty() => format!(
            "call-{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_or(0, |d| d.as_millis())
        ),
        id => id,
    };
    let path = dir.join(format!("{id}.txt"));
    let saved = write_private_output(dir, &path, text);
    match saved {
        Ok(()) => format!(
            "{head}\n\n[truncated: {tokens} tokens, over the {max_tokens}-token limit; full output \
             at {} (read it with read)]",
            path.display()
        ),
        Err(e) => format!(
            "{head}\n\n[truncated: {tokens} tokens, over the {max_tokens}-token limit; the full \
             output could not be saved: {e}]"
        ),
    }
}

/// Writes a spill file 0600 inside `dir` (0700, as is `<state>/mcp-output/` when it is created
/// here). Tool output can hold anything the server could read, so no other user may read it.
fn write_private_output(dir: &Path, path: &Path, text: &str) -> std::io::Result<()> {
    let mut builder = std::fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
        builder.mode(0o700);
        builder.create(dir)?;
        std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
    }
    #[cfg(not(unix))]
    builder.create(dir)?;
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options.open(path)?;
    #[cfg(unix)]
    {
        // An older file of the same name may have been created with the umask's mode.
        use std::os::unix::fs::PermissionsExt;
        file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
    }
    std::io::Write::write_all(&mut file, text.as_bytes())
}

/// What [`prune_output`] did.
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub struct PruneOutcome {
    /// Files deleted.
    pub removed: usize,
    /// Files kept.
    pub kept: usize,
    /// Their total size in bytes.
    pub kept_bytes: u64,
}

/// Prunes spilled MCP output under `root` (`<state>/mcp-output/`, laid out `<key>/<call>.txt`):
/// files older than `retention.max_age` go, then the oldest until the rest fit in
/// `retention.max_bytes`; empty key directories older than an hour go too. Everything kept is
/// made private (files 0600, directories 0700), which also fixes files from older versions.
/// Symlinks are never followed or deleted through. Errors on single entries are skipped.
pub fn prune_output(
    root: &Path,
    retention: OutputRetention,
    now: std::time::SystemTime,
) -> PruneOutcome {
    let mut out = PruneOutcome::default();
    let is_dir = |p: &Path| std::fs::symlink_metadata(p).is_ok_and(|m| m.file_type().is_dir());
    if !is_dir(root) {
        return out;
    }
    make_private(root, 0o700);
    let age = |m: &std::fs::Metadata| {
        m.modified()
            .ok()
            .and_then(|t| now.duration_since(t).ok())
            .unwrap_or_default()
    };
    let mut files: Vec<(PathBuf, std::time::SystemTime, u64)> = Vec::new();
    let mut dirs: Vec<(PathBuf, bool)> = Vec::new();
    let Ok(keys) = std::fs::read_dir(root) else {
        return out;
    };
    for key in keys.flatten() {
        let Ok(meta) = std::fs::symlink_metadata(key.path()) else {
            continue;
        };
        if !meta.file_type().is_dir() {
            continue;
        }
        let dir = key.path();
        // Judged before its files are pruned (which touches it): a directory nobody has written
        // to for an hour is no session's current one.
        let idle = age(&meta) > EMPTY_DIR_GRACE;
        make_private(&dir, 0o700);
        if let Ok(entries) = std::fs::read_dir(&dir) {
            for entry in entries.flatten() {
                let Ok(m) = std::fs::symlink_metadata(entry.path()) else {
                    continue;
                };
                if !m.file_type().is_file() {
                    continue;
                }
                if retention.max_age.is_some_and(|max| age(&m) > max) {
                    if std::fs::remove_file(entry.path()).is_ok() {
                        out.removed += 1;
                    }
                    continue;
                }
                make_private(&entry.path(), 0o600);
                let modified = m.modified().unwrap_or(std::time::UNIX_EPOCH);
                files.push((entry.path(), modified, m.len()));
            }
        }
        dirs.push((dir, idle));
    }
    // Oldest first, until the rest fit.
    files.sort_by(|a, b| a.1.cmp(&b.1).then_with(|| a.0.cmp(&b.0)));
    let mut total: u64 = files.iter().map(|f| f.2).sum();
    for (path, _, len) in files {
        if retention.max_bytes.is_some_and(|max| total > max) && std::fs::remove_file(&path).is_ok()
        {
            out.removed += 1;
            total -= len;
        } else {
            out.kept += 1;
        }
    }
    out.kept_bytes = total;
    for (dir, idle) in dirs {
        if idle {
            // Fails (and is skipped) unless it is empty.
            let _ = std::fs::remove_dir(&dir);
        }
    }
    out
}

fn make_private(path: &Path, mode: u32) {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode));
    }
    #[cfg(not(unix))]
    let _ = (path, mode);
}

/// `s` with everything but `[A-Za-z0-9_.-]` replaced by `_` (a file or directory name).
fn file_component(s: &str) -> String {
    s.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || matches!(c, '_' | '-' | '.') {
                c
            } else {
                '_'
            }
        })
        .collect::<String>()
        .trim_start_matches('.')
        .to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backoff_doubles_to_the_cap() {
        let b = Backoff::default();
        let secs: Vec<u64> = (1..=8).map(|n| b.delay(n).as_secs()).collect();
        assert_eq!(secs, [1, 2, 4, 8, 16, 32, 60, 60]);
        assert_eq!(b.delay(0), Duration::from_secs(1));
        assert_eq!(b.delay(99), Duration::from_secs(60));
    }

    #[test]
    fn output_under_the_cap_is_unchanged() {
        let dir = scratch("cap-small");
        assert_eq!(cap_output("hello", 2, &dir, "c1"), "hello");
        assert_eq!(cap_output("12345678", 2, &dir, "c1"), "12345678");
        assert!(!dir.join("c1.txt").exists());
    }

    #[test]
    fn output_over_the_cap_is_cut_and_saved_whole() {
        let dir = scratch("cap-big");
        let text: String = "é".repeat(30) + &"x".repeat(70);
        let out = cap_output(&text, 10, &dir, "call/../7");
        let file = dir.join("call_.._7.txt");
        assert_eq!(std::fs::read_to_string(&file).unwrap(), text);
        assert!(out.starts_with(&"é".repeat(30)), "{out}");
        let head: String = text.chars().take(40).collect();
        assert!(
            out.starts_with(&format!(
                "{head}\n\n[truncated: 25 tokens, over the 10-token limit; full output at "
            )),
            "{out}"
        );
        assert!(
            out.ends_with(&format!("{} (read it with read)]", file.display())),
            "{out}"
        );
    }

    #[test]
    fn instructions_go_before_amp_context_and_are_replaced() {
        let prompt = "ROLE\n\n<amp_server_instructions server=\"amp 1\">\nA\n</amp_server_instructions>\n\n<amp_context>\nrole: x\n</amp_context>\n\nTAIL";
        let a = instructions_block("fs", "use fs");
        let b = instructions_block("web", "use web");
        let one = with_mcp_instructions(prompt, std::slice::from_ref(&a));
        assert_eq!(
            one,
            "ROLE\n\n<amp_server_instructions server=\"amp 1\">\nA\n</amp_server_instructions>\n\n<mcp_server_instructions server=\"fs\">\nuse fs\n</mcp_server_instructions>\n\n<amp_context>\nrole: x\n</amp_context>\n\nTAIL"
        );
        let two = with_mcp_instructions(&one, &[a.clone(), b.clone()]);
        assert_eq!(two.matches("<mcp_server_instructions").count(), 2);
        assert!(two.find("server=\"fs\"").unwrap() < two.find("server=\"web\"").unwrap());
        assert_eq!(with_mcp_instructions(&two, &[]), prompt);
        assert_eq!(with_mcp_instructions(&two, &[a.clone(), b]), two);
        // No <amp_context>: appended.
        assert_eq!(
            with_mcp_instructions("P", std::slice::from_ref(&a)),
            format!("P\n\n{a}")
        );
    }

    #[test]
    fn long_instructions_are_cut_at_4_kb() {
        let block = instructions_block("fs", &"ü".repeat(3000));
        assert!(block.contains("[instructions truncated at 4 KB]"));
        assert!(block.len() < MAX_INSTRUCTIONS_BYTES + 200);
    }

    #[test]
    fn statuses_have_words_levels_and_lines() {
        let failed = ServerStatus::Failed {
            reason: "boom".into(),
            retry_in_ms: 4000,
        };
        assert_eq!(failed.to_string(), "failed: boom (retrying in 4 s)");
        assert_eq!(failed.level(), HealthLevel::Degraded);
        assert_eq!(ServerStatus::Ready.level(), HealthLevel::Ok);
        assert_eq!(
            ServerStatus::NeedsLogin { reason: "x".into() }.word(),
            "needs-login"
        );
        assert_eq!(
            serde_json::to_value(&failed).unwrap(),
            serde_json::json!({"state": "failed", "reason": "boom", "retry_in_ms": 4000})
        );
        let ready = HubEvent::Ready {
            server: "github".into(),
            instance: None,
            tools: 12,
        };
        assert_eq!(ready.line(), "github: 12 tools");
        let changed = HubEvent::ToolsChanged {
            server: "fs".into(),
            instance: Some("7".into()),
            tools: 1,
        };
        assert_eq!(changed.line(), "fs (#7): tools changed (1 tool)");
        // #2334: the reason's own `MCP server `x`:` isn't repeated, and an unset variable says
        // what to do.
        let unset = HubEvent::Failed {
            server: "authd".into(),
            instance: None,
            status: ServerStatus::Stopped {
                reason: "MCP server `authd`: environment variable QA_TOKEN is not set".into(),
            },
        };
        assert_eq!(
            unset.line(),
            "MCP server authd: stopped: environment variable QA_TOKEN is not set; set it where \
             you start amp-harness, then /reload"
        );
        let other = HubEvent::Failed {
            server: "fs".into(),
            instance: None,
            status: failed.clone(),
        };
        assert_eq!(
            other.line(),
            "MCP server fs: failed: boom (retrying in 4 s)"
        );
        assert_eq!(
            HubSummary {
                ready: 3,
                total: 4,
                level: HubLevel::Degraded
            }
            .to_string(),
            "mcp 3/4"
        );
    }

    /// #2334: a reload retries a stopped server and it fails the same way: shown once, again
    /// when the failure changes or after it was ready in between.
    #[test]
    fn a_failure_is_shown_once_per_change() {
        let fail = |server: &str, reason: &str, retry_in_ms: u64| HubEvent::Failed {
            server: server.into(),
            instance: None,
            status: ServerStatus::Failed {
                reason: reason.into(),
                retry_in_ms,
            },
        };
        let stopped = |reason: &str| HubEvent::Failed {
            server: "a".into(),
            instance: None,
            status: ServerStatus::Stopped {
                reason: reason.into(),
            },
        };
        let mut log = FailureLog::default();
        assert!(log.line(&stopped("no TOKEN")).is_some());
        assert_eq!(log.line(&stopped("no TOKEN")), None);
        assert_eq!(log.line(&stopped("no TOKEN")), None);
        // Another server's failure is its own.
        assert!(log.line(&fail("b", "refused", 1000)).is_some());
        // The backoff's delay is not a change; the reason is.
        assert_eq!(log.line(&fail("b", "refused", 2000)), None);
        assert!(log.line(&fail("b", "timed out", 4000)).is_some());
        assert!(log.line(&stopped("no OTHER")).is_some());
        // Ready in between: the same failure is news again.
        let ready = HubEvent::Ready {
            server: "a".into(),
            instance: None,
            tools: 2,
        };
        assert_eq!(log.line(&ready).as_deref(), Some("a: 2 tools"));
        assert!(log.line(&stopped("no OTHER")).is_some());
    }

    #[test]
    fn file_components_are_safe() {
        assert_eq!(file_component("../x y"), "_x_y");
        assert_eq!(file_component("amp-manager-17"), "amp-manager-17");
    }

    /// #2309 (H18 finding 10): spill files are 0600 in 0700 directories, even when an older
    /// file of the same name was world-readable.
    #[cfg(unix)]
    #[test]
    fn spilled_output_is_private() {
        use std::os::unix::fs::PermissionsExt;
        let root = scratch("cap-private").join("mcp-output");
        let dir = root.join("amp-manager-1");
        let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
        cap_output(&"x".repeat(100), 2, &dir, "c1");
        assert_eq!(mode(&dir.join("c1.txt")), 0o600);
        assert_eq!(mode(&dir), 0o700);
        assert_eq!(mode(&root), 0o700);
        std::fs::set_permissions(dir.join("c1.txt"), std::fs::Permissions::from_mode(0o644))
            .unwrap();
        cap_output(&"y".repeat(100), 2, &dir, "c1");
        assert_eq!(mode(&dir.join("c1.txt")), 0o600);
        assert_eq!(
            std::fs::read_to_string(dir.join("c1.txt")).unwrap(),
            "y".repeat(100)
        );
    }

    /// Writes `len` bytes to `path`, last modified `age` ago.
    fn aged(path: &Path, len: usize, age: Duration) {
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, "z".repeat(len)).unwrap();
        let f = std::fs::File::options().write(true).open(path).unwrap();
        f.set_modified(std::time::SystemTime::now() - age).unwrap();
    }

    const DAY: Duration = Duration::from_secs(86_400);

    #[test]
    fn retention_comes_from_config_with_defaults_and_zero_turns_a_limit_off() {
        assert_eq!(
            OutputRetention::default(),
            OutputRetention {
                max_age: Some(DAY * 7),
                max_bytes: Some(512 * 1024 * 1024)
            }
        );
        assert_eq!(
            OutputRetention::from_config(Some(0), Some(1)),
            OutputRetention {
                max_age: None,
                max_bytes: Some(1024 * 1024)
            }
        );
        assert_eq!(
            OutputRetention::from_config(Some(2), Some(0)).max_bytes,
            None
        );
    }

    /// #2309: pruning removes files past the age limit, then the oldest until the rest fit, and
    /// never deletes through a symlink.
    #[test]
    fn prune_removes_old_output_then_the_oldest_over_the_size_cap() {
        let base = scratch("prune");
        let root = base.join("mcp-output");
        aged(&root.join("old-session/a.txt"), 10, DAY * 9);
        std::fs::File::open(root.join("old-session"))
            .unwrap()
            .set_modified(std::time::SystemTime::now() - DAY * 9)
            .unwrap();
        aged(&root.join("t42/b.txt"), 100, DAY * 3);
        aged(&root.join("t42/c.txt"), 100, DAY * 2);
        aged(&root.join("now/d.txt"), 100, Duration::from_secs(5));
        // A file outside, reached through a symlink in the tree: never touched.
        aged(&base.join("outside.txt"), 10, DAY * 30);
        #[cfg(unix)]
        std::os::unix::fs::symlink(base.join("outside.txt"), root.join("t42/link.txt")).unwrap();
        #[cfg(unix)]
        std::os::unix::fs::symlink(&base, root.join("linked-dir")).unwrap();
        // An empty directory from long ago goes; the empty one just made stays.
        std::fs::create_dir_all(root.join("empty-old")).unwrap();
        std::fs::File::open(root.join("empty-old"))
            .unwrap()
            .set_modified(std::time::SystemTime::now() - DAY)
            .unwrap();
        std::fs::create_dir_all(root.join("empty-new")).unwrap();

        let retention = OutputRetention {
            max_age: Some(DAY * 7),
            max_bytes: Some(250),
        };
        let out = prune_output(&root, retention, std::time::SystemTime::now());
        assert_eq!(
            out,
            PruneOutcome {
                removed: 2,
                kept: 2,
                kept_bytes: 200
            }
        );
        assert!(!root.join("old-session/a.txt").exists());
        assert!(!root.join("old-session").exists());
        assert!(!root.join("t42/b.txt").exists(), "the oldest goes first");
        assert!(root.join("t42/c.txt").exists() && root.join("now/d.txt").exists());
        assert!(base.join("outside.txt").exists());
        assert!(!root.join("empty-old").exists());
        assert!(root.join("empty-new").exists());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
            assert_eq!(
                mode(&root.join("t42/c.txt")),
                0o600,
                "fixed from the umask's mode"
            );
            assert_eq!(mode(&root.join("t42")), 0o700);
            assert_eq!(mode(&root), 0o700);
            assert!(
                std::fs::symlink_metadata(root.join("t42/link.txt"))
                    .unwrap()
                    .file_type()
                    .is_symlink()
            );
        }
        // No limits: nothing goes. No directory: nothing to do.
        let none = OutputRetention {
            max_age: None,
            max_bytes: None,
        };
        assert_eq!(
            prune_output(&root, none, std::time::SystemTime::now()).removed,
            0
        );
        assert_eq!(
            prune_output(
                &base.join("missing"),
                retention,
                std::time::SystemTime::now()
            ),
            PruneOutcome::default()
        );
    }

    /// #2309: a session's hub prunes when it starts; a hub without a retention never does.
    #[tokio::test]
    async fn a_started_hub_prunes_old_output() {
        let base = scratch("prune-start");
        let old = base.join("state/mcp-output/s/old.txt");
        let config = || McpConfig {
            servers: BTreeMap::new(),
            problems: Vec::new(),
            broken: BTreeSet::new(),
        };
        let options = |retention| {
            let mut o = HubOptions::new(ConfigPaths::default(), base.join("state"), base.clone());
            o.trust_store = None;
            o.output_retention = retention;
            o
        };
        aged(&old, 10, DAY * 30);
        let hub = McpHub::new(config(), options(None));
        hub.start();
        tokio::time::sleep(Duration::from_millis(200)).await;
        assert!(old.exists(), "no retention: nothing pruned");
        hub.shutdown().await;

        let hub = McpHub::new(config(), options(Some(OutputRetention::default())));
        hub.start();
        let started = Instant::now();
        while old.exists() && started.elapsed() < Duration::from_secs(10) {
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert!(!old.exists(), "pruned at start");
        // The hourly pruner stops with the hub.
        tokio::time::timeout(Duration::from_secs(5), hub.shutdown())
            .await
            .expect("shutdown is not held up by the pruner");
    }

    /// #2311 (review #2248 finding 11): a cell's task clears `task_running` when it ends, and the
    /// hub's task list keeps only live handles however often a server is restarted.
    #[tokio::test]
    async fn finished_cell_tasks_are_forgotten() {
        let base = scratch("task-handles");
        let table: toml::Table = toml::from_str(
            "[gone]\ncommand = \"/nonexistent/amp-test-server\"\nstartup = \"background\"\n",
        )
        .unwrap();
        let config = McpConfig::from_tables(Some((&table, &base.join("config.toml"))), None);
        assert!(config.servers.contains_key("gone"), "{:?}", config.problems);
        let mut opts = HubOptions::new(ConfigPaths::default(), base.join("state"), base.clone());
        opts.trust_store = None;
        let hub = McpHub::new(config, opts);
        hub.start();
        let cell = hub.inner.shared_cell("gone").unwrap();
        assert!(cell.task_running.load(Ordering::Acquire));

        // The cell's task ends (as when a reload retires it): the flag is cleared.
        cell.stop.cancel();
        let started = Instant::now();
        while cell.task_running.load(Ordering::Acquire)
            && started.elapsed() < Duration::from_secs(5)
        {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(!cell.task_running.load(Ordering::Acquire), "flag cleared");

        // Each restart now spawns a task (its token is cancelled, so it ends at once); the list
        // never holds more than the live ones plus the newest.
        for _ in 0..50 {
            hub.restart("gone").unwrap();
            let started = Instant::now();
            while cell.task_running.load(Ordering::Acquire)
                && started.elapsed() < Duration::from_secs(5)
            {
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        }
        let held = hub.inner.tasks.lock().unwrap().len();
        assert!(held <= 2, "{held} task handles kept");
        hub.shutdown().await;
    }

    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/amp-core-mcp-hub")
            .join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }
}
