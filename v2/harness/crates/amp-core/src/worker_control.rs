//! The manager's worker-control tools (story 582): `amp_stop_worker` (#4233) stops one of the
//! workers this process's [`Supervisor`] runs; `amp_restart_worker` and `amp_resume_worker`
//! (#4240) stop it if it is live, wait until it is not, and run the ticket again through the
//! normal dispatch path ([`Supervisor::launch`]: a new `amp_dispatch_task`, a claim with the new
//! dispatch_id, a ledger record), so a second live worker on one ticket stays impossible.
//!
//! Register them with [`tools`], next to the supervisor's `amp_run_ticket`, both when the
//! manager's session is built and on `/reload`; nothing else needs to change when a tool is added.
//! Only the manager gets them: a worker never receives the manager's extra tools, and
//! [`crate::policy`] refuses them to every child role all the same
//! ([`crate::policy::DenialReason::ChildWorkerControl`]).
//!
//! Scope is this process's supervisor: a ticket it never ran is `not_found`. A stop never closes
//! the ticket: the run ends `cancelled` with the error `stopped by manager: <reason>`, run
//! reporting records it abandoned with that note and hands the ticket back to ready (#2204,
//! #2802), and the tool waits (bounded) for that to finish so it can say what really happened.

use std::{
    collections::HashSet,
    path::Path,
    sync::{Arc, LazyLock, Mutex, PoisonError},
    time::Duration,
};

use rpi_agent::{AgentError as ToolError, AgentTool, AgentToolResult, ToolResultPartial};
use rpi_ai::types::Tool;
use rpi_harness::types::HarnessTool;
use serde::Serialize;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    policy::{RESTART_WORKER_TOOL, RESUME_WORKER_TOOL, STOP_WORKER_TOOL},
    run_report::ResumeMode,
    supervisor::{
        ControlNote, DispatchMode, RunRequest, RunTicketError, StopBy, StopError, Supervisor,
        TicketHold, WorkerSessionInfo, task_id_arg,
    },
};

/// The longest reason `amp_stop_worker` takes, in characters.
pub const REASON_MAX_CHARS: usize = 200;

/// How much longer than the supervisor's `abort_grace` a stop waits for the run's end: the
/// hand-back after it is an MCP round-trip (readiness review #3138).
pub const STOP_WAIT_SLACK: Duration = Duration::from_secs(20);

/// The manager's worker-control tools, for `sup`. Call it wherever `sup.tool()` is registered.
pub fn tools(sup: &Arc<Supervisor>) -> Vec<HarnessTool> {
    vec![
        HarnessTool::new(Arc::new(StopWorkerTool::new(Arc::clone(sup)))),
        HarnessTool::new(Arc::new(RelaunchTool::restart(Arc::clone(sup)))),
        HarnessTool::new(Arc::new(RelaunchTool::resume(Arc::clone(sup)))),
    ]
}

/// What a stop found or did (`status` in the tool's result).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum StopStatus {
    /// The run ended and its ticket is back in ready.
    Stopped,
    /// The run ended but handing the ticket back failed: it may still be in progress in AMP.
    StoppedNotReturned,
    /// Someone had already asked this worker to stop; nothing more was done.
    AlreadyStopping,
    /// The worker had already finished; nothing was done.
    AlreadyFinished,
    /// The worker completed or blocked its ticket while it was being stopped: that stands.
    ClosedBeforeStop,
    /// It was still waiting for a slot: nothing was dispatched, the ticket is untouched.
    NotDispatched,
    /// The run had not ended when the wait ran out; it will, and its ticket will be handed back.
    Stopping,
    /// This chat has no worker on that ticket.
    NotFound,
}

impl StopStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Stopped => "stopped",
            Self::StoppedNotReturned => "stopped_not_returned",
            Self::AlreadyStopping => "already_stopping",
            Self::AlreadyFinished => "already_finished",
            Self::ClosedBeforeStop => "closed_before_stop",
            Self::NotDispatched => "not_dispatched",
            Self::Stopping => "stopping",
            Self::NotFound => "not_found",
        }
    }
}

/// `amp_stop_worker`'s result.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct StopResult {
    pub task_id: i64,
    /// Always `"stop"` (restart and resume share the shape, #4240).
    pub action: &'static str,
    pub status: StopStatus,
    /// How the run ended (`cancelled`, or `completed`/`blocked` for `closed_before_stop`).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub outcome: Option<String>,
    /// Whether its ticket was handed back to ready (`None`: no hand-back was attempted or known).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub returned: Option<bool>,
    /// Why the hand-back failed, when it did.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub return_error: Option<String>,
    /// What the manager should know, in words.
    pub note: String,
}

/// The reason as a stop records it: one line, whitespace collapsed, required, at most
/// [`REASON_MAX_CHARS`].
pub fn normalize_reason(reason: &str) -> Result<String, String> {
    let reason = reason.split_whitespace().collect::<Vec<_>>().join(" ");
    if reason.is_empty() {
        return Err("reason is required: say in a few words why the worker is stopped".into());
    }
    let n = reason.chars().count();
    if n > REASON_MAX_CHARS {
        return Err(format!(
            "reason is {n} characters; keep it to {REASON_MAX_CHARS} (one line)"
        ));
    }
    Ok(reason)
}

