//! The repeat guard (#2225): a model that keeps making the same tool call is stopped.
//!
//! QA pass 2 found a manager (openrouter `xiaomi/mimo-v2.6-pro`) that emitted `bash {"command":
//! "ls"}` about 30 times over three user turns while its thinking said it was calling `read` and
//! `amp_*` tools. Every call succeeded with the same `README.md`, so nothing in the loop ever
//! looked wrong to the harness, and the model finally told its user AMP's tools were broken.
//!
//! The guard counts identical calls (same tool name, same arguments) in a row since the user's
//! last message. Any other call in between resets the count, and so does a new user message.
//! - From the [`NUDGE_AT`]th identical call, the call runs and the model reads a line in its
//!   result saying it is repeating itself ([`RepeatGuard::after_tool_call_then`]).
//! - The [`STOP_AT`]th identical call is not run: it is blocked with `terminate`, and the tool
//!   error starts with [`STOPPED_MARK`]. Every later call in that run is blocked with `terminate`
//!   too, so a batch that mixed in other calls still ends at the next turn.
//! - [`stopped`] recognises the stop in a tool result, so the chat transcript shows one sentence
//!   ([`RepeatStop::user_line`]), `run -p` one stderr line, and a worker records an abandoned run
//!   saying why ([`RepeatStop::reason`]).
//!
//! It is the first `before_tool_call` hook for every role (before the manager's approval guard and
//! the role policy), so a call the policy denies over and over is stopped as well. It counts calls
//! from the conversation itself, so it keeps no state of its own.

use std::{future::Future, pin::Pin, sync::Arc};

use rpi_agent::{
    AfterToolCall, AfterToolCallContext, AfterToolCallResult, AgentContext, AgentMessage,
    BeforeToolCall, BeforeToolCallContext, BeforeToolCallResult, TextContentOrImage,
};
use rpi_ai::types::{AssistantMessage, Content};
use serde_json::Value;
use tokio_util::sync::CancellationToken;

/// The identical call (counting the first) whose result tells the model it is repeating itself.
pub const NUDGE_AT: u32 = 3;
/// The identical call (counting the first) that is not run and ends the run.
pub const STOP_AT: u32 = 5;

/// Starts the tool error of the call that stopped the run (what [`stopped`] looks for).
pub const STOPPED_MARK: &str = "[amp-harness] Stopped a repeating model:";
/// The tool error of every call after the stop, in the same run.
pub const AFTER_STOP: &str = "Not run: the harness stopped this run because the model kept \
     repeating one tool call. Tell your user what you were trying to do.";

/// How many identical calls in a row are nudged and stopped.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RepeatGuard {
    pub nudge_at: u32,
    pub stop_at: u32,
}

impl Default for RepeatGuard {
    fn default() -> Self {
        Self {
            nudge_at: NUDGE_AT,
            stop_at: STOP_AT,
        }
    }
}

/// What the guard makes of one call.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Verdict {
    Run,
    /// Run it, and tell the model it made this call `count` times in a row.
    Nudge {
        count: u32,
    },
    /// Don't run it: the run ends here.
    Stop {
        count: u32,
        call: String,
    },
    /// The run was already stopped: don't run anything more.
    AfterStop,
}

impl RepeatGuard {
    /// The verdict on the last of `calls` (the calls since the user's last message, in order, as
    /// `(tool name, arguments)`).
    pub fn assess(&self, calls: &[(&str, &Value)]) -> Verdict {
        let Some((last, earlier)) = calls.split_last() else {
            return Verdict::Run;
        };
        let mut streak = 0u32;
        let mut prev: Option<&(&str, &Value)> = None;
        for call in earlier {
            streak = if prev == Some(call) { streak + 1 } else { 1 };
            if streak >= self.stop_at {
                return Verdict::AfterStop;
            }
            prev = Some(call);
        }
        let count = if prev == Some(last) { streak + 1 } else { 1 };
        if count >= self.stop_at {
            Verdict::Stop {
                count,
                call: summary(last.0, last.1),
            }
        } else if count >= self.nudge_at {
            Verdict::Nudge { count }
        } else {
            Verdict::Run
        }
    }

    /// The verdict on `current` (a call of `assistant`), given the conversation in `context`.
    pub fn check(
        &self,
        context: &AgentContext,
        assistant: &AssistantMessage,
        current_id: &str,
    ) -> Verdict {
        let calls = calls_up_to(&context.messages, assistant, current_id);
        self.assess(&calls)
    }

