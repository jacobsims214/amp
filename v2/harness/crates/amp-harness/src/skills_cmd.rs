//! `amp-harness skills ...` (#2234, decision 13): list, show, create, import, edit, remove and
//! reset skills.
//!
//! A skill is a directory with a `SKILL.md` (YAML frontmatter `name` + `description`, then the
//! instructions) and any supporting files: the format Claude Code and opencode use, so
//! `skills import` takes their skill directories as they are. Layers are the same as agents':
//! built-in < user (`<config dir>/skills/`) < repo (`<repo>/.amp/harness/skills/`), with overrides
//! per file. Writers take `--user` (default) or `--repo`, and validate before writing.

use std::path::{Path, PathBuf};

use amp_core::{
    assets::{Assets, Origin, SKILL_FILE, SKILLS_DIR, parse_frontmatter, valid_name},
    repo_trust::ItemKind,
    roles::RoleRegistry,
    templates::{self, set_frontmatter_key, yaml_string},
};
use clap::{Args, Subcommand};
use serde::Serialize;
use serde_json::Value;

use crate::{
    agents_cmd::{
        Ctx, EDIT_DIR, JsonArgs, Scope, ScopeArgs, ask_again, edit_loop, editor_command, one_line,
        print_table, read_text, stash_and_check, write_file,
    },
    conn::{CliError, print_json},
    roster,
};

#[derive(Subcommand)]
pub enum SkillsCommand {
    /// List every skill, where it comes from, and the agents that preload it.
    List(JsonArgs),
    /// Show one skill: its files (and where each comes from), who preloads it, its instructions.
    Show(ShowArgs),
    /// Create a skill from the template, or from a copy of a skill directory (--from).
    New(NewArgs),
    /// Copy a skill directory in as it is (Claude Code's ~/.claude/skills/<name>, opencode's
    /// .opencode/skills/<name>, or any directory with a SKILL.md).
    Import(ImportArgs),
    /// Edit a skill's SKILL.md in $EDITOR (a built-in one is copied to the layer first).
    Edit(NameScopeArgs),
    /// Delete a skill directory from the user or repo layer.
    Remove(NameScopeArgs),
    /// Drop the user/repo overrides of a built-in skill.
    Reset(ResetArgs),
    /// Check every skill loads and every preloaded skill exists.
    Validate(JsonArgs),
}

#[derive(Args)]
pub struct ShowArgs {
    name: String,
    /// Print JSON.
    #[arg(long)]
    json: bool,
}

#[derive(Args)]
pub struct NewArgs {
    /// The skill's name: lowercase letters, digits and hyphens.
    name: String,
    /// When to use it (the model sees this in the skills list).
    #[arg(long)]
    description: Option<String>,
    /// Copy this skill directory instead of the template (its SKILL.md gets the new name).
    #[arg(long, value_name = "DIR")]
    from: Option<PathBuf>,
    #[command(flatten)]
    scope: ScopeArgs,
    /// Open SKILL.md in $EDITOR.
    #[arg(long)]
    edit: bool,
}

#[derive(Args)]
pub struct ImportArgs {
    /// A skill directory (holding SKILL.md).
    dir: PathBuf,
    /// Name it this instead of its frontmatter name (or directory name).
    #[arg(long)]
    name: Option<String>,
    #[command(flatten)]
    scope: ScopeArgs,
}

#[derive(Args)]
pub struct NameScopeArgs {
    name: String,
    #[command(flatten)]
    scope: ScopeArgs,
}

#[derive(Args)]
pub struct ResetArgs {
    name: String,
    /// Only drop the user layer's override.
    #[arg(long, conflicts_with = "repo")]
    user: bool,
    /// Only drop the repo layer's override.
    #[arg(long)]
    repo: bool,
}

pub async fn run(command: SkillsCommand) -> Result<(), CliError> {
    let ctx = Ctx::here()?;
    match command {
        SkillsCommand::List(args) => list(&ctx, args.json),
        SkillsCommand::Show(args) => show(&ctx, &args.name, args.json),
        SkillsCommand::New(args) => new(&ctx, args),
        SkillsCommand::Import(args) => import(&ctx, args),
        SkillsCommand::Edit(args) => edit(&ctx, &args.name, args.scope.scope()),
        SkillsCommand::Remove(args) => remove(&ctx, &args.name, args.scope.scope()),
        SkillsCommand::Reset(args) => reset(&ctx, &args.name, args.user, args.repo),
        SkillsCommand::Validate(args) => validate(&ctx, args.json),
    }
}

