//! Run events: what a running agent did, in a stable JSON shape (`amp-harness run --json`), plus
//! the emitter that gets rpi's events out of the agent loop.
//!
//! - [`ChannelEmitter`] is the `AgentHarnessOptions::agent_emitter`: it forwards every rpi
//!   [`AgentEvent`] into an unbounded channel, so the consumer (the headless printer, the M2-7 TUI,
//!   the M2-5 worker supervisor) never slows the loop down.
//! - [`RunEvent::from_agent`] maps rpi's events to ours. Streaming noise (thinking and tool-call
//!   argument deltas, per-message starts) is dropped; text deltas, finished assistant messages
//!   (with usage), tool starts/ends and retries are kept.
//! - [`Totals`] sums usage over assistant messages and counts turns and tool calls (D8's
//!   accounting starts here). [`TotalsEmitter`] keeps a session's live totals (the manager's
//!   `amp_session_usage`) and passes every event on.
//!
//! The JSON is a contract for scripts: fields may be added, never renamed or removed.

use std::sync::{Arc, Mutex};

use rpi_agent::{AgentEmitter, AgentEvent, AgentMessage, AgentToolResult, TextContentOrImage};
use rpi_ai::types::{AssistantMessage, AssistantMessageEvent, Content, StopReason, Usage};
use serde::Serialize;
use serde_json::Value;
use tokio::sync::mpsc;

/// Forwards rpi agent events into a channel.
#[derive(Clone)]
pub struct ChannelEmitter {
    tx: mpsc::UnboundedSender<AgentEvent>,
}

impl ChannelEmitter {
    pub fn new() -> (Self, mpsc::UnboundedReceiver<AgentEvent>) {
        let (tx, rx) = mpsc::unbounded_channel();
        (Self { tx }, rx)
    }

    pub fn into_emitter(self) -> Arc<dyn AgentEmitter> {
        Arc::new(self)
    }
}

impl AgentEmitter for ChannelEmitter {
    fn emit(&self, event: AgentEvent) -> futures_core_box::BoxFuture {
        // A closed receiver means nobody is listening any more; the run carries on.
        let _ = self.tx.send(event);
        Box::pin(std::future::ready(()))
    }

    fn try_emit(&self, event: AgentEvent) {
        let _ = self.tx.send(event);
    }
}

/// Updates a session's live [`Totals`] synchronously with every event (so a tool that reads them
/// sees the message that called it), then passes the event on to `inner`.
pub struct TotalsEmitter {
    totals: Arc<Mutex<Totals>>,
    inner: Option<Arc<dyn AgentEmitter>>,
}

impl TotalsEmitter {
    pub fn new(totals: Arc<Mutex<Totals>>, inner: Option<Arc<dyn AgentEmitter>>) -> Self {
        Self { totals, inner }
    }

    pub fn into_emitter(self) -> Arc<dyn AgentEmitter> {
        Arc::new(self)
    }

    fn observe(&self, event: &AgentEvent) {
        if let Some(e) = RunEvent::from_agent(event)
            && let Ok(mut t) = self.totals.lock()
        {
            t.observe(&e);
        }
    }
}

impl AgentEmitter for TotalsEmitter {
    fn emit(&self, event: AgentEvent) -> futures_core_box::BoxFuture {
        self.observe(&event);
        match &self.inner {
            Some(inner) => inner.emit(event),
            None => Box::pin(std::future::ready(())),
        }
    }

    fn try_emit(&self, event: AgentEvent) {
        self.observe(&event);
        if let Some(inner) = &self.inner {
            inner.try_emit(event);
        }
    }
}

/// `futures::future::BoxFuture<'static, ()>` without depending on `futures`.
mod futures_core_box {
    pub type BoxFuture = std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send + 'static>>;
}

/// Token usage, as AMP's run record wants it.
#[derive(Debug, Clone, Copy, Default, PartialEq, Serialize)]
pub struct UsageJson {
    pub input_tokens: i64,
    pub output_tokens: i64,
    pub cache_read_tokens: i64,
    pub cache_write_tokens: i64,
    pub cost_usd: f64,
}

impl From<&Usage> for UsageJson {
    fn from(u: &Usage) -> Self {
        Self {
            input_tokens: u.input,
            output_tokens: u.output,
            cache_read_tokens: u.cache_read,
            cache_write_tokens: u.cache_write,
            cost_usd: u.cost.total,
        }
    }
}

/// A tool call the model made.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct ToolCallJson {
    pub id: String,
    pub name: String,
    pub arguments: Value,
}

