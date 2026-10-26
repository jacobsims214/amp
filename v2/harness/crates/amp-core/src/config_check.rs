//! `amp-harness config check` (#2235, "Validation everywhere" in the configurability design): every
//! problem in the person's configuration, each naming its layer, file and key, with a fix.
//!
//! The configuration is read once into [`Inputs`] (the user `config.toml`, the repo's
//! `.amp/harness/config.toml`, and the merged [`ModelsConfig`] when it loads). Then each
//! [`Checker`] in [`CHECKERS`] looks at one area and returns its [`Problem`]s. A checker never stops
//! the others, so one broken table does not hide the rest.
//!
//! ## Adding an area (MCP #2233, agents and skills #2234, ...)
//!
//! Write `fn check(inputs: &Inputs) -> Vec<Problem>` next to the code that owns the area and add
//! one `("<area>", path::check)` line to [`CHECKERS`]. Read the raw tables from
//! [`Inputs::user`] (the user file as a TOML table) and [`Inputs::repo`] (`RepoConfig.mcp` for the
//! repo's `[mcp]`), and directories from [`Inputs::paths`] (`user_dir`, `repo_dir`). Use
//! [`Problem::error`] / [`Problem::warning`] with the file, the dotted key and a fix. Checks that
//! need the harness binary (the TUI's `[tui]`, network) are passed to [`check`] as `extra`.
//! Never put a secret value in a message.

use std::path::{Path, PathBuf};

use serde::Serialize;

use crate::{
    models::{self, ModelsConfig, ModelsFile, normalize_role},
    providers::{KeyLookup, resolve_key},
    repo_config::RepoConfig,
};

/// How bad a problem is. Errors make `config check` exit 1.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Severity {
    Error,
    Warning,
}

/// Which layer a problem is in.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Layer {
    /// The shipped defaults (a problem here comes from a user override of them).
    Defaults,
    /// The user's config.toml (and the user config dir).
    User,
    /// The repo's `.amp/harness/config.toml` (and `.amp/harness/`).
    Repo,
    /// The environment (`AMP_HARNESS_PROFILE`, key variables).
    Env,
}

/// One problem. `config check --json` prints these; add fields, never rename or remove them.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Problem {
    pub severity: Severity,
    pub layer: Layer,
    /// The checker that found it: `files`, `server`, `providers`, `models`, `roles`, `workers`,
    /// `tools`, `tui`, `catalog`, later `mcp`, `agents`, `skills`.
    pub area: &'static str,
    pub file: Option<PathBuf>,
    /// The dotted key (`roles.reviewer.model`, `providers.local`).
    pub key: Option<String>,
    pub message: String,
    pub fix: Option<String>,
}

impl Problem {
    pub fn error(area: &'static str, layer: Layer, message: impl Into<String>) -> Self {
        Self {
            severity: Severity::Error,
            layer,
            area,
            file: None,
            key: None,
            message: message.into(),
            fix: None,
        }
    }

    pub fn warning(area: &'static str, layer: Layer, message: impl Into<String>) -> Self {
        Self {
            severity: Severity::Warning,
            ..Self::error(area, layer, message)
        }
    }

    pub fn file(mut self, file: Option<&Path>) -> Self {
        self.file = file.map(Path::to_path_buf);
        self
    }

    pub fn key(mut self, key: impl Into<String>) -> Self {
        self.key = Some(key.into());
        self
    }

    pub fn fix(mut self, fix: impl Into<String>) -> Self {
        self.fix = Some(fix.into());
        self
    }
}

/// Everything a checker may look at, read once.
pub struct Inputs<'a> {
    pub paths: &'a amp_mcp::ConfigPaths,
    pub env: &'a dyn Fn(&str) -> Option<String>,
    pub keys: &'a dyn KeyLookup,
    /// The user file's text, when it exists and could be read.
    pub user_text: Option<String>,
    /// The user file as TOML, when it parses.
    pub user: Option<toml::Table>,
    /// The user file's model tables, when they deserialize.
    pub user_models: Option<ModelsFile>,
    /// The repo file, when it exists and passes [`RepoConfig::parse`].
    pub repo: Option<RepoConfig>,
    /// Defaults + user + repo, when they merge.
    pub models: Option<ModelsConfig>,
    /// Problems found while reading (syntax, unreadable files).
    read_problems: Vec<Problem>,
}

