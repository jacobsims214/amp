//! The worker supervisor and the manager's `amp_run_ticket` tool (D7, M2-5).
//!
//! `amp_run_ticket(task_id, agent_id)` runs one specialist on one AMP ticket, in the foreground of
//! the tool call, and returns the worker's final message and outcome. Per call:
//!
//! 1. **Check the agent.** It must be a role [`RoleRegistry::get`] knows *and* one the manager may
//!    dispatch ([`RoleRegistry::dispatchable`]). An id the policy's `amp-*` rule would allow but
//!    the roster doesn't have is refused before anything happens in AMP.
//! 2. **Wait for a slot.** A semaphore caps running workers ([`SupervisorConfig::max_parallel`],
//!    default 4); extra calls wait, and a cancelled wait dispatches nothing.
//! 3. **`amp_dispatch_task`**, so the board shows the ticket in progress only once its worker is
//!    about to start. AMP refusing it (someone else's ticket, blocked, human) ends the call.
//! 4. **Worktree.** Roles that can edit files run in `git worktree` `<repo>/.worktrees/<ticket>`
//!    (created on branch `amp-<ticket>` off `HEAD`, or reused if it already exists); read-only
//!    roles run in the manager's directory. `worktree`, `branch` and `base` arguments override.
//!    `.worktrees/` is added to the repo's `info/exclude` if nothing ignores it yet.
//! 5. **An independent `AgentHarness`** from [`Runtime::build`]: the role's own model and thinking,
//!    its prompt + AMP instructions + `<amp_context>` with the ticket, worktree and branch,
//!    rpi-tools bound to the worker's directory, the role-filtered `amp_*` tools on the shared MCP
//!    client, and its own JSONL session under `<state>/sessions/<ticket>/`.
//! 6. **Run** until the worker stops, the wall-clock timeout ([`SupervisorConfig::timeout`],
//!    default 60 min) or the turn budget runs out, or the call is cancelled (the manager's run was
//!    aborted, or [`Supervisor::cancel_all`]). Ending a worker early aborts its harness, which
//!    cancels its in-flight tools.
//!
//! Every call that got as far as step 3 ends in a [`WorkerReport`] with a [`WorkerOutcome`], the
//! worker's usage [`Totals`] and where its work is. Run reporting ([`crate::run_report`], M2-6)
//! reports it to AMP through [`WorkerHooks`]. Errors before step 3 are tool errors for the
//! manager and touch nothing in AMP.

use std::{
    collections::{BTreeMap, BTreeSet, HashMap},
    path::{Path, PathBuf},
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::{Duration, Instant, SystemTime},
};

use rpi_agent::{
    AgentEmitter, AgentError as ToolError, AgentEvent, AgentTool, AgentToolResult, BeforeToolCall,
    ToolResultPartial,
};
use rpi_ai::types::Tool;
use rpi_harness::{
    agent_harness::{AgentLane, HarnessRunOutcome},
    types::HarnessTool,
};
use serde::Serialize;
use serde_json::{Value, json};
use tokio::sync::Semaphore;
use tokio_util::sync::CancellationToken;

use crate::{
    agent::{AgentSpec, Runtime, SessionKind},
    events::{RunEvent, Totals, assistant_text},
    policy::DISPATCH_TOOL,
    project::ProjectMarker,
    roles::Role,
    tools::call_json,
    worker_session::{self, WorkerFeed},
};

/// The AMP tools that close a ticket. A worker that ends without one of them succeeding on its
/// ticket ended abnormally (D8).
pub const COMPLETE_TOOL: &str = "amp_complete_task";
pub const BLOCK_TOOL: &str = "amp_block_task";
/// `config.toml` table with the supervisor's settings.
pub const CONFIG_TABLE: &str = "workers";

/// Limits for workers. [`SupervisorConfig::load`] reads them from config.toml `[workers]` and the
/// environment.
#[derive(Debug, Clone, PartialEq)]
pub struct SupervisorConfig {
    /// Workers running at once (`max_parallel`, `AMP_HARNESS_MAX_WORKERS`). Default 4.
    pub max_parallel: usize,
    /// Wall-clock limit per worker (`timeout_minutes`, `AMP_HARNESS_WORKER_TIMEOUT_SECS`). Default
    /// 60 minutes.
    pub timeout: Duration,
    /// Model turns per worker before it is stopped (`max_turns`, `AMP_HARNESS_WORKER_MAX_TURNS`);
    /// 0 = no limit. Default 400. rpi's own turn budget is only settable on `create_child`, so the
    /// supervisor counts turns itself.
    pub max_turns: u32,
    /// How long an aborted worker gets to wind down before it is dropped.
    pub abort_grace: Duration,
}

impl Default for SupervisorConfig {
    fn default() -> Self {
        Self {
            max_parallel: 4,
            timeout: Duration::from_secs(60 * 60),
            max_turns: 400,
            abort_grace: Duration::from_secs(10),
        }
    }
}

pub const ENV_MAX_WORKERS: &str = "AMP_HARNESS_MAX_WORKERS";
pub const ENV_WORKER_TIMEOUT_SECS: &str = "AMP_HARNESS_WORKER_TIMEOUT_SECS";
pub const ENV_WORKER_MAX_TURNS: &str = "AMP_HARNESS_WORKER_MAX_TURNS";

#[derive(Debug, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct WorkersTable {
    max_parallel: Option<usize>,
    timeout_minutes: Option<f64>,
    max_turns: Option<u32>,
}

#[derive(Debug, Default, serde::Deserialize)]
struct ConfigFile {
    #[serde(default)]
    workers: Option<WorkersTable>,
}

impl SupervisorConfig {
    /// Defaults, then config.toml `[workers]` (at `config_path`, else the default location), then
    /// the environment. A bad value is an error naming where it came from.
    pub fn load(
        env: &dyn Fn(&str) -> Option<String>,
        config_path: Option<PathBuf>,
    ) -> Result<Self, String> {
        let mut cfg = Self::default();
        if let Some(path) = config_path.or_else(|| amp_mcp::default_config_path(env))
            && let Ok(text) = std::fs::read_to_string(&path)
        {
            cfg.apply_toml(&text)
                .map_err(|e| format!("{} [{CONFIG_TABLE}]: {e}", path.display()))?;
        }
        cfg.apply_env(env)?;
        Ok(cfg)
    }

    fn apply_toml(&mut self, text: &str) -> Result<(), String> {
        let file: ConfigFile = toml::from_str(text).map_err(|e| e.to_string())?;
        let Some(w) = file.workers else {
            return Ok(());
        };
        if let Some(n) = w.max_parallel {
            self.max_parallel = positive(n, "max_parallel")?;
        }
        if let Some(m) = w.timeout_minutes {
            if !(m.is_finite() && m > 0.0) {
                return Err(format!("timeout_minutes must be > 0, not {m}"));
            }
            self.timeout = Duration::from_secs_f64(m * 60.0);
        }
        if let Some(t) = w.max_turns {
            self.max_turns = t;
        }
        Ok(())
    }

    fn apply_env(&mut self, env: &dyn Fn(&str) -> Option<String>) -> Result<(), String> {
        let get = |k: &str| env(k).filter(|v| !v.trim().is_empty());
        let num = |k: &str, v: String| -> Result<u64, String> {
            v.trim()
                .parse::<u64>()
                .map_err(|_| format!("{k}={v}: expected a whole number"))
        };
        if let Some(v) = get(ENV_MAX_WORKERS) {
            self.max_parallel = positive(num(ENV_MAX_WORKERS, v)? as usize, ENV_MAX_WORKERS)?;
        }
        if let Some(v) = get(ENV_WORKER_TIMEOUT_SECS) {
            let secs = num(ENV_WORKER_TIMEOUT_SECS, v)?;
            self.timeout =
                Duration::from_secs(positive(secs as usize, ENV_WORKER_TIMEOUT_SECS)? as u64);
        }
        if let Some(v) = get(ENV_WORKER_MAX_TURNS) {
            self.max_turns = num(ENV_WORKER_MAX_TURNS, v)? as u32;
        }
        Ok(())
    }
}

