//! `amp-harness connect [provider]`, `connect --list` and `disconnect <provider>` (#2163), and the
//! harness side of the chat's `/connect`.
//!
//! People add model providers without shell commands:
//!
//! - **Built in:** OpenRouter and Anthropic (shipped in models.toml) and OpenAI (a preset written to
//!   config.toml on first connect). Anthropic means an **API key** from console.anthropic.com:
//!   Claude Free/Pro/Max subscription logins are not offered, because Anthropic allows them only
//!   in its own apps, and a subscription token (`sk-ant-oat…`) is refused.
//! - **Custom:** any OpenAI-compatible (or Anthropic Messages) endpoint: id, display name, base
//!   URL, API style, extra headers, model ids (listed by the endpoint, or typed). The
//!   `[providers.<id>]` table is merged into config.toml ([`ConfigFile::replace_tables`]: the
//!   old file is kept as config.toml.bak, and the result must load). Header values that look
//!   secret go to the keychain (`secret_headers`), never the file.
//!
//! The key is read hidden (or from stdin with `--key-stdin`, never argv), checked with a cheap
//! live call that spends no tokens (setup's [`check_key`]), and stored only in the OS keychain
//! (service `amp-harness`, account `provider:<id>`). Where there is no keychain the key must come
//! from the environment (the CI/headless path); there is no file fallback for provider keys.
//!
//! After connecting, `connect` offers to point the roles at the provider (Anthropic → the
//! `anthropic` profile, OpenRouter → the default roster, anything else → `inherit` with a chosen
//! model) and prints each role's resolved model, like `config show`.

use std::{
    collections::{BTreeMap, BTreeSet},
    io::Read,
    path::PathBuf,
    sync::Arc,
};

use amp_core::{
    agent::Runtime,
    models::{DEFAULT_PROFILE, ModelsConfig, ModelsFile, ProviderApi, ProviderConfig, Selection},
    providers::{
        KeySource, ProviderSet, StaticKeys, SystemKeys, header_account, keychain_account,
        live_model_ids, resolve_key, resolve_secret_headers,
    },
};
use clap::Args;
use serde::Serialize;

use crate::{
    config_cmd,
    conn::{CliError, print_json},
    setup::{
        self, ConfigFile, KeyCheck,
        config_file::toml_string,
        ui::{self, Ui},
    },
};

/// The comment inside every table `connect` writes: `disconnect` removes only those tables.
const ADDED_MARK: &str = "# Added by `amp-harness connect`.";
/// The menu entry and `--list` name for a custom endpoint.
const CUSTOM: &str = "custom";
/// How many listed models a non-interactive custom connect takes without `--model`.
const MAX_AUTO_MODELS: usize = 50;
/// How many listed models the interactive picker shows.
const PICKER_MODELS: usize = 15;

#[derive(Args, Debug, Default)]
pub struct ConnectArgs {
    /// The provider: openrouter, anthropic, openai, one from your config.toml, `custom`, or a new
    /// id with --base-url (a custom endpoint). Asked in a terminal when left out.
    pub provider: Option<String>,
    /// List the providers and whether each has a key (and where it comes from; never the key).
    #[arg(long, conflicts_with_all = ["key_stdin", "base_url", "api", "name", "models", "headers", "use_for_roles"])]
    pub list: bool,
    /// Read the key from stdin (one line) instead of asking. Never pass a key as an argument.
    #[arg(long)]
    pub key_stdin: bool,
    /// Custom provider: the API base URL (e.g. http://localhost:11434/v1).
    #[arg(long, value_name = "URL")]
    pub base_url: Option<String>,
    /// Custom provider: openai-completions (default) or anthropic-messages.
    #[arg(long, value_name = "API")]
    pub api: Option<String>,
    /// Custom provider: the display name.
    #[arg(long)]
    pub name: Option<String>,
    /// Custom provider: a model id to add (repeatable; default: the models the endpoint lists).
    #[arg(long = "model", value_name = "ID")]
    pub models: Vec<String>,
    /// Custom provider: an extra header, NAME=VALUE (repeatable). Secret values are refused here
    /// (argv is visible to other processes): add those in a terminal, where they go to the keychain.
    #[arg(long = "header", value_name = "NAME=VALUE")]
    pub headers: Vec<String>,
    /// Point the agent roles at this provider without asking (Anthropic: the anthropic profile;
    /// OpenRouter: the default roster; others: every role on --role-model or the first model).
    #[arg(long = "use")]
    pub use_for_roles: bool,
    /// With --use on a custom provider: the model every role runs on.
    #[arg(long, value_name = "ID", requires = "use_for_roles")]
    pub role_model: Option<String>,
    /// Print a JSON summary (never a secret).
    #[arg(long)]
    pub json: bool,
}

#[derive(Args, Debug, Default)]
pub struct DisconnectArgs {
    /// The provider whose key to remove.
    pub provider: String,
    /// Keep the provider's [providers.<id>] table that `connect` added to config.toml (only the
    /// key goes). Tables written by hand are always kept.
    #[arg(long)]
    pub keep_config: bool,
    /// Print a JSON summary.
    #[arg(long)]
    pub json: bool,
}

// ---------------------------------------------------------------------------------------------
// Words shared with setup
// ---------------------------------------------------------------------------------------------

/// What to ask for: the key and where to get one.
pub fn key_prompt(id: &str, name: &str) -> String {
    match id {
        "anthropic" => "Anthropic API key (console.anthropic.com)".to_owned(),
        "openrouter" => "OpenRouter API key (openrouter.ai/keys)".to_owned(),
        "openai" => "OpenAI API key (platform.openai.com)".to_owned(),
        _ => format!("{name} API key"),
    }
}

/// Claude subscription (OAuth) tokens are refused: Anthropic allows Free/Pro/Max subscriptions
/// only in its own apps (Claude.ai, Claude Code, Claude Desktop, Cowork), not in other tools.
pub fn refuse_subscription_token(_provider: &str, key: &str) -> Result<(), String> {
    if key.trim().starts_with("sk-ant-oat") {
        return Err(
            "that is a Claude subscription login token, not an API key. amp-harness can't \
                    use Claude Free/Pro/Max subscriptions (Anthropic allows them only in its own \
                    apps). Create an API key at console.anthropic.com instead."
                .to_owned(),
        );
    }
    Ok(())
}

/// One line about a built-in provider for the picker.
fn about(id: &str) -> Option<&'static str> {
    match id {
        "openrouter" => Some("one key for many models; the shipped roles run on it"),
        "anthropic" => Some("API key (console.anthropic.com); Claude subscriptions can't be used"),
        "openai" => Some("API key (platform.openai.com)"),
        _ => None,
    }
}

/// The OpenAI preset (not shipped in models.toml: its models are listed live on connect).
fn openai_preset() -> CustomSpec {
    CustomSpec {
        id: "openai".to_owned(),
        name: Some("OpenAI".to_owned()),
        base_url: "https://api.openai.com/v1".to_owned(),
        api: ProviderApi::OpenaiCompletions,
        key_env: Some("OPENAI_API_KEY".to_owned()),
        needs_key: true,
        ..CustomSpec::default()
    }
}

fn shipped_ids() -> BTreeSet<String> {
    ModelsFile::defaults().providers.keys().cloned().collect()
}

/// Header names whose values are secrets.
fn secret_name(name: &str) -> bool {
    let n = name.to_ascii_lowercase();
    [
        "auth",
        "key",
        "token",
        "secret",
        "password",
        "cookie",
        "signature",
        "session",
    ]
    .iter()
    .any(|w| n.contains(w))
}

