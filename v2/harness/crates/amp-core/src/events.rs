//! Run events: what a running agent did, in a stable JSON shape (`amp-harness run --json`), plus
//! the emitter that gets rpi's events out of the agent loop.
//!
//! - [`ChannelEmitter`] is the `AgentHarnessOptions::agent_emitter`: it forwards every rpi
//!   [`AgentEvent`] into an unbounded channel, so the consumer (the headless printer, the M2-7 TUI,
//!   the M2-5 worker supervisor) never slows the loop down.
//! - [`FanOutEmitter`] (#2370) tees every event to several consumers: the TUI's channel, the
//!   manager's [`crate::manager_feed::ManagerFeed`], and any subscriber added later (the remote
//!   link, #2373). One slow or closed consumer never holds the others back.
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

/// Tees every rpi event to several consumers (#2370). Cheap to clone: clones share the sinks and
/// subscribers.
///
/// - [`FanOutEmitter::add_sink`]: another [`AgentEmitter`] (the TUI's [`ChannelEmitter`], the
///   manager feed's emitter). Sinks get every event in order; `emit` awaits them one after another.
/// - [`FanOutEmitter::subscribe`]: an unbounded channel of every event from now on, for a consumer
///   that joins later. A dropped receiver is forgotten on the next event.
#[derive(Clone, Default)]
pub struct FanOutEmitter {
    inner: Arc<FanOutInner>,
}

#[derive(Default)]
struct FanOutInner {
    sinks: Mutex<Vec<Arc<dyn AgentEmitter>>>,
    subscribers: Mutex<Vec<mpsc::UnboundedSender<AgentEvent>>>,
}

impl FanOutEmitter {
    pub fn new() -> Self {
        Self::default()
    }

    /// Adds a sink; it gets every event emitted from now on.
    pub fn add_sink(&self, sink: Arc<dyn AgentEmitter>) {
        self.inner
            .sinks
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(sink);
    }

    /// A channel of every event emitted from now on.
    pub fn subscribe(&self) -> mpsc::UnboundedReceiver<AgentEvent> {
        let (tx, rx) = mpsc::unbounded_channel();
        self.inner
            .subscribers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(tx);
        rx
    }

    /// How many sinks and live subscribers it feeds.
    pub fn consumers(&self) -> usize {
        let sinks = self
            .inner
            .sinks
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .len();
        let subs = self
            .inner
            .subscribers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .filter(|s| !s.is_closed())
            .count();
        sinks + subs
    }

    pub fn into_emitter(self) -> Arc<dyn AgentEmitter> {
        Arc::new(self)
    }

    fn sinks(&self) -> Vec<Arc<dyn AgentEmitter>> {
        self.inner
            .sinks
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    fn send_subscribers(&self, event: &AgentEvent) {
        let mut subs = self
            .inner
            .subscribers
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        subs.retain(|tx| tx.send(event.clone()).is_ok());
    }
}

impl AgentEmitter for FanOutEmitter {
    fn emit(&self, event: AgentEvent) -> futures_core_box::BoxFuture {
        self.send_subscribers(&event);
        // Each sink's emit is called now, in order (so a sink that records synchronously sees the
        // event before the loop goes on); their futures are awaited one after another.
        let futures: Vec<futures_core_box::BoxFuture> = self
            .sinks()
            .iter()
            .map(|sink| sink.emit(event.clone()))
            .collect();
        Box::pin(async move {
            for f in futures {
                f.await;
            }
        })
    }

