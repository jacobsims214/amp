//! `amp-harness agents ...` (#2234, decision 13 of decisions/amp-harness-configurability.md):
//! list, show, create, edit, remove and reset agents, and change the skills they preload, without
//! editing files by hand.
//!
//! Agents are markdown files with YAML frontmatter in three layers (lowest to highest): built-in
//! (embedded), user (`<config dir>/agents/`) and repo (`<repo>/.amp/harness/agents/`). Writers take
//! `--user` (the default) or `--repo` and only ever touch that layer's directory, and the layer's
//! config.toml for `[roles.<id>]` (`--model`). Every write is validated first with the same checks
//! the harness runs at start (`Assets` + `RoleRegistry` + the model the role resolves to), so a
//! broken file is never left behind.
//!
//! The helpers at the bottom (scopes, the `$EDITOR` loop, stashing for removals) are shared with
//! [`crate::skills_cmd`].

use std::{
    io::{BufRead, IsTerminal, Write as _},
    path::{Path, PathBuf},
    process::Command,
};

use amp_core::{
    assets::{AGENTS_DIR, Assets, Origin, OverrideDirs, valid_name},
    models::{ModelsConfig, ResolvedRole, normalize_role, parse_thinking, thinking_name},
    roles::{Role, RoleRegistry},
    templates::{self, set_frontmatter_key, yaml_list, yaml_string},
};
use amp_mcp::ConfigPaths;
use clap::{Args, Subcommand};
use serde::Serialize;
use serde_json::Value;

use crate::{
    conn::{self, CliError, print_json},
    setup::{Change, ConfigFile},
};

#[derive(Subcommand)]
pub enum AgentsCommand {
    /// List every agent: where it comes from, the model it runs on, and whether the manager can
    /// dispatch it.
    List(JsonArgs),
    /// Show one agent: its file, model (and which setting chose it), skills and tool rules.
    Show(ShowArgs),
    /// Create an agent from a template (worker, reviewer, researcher, blank) or a copy of an
    /// existing agent. It works at once: no [roles] entry is needed, and the manager can dispatch it.
    New(NewArgs),
    /// Edit an agent in $EDITOR (a built-in agent is copied to the layer first). Validated before
    /// it is saved; an invalid edit can be reopened.
    Edit(IdScopeArgs),
    /// Delete an agent file from the user or repo layer.
    Remove(IdScopeArgs),
    /// Drop the user/repo overrides of a built-in agent, back to the built-in one.
    Reset(ResetArgs),
    /// Check every agent loads, its skills exist and it resolves to a model.
    Validate(JsonArgs),
    /// Add or remove a skill an agent preloads (`agents skills <id> add <skill>`).
    Skills(AgentSkillsArgs),
}