fn positive(n: usize, what: &str) -> Result<usize, String> {
    if n == 0 {
        Err(format!("{what} must be at least 1"))
    } else {
        Ok(n)
    }
}

/// Where the manager runs: workers' worktrees go inside this repo.
#[derive(Debug, Clone)]
pub struct ManagerContext {
    /// The manager's role id.
    pub role: String,
    /// The manager's working directory (inside the repo it manages).
    pub cwd: PathBuf,
    /// The manager's `.amp.json`, passed on to workers.
    pub project: Option<ProjectMarker>,
}

/// One `amp_run_ticket` call.
#[derive(Debug, Clone, PartialEq, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RunRequest {
    pub task_id: i64,
    pub agent_id: String,
    /// Extra instructions from the manager, appended to the worker's prompt.
    #[serde(default)]
    pub instructions: Option<String>,
    /// Force a worktree on (`true`) or off (`false`). Default: when the role can edit files.
    #[serde(default)]
    pub worktree: Option<bool>,
    /// The worktree's branch. Default `amp-<task_id>`.
    #[serde(default)]
    pub branch: Option<String>,
    /// Where a new branch starts. Default `HEAD` of the manager's repo.
    #[serde(default)]
    pub base: Option<String>,
}

/// How a worker's run ended. M2-6 reports a run for each; every outcome but `Completed` and
/// `Blocked` is an abnormal end (D8: "abandoned: <reason>").
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerOutcome {
    /// The worker completed its ticket (`amp_complete_task` succeeded).
    Completed,
    /// The worker blocked its ticket with a question (`amp_block_task` succeeded).
    Blocked,
    /// The worker stopped on its own without completing or blocking its ticket, or the repeat
    /// guard stopped its model (#2225; `error` then says so).
    Stopped,
    /// The worker could not be set up (worktree, model key, ...) or its run failed (model error).
    Error,
    /// The wall-clock timeout ran out.
    Timeout,
    /// The manager's run was aborted, or every worker was cancelled.
    Cancelled,
    /// The turn budget ran out.
    TurnBudget,
}

impl WorkerOutcome {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Completed => "completed",
            Self::Blocked => "blocked",
            Self::Stopped => "stopped",
            Self::Error => "error",
            Self::Timeout => "timeout",
            Self::Cancelled => "cancelled",
            Self::TurnBudget => "turn_budget",
        }
    }

    /// The worker closed its ticket itself.
    pub fn closed_ticket(self) -> bool {
        matches!(self, Self::Completed | Self::Blocked)
    }
}

/// What a worker is, known before it starts (hooks get this).
#[derive(Debug, Clone, Serialize)]
pub struct WorkerInfo {
    pub task_id: i64,
    /// AMP's form: `amp:amp-worker-backend`.
    pub agent_id: String,
    /// The role id: `amp-worker-backend`.
    pub role: String,
    /// `provider/model`.
    pub model: String,
    pub cwd: PathBuf,
    pub worktree: Option<PathBuf>,
    pub branch: Option<String>,
    /// The worker's live usage totals, updated as each event is emitted (so a before-tool hook on
    /// `amp_complete_task` sees every assistant message before that call).
    #[serde(skip)]
    pub totals: Arc<Mutex<Totals>>,
}

/// How a worker's run ended, returned to the manager as `amp_run_ticket`'s result.
#[derive(Debug, Clone, Serialize)]
pub struct WorkerReport {
    pub task_id: i64,
    pub agent_id: String,
    pub role: String,
    pub outcome: WorkerOutcome,
    /// The worker's last assistant text.
    pub final_message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    /// `provider/model` (empty when the worker could not be built).
    pub model: String,
    pub cwd: PathBuf,
    pub worktree: Option<PathBuf>,
    pub branch: Option<String>,
    pub session_file: Option<PathBuf>,
    pub turns: u32,
    pub tool_calls: u32,
    pub tool_errors: u32,
    pub usage: crate::events::UsageJson,
    pub duration_ms: u64,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub notes: Vec<String>,
    #[serde(skip)]
    pub totals: Totals,
}

/// The supervisor's extension points; run reporting ([`crate::run_report::RunReporter`], M2-6)
/// plugs in here.
#[async_trait::async_trait]
pub trait WorkerHooks: Send + Sync {
    /// A before-tool hook for this worker. It runs after the role's policy allowed a call
    /// ([`AgentSpec::before_tool_call`]); run reporting fills run fields into
    /// `amp_complete_task` / `amp_block_task` here.
    fn before_tool_call(&self, _worker: &WorkerInfo) -> Option<BeforeToolCall> {
        None
    }

    /// Extra tools for this worker (run reporting: `amp_session_usage`). The role's policy
    /// filters them like any other tool.
    fn extra_tools(&self, _worker: &WorkerInfo) -> Vec<HarnessTool> {
        Vec::new()
    }

    /// Every dispatched worker ends here exactly once, whatever the outcome, before the report
    /// goes back to the manager. Run reporting calls `amp_report_run` here when
    /// `!report.outcome.closed_ticket()` and adds a note to the report.
    async fn on_finish(&self, _worker: &WorkerInfo, _report: &mut WorkerReport) {}

    /// Hands ticket `task_id` back after this process abandoned its worker (#2204): AMP moves it
    /// from in progress to ready, so it isn't left in progress with nobody on it. `why` is one
    /// line for the ticket's history. Run reporting calls it from `on_finish` for a `cancelled`
    /// worker (the chat stopped or quit), and [`Supervisor::release_unfinished`] calls it when
    /// the chat quits without waiting for `on_finish`.
    async fn release(&self, _task_id: i64, _why: &str) -> Result<(), String> {
        Err("no run reporting in this process".to_owned())
    }
}

/// Why `amp_run_ticket` refused or failed before a worker was dispatched.
#[derive(Debug, thiserror::Error)]
pub enum RunTicketError {
    #[error("unknown agent `{agent}`. Agents you can run: {available}")]
    UnknownAgent { agent: String, available: String },
    #[error("`{agent}` can't be dispatched by {manager}. Agents you can run: {available}")]
    NotDispatchable {
        agent: String,
        manager: String,
        available: String,
    },
    #[error("ticket #{0} already has a worker running in this session")]
    AlreadyRunning(i64),
    #[error("AMP refused to dispatch ticket #{task_id}: {message}")]
    Dispatch { task_id: i64, message: String },
    #[error("cancelled before the worker started")]
    Cancelled,
    #[error("bad arguments: {0}")]
    BadArguments(String),
    /// The person stopped the worker from the chat while it waited for a slot (#2252).
    #[error(
        "{by} stopped the worker on #{task_id} from the chat before it started; nothing was dispatched. Ask them before running it again"
    )]
    Stopped { task_id: i64, by: String },
}

/// Why [`Supervisor::stop_worker`] did nothing.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum StopError {
    #[error("#{0} has no worker in this chat")]
    NotFound(i64),
    #[error("the worker on #{0} has already finished")]
    Finished(i64),
    #[error("the worker on #{0} is already stopping")]
    AlreadyStopping(i64),
}

/// A worker's session as the chat lists it (#2252): live (running or queued) or finished in this
/// process.
#[derive(Debug, Clone)]
pub struct WorkerSessionInfo {
    pub task_id: i64,
    pub agent_id: String,
    /// `Some` while the worker is live; `None` once it has finished.
    pub phase: Option<WorkerPhase>,
    pub model: Option<String>,
    pub cwd: Option<PathBuf>,
    /// When it started running (or was queued).
    pub since: SystemTime,
    /// When it finished.
    pub ended: Option<SystemTime>,
    pub totals: Totals,
    /// How it ended (finished workers).
    pub outcome: Option<WorkerOutcome>,
    pub error: Option<String>,
    /// Who stopped it from the chat, once someone has asked it to stop.
    pub stopped_by: Option<String>,
    pub session_file: Option<PathBuf>,
}

