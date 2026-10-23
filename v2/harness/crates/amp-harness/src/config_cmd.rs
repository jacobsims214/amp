//! `amp-harness [--config FILE] [--config-dir DIR] config show [--json] [--check]`: the providers
//! and the provider/model each role resolves to (M2-3, #2091), from the user config.toml and the
//! repo's `.amp/harness/config.toml` (#2232).
//!
//! Never prints a key: a provider's key is described by where it came from (`env OPENROUTER_API_KEY`,
//! `keychain`, `missing (...)`). `--check` asks each provider in use for its live model catalog and
//! marks roles whose model id it does not list. Exit code 1 when a role does not resolve or a
//! checked model is missing.
//!
//! `config check [--json] [--live]` (#2235) validates every layer (user file, repo file, defaults
//! as overridden) area by area through [`amp_core::config_check`], naming the file and key of each
//! problem and how to fix it. Exit code 1 when there is an error (warnings alone exit 0).

use std::collections::{BTreeMap, BTreeSet};

use amp_core::{
    assets::{Assets, OverrideDirs},
    config_check::{self, Layer, Problem, Severity},
    models::{ModelHints, ModelsConfig, Selection, thinking_name},
    providers::{KeyLookup, SystemKeys, live_model_ids, resolve_key},
    roles::RoleRegistry,
};
use clap::{Args, Subcommand};
use serde::Serialize;

use crate::conn::{CliError, print_json};

#[derive(Subcommand)]
pub enum ConfigCommand {
    /// Show the providers and the provider/model/thinking level each role resolves to.
    Show(ShowArgs),
    /// Check every layer (user config.toml, the repo's .amp/harness/config.toml) and report each
    /// problem with its file, key and a fix. Exit code 1 on errors.
    Check(CheckArgs),
}

#[derive(Args)]
pub struct CheckArgs {
    /// Print JSON.
    #[arg(long)]
    json: bool,
    /// Also check each role's model against its provider's live catalog (network; needs keys).
    #[arg(long)]
    live: bool,
}

#[derive(Args)]
pub struct ShowArgs {
    /// Print JSON.
    #[arg(long)]
    json: bool,
    /// Check each model id against the provider's live catalog (network; Anthropic needs its key).
    #[arg(long)]
    check: bool,
}

/// `config show --json`. Add fields, never rename or remove them.
#[derive(Debug, Serialize)]
pub struct Report {
    pub ok: bool,
    pub config_file: Option<String>,
    pub config_found: bool,
    /// The repo's `.amp/harness/config.toml`, when one was merged in (#2232).
    pub repo_file: Option<String>,
    /// `profile` or `inherit`.
    pub mode: &'static str,
    /// Profile name, or the inherited `provider/model`.
    pub selection: String,
    pub selection_source: String,
    pub profiles: Vec<ProfileReport>,
    pub providers: Vec<ProviderReport>,
    pub roles: Vec<RoleReport>,
}

