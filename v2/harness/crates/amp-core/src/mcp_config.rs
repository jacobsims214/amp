//! External MCP server configuration (#2233, H2; D2, D4, D5, D6 and D7 of the configurability
//! design, KB decisions/amp-harness-configurability.md).
//!
//! `[mcp.<name>]` tables in the user's config.toml and in the repo's `.amp/harness/config.toml`
//! describe the MCP servers the harness connects to besides AMP's own. This module parses and
//! validates them, merges the two layers, expands `${...}` references and names the keychain items
//! that hold their secrets. It connects nothing: the transports (H3 stdio, H4/H5 HTTP and SSE) and
//! the hub (H6) build on [`McpServer`], [`McpServer::resolve`] and [`ToolNames`].
//!
//! # Schema
//!
//! ```toml
//! [mcp.github]
//! type = "http"                       # stdio | http | sse (missing: command => stdio, url => http)
//! url = "https://api.githubcopilot.com/mcp/"
//! headers = { "X-Org" = "simstech" }  # non-secret only
//! secret_headers = ["X-Api-Key"]      # value in the keychain: mcp:github:header:x-api-key
//! bearer_env = "GITHUB_TOKEN"         # Authorization: Bearer $GITHUB_TOKEN
//! auth = "auto"                       # auto (OAuth on 401) | oauth | none
//! oauth = { client_id = "abc", scopes = ["repo"], callback_port = 0, client_secret = true }
//! ca_file = "/etc/ssl/corp.pem"
//! protocol = "auto"                   # auto | modern | legacy (amp_mcp::ProtocolMode)
//! socket = "/run/mcp.sock"            # http only: Streamable HTTP over a unix socket
//! fallback_sse = true                 # http only: retry as legacy SSE on 400/404/405
//! enabled = true
//! roles = ["*"]                       # globs on role ids, `amp-` optional
//! startup = "background"              # background | eager | lazy
//! startup_timeout_secs = 30
//! call_timeout_secs = 120
//! max_output_tokens = 25000
//! instructions = true
//!
//! [mcp.fs]
//! type = "stdio"
//! command = "npx"
//! args = ["-y", "@modelcontextprotocol/server-filesystem", "${worktree}"]
//! env = { LOG_LEVEL = "info", API_URL = "${env:API_URL}" }
//! secret_env = ["BRAVE_API_KEY"]      # value in the keychain: mcp:fs:env:BRAVE_API_KEY
//! env_file = ".env.mcp"               # relative to the repo (repo layer) or the config file's dir
//! env_inherit = "all"                 # all | minimal
//! cwd = "${worktree}"
//! scope = "worktree"                  # shared | worktree (implied by any ${worktree})
//! ```
//!
//! Unknown keys are errors, and so are keys of the other transport (`url` on a stdio server).
//!
//! # Layers
//!
//! User file < repo file, per server name: a repo table replaces the user table of the same name.
//! The person keeps the last word on whether it runs: an `enabled` the user file sets for that name
//! wins over the repo's, and a user table holding only `enabled` is a switch for a repo server.
//!
//! # Substitution
//!
//! `${env:VAR}`, `${env:VAR:-default}` (default when unset or empty), `${repo}`, `${worktree}`,
//! `${home}`, `${config_dir}`; `$${` is a literal `${`. References are checked when the config is
//! parsed and expanded only by [`McpServer::resolve`] at spawn or connect time; nothing is written
//! back. Every `${env:VAR}` in `headers` or `env`, every `bearer_env` and every `secret_env` name
//! joins [`crate::agent::Runtime::secret_env`] ([`McpConfig::secret_env_names`]), so bash never
//! sees them.
//!
//! # Secrets
//!
//! Only in the keychain (service [`amp_mcp::keychain_service`]), under [`header_account`],
//! [`env_account`] and [`oauth_client_secret_account`], or in the process environment
//! (`bearer_env`, `${env:..}`). A literal value that looks secret (an `Authorization` header, a
//! `*_TOKEN` variable, `Bearer ...`, `sk-...`, `ghp_...`) is an error whose fix says where to put
//! it, and no message here ever repeats a configured value.

use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    path::{Path, PathBuf},
    time::Duration,
};

use amp_mcp::{ConfigPaths, ProtocolMode};
use serde::Deserialize;

use crate::{policy::wildcard_match, repo_config::RepoConfig};

/// Server names nobody may configure: AMP's own server and the harness.
pub const RESERVED_NAMES: [&str; 2] = ["amp", "harness"];
/// The longest server name.
pub const MAX_SERVER_NAME_LEN: usize = 32;
/// The longest tool name a model sees (OpenAI's limit, the strictest of the providers).
pub const MAX_TOOL_NAME_LEN: usize = 64;
/// Every external tool's name starts with this; AMP's own tools never do.
pub const TOOL_PREFIX: &str = "mcp__";
/// `startup_timeout_secs` when the table sets none.
pub const DEFAULT_STARTUP_TIMEOUT_SECS: u64 = 30;
/// `call_timeout_secs` when the table sets none (the same as AMP's tools).
pub const DEFAULT_CALL_TIMEOUT_SECS: u64 = 120;
/// `max_output_tokens` when the table sets none (Claude Code's default).
pub const DEFAULT_MAX_OUTPUT_TOKENS: u64 = 25_000;
/// The variables `env_inherit = "minimal"` keeps (plus every `LC_*`).
pub const MINIMAL_ENV: [&str; 7] = ["PATH", "HOME", "USER", "LANG", "TMPDIR", "SHELL", "TERM"];

// ---------------------------------------------------------------------------------------------
// Problems

/// How bad a [`McpProblem`] is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Severity {
    /// The server is not usable (it is left out of [`McpConfig::servers`]).
    Error,
    /// Worth telling the person; the server still loads.
    Warning,
}

/// One thing wrong with the MCP config, naming the file and the key.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct McpProblem {
    pub severity: Severity,
    /// The file the key is in.
    pub file: PathBuf,
    /// The dotted key, e.g. `mcp.github.url`.
    pub key: String,
    /// What is wrong. Never contains a configured value.
    pub message: String,
    /// How to fix it, when there is an obvious way.
    pub fix: Option<String>,
}

impl McpProblem {
    fn error(file: &Path, key: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            severity: Severity::Error,
            file: file.to_path_buf(),
            key: key.into(),
            message: message.into(),
            fix: None,
        }
    }

    fn warning(file: &Path, key: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            severity: Severity::Warning,
            ..Self::error(file, key, message)
        }
    }

    fn fix(mut self, fix: impl Into<String>) -> Self {
        self.fix = Some(fix.into());
        self
    }
}

impl fmt::Display for McpProblem {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.severity == Severity::Warning {
            write!(f, "warning: ")?;
        }
        write!(
            f,
            "{}: `{}`: {}",
            self.file.display(),
            self.key,
            self.message
        )?;
        if let Some(fix) = &self.fix {
            write!(f, " (fix: {fix})")?;
        }
        Ok(())
    }
}

impl std::error::Error for McpProblem {}

// ---------------------------------------------------------------------------------------------
// Schema

/// Which file a server (or its `enabled`) came from.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Layer {
    /// The user's config.toml (trusted: the person wrote it).
    User,
    /// `<repo>/.amp/harness/config.toml` (needs trust, H8).
    Repo,
}

impl Layer {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::User => "user",
            Self::Repo => "repo",
        }
    }
}

/// A layer and its file.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Origin {
    pub layer: Layer,
    pub file: PathBuf,
}

/// `type`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum TransportKind {
    Stdio,
    Http,
    Sse,
}

impl TransportKind {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Stdio => "stdio",
            Self::Http => "http",
            Self::Sse => "sse",
        }
    }
}

/// `auth`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum AuthMode {
    /// OAuth when the server answers 401 (MCP's authorization flow).
    #[default]
    Auto,
    /// Always OAuth.
    Oauth,
    /// Never OAuth (headers or nothing).
    None,
}

/// `startup` (D6).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Startup {
    /// Connect in parallel while the chat opens; headless runs wait up to the startup timeout.
    #[default]
    Background,
    /// The chat waits too.
    Eager,
    /// Connect on first use.
    Lazy,
}

/// `scope` (D6).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Scope {
    /// One instance for the manager and every worker.
    #[default]
    Shared,
    /// One instance per worker worktree (stdio only).
    Worktree,
}

/// `env_inherit` (D7): which of the harness's environment a stdio server starts from (always minus
/// host-private and secret variables).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum EnvInherit {
    #[default]
    All,
    /// Only [`MINIMAL_ENV`] and `LC_*`.
    Minimal,
}

/// `oauth = { ... }`: a pre-registered client or extra settings for MCP's OAuth flow.
#[derive(Debug, Clone, PartialEq, Eq, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OAuthSpec {
    pub client_id: Option<String>,
    #[serde(default)]
    pub scopes: Vec<String>,
    /// 0: any free port.
    #[serde(default)]
    pub callback_port: u16,
    /// The client has a secret, in the keychain under [`oauth_client_secret_account`].
    #[serde(default)]
    pub client_secret: bool,
    pub auth_server_metadata_url: Option<String>,
}

/// A stdio server's launch settings, as written (unexpanded templates).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StdioSpec {
    pub command: String,
    pub args: Vec<String>,
    pub env: BTreeMap<String, String>,
    /// Variable names whose values are in the keychain ([`env_account`]).
    pub secret_env: Vec<String>,
    pub env_file: Option<String>,
    pub cwd: Option<String>,
    pub env_inherit: EnvInherit,
}

/// An http or sse server's settings, as written (unexpanded templates).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RemoteSpec {
    pub url: String,
    pub headers: BTreeMap<String, String>,
    /// Header names whose values are in the keychain ([`header_account`]).
    pub secret_headers: Vec<String>,
    /// `Authorization: Bearer $<bearer_env>`.
    pub bearer_env: Option<String>,
    pub auth: AuthMode,
    pub oauth: Option<OAuthSpec>,
    pub ca_file: Option<String>,
    pub protocol: ProtocolMode,
    /// http only.
    pub socket: Option<String>,
    /// http only: fall back to legacy SSE when Streamable HTTP is refused (default true).
    pub fallback_sse: bool,
}

/// How a server is reached.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Transport {
    Stdio(StdioSpec),
    Http(RemoteSpec),
    Sse(RemoteSpec),
}

impl Transport {
    pub fn kind(&self) -> TransportKind {
        match self {
            Self::Stdio(_) => TransportKind::Stdio,
            Self::Http(_) => TransportKind::Http,
            Self::Sse(_) => TransportKind::Sse,
        }
    }

    /// The http/sse settings.
    pub fn remote(&self) -> Option<&RemoteSpec> {
        match self {
            Self::Http(r) | Self::Sse(r) => Some(r),
            Self::Stdio(_) => None,
        }
    }

    /// The stdio settings.
    pub fn stdio(&self) -> Option<&StdioSpec> {
        match self {
            Self::Stdio(s) => Some(s),
            _ => None,
        }
    }
}

