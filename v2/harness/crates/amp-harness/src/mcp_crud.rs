//! `amp-harness mcp …` (#2243, H9 of decisions/amp-harness-configurability.md): add, list, show,
//! edit, remove, enable, disable, test, log in to and out of any MCP server (stdio, Streamable HTTP
//! or legacy HTTP+SSE), in the user's config.toml or the repo's `.amp/harness/config.toml`, without
//! editing files by hand.
//!
//! The command shapes follow Claude Code's `claude mcp …` so habits carry over:
//! `mcp add <name> -- <command> [args…]` (stdio), `mcp add <name> --url URL [--transport
//! http|sse]` (or the URL as the positional), `--header`, `--env`, `--scope user|repo`,
//! `mcp add-json <name> '<json>'`.
//!
//! Rules every writer keeps:
//! - **Secrets never touch a file or argv.** `--secret-header NAME` / `--secret-env NAME` (and
//!   `--client-secret`) are asked hidden, or read from stdin with `--secret-stdin`, and stored only
//!   in the OS keychain (`mcp:<server>:header:<name>`, `mcp:<server>:env:<VAR>`,
//!   `mcp:<server>:oauth-client-secret`). A `--header`/`--env` value that looks secret is refused
//!   with the safe spellings; `${VAR}` (Claude Code's form) becomes `${env:VAR}`, a reference.
//!   Output shows where a secret comes from and whether it is there, never its value.
//! - **Writes are checked and backed up.** Each change replaces only the server's own tables
//!   ([`ConfigFile::replace_tables`]: comments elsewhere kept, `config.toml.bak` first, 0600), and
//!   the result must load through [`McpConfig`] with the server as intended, or nothing is written.
//! - **Repo servers need trust (#2241).** Adding or editing one here is the person's own act, so it
//!   is also trusted for them as configured now; removing it forgets the trust.
//! - `mcp test`, `mcp list`'s health and `mcp call/tools --server` connect through a short-lived
//!   [`McpHub`] (start, wait, shutdown), so they behave exactly as sessions do: same env stripping,
//!   trust, keychain and transports.

