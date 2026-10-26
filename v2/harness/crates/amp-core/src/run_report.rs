//! Automatic run reporting (D8, M2-6).
//!
//! AMP reports token spend per ticket and traces code back to tickets from the run fields a
//! worker passes to `amp_complete_task` / `amp_block_task`. Models forget them, guess them, or
//! cannot know them, so the harness fills them in:
//!
//! - [`RunReporter`] is the supervisor's [`WorkerHooks`]:
//!   - its before-tool hook (chained after the role's policy) fills `model`, `input_tokens`,
//!     `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `cost_usd`, `branch`, `commit`
//!     and `pushed` into the worker's `amp_complete_task` / `amp_block_task` **on its own
//!     ticket**, but only the fields the model left out (absent, `null` or `""`). Usage is the
//!     worker's live [`Totals`], which already include the message that made the call; git comes
//!     from the worker's worktree ([`git_state`]).
//!   - its `on_finish` reports every worker that ended without closing its ticket (error,
//!     timeout, cancel, turn budget, or it just stopped) with `amp_report_run` (outcome
//!     `abandoned`, `run_note` "abandoned: <reason>"), the same usage and git fields, so the
//!     run and its tokens are on the ticket and the manager sees it as quiet. A worker the chat
//!     stopped (`cancelled`: Esc/Ctrl+C on the reply, or quitting) leaves nobody on its ticket,
//!     so its ticket is also handed back to ready ([`WorkerHooks::release`]:
//!     `amp_set_task_state` backlog, with a "returned to ready by amp-harness" reason; #2204).
//! - [`SessionUsageTool`] is the native `amp_session_usage`, so the existing prompts (amp-execution
//!   step 6) keep working: the session's own model, tokens and cost so far.
//!
//! Only numbers the provider reported are filled: no token fields when the provider reported no
//! usage, and no `cost_usd` when the cost is 0 (a model without pricing in models.toml).

use std::{
    path::Path,
    sync::{Arc, Mutex},
    time::Duration,
};

use amp_mcp::AmpMcpClient;
use rpi_agent::{
    AgentError as ToolError, AgentTool, AgentToolResult, BeforeToolCall, BeforeToolCallContext,
    BeforeToolCallResult, ToolResultPartial,
};
use rpi_ai::types::Tool;
use rpi_harness::types::{HarnessTool, ToolReplay};
use serde::Serialize;
use serde_json::{Map, Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    events::Totals,
    supervisor::{
        BLOCK_TOOL, COMPLETE_TOOL, WorkerHooks, WorkerInfo, WorkerOutcome, WorkerReport,
        task_id_arg,
    },
    tools::result_text,
};

/// The harness-native usage tool (D8).
pub const SESSION_USAGE_TOOL: &str = "amp_session_usage";
/// AMP's tool for a run that ended without closing its ticket.
pub const REPORT_RUN_TOOL: &str = "amp_report_run";
/// The `amp_report_run` outcome for a worker that ended without closing its ticket.
pub const ABANDONED: &str = "abandoned";
/// AMP's tool that hands an abandoned ticket back (in progress -> ready).
pub const SET_STATE_TOOL: &str = "amp_set_task_state";
/// The AMP state "ready" is stored as (AMP keeps it blocked while a dependency is open).
pub const READY_STATE: &str = "backlog";
/// How long `on_finish` waits for AMP to take an abandoned run (shutdown waits on it).
const REPORT_TIMEOUT: Duration = Duration::from_secs(30);
/// How long handing an abandoned ticket back may take.
const RELEASE_TIMEOUT: Duration = Duration::from_secs(10);
/// How long the git commands behind the run fields may take.
const GIT_TIMEOUT: Duration = Duration::from_secs(15);
/// `run_note` is one line; long errors are cut.
const NOTE_MAX_CHARS: usize = 300;