/// One validated `[mcp.<name>]`, after merging the layers.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct McpServer {
    pub name: String,
    /// The file the table came from.
    pub origin: Origin,
    pub enabled: bool,
    /// The file `enabled` came from (the user file can switch a repo server).
    pub enabled_origin: Origin,
    pub transport: Transport,
    /// Role globs (without `amp-`); `["*"]` by default.
    pub roles: Vec<String>,
    pub startup: Startup,
    pub startup_timeout: Duration,
    pub call_timeout: Duration,
    pub max_output_tokens: u64,
    /// Put the server's instructions in the system prompt.
    pub instructions: bool,
    /// Explicit, or `worktree` because a stdio field uses `${worktree}`.
    pub scope: Scope,
}

/// The table as written; everything optional so the checks can name what is missing or misplaced.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawServer {
    #[serde(rename = "type")]
    transport: Option<TransportKind>,
    command: Option<String>,
    args: Option<Vec<String>>,
    env: Option<BTreeMap<String, String>>,
    secret_env: Option<Vec<String>>,
    env_file: Option<String>,
    cwd: Option<String>,
    env_inherit: Option<EnvInherit>,
    url: Option<String>,
    headers: Option<BTreeMap<String, String>>,
    secret_headers: Option<Vec<String>>,
    bearer_env: Option<String>,
    auth: Option<AuthMode>,
    oauth: Option<OAuthSpec>,
    ca_file: Option<String>,
    protocol: Option<String>,
    socket: Option<String>,
    fallback_sse: Option<bool>,
    enabled: Option<bool>,
    roles: Option<Vec<String>>,
    startup: Option<Startup>,
    startup_timeout_secs: Option<u64>,
    call_timeout_secs: Option<u64>,
    max_output_tokens: Option<u64>,
    instructions: Option<bool>,
    scope: Option<Scope>,
}

impl RawServer {
    /// Only `enabled` is set: a user-file switch for a repo server.
    fn is_switch(&self) -> bool {
        self.enabled.is_some() && self.presence() == Self::default().presence()
    }

    /// Which fields are set, `enabled` aside.
    fn presence(&self) -> [bool; 25] {
        [
            self.transport.is_some(),
            self.command.is_some(),
            self.args.is_some(),
            self.env.is_some(),
            self.secret_env.is_some(),
            self.env_file.is_some(),
            self.cwd.is_some(),
            self.env_inherit.is_some(),
            self.url.is_some(),
            self.headers.is_some(),
            self.secret_headers.is_some(),
            self.bearer_env.is_some(),
            self.auth.is_some(),
            self.oauth.is_some(),
            self.ca_file.is_some(),
            self.protocol.is_some(),
            self.socket.is_some(),
            self.fallback_sse.is_some(),
            self.roles.is_some(),
            self.startup.is_some(),
            self.startup_timeout_secs.is_some(),
            self.call_timeout_secs.is_some(),
            self.max_output_tokens.is_some(),
            self.instructions.is_some(),
            self.scope.is_some(),
        ]
    }
}

// ---------------------------------------------------------------------------------------------
// Names

/// Checks a server name: `[a-z0-9][a-z0-9-]{0,31}`, not reserved. The error is the message; the
/// fix suggests a valid name.
pub fn check_server_name(name: &str) -> Result<(), (String, Option<String>)> {
    let lower = name.to_ascii_lowercase();
    if RESERVED_NAMES.contains(&lower.as_str()) || lower.starts_with("amp_") {
        return Err((
            format!("server name `{name}` is reserved (amp, harness and amp_* belong to AMP)"),
            Some("pick another name".into()),
        ));
    }
    let valid = !name.is_empty()
        && name.len() <= MAX_SERVER_NAME_LEN
        && name
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        && !name.starts_with('-');
    if valid {
        return Ok(());
    }
    let mut suggestion: String = lower
        .chars()
        .map(|c| {
            if c.is_ascii_lowercase() || c.is_ascii_digit() {
                c
            } else {
                '-'
            }
        })
        .collect();
    suggestion = suggestion.trim_start_matches('-').to_owned();
    suggestion.truncate(MAX_SERVER_NAME_LEN);
    let fix = (!suggestion.is_empty() && check_server_name(&suggestion).is_ok())
        .then(|| format!("rename it to `{suggestion}`"));
    Err((
        format!(
            "server name `{name}` must be 1-{MAX_SERVER_NAME_LEN} characters of a-z, 0-9 and -, \
             starting with a letter or digit"
        ),
        fix,
    ))
}

fn valid_env_name(name: &str) -> bool {
    let mut bytes = name.bytes();
    matches!(bytes.next(), Some(b) if b.is_ascii_alphabetic() || b == b'_')
        && bytes.all(|b| b.is_ascii_alphanumeric() || b == b'_')
}

fn valid_header_name(name: &str) -> bool {
    // RFC 9110 token characters.
    !name.is_empty()
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b))
}

/// The keychain account of a secret header: `mcp:<server>:header:<lowercase header>`.
pub fn header_account(server: &str, header: &str) -> String {
    format!("mcp:{server}:header:{}", header.to_ascii_lowercase())
}

/// The keychain account of a secret environment variable: `mcp:<server>:env:<VAR>`.
pub fn env_account(server: &str, var: &str) -> String {
    format!("mcp:{server}:env:{var}")
}

/// The keychain account of a pre-registered OAuth client's secret.
pub fn oauth_client_secret_account(server: &str) -> String {
    format!("mcp:{server}:oauth-client-secret")
}

// ---------------------------------------------------------------------------------------------
// Secret detection

/// Whether a header or variable name usually holds a secret.
pub fn looks_secret_name(name: &str) -> bool {
    let upper = name.to_ascii_uppercase().replace('-', "_");
    const PARTS: [&str; 10] = [
        "AUTHORIZATION",
        "TOKEN",
        "SECRET",
        "PASSWORD",
        "PASSWD",
        "API_KEY",
        "APIKEY",
        "PRIVATE_KEY",
        "COOKIE",
        "CREDENTIAL",
    ];
    PARTS.iter().any(|p| upper.contains(p)) || upper.ends_with("_KEY") || upper == "KEY"
}

/// Whether a literal value looks like a credential.
pub fn looks_secret_value(value: &str) -> bool {
    let v = value.trim();
    let lower = v.to_ascii_lowercase();
    const PREFIXES: [&str; 11] = [
        "bearer ",
        "basic ",
        "sk-",
        "ghp_",
        "gho_",
        "ghs_",
        "github_pat_",
        "glpat-",
        "xoxb-",
        "xoxp-",
        "akia",
    ];
    PREFIXES
        .iter()
        .any(|p| lower.starts_with(p) && v.len() > p.len())
}

/// Whether a URL carries a literal credential in its query (`?token=...`, `&api_key=...`).
fn url_has_literal_secret(url: &str) -> bool {
    let Some((_, query)) = url.split_once('?') else {
        return false;
    };
    query.split('&').any(|pair| match pair.split_once('=') {
        Some((k, v)) => looks_secret_name(k) && !v.is_empty() && !v.starts_with("${"),
        None => false,
    })
}

// ---------------------------------------------------------------------------------------------
// Substitution

/// One piece of a `${...}` template.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Piece {
    Lit(String),
    Env {
        name: String,
        default: Option<String>,
    },
    Repo,
    Worktree,
    Home,
    ConfigDir,
}

/// Parses a template. Errors say what is wrong without repeating the value.
pub fn parse_template(s: &str) -> Result<Vec<Piece>, String> {
    let mut pieces = Vec::new();
    let mut lit = String::new();
    let mut rest = s;
    while let Some(i) = rest.find('$') {
        lit.push_str(&rest[..i]);
        let after = &rest[i + 1..];
        if let Some(escaped) = after.strip_prefix("${") {
            lit.push_str("${");
            rest = escaped;
            continue;
        }
        let Some(body_start) = after.strip_prefix('{') else {
            lit.push('$');
            rest = after;
            continue;
        };
        let end = body_start
            .find('}')
            .ok_or_else(|| "unterminated `${` (write `$${` for a literal `${`)".to_owned())?;
        let body = &body_start[..end];
        rest = &body_start[end + 1..];
        let piece = match body {
            "repo" => Piece::Repo,
            "worktree" => Piece::Worktree,
            "home" => Piece::Home,
            "config_dir" => Piece::ConfigDir,
            _ => match body.strip_prefix("env:") {
                Some(var) => {
                    let (name, default) = match var.split_once(":-") {
                        Some((n, d)) => (n, Some(d.to_owned())),
                        None => (var, None),
                    };
                    if !valid_env_name(name) {
                        return Err(format!(
                            "`${{env:{name}}}`: `{name}` is not a valid environment variable name"
                        ));
                    }
                    Piece::Env {
                        name: name.to_owned(),
                        default,
                    }
                }
                None => {
                    return Err(format!(
                        "unknown reference `${{{body}}}` (expected ${{env:VAR}}, \
                         ${{env:VAR:-default}}, ${{repo}}, ${{worktree}}, ${{home}} or ${{config_dir}})"
                    ));
                }
            },
        };
        if !lit.is_empty() {
            pieces.push(Piece::Lit(std::mem::take(&mut lit)));
        }
        pieces.push(piece);
    }
    lit.push_str(rest);
    if !lit.is_empty() {
        pieces.push(Piece::Lit(lit));
    }
    Ok(pieces)
}

fn template_env_names(s: &str) -> Vec<String> {
    parse_template(s)
        .unwrap_or_default()
        .into_iter()
        .filter_map(|p| match p {
            Piece::Env { name, .. } => Some(name),
            _ => None,
        })
        .collect()
}

fn template_uses(s: &str, want: &Piece) -> bool {
    parse_template(s).unwrap_or_default().contains(want)
}

/// Whether a literal part of the template looks like a credential (references don't count, so
/// `Bearer ${env:TOKEN}` is fine).
fn template_literal_secret(s: &str) -> bool {
    parse_template(s).is_ok_and(|pieces| {
        pieces
            .iter()
            .any(|p| matches!(p, Piece::Lit(l) if looks_secret_value(l)))
    })
}

/// What `${...}` expands to.
pub struct ExpandContext<'a> {
    pub env: &'a dyn Fn(&str) -> Option<String>,
    /// The repo root (the directory holding `.amp.json`).
    pub repo: Option<PathBuf>,
    /// The worker's worktree; `None` for the manager or a shared instance.
    pub worktree: Option<PathBuf>,
    pub home: Option<PathBuf>,
    /// The user config dir ([`ConfigPaths::user_dir`]).
    pub config_dir: Option<PathBuf>,
}

impl<'a> ExpandContext<'a> {
    /// From the session's paths; `worktree` for a per-worktree instance.
    pub fn from_paths(
        paths: &ConfigPaths,
        env: &'a dyn Fn(&str) -> Option<String>,
        worktree: Option<&Path>,
    ) -> Self {
        Self {
            env,
            repo: paths
                .repo_dir
                .as_deref()
                .and_then(|d| d.parent())
                .and_then(|d| d.parent())
                .map(Path::to_path_buf),
            worktree: worktree.map(Path::to_path_buf),
            home: env("HOME").filter(|h| !h.is_empty()).map(PathBuf::from),
            config_dir: paths.user_dir.clone(),
        }
    }

