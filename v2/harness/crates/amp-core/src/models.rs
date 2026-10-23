//! Providers, models and the model each role runs on (D9 as changed by #2091: several providers at
//! once, the way opencode does it).
//!
//! Two layers, both in the same TOML schema:
//! 1. the shipped defaults, [`DEFAULTS_TOML`] (`crates/amp-core/models.toml`): the Anthropic and
//!    OpenRouter providers, their models, the opencode roster as `[roles.*]` and the alternative
//!    `[profiles.anthropic]`;
//! 2. the user's `config.toml` (`--config`, `AMP_HARNESS_CONFIG`, `--config-dir`, else
//!    `$XDG_CONFIG_HOME/amp-harness/config.toml`, else `~/.config/amp-harness/config.toml`; see
//!    [`amp_mcp::config_paths`]). Its `[providers.*]`, `[roles.*]` and `[profiles.*]` are merged
//!    over the defaults, and its `[models]` table picks the assignment mode;
//! 3. the repo's `<repo>/.amp/harness/config.toml` ([`crate::repo_config`]): only `[roles.*]` and
//!    `[models] profile`, merged over the user file.
//!
//! There is no default provider. Each role names its own `provider/model` (the model id may contain
//! `/`, e.g. `openrouter/xiaomi/mimo-v2.6-pro`; the provider is everything before the first `/`).
//!
//! A role's assignment, lowest to highest precedence:
//! 1. `[roles.<role>]` in the defaults (the opencode roster);
//! 2. the selected profile's `[profiles.<name>.roles.<role>]`, or, in inherit mode, the one
//!    `[models] inherit = "provider/model"` for every role;
//! 3. `[roles.<role>]` in config.toml;
//! 4. `[roles.<role>]` in the repo config.
//!
//! The mode comes from `AMP_HARNESS_PROFILE`, then the repo's `[models] profile`, then the user's,
//! then `opencode` (the defaults as shipped). `inherit` is a mode, not a profile name.
//!
//! When a layer changes a role's `model`, the `extra_body` of the layers below is dropped (it was
//! written for the other model), while `thinking` carries over unless the layer sets it too.
//!
//! Keys are never read from these files: see [`crate::providers`].

use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    path::{Path, PathBuf},
    time::Duration,
};

use rpi_ai::{
    Api, InputModality, Model, ModelCost, ModelCostRates, ThinkingLevel,
    model::{AnthropicMessagesCompat, OpenaiCompletionsCompat, StreamingProtocolCompat},
};
use serde::Deserialize;
use serde_json::Value;

use crate::repo_config::RepoConfig;

/// The shipped defaults (embedded).
pub const DEFAULTS_TOML: &str = include_str!("../models.toml");
/// Overrides `[models] profile` (handy for one run, CI and the smoke test).
pub const ENV_PROFILE: &str = "AMP_HARNESS_PROFILE";
/// The defaults as shipped: the opencode roster.
pub const DEFAULT_PROFILE: &str = "opencode";
/// The mode where every role uses `[models] inherit`.
pub const INHERIT: &str = "inherit";
/// Thinking level for a role that names none (and whose model reasons).
pub const DEFAULT_THINKING: ThinkingLevel = ThinkingLevel::Medium;
/// The role whose model an unconfigured role falls back to, unless `[models] fallback` says
/// otherwise (#2234, D11).
pub const DEFAULT_FALLBACK_ROLE: &str = "worker-backend";

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ModelsError(pub String);

impl fmt::Display for ModelsError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ModelsError {}

fn err<T>(message: impl Into<String>) -> Result<T, ModelsError> {
    Err(ModelsError(message.into()))
}

// ------------------------------------------------------------------------------------------------
// File schema (shared by the defaults and config.toml). Every field is optional so that a layer can
// override one field of a provider or model.
// ------------------------------------------------------------------------------------------------

/// The model-related tables of a TOML file. Other top-level tables (`[server]`, ...) are ignored;
/// inside these tables unknown keys are errors, so typos surface.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct ModelsFile {
    #[serde(default)]
    pub models: ModelsSection,
    #[serde(default)]
    pub providers: BTreeMap<String, ProviderSpec>,
    #[serde(default)]
    pub roles: BTreeMap<String, RoleSpec>,
    #[serde(default)]
    pub profiles: BTreeMap<String, ProfileSpec>,
}

