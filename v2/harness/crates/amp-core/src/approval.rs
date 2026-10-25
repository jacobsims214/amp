//! Plan approval and the steps only a person takes, in the web UI (#2171, epic 162).
//!
//! Since #2167 AMP refuses every agent attempt to approve a plan, approve an artifact, mark a
//! person's ticket done or turn plan approval off: "… is done by a person in the AMP web UI, not by
//! an agent or over MCP: <link>. Stop and ask your person to do it there." It also refuses to
//! dispatch (or complete) a ticket nobody approved: before #2198 one "planned after the plan was
//! last approved"; since #2198 (per-ticket approval) a ticket that "isn't approved … since it was
//! planned or its scope last changed", which "Its owner approves … (a project lead may approve it
//! on their behalf)". The prompts (#2169)
//! tell the manager to hand the link to its person and stop; this module makes the harness hold
//! it to that:
//!
//! - [`refusal`] recognises those refusals in a tool error's text, for the TUI's notice
//!   (`amp-tui`'s `ApprovalNotice`) and the metadata poller's refresh.
//! - [`end_turn_on_refusal`] is the manager's `after_tool_call` hook: a refused call ends the
//!   tool batch (`terminate`), and a line tells the model why.
//! - [`ManagerGuard`] is the manager's `before_tool_call` hook, before the role policy. Once a
//!   refusal came back since the user's last message, every further tool call in the run is
//!   blocked with `terminate`, so a batch that was not all refusals still ends at the next turn
//!   instead of retrying another way. It also keeps the manager's `amp_complete_task` and
//!   `amp_claim_task` to the tickets its user named in this session or that it dispatched itself
//!   (status questions are read-only; never touch a ticket nobody asked about). Whether a ticket
//!   is a person's is the server's call: completing one is refused there, with its link.
//!
//! `amp_approve_plan`, and `amp_update_project_settings` with `require_plan_approval` or
//! `open_reassign`, are denied to every role by the role policy itself
//! ([`crate::policy::person_only`]).

use std::{collections::BTreeSet, future::Future, pin::Pin, sync::Arc};

use rpi_agent::{
    AfterToolCall, AfterToolCallContext, AfterToolCallResult, AgentContext, AgentMessage,
    BeforeToolCall, BeforeToolCallContext, BeforeToolCallResult, TextContentOrImage,
};
use rpi_ai::types::{Content, UserContent};
use serde_json::Value;
use tokio_util::sync::CancellationToken;

/// A refused call's own row in the chat (#2226): the approval notice under it carries the
/// server's sentence. The TUI draws it, and the manager feed's `tool_end.refused_line` carries it
/// (#2388) so a remote viewer shows the same line.
pub const REFUSED_LINE: &str = "AMP refused: this needs a person in the AMP web UI (below)";

/// The UI-only refusal (`authz.UIOnly` in amp-api).
pub const UI_ONLY_MARK: &str = "is done by a person in the AMP web UI";
/// The plan-approval gate (`planApprovalGate`, `approvalGate` in amp-api) before #2198.
pub const PLAN_GATE_MARK: &str = "planned after the plan was last approved";
/// The same gates since #2198 (per-ticket approval): "ticket N isn't approved: nobody has approved
/// it since it was planned or its scope last changed" (dispatch), "ticket N can't be completed: it
/// isn't approved since it was planned or its scope last changed" (complete).
pub const TICKET_GATE_MARK: &str = "since it was planned or its scope last changed";
/// Words in the #2198 dispatch refusal saying the ticket's owner approves it.
const OWNER_APPROVES: &str = "Its owner approves it";

/// Tools the manager may use only on tickets its user named or it dispatched.
pub const TICKET_SCOPED_TOOLS: &[&str] = &["amp_complete_task", "amp_claim_task"];
/// Tools that dispatch a ticket in this session (their `task_id` is then in scope).
const DISPATCH_TOOLS: &[&str] = &["amp_run_ticket", "amp_dispatch_task"];

/// What a refusal asks of the person.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RefusalKind {
    /// The plan waits for a person's approval (the Flight plan page).
    PlanApproval,
    /// Another step only a person takes in the web UI (an artifact, a person's ticket, settings).
    PersonInUi,
}