impl WorkerSessionInfo {
    pub fn is_live(&self) -> bool {
        self.phase.is_some()
    }
}

/// A worker that finished in this process: its feed stays viewable.
struct FinishedWorker {
    info: WorkerSessionInfo,
    feed: Arc<WorkerFeed>,
}

/// What a stop from the chat says (#2252): the worker's error, the abandoned run's note
/// (`abandoned: cancelled: stopped by ... from the chat`) and the reason the ticket is handed back.
pub fn stopped_from_chat(by: &str) -> String {
    format!("stopped by {by} from the chat")
}

/// A worker's phase, for a `/workers` view.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerPhase {
    /// Waiting for a free slot.
    Queued,
    Running,
}

/// A live worker, for a `/workers` view.
#[derive(Debug, Clone, Serialize)]
pub struct WorkerStatus {
    pub task_id: i64,
    pub agent_id: String,
    pub phase: WorkerPhase,
    pub model: Option<String>,
    pub cwd: Option<PathBuf>,
    pub since: SystemTime,
    pub totals: Totals,
}

struct LiveWorker {
    agent_id: String,
    /// AMP has the dispatch (the ticket is in progress).
    dispatched: bool,
    /// Whether the worker has closed its ticket yet (set once it is running).
    watch: Option<Arc<WorkerWatch>>,
    phase: WorkerPhase,
    model: Option<String>,
    cwd: Option<PathBuf>,
    since: SystemTime,
    totals: Option<Arc<Mutex<Totals>>>,
    /// Every event the worker emitted, for the chat's session view (#2252).
    feed: Arc<WorkerFeed>,
    /// Cancelled by [`Supervisor::stop_worker`]; `stopped_by` says who.
    stop: CancellationToken,
    stopped_by: Arc<Mutex<Option<String>>>,
}

/// Runs workers for one manager. Share it as `Arc<Supervisor>`; [`Supervisor::tool`] is the
/// manager's `amp_run_ticket`.
pub struct Supervisor {
    rt: Arc<Runtime>,
    manager: ManagerContext,
    config: SupervisorConfig,
    slots: Arc<Semaphore>,
    cancel: CancellationToken,
    hooks: Option<Arc<dyn WorkerHooks>>,
    live: Mutex<BTreeMap<i64, LiveWorker>>,
    /// Workers that finished in this process, by ticket (the latest run of each).
    finished: Mutex<BTreeMap<i64, FinishedWorker>>,
    running: AtomicUsize,
    peak: AtomicUsize,
    /// Serializes `git worktree` changes (git's own lock fails rather than waits).
    git_lock: tokio::sync::Mutex<()>,
}

impl std::fmt::Debug for Supervisor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Supervisor")
            .field("manager", &self.manager.role)
            .field("config", &self.config)
            .finish_non_exhaustive()
    }
}

impl Supervisor {
    pub fn new(rt: Arc<Runtime>, manager: ManagerContext, config: SupervisorConfig) -> Arc<Self> {
        Self::with_hooks(rt, manager, config, None)
    }

    pub fn with_hooks(
        rt: Arc<Runtime>,
        manager: ManagerContext,
        config: SupervisorConfig,
        hooks: Option<Arc<dyn WorkerHooks>>,
    ) -> Arc<Self> {
        Arc::new(Self {
            slots: Arc::new(Semaphore::new(config.max_parallel.max(1))),
            rt,
            manager,
            config,
            cancel: CancellationToken::new(),
            hooks,
            live: Mutex::new(BTreeMap::new()),
            finished: Mutex::new(BTreeMap::new()),
            running: AtomicUsize::new(0),
            peak: AtomicUsize::new(0),
            git_lock: tokio::sync::Mutex::new(()),
        })
    }

    pub fn config(&self) -> &SupervisorConfig {
        &self.config
    }

    /// The manager's `amp_run_ticket` tool, for [`AgentSpec::extra_tools`].
    pub fn tool(self: &Arc<Self>) -> HarnessTool {
        HarnessTool::new(Arc::new(RunTicketTool::new(Arc::clone(self))))
    }

    /// Cancels every queued and running worker (the TUI's Ctrl-C, or shutdown).
    pub fn cancel_all(&self) {
        self.cancel.cancel();
    }

    /// Live workers, by ticket.
    pub fn workers(&self) -> Vec<WorkerStatus> {
        let live = self.live.lock().unwrap();
        live.iter()
            .map(|(id, w)| WorkerStatus {
                task_id: *id,
                agent_id: w.agent_id.clone(),
                phase: w.phase,
                model: w.model.clone(),
                cwd: w.cwd.clone(),
                since: w.since,
                totals: w
                    .totals
                    .as_ref()
                    .map(|t| t.lock().unwrap().clone())
                    .unwrap_or_default(),
            })
            .collect()
    }

    /// Tickets AMP has as dispatched to a live worker of this process that hasn't closed its
    /// ticket (completed or blocked) yet: what quitting now would leave in progress.
    pub fn unfinished(&self) -> Vec<i64> {
        let live = self.live.lock().unwrap();
        live.iter()
            .filter(|(_, w)| {
                w.dispatched
                    && w.watch
                        .as_ref()
                        .is_none_or(|watch| watch.closed.lock().unwrap().is_none())
            })
            .map(|(id, _)| *id)
            .collect()
    }

    /// Returns every [`Supervisor::unfinished`] ticket to ready through the hooks
    /// ([`WorkerHooks::release`]), all at once, within `bound`. Quitting without waiting calls
    /// it, because the workers' own `on_finish` will not get to run (#2204). Each ticket comes
    /// back with its result; one still going when `bound` passes says so.
    pub async fn release_unfinished(
        &self,
        why: &str,
        bound: Duration,
    ) -> Vec<(i64, Result<(), String>)> {
        let ids = self.unfinished();
        let Some(hooks) = self.hooks.clone() else {
            return ids
                .into_iter()
                .map(|id| (id, Err("no run reporting in this process".to_owned())))
                .collect();
        };
        let mut set = tokio::task::JoinSet::new();
        for id in &ids {
            let (hooks, id, why) = (Arc::clone(&hooks), *id, why.to_owned());
            set.spawn(async move { (id, hooks.release(id, &why).await) });
        }
        let mut out = Vec::new();
        let _ = tokio::time::timeout(bound, async {
            while let Some(done) = set.join_next().await {
                if let Ok(r) = done {
                    out.push(r);
                }
            }
        })
        .await;
        for id in ids {
            if !out.iter().any(|(d, _)| *d == id) {
                out.push((
                    id,
                    Err(format!(
                        "AMP did not answer within {}",
                        human_duration(bound)
                    )),
                ));
            }
        }
        out.sort_by_key(|(id, _)| *id);
        out
    }

    /// The most workers that ran at the same time.
    pub fn peak_running(&self) -> usize {
        self.peak.load(Ordering::SeqCst)
    }

    /// Every worker session of this chat: the live ones (by ticket), then the finished ones
    /// (by ticket) that have no live run (#2252).
    pub fn sessions(&self) -> Vec<WorkerSessionInfo> {
        let mut out: Vec<WorkerSessionInfo> = {
            let live = self.live.lock().unwrap();
            live.iter().map(|(id, w)| live_info(*id, w)).collect()
        };
        let finished = self.finished.lock().unwrap();
        for (id, f) in finished.iter() {
            if !out.iter().any(|s| s.task_id == *id) {
                out.push(f.info.clone());
            }
        }
        out
    }

    /// One worker session of this chat, live or finished.
    pub fn session(&self, task_id: i64) -> Option<WorkerSessionInfo> {
        if let Some(w) = self.live.lock().unwrap().get(&task_id) {
            return Some(live_info(task_id, w));
        }
        self.finished
            .lock()
            .unwrap()
            .get(&task_id)
            .map(|f| f.info.clone())
    }