    /// Expands `template` (the value of `key`). Errors name the reference, never the value.
    pub fn expand(&self, template: &str) -> Result<String, String> {
        let path = |p: &Option<PathBuf>, what: &str| {
            p.as_deref()
                .map(|p| p.display().to_string())
                .ok_or_else(|| format!("`${{{what}}}` has no value here"))
        };
        let mut out = String::new();
        for piece in parse_template(template)? {
            match piece {
                Piece::Lit(l) => out.push_str(&l),
                Piece::Env { name, default } => match ((self.env)(&name), default) {
                    (Some(v), Some(d)) if v.is_empty() => out.push_str(&d),
                    (Some(v), _) => out.push_str(&v),
                    (None, Some(d)) => out.push_str(&d),
                    (None, None) => {
                        return Err(format!(
                            "environment variable {name} is not set (use ${{env:{name}:-default}} \
                             for a default)"
                        ));
                    }
                },
                Piece::Repo => out.push_str(&path(&self.repo, "repo")?),
                Piece::Worktree => out.push_str(&path(&self.worktree, "worktree")?),
                Piece::Home => out.push_str(&path(&self.home, "home")?),
                Piece::ConfigDir => out.push_str(&path(&self.config_dir, "config_dir")?),
            }
        }
        Ok(out)
    }
}

// ---------------------------------------------------------------------------------------------
// Secrets at connect time

/// Where a secret goes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SecretTarget {
    /// A stdio server's environment variable.
    Env(String),
    /// An HTTP header.
    Header(String),
    /// `Authorization: Bearer <value>`.
    Bearer,
    /// The OAuth client secret.
    OAuthClientSecret,
}

/// Where a secret comes from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SecretSource {
    Keychain { account: String },
    Env { var: String },
}

/// A secret the transport fetches when it connects. Holds names, never values.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SecretRef {
    pub target: SecretTarget,
    pub source: SecretSource,
}

impl SecretRef {
    /// For `mcp show`/`list`: `keychain mcp:x:env:Y` or `env VAR`.
    pub fn describe(&self) -> String {
        match &self.source {
            SecretSource::Keychain { account } => format!("keychain {account}"),
            SecretSource::Env { var } => format!("env {var}"),
        }
    }

    /// The value: from `service` in the keychain ([`amp_mcp::keychain_service`] in production) or
    /// from `env`. `Ok(None)` when it is not there.
    pub fn fetch(
        &self,
        service: &str,
        env: &dyn Fn(&str) -> Option<String>,
    ) -> Result<Option<String>, String> {
        match &self.source {
            SecretSource::Keychain { account } => amp_mcp::keychain_get(service, account)
                .map_err(|e| format!("keychain {account}: {e}")),
            SecretSource::Env { var } => Ok(env(var).filter(|v| !v.is_empty())),
        }
    }
}

/// A stdio server ready to spawn: references expanded, paths absolute, secrets still references.
#[derive(Clone, PartialEq, Eq)]
pub struct ResolvedStdio {
    pub command: String,
    pub args: Vec<String>,
    /// `env_file` then `env`, expanded. Goes on top of the inherited environment (D7).
    pub env: BTreeMap<String, String>,
    /// Keychain values for `secret_env`; the transport adds them last.
    pub secrets: Vec<SecretRef>,
    pub cwd: Option<PathBuf>,
    pub env_inherit: EnvInherit,
}

/// An http or sse server ready to connect.
#[derive(Clone, PartialEq, Eq)]
pub struct ResolvedRemote {
    pub url: String,
    /// Non-secret headers, expanded (may hold `${env:..}` values, which are secret_env names).
    pub headers: BTreeMap<String, String>,
    /// `secret_headers`, `bearer_env` and the OAuth client secret.
    pub secrets: Vec<SecretRef>,
    pub auth: AuthMode,
    pub oauth: Option<OAuthSpec>,
    pub ca_file: Option<PathBuf>,
    pub protocol: ProtocolMode,
    pub socket: Option<PathBuf>,
    pub fallback_sse: bool,
}

/// [`McpServer::resolve`]'s result.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Resolved {
    Stdio(ResolvedStdio),
    Http(ResolvedRemote),
    Sse(ResolvedRemote),
}

// Values may come from `${env:..}`: Debug prints names only.
impl fmt::Debug for ResolvedStdio {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ResolvedStdio")
            .field("command", &self.command)
            .field("args", &format_args!("[{} args]", self.args.len()))
            .field("env", &self.env.keys().collect::<Vec<_>>())
            .field("secrets", &self.secrets)
            .field("cwd", &self.cwd)
            .field("env_inherit", &self.env_inherit)
            .finish()
    }
}

impl fmt::Debug for ResolvedRemote {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ResolvedRemote")
            .field("url", &amp_mcp::url_origin(&self.url))
            .field("headers", &self.headers.keys().collect::<Vec<_>>())
            .field("secrets", &self.secrets)
            .field("auth", &self.auth)
            .field("protocol", &self.protocol)
            .field("fallback_sse", &self.fallback_sse)
            .finish_non_exhaustive()
    }
}

impl McpServer {
    /// Whether `role` (with or without `amp-`) may use this server (the coarse D5 filter; the
    /// role's `mcp` permission applies after it).
    pub fn allows_role(&self, role: &str) -> bool {
        let id = role.strip_prefix("amp-").unwrap_or(role);
        self.roles.iter().any(|pattern| {
            let pattern = pattern.strip_prefix("amp-").unwrap_or(pattern);
            wildcard_match(id, pattern)
        })
    }

    /// The keychain accounts this server's config names (for `mcp remove` and `mcp show`).
    pub fn keychain_accounts(&self) -> Vec<String> {
        let mut accounts = Vec::new();
        match &self.transport {
            Transport::Stdio(s) => {
                accounts.extend(s.secret_env.iter().map(|v| env_account(&self.name, v)));
            }
            Transport::Http(r) | Transport::Sse(r) => {
                accounts.extend(
                    r.secret_headers
                        .iter()
                        .map(|h| header_account(&self.name, h)),
                );
                if r.oauth.as_ref().is_some_and(|o| o.client_secret) {
                    accounts.push(oauth_client_secret_account(&self.name));
                }
            }
        }
        accounts
    }

    /// Whether any secret lives in the keychain (`AMP_CREDENTIAL_STORE=file` cannot serve it).
    pub fn uses_keychain(&self) -> bool {
        !self.keychain_accounts().is_empty()
    }

    /// The variables to hide from bash: `${env:..}` in headers/env, `bearer_env`, `secret_env`.
    pub fn secret_env_names(&self) -> BTreeSet<String> {
        let mut names = BTreeSet::new();
        match &self.transport {
            Transport::Stdio(s) => {
                for v in s.env.values() {
                    names.extend(template_env_names(v));
                }
                names.extend(s.secret_env.iter().cloned());
            }
            Transport::Http(r) | Transport::Sse(r) => {
                for v in r.headers.values() {
                    names.extend(template_env_names(v));
                }
                names.extend(r.bearer_env.iter().cloned());
            }
        }
        names
    }

    /// The directory relative paths (`env_file`, `cwd`, `ca_file`, `socket`) are relative to: the
    /// repo root for a repo server, else the user config file's directory.
    pub fn base_dir(&self, ctx: &ExpandContext<'_>) -> Option<PathBuf> {
        match self.origin.layer {
            Layer::Repo => ctx.repo.clone().or_else(|| {
                // <repo>/.amp/harness/config.toml
                self.origin.file.ancestors().nth(3).map(Path::to_path_buf)
            }),
            Layer::User => self.origin.file.parent().map(Path::to_path_buf),
        }
    }

    /// Expands every reference and reads `env_file`, for spawning or connecting now. Secret
    /// values are not read here: the transport calls [`SecretRef::fetch`].
    pub fn resolve(&self, ctx: &ExpandContext<'_>) -> Result<Resolved, McpProblem> {
        let key = |k: &str| format!("mcp.{}.{k}", self.name);
        let fail = |k: &str, m: String| McpProblem::error(&self.origin.file, key(k), m);
        let base = self.base_dir(ctx);
        let absolute = |p: String| {
            let p = PathBuf::from(p);
            match (&base, p.is_absolute()) {
                (Some(b), false) => b.join(p),
                _ => p,
            }
        };
        let expand = |k: &str, v: &str| ctx.expand(v).map_err(|m| fail(k, m));
        match &self.transport {
            Transport::Stdio(s) => {
                let mut env = BTreeMap::new();
                if let Some(file) = &s.env_file {
                    let path = absolute(expand("env_file", file)?);
                    env.extend(read_env_file(&path).map_err(|m| fail("env_file", m))?);
                }
                for (k, v) in &s.env {
                    env.insert(k.clone(), expand(&format!("env.{k}"), v)?);
                }
                Ok(Resolved::Stdio(ResolvedStdio {
                    command: expand("command", &s.command)?,
                    args: s
                        .args
                        .iter()
                        .map(|a| expand("args", a))
                        .collect::<Result<_, _>>()?,
                    env,
                    secrets: s
                        .secret_env
                        .iter()
                        .map(|v| SecretRef {
                            target: SecretTarget::Env(v.clone()),
                            source: SecretSource::Keychain {
                                account: env_account(&self.name, v),
                            },
                        })
                        .collect(),
                    cwd: s
                        .cwd
                        .as_deref()
                        .map(|c| expand("cwd", c).map(absolute))
                        .transpose()?,
                    env_inherit: s.env_inherit,
                }))
            }
            Transport::Http(r) | Transport::Sse(r) => {
                let mut headers = BTreeMap::new();
                for (k, v) in &r.headers {
                    headers.insert(k.clone(), expand(&format!("headers.{k}"), v)?);
                }
                let mut secrets: Vec<SecretRef> = r
                    .secret_headers
                    .iter()
                    .map(|h| SecretRef {
                        target: SecretTarget::Header(h.clone()),
                        source: SecretSource::Keychain {
                            account: header_account(&self.name, h),
                        },
                    })
                    .collect();
                if let Some(var) = &r.bearer_env {
                    secrets.push(SecretRef {
                        target: SecretTarget::Bearer,
                        source: SecretSource::Env { var: var.clone() },
                    });
                }
                if r.oauth.as_ref().is_some_and(|o| o.client_secret) {
                    secrets.push(SecretRef {
                        target: SecretTarget::OAuthClientSecret,
                        source: SecretSource::Keychain {
                            account: oauth_client_secret_account(&self.name),
                        },
                    });
                }
                let remote = ResolvedRemote {
                    url: expand("url", &r.url)?,
                    headers,
                    secrets,
                    auth: r.auth,
                    oauth: r.oauth.clone(),
                    ca_file: r
                        .ca_file
                        .as_deref()
                        .map(|c| expand("ca_file", c).map(absolute))
                        .transpose()?,
                    protocol: r.protocol,
                    socket: r
                        .socket
                        .as_deref()
                        .map(|c| expand("socket", c).map(absolute))
                        .transpose()?,
                    fallback_sse: r.fallback_sse,
                };
                let lower = remote.url.to_ascii_lowercase();
                if !(lower.starts_with("https://") || lower.starts_with("http://")) {
                    return Err(fail(
                        "url",
                        "must expand to an http:// or https:// URL".into(),
                    ));
                }
                Ok(match self.transport.kind() {
                    TransportKind::Sse => Resolved::Sse(remote),
                    _ => Resolved::Http(remote),
                })
            }
        }
    }
}

