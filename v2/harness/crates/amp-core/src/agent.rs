//! Building an agent session: one rpi `AgentHarness` for one role (D5-D7).
//!
//! [`Runtime`] is what every session in a process shares: the loaded assets and role registry, the
//! models config and live providers, the materialized skills, the state directory and one
//! authenticated AMP MCP client. [`Runtime::build`] turns an [`AgentSpec`] into a ready
//! [`BuiltAgent`]. The manager (`run -p`, the M2-7 TUI) and M2-5's workers are built the same way;
//! they differ only in role, working directory, extra context, extra tools and session.
//!
//! What a session gets:
//! - **Model:** the role's provider/model/thinking from models.toml + config.toml (#2091). A role
//!   whose provider has no key fails here, with the key it needs.
//! - **Manager only (#2171):** [`crate::approval`]'s guard runs before the policy, and a refusal
//!   that needs a person in the AMP web UI (plan approval, a person's ticket) ends the turn.
//! - **Tools:** rpi-tools (read, bash, edit, write, grep, find, ls) bound to the working
//!   directory, every AMP tool under AMP's own name ([`crate::mcp_tools`]) and the spec's extra
//!   tools, filtered by the role's policy; the policy's before-tool hook enforces the rest (D6).
//! - **System prompt**, in this order:
//!   1. the role prompt;
//!   2. `<amp_server_instructions>`: what AMP's server says about using it (from discover or
//!      initialize);
//!   3. `<amp_context>`: role, AMP server, project (from `.amp.json`), working directory, plus the
//!      spec's extra lines (M2-5: ticket, worktree, branch);
//!   4. the role's preloaded skills, in full (D5);
//!   5. `<project_context>`: `AGENTS.md`/`CLAUDE.md` files, pi-style (the user's global one in
//!      the harness config dir, then the working directory and its ancestors, deepest last);
//!   6. `<available_skills>`: every skill by name, description and path, appended by rpi.
//! - **External MCP servers (#2239):** every `[mcp.<name>]` server's tools as
//!   `mcp__<server>__<tool>` next to AMP's, and each server's instructions as
//!   `<mcp_server_instructions>` before `<amp_context>`, from the process-wide
//!   [`crate::mcp_hub::McpHub`]. The session is attached to the hub, which keeps both in step as
//!   servers connect, restart or change their tools.
//! - **Session:** in memory, or a JSONL file under the state directory.
//! - **Reload (#2242):** [`Runtime::reload`] re-reads agents, skills, models and `[mcp.*]` from
//!   the same files (a CLI in another terminal changed them) and puts them in place for sessions
//!   built from then on; [`Runtime::refresh_session`] brings a live session (the chat's manager)
//!   up to date from its next run. Sessions already running (workers) keep what they were built
//!   with.
//! - **Bash environment:** the process environment minus the harness's own secrets (#2142):
//!   `AMP_TOKEN`, the `--token-env` variable and every provider's `key_env`. A worker that runs
//!   `env` never puts them in its transcript. `[secrets] pass_to_bash = [...]` in config.toml
//!   passes a named one through on purpose (off by default).
//! - **Bash never waits on a person (#2143):** every call has a default timeout (config.toml
//!   `[tools] bash_timeout_secs`, default 600 s, `AMP_HARNESS_BASH_TIMEOUT_SECS` overrides, `0`
//!   turns it off; a model-supplied `timeout` still wins) and a non-interactive environment
//!   ([`noninteractive_env`]): editors are `true`, git and ssh never prompt, `CI=1`. rpi closes
//!   stdin and puts the child in its own process group, but gives no `pre_exec` hook, so it cannot
//!   `setsid`: under a terminal a child that reads `/dev/tty` is stopped (SIGTTIN) until the
//!   timeout kills it. Without a controlling terminal (CI, the supervisor) the open fails at once.

use std::{
    collections::{BTreeMap, BTreeSet, HashMap},
    path::{Path, PathBuf},
    sync::Arc,
    time::{SystemTime, UNIX_EPOCH},
};

use amp_mcp::{AmpMcpClient, ServerSession};
use rpi_agent::{AgentEmitter, BeforeToolCall};
use rpi_harness::{
    agent_harness::AgentHarness,
    context_files::{ContextFile, format_project_context, load_project_context_files},
    session::{
        DefaultIdGenerator, InMemorySessionStorage, Session, SystemClock,
        jsonl::{JsonlV4Header, storage::JsonlSessionStorage},
        types::SessionMetadata,
    },
    skills::format_skill_invocation,
    system_prompt::compose_system_prompt,
    types::{AgentHarnessOptions, AgentHarnessResources, HarnessTool, Skill},
};
use rpi_tools::{
    BashExecution, BashPrepare, BashToolOptions, ExecutionEnv, ExecutionToolContext,
    OsExecutionEnv, is_host_private_env,
};

use crate::{
    assets::{AssetError, Assets, OverrideDirs},
    faux::{FauxScriptError, ScriptFile},
    mcp_config::McpConfig,
    mcp_hub::{HubOptions, McpHub, McpReload, WorktreeSet, with_mcp_instructions},
    mcp_tools,
    models::{ModelsConfig, ModelsError, ResolvedRole, thinking_name},
    policy::ToolPolicy,
    project::{ENV_CEILING_DIR, ProjectMarker},
    providers::{KeyLookup, ProviderSet},
    roles::{Role, RoleError, RoleRegistry},
};

/// The rpi-tools every session starts from, before the role's policy filters them.
pub const BUILTIN_TOOLS: &[&str] = &["read", "bash", "edit", "write", "grep", "find", "ls"];

