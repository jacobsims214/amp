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
//! - **Session:** in memory, or a JSONL file under the state directory.
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
    collections::{BTreeSet, HashMap},
    path::{Path, PathBuf},
    sync::Arc,
    time::{SystemTime, UNIX_EPOCH},
};

use amp_mcp::AmpMcpClient;
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
    mcp_tools,
    models::{ModelsConfig, ModelsError, ResolvedRole},
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

/// Everything sessions in one process share.
pub struct Runtime {
    pub assets: Assets,
    pub roles: RoleRegistry,
    pub models: ModelsConfig,
    pub providers: ProviderSet,
    /// Every skill, written to disk so the model can read them (and their scripts) by path.
    pub skills: Vec<Skill>,
    pub mcp: Arc<AmpMcpClient>,
    pub state_dir: PathBuf,
    /// The user's harness config dir (`~/.config/amp-harness`): its `AGENTS.md` is the global
    /// context file.
    pub config_dir: Option<PathBuf>,
    /// The faux providers a script was loaded into (`AMP_HARNESS_FAUX_SCRIPT`).
    pub faux_scripted: Vec<String>,
    /// `AMP_HARNESS_CEILING_DIR`: context files above it are not loaded.
    pub ceiling_dir: Option<PathBuf>,
    /// Environment variables removed from every bash call: `AMP_TOKEN`, every provider's
    /// `key_env` and (via [`Runtime::with_secret_env`]) the `--token-env` variable, minus
    /// `pass_env`. Names only; values are never held here.
    pub secret_env: BTreeSet<String>,
    /// `[secrets] pass_to_bash` in config.toml: secrets passed to bash on purpose.
    pub pass_env: BTreeSet<String>,
    /// The bash tool's default timeout in seconds, when the model passes none (#2143). `None`:
    /// no default (`bash_timeout_secs = 0`).
    pub bash_timeout: Option<f64>,
}

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
        let dirs = OverrideDirs::discover_with_env(opts.repo_root.as_deref(), opts.env);
        let config_dir = dirs.user.clone();
        let assets = Assets::load(&dirs)?;
        let roles = RoleRegistry::from_assets(&assets)?;
        let models = match opts.models {
            Some(m) => m,
            None => ModelsConfig::load_with_env(opts.env, None)?,
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
        let hash = assets.manifest().source_sha256.clone();
        let short = if hash.len() >= 12 { &hash[..12] } else { "dev" };
        let skills = assets.materialize_skills(&state_dir.join("skills").join(short))?;
        let file = harness_config_from(&models)?;
        let pass_env = pass_env_from(&file);
        let bash_timeout = bash_timeout_from(
            file.tools.bash_timeout_secs,
            (opts.env)(ENV_BASH_TIMEOUT_SECS).as_deref(),
        )?;
        let secret_env = secret_env_names(&models)
            .into_iter()
            .filter(|n| !pass_env.contains(n))
            .collect();
        Ok(Self {
            assets,
            roles,
            models,
            providers,
            skills,
            mcp,
            state_dir,
            config_dir,
            faux_scripted,
            ceiling_dir: (opts.env)(ENV_CEILING_DIR)
                .filter(|v| !v.is_empty())
                .map(PathBuf::from),
            secret_env,
            pass_env,
            bash_timeout,
        })
    }

    /// Also removes `name` from bash calls (the `--token-env` variable), unless it is in
    /// `[secrets] pass_to_bash`.
    pub fn with_secret_env(mut self, name: Option<&str>) -> Self {
        if let Some(name) = name.map(str::trim).filter(|n| !n.is_empty())
            && !self.pass_env.contains(name)
        {
            self.secret_env.insert(name.to_owned());
        }
        self
    }

    pub fn role(&self, name: &str) -> Result<&Role, AgentError> {
        self.roles
            .get(name)
            .ok_or_else(|| AgentError::UnknownRole(name.to_owned()))
    }

    /// Builds one session.
    pub async fn build(&self, spec: AgentSpec) -> Result<BuiltAgent, AgentError> {
        let role = self.role(&spec.role)?.clone();
        let resolved = self.models.resolve_role(&role.model_role)?;
        // Fail early with the key the role needs ("no key: set OPENROUTER_API_KEY ...").
        self.providers.for_role(&resolved)?;

        let cwd = spec.cwd.clone();
        let env = Arc::new(OsExecutionEnv::with_cwd(cwd.clone()));
        let exec: Arc<dyn ExecutionEnv> = env.clone();
        let ctx = ExecutionToolContext::new(exec.clone(), Some(env.clone()));

        let mut tools: Vec<HarnessTool> = vec![
            HarnessTool::new(rpi_tools::create_read_tool(&ctx, None)),
            HarnessTool::new(rpi_tools::create_bash_tool(
                &ctx,
                Some(BashToolOptions {
                    prepare: Some(scrubbing_prepare(self.secret_env.clone())),
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
        tools.extend(mcp_tools::amp_tools(&self.mcp).await?);
        tools.extend(spec.extra_tools);
        let policy = Arc::new(role.tool_policy(&cwd));
        let tools = filter_harness_tools(&policy, tools);
        let tool_names: Vec<String> = tools.iter().map(|t| t.tool.schema().name.clone()).collect();

        let context_files = self.context_files(&exec, &cwd).await;
        let system_prompt = self.system_prompt(&SystemPromptInput {
            role: &role,
            cwd: &cwd,
            project: spec.project.as_ref(),
            extra_context: spec.extra_context.as_deref(),
            context_files: &context_files,
        });

        let (session, session_file) = match &spec.session {
            SessionKind::Memory => (memory_session(), None),
            SessionKind::Jsonl { dir } => {
                let (s, path) = jsonl_session(dir, &cwd).await?;
                (s, Some(path))
            }
        };

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
                skills: Some(self.skills.clone()),
            },
            session,
            models: self.providers.all(),
            agent_emitter: spec.emitter,
            before_tool_call: Some(policy.before_tool_call_then(spec.before_tool_call)),
            ..Default::default()
        };
        let harness = AgentHarness::create(options)
            .await
            .map_err(|e| AgentError::Harness(format!("{e}")))?;
        Ok(BuiltAgent {
            harness: Arc::new(harness),
            role,
            resolved,
            system_prompt,
            tool_names,
            context_files: context_files.into_iter().map(|f| f.path).collect(),
            session_file,
            policy,
        })
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
        let session = self.mcp.session();
        let mut append = Vec::new();

        if let Some(text) = session
            .instructions
            .as_deref()
            .filter(|t| !t.trim().is_empty())
        {
            append.push(format!(
                "<amp_server_instructions server=\"{} {}\">\n{}\n</amp_server_instructions>",
                session.server_name.as_deref().unwrap_or("amp"),
                session.server_version.as_deref().unwrap_or(""),
                text.trim()
            ));
        }
        append.push(amp_context(
            input.role,
            &self.mcp_url(),
            input.project,
            input.cwd,
            input.extra_context,
        ));

        let preloaded: Vec<String> = input
            .role
            .skills
            .iter()
            .filter_map(|name| self.skills.iter().find(|s| &s.name == name))
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

    fn mcp_url(&self) -> String {
        self.mcp.session().url.clone()
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

fn amp_context(
    role: &Role,
    url: &str,
    project: Option<&ProjectMarker>,
    cwd: &Path,
    extra: Option<&str>,
) -> String {
    let mut lines = vec![
        "<amp_context>".to_owned(),
        format!("role: {}", role.id),
        format!("amp_server: {url}"),
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
        };
        let text = amp_context(
            manager,
            "http://localhost:8000/mcp",
            Some(&marker),
            Path::new("/repo"),
            Some("ticket: #7"),
        );
        assert_eq!(
            text,
            "<amp_context>\nrole: amp-manager\namp_server: http://localhost:8000/mcp\nproject_id: 2\nproject_name: AMP v2\nproject_marker: /repo/.amp.json\nworking_directory: /repo\nticket: #7\n</amp_context>"
        );
        let text = amp_context(manager, "u", None, Path::new("/r"), None);
        assert!(text.contains("project_id: none"), "{text}");
    }
}