/// Reads a dotenv file: `KEY=VALUE` lines, `#` comments, optional `export `, optional quotes.
/// Errors name the file and line, never a value.
pub fn read_env_file(path: &Path) -> Result<BTreeMap<String, String>, String> {
    let text =
        std::fs::read_to_string(path).map_err(|e| format!("read {}: {e}", path.display()))?;
    let mut out = BTreeMap::new();
    for (i, line) in text.lines().enumerate() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let line = line.strip_prefix("export ").unwrap_or(line);
        let Some((k, v)) = line.split_once('=') else {
            return Err(format!("{}:{}: expected KEY=VALUE", path.display(), i + 1));
        };
        let k = k.trim();
        if !valid_env_name(k) {
            return Err(format!(
                "{}:{}: `{k}` is not a valid variable name",
                path.display(),
                i + 1
            ));
        }
        let v = v.trim();
        let v = v
            .strip_prefix('"')
            .and_then(|v| v.strip_suffix('"'))
            .or_else(|| v.strip_prefix('\'').and_then(|v| v.strip_suffix('\'')))
            .unwrap_or(v);
        out.insert(k.to_owned(), v.to_owned());
    }
    Ok(out)
}

// ---------------------------------------------------------------------------------------------
// Parsing and validation

/// Serde's messages can quote a value (`invalid type: string "..."`); drop the quoted part.
pub(crate) fn redact_serde(message: &str) -> String {
    let mut out = String::new();
    let mut rest = message;
    while let Some(i) = rest.find("string \"") {
        out.push_str(&rest[..i + "string".len()]);
        let after = &rest[i + "string \"".len()..];
        let mut end = None;
        let mut escaped = false;
        for (j, c) in after.char_indices() {
            match c {
                '\\' if !escaped => escaped = true,
                '"' if !escaped => {
                    end = Some(j);
                    break;
                }
                _ => escaped = false,
            }
        }
        match end {
            Some(j) => rest = &after[j + 1..],
            None => {
                rest = "";
            }
        }
    }
    out.push_str(rest);
    out
}

struct Checker<'a> {
    name: &'a str,
    file: &'a Path,
    problems: Vec<McpProblem>,
}

impl Checker<'_> {
    fn key(&self, k: &str) -> String {
        if k.is_empty() {
            format!("mcp.{}", self.name)
        } else {
            format!("mcp.{}.{k}", self.name)
        }
    }
    fn error(&mut self, k: &str, message: impl Into<String>) -> &mut McpProblem {
        let p = McpProblem::error(self.file, self.key(k), message);
        self.problems.push(p);
        self.problems.last_mut().expect("just pushed")
    }
    fn warning(&mut self, k: &str, message: impl Into<String>) {
        let p = McpProblem::warning(self.file, self.key(k), message);
        self.problems.push(p);
    }
    fn template(&mut self, k: &str, value: &str) {
        if let Err(m) = parse_template(value) {
            self.error(k, m);
        }
    }
    fn not_for(&mut self, k: &str, set: bool, kind: TransportKind, fix: &str) {
        if set {
            let p = McpProblem::error(
                self.file,
                self.key(k),
                format!("`{k}` does not apply to a {} server", kind.as_str()),
            )
            .fix(fix.to_owned());
            self.problems.push(p);
        }
    }
    fn has_errors(&self) -> bool {
        self.problems.iter().any(|p| p.severity == Severity::Error)
    }
}

/// Parses and validates one `[mcp.<name>]` table. On success the problems are warnings only.
pub fn parse_server(
    name: &str,
    value: &toml::Value,
    origin: Origin,
) -> Result<(McpServer, Vec<McpProblem>), Vec<McpProblem>> {
    let raw = parse_raw(name, value, &origin.file)?;
    build_server(name, raw, origin)
}

fn parse_raw(name: &str, value: &toml::Value, file: &Path) -> Result<RawServer, Vec<McpProblem>> {
    if let Err((message, fix)) = check_server_name(name) {
        let mut p = McpProblem::error(file, format!("mcp.{name}"), message);
        p.fix = fix;
        return Err(vec![p]);
    }
    if !value.is_table() {
        return Err(vec![McpProblem::error(
            file,
            format!("mcp.{name}"),
            "must be a table ([mcp.<name>])",
        )]);
    }
    value.clone().try_into::<RawServer>().map_err(|e| {
        vec![McpProblem::error(
            file,
            format!("mcp.{name}"),
            redact_serde(e.message()),
        )]
    })
}

fn build_server(
    name: &str,
    raw: RawServer,
    origin: Origin,
) -> Result<(McpServer, Vec<McpProblem>), Vec<McpProblem>> {
    let file = origin.file.clone();
    let mut c = Checker {
        name,
        file: &file,
        problems: Vec::new(),
    };
    let kind = match (raw.transport, &raw.command, &raw.url) {
        (Some(k), _, _) => k,
        (None, Some(_), None) => TransportKind::Stdio,
        (None, None, Some(_)) => TransportKind::Http,
        _ => {
            c.error("type", "missing: set `type` to stdio, http or sse")
                .fix = Some("type = \"stdio\" with command, or type = \"http\" with url".into());
            return Err(c.problems);
        }
    };

    // Shared settings.
    for (k, v) in [
        ("startup_timeout_secs", raw.startup_timeout_secs),
        ("call_timeout_secs", raw.call_timeout_secs),
        ("max_output_tokens", raw.max_output_tokens),
    ] {
        if v == Some(0) {
            c.error(k, "must be greater than 0");
        }
    }
    let roles = raw.roles.clone().unwrap_or_else(|| vec!["*".into()]);
    if roles.iter().any(|r| r.trim().is_empty()) {
        c.error("roles", "role patterns must not be empty");
    }
    if roles.is_empty() {
        c.warning("roles", "is empty, so no role can use this server");
    }

    let transport = match kind {
        TransportKind::Stdio => {
            let fix = "remove it, or use type = \"http\"/\"sse\" with url";
            c.not_for("url", raw.url.is_some(), kind, fix);
            c.not_for("headers", raw.headers.is_some(), kind, fix);
            c.not_for("secret_headers", raw.secret_headers.is_some(), kind, fix);
            c.not_for("bearer_env", raw.bearer_env.is_some(), kind, fix);
            c.not_for("auth", raw.auth.is_some(), kind, fix);
            c.not_for("oauth", raw.oauth.is_some(), kind, fix);
            c.not_for("ca_file", raw.ca_file.is_some(), kind, fix);
            c.not_for("protocol", raw.protocol.is_some(), kind, fix);
            c.not_for("socket", raw.socket.is_some(), kind, fix);
            c.not_for("fallback_sse", raw.fallback_sse.is_some(), kind, fix);
            let command = raw.command.clone().unwrap_or_default();
            if command.trim().is_empty() {
                c.error("command", "missing: a stdio server needs a command")
                    .fix = Some("command = \"npx\", args = [...]".into());
            }
            c.template("command", &command);
            let args = raw.args.clone().unwrap_or_default();
            for a in &args {
                c.template("args", a);
                if template_literal_secret(a) {
                    c.error("args", "an argument looks like a credential").fix =
                        Some("pass it through secret_env (keychain) or ${env:VAR} instead".into());
                }
            }
            let env = raw.env.clone().unwrap_or_default();
            for (k, v) in &env {
                let key = format!("env.{k}");
                if !valid_env_name(k) {
                    c.error(
                        &key,
                        format!("`{k}` is not a valid environment variable name"),
                    );
                }
                c.template(&key, v);
                literal_secret_check(&mut c, &key, k, v, "secret_env", "env");
            }
            let secret_env = raw.secret_env.clone().unwrap_or_default();
            let mut seen = BTreeSet::new();
            for v in &secret_env {
                if !valid_env_name(v) {
                    c.error(
                        "secret_env",
                        format!("`{v}` is not a valid environment variable name"),
                    );
                }
                if !seen.insert(v.as_str()) {
                    c.error("secret_env", format!("`{v}` is listed twice"));
                }
                if env.contains_key(v) {
                    c.error(
                        "secret_env",
                        format!("`{v}` is also in env; a variable comes from one place"),
                    )
                    .fix = Some(format!("remove env.{v}"));
                }
            }
            for (k, v) in [("env_file", &raw.env_file), ("cwd", &raw.cwd)] {
                if let Some(v) = v {
                    c.template(k, v);
                }
            }
            Transport::Stdio(StdioSpec {
                command,
                args,
                env,
                secret_env,
                env_file: raw.env_file.clone(),
                cwd: raw.cwd.clone(),
                env_inherit: raw.env_inherit.unwrap_or_default(),
            })
        }
        TransportKind::Http | TransportKind::Sse => {
            let fix = "remove it, or use type = \"stdio\" with command";
            c.not_for("command", raw.command.is_some(), kind, fix);
            c.not_for("args", raw.args.is_some(), kind, fix);
            c.not_for("env", raw.env.is_some(), kind, fix);
            c.not_for("secret_env", raw.secret_env.is_some(), kind, fix);
            c.not_for("env_file", raw.env_file.is_some(), kind, fix);
            c.not_for("cwd", raw.cwd.is_some(), kind, fix);
            c.not_for("env_inherit", raw.env_inherit.is_some(), kind, fix);
            if kind == TransportKind::Sse {
                let fix = "socket and fallback_sse apply to type = \"http\" only";
                c.not_for("socket", raw.socket.is_some(), kind, fix);
                c.not_for("fallback_sse", raw.fallback_sse.is_some(), kind, fix);
            }
            if raw.scope == Some(Scope::Worktree) {
                c.error("scope", "a remote server is always shared").fix =
                    Some("remove scope (worktree applies to stdio servers)".into());
            }
            let url = raw.url.clone().unwrap_or_default();
            if url.trim().is_empty() {
                c.error(
                    "url",
                    format!("missing: a {} server needs a url", kind.as_str()),
                );
            } else {
                c.template("url", &url);
                let lower = url.to_ascii_lowercase();
                if !(lower.starts_with("https://")
                    || lower.starts_with("http://")
                    || lower.starts_with("${"))
                {
                    c.error("url", "must start with http:// or https://");
                }
                if url_has_literal_secret(&url) {
                    c.error("url", "the query string holds a credential").fix =
                        Some("use ${env:VAR} in the URL, or a secret header".into());
                }
                if template_uses(&url, &Piece::Worktree) {
                    c.error("url", "`${worktree}` is for stdio servers");
                }
            }
            let headers = raw.headers.clone().unwrap_or_default();
            let mut lower_names = BTreeSet::new();
            for (k, v) in &headers {
                let key = format!("headers.{k}");
                if !valid_header_name(k) {
                    c.error(&key, format!("`{k}` is not a valid header name"));
                }
                if !lower_names.insert(k.to_ascii_lowercase()) {
                    c.error(
                        &key,
                        format!("header `{k}` is set twice (names ignore case)"),
                    );
                }
                c.template(&key, v);
                if template_uses(v, &Piece::Worktree) {
                    c.error(&key, "`${worktree}` is for stdio servers");
                }
                literal_secret_check(&mut c, &key, k, v, "secret_headers", "headers");
            }
            let secret_headers = raw.secret_headers.clone().unwrap_or_default();
            for h in &secret_headers {
                if !valid_header_name(h) {
                    c.error(
                        "secret_headers",
                        format!("`{h}` is not a valid header name"),
                    );
                }
                if !lower_names.insert(h.to_ascii_lowercase()) {
                    c.error(
                        "secret_headers",
                        format!("header `{h}` is set twice (in headers or secret_headers)"),
                    );
                }
            }
            if let Some(var) = &raw.bearer_env {
                if !valid_env_name(var) {
                    c.error(
                        "bearer_env",
                        format!("`{var}` is not a valid environment variable name"),
                    );
                }
                if lower_names.contains("authorization") {
                    c.error(
                        "bearer_env",
                        "Authorization is also set in headers or secret_headers",
                    )
                    .fix = Some("keep one of them".into());
                }
            }
            let auth = raw.auth.unwrap_or_default();
            if let Some(o) = &raw.oauth {
                if auth == AuthMode::None {
                    c.error("oauth", "is set but auth = \"none\"").fix =
                        Some("remove oauth, or set auth = \"auto\"/\"oauth\"".into());
                }
                if o.client_secret && o.client_id.as_deref().is_none_or(str::is_empty) {
                    c.error(
                        "oauth.client_secret",
                        "a client secret needs oauth.client_id",
                    );
                }
                if o.scopes.iter().any(|s| s.trim().is_empty()) {
                    c.error("oauth.scopes", "scopes must not be empty");
                }
            }
            let protocol = match raw.protocol.as_deref().map(str::parse::<ProtocolMode>) {
                None => ProtocolMode::Auto,
                Some(Ok(p)) => p,
                Some(Err(m)) => {
                    c.error("protocol", redact_serde(&m));
                    ProtocolMode::Auto
                }
            };
            for (k, v) in [("ca_file", &raw.ca_file), ("socket", &raw.socket)] {
                if let Some(v) = v {
                    c.template(k, v);
                }
            }
            let spec = RemoteSpec {
                url,
                headers,
                secret_headers,
                bearer_env: raw.bearer_env.clone(),
                auth,
                oauth: raw.oauth.clone(),
                ca_file: raw.ca_file.clone(),
                protocol,
                socket: raw.socket.clone(),
                fallback_sse: raw.fallback_sse.unwrap_or(true),
            };
            if kind == TransportKind::Sse {
                Transport::Sse(spec)
            } else {
                Transport::Http(spec)
            }
        }
    };

    // ${worktree} implies scope = "worktree".
    let uses_worktree = transport.stdio().is_some_and(|s| {
        std::iter::once(&s.command)
            .chain(&s.args)
            .chain(s.env.values())
            .chain(&s.env_file)
            .chain(&s.cwd)
            .any(|v| template_uses(v, &Piece::Worktree))
    });
    let scope = match (raw.scope, uses_worktree) {
        (Some(Scope::Shared), true) => {
            c.error(
                "scope",
                "is \"shared\" but the server uses ${worktree}, which needs one instance per worktree",
            )
            .fix = Some("remove scope, or set scope = \"worktree\"".into());
            Scope::Shared
        }
        (Some(s), _) => s,
        (None, true) => Scope::Worktree,
        (None, false) => Scope::Shared,
    };

    if c.has_errors() {
        return Err(c.problems);
    }
    let server = McpServer {
        name: name.to_owned(),
        origin: origin.clone(),
        enabled: raw.enabled.unwrap_or(true),
        enabled_origin: origin,
        transport,
        roles,
        startup: raw.startup.unwrap_or_default(),
        startup_timeout: Duration::from_secs(
            raw.startup_timeout_secs
                .unwrap_or(DEFAULT_STARTUP_TIMEOUT_SECS),
        ),
        call_timeout: Duration::from_secs(
            raw.call_timeout_secs.unwrap_or(DEFAULT_CALL_TIMEOUT_SECS),
        ),
        max_output_tokens: raw.max_output_tokens.unwrap_or(DEFAULT_MAX_OUTPUT_TOKENS),
        instructions: raw.instructions.unwrap_or(true),
        scope,
    };
    Ok((server, c.problems))
}