#[derive(Args)]
pub struct JsonArgs {
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct ShowArgs {
    /// Agent id (`amp-worker-backend`, `worker-backend` or `amp:amp-worker-backend`).
    id: String,
    /// Print JSON.
    #[arg(long)]
    json: bool,
}

/// `--user` (default) or `--repo`: which layer a writer changes.
#[derive(Args, Clone, Copy, Default)]
pub struct ScopeArgs {
    /// Write to the user layer (<config dir>, default ~/.config/amp-harness). The default.
    #[arg(long, conflicts_with = "repo")]
    pub user: bool,
    /// Write to this repo's layer (<repo>/.amp/harness, next to .amp.json), shared with everyone
    /// who clones it.
    #[arg(long)]
    pub repo: bool,
}

impl ScopeArgs {
    pub fn scope(self) -> Scope {
        if self.repo { Scope::Repo } else { Scope::User }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Scope {
    User,
    Repo,
}

impl Scope {
    pub fn name(self) -> &'static str {
        match self {
            Scope::User => "user",
            Scope::Repo => "repo",
        }
    }

    pub fn origin(self, path: PathBuf) -> Origin {
        match self {
            Scope::User => Origin::User(path),
            Scope::Repo => Origin::Repo(path),
        }
    }

    fn other(self) -> Scope {
        match self {
            Scope::User => Scope::Repo,
            Scope::Repo => Scope::User,
        }
    }
}

#[derive(Args)]
pub struct NewArgs {
    /// The new agent's id: lowercase letters, digits and hyphens (`sql-expert`, `amp-sql-expert`).
    id: String,
    /// Template (worker, reviewer, researcher, blank) or an existing agent id to copy.
    #[arg(long, default_value = templates::DEFAULT_AGENT_TEMPLATE)]
    from: String,
    /// One line the manager reads to decide when to dispatch it.
    #[arg(long)]
    description: Option<String>,
    /// Run it on this provider/model (writes [roles.<id>] to the layer's config.toml). Without it
    /// the agent runs on the model of the role it is based on.
    #[arg(long, value_name = "PROVIDER/MODEL")]
    model: Option<String>,
    /// Thinking level with --model (off, minimal, low, medium, high, xhigh, max).
    #[arg(long, requires = "model")]
    thinking: Option<String>,
    /// Skills to preload, comma-separated (replaces the template's).
    #[arg(long, value_delimiter = ',')]
    skills: Option<Vec<String>>,
    #[command(flatten)]
    scope: ScopeArgs,
    /// Open the new file in $EDITOR.
    #[arg(long)]
    edit: bool,
    /// Print JSON (the new agent, as `agents show --json`).
    #[arg(long)]
    json: bool,
}

#[derive(Args)]
pub struct IdScopeArgs {
    /// Agent id.
    id: String,
    #[command(flatten)]
    scope: ScopeArgs,
}

#[derive(Args)]
pub struct ResetArgs {
    /// A built-in agent's id.
    id: String,
    /// Only drop the user layer's override.
    #[arg(long, conflicts_with = "repo")]
    user: bool,
    /// Only drop the repo layer's override.
    #[arg(long)]
    repo: bool,
}

#[derive(Args)]
pub struct AgentSkillsArgs {
    /// Agent id.
    id: String,
    #[command(subcommand)]
    action: AgentSkillsAction,
}

#[derive(Subcommand)]
pub enum AgentSkillsAction {
    /// Preload this skill too.
    Add(SkillNameArgs),
    /// Stop preloading this skill.
    Remove(SkillNameArgs),
}

#[derive(Args)]
pub struct SkillNameArgs {
    /// Skill name.
    skill: String,
    #[command(flatten)]
    scope: ScopeArgs,
}

pub async fn run(command: AgentsCommand) -> Result<(), CliError> {
    let ctx = Ctx::here()?;
    match command {
        AgentsCommand::List(args) => list(&ctx, args.json),
        AgentsCommand::Show(args) => show(&ctx, &args.id, args.json),
        AgentsCommand::New(args) => new(&ctx, args),
        AgentsCommand::Edit(args) => edit(&ctx, &args.id, args.scope.scope()),
        AgentsCommand::Remove(args) => remove(&ctx, &args.id, args.scope.scope()),
        AgentsCommand::Reset(args) => reset(&ctx, &args.id, args.user, args.repo),
        AgentsCommand::Validate(args) => validate(&ctx, args.json),
        AgentsCommand::Skills(args) => {
            let (skill, scope, add) = match args.action {
                AgentSkillsAction::Add(a) => (a.skill, a.scope, true),
                AgentSkillsAction::Remove(a) => (a.skill, a.scope, false),
            };
            agent_skills(&ctx, &args.id, &skill, scope.scope(), add)
        }
    }
}

// ------------------------------------------------------------------------------------------------
// Reading.
// ------------------------------------------------------------------------------------------------

/// `agents list --json` / `agents show --json` (one agent). Add fields, never rename or remove.
#[derive(Debug, Serialize)]
pub struct AgentReport {
    pub id: String,
    pub description: String,
    /// `primary` (the manager) or `subagent`.
    pub mode: String,
    /// `built-in`, `user` or `repo`: the layer whose file is used.
    pub origin: &'static str,
    /// That file (`None` for built-in).
    pub path: Option<String>,
    /// Lower layers this file overrides (`built-in`, `user`).
    pub overrides: Vec<&'static str>,
    /// `provider/model`, when it resolves.
    pub model: Option<String>,
    pub thinking: Option<&'static str>,
    /// Which setting chose the model (`config <path>`, `based_on worker-backend (...)`, ...).
    pub model_source: Option<String>,
    pub model_error: Option<String>,
    pub skills: Vec<String>,
    /// The manager may dispatch it (`amp_run_ticket`).
    pub dispatchable: bool,
    /// Its frontmatter says `dispatch: true`.
    pub dispatch: bool,
    pub based_on: Option<String>,
}

#[derive(Debug, Serialize)]
struct ListReport {
    user_dir: Option<String>,
    repo_dir: Option<String>,
    agents: Vec<AgentReport>,
}

fn origin_kind(origin: &Origin) -> (&'static str, Option<String>) {
    match origin {
        Origin::Embedded => ("built-in", None),
        Origin::User(p) => ("user", Some(p.display().to_string())),
        Origin::Repo(p) => ("repo", Some(p.display().to_string())),
    }
}

fn agent_report(ctx: &Ctx, loaded: &Loaded, role: &Role) -> AgentReport {
    agent_report_from(ctx, &loaded.assets, &loaded.roles, &loaded.models, role)
}

/// One agent's report from what is loaded (the CLI's files, or a running chat's catalog, #2245).
pub(crate) fn agent_report_from(
    ctx: &Ctx,
    assets: &Assets,
    roles: &RoleRegistry,
    models: &ModelsConfig,
    role: &Role,
) -> AgentReport {
    let asset = assets.agent(&role.id).expect("roles come from assets");
    let (origin, path) = origin_kind(&asset.origin);
    let mut overrides = Vec::new();
    if Assets::embedded_agent_text(&role.id).is_some() && origin != "built-in" {
        overrides.push("built-in");
    }
    if origin == "repo"
        && ctx
            .agent_file(Scope::User, &role.id)
            .is_some_and(|p| p.is_file())
    {
        overrides.push("user");
    }
    let resolved = role.resolve_model(models);
    let manager = roles.manager();
    let dispatchable = roles.dispatchable(manager).iter().any(|r| r.id == role.id);
    AgentReport {
        id: role.id.clone(),
        description: role.description.clone(),
        mode: asset.mode().to_owned(),
        origin,
        path,
        overrides,
        model: resolved.as_ref().ok().map(ResolvedRole::model_ref),
        thinking: resolved.as_ref().ok().map(|r| thinking_name(r.thinking)),
        model_source: resolved.as_ref().ok().map(|r| r.source.clone()),
        model_error: resolved.as_ref().err().map(|e| e.to_string()),
        skills: role.skills.clone(),
        dispatchable,
        dispatch: role.dispatch,
        based_on: role.based_on.clone(),
    }
}

fn list(ctx: &Ctx, json: bool) -> Result<(), CliError> {
    let loaded = ctx.load()?;
    let agents: Vec<AgentReport> = loaded
        .roles
        .roles()
        .map(|r| agent_report(ctx, &loaded, r))
        .collect();
    if json {
        return print_json(&ListReport {
            user_dir: ctx.paths.user_dir.as_ref().map(|p| p.display().to_string()),
            repo_dir: ctx.paths.repo_dir.as_ref().map(|p| p.display().to_string()),
            agents,
        });
    }
    let rows: Vec<Vec<String>> = agents
        .iter()
        .map(|a| {
            vec![
                a.id.clone(),
                a.mode.clone(),
                a.origin.to_owned(),
                a.model.clone().unwrap_or_else(|| "(none)".into()),
                // #2336: `config config.toml`, as /model roles shows it; the full paths are in
                // --json and under the table.
                a.model_source.as_deref().map_or_else(
                    || a.model_error.clone().unwrap_or_default(),
                    crate::chat_config::short_source,
                ),
                if a.mode == "primary" {
                    "-".into()
                } else if a.dispatchable {
                    "yes".into()
                } else {
                    "no".into()
                },
                a.skills.join(", "),
            ]
        })
        .collect();
    print_table(
        &[
            "AGENT",
            "MODE",
            "FROM",
            "MODEL",
            "MODEL FROM",
            "DISPATCH",
            "SKILLS",
        ],
        &rows,
    );
    println!();
    ctx.print_layers(AGENTS_DIR);
    Ok(())
}

fn show(ctx: &Ctx, id: &str, json: bool) -> Result<(), CliError> {
    let loaded = ctx.load()?;
    let role = loaded.role(id)?;
    let report = agent_report(ctx, &loaded, role);
    if json {
        return print_json(&report);
    }
    let asset = loaded
        .assets
        .agent(&role.id)
        .expect("roles come from assets");
    println!("{}  ({})", report.id, report.origin);
    println!(
        "  file:        {}",
        report
            .path
            .clone()
            .unwrap_or_else(|| format!("built-in {AGENTS_DIR}/{}.md", report.id))
    );
    if !report.overrides.is_empty() {
        println!("  overrides:   {}", report.overrides.join(", "));
    }
    println!("  description: {}", one_line(&report.description));
    println!("  mode:        {}", report.mode);
    match (&report.model, &report.model_error) {
        (Some(model), _) => println!(
            "  model:       {model} (thinking {}), from {}",
            report.thinking.unwrap_or("off"),
            report.model_source.as_deref().unwrap_or("?")
        ),
        (None, Some(e)) => println!("  model:       none: {e}"),
        _ => {}
    }
    if let Some(base) = &report.based_on {
        println!("  based_on:    {base}");
    }
    println!(
        "  skills:      {}",
        if report.skills.is_empty() {
            "(none)".into()
        } else {
            report.skills.join(", ")
        }
    );
    if report.mode != "primary" {
        let why = if !report.dispatchable {
            "no (the manager's task rules deny it; add `dispatch: true`)".to_owned()
        } else if report.dispatch {
            "yes (dispatch: true)".to_owned()
        } else {
            "yes (the manager's task rules allow it)".to_owned()
        };
        println!("  dispatch:    {why}");
    }
    println!("  permissions:");
    match asset.frontmatter.get("permission") {
        Some(Value::Object(map)) if !map.is_empty() => {
            for (perm, rule) in map {
                let text = match rule {
                    Value::String(a) => a.clone(),
                    Value::Object(patterns) => patterns
                        .iter()
                        .map(|(p, a)| format!("{p:?} {}", a.as_str().unwrap_or("?")))
                        .collect::<Vec<_>>()
                        .join(", "),
                    other => other.to_string(),
                };
                println!("    {perm}: {text}");
            }
        }
        Some(Value::String(a)) => println!("    everything: {a}"),
        _ => println!("    (none: every tool allowed)"),
    }
    Ok(())
}

#[derive(Debug, Serialize)]
struct ValidateReport {
    ok: bool,
    agents: usize,
    problems: Vec<String>,
    warnings: Vec<String>,
}

fn validate(ctx: &Ctx, json: bool) -> Result<(), CliError> {
    let mut problems = Vec::new();
    let mut warnings = Vec::new();
    let mut count = 0;
    match ctx.load() {
        Err(e) => problems.push(e.message),
        Ok(loaded) => {
            for role in loaded.roles.roles() {
                count += 1;
                if let Err(e) = role.resolve_model(&loaded.models) {
                    problems.push(format!("agent {}: {e}", role.id));
                }
                if let Some(base) = &role.based_on
                    && loaded.roles.get(base).is_none()
                    && !loaded.models.has_role(base)
                {
                    warnings.push(format!(
                        "agent {}: based_on {base:?} is not an agent or a configured role",
                        role.id
                    ));
                }
                if role.dispatch && role.primary {
                    warnings.push(format!(
                        "agent {}: dispatch: true has no effect on a primary agent",
                        role.id
                    ));
                }
            }
        }
    }
    let ok = problems.is_empty();
    if json {
        print_json(&ValidateReport {
            ok,
            agents: count,
            problems,
            warnings,
        })?;
    } else {
        for w in &warnings {
            println!("warning: {w}");
        }
        for p in &problems {
            println!("error: {p}");
        }
        if ok {
            println!("{count} agents OK");
        }
    }
    if ok { Ok(()) } else { Err(CliError::silent(1)) }
}

// ------------------------------------------------------------------------------------------------
// Writing.
// ------------------------------------------------------------------------------------------------

fn new(ctx: &Ctx, args: NewArgs) -> Result<(), CliError> {
    let id = args.id.trim().to_owned();
    if !valid_name(&id) {
        return Err(CliError::usage(format!(
            "agent id {id:?} must be lowercase letters, digits and single hyphens"
        )));
    }
    let scope = args.scope.scope();
    let mut loaded = ctx.load()?;
    if let Some(existing) = loaded.assets.agent(&id) {
        return Err(CliError::usage(format!(
            "agent {id} already exists ({}); change it with `amp-harness agents edit {id}`",
            existing.origin
        )));
    }
    let model_role = normalize_role(&id).to_owned();
    if loaded.roles.get(&model_role).is_some_and(|r| r.id != id) {
        return Err(CliError::usage(format!(
            "agent id {id} would share the model settings [roles.{model_role}] of agent amp-{model_role}; pick another id"
        )));
    }
    let description = args
        .description
        .clone()
        .map(|d| one_line(&d))
        .filter(|d| !d.is_empty());

    let mut text = if let Some(rendered) = templates::render_agent(
        &args.from,
        &id,
        description.as_deref().unwrap_or(&format!(
            "{id}: describe when the manager should dispatch this agent."
        )),
    ) {
        rendered
    } else {
        // A copy of an existing agent: its prompt and rules, as a subagent that shares its model.
        let base = loaded.role(&args.from).map_err(|_| {
            CliError::usage(format!(
                "--from {}: not a template ({}) or an agent id",
                args.from,
                templates::agent_template_names().join(", ")
            ))
        })?;
        let base_id = base.id.clone();
        let source = agent_text(&loaded.assets, &base_id)?;
        let description = description
            .clone()
            .unwrap_or_else(|| format!("{} (copy of {base_id})", one_line(&base.description)));
        let mut text = source;
        for (key, value) in [
            ("description", Some(yaml_string(&description))),
            ("mode", Some("subagent".to_owned())),
            ("dispatch", Some("true".to_owned())),
            ("based_on", Some(yaml_string(&base_id))),
            // The copy runs on the base role's model (or its own [roles]), not on opencode's id.
            ("model", None),
            ("variant", None),
        ] {
            text = set_frontmatter_key(&text, key, value.as_deref()).map_err(CliError::new)?;
        }
        // The base's preloads, written out: a new id's defaults would differ.
        set_frontmatter_key(&text, "skills", Some(&yaml_list(&base.skills)))
            .map_err(CliError::new)?
    };
    if let Some(skills) = &args.skills {
        let skills: Vec<String> = skills
            .iter()
            .map(|s| s.trim().to_owned())
            .filter(|s| !s.is_empty())
            .collect();
        for s in &skills {
            if loaded.assets.skill(s).is_none() {
                return Err(CliError::usage(format!(
                    "--skills: no skill {s:?} (`amp-harness skills list` shows them)"
                )));
            }
        }
        text = set_frontmatter_key(&text, "skills", Some(&yaml_list(&skills)))
            .map_err(CliError::new)?;
    }
    if let Some(model) = &args.model {
        if !loaded.models.knows_model(model) {
            return Err(CliError::usage(format!(
                "--model {model}: not a configured provider/model (`amp-harness config show` lists them)"
            )));
        }
        if let Some(t) = &args.thinking {
            parse_thinking(t).map_err(|e| CliError::usage(format!("--thinking: {e}")))?;
        }
    }

    let dir = ctx.scope_dir(scope)?;
    let path = dir.join(AGENTS_DIR).join(format!("{id}.md"));
    if path.exists() {
        return Err(CliError::usage(format!(
            "{} already exists; nothing was written",
            path.display()
        )));
    }
    // Validate before writing: the file parses, the roster builds, and (without --model) the
    // agent resolves to a model through the fallback chain.
    loaded
        .assets
        .put_agent(&id, &text, scope.origin(path.clone()))
        .map_err(|e| CliError::new(e.to_string()))?;
    loaded.roles =
        RoleRegistry::from_assets(&loaded.assets).map_err(|e| CliError::new(e.to_string()))?;
    if args.model.is_none() {
        let role = loaded.role(&id)?;
        role.resolve_model(&loaded.models)
            .map_err(|e| CliError::new(format!("{e}; pass --model provider/model")))?;
    }

    write_file(&path, text.as_bytes())?;
    if let Some(model) = &args.model {
        let file = ctx.scope_config_file(scope)?;
        if scope == Scope::Repo && !file.exists() {
            write_file(&file, REPO_CONFIG_HEADER.as_bytes())?;
        }
        let section = format!("roles.{model_role}");
        let mut changes = vec![Change::set(&section, "model", model.clone())];
        if let Some(t) = &args.thinking {
            changes.push(Change::set(
                &section,
                "thinking",
                t.trim().to_ascii_lowercase(),
            ));
        }
        if let Err(e) = ConfigFile::new(file.clone()).edit(&changes) {
            let _ = std::fs::remove_file(&path);
            return Err(CliError::new(format!("{e}; the agent was not created")));
        }
    }
    if args.edit {
        edit_file(ctx, &path, &id, scope)?;
    }

    let loaded = ctx.load()?;
    let role = loaded.role(&id)?;
    let report = agent_report(ctx, &loaded, role);
    if args.json {
        return print_json(&report);
    }
    println!("Created agent {id} ({}): {}", scope.name(), path.display());
    match (&report.model, &report.model_error) {
        (Some(m), _) => println!(
            "  model:    {m}, from {}",
            report.model_source.as_deref().unwrap_or("?")
        ),
        (None, Some(e)) => println!("  model:    none: {e}"),
        _ => {}
    }
    println!(
        "  dispatch: {}",
        if report.dispatchable {
            "the manager can run it (amp_run_ticket agent_id amp:".to_owned() + &id + ")"
        } else {
            "no: the manager's task rules deny it".to_owned()
        }
    );
    if args.model.is_some() {
        println!(
            "  [roles.{model_role}] written to {}",
            ctx.scope_config_file(scope)?.display()
        );
    }
    println!("Edit it with `amp-harness agents edit {id}`.");
    Ok(())
}

fn edit(ctx: &Ctx, id: &str, scope: Scope) -> Result<(), CliError> {
    let loaded = ctx.load()?;
    let role = loaded.role(id)?;
    let id = role.id.clone();
    let path = ctx
        .scope_dir(scope)?
        .join(AGENTS_DIR)
        .join(format!("{id}.md"));
    edit_file(ctx, &path, &id, scope)
}

/// `$EDITOR` on `path` (seeded from the agent in effect when the layer has no file yet), validated
/// before it is written.
fn edit_file(ctx: &Ctx, path: &Path, id: &str, scope: Scope) -> Result<(), CliError> {
    let loaded = ctx.load()?;
    let initial = if path.is_file() {
        read_text(path)?
    } else {
        agent_text(&loaded.assets, id)?
    };
    let validate = |text: &str| -> Result<(), String> {
        let mut assets = loaded.assets.clone();
        assets
            .put_agent(id, text, scope.origin(path.to_path_buf()))
            .map_err(|e| e.to_string())?;
        let roles = RoleRegistry::from_assets(&assets).map_err(|e| e.to_string())?;
        let role = roles.get(id).ok_or("the agent disappeared")?;
        role.resolve_model(&loaded.models)
            .map_err(|e| e.to_string())?;
        Ok(())
    };
    let scratch = ctx
        .scope_dir(scope)?
        .join(EDIT_DIR)
        .join(format!("{id}.md"));
    match edit_loop(&editor_command(), &scratch, &initial, &validate, &ask_again)? {
        None => println!("No changes to {id}."),
        Some(text) => {
            write_file(path, text.as_bytes())?;
            println!("Saved {id}: {}", path.display());
            if scope == Scope::User
                && let Some(repo) = ctx.agent_file(Scope::Repo, id)
                && repo.is_file()
            {
                println!("note: {} overrides this file in this repo", repo.display());
            }
        }
    }
    Ok(())
}

fn remove(ctx: &Ctx, id: &str, scope: Scope) -> Result<(), CliError> {
    let id = ctx.resolve_id(id)?;
    let Some(path) = ctx.agent_file(scope, &id).filter(|p| p.is_file()) else {
        let other = ctx.agent_file(scope.other(), &id).filter(|p| p.is_file());
        return Err(CliError::usage(match other {
            Some(p) => format!(
                "agent {id} is not in the {} layer; it is in the {} layer ({}): add --{}",
                scope.name(),
                scope.other().name(),
                p.display(),
                scope.other().name()
            ),
            None if Assets::embedded_agent_text(&id).is_some() => format!(
                "agent {id} is built in and can't be removed; `amp-harness agents reset {id}` drops overrides"
            ),
            None => format!("no agent {id}"),
        }));
    };
    stash_and_check(ctx, &path, &|| ctx.load().map(drop).map_err(|e| e.message))?;
    println!("Removed agent {id} ({}): {}", scope.name(), path.display());
    // A custom agent that is now gone everywhere: drop its [roles] in this layer too.
    let loaded = ctx.load()?;
    if loaded.assets.agent(&id).is_none() {
        let role = normalize_role(&id).to_owned();
        if let Ok(file) = ctx.scope_config_file(scope) {
            let cfg = ConfigFile::new(file.clone());
            if cfg.text().ok().flatten().is_some_and(|t| {
                toml::from_str::<toml::Table>(&t)
                    .is_ok_and(|doc| doc.get("roles").and_then(|r| r.get(&role)).is_some())
            }) {
                cfg.edit(&[Change::DropSection(format!("roles.{role}"))])
                    .map_err(CliError::new)?;
                println!("  and [roles.{role}] from {}", file.display());
            }
        }
    } else if let Some(a) = loaded.assets.agent(&id) {
        println!("  {id} is now the {} one", origin_kind(&a.origin).0);
    }
    Ok(())
}

fn reset(ctx: &Ctx, id: &str, user: bool, repo: bool) -> Result<(), CliError> {
    let id = ctx.resolve_id(id)?;
    if Assets::embedded_agent_text(&id).is_none() {
        return Err(CliError::usage(format!(
            "agent {id} is not built in; `amp-harness agents remove {id}` deletes it"
        )));
    }
    let scopes: Vec<Scope> = match (user, repo) {
        (true, _) => vec![Scope::User],
        (_, true) => vec![Scope::Repo],
        _ => vec![Scope::User, Scope::Repo],
    };
    let mut removed = Vec::new();
    for scope in scopes {
        if let Some(path) = ctx.agent_file(scope, &id).filter(|p| p.is_file()) {
            stash_and_check(ctx, &path, &|| ctx.load().map(drop).map_err(|e| e.message))?;
            removed.push(format!("{} ({})", scope.name(), path.display()));
        }
    }
    if removed.is_empty() {
        println!("{id} has no overrides; it is the built-in one.");
    } else {
        println!("Reset {id}: removed {}", removed.join(", "));
        let loaded = ctx.load()?;
        if let Some(a) = loaded.assets.agent(&id) {
            println!("  {id} is now the {} one", origin_kind(&a.origin).0);
        }
    }
    Ok(())
}

fn agent_skills(ctx: &Ctx, id: &str, skill: &str, scope: Scope, add: bool) -> Result<(), CliError> {
    let mut loaded = ctx.load()?;
    let role = loaded.role(id)?.clone();
    let mut skills = role.skills.clone();
    if add {
        if loaded.assets.skill(skill).is_none() {
            return Err(CliError::usage(format!(
                "no skill {skill:?} (`amp-harness skills list` shows them)"
            )));
        }
        if skills.iter().any(|s| s == skill) {
            println!("{} already preloads {skill}.", role.id);
            return Ok(());
        }
        skills.push(skill.to_owned());
    } else {
        if !skills.iter().any(|s| s == skill) {
            return Err(CliError::usage(format!(
                "{} does not preload {skill:?} (it preloads: {})",
                role.id,
                if skills.is_empty() {
                    "nothing".to_owned()
                } else {
                    skills.join(", ")
                }
            )));
        }
        skills.retain(|s| s != skill);
    }
    let path = ctx
        .scope_dir(scope)?
        .join(AGENTS_DIR)
        .join(format!("{}.md", role.id));
    let current = if path.is_file() {
        read_text(&path)?
    } else {
        agent_text(&loaded.assets, &role.id)?
    };
    let text = set_frontmatter_key(&current, "skills", Some(&yaml_list(&skills)))
        .map_err(CliError::new)?;
    loaded
        .assets
        .put_agent(&role.id, &text, scope.origin(path.clone()))
        .map_err(|e| CliError::new(e.to_string()))?;
    RoleRegistry::from_assets(&loaded.assets).map_err(|e| CliError::new(e.to_string()))?;
    write_file(&path, text.as_bytes())?;
    println!(
        "{} now preloads: {} ({})",
        role.id,
        if skills.is_empty() {
            "nothing".to_owned()
        } else {
            skills.join(", ")
        },
        path.display()
    );
    Ok(())
}

/// The file text of agent `id` as loaded (built-in or the override in effect).
fn agent_text(assets: &Assets, id: &str) -> Result<String, CliError> {
    match assets.agent(id).map(|a| &a.origin) {
        Some(Origin::User(p) | Origin::Repo(p)) => read_text(p),
        Some(Origin::Embedded) => Assets::embedded_agent_text(id)
            .map(str::to_owned)
            .ok_or_else(|| CliError::new(format!("built-in agent {id} has no text"))),
        None => Err(CliError::usage(format!("no agent {id}"))),
    }
}

// ------------------------------------------------------------------------------------------------
// Shared with skills_cmd.
// ------------------------------------------------------------------------------------------------

/// Scratch files for `$EDITOR` and stashed removals live in `<layer dir>/.edit/`, outside
/// `agents/` and `skills/`, so a harness starting meanwhile never loads them.
pub const EDIT_DIR: &str = ".edit";

/// The first lines of a repo config.toml this command creates.
const REPO_CONFIG_HEADER: &str = "# amp-harness settings for this repo (shared with everyone who clones it). Only [roles.*],\n\
# [mcp.*] and [models] profile are allowed here; everything else goes in your user config.toml.\n";

/// The config paths for this folder (user layer plus the repo layer of the `.amp.json` above the
/// current directory) and the process environment.
pub struct Ctx {
    pub paths: ConfigPaths,
    pub cwd: PathBuf,
}

/// Assets, roles and models as the harness would load them here.
pub struct Loaded {
    pub assets: Assets,
    pub roles: RoleRegistry,
    pub models: ModelsConfig,
}

impl Loaded {
    pub fn role(&self, id: &str) -> Result<&Role, CliError> {
        self.roles.get(id).ok_or_else(|| {
            CliError::usage(format!(
                "no agent {id} (`amp-harness agents list` shows them)"
            ))
        })
    }
}

impl Ctx {
    pub fn here() -> Result<Self, CliError> {
        let cwd = std::env::current_dir()
            .map_err(|e| CliError::new(format!("current directory: {e}")))?;
        Ok(Self {
            paths: conn::config_paths_in(&cwd),
            cwd,
        })
    }