    /// The events of ticket `task_id`'s worker in this chat (live, or the last finished run).
    pub fn feed(&self, task_id: i64) -> Option<Arc<WorkerFeed>> {
        if let Some(w) = self.live.lock().unwrap().get(&task_id) {
            return Some(Arc::clone(&w.feed));
        }
        self.finished
            .lock()
            .unwrap()
            .get(&task_id)
            .map(|f| Arc::clone(&f.feed))
    }

    /// The newest session file a worker on `task_id` wrote, in this process or an earlier one.
    pub fn latest_session_file(&self, task_id: i64) -> Option<PathBuf> {
        worker_session::latest_session(&self.rt.state_dir, task_id)
    }

    /// Stops the live worker on `task_id` because `by` (the person at the chat) asked to
    /// (#2252). Its run ends as `cancelled` with the error [`stopped_from_chat`]: run reporting
    /// records it abandoned and hands the ticket back to ready (#2204), and the manager's
    /// `amp_run_ticket` returns with a note saying who stopped it. A worker still waiting for a
    /// slot is never dispatched.
    pub fn stop_worker(&self, task_id: i64, by: &str) -> Result<(), StopError> {
        let live = self.live.lock().unwrap();
        let Some(w) = live.get(&task_id) else {
            return Err(if self.finished.lock().unwrap().contains_key(&task_id) {
                StopError::Finished(task_id)
            } else {
                StopError::NotFound(task_id)
            });
        };
        if w.stop.is_cancelled() {
            return Err(StopError::AlreadyStopping(task_id));
        }
        let by = by.trim();
        *w.stopped_by.lock().unwrap() = Some(if by.is_empty() {
            "the person".to_owned()
        } else {
            by.to_owned()
        });
        w.stop.cancel();
        Ok(())
    }

    /// Keeps a finished worker's session viewable, and forgets it from the live map's point of
    /// view (the live guard removes it right after).
    fn record_finished(&self, report: &WorkerReport) {
        let (feed, since, stopped_by) = {
            let live = self.live.lock().unwrap();
            match live.get(&report.task_id) {
                Some(w) => (
                    Arc::clone(&w.feed),
                    w.since,
                    w.stopped_by.lock().unwrap().clone(),
                ),
                None => (WorkerFeed::new(), SystemTime::now(), None),
            }
        };
        let info = WorkerSessionInfo {
            task_id: report.task_id,
            agent_id: report.agent_id.clone(),
            phase: None,
            model: Some(report.model.clone()).filter(|m| !m.is_empty()),
            cwd: Some(report.cwd.clone()),
            since,
            ended: Some(SystemTime::now()),
            totals: report.totals.clone(),
            outcome: Some(report.outcome),
            error: report.error.clone(),
            stopped_by,
            session_file: report.session_file.clone(),
        };
        self.finished
            .lock()
            .unwrap()
            .insert(report.task_id, FinishedWorker { info, feed });
    }

    fn manager_role(&self) -> Option<&Role> {
        self.rt.roles.get(&self.manager.role)
    }

    /// The roles this manager may dispatch.
    pub fn dispatchable(&self) -> Vec<&Role> {
        match self.manager_role() {
            Some(m) => self.rt.roles.dispatchable(m),
            None => Vec::new(),
        }
    }

    fn available(&self) -> String {
        let ids: Vec<String> = self
            .dispatchable()
            .iter()
            .map(|r| format!("amp:{}", r.id))
            .collect();
        if ids.is_empty() {
            "none".to_owned()
        } else {
            ids.join(", ")
        }
    }

    /// `amp_run_ticket`'s description: what it does, plus every dispatchable role with its
    /// description.
    pub fn tool_description(&self) -> String {
        let mut text = format!(
            "Run a specialist agent on one AMP ticket and wait for it to finish. Marks the ticket \
             dispatched in AMP (amp_dispatch_task), gives the worker its own git worktree at \
             .worktrees/<task_id> when its role edits files, runs it on its own model, and returns \
             its final message and outcome (completed, blocked, stopped, error, timeout, cancelled \
             or turn_budget) as JSON. Call it once per ticket in the same message to run tickets in \
             parallel: at most {} run at once, extra calls wait for a free slot; each worker has \
             {} of wall-clock time. agent_id is the ticket's assigned_to.\n\nAgents:",
            self.config.max_parallel,
            human_duration(self.config.timeout),
        );
        for role in self.dispatchable() {
            let desc = role
                .description
                .split_whitespace()
                .collect::<Vec<_>>()
                .join(" ");
            text.push_str(&format!("\n- amp:{}: {desc}", role.id));
        }
        text
    }

    fn resolve_role(&self, agent: &str) -> Result<Role, RunTicketError> {
        let role = self
            .rt
            .roles
            .get(agent)
            .ok_or_else(|| RunTicketError::UnknownAgent {
                agent: agent.to_owned(),
                available: self.available(),
            })?;
        if !self.dispatchable().iter().any(|r| r.id == role.id) {
            return Err(RunTicketError::NotDispatchable {
                agent: agent.to_owned(),
                manager: self.manager.role.clone(),
                available: self.available(),
            });
        }
        Ok(role.clone())
    }

    /// Runs one ticket. `Err` means nothing was dispatched; once AMP has the dispatch, every end
    /// is an `Ok(WorkerReport)` (and [`WorkerHooks::on_finish`] has run).
    pub async fn run_ticket(
        &self,
        req: RunRequest,
        signal: CancellationToken,
    ) -> Result<WorkerReport, RunTicketError> {
        if req.task_id <= 0 {
            return Err(RunTicketError::BadArguments(format!(
                "task_id must be a ticket id, not {}",
                req.task_id
            )));
        }
        let role = self.resolve_role(&req.agent_id)?;
        let agent_id = format!("amp:{}", role.id);
        let _guard = self.claim(req.task_id, &agent_id)?;
        let (stop, stopped_by) = self.stop_handle(req.task_id);

        // Wait for a slot; a cancelled wait dispatches nothing.
        let permit = tokio::select! {
            biased;
            _ = signal.cancelled() => return Err(RunTicketError::Cancelled),
            _ = self.cancel.cancelled() => return Err(RunTicketError::Cancelled),
            _ = stop.cancelled() => {
                let by = stopped_by.lock().unwrap().clone().unwrap_or_default();
                return Err(RunTicketError::Stopped { task_id: req.task_id, by });
            }
            p = Arc::clone(&self.slots).acquire_owned() => p.map_err(|_| RunTicketError::Cancelled)?,
        };
        let _running = Running::start(self);

        call_json(
            &self.rt.mcp,
            "amp_dispatch_task",
            json!({"task_id": req.task_id, "agent_id": agent_id}),
        )
        .await
        .map_err(|e| {
            // #2226: AMP's refusals link the web UI; without a public URL the link is relative.
            // Same rewrite as every other AMP tool result (#2205), so the model and the TUI see
            // the web UI's absolute link, not `/project/71/plan`.
            let session = self.rt.mcp.session();
            RunTicketError::Dispatch {
                task_id: req.task_id,
                message: crate::links::web_links(
                    &e.to_string(),
                    &session.web_base(),
                    &session.mcp_origin(),
                )
                .into_owned(),
            }
        })?;
        if let Some(w) = self.live.lock().unwrap().get_mut(&req.task_id) {
            w.dispatched = true;
        }

        let report = self
            .run_dispatched(&req, &role, &agent_id, signal, stop, stopped_by)
            .await;
        drop(permit);
        Ok(report)
    }

    fn stop_handle(&self, task_id: i64) -> (CancellationToken, Arc<Mutex<Option<String>>>) {
        let live = self.live.lock().unwrap();
        match live.get(&task_id) {
            Some(w) => (w.stop.clone(), Arc::clone(&w.stopped_by)),
            None => (CancellationToken::new(), Arc::default()),
        }
    }