/// Stops the worker on `task_id` for the manager, with `reason`, and waits at most `bound` for
/// its run to end and its ticket to be handed back. `cancel` ends the wait early (the manager's
/// turn was aborted): the stop stands and the result says `stopping`.
///
/// Never calls `amp_complete_task`: the abandoned run and the hand-back are run reporting's
/// (`WorkerHooks::on_finish`).
pub async fn stop_worker(
    sup: &Supervisor,
    task_id: i64,
    reason: &str,
    bound: Duration,
    cancel: &CancellationToken,
) -> StopResult {
    // The finished entry a run of this stop will replace, to tell it from an earlier run's.
    let before = sup.finished_session(task_id).and_then(|s| s.ended);
    let by = StopBy::Manager {
        reason: reason.to_owned(),
    };
    let result = |status, note: String| StopResult {
        task_id,
        action: "stop",
        status,
        outcome: None,
        returned: None,
        return_error: None,
        note,
    };
    match sup.stop_worker_as(task_id, by) {
        Err(StopError::NotFound(_)) => {
            return result(
                StopStatus::NotFound,
                format!(
                    "#{task_id} has no worker in this chat. amp_stop_worker only stops workers \
                     this chat started with amp_run_ticket."
                ),
            );
        }
        Err(StopError::AlreadyStopping(_)) => {
            return result(
                StopStatus::AlreadyStopping,
                format!(
                    "the worker on #{task_id} is already stopping; its end will arrive in your \
                     worker summaries."
                ),
            );
        }
        Err(StopError::Finished(_)) => {
            let mut r = result(
                StopStatus::AlreadyFinished,
                format!("the worker on #{task_id} had already finished; nothing was stopped."),
            );
            if let Some(s) = sup.finished_session(task_id) {
                fill_end(&mut r, &s);
            }
            return r;
        }
        Ok(()) => {}
    }

    let ended = tokio::select! {
        ended = sup.wait_not_live(task_id, bound) => ended,
        _ = cancel.cancelled() => false,
    };
    if !ended {
        return result(
            StopStatus::Stopping,
            format!(
                "the worker on #{task_id} was told to stop but had not ended after {}s. It will \
                 end as cancelled and its ticket will be handed back; its end arrives in your \
                 worker summaries.",
                bound.as_secs()
            ),
        );
    }
    match sup
        .finished_session(task_id)
        .filter(|s| s.ended.is_some() && s.ended != before)
    {
        // Ended without a run of its own: it was still waiting for a slot.
        None => result(
            StopStatus::NotDispatched,
            format!(
                "the worker on #{task_id} was still waiting for a slot; nothing was dispatched \
                 and the ticket was not touched."
            ),
        ),
        Some(s) => {
            let closed = s.outcome.is_some_and(|o| o.closed_ticket());
            let mut r = result(StopStatus::Stopped, String::new());
            fill_end(&mut r, &s);
            if closed {
                r.status = StopStatus::ClosedBeforeStop;
                r.note = format!(
                    "the worker on #{task_id} closed its ticket ({}) before it stopped; that \
                     stands and nothing was handed back.",
                    r.outcome.as_deref().unwrap_or("closed")
                );
            } else if let Some(Err(e)) = &s.returned {
                r.status = StopStatus::StoppedNotReturned;
                r.note = format!(
                    "the worker on #{task_id} stopped, but its ticket could not be returned to \
                     ready: {e}. It may still be in progress in AMP."
                );
            } else {
                r.note = format!(
                    "the worker on #{task_id} stopped; its run was recorded abandoned \
                     ({}) and the ticket is back in ready.",
                    s.error.as_deref().unwrap_or("cancelled")
                );
            }
            r
        }
    }
}

fn fill_end(r: &mut StopResult, s: &WorkerSessionInfo) {
    r.outcome = s.outcome.map(|o| o.as_str().to_owned());
    match &s.returned {
        Some(Ok(())) => r.returned = Some(true),
        Some(Err(e)) => {
            r.returned = Some(false);
            r.return_error = Some(e.clone());
        }
        None => {}
    }
}

/// `amp_stop_worker {task_id, reason}`: the manager stops one of its running workers.
pub struct StopWorkerTool {
    sup: Arc<Supervisor>,
    schema: Tool,
}

impl StopWorkerTool {
    pub fn new(sup: Arc<Supervisor>) -> Self {
        let schema = Tool {
            name: STOP_WORKER_TOOL.to_owned(),
            description: format!(
                "Stop a worker you started with amp_run_ticket that is still running (or \
                 waiting for a slot) in this chat. Use it when the worker is going the wrong \
                 way, the ticket changed under it, or it is stuck. Its run ends as cancelled \
                 and is recorded abandoned with your reason (`stopped by manager: <reason>`), \
                 and its ticket goes back to ready; this never completes the ticket. Waits \
                 until the worker has ended and its ticket is handed back (up to the abort \
                 grace plus {}s), then returns `status`: stopped, stopped_not_returned, \
                 already_stopping, already_finished, closed_before_stop (it completed or \
                 blocked first; that stands), not_dispatched (it was still queued), stopping \
                 (still ending; its end comes in your worker summaries) or not_found (no \
                 worker on that ticket in this chat). Works on launched workers: while \
                 amp_run_ticket awaits a worker, your turn is blocked in it.",
                STOP_WAIT_SLACK.as_secs()
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "task_id": {"type": "integer", "description": "REQUIRED. The ticket whose worker to stop."},
                    "reason": {"type": "string", "description": format!("REQUIRED. Why, in one line (at most {REASON_MAX_CHARS} characters). Recorded on the abandoned run.")}
                },
                "required": ["task_id", "reason"],
                "additionalProperties": false
            })
            .into(),
            constrained_sampling: None,
        };
        Self { sup, schema }
    }
}

