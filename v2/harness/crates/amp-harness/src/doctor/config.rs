//! doctor's configuration checks (#2246, H16):
//!
//! - `agents`: the roster loads (built-in, user and repo agent files), every agent resolves to a
//!   model through D11's fallback chain, and which agents the manager may dispatch. Agents that
//!   are not built in are listed with their model and how they got it.
//! - `skills`: every skill file loads; skills that are not built in are listed.
//! - `config-check`: the rest of `amp-harness config check` (files, server, workers, tools, `[tui]`):
//!   the areas doctor does not already cover with its own checks (roles, keys, models, MCP,
//!   agents and skills).
//!
//! Problems come from [`amp_core::config_check`], so doctor and `config check` never disagree.

use amp_core::{
    assets::{Assets, Origin, OverrideDirs},
    config_check::{self, CheckReport, Problem, Severity},
    models::{ModelHints, ModelsConfig},
    providers::SystemKeys,
    roles::RoleRegistry,
};
use serde::Serialize;

use super::{CheckStatus, Finding, providers::Named};
use crate::agents_cmd::Ctx;

/// Areas with a doctor check of their own (or covered by `roles`, `key:*`, `models:*`).
const OWN_CHECKS: &[&str] = &["models", "providers", "roles", "mcp", "agents", "skills"];

/// `doctor --json` `.agents`. Add fields, never rename or remove them.
#[derive(Debug, Default, Serialize)]
pub struct AgentsReport {
    pub count: usize,
    /// The agents the manager may dispatch (ids).
    pub dispatchable: Vec<String>,
    /// Agents from the user or repo layer (new ones and overrides of built-in ones).
    pub custom: Vec<AgentRow>,
    pub problems: Vec<Problem>,
}

#[derive(Debug, Serialize)]
pub struct AgentRow {
    pub id: String,
    /// user or repo.
    pub layer: &'static str,
    pub file: String,
    /// `provider/model` (`null` when it does not resolve).
    pub model: Option<String>,
    pub model_source: Option<String>,
    /// How it got its model when it has no `[roles.<id>]` (`fallback worker-backend`, ...).
    pub fallback: Option<String>,
    pub dispatchable: bool,
}

/// `doctor --json` `.skills`.
#[derive(Debug, Default, Serialize)]
pub struct SkillsReport {
    pub count: usize,
    /// Skills from the user or repo layer.
    pub custom: Vec<SkillRow>,
    pub problems: Vec<Problem>,
}

#[derive(Debug, Serialize)]
pub struct SkillRow {
    pub name: String,
    pub layer: &'static str,
    pub file: String,
}

/// What [`load`] read: the roster for the other checks (MCP role access, model fallbacks).
pub struct Loaded {
    pub roles: Option<RoleRegistry>,
    pub assets: Option<Assets>,
}

impl Loaded {
    /// Every agent's model role and fallback hints.
    pub fn hints(&self) -> Vec<(String, ModelHints)> {
        self.roles
            .as_ref()
            .map(|r| {
                r.roles()
                    .map(|role| (role.model_role.clone(), role.model_hints()))
                    .collect()
            })
            .unwrap_or_default()
    }
}

pub fn load(ctx: &Ctx) -> Loaded {
    let assets = Assets::load(&OverrideDirs::from_paths(&ctx.paths)).ok();
    let roles = assets
        .as_ref()
        .and_then(|a| RoleRegistry::from_assets(a).ok());
    Loaded { roles, assets }
}

/// `config check`'s report for this folder (the same checkers, plus `[tui]`).
pub fn config_report(ctx: &Ctx) -> CheckReport {
    let env = |k: &str| std::env::var(k).ok();
    config_check::check(
        &ctx.paths,
        &env,
        &SystemKeys::from_env(),
        crate::config_cmd::HARNESS_CHECKERS,
    )
}

fn layer_of(origin: &Origin) -> Option<(&'static str, String)> {
    match origin {
        Origin::Embedded => None,
        Origin::User(p) => Some(("user", p.display().to_string())),
        Origin::Repo(p) => Some(("repo", p.display().to_string())),
    }
}

/// A finding from an area's problems: errors fail, warnings warn, else `ok`.
fn from_problems(problems: &[Problem], ok: String, fix: &str) -> Finding {
    let errors: Vec<&Problem> = problems
        .iter()
        .filter(|p| p.severity == Severity::Error)
        .collect();
    let text = |ps: &[&Problem]| {
        ps.iter()
            .map(|p| match &p.file {
                Some(f) => format!("{} ({})", p.message, f.display()),
                None => p.message.clone(),
            })
            .collect::<Vec<_>>()
            .join("; ")
    };
    let first_fix = |ps: &[&Problem]| {
        ps.iter()
            .find_map(|p| p.fix.clone())
            .unwrap_or_else(|| fix.to_owned())
    };
    if !errors.is_empty() {
        return Finding::new(CheckStatus::Fail, text(&errors)).fix(first_fix(&errors));
    }
    let warnings: Vec<&Problem> = problems.iter().collect();
    if !warnings.is_empty() {
        return Finding::new(CheckStatus::Warn, format!("{ok}; {}", text(&warnings)))
            .fix(first_fix(&warnings));
    }
    Finding::new(CheckStatus::Ok, ok)
}