    fn claim(&self, task_id: i64, agent_id: &str) -> Result<LiveGuard<'_>, RunTicketError> {
        let mut live = self.live.lock().unwrap();
        if live.contains_key(&task_id) {
            return Err(RunTicketError::AlreadyRunning(task_id));
        }
        live.insert(
            task_id,
            LiveWorker {
                agent_id: agent_id.to_owned(),
                dispatched: false,
                watch: None,
                phase: WorkerPhase::Queued,
                model: None,
                cwd: None,
                since: SystemTime::now(),
                totals: None,
                feed: WorkerFeed::new(),
                stop: CancellationToken::new(),
                stopped_by: Arc::default(),
            },
        );
        Ok(LiveGuard { sup: self, task_id })
    }

    async fn run_dispatched(
        &self,
        req: &RunRequest,
        role: &Role,
        agent_id: &str,
        signal: CancellationToken,
        stop: CancellationToken,
        stopped_by: Arc<Mutex<Option<String>>>,
    ) -> WorkerReport {
        let started = Instant::now();
        let feed = self
            .live
            .lock()
            .unwrap()
            .get(&req.task_id)
            .map(|w| Arc::clone(&w.feed))
            .unwrap_or_default();
        let mut notes = Vec::new();
        let totals = Arc::new(Mutex::new(Totals::default()));
        let model = role
            .resolve_model(&self.rt.models)
            .map(|r| r.model_ref())
            .unwrap_or_default();

        // Where the worker runs.
        let wants_worktree = req
            .worktree
            .unwrap_or_else(|| role.tool_policy(&self.manager.cwd).exposes("edit"));
        let mut info = WorkerInfo {
            task_id: req.task_id,
            agent_id: agent_id.to_owned(),
            role: role.id.clone(),
            model,
            cwd: self.manager.cwd.clone(),
            worktree: None,
            branch: None,
            totals: Arc::clone(&totals),
        };
        if wants_worktree {
            match self.worktree(req).await {
                Ok(Some(wt)) => {
                    if wt.reused {
                        notes.push(format!(
                            "reused the existing worktree {}",
                            wt.path.display()
                        ));
                    }
                    info.cwd = wt.path.clone();
                    info.worktree = Some(wt.path);
                    info.branch = wt.branch;
                }
                Ok(None) => notes.push(format!(
                    "{} is not in a git repository; the worker ran there without a worktree",
                    self.manager.cwd.display()
                )),
                Err(e) => {
                    return self
                        .finish(
                            &info,
                            started,
                            notes,
                            Finish::error(format!("worktree: {e}")),
                        )
                        .await;
                }
            }
        }
        {
            let mut live = self.live.lock().unwrap();
            if let Some(w) = live.get_mut(&req.task_id) {
                w.phase = WorkerPhase::Running;
                w.model = Some(info.model.clone());
                w.cwd = Some(info.cwd.clone());
                w.since = SystemTime::now();
                w.totals = Some(Arc::clone(&totals));
            }
        }

        // Build the worker's own harness.
        let budget = CancellationToken::new();
        let watch = Arc::new(WorkerWatch {
            task_id: req.task_id,
            totals: Arc::clone(&totals),
            max_turns: self.config.max_turns,
            budget: budget.clone(),
            closing: Mutex::new(HashMap::new()),
            closed: Mutex::new(None),
            repeat_stop: Mutex::new(None),
            feed,
        });
        if let Some(w) = self.live.lock().unwrap().get_mut(&req.task_id) {
            w.watch = Some(Arc::clone(&watch));
        }
        let mut spec = AgentSpec::new(role.id.clone(), info.cwd.clone());
        spec.project = self.manager.project.clone();
        spec.extra_context = Some(worker_context(&info, &self.manager.role));
        spec.session = SessionKind::Jsonl {
            dir: self
                .rt
                .state_dir
                .join("sessions")
                .join(req.task_id.to_string()),
        };
        spec.emitter = Some(Arc::new(WorkerEmitter {
            watch: Arc::clone(&watch),
        }));
        if let Some(hooks) = &self.hooks {
            spec.before_tool_call = hooks.before_tool_call(&info);
            spec.extra_tools.extend(hooks.extra_tools(&info));
        }
        let built = match self.rt.build(spec).await {
            Ok(b) => b,
            Err(e) => {
                return self
                    .finish(
                        &info,
                        started,
                        notes,
                        Finish::error(format!("build the worker: {e}")),
                    )
                    .await;
            }
        };
        info.model = built.resolved.model_ref();
        let session_file = built.session_file.clone();
        let harness = Arc::clone(&built.harness);
        let prompt = worker_prompt(&info, req, self.manager.project.as_ref());
        // The chat's session view starts with the prompt (#2252); rpi doesn't emit it.
        watch.feed.push(&AgentEvent::MessageStart {
            message: rpi_agent::AgentMessage::User(rpi_ai::types::UserMessage {
                role: Default::default(),
                content: rpi_ai::types::UserContent::Text(prompt.clone()),
                timestamp: SystemTime::now()
                    .duration_since(SystemTime::UNIX_EPOCH)
                    .map_or(0, |d| d.as_millis() as i64),
            }),
        });

        // Run it in the foreground of this call.
        let run = harness.prompt_text(&prompt, Vec::new());
        tokio::pin!(run);
        let deadline = tokio::time::sleep(self.config.timeout);
        tokio::pin!(deadline);
        let stop = tokio::select! {
            biased;
            r = &mut run => Stop::Returned(r),
            _ = signal.cancelled() => Stop::Ended(WorkerOutcome::Cancelled, "the manager's run was cancelled".to_owned()),
            _ = self.cancel.cancelled() => Stop::Ended(WorkerOutcome::Cancelled, "every worker was cancelled".to_owned()),
            _ = &mut deadline => Stop::Ended(WorkerOutcome::Timeout, format!("no result after {}", human_duration(self.config.timeout))),
            _ = budget.cancelled() => Stop::Ended(WorkerOutcome::TurnBudget, format!("turn budget of {} turns used up", self.config.max_turns)),
            _ = stop.cancelled() => {
                let by = stopped_by.lock().unwrap().clone().unwrap_or_default();
                Stop::Ended(WorkerOutcome::Cancelled, stopped_from_chat(&by))
            }
        };
        let result = match stop {
            Stop::Returned(r) => Some(r),
            Stop::Ended(outcome, why) => {
                let _ = harness.abort().await;
                let settled = tokio::time::timeout(self.config.abort_grace, &mut run).await;
                if settled.is_err() {
                    notes.push(format!(
                        "the worker did not stop within {} of the abort and was dropped",
                        human_duration(self.config.abort_grace)
                    ));
                }
                let last = totals.lock().unwrap().last_text.clone();
                let mut finish = Finish {
                    outcome,
                    error: Some(why),
                    text: last,
                    session_file,
                };
                if let Ok(Ok(r)) = settled
                    && let Some(text) = outcome_text(&r.outcome)
                    && !text.is_empty()
                {
                    finish.text = text;
                }
                // Read after the abort settled: a closing call can end during the grace period.
                let closed = *watch.closed.lock().unwrap();
                if closed.is_none()
                    && let Some(by) = stopped_by.lock().unwrap().clone()
                {
                    // #2252: the manager reads this in amp_run_ticket's result.
                    notes.push(format!(
                        "{by} watched this worker and stopped it from the chat; ask them \
                         before running #{} again",
                        req.task_id
                    ));
                }
                let finish = finish.after_closing(closed, &mut notes);
                return self.finish(&info, started, notes, finish).await;
            }
        };

        let closed = *watch.closed.lock().unwrap();
        let last = totals.lock().unwrap().last_text.clone();
        let mut finish = match result {
            Some(Ok(r)) => match r.outcome {
                HarnessRunOutcome::Completed { final_message, .. } => Finish {
                    outcome: closed.unwrap_or(WorkerOutcome::Stopped),
                    // #2225: say why it stopped (the abandoned run's note carries it).
                    error: closed
                        .is_none()
                        .then(|| {
                            watch
                                .repeat_stop
                                .lock()
                                .unwrap()
                                .as_ref()
                                .map(|s| s.reason())
                        })
                        .flatten(),
                    text: assistant_text(&final_message),
                    session_file: None,
                },
                HarnessRunOutcome::Aborted { final_message, .. } => Finish {
                    outcome: WorkerOutcome::Cancelled,
                    error: Some("the worker's run was aborted".to_owned()),
                    text: assistant_text(&final_message),
                    session_file: None,
                },
                HarnessRunOutcome::Failed {
                    error,
                    final_message,
                    ..
                } => Finish {
                    outcome: WorkerOutcome::Error,
                    // One line, no response body (#2207): the manager reads this as a tool result.
                    error: Some(crate::provider_error::run_failure(
                        &error.code,
                        &error.message,
                        final_message.as_ref().map_or("", |m| m.provider.as_str()),
                    )),
                    text: final_message
                        .as_ref()
                        .map(assistant_text)
                        .unwrap_or_default(),
                    session_file: None,
                },
                HarnessRunOutcome::Suspended { .. } => Finish::error(
                    "the provider deferred its response (suspended runs are not supported)",
                ),
            },
            Some(Err(e)) => Finish::error(format!("{e}")),
            None => Finish::error("no result"),
        };
        if finish.text.is_empty() {
            finish.text = last;
        }
        finish.session_file = session_file;
        let finish = finish.after_closing(closed, &mut notes);
        self.finish(&info, started, notes, finish).await
    }

    async fn finish(
        &self,
        info: &WorkerInfo,
        started: Instant,
        notes: Vec<String>,
        finish: Finish,
    ) -> WorkerReport {
        let totals = info.totals.lock().unwrap().clone();
        let mut report = WorkerReport {
            task_id: info.task_id,
            agent_id: info.agent_id.clone(),
            role: info.role.clone(),
            outcome: finish.outcome,
            final_message: finish.text,
            error: finish.error,
            model: info.model.clone(),
            cwd: info.cwd.clone(),
            worktree: info.worktree.clone(),
            branch: info.branch.clone(),
            session_file: finish.session_file,
            turns: totals.turns,
            tool_calls: totals.tool_calls,
            tool_errors: totals.tool_errors,
            usage: totals.usage,
            duration_ms: started.elapsed().as_millis() as u64,
            notes,
            totals,
        };
        if let Some(hooks) = &self.hooks {
            hooks.on_finish(info, &mut report).await;
        }
        self.record_finished(&report);
        report
    }

    /// Creates or reuses `<repo>/.worktrees/<task_id>`. `Ok(None)` when the manager is not in a
    /// git repository.
    async fn worktree(&self, req: &RunRequest) -> Result<Option<Worktree>, String> {
        let _lock = self.git_lock.lock().await;
        let Ok(top) = git(&self.manager.cwd, &["rev-parse", "--show-toplevel"]).await else {
            return Ok(None);
        };
        let top = PathBuf::from(top);
        let path = top.join(".worktrees").join(req.task_id.to_string());
        exclude_worktrees_dir(&top).await;

        if path.exists() {
            let inner = git(&path, &["rev-parse", "--show-toplevel"])
                .await
                .map_err(|e| format!("{} exists but is not a git worktree: {e}", path.display()))?;
            if !same_path(Path::new(&inner), &path) {
                return Err(format!(
                    "{} exists but is not a git worktree (it belongs to {inner})",
                    path.display()
                ));
            }
            let branch = git(&path, &["branch", "--show-current"])
                .await
                .ok()
                .filter(|b| !b.is_empty());
            return Ok(Some(Worktree {
                path,
                branch,
                reused: true,
            }));
        }

        let branch = req
            .branch
            .clone()
            .filter(|b| !b.trim().is_empty())
            .unwrap_or_else(|| format!("amp-{}", req.task_id));
        let path_str = path.to_string_lossy().into_owned();
        let exists = git(
            &top,
            &[
                "rev-parse",
                "--verify",
                "--quiet",
                &format!("refs/heads/{branch}"),
            ],
        )
        .await
        .is_ok();
        if exists {
            git(&top, &["worktree", "add", &path_str, &branch]).await?;
        } else {
            let base = req.base.clone().unwrap_or_else(|| "HEAD".to_owned());
            git(&top, &["worktree", "add", "-b", &branch, &path_str, &base]).await?;
        }
        Ok(Some(Worktree {
            path,
            branch: Some(branch),
            reused: false,
        }))
    }
}