/// `[models]`: which assignment mode is active.
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelsSection {
    /// `opencode` (default), `inherit`, or a profile name (`anthropic`, or one defined in config.toml).
    pub profile: Option<String>,
    /// `provider/model` every role uses when `profile = "inherit"`.
    pub inherit: Option<String>,
    /// Thinking level for every role in inherit mode.
    pub inherit_thinking: Option<String>,
    /// The model a role with no `[roles.<id>]` runs on when its agent file names no usable model
    /// and no `based_on` role (#2234, D11): a role name (`worker-backend`, the default) or a
    /// `provider/model`.
    pub fallback: Option<String>,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProviderSpec {
    /// Display name (`amp-harness connect --list`, the chat). Default: the id.
    pub name: Option<String>,
    /// `anthropic-messages`, `openai-completions` or `faux` (scripted, offline; tests and smoke).
    pub api: Option<String>,
    pub base_url: Option<String>,
    /// Environment variable holding the key. The keychain is the fallback.
    pub key_env: Option<String>,
    /// How requests authenticate (#2277): `bearer` (default; `key` is the same), `header` (the key
    /// in `auth_header`, e.g. Azure OpenAI's `api-key`), `none`, `command` (a token printed by
    /// `auth_command`), `oauth-client-credentials` or `azure-entra`. See [`ProviderAuth`].
    pub auth: Option<String>,
    /// `auth = "header"`: the header the key goes in (default `api-key`).
    pub auth_header: Option<String>,
    /// `auth = "command"` (or `azure-entra` with your own helper): the program and its arguments.
    /// It prints a token, or JSON `{"token"|"access_token"|"accessToken", "expires_in"|"expires_on"}`.
    pub auth_command: Option<Vec<String>>,
    /// How long `auth_command` may run (default 30 s).
    pub auth_command_timeout_secs: Option<u64>,
    /// `auth = "oauth-client-credentials"`: the token endpoint.
    pub token_url: Option<String>,
    /// OAuth / Entra client id (not a secret; the client secret is the provider's key).
    pub client_id: Option<String>,
    /// OAuth scopes (Entra default: `https://cognitiveservices.azure.com/.default`).
    pub scopes: Option<Vec<String>>,
    /// `auth = "azure-entra"`: the Microsoft Entra tenant (with `client_id`: client credentials;
    /// without: `az account get-access-token`).
    pub tenant_id: Option<String>,
    /// Entra authority (default `https://login.microsoftonline.com`; sovereign clouds differ).
    pub authority_host: Option<String>,
    /// OpenAI's `OpenAI-Organization` header.
    pub organization: Option<String>,
    /// OpenAI's `OpenAI-Project` header.
    pub project: Option<String>,
    /// Query parameters on every request, e.g. `{ "api-version" = "2024-10-21" }` (Azure OpenAI).
    pub query: Option<BTreeMap<String, String>>,
    /// PEM bundle of extra CA certificates for this endpoint (added to the OS trust store).
    pub ca_file: Option<String>,
    /// mTLS: the client certificate (PEM; may also hold the key).
    pub client_cert: Option<String>,
    /// mTLS: the client key file (PEM). Left out: the cert file's key, else the keychain
    /// (`provider:<id>:client-key`).
    pub client_key: Option<String>,
    /// Proxy for this endpoint (`http://proxy:3128`), or `direct`. Default: HTTPS_PROXY/NO_PROXY.
    pub proxy: Option<String>,
    /// Extra HTTP headers on every request.
    pub headers: Option<BTreeMap<String, String>>,
    /// Headers whose values are secrets: kept in the OS keychain (account
    /// `provider:<id>:header:<name>`, written by `amp-harness connect`), never in a file.
    pub secret_headers: Option<Vec<String>>,
    pub connect_timeout_secs: Option<u64>,
    /// Time to the first streamed event. 0 turns it off.
    pub header_timeout_secs: Option<u64>,
    /// Longest gap between streamed events. 0 turns it off.
    pub stream_idle_timeout_secs: Option<u64>,
    /// Whole-request deadline (rpi's `SimpleStreamOptions::timeout`).
    pub request_timeout_secs: Option<u64>,
    pub max_retries: Option<u32>,
    #[serde(default)]
    pub models: BTreeMap<String, ModelSpec>,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelSpec {
    pub name: Option<String>,
    /// Whether the model reasons (thinking levels apply).
    pub reasoning: Option<bool>,
    pub context_window: Option<u64>,
    /// Output tokens requested per turn.
    pub max_tokens: Option<u64>,
    /// `text`, `image`.
    pub input: Option<Vec<String>>,
    /// USD per million tokens.
    pub cost: Option<CostSpec>,
    /// Thinking level -> the provider's value (Anthropic effort, `reasoning_effort`).
    pub thinking_map: Option<BTreeMap<String, String>>,
    /// Levels the model rejects; they are never sent.
    pub unsupported_thinking: Option<Vec<String>>,
    /// rpi compat flags for the provider's API, snake_case (e.g. `force_adaptive_thinking`).
    pub compat: Option<toml::Table>,
    /// Extra JSON body fields on every request (opencode's variant `body`).
    pub extra_body: Option<toml::Table>,
    pub headers: Option<BTreeMap<String, String>>,
}

#[derive(Debug, Clone, Copy, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CostSpec {
    #[serde(default)]
    pub input: f64,
    #[serde(default)]
    pub output: f64,
    #[serde(default)]
    pub cache_read: f64,
    #[serde(default)]
    pub cache_write: f64,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RoleSpec {
    /// `provider/model`.
    pub model: Option<String>,
    /// `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`.
    pub thinking: Option<String>,
    /// Extra JSON body fields for this role's requests (merged over the model's).
    pub extra_body: Option<toml::Table>,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileSpec {
    pub description: Option<String>,
    #[serde(default)]
    pub roles: BTreeMap<String, RoleSpec>,
}

impl ModelsFile {
    pub fn parse(text: &str, origin: &str) -> Result<Self, ModelsError> {
        toml::from_str(text).map_err(|e| ModelsError(format!("parse {origin}: {e}")))
    }

    /// The shipped defaults.
    pub fn defaults() -> Self {
        Self::parse(DEFAULTS_TOML, "shipped models.toml").expect("shipped models.toml parses")
    }

    /// Reads `path`; `Ok(None)` if it does not exist.
    pub fn load(path: &Path) -> Result<Option<Self>, ModelsError> {
        match std::fs::read_to_string(path) {
            Ok(text) => Self::parse(&text, &path.display().to_string()).map(Some),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => err(format!("read {}: {e}", path.display())),
        }
    }
}

// ------------------------------------------------------------------------------------------------
// Merging layers.
// ------------------------------------------------------------------------------------------------

fn over<T>(base: &mut Option<T>, top: Option<T>) {
    if top.is_some() {
        *base = top;
    }
}

fn over_map<V>(base: &mut Option<BTreeMap<String, V>>, top: Option<BTreeMap<String, V>>) {
    if let Some(top) = top {
        base.get_or_insert_with(BTreeMap::new).extend(top);
    }
}

fn over_table(base: &mut Option<toml::Table>, top: Option<toml::Table>) {
    if let Some(top) = top {
        base.get_or_insert_with(toml::Table::new).extend(top);
    }
}

fn merge_provider(base: &mut ProviderSpec, top: ProviderSpec) {
    over(&mut base.name, top.name);
    over(&mut base.api, top.api);
    over(&mut base.base_url, top.base_url);
    over(&mut base.key_env, top.key_env);
    over(&mut base.auth, top.auth);
    over(&mut base.auth_header, top.auth_header);
    over(&mut base.auth_command, top.auth_command);
    over(
        &mut base.auth_command_timeout_secs,
        top.auth_command_timeout_secs,
    );
    over(&mut base.token_url, top.token_url);
    over(&mut base.client_id, top.client_id);
    over(&mut base.scopes, top.scopes);
    over(&mut base.tenant_id, top.tenant_id);
    over(&mut base.authority_host, top.authority_host);
    over(&mut base.organization, top.organization);
    over(&mut base.project, top.project);
    over_map(&mut base.query, top.query);
    over(&mut base.ca_file, top.ca_file);
    over(&mut base.client_cert, top.client_cert);
    over(&mut base.client_key, top.client_key);
    over(&mut base.proxy, top.proxy);
    over_map(&mut base.headers, top.headers);
    over(&mut base.secret_headers, top.secret_headers);
    over(&mut base.connect_timeout_secs, top.connect_timeout_secs);
    over(&mut base.header_timeout_secs, top.header_timeout_secs);
    over(
        &mut base.stream_idle_timeout_secs,
        top.stream_idle_timeout_secs,
    );
    over(&mut base.request_timeout_secs, top.request_timeout_secs);
    over(&mut base.max_retries, top.max_retries);
    for (id, model) in top.models {
        merge_model(base.models.entry(id).or_default(), model);
    }
}

fn merge_model(base: &mut ModelSpec, top: ModelSpec) {
    over(&mut base.name, top.name);
    over(&mut base.reasoning, top.reasoning);
    over(&mut base.context_window, top.context_window);
    over(&mut base.max_tokens, top.max_tokens);
    over(&mut base.input, top.input);
    over(&mut base.cost, top.cost);
    over_map(&mut base.thinking_map, top.thinking_map);
    over(&mut base.unsupported_thinking, top.unsupported_thinking);
    over_table(&mut base.compat, top.compat);
    over(&mut base.extra_body, top.extra_body);
    over_map(&mut base.headers, top.headers);
}

/// A role's assignment and the layer that last set its model.
#[derive(Debug, Clone)]
struct RoleEntry {
    spec: RoleSpec,
    source: String,
}

fn overlay_role(base: Option<RoleEntry>, top: RoleSpec, source: &str) -> RoleEntry {
    match base {
        // A new model: the lower layers' extra_body was for another model.
        Some(base) if top.model.is_none() => {
            let mut spec = base.spec;
            over(&mut spec.thinking, top.thinking);
            over_table(&mut spec.extra_body, top.extra_body);
            RoleEntry {
                spec,
                source: format!("{} + {source}", base.source),
            }
        }
        Some(base) => RoleEntry {
            spec: RoleSpec {
                model: top.model,
                thinking: top.thinking.or(base.spec.thinking),
                extra_body: top.extra_body,
            },
            source: source.to_owned(),
        },
        None => RoleEntry {
            spec: top,
            source: source.to_owned(),
        },
    }
}

/// Role names are the opencode agent names without the `amp-` prefix (`amp-worker-backend` and
/// `worker-backend` are the same role).
pub fn normalize_role(role: &str) -> &str {
    let role = role.trim();
    role.strip_prefix("amp-").unwrap_or(role)
}

// ------------------------------------------------------------------------------------------------
// Resolved configuration.
// ------------------------------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProviderApi {
    AnthropicMessages,
    OpenaiCompletions,
    Faux,
}

impl ProviderApi {
    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "anthropic-messages" | "anthropic" => Some(Self::AnthropicMessages),
            "openai-completions" | "openai-compatible" => Some(Self::OpenaiCompletions),
            "faux" => Some(Self::Faux),
            _ => None,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Self::AnthropicMessages => "anthropic-messages",
            Self::OpenaiCompletions => "openai-completions",
            Self::Faux => "faux",
        }
    }

    fn rpi_api(self) -> Api {
        match self {
            Self::AnthropicMessages => Api::AnthropicMessages,
            Self::OpenaiCompletions => Api::OpenaiCompletions,
            Self::Faux => Api::Faux,
        }
    }
}

/// Stall and deadline settings for one provider. `None` = off.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Timeouts {
    pub connect: Option<Duration>,
    pub header: Option<Duration>,
    pub stream_idle: Option<Duration>,
    pub request: Option<Duration>,
}

/// How a provider's requests authenticate (#2277). Secrets are never here: the API key or client
/// secret is the provider's key (`key_env`, else the keychain), tokens live in memory only.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum ProviderAuth {
    /// `Authorization: Bearer <key>` (Anthropic Messages: `x-api-key`). `auth = "bearer"` / `"key"`.
    #[default]
    Bearer,
    /// The key in a header of its own (`auth_header`, default `api-key`).
    Header { name: String },
    /// No credentials.
    None,
    /// A token printed by a program, sent as a bearer token; re-run at expiry and once on 401.
    Command {
        argv: Vec<String>,
        timeout: Duration,
    },
    /// OAuth 2.0 client credentials (client secret = the provider's key), sent as a bearer token.
    ClientCredentials {
        token_url: String,
        client_id: String,
        scopes: Vec<String>,
    },
    /// Microsoft Entra ID: client credentials against the tenant's token endpoint, or a command
    /// (`az account get-access-token` by default).
    AzureEntra(Box<ProviderAuth>),
}

impl ProviderAuth {
    /// The `auth` word it was configured with.
    pub fn name(&self) -> &'static str {
        match self {
            Self::Bearer => "bearer",
            Self::Header { .. } => "header",
            Self::None => "none",
            Self::Command { .. } => "command",
            Self::ClientCredentials { .. } => "oauth-client-credentials",
            Self::AzureEntra(_) => "azure-entra",
        }
    }

    /// Whether the provider's key (API key or client secret) is needed.
    pub fn needs_key(&self) -> bool {
        match self {
            Self::Bearer | Self::Header { .. } | Self::ClientCredentials { .. } => true,
            Self::None | Self::Command { .. } => false,
            Self::AzureEntra(inner) => inner.needs_key(),
        }
    }

    /// Whether requests carry a short-lived token (fetched, cached, refreshed).
    pub fn is_token(&self) -> bool {
        matches!(
            self,
            Self::Command { .. } | Self::ClientCredentials { .. } | Self::AzureEntra(_)
        )
    }

    /// What the key is called in prompts and messages.
    pub fn key_word(&self) -> &'static str {
        if matches!(self, Self::ClientCredentials { .. } | Self::AzureEntra(_)) {
            "client secret"
        } else {
            "key"
        }
    }

    /// One line for people (doctor, connect --list): never a secret.
    pub fn describe(&self) -> String {
        match self {
            Self::Bearer => "bearer key".to_owned(),
            Self::Header { name } => format!("key in the {name} header"),
            Self::None => "none".to_owned(),
            Self::Command { argv, .. } => format!(
                "token from `{}`",
                argv.first().map(String::as_str).unwrap_or("")
            ),
            Self::ClientCredentials {
                token_url,
                client_id,
                ..
            } => format!("OAuth client credentials ({client_id} at {token_url})"),
            Self::AzureEntra(inner) => match inner.as_ref() {
                Self::ClientCredentials { client_id, .. } => {
                    format!("Microsoft Entra ID (client {client_id})")
                }
                other => format!("Microsoft Entra ID, {}", other.describe()),
            },
        }
    }
}

/// The Entra scope for Azure OpenAI / AI Foundry.
pub const ENTRA_COGNITIVE_SCOPE: &str = "https://cognitiveservices.azure.com/.default";
/// The public-cloud Entra authority.
pub const ENTRA_AUTHORITY: &str = "https://login.microsoftonline.com";
/// Default bound on `auth_command`.
pub const DEFAULT_AUTH_COMMAND_TIMEOUT: Duration = Duration::from_secs(30);