use std::{
    collections::{BTreeMap, BTreeSet},
    io::Read as _,
    path::{Path, PathBuf},
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::{
    agent::{mcp_strip_names, state_dir},
    mcp_config::{
        self, AuthMode, ExpandContext, Layer, McpConfig, McpProblem, McpServer, Origin, Resolved,
        Scope as RunScope, Severity, Startup, Transport, env_account, header_account,
        oauth_client_secret_account,
    },
    mcp_http,
    mcp_hub::{HubOptions, McpHub, ServerHealth, ServerStatus, WorktreeSet},
    mcp_trust::{self, TrustState, TrustStore},
    models::ModelsConfig,
    repo_config::RepoConfig,
};
use amp_mcp::{CredentialBackend, credential_key, external::http, oauth};
use clap::{Args, ValueEnum};
use serde::Serialize;
use serde_json::{Value, json};

use crate::{
    agents_cmd::{self, Ctx, EDIT_DIR, Scope},
    conn::{CliError, print_json},
    setup::{
        self, Change, ConfigFile,
        config_file::{remove_tables, set_raw, table_text, toml_key},
        ui::{self, Ui},
    },
};

// ------------------------------------------------------------------------------------------------
// Arguments
// ------------------------------------------------------------------------------------------------

/// `--scope user|repo` (Claude Code's flag; `project` is accepted for repo), or `--user`/`--repo`
/// like `agents` and `models`.
#[derive(Args, Clone, Copy, Default, Debug)]
pub struct ScopeOpt {
    /// Which config: `user` (your config.toml) or `repo` (<repo>/.amp/harness/config.toml, shared
    /// with everyone who clones the repo; `project` means the same).
    #[arg(long, short = 's', value_enum, value_name = "user|repo")]
    scope: Option<ScopeName>,
    /// Same as --scope user.
    #[arg(long, conflicts_with_all = ["scope", "repo"])]
    user: bool,
    /// Same as --scope repo.
    #[arg(long, conflicts_with = "scope")]
    repo: bool,
}

#[derive(Clone, Copy, Debug, ValueEnum)]
enum ScopeName {
    User,
    #[value(alias = "project")]
    Repo,
}

impl ScopeOpt {
    fn get(self) -> Option<Scope> {
        match (self.scope, self.user, self.repo) {
            (Some(ScopeName::User), ..) | (None, true, _) => Some(Scope::User),
            (Some(ScopeName::Repo), ..) | (None, _, true) => Some(Scope::Repo),
            _ => None,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, ValueEnum)]
pub enum TransportArg {
    Stdio,
    Http,
    Sse,
}

#[derive(Clone, Copy, Debug, ValueEnum)]
pub enum AuthArg {
    Auto,
    Oauth,
    None,
}

#[derive(Clone, Copy, Debug, ValueEnum)]
pub enum StartupArg {
    Background,
    Eager,
    Lazy,
}

#[derive(Args)]
pub struct AddArgs {
    /// Server name: lowercase letters, digits and dashes. Its tools appear as
    /// mcp__<name>__<tool>.
    pub name: String,
    /// http/sse: the URL (or --url). stdio: the command, when it takes no arguments.
    #[arg(value_name = "URL | COMMAND")]
    pub first: Option<String>,
    /// stdio: the command and its arguments after `--` (`mcp add fs -- npx -y pkg`).
    #[arg(last = true, value_name = "COMMAND [ARGS]…")]
    pub rest: Vec<String>,
    /// stdio, http (Streamable HTTP; falls back to SSE) or sse (legacy HTTP+SSE). Default: http
    /// for a URL, stdio for a command.
    #[arg(long, short = 't', value_enum)]
    pub transport: Option<TransportArg>,
    /// The server's URL (http/sse).
    #[arg(long)]
    pub url: Option<String>,
    /// A request header, `"Name: value"` or `Name=value` (repeatable). Not for secrets: use
    /// --secret-header, --bearer-env or a reference such as 'Authorization: Bearer ${env:TOKEN}'.
    #[arg(long = "header", short = 'H', value_name = "NAME: VALUE")]
    pub headers: Vec<String>,
    /// An environment variable for a stdio server, `KEY=VALUE` (repeatable). Not for secrets: use
    /// --secret-env or a reference such as KEY='${env:KEY}'.
    #[arg(long = "env", short = 'e', value_name = "KEY=VALUE")]
    pub env: Vec<String>,
    /// A header whose value is secret: asked hidden (or read with --secret-stdin) and kept only in
    /// the OS keychain (repeatable).
    #[arg(long = "secret-header", value_name = "NAME")]
    pub secret_headers: Vec<String>,
    /// A stdio server's secret environment variable: asked hidden and kept only in the OS keychain
    /// (repeatable).
    #[arg(long = "secret-env", value_name = "KEY")]
    pub secret_env: Vec<String>,
    /// Read the secret values from stdin, one per line: secret headers, then secret env vars, then
    /// the OAuth client secret, in the order given.
    #[arg(long)]
    pub secret_stdin: bool,
    /// Send `Authorization: Bearer $VAR` (the variable's value at connect time; nothing stored).
    #[arg(long, value_name = "VAR")]
    pub bearer_env: Option<String>,
    /// auto (OAuth when the server asks, the default), oauth (always; run `mcp login`) or none.
    #[arg(long, value_enum)]
    pub auth: Option<AuthArg>,
    /// A pre-registered OAuth client id (else the client registers itself).
    #[arg(long, value_name = "ID")]
    pub client_id: Option<String>,
    /// The OAuth client has a secret: asked hidden and kept in the OS keychain (needs
    /// --client-id).
    #[arg(long)]
    pub client_secret: bool,
    /// OAuth scopes to ask for (comma-separated or repeated).
    #[arg(long = "oauth-scope", value_delimiter = ',', value_name = "SCOPE")]
    pub oauth_scopes: Vec<String>,
    /// The fixed loopback port for the OAuth redirect (some servers register one).
    #[arg(long, value_name = "PORT")]
    pub callback_port: Option<u16>,
    /// http only: don't retry as legacy SSE when Streamable HTTP is refused.
    #[arg(long)]
    pub no_sse_fallback: bool,
    /// A stdio server's working directory (default: the repo root). May use ${repo}, ${worktree}.
    #[arg(long, value_name = "DIR")]
    pub workdir: Option<String>,
    /// A dotenv file for a stdio server (relative to the config file's folder, or the repo root).
    #[arg(long, value_name = "FILE")]
    pub env_file: Option<String>,
    /// stdio: one instance per worker worktree instead of one shared by everyone.
    #[arg(long)]
    pub per_worktree: bool,
    /// Only these roles may use it (globs without `amp-`, e.g. `worker-*,reviewer`).
    #[arg(long, value_delimiter = ',', value_name = "ROLES")]
    pub roles: Vec<String>,
    /// When it connects: background (the default), eager (the chat waits) or lazy.
    #[arg(long, value_enum)]
    pub startup: Option<StartupArg>,
    /// Seconds to wait for it to start.
    #[arg(long, value_name = "SECS")]
    pub startup_timeout: Option<u64>,
    /// Seconds a tool call may take.
    #[arg(long, value_name = "SECS")]
    pub timeout: Option<u64>,
    /// Add it switched off (`mcp enable` turns it on).
    #[arg(long)]
    pub disabled: bool,
    /// Replace a server of the same name in that config.
    #[arg(long)]
    pub force: bool,
    /// Don't connect to it after adding.
    #[arg(long)]
    pub no_test: bool,
    #[command(flatten)]
    pub scope: ScopeOpt,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct AddJsonArgs {
    /// Server name.
    pub name: String,
    /// The server as Claude Code / Cursor / VS Code JSON (`{"type":"http","url":…,"headers":…}`
    /// or `{"command":…,"args":[…],"env":{…}}`); `-` reads it from stdin, which keeps secret
    /// values off the command line. Secret-looking values go to the OS keychain.
    pub json_config: String,
    /// Replace a server of the same name in that config.
    #[arg(long)]
    pub force: bool,
    /// Don't connect to it after adding.
    #[arg(long)]
    pub no_test: bool,
    #[command(flatten)]
    pub scope: ScopeOpt,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct ListArgs {
    /// Don't connect: list the configuration only.
    #[arg(long)]
    pub no_health: bool,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct GetArgs {
    pub name: String,
    /// Show this config's table (default: the one in effect).
    #[command(flatten)]
    pub scope: ScopeOpt,
    /// Don't connect to it.
    #[arg(long)]
    pub no_health: bool,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct EditArgs {
    pub name: String,
    /// Set the roles that may use it (globs without `amp-`; comma-separated) instead of opening
    /// $EDITOR. `*` is everyone.
    #[arg(long, value_delimiter = ',', value_name = "ROLES")]
    pub roles: Option<Vec<String>>,
    /// Read the secret values the edit adds from stdin, one per line.
    #[arg(long)]
    pub secret_stdin: bool,
    #[command(flatten)]
    pub scope: ScopeOpt,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct RemoveArgs {
    pub name: String,
    #[command(flatten)]
    pub scope: ScopeOpt,
    /// Keep its secrets in the OS keychain.
    #[arg(long)]
    pub keep_secrets: bool,
    /// Keep its OAuth login.
    #[arg(long)]
    pub keep_login: bool,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct NameArgs {
    pub name: String,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct TestArgs {
    pub name: String,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct LoginArgs {
    pub name: String,
    /// Print the login URL instead of opening a browser.
    #[arg(long)]
    pub no_browser: bool,
    /// Seconds to wait for the browser to finish.
    #[arg(long, default_value_t = oauth::DEFAULT_LOGIN_TIMEOUT.as_secs())]
    pub timeout: u64,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct SecretArgs {
    pub name: String,
    /// Which secrets (header names, env var names, or `oauth`); default: every one the server
    /// keeps in the keychain.
    pub which: Vec<String>,
    /// Only those not stored yet.
    #[arg(long)]
    pub missing: bool,
    /// Read the values from stdin, one per line, in the order `mcp get` lists them.
    #[arg(long)]
    pub secret_stdin: bool,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

// ------------------------------------------------------------------------------------------------
// Context: files, layers, hub
// ------------------------------------------------------------------------------------------------

fn ctx() -> Result<Ctx, CliError> {
    Ctx::here()
}

fn env(k: &str) -> Option<String> {
    std::env::var(k).ok()
}

fn layer_of(scope: Scope) -> Layer {
    match scope {
        Scope::User => Layer::User,
        Scope::Repo => Layer::Repo,
    }
}

fn config_file(ctx: &Ctx, scope: Scope) -> Result<ConfigFile, CliError> {
    let path = ctx.scope_config_file(scope)?;
    Ok(match scope {
        Scope::User => ConfigFile::new(path),
        Scope::Repo => ConfigFile::repo(path),
    })
}

/// The `[mcp]` table of a layer's file as written (empty when the file or table is missing).
fn layer_mcp(text: &str, file: &Path) -> Result<toml::Table, String> {
    let mut doc: toml::Table = toml::from_str(text)
        .map_err(|e| format!("{} does not parse: {}", file.display(), e.message()))?;
    match doc.remove("mcp") {
        Some(toml::Value::Table(t)) => Ok(t),
        Some(_) => Err(format!("{}: `mcp` is not a table", file.display())),
        None => Ok(toml::Table::new()),
    }
}

/// The raw `[mcp.<name>]` of `scope`'s file, if it has one.
fn layer_table(ctx: &Ctx, scope: Scope, name: &str) -> Result<Option<toml::Value>, CliError> {
    let Ok(file) = config_file(ctx, scope) else {
        return Ok(None);
    };
    let Some(text) = file.text().map_err(CliError::new)? else {
        return Ok(None);
    };
    Ok(layer_mcp(&text, &file.path)
        .map_err(CliError::new)?
        .get(name)
        .cloned())
}

/// Only `enabled` set: a user-file switch for a repo server.
fn is_switch(value: &toml::Value) -> bool {
    value
        .as_table()
        .is_some_and(|t| t.len() == 1 && t.contains_key("enabled"))
}

/// The layer whose table `name` a writer changes: the one asked for, else the only one defining
/// it (a full table first, a user switch only when nothing else does).
fn pick_scope(
    ctx: &Ctx,
    name: &str,
    wanted: Option<Scope>,
    verb: &str,
) -> Result<(Scope, toml::Value), CliError> {
    if let Some(scope) = wanted {
        return match layer_table(ctx, scope, name)? {
            Some(v) => Ok((scope, v)),
            None => Err(CliError::usage(format!(
                "no MCP server `{name}` in the {} config ({})",
                scope.name(),
                config_file(ctx, scope)?.path.display()
            ))),
        };
    }
    let mut full = Vec::new();
    let mut switches = Vec::new();
    for scope in [Scope::User, Scope::Repo] {
        if let Some(v) = layer_table(ctx, scope, name)? {
            if is_switch(&v) {
                switches.push((scope, v));
            } else {
                full.push((scope, v));
            }
        }
    }
    let found = if full.is_empty() { switches } else { full };
    match found.len() {
        0 => Err(CliError::usage(format!(
            "no MCP server `{name}` (`amp-harness mcp list` shows them)"
        ))),
        1 => Ok(found.into_iter().next().expect("one")),
        _ => Err(CliError::usage(format!(
            "MCP server `{name}` is in both the user and the repo config; say which to {verb} \
             with --scope user or --scope repo"
        ))),
    }
}

fn origin(ctx: &Ctx, scope: Scope) -> Result<Origin, CliError> {
    Ok(Origin {
        layer: layer_of(scope),
        file: ctx.scope_config_file(scope)?,
    })
}

/// Parses one table as `scope` would load it; errors list every problem.
fn parse_table(
    name: &str,
    value: &toml::Value,
    origin: Origin,
) -> Result<(McpServer, Vec<McpProblem>), CliError> {
    mcp_config::parse_server(name, value, origin).map_err(|problems| {
        CliError::usage(format!(
            "MCP server `{name}` would not load:\n{}",
            problems
                .iter()
                .filter(|p| p.severity == Severity::Error)
                .map(|p| format!("  {p}"))
                .collect::<Vec<_>>()
                .join("\n")
        ))
    })
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Expect {
    Present,
    Absent,
}

/// What a changed layer file must load to: `name` present and valid, or gone.
fn layer_check(
    scope: Scope,
    file: PathBuf,
    name: String,
    expect: Expect,
) -> impl Fn(&str) -> Result<(), String> {
    move |text: &str| {
        let mcp = match scope {
            Scope::User => layer_mcp(text, &file)?,
            Scope::Repo => {
                RepoConfig::parse(text, &file)
                    .map_err(|e| e.to_string())?
                    .mcp
            }
        };
        let config = match scope {
            Scope::User => McpConfig::from_tables(Some((&mcp, file.as_path())), None),
            Scope::Repo => McpConfig::from_tables(None, Some((&mcp, file.as_path()))),
        };
        match expect {
            Expect::Present if config.servers.contains_key(&name) => Ok(()),
            Expect::Present => Err(config
                .problems
                .iter()
                .find(|p| p.severity == Severity::Error)
                .map(|p| p.to_string())
                .unwrap_or_else(|| format!("[mcp.{name}] did not load"))),
            Expect::Absent if !mcp.contains_key(&name) => Ok(()),
            Expect::Absent => Err(format!(
                "`mcp.{name}` is still defined (inline under [mcp]?)"
            )),
        }
    }
}

/// The text `replace_tables` would write (to check before anything else is changed).
fn preview_replace(file: &ConfigFile, prefix: &str, block: Option<&str>) -> Result<String, String> {
    let before = file.text()?.unwrap_or_default();
    let (mut text, _) = remove_tables(&before, prefix);
    if let Some(block) = block {
        let trimmed = text.trim_end().to_owned();
        text = if trimmed.is_empty() {
            String::new()
        } else {
            format!("{trimmed}\n\n")
        };
        text.push_str(block.trim_end());
        text.push('\n');
    }
    Ok(text)
}

/// The order keys are written in: what it is and runs first, tuning last.
const KEY_ORDER: [&str; 26] = [
    "type",
    "command",
    "args",
    "url",
    "headers",
    "secret_headers",
    "bearer_env",
    "auth",
    "oauth",
    "env",
    "secret_env",
    "env_file",
    "env_inherit",
    "cwd",
    "scope",
    "ca_file",
    "protocol",
    "socket",
    "fallback_sse",
    "enabled",
    "roles",
    "startup",
    "startup_timeout_secs",
    "call_timeout_secs",
    "max_output_tokens",
    "instructions",
];

/// `[mcp.<name>]` as TOML text: one header, `note` as a comment under it, keys in
/// [`KEY_ORDER`], and `env`/`headers`/`oauth` as inline tables.
fn mcp_block(name: &str, value: &toml::Value, note: Option<&str>) -> Result<String, CliError> {
    let table = value
        .as_table()
        .ok_or_else(|| CliError::new(format!("`mcp.{name}` is not a table")))?;
    let mut keys: Vec<&String> = table.keys().collect();
    keys.sort_by_key(|k| {
        (
            KEY_ORDER
                .iter()
                .position(|o| o == k)
                .unwrap_or(KEY_ORDER.len()),
            (*k).clone(),
        )
    });
    let mut out = format!("[mcp.{}]\n", toml_key(name));
    if let Some(note) = note {
        out.push_str(&format!("# {note}\n"));
    }
    for k in keys {
        out.push_str(&format!("{} = {}\n", toml_key(k), inline(&table[k])));
    }
    // Written by hand, so prove it reads back as the same table.
    let back: toml::Table = toml::from_str(&out).map_err(|e| CliError::new(e.to_string()))?;
    if back.get("mcp").and_then(|m| m.get(name)) != Some(value) {
        return Err(CliError::new(format!(
            "could not write `mcp.{name}` as TOML"
        )));
    }
    Ok(out)
}

/// A value as an inline TOML value (tables as `{ k = v }`).
fn inline(v: &toml::Value) -> String {
    match v {
        toml::Value::Table(t) => {
            if t.is_empty() {
                return "{}".to_owned();
            }
            let parts: Vec<String> = t
                .iter()
                .map(|(k, v)| format!("{} = {}", toml_key(k), inline(v)))
                .collect();
            format!("{{ {} }}", parts.join(", "))
        }
        toml::Value::Array(a) => {
            let parts: Vec<String> = a.iter().map(inline).collect();
            format!("[{}]", parts.join(", "))
        }
        toml::Value::String(s) => crate::setup::config_file::toml_string(s),
        other => other.to_string(),
    }
}

fn repo_root(ctx: &Ctx) -> Option<PathBuf> {
    ctx.paths
        .repo_dir
        .as_deref()
        .and_then(|d| d.ancestors().nth(2))
        .map(Path::to_path_buf)
}

fn state() -> Result<PathBuf, CliError> {
    state_dir(&env).ok_or_else(|| CliError::new("no state directory: set HOME or XDG_STATE_HOME"))
}

/// A hub for `servers` only, set up as `Runtime::load` sets up a session's (env stripping, trust
/// store, keychain), not started.
fn hub_for(ctx: &Ctx, servers: Vec<McpServer>) -> Result<McpHub, CliError> {
    let full = McpConfig::load(&ctx.paths);
    let strip = match ModelsConfig::load_paths(&env, &ctx.paths) {
        Ok(models) => mcp_strip_names(&models, &full),
        Err(_) => {
            let mut names = full.secret_env_names();
            names.insert(amp_mcp::ENV_TOKEN.to_owned());
            names
        }
    };
    let config = McpConfig {
        servers: servers.into_iter().map(|s| (s.name.clone(), s)).collect(),
        problems: Vec::new(),
        broken: BTreeSet::new(),
    };
    let cwd = repo_root(ctx).unwrap_or_else(|| ctx.cwd.clone());
    let opts = HubOptions::new(ctx.paths.clone(), state()?, cwd);
    let hub = McpHub::new(config, opts);
    hub.set_strip(strip);
    Ok(hub)
}

/// What connecting to some servers showed.
struct Probe {
    health: BTreeMap<String, ServerHealth>,
    tools: BTreeMap<String, Vec<String>>,
    elapsed: Duration,
}

/// Connects to `servers` (each as it would start in a session, lazy ones included), waits for the
/// first attempts, records health and tool names, and shuts everything down.
async fn probe(ctx: &Ctx, servers: Vec<McpServer>) -> Result<Probe, CliError> {
    let servers: Vec<McpServer> = servers
        .into_iter()
        .map(|mut s| {
            s.startup = Startup::Background;
            s
        })
        .collect();
    let worktree = servers
        .iter()
        .any(|s| s.enabled && s.scope == RunScope::Worktree);
    let names: Vec<String> = servers.iter().map(|s| s.name.clone()).collect();
    let hub = hub_for(ctx, servers)?;
    let started = Instant::now();
    hub.start();
    hub.wait_started(true).await;
    let cwd = repo_root(ctx).unwrap_or_else(|| ctx.cwd.clone());
    let set: Option<Arc<WorktreeSet>> = if worktree {
        hub.start_worktree("mcp-cli", &cwd).await
    } else {
        None
    };
    let elapsed = started.elapsed();
    let mut health: BTreeMap<String, ServerHealth> = BTreeMap::new();
    for h in hub.health() {
        if h.instance.is_none() || !health.contains_key(&h.name) {
            health.insert(h.name.clone(), h);
        }
    }
    if let Some(set) = &set {
        for h in set.health() {
            health.insert(h.name.clone(), h);
        }
    }
    let mut tools = BTreeMap::new();
    for name in &names {
        let found = hub
            .connection(name)
            .or_else(|e| set.as_ref().map_or(Err(e), |s| s.connection(name)));
        if let Ok((_, list)) = found {
            let mut list: Vec<String> = list.iter().map(|t| t.name.to_string()).collect();
            list.sort();
            tools.insert(name.clone(), list);
        }
    }
    if set.is_some() {
        hub.release_worktree("mcp-cli").await;
    }
    hub.shutdown().await;
    Ok(Probe {
        health,
        tools,
        elapsed,
    })
}

/// The last `n` lines of a server's stderr log.
fn stderr_tail(path: Option<&Path>, n: usize) -> Vec<String> {
    let Some(text) = path.and_then(|p| std::fs::read_to_string(p).ok()) else {
        return Vec::new();
    };
    let lines: Vec<&str> = text.lines().filter(|l| !l.trim().is_empty()).collect();
    lines[lines.len().saturating_sub(n)..]
        .iter()
        .map(|l| (*l).to_owned())
        .collect()
}

fn trust_store() -> Option<TrustStore> {
    let state = state().ok()?;
    TrustStore::load(&TrustStore::path_in(&state)).ok()
}

fn trust_of(server: &McpServer, store: Option<&TrustStore>) -> TrustState {
    mcp_trust::state_of(server, store, mcp_trust::trust_all_from(&env))
}

/// Records trust for the repo server `name` as configured now (the person just wrote it).
fn trust_now(ctx: &Ctx, name: &str) -> Result<PathBuf, CliError> {
    let config = McpConfig::load(&ctx.paths);
    let server = config
        .servers
        .get(name)
        .filter(|s| s.is_repo())
        .ok_or_else(|| CliError::new(format!("repo MCP server `{name}` did not load")))?;
    let path = TrustStore::path_in(&state()?);
    let mut store = TrustStore::load(&path).map_err(CliError::new)?;
    store.trust(server);
    store.save().map_err(CliError::new)?;
    Ok(path)
}

fn untrust_now(ctx: &Ctx, name: &str) {
    let (Some(repo), Ok(state)) = (repo_root(ctx), state()) else {
        return;
    };
    let repo = std::fs::canonicalize(&repo).unwrap_or(repo);
    let path = TrustStore::path_in(&state);
    if let Ok(mut store) = TrustStore::load(&path)
        && store.untrust(&repo, name)
    {
        let _ = store.save();
    }
}

// ------------------------------------------------------------------------------------------------
// Secrets
// ------------------------------------------------------------------------------------------------

/// One secret a server keeps in the keychain.
#[derive(Debug, Clone, Serialize)]
struct SecretItem {
    /// `header X-Api-Key`, `env BRAVE_API_KEY`, `OAuth client secret`.
    label: String,
    /// What `mcp secret <name> <which>` matches.
    #[serde(skip)]
    which: String,
    account: String,
}

fn keychain_items(server: &McpServer) -> Vec<SecretItem> {
    let name = &server.name;
    let mut out = Vec::new();
    match &server.transport {
        Transport::Stdio(s) => {
            for v in &s.secret_env {
                out.push(SecretItem {
                    label: format!("env {v}"),
                    which: v.clone(),
                    account: env_account(name, v),
                });
            }
        }
        Transport::Http(r) | Transport::Sse(r) => {
            for h in &r.secret_headers {
                out.push(SecretItem {
                    label: format!("header {h}"),
                    which: h.clone(),
                    account: header_account(name, h),
                });
            }
            if r.oauth.as_ref().is_some_and(|o| o.client_secret) {
                out.push(SecretItem {
                    label: "OAuth client secret".to_owned(),
                    which: "oauth".to_owned(),
                    account: oauth_client_secret_account(name),
                });
            }
        }
    }
    out
}

/// The keychain service, or why secrets can't be kept here (with the safe alternatives).
fn keychain_service(server: &str) -> Result<String, CliError> {
    setup::keychain_for_keys().map_err(|why| {
        CliError::new(format!(
            "MCP server `{server}`: {why}. Its secrets are kept only in the OS keychain, never in \
             a file: use a reference to an environment variable instead (--header 'NAME: \
             ${{env:VAR}}', --env KEY='${{env:VAR}}' or --bearer-env VAR)"
        ))
    })
}

/// Reads one value per item: from stdin lines (`--secret-stdin`), else hidden from the person.
fn read_secret_values(
    server: &str,
    items: &[SecretItem],
    from_stdin: bool,
) -> Result<Vec<String>, CliError> {
    if items.is_empty() {
        return Ok(Vec::new());
    }
    let values: Vec<String> = if from_stdin {
        let mut text = String::new();
        std::io::stdin()
            .read_to_string(&mut text)
            .map_err(|e| CliError::new(format!("read secrets from stdin: {e}")))?;
        let lines: Vec<String> = text
            .lines()
            .map(|l| l.trim().to_owned())
            .filter(|l| !l.is_empty())
            .collect();
        if lines.len() < items.len() {
            return Err(CliError::usage(format!(
                "--secret-stdin: expected {} line(s) on stdin ({}), got {}",
                items.len(),
                labels(items),
                lines.len()
            )));
        }
        lines.into_iter().take(items.len()).collect()
    } else {
        let mut ui = ui::detect(true);
        if !ui.interactive() {
            return Err(CliError::usage(format!(
                "MCP server `{server}` needs {} (kept in the OS keychain): run this in a terminal \
                 to type it hidden, or pass --secret-stdin and pipe it in",
                labels(items)
            )));
        }
        items
            .iter()
            .map(|item| ask_secret(ui.as_mut(), server, item))
            .collect::<Result<_, _>>()?
    };
    for (item, v) in items.iter().zip(&values) {
        if v.is_empty() {
            return Err(CliError::usage(format!(
                "{} for `{server}` is empty; nothing was stored",
                item.label
            )));
        }
    }
    Ok(values)
}

fn ask_secret(ui: &mut dyn Ui, server: &str, item: &SecretItem) -> Result<String, CliError> {
    let v = ui.secret(&format!(
        "{server}: {} (hidden; kept in the OS keychain)",
        item.label
    ))?;
    Ok(v.trim().to_owned())
}

fn labels(items: &[SecretItem]) -> String {
    items
        .iter()
        .map(|i| i.label.as_str())
        .collect::<Vec<_>>()
        .join(", ")
}

fn store_secrets(service: &str, items: &[SecretItem], values: &[String]) -> Result<(), CliError> {
    for (item, value) in items.iter().zip(values) {
        amp_mcp::keychain_set(service, &item.account, value)
            .map_err(|e| CliError::new(format!("keychain {}: {e}", item.account)))?;
    }
    Ok(())
}

/// Whether the item is stored: `Some(true/false)`, `None` when the keychain can't say.
fn stored(service: Option<&str>, item: &SecretItem) -> Option<bool> {
    let service = service?;
    amp_mcp::keychain_get(service, &item.account)
        .ok()
        .map(|v| v.is_some())
}

/// Deletes keychain items of `old` that nothing in the config uses any more.
fn forget_unused_secrets(ctx: &Ctx, old: &[SecretItem]) -> Vec<String> {
    if old.is_empty() || setup::credential_store_is_file() {
        return Vec::new();
    }
    let config = McpConfig::load(&ctx.paths);
    let used: BTreeSet<String> = config
        .servers
        .values()
        .flat_map(McpServer::keychain_accounts)
        .collect();
    let service = amp_mcp::keychain_service();
    let mut deleted = Vec::new();
    for item in old {
        if used.contains(&item.account) {
            continue;
        }
        if let Ok(true) = amp_mcp::keychain_delete(&service, &item.account) {
            deleted.push(item.label.clone());
        }
    }
    deleted
}

/// `${VAR}` / `${VAR:-default}` (Claude Code, Cursor) -> `${env:VAR}` / `${env:VAR:-default}`;
/// our own references and `$${` are left as they are.
fn env_refs(s: &str) -> String {
    const OURS: [&str; 4] = ["repo", "worktree", "home", "config_dir"];
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while let Some(i) = rest.find("${") {
        let escaped = rest[..i].ends_with('$');
        out.push_str(&rest[..i]);
        let after = &rest[i + 2..];
        let name_len = after
            .find(|c: char| !(c.is_ascii_alphanumeric() || c == '_'))
            .unwrap_or(after.len());
        let name = &after[..name_len];
        let next = after[name_len..].chars().next();
        let is_var = !escaped
            && !name.is_empty()
            && !name.starts_with(|c: char| c.is_ascii_digit())
            && !OURS.contains(&name)
            && matches!(next, Some('}') | Some(':'))
            && !(next == Some(':') && !after[name_len..].starts_with(":-"));
        if is_var {
            out.push_str("${env:");
            rest = after;
        } else {
            out.push_str("${");
            rest = after;
        }
    }
    out.push_str(rest);
    out
}

/// Refuses a literal `--header`/`--env` value that looks secret, naming the safe spellings.
fn refuse_secret(kind: &str, name: &str, value: &str) -> Result<(), CliError> {
    let literal = !value.contains("${");
    let secret = (mcp_config::looks_secret_name(name) && literal && !value.is_empty())
        || value
            .split("${")
            .next()
            .is_some_and(mcp_config::looks_secret_value);
    if !secret {
        return Ok(());
    }
    let (flag, reference) = match kind {
        "header" => (
            format!("--secret-header {name}"),
            format!("--header '{name}: Bearer ${{env:VAR}}' or --bearer-env VAR"),
        ),
        _ => (
            format!("--secret-env {name}"),
            format!("--env {name}='${{env:{name}}}'"),
        ),
    };
    Err(CliError::usage(format!(
        "the {kind} {name} looks secret, and command-line arguments are visible to other \
         processes while config.toml is plain text; nothing was written. Use {flag} (asked \
         hidden, kept in the OS keychain), or a reference: {reference}"
    )))
}

// ------------------------------------------------------------------------------------------------
// add / add-json
// ------------------------------------------------------------------------------------------------

/// A server to add: its table and the secrets to store (a value already known, or to ask).
struct NewServer {
    table: toml::Table,
    secrets: Vec<(SecretItem, Option<String>)>,
}

fn split_pair(s: &str, seps: &[char], what: &str) -> Result<(String, String), CliError> {
    let i = s
        .find(seps)
        .ok_or_else(|| CliError::usage(format!("{what} must look like {}", what_shape(what))))?;
    let (k, v) = (s[..i].trim(), s[i + 1..].trim());
    if k.is_empty() {
        return Err(CliError::usage(format!(
            "{what} must look like {}",
            what_shape(what)
        )));
    }
    Ok((k.to_owned(), v.to_owned()))
}

fn what_shape(what: &str) -> &'static str {
    if what == "--header" {
        "\"Name: value\" or Name=value"
    } else {
        "KEY=VALUE"
    }
}

fn str_list(items: &[String]) -> toml::Value {
    toml::Value::Array(
        items
            .iter()
            .map(|s| toml::Value::String(s.clone()))
            .collect(),
    )
}

fn str_map(map: &BTreeMap<String, String>) -> toml::Value {
    toml::Value::Table(
        map.iter()
            .map(|(k, v)| (k.clone(), toml::Value::String(v.clone())))
            .collect(),
    )
}

fn new_from_flags(a: &AddArgs) -> Result<NewServer, CliError> {
    let looks_url = |s: &str| s.starts_with("http://") || s.starts_with("https://");
    let positional_url = a.first.as_deref().is_some_and(looks_url);
    let target: Vec<String> = a.first.iter().chain(&a.rest).cloned().collect();
    let kind = match a.transport {
        Some(t) => t,
        None if a.url.is_some() || positional_url => TransportArg::Http,
        None => TransportArg::Stdio,
    };
    let mut t = toml::Table::new();
    let mut secrets = Vec::new();
    let name = &a.name;
    let set = |t: &mut toml::Table, k: &str, v: toml::Value| {
        t.insert(k.to_owned(), v);
    };
    let s = |v: &str| toml::Value::String(v.to_owned());
    match kind {
        TransportArg::Stdio => {
            if a.url.is_some() {
                return Err(CliError::usage(
                    "--url is for http/sse servers; a stdio server takes its command after `--`",
                ));
            }
            let (command, args) = target.split_first().ok_or_else(|| {
                CliError::usage(format!(
                    "give the command after `--` (`amp-harness mcp add {name} -- npx -y \
                     <package>`), or --url for an http/sse server"
                ))
            })?;
            set(&mut t, "type", s("stdio"));
            set(&mut t, "command", s(&env_refs(command)));
            if !args.is_empty() {
                let args: Vec<String> = args.iter().map(|x| env_refs(x)).collect();
                for x in &args {
                    if mcp_config::looks_secret_value(x.split("${").next().unwrap_or_default()) {
                        return Err(CliError::usage(
                            "an argument looks like a secret, and command-line arguments end up \
                             in config.toml; nothing was written. Pass it as --secret-env KEY (if \
                             the server reads it from the environment) or a reference such as \
                             '${env:VAR}'",
                        ));
                    }
                }
                set(&mut t, "args", str_list(&args));
            }
            let mut env_map = BTreeMap::new();
            for e in &a.env {
                let (k, v) = split_pair(e, &['='], "--env")?;
                let v = env_refs(&v);
                refuse_secret("env", &k, &v)?;
                env_map.insert(k, v);
            }
            if !env_map.is_empty() {
                set(&mut t, "env", str_map(&env_map));
            }
            if !a.secret_env.is_empty() {
                set(&mut t, "secret_env", str_list(&a.secret_env));
                for v in &a.secret_env {
                    secrets.push((
                        SecretItem {
                            label: format!("env {v}"),
                            which: v.clone(),
                            account: env_account(name, v),
                        },
                        None,
                    ));
                }
            }
            if let Some(f) = &a.env_file {
                set(&mut t, "env_file", s(f));
            }
            if let Some(d) = &a.workdir {
                set(&mut t, "cwd", s(d));
            }
            if a.per_worktree {
                set(&mut t, "scope", s("worktree"));
            }
            let remote_only = !a.headers.is_empty()
                || !a.secret_headers.is_empty()
                || a.bearer_env.is_some()
                || a.auth.is_some()
                || a.client_id.is_some()
                || a.client_secret
                || !a.oauth_scopes.is_empty()
                || a.callback_port.is_some()
                || a.no_sse_fallback;
            if remote_only {
                return Err(CliError::usage(
                    "--header, --secret-header, --bearer-env, --auth and the OAuth flags are for \
                     http/sse servers",
                ));
            }
        }
        TransportArg::Http | TransportArg::Sse => {
            let url = match (&a.url, target.as_slice()) {
                (Some(u), []) => u.clone(),
                (None, [u]) => u.clone(),
                (Some(_), _) => {
                    return Err(CliError::usage(
                        "give the URL once: --url URL or as the argument after the name",
                    ));
                }
                (None, []) => {
                    return Err(CliError::usage(format!(
                        "an {} server needs its URL: `amp-harness mcp add {name} --url URL`",
                        if kind == TransportArg::Sse {
                            "sse"
                        } else {
                            "http"
                        }
                    )));
                }
                (None, _) => {
                    return Err(CliError::usage(
                        "an http/sse server takes one URL, not a command (for a stdio server \
                         use --transport stdio or leave --transport out)",
                    ));
                }
            };
            set(
                &mut t,
                "type",
                s(if kind == TransportArg::Sse {
                    "sse"
                } else {
                    "http"
                }),
            );
            set(&mut t, "url", s(&url));
            let mut headers = BTreeMap::new();
            for h in &a.headers {
                let (k, v) = split_pair(h, &[':', '='], "--header")?;
                let v = env_refs(&v);
                refuse_secret("header", &k, &v)?;
                headers.insert(k, v);
            }
            if !headers.is_empty() {
                set(&mut t, "headers", str_map(&headers));
            }
            if !a.secret_headers.is_empty() {
                set(&mut t, "secret_headers", str_list(&a.secret_headers));
                for h in &a.secret_headers {
                    secrets.push((
                        SecretItem {
                            label: format!("header {h}"),
                            which: h.clone(),
                            account: header_account(name, h),
                        },
                        None,
                    ));
                }
            }
            if let Some(v) = &a.bearer_env {
                set(&mut t, "bearer_env", s(v));
            }
            if let Some(auth) = a.auth {
                let word = match auth {
                    AuthArg::Auto => "auto",
                    AuthArg::Oauth => "oauth",
                    AuthArg::None => "none",
                };
                set(&mut t, "auth", s(word));
            }
            let mut oauth_t = toml::Table::new();
            if let Some(id) = &a.client_id {
                oauth_t.insert("client_id".into(), s(id));
            }
            if !a.oauth_scopes.is_empty() {
                oauth_t.insert("scopes".into(), str_list(&a.oauth_scopes));
            }
            if let Some(p) = a.callback_port {
                oauth_t.insert("callback_port".into(), toml::Value::Integer(p.into()));
            }
            if a.client_secret {
                oauth_t.insert("client_secret".into(), toml::Value::Boolean(true));
                secrets.push((
                    SecretItem {
                        label: "OAuth client secret".to_owned(),
                        which: "oauth".to_owned(),
                        account: oauth_client_secret_account(name),
                    },
                    None,
                ));
            }
            if !oauth_t.is_empty() {
                set(&mut t, "oauth", toml::Value::Table(oauth_t));
            }
            if a.no_sse_fallback {
                set(&mut t, "fallback_sse", toml::Value::Boolean(false));
            }
            if !a.env.is_empty()
                || !a.secret_env.is_empty()
                || a.env_file.is_some()
                || a.workdir.is_some()
                || a.per_worktree
            {
                return Err(CliError::usage(
                    "--env, --secret-env, --env-file, --workdir and --per-worktree are for stdio \
                     servers",
                ));
            }
        }
    }
    common_fields(
        &mut t,
        &a.roles,
        a.startup,
        a.startup_timeout,
        a.timeout,
        a.disabled,
    );
    Ok(NewServer { table: t, secrets })
}

fn common_fields(
    t: &mut toml::Table,
    roles: &[String],
    startup: Option<StartupArg>,
    startup_timeout: Option<u64>,
    timeout: Option<u64>,
    disabled: bool,
) {
    if disabled {
        t.insert("enabled".into(), toml::Value::Boolean(false));
    }
    if !roles.is_empty() {
        t.insert("roles".into(), str_list(roles));
    }
    if let Some(st) = startup {
        let word = match st {
            StartupArg::Background => "background",
            StartupArg::Eager => "eager",
            StartupArg::Lazy => "lazy",
        };
        t.insert("startup".into(), toml::Value::String(word.into()));
    }
    if let Some(n) = startup_timeout {
        t.insert(
            "startup_timeout_secs".into(),
            toml::Value::Integer(n.min(i64::MAX as u64) as i64),
        );
    }
    if let Some(n) = timeout {
        t.insert(
            "call_timeout_secs".into(),
            toml::Value::Integer(n.min(i64::MAX as u64) as i64),
        );
    }
}

/// Claude Code / Cursor / VS Code server JSON -> our table. Literal secret-looking header and env
/// values are moved to the keychain (`secret_headers` / `secret_env`), never written.
fn new_from_json(name: &str, text: &str) -> Result<NewServer, CliError> {
    let v: Value = serde_json::from_str(text).map_err(|e| {
        CliError::usage(format!(
            "the server JSON does not parse (line {}, column {})",
            e.line(),
            e.column()
        ))
    })?;
    let obj = v
        .as_object()
        .ok_or_else(|| CliError::usage("the server JSON must be an object"))?;
    // A whole `{"mcpServers": {...}}` file is H10's `mcp import`.
    if obj.contains_key("mcpServers") || obj.contains_key("servers") {
        return Err(CliError::usage(
            "that is a whole config file; give one server's object (`{\"command\": …}` or \
             `{\"type\": \"http\", \"url\": …}`)",
        ));
    }
    let get_str = |k: &str| obj.get(k).and_then(Value::as_str).map(str::to_owned);
    let map_of = |k: &str| -> Result<BTreeMap<String, String>, CliError> {
        match obj.get(k) {
            None | Some(Value::Null) => Ok(BTreeMap::new()),
            Some(Value::Object(m)) => m
                .iter()
                .map(|(mk, mv)| match mv {
                    Value::String(s) => Ok((mk.clone(), s.clone())),
                    _ => Err(CliError::usage(format!("`{k}.{mk}` must be a string"))),
                })
                .collect(),
            Some(_) => Err(CliError::usage(format!("`{k}` must be an object"))),
        }
    };
    let kind = get_str("type").map(|t| t.to_ascii_lowercase());
    let kind = match (
        kind.as_deref(),
        obj.contains_key("command"),
        obj.contains_key("url"),
    ) {
        (Some("stdio" | "local"), ..) | (None, true, _) => "stdio",
        (Some("http" | "streamable-http" | "streamablehttp" | "remote"), ..)
        | (None, false, true) => "http",
        (Some("sse"), ..) => "sse",
        (Some(other), ..) => {
            return Err(CliError::usage(format!(
                "type `{other}` is not supported (stdio, http or sse)"
            )));
        }
        (None, false, false) => {
            return Err(CliError::usage(
                "the server JSON needs a `command` (stdio) or a `url` (http/sse)",
            ));
        }
    };
    let mut t = toml::Table::new();
    let mut secrets = Vec::new();
    t.insert("type".into(), toml::Value::String(kind.into()));
    let mut keep_or_move =
        |kind: &str, map: BTreeMap<String, String>| -> (BTreeMap<String, String>, Vec<String>) {
            let mut keep = BTreeMap::new();
            let mut moved = Vec::new();
            for (k, v) in map {
                let v = env_refs(&v);
                if refuse_secret(kind, &k, &v).is_err() {
                    let item = if kind == "header" {
                        SecretItem {
                            label: format!("header {k}"),
                            which: k.clone(),
                            account: header_account(name, &k),
                        }
                    } else {
                        SecretItem {
                            label: format!("env {k}"),
                            which: k.clone(),
                            account: env_account(name, &k),
                        }
                    };
                    secrets.push((item, Some(v)));
                    moved.push(k);
                } else {
                    keep.insert(k, v);
                }
            }
            (keep, moved)
        };
    if kind == "stdio" {
        // opencode's `command: ["npx", "-y", "pkg"]` too.
        let (command, mut args) = match obj.get("command") {
            Some(Value::String(c)) => (c.clone(), Vec::new()),
            Some(Value::Array(a)) => {
                let parts: Vec<String> = a
                    .iter()
                    .filter_map(|x| x.as_str().map(str::to_owned))
                    .collect();
                let (c, rest) = parts
                    .split_first()
                    .ok_or_else(|| CliError::usage("`command` is empty"))?;
                (c.clone(), rest.to_vec())
            }
            _ => return Err(CliError::usage("`command` must be a string")),
        };
        if let Some(Value::Array(a)) = obj.get("args") {
            for x in a {
                let x = x
                    .as_str()
                    .ok_or_else(|| CliError::usage("`args` must be strings"))?;
                args.push(x.to_owned());
            }
        }
        t.insert("command".into(), toml::Value::String(env_refs(&command)));
        if !args.is_empty() {
            let args: Vec<String> = args.iter().map(|x| env_refs(x)).collect();
            t.insert("args".into(), str_list(&args));
        }
        let mut env_map = map_of("env")?;
        env_map.extend(map_of("environment")?);
        let (keep, moved) = keep_or_move("env", env_map);
        if !keep.is_empty() {
            t.insert("env".into(), str_map(&keep));
        }
        if !moved.is_empty() {
            t.insert("secret_env".into(), str_list(&moved));
        }
        if let Some(cwd) = get_str("cwd") {
            t.insert("cwd".into(), toml::Value::String(cwd));
        }
        if let Some(f) = get_str("envFile") {
            t.insert("env_file".into(), toml::Value::String(f));
        }
    } else {
        let url = get_str("url").ok_or_else(|| CliError::usage("`url` is missing"))?;
        t.insert("url".into(), toml::Value::String(url));
        let (keep, moved) = keep_or_move("header", map_of("headers")?);
        if !keep.is_empty() {
            t.insert("headers".into(), str_map(&keep));
        }
        if !moved.is_empty() {
            t.insert("secret_headers".into(), str_list(&moved));
        }
        match obj.get("oauth") {
            Some(Value::Bool(false)) => {
                t.insert("auth".into(), toml::Value::String("none".into()));
            }
            Some(Value::Object(o)) => {
                let mut ot = toml::Table::new();
                if let Some(id) = o
                    .get("clientId")
                    .or_else(|| o.get("client_id"))
                    .and_then(Value::as_str)
                {
                    ot.insert("client_id".into(), toml::Value::String(id.into()));
                }
                if let Some(p) = o.get("callbackPort").and_then(Value::as_u64) {
                    ot.insert(
                        "callback_port".into(),
                        toml::Value::Integer(p.min(65535) as i64),
                    );
                }
                let scopes: Vec<String> = match o.get("scopes").or_else(|| o.get("scope")) {
                    Some(Value::Array(a)) => a
                        .iter()
                        .filter_map(|x| x.as_str().map(str::to_owned))
                        .collect(),
                    Some(Value::String(s)) => s.split_whitespace().map(str::to_owned).collect(),
                    _ => Vec::new(),
                };
                if !scopes.is_empty() {
                    ot.insert("scopes".into(), str_list(&scopes));
                }
                if let Some(u) = o.get("authServerMetadataUrl").and_then(Value::as_str) {
                    ot.insert(
                        "auth_server_metadata_url".into(),
                        toml::Value::String(u.into()),
                    );
                }
                if let Some(secret) = o
                    .get("clientSecret")
                    .or_else(|| o.get("client_secret"))
                    .and_then(Value::as_str)
                {
                    ot.insert("client_secret".into(), toml::Value::Boolean(true));
                    secrets.push((
                        SecretItem {
                            label: "OAuth client secret".to_owned(),
                            which: "oauth".to_owned(),
                            account: oauth_client_secret_account(name),
                        },
                        Some(secret.to_owned()),
                    ));
                }
                if !ot.is_empty() {
                    t.insert("oauth".into(), toml::Value::Table(ot));
                }
            }
            _ => {}
        }
    }
    if let Some(ms) = obj.get("timeout").and_then(Value::as_u64) {
        t.insert(
            "call_timeout_secs".into(),
            toml::Value::Integer(ms.div_ceil(1000).max(1) as i64),
        );
    }
    if obj.get("enabled").and_then(Value::as_bool) == Some(false)
        || obj.get("disabled").and_then(Value::as_bool) == Some(true)
    {
        t.insert("enabled".into(), toml::Value::Boolean(false));
    }
    Ok(NewServer { table: t, secrets })
}

#[derive(Serialize)]
struct AddReport {
    added: String,
    #[serde(rename = "type")]
    kind: &'static str,
    scope: &'static str,
    file: PathBuf,
    backup: Option<PathBuf>,
    replaced: bool,
    secrets_stored: Vec<String>,
    trusted: bool,
    test: Option<TestReport>,
}

pub async fn add(args: AddArgs) -> Result<(), CliError> {
    let spec = new_from_flags(&args)?;
    let scope = args.scope.get().unwrap_or(Scope::User);
    add_server(
        &args.name,
        scope,
        spec,
        AddOpts {
            force: args.force,
            no_test: args.no_test,
            secret_stdin: args.secret_stdin,
            json: args.json,
            note: "Added by `amp-harness mcp add`.",
        },
    )
    .await
}

pub async fn add_json(args: AddJsonArgs) -> Result<(), CliError> {
    let text = if args.json_config == "-" {
        let mut buf = String::new();
        std::io::stdin()
            .read_to_string(&mut buf)
            .map_err(|e| CliError::new(format!("read the server JSON from stdin: {e}")))?;
        buf
    } else {
        args.json_config.clone()
    };
    let spec = new_from_json(&args.name, &text)?;
    let scope = args.scope.get().unwrap_or(Scope::User);
    add_server(
        &args.name,
        scope,
        spec,
        AddOpts {
            force: args.force,
            no_test: args.no_test,
            secret_stdin: false,
            json: args.json,
            note: "Added by `amp-harness mcp add-json`.",
        },
    )
    .await
}

struct AddOpts {
    force: bool,
    no_test: bool,
    secret_stdin: bool,
    json: bool,
    note: &'static str,
}

async fn add_server(
    name: &str,
    scope: Scope,
    spec: NewServer,
    opts: AddOpts,
) -> Result<(), CliError> {
    mcp_config::check_server_name(name).map_err(|(message, fix)| {
        CliError::usage(match fix {
            Some(fix) => format!("{message} ({fix})"),
            None => message,
        })
    })?;
    let ctx = ctx()?;
    let file = config_file(&ctx, scope)?;
    let existing = layer_table(&ctx, scope, name)?;
    if existing.is_some() && !opts.force {
        return Err(CliError::usage(format!(
            "MCP server `{name}` already exists in {}; use --force to replace it, or `amp-harness \
             mcp edit {name}`",
            file.path.display()
        )));
    }
    let old_items: Vec<SecretItem> = existing
        .as_ref()
        .and_then(|v| parse_table(name, v, origin(&ctx, scope).ok()?).ok())
        .map(|(s, _)| keychain_items(&s))
        .unwrap_or_default();
    let value = toml::Value::Table(spec.table);
    let (server, warnings) = parse_table(name, &value, origin(&ctx, scope)?)?;
    let block = mcp_block(name, &value, Some(opts.note))?;
    let prefix = format!("mcp.{name}");
    let check = layer_check(scope, file.path.clone(), name.to_owned(), Expect::Present);
    let preview = preview_replace(&file, &prefix, Some(&block)).map_err(CliError::new)?;
    check(&preview).map_err(|e| {
        CliError::new(format!(
            "{}: the change would not load ({e}); nothing was written",
            file.path.display()
        ))
    })?;

    // Secrets: asked (or taken) and stored before the config names them.
    let (items, known): (Vec<SecretItem>, Vec<Option<String>>) = spec.secrets.into_iter().unzip();
    let mut stored_labels = Vec::new();
    if !items.is_empty() {
        let service = keychain_service(name)?;
        let ask: Vec<SecretItem> = items
            .iter()
            .zip(&known)
            .filter(|(_, v)| v.is_none())
            .map(|(i, _)| i.clone())
            .collect();
        let mut asked = read_secret_values(name, &ask, opts.secret_stdin)?.into_iter();
        let values: Vec<String> = known
            .into_iter()
            .map(|v| v.or_else(|| asked.next()).unwrap_or_default())
            .collect();
        store_secrets(&service, &items, &values)?;
        stored_labels = items.iter().map(|i| i.label.clone()).collect();
    }

    let backup = file
        .replace_tables(&prefix, Some(&block), &check)
        .map_err(CliError::new)?;
    let dropped: Vec<SecretItem> = old_items
        .into_iter()
        .filter(|o| !items.iter().any(|i| i.account == o.account))
        .collect();
    forget_unused_secrets(&ctx, &dropped);
    let trusted = if scope == Scope::Repo {
        Some(trust_now(&ctx, name)?)
    } else {
        None
    };
    let kind = server.transport.kind().as_str();
    if !opts.json {
        println!(
            "{} {kind} MCP server `{name}` in the {} config ({}).",
            if existing.is_some() {
                "Replaced"
            } else {
                "Added"
            },
            scope.name(),
            file.path.display()
        );
        for w in &warnings {
            eprintln!("  warning: {w}");
        }
        for (label, item) in stored_labels.iter().zip(&items) {
            println!(
                "  Stored {label} in the OS keychain (account {}).",
                item.account
            );
        }
        if let Some(path) = &trusted {
            println!(
                "  Trusted it for you as configured now (recorded in {}); others who clone the \
                 repo are asked before it runs.",
                path.display()
            );
        }
        if let Some(b) = &backup {
            println!("  Backup: {}", b.display());
        }
    }
    let test = if opts.no_test || !server.enabled {
        None
    } else {
        let report = test_server(&ctx, server.clone()).await?;
        if !opts.json {
            print_test(&report, "  Test: ");
        }
        Some(report)
    };
    if opts.json {
        return print_json(&AddReport {
            added: name.to_owned(),
            kind,
            scope: scope.name(),
            file: file.path.clone(),
            backup,
            replaced: existing.is_some(),
            secrets_stored: stored_labels,
            trusted: trusted.is_some(),
            test,
        });
    }
    Ok(())
}

// ------------------------------------------------------------------------------------------------
// list / get
// ------------------------------------------------------------------------------------------------

/// What a server runs or connects to, as written (no secrets: those are refused in these fields).
fn target(server: &McpServer) -> String {
    match &server.transport {
        Transport::Stdio(s) => std::iter::once(s.command.as_str())
            .chain(s.args.iter().map(String::as_str))
            .map(|a| {
                if a.contains(' ') || a.is_empty() {
                    format!("{a:?}")
                } else {
                    a.to_owned()
                }
            })
            .collect::<Vec<_>>()
            .join(" "),
        Transport::Http(r) | Transport::Sse(r) => r.url.clone(),
    }
}

fn status_text(status: &ServerStatus, tools: usize) -> String {
    match status {
        ServerStatus::Ready => format!("ready, {tools} tool{}", if tools == 1 { "" } else { "s" }),
        other => other.to_string(),
    }
}

#[derive(Serialize)]
struct ListRow {
    name: String,
    #[serde(rename = "type")]
    kind: Option<&'static str>,
    layer: Option<&'static str>,
    file: Option<PathBuf>,
    enabled: Option<bool>,
    trust: Option<TrustState>,
    target: Option<String>,
    /// The health (`--no-health`: absent), or `{"state": "broken"}` for a table with errors.
    status: Option<Value>,
    tools: Option<usize>,
    problems: Vec<String>,
}

pub async fn list(args: ListArgs) -> Result<(), CliError> {
    let ctx = ctx()?;
    let config = McpConfig::load(&ctx.paths);
    let store = trust_store();
    let health = if args.no_health || config.servers.is_empty() {
        None
    } else {
        if !args.json {
            eprintln!("Checking MCP server health…");
        }
        Some(probe(&ctx, config.servers.values().cloned().collect()).await?)
    };
    let mut rows: Vec<ListRow> = Vec::new();
    for s in config.servers.values() {
        let h = health.as_ref().and_then(|p| p.health.get(&s.name));
        let status = match (h, s.enabled) {
            (_, false) => Some(json!({"state": "disabled"})),
            (Some(h), _) => serde_json::to_value(&h.status).ok(),
            (None, _) => None,
        };
        rows.push(ListRow {
            name: s.name.clone(),
            kind: Some(s.transport.kind().as_str()),
            layer: Some(s.origin.layer.as_str()),
            file: Some(s.origin.file.clone()),
            enabled: Some(s.enabled),
            trust: s.is_repo().then(|| trust_of(s, store.as_ref())),
            target: Some(target(s)),
            status,
            tools: h.map(|h| h.tools),
            problems: Vec::new(),
        });
    }
    for name in &config.broken {
        let problems: Vec<String> = config
            .problems
            .iter()
            .filter(|p| {
                p.severity == Severity::Error
                    && (p.key == format!("mcp.{name}")
                        || p.key.starts_with(&format!("mcp.{name}.")))
            })
            .map(|p| p.to_string())
            .collect();
        rows.push(ListRow {
            name: name.clone(),
            kind: None,
            layer: None,
            file: None,
            enabled: None,
            trust: None,
            target: None,
            status: Some(json!({"state": "broken"})),
            tools: None,
            problems,
        });
    }
    rows.sort_by(|a, b| a.name.cmp(&b.name));
    if args.json {
        return print_json(&rows);
    }
    if rows.is_empty() {
        println!(
            "No MCP servers configured. Add one with `amp-harness mcp add <name> -- <command>` or \
             `amp-harness mcp add <name> --url <url>`."
        );
        return Ok(());
    }
    for r in &rows {
        if !r.problems.is_empty() || r.kind.is_none() {
            println!("{}: config error", r.name);
            for p in &r.problems {
                println!("  {p}");
            }
            continue;
        }
        let s = config.servers.get(&r.name).expect("listed");
        let state = if !s.enabled {
            "disabled".to_owned()
        } else if let Some(h) = health.as_ref().and_then(|p| p.health.get(&s.name)) {
            status_text(&h.status, h.tools)
        } else if let Some(t) = &r.trust
            && !t.allows()
        {
            format!(
                "{} (`amp-harness mcp trust {}` to allow it)",
                t.word(),
                s.name
            )
        } else {
            "not checked".to_owned()
        };
        println!(
            "{} ({}, {}): {} - {state}",
            r.name,
            r.kind.unwrap_or_default(),
            r.layer.unwrap_or_default(),
            r.target.as_deref().unwrap_or_default()
        );
    }
    Ok(())
}

pub async fn get(args: GetArgs) -> Result<(), CliError> {
    let ctx = ctx()?;
    let config = McpConfig::load(&ctx.paths);
    let server = match args.scope.get() {
        Some(scope) => {
            let (scope, value) = pick_scope(&ctx, &args.name, Some(scope), "show")?;
            if is_switch(&value) {
                return Err(CliError::usage(format!(
                    "the {} config only switches `{}` on or off (enabled = {})",
                    scope.name(),
                    args.name,
                    value
                        .get("enabled")
                        .map(|v| v.to_string())
                        .unwrap_or_default()
                )));
            }
            parse_table(&args.name, &value, origin(&ctx, scope)?)?.0
        }
        None => match config.servers.get(&args.name) {
            Some(s) => s.clone(),
            None if config.broken.contains(&args.name) => {
                let problems: Vec<String> = config
                    .errors()
                    .filter(|p| p.key.starts_with(&format!("mcp.{}", args.name)))
                    .map(|p| format!("  {p}"))
                    .collect();
                return Err(CliError::new(format!(
                    "MCP server `{}` has configuration errors:\n{}",
                    args.name,
                    problems.join("\n")
                )));
            }
            None => {
                return Err(CliError::usage(format!(
                    "no MCP server `{}` (`amp-harness mcp list` shows them)",
                    args.name
                )));
            }
        },
    };
    let store = trust_store();
    let shadowed = server.origin.layer == Layer::User
        && config
            .servers
            .get(&server.name)
            .is_some_and(|s| s.is_repo());
    let service = setup::keychain_for_keys().ok();
    let secrets: Vec<Value> = secret_rows(&server, service.as_deref());
    let login = match server.transport.remote() {
        Some(r) if r.auth != AuthMode::None => match CredentialBackend::resolve() {
            Ok(backend) => {
                let url = expand_url(&ctx, &server).unwrap_or_else(|| r.url.clone());
                Some(oauth::has_login(&url, &backend).await.unwrap_or(false))
            }
            Err(_) => None,
        },
        _ => None,
    };
    let health = if args.no_health || !server.enabled {
        None
    } else {
        Some(test_server(&ctx, server.clone()).await?)
    };
    let mut view = server_json(&server);
    let obj = view.as_object_mut().expect("an object");
    obj.insert("secrets".into(), Value::Array(secrets.clone()));
    obj.insert(
        "trust".into(),
        json!(server.is_repo().then(|| trust_of(&server, store.as_ref()))),
    );
    obj.insert("login".into(), json!(login));
    obj.insert("shadowed_by_repo".into(), json!(shadowed));
    if let Some(h) = &health {
        obj.insert("status".into(), serde_json::to_value(h).unwrap_or_default());
    }
    if args.json {
        return print_json(&view);
    }
    print_server(&server, &secrets, login, store.as_ref());
    if shadowed {
        println!(
            "  note:     the repo config defines `{}` too, and wins",
            server.name
        );
    }
    if let Some(h) = &health {
        print_test(h, "  status:   ");
    }
    Ok(())
}

/// The server's URL with references expanded (for its login key).
fn expand_url(ctx: &Ctx, server: &McpServer) -> Option<String> {
    let expand = ExpandContext::from_paths(&ctx.paths, &env, None);
    match server.resolve(&expand).ok()? {
        Resolved::Http(r) | Resolved::Sse(r) => Some(r.url),
        Resolved::Stdio(_) => None,
    }
}

fn secret_rows(server: &McpServer, service: Option<&str>) -> Vec<Value> {
    let mut rows: Vec<Value> = keychain_items(server)
        .iter()
        .map(|item| {
            let state = match (service, stored(service, item)) {
                (None, _) => "keychain off",
                (_, Some(true)) => "stored",
                (_, Some(false)) => "missing",
                (_, None) => "unreadable",
            };
            json!({"secret": item.label, "source": format!("keychain {}", item.account), "state": state})
        })
        .collect();
    if let Some(r) = server.transport.remote()
        && let Some(var) = &r.bearer_env
    {
        let set = std::env::var(var).is_ok_and(|v| !v.is_empty());
        rows.push(json!({"secret": "bearer token", "source": format!("env {var}"), "state": if set { "set" } else { "not set" }}));
    }
    rows
}

fn server_json(s: &McpServer) -> Value {
    let mut v = json!({
        "name": s.name,
        "type": s.transport.kind().as_str(),
        "layer": s.origin.layer.as_str(),
        "file": s.origin.file,
        "enabled": s.enabled,
        "enabled_from": s.enabled_origin.file,
        "roles": s.roles,
        "startup": match s.startup { Startup::Background => "background", Startup::Eager => "eager", Startup::Lazy => "lazy" },
        "startup_timeout_secs": s.startup_timeout.as_secs(),
        "call_timeout_secs": s.call_timeout.as_secs(),
        "max_output_tokens": s.max_output_tokens,
        "instructions": s.instructions,
        "scope": match s.scope { RunScope::Shared => "shared", RunScope::Worktree => "worktree" },
    });
    let obj = v.as_object_mut().expect("an object");
    match &s.transport {
        Transport::Stdio(st) => {
            obj.insert("command".into(), json!(st.command));
            obj.insert("args".into(), json!(st.args));
            obj.insert("env".into(), json!(st.env));
            obj.insert("secret_env".into(), json!(st.secret_env));
            obj.insert("env_file".into(), json!(st.env_file));
            obj.insert("cwd".into(), json!(st.cwd));
        }
        Transport::Http(r) | Transport::Sse(r) => {
            obj.insert("url".into(), json!(r.url));
            obj.insert("headers".into(), json!(r.headers));
            obj.insert("secret_headers".into(), json!(r.secret_headers));
            obj.insert("bearer_env".into(), json!(r.bearer_env));
            obj.insert(
                "auth".into(),
                json!(match r.auth {
                    AuthMode::Auto => "auto",
                    AuthMode::Oauth => "oauth",
                    AuthMode::None => "none",
                }),
            );
            obj.insert(
                "oauth".into(),
                json!(r.oauth.as_ref().map(|o| json!({
                    "client_id": o.client_id,
                    "scopes": o.scopes,
                    "callback_port": o.callback_port,
                    "client_secret": o.client_secret,
                }))),
            );
            obj.insert("fallback_sse".into(), json!(r.fallback_sse));
        }
    }
    v
}

fn print_server(s: &McpServer, secrets: &[Value], login: Option<bool>, store: Option<&TrustStore>) {
    println!("{}:", s.name);
    println!(
        "  scope:    {} ({})",
        s.origin.layer.as_str(),
        s.origin.file.display()
    );
    println!("  type:     {}", s.transport.kind().as_str());
    match &s.transport {
        Transport::Stdio(st) => {
            println!("  command:  {}", target(s));
            for (k, v) in &st.env {
                println!("  env:      {k}={v}");
            }
            if let Some(f) = &st.env_file {
                println!("  env file: {f}");
            }
            if let Some(c) = &st.cwd {
                println!("  cwd:      {c}");
            }
        }
        Transport::Http(r) | Transport::Sse(r) => {
            println!("  url:      {}", r.url);
            for (k, v) in &r.headers {
                println!("  header:   {k}: {v}");
            }
            println!(
                "  auth:     {}{}",
                match r.auth {
                    AuthMode::Auto => "auto (OAuth when the server asks)",
                    AuthMode::Oauth => "oauth",
                    AuthMode::None => "none",
                },
                match login {
                    Some(true) => "; logged in",
                    Some(false) if r.auth != AuthMode::None => "; not logged in",
                    _ => "",
                }
            );
        }
    }
    for row in secrets {
        println!(
            "  secret:   {}: {} ({})",
            row["secret"].as_str().unwrap_or_default(),
            row["source"].as_str().unwrap_or_default(),
            row["state"].as_str().unwrap_or_default()
        );
    }
    println!(
        "  enabled:  {}{}",
        if s.enabled { "yes" } else { "no" },
        if s.enabled_origin.file != s.origin.file {
            format!(" (set in {})", s.enabled_origin.file.display())
        } else {
            String::new()
        }
    );
    if s.is_repo() {
        let t = trust_of(s, store);
        println!(
            "  trust:    {}{}",
            t.word(),
            if t.allows() {
                String::new()
            } else {
                format!(" (`amp-harness mcp trust {}` to allow it)", s.name)
            }
        );
    }
    println!("  roles:    {}", s.roles.join(", "));
    println!(
        "  startup:  {} (waits {} s); tool calls time out after {} s",
        match s.startup {
            Startup::Background => "background",
            Startup::Eager => "eager",
            Startup::Lazy => "lazy",
        },
        s.startup_timeout.as_secs(),
        s.call_timeout.as_secs()
    );
    if s.scope == RunScope::Worktree {
        println!("  instance: one per worker worktree");
    }
}

// ------------------------------------------------------------------------------------------------
// test
// ------------------------------------------------------------------------------------------------

#[derive(Serialize, Clone)]
pub struct TestReport {
    name: String,
    ok: bool,
    #[serde(rename = "type")]
    kind: &'static str,
    status: ServerStatus,
    elapsed_ms: u64,
    tools: Vec<String>,
    server: Option<String>,
    protocol: Option<String>,
    stderr_log: Option<PathBuf>,
    /// The last lines the server wrote to stderr, when it is not ready.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    stderr_tail: Vec<String>,
}

/// Connects to `server` through a one-server hub (enabled for the test, roles opened so its tools
/// can be listed) and reports what happened.
async fn test_server(ctx: &Ctx, mut server: McpServer) -> Result<TestReport, CliError> {
    server.enabled = true;
    server.roles = vec!["*".to_owned()];
    let name = server.name.clone();
    let kind = server.transport.kind().as_str();
    let probe = probe(ctx, vec![server]).await?;
    let h = probe.health.get(&name);
    let status = h
        .map(|h| h.status.clone())
        .unwrap_or(ServerStatus::Stopped {
            reason: "the hub did not start it".to_owned(),
        });
    let ok = status == ServerStatus::Ready;
    let stderr_log = h.and_then(|h| h.stderr_log.clone());
    Ok(TestReport {
        name: name.clone(),
        ok,
        kind,
        elapsed_ms: probe.elapsed.as_millis().min(u64::MAX as u128) as u64,
        tools: probe.tools.get(&name).cloned().unwrap_or_default(),
        server: h.and_then(|h| h.server.clone()),
        protocol: h.and_then(|h| h.protocol.clone()),
        stderr_tail: if ok {
            Vec::new()
        } else {
            stderr_tail(stderr_log.as_deref(), 10)
        },
        stderr_log,
        status,
    })
}

fn print_test(r: &TestReport, prefix: &str) {
    if r.ok {
        let mut line = format!(
            "{prefix}ready in {:.1} s, {} tool{}",
            r.elapsed_ms as f64 / 1000.0,
            r.tools.len(),
            if r.tools.len() == 1 { "" } else { "s" }
        );
        if !r.tools.is_empty() {
            line.push_str(&format!(": {}", r.tools.join(", ")));
        }
        println!("{line}");
        let mut about = Vec::new();
        if let Some(s) = &r.server {
            about.push(format!("server {s}"));
        }
        if let Some(p) = &r.protocol {
            about.push(format!("protocol {p}"));
        }
        if !about.is_empty() {
            println!("{}{}", " ".repeat(prefix.len()), about.join(", "));
        }
    } else {
        println!("{prefix}{}", r.status);
        if !r.stderr_tail.is_empty() {
            println!(
                "{}last stderr lines ({}):",
                " ".repeat(prefix.len()),
                r.stderr_log
                    .as_deref()
                    .map(|p| p.display().to_string())
                    .unwrap_or_default()
            );
            for l in &r.stderr_tail {
                println!("{}  {l}", " ".repeat(prefix.len()));
            }
        }
    }
}

pub async fn test(args: TestArgs) -> Result<(), CliError> {
    let ctx = ctx()?;
    let server = effective(&ctx, &args.name)?;
    if !args.json {
        eprintln!(
            "Connecting to `{}` ({})…",
            server.name,
            server.transport.kind().as_str()
        );
    }
    let report = test_server(&ctx, server).await?;
    let ok = report.ok;
    if args.json {
        print_json(&report)?;
    } else {
        print_test(&report, &format!("{}: ", report.name));
    }
    if ok { Ok(()) } else { Err(CliError::silent(1)) }
}

/// The server in effect called `name`, or why there is none.
fn effective(ctx: &Ctx, name: &str) -> Result<McpServer, CliError> {
    let config = McpConfig::load(&ctx.paths);
    match config.servers.get(name) {
        Some(s) => Ok(s.clone()),
        None if config.broken.contains(name) => Err(CliError::new(format!(
            "MCP server `{name}` has configuration errors (`amp-harness mcp get {name}` or \
             `amp-harness config check` lists them)"
        ))),
        None => Err(CliError::usage(format!(
            "no MCP server `{name}` (`amp-harness mcp list` shows them)"
        ))),
    }
}

// ------------------------------------------------------------------------------------------------
// edit / remove / enable / disable / secret
// ------------------------------------------------------------------------------------------------

pub async fn edit(args: EditArgs) -> Result<(), CliError> {
    let ctx = ctx()?;
    let name = args.name.clone();
    let (scope, value) = pick_scope(&ctx, &name, args.scope.get(), "edit")?;
    if is_switch(&value) {
        return Err(CliError::usage(format!(
            "the {} config only switches `{name}` on or off; use `amp-harness mcp enable|disable \
             {name}`, or edit the server where it is defined",
            scope.name()
        )));
    }
    let file = config_file(&ctx, scope)?;
    let org = origin(&ctx, scope)?;
    let old_items = parse_table(&name, &value, org.clone())
        .map(|(s, _)| keychain_items(&s))
        .unwrap_or_default();
    let prefix = format!("mcp.{name}");
    let text = file.text().map_err(CliError::new)?.unwrap_or_default();
    let current = table_text(&text, &prefix).ok_or_else(|| {
        CliError::usage(format!(
            "`mcp.{name}` in {} is written inline under [mcp]; edit that file by hand",
            file.path.display()
        ))
    })?;
    let check = layer_check(scope, file.path.clone(), name.clone(), Expect::Present);
    let block = match &args.roles {
        Some(roles) => {
            // Rewrite just this key, keeping everything else (and comments) as written.
            let edited = set_raw(&current, &prefix, "roles", &str_list(roles).to_string());
            let parsed: toml::Table = toml::from_str(&edited).map_err(|_| {
                CliError::new(format!(
                    "could not set roles in {} without breaking it; use `amp-harness mcp edit \
                     {name}` (no --roles) or edit it by hand",
                    file.path.display()
                ))
            })?;
            validate_edited(&name, &parsed, org.clone()).map_err(CliError::usage)?;
            edited
        }
        None => {
            let dir = ctx.scope_dir(scope)?.join(EDIT_DIR);
            let scratch = dir.join(format!("mcp-{name}.toml"));
            let validate = |t: &str| -> Result<(), String> {
                let parsed: toml::Table =
                    toml::from_str(t).map_err(|e| format!("does not parse: {}", e.message()))?;
                validate_edited(&name, &parsed, org.clone())?;
                check(&preview_replace(&file, &prefix, Some(t))?)
            };
            match agents_cmd::edit_loop(
                &agents_cmd::editor_command(),
                &scratch,
                &current,
                &validate,
                &agents_cmd::ask_again,
            )? {
                Some(t) => t,
                None => {
                    if args.json {
                        return print_json(&json!({"edited": name, "changed": false}));
                    }
                    println!("No changes to `{name}`.");
                    return Ok(());
                }
            }
        }
    };
    let parsed: toml::Table =
        toml::from_str(&block).map_err(|e| CliError::new(e.message().to_owned()))?;
    let new_value = parsed
        .get("mcp")
        .and_then(|m| m.get(&name))
        .cloned()
        .ok_or_else(|| CliError::new(format!("`mcp.{name}` is missing")))?;
    let (server, _) = parse_table(&name, &new_value, org)?;
    // Secrets the edit adds are asked now (they must exist before anything uses them).
    let new_items: Vec<SecretItem> = keychain_items(&server)
        .into_iter()
        .filter(|i| !old_items.iter().any(|o| o.account == i.account))
        .collect();
    let mut stored_labels = Vec::new();
    if !new_items.is_empty() {
        let service = keychain_service(&name)?;
        let values = read_secret_values(&name, &new_items, args.secret_stdin)?;
        store_secrets(&service, &new_items, &values)?;
        stored_labels = new_items.iter().map(|i| i.label.clone()).collect();
    }
    let backup = file
        .replace_tables(&prefix, Some(&block), &check)
        .map_err(CliError::new)?;
    let dropped: Vec<SecretItem> = old_items
        .into_iter()
        .filter(|o| {
            !keychain_items(&server)
                .iter()
                .any(|i| i.account == o.account)
        })
        .collect();
    let forgotten = forget_unused_secrets(&ctx, &dropped);
    let trusted = if scope == Scope::Repo {
        Some(trust_now(&ctx, &name)?)
    } else {
        None
    };
    if args.json {
        return print_json(&json!({
            "edited": name, "changed": true, "scope": scope.name(), "file": file.path,
            "backup": backup, "secrets_stored": stored_labels, "secrets_removed": forgotten,
            "trusted": trusted.is_some(),
        }));
    }
    println!(
        "Saved `{name}` in the {} config ({}).",
        scope.name(),
        file.path.display()
    );
    for l in &stored_labels {
        println!("  Stored {l} in the OS keychain.");
    }
    for l in &forgotten {
        println!("  Removed {l} from the OS keychain (no longer used).");
    }
    if let Some(p) = trusted {
        println!(
            "  Trusted the new version for you (recorded in {}).",
            p.display()
        );
    }
    Ok(())
}

/// An edited block must hold exactly `[mcp.<name>]` (renaming is `add` + `remove`) and load.
fn validate_edited(name: &str, parsed: &toml::Table, origin: Origin) -> Result<(), String> {
    let keys: Vec<&String> = parsed.keys().collect();
    let mcp = match (keys.as_slice(), parsed.get("mcp")) {
        ([k], Some(toml::Value::Table(m))) if *k == "mcp" => m,
        _ => {
            return Err(format!(
                "keep only the [mcp.{name}] table (and its sub-tables)"
            ));
        }
    };
    let value = match (mcp.len(), mcp.get(name)) {
        (1, Some(v)) => v,
        _ => {
            return Err(format!(
                "keep the server named `{name}` (to rename it, add the new name and remove the old)"
            ));
        }
    };
    mcp_config::parse_server(name, value, origin)
        .map(|_| ())
        .map_err(|problems| {
            problems
                .iter()
                .filter(|p| p.severity == Severity::Error)
                .map(|p| {
                    format!(
                        "`{}`: {}{}",
                        p.key,
                        p.message,
                        p.fix
                            .as_deref()
                            .map(|f| format!(" (fix: {f})"))
                            .unwrap_or_default()
                    )
                })
                .collect::<Vec<_>>()
                .join("; ")
        })
}

pub async fn remove(args: RemoveArgs) -> Result<(), CliError> {
    let ctx = ctx()?;
    let name = args.name.clone();
    let (scope, value) = pick_scope(&ctx, &name, args.scope.get(), "remove")?;
    let file = config_file(&ctx, scope)?;
    let removed = parse_table(&name, &value, origin(&ctx, scope)?)
        .ok()
        .map(|(s, _)| s);
    let login_url = removed
        .as_ref()
        .filter(|s| s.transport.remote().is_some())
        .and_then(|s| expand_url(&ctx, s));
    let prefix = format!("mcp.{name}");
    let check = layer_check(scope, file.path.clone(), name.clone(), Expect::Absent);
    let backup = file
        .replace_tables(&prefix, None, &check)
        .map_err(CliError::new)?;
    let forgotten = match (&removed, args.keep_secrets) {
        (Some(s), false) => forget_unused_secrets(&ctx, &keychain_items(s)),
        _ => Vec::new(),
    };
    if scope == Scope::Repo {
        untrust_now(&ctx, &name);
    }
    let mut logged_out = false;
    if let Some(url) = login_url.filter(|_| !args.keep_login) {
        let key = credential_key(&url);
        let config = McpConfig::load(&ctx.paths);
        let still_used = config
            .servers
            .values()
            .filter_map(|s| expand_url(&ctx, s))
            .any(|u| credential_key(&u) == key);
        let amp = crate::conn::ConnArgs::default()
            .settings()
            .map(|s| credential_key(&s.url))
            .ok();
        if !still_used
            && amp.as_deref() != Some(key.as_str())
            && let Ok(backend) = CredentialBackend::resolve()
        {
            logged_out = oauth::logout(&url, &backend).await.unwrap_or(false);
        }
    }
    let effective_now = McpConfig::load(&ctx.paths).servers.get(&name).cloned();
    if args.json {
        return print_json(&json!({
            "removed": name, "scope": scope.name(), "file": file.path, "backup": backup,
            "secrets_removed": forgotten, "logged_out": logged_out,
            "still_defined_in": effective_now.as_ref().map(|s| s.origin.layer.as_str()),
        }));
    }
    println!(
        "Removed MCP server `{name}` from the {} config ({}).",
        scope.name(),
        file.path.display()
    );
    for l in &forgotten {
        println!("  Removed {l} from the OS keychain.");
    }
    if logged_out {
        println!("  Removed its stored OAuth login.");
    }
    if let Some(s) = effective_now {
        println!(
            "  `{name}` is still defined in the {} config ({}).",
            s.origin.layer.as_str(),
            s.origin.file.display()
        );
    }
    Ok(())
}

pub async fn set_enabled(args: NameArgs, on: bool) -> Result<(), CliError> {
    let ctx = ctx()?;
    let name = args.name.clone();
    let server = effective(&ctx, &name)?;
    let user = ConfigFile::new(ctx.scope_config_file(Scope::User)?);
    let section = format!("mcp.{name}");
    let user_table = layer_table(&ctx, Scope::User, &name)?;
    let changes: Vec<Change> = match (&server.origin.layer, &user_table) {
        // A repo server whose user switch only turned it off: drop the switch, unless the repo
        // table itself says enabled = false.
        (Layer::Repo, Some(v))
            if on
                && is_switch(v)
                && layer_table(&ctx, Scope::Repo, &name)?
                    .and_then(|r| r.get("enabled").and_then(toml::Value::as_bool))
                    != Some(false) =>
        {
            vec![Change::DropTables(section.clone())]
        }
        _ => vec![Change::set_value(
            &section,
            "enabled",
            &toml::Value::Boolean(on),
        )],
    };
    if server.enabled == on {
        if args.json {
            return print_json(&json!({"name": name, "enabled": on, "changed": false}));
        }
        println!(
            "`{name}` is already {}.",
            if on { "enabled" } else { "disabled" }
        );
        return Ok(());
    }
    let preview = user.preview(&changes).map_err(CliError::new)?;
    let check = {
        let repo = ctx.paths.repo_file();
        let user_path = user.path.clone();
        let name = name.clone();
        move |text: &str| -> Result<(), String> {
            let mcp = layer_mcp(text, &user_path)?;
            let repo_mcp = match repo.as_deref() {
                Some(f) => RepoConfig::load(f)
                    .map_err(|e| e.to_string())?
                    .map(|r| r.mcp),
                None => None,
            };
            let config = McpConfig::from_tables(
                Some((&mcp, user_path.as_path())),
                repo_mcp.as_ref().zip(repo.as_deref()),
            );
            match config.servers.get(&name) {
                Some(s) if s.enabled == on => Ok(()),
                Some(_) => Err(format!(
                    "`{name}` would still be {}",
                    if on { "disabled" } else { "enabled" }
                )),
                None => Err(format!("`{name}` would not load")),
            }
        }
    };
    check(&preview).map_err(|e| {
        CliError::new(format!(
            "{}: {e}; nothing was changed, edit it by hand",
            user.path.display()
        ))
    })?;
    user.edit(&changes).map_err(CliError::new)?;
    if args.json {
        return print_json(&json!({
            "name": name, "enabled": on, "changed": true, "file": user.path,
            "layer": server.origin.layer.as_str(),
        }));
    }
    let what = if on { "Enabled" } else { "Disabled" };
    if server.is_repo() {
        println!(
            "{what} repo MCP server `{name}` for you ({}); the repo's file is unchanged.",
            user.path.display()
        );
    } else {
        println!("{what} MCP server `{name}` ({}).", user.path.display());
    }
    Ok(())
}

pub async fn secret(args: SecretArgs) -> Result<(), CliError> {
    let ctx = ctx()?;
    let server = effective(&ctx, &args.name)?;
    let all = keychain_items(&server);
    if all.is_empty() {
        return Err(CliError::usage(format!(
            "MCP server `{}` keeps no secrets in the keychain (add one with `amp-harness mcp edit \
             {}`: secret_headers, secret_env or oauth.client_secret)",
            server.name, server.name
        )));
    }
    let mut items: Vec<SecretItem> = if args.which.is_empty() {
        all.clone()
    } else {
        let mut picked = Vec::new();
        for w in &args.which {
            let item = all
                .iter()
                .find(|i| i.which.eq_ignore_ascii_case(w))
                .ok_or_else(|| {
                    CliError::usage(format!(
                        "`{}` keeps no secret `{w}`; it keeps: {}",
                        server.name,
                        all.iter()
                            .map(|i| i.which.as_str())
                            .collect::<Vec<_>>()
                            .join(", ")
                    ))
                })?;
            picked.push(item.clone());
        }
        picked
    };
    let service = keychain_service(&server.name)?;
    if args.missing {
        items.retain(|i| stored(Some(&service), i) != Some(true));
        if items.is_empty() {
            if args.json {
                return print_json(&json!({"name": server.name, "stored": []}));
            }
            println!("Every secret of `{}` is stored.", server.name);
            return Ok(());
        }
    }
    let values = read_secret_values(&server.name, &items, args.secret_stdin)?;
    store_secrets(&service, &items, &values)?;
    let labels: Vec<&str> = items.iter().map(|i| i.label.as_str()).collect();
    if args.json {
        return print_json(&json!({"name": server.name, "stored": labels}));
    }
    for l in labels {
        println!("Stored {l} for `{}` in the OS keychain.", server.name);
    }
    Ok(())
}

// ------------------------------------------------------------------------------------------------
// login / logout
// ------------------------------------------------------------------------------------------------

/// The remote server `name` with its connect options (secrets fetched), refusing stdio servers and
/// untrusted repo ones.
fn remote_options(
    ctx: &Ctx,
    name: &str,
    verb: &str,
) -> Result<(McpServer, amp_mcp::HttpServerOptions), CliError> {
    let server = effective(ctx, name)?;
    if server.transport.remote().is_none() {
        return Err(CliError::usage(format!(
            "`{name}` is a stdio server: it has no {verb}. Its secrets are environment variables \
             (`amp-harness mcp secret {name}`, or --env KEY='${{env:VAR}}')"
        )));
    }
    let trust = trust_of(&server, trust_store().as_ref());
    if !trust.allows() {
        return Err(CliError::usage(format!(
            "repo MCP server `{name}` is not trusted ({}); trust it first with `amp-harness mcp \
             trust {name}`",
            trust.word()
        )));
    }
    let expand = ExpandContext::from_paths(&ctx.paths, &env, None);
    let resolved = server
        .resolve(&expand)
        .map_err(|p| CliError::new(p.to_string()))?;
    let (Resolved::Http(r) | Resolved::Sse(r)) = resolved else {
        unreachable!("checked: a remote server");
    };
    let keychain = (!setup::credential_store_is_file()).then(amp_mcp::keychain_service);
    let options =
        mcp_http::http_options(&server, &r, keychain.as_deref(), &env).map_err(CliError::new)?;
    Ok((server, options))
}

pub async fn login(args: LoginArgs) -> Result<(), CliError> {
    let ctx = ctx()?;
    let (server, options) = remote_options(&ctx, &args.name, "login")?;
    let backend = CredentialBackend::resolve().map_err(|e| CliError::new(e.to_string()))?;
    let no_browser = args.no_browser;
    if !args.json {
        eprintln!(
            "Logging in to MCP server `{}` ({}) …",
            server.name, options.url
        );
    }
    let outcome = http::login(
        &options,
        backend,
        Duration::from_secs(args.timeout),
        |prompt: &oauth::LoginPrompt| {
            eprintln!(
                "Waiting for the browser on {} (Ctrl-C to cancel).",
                prompt.redirect_uri
            );
            let opened = !no_browser && oauth::open_browser(&prompt.authorization_url).is_ok();
            if opened {
                eprintln!("Opened your browser. If nothing happened, open this URL:");
            } else {
                eprintln!("Open this URL in a browser to log in:");
            }
            eprintln!("\n  {}\n", prompt.authorization_url);
        },
    )
    .await
    .map_err(|e| CliError::new(e.to_string()))?;
    if args.json {
        return print_json(&json!({
            "name": server.name, "logged_in": outcome.key, "client_id": outcome.client_id,
            "scopes": outcome.scopes, "refresh": outcome.has_refresh_token,
            "stored_in": outcome.stored_in,
        }));
    }
    println!("Logged in to `{}` ({})", server.name, outcome.key);
    if !outcome.scopes.is_empty() {
        println!("  scopes:  {}", outcome.scopes.join(" "));
    }
    println!("  stored:  {}", outcome.stored_in);
    Ok(())
}

pub async fn logout(args: NameArgs) -> Result<(), CliError> {
    let ctx = ctx()?;
    let server = effective(&ctx, &args.name)?;
    if server.transport.remote().is_none() {
        return Err(CliError::usage(format!(
            "`{}` is a stdio server: it has no login",
            server.name
        )));
    }
    let url = expand_url(&ctx, &server).ok_or_else(|| {
        CliError::new(format!(
            "`{}`: its url references an unset variable",
            server.name
        ))
    })?;
    let backend = CredentialBackend::resolve().map_err(|e| CliError::new(e.to_string()))?;
    let removed = oauth::logout(&url, &backend)
        .await
        .map_err(|e| CliError::new(e.to_string()))?;
    if args.json {
        return print_json(&json!({"name": server.name, "logged_out": removed}));
    }
    if removed {
        println!(
            "Logged out of `{}` ({}; removed from {}).",
            server.name,
            credential_key(&url),
            backend.describe()
        );
    } else {
        println!(
            "No stored login for `{}` ({}).",
            server.name,
            credential_key(&url)
        );
    }
    Ok(())
}

// ------------------------------------------------------------------------------------------------
// call / tools on an external server (`mcp call --server`, `mcp tools --server`)
// ------------------------------------------------------------------------------------------------

/// A one-server hub connected to `name`, for `mcp call/tools --server`; the caller shuts it down.
/// One tool of an external server, for `mcp tools --server`.
#[derive(Serialize)]
pub struct ExternalToolInfo {
    pub name: String,
    pub title: Option<String>,
    pub description: Option<String>,
}

pub async fn connect_external(
    name: &str,
) -> Result<(McpHub, Arc<amp_mcp::ExternalClient>, Vec<ExternalToolInfo>), CliError> {
    let ctx = ctx()?;
    let mut server = effective(&ctx, name)?;
    server.enabled = true;
    server.startup = Startup::Background;
    server.roles = vec!["*".to_owned()];
    if server.scope == RunScope::Worktree {
        return Err(CliError::usage(format!(
            "`{name}` runs one instance per worker worktree; `amp-harness mcp test {name}` checks \
             it"
        )));
    }
    let hub = hub_for(&ctx, vec![server])?;
    hub.start();
    hub.wait_started(true).await;
    match hub.connection(name) {
        Ok((client, tools)) => {
            let mut tools: Vec<ExternalToolInfo> = tools
                .iter()
                .map(|t| ExternalToolInfo {
                    name: t.name.to_string(),
                    title: t.title.clone(),
                    description: t.description.as_ref().map(|d| d.to_string()),
                })
                .collect();
            tools.sort_by(|a, b| a.name.cmp(&b.name));
            Ok((hub, client, tools))
        }
        Err(e) => {
            let status = hub
                .health()
                .into_iter()
                .find(|h| h.name == name)
                .map(|h| h.status.to_string());
            hub.shutdown().await;
            Err(CliError::new(match status {
                Some(s) => format!("MCP server `{name}`: {s}"),
                None => e,
            }))
        }
    }
}

// ------------------------------------------------------------------------------------------------
// Org presets (#2275 gap): the secrets their MCP servers declare
// ------------------------------------------------------------------------------------------------

/// After `setup --preset` wrote `names` into `config` (the user file): asks for each keychain
/// secret those servers declare that is not stored yet (hidden), and stores it. Without a person
/// or a keychain it asks nothing and returns what is still needed, with the command that sets it.
pub fn ask_preset_secrets(ui: &mut dyn Ui, config: &Path, names: &[String]) -> Vec<String> {
    let mut notes = Vec::new();
    let Ok(Some(text)) = ConfigFile::new(config.to_path_buf()).text() else {
        return notes;
    };
    let Ok(mcp) = layer_mcp(&text, config) else {
        return notes;
    };
    let loaded = McpConfig::from_tables(Some((&mcp, config)), None);
    let service = setup::keychain_for_keys();
    for name in names {
        let Some(server) = loaded.servers.get(name) else {
            continue;
        };
        let items = keychain_items(server);
        if items.is_empty() {
            continue;
        }
        let service = match &service {
            Ok(s) => s,
            Err(why) => {
                notes.push(format!(
                    "MCP server `{name}` needs {} in the OS keychain, which is not available here \
                     ({why}); set them with `amp-harness mcp secret {name}` on a machine with a \
                     keychain, or change the server to use ${{env:..}} references",
                    labels(&items)
                ));
                continue;
            }
        };
        let missing: Vec<&SecretItem> = items
            .iter()
            .filter(|i| stored(Some(service), i) != Some(true))
            .collect();
        if missing.is_empty() {
            continue;
        }
        if !ui.interactive() {
            notes.push(format!(
                "MCP server `{name}` needs {}: run `amp-harness mcp secret {name}`",
                missing
                    .iter()
                    .map(|i| i.label.as_str())
                    .collect::<Vec<_>>()
                    .join(", ")
            ));
            continue;
        }
        ui.say(&format!(
            "The org preset's MCP server `{name}` needs {} (kept in the OS keychain).",
            missing
                .iter()
                .map(|i| i.label.as_str())
                .collect::<Vec<_>>()
                .join(", ")
        ));
        for item in missing {
            let value = match ask_secret(ui, name, item) {
                Ok(v) if !v.is_empty() => v,
                Ok(_) => {
                    notes.push(format!(
                        "MCP server `{name}`: {} was left empty; set it later with `amp-harness \
                         mcp secret {name}`",
                        item.label
                    ));
                    continue;
                }
                Err(e) => {
                    notes.push(format!("MCP server `{name}`: {}: {e}", item.label));
                    continue;
                }
            };
            match amp_mcp::keychain_set(service, &item.account, &value) {
                Ok(()) => ui.say(&format!("  Stored {} in the OS keychain.", item.label)),
                Err(e) => notes.push(format!(
                    "MCP server `{name}`: could not store {}: {e}",
                    item.label
                )),
            }
        }
    }
    notes
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn claude_style_references_become_env_references() {
        for (given, want) in [
            ("${TOKEN}", "${env:TOKEN}"),
            ("Bearer ${TOKEN}", "Bearer ${env:TOKEN}"),
            ("${A:-x}/${B}", "${env:A:-x}/${env:B}"),
            ("${env:A}", "${env:A}"),
            ("${repo}/x ${worktree}", "${repo}/x ${worktree}"),
            ("$${LITERAL}", "$${LITERAL}"),
            ("plain $HOME", "plain $HOME"),
            ("${1BAD}", "${1BAD}"),
        ] {
            assert_eq!(env_refs(given), want, "{given}");
        }
    }

    #[test]
    fn secret_looking_flags_are_refused_without_echoing_them() {
        let planted = "ghp_abcdefghijklmnopqrstuvwxyz0123456789";
        for (kind, name, value) in [
            ("header", "Authorization", format!("Bearer {planted}")),
            ("header", "X-Api-Key", "literal-value".to_owned()),
            ("env", "GITHUB_TOKEN", planted.to_owned()),
            ("env", "OTHER", planted.to_owned()),
        ] {
            let err = refuse_secret(kind, name, &value).expect_err(name);
            assert!(!err.message.contains(planted), "{}", err.message);
            assert!(!err.message.contains("literal-value"), "{}", err.message);
            assert!(err.message.contains("--secret-"), "{}", err.message);
        }
        for (kind, name, value) in [
            ("header", "Authorization", "Bearer ${env:GITHUB_TOKEN}"),
            ("header", "X-Org", "simstech"),
            ("env", "LOG_LEVEL", "debug"),
            ("env", "GITHUB_TOKEN", "${env:GITHUB_TOKEN}"),
        ] {
            refuse_secret(kind, name, value).unwrap_or_else(|e| panic!("{name}: {}", e.message));
        }
    }

    #[test]
    fn json_servers_map_to_tables_and_move_secrets_aside() {
        let s = new_from_json(
            "gh",
            r#"{"type":"http","url":"https://x/mcp","headers":{"X-Org":"s","Authorization":"Bearer ${GH}","X-Api-Key":"k-123"},"oauth":{"clientId":"c","scopes":["repo"]},"timeout":1500}"#,
        )
        .unwrap();
        let t = &s.table;
        assert_eq!(t["type"].as_str(), Some("http"));
        assert_eq!(t["headers"]["X-Org"].as_str(), Some("s"));
        assert_eq!(
            t["headers"]["Authorization"].as_str(),
            Some("Bearer ${env:GH}")
        );
        assert_eq!(t["secret_headers"][0].as_str(), Some("X-Api-Key"));
        assert!(!toml::to_string(t).unwrap().contains("k-123"));
        assert_eq!(t["oauth"]["client_id"].as_str(), Some("c"));
        assert_eq!(t["call_timeout_secs"].as_integer(), Some(2));
        assert_eq!(s.secrets.len(), 1);
        assert_eq!(s.secrets[0].0.account, "mcp:gh:header:x-api-key");

        let s = new_from_json(
            "fs",
            r#"{"command":"npx","args":["-y","pkg","${HOME}"],"env":{"LOG":"1","BRAVE_API_KEY":"abc"}}"#,
        )
        .unwrap();
        assert_eq!(s.table["type"].as_str(), Some("stdio"));
        assert_eq!(s.table["args"][2].as_str(), Some("${env:HOME}"));
        assert_eq!(s.table["secret_env"][0].as_str(), Some("BRAVE_API_KEY"));
        assert!(s.table["env"].get("BRAVE_API_KEY").is_none());

        for bad in [
            "[]",
            "{}",
            r#"{"mcpServers":{}}"#,
            r#"{"type":"ws","url":"x"}"#,
            "{",
        ] {
            assert!(new_from_json("x", bad).is_err(), "{bad}");
        }
    }
}