/// Where a worktree's work is: the run's git fields.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct GitState {
    /// `None` on a detached HEAD.
    pub branch: Option<String>,
    pub commit: String,
    /// HEAD is on its upstream (or, without an upstream, on any remote-tracking branch, as when a
    /// ticket branch is merged into the milestone branch and that is pushed). Remote-tracking refs
    /// are as of the last fetch or push; nothing is fetched here.
    pub pushed: bool,
}

/// The git fields for `dir`, or `None` when it is not a git checkout (or has no commit).
pub async fn git_state(dir: &Path) -> Option<GitState> {
    let commit = git(dir, &["rev-parse", "HEAD"]).await.ok()?;
    let branch = git(dir, &["branch", "--show-current"])
        .await
        .ok()
        .filter(|b| !b.is_empty());
    let pushed = match git(
        dir,
        &["rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"],
    )
    .await
    {
        Ok(upstream) if !upstream.is_empty() => git(
            dir,
            &["merge-base", "--is-ancestor", "HEAD", upstream.as_str()],
        )
        .await
        .is_ok(),
        _ => git(
            dir,
            &[
                "for-each-ref",
                "--count=1",
                "--contains",
                "HEAD",
                "--format=%(refname)",
                "refs/remotes",
            ],
        )
        .await
        .is_ok_and(|refs| !refs.is_empty()),
    };
    Some(GitState {
        branch,
        commit,
        pushed,
    })
}

async fn git(dir: &Path, args: &[&str]) -> Result<String, String> {
    let out = tokio::time::timeout(
        GIT_TIMEOUT,
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
        Err(String::from_utf8_lossy(&out.stderr).trim().to_owned())
    }
}

/// The run fields the harness knows for a session: model, usage and (in a worktree) git.
pub fn run_fields(model: &str, totals: &Totals, git: Option<&GitState>) -> Map<String, Value> {
    let mut out = Map::new();
    if !model.is_empty() {
        out.insert("model".into(), json!(model));
    }
    let u = &totals.usage;
    if u.input_tokens + u.output_tokens > 0 {
        out.insert("input_tokens".into(), json!(u.input_tokens));
        out.insert("output_tokens".into(), json!(u.output_tokens));
        out.insert("cache_read_tokens".into(), json!(u.cache_read_tokens));
        out.insert("cache_write_tokens".into(), json!(u.cache_write_tokens));
    }
    if u.cost_usd > 0.0 {
        out.insert("cost_usd".into(), json!(round_cost(u.cost_usd)));
    }
    if let Some(g) = git {
        if let Some(b) = &g.branch {
            out.insert("branch".into(), json!(b));
        }
        out.insert("commit".into(), json!(g.commit));
        out.insert("pushed".into(), json!(g.pushed));
    }
    out
}

/// Dollars to 6 decimals (sums of per-message costs carry float noise).
fn round_cost(c: f64) -> f64 {
    (c * 1e6).round() / 1e6
}

/// A field the model left out: absent, `null`, or an empty string.
fn is_empty_field(v: Option<&Value>) -> bool {
    match v {
        None | Some(Value::Null) => true,
        Some(Value::String(s)) => s.trim().is_empty(),
        _ => false,
    }
}

/// `args` with every field of `fields` the model left out filled in. `None` when nothing
/// changes (or `args` is not an object).
pub fn fill_missing(args: &Value, fields: &Map<String, Value>) -> Option<Value> {
    let Value::Object(map) = args else {
        return None;
    };
    let mut out = map.clone();
    let mut changed = false;
    for (k, v) in fields {
        if is_empty_field(map.get(k)) {
            out.insert(k.clone(), v.clone());
            changed = true;
        }
    }
    changed.then_some(Value::Object(out))
}