/// Why a session could not be built. Every message names what to fix.
#[derive(Debug, thiserror::Error)]
pub enum AgentError {
    #[error("assets: {0}")]
    Assets(#[from] AssetError),
    #[error("roles: {0}")]
    Roles(#[from] RoleError),
    #[error("models: {0}")]
    Models(#[from] ModelsError),
    #[error("{0}")]
    Faux(#[from] FauxScriptError),
    #[error("unknown role `{0}` (try manager, worker-backend, reviewer, ...)")]
    UnknownRole(String),
    #[error("AMP tools: {0}")]
    Mcp(#[from] amp_mcp::McpError),
    #[error("{0}")]
    Io(String),
    #[error("session: {0}")]
    Harness(String),
}

/// `$XDG_STATE_HOME/amp-harness`, else `~/.local/state/amp-harness`. Sessions and the
/// materialized skills live here.
pub fn state_dir(env: &dyn Fn(&str) -> Option<String>) -> Option<PathBuf> {
    if let Some(x) = env("XDG_STATE_HOME").filter(|v| !v.is_empty()) {
        return Some(PathBuf::from(x).join("amp-harness"));
    }
    env("HOME")
        .filter(|v| !v.is_empty())
        .map(|h| PathBuf::from(h).join(".local/state/amp-harness"))
}

/// The agents and skills sessions are built from: what [`Runtime::reload`] replaces as one.
#[derive(Debug, Clone)]
pub struct Catalog {
    pub assets: Assets,
    pub roles: RoleRegistry,
    /// Every skill, written to disk so the model can read them (and their scripts) by path.
    pub skills: Vec<Skill>,
}

impl Catalog {
    /// Loads the assets with their user and repo overrides, the roles, and writes the skills
    /// under `<state>/skills/<assets hash>/`.
    fn load(paths: &amp_mcp::ConfigPaths, state_dir: &Path) -> Result<Self, AgentError> {
        let assets = Assets::load(&OverrideDirs::from_paths(paths))?;
        let roles = RoleRegistry::from_assets(&assets)?;
        let hash = assets.manifest().source_sha256.clone();
        let short = if hash.len() >= 12 { &hash[..12] } else { "dev" };
        let skills = assets.materialize_skills(&state_dir.join("skills").join(short))?;
        Ok(Self {
            assets,
            roles,
            skills,
        })
    }
}

/// Everything sessions in one process share.
pub struct Runtime {
    /// Agents, roles and skills; [`Runtime::catalog`] gives the current ones.
    catalog: std::sync::RwLock<Arc<Catalog>>,
    /// The models config and providers as loaded. After [`Runtime::reload_models`] (a provider
    /// connected in the chat, #2163) sessions use [`Runtime::current_models`] /
    /// [`Runtime::current_providers`] instead.
    pub models: ModelsConfig,
    pub providers: ProviderSet,
    /// What [`Runtime::reload_models`] put in place, if anything.
    reloaded: std::sync::RwLock<Option<(ModelsConfig, ProviderSet)>>,
    pub mcp: Arc<AmpMcpClient>,
    pub state_dir: PathBuf,
    /// The user's harness config dir (`~/.config/amp-harness`): its `AGENTS.md` is the global
    /// context file.
    pub config_dir: Option<PathBuf>,
    /// The configuration paths this runtime loaded from (#2232): the user file and dir (`--config`,
    /// `--config-dir`, their env variables, else the defaults) and the repo layer
    /// (`<repo>/.amp/harness`). Reloads and writers use these, so they read and write the same
    /// files the session started with.
    pub paths: amp_mcp::ConfigPaths,
    /// The faux providers a script was loaded into (`AMP_HARNESS_FAUX_SCRIPT`).
    pub faux_scripted: Vec<String>,
    /// `AMP_HARNESS_CEILING_DIR`: context files above it are not loaded.
    pub ceiling_dir: Option<PathBuf>,
    /// Environment variables removed from every bash call: `AMP_TOKEN`, every provider's
    /// `key_env`, every variable an MCP server reads a secret from and (via
    /// [`Runtime::with_secret_env`]) the `--token-env` variable, minus `pass_env`. Names only;
    /// values are never held here. A reload only adds names ([`Runtime::secret_env`]).
    secret_env: std::sync::RwLock<BTreeSet<String>>,
    /// `[secrets] pass_to_bash` in config.toml: secrets passed to bash on purpose.
    pub pass_env: BTreeSet<String>,
    /// The bash tool's default timeout in seconds, when the model passes none (#2143). `None`:
    /// no default (`bash_timeout_secs = 0`).
    pub bash_timeout: Option<f64>,
    /// Every configured external MCP server (#2239). Built here, started by whoever runs sessions
    /// ([`McpHub::start`]); shut it down before the process exits ([`McpHub::shutdown`]).
    pub mcp_hub: Arc<McpHub>,
    /// Role models set for this process only (the chat's `/model <role> <provider/model>`,
    /// #2245), by role: (`provider/model`, thinking). [`Runtime::reload`] keeps them over what the
    /// files say.
    role_overrides: std::sync::RwLock<BTreeMap<String, RoleOverride>>,
}

/// A role's model set for this process only ([`Runtime::set_role_override`]).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RoleOverride {
    pub model: String,
    pub thinking: Option<String>,
}

/// The source a process-only role model reports.
pub const ROLE_OVERRIDE_SOURCE: &str = "this chat (/model, until it ends)";

/// The bash tool's default timeout when nothing configures one.
pub const DEFAULT_BASH_TIMEOUT_SECS: f64 = 600.0;
/// Overrides config.toml `[tools] bash_timeout_secs`.
pub const ENV_BASH_TIMEOUT_SECS: &str = "AMP_HARNESS_BASH_TIMEOUT_SECS";
/// rpi's largest accepted timeout (its `MAX_TIMEOUT_SECONDS`).
const MAX_BASH_TIMEOUT_SECS: f64 = 2_147_483.647;

/// `[secrets]` and `[tools]` in config.toml. Other tables are ignored.
#[derive(Debug, Default, serde::Deserialize)]
struct HarnessConfigFile {
    #[serde(default)]
    secrets: SecretsSection,
    #[serde(default)]
    tools: ToolsSection,
}

#[derive(Debug, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct ToolsSection {
    /// The bash tool's default timeout in seconds; 0 = none.
    bash_timeout_secs: Option<f64>,
    /// The output cap for MCP servers that keep the default `max_output_tokens` (#2239).
    mcp_max_output_tokens: Option<u64>,
    /// Days spilled MCP output (`<state>/mcp-output/`) is kept; 0 = no age limit (#2309).
    mcp_output_keep_days: Option<u64>,
    /// Most megabytes of spilled MCP output kept, oldest removed first; 0 = no size limit.
    mcp_output_max_mb: Option<u64>,
}

#[derive(Debug, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct SecretsSection {
    /// Secret env vars to pass to bash anyway (default: none).
    #[serde(default)]
    pass_to_bash: Vec<String>,
}

/// `[secrets]` and `[tools]` from the config file the models config was loaded from.
fn harness_config_from(models: &ModelsConfig) -> Result<HarnessConfigFile, AgentError> {
    let Some(path) = models.config_file.as_ref().filter(|_| models.config_found) else {
        return Ok(HarnessConfigFile::default());
    };
    let text = std::fs::read_to_string(path)
        .map_err(|e| AgentError::Io(format!("{}: {e}", path.display())))?;
    toml::from_str(&text)
        .map_err(|e| AgentError::Io(format!("{} [secrets]/[tools]: {e}", path.display())))
}

/// Checks config.toml's `[secrets]` and `[tools]` tables the way a session reads them
/// (`config check`, #2235).
pub fn check_harness_tables(text: &str) -> Result<(), String> {
    toml::from_str::<HarnessConfigFile>(text)
        .map(|_| ())
        .map_err(|e| e.to_string())
}

/// The default MCP output cap: [`crate::mcp_hub::ENV_MAX_OUTPUT_TOKENS`], else `[tools]
/// mcp_max_output_tokens`, else `None` (each server's own, 25 000 by default). Zero or a
/// non-number is an error naming its source.
pub fn mcp_max_output_from(
    config: Option<u64>,
    env: Option<&str>,
) -> Result<Option<u64>, AgentError> {
    use crate::mcp_hub::ENV_MAX_OUTPUT_TOKENS;
    match env.map(str::trim).filter(|v| !v.is_empty()) {
        Some(v) => match v.parse::<u64>() {
            Ok(n) if n > 0 => Ok(Some(n)),
            _ => Err(AgentError::Io(format!(
                "{ENV_MAX_OUTPUT_TOKENS}: must be a whole number of tokens above 0, got {v:?}"
            ))),
        },
        None => match config {
            Some(0) => Err(AgentError::Io(
                "config.toml [tools] mcp_max_output_tokens: must be above 0".to_owned(),
            )),
            other => Ok(other),
        },
    }
}

/// `[secrets] pass_to_bash`, trimmed, empty names dropped.
fn pass_env_from(file: &HarnessConfigFile) -> BTreeSet<String> {
    file.secrets
        .pass_to_bash
        .iter()
        .map(|n| n.trim().to_owned())
        .filter(|n| !n.is_empty())
        .collect()
}

/// The bash default timeout: [`ENV_BASH_TIMEOUT_SECS`], else `[tools] bash_timeout_secs`, else
/// [`DEFAULT_BASH_TIMEOUT_SECS`]. `0` means no default. A negative, non-finite or too large value
/// is an error naming where it came from.
pub fn bash_timeout_from(
    config: Option<f64>,
    env: Option<&str>,
) -> Result<Option<f64>, AgentError> {
    let check = |secs: f64, from: &str| {
        if !secs.is_finite() || secs < 0.0 || secs > MAX_BASH_TIMEOUT_SECS {
            return Err(AgentError::Io(format!(
                "{from}: bash timeout must be 0 (none) or 0 < seconds <= {MAX_BASH_TIMEOUT_SECS}, got {secs}"
            )));
        }
        Ok((secs > 0.0).then_some(secs))
    };
    match env.map(str::trim).filter(|v| !v.is_empty()) {
        Some(v) => {
            let secs = v.parse::<f64>().map_err(|_| {
                AgentError::Io(format!(
                    "{ENV_BASH_TIMEOUT_SECS}: not a number of seconds: {v:?}"
                ))
            })?;
            check(secs, ENV_BASH_TIMEOUT_SECS)
        }
        None => check(
            config.unwrap_or(DEFAULT_BASH_TIMEOUT_SECS),
            "config.toml [tools] bash_timeout_secs",
        ),
    }
}

/// Makes `env` non-interactive for a bash call (#2143): nothing a worker runs may wait on a person.
/// Editors are `true` (`git commit` without `-m` aborts on an empty message instead of opening
/// vim), git never prompts for credentials, ssh runs in batch mode, askpass helpers are removed,
/// and package managers assume yes. `GIT_SSH_COMMAND` and `CI` are only set when unset, so a
/// user's own ssh command or CI value is kept.
pub fn noninteractive_env(env: &mut HashMap<String, String>) {
    for (k, v) in [
        ("GIT_TERMINAL_PROMPT", "0"),
        ("GIT_EDITOR", "true"),
        ("GIT_SEQUENCE_EDITOR", "true"),
        ("EDITOR", "true"),
        ("VISUAL", "true"),
        ("GCM_INTERACTIVE", "never"),
        ("SSH_ASKPASS_REQUIRE", "never"),
        ("DEBIAN_FRONTEND", "noninteractive"),
        ("npm_config_yes", "true"),
    ] {
        env.insert(k.to_owned(), v.to_owned());
    }
    for (k, v) in [("GIT_SSH_COMMAND", "ssh -o BatchMode=yes"), ("CI", "1")] {
        env.entry(k.to_owned()).or_insert_with(|| v.to_owned());
    }
    for k in ["GIT_ASKPASS", "SSH_ASKPASS"] {
        env.remove(k);
    }
}

/// What stdio MCP servers must not inherit, computed the way [`Runtime::load`] does
/// ([`crate::mcp_stdio::strip_set`] of the secret names and `[secrets] pass_to_bash`), for a hub
/// built outside a runtime (the `amp-harness mcp` CLI, #2243). An unreadable `[secrets]` table
/// strips the secret names only.
pub fn mcp_strip_names(
    models: &ModelsConfig,
    mcp: &crate::mcp_config::McpConfig,
) -> BTreeSet<String> {
    let pass_env = harness_config_from(models)
        .map(|f| pass_env_from(&f))
        .unwrap_or_default();
    let secret_env: BTreeSet<String> = secret_env_names(models)
        .into_iter()
        .chain(mcp.secret_env_names())
        .filter(|n| !pass_env.contains(n))
        .collect();
    crate::mcp_stdio::strip_set(&secret_env, &pass_env)
}

/// The variables no repo MCP server may read (#2308, [`crate::mcp_hub::McpHub::protect_env`]):
/// `AMP_TOKEN`, every provider's `key_env`, `[secrets] pass_to_bash` and what the person's own
/// MCP servers read. An unreadable `[secrets]` table protects the rest.
pub fn mcp_protected_names(
    models: &ModelsConfig,
    mcp: &crate::mcp_config::McpConfig,
) -> BTreeSet<String> {
    let pass_env = harness_config_from(models)
        .map(|f| pass_env_from(&f))
        .unwrap_or_default();
    secret_env_names(models)
        .into_iter()
        .chain(mcp.user_secret_env_names())
        .chain(pass_env)
        .collect()
}

/// The names [`Runtime::secret_env`] starts from: `AMP_TOKEN` and every provider's `key_env`.
pub fn secret_env_names(models: &ModelsConfig) -> BTreeSet<String> {
    let mut names: BTreeSet<String> = models
        .providers
        .values()
        .filter_map(|p| p.key_env.clone())
        .filter(|n| !n.trim().is_empty())
        .collect();
    names.insert(amp_mcp::ENV_TOKEN.to_owned());
    names
}

/// The environment a bash call gets: `vars` minus rpi's host-private plumbing and `secrets`.
pub fn bash_env(
    vars: impl IntoIterator<Item = (String, String)>,
    secrets: &BTreeSet<String>,
) -> HashMap<String, String> {
    vars.into_iter()
        .filter(|(k, _)| !is_host_private_env(k) && !secrets.contains(k))
        .collect()
}

/// A bash `prepare` hook that replaces the inherited environment with a filtered copy of the
/// process environment, read at call time, made non-interactive ([`noninteractive_env`]).
fn scrubbing_prepare(secrets: BTreeSet<String>) -> BashPrepare {
    let secrets = Arc::new(secrets);
    Arc::new(move |exec: &mut BashExecution| {
        let mut env = bash_env(std::env::vars(), &secrets);
        noninteractive_env(&mut env);
        // Whatever an earlier hook set explicitly still wins (rpi applies `env` last).
        env.extend(std::mem::take(&mut exec.env));
        exec.env = env;
        exec.inherit_env = false;
        Box::pin(async { Ok(()) })
    })
}

/// How [`Runtime::load`] finds things. [`RuntimeOptions::from_env`] is the real process.
pub struct RuntimeOptions<'a> {
    /// The repo root, for `<repo>/.amp/harness/` overrides.
    pub repo_root: Option<PathBuf>,
    /// Use this models config instead of loading one.
    pub models: Option<ModelsConfig>,
    pub keys: &'a dyn KeyLookup,
    pub env: &'a dyn Fn(&str) -> Option<String>,
    /// Use this state directory instead of [`state_dir`].
    pub state_dir: Option<PathBuf>,
}

impl Runtime {
    /// Loads assets (with overrides), roles, models and providers, applies a faux script from the
    /// environment, and materializes the skills under `<state>/skills/<assets hash>/`.
    pub fn load(mcp: Arc<AmpMcpClient>, opts: RuntimeOptions<'_>) -> Result<Self, AgentError> {
        let paths = amp_mcp::config_paths::current(opts.env).with_repo(opts.repo_root.as_deref());
        let config_dir = OverrideDirs::from_paths(&paths).user;
        let models = match opts.models {
            Some(m) => m,
            None => ModelsConfig::load_paths(opts.env, &paths)?,
        };
        let providers = ProviderSet::build(&models, opts.keys);
        let faux_scripted = match ScriptFile::from_env(opts.env)? {
            Some(script) => script
                .apply(&providers)
                .map_err(|message| FauxScriptError {
                    path: (opts.env)(crate::faux::ENV_FAUX_SCRIPT).unwrap_or_default(),
                    message,
                })?,
            None => Vec::new(),
        };
        let state_dir = opts
            .state_dir
            .or_else(|| state_dir(opts.env))
            .ok_or_else(|| {
                AgentError::Io("no state directory: set HOME or XDG_STATE_HOME".into())
            })?;
        let catalog = Catalog::load(&paths, &state_dir)?;
        let file = harness_config_from(&models)?;
        let pass_env = pass_env_from(&file);
        let bash_timeout = bash_timeout_from(
            file.tools.bash_timeout_secs,
            (opts.env)(ENV_BASH_TIMEOUT_SECS).as_deref(),
        )?;
        // #2233: every variable an MCP server reads a secret from is hidden from bash too. A
        // broken [mcp] table is not fatal here; the MCP hub reports it.
        let mcp_config = crate::mcp_config::McpConfig::load(&paths);
        let secret_env: BTreeSet<String> = secret_env_names(&models)
            .into_iter()
            .chain(mcp_config.secret_env_names())
            .filter(|n| !pass_env.contains(n))
            .collect();
        // #2239: the hub (not started yet). Stdio servers start in the repo root by default.
        let default_cwd = opts
            .repo_root
            .clone()
            .or_else(|| std::env::current_dir().ok())
            .unwrap_or_else(|| state_dir.clone());
        let mut hub_options = HubOptions::new(paths.clone(), state_dir.clone(), default_cwd);
        hub_options.default_max_output_tokens = mcp_max_output_from(
            file.tools.mcp_max_output_tokens,
            (opts.env)(crate::mcp_hub::ENV_MAX_OUTPUT_TOKENS).as_deref(),
        )?;
        hub_options.output_retention = Some(crate::mcp_hub::OutputRetention::from_config(
            file.tools.mcp_output_keep_days,
            file.tools.mcp_output_max_mb,
        ));
        let protected: BTreeSet<String> = secret_env_names(&models)
            .into_iter()
            .chain(mcp_config.user_secret_env_names())
            .chain(pass_env.iter().cloned())
            .collect();
        let mcp_hub = McpHub::new(mcp_config, hub_options);
        mcp_hub.set_strip(crate::mcp_stdio::strip_set(&secret_env, &pass_env));
        mcp_hub.protect_env(protected);
        Ok(Self {
            catalog: std::sync::RwLock::new(Arc::new(catalog)),
            models,
            providers,
            reloaded: std::sync::RwLock::new(None),
            mcp,
            state_dir,
            config_dir,
            paths,
            faux_scripted,
            ceiling_dir: (opts.env)(ENV_CEILING_DIR)
                .filter(|v| !v.is_empty())
                .map(PathBuf::from),
            secret_env: std::sync::RwLock::new(secret_env),
            pass_env,
            bash_timeout,
            mcp_hub: Arc::new(mcp_hub),
            role_overrides: std::sync::RwLock::new(BTreeMap::new()),
        })
    }