/// A literal secret in `headers`/`env` is an error: its value belongs in the keychain
/// (`secret_list`) or the environment (`${env:VAR}`).
fn literal_secret_check(
    c: &mut Checker<'_>,
    key: &str,
    name: &str,
    value: &str,
    secret_list: &str,
    table: &str,
) {
    let literal_name = looks_secret_name(name)
        && !value.trim().is_empty()
        && parse_template(value).is_ok_and(|p| p.iter().all(|p| matches!(p, Piece::Lit(_))));
    if literal_name || template_literal_secret(value) {
        c.error(
            key,
            "looks like a secret; secrets are never stored in config files",
        )
        .fix = Some(format!(
            "remove it from {table} and add `{name}` to {secret_list} (value in the keychain), \
             or reference the environment with ${{env:VAR}}"
        ));
    }
}

// ---------------------------------------------------------------------------------------------
// The merged config

/// Every configured MCP server after merging the user and repo files.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct McpConfig {
    /// Valid servers by name (enabled or not).
    pub servers: BTreeMap<String, McpServer>,
    /// Everything wrong; a server with an error is absent from `servers`.
    pub problems: Vec<McpProblem>,
    /// Names whose winning table had errors (for `mcp list`: shown as failed).
    pub broken: BTreeSet<String>,
}

impl McpConfig {
    /// Reads `[mcp]` from `paths.user_file` and `paths.repo_file()`. Missing files are empty;
    /// unreadable or unparsable ones are problems.
    pub fn load(paths: &ConfigPaths) -> Self {
        let mut problems = Vec::new();
        let user = paths.user_file.as_deref().and_then(|file| {
            match std::fs::read_to_string(file) {
                Ok(text) => match toml::from_str::<toml::Table>(&text) {
                    Ok(mut table) => match table.remove("mcp") {
                        Some(toml::Value::Table(t)) => Some((t, file.to_path_buf())),
                        Some(_) => {
                            problems.push(McpProblem::error(file, "mcp", "must be a table"));
                            None
                        }
                        None => None,
                    },
                    Err(e) => {
                        // Not `e`'s Display: it quotes the line, which may hold a secret.
                        problems.push(McpProblem::error(
                            file,
                            "mcp",
                            format!("parse: {}", e.message()),
                        ));
                        None
                    }
                },
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => None,
                Err(e) => {
                    problems.push(McpProblem::error(file, "mcp", format!("read: {e}")));
                    None
                }
            }
        });
        let repo = paths
            .repo_file()
            .and_then(|file| match RepoConfig::load(&file) {
                Ok(Some(rc)) => Some((rc.mcp, file)),
                Ok(None) => None,
                Err(e) => {
                    problems.push(McpProblem::error(&file, "mcp", redact_serde(&e.0)));
                    None
                }
            });
        let mut config = Self::from_tables(
            user.as_ref().map(|(t, f)| (t, f.as_path())),
            repo.as_ref().map(|(t, f)| (t, f.as_path())),
        );
        problems.append(&mut config.problems);
        config.problems = problems;
        config
    }

    /// Merges the user and repo `[mcp]` tables (each with its file).
    pub fn from_tables(
        user: Option<(&toml::Table, &Path)>,
        repo: Option<(&toml::Table, &Path)>,
    ) -> Self {
        let mut config = Self::default();
        // The user's own `enabled` per name (from a full table or a switch).
        let mut user_enabled: BTreeMap<String, (bool, PathBuf)> = BTreeMap::new();
        let mut switches: BTreeMap<String, PathBuf> = BTreeMap::new();
        let mut user_servers: BTreeMap<String, Result<McpServer, ()>> = BTreeMap::new();

        if let Some((table, file)) = user {
            for (name, value) in table {
                let origin = Origin {
                    layer: Layer::User,
                    file: file.to_path_buf(),
                };
                let raw = match parse_raw(name, value, file) {
                    Ok(raw) => raw,
                    Err(mut p) => {
                        config.problems.append(&mut p);
                        user_servers.insert(name.clone(), Err(()));
                        continue;
                    }
                };
                if let Some(e) = raw.enabled {
                    user_enabled.insert(name.clone(), (e, file.to_path_buf()));
                }
                if raw.is_switch() {
                    switches.insert(name.clone(), file.to_path_buf());
                    continue;
                }
                match build_server(name, raw, origin) {
                    Ok((s, mut warnings)) => {
                        config.problems.append(&mut warnings);
                        user_servers.insert(name.clone(), Ok(s));
                    }
                    Err(mut p) => {
                        config.problems.append(&mut p);
                        user_servers.insert(name.clone(), Err(()));
                    }
                }
            }
        }

        let mut repo_servers: BTreeMap<String, Result<McpServer, ()>> = BTreeMap::new();
        if let Some((table, file)) = repo {
            for (name, value) in table {
                let origin = Origin {
                    layer: Layer::Repo,
                    file: file.to_path_buf(),
                };
                match parse_server(name, value, origin) {
                    Ok((s, mut warnings)) => {
                        config.problems.append(&mut warnings);
                        repo_servers.insert(name.clone(), Ok(s));
                    }
                    Err(mut p) => {
                        config.problems.append(&mut p);
                        repo_servers.insert(name.clone(), Err(()));
                    }
                }
            }
        }

        for (name, file) in &switches {
            if !repo_servers.contains_key(name) {
                config.problems.push(
                    McpProblem::warning(
                        file,
                        format!("mcp.{name}"),
                        format!(
                            "only sets `enabled`, but no repo config defines a server named `{name}`"
                        ),
                    )
                    .fix("add type and command/url, or remove the table"),
                );
            }
        }

        // Repo replaces user per name.
        let mut merged = user_servers;
        merged.extend(repo_servers);
        for (name, server) in merged {
            match server {
                Ok(mut s) => {
                    if s.origin.layer == Layer::Repo
                        && let Some((enabled, file)) = user_enabled.get(&name)
                    {
                        s.enabled = *enabled;
                        s.enabled_origin = Origin {
                            layer: Layer::User,
                            file: file.clone(),
                        };
                    }
                    config.servers.insert(name, s);
                }
                Err(()) => {
                    config.broken.insert(name);
                }
            }
        }
        config
    }

    /// The enabled servers.
    pub fn enabled(&self) -> impl Iterator<Item = &McpServer> {
        self.servers.values().filter(|s| s.enabled)
    }

    /// The errors (servers left out).
    pub fn errors(&self) -> impl Iterator<Item = &McpProblem> {
        self.problems
            .iter()
            .filter(|p| p.severity == Severity::Error)
    }

    /// Every variable any configured server reads a secret from, for
    /// [`crate::agent::Runtime::secret_env`].
    pub fn secret_env_names(&self) -> BTreeSet<String> {
        self.servers
            .values()
            .flat_map(McpServer::secret_env_names)
            .collect()
    }
}

// ---------------------------------------------------------------------------------------------
// Tool names (D4)

/// 6 hex digits of FNV-1a 64 over `input`: stable across runs and platforms.
pub fn short_hash(input: &str) -> String {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in input.bytes() {
        h ^= u64::from(b);
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    format!("{:06x}", h & 0xff_ffff)
}

/// `tool` with every character outside `[A-Za-z0-9_-]` replaced by `_`.
pub fn sanitize_tool(tool: &str) -> String {
    tool.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '_' || c == '-' {
                c
            } else {
                '_'
            }
        })
        .collect()
}

/// The name a model sees for `server`'s `tool`: `mcp__<server>__<sanitised tool>`, at most
/// [`MAX_TOOL_NAME_LEN`] characters (longer names are cut and end in `_<6 hex>` of the original).
pub fn tool_name(server: &str, tool: &str) -> String {
    let full = format!("{TOOL_PREFIX}{server}__{}", sanitize_tool(tool));
    if full.len() <= MAX_TOOL_NAME_LEN {
        full
    } else {
        hashed_tool_name(server, tool)
    }
}

