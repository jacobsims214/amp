//! `amp-harness models list|set|reset|profile` (#2235, H12 of the configurability design): the
//! model each role runs on, changed without editing config.toml by hand.
//!
//! - `models list [--available] [--check] [--json]`: each role's provider/model/thinking and the
//!   layer that set it; `--available` lists the providers' models instead (`--check` adds the
//!   live catalogs).
//! - `models set <role> <provider/model> [--thinking L] [--user|--repo] [--no-check]`: writes
//!   `[roles.<role>] model` (and `thinking`). The provider must be configured. A model the config
//!   doesn't define yet is looked up in the provider's live catalog and, in the user file, added
//!   as an empty `[providers.<p>.models."<m>"]` table; a model the catalog doesn't list is refused.
//! - `models reset <role> [--user|--repo]`: drops `[roles.<role>]` from that file, so the role
//!   falls back to the layer below (profile, then the shipped default).
//! - `models profile <opencode|anthropic|inherit|name> [--inherit p/m] [--thinking L]`: writes
//!   `[models] profile` (and `inherit` / `inherit_thinking`, user file only).
//!
//! `--user` (default) edits the user config.toml (`--config` / `--config-dir` apply); `--repo`
//! edits `<repo>/.amp/harness/config.toml`, which may hold only `[roles.*]` and `[models] profile`.
//! Every write goes through [`ConfigFile::edit`]: comments and other settings stay, the old file
//! is kept as `config.toml.bak`, and nothing is written unless the result loads and the role
//! resolves. Keys are never printed.

use std::path::PathBuf;

use amp_core::{
    models::{
        INHERIT, ModelsConfig, ModelsFile, normalize_role, parse_model_ref, parse_thinking,
        thinking_name,
    },
    providers::{KeyLookup, SystemKeys, live_model_ids, resolve_key},
    repo_config::RepoConfig,
};
use clap::{Args, Subcommand};
use serde::Serialize;

use crate::{
    config_cmd,
    conn::{CliError, print_json},
    setup::config_file::{Change, ConfigFile},
};

#[derive(Subcommand)]
pub enum ModelsCommand {
    /// Each role's provider/model/thinking and the layer that set it (`--available`: the
    /// providers' models).
    List(ListArgs),
    /// Run a role on a model: `models set reviewer anthropic/claude-opus-5-5 --thinking high`.
    Set(SetArgs),
    /// Drop a role's setting from the user (or repo) file; it falls back to the layer below.
    Reset(ResetArgs),
    /// Switch every role's assignment: `opencode` (shipped), `anthropic`, a [profiles.X] of yours,
    /// or `inherit --inherit provider/model` (one model for every role).
    Profile(ProfileArgs),
}

#[derive(Args)]
pub struct ListArgs {
    /// List the configured providers' models instead of the roles.
    #[arg(long)]
    available: bool,
    /// Ask each provider in use for its live catalog (network; needs the provider's key).
    #[arg(long)]
    check: bool,
    /// Print JSON.
    #[arg(long)]
    json: bool,
}

#[derive(Args, Clone, Copy)]
pub struct ScopeArgs {
    /// Write the user config.toml (the default).
    #[arg(long, conflicts_with = "repo")]
    user: bool,
    /// Write this repo's .amp/harness/config.toml (shared with everyone in the repo).
    #[arg(long)]
    repo: bool,
}

#[derive(Args)]
pub struct SetArgs {
    /// The role (agent id; the `amp-` prefix is optional).
    role: String,
    /// `provider/model`, e.g. `openrouter/z-ai/glm-5.3`.
    model: String,
    /// off, minimal, low, medium, high, xhigh, max.
    #[arg(long)]
    thinking: Option<String>,
    #[command(flatten)]
    scope: ScopeArgs,
    /// Don't ask the provider's live catalog whether it has the model.
    #[arg(long)]
    no_check: bool,
    /// Print JSON.
    #[arg(long)]
    json: bool,
}