/// The command `auth = "azure-entra"` runs when no client id is configured.
pub fn az_cli_command(scope: &str) -> Vec<String> {
    [
        "az",
        "account",
        "get-access-token",
        "--scope",
        scope,
        "--output",
        "json",
    ]
    .iter()
    .map(|s| (*s).to_owned())
    .collect()
}

/// A provider's TLS settings (#2277). Paths only; key material in the keychain is read when the
/// HTTP client is built.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ProviderTls {
    pub ca_file: Option<PathBuf>,
    pub client_cert: Option<PathBuf>,
    pub client_key: Option<PathBuf>,
}

impl ProviderTls {
    pub fn is_default(&self) -> bool {
        *self == Self::default()
    }
}

/// A provider's `proxy`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProxySetting {
    /// `http(s)://host:port`
    Url(String),
    /// `direct`: no proxy, whatever HTTPS_PROXY says.
    Direct,
}

fn expand_home(path: &str) -> PathBuf {
    match path.strip_prefix("~/") {
        Some(rest) => dirs::home_dir()
            .map(|h| h.join(rest))
            .unwrap_or_else(|| PathBuf::from(path)),
        None => PathBuf::from(path),
    }
}

fn nonempty(value: Option<String>) -> Option<String> {
    value.map(|v| v.trim().to_owned()).filter(|v| !v.is_empty())
}

fn https_or_local(url: &str, what: &str) -> Result<(), String> {
    let lower = url.to_ascii_lowercase();
    if lower.starts_with("https://") {
        return Ok(());
    }
    if let Some(rest) = lower.strip_prefix("http://") {
        let host = rest.split(['/', '?', '#']).next().unwrap_or("");
        let host = host.rsplit_once('@').map_or(host, |(_, h)| h);
        let name = if host.starts_with('[') {
            host.split(']')
                .next()
                .unwrap_or(host)
                .trim_start_matches('[')
        } else {
            host.split(':').next().unwrap_or(host)
        };
        if matches!(name, "localhost" | "127.0.0.1" | "::1") {
            return Ok(());
        }
    }
    Err(format!(
        "{what} `{url}` must be https:// (plain http only to localhost)"
    ))
}

/// `auth` and its fields, checked. Every field that does not apply to the chosen auth is an error,
/// so a typo or a leftover surfaces.
fn build_auth(api: ProviderApi, spec: &ProviderSpec) -> Result<ProviderAuth, String> {
    let word = spec
        .auth
        .as_deref()
        .map(|a| a.trim().to_ascii_lowercase())
        .unwrap_or_else(|| "bearer".to_owned());
    let timeout = spec
        .auth_command_timeout_secs
        .filter(|s| *s > 0)
        .map_or(DEFAULT_AUTH_COMMAND_TIMEOUT, Duration::from_secs);
    let command = spec
        .auth_command
        .clone()
        .map(|argv| {
            argv.into_iter()
                .map(|a| a.trim().to_owned())
                .collect::<Vec<_>>()
        })
        .filter(|argv| !argv.is_empty());
    if command.as_ref().is_some_and(|a| a[0].is_empty()) {
        return Err("`auth_command` needs the program as its first element".to_owned());
    }
    let scopes: Vec<String> = spec
        .scopes
        .clone()
        .unwrap_or_default()
        .into_iter()
        .map(|s| s.trim().to_owned())
        .filter(|s| !s.is_empty())
        .collect();
    let client_id = nonempty(spec.client_id.clone());
    let token_url = nonempty(spec.token_url.clone());
    let tenant = nonempty(spec.tenant_id.clone());
    let authority = nonempty(spec.authority_host.clone());
    let only = |field: &str, present: bool, allowed: &[&str]| -> Result<(), String> {
        if present && !allowed.contains(&word.as_str()) {
            return Err(format!(
                "`{field}` applies to auth = {} (this provider has auth = \"{word}\")",
                allowed
                    .iter()
                    .map(|a| format!("\"{a}\""))
                    .collect::<Vec<_>>()
                    .join(" or ")
            ));
        }
        Ok(())
    };
    only("auth_header", spec.auth_header.is_some(), &["header"])?;
    only(
        "auth_command",
        command.is_some(),
        &["command", "azure-entra"],
    )?;
    only(
        "auth_command_timeout_secs",
        spec.auth_command_timeout_secs.is_some(),
        &["command", "azure-entra"],
    )?;
    only(
        "token_url",
        token_url.is_some(),
        &["oauth-client-credentials"],
    )?;
    only(
        "client_id",
        client_id.is_some(),
        &["oauth-client-credentials", "azure-entra"],
    )?;
    only(
        "scopes",
        !scopes.is_empty(),
        &["oauth-client-credentials", "azure-entra"],
    )?;
    only("tenant_id", tenant.is_some(), &["azure-entra"])?;
    only("authority_host", authority.is_some(), &["azure-entra"])?;
    Ok(match word.as_str() {
        "bearer" | "key" => ProviderAuth::Bearer,
        "none" => ProviderAuth::None,
        "header" => {
            let name = nonempty(spec.auth_header.clone()).unwrap_or_else(|| match api {
                ProviderApi::AnthropicMessages => "x-api-key".to_owned(),
                _ => "api-key".to_owned(),
            });
            if !name
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || "-_".contains(c))
            {
                return Err(format!("`auth_header` `{name}` is not a header name"));
            }
            // rpi's Anthropic provider only sends a request whose auth it recognises.
            if api == ProviderApi::AnthropicMessages
                && !["x-api-key", "authorization", "cf-aig-authorization"]
                    .contains(&name.to_ascii_lowercase().as_str())
            {
                return Err(format!(
                    "anthropic-messages providers take the key in x-api-key, authorization or                      cf-aig-authorization, not `{name}`"
                ));
            }
            ProviderAuth::Header { name }
        }
        "command" => ProviderAuth::Command {
            argv: command
                .ok_or("auth = \"command\" needs `auth_command = [\"program\", \"arg\"]`")?,
            timeout,
        },
        "oauth-client-credentials" | "client-credentials" | "oauth" => {
            let token_url =
                token_url.ok_or("auth = \"oauth-client-credentials\" needs `token_url`")?;
            https_or_local(&token_url, "token_url")?;
            ProviderAuth::ClientCredentials {
                token_url,
                client_id: client_id
                    .ok_or("auth = \"oauth-client-credentials\" needs `client_id`")?,
                scopes,
            }
        }
        "azure-entra" | "entra" | "azure-ad" => {
            let scopes = if scopes.is_empty() {
                vec![ENTRA_COGNITIVE_SCOPE.to_owned()]
            } else {
                scopes
            };
            let inner = match (command, tenant, client_id) {
                (Some(argv), None, None) => ProviderAuth::Command { argv, timeout },
                (Some(_), _, _) => {
                    return Err(
                        "azure-entra: use either `auth_command` or `tenant_id` + `client_id`"
                            .to_owned(),
                    );
                }
                (None, Some(tenant), Some(client_id)) => {
                    let authority = authority.unwrap_or_else(|| ENTRA_AUTHORITY.to_owned());
                    https_or_local(&authority, "authority_host")?;
                    ProviderAuth::ClientCredentials {
                        token_url: format!(
                            "{}/{tenant}/oauth2/v2.0/token",
                            authority.trim_end_matches('/')
                        ),
                        client_id,
                        scopes,
                    }
                }
                (None, None, None) => ProviderAuth::Command {
                    argv: az_cli_command(&scopes[0]),
                    timeout,
                },
                (None, _, _) => {
                    return Err(
                        "azure-entra client credentials need both `tenant_id` and `client_id`                          (or neither, to use `az account get-access-token`)"
                            .to_owned(),
                    );
                }
            };
            ProviderAuth::AzureEntra(Box::new(inner))
        }
        other => {
            return Err(format!(
                "unknown auth `{other}` (bearer, header, none, command, oauth-client-credentials,                  azure-entra)"
            ));
        }
    })
}

fn build_proxy(proxy: Option<String>) -> Result<Option<ProxySetting>, String> {
    let Some(proxy) = nonempty(proxy) else {
        return Ok(None);
    };
    if matches!(
        proxy.to_ascii_lowercase().as_str(),
        "direct" | "none" | "off"
    ) {
        return Ok(Some(ProxySetting::Direct));
    }
    let lower = proxy.to_ascii_lowercase();
    if !(lower.starts_with("http://") || lower.starts_with("https://")) {
        return Err(format!(
            "`proxy` must be an http:// or https:// URL, or \"direct\" (SOCKS and PAC are not supported), not `{proxy}`"
        ));
    }
    let authority = proxy
        .split_once("://")
        .map_or("", |(_, r)| r)
        .split('/')
        .next()
        .unwrap_or("");
    if let Some((userinfo, _)) = authority.rsplit_once('@')
        && userinfo.contains(':')
    {
        return Err(
            "`proxy` holds a password: config files never hold secrets. Put the proxy URL with \
             its credentials in HTTPS_PROXY instead"
                .to_owned(),
        );
    }
    Ok(Some(ProxySetting::Url(proxy)))
}

fn secs(value: Option<u64>) -> Option<Duration> {
    value.filter(|s| *s > 0).map(Duration::from_secs)
}

