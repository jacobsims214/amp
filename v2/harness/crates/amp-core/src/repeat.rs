//! The repeat guard (#2225, #5624): a model that keeps making the same tool call is refused, and
//! then stopped.
//!
//! QA pass 2 found a manager (openrouter `xiaomi/mimo-v2.6-pro`) that emitted `bash {"command":
//! "ls"}` about 30 times over three user turns while its thinking said it was calling `read` and
//! `amp_*` tools. Every call succeeded with the same `README.md`, so nothing in the loop ever
//! looked wrong to the harness, and the model finally told its user AMP's tools were broken.
//!
//! **#5624 changed the escalation, because the old one lost whole worker runs.** The old guard ran
//! the 3rd identical call and appended one sentence after an *identical success* — still
//! `is_error: false`, still the same result the model had already read. Four workers (#5540,
//! #5557, #5585, #5587) kept going to the 5th call anyway and were killed there; #5587 burned an
//! hour and 393 K output tokens and produced nothing. A postscript on a repeated success does not
//! change the next generation, so the old nudge was not a control at all. Now:
//!
//! The guard counts identical calls (same tool name, same arguments) in a row since the user's
//! last message. Any other call in between resets the count, and so does a new user message.
//! - From the [`WARN_AT`]th identical call, the call runs and a line in its result says the next
//!   identical call will **not** run ([`RepeatGuard::after_tool_call_then`]). This is the last
//!   chance to change course, and it still costs nothing.
//! - The [`REFUSE_AT`]th identical call **does not run**: it is blocked as an error that says so,
//!   with `terminate` **false**, so the run continues. The model's next context is no longer an
//!   identical success it can rationalise — it is a distinct refusal it has to act on.
//! - The [`STOP_AT`]th identical call is not run and **ends the run**: blocked with `terminate`,
//!   the tool error starting with [`STOPPED_MARK`]. Every later call in that run is blocked with
//!   `terminate` too, so a batch that mixed in other calls still ends at the next turn.
//! - [`stopped`] recognises the stop in a tool result, so the chat transcript shows one sentence
//!   ([`RepeatStop::user_line`]), `run -p` one stderr line, and a worker records an abandoned run
//!   saying why ([`RepeatStop::reason`]).
//!
//! It is the first `before_tool_call` hook for every role (before the manager's approval guard and
//! the role policy), so a call the policy denies over and over is refused as well. It counts calls
//! from the conversation itself, so it keeps no state of its own.

use std::{future::Future, pin::Pin, sync::Arc};

use rpi_agent::{
    AfterToolCall, AfterToolCallContext, AfterToolCallResult, AgentContext, AgentMessage,
    BeforeToolCall, BeforeToolCallContext, BeforeToolCallResult, TextContentOrImage,
};
use rpi_ai::types::{AssistantMessage, Content};
use serde_json::Value;
use tokio_util::sync::CancellationToken;

/// The identical call (counting the first) whose result warns that the next one will not run.
pub const WARN_AT: u32 = 2;
/// The identical call (counting the first) that is **not run**: the model is told to act on the
/// result it already has, and the run continues.
pub const REFUSE_AT: u32 = 3;
/// The identical call (counting the first) that is not run and ends the run.
pub const STOP_AT: u32 = 5;

/// Starts the tool error of the call that stopped the run (what [`stopped`] looks for).
pub const STOPPED_MARK: &str = "[amp-harness] Stopped a repeating model:";
/// Starts the tool error of a call the guard refused (the run continues).
pub const REFUSED_MARK: &str = "[amp-harness] Not run: this exact call";
/// The tool error of every call after the stop, in the same run.
pub const AFTER_STOP: &str = "Not run: the harness stopped this run because the model kept \
     repeating one tool call. Tell your user what you were trying to do.";

/// How many identical calls in a row are warned, refused and stopped.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RepeatGuard {
    pub warn_at: u32,
    pub refuse_at: u32,
    pub stop_at: u32,
}

impl Default for RepeatGuard {
    fn default() -> Self {
        Self {
            warn_at: WARN_AT,
            refuse_at: REFUSE_AT,
            stop_at: STOP_AT,
        }
    }
}