#[derive(Args)]
pub struct ResetArgs {
    /// The role (the `amp-` prefix is optional).
    role: String,
    #[command(flatten)]
    scope: ScopeArgs,
    /// Print JSON.
    #[arg(long)]
    json: bool,
}

#[derive(Args)]
pub struct ProfileArgs {
    /// `opencode`, `anthropic`, `inherit` or a [profiles.<name>] of yours.
    name: String,
    /// With `inherit`: the `provider/model` every role runs on (user file only).
    #[arg(long, value_name = "PROVIDER/MODEL")]
    inherit: Option<String>,
    /// With `inherit`: the thinking level of every role.
    #[arg(long)]
    thinking: Option<String>,
    #[command(flatten)]
    scope: ScopeArgs,
    /// Don't ask the provider's live catalog whether it has the inherited model.
    #[arg(long)]
    no_check: bool,
    /// Print JSON.
    #[arg(long)]
    json: bool,
}

/// `models set|reset|profile --json`. Add fields, never rename or remove them.
#[derive(Debug, Serialize)]
pub struct Outcome {
    /// `set`, `reset` or `profile`.
    pub action: &'static str,
    /// `user` or `repo`.
    pub scope: &'static str,
    pub file: String,
    /// False when the file already said so (nothing written).
    pub changed: bool,
    /// The previous file, kept as config.toml.bak.
    pub backup: Option<String>,
    /// The role's resolution after the change (`set`, `reset`).
    pub role: Option<config_cmd::RoleReport>,
    /// The assignment mode after the change (`profile`).
    pub profile: Option<String>,
    /// Model tables added to the user file (`providers.<p>.models."<m>"`).
    pub added_models: Vec<String>,
    pub warnings: Vec<String>,
}

pub async fn run(command: ModelsCommand) -> Result<(), CliError> {
    match command {
        ModelsCommand::List(args) => list(args).await,
        ModelsCommand::Set(args) => set(args).await,
        ModelsCommand::Reset(args) => reset(args).await,
        ModelsCommand::Profile(args) => profile(args).await,
    }
}

fn env(name: &str) -> Option<String> {
    std::env::var(name).ok()
}

/// The config paths for this directory (with its repo layer).
fn paths() -> Result<amp_mcp::ConfigPaths, CliError> {
    let cwd =
        std::env::current_dir().map_err(|e| CliError::new(format!("current directory: {e}")))?;
    Ok(crate::conn::config_paths_in(&cwd))
}

/// The file one scope writes.
struct Target {
    repo: bool,
    file: ConfigFile,
}

impl Target {
    fn new(paths: &amp_mcp::ConfigPaths, scope: ScopeArgs) -> Result<Self, CliError> {
        if scope.repo {
            let file = paths.repo_file().ok_or_else(|| {
                CliError::usage(
                    "--repo: this directory is not in an AMP project (no .amp.json above it); \
                     run `amp-harness init` here, or leave out --repo to change your user config",
                )
            })?;
            Ok(Self {
                repo: true,
                file: ConfigFile::repo(file),
            })
        } else {
            let file = paths.user_file_for_writing().map_err(CliError::new)?;
            Ok(Self {
                repo: false,
                file: ConfigFile::new(file),
            })
        }
    }

    fn scope(&self) -> &'static str {
        if self.repo { "repo" } else { "user" }
    }

    /// The file as TOML (empty when it doesn't exist).
    fn doc(&self) -> Result<toml::Table, CliError> {
        match self.file.text().map_err(CliError::new)? {
            Some(text) => toml::from_str(&text).map_err(|e| {
                CliError::new(format!(
                    "{} does not parse ({e}); fix it first (`amp-harness config check`), nothing was changed",
                    self.file.path.display()
                ))
            }),
            None => Ok(toml::Table::new()),
        }
    }

    /// The `[roles.*]` tables in this file that name `role` (`role` and `amp-role`).
    fn role_tables(&self, role: &str) -> Result<Vec<String>, CliError> {
        let doc = self.doc()?;
        let roles = doc.get("roles").and_then(toml::Value::as_table);
        Ok([role.to_owned(), format!("amp-{role}")]
            .into_iter()
            .filter(|k| roles.is_some_and(|r| r.contains_key(k)))
            .collect())
    }
}