impl Inputs<'_> {
    pub fn user_file(&self) -> Option<&Path> {
        self.paths.user_file.as_deref()
    }

    /// `paths.repo_file()`, when the repo layer is in use.
    pub fn repo_file(&self) -> Option<PathBuf> {
        self.paths.repo_file()
    }

    /// The layer and file that set `[roles.<role>]` last: the repo, the user file, else the
    /// defaults (or a profile).
    pub fn role_layer(&self, role: &str) -> (Layer, Option<PathBuf>) {
        let role = normalize_role(role);
        let has = |roles: Option<&toml::Value>| {
            roles
                .and_then(toml::Value::as_table)
                .is_some_and(|t| t.contains_key(role) || t.contains_key(&format!("amp-{role}")))
        };
        if self
            .repo
            .as_ref()
            .is_some_and(|r| r.roles.keys().any(|k| normalize_role(k) == role))
        {
            return (Layer::Repo, self.repo_file());
        }
        if has(self.user.as_ref().and_then(|u| u.get("roles"))) {
            return (Layer::User, self.paths.user_file.clone());
        }
        (Layer::Defaults, None)
    }
}

/// A check of one area.
pub type Checker = fn(&Inputs) -> Vec<Problem>;

/// Every area `config check` validates, in order. Add new areas here (see the module docs).
pub const CHECKERS: &[(&str, Checker)] = &[
    ("files", check_files),
    ("server", check_server),
    ("models", check_models),
    ("providers", check_providers),
    ("roles", check_roles),
    ("workers", check_workers),
    ("manager", check_manager),
    ("tools", check_tools),
    ("mcp", check_mcp),
    ("skills", check_skills),
    ("agents", check_agents),
];

/// Top-level tables some part of the harness reads from the user file. Anything else is a warning
/// (a typo such as `[role.x]` would otherwise be ignored silently).
pub const USER_TABLES: &[&str] = &[
    "server",
    "servers",
    "models",
    "providers",
    "roles",
    "profiles",
    "workers",
    "manager",
    "tools",
    "secrets",
    "tui",
    "mcp",
];

/// The result of [`check`].
#[derive(Debug, Clone, Serialize)]
pub struct CheckReport {
    /// No errors (warnings allowed).
    pub ok: bool,
    pub user_file: Option<PathBuf>,
    pub user_file_found: bool,
    pub repo_file: Option<PathBuf>,
    pub repo_file_found: bool,
    /// The areas that were checked, in order.
    pub areas: Vec<String>,
    pub problems: Vec<Problem>,
}

impl CheckReport {
    pub fn errors(&self) -> usize {
        self.problems
            .iter()
            .filter(|p| p.severity == Severity::Error)
            .count()
    }

    pub fn warnings(&self) -> usize {
        self.problems.len() - self.errors()
    }

    /// Adds problems found outside [`check`] (the live catalog) and updates `ok`.
    pub fn extend(&mut self, area: &str, problems: Vec<Problem>) {
        if !self.areas.iter().any(|a| a == area) {
            self.areas.push(area.to_owned());
        }
        self.problems.extend(problems);
        self.ok = self.errors() == 0;
    }
}

/// Reads the layers in `paths` and runs [`CHECKERS`], then `extra`.
pub fn check(
    paths: &amp_mcp::ConfigPaths,
    env: &dyn Fn(&str) -> Option<String>,
    keys: &dyn KeyLookup,
    extra: &[(&str, Checker)],
) -> CheckReport {
    let inputs = read_inputs(paths, env, keys);
    let mut problems = inputs.read_problems.clone();
    let mut areas = Vec::new();
    for (area, checker) in CHECKERS.iter().chain(extra) {
        areas.push((*area).to_owned());
        problems.extend(checker(&inputs));
    }
    let repo_file = paths.repo_file();
    let mut report = CheckReport {
        ok: true,
        user_file_found: inputs.user_text.is_some(),
        user_file: paths.user_file.clone(),
        repo_file_found: repo_file.as_deref().is_some_and(Path::is_file),
        repo_file,
        areas,
        problems,
    };
    report.ok = report.errors() == 0;
    report
}