impl std::fmt::Debug for StopWorkerTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("StopWorkerTool").finish_non_exhaustive()
    }
}

/// `amp_stop_worker`'s arguments: `task_id` may come as a string.
fn parse_args(params: &Value) -> Result<(i64, String), String> {
    if !params.is_object() {
        return Err(format!("arguments must be a JSON object, not {params}"));
    }
    let task_id = match params.get("task_id") {
        None | Some(Value::Null) => return Err("task_id is required".into()),
        Some(v) => task_id_arg(params)
            .filter(|id| *id > 0)
            .ok_or_else(|| format!("task_id must be a ticket id, not {v}"))?,
    };
    let reason = params
        .get("reason")
        .and_then(Value::as_str)
        .unwrap_or_default();
    Ok((task_id, normalize_reason(reason)?))
}

#[async_trait::async_trait]
impl AgentTool for StopWorkerTool {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        "Stop worker"
    }

    async fn execute(
        &self,
        _tool_call_id: &str,
        params: Value,
        signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, ToolError> {
        let (task_id, reason) = parse_args(&params).map_err(ToolError::Validation)?;
        let bound = self.sup.config().abort_grace + STOP_WAIT_SLACK;
        let out = stop_worker(&self.sup, task_id, &reason, bound, &signal).await;
        let value = serde_json::to_value(&out).unwrap_or(Value::Null);
        let mut result =
            AgentToolResult::text(serde_json::to_string_pretty(&value).unwrap_or_default());
        result.details = value;
        Ok(result)
    }
}

// ------------------------------------------------------------------------------------------------
// #4240: amp_restart_worker and amp_resume_worker.
// ------------------------------------------------------------------------------------------------

/// What the manager asks [`relaunch`] to do.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Relaunch {
    /// `amp_restart_worker`: a fresh session (the run note is `restarted`).
    Restart,
    /// `amp_resume_worker`: the ticket's last session, continued or replayed (`resumed:<mode>`).
    Resume(ResumeMode),
}

impl Relaunch {
    fn action(self) -> &'static str {
        match self {
            Self::Restart => "restart",
            Self::Resume(_) => "resume",
        }
    }

    fn tool(self) -> &'static str {
        match self {
            Self::Restart => RESTART_WORKER_TOOL,
            Self::Resume(_) => RESUME_WORKER_TOOL,
        }
    }

    /// The request's `resume` and `control`, which only these tools set (AMP #4239).
    fn request_fields(self) -> (Option<ResumeMode>, Option<ControlNote>) {
        match self {
            Self::Restart => (None, Some(ControlNote::Restarted)),
            Self::Resume(mode) => (Some(mode), Some(ControlNote::Resumed(mode))),
        }
    }
}

/// What a restart or resume did (`status` in the tool's result).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum RelaunchStatus {
    /// A fresh worker was started on the ticket through a new dispatch.
    Restarted,
    /// The ticket's last session was started again through a new dispatch.
    Resumed,
    /// Nothing was dispatched: the old worker is still live (its stop had not finished, or
    /// another restart/resume of the ticket is under way), or its hand-back failed and AMP still
    /// has the ticket in progress under the old dispatch (or checking AMP failed, #4304), or the
    /// session's lock is held.
    Busy,
    /// Resume only: no session of the ticket's to resume here (none, unreadable, other cwd).
    NoSession,
    /// Resume `replay` only: nothing was interrupted, or the resume budget is used up.
    NothingToReplay,
    /// The live worker completed or blocked its ticket while it was being stopped: that stands
    /// and nothing was dispatched.
    ClosedBeforeStop,
    /// This chat has no worker on that ticket and no `agent_id` was given.
    NotFound,
    /// The run was refused for another reason (AMP refused the dispatch, a bad agent, ...).
    Refused,
}

impl RelaunchStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Restarted => "restarted",
            Self::Resumed => "resumed",
            Self::Busy => "busy",
            Self::NoSession => "no_session",
            Self::NothingToReplay => "nothing_to_replay",
            Self::ClosedBeforeStop => "closed_before_stop",
            Self::NotFound => "not_found",
            Self::Refused => "refused",
        }
    }

    /// The status of a resume refusal's code (`no_session`, `busy`, `nothing_to_replay`).
    fn from_refusal_code(code: &str) -> Option<Self> {
        match code {
            "no_session" => Some(Self::NoSession),
            "busy" => Some(Self::Busy),
            "nothing_to_replay" => Some(Self::NothingToReplay),
            _ => None,
        }
    }
}

/// `amp_restart_worker` / `amp_resume_worker`'s result.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct RelaunchResult {
    pub task_id: i64,
    /// `"restart"` or `"resume"`.
    pub action: &'static str,
    /// The resume mode (`continue` / `replay`).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mode: Option<&'static str>,
    pub status: RelaunchStatus,
    /// What stopping the old worker did, when one was live.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stop: Option<StopResult>,
    /// The new run: the launch ack (launched workers) or the worker's report (awaited).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub run: Option<Value>,
    /// What the manager should know, in words.
    pub note: String,
}

/// Restarts or resumes in flight, per supervisor and ticket: a second concurrent call for the
/// same ticket is `busy` at once instead of stopping the first call's new worker.
static IN_FLIGHT: LazyLock<Mutex<HashSet<(usize, i64)>>> = LazyLock::new(Default::default);

/// Holds a ticket's place in [`IN_FLIGHT`] for one call.
struct InFlight((usize, i64));