/// What the guard makes of one call.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Verdict {
    Run,
    /// Run it, and warn that the next identical call will not run.
    Warn {
        count: u32,
    },
    /// Don't run it: the model already has this result and must act on it. The run continues.
    Refuse {
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
        } else if count >= self.refuse_at {
            Verdict::Refuse { count }
        } else if count >= self.warn_at {
            Verdict::Warn { count }
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
    ///
    /// A refused or stopped call is blocked with `reason` (the model reads it). Only the stop sets
    /// `terminate`: a refusal leaves the run alive on purpose, so the model still has the whole
    /// rest of its turn to do the work from the result it already holds.
    pub fn before_tool_call_then(self, next: Option<BeforeToolCall>) -> BeforeToolCall {
        Arc::new(
            move |ctx: BeforeToolCallContext<'_>, signal: CancellationToken| -> BeforeFuture {
                let blocked =
                    match self.check(ctx.context, ctx.assistant_message, &ctx.tool_call.id) {
                        Verdict::Stop { count, call } => Some((stop_text(&call, count), true)),
                        Verdict::Refuse { count } => Some((refused_text(count), false)),
                        Verdict::AfterStop => Some((AFTER_STOP.to_owned(), true)),
                        Verdict::Run | Verdict::Warn { .. } => None,
                    };
                match (blocked, &next) {
                    (Some((reason, terminate)), _) => Box::pin(async move {
                        Some(BeforeToolCallResult {
                            block: true,
                            reason: Some(reason),
                            terminate,
                            args: None,
                        })
                    }),
                    (None, Some(next)) => next(ctx, signal),
                    (None, None) => Box::pin(async { None }),
                }
            },
        )
    }

    /// The `after_tool_call` hook: `next`, then the warning line on a repeated call's result.
    pub fn after_tool_call_then(self, next: Option<AfterToolCall>) -> AfterToolCall {
        Arc::new(
            move |ctx: AfterToolCallContext<'_>, signal: CancellationToken| -> AfterFuture {
                let warning =
                    match self.check(ctx.context, ctx.assistant_message, &ctx.tool_call.id) {
                        Verdict::Warn { count } => Some(warn_text(count, self.refuse_at)),
                        _ => None,
                    };
                let original = ctx.result.content.clone();
                let inner = next.as_ref().map(|n| n(ctx, signal));
                Box::pin(async move {
                    let mut out = match inner {
                        Some(f) => f.await,
                        None => None,
                    };
                    if let Some(line) = warning {
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

fn warn_text(count: u32, refuse_at: u32) -> String {
    format!(
        "[amp-harness] You have made this exact call {count} times in a row (same tool, same \
         arguments). The next identical call will NOT run (that would be {refuse_at} in a row): the \
         result you already have is the answer, and only new arguments or a different tool will \
         run. Act on what you have."
    )
}

/// The tool error of a call the guard refused (the model reads it; the run continues).
///
/// It names the call and says plainly that nothing was run, so a model cannot read it as another
/// success to re-request — which is exactly how four workers burned their runs (#5624).
pub fn refused_text(count: u32) -> String {
    format!(
        "{REFUSED_MARK} has already run {count} times in a row with the same arguments, so it was \
         NOT run again and this is not a new result. The result you already have is the whole \
         answer to this call: use it and continue the work. Change the arguments or use a \
         different tool if you need something else; calling it again unchanged ends the run."
    )
}

/// Whether a tool result is a refusal (as opposed to a stop): the run is still alive.
pub fn refused(text: &str) -> bool {
    text.trim_start().starts_with(REFUSED_MARK)
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
    fn identical_calls_are_warned_then_refused_then_stopped_then_blocked() {
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
                Verdict::Warn { count: 2 },
                // #5624: the 3rd identical call is not run, but the run continues.
                Verdict::Refuse { count: 3 },
                Verdict::Refuse { count: 4 },
                Verdict::Stop {
                    count: 5,
                    call: "bash ls".into()
                },
                Verdict::AfterStop,
            ]
        );
    }

    #[test]
    fn a_refusal_is_not_run_but_does_not_end_the_run() {
        // The whole point of #5624: at 3 and 4 the model gets an error it must act on, and the
        // run is still alive — so the work can still land. Only 5 ends it.
        let ls = ("bash", json!({"command": "ls"}));
        let v = verdicts(&[ls.clone(), ls.clone(), ls.clone(), ls.clone()]);
        assert_eq!(v[2], Verdict::Refuse { count: 3 });
        assert_eq!(v[3], Verdict::Refuse { count: 4 });
        assert!(
            !v.iter().any(|x| matches!(x, Verdict::Stop { .. })),
            "nothing below 5 stops the run: {v:?}"
        );
        assert!(!v.iter().any(|x| matches!(x, Verdict::AfterStop)), "{v:?}");
    }

    #[test]
    fn a_refused_call_reads_as_a_refusal_and_never_as_the_success_it_replaces() {
        let text = refused_text(3);
        assert!(refused(&text), "{text}");
        // It parses as no stop, so the run is not reported as abandoned.
        assert_eq!(stopped(&text), None);
        // And it says plainly that nothing was run, which is what stops the retry loop.
        assert!(text.contains("NOT run"), "{text}");
        assert!(text.contains("not a new result"), "{text}");
        assert!(text.contains("already have"), "{text}");
        assert!(!refused(&stop_text("bash ls", 5)));
        assert!(!refused(AFTER_STOP));
        assert!(!refused("README.md"));
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
        // The most in a row here is four, so a run is never *ended*; the 3rd and 4th identical
        // call are refused (#5624), which costs the model a result it already has and nothing else.
        assert!(verdicts(&calls).iter().all(|v| matches!(
            v,
            Verdict::Run | Verdict::Warn { .. } | Verdict::Refuse { .. }
        )));
        assert!(
            !verdicts(&calls)
                .iter()
                .any(|v| matches!(v, Verdict::Stop { .. } | Verdict::AfterStop))
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