/// The models config with `new_text` in place of the target file: what the change would load as.
fn load_with(
    paths: &amp_mcp::ConfigPaths,
    target: &Target,
    new_text: &str,
) -> Result<ModelsConfig, String> {
    let read = |path: Option<PathBuf>| -> Result<Option<String>, String> {
        match path {
            Some(path) => match std::fs::read_to_string(&path) {
                Ok(text) => Ok(Some(text)),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
                Err(e) => Err(format!("read {}: {e}", path.display())),
            },
            None => Ok(None),
        }
    };
    let (user_text, repo_text) = if target.repo {
        (read(paths.user_file.clone())?, Some(new_text.to_owned()))
    } else {
        (Some(new_text.to_owned()), read(paths.repo_file())?)
    };
    let user_label = paths
        .user_file
        .as_ref()
        .map(|p| format!("config {}", p.display()))
        .unwrap_or_else(|| "config".into());
    let user = user_text
        .map(|t| ModelsFile::parse(&t, &user_label))
        .transpose()
        .map_err(|e| e.0)?;
    let repo = match (paths.repo_file(), repo_text) {
        (Some(path), Some(text)) => Some(RepoConfig::parse(&text, &path).map_err(|e| e.0)?),
        _ => None,
    };
    let mut config = ModelsConfig::from_layers(
        ModelsFile::defaults(),
        user,
        &user_label,
        repo.as_ref(),
        &env,
    )
    .map_err(|e| e.0)?;
    config.config_file = paths.user_file.clone();
    Ok(config)
}

/// Checks `provider/model` against the merged config and the live catalog. Returns the changes
/// that add a missing model table (user file only) and warnings; errors when it can't work.
async fn check_model(
    current: &ModelsConfig,
    target: &Target,
    model_ref: &str,
    no_check: bool,
    keys: &dyn KeyLookup,
    warnings: &mut Vec<String>,
) -> Result<Vec<(Change, String)>, CliError> {
    let (provider_id, model_id) = parse_model_ref(model_ref).map_err(|e| CliError::usage(e.0))?;
    let Some(provider) = current.providers.get(provider_id) else {
        return Err(CliError::new(format!(
            "unknown provider `{provider_id}` (configured: {}); add it with `amp-harness connect`",
            current
                .providers
                .keys()
                .cloned()
                .collect::<Vec<_>>()
                .join(", ")
        )));
    };
    let (key, source) = resolve_key(provider, keys);
    let has_key = !provider.needs_key || key.is_some();
    if !has_key {
        warnings.push(format!(
            "provider `{provider_id}` has no key ({source}), so `{model_ref}` was not checked against its live catalog; the role fails until you run `amp-harness connect {provider_id}`"
        ));
    }
    let configured = provider.models.contains_key(model_id);
    let live = if no_check || !has_key {
        None
    } else {
        match live_model_ids(provider, keys).await {
            Ok(ids) => Some(ids),
            Err(e) => {
                warnings.push(format!(
                    "could not read {provider_id}'s live catalog ({e}); `{model_ref}` was not checked"
                ));
                None
            }
        }
    };
    if let Some(ids) = &live
        && !ids.contains(model_id)
    {
        let near: Vec<&String> = ids
            .iter()
            .filter(|id| {
                let tail = model_id.rsplit('/').next().unwrap_or(model_id);
                id.contains(tail.split('-').next().unwrap_or(tail))
            })
            .take(5)
            .collect();
        let hint = if near.is_empty() {
            String::new()
        } else {
            format!(
                "; similar: {}",
                near.iter()
                    .map(|s| s.as_str())
                    .collect::<Vec<_>>()
                    .join(", ")
            )
        };
        return Err(CliError::new(format!(
            "{provider_id}'s live catalog has no model `{model_id}`{hint}. \
             `amp-harness models list --available --check` lists them; --no-check sets it anyway"
        )));
    }
    if configured {
        return Ok(Vec::new());
    }
    let header = format!(
        "[providers.{provider_id}.models.{}]",
        crate::setup::config_file::toml_key(model_id)
    );
    if target.repo {
        return Err(CliError::new(format!(
            "provider `{provider_id}` has no model `{model_id}` in your config. A repo file can't \
             define models: add {header} to your user config.toml (or run `amp-harness models set \
             <role> {model_ref}` once without --repo), then set it here"
        )));
    }
    if live.is_none() {
        warnings.push(format!(
            "added {header} with default settings (128K context, no reasoning); edit it if the model differs"
        ));
    }
    Ok(vec![(
        Change::AddTable(vec![
            "providers".into(),
            provider_id.into(),
            "models".into(),
            model_id.into(),
        ]),
        format!("providers.{provider_id}.models.\"{model_id}\""),
    )])
}