struct Worktree {
    path: PathBuf,
    branch: Option<String>,
    reused: bool,
}

enum Stop<R> {
    Returned(R),
    Ended(WorkerOutcome, String),
}

struct Finish {
    outcome: WorkerOutcome,
    error: Option<String>,
    text: String,
    session_file: Option<PathBuf>,
}

impl Finish {
    fn error(message: impl Into<String>) -> Self {
        Self {
            outcome: WorkerOutcome::Error,
            error: Some(message.into()),
            text: String::new(),
            session_file: None,
        }
    }

    /// A worker whose `amp_complete_task` / `amp_block_task` on its own ticket already succeeded
    /// keeps that outcome, however its run then ended (cancelled, timed out, over its turn budget,
    /// aborted or failed on a later turn). The ticket is closed and its run recorded with the
    /// closing call, so this is not an abandoned run: reporting one would count the work as rework
    /// and its tokens twice. The reason it stopped becomes a note, not an error.
    fn after_closing(self, closed: Option<WorkerOutcome>, notes: &mut Vec<String>) -> Self {
        let Some(closed) = closed else {
            return self;
        };
        if self.outcome.closed_ticket() {
            return self;
        }
        let mut why = self.outcome.as_str().to_owned();
        if let Some(err) = self
            .error
            .as_deref()
            .map(str::trim)
            .filter(|e| !e.is_empty())
        {
            why.push_str(": ");
            why.push_str(err);
        }
        notes.push(format!("stopped after closing its ticket: {why}"));
        Self {
            outcome: closed,
            error: None,
            ..self
        }
    }
}

fn outcome_text(outcome: &HarnessRunOutcome) -> Option<String> {
    match outcome {
        HarnessRunOutcome::Completed { final_message, .. }
        | HarnessRunOutcome::Aborted { final_message, .. } => Some(assistant_text(final_message)),
        HarnessRunOutcome::Failed { final_message, .. } => {
            final_message.as_ref().map(assistant_text)
        }
        HarnessRunOutcome::Suspended { .. } => None,
    }
}

/// Removes the ticket from the live map when the call ends, however it ends.
struct LiveGuard<'a> {
    sup: &'a Supervisor,
    task_id: i64,
}

impl Drop for LiveGuard<'_> {
    fn drop(&mut self) {
        if let Ok(mut live) = self.sup.live.lock() {
            live.remove(&self.task_id);
        }
    }
}

/// Counts running workers and records the peak.
struct Running<'a>(&'a Supervisor);

impl<'a> Running<'a> {
    fn start(sup: &'a Supervisor) -> Self {
        let now = sup.running.fetch_add(1, Ordering::SeqCst) + 1;
        sup.peak.fetch_max(now, Ordering::SeqCst);
        Self(sup)
    }
}

impl Drop for Running<'_> {
    fn drop(&mut self) {
        self.0.running.fetch_sub(1, Ordering::SeqCst);
    }
}

/// Watches a worker's events as they are emitted: usage totals, the turn budget, and whether
/// the worker closed its ticket.
struct WorkerWatch {
    task_id: i64,
    totals: Arc<Mutex<Totals>>,
    max_turns: u32,
    budget: CancellationToken,
    /// Tool call id -> the outcome it means, for calls that close this ticket.
    closing: Mutex<HashMap<String, WorkerOutcome>>,
    closed: Mutex<Option<WorkerOutcome>>,
    /// #2225: the repeat guard stopped the worker's model.
    repeat_stop: Mutex<Option<crate::repeat::RepeatStop>>,
    /// The chat's session view reads the worker's events from here (#2252).
    feed: Arc<WorkerFeed>,
}