    /// Records `role`'s model for this process only (#2245), after checking it resolves against
    /// the current models config. It takes effect with the next [`Runtime::reload`] (the chat
    /// reloads at once), which keeps it over the files from then on.
    pub fn set_role_override(
        &self,
        role: &str,
        model_ref: &str,
        thinking: Option<&str>,
    ) -> Result<ResolvedRole, AgentError> {
        let role = crate::models::normalize_role(role).to_owned();
        let resolved = self.current_models().override_role(
            &role,
            model_ref,
            thinking,
            ROLE_OVERRIDE_SOURCE,
        )?;
        self.role_overrides
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .insert(
                role,
                RoleOverride {
                    model: model_ref.trim().to_owned(),
                    thinking: thinking.map(str::to_owned),
                },
            );
        Ok(resolved)
    }

    /// Forgets `role`'s process-only model (it was saved to a file, or reset); true when there
    /// was one. Takes effect with the next [`Runtime::reload`].
    pub fn clear_role_override(&self, role: &str) -> bool {
        self.role_overrides
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .remove(crate::models::normalize_role(role))
            .is_some()
    }

    /// The process-only role models, by role.
    pub fn role_overrides(&self) -> BTreeMap<String, RoleOverride> {
        self.role_overrides
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// Also removes `name` from bash calls (the `--token-env` variable), unless it is in
    /// `[secrets] pass_to_bash`.
    pub fn with_secret_env(self, name: Option<&str>) -> Self {
        if let Some(name) = name.map(str::trim).filter(|n| !n.is_empty()) {
            self.hide_env([name.to_owned()]);
            // #2308: the AMP token's variable is the person's own credential.
            self.mcp_hub.protect_env([name.to_owned()]);
        }
        self
    }

    /// Adds `names` to [`Runtime::secret_env`] (those in `[secrets] pass_to_bash` excepted) and
    /// tells the MCP hub, so stdio servers started from now on don't inherit them either.
    fn hide_env(&self, names: impl IntoIterator<Item = String>) {
        let mut set = self.secret_env.write().unwrap_or_else(|e| e.into_inner());
        let before = set.len();
        set.extend(names.into_iter().filter(|n| !self.pass_env.contains(n)));
        if set.len() != before {
            self.mcp_hub
                .set_strip(crate::mcp_stdio::strip_set(&set, &self.pass_env));
        }
    }

    /// The variables removed from bash calls now (names only).
    pub fn secret_env(&self) -> BTreeSet<String> {
        self.secret_env
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// The agents, roles and skills sessions are built from now.
    pub fn catalog(&self) -> Arc<Catalog> {
        Arc::clone(&self.catalog.read().unwrap_or_else(|e| e.into_inner()))
    }

    /// The models config sessions are built with now.
    pub fn current_models(&self) -> ModelsConfig {
        match &*self.reloaded.read().unwrap_or_else(|e| e.into_inner()) {
            Some((models, _)) => models.clone(),
            None => self.models.clone(),
        }
    }

    /// The providers sessions are built with now.
    pub fn current_providers(&self) -> ProviderSet {
        match &*self.reloaded.read().unwrap_or_else(|e| e.into_inner()) {
            Some((_, providers)) => providers.clone(),
            None => self.providers.clone(),
        }
    }

    /// Puts `models` in place with providers rebuilt from the keys as they are now (a key just
    /// stored, a provider just added). Sessions built from now on (workers) use them; a running
    /// harness takes the returned set with `AgentHarness::set_models`. Faux providers keep their
    /// scripts.
    pub fn reload_models(&self, models: ModelsConfig, keys: &dyn KeyLookup) -> ProviderSet {
        let providers = self.current_providers().rebuild(&models, keys);
        *self.reloaded.write().unwrap_or_else(|e| e.into_inner()) =
            Some((models, providers.clone()));
        providers
    }

    /// Re-reads everything a CLI in another terminal may have changed (#2242, the chat's
    /// `/reload`): agents and skills (built-in < user < repo), the models config (`[models]`,
    /// `[roles]`, `[providers]`, with `keys` as they are now) and `[mcp.*]`, from the files this
    /// runtime was loaded from ([`Runtime::paths`]).
    ///
    /// Agents, skills and models are all-or-nothing: if any of them fails to load (a broken
    /// agent file, an unparsable config.toml) nothing changes and the error says what to fix. A
    /// broken `[mcp]` table is not fatal; its server is left out and named in
    /// [`McpReload::problems`].
    ///
    /// Sessions built from now on (workers the manager dispatches next) use what was loaded;
    /// sessions already running keep what they were built with, except that the MCP hub keeps
    /// attached sessions' `mcp__*` tools in step from their next run ([`McpHub::reload`]). A live
    /// session that should take the rest too (the chat's manager) calls
    /// [`Runtime::refresh_session`]. New secret variable names (a provider's `key_env`, an MCP
    /// server's) are hidden from bash; none is ever un-hidden. `[secrets]`, `[tools]` and
    /// `[workers]` are read at start only.
    pub async fn reload(
        &self,
        keys: &(dyn KeyLookup + Sync),
        env: &(dyn Fn(&str) -> Option<String> + Sync),
    ) -> Result<Reloaded, AgentError> {
        // Read everything first: a failure here leaves the runtime as it was.
        let catalog = Catalog::load(&self.paths, &self.state_dir)?;
        let mut models = ModelsConfig::load_paths(env, &self.paths)?;
        // #2245: a role set for this process in the chat stays set (a model that is gone is
        // dropped from the overrides, and the role falls back to its files).
        for (role, o) in self.role_overrides() {
            if models
                .override_role(&role, &o.model, o.thinking.as_deref(), ROLE_OVERRIDE_SOURCE)
                .is_err()
            {
                self.clear_role_override(&role);
            }
        }
        let mcp_config = McpConfig::load(&self.paths);

        let old_catalog = self.catalog();
        let old_models = self.current_models();
        let mut report = Reloaded {
            agents: Changes::between(
                old_catalog.roles.roles().map(|r| (r.id.as_str(), r)),
                catalog.roles.roles().map(|r| (r.id.as_str(), r)),
            ),
            skills: Changes::between(
                old_catalog
                    .skills
                    .iter()
                    .map(|s| (s.name.as_str(), skill_key(s))),
                catalog
                    .skills
                    .iter()
                    .map(|s| (s.name.as_str(), skill_key(s))),
            ),
            providers: Changes::between(
                old_models
                    .providers
                    .iter()
                    .map(|(id, p)| (id.as_str(), format!("{p:?}"))),
                models
                    .providers
                    .iter()
                    .map(|(id, p)| (id.as_str(), format!("{p:?}"))),
            ),
            ..Reloaded::default()
        };
        for role in catalog.roles.roles() {
            let Some(old_role) = old_catalog.roles.get(&role.id) else {
                continue;
            };
            let from = describe_model(old_role.resolve_model(&old_models));
            let to = describe_model(role.resolve_model(&models));
            if from != to {
                report.models.push(ModelChange {
                    role: role.id.clone(),
                    from,
                    to,
                });
            }
        }

        *self.catalog.write().unwrap_or_else(|e| e.into_inner()) = Arc::new(catalog);
        self.hide_env(secret_env_names(&models));
        // #2308: repo MCP servers never read the person's credentials.
        self.mcp_hub.protect_env(
            secret_env_names(&models)
                .into_iter()
                .chain(mcp_config.user_secret_env_names()),
        );
        self.reload_models(models, keys);
        // Before the hub starts anything: a new stdio server must not inherit a new secret.
        self.hide_env(mcp_config.secret_env_names());
        report.mcp = self.mcp_hub.reload(mcp_config).await;
        Ok(report)
    }

    /// Brings a live session (the chat's manager) up to date after [`Runtime::reload`], from its
    /// next run, for its role as the catalog has it now:
    /// - its system prompt is rebuilt (role prompt, preloaded skills, `AGENTS.md` files re-read;
    ///   the MCP blocks kept) and it lists the current skills;
    /// - its policy takes the role's current rules in place ([`ToolPolicy::replace_policy`]: the
    ///   before-tool hook, the MCP hub and `/server` hold the same policy). That is how a new
    ///   `dispatch: true` agent becomes dispatchable; tools the new rules deny everywhere leave
    ///   its tool list, and built-in or AMP tools they now allow join it;
    /// - `replace_tools` take the place of its tools of the same names (a fresh `amp_run_ticket`
    ///   listing the new roster).
    ///
    /// The transcript stays. Its model is not changed here: the caller switches it
    /// ([`Refreshed::resolved`]).
    pub async fn refresh_session(&self, live: LiveSession<'_>) -> Result<Refreshed, AgentError> {
        let catalog = self.catalog();
        let role = catalog
            .roles
            .get(live.role)
            .cloned()
            .ok_or_else(|| AgentError::UnknownRole(live.role.to_owned()))?;
        let (exec, builtin) = self.builtin_tools(live.cwd);
        let context_files = self.context_files(&exec, live.cwd).await;
        let base = self.system_prompt_with(
            &catalog,
            &SystemPromptInput {
                role: &role,
                cwd: live.cwd,
                project: live.project,
                extra_context: live.extra_context,
                context_files: &context_files,
            },
        );
        let before = live
            .harness
            .get_system_prompt()
            .await
            .map_err(harness_error)?
            .unwrap_or_default();
        let after = self
            .mcp_hub
            .set_session_prompt(live.harness, &base)
            .await
            .map_err(AgentError::Harness)?;

        let mut resources = live.harness.get_resources().await.map_err(harness_error)?;
        let skills_before = resources.skills.clone().unwrap_or_default();
        resources.skills = Some(catalog.skills.clone());
        resources.context_files = context_files
            .iter()
            .map(|f| f.path.to_string_lossy().into_owned())
            .collect();
        live.harness
            .set_resources(resources)
            .await
            .map_err(harness_error)?;

        // The role's rules now, in force for every holder of this session's policy.
        let rules = role.tool_policy(live.cwd).policy();
        let policy_changed = rules != live.policy.policy();
        if policy_changed {
            live.policy.replace_policy(rules);
        }

        // Tools: replaced ones by name; the rest kept unless the rules now deny them
        // everywhere; built-in and AMP tools the rules now allow added. `mcp__*` tools are the
        // hub's (re-applied below).
        let replaced: BTreeSet<String> = live
            .replace_tools
            .iter()
            .map(|t| t.tool.schema().name.clone())
            .collect();
        let current = live.harness.get_tools().await.map_err(harness_error)?;
        let before_names: Vec<String> = current
            .iter()
            .map(|t| t.tool.schema().name.clone())
            .collect();
        let mut tools: Vec<HarnessTool> = current
            .into_iter()
            .filter(|t| {
                let name = &t.tool.schema().name;
                !replaced.contains(name)
                    && (name.starts_with(crate::mcp_config::TOOL_PREFIX)
                        || live.policy.exposes(name))
            })
            .collect();
        tools.extend(filter_harness_tools(live.policy, live.replace_tools));
        let mut candidates = builtin;
        if policy_changed && self.mcp.is_online() {
            candidates.extend(mcp_tools::amp_tools(&self.mcp).await?);
        }
        for tool in filter_harness_tools(live.policy, candidates) {
            let name = tool.tool.schema().name.clone();
            if !tools.iter().any(|t| t.tool.schema().name == name) {
                tools.push(tool);
            }
        }
        let names: Vec<String> = tools.iter().map(|t| t.tool.schema().name.clone()).collect();
        if names != before_names || !replaced.is_empty() {
            live.harness
                .set_tools(tools, Some(names))
                .await
                .map_err(harness_error)?;
        }
        if policy_changed {
            self.mcp_hub.reapply(live.harness).await;
        }

        Ok(Refreshed {
            prompt_changed: before != after,
            skills_changed: skills_before.iter().map(skill_key).collect::<Vec<_>>()
                != catalog.skills.iter().map(skill_key).collect::<Vec<_>>(),
            policy_changed,
            resolved: role.resolve_model(&self.current_models()).ok(),
        })
    }

    /// The role `name` names now (a copy: a reload may replace the registry).
    pub fn role(&self, name: &str) -> Result<Role, AgentError> {
        self.catalog()
            .roles
            .get(name)
            .cloned()
            .ok_or_else(|| AgentError::UnknownRole(name.to_owned()))
    }

    /// Builds one session.
    pub async fn build(&self, spec: AgentSpec) -> Result<BuiltAgent, AgentError> {
        let catalog = self.catalog();
        let role = catalog
            .roles
            .get(&spec.role)
            .cloned()
            .ok_or_else(|| AgentError::UnknownRole(spec.role.clone()))?;
        let models = self.current_models();
        let providers = self.current_providers();
        let resolved = role.resolve_model(&models)?;
        // Fail early with the key the role needs ("no key: set OPENROUTER_API_KEY ...").
        providers.for_role(&resolved)?;

        let cwd = spec.cwd.clone();
        let (exec, mut tools) = self.builtin_tools(&cwd);
        // Offline (#2178: the chat opened while AMP was unreachable): no AMP tools yet;
        // `Runtime::retarget` adds them once the connection is made.
        if self.mcp.is_online() {
            tools.extend(mcp_tools::amp_tools(&self.mcp).await?);
        }
        tools.extend(spec.extra_tools);
        // #2239: the external MCP servers' tools as they are now; the hub keeps them in step.
        let mcp_worktree = spec.mcp.worktree.clone();
        let output_key = spec
            .mcp
            .output_key
            .clone()
            .unwrap_or_else(|| format!("{}-{}", role.id, now_ms()));
        let mut mcp_view = self
            .mcp_hub
            .session_view(&role.id, mcp_worktree.as_ref(), &output_key);
        tools.extend(std::mem::take(&mut mcp_view.tools));
        let policy = Arc::new(role.tool_policy(&cwd));
        // #2240: the `mcp` permission judges each external tool by `<server>/<tool>`.
        policy.learn_mcp_subjects(
            mcp_view
                .subjects
                .iter()
                .map(|(n, s)| (n.as_str(), s.as_str())),
        );
        let tools = filter_harness_tools(&policy, tools);
        let tool_names: Vec<String> = tools.iter().map(|t| t.tool.schema().name.clone()).collect();

        let context_files = self.context_files(&exec, &cwd).await;
        let system_prompt = self.system_prompt_with(
            &catalog,
            &SystemPromptInput {
                role: &role,
                cwd: &cwd,
                project: spec.project.as_ref(),
                extra_context: spec.extra_context.as_deref(),
                context_files: &context_files,
            },
        );
        let system_prompt = with_mcp_instructions(&system_prompt, &mcp_view.instructions);

        let (session, session_file) = match &spec.session {
            SessionKind::Memory => (memory_session(), None),
            SessionKind::Jsonl { dir } => {
                let (s, path) = jsonl_session(dir, &cwd).await?;
                (s, Some(path))
            }
        };

        let repeat = crate::repeat::RepeatGuard::default();
        let options = AgentHarnessOptions {
            model: resolved.model.clone(),
            thinking_level: resolved.thinking,
            // Listed explicitly: rpi adds the <available_skills> listing only when `read` is an
            // active tool name, and an empty list would hide it.
            active_tool_names: tool_names.clone(),
            tools,
            system_prompt: Some(system_prompt.clone()),
            resources: AgentHarnessResources {
                context_files: context_files
                    .iter()
                    .map(|f| f.path.to_string_lossy().into_owned())
                    .collect(),
                prompt_templates: None,
                skills: Some(catalog.skills.clone()),
            },
            session,
            models: providers.all(),
            agent_emitter: spec.emitter,
            // The manager (#2171): the approval guard before the policy (once AMP refused a step
            // that needs a person, every further call ends the turn, denied or not); a refusal
            // ends its turn. Workers report a refusal in their final message.
            // Every role (#2225): the repeat guard first, so a model stuck on one call (even one
            // the policy denies each time) is stopped; it nudges the model in the result first.
            before_tool_call: Some(repeat.before_tool_call_then(Some(if role.primary {
                let guard = Arc::new(crate::approval::ManagerGuard);
                guard.before_tool_call_then(Some(
                    policy.before_tool_call_then(spec.before_tool_call),
                ))
            } else {
                policy.before_tool_call_then(spec.before_tool_call)
            }))),
            after_tool_call: Some(
                repeat
                    .after_tool_call_then(role.primary.then(crate::approval::end_turn_on_refusal)),
            ),
            ..Default::default()
        };
        let harness = Arc::new(
            AgentHarness::create(options)
                .await
                .map_err(|e| AgentError::Harness(format!("{e}")))?,
        );
        // Attached even with no server configured yet: a reload may add one (#2242).
        {
            self.mcp_hub
                .attach(
                    &harness,
                    &role.id,
                    Arc::clone(&policy),
                    output_key,
                    mcp_worktree,
                    mcp_view.signature,
                )
                .await;
        }
        Ok(BuiltAgent {
            harness,
            role,
            resolved,
            system_prompt,
            tool_names,
            context_files: context_files.into_iter().map(|f| f.path).collect(),
            session_file,
            policy,
        })
    }

    /// The rpi-tools ([`BUILTIN_TOOLS`]) bound to `cwd`, and the execution env they share.
    fn builtin_tools(&self, cwd: &Path) -> (Arc<dyn ExecutionEnv>, Vec<HarnessTool>) {
        let env = Arc::new(OsExecutionEnv::with_cwd(cwd.to_path_buf()));
        let exec: Arc<dyn ExecutionEnv> = env.clone();
        let ctx = ExecutionToolContext::new(exec.clone(), Some(env));
        let tools = vec![
            HarnessTool::new(rpi_tools::create_read_tool(&ctx, None)),
            HarnessTool::new(rpi_tools::create_bash_tool(
                &ctx,
                Some(BashToolOptions {
                    prepare: Some(scrubbing_prepare(self.secret_env())),
                    default_timeout: self.bash_timeout,
                    ..Default::default()
                }),
            )),
            HarnessTool::new(rpi_tools::create_edit_tool(&ctx)),
            HarnessTool::new(rpi_tools::create_write_tool(&ctx)),
            HarnessTool::new(rpi_tools::create_grep_tool(&ctx, None)),
            HarnessTool::new(rpi_tools::create_find_tool(&ctx, None)),
            HarnessTool::new(rpi_tools::create_ls_tool(&ctx, None)),
        ];
        (exec, tools)
    }

    /// pi's context files, minus any above `AMP_HARNESS_CEILING_DIR`.
    async fn context_files(&self, env: &Arc<dyn ExecutionEnv>, cwd: &Path) -> Vec<ContextFile> {
        // A directory that cannot exist stands in for "no global dir".
        let global = self
            .config_dir
            .clone()
            .unwrap_or_else(|| PathBuf::from("/nonexistent/amp-harness"));
        let files = load_project_context_files(env, cwd, &global).await;
        files
            .into_iter()
            .filter(|f| match &self.ceiling_dir {
                Some(ceiling) => f.path.starts_with(ceiling) || f.path.starts_with(&global),
                None => true,
            })
            .collect()
    }

    /// The system prompt rpi gets as its base (it appends `<available_skills>` itself).
    pub fn system_prompt(&self, input: &SystemPromptInput<'_>) -> String {
        self.system_prompt_with(&self.catalog(), input)
    }

    fn system_prompt_with(&self, catalog: &Catalog, input: &SystemPromptInput<'_>) -> String {
        let session = self.mcp.session();
        let mut append = Vec::new();

        if let Some(block) = server_instructions_block(&session) {
            append.push(block);
        }
        append.push(amp_context(
            input.role,
            &session,
            input.project,
            input.cwd,
            input.extra_context,
        ));

        let preloaded: Vec<String> = input
            .role
            .skills
            .iter()
            .filter_map(|name| catalog.skills.iter().find(|s| &s.name == name))
            .map(|s| format_skill_invocation(s, None))
            .collect();
        if !preloaded.is_empty() {
            append.push(format!(
                "<preloaded_skills>\nThese skills are already loaded; do not read them again.\n\n{}\n</preloaded_skills>",
                preloaded.join("\n\n")
            ));
        }

        let context = format_project_context(input.context_files);
        compose_system_prompt(
            Some(&input.role.prompt),
            &[],
            Some(&context),
            Some(&append.join("\n\n")),
        )
    }

    /// After [`Runtime::mcp`] was pointed at another AMP server (`/server`, #2164): gives
    /// `harness` the new server's AMP tools in place of `old_tools` (the AMP tool names before
    /// the switch), filtered by `policy` as [`Runtime::build`] does, and rewrites the server
    /// details in its system prompt ([`retarget_system_prompt`]). Both apply from the next run;
    /// the transcript stays. Built-in and extra tools (`amp_run_ticket`, `amp_session_usage`) are
    /// kept as they are.
    pub async fn retarget(
        &self,
        harness: &AgentHarness,
        policy: &ToolPolicy,
        old_tools: &BTreeSet<String>,
    ) -> Result<Retargeted, AgentError> {
        let new = filter_harness_tools(policy, mcp_tools::amp_tools(&self.mcp).await?);
        let new_names: BTreeSet<String> =
            new.iter().map(|t| t.tool.schema().name.clone()).collect();
        let current = harness
            .get_tools()
            .await
            .map_err(|e| AgentError::Harness(e.to_string()))?;
        let mut tools: Vec<HarnessTool> = current
            .into_iter()
            .filter(|t| !old_tools.contains(&t.tool.schema().name))
            .collect();
        tools.extend(new);
        let names: Vec<String> = tools.iter().map(|t| t.tool.schema().name.clone()).collect();
        harness
            .set_tools(tools, Some(names.clone()))
            .await
            .map_err(|e| AgentError::Harness(e.to_string()))?;
        if let Some(prompt) = harness
            .get_system_prompt()
            .await
            .map_err(|e| AgentError::Harness(e.to_string()))?
        {
            let prompt = retarget_system_prompt(&prompt, &self.mcp.session());
            harness
                .set_system_prompt(Some(prompt))
                .await
                .map_err(|e| AgentError::Harness(e.to_string()))?;
        }
        Ok(Retargeted {
            added: new_names.difference(old_tools).cloned().collect(),
            removed: old_tools.difference(&new_names).cloned().collect(),
            amp_tools: new_names.len(),
            tool_names: names,
        })
    }
}

/// What [`Runtime::retarget`] changed.
#[derive(Debug, Clone, Default)]
pub struct Retargeted {
    /// AMP tools the new server has and the old one did not (after the policy).
    pub added: Vec<String>,
    /// AMP tools that are gone.
    pub removed: Vec<String>,
    /// How many AMP tools the session has now.
    pub amp_tools: usize,
    /// Every tool name the session has now.
    pub tool_names: Vec<String>,
}

/// What changed in one kind of thing (agents, skills, providers) across a reload, by id.
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Serialize)]
pub struct Changes {
    pub added: Vec<String>,
    pub removed: Vec<String>,
    pub changed: Vec<String>,
}

impl Changes {
    fn between<'a, T: PartialEq>(
        old: impl Iterator<Item = (&'a str, T)>,
        new: impl Iterator<Item = (&'a str, T)>,
    ) -> Self {
        let old: std::collections::BTreeMap<&str, T> = old.collect();
        let new: std::collections::BTreeMap<&str, T> = new.collect();
        let mut out = Self::default();
        for (id, v) in &new {
            match old.get(id) {
                None => out.added.push((*id).to_owned()),
                Some(o) if o != v => out.changed.push((*id).to_owned()),
                Some(_) => {}
            }
        }
        out.removed = old
            .keys()
            .filter(|id| !new.contains_key(*id))
            .map(|id| (*id).to_owned())
            .collect();
        out
    }

    pub fn is_empty(&self) -> bool {
        self.added.is_empty() && self.removed.is_empty() && self.changed.is_empty()
    }

    /// `+a +b ~c -d`, or `None` when nothing changed.
    pub fn line(&self) -> Option<String> {
        if self.is_empty() {
            return None;
        }
        let marked = |mark: &str, ids: &[String]| {
            ids.iter().map(|i| format!("{mark}{i}")).collect::<Vec<_>>()
        };
        let mut parts = marked("+", &self.added);
        parts.extend(marked("~", &self.changed));
        parts.extend(marked("-", &self.removed));
        Some(parts.join(" "))
    }
}

/// A role whose model a reload changed: `provider/model (thinking level)`, or `None` when it
/// doesn't resolve (its error in [`ModelChange::to`]'s place is not kept: `models list` says why).
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct ModelChange {
    pub role: String,
    pub from: Option<String>,
    pub to: Option<String>,
}

fn harness_error(e: impl std::fmt::Display) -> AgentError {
    AgentError::Harness(e.to_string())
}

fn describe_model(resolved: Result<ResolvedRole, ModelsError>) -> Option<String> {
    resolved
        .ok()
        .map(|r| format!("{} (thinking {})", r.model_ref(), thinking_name(r.thinking)))
}

/// What a skill's listing and content are, to tell a changed one.
fn skill_key(s: &Skill) -> (String, String, String, Option<bool>) {
    (
        s.name.clone(),
        s.description.clone(),
        s.content.clone(),
        s.disable_model_invocation,
    )
}

/// What [`Runtime::reload`] changed.
#[derive(Debug, Clone, Default, serde::Serialize)]
pub struct Reloaded {
    pub agents: Changes,
    pub skills: Changes,
    pub providers: Changes,
    /// Roles (that existed before) whose resolved model or thinking changed.
    pub models: Vec<ModelChange>,
    pub mcp: McpReload,
}

impl Reloaded {
    /// Nothing changed.
    pub fn is_empty(&self) -> bool {
        self.agents.is_empty()
            && self.skills.is_empty()
            && self.providers.is_empty()
            && self.models.is_empty()
            && self.mcp.is_empty()
    }