/// Values that look like credentials whatever the header is called.
fn secret_value(value: &str) -> bool {
    let v = value.trim();
    let lower = v.to_ascii_lowercase();
    lower.starts_with("bearer ")
        || lower.starts_with("basic ")
        || lower.starts_with("sk-")
        || (v.len() >= 24
            && !v.contains(char::is_whitespace)
            && v.chars().any(|c| c.is_ascii_digit())
            && v.chars().any(|c| c.is_ascii_alphabetic()))
}

/// Whether config.toml's `[providers.<id>]` table was written by `connect` (it has
/// [`ADDED_MARK`] inside). Tables people wrote by hand are never removed.
fn added_by_connect(text: &str, id: &str) -> bool {
    let header = format!("[providers.{id}]");
    let mut inside = false;
    for line in text.lines() {
        let t = line.trim();
        if t.starts_with('[') {
            inside = t == header;
        } else if inside && t.starts_with(ADDED_MARK) {
            return true;
        }
    }
    false
}

fn valid_id(id: &str) -> Result<(), String> {
    let ok = !id.is_empty()
        && id.len() <= 40
        && id
            .chars()
            .next()
            .is_some_and(|c| c.is_ascii_lowercase() || c.is_ascii_digit())
        && id
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-' || c == '_');
    if !ok {
        return Err(format!(
            "`{id}` is not a provider id: use lowercase letters, digits, - and _ (e.g. my-llm)"
        ));
    }
    if [CUSTOM, "inherit", DEFAULT_PROFILE].contains(&id) {
        return Err(format!("`{id}` is reserved; pick another provider id"));
    }
    Ok(())
}

fn slug(name: &str) -> String {
    let mut out = String::new();
    for c in name.trim().chars() {
        if c.is_ascii_alphanumeric() {
            out.push(c.to_ascii_lowercase());
        } else if !out.ends_with('-') && !out.is_empty() {
            out.push('-');
        }
    }
    out.trim_end_matches('-').chars().take(40).collect()
}

fn valid_url(url: &str) -> Result<String, String> {
    let u = url.trim().trim_end_matches('/');
    if !(u.starts_with("http://") || u.starts_with("https://")) || u.contains(char::is_whitespace) {
        return Err(format!("`{url}` is not an http(s) URL"));
    }
    if u.split_once("://").is_none_or(|(_, rest)| rest.is_empty()) {
        return Err(format!("`{url}` has no host"));
    }
    Ok(u.to_owned())
}

fn parse_api(api: &str) -> Result<ProviderApi, String> {
    match ProviderApi::parse(api.trim()) {
        Some(a @ (ProviderApi::OpenaiCompletions | ProviderApi::AnthropicMessages)) => Ok(a),
        _ => Err(format!(
            "`{api}`: the API is openai-completions or anthropic-messages"
        )),
    }
}

// ---------------------------------------------------------------------------------------------
// A custom provider's table
// ---------------------------------------------------------------------------------------------

/// A provider `connect` writes to config.toml (custom endpoints and the OpenAI preset).
#[derive(Debug, Clone)]
pub struct CustomSpec {
    pub id: String,
    pub name: Option<String>,
    pub base_url: String,
    pub api: ProviderApi,
    pub key_env: Option<String>,
    pub needs_key: bool,
    /// Plain headers (written to the file).
    pub headers: BTreeMap<String, String>,
    /// Secret headers: name -> value (the value goes to the keychain, the name to the file).
    pub secret_headers: BTreeMap<String, String>,
    pub models: Vec<String>,
}

impl Default for CustomSpec {
    fn default() -> Self {
        Self {
            id: String::new(),
            name: None,
            base_url: String::new(),
            api: ProviderApi::OpenaiCompletions,
            key_env: None,
            needs_key: true,
            headers: BTreeMap::new(),
            secret_headers: BTreeMap::new(),
            models: Vec::new(),
        }
    }
}

impl CustomSpec {
    /// `MY_LLM_API_KEY` for `my-llm`: the variable a headless run can set instead.
    fn default_key_env(id: &str) -> String {
        format!(
            "{}_API_KEY",
            id.to_ascii_uppercase().replace(['-', '.'], "_")
        )
    }

    /// The `[providers.<id>]` TOML (no secret in it).
    pub fn block(&self) -> String {
        let mut t = format!("[providers.{}]\n", self.id);
        t.push_str(&format!(
            "{ADDED_MARK} Its key is in the OS keychain (or {}), never here.\n",
            self.key_env.as_deref().unwrap_or("the environment")
        ));
        if let Some(name) = &self.name {
            t.push_str(&format!("name = {}\n", toml_string(name)));
        }
        t.push_str(&format!("api = {}\n", toml_string(self.api.as_str())));
        t.push_str(&format!("base_url = {}\n", toml_string(&self.base_url)));
        if self.needs_key {
            if let Some(var) = &self.key_env {
                t.push_str(&format!("key_env = {}\n", toml_string(var)));
            }
        } else {
            t.push_str("auth = \"none\"\n");
        }
        if !self.headers.is_empty() {
            let pairs: Vec<String> = self
                .headers
                .iter()
                .map(|(k, v)| format!("{} = {}", toml_string(k), toml_string(v)))
                .collect();
            t.push_str(&format!("headers = {{ {} }}\n", pairs.join(", ")));
        }
        if !self.secret_headers.is_empty() {
            let names: Vec<String> = self.secret_headers.keys().map(|k| toml_string(k)).collect();
            t.push_str(&format!("secret_headers = [{}]\n", names.join(", ")));
        }
        for model in &self.models {
            t.push_str(&format!(
                "\n[providers.{}.models.{}]\n",
                self.id,
                toml_string(model)
            ));
        }
        t
    }

    /// The provider as the harness will load it (from the block alone).
    fn config(&self) -> Result<ProviderConfig, String> {
        let file = ModelsFile::parse(&self.block(), "the new provider").map_err(|e| e.0)?;
        let cfg =
            ModelsConfig::from_files(ModelsFile::defaults(), Some(file), "connect", &|_| None)
                .map_err(|e| e.0)?;
        cfg.providers
            .get(&self.id)
            .cloned()
            .ok_or_else(|| format!("provider {} did not load", self.id))
    }

    /// Writes the table into config.toml; the result must load with this provider in it.
    fn write(&self, config: &ConfigFile) -> Result<Option<PathBuf>, String> {
        let id = self.id.clone();
        let base = self.base_url.clone();
        let check = move |text: &str| -> Result<(), String> {
            let user = ModelsFile::parse(text, "config.toml").map_err(|e| e.0)?;
            let cfg =
                ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "config", &|_| None)
                    .map_err(|e| e.0)?;
            match cfg.providers.get(&id) {
                Some(p) if p.base_url == base => Ok(()),
                _ => Err(format!("[providers.{id}] did not come out as written")),
            }
        };
        config.replace_tables(
            &format!("providers.{}", self.id),
            Some(&self.block()),
            &check,
        )
    }
}

/// Every secret this connect stores, keyed as [`StaticKeys`] looks them up.
fn static_keys(id: &str, key: &str, secret_headers: &BTreeMap<String, String>) -> StaticKeys {
    let mut map = BTreeMap::new();
    if !key.is_empty() {
        map.insert(id.to_owned(), key.to_owned());
    }
    for (name, value) in secret_headers {
        let account = header_account(id, name);
        let short = account
            .strip_prefix(amp_core::providers::KEYCHAIN_ACCOUNT_PREFIX)
            .unwrap_or(&account)
            .to_owned();
        map.insert(short, value.clone());
    }
    StaticKeys {
        env: BTreeMap::new(),
        keychain: Some(map),
    }
}