/// One event of a run, as `run --json` prints it (one JSON object per line, tagged by `type`).
#[derive(Debug, Clone, PartialEq, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum RunEvent {
    /// A model turn started (one assistant response plus its tool calls).
    TurnStart,
    /// Streamed assistant text.
    TextDelta { delta: String },
    /// A finished assistant message.
    Assistant {
        text: String,
        tool_calls: Vec<ToolCallJson>,
        /// rpi's stop reason: stop, length, tool_use, error, aborted (pending, deferred).
        stop_reason: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        error: Option<String>,
        model: String,
        provider: String,
        usage: UsageJson,
    },
    /// A tool call started executing.
    ToolStart {
        id: String,
        name: String,
        arguments: Value,
    },
    /// A tool call finished. `is_error` covers policy denials, AMP tool errors and timeouts.
    ToolEnd {
        id: String,
        name: String,
        is_error: bool,
        text: String,
    },
    /// The provider failed and the request will be retried.
    Retry {
        attempt: u32,
        max_retries: u32,
        delay_ms: u64,
        error: String,
    },
    /// A turn's assistant message and tool results settled.
    TurnEnd,
}

impl RunEvent {
    /// Maps one rpi event; `None` for events `run --json` does not report.
    pub fn from_agent(event: &AgentEvent) -> Option<Self> {
        match event {
            AgentEvent::TurnStart => Some(Self::TurnStart),
            AgentEvent::TurnEnd { .. } => Some(Self::TurnEnd),
            AgentEvent::MessageUpdate {
                assistant_message_event: AssistantMessageEvent::TextDelta { delta, .. },
                ..
            } => Some(Self::TextDelta {
                delta: delta.clone(),
            }),
            AgentEvent::MessageEnd {
                message: AgentMessage::Assistant(m),
            } => Some(Self::assistant(m)),
            AgentEvent::ToolExecutionStart {
                tool_call_id,
                tool_name,
                args,
            } => Some(Self::ToolStart {
                id: tool_call_id.clone(),
                name: tool_name.clone(),
                arguments: args.clone(),
            }),
            AgentEvent::ToolExecutionEnd {
                tool_call_id,
                tool_name,
                result,
                is_error,
            } => Some(Self::ToolEnd {
                id: tool_call_id.clone(),
                name: tool_name.clone(),
                is_error: *is_error,
                text: tool_result_text(result),
            }),
            AgentEvent::RetryScheduled {
                attempt,
                max_retries,
                delay_ms,
                error,
            } => Some(Self::Retry {
                attempt: *attempt,
                max_retries: *max_retries,
                delay_ms: *delay_ms,
                // The whole provider body, minus the account id (#2207).
                error: crate::provider_error::redact(error),
            }),
            _ => None,
        }
    }

    fn assistant(m: &AssistantMessage) -> Self {
        Self::Assistant {
            text: assistant_text(m),
            tool_calls: m
                .content
                .iter()
                .filter_map(|c| match c {
                    Content::ToolCall(tc) => Some(ToolCallJson {
                        id: tc.id.clone(),
                        name: tc.name.clone(),
                        arguments: tc.arguments.clone(),
                    }),
                    _ => None,
                })
                .collect(),
            stop_reason: stop_reason_str(m.stop_reason).to_owned(),
            error: m.error_message.clone(),
            model: m.model.clone(),
            provider: m.provider.clone(),
            usage: UsageJson::from(&m.usage),
        }
    }
}

/// rpi's stop reason as a snake_case word.
pub fn stop_reason_str(reason: StopReason) -> &'static str {
    match reason {
        StopReason::Stop => "stop",
        StopReason::Length => "length",
        StopReason::ToolUse => "tool_use",
        StopReason::Error => "error",
        StopReason::Aborted => "aborted",
        StopReason::Pending => "pending",
        StopReason::Deferred => "deferred",
    }
}