    /// The change for `role` (an id), if its model changed.
    pub fn model_change(&self, role: &str) -> Option<&ModelChange> {
        self.models.iter().find(|m| m.role == role)
    }

    /// One line per kind of change, for a transcript or a terminal.
    pub fn lines(&self) -> Vec<String> {
        let mut out = Vec::new();
        if let Some(l) = self.agents.line() {
            out.push(format!("agents: {l}"));
        }
        if let Some(l) = self.skills.line() {
            out.push(format!("skills: {l}"));
        }
        if let Some(l) = self.providers.line() {
            out.push(format!("providers: {l}"));
        }
        for m in &self.models {
            let show = |v: &Option<String>| v.clone().unwrap_or_else(|| "unresolved".to_owned());
            out.push(format!(
                "model {}: {} → {}",
                m.role,
                show(&m.from),
                show(&m.to)
            ));
        }
        let mcp = Changes {
            added: self.mcp.added.clone(),
            removed: self.mcp.removed.clone(),
            changed: self.mcp.changed.clone(),
        };
        if let Some(l) = mcp.line() {
            out.push(format!("mcp: {l}"));
        }
        if !self.mcp.retried.is_empty() {
            out.push(format!("mcp: retrying {}", self.mcp.retried.join(", ")));
        }
        for name in &self.mcp.untrusted {
            out.push(format!(
                "mcp: {name} is not trusted yet (`amp-harness mcp trust {name}`, then /reload)"
            ));
        }
        for p in &self.mcp.problems {
            out.push(format!("mcp problem: {p}"));
        }
        out
    }
}

/// A live session for [`Runtime::refresh_session`]: what it was built with.
pub struct LiveSession<'a> {
    pub harness: &'a Arc<AgentHarness>,
    /// The role id it was built for.
    pub role: &'a str,
    pub cwd: &'a Path,
    pub project: Option<&'a ProjectMarker>,
    pub extra_context: Option<&'a str>,
    /// The policy it was built with (its before-tool hook's): it takes the role's current rules.
    pub policy: &'a ToolPolicy,
    /// Tools to put in place of those with the same names (a fresh `amp_run_ticket`).
    pub replace_tools: Vec<HarnessTool>,
}

/// What [`Runtime::refresh_session`] did.
#[derive(Debug, Clone, Default)]
pub struct Refreshed {
    /// The system prompt is different now.
    pub prompt_changed: bool,
    /// The skills listed are different now.
    pub skills_changed: bool,
    /// The role's permissions changed; the live session enforces the new ones now.
    pub policy_changed: bool,
    /// The role's model as configured now (`None` when it no longer resolves).
    pub resolved: Option<ResolvedRole>,
}

/// `<amp_server_instructions server="amp 1.2">…</amp_server_instructions>` for `session`, or
/// `None` when the server gave no instructions.
fn server_instructions_block(session: &ServerSession) -> Option<String> {
    let text = session
        .instructions
        .as_deref()
        .filter(|t| !t.trim().is_empty())?;
    Some(format!(
        "<amp_server_instructions server=\"{} {}\">\n{}\n</amp_server_instructions>",
        session.server_name.as_deref().unwrap_or("amp"),
        session.server_version.as_deref().unwrap_or(""),
        text.trim()
    ))
}

const INSTRUCTIONS_OPEN: &str = "<amp_server_instructions";
const INSTRUCTIONS_CLOSE: &str = "</amp_server_instructions>";
const CONTEXT_OPEN: &str = "<amp_context>";

/// A system prompt built by [`Runtime::system_prompt`] for one server, rewritten for `session`'s:
/// its `<amp_server_instructions>` block is replaced (added before `<amp_context>`, or removed),
/// and the `amp_server:` line in `<amp_context>` names the new URL. Everything else is kept.
pub fn retarget_system_prompt(prompt: &str, session: &ServerSession) -> String {
    let block = server_instructions_block(session);
    let mut out = prompt.to_owned();
    // The block as Runtime::system_prompt writes it: the first one, before <amp_context>.
    let context_at = out.find(CONTEXT_OPEN);
    let existing = out.find(INSTRUCTIONS_OPEN).and_then(|start| {
        let end = out[start..].find(INSTRUCTIONS_CLOSE)? + start + INSTRUCTIONS_CLOSE.len();
        context_at.is_none_or(|c| end <= c).then_some((start, end))
    });
    match (existing, block) {
        (Some((start, end)), Some(block)) => out.replace_range(start..end, &block),
        (Some((start, end)), None) => {
            // Drop the blank line that separated it from what follows.
            let end = if out[end..].starts_with("\n\n") {
                end + 2
            } else {
                end
            };
            out.replace_range(start..end, "");
        }
        (None, Some(block)) => {
            if let Some(at) = context_at {
                out.insert_str(at, &format!("{block}\n\n"));
            }
        }
        (None, None) => {}
    }
    replace_context_line(
        &mut out,
        SERVER_KEY,
        &format!("amp_server: {}", session.url),
    );
    // A prompt from before #2205 has no web UI line: add it after amp_server.
    if !replace_context_line(&mut out, WEB_UI_KEY, &web_ui_line(session)) {
        insert_context_line_after(&mut out, SERVER_KEY, &web_ui_line(session));
    }
    out
}

/// The `<amp_context>` block's bounds in `prompt`.
fn context_bounds(prompt: &str) -> Option<(usize, usize)> {
    let start = prompt.find(CONTEXT_OPEN)?;
    let end = prompt[start..]
        .find("</amp_context>")
        .map_or(prompt.len(), |i| start + i);
    Some((start, end))
}

/// The line starting with `key` (`"\nname: "`) inside `<amp_context>`: (start, end) without
/// the newlines.
fn context_line(prompt: &str, key: &str) -> Option<(usize, usize)> {
    let (start, end) = context_bounds(prompt)?;
    let line_start = start + prompt[start..end].find(key)? + 1;
    let line_end = prompt[line_start..]
        .find('\n')
        .map_or(prompt.len(), |j| line_start + j);
    Some((line_start, line_end))
}

/// Replaces the `<amp_context>` line starting with `key`; false when there is none.
fn replace_context_line(prompt: &mut String, key: &str, line: &str) -> bool {
    match context_line(prompt, key) {
        Some((start, end)) => {
            prompt.replace_range(start..end, line);
            true
        }
        None => false,
    }
}

fn insert_context_line_after(prompt: &mut String, key: &str, line: &str) {
    if let Some((_, end)) = context_line(prompt, key) {
        prompt.insert_str(end, &format!("\n{line}"));
    }
}

/// What [`Runtime::system_prompt`] needs.
pub struct SystemPromptInput<'a> {
    pub role: &'a Role,
    pub cwd: &'a Path,
    pub project: Option<&'a ProjectMarker>,
    pub extra_context: Option<&'a str>,
    pub context_files: &'a [ContextFile],
}