/// The git run fields a worker without a worktree may not set (#2364): `branch`, `commit` and
/// `pushed` (they come only from a worktree), and a `pr_url` that isn't a URL. `None` when there
/// is nothing to drop.
pub fn drop_git_fields(args: &Value) -> Option<Value> {
    let Value::Object(map) = args else {
        return None;
    };
    let mut out = map.clone();
    let mut changed = false;
    for k in ["branch", "commit", "pushed"] {
        changed |= out.remove(k).is_some();
    }
    if out
        .get("pr_url")
        .is_some_and(|u| !u.as_str().is_some_and(|u| u.starts_with("http")))
    {
        out.remove("pr_url");
        changed = true;
    }
    changed.then_some(Value::Object(out))
}

/// `"abandoned: <outcome>[: <error>]"` on one line, cut to [`NOTE_MAX_CHARS`].
pub fn abandoned_note(report: &WorkerReport) -> String {
    let outcome = report.outcome.as_str();
    let mut note = format!("{ABANDONED}: {outcome}");
    // rpi's run errors often start with the outcome itself ("error: No more faux responses
    // queued"); don't say it twice ("abandoned: error: error: ...").
    if let Some(err) = report
        .error
        .as_deref()
        .map(str::trim)
        .map(|e| {
            e.strip_prefix(outcome)
                .and_then(|rest| rest.strip_prefix(':'))
                .map_or(e, str::trim_start)
        })
        .filter(|e| !e.is_empty())
    {
        note.push_str(": ");
        note.push_str(&err.split_whitespace().collect::<Vec<_>>().join(" "));
    }
    if note.chars().count() > NOTE_MAX_CHARS {
        note = note.chars().take(NOTE_MAX_CHARS - 1).collect::<String>() + "…";
    }
    note
}

/// The supervisor's run-reporting hooks (D8). Pass it to `Supervisor::with_hooks`.
pub struct RunReporter {
    mcp: Arc<AmpMcpClient>,
}

impl RunReporter {
    pub fn new(mcp: Arc<AmpMcpClient>) -> Arc<Self> {
        Arc::new(Self { mcp })
    }

    /// `amp_report_run` for a worker that ended without closing its ticket.
    async fn report_abandoned(&self, worker: &WorkerInfo, report: &WorkerReport) -> String {
        let git = match &worker.worktree {
            Some(wt) => git_state(wt).await,
            None => None,
        };
        let model = if report.model.is_empty() {
            &worker.model
        } else {
            &report.model
        };
        let mut args = run_fields(model, &report.totals, git.as_ref());
        args.insert("task_id".into(), json!(worker.task_id));
        args.insert("outcome".into(), json!(ABANDONED));
        args.insert("agent_id".into(), json!(worker.agent_id));
        let note = abandoned_note(report);
        args.insert("run_note".into(), json!(note));
        match self
            .mcp
            .call_tool_with_timeout(REPORT_RUN_TOOL, Some(args), REPORT_TIMEOUT)
            .await
        {
            Ok(r) if r.is_error == Some(true) => format!(
                "could not report the abandoned run to AMP: {}",
                result_text(&r).trim()
            ),
            Ok(_) => format!("reported the run to AMP ({REPORT_RUN_TOOL}, \"{note}\")"),
            Err(e) => format!("could not report the abandoned run to AMP: {e}"),
        }
    }
}

/// The history line of a ticket returned to ready: `returned to ready by amp-harness: <why>`,
/// one line, cut to [`NOTE_MAX_CHARS`].
pub fn release_reason(why: &str) -> String {
    let why = why.split_whitespace().collect::<Vec<_>>().join(" ");
    let mut reason = format!("returned to ready by amp-harness: {why}");
    if reason.chars().count() > NOTE_MAX_CHARS {
        reason = reason.chars().take(NOTE_MAX_CHARS - 1).collect::<String>() + "…";
    }
    reason
}

type HookFuture =
    std::pin::Pin<Box<dyn std::future::Future<Output = Option<BeforeToolCallResult>> + Send>>;