fn hashed_tool_name(server: &str, tool: &str) -> String {
    let full = format!("{TOOL_PREFIX}{server}__{}", sanitize_tool(tool));
    let suffix = format!("_{}", short_hash(&format!("{server}\0{tool}")));
    let keep = MAX_TOOL_NAME_LEN.min(full.len() + suffix.len()) - suffix.len();
    format!("{}{suffix}", &full[..keep.min(full.len())])
}

/// A session's map from model-facing tool names to (server, original tool name).
#[derive(Debug, Clone, Default)]
pub struct ToolNames {
    by_name: BTreeMap<String, (String, String)>,
    /// (server, lowercase tool) -> original tool, to catch case-only duplicates.
    folded: BTreeMap<(String, String), String>,
}

impl ToolNames {
    pub fn new() -> Self {
        Self::default()
    }

    /// Registers `server`'s `tool` and returns its model-facing name. Registering the same pair
    /// again returns the same name. A different tool whose sanitised name is taken gets the hashed
    /// form; two tools of one server that differ only in case are an error naming both.
    pub fn insert(&mut self, server: &str, tool: &str) -> Result<String, String> {
        if tool.is_empty() {
            return Err(format!("server `{server}` lists a tool with an empty name"));
        }
        let folded_key = (server.to_owned(), tool.to_lowercase());
        if let Some(existing) = self.folded.get(&folded_key) {
            if existing == tool {
                return Ok(self
                    .name_of(server, tool)
                    .expect("registered tool has a name")
                    .to_owned());
            }
            return Err(format!(
                "server `{server}` lists both `{existing}` and `{tool}`; tool names must differ by \
                 more than case"
            ));
        }
        let mut name = tool_name(server, tool);
        if self.by_name.contains_key(&name) {
            name = hashed_tool_name(server, tool);
            if self.by_name.contains_key(&name) {
                return Err(format!(
                    "server `{server}`: tool `{tool}` collides with another tool's name `{name}`"
                ));
            }
        }
        self.by_name
            .insert(name.clone(), (server.to_owned(), tool.to_owned()));
        self.folded.insert(folded_key, tool.to_owned());
        Ok(name)
    }

    /// The (server, original tool) behind a model-facing name.
    pub fn lookup(&self, name: &str) -> Option<(&str, &str)> {
        self.by_name
            .get(name)
            .map(|(s, t)| (s.as_str(), t.as_str()))
    }

    /// The model-facing name of a registered tool.
    pub fn name_of(&self, server: &str, tool: &str) -> Option<&str> {
        self.by_name
            .iter()
            .find(|(_, (s, t))| s == server && t == tool)
            .map(|(n, _)| n.as_str())
    }

    /// Forgets every tool of `server` (it disconnected or its list changed).
    pub fn remove_server(&mut self, server: &str) {
        self.by_name.retain(|_, (s, _)| s != server);
        self.folded.retain(|(s, _), _| s != server);
    }

    pub fn len(&self) -> usize {
        self.by_name.len()
    }

    pub fn is_empty(&self) -> bool {
        self.by_name.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const USER: &str = "/home/u/.config/amp-harness/config.toml";
    const REPO: &str = "/r/.amp/harness/config.toml";

    fn table(text: &str) -> toml::Table {
        toml::from_str::<toml::Table>(text)
            .unwrap()
            .remove("mcp")
            .and_then(|v| v.as_table().cloned())
            .unwrap_or_default()
    }

    fn user(text: &str) -> McpConfig {
        McpConfig::from_tables(Some((&table(text), Path::new(USER))), None)
    }

    fn layered(user_text: &str, repo_text: &str) -> McpConfig {
        McpConfig::from_tables(
            Some((&table(user_text), Path::new(USER))),
            Some((&table(repo_text), Path::new(REPO))),
        )
    }

    fn no_env(_: &str) -> Option<String> {
        None
    }

    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/mcp-config")
            .join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    const FULL: &str = r#"
[mcp.github]
type = "http"
url = "https://api.githubcopilot.com/mcp/"
headers = { "X-Org" = "simstech", "X-Trace" = "${env:TRACE_ID}" }
secret_headers = ["X-Api-Key"]
auth = "auto"
oauth = { client_id = "abc", scopes = ["repo"], callback_port = 8765, client_secret = true }
ca_file = "corp.pem"
protocol = "legacy"
socket = "/run/gh.sock"
fallback_sse = false
roles = ["worker-*", "amp-reviewer"]
startup = "eager"
startup_timeout_secs = 5
call_timeout_secs = 60
max_output_tokens = 1000
instructions = false

[mcp.fs]
type = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "${worktree}"]
env = { LOG_LEVEL = "info", API_URL = "${env:API_URL}" }
secret_env = ["BRAVE_API_KEY"]
env_file = ".env.mcp"
env_inherit = "minimal"
cwd = "${worktree}"

[mcp.legacy]
type = "sse"
url = "http://localhost:9000/sse"
bearer_env = "LEGACY_TOKEN"
"#;

    #[test]
    fn full_schema_parses() {
        let c = user(FULL);
        assert_eq!(c.problems, vec![], "{:?}", c.problems);
        assert_eq!(c.servers.len(), 3);

        let gh = &c.servers["github"];
        assert_eq!(gh.transport.kind(), TransportKind::Http);
        assert_eq!(gh.origin.layer, Layer::User);
        let r = gh.transport.remote().unwrap();
        assert_eq!(r.headers.len(), 2);
        assert_eq!(r.secret_headers, ["X-Api-Key"]);
        assert_eq!(r.auth, AuthMode::Auto);
        let o = r.oauth.as_ref().unwrap();
        assert_eq!(
            (o.client_id.as_deref(), o.callback_port, o.client_secret),
            (Some("abc"), 8765, true)
        );
        assert_eq!(r.protocol, ProtocolMode::Legacy);
        assert!(!r.fallback_sse);
        assert_eq!(gh.startup, Startup::Eager);
        assert_eq!(gh.startup_timeout, Duration::from_secs(5));
        assert_eq!(gh.call_timeout, Duration::from_secs(60));
        assert_eq!(gh.max_output_tokens, 1000);
        assert!(!gh.instructions);
        assert_eq!(gh.scope, Scope::Shared);
        assert_eq!(
            gh.keychain_accounts(),
            [
                "mcp:github:header:x-api-key",
                "mcp:github:oauth-client-secret"
            ]
        );

        let fs = &c.servers["fs"];
        let s = fs.transport.stdio().unwrap();
        assert_eq!(s.command, "npx");
        assert_eq!(s.env_inherit, EnvInherit::Minimal);
        // ${worktree} implies one instance per worktree.
        assert_eq!(fs.scope, Scope::Worktree);
        assert_eq!(fs.keychain_accounts(), ["mcp:fs:env:BRAVE_API_KEY"]);
        assert!(fs.uses_keychain());
        // Defaults.
        assert!(fs.enabled && fs.instructions);
        assert_eq!(fs.roles, ["*"]);
        assert_eq!(fs.startup, Startup::Background);
        assert_eq!(
            fs.call_timeout,
            Duration::from_secs(DEFAULT_CALL_TIMEOUT_SECS)
        );
        assert_eq!(fs.max_output_tokens, DEFAULT_MAX_OUTPUT_TOKENS);

        let legacy = &c.servers["legacy"];
        assert_eq!(legacy.transport.kind(), TransportKind::Sse);
        assert!(!legacy.uses_keychain());

        assert_eq!(
            c.secret_env_names().into_iter().collect::<Vec<_>>(),
            ["API_URL", "BRAVE_API_KEY", "LEGACY_TOKEN", "TRACE_ID"]
        );
    }

    #[test]
    fn type_is_inferred_from_command_or_url() {
        let c = user(
            "[mcp.a]\ncommand = \"srv\"\n[mcp.b]\nurl = \"https://x.test/mcp\"\n[mcp.c]\nenabled = true\nroles = [\"*\"]\n",
        );
        assert_eq!(c.servers["a"].transport.kind(), TransportKind::Stdio);
        assert_eq!(c.servers["b"].transport.kind(), TransportKind::Http);
        assert!(c.broken.contains("c"));
        assert!(c.problems[0].to_string().contains("`mcp.c.type`: missing"));
    }