/// `skills list --json` / `skills show --json`. Add fields, never rename or remove.
#[derive(Debug, Serialize)]
pub(crate) struct SkillReport {
    pub(crate) name: String,
    pub(crate) description: String,
    /// `built-in`, `user` or `repo`: where SKILL.md comes from.
    pub(crate) origin: &'static str,
    /// SKILL.md's file (`None` for built-in).
    pub(crate) path: Option<String>,
    /// Every layer some file comes from (a user SKILL.md over built-in scripts: built-in, user).
    pub(crate) layers: Vec<&'static str>,
    pub(crate) files: Vec<FileReport>,
    /// Agents in use that preload it (an untrusted repo agent is not one, #2338).
    pub(crate) preloaded_by: Vec<String>,
    /// It is used: `false` only for a repo skill that is not trusted as it is now (`skills show`
    /// of one; `skills list` puts those in `untrusted`), which no session loads (#2338).
    pub(crate) trusted: bool,
    /// SKILL.md's instructions (show only).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) content: Option<String>,
}

#[derive(Debug, Serialize)]
pub(crate) struct FileReport {
    pub(crate) path: String,
    pub(crate) origin: &'static str,
    pub(crate) file: Option<String>,
}

fn kind(origin: &Origin) -> (&'static str, Option<String>) {
    match origin {
        Origin::Embedded => ("built-in", None),
        Origin::User(p) => ("user", Some(p.display().to_string())),
        Origin::Repo(p) => ("repo", Some(p.display().to_string())),
    }
}

pub(crate) fn report(
    assets: &Assets,
    roles: Option<&RoleRegistry>,
    name: &str,
    content: bool,
) -> SkillReport {
    let skill = assets.skill(name).expect("listed skill");
    let (origin, path) = kind(skill.origin());
    let mut layers = Vec::new();
    for layer in ["built-in", "user", "repo"] {
        if skill.files.values().any(|f| kind(&f.origin).0 == layer) {
            layers.push(layer);
        }
    }
    SkillReport {
        name: skill.name.clone(),
        description: skill.description.clone(),
        origin,
        path,
        layers,
        files: skill
            .files
            .iter()
            .map(|(rel, f)| {
                let (origin, file) = kind(&f.origin);
                FileReport {
                    path: rel.clone(),
                    origin,
                    file,
                }
            })
            .collect(),
        preloaded_by: roles
            .map(|r| {
                r.roles()
                    .filter(|role| role.skills.iter().any(|s| s == name))
                    .map(|role| role.id.clone())
                    .collect()
            })
            .unwrap_or_default(),
        content: content.then(|| skill.content.clone()),
        trusted: true,
    }
}

/// The skills and agents sessions here use (repo ones only when trusted, #2338), and the repo
/// skills left out.
fn load_in_use(
    ctx: &Ctx,
) -> Result<(Assets, Option<RoleRegistry>, Vec<roster::UntrustedRow>), CliError> {
    let gate = roster::gate(&ctx.paths);
    let assets = roster::load_assets(&ctx.paths, &gate).map_err(CliError::new)?;
    let roles = RoleRegistry::from_assets(&assets).ok();
    let untrusted = roster::rows(&roster::untrusted(&ctx.paths, &gate), ItemKind::Skill);
    Ok((assets, roles, untrusted))
}

fn list(ctx: &Ctx, json: bool) -> Result<(), CliError> {
    let (assets, roles, untrusted) = load_in_use(ctx)?;
    let skills: Vec<SkillReport> = assets
        .skills()
        .map(|s| report(&assets, roles.as_ref(), &s.name, false))
        .collect();
    if json {
        return print_json(&serde_json::json!({
            "user_dir": ctx.paths.user_dir.as_ref().map(|p| p.display().to_string()),
            "repo_dir": ctx.paths.repo_dir.as_ref().map(|p| p.display().to_string()),
            "skills": skills,
            "untrusted": untrusted,
        }));
    }
    let rows: Vec<Vec<String>> = skills
        .iter()
        .map(|s| {
            let mut desc = one_line(&s.description);
            if desc.chars().count() > 60 {
                desc = desc.chars().take(57).collect::<String>() + "...";
            }
            vec![
                s.name.clone(),
                s.layers.join("+"),
                desc,
                if s.preloaded_by.is_empty() {
                    "-".into()
                } else {
                    s.preloaded_by.join(", ")
                },
            ]
        })
        .collect();
    print_table(&["SKILL", "FROM", "DESCRIPTION", "PRELOADED BY"], &rows);
    if let Some(footer) = roster::footer(ItemKind::Skill, &untrusted) {
        println!("\n{footer}");
    }
    println!();
    ctx.print_layers(SKILLS_DIR);
    Ok(())
}