#[async_trait::async_trait]
impl WorkerHooks for RunReporter {
    fn before_tool_call(&self, worker: &WorkerInfo) -> Option<BeforeToolCall> {
        let task_id = worker.task_id;
        let model = worker.model.clone();
        let worktree = worker.worktree.clone();
        // #2364: no git of its own: no repository at all, or its own folder (`amp-work/<id>`).
        let no_git = worktree.is_none()
            && (!worker.git || worker.workspace == crate::workspace::Placement::Folder);
        let totals = Arc::clone(&worker.totals);
        Some(Arc::new(
            move |ctx: BeforeToolCallContext<'_>, _signal: CancellationToken| -> HookFuture {
                let name = ctx.tool_call.name.as_str();
                if !matches!(name, COMPLETE_TOOL | BLOCK_TOOL)
                    || task_id_arg(ctx.args) != Some(task_id)
                {
                    return Box::pin(async { None });
                }
                let args = ctx.args.clone();
                let model = model.clone();
                let worktree = worktree.clone();
                // Read now: the totals already include the message that made this call.
                let totals = totals.lock().unwrap().clone();
                Box::pin(async move {
                    let git = match &worktree {
                        Some(wt) => git_state(wt).await,
                        None => None,
                    };
                    let fields = run_fields(&model, &totals, git.as_ref());
                    // #2364: a worker with no git of its own has no git fields; drop whatever
                    // it made up (a real run passed branch "__OMIT__").
                    let stripped = no_git.then(|| drop_git_fields(&args)).flatten();
                    let base = stripped.clone().unwrap_or(args);
                    fill_missing(&base, &fields)
                        .or(stripped)
                        .map(|args| BeforeToolCallResult {
                            block: false,
                            reason: None,
                            terminate: false,
                            args: Some(args),
                        })
                })
            },
        ))
    }

    fn extra_tools(&self, worker: &WorkerInfo) -> Vec<HarnessTool> {
        vec![SessionUsageTool::harness_tool(
            worker.model.clone(),
            Arc::clone(&worker.totals),
            true,
        )]
    }

    async fn on_finish(&self, worker: &WorkerInfo, report: &mut WorkerReport) {
        if report.outcome.closed_ticket() {
            return;
        }
        let note = self.report_abandoned(worker, report).await;
        report.notes.push(note);
        // #2204: a worker the chat stopped (Esc/Ctrl+C on the reply, or quitting) leaves nobody
        // on its ticket: hand it back so it isn't stuck in progress. Other ends (error, timeout,
        // turn budget, stopped) go back to a live manager, which decides what happens next.
        if report.outcome == WorkerOutcome::Cancelled {
            let id = worker.task_id;
            report
                .notes
                .push(match self.release(id, &abandoned_note(report)).await {
                    Ok(()) => {
                        format!("returned #{id} to ready in AMP (nobody is working on it now)")
                    }
                    Err(e) => format!("could not return #{id} to ready in AMP: {e}"),
                });
        }
    }

    async fn release(&self, task_id: i64, why: &str) -> Result<(), String> {
        let args = json!({
            "task_id": task_id,
            "state": READY_STATE,
            "reason": release_reason(why),
        });
        let Value::Object(args) = args else {
            unreachable!()
        };
        match self
            .mcp
            .call_tool_with_timeout(SET_STATE_TOOL, Some(args), RELEASE_TIMEOUT)
            .await
        {
            Ok(r) if r.is_error == Some(true) => Err(result_text(&r).trim().to_owned()),
            Ok(_) => Ok(()),
            Err(e) => Err(e.to_string()),
        }
    }
}

/// The native `amp_session_usage`: this session's model, tokens and cost so far.
pub struct SessionUsageTool {
    model: String,
    totals: Arc<Mutex<Totals>>,
    /// The harness fills these into `amp_complete_task` / `amp_block_task` itself (workers).
    auto_filled: bool,
    schema: Tool,
}

