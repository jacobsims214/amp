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
//!     so its ticket is also handed back to ready ([`WorkerHooks::release`]: `amp_return_task`
//!     with the run's dispatch_id, so AMP records the return even though the harness started the
//!     ticket (AMP #2909); `amp_set_task_state` backlog on an older server that refuses it, with
//!     a "returned to ready by amp-harness" reason; #2204).
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
        BLOCK_TOOL, COMPLETE_TOOL, RETURN_TOOL, SET_STATE_TOOL, WorkerHooks, WorkerInfo,
        WorkerOutcome, WorkerReport, task_id_arg,
    },
    tools::result_text,
};

/// The harness-native usage tool (D8).
pub const SESSION_USAGE_TOOL: &str = "amp_session_usage";
/// AMP's tool for a run that ended without closing its ticket.
pub const REPORT_RUN_TOOL: &str = "amp_report_run";
/// The `amp_report_run` outcome for a worker that ended without closing its ticket.
pub const ABANDONED: &str = "abandoned";
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

/// How the person at the manager's chat is told something: a line of text, above the editor. Set
/// by the chat once its shell exists ([`RunReporter::set_notice`]).
pub type Notice = Arc<dyn Fn(&str) + Send + Sync>;

/// The supervisor's run-reporting hooks (D8). Pass it to `Supervisor::with_hooks`.
pub struct RunReporter {
    mcp: Arc<AmpMcpClient>,
    /// The dispatch ledger, so a ticket this reporter hands back or sees closed stops being an
    /// orphan for the next startup (AMP #2802). `None` in a process without one.
    ledger: Option<Arc<crate::dispatch_ledger::Ledger>>,
    /// How the person at the manager's chat is told a ticket came back. Empty in a process with
    /// no chat yet (`run -p`, tests): the run's notes still record it. A slot rather than a plain
    /// field because the chat's notifier exists only once its shell does ([`RunReporter::set_notice`]).
    notice: Mutex<Option<Notice>>,
}

impl RunReporter {
    pub fn new(mcp: Arc<AmpMcpClient>) -> Arc<Self> {
        Arc::new(Self {
            mcp,
            ledger: None,
            notice: Mutex::new(None),
        })
    }

    /// The same reporter, also keeping `ledger` in step with what it does.
    pub fn with_ledger(self: Arc<Self>, ledger: Arc<crate::dispatch_ledger::Ledger>) -> Arc<Self> {
        Arc::new(Self {
            mcp: Arc::clone(&self.mcp),
            ledger: Some(ledger),
            notice: Mutex::new(self.tell_target()),
        })
    }

    /// Where the person at the chat is told a ticket came back (`None`: nowhere yet). The chat
    /// sets it once it has a notifier ([`RunReporter::set_notice`]); an `Arc` shared with the
    /// same reporter keeps working.
    pub fn set_notice(&self, notice: Notice) {
        *self.notice.lock().unwrap_or_else(|e| e.into_inner()) = Some(notice);
    }