/// The text blocks of an assistant message, joined (thinking and tool calls left out).
pub fn assistant_text(m: &AssistantMessage) -> String {
    m.content
        .iter()
        .filter_map(|c| match c {
            Content::Text(t) => Some(t.text.as_str()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("")
}

/// The text blocks of a tool result, joined with newlines.
pub fn tool_result_text(result: &AgentToolResult) -> String {
    result
        .content
        .iter()
        .filter_map(|c| match c {
            TextContentOrImage::Text(t) => Some(t.text.as_str()),
            TextContentOrImage::Image(_) => None,
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// Running totals over a run's events.
#[derive(Debug, Clone, Default, PartialEq, Serialize)]
pub struct Totals {
    pub turns: u32,
    pub tool_calls: u32,
    pub tool_errors: u32,
    /// Distinct tool names called, in first-call order.
    pub tools_used: Vec<String>,
    pub usage: UsageJson,
    /// The last assistant message's text (the run's answer).
    pub last_text: String,
    /// The latest response's context size: its input, output and cache tokens (#2347), what the
    /// chat's `ctx N%` divides by the model's window. Not part of the run record.
    #[serde(skip)]
    pub context_tokens: i64,
}

impl Totals {
    pub fn observe(&mut self, event: &RunEvent) {
        match event {
            RunEvent::TurnStart => self.turns += 1,
            RunEvent::Assistant { text, usage, .. } => {
                self.usage.input_tokens += usage.input_tokens;
                self.usage.output_tokens += usage.output_tokens;
                self.usage.cache_read_tokens += usage.cache_read_tokens;
                self.usage.cache_write_tokens += usage.cache_write_tokens;
                self.usage.cost_usd += usage.cost_usd;
                let context = usage.input_tokens
                    + usage.output_tokens
                    + usage.cache_read_tokens
                    + usage.cache_write_tokens;
                if context > 0 {
                    self.context_tokens = context;
                }
                self.last_text = text.clone();
            }
            RunEvent::ToolStart { name, .. } => {
                self.tool_calls += 1;
                if !self.tools_used.iter().any(|n| n == name) {
                    self.tools_used.push(name.clone());
                }
            }
            RunEvent::ToolEnd { is_error: true, .. } => self.tool_errors += 1,
            _ => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use rpi_ai::providers::faux::{FauxBlock, faux_assistant_message};
    use serde_json::json;

    use super::*;

    #[test]
    fn assistant_messages_and_tools_map_to_the_json_contract() {
        let m = faux_assistant_message(
            vec![
                FauxBlock::text("Checking."),
                FauxBlock::tool_call("amp_whoami", json!({})),
            ],
            StopReason::ToolUse,
        );
        let e = RunEvent::from_agent(&AgentEvent::MessageEnd {
            message: AgentMessage::Assistant(Box::new(m)),
        })
        .unwrap();
        let v = serde_json::to_value(&e).unwrap();
        assert_eq!(v["type"], "assistant");
        assert_eq!(v["text"], "Checking.");
        assert_eq!(v["stop_reason"], "tool_use");
        assert_eq!(v["tool_calls"][0]["name"], "amp_whoami");
        assert_eq!(v["usage"]["input_tokens"], 0);
        assert!(v.get("error").is_none());

        let end = RunEvent::from_agent(&AgentEvent::ToolExecutionEnd {
            tool_call_id: "c1".into(),
            tool_name: "amp_whoami".into(),
            result: AgentToolResult::text("{\"user_id\":7}"),
            is_error: false,
        })
        .unwrap();
        assert_eq!(
            serde_json::to_value(&end).unwrap(),
            json!({"type": "tool_end", "id": "c1", "name": "amp_whoami", "is_error": false,
                   "text": "{\"user_id\":7}"})
        );
        assert_eq!(
            serde_json::to_value(RunEvent::TurnStart).unwrap(),
            json!({"type": "turn_start"})
        );
        assert!(RunEvent::from_agent(&AgentEvent::AgentStart).is_none());
    }

    #[test]
    fn totals_count_turns_tools_and_usage() {
        let mut t = Totals::default();
        let usage = UsageJson {
            input_tokens: 10,
            output_tokens: 5,
            cache_read_tokens: 1,
            cache_write_tokens: 0,
            cost_usd: 0.5,
        };
        for e in [
            RunEvent::TurnStart,
            RunEvent::Assistant {
                text: "a".into(),
                tool_calls: vec![],
                stop_reason: "tool_use".into(),
                error: None,
                model: "m".into(),
                provider: "p".into(),
                usage,
            },
            RunEvent::ToolStart {
                id: "1".into(),
                name: "amp_whoami".into(),
                arguments: json!({}),
            },
            RunEvent::ToolEnd {
                id: "1".into(),
                name: "amp_whoami".into(),
                is_error: true,
                text: "x".into(),
            },
            RunEvent::TurnStart,
            RunEvent::Assistant {
                text: "done".into(),
                tool_calls: vec![],
                stop_reason: "stop".into(),
                error: None,
                model: "m".into(),
                provider: "p".into(),
                usage,
            },
        ] {
            t.observe(&e);
        }
        assert_eq!(t.turns, 2);
        assert_eq!(t.tool_calls, 1);
        assert_eq!(t.tool_errors, 1);
        assert_eq!(t.tools_used, ["amp_whoami"]);
        assert_eq!(t.usage.input_tokens, 20);
        assert_eq!(t.usage.cost_usd, 1.0);
        assert_eq!(t.last_text, "done");
    }
}