/// The `agents`, `skills` and `config-check` checks.
pub fn check(
    loaded: &Loaded,
    models: Option<&ModelsConfig>,
    report: &CheckReport,
) -> (AgentsReport, SkillsReport, Vec<Named>) {
    let area = |name: &str| -> Vec<Problem> {
        report
            .problems
            .iter()
            .filter(|p| p.area == name)
            .cloned()
            .collect()
    };
    let mut out = Vec::new();

    let mut skills = SkillsReport {
        problems: area("skills"),
        ..SkillsReport::default()
    };
    if let Some(assets) = &loaded.assets {
        skills.count = assets.skills().count();
        for s in assets.skills() {
            if let Some((layer, file)) = layer_of(s.origin()) {
                skills.custom.push(SkillRow {
                    name: s.name.clone(),
                    layer,
                    file,
                });
            }
        }
    }
    let custom: Vec<String> = skills
        .custom
        .iter()
        .map(|s| format!("{} ({})", s.name, s.layer))
        .collect();
    let ok = if custom.is_empty() {
        format!("{} skills, all built in", skills.count)
    } else {
        format!("{} skills; yours: {}", skills.count, custom.join(", "))
    };
    out.push((
        "skills".to_owned(),
        from_problems(
            &skills.problems,
            ok,
            "fix the skill file it names; `amp-harness skills validate` checks again",
        ),
    ));

    let mut agents = AgentsReport {
        problems: area("agents"),
        ..AgentsReport::default()
    };
    if let (Some(roles), Some(assets)) = (&loaded.roles, &loaded.assets) {
        let dispatchable: Vec<String> = roles
            .dispatchable(roles.manager())
            .iter()
            .map(|r| r.id.clone())
            .collect();
        agents.count = roles.roles().count();
        for role in roles.roles() {
            let Some((layer, file)) = assets.agent(&role.id).and_then(|a| layer_of(&a.origin))
            else {
                continue;
            };
            let resolved = models.map(|m| role.resolve_model(m));
            let ok = resolved.as_ref().and_then(|r| r.as_ref().ok());
            agents.custom.push(AgentRow {
                id: role.id.clone(),
                layer,
                file,
                model: ok.map(|r| r.model_ref()),
                model_source: ok.map(|r| r.source.clone()),
                fallback: ok.and_then(|r| r.fell_back()),
                dispatchable: dispatchable.contains(&role.id),
            });
        }
        agents.dispatchable = dispatchable;
    }
    let custom: Vec<String> = agents
        .custom
        .iter()
        .map(|a| {
            let model = match (&a.model, &a.fallback) {
                (Some(m), Some(f)) => format!(", {m} via {f}"),
                (Some(m), None) => format!(", {m}"),
                _ => String::new(),
            };
            format!(
                "{} ({}{model}{})",
                a.id,
                a.layer,
                if a.dispatchable { ", dispatchable" } else { "" }
            )
        })
        .collect();
    let mut ok = format!(
        "{} agents, {} the manager can dispatch",
        agents.count,
        agents.dispatchable.len()
    );
    if !custom.is_empty() {
        ok.push_str(&format!("; yours: {}", custom.join(", ")));
    }
    out.push((
        "agents".to_owned(),
        from_problems(
            &agents.problems,
            ok,
            "fix the agent file it names; `amp-harness agents validate` checks again",
        ),
    ));

    let rest: Vec<Problem> = report
        .problems
        .iter()
        .filter(|p| !OWN_CHECKS.contains(&p.area))
        .cloned()
        .collect();
    let mut files: Vec<String> = Vec::new();
    if let Some(f) = report.user_file.as_ref().filter(|_| report.user_file_found) {
        files.push(f.display().to_string());
    }
    if let Some(f) = report.repo_file.as_ref().filter(|_| report.repo_file_found) {
        files.push(f.display().to_string());
    }
    let ok = if files.is_empty() {
        "no config files (built-in defaults)".to_owned()
    } else {
        format!("no problems in {}", files.join(", "))
    };
    out.push((
        "config-check".to_owned(),
        from_problems(
            &rest,
            ok,
            "`amp-harness config check` lists each problem with its file, key and fix",
        ),
    ));
    (agents, skills, out)
}