impl InFlight {
    fn take(sup: &Supervisor, task_id: i64) -> Option<Self> {
        Self::take_key((std::ptr::from_ref(sup) as usize, task_id))
    }

    fn take_key(key: (usize, i64)) -> Option<Self> {
        // The lock is released before a `Self` exists: dropping one (its `Drop` locks it too)
        // under the lock would deadlock.
        let taken = IN_FLIGHT
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .insert(key);
        taken.then(|| Self(key))
    }
}

impl Drop for InFlight {
    fn drop(&mut self) {
        IN_FLIGHT
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .remove(&self.0);
    }
}

/// How often [`wait_session_free`] tries the session's lock.
const LOCK_POLL: Duration = Duration::from_millis(25);

/// Waits until no writer holds `session`'s lock, trying it every [`LOCK_POLL`] (the lock is taken
/// and dropped at once), until `deadline`. `true` once it is free; `false` when it is still held
/// at `deadline` or `cancel` fires first (#4281). A stopped run's lock can outlive its live entry
/// by ~1.5 s when its killed tool was a long bash command, so a resume right after a stop waits
/// here instead of being refused `busy`.
async fn wait_session_free(
    session: &Path,
    deadline: tokio::time::Instant,
    cancel: &CancellationToken,
) -> bool {
    loop {
        if crate::session_lock::claim_session_lock(session).is_ok() {
            return true;
        }
        if cancel.is_cancelled() || tokio::time::Instant::now() >= deadline {
            return false;
        }
        tokio::select! {
            () = tokio::time::sleep(LOCK_POLL) => {}
            () = cancel.cancelled() => return false,
        }
    }
}

/// The prompt addition that tells the new worker why it runs again.
fn relaunch_instructions(what: Relaunch, reason: &str, extra: Option<&str>) -> String {
    let mut text = match what {
        Relaunch::Restart => format!(
            "The manager restarted this ticket's worker in a fresh session ({reason}). An \
             earlier worker may have worked on it: read the ticket's newest comments first \
             (amp_get_task with comments), check what is already in your working directory, \
             and build on it rather than redoing finished work."
        ),
        Relaunch::Resume(mode) => format!(
            "The manager resumed this worker ({}): {reason}. Read the ticket's newest comments \
             first; they may change what to do.",
            mode.as_str()
        ),
    };
    if let Some(extra) = extra.map(str::trim).filter(|s| !s.is_empty()) {
        text.push_str("\n\n");
        text.push_str(extra);
    }
    text
}

/// What the caller of [`relaunch`] may set besides the ticket and the reason.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RelaunchOptions {
    /// The agent to run it. Default: the agent of the ticket's last worker in this chat.
    pub agent_id: Option<String>,
    /// More instructions for the new worker, after the restart/resume note.
    pub instructions: Option<String>,
}