impl WorkerWatch {
    fn observe(&self, event: &AgentEvent) {
        self.feed.push(event);
        let Some(e) = RunEvent::from_agent(event) else {
            return;
        };
        let turns = {
            let mut t = self.totals.lock().unwrap();
            t.observe(&e);
            t.turns
        };
        match &e {
            RunEvent::TurnStart if self.max_turns > 0 && turns > self.max_turns => {
                self.budget.cancel();
            }
            RunEvent::ToolStart {
                id,
                name,
                arguments,
            } => {
                let outcome = match name.as_str() {
                    COMPLETE_TOOL => Some(WorkerOutcome::Completed),
                    BLOCK_TOOL => Some(WorkerOutcome::Blocked),
                    _ => None,
                };
                if let Some(outcome) = outcome
                    && task_id_arg(arguments) == Some(self.task_id)
                {
                    self.closing.lock().unwrap().insert(id.clone(), outcome);
                }
            }
            RunEvent::ToolEnd {
                id, is_error, text, ..
            } => {
                if let Some(outcome) = self.closing.lock().unwrap().remove(id)
                    && !is_error
                {
                    *self.closed.lock().unwrap() = Some(outcome);
                }
                if *is_error && let Some(stop) = crate::repeat::stopped(text) {
                    *self.repeat_stop.lock().unwrap() = Some(stop);
                }
            }
            _ => {}
        }
    }
}

fn live_info(task_id: i64, w: &LiveWorker) -> WorkerSessionInfo {
    WorkerSessionInfo {
        task_id,
        agent_id: w.agent_id.clone(),
        phase: Some(w.phase),
        model: w.model.clone(),
        cwd: w.cwd.clone(),
        since: w.since,
        ended: None,
        totals: w
            .totals
            .as_ref()
            .map(|t| t.lock().unwrap().clone())
            .unwrap_or_default(),
        outcome: None,
        error: None,
        stopped_by: w.stopped_by.lock().unwrap().clone(),
        session_file: None,
    }
}

/// A tool call's `task_id` argument: a number, or a string such as `"#12"`.
pub(crate) fn task_id_arg(args: &Value) -> Option<i64> {
    match &args["task_id"] {
        Value::Number(n) => n.as_i64().or_else(|| n.as_f64().map(|f| f as i64)),
        Value::String(s) => s.trim().trim_start_matches('#').parse().ok(),
        _ => None,
    }
}

/// The worker's `agent_emitter`: updates the watch synchronously, so totals are current when the
/// next tool hook runs.
struct WorkerEmitter {
    watch: Arc<WorkerWatch>,
}

impl AgentEmitter for WorkerEmitter {
    fn emit(
        &self,
        event: AgentEvent,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send + 'static>> {
        self.watch.observe(&event);
        Box::pin(std::future::ready(()))
    }

    fn try_emit(&self, event: AgentEvent) {
        self.watch.observe(&event);
    }
}

fn worker_context(info: &WorkerInfo, manager: &str) -> String {
    let mut lines = vec![
        format!("ticket: #{}", info.task_id),
        format!("agent_id: {}", info.agent_id),
        format!("dispatched_by: {manager} (amp_run_ticket)"),
    ];
    match (&info.worktree, &info.branch) {
        (Some(wt), branch) => {
            lines.push(format!("worktree: {}", wt.display()));
            if let Some(b) = branch {
                lines.push(format!("branch: {b}"));
            }
        }
        (None, _) => lines.push("worktree: none (working in the manager's directory)".to_owned()),
    }
    lines.join("\n")
}

fn worker_prompt(info: &WorkerInfo, req: &RunRequest, project: Option<&ProjectMarker>) -> String {
    let mut text = format!(
        "You are working AMP ticket #{}{}. Read it with amp_get_task (include comments): its \
         description and acceptance criteria are the spec. Follow your execution protocol, and \
         finish by completing the ticket (amp_complete_task) or blocking it with a question \
         (amp_block_task).",
        info.task_id,
        project
            .map(|p| format!(" in project {}", p.project_id))
            .unwrap_or_default(),
    );
    match (&info.worktree, &info.branch) {
        (Some(wt), Some(branch)) => text.push_str(&format!(
            "\n\nYour working directory is the git worktree {} on branch {branch}; commit your work there.",
            wt.display()
        )),
        (Some(wt), None) => text.push_str(&format!(
            "\n\nYour working directory is the git worktree {}.",
            wt.display()
        )),
        (None, _) => text.push_str(&format!(
            "\n\nYour working directory is {}.",
            info.cwd.display()
        )),
    }
    if let Some(extra) = req
        .instructions
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty())
    {
        text.push_str("\n\nFrom the manager:\n");
        text.push_str(extra);
    }
    text
}

fn human_duration(d: Duration) -> String {
    let secs = d.as_secs();
    if secs >= 60 && secs.is_multiple_of(60) {
        format!("{} min", secs / 60)
    } else if secs >= 1 {
        format!("{secs} s")
    } else {
        format!("{} ms", d.as_millis())
    }
}

fn same_path(a: &Path, b: &Path) -> bool {
    match (std::fs::canonicalize(a), std::fs::canonicalize(b)) {
        (Ok(a), Ok(b)) => a == b,
        _ => a == b,
    }
}

/// Runs git in `dir`; stdout trimmed, or stderr as the error.
async fn git(dir: &Path, args: &[&str]) -> Result<String, String> {
    let out = tokio::time::timeout(
        Duration::from_secs(120),
        tokio::process::Command::new("git")
            .arg("-C")
            .arg(dir)
            .args(args)
            .env("GIT_TERMINAL_PROMPT", "0")
            .kill_on_drop(true)
            .output(),
    )
    .await
    .map_err(|_| format!("git {} timed out", args.join(" ")))?
    .map_err(|e| format!("run git: {e}"))?;
    if out.status.success() {
        Ok(String::from_utf8_lossy(&out.stdout).trim().to_owned())
    } else {
        Err(format!(
            "git {}: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr).trim()
        ))
    }
}

/// Adds `.worktrees/` to the repo's `info/exclude` unless something already ignores it, so the
/// workers' checkouts never show up as untracked files. Best effort.
async fn exclude_worktrees_dir(top: &Path) {
    if git(top, &["check-ignore", "-q", ".worktrees/x"])
        .await
        .is_ok()
    {
        return;
    }
    let Ok(common) = git(top, &["rev-parse", "--git-common-dir"]).await else {
        return;
    };
    let common = if Path::new(&common).is_absolute() {
        PathBuf::from(common)
    } else {
        top.join(common)
    };
    let file = common.join("info").join("exclude");
    let mut text = std::fs::read_to_string(&file).unwrap_or_default();
    if !text.is_empty() && !text.ends_with('\n') {
        text.push('\n');
    }
    text.push_str("# amp-harness worker worktrees\n.worktrees/\n");
    let _ = std::fs::create_dir_all(common.join("info"));
    let _ = std::fs::write(&file, text);
}

/// The manager's `amp_run_ticket` tool.
pub struct RunTicketTool {
    sup: Arc<Supervisor>,
    schema: Tool,
}

impl RunTicketTool {
    pub fn new(sup: Arc<Supervisor>) -> Self {
        let schema = Tool {
            name: DISPATCH_TOOL.to_owned(),
            description: sup.tool_description(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "task_id": {"type": "integer", "description": "REQUIRED. The ticket to run."},
                    "agent_id": {"type": "string", "description": "REQUIRED. The agent to run it, e.g. amp:amp-worker-backend (the ticket's assigned_to)."},
                    "instructions": {"type": "string", "description": "Optional. Extra instructions for the worker, added to its prompt (the ticket itself is the spec)."},
                    "worktree": {"type": "boolean", "description": "Optional. Force a git worktree on or off. Default: on for agents that edit files."},
                    "branch": {"type": "string", "description": "Optional. Branch for a new worktree. Default amp-<task_id>."},
                    "base": {"type": "string", "description": "Optional. Where a new branch starts. Default HEAD."}
                },
                "required": ["task_id", "agent_id"],
                "additionalProperties": false
            })
            .into(),
            constrained_sampling: None,
        };
        Self { sup, schema }
    }
}

