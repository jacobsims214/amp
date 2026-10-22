//! Building one agent session the way every interactive or headless command does (M2-4, M2-7):
//! resolve the working directory and `.amp.json`, connect to AMP, load the [`Runtime`], and
//! build the role's harness with a [`ChannelEmitter`] so the caller sees its events.
//!
//! A primary role (the manager) also gets a worker [`Supervisor`] and its `amp_run_ticket` tool
//! (D7, M2-5), limited by config.toml `[workers]` and `AMP_HARNESS_MAX_WORKERS` /
//! `AMP_HARNESS_WORKER_TIMEOUT_SECS` / `AMP_HARNESS_WORKER_MAX_TURNS`. Aborting the manager's run
//! cancels the workers its tool calls started; [`Supervisor::cancel_all`] cancels every worker.
//!
//! Run reporting (D8, M2-6): the supervisor runs with a [`RunReporter`], so every worker's
//! `amp_complete_task` / `amp_block_task` carries its model, tokens, cost and git fields, and a
//! worker that ends without closing its ticket is reported with `amp_report_run`. Every session
//! also has the native `amp_session_usage` tool over its own live [`Totals`].

use std::{
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

use amp_core::{
    agent::{AgentSpec, BuiltAgent, Runtime, RuntimeOptions, SessionKind},
    events::{ChannelEmitter, Totals, TotalsEmitter},
    project::{ENV_CEILING_DIR, ProjectMarker},
    providers::SystemKeys,
    run_report::{RunReporter, SessionUsageTool},
    supervisor::{ManagerContext, Supervisor, SupervisorConfig, WorkerHooks},
};
use rpi_agent::AgentEvent;
use tokio::sync::mpsc::UnboundedReceiver;

use crate::conn::{self, CliError, ConnArgs};

pub struct SessionRequest<'a> {
    pub conn: &'a ConnArgs,
    /// The role (default: the manager).
    pub role: Option<&'a str>,
    /// Working directory for the agent's tools (default: the current directory).
    pub cwd: Option<&'a Path>,
    /// Keep the transcript in memory instead of a JSONL file under `<state>/sessions/<role>/`.
    pub memory: bool,
    /// When AMP can't be reached, build the session anyway on an offline client (the chat,
    /// #2178) instead of failing: no AMP tools until the caller connects it. Bad settings (an
    /// unparsable URL, an unreadable CA file) still fail.
    pub allow_offline: bool,
}

pub struct Session {
    pub rt: Arc<Runtime>,
    pub agent: BuiltAgent,
    pub events: UnboundedReceiver<AgentEvent>,
    pub project: Option<ProjectMarker>,
    pub cwd: PathBuf,
    /// The manager's worker supervisor (`None` for a worker role).
    pub supervisor: Option<Arc<Supervisor>>,
    /// Why AMP could not be reached, when the session started offline (`allow_offline`).
    pub offline: Option<amp_mcp::McpError>,
}

pub async fn build(req: SessionRequest<'_>) -> Result<Session, CliError> {
    let cwd = match req.cwd {
        Some(dir) => dir.to_path_buf(),
        None => {
            std::env::current_dir().map_err(|e| CliError::new(format!("current directory: {e}")))?
        }
    };
    let cwd = std::fs::canonicalize(&cwd)
        .map_err(|e| CliError::usage(format!("--cwd {}: {e}", cwd.display())))?;
    let ceiling = std::env::var_os(ENV_CEILING_DIR)
        .filter(|v| !v.is_empty())
        .map(PathBuf::from);
    let project = ProjectMarker::find_within(&cwd, ceiling.as_deref())?;
    let repo_root = project
        .as_ref()
        .and_then(|p| p.path.parent().map(PathBuf::from));

    let settings = req.conn.settings()?;
    // Heals itself after `amp-harness login` (#2159); everyone below shares this one client.
    let (client, offline) = match conn::connect_session(&settings).await {
        Ok(client) => (client, None),
        Err(e) if req.allow_offline && e.kind() != amp_mcp::FailureKind::Config => {
            // No reconnector yet: whoever connects it (the chat's retry, `/server`) also gives
            // the model its AMP tools, which a silent reconnect would not.
            let options = amp_mcp::ClientOptions::from_settings(&settings);
            (
                amp_mcp::AmpMcpClient::offline(&options, e.reason()),
                Some(e),
            )
        }
        Err(e) => {
            return Err(CliError::new(conn::connect_message_verbose(
                &settings.url,
                Some(&settings.auth),
                &e,
            )));
        }
    };
    let client = Arc::new(client);
    let env = |k: &str| std::env::var(k).ok();
    let keys = SystemKeys::from_env();
    let rt = Arc::new(
        Runtime::load(
            Arc::clone(&client),
            RuntimeOptions {
                repo_root,
                models: None,
                keys: &keys,
                env: &env,
                state_dir: None,
            },
        )
        .map_err(|e| CliError::new(e.to_string()))?
        // --token-env's variable is a secret too: keep it out of bash (#2142).
        .with_secret_env(req.conn.token_env.as_deref()),
    );

    let role_name = req
        .role
        .map(str::to_owned)
        .unwrap_or_else(|| rt.roles.manager().id.clone());
    let role = rt
        .role(&role_name)
        .map_err(|e| CliError::usage(e.to_string()))?;
    let role_id = role.id.clone();
    let supervisor = if role.primary {
        let config = SupervisorConfig::load(&env, None).map_err(CliError::new)?;
        Some(Supervisor::with_hooks(
            Arc::clone(&rt),
            ManagerContext {
                role: role_id.clone(),
                cwd: cwd.clone(),
                project: project.clone(),
            },
            config,
            Some(RunReporter::new(Arc::clone(&client)) as Arc<dyn WorkerHooks>),
        ))
    } else {
        None
    };
    let (emitter, events) = ChannelEmitter::new();
    let totals = Arc::new(Mutex::new(Totals::default()));
    let model = rt
        .models
        .resolve_role(&role.model_role)
        .map(|r| r.model_ref())
        .unwrap_or_default();
    let mut spec = AgentSpec::new(role_id.clone(), cwd.clone());
    spec.project = project.clone();
    spec.extra_tools = supervisor.iter().map(|s| s.tool()).collect();
    spec.extra_tools.push(SessionUsageTool::harness_tool(
        model,
        Arc::clone(&totals),
        false,
    ));
    spec.emitter =
        Some(TotalsEmitter::new(Arc::clone(&totals), Some(emitter.into_emitter())).into_emitter());
    spec.session = if req.memory {
        SessionKind::Memory
    } else {
        SessionKind::Jsonl {
            dir: rt.state_dir.join("sessions").join(&role_id),
        }
    };
    let agent = rt.build(spec).await.map_err(|e| {
        let text = e.to_string();
        if text.contains("no key") {
            // The first run asks for a key in a terminal; say how to get there from here.
            CliError::new(format!(
                "{text}\nStore a key with `amp-harness connect` (it asks, hidden, checks it and keeps it in \
                 the OS keychain)."
            ))
        } else {
            CliError::new(text)
        }
    })?;
    Ok(Session {
        rt,
        agent,
        events,
        project,
        cwd,
        supervisor,
        offline,
    })
}