    /// The `before_tool_call` hook: the guard, then `next` for calls it lets run.
    pub fn before_tool_call_then(self, next: Option<BeforeToolCall>) -> BeforeToolCall {
        Arc::new(
            move |ctx: BeforeToolCallContext<'_>, signal: CancellationToken| -> BeforeFuture {
                let blocked =
                    match self.check(ctx.context, ctx.assistant_message, &ctx.tool_call.id) {
                        Verdict::Stop { count, call } => Some(stop_text(&call, count)),
                        Verdict::AfterStop => Some(AFTER_STOP.to_owned()),
                        Verdict::Run | Verdict::Nudge { .. } => None,
                    };
                match (blocked, &next) {
                    (Some(reason), _) => Box::pin(async move {
                        Some(BeforeToolCallResult {
                            block: true,
                            reason: Some(reason),
                            terminate: true,
                            args: None,
                        })
                    }),
                    (None, Some(next)) => next(ctx, signal),
                    (None, None) => Box::pin(async { None }),
                }
            },
        )
    }

    /// The `after_tool_call` hook: `next`, then the nudge line on a repeated call's result.
    pub fn after_tool_call_then(self, next: Option<AfterToolCall>) -> AfterToolCall {
        Arc::new(
            move |ctx: AfterToolCallContext<'_>, signal: CancellationToken| -> AfterFuture {
                let nudge = match self.check(ctx.context, ctx.assistant_message, &ctx.tool_call.id)
                {
                    Verdict::Nudge { count } => Some(nudge_text(count, self.stop_at)),
                    _ => None,
                };
                let original = ctx.result.content.clone();
                let inner = next.as_ref().map(|n| n(ctx, signal));
                Box::pin(async move {
                    let mut out = match inner {
                        Some(f) => f.await,
                        None => None,
                    };
                    if let Some(line) = nudge {
                        let r = out.get_or_insert_with(AfterToolCallResult::default);
                        let content = r.content.get_or_insert(original);
                        content.push(TextContentOrImage::text(line));
                    }
                    out
                })
            },
        )
    }
}

type BeforeFuture = Pin<Box<dyn Future<Output = Option<BeforeToolCallResult>> + Send>>;
type AfterFuture = Pin<Box<dyn Future<Output = Option<AfterToolCallResult>> + Send>>;

/// The calls since the user's last message, up to and including `current_id` in `assistant`.
/// `context` may or may not hold `assistant` already; it is counted once either way.
fn calls_up_to<'a>(
    messages: &'a [AgentMessage],
    assistant: &'a AssistantMessage,
    current_id: &str,
) -> Vec<(&'a str, &'a Value)> {
    let start = messages
        .iter()
        .rposition(|m| matches!(m, AgentMessage::User(_)))
        .map_or(0, |i| i + 1);
    let has_current = |a: &AssistantMessage| {
        a.content
            .iter()
            .any(|c| matches!(c, Content::ToolCall(tc) if tc.id == current_id))
    };
    let mut calls: Vec<(&str, &Value)> = Vec::new();
    for m in &messages[start..] {
        if let AgentMessage::Assistant(a) = m
            && !has_current(a)
        {
            calls.extend(a.content.iter().filter_map(|c| match c {
                Content::ToolCall(tc) => Some((tc.name.as_str(), &tc.arguments)),
                _ => None,
            }));
        }
    }
    for c in &assistant.content {
        if let Content::ToolCall(tc) = c {
            calls.push((tc.name.as_str(), &tc.arguments));
            if tc.id == current_id {
                break;
            }
        }
    }
    calls
}

/// A call in one short line: `bash ls`, `amp_get_task {"task_id":7}`.
pub fn summary(name: &str, args: &Value) -> String {
    let rest = match args.get("command").and_then(Value::as_str) {
        Some(cmd) if name == "bash" => cmd.to_owned(),
        _ => match args {
            Value::Object(m) if m.is_empty() => String::new(),
            Value::Null => String::new(),
            other => other.to_string(),
        },
    };
    let one_line = format!("{name} {rest}")
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
        .replace('`', "'");
    const MAX: usize = 80;
    if one_line.chars().count() > MAX {
        one_line.chars().take(MAX - 3).collect::<String>() + "..."
    } else {
        one_line
    }
}

/// The tool error of the call that stops the run (the model reads it; [`stopped`] parses it).
pub fn stop_text(call: &str, count: u32) -> String {
    format!(
        "{STOPPED_MARK} `{call}` was called {count} times in a row with the same arguments, so \
         the harness ended this run without running it again."
    )
}

fn nudge_text(count: u32, stop_at: u32) -> String {
    format!(
        "[amp-harness] You have made this exact call {count} times in a row (same tool, same \
         arguments). Repeating it won't change the result: use a different tool or different \
         arguments, or answer your user. At {stop_at} identical calls in a row the harness stops \
         the run."
    )
}