/// Stores the key and secret header values in the OS keychain (never a file).
fn store_secrets(
    id: &str,
    key: &str,
    secret_headers: &BTreeMap<String, String>,
    key_env: Option<&str>,
) -> Result<(), String> {
    if key.is_empty() && secret_headers.is_empty() {
        return Ok(());
    }
    let service = setup::keychain_for_keys().map_err(|why| {
        format!(
            "{why}. Keys are only kept in the OS keychain, never in a file: set {} in the \
             environment instead (the CI/headless path)",
            key_env.unwrap_or("the provider's key_env variable")
        )
    })?;
    if !key.is_empty() {
        setup::store_key(id, key)?;
    }
    for (name, value) in secret_headers {
        amp_mcp::keychain_set(&service, &header_account(id, name), value)
            .map_err(|e| e.to_string())?;
    }
    Ok(())
}

/// The model ids the endpoint lists (with the new key), sorted.
async fn list_models(
    p: &ProviderConfig,
    key: &str,
    secret_headers: &BTreeMap<String, String>,
) -> Result<Vec<String>, String> {
    let keys = static_keys(&p.id, key, secret_headers);
    live_model_ids(p, &keys)
        .await
        .map(|ids| ids.into_iter().collect())
}

/// What the key check said, for people.
fn check_note(name: &str, check: &KeyCheck) -> String {
    match check {
        KeyCheck::Ok => format!("{name} accepted the key."),
        KeyCheck::Rejected(status) => format!("{name} rejected the key (HTTP {status})."),
        KeyCheck::Unknown(why) => {
            format!("Could not check the key with {name} ({why}); keeping it.")
        }
    }
}

fn check_json(check: &KeyCheck) -> String {
    match check {
        KeyCheck::Ok => "ok".to_owned(),
        KeyCheck::Rejected(status) => format!("rejected {status}"),
        KeyCheck::Unknown(why) => format!("unchecked: {why}"),
    }
}

// ---------------------------------------------------------------------------------------------
// connect --list
// ---------------------------------------------------------------------------------------------

/// One provider in `connect --list` (never a key).
#[derive(Debug, Serialize)]
pub struct ListedProvider {
    pub id: String,
    pub name: String,
    pub api: &'static str,
    pub base_url: String,
    /// Has a key (or needs none).
    pub key_present: bool,
    /// `env`, `keychain`, `not-needed`, `missing`, `keychain-error`.
    pub key: &'static str,
    /// Where the key comes from, for people (`env OPENROUTER_API_KEY`, `keychain`, …).
    pub key_source: String,
    /// Secret header values found in the keychain (or why not).
    pub secret_headers: Option<String>,
    /// Shipped with the harness (vs. added in config.toml).
    pub builtin: bool,
    /// Roles that run on it.
    pub roles: Vec<String>,
}

pub fn list_providers(
    cfg: &ModelsConfig,
    keys: &dyn amp_core::providers::KeyLookup,
) -> Vec<ListedProvider> {
    let shipped = shipped_ids();
    let mut by_provider: BTreeMap<String, Vec<String>> = BTreeMap::new();
    for (role, r) in cfg.resolve_all() {
        if let Ok(r) = r {
            by_provider.entry(r.provider).or_default().push(role);
        }
    }
    cfg.providers
        .values()
        .filter(|p| p.api != ProviderApi::Faux || by_provider.contains_key(&p.id))
        .map(|p| {
            let (_, source) = resolve_key(p, keys);
            let headers =
                (!p.secret_headers.is_empty()).then(|| match resolve_secret_headers(p, keys) {
                    Ok(_) => format!("{} in the keychain", p.secret_headers.join(", ")),
                    Err(why) => why,
                });
            ListedProvider {
                id: p.id.clone(),
                name: setup::display_name(p),
                api: p.api.as_str(),
                base_url: p.base_url.clone(),
                key_present: !source.is_missing(),
                key: source.kind(),
                key_source: describe_source(&source),
                secret_headers: headers,
                builtin: shipped.contains(&p.id),
                roles: by_provider.get(&p.id).cloned().unwrap_or_default(),
            }
        })
        .collect()
}

fn describe_source(source: &KeySource) -> String {
    match source {
        KeySource::Env(var) => format!("yes (env {var})"),
        KeySource::Keychain => "yes (keychain)".to_owned(),
        KeySource::NotNeeded => "not needed".to_owned(),
        KeySource::Missing { .. } => "no".to_owned(),
        KeySource::KeychainFailed(why) => format!("no (keychain: {why})"),
    }
}

/// The `--list` table as text (also the chat's `/connect`).
pub fn list_text(listed: &[ListedProvider], chat: bool) -> String {
    let id_w = listed.iter().map(|p| p.id.len()).max().unwrap_or(8).max(8);
    let name_w = listed
        .iter()
        .map(|p| p.name.len())
        .max()
        .unwrap_or(4)
        .max(4);
    let key_w = listed
        .iter()
        .map(|p| p.key_source.len())
        .max()
        .unwrap_or(3)
        .clamp(3, 40);
    let mut out = format!(
        "{:<id_w$}  {:<name_w$}  {:<key_w$}  ROLES\n",
        "PROVIDER", "NAME", "KEY"
    );
    for p in listed {
        let roles = match p.roles.len() {
            0 => "-".to_owned(),
            n if n > 4 => format!("{n} roles ({}, …)", p.roles[..3].join(", ")),
            _ => p.roles.join(", "),
        };
        out.push_str(&format!(
            "{:<id_w$}  {:<name_w$}  {:<key_w$}  {roles}\n",
            p.id, p.name, p.key_source
        ));
        if let Some(h) = &p.secret_headers {
            out.push_str(&format!("{:<id_w$}  headers: {h}\n", ""));
        }
    }
    if chat {
        out.push_str(
            "/connect <provider> stores a key (openrouter, anthropic, openai, or one above); \
             /connect <id> <base-url> [openai-completions|anthropic-messages] adds a custom one",
        );
    } else {
        out.push_str(
            "\nAdd or replace a key: amp-harness connect [provider]   remove one: amp-harness \
             disconnect <provider>",
        );
    }
    out
}

// ---------------------------------------------------------------------------------------------
// connect (CLI)
// ---------------------------------------------------------------------------------------------

/// `connect --json` (never a secret).
#[derive(Debug, Default, Serialize)]
pub struct ConnectSummary {
    pub provider: String,
    pub name: String,
    /// `keychain`, or `none` for a provider that takes no key.
    pub key: String,
    /// `ok`, `rejected <status>`, `unchecked: <why>`.
    pub check: String,
    pub secret_headers: Vec<String>,
    pub config: Option<PathBuf>,
    /// The previous config.toml, when this run changed it.
    pub backup: Option<PathBuf>,
    pub models_added: Vec<String>,
    /// The roles' new model selection, when this run changed it.
    pub selection: Option<String>,
}

enum Target {
    /// Configured already (shipped or in config.toml).
    Known(ProviderConfig),
    /// Written to config.toml by this run.
    New(CustomSpec),
}