/// A refusal recognised in a tool error.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Refusal {
    pub kind: RefusalKind,
    /// The server's sentence(s), without the JSON detail `amp_approve_plan` appends.
    pub message: String,
    /// The link the server gave (absolute, or relative when AMP has no public URL). Untrusted:
    /// show it as text; build clickable links from the instance's own URL helpers.
    pub link: Option<String>,
    /// `/project/{id}/…` in the link.
    pub project_id: Option<i64>,
    /// `?task={id}` in the link, or the "ticket N" a plan-gate refusal names.
    pub ticket_id: Option<i64>,
    /// The server approves per ticket (#2198): the refusal is about one ticket, approved by its
    /// owner (or a project lead on their behalf), not by whoever approves the plan.
    pub per_ticket: bool,
}

/// Recognises an approval or UI-only refusal in a tool error's text.
pub fn refusal(text: &str) -> Option<Refusal> {
    let ui = text.contains(UI_ONLY_MARK);
    let ticket_gate = text.contains(TICKET_GATE_MARK);
    let gate = ticket_gate || text.contains(PLAN_GATE_MARK);
    if !ui && !gate {
        return None;
    }
    // amp_approve_plan appends its status JSON after the sentence; the harness appends
    // [`TURN_ENDED`] when it ends the manager's turn.
    let message = text.split("[amp-harness]").next().unwrap_or(text);
    let message = match message.find(" {\"") {
        Some(i) => &message[..i],
        None => message,
    };
    let message = message.split_whitespace().collect::<Vec<_>>().join(" ");
    let message = server_sentence(&message).to_owned();
    let link = find_link(&message);
    let project_id = link.as_deref().and_then(|l| number_after(l, "/project/"));
    let ticket_id = link
        .as_deref()
        .and_then(|l| number_after(l, "task="))
        .or_else(|| gate.then(|| ticket_named(text)).flatten());
    let plan_link = link
        .as_deref()
        .is_some_and(|l| l.trim_end_matches('/').ends_with("/plan"));
    let kind = if gate || plan_link || message.contains("Plan approval is done by a person") {
        RefusalKind::PlanApproval
    } else {
        RefusalKind::PersonInUi
    };
    let per_ticket = ticket_gate || message.contains(OWNER_APPROVES);
    Some(Refusal {
        kind,
        message,
        link,
        project_id,
        ticket_id,
        per_ticket,
    })
}

/// AMP's own sentence in a tool error, without the transport's wrapping before it:
/// `tool error: amp_x: `, `AMP refused to dispatch ticket #N: ` (`amp_run_ticket`, #2226),
/// `tools/call amp_x: Mcp error: -32603: `.
pub fn server_sentence(text: &str) -> &str {
    let mut text = text.trim();
    if let Some(at) = text.find("Mcp error: ") {
        text = &text[at + "Mcp error: ".len()..];
        // The JSON-RPC code, `-32603: `.
        if let Some((code, rest)) = text.split_once(": ")
            && !code.is_empty()
            && code
                .trim_start_matches('-')
                .chars()
                .all(|c| c.is_ascii_digit())
        {
            text = rest;
        }
        return text.trim_start();
    }
    if let Some(rest) = text.strip_prefix("tool error: ") {
        text = rest;
    }
    if let Some(rest) = text.strip_prefix("AMP refused to dispatch ticket #")
        && let Some((id, rest)) = rest.split_once(": ")
        && id.chars().all(|c| c.is_ascii_digit())
    {
        text = rest;
    }
    // A tool's name (`amp_dispatch_task: `), as `tools/call amp_x: ` without an MCP code.
    let text = text.strip_prefix("tools/call ").unwrap_or(text);
    match text.split_once(": ") {
        Some((name, rest)) if name.starts_with("amp_") && !name.contains(' ') => rest.trim_start(),
        _ => text,
    }
}

/// The first "ticket N" or "ticket #N" in `text`.
fn ticket_named(text: &str) -> Option<i64> {
    text.match_indices("ticket ").find_map(|(i, m)| {
        let rest = &text[i + m.len()..];
        number_after(rest.strip_prefix('#').unwrap_or(rest), "")
    })
}

/// The first `http(s)://…` or `/project/…` token, without trailing punctuation.
fn find_link(text: &str) -> Option<String> {
    text.split_whitespace()
        .find(|w| {
            w.starts_with("http://") || w.starts_with("https://") || w.starts_with("/project/")
        })
        .map(|w| {
            w.trim_end_matches(['.', ',', ';', ')', '"', '\''])
                .to_owned()
        })
        .filter(|w| !w.is_empty())
}