    pub fn load_assets(&self) -> Result<Assets, CliError> {
        Assets::load(&OverrideDirs::from_paths(&self.paths))
            .map_err(|e| CliError::new(e.to_string()))
    }

    pub fn load(&self) -> Result<Loaded, CliError> {
        let assets = self.load_assets()?;
        let roles = RoleRegistry::from_assets(&assets).map_err(|e| CliError::new(e.to_string()))?;
        let models = ModelsConfig::load_paths(&|k| std::env::var(k).ok(), &self.paths)
            .map_err(|e| CliError::new(e.to_string()))?;
        Ok(Loaded {
            assets,
            roles,
            models,
        })
    }

    /// The layer's directory (`<config dir>` or `<repo>/.amp/harness`).
    pub fn scope_dir(&self, scope: Scope) -> Result<PathBuf, CliError> {
        match scope {
            Scope::User => self.paths.user_dir.clone().ok_or_else(|| {
                CliError::new(
                    "no user config directory: set HOME, XDG_CONFIG_HOME or --config-dir",
                )
            }),
            Scope::Repo => self.paths.repo_dir.clone().ok_or_else(|| {
                CliError::usage(format!(
                    "--repo: {} is not in an AMP project (no .amp.json above it); run `amp-harness init` there first",
                    self.cwd.display()
                ))
            }),
        }
    }