pub async fn run(args: ConnectArgs) -> Result<(), CliError> {
    let config = config_file()?;
    let cfg = setup::load_models(&config, None)?;
    if args.list {
        let listed = list_providers(&cfg, &SystemKeys::from_env());
        if args.json {
            return print_json(&listed);
        }
        println!("{}", list_text(&listed, false));
        return Ok(());
    }
    // --key-stdin is the scripted path: no questions at all.
    let mut ui = ui::detect(!args.key_stdin && !args.json);
    let ui = ui.as_mut();

    let target = pick_target(&args, &cfg, ui)?;
    let (provider, mut spec) = match &target {
        Target::Known(p) => (p.clone(), None),
        Target::New(spec) => (spec.config().map_err(CliError::new)?, Some(spec.clone())),
    };
    let name = setup::display_name(&provider);
    if spec.is_none() && !provider.needs_key && provider.secret_headers.is_empty() {
        return Err(CliError::usage(format!(
            "{name} ({}) needs no key (auth = \"none\")",
            provider.id
        )));
    }

    // The key: hidden in a terminal, else one line of stdin.
    let keyless_ok = spec.as_ref().is_some_and(|s| s.id != "openai");
    let extra = spec
        .as_ref()
        .map(|s| s.secret_headers.clone())
        .unwrap_or_default();
    let (key, check) = read_and_check_key(&args, ui, &provider, &name, keyless_ok, &extra).await?;
    // A custom endpoint that takes no key.
    if key.is_empty()
        && let Some(s) = spec.as_mut()
    {
        s.needs_key = false;
    }

    // Model ids for a new provider.
    if let Some(s) = spec.as_mut() {
        let provider = s.config().map_err(CliError::new)?;
        s.models = pick_models(&args, ui, &provider, &key, &s.secret_headers).await?;
    }

    // Store: keychain first, then the config table.
    let key_env = spec
        .as_ref()
        .and_then(|s| s.key_env.clone())
        .or(provider.key_env.clone());
    store_secrets(&provider.id, &key, &extra, key_env.as_deref())
        .map_err(|e| CliError::new(format!("store the {name} key: {e}")))?;
    let mut summary = ConnectSummary {
        provider: provider.id.clone(),
        name: name.clone(),
        key: if key.is_empty() { "none" } else { "keychain" }.to_owned(),
        check: check_json(&check),
        secret_headers: extra.keys().cloned().collect(),
        config: Some(config.path.clone()),
        ..ConnectSummary::default()
    };
    if !key.is_empty() {
        ui.say(&format!(
            "Stored the {name} key in the OS keychain (service {}, account {}).",
            amp_mcp::keychain_service(),
            keychain_account(&provider.id)
        ));
    }
    for header in extra.keys() {
        ui.say(&format!(
            "Stored the {header} header value in the OS keychain."
        ));
    }
    if let Some(s) = &spec {
        summary.backup = s.write(&config).map_err(CliError::new)?;
        summary.models_added = s.models.clone();
        ui.say(&format!(
            "Added [providers.{}] to {}{} with {} model{}.",
            s.id,
            config.path.display(),
            summary
                .backup
                .as_ref()
                .map(|b| format!(" (previous file: {})", b.display()))
                .unwrap_or_default(),
            s.models.len(),
            if s.models.len() == 1 { "" } else { "s" }
        ));
    }

    // 3. Point the roles at it?
    summary.selection = offer_roles(&args, ui, &config, &provider.id, &summary.models_added)?;

    if args.json {
        return print_json(&summary);
    }
    let cfg = setup::load_models(&config, None)?;
    let report = config_cmd::build_report(&cfg, &SystemKeys::from_env(), false).await;
    let mode = match &cfg.selection {
        Selection::Profile { name, .. } => format!("profile {name}"),
        Selection::Inherit { model, .. } => format!("inherit {model}"),
    };
    println!("Connected {name} ({}). Models: {mode}.", provider.id);
    config_cmd::print_roles(&report);
    println!(
        "A chat that is already open picks it up with /connect; `amp-harness connect --list` shows \
         every provider."
    );
    Ok(())
}

/// The user config.toml `connect` / `disconnect` write: the global `--config` / `--config-dir`
/// (#2232), else the default path.
fn config_file() -> Result<ConfigFile, CliError> {
    crate::conn::user_config_file().map(ConfigFile::new)
}

/// Which provider: the argument, else the picker.
fn pick_target(
    args: &ConnectArgs,
    cfg: &ModelsConfig,
    ui: &mut dyn Ui,
) -> Result<Target, CliError> {
    let custom_flags = args.base_url.is_some() || args.api.is_some() || args.name.is_some();
    let chosen = match &args.provider {
        Some(p) => p.trim().to_owned(),
        None => {
            if !ui.interactive() {
                return Err(CliError::usage(
                    "which provider? `amp-harness connect <provider>` (openrouter, anthropic, \
                     openai, or a custom id with --base-url); `connect --list` shows them",
                ));
            }
            picker(cfg, ui)?
        }
    };
    let shipped = shipped_ids();
    if chosen == CUSTOM {
        return custom_target(args, ui, None).map(Target::New);
    }
    if let Some(p) = cfg.providers.get(&chosen) {
        if custom_flags {
            if shipped.contains(&chosen) {
                return Err(CliError::usage(format!(
                    "{chosen} is built in: --base-url/--api/--name are for custom providers \
                     (pick another id)"
                )));
            }
            // Redefine a provider this person added before.
            return custom_target(args, ui, Some(&chosen)).map(Target::New);
        }
        return Ok(Target::Known(p.clone()));
    }
    if chosen == "openai" && !custom_flags {
        let mut spec = openai_preset();
        spec.models = args.models.clone();
        return Ok(Target::New(spec));
    }
    valid_id(&chosen).map_err(CliError::usage)?;
    if args.base_url.is_none() && !ui.interactive() {
        return Err(CliError::usage(format!(
            "unknown provider `{chosen}`: add it as a custom provider with --base-url URL [--api \
             openai-completions|anthropic-messages] [--name NAME]"
        )));
    }
    custom_target(args, ui, Some(&chosen)).map(Target::New)
}

fn picker(cfg: &ModelsConfig, ui: &mut dyn Ui) -> Result<String, CliError> {
    let keys = SystemKeys::from_env();
    let mut ids: Vec<String> = ["openrouter", "anthropic", "openai"]
        .iter()
        .map(|s| (*s).to_owned())
        .collect();
    for (id, p) in &cfg.providers {
        if !ids.contains(id) && p.api != ProviderApi::Faux {
            ids.push(id.clone());
        }
    }
    ui.say("Which model provider do you want to connect?");
    for (i, id) in ids.iter().enumerate() {
        let (name, status) = match cfg.providers.get(id) {
            Some(p) => {
                let (_, source) = resolve_key(p, &keys);
                let status = match &source {
                    KeySource::Env(var) => format!("  [connected: env {var}]"),
                    KeySource::Keychain => "  [connected: keychain]".to_owned(),
                    _ => String::new(),
                };
                (setup::display_name(p), status)
            }
            None => (setup::provider_name(id), String::new()),
        };
        let text = about(id).map(str::to_owned).unwrap_or_else(|| {
            cfg.providers
                .get(id)
                .map(|p| format!("{} ({})", p.base_url, p.api.as_str()))
                .unwrap_or_default()
        });
        ui.say(&format!("  {}) {name} — {text}{status}", i + 1));
    }
    ui.say(&format!(
        "  {}) Custom — any OpenAI-compatible endpoint (vLLM, Ollama, LM Studio, LiteLLM, a \
         gateway…) or an Anthropic Messages one",
        ids.len() + 1
    ));
    loop {
        let a = ui.ask("Provider", Some("1"))?;
        if let Ok(n) = a.trim().parse::<usize>() {
            if (1..=ids.len()).contains(&n) {
                return Ok(ids[n - 1].clone());
            }
            if n == ids.len() + 1 {
                return Ok(CUSTOM.to_owned());
            }
        }
        if ids.contains(&a) || a == CUSTOM {
            return Ok(a);
        }
        ui.say("  Type a number from the list.");
    }
}