fn role_id(role: &str) -> Result<String, CliError> {
    let role = normalize_role(role);
    if role.is_empty()
        || !role
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
    {
        return Err(CliError::usage(format!(
            "`{role}` is not a role id (letters, digits, `-` and `_`)"
        )));
    }
    Ok(role.to_owned())
}

/// The current merged config, or why it does not load.
fn current(paths: &amp_mcp::ConfigPaths) -> Result<ModelsConfig, CliError> {
    ModelsConfig::load_paths(&env, paths).map_err(|e| {
        CliError::new(format!(
            "the config does not load ({e}); `amp-harness config check` shows every problem"
        ))
    })
}

/// One role's line, as `config show` reports it.
async fn role_report(
    config: &ModelsConfig,
    keys: &dyn KeyLookup,
    role: &str,
) -> config_cmd::RoleReport {
    let report = config_cmd::build_report(config, keys, false).await;
    match report.roles.into_iter().find(|r| r.role == role) {
        Some(r) => r,
        None => config_cmd::RoleReport {
            role: role.to_owned(),
            provider: None,
            model: None,
            thinking: None,
            requested_thinking: None,
            source: None,
            ready: false,
            live: None,
            error: config.resolve_role(role).err().map(|e| e.0),
        },
    }
}

/// Writes `changes` after `check` accepted the result; returns (changed, backup).
fn write(
    paths: &amp_mcp::ConfigPaths,
    target: &Target,
    changes: &[Change],
    check: &dyn Fn(&ModelsConfig) -> Result<(), String>,
) -> Result<(bool, Option<PathBuf>, ModelsConfig), CliError> {
    let text = target.file.preview(changes).map_err(CliError::new)?;
    let config = load_with(paths, target, &text).and_then(|c| check(&c).map(|()| c));
    let config = config.map_err(|e| {
        CliError::new(format!(
            "{}: the change would not load ({e}); nothing was written",
            target.file.path.display()
        ))
    })?;
    let before = target.file.text().map_err(CliError::new)?;
    if before.as_deref() == Some(text.as_str()) {
        return Ok((false, None, config));
    }
    let backup = target.file.edit(changes).map_err(CliError::new)?;
    Ok((true, backup, config))
}

fn finish(outcome: &Outcome, json: bool) -> Result<(), CliError> {
    if json {
        return print_json(outcome);
    }
    if outcome.changed {
        println!("Updated {} ({} config).", outcome.file, outcome.scope);
    } else {
        println!("{} already says so; nothing changed.", outcome.file);
    }
    if let Some(b) = &outcome.backup {
        println!("  previous file: {b}");
    }
    for m in &outcome.added_models {
        println!("  added [{m}]");
    }
    if let Some(p) = &outcome.profile {
        println!("  models: {p}");
    }
    if let Some(r) = &outcome.role {
        match (&r.provider, &r.model, &r.error) {
            (Some(p), Some(m), _) => println!(
                "  {}: {p}/{m}, thinking {} ({})",
                r.role,
                r.thinking.unwrap_or("-"),
                r.source.as_deref().unwrap_or("")
            ),
            (_, _, Some(e)) => println!("  {}: {e}", r.role),
            _ => {}
        }
    }
    for w in &outcome.warnings {
        eprintln!("warning: {w}");
    }
    Ok(())
}