/// The `<amp_context>` line naming the web UI (#2205): links people click (the plan to approve, a
/// ticket, an artifact) go there, never to the MCP server's origin.
pub fn web_ui_line(session: &ServerSession) -> String {
    let web = session.web_base();
    format!(
        "amp_web_ui: {web} (AMP's web UI: every link you give a person is on this origin, e.g. \
         {web}/project/<id>/plan to approve a plan; never build links from amp_server)"
    )
}

const WEB_UI_KEY: &str = "\namp_web_ui: ";
const SERVER_KEY: &str = "\namp_server: ";

fn amp_context(
    role: &Role,
    session: &ServerSession,
    project: Option<&ProjectMarker>,
    cwd: &Path,
    extra: Option<&str>,
) -> String {
    let mut lines = vec![
        "<amp_context>".to_owned(),
        format!("role: {}", role.id),
        format!("amp_server: {}", session.url),
        web_ui_line(session),
    ];
    match project {
        Some(p) => {
            lines.push(format!("project_id: {}", p.project_id));
            if let Some(name) = &p.project_name {
                lines.push(format!("project_name: {name}"));
            }
            if let Some(code) = &p.project_code {
                lines.push(format!("project_code: {code}"));
            }
            lines.push(format!("project_marker: {}", p.path.display()));
        }
        None => lines.push(
            "project_id: none (no .amp.json found; ask which project, or use amp_list_projects)"
                .to_owned(),
        ),
    }
    lines.push(format!("working_directory: {}", cwd.display()));
    if let Some(extra) = extra.map(str::trim).filter(|e| !e.is_empty()) {
        lines.push(extra.to_owned());
    }
    // #2250: what the per-request <current_time> stamp is. Fixed text, so the prompt stays cached.
    lines.push(crate::request_time::CONTEXT_LINE.to_owned());
    lines.push("</amp_context>".to_owned());
    lines.join("\n")
}

