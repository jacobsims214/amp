//! Building one agent session the way every interactive or headless command does (M2-4, M2-7):
//! resolve the working directory and `.amp.json`, connect to AMP, load the [`Runtime`], and
//! build the role's harness with a [`ChannelEmitter`] so the caller sees its events.
//!
//! Remote control M1 (#2370): the harness's emitter is a [`FanOutEmitter`]. Its first sink is
//! that channel (the TUI, or `run`'s printer: what they see is unchanged), its second the
//! session's [`ManagerFeed`]; a later client (the remote link) adds a sink or subscribes through
//! [`Session::fanout`], or reads the numbered chat from [`Session::feed`].
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
//!
//! External MCP servers (#2239): the runtime's [`amp_core::mcp_hub::McpHub`] is started here,
//! before the session is built. A headless run (`wait_mcp`) waits for every background and eager
//! server's first attempt (each bounded by its `startup_timeout_secs`); the chat waits only for
//! eager ones and picks the rest up as they connect. Callers shut the hub down before exiting.
//! Repo servers start only once trusted (#2241): the chat asks first (`ask_trust`); headless runs
//! skip untrusted ones. The same goes for the repo's agents, skills and model settings (#2310),
//! asked about before the runtime loads them.

use std::{
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

use amp_core::{
    agent::{AgentSpec, BuiltAgent, Runtime, RuntimeOptions, SessionKind},
    events::{ChannelEmitter, FanOutEmitter, Totals, TotalsEmitter},
    manager_feed::ManagerFeed,
    project::{ENV_CEILING_DIR, ProjectMarker},
    providers::SystemKeys,
    run_report::{RunReporter, SessionUsageTool},
    supervisor::{DispatchMode, ManagerContext, Supervisor, SupervisorConfig, WorkerHooks},
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
    /// Wait for background MCP servers too (`run -p`); otherwise only for eager ones (the chat).
    pub wait_mcp: bool,
    /// Ask the person whether to trust this repo's untrusted MCP servers before they start (the
    /// chat, #2241). Headless runs never ask: untrusted repo servers are skipped.
    pub ask_trust: bool,
    /// How `amp_run_ticket` picks its [`DispatchMode`]: [`HEADLESS_DISPATCH`] for `run -p`,
    /// [`CHAT_DISPATCH`] for the interactive chat.
    pub dispatch: Dispatch,
}

/// How a front end chooses the manager's [`DispatchMode`] against `[workers] dispatch` and
/// `AMP_HARNESS_WORKER_DISPATCH`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Dispatch {
    /// This mode, whatever config.toml or the environment says (AMP #2884).
    Force(DispatchMode),
    /// This mode unless config.toml or the environment names one (AMP #2837).
    Default(DispatchMode),
}

/// `run -p` (AMP #2884): a headless run exits when the manager's reply ends, so a launched worker
/// would be left mid-run with its ticket in progress, and its callers read the worker's report
/// from the tool result. Always `await`.
pub const HEADLESS_DISPATCH: Dispatch = Dispatch::Force(DispatchMode::Await);

/// The interactive chat (AMP #2837): `launch` by default, so the manager's turn ends once its
/// workers are dispatched and the person can keep talking while they run; each worker's end
/// arrives as a notice (#2861) and in the next turn's context (#2857). An explicit
/// `dispatch = "await"` in config.toml or `AMP_HARNESS_WORKER_DISPATCH=await` still wins, as the
/// way back to foreground dispatch if launch misbehaves somewhere.
pub const CHAT_DISPATCH: Dispatch = Dispatch::Default(DispatchMode::Launch);

pub struct Session {
    pub rt: Arc<Runtime>,
    pub agent: BuiltAgent,
    pub events: UnboundedReceiver<AgentEvent>,
    /// Every event of the harness, for more consumers than `events` (#2370).
    #[allow(dead_code, reason = "the remote link (#2373) subscribes here")]
    pub fanout: FanOutEmitter,
    /// The session's chat as numbered [`amp_core::chat_event::ChatEvent`]s (#2370). Its deltas
    /// are numbered once flushed: run [`ManagerFeed::spawn_flusher`] where something reads it.
    pub feed: Arc<ManagerFeed>,
    pub project: Option<ProjectMarker>,
    pub cwd: PathBuf,
    /// The manager's worker supervisor (`None` for a worker role).
    pub supervisor: Option<Arc<Supervisor>>,
    /// Automatic run reporting (D8), kept so a chat can give it its notifier (AMP #2802: a
    /// returned ticket is told to the person, not only to the manager's tool result).
    pub reporter: Option<Arc<RunReporter>>,
    /// Why AMP could not be reached, when the session started offline (`allow_offline`).
    pub offline: Option<amp_mcp::McpError>,
    /// MCP hub changes from before the hub started (a server ready, failed, its tools changed).
    pub mcp_events: tokio::sync::broadcast::Receiver<amp_core::mcp_hub::HubEvent>,
    /// MCP servers still on their first attempt when the session was built.
    pub mcp_starting: Vec<String>,
}