/// A run the guard stopped, read back from the stopping call's tool error.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RepeatStop {
    /// The call, as [`summary`] wrote it.
    pub call: String,
    pub count: u32,
}

impl RepeatStop {
    /// The one sentence for a person. `switch` says how to change model: `/model` in the chat.
    pub fn user_line(&self, switch: &str) -> String {
        format!(
            "The model kept repeating `{}` and was stopped; try again, or {switch} to switch model.",
            self.call
        )
    }

    /// Why a worker's run ended, for its report and the abandoned run's note.
    pub fn reason(&self) -> String {
        format!(
            "the model kept repeating `{}` ({} identical calls in a row) and was stopped",
            self.call, self.count
        )
    }
}

/// The stop in a tool error's text, if this is the call that stopped the run.
pub fn stopped(text: &str) -> Option<RepeatStop> {
    let rest = text.trim_start().strip_prefix(STOPPED_MARK)?;
    let rest = rest.trim_start().strip_prefix('`')?;
    let (call, rest) = rest.split_once('`')?;
    let count = rest
        .trim_start()
        .strip_prefix("was called ")?
        .split_whitespace()
        .next()?
        .parse()
        .ok()?;
    Some(RepeatStop {
        call: call.to_owned(),
        count,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn verdicts(calls: &[(&str, Value)]) -> Vec<Verdict> {
        let g = RepeatGuard::default();
        (1..=calls.len())
            .map(|n| {
                let seq: Vec<(&str, &Value)> = calls[..n].iter().map(|(a, b)| (*a, b)).collect();
                g.assess(&seq)
            })
            .collect()
    }

    #[test]
    fn identical_calls_are_nudged_then_stopped_then_everything_is_blocked() {
        let ls = ("bash", json!({"command": "ls"}));
        let other = ("read", json!({"path": "README.md"}));
        let v = verdicts(&[
            ls.clone(),
            ls.clone(),
            ls.clone(),
            ls.clone(),
            ls.clone(),
            other,
        ]);
        assert_eq!(
            v,
            vec![
                Verdict::Run,
                Verdict::Run,
                Verdict::Nudge { count: 3 },
                Verdict::Nudge { count: 4 },
                Verdict::Stop {
                    count: 5,
                    call: "bash ls".into()
                },
                Verdict::AfterStop,
            ]
        );
    }

    #[test]
    fn different_calls_or_arguments_reset_the_count() {
        let ls = ("bash", json!({"command": "ls"}));
        let la = ("bash", json!({"command": "ls -a"}));
        let calls = [
            ls.clone(),
            ls.clone(),
            ls.clone(),
            ls.clone(),
            la.clone(),
            ls.clone(),
            ls.clone(),
            ls.clone(),
            ls.clone(),
            la,
        ];
        assert!(
            verdicts(&calls)
                .iter()
                .all(|v| matches!(v, Verdict::Run | Verdict::Nudge { .. }))
        );
        // Same arguments in another key order are the same call.
        let a = (
            "amp_get_task",
            json!({"task_id": 7, "include": ["comments"]}),
        );
        let b: Value = serde_json::from_str(r#"{"include":["comments"],"task_id":7}"#).unwrap();
        let v = verdicts(&[a.clone(), a.clone(), a.clone(), a, ("amp_get_task", b)]);
        assert!(matches!(v[4], Verdict::Stop { count: 5, .. }), "{v:?}");
    }

    #[test]
    fn the_stop_reads_back_as_one_sentence() {
        let text = stop_text(&summary("bash", &json!({"command": "ls"})), 5);
        let stop = stopped(&text).unwrap();
        assert_eq!(
            stop,
            RepeatStop {
                call: "bash ls".into(),
                count: 5
            }
        );
        assert_eq!(
            stop.user_line("/model"),
            "The model kept repeating `bash ls` and was stopped; try again, or /model to switch \
             model."
        );
        assert_eq!(
            stop.reason(),
            "the model kept repeating `bash ls` (5 identical calls in a row) and was stopped"
        );
        assert_eq!(stopped(AFTER_STOP), None);
        assert_eq!(stopped("README.md"), None);
    }

    #[test]
    fn summaries_are_short_and_backtick_free() {
        assert_eq!(summary("amp_whoami", &json!({})), "amp_whoami");
        assert_eq!(
            summary("amp_get_task", &json!({"task_id": 7})),
            "amp_get_task {\"task_id\":7}"
        );
        assert_eq!(
            summary("bash", &json!({"command": "echo `x`\n"})),
            "bash echo 'x'"
        );
        let long = summary("bash", &json!({"command": "x".repeat(200)}));
        assert_eq!(long.chars().count(), 80);
        assert!(long.ends_with("..."));
    }
}