impl SessionUsageTool {
    pub fn new(model: String, totals: Arc<Mutex<Totals>>, auto_filled: bool) -> Self {
        let schema = Tool {
            name: SESSION_USAGE_TOOL.to_owned(),
            description: "Your own model and token usage in this session so far: model, \
                          input/output/cache tokens, and cost when the provider reports it. Call \
                          it right before amp_complete_task, amp_block_task or amp_report_run and \
                          pass the numbers along, so AMP can report token spend per ticket."
                .to_owned(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "ticket": {"type": "integer", "description": "Optional: the AMP ticket you're about to report on."}
                }
            })
            .into(),
            constrained_sampling: None,
        };
        Self {
            model,
            totals,
            auto_filled,
            schema,
        }
    }

    pub fn harness_tool(
        model: String,
        totals: Arc<Mutex<Totals>>,
        auto_filled: bool,
    ) -> HarnessTool {
        HarnessTool::new(Arc::new(Self::new(model, totals, auto_filled)))
            .with_replay(ToolReplay::Safe)
    }

    /// The tool's JSON: the opencode plugin's fields (`null` for what the provider didn't report),
    /// plus turns and tool calls.
    pub fn usage_json(&self) -> Value {
        let t = self.totals.lock().unwrap().clone();
        let u = &t.usage;
        let reported = u.input_tokens + u.output_tokens > 0;
        let tokens = |n: i64| if reported { json!(n) } else { Value::Null };
        let mut out = json!({
            "model": self.model,
            "input_tokens": tokens(u.input_tokens),
            "output_tokens": tokens(u.output_tokens),
            "cache_read_tokens": tokens(u.cache_read_tokens),
            "cache_write_tokens": tokens(u.cache_write_tokens),
            "cost_usd": if u.cost_usd > 0.0 { json!(round_cost(u.cost_usd)) } else { Value::Null },
            "turns": t.turns,
            "tool_calls": t.tool_calls,
        });
        if self.auto_filled {
            out["note"] = json!(
                "amp-harness fills these (and branch, commit, pushed from your worktree) into \
                 amp_complete_task / amp_block_task on your ticket when you leave them out."
            );
        }
        out
    }
}

impl std::fmt::Debug for SessionUsageTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SessionUsageTool")
            .field("model", &self.model)
            .finish_non_exhaustive()
    }
}