    /// (table, key the error names, text the message or fix contains)
    #[test]
    fn errors_name_file_and_key_and_never_echo_values() {
        let secret = "s3cr3tVALUE0123456789";
        let cases: Vec<(String, &str, &str)> = vec![
            ("[mcp.x]\ntype = \"stdio\"\ncommand = \"a\"\nbogus = 1\n".into(), "mcp.x", "unknown field `bogus`"),
            ("[mcp.x]\ntype = \"ftp\"\n".into(), "mcp.x", "unknown variant `ftp`"),
            ("[mcp.x]\ntype = \"stdio\"\ncommand = \"a\"\nurl = \"https://x.test\"\n".into(), "mcp.x.url", "does not apply to a stdio server"),
            ("[mcp.x]\ntype = \"http\"\nurl = \"https://x.test\"\ncommand = \"a\"\n".into(), "mcp.x.command", "does not apply to a http server"),
            ("[mcp.x]\ntype = \"sse\"\nurl = \"https://x.test\"\nsocket = \"/s\"\n".into(), "mcp.x.socket", "does not apply to a sse server"),
            ("[mcp.x]\ntype = \"stdio\"\n".into(), "mcp.x.command", "needs a command"),
            ("[mcp.x]\ntype = \"http\"\n".into(), "mcp.x.url", "needs a url"),
            ("[mcp.x]\ntype = \"http\"\nurl = \"ftp://x\"\n".into(), "mcp.x.url", "http:// or https://"),
            ("[mcp.amp]\ncommand = \"a\"\n".into(), "mcp.amp", "reserved"),
            ("[mcp.harness]\ncommand = \"a\"\n".into(), "mcp.harness", "reserved"),
            ("[mcp.amp_x]\ncommand = \"a\"\n".into(), "mcp.amp_x", "reserved"),
            ("[mcp.GitHub]\ncommand = \"a\"\n".into(), "mcp.GitHub", "rename it to `github`"),
            ("[mcp.\"my server\"]\ncommand = \"a\"\n".into(), "mcp.my server", "rename it to `my-server`"),
            (format!("[mcp.x]\ntype = \"http\"\nurl = \"https://x.test\"\nheaders = {{ Authorization = \"Bearer {secret}\" }}\n"), "mcp.x.headers.Authorization", "secret_headers"),
            (format!("[mcp.x]\ntype = \"http\"\nurl = \"https://x.test\"\nheaders = {{ X-Thing = \"ghp_{secret}\" }}\n"), "mcp.x.headers.X-Thing", "never stored in config files"),
            (format!("[mcp.x]\ncommand = \"a\"\nenv = {{ GITHUB_TOKEN = \"{secret}\" }}\n"), "mcp.x.env.GITHUB_TOKEN", "add `GITHUB_TOKEN` to secret_env"),
            (format!("[mcp.x]\ncommand = \"a\"\nargs = [\"--key\", \"sk-{secret}\"]\n"), "mcp.x.args", "looks like a credential"),
            (format!("[mcp.x]\nurl = \"https://x.test/mcp?api_key={secret}\"\n"), "mcp.x.url", "query string holds a credential"),
            (format!("[mcp.x]\ntype = \"http\"\nheaders = \"Bearer {secret}\"\nurl = \"https://x.test\"\n"), "mcp.x", "invalid type: string"),
            ("[mcp.x]\ncommand = \"a\"\nargs = [\"${nope}\"]\n".into(), "mcp.x.args", "unknown reference `${nope}`"),
            ("[mcp.x]\ncommand = \"a\"\ncwd = \"${env:A\"\n".into(), "mcp.x.cwd", "unterminated"),
            ("[mcp.x]\ncommand = \"a\"\nenv = { A = \"${env:1BAD}\" }\n".into(), "mcp.x.env.A", "not a valid environment variable name"),
            ("[mcp.x]\ncommand = \"a\"\ncwd = \"${worktree}\"\nscope = \"shared\"\n".into(), "mcp.x.scope", "set scope = \"worktree\""),
            ("[mcp.x]\nurl = \"https://x.test\"\nscope = \"worktree\"\n".into(), "mcp.x.scope", "always shared"),
            ("[mcp.x]\nurl = \"https://x.test\"\nheaders = { A = \"${worktree}\" }\n".into(), "mcp.x.headers.A", "for stdio servers"),
            ("[mcp.x]\ncommand = \"a\"\ncall_timeout_secs = 0\n".into(), "mcp.x.call_timeout_secs", "greater than 0"),
            ("[mcp.x]\nurl = \"https://x.test\"\nprotocol = \"2024-11-05\"\n".into(), "mcp.x.protocol", "unknown MCP protocol"),
            ("[mcp.x]\nurl = \"https://x.test\"\nauth = \"none\"\noauth = { client_id = \"c\" }\n".into(), "mcp.x.oauth", "auth = \"none\""),
            ("[mcp.x]\nurl = \"https://x.test\"\noauth = { client_secret = true }\n".into(), "mcp.x.oauth.client_secret", "needs oauth.client_id"),
            ("[mcp.x]\nurl = \"https://x.test\"\noauth = { client_id = \"c\", extra = 1 }\n".into(), "mcp.x", "unknown field `extra`"),
            ("[mcp.x]\nurl = \"https://x.test\"\nbearer_env = \"T\"\nsecret_headers = [\"authorization\"]\n".into(), "mcp.x.bearer_env", "keep one of them"),
            ("[mcp.x]\nurl = \"https://x.test\"\nheaders = { X-A = \"1\" }\nsecret_headers = [\"x-a\"]\n".into(), "mcp.x.secret_headers", "set twice"),
            ("[mcp.x]\ncommand = \"a\"\nenv = { K = \"1\" }\nsecret_env = [\"K\"]\n".into(), "mcp.x.secret_env", "remove env.K"),
            ("[mcp.x]\ncommand = \"a\"\nroles = [\"\"]\n".into(), "mcp.x.roles", "must not be empty"),
            ("[mcp]\nx = 1\n".into(), "mcp.x", "must be a table"),
        ];
        for (text, key, want) in cases {
            let c = user(&text);
            assert!(c.servers.is_empty(), "{text}: should not load");
            let errors: Vec<String> = c.errors().map(ToString::to_string).collect();
            let hit = errors
                .iter()
                .find(|e| e.contains(&format!("`{key}`")) && e.contains(want));
            assert!(
                hit.is_some(),
                "{text}\nwant `{key}` + {want:?}, got {errors:#?}"
            );
            for e in &errors {
                assert!(e.starts_with(USER), "{e}");
                assert!(!e.contains(secret), "secret echoed: {e}");
            }
        }
    }

    #[test]
    fn non_secret_values_and_references_are_fine() {
        let c = user(
            r#"
[mcp.x]
url = "https://x.test/mcp?team=core&token=${env:TOK}"
headers = { Authorization = "Bearer ${env:TOK}", X-Api-Key = "${env:KEY}", Accept = "application/json" }
[mcp.y]
command = "srv"
args = ["--token-file", "/run/tok", "$${literal}", "cost=$5"]
env = { API_TOKEN = "${env:API_TOKEN}", LOG_LEVEL = "debug", EMPTY_TOKEN = "" }
"#,
        );
        assert_eq!(c.errors().count(), 0, "{:#?}", c.problems);
        assert_eq!(
            c.secret_env_names().into_iter().collect::<Vec<_>>(),
            ["API_TOKEN", "KEY", "TOK"]
        );
    }

    #[test]
    fn empty_roles_is_a_warning() {
        let c = user("[mcp.x]\ncommand = \"a\"\nroles = []\n");
        assert!(c.servers.contains_key("x"));
        assert_eq!(c.problems.len(), 1);
        assert_eq!(c.problems[0].severity, Severity::Warning);
        assert!(!c.servers["x"].allows_role("manager"));
    }

    #[test]
    fn repo_replaces_user_and_user_enabled_wins() {
        let c = layered(
            "[mcp.a]\ncommand = \"user-a\"\n[mcp.b]\ncommand = \"user-b\"\n[mcp.c]\nenabled = false\n[mcp.d]\nenabled = true\n[mcp.e]\nenabled = false\n",
            "[mcp.a]\nurl = \"https://repo.test/a\"\n[mcp.c]\ncommand = \"repo-c\"\n[mcp.d]\ncommand = \"repo-d\"\nenabled = false\n",
        );
        assert_eq!(c.errors().count(), 0, "{:#?}", c.problems);
        // Repo replaces the user's table of the same name.
        let a = &c.servers["a"];
        assert_eq!(a.origin.layer, Layer::Repo);
        assert_eq!(a.transport.kind(), TransportKind::Http);
        // User-only servers stay.
        assert_eq!(c.servers["b"].origin.layer, Layer::User);
        // A user switch turns a repo server off, or back on.
        let cc = &c.servers["c"];
        assert!(!cc.enabled);
        assert_eq!(
            (cc.origin.layer, cc.enabled_origin.layer),
            (Layer::Repo, Layer::User)
        );
        assert!(c.servers["d"].enabled);
        assert_eq!(
            c.enabled().map(|s| s.name.as_str()).collect::<Vec<_>>(),
            ["a", "b", "d"]
        );
        // A switch for a server no repo defines is a warning, and not a server.
        assert!(!c.servers.contains_key("e"));
        let w: Vec<_> = c
            .problems
            .iter()
            .filter(|p| p.severity == Severity::Warning)
            .collect();
        assert_eq!(w.len(), 1);
        assert_eq!(w[0].key, "mcp.e");
        assert!(w[0].to_string().starts_with("warning: "));
    }

    #[test]
    fn a_broken_repo_table_breaks_that_name() {
        let c = layered(
            "[mcp.a]\ncommand = \"ok\"\n",
            "[mcp.a]\ncommand = \"x\"\nurl = \"https://x.test\"\ntype = \"stdio\"\n",
        );
        assert!(c.servers.is_empty());
        assert!(c.broken.contains("a"));
        let e = c.errors().next().unwrap().to_string();
        assert!(e.starts_with(REPO) && e.contains("`mcp.a.url`"), "{e}");
    }

    #[test]
    fn templates_expand() {
        let env = |k: &str| match k {
            "SET" => Some("v".to_owned()),
            "EMPTY" => Some(String::new()),
            _ => None,
        };
        let ctx = ExpandContext {
            env: &env,
            repo: Some("/repo".into()),
            worktree: Some("/wt/1".into()),
            home: Some("/home/u".into()),
            config_dir: Some("/home/u/.config/amp-harness".into()),
        };
        for (template, want) in [
            ("plain", "plain"),
            ("${env:SET}", "v"),
            ("a-${env:SET}-b", "a-v-b"),
            ("${env:UNSET:-dflt}", "dflt"),
            ("${env:EMPTY:-dflt}", "dflt"),
            ("${env:EMPTY}", ""),
            ("${env:SET:-dflt}", "v"),
            ("${repo}/x", "/repo/x"),
            ("${worktree}", "/wt/1"),
            ("${home}/.npmrc", "/home/u/.npmrc"),
            ("${config_dir}/f", "/home/u/.config/amp-harness/f"),
            ("$${env:SET}", "${env:SET}"),
            ("$5 and $", "$5 and $"),
        ] {
            assert_eq!(ctx.expand(template).unwrap(), want, "{template}");
        }
        let e = ctx.expand("${env:UNSET}").unwrap_err();
        assert!(e.contains("UNSET is not set"), "{e}");
        let shared = ExpandContext {
            worktree: None,
            ..ctx
        };
        assert!(
            shared
                .expand("${worktree}")
                .unwrap_err()
                .contains("no value here")
        );
    }

    #[test]
    fn expand_context_from_paths() {
        let env = |k: &str| (k == "HOME").then(|| "/home/u".to_owned());
        let paths =
            ConfigPaths::resolve(Some("/cfg/config.toml".into()), Some("/cfg".into()), &env)
                .with_repo(Some(Path::new("/repo")));
        let ctx = ExpandContext::from_paths(&paths, &env, Some(Path::new("/wt")));
        assert_eq!(ctx.repo.as_deref(), Some(Path::new("/repo")));
        assert_eq!(ctx.worktree.as_deref(), Some(Path::new("/wt")));
        assert_eq!(ctx.home.as_deref(), Some(Path::new("/home/u")));
        assert_eq!(ctx.config_dir.as_deref(), Some(Path::new("/cfg")));
    }

    #[test]
    fn resolve_stdio_reads_env_file_and_keeps_secrets_as_references() {
        let dir = scratch("resolve-stdio");
        std::fs::write(
            dir.join(".env.mcp"),
            "# comment\nexport FROM_FILE=\"one\"\nOVERRIDDEN=file\n\nQUOTED='two'\n",
        )
        .unwrap();
        let file = dir.join(".amp/harness/config.toml");
        let c = McpConfig::from_tables(
            None,
            Some((
                &table(
                    "[mcp.s]\ncommand = \"${repo}/bin/srv\"\nargs = [\"--root\", \"${worktree}\"]\nenv = { OVERRIDDEN = \"${env:V:-env}\" }\nenv_file = \".env.mcp\"\ncwd = \"sub\"\nsecret_env = [\"API_KEY\"]\n",
                ),
                &file,
            )),
        );
        assert_eq!(c.errors().count(), 0, "{:#?}", c.problems);
        let ctx = ExpandContext {
            env: &no_env,
            repo: Some(dir.clone()),
            worktree: Some("/wt".into()),
            home: None,
            config_dir: None,
        };
        let Resolved::Stdio(s) = c.servers["s"].resolve(&ctx).unwrap() else {
            panic!("not stdio")
        };
        assert_eq!(s.command, format!("{}/bin/srv", dir.display()));
        assert_eq!(s.args, ["--root", "/wt"]);
        assert_eq!(s.cwd.as_deref(), Some(dir.join("sub").as_path()));
        assert_eq!(
            s.env
                .iter()
                .map(|(k, v)| format!("{k}={v}"))
                .collect::<Vec<_>>(),
            ["FROM_FILE=one", "OVERRIDDEN=env", "QUOTED=two"]
        );
        assert_eq!(
            s.secrets,
            [SecretRef {
                target: SecretTarget::Env("API_KEY".into()),
                source: SecretSource::Keychain {
                    account: "mcp:s:env:API_KEY".into()
                },
            }]
        );
        assert_eq!(s.secrets[0].describe(), "keychain mcp:s:env:API_KEY");
        // Debug lists env names, not values.
        let debug = format!("{s:?}");
        assert!(
            debug.contains("FROM_FILE") && !debug.contains("one"),
            "{debug}"
        );

        // Without a worktree (the manager), ${worktree} fails naming the key.
        let manager = ExpandContext {
            worktree: None,
            ..ctx
        };
        let e = c.servers["s"].resolve(&manager).unwrap_err().to_string();
        assert!(
            e.contains("`mcp.s.args`") && e.contains("${worktree}"),
            "{e}"
        );

        // A broken env_file names file and line, not the value.
        std::fs::write(dir.join(".env.mcp"), "OK=1\nnot-an-assignment-s3cr3t\n").unwrap();
        let e = c.servers["s"]
            .resolve(&ExpandContext {
                worktree: Some("/wt".into()),
                ..manager
            })
            .unwrap_err()
            .to_string();
        assert!(
            e.contains(".env.mcp:2: expected KEY=VALUE") && !e.contains("s3cr3t"),
            "{e}"
        );
    }