fn number_after(text: &str, prefix: &str) -> Option<i64> {
    let start = text.find(prefix)? + prefix.len();
    let digits: String = text[start..]
        .chars()
        .take_while(char::is_ascii_digit)
        .collect();
    digits.parse().ok()
}

/// The text of a tool result's content.
fn result_text(content: &[TextContentOrImage]) -> String {
    content
        .iter()
        .filter_map(|c| match c {
            TextContentOrImage::Text(t) => Some(t.text.as_str()),
            TextContentOrImage::Image(_) => None,
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// The line added to a refused result, so the model knows the turn ended on purpose.
pub const TURN_ENDED: &str = "[amp-harness] This needs a person in the AMP web UI. The harness \
     ended your turn: your user sees the link. When they're back, give them the link and what to \
     click, and wait; don't look for another way round it.";

type AfterFuture = Pin<Box<dyn Future<Output = Option<AfterToolCallResult>> + Send>>;
type BeforeFuture = Pin<Box<dyn Future<Output = Option<BeforeToolCallResult>> + Send>>;

/// The manager's `after_tool_call` hook: a refused call terminates the batch (rpi ends the run
/// when every call in the batch terminates; [`ManagerGuard`] covers the rest).
pub fn end_turn_on_refusal() -> AfterToolCall {
    Arc::new(
        |ctx: AfterToolCallContext<'_>, _signal: CancellationToken| -> AfterFuture {
            let result = if ctx.is_error && refusal(&result_text(&ctx.result.content)).is_some() {
                let mut content = ctx.result.content.clone();
                content.push(TextContentOrImage::text(TURN_ENDED));
                Some(AfterToolCallResult {
                    content: Some(content),
                    terminate: Some(true),
                    ..Default::default()
                })
            } else {
                None
            };
            Box::pin(async move { result })
        },
    )
}

/// The manager's extra `before_tool_call` rules (checked before the role policy).
#[derive(Debug, Clone, Default)]
pub struct ManagerGuard;

/// Why the guard blocked a call. `Display` is what the model reads.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum GuardDenial {
    /// A refusal came back since the user's last message: the run is over.
    AfterRefusal,
    /// `amp_complete_task` / `amp_claim_task` on a ticket nobody asked about.
    OutOfScope { tool: String, ticket: Option<i64> },
}

impl std::fmt::Display for GuardDenial {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            GuardDenial::AfterRefusal => write!(
                f,
                "Not run: AMP refused an earlier call because it needs a person in the AMP web UI, \
                 so the harness ended this turn. Give your user the link and wait."
            ),
            GuardDenial::OutOfScope { tool, ticket } => {
                let which = ticket.map_or_else(|| "this ticket".to_owned(), |t| format!("#{t}"));
                write!(
                    f,
                    "Denied by the amp-manager tool policy: `{tool}` on {which}, which your user \
                     didn't name in this session and you didn't dispatch. Only touch the tickets \
                     you were asked about; status questions are read-only. A person's own ticket \
                     is marked done by them in the AMP web UI. Tell your user what you see and \
                     ask before acting."
                )
            }
        }
    }
}

impl ManagerGuard {
    /// `Err` when the call must not run, given the conversation so far.
    pub fn check(
        &self,
        tool: &str,
        args: &Value,
        context: &AgentContext,
    ) -> Result<(), GuardDenial> {
        if refused_since_last_user(&context.messages) {
            return Err(GuardDenial::AfterRefusal);
        }
        if TICKET_SCOPED_TOOLS.contains(&tool) {
            let ticket = id_of(&args["task_id"]);
            let in_scope = ticket.is_some_and(|t| {
                user_named(&context.messages).contains(&t)
                    || dispatched(&context.messages).contains(&t)
            });
            if !in_scope {
                return Err(GuardDenial::OutOfScope {
                    tool: tool.to_owned(),
                    ticket,
                });
            }
        }
        Ok(())
    }