/// Restarts or resumes the worker on `task_id` for the manager (#4240): stops it with `reason`
/// if it is live ([`stop_worker`], waiting at most `bound`), refuses `busy` while it is still
/// live or when handing its ticket back failed and AMP still has the ticket under the old
/// run's dispatch (#4304: re-read with [`Supervisor::ticket_hold`], so returning the ticket in
/// AMP lets the next call through; another dispatch is `refused`), waits (resume only, #4281)
/// until the old
/// session's lock is free, then runs the ticket again through the supervisor's normal dispatch
/// path, exactly once per call. `bound` is the limit for all the waiting together: a lock still
/// held when it runs out is `busy`, with nothing dispatched. `cancel` (the manager's turn) ends
/// the waits early, which is `busy`, and cancels a run not dispatched yet:
/// `Err(RunTicketError::Cancelled)`.
pub async fn relaunch(
    sup: &Arc<Supervisor>,
    task_id: i64,
    what: Relaunch,
    reason: &str,
    opts: RelaunchOptions,
    bound: Duration,
    cancel: &CancellationToken,
) -> Result<RelaunchResult, RunTicketError> {
    let deadline = tokio::time::Instant::now() + bound;
    let mut out = RelaunchResult {
        task_id,
        action: what.action(),
        mode: match what {
            Relaunch::Resume(mode) => Some(mode.as_str()),
            Relaunch::Restart => None,
        },
        status: RelaunchStatus::Busy,
        stop: None,
        run: None,
        note: String::new(),
    };
    let refuse = |mut out: RelaunchResult, status, note: String| {
        out.status = status;
        out.note = note;
        Ok(out)
    };
    let Some(_in_flight) = InFlight::take(sup, task_id) else {
        return refuse(
            out,
            RelaunchStatus::Busy,
            format!(
                "another restart or resume of #{task_id} is already under way; nothing was \
                 dispatched."
            ),
        );
    };

    let prior = sup.session(task_id);
    let agent_id = match (&opts.agent_id, &prior) {
        (Some(a), _) if !a.trim().is_empty() => a.trim().to_owned(),
        (_, Some(p)) => p.agent_id.clone(),
        _ => {
            return refuse(
                out,
                RelaunchStatus::NotFound,
                format!(
                    "#{task_id} has no worker in this chat; name the agent (agent_id) to {} it \
                     anyway, or start it with amp_run_ticket.",
                    what.action()
                ),
            );
        }
    };

    if prior.as_ref().is_some_and(WorkerSessionInfo::is_live) {
        let stop = stop_worker(sup, task_id, reason, bound, cancel).await;
        let status = stop.status;
        out.stop = Some(stop);
        match status {
            StopStatus::ClosedBeforeStop => {
                return refuse(
                    out,
                    RelaunchStatus::ClosedBeforeStop,
                    format!(
                        "the worker on #{task_id} closed its ticket before it stopped; that \
                         stands and nothing was dispatched."
                    ),
                );
            }
            StopStatus::StoppedNotReturned => {
                return refuse(
                    out,
                    RelaunchStatus::Busy,
                    format!(
                        "the worker on #{task_id} stopped, but its ticket could not be handed \
                         back, so it may still be held by the old dispatch; nothing was \
                         dispatched. Return the ticket in AMP, then try again."
                    ),
                );
            }
            StopStatus::Stopping => {}
            StopStatus::AlreadyStopping => {
                // Someone else's stop: wait for its end like a stop of ours.
                tokio::select! {
                    _ = sup.wait_not_live(task_id, bound) => {}
                    _ = cancel.cancelled() => {}
                }
            }
            StopStatus::Stopped
            | StopStatus::NotDispatched
            | StopStatus::AlreadyFinished
            | StopStatus::NotFound => {}
        }
    }
    if cancel.is_cancelled() {
        return Err(RunTicketError::Cancelled);
    }
    if sup.session(task_id).is_some_and(|s| s.is_live()) {
        return refuse(
            out,
            RelaunchStatus::Busy,
            format!(
                "the worker on #{task_id} is still ending; nothing was dispatched. Its end comes \
                 in your worker summaries; try again after it."
            ),
        );
    }
    if let Some(Err(e)) = sup.finished_session(task_id).and_then(|s| s.returned) {
        // That record stays until the ticket runs again here, so ask AMP who holds the ticket
        // now: once a person or the manager has returned it, the relaunch goes ahead (#4304).
        let old = sup.finished_dispatch_id(task_id).unwrap_or_default();
        let hold = tokio::select! {
            hold = sup.ticket_hold(task_id, &old) => hold,
            () = cancel.cancelled() => return Err(RunTicketError::Cancelled),
        };
        let old_named = if old.is_empty() {
            "the old dispatch".to_owned()
        } else {
            format!("the old dispatch ({old})")
        };
        match hold {
            Ok(TicketHold::Free(_)) => {}
            Ok(TicketHold::Held) => {
                return refuse(
                    out,
                    RelaunchStatus::Busy,
                    format!(
                        "the last worker on #{task_id} could not hand its ticket back ({e}), and \
                         AMP still has it in progress under {old_named}; nothing was dispatched. \
                         Return the ticket in AMP (amp_return_task with that dispatch_id), then \
                         try again."
                    ),
                );
            }
            Ok(TicketHold::HeldElsewhere(current)) => {
                return refuse(
                    out,
                    RelaunchStatus::Refused,
                    format!(
                        "#{task_id} is in progress in AMP under another dispatch ({current}), not \
                         this chat's last run ({old}); nothing was dispatched. Another worker has \
                         it now: leave it, or return it in AMP first."
                    ),
                );
            }
            Err(why) => {
                return refuse(
                    out,
                    RelaunchStatus::Busy,
                    format!(
                        "the last worker on #{task_id} could not hand its ticket back ({e}), and \
                         checking the ticket in AMP failed ({why}), so it may still be held by \
                         {old_named}; nothing was dispatched. Return the ticket in AMP, then try \
                         again."
                    ),
                );
            }
        }
    }

    // A resume opens the old session file, whose lock the stopped run may still hold for a
    // moment after it is no longer live (#4281). Wait for it, within what is left of `bound`.
    // A restart starts a new session file, so it has nothing to wait for.
    if let (Relaunch::Resume(_), Some(session)) = (what, sup.latest_session_file(task_id)) {
        let free = wait_session_free(&session, deadline, cancel).await;
        if cancel.is_cancelled() {
            return Err(RunTicketError::Cancelled);
        }
        if !free {
            return refuse(
                out,
                RelaunchStatus::Busy,
                format!(
                    "the session of #{task_id} ({}) is still held by the stopped worker; nothing \
                     was dispatched. Try again in a moment.",
                    session.display()
                ),
            );
        }
    }

    let (resume, control) = what.request_fields();
    let req = RunRequest {
        task_id,
        agent_id,
        instructions: Some(relaunch_instructions(
            what,
            reason,
            opts.instructions.as_deref(),
        )),
        // The worktree (`.worktrees/<id>`) is reused as it is; keep its branch.
        branch: prior.and_then(|p| p.branch),
        resume,
        control,
        ..RunRequest::default()
    };
    let started = match what {
        Relaunch::Restart => RelaunchStatus::Restarted,
        Relaunch::Resume(_) => RelaunchStatus::Resumed,
    };
    let run = match sup.config().dispatch {
        DispatchMode::Launch => sup
            .launch(req, cancel.clone())
            .await
            .map(|ack| serde_json::to_value(ack).unwrap_or(Value::Null)),
        DispatchMode::Await => sup
            .run_ticket(req, cancel.clone())
            .await
            .map(|report| serde_json::to_value(report).unwrap_or(Value::Null)),
    };
    match run {
        Ok(run) => {
            // An awaited run refused after its dispatch ends with "resume refused (<code>)".
            let refused = run["error"]
                .as_str()
                .and_then(|e| e.strip_prefix("resume refused ("))
                .and_then(|e| e.split_once(')'))
                .and_then(|(code, _)| RelaunchStatus::from_refusal_code(code));
            out.run = Some(run);
            match refused {
                Some(status) => refuse(
                    out,
                    status,
                    format!(
                        "#{task_id} was dispatched but the resume was refused before the worker \
                         started; the ticket was handed back."
                    ),
                ),
                None => {
                    out.status = started;
                    out.note = format!(
                        "#{task_id} runs again through a new dispatch ({}). If a resume is \
                         refused after the dispatch (`resume refused (<code>)`), the ticket is \
                         handed back and the end comes in your worker summaries.",
                        what.request_fields()
                            .1
                            .map(ControlNote::note)
                            .unwrap_or_default()
                    );
                    Ok(out)
                }
            }
        }
        Err(RunTicketError::Cancelled) => Err(RunTicketError::Cancelled),
        Err(RunTicketError::AlreadyRunning(_)) => refuse(
            out,
            RelaunchStatus::Busy,
            format!("#{task_id} already has a live worker in this chat; nothing was dispatched."),
        ),
        Err(RunTicketError::ResumeRefused {
            refusal, detail, ..
        }) => refuse(
            out,
            RelaunchStatus::from_refusal_code(refusal.code()).unwrap_or(RelaunchStatus::Refused),
            format!("cannot resume #{task_id}: {detail}; nothing was dispatched."),
        ),
        Err(e) => refuse(out, RelaunchStatus::Refused, e.to_string()),
    }
}

