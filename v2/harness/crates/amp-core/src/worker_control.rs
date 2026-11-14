//! The manager's worker-control tools (story 582): `amp_stop_worker` (#4233) stops one of the
//! workers this process's [`Supervisor`] runs. Restart and resume (#4240) join it here.
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

use std::{sync::Arc, time::Duration};

use rpi_agent::{AgentError as ToolError, AgentTool, AgentToolResult, ToolResultPartial};
use rpi_ai::types::Tool;
use rpi_harness::types::HarnessTool;
use serde::Serialize;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    policy::STOP_WORKER_TOOL,
    supervisor::{StopBy, StopError, Supervisor, WorkerSessionInfo, task_id_arg},
};

/// The longest reason `amp_stop_worker` takes, in characters.
pub const REASON_MAX_CHARS: usize = 200;

/// How much longer than the supervisor's `abort_grace` a stop waits for the run's end: the
/// hand-back after it is an MCP round-trip (readiness review #3138).
pub const STOP_WAIT_SLACK: Duration = Duration::from_secs(20);

/// The manager's worker-control tools, for `sup`. Call it wherever `sup.tool()` is registered.
pub fn tools(sup: &Arc<Supervisor>) -> Vec<HarnessTool> {
    vec![HarnessTool::new(Arc::new(StopWorkerTool::new(Arc::clone(
        sup,
    ))))]
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
}