/// An error message fit to print: toml's multi-line report (which quotes the source line, maybe a
/// secret) becomes its first line plus the message, and serde's quoted string values are dropped.
pub fn clean(message: &str) -> String {
    let lines: Vec<&str> = message
        .lines()
        .map(str::trim)
        .filter(|l| !l.is_empty())
        .collect();
    let short = match lines.as_slice() {
        [] => String::new(),
        [one] => (*one).to_owned(),
        [first, .., last] => format!("{first}: {last}"),
    };
    crate::mcp_config::redact_serde(&short)
}

/// Reads and parses the layers once; read problems go in `read_problems`.
pub fn read_inputs<'a>(
    paths: &'a amp_mcp::ConfigPaths,
    env: &'a dyn Fn(&str) -> Option<String>,
    keys: &'a dyn KeyLookup,
) -> Inputs<'a> {
    let mut read_problems = Vec::new();
    let user_path = paths.user_file.as_deref();

    let user_text = match user_path.map(std::fs::read_to_string) {
        Some(Ok(text)) => Some(text),
        Some(Err(e)) if e.kind() == std::io::ErrorKind::NotFound => {
            if paths.user_file_named {
                read_problems.push(
                    Problem::error("files", Layer::User, "the config file named does not exist")
                        .file(user_path)
                        .fix("check the --config / AMP_HARNESS_CONFIG path"),
                );
            }
            None
        }
        Some(Err(e)) => {
            read_problems.push(
                Problem::error("files", Layer::User, format!("cannot read: {e}")).file(user_path),
            );
            None
        }
        None => None,
    };
    let user: Option<toml::Table> = user_text.as_deref().and_then(|text| {
        toml::from_str(text)
            .map_err(|e| {
                read_problems.push(
                    Problem::error(
                        "files",
                        Layer::User,
                        format!("not valid TOML: {}", clean(&e.to_string())),
                    )
                    .file(user_path)
                    .fix("fix the syntax; nothing else in this file is checked until it parses"),
                );
            })
            .ok()
    });
    let user_models = match (&user_text, &user) {
        (Some(text), Some(_)) => ModelsFile::parse(text, "").ok(),
        (None, _) => Some(ModelsFile::default()),
        _ => None,
    };

    let repo_path = paths.repo_file();
    let repo = match repo_path.as_deref().map(RepoConfig::load) {
        Some(Ok(repo)) => repo,
        Some(Err(e)) => {
            read_problems.push(
                Problem::error("files", Layer::Repo, clean(&e.0))
                    .file(repo_path.as_deref())
                    .fix("a repo config may hold only [mcp.*], [roles.*] and [models] profile"),
            );
            None
        }
        None => None,
    };
    let repo_ok = repo_path
        .as_deref()
        .is_none_or(|p| !p.is_file() || repo.is_some());

    let models = match &user_models {
        Some(user_models) if repo_ok => {
            let label = user_path
                .map(|p| format!("config {}", p.display()))
                .unwrap_or_else(|| "config".into());
            ModelsConfig::from_layers(
                ModelsFile::defaults(),
                Some(user_models.clone()),
                &label,
                repo.as_ref(),
                env,
            )
            .ok()
        }
        _ => None,
    };

    Inputs {
        paths,
        env,
        keys,
        user_text,
        user,
        user_models,
        repo,
        models,
        read_problems,
    }
}

/// Unknown top-level tables in the user file (a warning: they are ignored).
fn check_files(inputs: &Inputs) -> Vec<Problem> {
    let Some(user) = &inputs.user else {
        return Vec::new();
    };
    user.keys()
        .filter(|k| !USER_TABLES.contains(&k.as_str()))
        .map(|k| {
            Problem::warning(
                "files",
                Layer::User,
                format!("`{k}` is not a setting amp-harness reads; it is ignored"),
            )
            .file(inputs.user_file())
            .key(k.clone())
            .fix(format!("known tables: {}", USER_TABLES.join(", ")))
        })
        .collect()
}