/// Drops the tools `policy` can never allow (see [`ToolPolicy::exposes`]).
pub fn filter_harness_tools(policy: &ToolPolicy, tools: Vec<HarnessTool>) -> Vec<HarnessTool> {
    tools
        .into_iter()
        .filter(|t| policy.exposes(&t.tool.schema().name))
        .collect()
}

/// Where a session's transcript goes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SessionKind {
    Memory,
    /// A new JSONL file in `dir` (D7: workers use `<state>/sessions/<ticket>/`).
    Jsonl {
        dir: PathBuf,
    },
}

/// One session to build.
pub struct AgentSpec {
    /// Any name [`RoleRegistry::get`] accepts.
    pub role: String,
    /// The working directory (the repo, or M2-5's worktree): rpi-tools and the policy's paths are
    /// relative to it.
    pub cwd: PathBuf,
    pub project: Option<ProjectMarker>,
    /// Extra `<amp_context>` lines (M2-5: `ticket: #N`, `worktree: ...`, `branch: ...`).
    pub extra_context: Option<String>,
    /// Extra tools (M2-5: `amp_run_ticket` for the manager). Filtered by the policy too.
    pub extra_tools: Vec<HarnessTool>,
    pub session: SessionKind,
    pub emitter: Option<Arc<dyn AgentEmitter>>,
    /// Runs after the policy allows a call (M2-6's run reporting).
    pub before_tool_call: Option<BeforeToolCall>,
    /// External MCP servers for this session (#2239).
    pub mcp: McpSessionSpec,
}