    /// The layer's config.toml (`[roles.<id>]` goes here).
    pub fn scope_config_file(&self, scope: Scope) -> Result<PathBuf, CliError> {
        match scope {
            Scope::User => self.paths.user_file_for_writing().map_err(CliError::new),
            Scope::Repo => {
                self.scope_dir(Scope::Repo)?;
                self.paths
                    .repo_file()
                    .ok_or_else(|| CliError::new("no repo config file"))
            }
        }
    }

    /// `<layer>/agents/<id>.md` (whether or not it exists).
    pub fn agent_file(&self, scope: Scope, id: &str) -> Option<PathBuf> {
        self.scope_dir(scope)
            .ok()
            .map(|d| d.join(AGENTS_DIR).join(format!("{id}.md")))
    }

    /// The canonical id for `name` when it is loaded, else `name` itself (so a file that no
    /// longer loads can still be removed).
    fn resolve_id(&self, name: &str) -> Result<String, CliError> {
        let id = amp_core::policy::canonical_agent_id(name);
        if let Ok(assets) = self.load_assets()
            && let Ok(roles) = RoleRegistry::from_assets(&assets)
            && let Some(role) = roles.get(&id)
        {
            return Ok(role.id.clone());
        }
        Ok(id)
    }

    pub fn print_layers(&self, sub: &str) {
        let show = |p: &Option<PathBuf>| match p {
            Some(p) => p.join(sub).display().to_string(),
            None => "(none)".to_owned(),
        };
        println!("user layer: {}", show(&self.paths.user_dir));
        println!(
            "repo layer: {}",
            match &self.paths.repo_dir {
                Some(_) => show(&self.paths.repo_dir),
                None => "(not in an AMP project)".to_owned(),
            }
        );
    }
}

pub fn read_text(path: &Path) -> Result<String, CliError> {
    std::fs::read_to_string(path)
        .map_err(|e| CliError::new(format!("read {}: {e}", path.display())))
}

/// Writes `bytes` to `path` through a temp file in the same directory (created when missing).
pub fn write_file(path: &Path, bytes: &[u8]) -> Result<(), CliError> {
    let dir = path
        .parent()
        .ok_or_else(|| CliError::new(format!("{}: no parent directory", path.display())))?;
    std::fs::create_dir_all(dir)
        .map_err(|e| CliError::new(format!("create {}: {e}", dir.display())))?;
    let tmp = dir.join(format!(
        ".{}.tmp",
        path.file_name()
            .map(|n| n.to_string_lossy())
            .unwrap_or_default()
    ));
    std::fs::write(&tmp, bytes)
        .and_then(|()| std::fs::rename(&tmp, path))
        .map_err(|e| {
            let _ = std::fs::remove_file(&tmp);
            CliError::new(format!("write {}: {e}", path.display()))
        })
}

/// Moves `path` (a file or directory) aside into `<layer>/.edit/`, runs `check` (which reloads),
/// and deletes it when the check passes or moves it back (and fails) when it doesn't.
pub fn stash_and_check(
    ctx: &Ctx,
    path: &Path,
    check: &dyn Fn() -> Result<(), String>,
) -> Result<(), CliError> {
    let layer = [Scope::User, Scope::Repo]
        .into_iter()
        .filter_map(|s| ctx.scope_dir(s).ok())
        .find(|d| path.starts_with(d))
        .ok_or_else(|| CliError::new(format!("{} is in no layer", path.display())))?;
    let stash_dir = layer.join(EDIT_DIR).join("removed");
    std::fs::create_dir_all(&stash_dir)
        .map_err(|e| CliError::new(format!("create {}: {e}", stash_dir.display())))?;
    let stash = stash_dir.join(format!(
        "{}-{}",
        std::process::id(),
        path.file_name()
            .map(|n| n.to_string_lossy())
            .unwrap_or_default()
    ));
    let _ = remove_path(&stash);
    std::fs::rename(path, &stash)
        .map_err(|e| CliError::new(format!("move {}: {e}", path.display())))?;
    match check() {
        Ok(()) => {
            remove_path(&stash)
                .map_err(|e| CliError::new(format!("delete {}: {e}", stash.display())))?;
            let _ = std::fs::remove_dir(&stash_dir);
            let _ = std::fs::remove_dir(layer.join(EDIT_DIR));
            Ok(())
        }
        Err(e) => {
            std::fs::rename(&stash, path)
                .map_err(|re| CliError::new(format!("restore {}: {re}", path.display())))?;
            Err(CliError::new(format!(
                "removing {} would break the harness ({e}); nothing was removed",
                path.display()
            )))
        }
    }
}

fn remove_path(path: &Path) -> std::io::Result<()> {
    if path.is_dir() {
        std::fs::remove_dir_all(path)
    } else {
        std::fs::remove_file(path)
    }
}

/// Asks on the terminal whether to reopen the editor after an invalid edit. Without a terminal
/// the answer is no.
pub fn ask_again(error: &str) -> bool {
    if !(std::io::stdin().is_terminal() && std::io::stderr().is_terminal()) {
        return false;
    }
    eprintln!("amp-harness: {error}");
    eprint!("Edit again? [Y/n] ");
    let _ = std::io::stderr().flush();
    let mut line = String::new();
    if std::io::stdin().lock().read_line(&mut line).is_err() {
        return false;
    }
    !matches!(line.trim().to_ascii_lowercase().as_str(), "n" | "no")
}

/// The `$EDITOR` loop: writes `initial` to `scratch`, runs `editor` ([`editor_command`]) on it,
/// and validates the result. `Ok(None)`: unchanged. `Ok(Some(text))`: valid, for the caller to
/// write. On a validation error `again` decides whether to reopen; if not, nothing is written.
/// `scratch` is always deleted.
pub fn edit_loop(
    editor: &str,
    scratch: &Path,
    initial: &str,
    validate: &dyn Fn(&str) -> Result<(), String>,
    again: &dyn Fn(&str) -> bool,
) -> Result<Option<String>, CliError> {
    let result = (|| {
        write_file(scratch, initial.as_bytes())?;
        loop {
            run_editor(editor, scratch)?;
            let text = read_text(scratch)?;
            if text == initial {
                return Ok(None);
            }
            match validate(&text) {
                Ok(()) => return Ok(Some(text)),
                Err(e) => {
                    if !again(&e) {
                        return Err(CliError::new(format!("{e}; nothing was saved")));
                    }
                }
            }
        }
    })();
    let _ = std::fs::remove_file(scratch);
    if let Some(dir) = scratch.parent() {
        let _ = std::fs::remove_dir(dir);
    }
    result
}

/// `$VISUAL`, else `$EDITOR`, else `vi`.
pub fn editor_command() -> String {
    ["VISUAL", "EDITOR"]
        .into_iter()
        .filter_map(|k| std::env::var(k).ok())
        .find(|v| !v.trim().is_empty())
        .unwrap_or_else(|| "vi".to_owned())
}

fn run_editor(editor: &str, path: &Path) -> Result<(), CliError> {
    // Through the shell, so `EDITOR="code --wait"` works; the path is an argument, never parsed.
    let status = Command::new("sh")
        .arg("-c")
        .arg(format!("{editor} \"$1\""))
        .arg("amp-harness-editor")
        .arg(path)
        .status()
        .map_err(|e| CliError::new(format!("run the editor ({editor}): {e}")))?;
    if !status.success() {
        return Err(CliError::new(format!(
            "the editor ({editor}) exited with {status}; nothing was saved"
        )));
    }
    Ok(())
}

pub fn one_line(text: &str) -> String {
    text.split_whitespace().collect::<Vec<_>>().join(" ")
}

pub fn print_table(header: &[&str], rows: &[Vec<String>]) {
    let mut widths: Vec<usize> = header.iter().map(|h| h.len()).collect();
    for row in rows {
        for (i, cell) in row.iter().enumerate() {
            widths[i] = widths[i].max(cell.chars().count());
        }
    }
    let line = |cells: Vec<&str>| {
        let last = cells.len() - 1;
        let text: Vec<String> = cells
            .iter()
            .enumerate()
            .map(|(i, c)| {
                if i == last {
                    (*c).to_owned()
                } else {
                    format!("{c:<w$}", w = widths[i])
                }
            })
            .collect();
        println!("{}", text.join("  ").trim_end());
    };
    line(header.to_vec());
    for row in rows {
        line(row.iter().map(String::as_str).collect());
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;

    fn dir(name: &str) -> PathBuf {
        let d = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/unit-agents-cmd")
            .join(name);
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    /// The loop reopens after an invalid edit when asked to, and never writes on its own.
    #[test]
    fn edit_loop_reopens_until_valid_or_given_up() {
        let d = dir("edit-loop");
        // An "editor" that appends one line per run.
        let script = d.join("ed.sh");
        std::fs::write(&script, "#!/bin/sh\necho x >> \"$1\"\n").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        let scratch = d.join(".edit/a.md");
        let valid_after_two = |t: &str| {
            if t.lines().count() >= 3 {
                Ok(())
            } else {
                Err("too short".to_owned())
            }
        };
        let asked = Cell::new(0);
        let out = edit_loop(
            &script.to_string_lossy(),
            &scratch,
            "a\n",
            &valid_after_two,
            &|_| {
                asked.set(asked.get() + 1);
                true
            },
        )
        .unwrap();
        assert_eq!(out.as_deref(), Some("a\nx\nx\n"));
        assert_eq!(asked.get(), 1);
        assert!(!scratch.exists());

        let e = edit_loop(
            &script.to_string_lossy(),
            &scratch,
            "a\n",
            &|_| Err("bad".into()),
            &|_| false,
        )
        .unwrap_err();
        assert!(
            e.message.contains("bad; nothing was saved"),
            "{}",
            e.message
        );
        assert!(!scratch.exists());
    }
}