async fn set(args: SetArgs) -> Result<(), CliError> {
    let role = role_id(&args.role)?;
    let thinking = args
        .thinking
        .as_deref()
        .map(|t| parse_thinking(t).map(thinking_name))
        .transpose()
        .map_err(|e| CliError::usage(e.0))?;
    let model_ref = args.model.trim().to_owned();
    let paths = paths()?;
    let target = Target::new(&paths, args.scope)?;
    let config = current(&paths)?;
    let keys = SystemKeys::from_env();
    let mut warnings = Vec::new();
    if config.role_source(&role).is_none()
        && !matches!(
            config.selection,
            amp_core::models::Selection::Inherit { .. }
        )
    {
        warnings.push(format!(
            "`{role}` is a new role; it is used by an agent whose id is `{role}` or `amp-{role}`"
        ));
    }
    let added = check_model(
        &config,
        &target,
        &model_ref,
        args.no_check,
        &keys,
        &mut warnings,
    )
    .await?;

    let tables = target.role_tables(&role)?;
    let section = match tables.as_slice() {
        [only] => format!("roles.{only}"),
        _ => format!("roles.{role}"),
    };
    let doc = target.doc()?;
    let existing = super_table(&doc, &section);
    let mut changes: Vec<Change> = added.iter().map(|(c, _)| c.clone()).collect();
    let old_model = existing
        .and_then(|t| t.get("model"))
        .and_then(toml::Value::as_str)
        .map(str::to_owned);
    if old_model.as_deref() != Some(model_ref.as_str())
        && existing.is_some_and(|t| t.contains_key("extra_body"))
    {
        // It was written for the old model (D11's rule for layers, applied within one file).
        changes.push(Change::remove(&section, "extra_body"));
        changes.push(Change::DropTables(format!("{section}.extra_body")));
        warnings.push(format!(
            "removed [{section}] extra_body: it was for {}",
            old_model.as_deref().unwrap_or("the previous model")
        ));
    }
    changes.push(Change::set(&section, "model", &model_ref));
    if let Some(t) = thinking {
        changes.push(Change::set(&section, "thinking", t));
    }

    let repo_scope = target.repo;
    let want = model_ref.clone();
    let role_for_check = role.clone();
    let (changed, backup, after) = write(&paths, &target, &changes, &move |c: &ModelsConfig| {
        let r = c.resolve_role(&role_for_check).map_err(|e| e.0)?;
        // Only the repo layer can override the user's choice; that is a warning, not a failure.
        if r.model_ref() != want && (repo_scope || !r.source.contains("repo ")) {
            return Err(format!(
                "`{role_for_check}` would resolve to {}",
                r.model_ref()
            ));
        }
        Ok(())
    })?;
    let resolved = after.resolve_role(&role).map_err(|e| CliError::new(e.0))?;
    if resolved.model_ref() != model_ref {
        warnings.push(format!(
            "the repo's config sets `{role}` to {} and wins over your user config here; `amp-harness models reset {role} --repo` drops it",
            resolved.model_ref()
        ));
    }
    if let (Some(asked), Some(req)) = (thinking, resolved.requested_thinking)
        && thinking_name(resolved.thinking) != asked
        && thinking_name(req) == asked
    {
        warnings.push(format!(
            "{model_ref} does not support thinking `{asked}`; it runs at `{}`",
            thinking_name(resolved.thinking)
        ));
    }
    let outcome = Outcome {
        action: "set",
        scope: target.scope(),
        file: target.file.path.display().to_string(),
        changed,
        backup: backup.map(|b| b.display().to_string()),
        role: Some(role_report(&after, &keys, &role).await),
        profile: None,
        added_models: if changed {
            added.into_iter().map(|(_, k)| k).collect()
        } else {
            Vec::new()
        },
        warnings,
    };
    finish(&outcome, args.json)
}

/// The table at a dotted path (role ids never contain `.`).
fn super_table<'a>(doc: &'a toml::Table, section: &str) -> Option<&'a toml::Table> {
    crate::setup::config_file::table_at(doc, section)
}