/// What a session needs from the MCP hub besides its role (#2239).
#[derive(Debug, Clone, Default)]
pub struct McpSessionSpec {
    /// Names the directory oversized tool output is saved in (`<state>/mcp-output/<key>/`): the
    /// ticket id for a worker. Default: `<role>-<unix ms>`.
    pub output_key: Option<String>,
    /// The worker's worktree-scoped server instances ([`McpHub::start_worktree`]).
    pub worktree: Option<Arc<WorktreeSet>>,
}

impl AgentSpec {
    pub fn new(role: impl Into<String>, cwd: impl Into<PathBuf>) -> Self {
        Self {
            role: role.into(),
            cwd: cwd.into(),
            project: None,
            extra_context: None,
            extra_tools: Vec::new(),
            session: SessionKind::Memory,
            emitter: None,
            before_tool_call: None,
            mcp: McpSessionSpec::default(),
        }
    }
}

/// A ready session.
pub struct BuiltAgent {
    pub harness: Arc<AgentHarness>,
    pub role: Role,
    pub resolved: ResolvedRole,
    /// The base system prompt (rpi appends `<available_skills>`).
    pub system_prompt: String,
    pub tool_names: Vec<String>,
    pub context_files: Vec<PathBuf>,
    pub session_file: Option<PathBuf>,
    pub policy: Arc<ToolPolicy>,
}