#[async_trait::async_trait]
impl AgentTool for SessionUsageTool {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        "Session usage"
    }

    async fn execute(
        &self,
        _tool_call_id: &str,
        _params: Value,
        _signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, ToolError> {
        let value = self.usage_json();
        let mut out = AgentToolResult::text(value.to_string());
        out.details = value;
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use std::path::PathBuf;

    use super::*;
    use crate::{events::UsageJson, supervisor::WorkerOutcome};

    fn totals(input: i64, output: i64, cost: f64) -> Totals {
        Totals {
            usage: UsageJson {
                input_tokens: input,
                output_tokens: output,
                cache_read_tokens: 3,
                cache_write_tokens: 0,
                cost_usd: cost,
            },
            ..Totals::default()
        }
    }

    #[test]
    fn fields_carry_only_what_the_provider_reported() {
        let git = GitState {
            branch: Some("amp-7".into()),
            commit: "abc1234".into(),
            pushed: false,
        };
        let f = run_fields("wf/wf-1", &totals(10, 5, 0.1 + 0.2), Some(&git));
        assert_eq!(
            Value::Object(f),
            json!({"model": "wf/wf-1", "input_tokens": 10, "output_tokens": 5,
                   "cache_read_tokens": 3, "cache_write_tokens": 0, "cost_usd": 0.3,
                   "branch": "amp-7", "commit": "abc1234", "pushed": false})
        );
        // No usage reported, no pricing, no git: only the model.
        let f = run_fields("wf/wf-1", &totals(0, 0, 0.0), None);
        assert_eq!(Value::Object(f), json!({"model": "wf/wf-1"}));
    }

    #[test]
    fn filling_keeps_what_the_model_set() {
        let fields = run_fields("wf/wf-1", &totals(10, 5, 0.0), None);
        let args = json!({"task_id": 7, "model": "claude-x", "input_tokens": 0,
                          "output_tokens": null, "branch": "", "pr_url": "u"});
        let filled = fill_missing(&args, &fields).unwrap();
        assert_eq!(filled["model"], "claude-x", "the model's own value stays");
        assert_eq!(filled["input_tokens"], 0, "an explicit number stays");
        assert_eq!(filled["output_tokens"], 5, "null is empty");
        assert_eq!(filled["cache_read_tokens"], 3);
        assert_eq!(filled["branch"], "", "nothing to fill it with");
        assert_eq!(filled["pr_url"], "u");
        let full = json!({"task_id": 7, "model": "m", "input_tokens": 1, "output_tokens": 1,
                          "cache_read_tokens": 1, "cache_write_tokens": 1});
        assert_eq!(fill_missing(&full, &fields), None, "nothing to change");
        assert_eq!(fill_missing(&json!([1]), &fields), None);
    }

    #[test]
    fn a_worker_without_a_worktree_sends_no_git_fields() {
        let args = json!({"task_id": 7, "branch": "__OMIT__", "commit": "", "pushed": false,
                          "pr_url": "none", "model": "m"});
        assert_eq!(
            drop_git_fields(&args),
            Some(json!({"task_id": 7, "model": "m"}))
        );
        let pr = json!({"task_id": 7, "pr_url": "https://x/pr/1"});
        assert_eq!(drop_git_fields(&pr), None);
    }

    fn report(outcome: WorkerOutcome, error: Option<&str>) -> WorkerReport {
        WorkerReport {
            task_id: 7,
            agent_id: "amp:amp-worker-backend".into(),
            role: "amp-worker-backend".into(),
            outcome,
            final_message: String::new(),
            error: error.map(str::to_owned),
            model: "wf/wf-1".into(),
            cwd: PathBuf::from("."),
            worktree: None,
            branch: None,
            workspace: crate::workspace::Placement::Shared,
            session_file: None,
            turns: 0,
            tool_calls: 0,
            tool_errors: 0,
            usage: UsageJson::default(),
            duration_ms: 0,
            notes: Vec::new(),
            totals: Totals::default(),
        }
    }

    #[test]
    fn abandoned_notes_are_one_short_line() {
        assert_eq!(
            abandoned_note(&report(WorkerOutcome::Stopped, None)),
            "abandoned: stopped"
        );
        assert_eq!(
            abandoned_note(&report(
                WorkerOutcome::Error,
                Some("provider_error:\n  No more faux responses queued")
            )),
            "abandoned: error: provider_error: No more faux responses queued"
        );
        // What a real run reports (seen against compose AMP in #2099): no doubled outcome.
        assert_eq!(
            abandoned_note(&report(
                WorkerOutcome::Error,
                Some("error: No more faux responses queued")
            )),
            "abandoned: error: No more faux responses queued"
        );
        assert_eq!(
            abandoned_note(&report(WorkerOutcome::Error, Some("error:"))),
            "abandoned: error"
        );
        let long = "x".repeat(1000);
        let note = abandoned_note(&report(WorkerOutcome::Timeout, Some(&long)));
        assert_eq!(note.chars().count(), NOTE_MAX_CHARS);
        assert!(note.ends_with('…'));
    }

    #[test]
    fn session_usage_reports_null_for_what_is_unknown() {
        let t = Arc::new(Mutex::new(Totals::default()));
        let tool = SessionUsageTool::new("p/m".into(), Arc::clone(&t), false);
        let v = tool.usage_json();
        assert_eq!(v["model"], "p/m");
        assert!(v["input_tokens"].is_null() && v["cost_usd"].is_null());
        assert!(v.get("note").is_none());
        *t.lock().unwrap() = totals(10, 5, 0.25);
        let v = SessionUsageTool::new("p/m".into(), t, true).usage_json();
        assert_eq!(
            (v["input_tokens"].clone(), v["cost_usd"].clone()),
            (json!(10), json!(0.25))
        );
        assert!(v["note"].as_str().unwrap().contains("amp_complete_task"));
    }
}