/// `[server]` as amp-mcp reads it.
fn check_server(inputs: &Inputs) -> Vec<Problem> {
    let Some(text) = &inputs.user_text else {
        return Vec::new();
    };
    if inputs.user.is_none() {
        return Vec::new();
    }
    match toml::from_str::<amp_mcp::FileConfig>(text) {
        Ok(_) => Vec::new(),
        Err(e) => vec![
            Problem::error("server", Layer::User, clean(&e.to_string()))
                .file(inputs.user_file())
                .key("server"),
        ],
    }
}

/// `[models]`, `[profiles]` and the model tables' schema; the selected profile / inherit mode.
fn check_models(inputs: &Inputs) -> Vec<Problem> {
    let mut out = Vec::new();
    if let (Some(text), Some(_), None) = (&inputs.user_text, &inputs.user, &inputs.user_models)
        && let Err(e) = ModelsFile::parse(text, "")
    {
        out.push(
            Problem::error(
                "models",
                Layer::User,
                clean(e.0.trim_start_matches("parse : ")),
            )
            .file(inputs.user_file())
            .fix("[models], [providers.*], [roles.*] and [profiles.*] accept only known keys"),
        );
        return out;
    }
    let Some(user_models) = &inputs.user_models else {
        return out;
    };
    if inputs.models.is_some() {
        if let Some(p) = (inputs.env)(models::ENV_PROFILE).filter(|p| !p.trim().is_empty()) {
            out.push(
                Problem::warning(
                    "models",
                    Layer::Env,
                    format!(
                        "{} = {p} overrides [models] profile in every file",
                        models::ENV_PROFILE
                    ),
                )
                .key("models.profile"),
            );
        }
        return out;
    }
    // Providers that don't build are the providers check's; anything else is the selection.
    if !models::provider_problems(&ModelsFile::defaults(), user_models).is_empty() {
        return out;
    }
    let label = "config";
    let err = ModelsConfig::from_layers(
        ModelsFile::defaults(),
        Some(user_models.clone()),
        label,
        inputs.repo.as_ref(),
        inputs.env,
    )
    .err();
    if let Some(e) = err {
        let (layer, file) =
            if (inputs.env)(models::ENV_PROFILE).is_some_and(|p| !p.trim().is_empty()) {
                (Layer::Env, None)
            } else if inputs.repo.as_ref().is_some_and(|r| r.profile.is_some()) {
                (Layer::Repo, inputs.repo_file())
            } else {
                (Layer::User, inputs.paths.user_file.clone())
            };
        out.push(
            Problem::error("models", layer, e.0)
                .file(file.as_deref())
                .key("models.profile")
                .fix("`amp-harness models profile <opencode|anthropic|inherit|name>` sets a valid one"),
        );
    }
    out
}

/// Each provider builds; the providers some role uses have a key.
fn check_providers(inputs: &Inputs) -> Vec<Problem> {
    let Some(user_models) = &inputs.user_models else {
        return Vec::new();
    };
    let mut out: Vec<Problem> = models::provider_problems(&ModelsFile::defaults(), user_models)
        .into_iter()
        .map(|(id, message)| {
            let layer = if user_models.providers.contains_key(&id) {
                Layer::User
            } else {
                Layer::Defaults
            };
            Problem::error("providers", layer, message)
                .file(inputs.user_file().filter(|_| layer == Layer::User))
                .key(format!("providers.{id}"))
        })
        .collect();
    if let Some(config) = &inputs.models {
        for id in config.providers_in_use() {
            let Some(provider) = config.providers.get(&id) else {
                continue;
            };
            let (key, source) = resolve_key(provider, inputs.keys);
            if provider.needs_key && key.is_none() {
                out.push(
                    Problem::warning(
                        "providers",
                        Layer::Env,
                        format!("roles use `{id}` but it has no key: {source}"),
                    )
                    .key(format!("providers.{id}"))
                    .fix(format!(
                        "run `amp-harness connect {id}`{}",
                        provider
                            .key_env
                            .as_deref()
                            .map(|v| format!(", or set {v}"))
                            .unwrap_or_default()
                    )),
                );
            }
        }
    }
    out
}

