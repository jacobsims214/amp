//! Providers, models and the model each role runs on (D9 as changed by #2091: several providers at
//! once, the way opencode does it).
//!
//! Two layers, both in the same TOML schema:
//! 1. the shipped defaults, [`DEFAULTS_TOML`] (`crates/amp-core/models.toml`): the Anthropic and
//!    OpenRouter providers, their models, the opencode roster as `[roles.*]` and the alternative
//!    `[profiles.anthropic]`;
//! 2. the user's `config.toml` (`$XDG_CONFIG_HOME/amp-harness/config.toml`, else
//!    `~/.config/amp-harness/config.toml`). Its `[providers.*]`, `[roles.*]` and `[profiles.*]` are
//!    merged over the defaults, and its `[models]` table picks the assignment mode.
//!
//! There is no default provider. Each role names its own `provider/model` (the model id may contain
//! `/`, e.g. `openrouter/xiaomi/mimo-v2.6-pro`; the provider is everything before the first `/`).
//!
//! A role's assignment, lowest to highest precedence:
//! 1. `[roles.<role>]` in the defaults (the opencode roster);
//! 2. the selected profile's `[profiles.<name>.roles.<role>]`, or, in inherit mode, the one
//!    `[models] inherit = "provider/model"` for every role;
//! 3. `[roles.<role>]` in config.toml.
//!
//! The mode comes from `AMP_HARNESS_PROFILE`, then `[models] profile`, then `opencode` (the defaults
//! as shipped). `inherit` is a mode, not a profile name.
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
    /// `key` (default for HTTP providers) or `none` (local endpoints that take no key).
    pub auth: Option<String>,
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

#[derive(Debug, Clone, Default, Deserialize)]
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
    /// False for `auth = "none"` and faux.
    pub needs_key: bool,
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
    /// Profiles available (shipped and user), with descriptions.
    pub profiles: BTreeMap<String, Option<String>>,
    roles: BTreeMap<String, RoleEntry>,
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
    /// `HOME` and `AMP_HARNESS_PROFILE`.
    pub fn load_with_env(
        env: &dyn Fn(&str) -> Option<String>,
        config_path: Option<PathBuf>,
    ) -> Result<Self, ModelsError> {
        let path = config_path.or_else(|| amp_mcp::default_config_path(env));
        let user = match &path {
            Some(path) => ModelsFile::load(path)?,
            None => None,
        };
        let found = user.is_some();
        let label = path
            .as_ref()
            .map(|p| format!("config {}", p.display()))
            .unwrap_or_else(|| "config".to_owned());
        let mut config = Self::from_files(ModelsFile::defaults(), user, &label, env)?;
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
        let user = user.unwrap_or_default();

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
        let (profile, profile_source) = match (env_profile, user.models.profile.clone()) {
            (Some(p), _) => (p.trim().to_owned(), format!("env {ENV_PROFILE}")),
            (None, Some(p)) => (p.trim().to_owned(), user_label.to_owned()),
            (None, None) => (DEFAULT_PROFILE.to_owned(), "default".to_owned()),
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

        Ok(Self {
            providers,
            selection,
            config_file: None,
            config_found: false,
            profiles: profiles
                .into_iter()
                .map(|(name, spec)| (name, spec.description))
                .collect(),
            roles,
        })
    }

    /// Every role with an assignment, sorted.
    pub fn role_names(&self) -> Vec<String> {
        self.roles.keys().cloned().collect()
    }

    /// Resolve one role (`amp-` prefix optional). A role nobody configured resolves only in inherit
    /// mode.
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
    let base_url = match (api, spec.base_url) {
        (_, Some(url)) => url.trim_end_matches('/').to_owned(),
        (ProviderApi::Faux, None) => "faux://local".to_owned(),
        (_, None) => return Err(ctx("missing `base_url`".into())),
    };
    let needs_key = match (api, spec.auth.as_deref()) {
        (ProviderApi::Faux, _) => false,
        (_, None | Some("key")) => true,
        (_, Some("none")) => false,
        (_, Some(other)) => return Err(ctx(format!("unknown auth `{other}` (key, none)"))),
    };
    let headers = spec.headers.unwrap_or_default();
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