/// `amp_restart_worker {task_id, reason}` / `amp_resume_worker {task_id, mode, reason}`.
pub struct RelaunchTool {
    sup: Arc<Supervisor>,
    resume: bool,
    schema: Tool,
}

impl RelaunchTool {
    /// `amp_restart_worker`.
    pub fn restart(sup: Arc<Supervisor>) -> Self {
        let description = format!(
            "Restart the worker on a ticket in a fresh session. If it is still running (or \
             queued) in this chat it is stopped first, exactly as amp_stop_worker would (its run \
             is recorded abandoned with your reason and the ticket handed back); once it has \
             ended, the ticket is dispatched again through the normal path (a new dispatch and \
             claim, in the same worktree), recorded `restarted`. Never a second worker on one \
             ticket. Returns `status`: restarted, busy (it had not ended within the abort grace \
             plus {}s, its hand-back failed and AMP still has the ticket under the old dispatch \
             (return it in AMP, then call again), or another restart/resume of it is under way: \
             nothing was dispatched), closed_before_stop (it completed or blocked first; that \
             stands), not_found (no worker on it in this chat and no agent_id) or refused. For \
             launched workers (the chat): while amp_run_ticket awaits a worker, your turn is \
             blocked in it.",
            STOP_WAIT_SLACK.as_secs()
        );
        let parameters = json!({
            "type": "object",
            "properties": {
                "task_id": {"type": "integer", "description": "REQUIRED. The ticket whose worker to restart."},
                "reason": {"type": "string", "description": format!("REQUIRED. Why, in one line (at most {REASON_MAX_CHARS} characters). Recorded on the stopped run and told to the new worker.")},
                "agent_id": {"type": "string", "description": "Optional. The agent to run it. Default: the agent of its last worker in this chat."},
                "instructions": {"type": "string", "description": "Optional. More instructions for the new worker."}
            },
            "required": ["task_id", "reason"],
            "additionalProperties": false
        });
        Self::new(sup, false, RESTART_WORKER_TOOL, description, parameters)
    }

    /// `amp_resume_worker`.
    pub fn resume(sup: Arc<Supervisor>) -> Self {
        let description = format!(
            "Resume the worker on a ticket in its last session instead of a fresh one. If it is \
             still running (or queued) in this chat it is stopped first, exactly as \
             amp_stop_worker would; once it has ended, the ticket is dispatched again through \
             the normal path (a new dispatch and claim, never the old dispatch_id), recorded \
             `resumed:<mode>`. mode `continue`: the session carries on with a continue prompt \
             and nothing it did is re-run. mode `replay`: only after a crash or a run dropped \
             mid-tool; read-only tools of the interrupted turn re-run, others get an \
             unknown-outcome stand-in. Returns `status`: resumed, busy (still ending, hand-back \
             failed, session locked, or another restart/resume under way: nothing dispatched; \
             the stop waits up to the abort grace plus {}s), no_session (no session here, or \
             it is in another folder), nothing_to_replay (use continue), closed_before_stop, \
             not_found or refused. A refusal found after the dispatch ends the run with `resume \
             refused (<code>)` and hands the ticket back; it comes in your worker summaries. \
             For launched workers (the chat): while amp_run_ticket awaits a worker, your turn \
             is blocked in it.",
            STOP_WAIT_SLACK.as_secs()
        );
        let parameters = json!({
            "type": "object",
            "properties": {
                "task_id": {"type": "integer", "description": "REQUIRED. The ticket whose worker to resume."},
                "mode": {"type": "string", "enum": ["continue", "replay"], "description": "REQUIRED. continue (the usual) or replay (after a crash)."},
                "reason": {"type": "string", "description": format!("REQUIRED. Why, in one line (at most {REASON_MAX_CHARS} characters). Recorded on the stopped run and told to the worker.")},
                "agent_id": {"type": "string", "description": "Optional. Only for a ticket with no worker in this chat (after a crash): the agent that ran it. A different agent's place is refused."},
                "instructions": {"type": "string", "description": "Optional. More instructions for the resumed worker."}
            },
            "required": ["task_id", "mode", "reason"],
            "additionalProperties": false
        });
        Self::new(sup, true, RESUME_WORKER_TOOL, description, parameters)
    }