#[derive(Debug, Clone)]
pub struct ProviderConfig {
    pub id: String,
    /// Display name, if the config gives one.
    pub name: Option<String>,
    pub api: ProviderApi,
    pub base_url: String,
    pub key_env: Option<String>,
    /// True when the provider needs its key (`key_env` / keychain `provider:<id>`): bearer and
    /// header auth (the API key), OAuth client credentials and Entra client credentials (the client
    /// secret). False for `none`, `command`, Entra through `az`, and faux.
    pub needs_key: bool,
    /// How requests authenticate (#2277).
    pub auth: ProviderAuth,
    /// Query parameters added to every request (#2277).
    pub query: BTreeMap<String, String>,
    /// CA file and mTLS identity (#2277).
    pub tls: ProviderTls,
    /// `proxy`: `None` follows HTTPS_PROXY/NO_PROXY.
    pub proxy: Option<ProxySetting>,
    /// Provider-level headers (already merged into every model's headers).
    pub headers: BTreeMap<String, String>,
    /// Header names whose values come from the keychain (see [`ProviderSpec::secret_headers`]).
    pub secret_headers: Vec<String>,
    pub timeouts: Timeouts,
    pub max_retries: Option<u32>,
    /// By model id; each `Model` has `provider = id`, the provider's base URL and headers.
    pub models: BTreeMap<String, Model>,
}

/// How the role assignments were chosen.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Selection {
    Profile {
        name: String,
        /// `default`, `env AMP_HARNESS_PROFILE` or `config <path>`.
        source: String,
    },
    Inherit {
        model: String,
        thinking: Option<String>,
        source: String,
    },
}

/// The merged providers, models and role assignments.
#[derive(Debug, Clone)]
pub struct ModelsConfig {
    pub providers: BTreeMap<String, ProviderConfig>,
    pub selection: Selection,
    /// The config.toml looked at, and whether it exists.
    pub config_file: Option<PathBuf>,
    pub config_found: bool,
    /// The repo config that was merged in (it existed), if any.
    pub repo_file: Option<PathBuf>,
    /// Profiles available (shipped and user), with descriptions.
    pub profiles: BTreeMap<String, Option<String>>,
    /// `[models] fallback` and the layer that set it; `None` means [`DEFAULT_FALLBACK_ROLE`].
    pub fallback: Option<(String, String)>,
    roles: BTreeMap<String, RoleEntry>,
}

/// What an agent file says about its model, for roles with no `[roles.<id>]` (#2234, D11).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ModelHints {
    /// The frontmatter `model` (`provider/model`). Used only when that provider and model are
    /// configured; opencode's ids for other setups are skipped.
    pub model: Option<String>,
    /// The frontmatter `variant`, used as the thinking level when it is one.
    pub thinking: Option<String>,
    /// The frontmatter `based_on`: the role whose model this one shares (`agents new --from`).
    pub based_on: Option<String>,
}

/// One role's provider, model and thinking level, ready for an `AgentHarness`.
#[derive(Debug, Clone)]
pub struct ResolvedRole {
    pub role: String,
    pub provider: String,
    pub model_id: String,
    /// The rpi model (role `extra_body` already merged into `sampling_params`).
    pub model: Model,
    /// What the config asked for (`None` = unset).
    pub requested_thinking: Option<ThinkingLevel>,
    /// What will be sent: clamped to the model's supported levels, `off` if it does not reason.
    pub thinking: ThinkingLevel,
    /// Which layer set the model: `default`, `profile anthropic`, `inherit ...`, `config <path>`.
    pub source: String,
}

impl ResolvedRole {
    /// `provider/model`.
    pub fn model_ref(&self) -> String {
        format!("{}/{}", self.provider, self.model_id)
    }
}

impl ModelsConfig {
    /// Defaults + the user's config.toml, with the process environment.
    pub fn load() -> Result<Self, ModelsError> {
        Self::load_with_env(&|name| std::env::var(name).ok(), None)
    }

    /// `config_path` overrides the default location; `env` is looked up for `XDG_CONFIG_HOME`,
    /// `HOME`, `AMP_HARNESS_CONFIG[_DIR]` and `AMP_HARNESS_PROFILE`. No repo layer: use
    /// [`ModelsConfig::load_paths`] for that.
    pub fn load_with_env(
        env: &dyn Fn(&str) -> Option<String>,
        config_path: Option<PathBuf>,
    ) -> Result<Self, ModelsError> {
        let mut paths = amp_mcp::config_paths::current(env).with_repo(None);
        if let Some(path) = config_path {
            paths = paths.with_user_file(path, amp_mcp::Source::Flag);
        }
        Self::load_paths(env, &paths)
    }

    /// Defaults + `paths.user_file` + `paths.repo_file()` (each only if it exists). A repo file
    /// with keys other than `[roles]`, `[mcp]` and `[models] profile` is an error.
    pub fn load_paths(
        env: &dyn Fn(&str) -> Option<String>,
        paths: &amp_mcp::ConfigPaths,
    ) -> Result<Self, ModelsError> {
        let path = paths.user_file.clone();
        let repo = match paths.repo_file() {
            Some(file) => RepoConfig::load(&file)?,
            None => None,
        };
        let user = match &path {
            Some(path) => ModelsFile::load(path)?,
            None => None,
        };
        let found = user.is_some();
        let label = path
            .as_ref()
            .map(|p| format!("config {}", p.display()))
            .unwrap_or_else(|| "config".to_owned());
        let mut config =
            Self::from_layers(ModelsFile::defaults(), user, &label, repo.as_ref(), env)?;
        config.config_file = path;
        config.config_found = found;
        Ok(config)
    }

    /// Merge `user` over `defaults`. `user_label` names the user layer in sources.
    pub fn from_files(
        defaults: ModelsFile,
        user: Option<ModelsFile>,
        user_label: &str,
        env: &dyn Fn(&str) -> Option<String>,
    ) -> Result<Self, ModelsError> {
        Self::from_layers(defaults, user, user_label, None, env)
    }

    /// Merge `user` over `defaults`, then `repo` (its `[roles]` and `[models] profile`) over both.
    /// `user_label` names the user layer in sources; the repo layer is `repo <path>`.
    pub fn from_layers(
        defaults: ModelsFile,
        user: Option<ModelsFile>,
        user_label: &str,
        repo: Option<&RepoConfig>,
        env: &dyn Fn(&str) -> Option<String>,
    ) -> Result<Self, ModelsError> {
        let user = user.unwrap_or_default();
        let repo_label = repo.map(RepoConfig::label).unwrap_or_default();
        let repo_layer = repo.map(RepoConfig::models_file).unwrap_or_default();

        // Providers and models.
        let mut provider_specs = defaults.providers;
        for (id, spec) in user.providers {
            merge_provider(provider_specs.entry(id).or_default(), spec);
        }
        let mut providers = BTreeMap::new();
        for (id, spec) in provider_specs {
            providers.insert(id.clone(), build_provider(&id, spec)?);
        }

        // Profiles.
        let mut profiles: BTreeMap<String, ProfileSpec> = defaults.profiles;
        for (name, top) in user.profiles {
            let base = profiles.entry(name).or_default();
            over(&mut base.description, top.description);
            for (role, spec) in top.roles {
                let merged = overlay_role(
                    base.roles.remove(&role).map(|spec| RoleEntry {
                        spec,
                        source: String::new(),
                    }),
                    spec,
                    "",
                );
                base.roles.insert(role, merged.spec);
            }
        }
        for name in [DEFAULT_PROFILE, INHERIT] {
            if profiles.contains_key(name) {
                return err(format!(
                    "`{name}` is a mode, not a profile name: rename [profiles.{name}]"
                ));
            }
        }

        // Mode.
        let env_profile = env(ENV_PROFILE).filter(|s| !s.trim().is_empty());
        let (profile, profile_source) = match (
            env_profile,
            repo_layer.models.profile.clone(),
            user.models.profile.clone(),
        ) {
            (Some(p), _, _) => (p.trim().to_owned(), format!("env {ENV_PROFILE}")),
            (None, Some(p), _) => (p.trim().to_owned(), repo_label.clone()),
            (None, None, Some(p)) => (p.trim().to_owned(), user_label.to_owned()),
            (None, None, None) => (DEFAULT_PROFILE.to_owned(), "default".to_owned()),
        };

        let mut roles: BTreeMap<String, RoleEntry> = BTreeMap::new();
        for (role, spec) in defaults.roles {
            let role = normalize_role(&role).to_owned();
            let entry = overlay_role(roles.remove(&role), spec, "default");
            roles.insert(role, entry);
        }

        let selection = if profile == INHERIT {
            let Some(model) = user.models.inherit.clone().filter(|m| !m.trim().is_empty()) else {
                return err(format!(
                    "profile = \"inherit\" ({profile_source}) needs [models] inherit = \"provider/model\" in config.toml"
                ));
            };
            let source = format!("inherit ({profile_source})");
            for entry in roles.values_mut() {
                *entry = RoleEntry {
                    spec: RoleSpec {
                        model: Some(model.clone()),
                        thinking: user.models.inherit_thinking.clone(),
                        extra_body: None,
                    },
                    source: source.clone(),
                };
            }
            Selection::Inherit {
                model,
                thinking: user.models.inherit_thinking.clone(),
                source: profile_source,
            }
        } else {
            if profile != DEFAULT_PROFILE {
                let Some(spec) = profiles.get(&profile) else {
                    let known: Vec<&str> = [DEFAULT_PROFILE, INHERIT]
                        .into_iter()
                        .chain(profiles.keys().map(String::as_str))
                        .collect();
                    return err(format!(
                        "unknown profile `{profile}` ({profile_source}); known: {}",
                        known.join(", ")
                    ));
                };
                let source = format!("profile {profile}");
                for (role, top) in spec.roles.clone() {
                    let role = normalize_role(&role).to_owned();
                    let entry = overlay_role(roles.remove(&role), top, &source);
                    roles.insert(role, entry);
                }
            }
            Selection::Profile {
                name: profile,
                source: profile_source,
            }
        };

        for (role, top) in user.roles {
            let role = normalize_role(&role).to_owned();
            let entry = overlay_role(roles.remove(&role), top, user_label);
            roles.insert(role, entry);
        }
        for (role, top) in repo_layer.roles {
            let role = normalize_role(&role).to_owned();
            let entry = overlay_role(roles.remove(&role), top, &repo_label);
            roles.insert(role, entry);
        }

        let fallback = user
            .models
            .fallback
            .clone()
            .map(|f| f.trim().to_owned())
            .filter(|f| !f.is_empty())
            .map(|f| (f, user_label.to_owned()));
        if let Some((value, source)) = &fallback
            && value.contains('/')
        {
            parse_model_ref(value)
                .map_err(|e| ModelsError(format!("[models] fallback ({source}): {e}")))?;
        }

        Ok(Self {
            providers,
            selection,
            config_file: None,
            config_found: false,
            repo_file: repo.map(|r| r.path.clone()),
            fallback,
            profiles: profiles
                .into_iter()
                .map(|(name, spec)| (name, spec.description))
                .collect(),
            roles,
        })
    }