fn show(ctx: &Ctx, name: &str, json: bool) -> Result<(), CliError> {
    let gate = roster::gate(&ctx.paths);
    let in_use = roster::load_assets(&ctx.paths, &gate).map_err(CliError::new)?;
    let roles = RoleRegistry::from_assets(&in_use).ok();
    let untrusted = roster::untrusted(&ctx.paths, &gate);
    let left_out = roster::find(&untrusted, ItemKind::Skill, name);
    // An untrusted repo skill nothing uses is reported from its files, marked as not trusted
    // (#2338); "preloaded by" counts only the agents in use.
    let untrusted_self = in_use.skill(name).is_none() && left_out.is_some();
    let assets = if untrusted_self {
        ctx.load_assets()?
    } else {
        in_use
    };
    if assets.skill(name).is_none() {
        return Err(no_skill(name));
    }
    let mut r = report(&assets, roles.as_ref(), name, true);
    r.trusted = !untrusted_self;
    if json {
        return print_json(&r);
    }
    if untrusted_self {
        println!(
            "{}  ({}, not trusted, not used)",
            r.name,
            r.layers.join(" + ")
        );
    } else {
        println!("{}  ({})", r.name, r.layers.join(" + "));
    }
    if let Some(u) = left_out {
        println!("  trust:        {}", u.reason());
    }
    println!(
        "  file:         {}",
        r.path
            .clone()
            .unwrap_or_else(|| format!("built-in {SKILLS_DIR}/{name}/{SKILL_FILE}"))
    );
    println!("  description:  {}", one_line(&r.description));
    println!(
        "  preloaded by: {}",
        if r.preloaded_by.is_empty() {
            "(none; agents can still load it on demand)".to_owned()
        } else {
            r.preloaded_by.join(", ")
        }
    );
    println!("  files:");
    for f in &r.files {
        println!(
            "    {}  ({}{})",
            f.path,
            f.origin,
            f.file
                .as_ref()
                .map(|p| format!(": {p}"))
                .unwrap_or_default()
        );
    }
    println!();
    println!("{}", r.content.unwrap_or_default());
    Ok(())
}

fn no_skill(name: &str) -> CliError {
    CliError::usage(format!(
        "no skill {name:?} (`amp-harness skills list` shows them)"
    ))
}

/// Checks a new or changed skill: `files` (relative path -> bytes) laid over what is loaded, and
/// the roster still builds.
fn check_skill(
    ctx: &Ctx,
    name: &str,
    files: &[(String, Vec<u8>)],
    scope: Scope,
    dest: &Path,
) -> Result<(), String> {
    let mut assets = ctx.load_assets().map_err(|e| e.message)?;
    // SKILL.md first: a new skill needs it before any other file can be added.
    let ordered = files
        .iter()
        .filter(|(r, _)| r == SKILL_FILE)
        .chain(files.iter().filter(|(r, _)| r != SKILL_FILE));
    for (rel, bytes) in ordered {
        assets
            .put_skill_file(name, rel, bytes.clone(), scope.origin(dest.join(rel)))
            .map_err(|e| e.to_string())?;
    }
    RoleRegistry::from_assets(&assets).map_err(|e| e.to_string())?;
    Ok(())
}

fn new(ctx: &Ctx, args: NewArgs) -> Result<(), CliError> {
    let name = args.name.trim().to_owned();
    let scope = args.scope.scope();
    let description = args.description.as_deref().map(one_line);
    let files = match &args.from {
        None => vec![(
            SKILL_FILE.to_owned(),
            templates::render_skill(
                &name,
                description
                    .as_deref()
                    .unwrap_or(&format!("{name}: say when an agent should use this skill.")),
            )
            .into_bytes(),
        )],
        Some(dir) => read_skill_dir(dir)?,
    };
    let dest = install(ctx, &name, files, description.as_deref(), scope)?;
    if args.edit {
        edit_skill_md(ctx, &name, scope)?;
    }
    println!(
        "Created skill {name} ({}): {}",
        scope.name(),
        dest.display()
    );
    println!(
        "Preload it with `amp-harness agents skills <agent> add {name}`; edit it with `amp-harness skills edit {name}`."
    );
    Ok(())
}

