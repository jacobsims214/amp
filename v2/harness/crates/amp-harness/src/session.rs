//! Building one agent session the way every interactive or headless command does (M2-4, M2-7):
//! resolve the working directory and `.amp.json`, connect to AMP, load the [`Runtime`], and
//! build the role's harness with a [`ChannelEmitter`] so the caller sees its events.

use std::{
    path::{Path, PathBuf},
    sync::Arc,
};

use amp_core::{
    agent::{AgentSpec, BuiltAgent, Runtime, RuntimeOptions, SessionKind},
    events::ChannelEmitter,
    project::{ENV_CEILING_DIR, ProjectMarker},
    providers::SystemKeys,
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
}

pub struct Session {
    pub rt: Runtime,
    pub agent: BuiltAgent,
    pub events: UnboundedReceiver<AgentEvent>,
    pub project: Option<ProjectMarker>,
    pub cwd: PathBuf,
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
    let client = Arc::new(conn::connect(&settings).await?);
    let env = |k: &str| std::env::var(k).ok();
    let keys = SystemKeys::from_env();
    let rt = Runtime::load(
        Arc::clone(&client),
        RuntimeOptions {
            repo_root,
            models: None,
            keys: &keys,
            env: &env,
            state_dir: None,
        },
    )
    .map_err(|e| CliError::new(e.to_string()))?;

    let role_name = req
        .role
        .map(str::to_owned)
        .unwrap_or_else(|| rt.roles.manager().id.clone());
    let role_id = rt
        .role(&role_name)
        .map_err(|e| CliError::usage(e.to_string()))?
        .id
        .clone();
    let (emitter, events) = ChannelEmitter::new();
    let mut spec = AgentSpec::new(role_id.clone(), cwd.clone());
    spec.project = project.clone();
    spec.emitter = Some(emitter.into_emitter());
    spec.session = if req.memory {
        SessionKind::Memory
    } else {
        SessionKind::Jsonl {
            dir: rt.state_dir.join("sessions").join(&role_id),
        }
    };
    let agent = rt
        .build(spec)
        .await
        .map_err(|e| CliError::new(e.to_string()))?;
    Ok(Session {
        rt,
        agent,
        events,
        project,
        cwd,
    })
}