/// Every configured role resolves to a provider, a model and a thinking level.
fn check_roles(inputs: &Inputs) -> Vec<Problem> {
    let Some(config) = &inputs.models else {
        return Vec::new();
    };
    config
        .resolve_all()
        .into_iter()
        .filter_map(|(role, r)| r.err().map(|e| (role, e)))
        .map(|(role, e)| {
            let (layer, file) = inputs.role_layer(&role);
            Problem::error("roles", layer, e.0)
                .file(file.as_deref())
                .key(format!("roles.{role}"))
                .fix(format!(
                    "`amp-harness models set {role} <provider/model>` or `amp-harness models reset {role}`"
                ))
        })
        .collect()
}

/// `[workers]` as the supervisor reads it.
fn check_workers(inputs: &Inputs) -> Vec<Problem> {
    let Some(path) = inputs.paths.user_file.clone() else {
        return Vec::new();
    };
    if inputs.user.is_none() {
        return Vec::new();
    }
    match crate::supervisor::SupervisorConfig::load(&|_| None, Some(path)) {
        Ok(_) => Vec::new(),
        Err(e) => vec![
            Problem::error("workers", Layer::User, clean(&e))
                .file(inputs.user_file())
                .key("workers"),
        ],
    }
}

/// `[manager]` (#2425): `auto_resume_on_approval`, a boolean.
fn check_manager(inputs: &Inputs) -> Vec<Problem> {
    let Some(text) = &inputs.user_text else {
        return Vec::new();
    };
    let Ok(doc) = toml::from_str::<toml::Table>(text) else {
        return Vec::new();
    };
    match crate::approval_wait::manager_auto_resume(&doc) {
        Ok(_) => Vec::new(),
        Err(e) => vec![
            Problem::error("manager", Layer::User, clean(&e))
                .file(inputs.user_file())
                .key("manager"),
        ],
    }
}

/// `[tools]` and `[secrets]` as a session reads them.
fn check_tools(inputs: &Inputs) -> Vec<Problem> {
    let (Some(text), Some(_)) = (&inputs.user_text, &inputs.user) else {
        return Vec::new();
    };
    match crate::agent::check_harness_tables(text) {
        Ok(()) => Vec::new(),
        Err(e) => vec![
            Problem::error(
                "tools",
                Layer::User,
                format!("[tools]/[secrets]: {}", clean(&e)),
            )
            .file(inputs.user_file()),
        ],
    }
}

/// `[mcp.*]` in both files, as the MCP layer reads them ([`crate::mcp_config::McpConfig::load`]).
fn check_mcp(inputs: &Inputs) -> Vec<Problem> {
    let repo_file = inputs.repo_file();
    // A file that doesn't parse at all is already the files check's problem.
    let user_broken = inputs.user_text.is_some() && inputs.user.is_none();
    let repo_broken = inputs.repo.is_none() && repo_file.as_deref().is_some_and(Path::is_file);
    crate::mcp_config::McpConfig::load(inputs.paths)
        .problems
        .into_iter()
        .filter_map(|p| {
            let layer = if repo_file.as_deref() == Some(p.file.as_path()) {
                Layer::Repo
            } else {
                Layer::User
            };
            let duplicate = p.key == "mcp"
                && match layer {
                    Layer::Repo => repo_broken,
                    _ => user_broken,
                };
            if duplicate {
                return None;
            }
            let problem = match p.severity {
                crate::mcp_config::Severity::Error => Problem::error("mcp", layer, p.message),
                crate::mcp_config::Severity::Warning => Problem::warning("mcp", layer, p.message),
            }
            .file(Some(&p.file))
            .key(p.key);
            Some(match p.fix {
                Some(fix) => problem.fix(fix),
                None => problem,
            })
        })
        .collect()
}

/// The layer an asset file belongs to, from its origin.
fn asset_layer(origin: &crate::assets::Origin) -> (Layer, Option<PathBuf>) {
    match origin {
        crate::assets::Origin::Embedded => (Layer::Defaults, None),
        crate::assets::Origin::User(p) => (Layer::User, Some(p.clone())),
        crate::assets::Origin::Repo(p) => (Layer::Repo, Some(p.clone())),
    }
}

/// The layer an asset loading error is in: the repo when it names a file under the repo's
/// `.amp/harness`, else the user layer.
fn error_layer(inputs: &Inputs, message: &str) -> Layer {
    match &inputs.paths.repo_dir {
        Some(repo) if message.contains(&*repo.to_string_lossy()) => Layer::Repo,
        _ => Layer::User,
    }
}