    /// The hook: the guard, then `next` for allowed calls.
    pub fn before_tool_call_then(self: &Arc<Self>, next: Option<BeforeToolCall>) -> BeforeToolCall {
        let this = Arc::clone(self);
        Arc::new(
            move |ctx: BeforeToolCallContext<'_>, signal: CancellationToken| -> BeforeFuture {
                match this.check(&ctx.tool_call.name, ctx.args, ctx.context) {
                    Err(denial) => {
                        let terminate = denial == GuardDenial::AfterRefusal;
                        let result = BeforeToolCallResult {
                            block: true,
                            reason: Some(denial.to_string()),
                            terminate,
                            args: None,
                        };
                        Box::pin(async move { Some(result) })
                    }
                    Ok(()) => match &next {
                        Some(next) => next(ctx, signal),
                        None => Box::pin(async { None }),
                    },
                }
            },
        )
    }
}

/// A tool call's `task_id` argument (`9`, `"9"`, `"#9"`).
pub fn task_id_arg(args: &Value) -> Option<i64> {
    id_of(&args["task_id"])
}

fn id_of(v: &Value) -> Option<i64> {
    match v {
        Value::Number(n) => n.as_i64(),
        Value::String(s) => s.trim().trim_start_matches('#').parse().ok(),
        _ => None,
    }
}

/// Whether an error tool result after the user's last message is a refusal.
pub fn refused_since_last_user(messages: &[AgentMessage]) -> bool {
    messages
        .iter()
        .rev()
        .take_while(|m| !matches!(m, AgentMessage::User(_)))
        .any(|m| match m {
            AgentMessage::ToolResult(r) => {
                r.is_error && refusal(&content_text(&r.content)).is_some()
            }
            _ => false,
        })
}