/// A custom provider from the flags, asking for what they leave open.
fn custom_target(
    args: &ConnectArgs,
    ui: &mut dyn Ui,
    id: Option<&str>,
) -> Result<CustomSpec, CliError> {
    let interactive = ui.interactive();
    let ask_or = |ui: &mut dyn Ui, flag: &str, prompt: &str, default: Option<&str>| {
        if interactive {
            ui.ask(prompt, default)
        } else {
            Err(CliError::usage(format!(
                "a custom provider needs {flag} (no terminal to ask in)"
            )))
        }
    };
    let name = match &args.name {
        Some(n) => Some(n.trim().to_owned()),
        None if interactive => {
            let n = ui.ask("Display name", id)?;
            Some(n.trim().to_owned()).filter(|n| !n.is_empty())
        }
        None => None,
    };
    let id = match id {
        Some(id) => id.to_owned(),
        None => loop {
            let default = name.as_deref().map(slug).filter(|s| !s.is_empty());
            let a = ask_or(
                ui,
                "a provider id",
                "Provider id (lowercase letters, digits, - and _)",
                default.as_deref(),
            )?;
            match valid_id(a.trim()) {
                Ok(()) if shipped_ids().contains(a.trim()) => {
                    ui.say(&format!("  {} is built in; pick another id.", a.trim()));
                }
                Ok(()) => break a.trim().to_owned(),
                Err(e) => ui.say(&format!("  {e}")),
            }
        },
    };
    valid_id(&id).map_err(CliError::usage)?;
    let base_url = match &args.base_url {
        Some(u) => valid_url(u).map_err(CliError::usage)?,
        None => loop {
            let a = ask_or(
                ui,
                "--base-url",
                "Base URL (e.g. http://localhost:11434/v1)",
                None,
            )?;
            match valid_url(&a) {
                Ok(u) => break u,
                Err(e) => ui.say(&format!("  {e}")),
            }
        },
    };
    let api = match &args.api {
        Some(a) => parse_api(a).map_err(CliError::usage)?,
        None if interactive => {
            ui.say("API style:");
            ui.say("  1) openai-completions — OpenAI-compatible chat completions (most endpoints)");
            ui.say("  2) anthropic-messages — the Anthropic Messages API");
            loop {
                let a = ui.ask("API", Some("1"))?;
                match a.trim() {
                    "1" => break ProviderApi::OpenaiCompletions,
                    "2" => break ProviderApi::AnthropicMessages,
                    other => match parse_api(other) {
                        Ok(api) => break api,
                        Err(e) => ui.say(&format!("  {e}")),
                    },
                }
            }
        }
        None => ProviderApi::OpenaiCompletions,
    };
    let mut spec = CustomSpec {
        key_env: Some(CustomSpec::default_key_env(&id)),
        id,
        name,
        base_url,
        api,
        models: args.models.clone(),
        ..CustomSpec::default()
    };
    for h in &args.headers {
        let (k, v) = h
            .split_once('=')
            .or_else(|| h.split_once(':'))
            .ok_or_else(|| CliError::usage(format!("--header {h}: expected NAME=VALUE")))?;
        let (k, v) = (k.trim(), v.trim());
        if secret_name(k) || secret_value(v) {
            return Err(CliError::usage(format!(
                "--header {k}: that looks like a secret, and arguments are visible to other \
                 processes. Add it in a terminal (`amp-harness connect {}`), where it is read \
                 hidden and kept in the OS keychain",
                spec.id
            )));
        }
        spec.headers.insert(k.to_owned(), v.to_owned());
    }
    if interactive && args.headers.is_empty() {
        loop {
            let name = ui.ask("Extra header name (Enter for none)", Some(""))?;
            let name = name.trim().trim_end_matches(':').to_owned();
            if name.is_empty() {
                break;
            }
            if secret_name(&name) {
                let v = ui.secret(&format!("{name} value (kept in the OS keychain)"))?;
                if !v.is_empty() {
                    spec.secret_headers.insert(name, v);
                }
                continue;
            }
            let v = ui.ask(&format!("{name} value"), None)?;
            if secret_value(&v) {
                ui.say(&format!(
                    "  That looks like a secret: {name} goes to the OS keychain, not config.toml."
                ));
                spec.secret_headers.insert(name, v.trim().to_owned());
            } else {
                spec.headers.insert(name, v.trim().to_owned());
            }
        }
    }
    Ok(spec)
}

/// Reads the key (hidden, or stdin with --key-stdin) and checks it with the provider. A rejected
/// key is asked again in a terminal (3 tries) and is an error otherwise. An empty key is allowed
/// only for a new custom provider (an endpoint without auth).
async fn read_and_check_key(
    args: &ConnectArgs,
    ui: &mut dyn Ui,
    provider: &ProviderConfig,
    name: &str,
    custom_without_key_ok: bool,
    extra: &BTreeMap<String, String>,
) -> Result<(String, KeyCheck), CliError> {
    for attempt in 0..3 {
        let key = if args.key_stdin {
            let mut line = String::new();
            std::io::stdin()
                .read_to_string(&mut line)
                .map_err(|e| CliError::new(format!("read the key from stdin: {e}")))?;
            line.lines().next().unwrap_or("").trim().to_owned()
        } else if ui.interactive() {
            let prompt = key_prompt(&provider.id, name);
            let suffix = if custom_without_key_ok {
                ", Enter if the endpoint needs none"
            } else {
                ""
            };
            ui.secret(&format!("{prompt}{suffix}"))?
        } else {
            return Err(CliError::usage(
                "no terminal to ask for the key in: pipe it in with --key-stdin (never put a key \
                 in an argument)",
            ));
        };
        if key.is_empty() {
            if custom_without_key_ok && attempt == 0 {
                ui.say(&format!("No key: {name} will be called without one."));
                let check = setup::check_key(provider, "", extra).await;
                return Ok((String::new(), check));
            }
            return Err(CliError::usage(format!(
                "no {name} key given: nothing stored"
            )));
        }
        if let Err(why) = refuse_subscription_token(&provider.id, &key) {
            if args.key_stdin || !ui.interactive() {
                return Err(CliError::usage(why));
            }
            ui.say(&format!("  {why}"));
            continue;
        }
        let check = setup::check_key(provider, &key, extra).await;
        ui.say(&format!("  {}", check_note(name, &check)));
        if let KeyCheck::Rejected(status) = check {
            if args.key_stdin || !ui.interactive() {
                return Err(CliError::new(format!(
                    "{name} rejected the key (HTTP {status}); nothing was stored"
                )));
            }
            continue;
        }
        return Ok((key, check));
    }
    Err(CliError::new(format!(
        "no {name} key was accepted; nothing was stored"
    )))
}