fn import(ctx: &Ctx, args: ImportArgs) -> Result<(), CliError> {
    let files = read_skill_dir(&args.dir)?;
    let skill_md = files
        .iter()
        .find(|(r, _)| r == SKILL_FILE)
        .map(|(_, b)| String::from_utf8_lossy(b).into_owned())
        .unwrap_or_default();
    let (fm, _) = parse_frontmatter(&skill_md)
        .map_err(|e| CliError::new(format!("{}: {e}", args.dir.join(SKILL_FILE).display())))?;
    let name = match &args.name {
        Some(n) => n.trim().to_owned(),
        None => fm
            .get("name")
            .and_then(Value::as_str)
            .map(str::to_owned)
            .unwrap_or_else(|| {
                args.dir
                    .file_name()
                    .map(|n| n.to_string_lossy().into_owned())
                    .unwrap_or_default()
            }),
    };
    let scope = args.scope.scope();
    let dest = install(ctx, &name, files, None, scope)?;
    println!(
        "Imported skill {name} ({}) from {}: {}",
        scope.name(),
        args.dir.display(),
        dest.display()
    );
    Ok(())
}

/// Every file of a skill directory (dot files skipped), relative `/`-paths. Needs a SKILL.md.
fn read_skill_dir(dir: &Path) -> Result<Vec<(String, Vec<u8>)>, CliError> {
    if !dir.join(SKILL_FILE).is_file() {
        return Err(CliError::usage(format!(
            "{} has no {SKILL_FILE}; a skill directory holds SKILL.md and its files",
            dir.display()
        )));
    }
    fn walk(root: &Path, dir: &Path, out: &mut Vec<(String, Vec<u8>)>) -> Result<(), CliError> {
        let mut entries: Vec<PathBuf> = std::fs::read_dir(dir)
            .map_err(|e| CliError::new(format!("read {}: {e}", dir.display())))?
            .filter_map(|e| e.ok().map(|e| e.path()))
            .collect();
        entries.sort();
        for path in entries {
            let hidden = path
                .file_name()
                .is_some_and(|n| n.to_string_lossy().starts_with('.'));
            if hidden {
                continue;
            }
            if path.is_dir() {
                walk(root, &path, out)?;
            } else if path.is_file() {
                let bytes = std::fs::read(&path)
                    .map_err(|e| CliError::new(format!("read {}: {e}", path.display())))?;
                let rel = path
                    .strip_prefix(root)
                    .unwrap_or(&path)
                    .components()
                    .map(|c| c.as_os_str().to_string_lossy())
                    .collect::<Vec<_>>()
                    .join("/");
                out.push((rel, bytes));
            }
        }
        Ok(())
    }
    let mut out = Vec::new();
    walk(dir, dir, &mut out)?;
    Ok(out)
}

/// Writes a new skill `name` into the layer: SKILL.md's `name` (and `description`, when given)
/// set, validated first. Refuses a name that is already loaded.
fn install(
    ctx: &Ctx,
    name: &str,
    mut files: Vec<(String, Vec<u8>)>,
    description: Option<&str>,
    scope: Scope,
) -> Result<PathBuf, CliError> {
    if !valid_name(name) {
        return Err(CliError::usage(format!(
            "skill name {name:?} must be lowercase letters, digits and single hyphens (use --name)"
        )));
    }
    let assets = ctx.load_assets()?;
    if let Some(existing) = assets.skill(name) {
        return Err(CliError::usage(format!(
            "skill {name} already exists ({}); change it with `amp-harness skills edit {name}`, or pick another name",
            kind(existing.origin()).0
        )));
    }
    let dest = ctx.scope_dir(scope)?.join(SKILLS_DIR).join(name);
    if dest.exists() {
        return Err(CliError::usage(format!(
            "{} already exists; nothing was written",
            dest.display()
        )));
    }
    for (rel, bytes) in files.iter_mut() {
        if rel != SKILL_FILE {
            continue;
        }
        let mut text = String::from_utf8(bytes.clone())
            .map_err(|_| CliError::new(format!("{SKILL_FILE} is not UTF-8")))?;
        text =
            set_frontmatter_key(&text, "name", Some(&yaml_string(name))).map_err(CliError::new)?;
        if let Some(d) = description {
            text = set_frontmatter_key(&text, "description", Some(&yaml_string(d)))
                .map_err(CliError::new)?;
        }
        *bytes = text.into_bytes();
    }
    check_skill(ctx, name, &files, scope, &dest).map_err(CliError::new)?;
    for (rel, bytes) in &files {
        write_file(&dest.join(rel), bytes)?;
    }
    Ok(dest)
}

fn edit(ctx: &Ctx, name: &str, scope: Scope) -> Result<(), CliError> {
    if ctx.load_assets()?.skill(name).is_none() {
        return Err(no_skill(name));
    }
    edit_skill_md(ctx, name, scope)
}