    fn try_emit(&self, event: AgentEvent) {
        self.send_subscribers(&event);
        for sink in self.sinks() {
            sink.try_emit(event.clone());
        }
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

    /// A fixed script of rpi events: a turn with streamed text, a tool call, a retry and a
    /// finished assistant message with usage.
    fn script() -> Vec<AgentEvent> {
        let mut m = faux_assistant_message(
            vec![
                FauxBlock::thinking("hmm"),
                FauxBlock::text("Checking."),
                FauxBlock::tool_call("bash", json!({"command": "ls", "timeout": 1.5})),
            ],
            StopReason::ToolUse,
        );
        m.model = "m".into();
        m.provider = "p".into();
        m.usage.input = 12;
        m.usage.output = 3;
        m.usage.cache_read = 100;
        m.usage.cost.total = 0.04125;
        let m = Arc::new(m);
        vec![
            AgentEvent::AgentStart,
            AgentEvent::TurnStart,
            AgentEvent::MessageStart {
                message: AgentMessage::Assistant(Box::new((*m).clone())),
            },
            AgentEvent::MessageUpdate {
                message: Arc::clone(&m),
                assistant_message_event: AssistantMessageEvent::ThinkingDelta {
                    content_index: 0,
                    delta: "hmm".into(),
                    partial: Arc::clone(&m),
                },
            },
            AgentEvent::MessageUpdate {
                message: Arc::clone(&m),
                assistant_message_event: AssistantMessageEvent::TextDelta {
                    content_index: 1,
                    delta: "Check\u{2026}\n\"ing\"".into(),
                    partial: Arc::clone(&m),
                },
            },
            AgentEvent::MessageEnd {
                message: AgentMessage::Assistant(Box::new((*m).clone())),
            },
            AgentEvent::ToolExecutionStart {
                tool_call_id: "c1".into(),
                tool_name: "bash".into(),
                args: json!({"command": "ls", "timeout": 1.5}),
            },
            AgentEvent::ToolExecutionEnd {
                tool_call_id: "c1".into(),
                tool_name: "bash".into(),
                result: AgentToolResult::text("a\nb"),
                is_error: false,
            },
            AgentEvent::RetryScheduled {
                attempt: 1,
                max_retries: 3,
                delay_ms: 250,
                error: "overloaded".into(),
            },
            AgentEvent::TurnEnd {
                message: AgentMessage::Assistant(Box::new((*m).clone())),
                tool_results: vec![],
            },
        ]
    }

    /// `run --json`'s event lines, byte for byte (#2370: the fan-out emitter and ChatEvent must
    /// not change them). A change here breaks scripts: fields may be added, never renamed.
    const RUN_JSON: &str = r#"{"type":"turn_start"}
{"type":"text_delta","delta":"Check…\n\"ing\""}
{"type":"assistant","text":"Checking.","tool_calls":[{"id":"call_2","name":"bash","arguments":{"command":"ls","timeout":1.5}}],"stop_reason":"tool_use","model":"m","provider":"p","usage":{"input_tokens":12,"output_tokens":3,"cache_read_tokens":100,"cache_write_tokens":0,"cost_usd":0.04125}}
{"type":"tool_start","id":"c1","name":"bash","arguments":{"command":"ls","timeout":1.5}}
{"type":"tool_end","id":"c1","name":"bash","is_error":false,"text":"a\nb"}
{"type":"retry","attempt":1,"max_retries":3,"delay_ms":250,"error":"overloaded"}
{"type":"turn_end"}"#;

    fn run_json(events: &[AgentEvent]) -> String {
        events
            .iter()
            .filter_map(RunEvent::from_agent)
            .map(|e| serde_json::to_string(&e).unwrap())
            .collect::<Vec<_>>()
            .join("\n")
    }

    #[test]
    fn run_json_lines_are_byte_identical() {
        let got = run_json(&script());
        // The faux tool call id is generated: pin it before comparing.
        let id = got
            .split("\"tool_calls\":[{\"id\":\"")
            .nth(1)
            .and_then(|s| s.split('"').next())
            .unwrap()
            .to_owned();
        assert_eq!(got.replace(&id, "call_2"), RUN_JSON);
    }

    #[tokio::test]
    async fn fan_out_gives_every_consumer_every_event_in_order() {
        let fan = FanOutEmitter::new();
        let (tui, mut tui_rx) = ChannelEmitter::new();
        fan.add_sink(tui.into_emitter());
        let mut late = fan.subscribe();
        let dropped = fan.subscribe();
        drop(dropped);
        let totals = Arc::new(Mutex::new(Totals::default()));
        let emitter = TotalsEmitter::new(Arc::clone(&totals), Some(fan.clone().into_emitter()))
            .into_emitter();
        let events = script();
        for (i, e) in events.iter().enumerate() {
            if i % 2 == 0 {
                emitter.emit(e.clone()).await;
            } else {
                emitter.try_emit(e.clone());
            }
        }
        let drain = |rx: &mut mpsc::UnboundedReceiver<AgentEvent>| {
            let mut out = Vec::new();
            while let Ok(e) = rx.try_recv() {
                out.push(e);
            }
            out
        };
        let a = drain(&mut tui_rx);
        let b = drain(&mut late);
        assert_eq!(a.len(), events.len());
        assert_eq!(b.len(), events.len());
        // What the TUI and `run --json` see through the fan-out is what they saw before.
        assert_eq!(run_json(&a), run_json(&events));
        assert_eq!(run_json(&b), run_json(&events));
        assert_eq!(totals.lock().unwrap().turns, 1);
        assert_eq!(fan.consumers(), 2, "the dropped subscriber is forgotten");
    }
}