/// The model ids to add for a new provider: --model, else what the endpoint lists.
async fn pick_models(
    args: &ConnectArgs,
    ui: &mut dyn Ui,
    p: &ProviderConfig,
    key: &str,
    secret_headers: &BTreeMap<String, String>,
) -> Result<Vec<String>, CliError> {
    if !args.models.is_empty() {
        return Ok(args.models.clone());
    }
    let listed = list_models(p, key, secret_headers).await;
    if !ui.interactive() {
        return match listed {
            Ok(ids) if !ids.is_empty() && ids.len() <= MAX_AUTO_MODELS => Ok(ids),
            Ok(ids) if ids.is_empty() => Err(CliError::usage(format!(
                "{} lists no models: name them with --model ID",
                p.base_url
            ))),
            Ok(ids) => Err(CliError::usage(format!(
                "{} lists {} models: pick the ones to add with --model ID (repeatable)",
                p.base_url,
                ids.len()
            ))),
            Err(e) => Err(CliError::usage(format!(
                "could not list the models at {} ({e}): name them with --model ID",
                p.base_url
            ))),
        };
    }
    let ids = match listed {
        Ok(ids) if !ids.is_empty() => ids,
        Ok(_) => {
            ui.say("The endpoint lists no models.");
            Vec::new()
        }
        Err(e) => {
            ui.say(&format!("Could not list the models ({e})."));
            Vec::new()
        }
    };
    if !ids.is_empty() {
        ui.say(&format!(
            "It serves {} model{}:",
            ids.len(),
            if ids.len() == 1 { "" } else { "s" }
        ));
        for (i, id) in ids.iter().take(PICKER_MODELS).enumerate() {
            ui.say(&format!("  {:>2}) {id}", i + 1));
        }
        if ids.len() > PICKER_MODELS {
            ui.say(&format!(
                "      … and {} more (type their ids)",
                ids.len() - PICKER_MODELS
            ));
        }
    }
    let default = if !ids.is_empty() && ids.len() <= PICKER_MODELS {
        Some("all")
    } else {
        ids.first().map(String::as_str)
    };
    loop {
        let a = ui.ask(
            "Models to add (numbers or ids, comma-separated; `all`)",
            default,
        )?;
        let mut out = Vec::new();
        for part in a.split(',').map(str::trim).filter(|s| !s.is_empty()) {
            if part == "all" && !ids.is_empty() {
                out.extend(ids.iter().cloned());
            } else if let Some(n) = part
                .parse::<usize>()
                .ok()
                .filter(|n| (1..=ids.len().min(PICKER_MODELS)).contains(n))
            {
                out.push(ids[n - 1].clone());
            } else {
                out.push(part.to_owned());
            }
        }
        out.dedup();
        if !out.is_empty() {
            return Ok(out);
        }
        ui.say("  Name at least one model.");
    }
}

/// The roles' model selection that puts them on `provider`: (profile, inherit model).
fn suggestion(
    provider: &str,
    models: &[String],
    role_model: Option<&str>,
    cfg: &ModelsConfig,
) -> Option<(String, Option<String>)> {
    match provider {
        "anthropic" if cfg.profiles.contains_key("anthropic") => {
            Some(("anthropic".to_owned(), None))
        }
        "openrouter" => Some((DEFAULT_PROFILE.to_owned(), None)),
        _ => {
            let model = role_model
                .map(str::to_owned)
                .or_else(|| models.first().cloned())
                .or_else(|| {
                    cfg.providers
                        .get(provider)
                        .and_then(|p| p.models.keys().next().cloned())
                })?;
            Some(("inherit".to_owned(), Some(format!("{provider}/{model}"))))
        }
    }
}

fn describe(profile: &str, inherit: Option<&str>) -> String {
    match (profile, inherit) {
        ("inherit", Some(m)) => format!("every role on {m}"),
        ("anthropic", _) => {
            "the anthropic profile (Opus for the manager and reviewers, Sonnet for the rest)"
                .to_owned()
        }
        (DEFAULT_PROFILE, _) => {
            "the default roster on OpenRouter (MiMo, DeepSeek, GLM, Kimi)".to_owned()
        }
        (p, _) => format!("profile {p}"),
    }
}

/// Offers (or with --use, applies) the selection that puts the roles on the provider. Returns
/// the new selection when it changed.
fn offer_roles(
    args: &ConnectArgs,
    ui: &mut dyn Ui,
    config: &ConfigFile,
    provider: &str,
    models_added: &[String],
) -> Result<Option<String>, CliError> {
    let cfg = setup::load_models(config, None)?;
    let Some((profile, inherit)) =
        suggestion(provider, models_added, args.role_model.as_deref(), &cfg)
    else {
        return Ok(None);
    };
    let (current, _) = setup::profile_of(&cfg);
    let current_inherit = match &cfg.selection {
        Selection::Inherit { model, .. } => Some(model.clone()),
        _ => None,
    };
    if current == profile && (profile != "inherit" || current_inherit == inherit) {
        return Ok(None);
    }
    let what = describe(&profile, inherit.as_deref());
    let go = if args.use_for_roles {
        true
    } else if ui.interactive() {
        // Default yes when the roles can't run now (their providers have no key).
        let stuck = !setup::missing_keys(&cfg, &SystemKeys::from_env()).is_empty();
        ui.confirm(&format!("Run the agent roles on {what}?"), stuck)?
    } else {
        false
    };
    if !go {
        if !args.json {
            ui.say(&format!(
                "The roles keep {}. `amp-harness connect {provider} --use` (or [models] in \
                 config.toml) puts them on {what}.",
                describe(&current, current_inherit.as_deref())
            ));
        }
        return Ok(None);
    }
    let mut inherit = inherit;
    if profile == "inherit" && ui.interactive() && !args.use_for_roles {
        let m = ui.ask(
            &format!("Model every role uses ({provider}/<model id>)"),
            inherit.as_deref(),
        )?;
        let m = m.trim();
        inherit = Some(if m.starts_with(&format!("{provider}/")) {
            m.to_owned()
        } else {
            format!("{provider}/{m}")
        });
    }
    apply_roles(ui, config, &profile, inherit.as_deref()).map(Some)
}

fn apply_roles(
    ui: &mut dyn Ui,
    config: &ConfigFile,
    profile: &str,
    inherit: Option<&str>,
) -> Result<String, CliError> {
    let mut entries = vec![("models", "profile", profile)];
    if let Some(m) = inherit {
        entries.push(("models", "inherit", m));
    }
    config.set(&entries).map_err(CliError::new)?;
    // It must load (an inherit model the provider does not list is an error).
    if let Err(e) = setup::load_models(config, None) {
        return Err(CliError::new(format!(
            "{}; fix [models] in {}",
            e.message,
            config.path.display()
        )));
    }
    let selection = match inherit {
        Some(m) => format!("inherit {m}"),
        None => format!("profile {profile}"),
    };
    ui.say(&format!(
        "The roles now run on {} (saved in config.toml).",
        describe(profile, inherit)
    ));
    Ok(selection)
}

// ---------------------------------------------------------------------------------------------
// disconnect
// ---------------------------------------------------------------------------------------------

#[derive(Debug, Default, Serialize)]
pub struct DisconnectSummary {
    pub provider: String,
    pub key_removed: bool,
    pub headers_removed: Vec<String>,
    pub config_removed: bool,
    pub backup: Option<PathBuf>,
    /// The provider's key variable is still set in this environment.
    pub env_still_set: Option<String>,
}