async fn reset(args: ResetArgs) -> Result<(), CliError> {
    let role = role_id(&args.role)?;
    let paths = paths()?;
    let target = Target::new(&paths, args.scope)?;
    let keys = SystemKeys::from_env();
    let tables = target.role_tables(&role)?;
    let mut warnings = Vec::new();
    let changes: Vec<Change> = tables
        .iter()
        .map(|t| Change::DropTables(format!("roles.{t}")))
        .collect();
    let (changed, backup, after) = if changes.is_empty() {
        (false, None, current(&paths)?)
    } else {
        write(&paths, &target, &changes, &|_| Ok(()))?
    };
    if changes.is_empty() {
        warnings.push(format!(
            "{} has no [roles.{role}]; nothing to reset{}",
            target.file.path.display(),
            after
                .role_source(&role)
                .map(|s| format!(" (it comes from {s})"))
                .unwrap_or_default()
        ));
    }
    if let Err(e) = after.resolve_role(&role) {
        warnings.push(format!("`{role}` now has no model: {e}"));
    }
    let outcome = Outcome {
        action: "reset",
        scope: target.scope(),
        file: target.file.path.display().to_string(),
        changed,
        backup: backup.map(|b| b.display().to_string()),
        role: Some(role_report(&after, &keys, &role).await),
        profile: None,
        added_models: Vec::new(),
        warnings,
    };
    finish(&outcome, args.json)
}

async fn profile(args: ProfileArgs) -> Result<(), CliError> {
    let name = args.name.trim().to_owned();
    let paths = paths()?;
    let target = Target::new(&paths, args.scope)?;
    if target.repo && (args.inherit.is_some() || args.thinking.is_some()) {
        return Err(CliError::usage(
            "--inherit and --thinking go in your user config (a repo file may set only [models] profile); leave out --repo",
        ));
    }
    if name != INHERIT && (args.inherit.is_some() || args.thinking.is_some()) {
        return Err(CliError::usage(format!(
            "--inherit and --thinking apply only to `models profile inherit`, not `{name}`"
        )));
    }
    let thinking = args
        .thinking
        .as_deref()
        .map(|t| parse_thinking(t).map(thinking_name))
        .transpose()
        .map_err(|e| CliError::usage(e.0))?;
    let keys = SystemKeys::from_env();
    let mut warnings = Vec::new();
    let mut changes = Vec::new();
    let mut added = Vec::new();
    if let Some(model_ref) = args.inherit.as_deref().map(str::trim) {
        // The providers come from the user file, which loads on its own even when the profile
        // the person is fixing doesn't.
        let base = load_providers(&paths)?;
        added = check_model(
            &base,
            &target,
            model_ref,
            args.no_check,
            &keys,
            &mut warnings,
        )
        .await?;
        changes.extend(added.iter().map(|(c, _)| c.clone()));
        changes.push(Change::set("models", "inherit", model_ref));
    }
    if let Some(t) = thinking {
        changes.push(Change::set("models", "inherit_thinking", t));
    }
    changes.push(Change::set("models", "profile", &name));
    let (changed, backup, after) = write(&paths, &target, &changes, &|_| Ok(()))?;
    if let Some(p) = env(amp_core::models::ENV_PROFILE).filter(|p| !p.trim().is_empty()) {
        warnings.push(format!(
            "{} = {p} is set and overrides [models] profile until it is unset",
            amp_core::models::ENV_PROFILE
        ));
    } else if !target.repo
        && let Some(repo) = paths
            .repo_file()
            .and_then(|f| RepoConfig::load(&f).ok().flatten())
        && let Some(p) = repo.profile
    {
        warnings.push(format!(
            "{} sets profile = \"{p}\", which wins in this repo; `amp-harness models profile <name> --repo` changes it",
            repo.path.display()
        ));
    }
    let profile = match &after.selection {
        amp_core::models::Selection::Profile { name, source } => {
            format!("profile {name} ({source})")
        }
        amp_core::models::Selection::Inherit { model, source, .. } => {
            format!("inherit {model} ({source})")
        }
    };
    let outcome = Outcome {
        action: "profile",
        scope: target.scope(),
        file: target.file.path.display().to_string(),
        changed,
        backup: backup.map(|b| b.display().to_string()),
        role: None,
        profile: Some(profile),
        added_models: if changed {
            added.into_iter().map(|(_, k)| k).collect()
        } else {
            Vec::new()
        },
        warnings,
    };
    finish(&outcome, args.json)?;
    if !args.json && changed {
        println!();
        let report = config_cmd::build_report(&after, &keys, false).await;
        config_cmd::print_roles(&report);
    }
    Ok(())
}