impl BuiltAgent {
    /// The system prompt as the model receives it: the base plus rpi's `<available_skills>`
    /// listing (which rpi adds only when `read` is an active tool).
    pub fn full_system_prompt(&self, skills: &[Skill]) -> String {
        let listed: &[Skill] = if self.tool_names.iter().any(|n| n == "read") {
            skills
        } else {
            &[]
        };
        compose_system_prompt(Some(&self.system_prompt), listed, None, None)
    }
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// A fresh in-memory session.
pub fn memory_session() -> Session {
    let ids = Arc::new(DefaultIdGenerator::new());
    let id = rpi_harness::session::types::IdGenerator::next(ids.as_ref());
    Session::new(
        Arc::new(InMemorySessionStorage::new(
            SessionMetadata {
                id,
                created_at: now_ms(),
                parent_session_id: None,
            },
            Arc::new(SystemClock),
            ids.clone(),
        )),
        Some(ids),
    )
}

/// A new JSONL session file in `dir` (created), named `<unix ms>-<session id>.jsonl`.
pub async fn jsonl_session(dir: &Path, cwd: &Path) -> Result<(Session, PathBuf), AgentError> {
    std::fs::create_dir_all(dir)
        .map_err(|e| AgentError::Io(format!("create {}: {e}", dir.display())))?;
    let ids = Arc::new(DefaultIdGenerator::new());
    let id = rpi_harness::session::types::IdGenerator::next(ids.as_ref());
    let created = now_ms();
    let path = dir.join(format!("{created}-{id}.jsonl"));
    let header = JsonlV4Header::new(
        id,
        created,
        cwd.to_string_lossy().into_owned(),
        None,
        None,
        None,
    );
    let fs = Arc::new(OsExecutionEnv::with_cwd(dir.to_path_buf()));
    let storage = JsonlSessionStorage::create(
        fs,
        &path.to_string_lossy(),
        header,
        Arc::new(SystemClock),
        ids.clone(),
    )
    .await
    .map_err(|e| AgentError::Harness(format!("{}: {e}", path.display())))?;
    Ok((Session::new(Arc::new(storage), Some(ids)), path))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn session(url: &str, instructions: Option<&str>) -> ServerSession {
        ServerSession {
            url: url.to_owned(),
            mode: amp_mcp::ProtocolMode::Auto,
            protocol_version: amp_mcp::rmcp::model::ProtocolVersion::V_2025_11_25,
            era: amp_mcp::Era::Legacy,
            fell_back: false,
            server_name: Some("amp".to_owned()),
            server_version: Some("2".to_owned()),
            instructions: instructions.map(str::to_owned),
            auth: "none".to_owned(),
            web_url: None,
        }
    }

    /// #2205: the web UI line names the configured web UI, else the MCP URL's origin.
    #[test]
    fn the_context_names_the_web_ui_origin() {
        let mut compose = session("http://localhost:18000/mcp", None);
        assert!(web_ui_line(&compose).starts_with("amp_web_ui: http://localhost:18000 ("));
        compose.web_url = Some("http://localhost:5173/".to_owned());
        let line = web_ui_line(&compose);
        assert!(
            line.starts_with(
                "amp_web_ui: http://localhost:5173 (AMP's web UI: every link you give a person \
                 is on this origin, e.g. http://localhost:5173/project/<id>/plan"
            ),
            "{line}"
        );
        assert!(!line.contains("18000"), "{line}");

        // A prompt from before #2205 gets the line after amp_server; a second retarget replaces it.
        let old = "<amp_context>\nrole: amp-manager\namp_server: http://localhost:18000/mcp\nproject_id: 2\n</amp_context>";
        let new = retarget_system_prompt(old, &compose);
        assert_eq!(
            new,
            format!(
                "<amp_context>\nrole: amp-manager\namp_server: http://localhost:18000/mcp\n{line}\nproject_id: 2\n</amp_context>"
            )
        );
        assert_eq!(retarget_system_prompt(&new, &compose), new);
    }

    /// #2164: `/server` rewrites the server details and keeps everything else.
    #[test]
    fn retargeting_the_system_prompt_swaps_only_the_server_details() {
        let old = session("http://old:1/mcp", Some("old rules"));
        let block = server_instructions_block(&old).unwrap();
        let prompt = format!(
            "ROLE\n\n{block}\n\n<amp_context>\nrole: amp-manager\namp_server: http://old:1/mcp\n{}\nproject_id: 2\n</amp_context>\n\nTAIL",
            web_ui_line(&old)
        );
        let new = retarget_system_prompt(&prompt, &session("https://new/mcp", Some("new rules")));
        assert!(
            new.contains("new rules") && !new.contains("old rules"),
            "{new}"
        );
        assert!(
            new.contains("\namp_server: https://new/mcp\namp_web_ui: https://new (AMP's web UI"),
            "{new}"
        );
        assert!(
            new.contains("never build links from amp_server)\nproject_id: 2\n"),
            "{new}"
        );
        assert!(new.starts_with("ROLE\n\n<amp_server_instructions") && new.ends_with("TAIL"));

        // No instructions on the new server: the block goes, with its blank line.
        let none = retarget_system_prompt(&prompt, &session("https://new/mcp", None));
        assert!(!none.contains("amp_server_instructions"), "{none}");
        assert!(none.starts_with("ROLE\n\n<amp_context>"), "{none}");

        // And back: the block is added before <amp_context>.
        let back = retarget_system_prompt(&none, &old);
        assert_eq!(back, prompt);
    }

    #[test]
    fn secrets_are_amp_token_and_every_provider_key_env() {
        let models = ModelsConfig::load_with_env(&|_| None, None).unwrap();
        let names = secret_env_names(&models);
        for n in ["AMP_TOKEN", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY"] {
            assert!(names.contains(n), "{n} missing from {names:?}");
        }
    }

    #[test]
    fn bash_timeout_defaults_to_600_and_env_beats_config() {
        assert_eq!(bash_timeout_from(None, None).unwrap(), Some(600.0));
        assert_eq!(bash_timeout_from(Some(30.0), None).unwrap(), Some(30.0));
        assert_eq!(
            bash_timeout_from(Some(30.0), Some(" 5 ")).unwrap(),
            Some(5.0)
        );
        assert_eq!(bash_timeout_from(Some(30.0), Some("")).unwrap(), Some(30.0));
        // 0 turns the default off.
        assert_eq!(bash_timeout_from(Some(0.0), None).unwrap(), None);
        assert_eq!(bash_timeout_from(None, Some("0")).unwrap(), None);
        for bad in ["-1", "nan", "inf", "ten", "9999999999"] {
            let err = bash_timeout_from(None, Some(bad)).unwrap_err().to_string();
            assert!(err.contains(ENV_BASH_TIMEOUT_SECS), "{bad}: {err}");
        }
        let err = bash_timeout_from(Some(-3.0), None).unwrap_err().to_string();
        assert!(err.contains("bash_timeout_secs"), "{err}");
    }

    #[test]
    fn tools_table_parses_and_rejects_unknown_keys() {
        let f: HarnessConfigFile =
            toml::from_str("[models]\nprofile = \"x\"\n[tools]\nbash_timeout_secs = 12.5\n")
                .unwrap();
        assert_eq!(f.tools.bash_timeout_secs, Some(12.5));
        assert!(toml::from_str::<HarnessConfigFile>("[tools]\nbash_timeout = 1\n").is_err());
        let f: HarnessConfigFile = toml::from_str("").unwrap();
        assert_eq!(f.tools.bash_timeout_secs, None);
    }

    #[test]
    fn noninteractive_env_overrides_editors_and_keeps_user_ssh_and_ci() {
        let mut env: HashMap<String, String> = [
            ("EDITOR", "vim"),
            ("GIT_ASKPASS", "/usr/bin/askpass"),
            ("SSH_ASKPASS", "/usr/bin/ssh-askpass"),
            ("GIT_SSH_COMMAND", "ssh -i key"),
            ("PATH", "/bin"),
        ]
        .into_iter()
        .map(|(k, v)| (k.to_owned(), v.to_owned()))
        .collect();
        noninteractive_env(&mut env);
        for (k, v) in [
            ("EDITOR", "true"),
            ("VISUAL", "true"),
            ("GIT_EDITOR", "true"),
            ("GIT_TERMINAL_PROMPT", "0"),
            ("DEBIAN_FRONTEND", "noninteractive"),
            ("npm_config_yes", "true"),
            ("CI", "1"),
            ("GIT_SSH_COMMAND", "ssh -i key"),
            ("PATH", "/bin"),
        ] {
            assert_eq!(env.get(k).map(String::as_str), Some(v), "{k}");
        }
        assert!(!env.contains_key("GIT_ASKPASS") && !env.contains_key("SSH_ASKPASS"));
        let mut env = HashMap::from([("CI".to_owned(), "true".to_owned())]);
        noninteractive_env(&mut env);
        assert_eq!(env["CI"], "true");
        assert_eq!(env["GIT_SSH_COMMAND"], "ssh -o BatchMode=yes");
    }

    #[test]
    fn bash_env_drops_secrets_and_host_plumbing_and_keeps_the_rest() {
        let secrets: BTreeSet<String> = ["AMP_TOKEN", "OPENROUTER_API_KEY"]
            .into_iter()
            .map(String::from)
            .collect();
        let vars = [
            ("AMP_TOKEN", "dummy-amp"),
            ("OPENROUTER_API_KEY", "dummy-or"),
            ("RPI_SESSION_ID", "dummy-session"),
            ("PATH", "/usr/bin"),
            ("AMP_MCP_URL", "http://x"),
        ]
        .map(|(k, v)| (k.to_owned(), v.to_owned()));
        let env = bash_env(vars, &secrets);
        let mut keys: Vec<&str> = env.keys().map(String::as_str).collect();
        keys.sort_unstable();
        assert_eq!(keys, ["AMP_MCP_URL", "PATH"]);
    }

    #[test]
    fn state_dir_prefers_xdg_state_home() {
        let env = |k: &str| match k {
            "XDG_STATE_HOME" => Some("/x/state".to_owned()),
            "HOME" => Some("/home/u".to_owned()),
            _ => None,
        };
        assert_eq!(state_dir(&env), Some(PathBuf::from("/x/state/amp-harness")));
        let env = |k: &str| (k == "HOME").then(|| "/home/u".to_owned());
        assert_eq!(
            state_dir(&env),
            Some(PathBuf::from("/home/u/.local/state/amp-harness"))
        );
        assert_eq!(state_dir(&|_| None), None);
    }

    #[test]
    fn amp_context_names_the_project_or_says_there_is_none() {
        let assets = Assets::embedded().unwrap();
        let roles = RoleRegistry::from_assets(&assets).unwrap();
        let manager = roles.manager();
        let marker = ProjectMarker {
            path: PathBuf::from("/repo/.amp.json"),
            project_id: 2,
            project_name: Some("AMP v2".into()),
            project_code: None,
            amp_api: None,
        };
        let mut compose = session("http://localhost:8000/mcp", None);
        compose.web_url = Some("http://localhost:5173".into());
        let text = amp_context(
            manager,
            &compose,
            Some(&marker),
            Path::new("/repo"),
            Some("ticket: #7"),
        );
        assert_eq!(
            text,
            format!(
                "<amp_context>\nrole: amp-manager\namp_server: http://localhost:8000/mcp\n{}\nproject_id: 2\nproject_name: AMP v2\nproject_marker: /repo/.amp.json\nworking_directory: /repo\nticket: #7\n{}\n</amp_context>",
                web_ui_line(&compose),
                crate::request_time::CONTEXT_LINE
            )
        );
        assert!(
            text.contains("\namp_web_ui: http://localhost:5173 ("),
            "{text}"
        );
        let text = amp_context(manager, &session("u", None), None, Path::new("/r"), None);
        assert!(text.contains("project_id: none"), "{text}");
    }
}