pub async fn disconnect(args: DisconnectArgs) -> Result<(), CliError> {
    let config = config_file()?;
    let cfg = setup::load_models(&config, None)?;
    let id = args.provider.trim().to_owned();
    let provider = cfg.providers.get(&id);
    let text = config.text().ok().flatten().unwrap_or_default();
    let user_defined = ModelsFile::parse(&text, "config.toml")
        .ok()
        .is_some_and(|f| f.providers.contains_key(&id))
        && !shipped_ids().contains(&id);
    let ours = user_defined && added_by_connect(&text, &id);
    let remove_table = ours && !args.keep_config;
    if remove_table {
        let users: Vec<String> = cfg
            .resolve_all()
            .into_iter()
            .filter_map(|(role, r)| r.ok().filter(|r| r.provider == id).map(|_| role))
            .collect();
        if !users.is_empty() {
            return Err(CliError::usage(format!(
                "roles still run on {id} ({}): point them at another provider first \
                 (`amp-harness connect <provider> --use`), or keep its config with --keep-config",
                users.join(", ")
            )));
        }
    }
    let service = setup::keychain_for_keys();
    let mut summary = DisconnectSummary {
        provider: id.clone(),
        ..DisconnectSummary::default()
    };
    if let Ok(service) = &service {
        summary.key_removed = amp_mcp::keychain_delete(service, &keychain_account(&id))
            .map_err(|e| CliError::new(e.to_string()))?;
        for header in provider
            .map(|p| p.secret_headers.clone())
            .unwrap_or_default()
        {
            if amp_mcp::keychain_delete(service, &header_account(&id, &header))
                .map_err(|e| CliError::new(e.to_string()))?
            {
                summary.headers_removed.push(header);
            }
        }
    }
    if remove_table {
        let check = |text: &str| -> Result<(), String> {
            let user = ModelsFile::parse(text, "config.toml").map_err(|e| e.0)?;
            ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "config", &|_| None)
                .map(|_| ())
                .map_err(|e| e.0)
        };
        summary.backup = config
            .replace_tables(&format!("providers.{id}"), None, &check)
            .map_err(CliError::new)?;
        summary.config_removed = true;
    }
    summary.env_still_set = provider
        .and_then(|p| p.key_env.clone())
        .filter(|var| std::env::var(var).is_ok_and(|v| !v.trim().is_empty()));
    if provider.is_none() && !summary.key_removed {
        return Err(CliError::usage(format!(
            "unknown provider `{id}` and no key stored for it (`amp-harness connect --list`)"
        )));
    }
    if args.json {
        return print_json(&summary);
    }
    let name = provider
        .map(setup::display_name)
        .unwrap_or_else(|| id.clone());
    match (&service, summary.key_removed) {
        (Ok(_), true) => println!("Removed the {name} key from the OS keychain."),
        (Ok(_), false) => println!("No {name} key in the OS keychain."),
        (Err(why), _) => println!("The keychain was not touched: {why}."),
    }
    for h in &summary.headers_removed {
        println!("Removed the {h} header value from the OS keychain.");
    }
    if summary.config_removed {
        println!(
            "Removed [providers.{id}] from {}{}.",
            config.path.display(),
            summary
                .backup
                .as_ref()
                .map(|b| format!(" (previous file: {})", b.display()))
                .unwrap_or_default()
        );
    }
    if user_defined && !ours {
        println!(
            "[providers.{id}] in {} was written by hand, so it stays (remove it there if you no \
             longer want it).",
            config.path.display()
        );
    }
    if let Some(var) = &summary.env_still_set {
        println!(
            "{var} is still set in your environment, so {name} still has a key: unset it too."
        );
    }
    Ok(())
}

// ---------------------------------------------------------------------------------------------
// The chat's /connect
// ---------------------------------------------------------------------------------------------

/// `/connect` for the chat: checks and stores keys like `connect`, then rebuilds the runtime's
/// providers so the running harness (and workers started later) use them without a restart.
#[derive(Clone)]
pub struct ChatConnect {
    rt: Arc<Runtime>,
    config: ConfigFile,
}

impl ChatConnect {
    /// Writes the user config.toml the session loaded (`rt.paths`, #2232).
    pub fn new(rt: Arc<Runtime>) -> Result<Arc<Self>, CliError> {
        let path = rt.paths.user_file_for_writing().map_err(CliError::new)?;
        Ok(Arc::new(Self {
            rt,
            config: ConfigFile::new(path),
        }))
    }

    /// The models config as the session loads it: the user file plus the session's repo layer,
    /// so a reload after `/connect` keeps the repo's `[roles]` (#2232).
    fn load_models(&self) -> Result<ModelsConfig, String> {
        ModelsConfig::load_paths(&|k| std::env::var(k).ok(), &self.rt.paths)
            .map_err(|e| format!("models: {e}"))
    }

    /// `<id> [<base-url> [api]]` -> the target.
    fn target(&self, args: &str) -> Result<Target, String> {
        let words: Vec<&str> = args.split_whitespace().collect();
        let id = words.first().copied().unwrap_or("");
        let cfg = self.load_models()?;
        if words.len() == 1 {
            if let Some(p) = cfg.providers.get(id) {
                if !p.needs_key {
                    return Err(format!("{id} needs no key (auth = \"none\")"));
                }
                return Ok(Target::Known(p.clone()));
            }
            if id == "openai" {
                return Ok(Target::New(openai_preset()));
            }
            if id == CUSTOM {
                return Err(
                    "/connect <id> <base-url> [openai-completions|anthropic-messages] adds a \
                            custom provider"
                        .to_owned(),
                );
            }
            return Err(format!(
                "unknown provider `{id}`: /connect lists them; /connect {id} <base-url> \
                 [openai-completions|anthropic-messages] adds it as a custom one"
            ));
        }
        if words.len() > 3 {
            return Err(
                "/connect <id> <base-url> [openai-completions|anthropic-messages]".to_owned(),
            );
        }
        valid_id(id)?;
        if shipped_ids().contains(id) {
            return Err(format!("{id} is built in: /connect {id}"));
        }
        let api = match words.get(2) {
            Some(a) => parse_api(a)?,
            None => ProviderApi::OpenaiCompletions,
        };
        Ok(Target::New(CustomSpec {
            id: id.to_owned(),
            base_url: valid_url(words[1])?,
            api,
            key_env: Some(CustomSpec::default_key_env(id)),
            ..CustomSpec::default()
        }))
    }

    /// Reloads config.toml and rebuilds the providers on the runtime.
    fn reload(&self) -> Result<(ProviderSet, ModelsConfig), String> {
        let cfg = self.load_models()?;
        let set = self.rt.reload_models(cfg.clone(), &SystemKeys::from_env());
        Ok((set, cfg))
    }

    fn reply(&self, text: String) -> Result<amp_tui::ConnectReply, String> {
        let (set, cfg) = self.reload()?;
        Ok(amp_tui::ConnectReply {
            text,
            providers: set.all(),
            models: crate::chat::model_choices(&cfg, &set),
        })
    }

    async fn store(self, args: String, key: String) -> Result<amp_tui::ConnectReply, String> {
        refuse_subscription_token("", &key)?;
        let (provider, mut spec) = match self.target(&args)? {
            Target::Known(p) => (p, None),
            Target::New(s) => (s.config()?, Some(s)),
        };
        let name = setup::display_name(&provider);
        let check = setup::check_key(&provider, &key, &BTreeMap::new()).await;
        if let KeyCheck::Rejected(status) = check {
            return Err(format!(
                "{name} rejected the key (HTTP {status}); nothing was stored. /connect {} to try \
                 again",
                provider.id
            ));
        }
        if let Some(s) = spec.as_mut() {
            let ids = list_models(&provider, &key, &BTreeMap::new())
                .await
                .map_err(|e| {
                    format!(
                        "could not list the models at {} ({e}); add it with `amp-harness connect \
                         {}` in a terminal to name them",
                        provider.base_url, provider.id
                    )
                })?;
            if ids.is_empty() || ids.len() > MAX_AUTO_MODELS {
                return Err(format!(
                    "{} lists {} models: add it with `amp-harness connect {}` in a terminal to \
                     pick them",
                    provider.base_url,
                    ids.len(),
                    provider.id
                ));
            }
            s.models = ids;
        }
        let key_env = spec
            .as_ref()
            .and_then(|s| s.key_env.clone())
            .or(provider.key_env.clone());
        store_secrets(&provider.id, &key, &BTreeMap::new(), key_env.as_deref())?;
        let mut text = format!(
            "{} Stored in the OS keychain (account {}).",
            check_note(&name, &check),
            keychain_account(&provider.id)
        );
        if let Some(s) = &spec {
            s.write(&self.config)?;
            text.push_str(&format!(
                "\nAdded [providers.{}] to config.toml with {} model{}.",
                s.id,
                s.models.len(),
                if s.models.len() == 1 { "" } else { "s" }
            ));
        }
        text.push_str(&format!(
            "\n{name}'s models are available now (/model lists them). `amp-harness connect {} \
             --use` points the agent roles at it.",
            provider.id
        ));
        self.reply(text)
    }