fn edit_skill_md(ctx: &Ctx, name: &str, scope: Scope) -> Result<(), CliError> {
    let assets = ctx.load_assets()?;
    let skill = assets.skill(name).ok_or_else(|| no_skill(name))?;
    let dest = ctx.scope_dir(scope)?.join(SKILLS_DIR).join(name);
    let path = dest.join(SKILL_FILE);
    let initial = if path.is_file() {
        read_text(&path)?
    } else {
        String::from_utf8(skill.files[SKILL_FILE].bytes.clone())
            .map_err(|_| CliError::new(format!("{name}/{SKILL_FILE} is not UTF-8")))?
    };
    let validate = |text: &str| {
        check_skill(
            ctx,
            name,
            &[(SKILL_FILE.to_owned(), text.as_bytes().to_vec())],
            scope,
            &dest,
        )
    };
    let scratch = ctx
        .scope_dir(scope)?
        .join(EDIT_DIR)
        .join(format!("{name}-{SKILL_FILE}"));
    match edit_loop(&editor_command(), &scratch, &initial, &validate, &ask_again)? {
        None => println!("No changes to {name}."),
        Some(text) => {
            write_file(&path, text.as_bytes())?;
            println!("Saved {name}: {}", path.display());
        }
    }
    Ok(())
}

fn remove(ctx: &Ctx, name: &str, scope: Scope) -> Result<(), CliError> {
    let dir = ctx.scope_dir(scope)?.join(SKILLS_DIR).join(name);
    if !dir.is_dir() {
        let other = match scope {
            Scope::User => Scope::Repo,
            Scope::Repo => Scope::User,
        };
        let elsewhere = ctx
            .scope_dir(other)
            .ok()
            .map(|d| d.join(SKILLS_DIR).join(name))
            .filter(|d| d.is_dir());
        return Err(CliError::usage(match elsewhere {
            Some(d) => format!(
                "skill {name} is not in the {} layer; it is in the {} layer ({}): add --{}",
                scope.name(),
                other.name(),
                d.display(),
                other.name()
            ),
            None if Assets::is_embedded_skill(name) => format!(
                "skill {name} is built in and can't be removed; `amp-harness skills reset {name}` drops overrides"
            ),
            None => format!("no skill {name}"),
        }));
    }
    stash_and_check(ctx, &dir, &|| check_roster(ctx, name))?;
    println!("Removed skill {name} ({}): {}", scope.name(), dir.display());
    Ok(())
}

/// The roster still builds; names the agents that preload `name` when it doesn't.
fn check_roster(ctx: &Ctx, name: &str) -> Result<(), String> {
    let assets = ctx.load_assets().map_err(|e| e.message)?;
    RoleRegistry::from_assets(&assets)
        .map_err(|e| format!("{e}; first run `amp-harness agents skills <agent> remove {name}`"))?;
    Ok(())
}

fn reset(ctx: &Ctx, name: &str, user: bool, repo: bool) -> Result<(), CliError> {
    if !Assets::is_embedded_skill(name) {
        return Err(CliError::usage(format!(
            "skill {name} is not built in; `amp-harness skills remove {name}` deletes it"
        )));
    }
    let scopes: Vec<Scope> = match (user, repo) {
        (true, _) => vec![Scope::User],
        (_, true) => vec![Scope::Repo],
        _ => vec![Scope::User, Scope::Repo],
    };
    let mut removed = Vec::new();
    for scope in scopes {
        let Ok(layer) = ctx.scope_dir(scope) else {
            continue;
        };
        let dir = layer.join(SKILLS_DIR).join(name);
        if dir.is_dir() {
            stash_and_check(ctx, &dir, &|| check_roster(ctx, name))?;
            removed.push(format!("{} ({})", scope.name(), dir.display()));
        }
    }
    if removed.is_empty() {
        println!("{name} has no overrides; it is the built-in one.");
    } else {
        println!("Reset {name}: removed {}", removed.join(", "));
    }
    Ok(())
}

#[derive(Serialize)]
struct ValidateReport {
    ok: bool,
    skills: usize,
    problems: Vec<String>,
}

fn validate(ctx: &Ctx, json: bool) -> Result<(), CliError> {
    let mut problems = Vec::new();
    let mut count = 0;
    match ctx.load_assets() {
        Err(e) => problems.push(e.message),
        Ok(assets) => {
            count = assets.skills().count();
            if let Err(e) = RoleRegistry::from_assets(&assets) {
                problems.push(e.to_string());
            }
        }
    }
    let ok = problems.is_empty();
    if json {
        print_json(&ValidateReport {
            ok,
            skills: count,
            problems,
        })?;
    } else {
        for p in &problems {
            println!("error: {p}");
        }
        if ok {
            println!("{count} skills OK");
        }
    }
    if ok { Ok(()) } else { Err(CliError::silent(1)) }
}