#[derive(Debug, Serialize)]
pub struct ProfileReport {
    pub name: String,
    pub description: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct ProviderReport {
    pub id: String,
    pub api: &'static str,
    pub base_url: String,
    /// `env`, `keychain`, `not-needed` or `missing`.
    pub key: &'static str,
    /// Human description of the key's source (never the key).
    pub key_source: String,
    pub in_use: bool,
    pub timeouts_secs: Timeouts,
    pub models: Vec<String>,
    /// `--check`: why the live catalog could not be read.
    pub check_error: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct Timeouts {
    pub connect: Option<u64>,
    pub header: Option<u64>,
    pub stream_idle: Option<u64>,
    pub request: Option<u64>,
}

#[derive(Debug, Serialize)]
pub struct RoleReport {
    pub role: String,
    pub provider: Option<String>,
    pub model: Option<String>,
    /// The level that will be sent.
    pub thinking: Option<&'static str>,
    /// The level the config asked for, when it differs or was unset.
    pub requested_thinking: Option<&'static str>,
    pub source: Option<String>,
    /// How a role with no `[roles.<id>]` of its own got its model (#2246): `fallback
    /// worker-backend`, `[models] fallback`, `based_on <role>` or `agent file model`.
    pub fallback: Option<String>,
    /// The provider has a key (or needs none).
    pub ready: bool,
    /// `--check`: `true` listed, `false` missing, `null` not checked.
    pub live: Option<bool>,
    pub error: Option<String>,
}

pub async fn run(command: ConfigCommand) -> Result<(), CliError> {
    match command {
        ConfigCommand::Show(args) => show(args).await,
        ConfigCommand::Check(args) => check(args).await,
    }
}

/// Checks only the harness binary can run (they need crates amp-core doesn't depend on).
pub(crate) const HARNESS_CHECKERS: &[(&str, config_check::Checker)] = &[("tui", check_tui)];

/// `[tui]` as the chat reads it.
fn check_tui(inputs: &config_check::Inputs) -> Vec<Problem> {
    let (Some(text), Some(_)) = (&inputs.user_text, &inputs.user) else {
        return Vec::new();
    };
    let home = (inputs.env)("HOME");
    let env = move |k: &str| (k == "HOME").then(|| home.clone()).flatten();
    match amp_tui::ThemeSettings::resolve(None, &env, Some(text)) {
        Ok(_) => Vec::new(),
        Err(e) => vec![
            Problem::error("tui", Layer::User, e.0)
                .file(inputs.user_file())
                .key("tui"),
        ],
    }
}

async fn check(args: CheckArgs) -> Result<(), CliError> {
    let cwd =
        std::env::current_dir().map_err(|e| CliError::new(format!("current directory: {e}")))?;
    let paths = crate::conn::config_paths_in(&cwd);
    let env = |k: &str| std::env::var(k).ok();
    let keys = SystemKeys::from_env();
    let mut report = config_check::check(&paths, &env, &keys, HARNESS_CHECKERS);
    if args.live {
        let inputs = config_check::read_inputs(&paths, &env, &keys);
        report.extend("catalog", live_problems(&inputs, &keys).await);
    }
    if args.json {
        print_json(&report)?;
    } else {
        print_check(&report);
    }
    if report.ok {
        Ok(())
    } else {
        Err(CliError::silent(1))
    }
}

/// `--live`: roles whose model the provider's live catalog does not list.
async fn live_problems(inputs: &config_check::Inputs<'_>, keys: &dyn KeyLookup) -> Vec<Problem> {
    let Some(config) = &inputs.models else {
        return Vec::new();
    };
    let mut out = Vec::new();
    let mut live: BTreeMap<String, Result<BTreeSet<String>, String>> = BTreeMap::new();
    for (role, resolved) in config.resolve_all() {
        let Ok(r) = resolved else { continue };
        let Some(provider) = config.providers.get(&r.provider) else {
            continue;
        };
        if !live.contains_key(&r.provider) {
            let (key, source) = resolve_key(provider, keys);
            let ids = if provider.needs_key && key.is_none() {
                Err(format!("no key ({source})"))
            } else {
                live_model_ids(provider, keys).await
            };
            if let Err(e) = &ids {
                out.push(
                    Problem::warning(
                        "catalog",
                        Layer::Env,
                        format!("{}'s live catalog was not checked: {e}", r.provider),
                    )
                    .key(format!("providers.{}", r.provider)),
                );
            }
            live.insert(r.provider.clone(), ids);
        }
        if let Some(Ok(ids)) = live.get(&r.provider)
            && !ids.contains(&r.model_id)
        {
            let (layer, file) = inputs.role_layer(&role);
            out.push(
                Problem::error(
                    "catalog",
                    layer,
                    format!(
                        "role `{role}` uses {}, which {}'s live catalog does not list",
                        r.model_ref(),
                        r.provider
                    ),
                )
                .file(file.as_deref())
                .key(format!("roles.{role}.model"))
                .fix(format!(
                    "`amp-harness models list --available --check` lists the models; `amp-harness models set {role} <provider/model>`"
                )),
            );
        }
    }
    out
}

fn print_check(r: &config_check::CheckReport) {
    let file = |path: &Option<std::path::PathBuf>, found: bool| match path {
        Some(p) if found => p.display().to_string(),
        Some(p) => format!("{} (not found)", p.display()),
        None => "none".to_owned(),
    };
    println!("User config: {}", file(&r.user_file, r.user_file_found));
    println!("Repo config: {}", file(&r.repo_file, r.repo_file_found));
    println!("Checked:     {}", r.areas.join(", "));
    for p in &r.problems {
        let severity = match p.severity {
            Severity::Error => "ERROR",
            Severity::Warning => "warning",
        };
        let layer = serde_json::to_value(p.layer)
            .ok()
            .and_then(|v| v.as_str().map(str::to_owned))
            .unwrap_or_default();
        let mut head = format!("\n{severity} [{layer} {}]", p.area);
        if let Some(key) = &p.key {
            head.push_str(&format!(" {key}"));
        }
        if let Some(file) = &p.file {
            head.push_str(&format!(" in {}", file.display()));
        }
        println!("{head}");
        println!("  {}", p.message);
        if let Some(fix) = &p.fix {
            println!("  fix: {fix}");
        }
    }
    let (errors, warnings) = (r.errors(), r.warnings());
    if errors == 0 && warnings == 0 {
        println!("\nOK: no problems.");
    } else {
        println!("\n{errors} error(s), {warnings} warning(s).");
    }
}

async fn show(args: ShowArgs) -> Result<(), CliError> {
    let cwd =
        std::env::current_dir().map_err(|e| CliError::new(format!("current directory: {e}")))?;
    let paths = crate::conn::config_paths_in(&cwd);
    let config = ModelsConfig::load_paths(&|k| std::env::var(k).ok(), &paths)
        .map_err(|e| CliError::new(e.to_string()))?;
    let keys = SystemKeys::from_env();
    let report = build_report_with(&config, &keys, args.check, &agent_hints(&paths)).await;
    if args.json {
        print_json(&report)?;
    } else {
        print_human(&report);
    }
    if report.ok {
        Ok(())
    } else {
        Err(CliError::silent(1))
    }
}

/// Every agent's model role and fallback hints, from the agent files `paths` names (none when
/// they do not load: `agents validate` reports that).
pub fn agent_hints(paths: &amp_mcp::ConfigPaths) -> Vec<(String, ModelHints)> {
    let Ok(assets) = Assets::load(&OverrideDirs::from_paths(paths)) else {
        return Vec::new();
    };
    RoleRegistry::from_assets(&assets)
        .map(|roles| {
            roles
                .roles()
                .map(|r| (r.model_role.clone(), r.model_hints()))
                .collect()
        })
        .unwrap_or_default()
}

/// The configured roles only. Use [`build_report_with`] to add the agents with no model of
/// their own.
pub async fn build_report(config: &ModelsConfig, keys: &dyn KeyLookup, check: bool) -> Report {
    build_report_with(config, keys, check, &[]).await
}

/// The configured roles plus every agent in `agents` that falls back (#2246).
pub async fn build_report_with(
    config: &ModelsConfig,
    keys: &dyn KeyLookup,
    check: bool,
    agents: &[(String, ModelHints)],
) -> Report {
    let resolved = config.resolve_agents(agents);
    let in_use: BTreeSet<String> = resolved
        .iter()
        .filter_map(|(_, r)| r.as_ref().ok().map(|r| r.provider.clone()))
        .collect();
    let mut live: BTreeMap<String, Result<BTreeSet<String>, String>> = BTreeMap::new();
    if check {
        for id in &in_use {
            if let Some(provider) = config.providers.get(id) {
                live.insert(id.clone(), live_model_ids(provider, keys).await);
            }
        }
    }

    let providers: Vec<ProviderReport> = config
        .providers
        .values()
        .map(|p| {
            let (_, source) = resolve_key(p, keys);
            let secs = |d: Option<std::time::Duration>| d.map(|d| d.as_secs());
            ProviderReport {
                id: p.id.clone(),
                api: p.api.as_str(),
                base_url: p.base_url.clone(),
                key: source.kind(),
                key_source: source.to_string(),
                in_use: in_use.contains(&p.id),
                timeouts_secs: Timeouts {
                    connect: secs(p.timeouts.connect),
                    header: secs(p.timeouts.header),
                    stream_idle: secs(p.timeouts.stream_idle),
                    request: secs(p.timeouts.request),
                },
                models: p.models.keys().cloned().collect(),
                check_error: live.get(&p.id).and_then(|r| r.as_ref().err().cloned()),
            }
        })
        .collect();
    let ready: BTreeMap<&str, bool> = providers
        .iter()
        .map(|p| (p.id.as_str(), p.key != "missing"))
        .collect();

    let mut ok = true;
    let roles = resolved
        .into_iter()
        .map(|(role, resolved)| match resolved {
            Ok(r) => {
                let live = match live.get(&r.provider) {
                    Some(Ok(ids)) => Some(ids.contains(&r.model_id)),
                    _ => None,
                };
                if live == Some(false) {
                    ok = false;
                }
                let requested = r
                    .requested_thinking
                    .filter(|t| *t != r.thinking)
                    .map(thinking_name)
                    .or(r.requested_thinking.is_none().then_some("unset"));
                RoleReport {
                    ready: ready.get(r.provider.as_str()).copied().unwrap_or(false),
                    provider: Some(r.provider.clone()),
                    model: Some(r.model_id.clone()),
                    thinking: Some(thinking_name(r.thinking)),
                    requested_thinking: requested,
                    source: Some(r.source.clone()),
                    fallback: r.fell_back(),
                    live,
                    error: None,
                    role,
                }
            }
            Err(e) => {
                ok = false;
                RoleReport {
                    role,
                    provider: None,
                    model: None,
                    thinking: None,
                    requested_thinking: None,
                    source: None,
                    fallback: None,
                    ready: false,
                    live: None,
                    error: Some(e.to_string()),
                }
            }
        })
        .collect();

    let (mode, selection, selection_source) = match &config.selection {
        Selection::Profile { name, source } => ("profile", name.clone(), source.clone()),
        Selection::Inherit { model, source, .. } => ("inherit", model.clone(), source.clone()),
    };
    Report {
        ok,
        config_file: config.config_file.as_ref().map(|p| p.display().to_string()),
        config_found: config.config_found,
        repo_file: config.repo_file.as_ref().map(|p| p.display().to_string()),
        mode,
        selection,
        selection_source,
        profiles: config
            .profiles
            .iter()
            .map(|(name, description)| ProfileReport {
                name: name.clone(),
                description: description.clone(),
            })
            .collect(),
        providers,
        roles,
    }
}

fn print_human(r: &Report) {
    match (&r.config_file, r.config_found) {
        (Some(path), true) => println!("Config:    {path}"),
        (Some(path), false) => println!("Config:    {path} (not found; shipped defaults)"),
        (None, _) => println!("Config:    none (shipped defaults)"),
    }
    if let Some(repo) = &r.repo_file {
        println!("Repo:      {repo}");
    }
    let mode = if r.mode == "inherit" {
        format!("inherit {}", r.selection)
    } else {
        format!("profile {}", r.selection)
    };
    println!("Models:    {mode} ({})", r.selection_source);
    let others: Vec<&str> = r.profiles.iter().map(|p| p.name.as_str()).collect();
    println!(
        "Profiles:  opencode (default), {}, inherit",
        others.join(", ")
    );

    println!("\nProviders:");
    for p in &r.providers {
        let t = &p.timeouts_secs;
        let fmt = |v: Option<u64>| v.map(|s| format!("{s}s")).unwrap_or_else(|| "off".into());
        println!(
            "  {:<12} {:<19} {}{}",
            p.id,
            p.api,
            p.base_url,
            if p.in_use { "" } else { "  (no role uses it)" }
        );
        println!(
            "  {:<12} key: {}; timeouts: connect {}, header {}, stream idle {}, request {}",
            "",
            p.key_source,
            fmt(t.connect),
            fmt(t.header),
            fmt(t.stream_idle),
            fmt(t.request)
        );
        if let Some(e) = &p.check_error {
            println!("  {:<12} live check: {e}", "");
        }
    }

    println!("\nRoles:");
    print_roles(r);
}

/// The ROLE / PROVIDER/MODEL / THINKING / READY / SOURCE table (`config show`, `connect`).
pub fn print_roles(r: &Report) {
    let width = r
        .roles
        .iter()
        .filter_map(|x| {
            x.provider
                .as_ref()
                .zip(x.model.as_ref())
                .map(|(p, m)| p.len() + m.len() + 1)
        })
        .max()
        .unwrap_or(20);
    let ready_w = if r.roles.iter().any(|x| x.live.is_some()) {
        22
    } else {
        7
    };
    println!(
        "  {:<18} {:<width$}  {:<8} {:<ready_w$} SOURCE",
        "ROLE", "PROVIDER/MODEL", "THINKING", "READY"
    );
    for role in &r.roles {
        match (&role.provider, &role.model) {
            (Some(p), Some(m)) => {
                let mut thinking = role.thinking.unwrap_or("-").to_owned();
                if let Some(req) = role.requested_thinking
                    && req != "unset"
                {
                    thinking = format!("{thinking} (asked {req})");
                }
                let mut ready = if role.ready { "yes" } else { "no key" }.to_owned();
                match role.live {
                    Some(true) => ready.push_str(", live ok"),
                    Some(false) => ready.push_str(", NOT IN CATALOG"),
                    None => {}
                }
                println!(
                    "  {:<18} {:<width$}  {:<8} {:<ready_w$} {}",
                    role.role,
                    format!("{p}/{m}"),
                    thinking,
                    ready,
                    role.source.as_deref().unwrap_or("")
                );
            }
            _ => println!(
                "  {:<18} ERROR: {}",
                role.role,
                role.error.as_deref().unwrap_or("unresolved")
            ),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use amp_core::{
        models::{ModelsConfig, ModelsFile},
        providers::StaticKeys,
    };

    #[tokio::test]
    async fn report_shows_per_role_resolution_without_secrets() {
        let user = ModelsFile::parse(
            "[roles.reviewer]\nmodel = \"anthropic/claude-opus-5-5\"\n",
            "test",
        )
        .unwrap();
        let config =
            ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "config test", &|_| None)
                .unwrap();
        let keys = StaticKeys {
            env: BTreeMap::from([("ANTHROPIC_API_KEY".into(), "sk-ant-TOPSECRET".into())]),
            keychain: None,
        };
        let report = build_report(&config, &keys, false).await;
        assert!(report.ok);
        let json = serde_json::to_string(&report).unwrap();
        assert!(!json.contains("TOPSECRET"), "{json}");

        let reviewer = report.roles.iter().find(|r| r.role == "reviewer").unwrap();
        assert_eq!(reviewer.provider.as_deref(), Some("anthropic"));
        assert_eq!(reviewer.model.as_deref(), Some("claude-opus-5-5"));
        assert!(reviewer.ready);
        let manager = report.roles.iter().find(|r| r.role == "manager").unwrap();
        assert_eq!(manager.provider.as_deref(), Some("openrouter"));
        assert!(!manager.ready, "no OpenRouter key in this test");

        let anthropic = report
            .providers
            .iter()
            .find(|p| p.id == "anthropic")
            .unwrap();
        assert_eq!(anthropic.key, "env");
        assert_eq!(anthropic.key_source, "env ANTHROPIC_API_KEY");
        assert!(anthropic.in_use);
    }
}