    async fn list_now(self) -> Result<amp_tui::ConnectReply, String> {
        // Keys stored in another terminal since this chat started: forget the cached "missing".
        if let Ok(service) = setup::keychain_for_keys() {
            let cfg = self.load_models()?;
            let keys = SystemKeys::from_env();
            for p in cfg.providers.values() {
                if resolve_key(p, &keys).1.is_missing() {
                    amp_mcp::keychain_forget(&service, &keychain_account(&p.id));
                }
                if resolve_secret_headers(p, &keys).is_err() {
                    for h in &p.secret_headers {
                        amp_mcp::keychain_forget(&service, &header_account(&p.id, h));
                    }
                }
            }
        }
        let cfg = self.load_models()?;
        let text = list_text(&list_providers(&cfg, &SystemKeys::from_env()), true);
        self.reply(text)
    }
}

impl amp_tui::ProviderConnect for ChatConnect {
    fn list(&self) -> amp_tui::ConnectFuture {
        let me = self.clone();
        Box::pin(async move { me.list_now().await })
    }

    fn prepare(&self, args: &str) -> Result<String, String> {
        let (id, name) = match self.target(args)? {
            Target::Known(p) => (p.id.clone(), setup::display_name(&p)),
            Target::New(s) => (
                s.id.clone(),
                s.name
                    .clone()
                    .unwrap_or_else(|| setup::provider_name(&s.id)),
            ),
        };
        Ok(key_prompt(&id, &name))
    }

    fn connect(&self, args: &str, key: String) -> amp_tui::ConnectFuture {
        let me = self.clone();
        let args = args.to_owned();
        Box::pin(async move { me.store(args, key).await })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn subscription_tokens_are_refused() {
        assert!(refuse_subscription_token("anthropic", "sk-ant-oat01-abc").is_err());
        assert!(refuse_subscription_token("x", " sk-ant-oat01-abc").is_err());
        assert!(refuse_subscription_token("anthropic", "sk-ant-api03-abc").is_ok());
        let why = refuse_subscription_token("anthropic", "sk-ant-oat01-abc").unwrap_err();
        assert!(why.contains("console.anthropic.com"), "{why}");
    }

    #[test]
    fn secret_headers_are_recognised() {
        assert!(secret_name("Authorization"));
        assert!(secret_name("X-Api-Key"));
        assert!(secret_name("cf-access-token"));
        assert!(!secret_name("X-Title"));
        assert!(secret_value("Bearer abc"));
        assert!(secret_value("sk-1234"));
        assert!(secret_value("a1b2c3d4e5f6g7h8i9j0k1l2m3"));
        assert!(!secret_value("AMP Harness"));
        assert!(!secret_value("https://github.com/x/y"));
    }

    #[test]
    fn ids_urls_and_slugs() {
        assert!(valid_id("my-llm_2").is_ok());
        assert!(valid_id("My").is_err());
        assert!(valid_id("a/b").is_err());
        assert!(valid_id("custom").is_err());
        assert!(valid_id("").is_err());
        assert_eq!(slug("My Local LLM!"), "my-local-llm");
        assert_eq!(
            valid_url("http://localhost:11434/v1/").unwrap(),
            "http://localhost:11434/v1"
        );
        assert!(valid_url("localhost:1").is_err());
        assert!(valid_url("https://").is_err());
        assert_eq!(CustomSpec::default_key_env("my-llm"), "MY_LLM_API_KEY");
    }

    #[test]
    fn a_custom_block_loads_and_holds_no_secret() {
        let spec = CustomSpec {
            id: "gw".into(),
            name: Some("My \"gateway\"".into()),
            base_url: "https://gw.example/v1".into(),
            key_env: Some("GW_API_KEY".into()),
            headers: BTreeMap::from([("X-Title".into(), "AMP".into())]),
            secret_headers: BTreeMap::from([("X-Api-Key".into(), "TOPSECRET".into())]),
            models: vec!["vendor/model-a".into(), "b".into()],
            ..CustomSpec::default()
        };
        let block = spec.block();
        assert!(!block.contains("TOPSECRET"), "{block}");
        assert!(added_by_connect(&block, "gw"));
        assert!(!added_by_connect(&block, "other"));
        assert!(!added_by_connect("[providers.gw]\napi = \"faux\"\n", "gw"));
        let p = spec.config().unwrap();
        assert_eq!(p.name.as_deref(), Some("My \"gateway\""));
        assert_eq!(p.base_url, "https://gw.example/v1");
        assert_eq!(p.key_env.as_deref(), Some("GW_API_KEY"));
        assert_eq!(p.secret_headers, vec!["X-Api-Key".to_owned()]);
        assert!(p.models.contains_key("vendor/model-a") && p.models.contains_key("b"));
        assert_eq!(p.models["b"].headers.as_ref().unwrap()["X-Title"], "AMP");

        let keyless = CustomSpec {
            id: "local".into(),
            base_url: "http://localhost:1/v1".into(),
            needs_key: false,
            models: vec!["m".into()],
            ..CustomSpec::default()
        };
        assert!(!keyless.config().unwrap().needs_key);
    }

    #[test]
    fn suggestions_follow_the_provider() {
        let cfg = ModelsConfig::from_files(ModelsFile::defaults(), None, "t", &|_| None).unwrap();
        assert_eq!(
            suggestion("anthropic", &[], None, &cfg),
            Some(("anthropic".into(), None))
        );
        assert_eq!(
            suggestion("openrouter", &[], None, &cfg),
            Some((DEFAULT_PROFILE.into(), None))
        );
        assert_eq!(
            suggestion("gw", &["m1".into(), "m2".into()], None, &cfg),
            Some(("inherit".into(), Some("gw/m1".into())))
        );
        assert_eq!(
            suggestion("gw", &["m1".into()], Some("m2"), &cfg),
            Some(("inherit".into(), Some("gw/m2".into())))
        );
        assert_eq!(suggestion("gw", &[], None, &cfg), None);
    }

    #[test]
    fn the_list_never_shows_a_key() {
        let cfg = ModelsConfig::from_files(ModelsFile::defaults(), None, "t", &|_| None).unwrap();
        let keys = StaticKeys {
            env: BTreeMap::from([("ANTHROPIC_API_KEY".into(), "sk-ant-TOPSECRET".into())]),
            keychain: Some(BTreeMap::from([(
                "openrouter".into(),
                "sk-or-TOPSECRET".into(),
            )])),
        };
        let listed = list_providers(&cfg, &keys);
        let text = list_text(&listed, false);
        let json = serde_json::to_string(&listed).unwrap();
        assert!(!text.contains("TOPSECRET") && !json.contains("TOPSECRET"));
        let or = listed.iter().find(|p| p.id == "openrouter").unwrap();
        assert_eq!(
            (or.key, or.key_present, or.builtin),
            ("keychain", true, true)
        );
        assert!(!or.roles.is_empty());
        let an = listed.iter().find(|p| p.id == "anthropic").unwrap();
        assert_eq!(an.key_source, "yes (env ANTHROPIC_API_KEY)");
    }
}