    #[test]
    fn resolve_remote_collects_secret_references() {
        let c = user(
            r#"
[mcp.h]
url = "https://${env:HOST:-mcp.test}/mcp"
headers = { X-Org = "o", X-Trace = "${env:TRACE}" }
secret_headers = ["X-Api-Key"]
bearer_env = "H_TOKEN"
oauth = { client_id = "cid", client_secret = true }
ca_file = "ca.pem"
[mcp.s]
type = "sse"
url = "http://localhost:1/sse"
"#,
        );
        assert_eq!(c.errors().count(), 0, "{:#?}", c.problems);
        let env = |k: &str| match k {
            "TRACE" => Some("t-1".to_owned()),
            "H_TOKEN" => Some("tok-value".to_owned()),
            _ => None,
        };
        let ctx = ExpandContext {
            env: &env,
            repo: None,
            worktree: None,
            home: None,
            config_dir: None,
        };
        let Resolved::Http(r) = c.servers["h"].resolve(&ctx).unwrap() else {
            panic!("not http")
        };
        assert_eq!(r.url, "https://mcp.test/mcp");
        assert_eq!(r.headers["X-Trace"], "t-1");
        assert_eq!(
            r.ca_file.as_deref(),
            Some(Path::new("/home/u/.config/amp-harness/ca.pem"))
        );
        assert!(r.fallback_sse);
        let described: Vec<_> = r
            .secrets
            .iter()
            .map(|s| (s.target.clone(), s.describe()))
            .collect();
        assert_eq!(
            described,
            [
                (
                    SecretTarget::Header("X-Api-Key".into()),
                    "keychain mcp:h:header:x-api-key".into()
                ),
                (SecretTarget::Bearer, "env H_TOKEN".into()),
                (
                    SecretTarget::OAuthClientSecret,
                    "keychain mcp:h:oauth-client-secret".into()
                ),
            ]
        );
        // Env-sourced secrets are fetched without the keychain.
        assert_eq!(
            r.secrets[1]
                .fetch("unused-test-service", &env)
                .unwrap()
                .as_deref(),
            Some("tok-value")
        );
        let debug = format!("{r:?}");
        assert!(
            !debug.contains("t-1") && debug.contains("X-Trace"),
            "{debug}"
        );
        assert!(matches!(
            c.servers["s"].resolve(&ctx).unwrap(),
            Resolved::Sse(_)
        ));
        // An env URL that expands to something else is caught at connect time.
        let user2 = user("[mcp.u]\nurl = \"${env:U}\"\n");
        let env_u = |k: &str| (k == "U").then(|| "file:///etc/passwd".to_owned());
        let e = user2.servers["u"]
            .resolve(&ExpandContext { env: &env_u, ..ctx })
            .unwrap_err();
        assert_eq!(e.key, "mcp.u.url");
    }

    #[test]
    fn roles_filter_globs_with_or_without_amp_prefix() {
        let c = user("[mcp.x]\ncommand = \"a\"\nroles = [\"worker-*\", \"amp-reviewer\"]\n");
        let s = &c.servers["x"];
        for (role, want) in [
            ("worker-backend", true),
            ("amp-worker-frontend", true),
            ("reviewer", true),
            ("amp-reviewer", true),
            ("manager", false),
            ("amp-manager", false),
        ] {
            assert_eq!(s.allows_role(role), want, "{role}");
        }
        assert!(user("[mcp.y]\ncommand = \"a\"\n").servers["y"].allows_role("amp-manager"));
    }

    #[test]
    fn server_names() {
        for ok in ["github", "a", "fs-2", "0x", &"a".repeat(32)] {
            assert!(check_server_name(ok).is_ok(), "{ok}");
        }
        for bad in [
            "",
            "-a",
            "a_b",
            "A",
            "a.b",
            "amp",
            "AMP",
            "harness",
            "amp_tools",
            &"a".repeat(33),
        ] {
            assert!(check_server_name(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn redaction_drops_quoted_strings() {
        assert_eq!(
            redact_serde(r#"invalid type: string "Bearer x\"y", expected a map"#),
            "invalid type: string, expected a map"
        );
        assert_eq!(redact_serde("no strings here"), "no strings here");
    }

    #[test]
    fn tool_names_sanitise_cap_and_map_back() {
        assert_eq!(
            tool_name("github", "list_issues"),
            "mcp__github__list_issues"
        );
        assert_eq!(tool_name("fs", "read.file/v2 ü"), "mcp__fs__read_file_v2__");
        let long = "t".repeat(80);
        let capped = tool_name("github", &long);
        assert_eq!(capped.len(), MAX_TOOL_NAME_LEN);
        assert!(capped.starts_with("mcp__github__ttt"));
        assert_eq!(&capped[57..58], "_");
        assert!(capped[58..].chars().all(|c| c.is_ascii_hexdigit()));
        // Stable, and different for different originals.
        assert_eq!(capped, tool_name("github", &long));
        assert_ne!(capped, tool_name("github", &format!("{long}x")));
        // FNV-1a 64 offset basis 0xcbf29ce484222325: its low 24 bits.
        assert_eq!(short_hash(""), "222325");
        assert_eq!(short_hash("a").len(), 6);

        let mut names = ToolNames::new();
        assert!(names.is_empty());
        let a = names.insert("fs", "read.file").unwrap();
        assert_eq!(a, "mcp__fs__read_file");
        // Same pair again: same name.
        assert_eq!(names.insert("fs", "read.file").unwrap(), a);
        // A different tool that sanitises to the same name gets the hashed form.
        let b = names.insert("fs", "read_file").unwrap();
        assert_ne!(a, b);
        assert!(
            b.starts_with("mcp__fs__read_file_") && b.len() == a.len() + 7,
            "{b}"
        );
        // Case-only duplicates are an error naming both.
        let e = names.insert("fs", "READ.FILE").unwrap_err();
        assert!(
            e.contains("`read.file`") && e.contains("`READ.FILE`"),
            "{e}"
        );
        // Other servers are separate namespaces.
        assert_eq!(
            names.insert("gh", "read.file").unwrap(),
            "mcp__gh__read_file"
        );
        assert_eq!(names.lookup(&a), Some(("fs", "read.file")));
        assert_eq!(names.lookup(&b), Some(("fs", "read_file")));
        assert_eq!(names.name_of("gh", "read.file"), Some("mcp__gh__read_file"));
        assert!(names.insert("fs", "").is_err());
        assert_eq!(names.len(), 3);
        names.remove_server("fs");
        assert_eq!(names.len(), 1);
        assert_eq!(names.lookup(&a), None);
        // After removal the case-folded entry is gone too.
        assert!(names.insert("fs", "READ.FILE").is_ok());
        // Every name fits the providers' charset and limit.
        for n in [&a, &b, &capped] {
            assert!(n.len() <= MAX_TOOL_NAME_LEN);
            assert!(
                n.chars()
                    .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-')
            );
        }
    }

    #[test]
    fn load_reads_user_and_repo_files() {
        let dir = scratch("load");
        let user_file = dir.join("home/config.toml");
        let repo = dir.join("repo");
        std::fs::create_dir_all(user_file.parent().unwrap()).unwrap();
        std::fs::create_dir_all(repo.join(".amp/harness")).unwrap();
        std::fs::write(
            &user_file,
            "[server]\nurl = \"https://amp.test/mcp\"\n\n[mcp.mine]\ncommand = \"srv\"\n\n[mcp.shared]\nenabled = false\n",
        )
        .unwrap();
        std::fs::write(
            repo.join(".amp/harness/config.toml"),
            "[mcp.shared]\nurl = \"https://repo.test/mcp\"\nbearer_env = \"REPO_TOKEN\"\n",
        )
        .unwrap();
        let env = |_: &str| None;
        let paths = ConfigPaths::resolve(Some(user_file.clone()), Some(dir.join("home")), &env)
            .with_repo(Some(&repo));
        let c = McpConfig::load(&paths);
        assert_eq!(c.problems, vec![]);
        assert_eq!(c.servers.len(), 2);
        assert!(!c.servers["shared"].enabled);
        assert_eq!(
            c.servers["shared"].origin.file,
            repo.join(".amp/harness/config.toml")
        );
        assert_eq!(
            c.secret_env_names().into_iter().collect::<Vec<_>>(),
            ["REPO_TOKEN"]
        );

        // A repo file with a forbidden table is a problem, not a panic; the user layer still loads.
        std::fs::write(
            repo.join(".amp/harness/config.toml"),
            "[server]\nurl = \"https://evil.test\"\n",
        )
        .unwrap();
        let c = McpConfig::load(&paths);
        assert!(c.servers.contains_key("mine"));
        assert!(
            c.errors()
                .any(|p| p.message.contains("`server` is not allowed"))
        );

        // Unparsable user file: a problem without the offending line.
        std::fs::write(
            &user_file,
            "[mcp.x]\nheaders = { Authorization = \"Bearer s3cr3t\" \n",
        )
        .unwrap();
        let c = McpConfig::load(&paths.clone().with_repo(None));
        let e = c.errors().next().unwrap().to_string();
        assert!(e.contains("parse:") && !e.contains("s3cr3t"), "{e}");

        // Missing files: nothing.
        let none = ConfigPaths::resolve(Some(dir.join("nope.toml")), None, &env);
        assert_eq!(McpConfig::load(&none), McpConfig::default());
    }

    #[test]
    fn secret_detection() {
        for name in [
            "Authorization",
            "X-Api-Key",
            "GITHUB_TOKEN",
            "DB_PASSWORD",
            "AWS_SECRET_ACCESS_KEY",
            "Cookie",
            "KEY",
        ] {
            assert!(looks_secret_name(name), "{name}");
        }
        for name in ["X-Org", "LOG_LEVEL", "API_URL", "KEYBOARD", "Accept"] {
            assert!(!looks_secret_name(name), "{name}");
        }
        for value in [
            "Bearer abc",
            "basic Zm9v",
            "sk-abc",
            "ghp_abc",
            "github_pat_x",
            "AKIAIOSFODNN7",
        ] {
            assert!(looks_secret_value(value), "{value}");
        }
        for value in ["Bearer", "Bearer ", "sk-", "simstech", "application/json"] {
            assert!(!looks_secret_value(value), "{value}");
        }
    }
}