fn content_text(content: &[Content]) -> String {
    content
        .iter()
        .filter_map(|c| match c {
            Content::Text(t) => Some(t.text.as_str()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// Every number the user typed in this session (`#2171`, `2171`, `ticket 2171`).
pub fn user_named(messages: &[AgentMessage]) -> BTreeSet<i64> {
    let mut out = BTreeSet::new();
    for m in messages {
        let AgentMessage::User(u) = m else { continue };
        let text = match &u.content {
            UserContent::Text(s) => s.clone(),
            UserContent::Blocks(blocks) => content_text(blocks),
        };
        out.extend(numbers(&text));
    }
    out
}

/// Digit runs that stand alone as words (`#12`, `12.` count; `v12`, `12px` don't).
fn numbers(text: &str) -> Vec<i64> {
    let chars: Vec<char> = text.chars().collect();
    let mut out = Vec::new();
    let mut i = 0;
    while i < chars.len() {
        if chars[i].is_ascii_digit() && (i == 0 || !chars[i - 1].is_alphanumeric()) {
            let start = i;
            while i < chars.len() && chars[i].is_ascii_digit() {
                i += 1;
            }
            let bounded = i == chars.len() || !chars[i].is_alphanumeric();
            if bounded && let Ok(n) = chars[start..i].iter().collect::<String>().parse() {
                out.push(n);
            }
        } else {
            i += 1;
        }
    }
    out
}

/// Tickets this session dispatched (`amp_run_ticket` / `amp_dispatch_task` calls).
pub fn dispatched(messages: &[AgentMessage]) -> BTreeSet<i64> {
    let mut out = BTreeSet::new();
    for m in messages {
        let AgentMessage::Assistant(a) = m else {
            continue;
        };
        for c in &a.content {
            if let Content::ToolCall(tc) = c
                && DISPATCH_TOOLS.contains(&tc.name.as_str())
                && let Some(id) = id_of(&tc.arguments["task_id"])
            {
                out.insert(id);
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const PLAN: &str = "Plan approval is done by a person in the AMP web UI, not by an agent or \
         over MCP: http://localhost:5173/project/2/plan. Stop and ask your person to do it there. \
         {\"approve_url\":\"http://localhost:5173/project/2/plan\",\"awaiting_approval\":[7,8]}";

    #[test]
    fn recognises_the_servers_refusals() {
        let r = refusal(PLAN).unwrap();
        assert_eq!(r.kind, RefusalKind::PlanApproval);
        assert_eq!(
            r.link.as_deref(),
            Some("http://localhost:5173/project/2/plan")
        );
        assert_eq!(r.project_id, Some(2));
        assert!(!r.message.contains("approve_url"), "{}", r.message);
        assert!(r.message.ends_with("do it there."), "{}", r.message);

        let gate = "ticket 9 was planned after the plan was last approved; this project needs the \
             plan approved before agents start new tickets. A person approves it in the AMP web UI \
             (Approve plan): /project/3/plan. Agents can't approve: stop and ask your person.";
        let r = refusal(gate).unwrap();
        assert_eq!(r.kind, RefusalKind::PlanApproval);
        assert_eq!(r.link.as_deref(), Some("/project/3/plan"));
        assert_eq!(r.ticket_id, Some(9));
        assert!(!r.per_ticket);

        let human = "amp_complete_task: Marking ticket 41 (a person's ticket) done is done by a \
             person in the AMP web UI, not by an agent or over MCP: \
             https://amp.test/project/2/board?task=41. Stop and ask your person to do it there.";
        let r = refusal(human).unwrap();
        assert_eq!(r.kind, RefusalKind::PersonInUi);
        assert_eq!((r.project_id, r.ticket_id), (Some(2), Some(41)));
        assert!(!r.per_ticket);

        // #2198: per-ticket approval.
        let dispatch = "tool error: amp_dispatch_task: tools/call amp_dispatch_task: Mcp error: \
             -32603: ticket 2203 isn't approved: nobody has approved it since it was planned or its \
             scope last changed, and this project needs each ticket approved before agents start \
             it. Its owner approves it in the AMP web UI (Approve plan; a project lead may approve \
             it on their behalf): /project/2/plan. Agents can't approve: stop and ask your person.";
        let r = refusal(dispatch).unwrap();
        assert_eq!(r.kind, RefusalKind::PlanApproval);
        assert_eq!((r.project_id, r.ticket_id), (Some(2), Some(2203)));
        assert!(r.per_ticket);
        let complete = "ticket 77 can't be completed: it isn't approved since it was planned or its \
             scope last changed, and agents can't move unapproved work forward. A person approves \
             the plan in the AMP web UI: https://amp.test/project/4/plan. Stop and ask.";
        let r = refusal(complete).unwrap();
        assert_eq!(
            (r.kind, r.ticket_id, r.per_ticket),
            (RefusalKind::PlanApproval, Some(77), true)
        );

        for other in [
            "ticket 5 is backlog: it was never dispatched, so an agent can't complete it",
            "task 99999 not found",
            "",
        ] {
            assert_eq!(refusal(other), None, "{other}");
        }
    }

    /// #2226: `amp_run_ticket` wraps the dispatch refusal; the ticket is the one it names.
    #[test]
    fn run_ticket_refusals_name_their_ticket() {
        let wrapped = "tool error: AMP refused to dispatch ticket #196: tools/call \
             amp_dispatch_task: Mcp error: -32603: ticket 196 isn't approved: nobody has approved \
             it since it was planned or its scope last changed, and this project needs each ticket \
             approved before agents start it. Its owner approves it in the AMP web UI (Approve \
             plan; a project lead may approve it on their behalf): /project/71/plan. Agents can't \
             approve: stop and ask your person to approve it there.";
        let r = refusal(wrapped).unwrap();
        assert_eq!(r.kind, RefusalKind::PlanApproval);
        assert_eq!((r.project_id, r.ticket_id), (Some(71), Some(196)));
        assert!(r.per_ticket);
        assert!(
            r.message.starts_with("ticket 196 isn't approved"),
            "{}",
            r.message
        );
        for noise in [
            "tool error",
            "tools/call",
            "Mcp error",
            "-32603",
            "refused to dispatch",
        ] {
            assert!(!r.message.contains(noise), "{noise}: {}", r.message);
        }

        // Only the harness's own wrapping (no MCP code): the ticket is still the one named.
        let r = refusal(
            "AMP refused to dispatch ticket #12: ticket #12 was planned after the plan was last \
             approved. A person approves it: /project/3/plan.",
        )
        .unwrap();
        assert_eq!(r.ticket_id, Some(12));
        assert!(
            r.message.starts_with("ticket #12 was planned"),
            "{}",
            r.message
        );

        assert_eq!(
            server_sentence("tool error: amp_dispatch_task: ticket 9 isn't approved"),
            "ticket 9 isn't approved"
        );
        assert_eq!(server_sentence("plain words: here"), "plain words: here");
    }

    #[test]
    fn numbers_stand_alone() {
        assert_eq!(
            numbers("finish #2171 and 12, not v3 or 4px; ticket 88."),
            vec![2171, 12, 88]
        );
    }
}