    fn tell_target(&self) -> Option<Notice> {
        self.notice
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// Forgets a ticket in the dispatch ledger: nothing is left in progress for a next startup
    /// to sweep (AMP #2802).
    fn forget(&self, task_id: i64) {
        if let Some(ledger) = &self.ledger {
            ledger.clear(task_id);
        }
    }

    /// Tells the person at the chat, when this process has one.
    fn tell(&self, text: &str) {
        if let Some(f) = self.tell_target() {
            f(text);
        }
    }

    /// Hands the ticket back, saying which path did it. `Ok` means it is in ready now, by
    /// whichever path worked; `Err` is AMP's own message when nothing did. `dispatch_id` is the
    /// run's dispatch token ("" when the server gave none).
    async fn hand_back(
        &self,
        task_id: i64,
        dispatch_id: &str,
        why: &str,
    ) -> Result<Released, String> {
        // AMP #2801's returned path first: amp_return_task clears the dispatch and the start,
        // records returned_at/returned_reason (so the ticket shows up under the manager's
        // `returned` list), and writes the history and audit entry. A started ticket — which is
        // what every harness run leaves behind, because the harness claims for its worker — is
        // returned only for the run holding its current dispatch (AMP #2909), so the token goes
        // with the call. A stale token is refused (the ticket is another run's now).
        let reason = release_reason(why);
        let mut args = json!({"task_id": task_id, "reason": reason});
        if !dispatch_id.is_empty() {
            args["dispatch_id"] = json!(dispatch_id);
        }
        let returned = self.call(RETURN_TOOL, args, RELEASE_TIMEOUT).await;
        tracing::debug!(ticket = task_id, why = %why, ok = returned.is_ok(), "handing a ticket back");
        match returned {
            Ok(()) => {
                self.forget(task_id);
                Ok(Released::Returned)
            }
            Err(e) if is_refused_started(&e) || is_unknown_tool(&e, RETURN_TOOL) => {
                // An older server (before AMP #2909) refuses a started ticket even with its
                // dispatch_id; so does any server when there is no token to present. A server
                // older still (before AMP #2801, or one that rolled it back) has no
                // amp_return_task at all: mcp-go answers "tool 'amp_return_task' not found",
                // and the ticket goes back the way #2204 always sent it (AMP #2946).
                // Documented fallback, not a workaround: amp_return_task's own refusal says to
                // "block it with a question, or close it out with closeout_reason"; the harness
                // does neither (the work didn't finish and it has no question), so it uses the
                // manager's escape hatch — the same `amp_set_task_state(backlog)` the #2204
                // cancellation path has always used. The refusal is recorded, so the difference
                // is visible in the run's notes and on the ticket's own history.
                let out = self
                    .call(
                        SET_STATE_TOOL,
                        json!({
                            "task_id": task_id,
                            "state": READY_STATE,
                            "reason": reason,
                        }),
                        RELEASE_TIMEOUT,
                    )
                    .await;
                match &out {
                    // The ticket IS back in ready: the fallback did what the returned path would
                    // have. `Ok` means exactly that (see [`Released::SetBack`]), so the person is
                    // not told a ticket they can re-dispatch is stuck in progress.
                    Ok(()) => {
                        self.forget(task_id);
                        if is_refused_started(&e) {
                            Ok(Released::SetBack)
                        } else {
                            Ok(Released::SetBackOldServer)
                        }
                    }
                    Err(fallback) => {
                        Err(format!("{RETURN_TOOL}: {e}; {SET_STATE_TOOL}: {fallback}"))
                    }
                }
            }
            Err(e) => Err(e),
        }
    }

    /// One AMP tool call, `Ok(())` when it succeeded. `Err` is AMP's own message.
    async fn call(&self, tool: &str, args: Value, timeout: Duration) -> Result<(), String> {
        let Value::Object(args) = args else {
            return Err(format!("{tool}: arguments must be an object"));
        };
        match self
            .mcp
            .call_tool_with_timeout(tool, Some(args), timeout)
            .await
        {
            Ok(r) if r.is_error == Some(true) => Err(result_text(&r).trim().to_owned()),
            Ok(_) => Ok(()),
            Err(e) => Err(e.to_string()),
        }
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
        // AMP #2801: the token identifies the run this report is about (an annotation: the report
        // itself starts the ticket by the fallback when its worker never claimed).
        if !worker.dispatch_id.is_empty() {
            args.insert("dispatch_id".into(), json!(worker.dispatch_id));
        }
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

/// Whether AMP refused `amp_return_task` because the ticket was started (AMP #2801: "task N was
/// started (by X): returning it would pull it out from under a worker that is running"). That is
/// the one refusal the harness answers by setting the state back instead.
pub fn is_refused_started(message: &str) -> bool {
    let m = message.to_ascii_lowercase();
    m.contains("was started") || m.contains("started (by")
}

/// Whether `message` says the server has no tool named `tool` (AMP #2946): an AMP from before
/// that tool existed. mcp-go answers an unknown tool with a JSON-RPC error, "tool 'X' not found:
/// tool not found"; other servers say "unknown tool X". A ticket that isn't found ("task 12 not
/// found") is not this.
pub fn is_unknown_tool(message: &str, tool: &str) -> bool {
    let m = message.to_ascii_lowercase();
    let tool = tool.to_ascii_lowercase();
    m.contains("tool not found")
        || m.contains(&format!("unknown tool {tool}"))
        || m.contains(&format!("unknown tool: {tool}"))
        || (m.contains(&format!("tool '{tool}'")) && m.contains("not found"))
        || (m.contains(&format!("tool \"{tool}\"")) && m.contains("not found"))
}

/// How a ticket got back to ready, when it did (AMP #2802). Both are a success — the ticket is in
/// ready either way — but the difference is visible, so nobody has to guess which call did it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Released {
    /// AMP's returned path: `amp_return_task` cleared the dispatch and recorded the return.
    Returned,
    /// The fallback: AMP refused `amp_return_task` because the ticket was started (a server
    /// older than AMP #2909, or no dispatch_id to present), so `amp_set_task_state` put it back. The dispatch token is not cleared and no return is
    /// recorded, so the ticket is re-dispatchable but not listed under AMP's `returned`.
    SetBack,
    /// The fallback on a server that has no `amp_return_task` at all (before AMP #2801, or after
    /// rolling it back): `amp_set_task_state` put it back, as #2204 always did (AMP #2946).
    SetBackOldServer,
}

impl Released {
    /// What the run's note says it did. Both lines start `returned #N to ready` so a reader (and
    /// a test) can tell the ticket is out of progress without parsing the mechanism.
    pub fn note(self, task_id: i64) -> String {
        match self {
            Self::Returned => {
                format!("returned #{task_id} to ready in AMP (nobody is working on it now)")
            }
            Self::SetBack => format!(
                "returned #{task_id} to ready in AMP: it was already started, so {RETURN_TOOL} \
                 refused it and {SET_STATE_TOOL} put it back instead (that does not clear the \
                 dispatch or record a return)"
            ),
            Self::SetBackOldServer => format!(
                "returned #{task_id} to ready in AMP: this AMP server has no {RETURN_TOOL}, so \
                 {SET_STATE_TOOL} put it back instead (that does not record a return)"
            ),
        }
    }
}

/// What the person at the chat is told when a worker's ticket came back, in the manager's own
/// words (AMP #2802 item 3). Names the ticket, the agent and the outcome, so the person can
/// decide: re-dispatch it, or look at the ticket.
pub fn returned_notice(
    worker: &WorkerInfo,
    report: &WorkerReport,
    released: &Result<Released, String>,
) -> String {
    let what = match report.outcome {
        WorkerOutcome::Timeout => format!("timed out after {}", human(report.duration_ms)),
        WorkerOutcome::TurnBudget => "used up its turn budget".to_owned(),
        WorkerOutcome::Cancelled => "was stopped".to_owned(),
        WorkerOutcome::Error => match report.error.as_deref().map(str::trim) {
            Some(e) if !e.is_empty() => format!("failed: {e}"),
            _ => "failed".to_owned(),
        },
        WorkerOutcome::Stopped => "stopped without finishing".to_owned(),
        // Closed tickets never reach here.
        WorkerOutcome::Completed | WorkerOutcome::Blocked => "finished".to_owned(),
    };
    match released {
        Ok(how) => match how {
            Released::Returned => format!(
                "#{0} was returned: worker {1} {what}. It's back in ready; re-dispatch it or ask \
                 your user.",
                worker.task_id, worker.agent_id
            ),
            // Also back in ready, but by the fallback: say so, so nobody looks for it under
            // AMP's `returned` list and fails to find it.
            Released::SetBack => format!(
                "#{0} was returned: worker {1} {what}. It's back in ready (it had already been \
                 started, so it was set back rather than returned); re-dispatch it or ask your user.",
                worker.task_id, worker.agent_id
            ),
            Released::SetBackOldServer => format!(
                "#{0} was returned: worker {1} {what}. It's back in ready (this AMP server is too \
                 old to record a return, so it was set back); re-dispatch it or ask your user.",
                worker.task_id, worker.agent_id
            ),
        },
        Err(e) => format!(
            "#{0}: worker {1} {what}, and it could not be returned: {e}. It may still be in \
             progress; check the ticket.",
            worker.task_id, worker.agent_id
        ),
    }
}

/// `20 min`, `3 s`, `1 h 2 min`: how long a run took, for a notice.
fn human(ms: u64) -> String {
    let secs = ms / 1000;
    match secs {
        0 => "under a second".to_owned(),
        s if s < 60 => format!("{s} s"),
        s if s < 3600 => format!("{} min", s / 60),
        s => format!("{} h {} min", s / 3600, (s % 3600) / 60),
    }
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
        // AMP #2801/#2802: the dispatch_id makes the completion the worker's own rather than a
        // closeout. The worker may not know it (an old prompt, a model that forgot), so the
        // harness fills it in exactly as it fills the usage fields: only when it was left out.
        let dispatch_id = worker.dispatch_id.clone();
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
                let dispatch_id = dispatch_id.clone();
                // Read now: the totals already include the message that made this call.
                let totals = totals.lock().unwrap().clone();
                Box::pin(async move {
                    let git = match &worktree {
                        Some(wt) => git_state(wt).await,
                        None => None,
                    };
                    let mut fields = run_fields(&model, &totals, git.as_ref());
                    if !dispatch_id.is_empty() {
                        fields.insert("dispatch_id".into(), json!(dispatch_id));
                    }
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
            // The worker closed its ticket: nothing is left in progress, so the ledger record
            // that would have swept it at the next startup goes away (AMP #2802).
            self.forget(worker.task_id);
            return;
        }
        let note = self.report_abandoned(worker, report).await;
        report.notes.push(note);
        // AMP #2802: every end that doesn't close the ticket hands it back. AMP's whole point in
        // the dispatch-integrity work is that a ticket must not stay in progress with nobody on
        // it, and a chat whose worker errored, timed out, used up its turn budget, was stopped,
        // or was cancelled has nobody on it. The manager is told, in words, either way.
        let id = worker.task_id;
        let why = abandoned_note(report);
        let released = self.hand_back(id, &worker.dispatch_id, &why).await;
        report.returned = Some(released.as_ref().map(|_| ()).map_err(String::clone));
        report.notes.push(match &released {
            Ok(how) => how.note(id),
            Err(e) => format!("could not return #{id} to ready in AMP: {e}"),
        });
        // AMP #2802 item 3: the manager is told, in words that say what to do next. This is the
        // manager's conversation — `amp_run_ticket`'s result is what the manager reads, and the
        // run's `returned` field says the same thing in machine form. It is deliberately *not*
        // also a transcript row: the `■ worker finished` line for this run is already there, and
        // a second paragraph per run pushed the conversation off a small screen (it is what
        // broke #2347's 80x24 chat in `chat_worker_watch`). The /workers panel shows it too
        // (`ticket returned to ready` / why not), so a person sees where the ticket went.
        report
            .notes
            .push(returned_notice(worker, report, &released));
    }

    /// A ticket this process hands back while nobody is at the chat (the startup orphan sweep)
    /// has no run to carry it, so it is said as a transcript line.
    fn notice(&self, text: &str) {
        self.tell(text);
    }

    /// [`WorkerHooks::release`]: the ticket is in ready when this is `Ok` (`Err` is AMP's own
    /// message). [`RunReporter::hand_back`] is the same work with the path it took.
    async fn release(&self, task_id: i64, dispatch_id: &str, why: &str) -> Result<(), String> {
        self.hand_back(task_id, dispatch_id, why).await.map(|_| ())
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
            dispatch_id: String::new(),
            started: false,
            returned: None,
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