    /// Which layers set `role`'s assignment (`default`, `profile anthropic`, `config <path>`,
    /// `config <path> + repo <path>`, ...); `None` for a role nobody configured.
    pub fn role_source(&self, role: &str) -> Option<&str> {
        self.roles
            .get(normalize_role(role))
            .map(|entry| entry.source.as_str())
    }

    /// Every role with an assignment, sorted.
    pub fn role_names(&self) -> Vec<String> {
        self.roles.keys().cloned().collect()
    }

    /// Whether `role` has an assignment of its own (defaults, profile, user or repo `[roles]`).
    pub fn has_role(&self, role: &str) -> bool {
        self.roles.contains_key(normalize_role(role))
    }

    /// Resolve an agent's role with D11's fallback chain, so a new agent always gets a model:
    /// 1. its own assignment ([`Self::resolve_role`]: `[roles.<id>]`, repo over user over the
    ///    defaults; or inherit mode);
    /// 2. the agent file's `model` (and `variant` as thinking), if that provider and model are
    ///    configured;
    /// 3. the `based_on` role's assignment;
    /// 4. `[models] fallback`: a role name or `provider/model` (default: [`DEFAULT_FALLBACK_ROLE`]).
    pub fn resolve_with(
        &self,
        role: &str,
        hints: &ModelHints,
    ) -> Result<ResolvedRole, ModelsError> {
        let role = normalize_role(role);
        if self.roles.contains_key(role) || matches!(self.selection, Selection::Inherit { .. }) {
            return self.resolve_role(role);
        }
        let thinking = hints.thinking.clone().filter(|t| parse_thinking(t).is_ok());
        if let Some(model_ref) = hints.model.as_deref()
            && self.knows_model(model_ref)
        {
            return self.resolve_entry(
                role,
                RoleEntry {
                    spec: RoleSpec {
                        model: Some(model_ref.trim().to_owned()),
                        thinking,
                        extra_body: None,
                    },
                    source: "agent file model".to_owned(),
                },
            );
        }
        if let Some(base) = hints.based_on.as_deref().map(normalize_role)
            && base != role
            && let Some(entry) = self.roles.get(base)
        {
            let mut entry = entry.clone();
            entry.source = format!("based_on {base} ({})", entry.source);
            return self.resolve_entry(role, entry);
        }
        let (value, source) = self
            .fallback
            .clone()
            .unwrap_or_else(|| (DEFAULT_FALLBACK_ROLE.to_owned(), "default".to_owned()));
        if value.contains('/') {
            return self.resolve_entry(
                role,
                RoleEntry {
                    spec: RoleSpec {
                        model: Some(value.clone()),
                        thinking,
                        extra_body: None,
                    },
                    source: format!("[models] fallback ({source})"),
                },
            );
        }
        let base = normalize_role(&value);
        match self.roles.get(base) {
            Some(entry) if base != role => {
                let mut entry = entry.clone();
                entry.source = format!("fallback {base} ({})", entry.source);
                self.resolve_entry(role, entry)
            }
            _ => err(format!(
                "no model for role `{role}`: add [roles.{role}] model = \"provider/model\" to config.toml, or set [models] fallback (the fallback role `{base}` has no model; configured roles: {})",
                self.role_names().join(", ")
            )),
        }
    }

    /// Whether `provider/model` names a configured provider and one of its models.
    pub fn knows_model(&self, model_ref: &str) -> bool {
        parse_model_ref(model_ref).is_ok_and(|(p, m)| {
            self.providers
                .get(p)
                .is_some_and(|provider| provider.models.contains_key(m))
        })
    }

    /// Resolve one role (`amp-` prefix optional). A role nobody configured resolves only in inherit
    /// mode; agents should use [`Self::resolve_with`], which falls back.
    pub fn resolve_role(&self, role: &str) -> Result<ResolvedRole, ModelsError> {
        let role = normalize_role(role);
        let entry = match (self.roles.get(role), &self.selection) {
            (Some(entry), _) => entry.clone(),
            (
                None,
                Selection::Inherit {
                    model,
                    thinking,
                    source,
                },
            ) => RoleEntry {
                spec: RoleSpec {
                    model: Some(model.clone()),
                    thinking: thinking.clone(),
                    extra_body: None,
                },
                source: format!("inherit ({source})"),
            },
            (None, _) => {
                return err(format!(
                    "no model for role `{role}`: add [roles.{role}] model = \"provider/model\" to config.toml (configured roles: {})",
                    self.role_names().join(", ")
                ));
            }
        };
        self.resolve_entry(role, entry)
    }

    fn resolve_entry(&self, role: &str, entry: RoleEntry) -> Result<ResolvedRole, ModelsError> {
        let Some(model_ref) = entry.spec.model.as_deref() else {
            return err(format!("role `{role}` has no model ({})", entry.source));
        };
        let (provider_id, model_id) = parse_model_ref(model_ref)?;
        let provider = self.providers.get(provider_id).ok_or_else(|| {
            ModelsError(format!(
                "role `{role}`: unknown provider `{provider_id}` in `{model_ref}` (providers: {})",
                self.providers
                    .keys()
                    .cloned()
                    .collect::<Vec<_>>()
                    .join(", ")
            ))
        })?;
        let mut model = provider.models.get(model_id).cloned().ok_or_else(|| {
            ModelsError(format!(
                "role `{role}`: provider `{provider_id}` has no model `{model_id}`; add [providers.{provider_id}.models.\"{model_id}\"] to config.toml"
            ))
        })?;
        if let Some(extra) = &entry.spec.extra_body {
            merge_extra_body(&mut model, extra)?;
        }
        let requested_thinking = entry
            .spec
            .thinking
            .as_deref()
            .map(parse_thinking)
            .transpose()
            .map_err(|e| ModelsError(format!("role `{role}`: {e}")))?;
        let thinking = if model.reasoning {
            model.clamp_thinking_level(requested_thinking.unwrap_or(DEFAULT_THINKING))
        } else {
            ThinkingLevel::Off
        };
        Ok(ResolvedRole {
            role: role.to_owned(),
            provider: provider_id.to_owned(),
            model_id: model_id.to_owned(),
            model,
            requested_thinking,
            thinking,
            source: entry.source,
        })
    }

    /// Every configured role, resolved (or why not).
    pub fn resolve_all(&self) -> Vec<(String, Result<ResolvedRole, ModelsError>)> {
        self.roles
            .keys()
            .map(|role| (role.clone(), self.resolve_role(role)))
            .collect()
    }

    /// Providers some role uses.
    pub fn providers_in_use(&self) -> BTreeSet<String> {
        self.resolve_all()
            .into_iter()
            .filter_map(|(_, r)| r.ok().map(|r| r.provider))
            .collect()
    }
}

/// `provider/model` -> (`provider`, `model`). The model id may contain `/`.
pub fn parse_model_ref(model_ref: &str) -> Result<(&str, &str), ModelsError> {
    match model_ref.trim().split_once('/') {
        Some((p, m)) if !p.is_empty() && !m.is_empty() => Ok((p, m)),
        _ => err(format!("`{model_ref}` is not `provider/model`")),
    }
}

pub fn parse_thinking(level: &str) -> Result<ThinkingLevel, ModelsError> {
    serde_json::from_value(Value::String(level.trim().to_ascii_lowercase())).map_err(|_| {
        ModelsError(format!(
            "unknown thinking level `{level}` (off, minimal, low, medium, high, xhigh, max)"
        ))
    })
}

pub fn thinking_name(level: ThinkingLevel) -> &'static str {
    match level {
        ThinkingLevel::Off => "off",
        ThinkingLevel::Minimal => "minimal",
        ThinkingLevel::Low => "low",
        ThinkingLevel::Medium => "medium",
        ThinkingLevel::High => "high",
        ThinkingLevel::Xhigh => "xhigh",
        ThinkingLevel::Max => "max",
    }
}

/// Each provider of `defaults` merged with `user` that would not build, as (`id`, why), for
/// `config check` (#2235): [`ModelsConfig::from_layers`] stops at the first one.
pub fn provider_problems(defaults: &ModelsFile, user: &ModelsFile) -> Vec<(String, String)> {
    let mut specs = defaults.providers.clone();
    for (id, spec) in user.providers.clone() {
        merge_provider(specs.entry(id).or_default(), spec);
    }
    specs
        .into_iter()
        .filter_map(|(id, spec)| build_provider(&id, spec).err().map(|e| (id, e.0)))
        .collect()
}

// ------------------------------------------------------------------------------------------------
// Building rpi models.
// ------------------------------------------------------------------------------------------------