impl std::fmt::Debug for RunTicketTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RunTicketTool").finish_non_exhaustive()
    }
}

/// The arguments as the model sends them: `task_id` may come as a string.
fn parse_request(params: Value) -> Result<RunRequest, String> {
    let mut params = match params {
        Value::Object(map) => map,
        other => return Err(format!("arguments must be a JSON object, not {other}")),
    };
    if let Some(v) = params.get("task_id").cloned() {
        let id = task_id_arg(&json!({ "task_id": v }))
            .ok_or_else(|| format!("task_id must be a number, not {v}"))?;
        params.insert("task_id".into(), json!(id));
    }
    serde_json::from_value(Value::Object(params)).map_err(|e| e.to_string())
}

#[async_trait::async_trait]
impl AgentTool for RunTicketTool {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        "Run ticket"
    }

    async fn execute(
        &self,
        _tool_call_id: &str,
        params: Value,
        signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, ToolError> {
        let req = parse_request(params).map_err(ToolError::Validation)?;
        match self.sup.run_ticket(req, signal).await {
            Ok(report) => {
                let value = serde_json::to_value(&report).unwrap_or(Value::Null);
                let mut out =
                    AgentToolResult::text(serde_json::to_string_pretty(&value).unwrap_or_default());
                out.details = value;
                Ok(out)
            }
            Err(RunTicketError::Cancelled) => Err(ToolError::Abort),
            Err(e) => Err(ToolError::Tool(e.to_string())),
        }
    }
}

/// Tickets with a worker in this process (for tests and diagnostics).
pub fn live_tickets(sup: &Supervisor) -> BTreeSet<i64> {
    sup.live.lock().unwrap().keys().copied().collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_layers_defaults_toml_and_env() {
        let mut c = SupervisorConfig::default();
        assert_eq!(c.max_parallel, 4);
        assert_eq!(c.timeout, Duration::from_secs(3600));
        c.apply_toml("[server]\nurl = \"x\"\n[workers]\nmax_parallel = 2\ntimeout_minutes = 0.5\nmax_turns = 9\n")
            .unwrap();
        assert_eq!(
            (c.max_parallel, c.timeout, c.max_turns),
            (2, Duration::from_secs(30), 9)
        );
        let env = |k: &str| match k {
            ENV_MAX_WORKERS => Some("6".to_owned()),
            ENV_WORKER_TIMEOUT_SECS => Some("5".to_owned()),
            _ => None,
        };
        c.apply_env(&env).unwrap();
        assert_eq!((c.max_parallel, c.timeout), (6, Duration::from_secs(5)));

        assert!(c.apply_toml("[workers]\nmax_parallel = 0\n").is_err());
        assert!(c.apply_toml("[workers]\nmaxparallel = 3\n").is_err());
        let bad = |k: &str| (k == ENV_MAX_WORKERS).then(|| "four".to_owned());
        assert!(c.apply_env(&bad).unwrap_err().contains(ENV_MAX_WORKERS));
    }

    #[test]
    fn requests_accept_string_ids_and_reject_unknown_fields() {
        let r =
            parse_request(json!({"task_id": "#12", "agent_id": "amp:amp-worker-docs"})).unwrap();
        assert_eq!(r.task_id, 12);
        assert!(parse_request(json!({"task_id": 1, "agent_id": "x", "agnet": 1})).is_err());
        assert!(parse_request(json!({"agent_id": "x"})).is_err());
        assert!(parse_request(json!([1])).is_err());
    }

    #[test]
    fn closing_calls_only_count_for_this_ticket_and_only_when_they_succeed() {
        let watch = WorkerWatch {
            task_id: 7,
            totals: Arc::default(),
            max_turns: 2,
            budget: CancellationToken::new(),
            closing: Mutex::default(),
            closed: Mutex::default(),
            repeat_stop: Mutex::default(),
            feed: WorkerFeed::new(),
        };
        let start = |id: &str, name: &str, task: i64| AgentEvent::ToolExecutionStart {
            tool_call_id: id.into(),
            tool_name: name.into(),
            args: json!({"task_id": task}),
        };
        let end = |id: &str, name: &str, is_error: bool| AgentEvent::ToolExecutionEnd {
            tool_call_id: id.into(),
            tool_name: name.into(),
            result: AgentToolResult::text("x"),
            is_error,
        };
        watch.observe(&start("a", COMPLETE_TOOL, 8));
        watch.observe(&end("a", COMPLETE_TOOL, false));
        assert_eq!(*watch.closed.lock().unwrap(), None, "another ticket");
        watch.observe(&start("b", COMPLETE_TOOL, 7));
        watch.observe(&end("b", COMPLETE_TOOL, true));
        assert_eq!(*watch.closed.lock().unwrap(), None, "a failed call");
        watch.observe(&start("c", BLOCK_TOOL, 7));
        watch.observe(&end("c", BLOCK_TOOL, false));
        assert_eq!(*watch.closed.lock().unwrap(), Some(WorkerOutcome::Blocked));

        for _ in 0..2 {
            watch.observe(&AgentEvent::TurnStart);
        }
        assert!(!watch.budget.is_cancelled());
        watch.observe(&AgentEvent::TurnStart);
        assert!(watch.budget.is_cancelled(), "the third turn is over budget");
    }

    #[test]
    fn outcomes_serialize_as_snake_case_words() {
        for o in [
            WorkerOutcome::Completed,
            WorkerOutcome::Blocked,
            WorkerOutcome::Stopped,
            WorkerOutcome::Error,
            WorkerOutcome::Timeout,
            WorkerOutcome::Cancelled,
            WorkerOutcome::TurnBudget,
        ] {
            assert_eq!(serde_json::to_value(o).unwrap(), json!(o.as_str()));
        }
        assert!(WorkerOutcome::Blocked.closed_ticket());
        assert!(!WorkerOutcome::Stopped.closed_ticket());
    }

    #[test]
    fn a_closed_ticket_outlives_how_the_run_ended() {
        let stopped = |outcome, error: Option<&str>| Finish {
            outcome,
            error: error.map(str::to_owned),
            text: String::new(),
            session_file: None,
        };
        // Closed, then budget-stopped: keeps the closing outcome; the reason is a note.
        let mut notes = Vec::new();
        let f = stopped(
            WorkerOutcome::TurnBudget,
            Some("turn budget of 1 turns used up"),
        )
        .after_closing(Some(WorkerOutcome::Completed), &mut notes);
        assert_eq!((f.outcome, f.error), (WorkerOutcome::Completed, None));
        assert_eq!(
            notes,
            vec!["stopped after closing its ticket: turn_budget: turn budget of 1 turns used up"]
        );
        // Never closed: unchanged.
        let mut notes = Vec::new();
        let f = stopped(WorkerOutcome::Cancelled, Some("every worker was cancelled"))
            .after_closing(None, &mut notes);
        assert_eq!(f.outcome, WorkerOutcome::Cancelled);
        assert!(f.error.is_some() && notes.is_empty());
        // Already the closing outcome: no note.
        let f = stopped(WorkerOutcome::Blocked, None)
            .after_closing(Some(WorkerOutcome::Blocked), &mut notes);
        assert_eq!(f.outcome, WorkerOutcome::Blocked);
        assert!(notes.is_empty());
    }

    #[test]
    fn durations_read_naturally() {
        assert_eq!(human_duration(Duration::from_secs(3600)), "60 min");
        assert_eq!(human_duration(Duration::from_secs(90)), "90 s");
        assert_eq!(human_duration(Duration::from_millis(300)), "300 ms");
    }
}