/// The manager's `[workers]` settings for this front end: `dispatch` decides the mode (see
/// [`Dispatch`]); everything else comes from config.toml and the environment as usual. A forced
/// mode that replaces a configured `launch` says so on stderr with `--verbose` / `RUST_LOG`.
fn supervisor_config(
    env: &dyn Fn(&str) -> Option<String>,
    config_path: Option<PathBuf>,
    dispatch: Dispatch,
) -> Result<SupervisorConfig, String> {
    let base = match dispatch {
        Dispatch::Default(mode) => SupervisorConfig {
            dispatch: mode,
            ..SupervisorConfig::default()
        },
        Dispatch::Force(_) => SupervisorConfig::default(),
    };
    let mut config = SupervisorConfig::load_with(env, config_path, base)?;
    if let Dispatch::Force(mode) = dispatch {
        if config.dispatch == DispatchMode::Launch
            && mode != DispatchMode::Launch
            && conn::verbose()
        {
            eprintln!(
                "debug: [workers] dispatch = \"{}\" ignored here; this run uses \"{}\"",
                config.dispatch.as_str(),
                mode.as_str()
            );
        }
        config.dispatch = mode;
    }
    Ok(config)
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
    // #2310: the chat asks about the repo's untrusted agents, skills and model settings before the
    // runtime loads them (it loads only trusted ones; headless runs leave the rest out).
    if req.ask_trust
        && let Some(state) = amp_core::agent::state_dir(&env)
    {
        let paths = conn::config_paths().with_repo(repo_root.as_deref());
        crate::trust_cmd::ask(&paths, &state)?;
    }
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

    // #2241: repo MCP servers start only once trusted; the chat asks here, before the hub starts.
    if req.ask_trust {
        crate::mcp_trust_cmd::ask(&rt.mcp_hub, &rt.state_dir)?;
    }
    // #2239: external MCP servers, connected in the background (waited for as asked).
    let mcp_events = rt.mcp_hub.subscribe();
    rt.mcp_hub.start();
    let mcp_starting = rt.mcp_hub.wait_started(req.wait_mcp).await;

    let role_name = req
        .role
        .map(str::to_owned)
        .unwrap_or_else(|| rt.catalog().roles.manager().id.clone());
    let role = rt
        .role(&role_name)
        .map_err(|e| CliError::usage(e.to_string()))?;
    let role_id = role.id.clone();
    // AMP #2802: the tickets this machine dispatched, shared by the supervisor (which records
    // them) and the reporter (which forgets them once a ticket is closed or handed back).
    let ledger = Arc::new(amp_core::dispatch_ledger::Ledger::open(&rt.state_dir));
    let mut reporter_slot: Option<Arc<RunReporter>> = None;
    let supervisor = if role.primary {
        // #2232: [workers] from the same config.toml the runtime loaded (not the default path).
        let config = supervisor_config(&env, rt.paths.user_file.clone(), req.dispatch)
            .map_err(CliError::new)?;
        let reporter = RunReporter::new(Arc::clone(&client)).with_ledger(Arc::clone(&ledger));
        reporter_slot = Some(Arc::clone(&reporter));
        Some(Supervisor::with_hooks_and_ledger(
            Arc::clone(&rt),
            ManagerContext {
                role: role_id.clone(),
                cwd: cwd.clone(),
                project: project.clone(),
            },
            config,
            Some(reporter as Arc<dyn WorkerHooks>),
            Arc::clone(&ledger),
        ))
    } else {
        None
    };
    let (emitter, events) = ChannelEmitter::new();
    let fanout = FanOutEmitter::new();
    fanout.add_sink(emitter.into_emitter());
    let feed = ManagerFeed::new();
    fanout.add_sink(feed.emitter());
    let totals = Arc::new(Mutex::new(Totals::default()));
    let model = role
        .resolve_model(&rt.current_models())
        .map(|r| r.model_ref())
        .unwrap_or_default();
    let mut spec = AgentSpec::new(role_id.clone(), cwd.clone());
    spec.project = project.clone();
    // #2364: where its workers work (git worktrees, a folder each, or this folder shared).
    spec.extra_context = supervisor.as_ref().map(|s| s.workspace_context());
    spec.extra_tools = supervisor.iter().map(|s| s.tool()).collect();
    spec.extra_tools.push(SessionUsageTool::harness_tool(
        model,
        Arc::clone(&totals),
        false,
    ));
    spec.emitter = Some(
        TotalsEmitter::new(Arc::clone(&totals), Some(fanout.clone().into_emitter())).into_emitter(),
    );
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
        fanout,
        feed,
        project,
        cwd,
        supervisor,
        reporter: reporter_slot,
        offline,
        mcp_events,
        mcp_starting,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    use amp_core::supervisor::ENV_WORKER_DISPATCH;

    /// A config.toml holding `text`, under the build's target dir (never the person's config).
    fn config_file(name: &str, text: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/unit-tmp")
            .join(format!("session-dispatch-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join(name);
        std::fs::write(&path, text).unwrap();
        path
    }

    /// The mode `dispatch` ends up with for this config.toml (none when `None`) and
    /// `AMP_HARNESS_WORKER_DISPATCH` value.
    fn mode(
        dispatch: Dispatch,
        config: Option<&str>,
        var: Option<&str>,
        name: &str,
    ) -> DispatchMode {
        // No config: a path that doesn't exist, so the default location is never read either.
        let path = match config {
            Some(text) => config_file(name, text),
            None => config_file(name, "").with_extension("missing"),
        };
        let env = |k: &str| var.filter(|_| k == ENV_WORKER_DISPATCH).map(str::to_owned);
        supervisor_config(&env, Some(path), dispatch)
            .unwrap()
            .dispatch
    }

    /// AMP #2837: the interactive chat launches its workers unless the person explicitly asked
    /// for `await`; headless `run -p` always awaits (AMP #2884), whatever is configured.
    #[test]
    fn the_chat_launches_by_default_and_headless_always_awaits() {
        use DispatchMode::{Await, Launch};
        let launch = Some("[workers]\ndispatch = \"launch\"\n");
        let await_ = Some("[workers]\ndispatch = \"await\"\n");
        let other = Some("[workers]\nmax_parallel = 2\n");

        // What chat.rs and run.rs pass.
        assert_eq!(CHAT_DISPATCH, Dispatch::Default(Launch));
        assert_eq!(HEADLESS_DISPATCH, Dispatch::Force(Await));

        // Chat: launch, unless config.toml or the environment says await.
        assert_eq!(mode(CHAT_DISPATCH, None, None, "c0"), Launch);
        assert_eq!(mode(CHAT_DISPATCH, other, None, "c1"), Launch);
        assert_eq!(mode(CHAT_DISPATCH, launch, None, "c2"), Launch);
        assert_eq!(mode(CHAT_DISPATCH, await_, None, "c3"), Await);
        assert_eq!(mode(CHAT_DISPATCH, None, Some("await"), "c4"), Await);
        // The environment beats config.toml, as for every worker setting; blank is unset.
        assert_eq!(mode(CHAT_DISPATCH, await_, Some("launch"), "c5"), Launch);
        assert_eq!(mode(CHAT_DISPATCH, None, Some(" "), "c6"), Launch);

        // Headless: await, whatever is configured.
        assert_eq!(mode(HEADLESS_DISPATCH, None, None, "h0"), Await);
        assert_eq!(mode(HEADLESS_DISPATCH, launch, None, "h1"), Await);
        assert_eq!(mode(HEADLESS_DISPATCH, None, Some("launch"), "h2"), Await);
        assert_eq!(mode(HEADLESS_DISPATCH, launch, Some("launch"), "h3"), Await);
    }

    /// The rest of `[workers]` still loads around the chosen mode, and a bad value still fails.
    #[test]
    fn the_dispatch_choice_keeps_the_other_worker_settings() {
        let none = |_: &str| None;
        let path = config_file("keep", "[workers]\nmax_parallel = 2\nmax_turns = 7\n");
        let c = supervisor_config(&none, Some(path), CHAT_DISPATCH).unwrap();
        assert_eq!((c.max_parallel, c.max_turns), (2, 7));
        assert_eq!(c.dispatch, DispatchMode::Launch);

        let bad = config_file("bad", "[workers]\ndispatch = \"later\"\n");
        let e = supervisor_config(&none, Some(bad), CHAT_DISPATCH).unwrap_err();
        assert!(e.contains("expected `await` or `launch`"), "{e}");
    }
}