fn build_provider(id: &str, spec: ProviderSpec) -> Result<ProviderConfig, ModelsError> {
    if id.is_empty() || id.contains('/') {
        return err(format!(
            "provider id `{id}` must be non-empty and contain no `/`"
        ));
    }
    let ctx = |m: String| ModelsError(format!("[providers.{id}]: {m}"));
    let api_name = spec.api.as_deref().ok_or_else(|| {
        ctx("missing `api` (anthropic-messages, openai-completions, faux)".into())
    })?;
    let api = ProviderApi::parse(api_name).ok_or_else(|| {
        ctx(format!(
            "unknown api `{api_name}` (anthropic-messages, openai-completions, faux)"
        ))
    })?;
    let auth_spec = spec.clone();
    let base_url = match (api, spec.base_url) {
        (_, Some(url)) => url.trim_end_matches('/').to_owned(),
        (ProviderApi::Faux, None) => "faux://local".to_owned(),
        (_, None) => return Err(ctx("missing `base_url`".into())),
    };
    let auth = match api {
        ProviderApi::Faux => ProviderAuth::None,
        _ => build_auth(api, &auth_spec).map_err(ctx)?,
    };
    let needs_key = auth.needs_key();
    let mut headers = spec.headers.clone().unwrap_or_default();
    // OpenAI's optional organization / project headers (#2277).
    for (field, header, value) in [
        ("organization", "OpenAI-Organization", &spec.organization),
        ("project", "OpenAI-Project", &spec.project),
    ] {
        if let Some(value) = nonempty(value.clone()) {
            if headers.keys().any(|k| k.eq_ignore_ascii_case(header)) {
                return Err(ctx(format!(
                    "`{field}` and headers.{header} both set it: keep one"
                )));
            }
            headers.insert(header.to_owned(), value);
        }
    }
    let query: BTreeMap<String, String> = spec
        .query
        .clone()
        .unwrap_or_default()
        .into_iter()
        .map(|(k, v)| (k.trim().to_owned(), v.trim().to_owned()))
        .collect();
    if query.keys().any(String::is_empty) {
        return Err(ctx("`query` has an empty parameter name".into()));
    }
    let tls = ProviderTls {
        ca_file: nonempty(spec.ca_file.clone()).map(|p| expand_home(&p)),
        client_cert: nonempty(spec.client_cert.clone()).map(|p| expand_home(&p)),
        client_key: nonempty(spec.client_key.clone()).map(|p| expand_home(&p)),
    };
    if tls.client_key.is_some() && tls.client_cert.is_none() {
        return Err(ctx("`client_key` needs `client_cert`".into()));
    }
    let proxy = build_proxy(spec.proxy.clone()).map_err(ctx)?;
    let mut models = BTreeMap::new();
    for (model_id, model_spec) in spec.models {
        let model = build_model(id, api, &base_url, &headers, &model_id, model_spec)
            .map_err(|e| ctx(format!("model `{model_id}`: {e}")))?;
        models.insert(model_id, model);
    }
    Ok(ProviderConfig {
        id: id.to_owned(),
        name: spec.name.filter(|n| !n.trim().is_empty()),
        api,
        base_url,
        key_env: spec.key_env.filter(|s| !s.trim().is_empty()),
        needs_key,
        auth,
        query,
        tls,
        proxy,
        headers,
        secret_headers: spec
            .secret_headers
            .unwrap_or_default()
            .into_iter()
            .map(|h| h.trim().to_owned())
            .filter(|h| !h.is_empty())
            .collect(),
        timeouts: Timeouts {
            connect: secs(spec.connect_timeout_secs),
            header: secs(spec.header_timeout_secs),
            stream_idle: secs(spec.stream_idle_timeout_secs),
            request: secs(spec.request_timeout_secs),
        },
        max_retries: spec.max_retries,
        models,
    })
}

fn build_model(
    provider: &str,
    api: ProviderApi,
    base_url: &str,
    provider_headers: &BTreeMap<String, String>,
    id: &str,
    spec: ModelSpec,
) -> Result<Model, String> {
    // Start from rpi's catalog entry when it has one (Anthropic), else generic defaults.
    let catalog = match api {
        ProviderApi::AnthropicMessages => rpi_ai::providers::anthropic::models::get_model(id),
        _ => None,
    };
    let mut model = match catalog {
        Some(mut m) => {
            m.provider = provider.to_owned();
            m.base_url = base_url.to_owned();
            m
        }
        None => {
            let mut m = Model::new(id, id, api.rpi_api(), provider, base_url);
            m.context_window = 128_000;
            m.max_tokens = 16_384;
            m
        }
    };
    if let Some(name) = spec.name {
        model.name = name;
    }
    over_value(&mut model.reasoning, spec.reasoning);
    over_value(&mut model.context_window, spec.context_window);
    over_value(&mut model.max_tokens, spec.max_tokens);
    if let Some(input) = spec.input {
        model.input = input
            .iter()
            .map(|i| match i.as_str() {
                "text" => Ok(InputModality::Text),
                "image" => Ok(InputModality::Image),
                other => Err(format!("unknown input `{other}` (text, image)")),
            })
            .collect::<Result<_, _>>()?;
    }
    if let Some(cost) = spec.cost {
        model.cost = ModelCost {
            rates: ModelCostRates {
                input: cost.input,
                output: cost.output,
                cache_read: cost.cache_read,
                cache_write: cost.cache_write,
            },
            tiers: Vec::new(),
        };
        model.cost_configured = true;
    }
    if spec.thinking_map.is_some() || spec.unsupported_thinking.is_some() {
        let map = model.thinking_level_map.get_or_insert_with(BTreeMap::new);
        for (level, value) in spec.thinking_map.unwrap_or_default() {
            map.insert(parse_thinking(&level).map_err(|e| e.0)?, Some(value));
        }
        for level in spec.unsupported_thinking.unwrap_or_default() {
            map.insert(parse_thinking(&level).map_err(|e| e.0)?, None);
        }
    }
    if let Some(compat) = spec.compat {
        model.compat = Some(build_compat(api, model.compat.take(), &compat)?);
    }
    let mut headers = model.headers.take().unwrap_or_default();
    headers.extend(provider_headers.clone());
    headers.extend(spec.headers.unwrap_or_default());
    model.headers = (!headers.is_empty()).then_some(headers);
    if let Some(extra) = spec.extra_body {
        merge_extra_body(&mut model, &extra).map_err(|e| e.0)?;
    }
    Ok(model)
}

fn over_value<T>(base: &mut T, top: Option<T>) {
    if let Some(top) = top {
        *base = top;
    }
}

fn snake_to_camel(key: &str) -> String {
    let mut out = String::with_capacity(key.len());
    let mut upper = false;
    for c in key.chars() {
        if c == '_' {
            upper = true;
        } else if upper {
            out.extend(c.to_uppercase());
            upper = false;
        } else {
            out.push(c);
        }
    }
    out
}

/// Overlay snake_case `flags` on the model's current compat for `api`. Unknown flags are errors.
fn build_compat(
    api: ProviderApi,
    current: Option<StreamingProtocolCompat>,
    flags: &toml::Table,
) -> Result<StreamingProtocolCompat, String> {
    fn overlay<T: serde::Serialize + serde::de::DeserializeOwned + Default>(
        current: Option<T>,
        flags: &toml::Table,
    ) -> Result<T, String> {
        let mut value =
            serde_json::to_value(current.unwrap_or_default()).map_err(|e| e.to_string())?;
        let known: BTreeSet<String> = serde_json::to_value(T::default())
            .ok()
            .and_then(|v| v.as_object().map(|o| o.keys().cloned().collect()))
            .unwrap_or_default();
        let object = value.as_object_mut().ok_or("compat is not an object")?;
        for (key, flag) in flags {
            let camel = snake_to_camel(key);
            if !known.contains(&camel) {
                return Err(format!("unknown compat flag `{key}`"));
            }
            object.insert(
                camel,
                serde_json::to_value(flag).map_err(|e| e.to_string())?,
            );
        }
        serde_json::from_value(value).map_err(|e| format!("compat: {e}"))
    }
    Ok(match api {
        ProviderApi::AnthropicMessages => {
            let current = current.and_then(|c| match c {
                StreamingProtocolCompat::AnthropicMessages(c) => Some(c),
                _ => None,
            });
            StreamingProtocolCompat::AnthropicMessages(overlay::<AnthropicMessagesCompat>(
                current, flags,
            )?)
        }
        ProviderApi::OpenaiCompletions => {
            let current = current.and_then(|c| match c {
                StreamingProtocolCompat::OpenaiCompletions(c) => Some(c),
                _ => None,
            });
            StreamingProtocolCompat::OpenaiCompletions(overlay::<OpenaiCompletionsCompat>(
                current, flags,
            )?)
        }
        ProviderApi::Faux => return Err("the faux api takes no compat flags".into()),
    })
}