    fn new(
        sup: Arc<Supervisor>,
        resume: bool,
        name: &str,
        description: String,
        parameters: Value,
    ) -> Self {
        let schema = Tool {
            name: name.to_owned(),
            description,
            parameters: parameters.into(),
            constrained_sampling: None,
        };
        Self {
            sup,
            resume,
            schema,
        }
    }
}

impl std::fmt::Debug for RelaunchTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RelaunchTool")
            .field("tool", &self.schema.name)
            .finish_non_exhaustive()
    }
}

/// A restart's or resume's arguments: what to do, the ticket, the reason and the options.
fn parse_relaunch_args(
    params: &Value,
    resume: bool,
) -> Result<(Relaunch, i64, String, RelaunchOptions), String> {
    let (task_id, reason) = parse_args(params)?;
    let what = if resume {
        match params.get("mode").and_then(Value::as_str).map(str::trim) {
            Some("continue") => Relaunch::Resume(ResumeMode::Continue),
            Some("replay") => Relaunch::Resume(ResumeMode::Replay),
            None | Some("") => return Err("mode is required: continue or replay".into()),
            Some(other) => {
                return Err(format!("mode must be continue or replay, not `{other}`"));
            }
        }
    } else {
        if params.get("mode").is_some_and(|m| !m.is_null()) {
            return Err(
                "amp_restart_worker takes no mode; resume a session with amp_resume_worker".into(),
            );
        }
        Relaunch::Restart
    };
    let text = |key: &str| -> Result<Option<String>, String> {
        match params.get(key) {
            None | Some(Value::Null) => Ok(None),
            Some(Value::String(s)) => Ok(Some(s.clone())),
            Some(v) => Err(format!("{key} must be a string, not {v}")),
        }
    };
    let opts = RelaunchOptions {
        agent_id: text("agent_id")?,
        instructions: text("instructions")?,
    };
    Ok((what, task_id, reason, opts))
}