/// Defaults + the user's providers, with the shipped profile: for checking a model while the
/// selection itself may be what's broken.
fn load_providers(paths: &amp_mcp::ConfigPaths) -> Result<ModelsConfig, CliError> {
    let mut user = match &paths.user_file {
        Some(path) => ModelsFile::load(path).map_err(|e| CliError::new(e.0))?,
        None => None,
    }
    .unwrap_or_default();
    user.models = Default::default();
    user.roles.clear();
    ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "config", &|_| None)
        .map_err(|e| CliError::new(e.0))
}

/// `models list --json`. Add fields, never rename or remove them.
#[derive(Debug, Serialize)]
struct Available {
    providers: Vec<AvailableProvider>,
}

#[derive(Debug, Serialize)]
struct AvailableProvider {
    id: String,
    key_source: String,
    in_use: bool,
    /// Defined in the config (shipped or yours).
    models: Vec<String>,
    /// `--check`: what the live catalog lists that the config doesn't define.
    live_only: Option<Vec<String>>,
    check_error: Option<String>,
}

async fn list(args: ListArgs) -> Result<(), CliError> {
    let paths = paths()?;
    let config = current(&paths)?;
    let keys = SystemKeys::from_env();
    if !args.available {
        let report = config_cmd::build_report(&config, &keys, args.check).await;
        if args.json {
            print_json(&report)?;
        } else {
            let mode = match report.mode {
                "inherit" => format!("inherit {}", report.selection),
                _ => format!("profile {}", report.selection),
            };
            println!("Models: {mode} ({})", report.selection_source);
            config_cmd::print_roles(&report);
        }
        return if report.ok {
            Ok(())
        } else {
            Err(CliError::silent(1))
        };
    }
    let in_use = config.providers_in_use();
    let mut providers = Vec::new();
    for p in config.providers.values() {
        let (key, source) = resolve_key(p, &keys);
        let (live_only, check_error) = if args.check && p.needs_key && key.is_none() {
            (None, Some(format!("not checked: no key ({source})")))
        } else if args.check {
            match live_model_ids(p, &keys).await {
                Ok(ids) => (
                    Some(
                        ids.into_iter()
                            .filter(|id| !p.models.contains_key(id))
                            .collect(),
                    ),
                    None,
                ),
                Err(e) => (None, Some(e)),
            }
        } else {
            (None, None)
        };
        providers.push(AvailableProvider {
            id: p.id.clone(),
            key_source: source.to_string(),
            in_use: in_use.contains(&p.id),
            models: p.models.keys().cloned().collect(),
            live_only,
            check_error,
        });
    }
    if args.json {
        return print_json(&Available { providers });
    }
    for p in &providers {
        println!(
            "{} (key: {}){}",
            p.id,
            p.key_source,
            if p.in_use { "" } else { "  (no role uses it)" }
        );
        for m in &p.models {
            println!("  {}/{m}", p.id);
        }
        if let Some(e) = &p.check_error {
            println!("  live catalog: {e}");
        }
        if let Some(live) = &p.live_only
            && !live.is_empty()
        {
            println!(
                "  also in the live catalog ({}; `models set <role> {}/<id>` adds one):",
                live.len(),
                p.id
            );
            for m in live {
                println!("    {}/{m}", p.id);
            }
        }
    }
    Ok(())
}