fn merge_extra_body(model: &mut Model, extra: &toml::Table) -> Result<(), ModelsError> {
    let params = model
        .sampling_params
        .get_or_insert_with(serde_json::Map::new);
    for (key, value) in extra {
        let value = serde_json::to_value(value)
            .map_err(|e| ModelsError(format!("extra_body.{key}: {e}")))?;
        params.insert(key.clone(), value);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn no_env(_: &str) -> Option<String> {
        None
    }

    fn config(user: &str) -> Result<ModelsConfig, ModelsError> {
        config_env(user, &no_env)
    }

    fn config_env(
        user: &str,
        env: &dyn Fn(&str) -> Option<String>,
    ) -> Result<ModelsConfig, ModelsError> {
        let user = ModelsFile::parse(user, "test config")?;
        ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "config test", env)
    }

    /// The opencode roster: v2/.opencode/agent/amp-<role>.md `model` + `variant`.
    const OPENCODE: &[(&str, &str, ThinkingLevel)] = &[
        (
            "manager",
            "openrouter/xiaomi/mimo-v2.6-pro",
            ThinkingLevel::High,
        ),
        (
            "researcher",
            "openrouter/xiaomi/mimo-v2.6-pro",
            ThinkingLevel::High,
        ),
        (
            "media",
            "openrouter/xiaomi/mimo-v2.6-pro",
            ThinkingLevel::High,
        ),
        (
            "worker-backend",
            "openrouter/deepseek/deepseek-v4.1-flash-1017",
            ThinkingLevel::Max,
        ),
        (
            "worker-frontend",
            "openrouter/deepseek/deepseek-v4.1-flash-1017",
            ThinkingLevel::Max,
        ),
        (
            "worker-docs",
            "openrouter/deepseek/deepseek-v4.1-flash-1017",
            ThinkingLevel::High,
        ),
        (
            "github-researcher",
            "openrouter/deepseek/deepseek-v4.1-flash-1017",
            ThinkingLevel::High,
        ),
        (
            "kb-curator",
            "openrouter/deepseek/deepseek-v4.1-flash-1017",
            ThinkingLevel::High,
        ),
        ("reviewer", "openrouter/z-ai/glm-5.3", ThinkingLevel::Max),
        (
            "github-reviewer",
            "openrouter/z-ai/glm-5.3",
            ThinkingLevel::Max,
        ),
        (
            "designer",
            "openrouter/moonshotai/kimi-k3",
            ThinkingLevel::High,
        ),
    ];

    /// #2234 (D11): a role with no [roles.<id>] still resolves: agent file model (if configured),
    /// then based_on, then [models] fallback (a role or provider/model), default worker-backend.
    #[test]
    fn unconfigured_roles_fall_back() {
        let user = r#"
[providers.f]
api = "faux"
[providers.f.models."one"]
[providers.f.models."two"]
[roles.worker-backend]
model = "f/one"
[roles.reviewer]
model = "f/two"
thinking = "low"
"#;
        let c = config(user).unwrap();
        let hints =
            |model: Option<&str>, thinking: Option<&str>, based_on: Option<&str>| ModelHints {
                model: model.map(str::to_owned),
                thinking: thinking.map(str::to_owned),
                based_on: based_on.map(str::to_owned),
            };
        // Its own [roles] wins over everything.
        let r = c
            .resolve_with(
                "amp-reviewer",
                &hints(Some("f/one"), None, Some("worker-backend")),
            )
            .unwrap();
        assert_eq!(r.model_ref(), "f/two");
        // Nothing: worker-backend's model.
        let r = c
            .resolve_with("sql-expert", &ModelHints::default())
            .unwrap();
        assert_eq!(r.model_ref(), "f/one");
        assert!(
            r.source.starts_with("fallback worker-backend (config test"),
            "{}",
            r.source
        );
        // A configured frontmatter model, with its variant as thinking.
        let r = c
            .resolve_with(
                "sql-expert",
                &hints(Some("f/two"), Some("high"), Some("worker-backend")),
            )
            .unwrap();
        assert_eq!(
            (r.model_ref().as_str(), r.source.as_str()),
            ("f/two", "agent file model")
        );
        assert_eq!(r.requested_thinking, Some(ThinkingLevel::High));
        // opencode's ids for providers that aren't configured are skipped; `on` isn't a level.
        let r = c
            .resolve_with(
                "sql-expert",
                &hints(Some("nowhere/x"), Some("on"), Some("amp-reviewer")),
            )
            .unwrap();
        assert_eq!(r.model_ref(), "f/two");
        assert_eq!(r.requested_thinking, Some(ThinkingLevel::Low));
        assert!(r.source.starts_with("based_on reviewer ("), "{}", r.source);
        // [models] fallback: a role, or a provider/model.
        let c = config(&format!("{user}\n[models]\nfallback = \"reviewer\"\n")).unwrap();
        let r = c.resolve_with("x", &ModelHints::default()).unwrap();
        assert_eq!(r.model_ref(), "f/two");
        let c = config(&format!("{user}\n[models]\nfallback = \"f/two\"\n")).unwrap();
        let r = c.resolve_with("x", &ModelHints::default()).unwrap();
        assert_eq!(r.model_ref(), "f/two");
        assert!(
            r.source.starts_with("[models] fallback (config test"),
            "{}",
            r.source
        );
        assert!(config(&format!("{user}\n[models]\nfallback = \"/\"\n")).is_err());
        let c = config(&format!("{user}\n[models]\nfallback = \"nobody\"\n")).unwrap();
        let e = c.resolve_with("x", &ModelHints::default()).unwrap_err();
        assert!(e.0.contains("no model for role `x`"), "{e}");
        // Inherit mode still gives every role its one model.
        let c = config(&format!(
            "{user}\n[models]\nprofile = \"inherit\"\ninherit = \"f/two\"\n"
        ))
        .unwrap();
        let r = c
            .resolve_with("x", &hints(Some("f/one"), None, Some("worker-backend")))
            .unwrap();
        assert_eq!(r.model_ref(), "f/two");
    }

    #[test]
    fn shipped_defaults_mirror_the_opencode_roster() {
        let c = config("").unwrap();
        assert_eq!(
            c.selection,
            Selection::Profile {
                name: "opencode".into(),
                source: "default".into()
            }
        );
        assert_eq!(c.role_names().len(), OPENCODE.len());
        for (role, model, thinking) in OPENCODE {
            let r = c.resolve_role(role).unwrap();
            assert_eq!(&r.model_ref(), model, "{role}");
            assert_eq!(r.thinking, *thinking, "{role}");
            assert_eq!(r.source, "default");
            assert_eq!(r.model.provider, "openrouter");
            assert_eq!(r.model.base_url, "https://openrouter.ai/api/v1");
        }
        // opencode's mimo variant "on" is a body field, not an effort.
        let manager = c.resolve_role("amp-manager").unwrap();
        assert_eq!(
            manager.model.sampling_params.unwrap()["reasoning"],
            serde_json::json!({"enabled": true})
        );
    }

    #[test]
    fn opencode_agent_files_match_the_defaults() {
        // Guard against drift: read the real agent files when the repo layout is there.
        let dir = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../.opencode/agent");
        let Ok(entries) = std::fs::read_dir(&dir) else {
            return;
        };
        let c = config("").unwrap();
        let mut seen = 0;
        for entry in entries.flatten() {
            let path = entry.path();
            let Some(stem) = path.file_stem().and_then(|s| s.to_str()) else {
                continue;
            };
            let text = std::fs::read_to_string(&path).unwrap();
            let field = |name: &str| {
                text.lines()
                    .find_map(|l| l.strip_prefix(&format!("{name}:")))
                    .map(|v| v.trim().to_owned())
            };
            let Some(model) = field("model") else {
                continue;
            };
            let r = c.resolve_role(stem).unwrap();
            assert_eq!(r.model_ref(), model, "{stem}");
            let expected = match field("variant").as_deref() {
                Some("on") => ThinkingLevel::High,
                Some(v) => parse_thinking(v).unwrap(),
                None => DEFAULT_THINKING,
            };
            assert_eq!(r.thinking, expected, "{stem}");
            seen += 1;
        }
        assert_eq!(seen, OPENCODE.len());
    }

    #[test]
    fn anthropic_profile_puts_every_role_on_anthropic() {
        let c = config("[models]\nprofile = \"anthropic\"\n").unwrap();
        for (role, result) in c.resolve_all() {
            let r = result.unwrap();
            assert_eq!(r.provider, "anthropic", "{role}");
            assert_eq!(r.source, "profile anthropic", "{role}");
            // The OpenRouter-only reasoning body must not follow the role to Anthropic.
            assert!(
                r.model
                    .sampling_params
                    .as_ref()
                    .is_none_or(|p| !p.contains_key("reasoning")),
                "{role}"
            );
        }
        let manager = c.resolve_role("manager").unwrap();
        assert_eq!(manager.model_ref(), "anthropic/claude-opus-5-5");
        assert_eq!(manager.model.api, Api::AnthropicMessages);
        assert_eq!(manager.model.context_window, 1_000_000);
        assert!(
            manager
                .model
                .compat
                .as_ref()
                .and_then(StreamingProtocolCompat::as_anthropic)
                .unwrap()
                .adaptive_thinking()
        );
        assert_eq!(
            c.resolve_role("reviewer").unwrap().model_id,
            "claude-opus-5-5"
        );
        assert_eq!(
            c.resolve_role("worker-backend").unwrap().model_id,
            "claude-sonnet-5-5"
        );
        assert_eq!(
            c.providers_in_use(),
            BTreeSet::from(["anthropic".to_owned()])
        );
    }

    #[test]
    fn env_profile_wins_over_config() {
        let env = |k: &str| (k == ENV_PROFILE).then(|| "anthropic".to_owned());
        let c = config_env("[models]\nprofile = \"opencode\"\n", &env).unwrap();
        assert_eq!(c.resolve_role("manager").unwrap().provider, "anthropic");
    }

    #[test]
    fn inherit_mode_uses_one_model_for_every_role_and_unknown_roles() {
        let c = config(
            "[models]\nprofile = \"inherit\"\ninherit = \"anthropic/claude-sonnet-5-5\"\ninherit_thinking = \"low\"\n",
        )
        .unwrap();
        for (role, result) in c.resolve_all() {
            let r = result.unwrap();
            assert_eq!(r.model_ref(), "anthropic/claude-sonnet-5-5", "{role}");
            assert_eq!(r.thinking, ThinkingLevel::Low, "{role}");
            assert!(r.model.sampling_params.is_none(), "{role}");
        }
        let custom = c.resolve_role("my-own-agent").unwrap();
        assert_eq!(custom.model_ref(), "anthropic/claude-sonnet-5-5");
    }

    #[test]
    fn inherit_without_a_model_is_an_error() {
        let e = config("[models]\nprofile = \"inherit\"\n").unwrap_err();
        assert!(e.0.contains("inherit = \"provider/model\""), "{e}");
    }

    #[test]
    fn user_roles_override_one_role_on_another_provider() {
        let c = config(
            r#"
[roles.reviewer]
model = "anthropic/claude-opus-5-5"
"#,
        )
        .unwrap();
        let reviewer = c.resolve_role("reviewer").unwrap();
        assert_eq!(reviewer.model_ref(), "anthropic/claude-opus-5-5");
        // thinking carries over from the opencode roster when the override doesn't set it.
        assert_eq!(reviewer.thinking, ThinkingLevel::Max);
        assert_eq!(reviewer.source, "config test");
        let manager = c.resolve_role("manager").unwrap();
        assert_eq!(manager.provider, "openrouter");
        assert_eq!(
            c.providers_in_use(),
            BTreeSet::from(["anthropic".to_owned(), "openrouter".to_owned()])
        );
    }

    #[test]
    fn user_thinking_only_override_keeps_the_model() {
        let c = config("[roles.manager]\nthinking = \"low\"\n").unwrap();
        let manager = c.resolve_role("manager").unwrap();
        assert_eq!(manager.model_ref(), "openrouter/xiaomi/mimo-v2.6-pro");
        assert_eq!(manager.thinking, ThinkingLevel::Low);
        assert_eq!(manager.source, "default + config test");
    }

    #[test]
    fn user_can_add_a_provider_and_a_model() {
        let c = config(
            r#"
[providers.llamabarn]
api = "openai-completions"
base_url = "http://localhost:2276/v1/"
auth = "none"
[providers.llamabarn.models."qwen-3.6-35b-a3b"]
context_window = 262144

[providers.openrouter.models."openai/gpt-oss-120b"]

[roles.worker-docs]
model = "llamabarn/qwen-3.6-35b-a3b"
[roles.kb-curator]
model = "openrouter/openai/gpt-oss-120b"
"#,
        )
        .unwrap();
        let docs = c.resolve_role("worker-docs").unwrap();
        assert_eq!(docs.model.base_url, "http://localhost:2276/v1");
        assert_eq!(docs.model.context_window, 262_144);
        assert_eq!(
            docs.thinking,
            ThinkingLevel::Off,
            "a non-reasoning model never thinks"
        );
        assert!(!c.providers["llamabarn"].needs_key);
        let curator = c.resolve_role("kb-curator").unwrap();
        // Provider-level headers reach models the user adds.
        assert_eq!(curator.model.headers.unwrap()["X-Title"], "AMP Harness");
    }

    #[test]
    fn user_provider_settings_merge_field_by_field() {
        let c = config(
            "[providers.openrouter]\nstream_idle_timeout_secs = 0\nheader_timeout_secs = 90\n",
        )
        .unwrap();
        let or = &c.providers["openrouter"];
        assert_eq!(or.timeouts.stream_idle, None);
        assert_eq!(or.timeouts.header, Some(Duration::from_secs(90)));
        assert_eq!(or.base_url, "https://openrouter.ai/api/v1");
        assert_eq!(or.key_env.as_deref(), Some("OPENROUTER_API_KEY"));
        assert_eq!(or.models.len(), 4);
    }

    #[test]
    fn unsupported_off_is_never_sent_as_disabled() {
        let c =
            config("[roles.manager]\nmodel = \"anthropic/claude-opus-5-5\"\nthinking = \"off\"\n")
                .unwrap();
        let r = c.resolve_role("manager").unwrap();
        let map = r.model.thinking_level_map.as_ref().unwrap();
        assert_eq!(map.get(&ThinkingLevel::Off), Some(&None));
        assert_eq!(map.get(&ThinkingLevel::Xhigh), Some(&Some("xhigh".into())));
        assert_eq!(r.requested_thinking, Some(ThinkingLevel::Off));
    }

    #[test]
    fn unsupported_level_clamps_down() {
        // DeepSeek has no xhigh mapping: xhigh clamps to high.
        let c = config("[roles.worker-docs]\nthinking = \"xhigh\"\n").unwrap();
        assert_eq!(
            c.resolve_role("worker-docs").unwrap().thinking,
            ThinkingLevel::High
        );
    }

    #[test]
    fn bad_references_are_clear_errors() {
        let c = config(
            r#"
[roles.a]
model = "nowhere/x"
[roles.b]
model = "openrouter/no-such-model"
[roles.c]
model = "no-slash"
[roles.d]
model = "anthropic/claude-opus-5-5"
thinking = "huge"
"#,
        )
        .unwrap();
        let e = |r: &str| c.resolve_role(r).unwrap_err().0;
        assert!(e("a").contains("unknown provider `nowhere`"), "{}", e("a"));
        assert!(
            e("b").contains("[providers.openrouter.models.\"no-such-model\"]"),
            "{}",
            e("b")
        );
        assert!(e("c").contains("not `provider/model`"), "{}", e("c"));
        assert!(e("d").contains("unknown thinking level"), "{}", e("d"));
        assert!(
            c.resolve_role("nobody")
                .unwrap_err()
                .0
                .contains("no model for role `nobody`")
        );
    }

    #[test]
    fn config_typos_are_errors() {
        assert!(config("[providers.openrouter]\nbase_ur = \"x\"\n").is_err());
        assert!(config("[roles.manager]\nmodle = \"x\"\n").is_err());
        let e = config(
            "[providers.anthropic.models.\"claude-opus-5-5\"]\ncompat = { no_such_flag = true }\n",
        )
        .unwrap_err();
        assert!(e.0.contains("unknown compat flag `no_such_flag`"), "{e}");
        let e = config("[models]\nprofile = \"nope\"\n").unwrap_err();
        assert!(e.0.contains("unknown profile `nope`"), "{e}");
        // Other harness tables in the same file are fine.
        assert!(config("[server]\nurl = \"http://localhost:8000/mcp\"\n").is_ok());
    }

    #[test]
    fn user_profiles_are_selectable() {
        let c = config(
            r#"
[models]
profile = "cheap"
[profiles.cheap]
description = "everything on DeepSeek"
[profiles.cheap.roles.manager]
model = "openrouter/deepseek/deepseek-v4.1-flash-1017"
thinking = "high"
"#,
        )
        .unwrap();
        assert_eq!(
            c.resolve_role("manager").unwrap().model_id,
            "deepseek/deepseek-v4.1-flash-1017"
        );
        assert!(
            c.resolve_role("manager")
                .unwrap()
                .model
                .sampling_params
                .is_none()
        );
        assert_eq!(c.resolve_role("reviewer").unwrap().model_id, "z-ai/glm-5.3");
        assert!(c.profiles.contains_key("anthropic") && c.profiles.contains_key("cheap"));
    }

    /// #2232: the repo layer's [roles] and [models] profile beat the user file's;
    /// AMP_HARNESS_PROFILE beats both; sources name the repo file.
    #[test]
    fn repo_layer_merges_over_the_user_file() {
        let user = ModelsFile::parse(
            "[models]\nprofile = \"anthropic\"\n[roles.reviewer]\nmodel = \"openrouter/xiaomi/mimo-v2.6-pro\"\nthinking = \"low\"\n",
            "user",
        )
        .unwrap();
        let repo = crate::repo_config::RepoConfig::parse(
            "[models]\nprofile = \"opencode\"\n[roles.reviewer]\nthinking = \"high\"\n",
            Path::new("/r/.amp/harness/config.toml"),
        )
        .unwrap();
        let c = ModelsConfig::from_layers(
            ModelsFile::defaults(),
            Some(user.clone()),
            "config u",
            Some(&repo),
            &|_| None,
        )
        .unwrap();
        match &c.selection {
            Selection::Profile { name, source } => {
                assert_eq!(name, "opencode");
                assert_eq!(source, "repo /r/.amp/harness/config.toml");
            }
            other => panic!("{other:?}"),
        }
        assert_eq!(
            c.repo_file,
            Some(PathBuf::from("/r/.amp/harness/config.toml"))
        );
        let reviewer = c.resolve_role("reviewer").unwrap();
        // The user's model, the repo's thinking.
        assert_eq!(reviewer.model_ref(), "openrouter/xiaomi/mimo-v2.6-pro");
        assert_eq!(reviewer.requested_thinking, Some(ThinkingLevel::High));
        assert_eq!(
            reviewer.source,
            "config u + repo /r/.amp/harness/config.toml"
        );

        let env = |k: &str| (k == ENV_PROFILE).then(|| "anthropic".to_owned());
        let c = ModelsConfig::from_layers(
            ModelsFile::defaults(),
            Some(user),
            "config u",
            Some(&repo),
            &env,
        )
        .unwrap();
        assert!(
            matches!(&c.selection, Selection::Profile { name, .. } if name == "anthropic"),
            "{:?}",
            c.selection
        );
    }

    #[test]
    fn load_reads_xdg_config() {
        // Under the workspace target dir, never /tmp (D13).
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/tmp/amp-core-models-test")
            .join(std::process::id().to_string());
        let cfg = dir.join("amp-harness");
        std::fs::create_dir_all(&cfg).unwrap();
        std::fs::write(
            cfg.join("config.toml"),
            "[models]\nprofile = \"anthropic\"\n",
        )
        .unwrap();
        let xdg = dir.to_string_lossy().into_owned();
        let env = move |k: &str| (k == "XDG_CONFIG_HOME").then(|| xdg.clone());
        let c = ModelsConfig::load_with_env(&env, None).unwrap();
        assert!(c.config_found);
        assert_eq!(c.resolve_role("manager").unwrap().provider, "anthropic");
        std::fs::remove_dir_all(&dir).unwrap();

        let missing = ModelsConfig::load_with_env(&|_| None, Some(dir.join("nope.toml"))).unwrap();
        assert!(!missing.config_found);
        assert_eq!(
            missing.resolve_role("manager").unwrap().provider,
            "openrouter"
        );
    }
}