#[async_trait::async_trait]
impl AgentTool for RelaunchTool {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        if self.resume {
            "Resume worker"
        } else {
            "Restart worker"
        }
    }

    async fn execute(
        &self,
        _tool_call_id: &str,
        params: Value,
        signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, ToolError> {
        let (what, task_id, reason, opts) =
            parse_relaunch_args(&params, self.resume).map_err(ToolError::Validation)?;
        debug_assert_eq!(what.tool(), self.schema.name);
        let bound = self.sup.config().abort_grace + STOP_WAIT_SLACK;
        let out = match relaunch(&self.sup, task_id, what, &reason, opts, bound, &signal).await {
            Ok(out) => out,
            Err(RunTicketError::Cancelled) => return Err(ToolError::Abort),
            Err(e) => return Err(ToolError::Tool(e.to_string())),
        };
        let value = serde_json::to_value(&out).unwrap_or(Value::Null);
        let mut result =
            AgentToolResult::text(serde_json::to_string_pretty(&value).unwrap_or_default());
        result.details = value;
        Ok(result)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reasons_are_one_required_line_of_at_most_200_chars() {
        assert_eq!(
            normalize_reason("  wrong\n approach\t ").unwrap(),
            "wrong approach"
        );
        assert!(normalize_reason(" \n ").is_err());
        assert!(normalize_reason(&"x".repeat(REASON_MAX_CHARS)).is_ok());
        assert!(normalize_reason(&"x".repeat(REASON_MAX_CHARS + 1)).is_err());
    }

    #[test]
    fn arguments_need_a_positive_task_id_and_a_reason() {
        assert_eq!(
            parse_args(&json!({"task_id": "42", "reason": "stuck"})).unwrap(),
            (42, "stuck".to_owned())
        );
        assert!(parse_args(&json!({"reason": "stuck"})).is_err());
        assert!(parse_args(&json!({"task_id": 0, "reason": "stuck"})).is_err());
        assert!(parse_args(&json!({"task_id": 4})).is_err());
        assert!(parse_args(&json!([1])).is_err());
    }

    #[test]
    fn statuses_serialize_as_the_kb_contract_names_them() {
        for s in [
            StopStatus::Stopped,
            StopStatus::StoppedNotReturned,
            StopStatus::AlreadyStopping,
            StopStatus::AlreadyFinished,
            StopStatus::ClosedBeforeStop,
            StopStatus::NotDispatched,
            StopStatus::Stopping,
            StopStatus::NotFound,
        ] {
            assert_eq!(serde_json::to_value(s).unwrap(), json!(s.as_str()));
        }
    }

    #[test]
    fn restart_and_resume_arguments() {
        let (what, id, reason, opts) =
            parse_relaunch_args(&json!({"task_id": 7, "reason": " stuck\n"}), false).unwrap();
        assert_eq!((what, id, reason.as_str()), (Relaunch::Restart, 7, "stuck"));
        assert_eq!(opts, RelaunchOptions::default());
        assert!(parse_relaunch_args(&json!({"task_id": 7}), false).is_err());
        assert!(
            parse_relaunch_args(
                &json!({"task_id": 7, "reason": "x", "mode": "replay"}),
                false
            )
            .is_err(),
            "restart takes no mode"
        );

        let (what, ..) = parse_relaunch_args(
            &json!({"task_id": 7, "reason": "x", "mode": "continue"}),
            true,
        )
        .unwrap();
        assert_eq!(what, Relaunch::Resume(ResumeMode::Continue));
        let (what, _, _, opts) = parse_relaunch_args(
            &json!({"task_id": "7", "reason": "x", "mode": "replay", "agent_id": "amp:amp-worker-backend", "instructions": "go"}),
            true,
        )
        .unwrap();
        assert_eq!(what, Relaunch::Resume(ResumeMode::Replay));
        assert_eq!(opts.agent_id.as_deref(), Some("amp:amp-worker-backend"));
        assert_eq!(opts.instructions.as_deref(), Some("go"));
        assert!(parse_relaunch_args(&json!({"task_id": 7, "reason": "x"}), true).is_err());
        assert!(
            parse_relaunch_args(&json!({"task_id": 7, "reason": "x", "mode": "redo"}), true)
                .is_err()
        );
        assert!(
            parse_relaunch_args(
                &json!({"task_id": 7, "reason": "x", "mode": "continue", "agent_id": 3}),
                true
            )
            .is_err()
        );
    }

    #[test]
    fn restart_and_resume_set_only_their_own_request_fields() {
        assert_eq!(
            Relaunch::Restart.request_fields(),
            (None, Some(ControlNote::Restarted))
        );
        assert_eq!(
            Relaunch::Resume(ResumeMode::Replay).request_fields(),
            (
                Some(ResumeMode::Replay),
                Some(ControlNote::Resumed(ResumeMode::Replay))
            )
        );
        assert_eq!(ControlNote::Restarted.note(), "restarted");
        assert_eq!(
            ControlNote::Resumed(ResumeMode::Continue).note(),
            "resumed:continue"
        );
        assert_eq!(Relaunch::Restart.tool(), RESTART_WORKER_TOOL);
        assert_eq!(
            Relaunch::Resume(ResumeMode::Continue).tool(),
            RESUME_WORKER_TOOL
        );
    }

    #[test]
    fn relaunch_instructions_say_why_and_to_read_the_newest_comments() {
        let text = relaunch_instructions(Relaunch::Restart, "wrong approach", Some(" also X "));
        assert!(
            text.contains("restarted") && text.contains("wrong approach"),
            "{text}"
        );
        assert!(text.contains("newest comments"), "{text}");
        assert!(text.ends_with("also X"), "{text}");
        let text = relaunch_instructions(Relaunch::Resume(ResumeMode::Continue), "r", None);
        assert!(text.contains("resumed this worker (continue)"), "{text}");
        assert!(text.contains("newest comments"), "{text}");
    }

    #[test]
    fn relaunch_statuses_serialize_and_map_resume_refusals() {
        use crate::supervisor::ResumeRefusal;
        for s in [
            RelaunchStatus::Restarted,
            RelaunchStatus::Resumed,
            RelaunchStatus::Busy,
            RelaunchStatus::NoSession,
            RelaunchStatus::NothingToReplay,
            RelaunchStatus::ClosedBeforeStop,
            RelaunchStatus::NotFound,
            RelaunchStatus::Refused,
        ] {
            assert_eq!(serde_json::to_value(s).unwrap(), json!(s.as_str()));
        }
        for r in [
            ResumeRefusal::NoSession,
            ResumeRefusal::Busy,
            ResumeRefusal::NothingToReplay,
        ] {
            let s = RelaunchStatus::from_refusal_code(r.code()).expect("mapped");
            assert_eq!(s.as_str(), r.code());
        }
        assert_eq!(RelaunchStatus::from_refusal_code("other"), None);
    }

    /// #4281: a resume waits for the stopped run's session lock, but only until the limit.
    #[tokio::test]
    async fn the_session_wait_ends_when_the_lock_frees_or_the_limit_runs_out() {
        use tokio::time::Instant;
        // Never /tmp (D13): cargo's target tmp dir, as the session_lock tests do.
        let dir = std::env::var_os("CARGO_TARGET_TMPDIR")
            .map(std::path::PathBuf::from)
            .or_else(|| {
                std::env::var_os("CARGO_MANIFEST_DIR")
                    .map(|d| std::path::PathBuf::from(d).join("../../target").join("tmp"))
            })
            .unwrap_or_else(|| std::path::PathBuf::from("target/tmp"))
            .join("amp-core-worker-control")
            .join("session-wait");
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let session = dir.join("1-a.jsonl");
        let cancel = CancellationToken::new();

        // Free: at once.
        assert!(wait_session_free(&session, Instant::now(), &cancel).await);

        // Held past the limit: false, after waiting out the limit.
        let held = crate::session_lock::claim_session_lock(&session).unwrap();
        let start = Instant::now();
        let limit = Duration::from_millis(150);
        assert!(!wait_session_free(&session, start + limit, &cancel).await);
        assert!(start.elapsed() >= limit, "gave up early");

        // Freed during the wait: true, before the limit.
        let release = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(100)).await;
            drop(held);
        });
        let start = Instant::now();
        assert!(wait_session_free(&session, start + Duration::from_secs(10), &cancel).await);
        assert!(start.elapsed() < Duration::from_secs(5));
        release.await.unwrap();

        // Cancelled: false at once, even with time left.
        let _held = crate::session_lock::claim_session_lock(&session).unwrap();
        cancel.cancel();
        let start = Instant::now();
        assert!(!wait_session_free(&session, start + Duration::from_secs(10), &cancel).await);
        assert!(start.elapsed() < Duration::from_secs(5));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_second_relaunch_of_one_ticket_is_refused_until_the_first_ends() {
        let sup = 0u8;
        let key = (std::ptr::from_ref(&sup) as usize, 4240);
        let take = || InFlight::take_key(key);
        let first = take().expect("the first call takes the ticket");
        assert!(take().is_none(), "a concurrent second call is refused");
        drop(first);
        assert!(take().is_some(), "free again once the first call ended");
    }
}