/// `skills`: every skill file loads (#2246). A preloaded skill that does not exist is the agents
/// area's problem (the roster names the agent).
fn check_skills(inputs: &Inputs) -> Vec<Problem> {
    match crate::assets::Assets::load(&crate::assets::OverrideDirs::from_paths(inputs.paths)) {
        Ok(_) => Vec::new(),
        Err(e) => {
            let message = clean(&e.to_string());
            vec![
                Problem::error("skills", error_layer(inputs, &message), message).fix(
                    "fix or remove the file it names (`amp-harness skills validate` checks again)",
                ),
            ]
        }
    }
}

/// `agents`: the roster builds (frontmatter, permissions, preloaded skills) and every agent
/// resolves to a model through D11's fallback chain (#2246).
fn check_agents(inputs: &Inputs) -> Vec<Problem> {
    // A file that does not load is reported by the skills area.
    let Ok(assets) =
        crate::assets::Assets::load(&crate::assets::OverrideDirs::from_paths(inputs.paths))
    else {
        return Vec::new();
    };
    let roles = match crate::roles::RoleRegistry::from_assets(&assets) {
        Ok(roles) => roles,
        Err(e) => {
            let message = clean(&e.to_string());
            return vec![
                Problem::error("agents", error_layer(inputs, &message), message).fix(
                    "fix the agent's frontmatter (`amp-harness agents validate` checks again)",
                ),
            ];
        }
    };
    let mut out = Vec::new();
    for role in roles.roles() {
        let (layer, file) = assets
            .agent(&role.id)
            .map(|a| asset_layer(&a.origin))
            .unwrap_or((Layer::Defaults, None));
        if let Some(models) = &inputs.models
            && let Err(e) = role.resolve_model(models)
        {
            out.push(
                Problem::error("agents", layer, format!("agent {}: {}", role.id, e.0))
                    .file(file.as_deref())
                    .fix(format!(
                        "`amp-harness models set {} <provider/model>`, or set [models] fallback \
                         to a role or provider/model that resolves",
                        normalize_role(&role.model_role)
                    )),
            );
        }
        if let Some(base) = &role.based_on
            && roles.get(base).is_none()
            && !inputs.models.as_ref().is_some_and(|m| m.has_role(base))
        {
            out.push(
                Problem::warning(
                    "agents",
                    layer,
                    format!(
                        "agent {}: based_on {base:?} is not an agent or a configured role",
                        role.id
                    ),
                )
                .file(file.as_deref())
                .key("based_on")
                .fix("name an existing agent, or remove based_on to use [models] fallback"),
            );
        }
        if role.dispatch && role.primary {
            out.push(
                Problem::warning(
                    "agents",
                    layer,
                    format!(
                        "agent {}: dispatch: true has no effect on a primary agent",
                        role.id
                    ),
                )
                .file(file.as_deref())
                .key("dispatch"),
            );
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use super::*;
    use crate::providers::StaticKeys;

    fn dir(name: &str) -> PathBuf {
        // Under the workspace target dir, never /tmp (D13).
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/tmp/amp-core-config-check")
            .join(format!("{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join("repo/.amp/harness")).unwrap();
        dir
    }

    fn paths(dir: &Path) -> amp_mcp::ConfigPaths {
        amp_mcp::ConfigPaths::resolve(None, Some(dir.join("user")), &|_| None)
            .with_repo(Some(&dir.join("repo")))
    }

    fn keys() -> StaticKeys {
        StaticKeys {
            env: BTreeMap::from([("OPENROUTER_API_KEY".into(), "sk-or-SECRET".into())]),
            keychain: None,
        }
    }

    fn run(dir: &Path, user: Option<&str>, repo: Option<&str>) -> CheckReport {
        let p = paths(dir);
        if let Some(text) = user {
            std::fs::create_dir_all(dir.join("user")).unwrap();
            std::fs::write(p.user_file.as_ref().unwrap(), text).unwrap();
        }
        if let Some(text) = repo {
            std::fs::write(p.repo_file().unwrap(), text).unwrap();
        }
        check(&p, &|_| None, &keys(), &[])
    }

    fn find<'a>(r: &'a CheckReport, area: &str) -> Vec<&'a Problem> {
        r.problems.iter().filter(|p| p.area == area).collect()
    }

    #[test]
    fn a_clean_setup_has_no_problems() {
        let d = dir("clean");
        let r = run(&d, None, None);
        assert!(r.ok && r.problems.is_empty(), "{:?}", r.problems);
        assert_eq!(
            r.areas,
            [
                "files",
                "server",
                "models",
                "providers",
                "roles",
                "workers",
                "manager",
                "tools",
                "mcp",
                "skills",
                "agents"
            ]
        );
        let r = run(
            &d,
            Some("[server]\nurl = \"http://x/mcp\"\n[roles.reviewer]\nthinking = \"low\"\n"),
            Some("[roles.manager]\nthinking = \"high\"\n"),
        );
        assert!(r.ok && r.problems.is_empty(), "{:?}", r.problems);
        assert!(r.user_file_found && r.repo_file_found);
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn agents_and_skills_are_checked() {
        let d = dir("agents");
        let agents = d.join("user/agents");
        std::fs::create_dir_all(&agents).unwrap();
        // No [roles] and no usable fallback: the agent has no model.
        std::fs::write(
            agents.join("orphan.md"),
            "---\ndescription: x\nmode: subagent\nbased_on: nobody\n---\n\nBody.\n",
        )
        .unwrap();
        let r = run(&d, Some("[models]\nfallback = \"nobody\"\n"), None);
        let found = find(&r, "agents");
        assert!(
            found
                .iter()
                .any(|p| p.severity == Severity::Error && p.message.starts_with("agent orphan:")),
            "{found:?}"
        );
        assert!(
            found
                .iter()
                .any(|p| p.severity == Severity::Warning && p.message.contains("based_on")),
            "{found:?}"
        );
        assert_eq!(found[0].layer, Layer::User);
        assert!(found[0].file.as_deref().unwrap().ends_with("orphan.md"));
        // With a fallback that resolves, only the based_on warning stays.
        let r = run(&d, Some("[models]\nfallback = \"worker-backend\"\n"), None);
        assert!(r.ok, "{:?}", r.problems);
        assert_eq!(find(&r, "agents").len(), 1);

        // A skill that does not load is the skills area's error, not agents'.
        let skill = d.join("user/skills/broken");
        std::fs::create_dir_all(&skill).unwrap();
        std::fs::write(skill.join("SKILL.md"), "---\nname: [unclosed\n---\n").unwrap();
        let r = run(&d, None, None);
        assert_eq!(find(&r, "skills").len(), 1, "{:?}", r.problems);
        assert!(find(&r, "agents").is_empty(), "{:?}", r.problems);
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn each_layer_reports_its_own_problems() {
        let d = dir("layers");
        let r = run(
            &d,
            Some(
                "[role.x]\nmodel = \"a/b\"\n\
                 [providers.local]\napi = \"openai-completions\"\n\
                 [roles.reviewer]\nmodel = \"openrouter/no-such\"\n\
                 [workers]\nmax_parallel = \"many\"\n",
            ),
            Some("[roles.manager]\nmodel = \"nowhere/x\"\n"),
        );
        assert!(!r.ok);
        let typo = &find(&r, "files")[0];
        assert_eq!(
            (typo.severity, typo.layer),
            (Severity::Warning, Layer::User)
        );
        assert_eq!(typo.key.as_deref(), Some("role"));

        let provider = &find(&r, "providers")[0];
        assert_eq!(provider.layer, Layer::User);
        assert_eq!(provider.key.as_deref(), Some("providers.local"));
        assert!(
            provider.message.contains("base_url"),
            "{}",
            provider.message
        );
        // The provider error stops the merge, so roles can't be checked yet, but workers can.
        assert!(find(&r, "roles").is_empty());
        assert_eq!(find(&r, "workers")[0].key.as_deref(), Some("workers"));

        // With the provider fixed, both the user's and the repo's broken roles show, each in its
        // own layer and file.
        let r = run(
            &d,
            Some("[roles.reviewer]\nmodel = \"openrouter/no-such\"\n"),
            Some("[roles.manager]\nmodel = \"nowhere/x\"\n"),
        );
        let roles = find(&r, "roles");
        let manager = roles
            .iter()
            .find(|p| p.key.as_deref() == Some("roles.manager"))
            .unwrap();
        assert_eq!(manager.layer, Layer::Repo);
        assert_eq!(manager.file, paths(&d).repo_file());
        assert!(manager.message.contains("unknown provider `nowhere`"));
        let reviewer = roles
            .iter()
            .find(|p| p.key.as_deref() == Some("roles.reviewer"))
            .unwrap();
        assert_eq!(reviewer.layer, Layer::User);
        assert!(
            reviewer
                .fix
                .as_deref()
                .unwrap()
                .contains("models set reviewer")
        );
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn syntax_schema_and_repo_rules() {
        let d = dir("syntax");
        let r = run(&d, Some("[server\nurl="), None);
        let p = &find(&r, "files")[0];
        assert!(p.message.contains("not valid TOML"), "{}", p.message);
        assert_eq!(r.errors(), 1, "{:?}", r.problems);

        let r = run(&d, Some("[roles.manager]\nmodle = \"x\"\n"), None);
        let p = &find(&r, "models")[0];
        assert!(p.message.contains("modle"), "{}", p.message);

        let r = run(&d, Some(""), Some("[server]\nurl = \"https://evil/mcp\"\n"));
        let p = &find(&r, "files")[0];
        assert_eq!(p.layer, Layer::Repo);
        assert!(
            p.message.contains("`server` is not allowed"),
            "{}",
            p.message
        );

        let r = run(&d, Some("[models]\nprofile = \"nope\"\n"), Some(""));
        let p = &find(&r, "models")[0];
        assert_eq!(
            (p.layer, p.key.as_deref()),
            (Layer::User, Some("models.profile"))
        );
        let r = run(&d, Some(""), Some("[models]\nprofile = \"inherit\"\n"));
        let p = &find(&r, "models")[0];
        assert_eq!(p.layer, Layer::Repo);
        assert!(p.message.contains("inherit"), "{}", p.message);
        let _ = std::fs::remove_dir_all(&d);
    }

    /// #2233's MCP problems come through with their layer, and a broken line is never quoted.
    #[test]
    fn mcp_problems_and_no_quoted_source_lines() {
        let d = dir("mcp");
        let r = run(
            &d,
            Some("[mcp.amp]\ntype = \"http\"\nurl = \"https://x.test/mcp\"\n"),
            Some("[mcp.fs]\ntype = \"stdio\"\n"),
        );
        let mcp = find(&r, "mcp");
        let has = |layer: Layer, prefix: &str| {
            mcp.iter().any(|p| {
                p.layer == layer && p.key.as_deref().is_some_and(|k| k.starts_with(prefix))
            })
        };
        assert!(has(Layer::User, "mcp.amp"), "{mcp:?}");
        assert!(has(Layer::Repo, "mcp.fs"), "{mcp:?}");

        let r = run(&d, Some("[server]\ntoken = sk-live-SECRET\n"), Some(""));
        let json = serde_json::to_string(&r).unwrap();
        assert!(!json.contains("SECRET"), "{json}");
        assert_eq!(find(&r, "files").len(), 1, "{:?}", r.problems);
        assert!(find(&r, "mcp").is_empty(), "no duplicate: {:?}", r.problems);
        let r = run(&d, Some("[workers]\nmax_parallel = \"SECRET\"\n"), Some(""));
        let json = serde_json::to_string(&r).unwrap();
        assert!(!json.contains("SECRET"), "{json}");
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn missing_keys_are_warnings_and_never_show_the_key() {
        let d = dir("keys");
        let r = run(&d, Some("[models]\nprofile = \"anthropic\"\n"), None);
        assert!(r.ok, "warnings only: {:?}", r.problems);
        let p = &find(&r, "providers")[0];
        assert_eq!(p.severity, Severity::Warning);
        assert!(p.fix.as_deref().unwrap().contains("connect anthropic"));
        let json = serde_json::to_string(&r).unwrap();
        assert!(!json.contains("SECRET"), "{json}");
        let _ = std::fs::remove_dir_all(&d);
    }
}
