//! The chat as a stream of [`ChatEvent`]s (#2370): everything the manager's chat shows, in the
//! wire shape of the remote control protocol (`v2/docs/remote-control/protocol.md` section 6.4,
//! vectors in `v2/testdata/remote-control/events.json`).
//!
//! It is a superset of [`crate::events::RunEvent`], which stays `run --json`'s contract unchanged:
//! - from rpi's [`AgentEvent`]s ([`ChatMapper`]): `message_start`, `text_delta`,
//!   `thinking_delta`, `tool_start`, `tool_end`, `message_end` (usage with integer
//!   `cost_usd_micros`), `retry`;
//! - from the chat itself (the TUI's `Shell` pushes them): `user_message` (with its origin, local
//!   or a remote device), `run_start`, `run_end` (the run's outcome), `notice` (every notice,
//!   warning and error line the transcript shows), `queue` and `worker_status` (the worker chips).
//!
//! Wire rules (protocol section 3.3 and 6.4): every member is always present and `X|null` members
//! are serialized as `null`, never skipped; there are no floats (cost travels as integer
//! micro-USD, tool arguments as their JSON **text** in `arguments_json`); long strings are cut on
//! a UTF-8 boundary ([`ARGUMENTS_MAX`], [`TOOL_TEXT_MAX`], [`DELTA_MAX`], [`USER_TEXT_MAX`]).
//! Redacting secrets is the remote link's job (#2373), before it signs an event.

use std::time::{SystemTime, UNIX_EPOCH};

use rpi_agent::{AgentEvent, AgentMessage};
use rpi_ai::types::{AssistantMessage, AssistantMessageEvent, Content, UserContent};
use serde::{Deserialize, Serialize};

use crate::events::{assistant_text, stop_reason_str, tool_result_text};
use crate::supervisor::{WorkerOutcome, WorkerPhase, WorkerSessionInfo};

/// `tool_start.arguments_json` is cut at 16 KiB.
pub const ARGUMENTS_MAX: usize = 16 * 1024;
/// `tool_end.text` is cut at 32 KiB.
pub const TOOL_TEXT_MAX: usize = 32 * 1024;
/// A `text_delta` or `thinking_delta` carries at most 16 KiB.
pub const DELTA_MAX: usize = 16 * 1024;
/// `user_message.text` is at most 49 152 bytes.
pub const USER_TEXT_MAX: usize = 49_152;
/// `notice.text` is cut at 32 KiB (a panel's text, say), so an event stays far below 1 MiB.
pub const NOTICE_MAX: usize = 32 * 1024;
/// `queue.items[].preview` is the first 80 characters.
pub const PREVIEW_CHARS: usize = 80;

/// One thing the chat shows, tagged by `type` (protocol section 6.4).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ChatEvent {
    /// A prompt the person sent, from the laptop or a paired device. `text` carries no
    /// `[remote: …]` tag: viewers render the origin from the fields.
    UserMessage {
        prompt_id: String,
        text: String,
        origin: Origin,
        device_id: Option<String>,
        device_name: Option<String>,
    },
    /// The agent started working on a prompt.
    RunStart { run_id: String, prompt_id: String },
    /// An assistant message started streaming.
    MessageStart {
        message_id: String,
        model: String,
        provider: String,
    },
    /// Streamed answer text (coalesced by the feed).
    TextDelta { message_id: String, delta: String },
    /// Streamed thinking (coalesced by the feed; viewers fold it).
    ThinkingDelta { message_id: String, delta: String },
    /// A tool call started.
    ToolStart {
        message_id: String,
        tool_call_id: String,
        name: String,
        /// The arguments' compact JSON text, cut at [`ARGUMENTS_MAX`].
        arguments_json: String,
        truncated: bool,
    },
    /// A tool call finished.
    ToolEnd {
        tool_call_id: String,
        name: String,
        is_error: bool,
        /// The result's text, cut at [`TOOL_TEXT_MAX`].
        text: String,
        truncated: bool,
        /// The result text's size before the cut.
        full_bytes: u64,
    },
    /// An assistant message finished.
    MessageEnd {
        message_id: String,
        stop_reason: String,
        error: Option<String>,
        model: String,
        provider: String,
        usage: ChatUsage,
    },
    /// The provider failed and the request will be retried.
    Retry {
        attempt: u32,
        max_retries: u32,
        delay_ms: u64,
        error: String,
    },
    /// The run ended.
    RunEnd {
        run_id: String,
        outcome: RunOutcome,
        error: Option<String>,
    },
    /// A notice, warning or error line of the transcript.
    Notice {
        level: NoticeLevel,
        /// A machine-readable kind (`stopped`, `approval_refused`, …) or null. Missing on input
        /// reads as null (the #2369 vectors leave it out of their notice bodies).
        code: Option<String>,
        text: String,
    },
    /// The follow-up queue changed (prompts sent while the agent was busy).
    Queue { items: Vec<QueueItem> },
    /// The worker roster changed.
    WorkerStatus { workers: Vec<WorkerChip> },
}

/// Where a prompt came from.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Origin {
    Local,
    Remote,
}

/// Who sent a prompt: the person at the laptop, or a paired device (the remote link fills it).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum PromptOrigin {
    #[default]
    Local,
    Remote {
        device_id: String,
        device_name: String,
    },
}

impl PromptOrigin {
    pub fn origin(&self) -> Origin {
        match self {
            Self::Local => Origin::Local,
            Self::Remote { .. } => Origin::Remote,
        }
    }

    pub fn device_id(&self) -> Option<String> {
        match self {
            Self::Local => None,
            Self::Remote { device_id, .. } => Some(device_id.clone()),
        }
    }

    pub fn device_name(&self) -> Option<String> {
        match self {
            Self::Local => None,
            Self::Remote { device_name, .. } => Some(device_name.clone()),
        }
    }
}

/// How a run ended.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RunOutcome {
    Completed,
    Aborted,
    Failed,
    Suspended,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum NoticeLevel {
    Info,
    Warn,
    Error,
}

/// An assistant message's usage, integers only.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ChatUsage {
    pub input_tokens: i64,
    pub output_tokens: i64,
    pub cache_read_tokens: i64,
    pub cache_write_tokens: i64,
    /// `round(cost_usd × 1e6)`.
    pub cost_usd_micros: i64,
}

impl From<&rpi_ai::types::Usage> for ChatUsage {
    fn from(u: &rpi_ai::types::Usage) -> Self {
        Self {
            input_tokens: u.input,
            output_tokens: u.output,
            cache_read_tokens: u.cache_read,
            cache_write_tokens: u.cache_write,
            cost_usd_micros: usd_micros(u.cost.total),
        }
    }
}

/// Dollars as integer micro-dollars.
pub fn usd_micros(usd: f64) -> i64 {
    (usd * 1e6).round() as i64
}

/// A prompt waiting in the follow-up queue.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct QueueItem {
    pub prompt_id: String,
    pub origin: Origin,
    pub device_id: Option<String>,
    /// The first [`PREVIEW_CHARS`] characters.
    pub preview: String,
}

impl QueueItem {
    pub fn new(prompt_id: impl Into<String>, text: &str, origin: &PromptOrigin) -> Self {
        Self {
            prompt_id: prompt_id.into(),
            origin: origin.origin(),
            device_id: origin.device_id(),
            preview: text.chars().take(PREVIEW_CHARS).collect(),
        }
    }
}

/// One worker chip.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct WorkerChip {
    pub task_id: i64,
    /// The ticket's name (`#<id>` when it isn't known).
    pub name: String,
    /// The role id: `amp-worker-backend`.
    pub role: String,
    pub state: WorkerState,
    pub model: Option<String>,
    pub started_at_ms: Option<i64>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerState {
    Running,
    Done,
    Failed,
    Stopped,
    Blocked,
}

impl ChatEvent {
    /// A prompt's `user_message`, its text cut at [`USER_TEXT_MAX`].
    pub fn user_message(prompt_id: impl Into<String>, text: &str, origin: &PromptOrigin) -> Self {
        Self::UserMessage {
            prompt_id: prompt_id.into(),
            text: cut(text, USER_TEXT_MAX).0.to_owned(),
            origin: origin.origin(),
            device_id: origin.device_id(),
            device_name: origin.device_name(),
        }
    }

    /// A notice line, cut at [`NOTICE_MAX`].
    pub fn notice(level: NoticeLevel, code: Option<&str>, text: &str) -> Self {
        Self::Notice {
            level,
            code: code.map(str::to_owned),
            text: cut(text, NOTICE_MAX).0.to_owned(),
        }
    }

    /// The `worker_status` snapshot of the supervisor's sessions; `name` gives a ticket's name.
    pub fn worker_status(
        sessions: &[WorkerSessionInfo],
        name: impl Fn(i64) -> Option<String>,
    ) -> Self {
        let mut workers: Vec<WorkerChip> = sessions
            .iter()
            .map(|s| WorkerChip {
                task_id: s.task_id,
                name: name(s.task_id).unwrap_or_else(|| format!("#{}", s.task_id)),
                role: s
                    .agent_id
                    .strip_prefix("amp:")
                    .unwrap_or(&s.agent_id)
                    .to_owned(),
                state: worker_state(s),
                model: s.model.clone(),
                started_at_ms: ms_since_epoch(s.since),
            })
            .collect();
        workers.sort_by_key(|w| w.task_id);
        Self::WorkerStatus { workers }
    }

    /// The JSON type tag.
    pub fn type_tag(&self) -> &'static str {
        match self {
            Self::UserMessage { .. } => "user_message",
            Self::RunStart { .. } => "run_start",
            Self::MessageStart { .. } => "message_start",
            Self::TextDelta { .. } => "text_delta",
            Self::ThinkingDelta { .. } => "thinking_delta",
            Self::ToolStart { .. } => "tool_start",
            Self::ToolEnd { .. } => "tool_end",
            Self::MessageEnd { .. } => "message_end",
            Self::Retry { .. } => "retry",
            Self::RunEnd { .. } => "run_end",
            Self::Notice { .. } => "notice",
            Self::Queue { .. } => "queue",
            Self::WorkerStatus { .. } => "worker_status",
        }
    }
}

fn worker_state(s: &WorkerSessionInfo) -> WorkerState {
    match (s.phase, s.outcome) {
        (Some(WorkerPhase::Queued | WorkerPhase::Running), _) => WorkerState::Running,
        (None, Some(WorkerOutcome::Completed)) => WorkerState::Done,
        (None, Some(WorkerOutcome::Blocked)) => WorkerState::Blocked,
        (None, Some(WorkerOutcome::Stopped | WorkerOutcome::Cancelled)) => WorkerState::Stopped,
        (None, Some(_)) => WorkerState::Failed,
        (None, None) => WorkerState::Stopped,
    }
}

fn ms_since_epoch(t: SystemTime) -> Option<i64> {
    i64::try_from(t.duration_since(UNIX_EPOCH).ok()?.as_millis()).ok()
}

/// `s` cut to at most `max` bytes on a char boundary, and whether it was cut.
pub fn cut(s: &str, max: usize) -> (&str, bool) {
    if s.len() <= max {
        return (s, false);
    }
    let mut end = max;
    while !s.is_char_boundary(end) {
        end -= 1;
    }
    (&s[..end], true)
}

/// `s` in pieces of at most `max` bytes, split on char boundaries (empty: no pieces).
pub fn chunks(s: &str, max: usize) -> Vec<&str> {
    let mut out = Vec::new();
    let mut rest = s;
    while !rest.is_empty() {
        let (head, _) = cut(rest, max);
        // A char wider than `max` (never with the caps above) still makes progress.
        let head = if head.is_empty() {
            &rest[..rest.chars().next().map_or(rest.len(), char::len_utf8)]
        } else {
            head
        };
        out.push(head);
        rest = &rest[head.len()..];
    }
    out
}

/// Turns rpi's [`AgentEvent`]s into [`ChatEvent`]s, numbering assistant messages `msg_1`,
/// `msg_2`, ….
///
/// - [`ChatMapper::live`]: the live stream. User messages are left out: the chat pushes its own
///   `user_message` with the prompt's origin.
/// - [`ChatMapper::replay`]: a session file's events ([`crate::worker_session::replay_text`]).
///   User messages become local `user_message`s, and an assistant message that streamed no
///   deltas gets its thinking and text as deltas before its `message_end`.
#[derive(Debug, Default)]
pub struct ChatMapper {
    replay: bool,
    next_message: u64,
    next_prompt: u64,
    /// The assistant message being streamed.
    current: Option<String>,
    /// The latest assistant message (tool calls belong to it).
    last: Option<String>,
    /// Whether the current message streamed any text or thinking.
    streamed: bool,
}

impl ChatMapper {
    pub fn live() -> Self {
        Self::default()
    }

    pub fn replay() -> Self {
        Self {
            replay: true,
            ..Self::default()
        }
    }

    fn new_message(&mut self) -> String {
        self.next_message += 1;
        let id = format!("msg_{}", self.next_message);
        self.current = Some(id.clone());
        self.last = Some(id.clone());
        self.streamed = false;
        id
    }

    /// The current message's id, starting one (with its `message_start`) if none is open.
    fn current_or_start(&mut self, m: &AssistantMessage, out: &mut Vec<ChatEvent>) -> String {
        if let Some(id) = &self.current {
            return id.clone();
        }
        let id = self.new_message();
        out.push(ChatEvent::MessageStart {
            message_id: id.clone(),
            model: m.model.clone(),
            provider: m.provider.clone(),
        });
        id
    }

    /// The chat events for one rpi event (often none).
    pub fn map(&mut self, event: &AgentEvent) -> Vec<ChatEvent> {
        let mut out = Vec::new();
        match event {
            AgentEvent::MessageStart {
                message: AgentMessage::Assistant(m),
            } => {
                let id = self.new_message();
                out.push(ChatEvent::MessageStart {
                    message_id: id,
                    model: m.model.clone(),
                    provider: m.provider.clone(),
                });
            }
            AgentEvent::MessageStart {
                message: AgentMessage::User(u),
            } if self.replay => {
                self.next_prompt += 1;
                let text = match &u.content {
                    UserContent::Text(t) => t.clone(),
                    UserContent::Blocks(blocks) => blocks
                        .iter()
                        .filter_map(|c| match c {
                            Content::Text(t) => Some(t.text.as_str()),
                            _ => None,
                        })
                        .collect::<Vec<_>>()
                        .join("\n"),
                };
                out.push(ChatEvent::user_message(
                    format!("p_{}", self.next_prompt),
                    &text,
                    &PromptOrigin::Local,
                ));
            }
            AgentEvent::MessageUpdate {
                message,
                assistant_message_event,
            } => {
                let (delta, thinking) = match assistant_message_event {
                    AssistantMessageEvent::TextDelta { delta, .. } => (delta, false),
                    AssistantMessageEvent::ThinkingDelta { delta, .. } => (delta, true),
                    _ => return out,
                };
                if delta.is_empty() {
                    return out;
                }
                let id = self.current_or_start(message, &mut out);
                self.streamed = true;
                push_deltas(&mut out, &id, delta, thinking);
            }
            AgentEvent::MessageEnd {
                message: AgentMessage::Assistant(m),
            } => {
                let id = self.current_or_start(m, &mut out);
                if !self.streamed {
                    // Nothing streamed (a replayed session, a provider that doesn't stream).
                    let thinking: String = m
                        .content
                        .iter()
                        .filter_map(|c| match c {
                            Content::Thinking(t) => Some(t.thinking.as_str()),
                            _ => None,
                        })
                        .collect::<Vec<_>>()
                        .join("\n");
                    push_deltas(&mut out, &id, &thinking, true);
                    push_deltas(&mut out, &id, &assistant_text(m), false);
                }
                self.current = None;
                out.push(ChatEvent::MessageEnd {
                    message_id: id,
                    stop_reason: stop_reason_str(m.stop_reason).to_owned(),
                    error: m.error_message.clone(),
                    model: m.model.clone(),
                    provider: m.provider.clone(),
                    usage: ChatUsage::from(&m.usage),
                });
            }
            AgentEvent::ToolExecutionStart {
                tool_call_id,
                tool_name,
                args,
            } => {
                let json = serde_json::to_string(args).unwrap_or_else(|_| "null".to_owned());
                let (text, truncated) = cut(&json, ARGUMENTS_MAX);
                out.push(ChatEvent::ToolStart {
                    message_id: self.last.clone().unwrap_or_else(|| "msg_0".to_owned()),
                    tool_call_id: tool_call_id.clone(),
                    name: tool_name.clone(),
                    arguments_json: text.to_owned(),
                    truncated,
                });
            }
            AgentEvent::ToolExecutionEnd {
                tool_call_id,
                tool_name,
                result,
                is_error,
            } => {
                let full = tool_result_text(result);
                let (text, truncated) = cut(&full, TOOL_TEXT_MAX);
                out.push(ChatEvent::ToolEnd {
                    tool_call_id: tool_call_id.clone(),
                    name: tool_name.clone(),
                    is_error: *is_error,
                    text: text.to_owned(),
                    truncated,
                    full_bytes: full.len() as u64,
                });
            }
            AgentEvent::RetryScheduled {
                attempt,
                max_retries,
                delay_ms,
                error,
            } => out.push(ChatEvent::Retry {
                attempt: *attempt,
                max_retries: *max_retries,
                delay_ms: *delay_ms,
                error: crate::provider_error::redact(error),
            }),
            _ => {}
        }
        out
    }
}

fn push_deltas(out: &mut Vec<ChatEvent>, id: &str, text: &str, thinking: bool) {
    for piece in chunks(text, DELTA_MAX) {
        out.push(if thinking {
            ChatEvent::ThinkingDelta {
                message_id: id.to_owned(),
                delta: piece.to_owned(),
            }
        } else {
            ChatEvent::TextDelta {
                message_id: id.to_owned(),
                delta: piece.to_owned(),
            }
        });
    }
}

/// The chat events of a session's JSONL file (the resync path: what a viewer that missed too
/// much can be sent instead of the feed). `Err` only when the file can't be read.
pub fn session_chat_events(path: &std::path::Path) -> Result<Vec<ChatEvent>, String> {
    let text =
        std::fs::read_to_string(path).map_err(|e| format!("read {}: {e}", path.display()))?;
    Ok(chat_events_from_session_text(&text))
}

/// [`session_chat_events`] on the file's text.
pub fn chat_events_from_session_text(text: &str) -> Vec<ChatEvent> {
    let mut mapper = ChatMapper::replay();
    crate::worker_session::replay_text(text)
        .iter()
        .flat_map(|(_, e)| mapper.map(e))
        .collect()
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use rpi_agent::AgentToolResult;
    use rpi_ai::{
        providers::faux::{FauxBlock, faux_assistant_message},
        types::StopReason,
    };
    use serde_json::{Value, json};

    use super::*;

    fn vectors_dir() -> std::path::PathBuf {
        std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../../testdata/remote-control")
    }

    /// Every `chat_event` body in the shared vectors parses as a ChatEvent and serializes back to
    /// the same JSON (members in any order; a missing nullable member comes back as null).
    #[test]
    fn shared_vectors_round_trip() {
        let text = std::fs::read_to_string(vectors_dir().join("events.json")).unwrap();
        let file: Value = serde_json::from_str(&text).unwrap();
        let mut seen = std::collections::BTreeSet::new();
        for v in file["vectors"].as_array().unwrap() {
            let wire: Value = serde_json::from_str(v["wire"].as_str().unwrap()).unwrap();
            if wire["op"] != "chat_event" {
                continue;
            }
            let body = &wire["body"];
            let event: ChatEvent = serde_json::from_value(body.clone())
                .unwrap_or_else(|e| panic!("vector {}: {e}", v["name"]));
            let mut back = serde_json::to_value(&event).unwrap();
            if let (Some(b), Some(o)) = (back.as_object_mut(), body.as_object()) {
                // A nullable member the vector left out comes back as null.
                b.retain(|k, val| o.contains_key(k) || !val.is_null());
            }
            assert_eq!(&back, body, "vector {}", v["name"]);
            seen.insert(event.type_tag());
        }
        for t in ["text_delta", "tool_start", "message_end", "notice"] {
            assert!(seen.contains(t), "the vectors have a {t}");
        }
    }

    #[test]
    fn nullable_members_are_null_never_skipped() {
        let v = serde_json::to_value(ChatEvent::user_message("p_1", "hi", &PromptOrigin::Local))
            .unwrap();
        assert_eq!(
            v,
            json!({"type": "user_message", "prompt_id": "p_1", "text": "hi", "origin": "local",
                   "device_id": null, "device_name": null})
        );
        let remote = PromptOrigin::Remote {
            device_id: "dev".into(),
            device_name: "Safari on iPhone".into(),
        };
        let v = serde_json::to_value(ChatEvent::user_message("p_2", "go", &remote)).unwrap();
        assert_eq!(v["origin"], "remote");
        assert_eq!(v["device_name"], "Safari on iPhone");
        assert_eq!(
            serde_json::to_value(ChatEvent::notice(NoticeLevel::Warn, None, "x")).unwrap(),
            json!({"type": "notice", "level": "warn", "code": null, "text": "x"})
        );
        assert_eq!(
            serde_json::to_value(ChatEvent::RunEnd {
                run_id: "run_1".into(),
                outcome: RunOutcome::Aborted,
                error: None,
            })
            .unwrap(),
            json!({"type": "run_end", "run_id": "run_1", "outcome": "aborted", "error": null})
        );
    }

    fn assistant(blocks: Vec<FauxBlock>, cost: f64) -> Arc<AssistantMessage> {
        let mut m = faux_assistant_message(blocks, StopReason::ToolUse);
        m.model = "claude-opus-5-5".into();
        m.provider = "anthropic".into();
        m.usage.input = 12034;
        m.usage.output = 311;
        m.usage.cache_read = 90000;
        m.usage.cost.total = cost;
        Arc::new(m)
    }

    fn has_no_floats(v: &Value) -> bool {
        match v {
            Value::Number(n) => !n.is_f64(),
            Value::Array(a) => a.iter().all(has_no_floats),
            Value::Object(o) => o.values().all(has_no_floats),
            _ => true,
        }
    }

    #[test]
    fn live_events_map_to_the_protocol_shapes() {
        let m = assistant(
            vec![
                FauxBlock::thinking("plan"),
                FauxBlock::text("Running"),
                FauxBlock::tool_call("bash", json!({"command": "cargo test", "timeout": 1.5})),
            ],
            0.04125,
        );
        let mut mapper = ChatMapper::live();
        let mut out = Vec::new();
        for e in [
            AgentEvent::MessageStart {
                message: AgentMessage::User(rpi_ai::types::UserMessage {
                    role: Default::default(),
                    content: UserContent::Text("go".into()),
                    timestamp: 0,
                }),
            },
            AgentEvent::MessageStart {
                message: AgentMessage::Assistant(Box::new((*m).clone())),
            },
            AgentEvent::MessageUpdate {
                message: Arc::clone(&m),
                assistant_message_event: AssistantMessageEvent::ThinkingDelta {
                    content_index: 0,
                    delta: "plan".into(),
                    partial: Arc::clone(&m),
                },
            },
            AgentEvent::MessageUpdate {
                message: Arc::clone(&m),
                assistant_message_event: AssistantMessageEvent::TextDelta {
                    content_index: 1,
                    delta: "Running".into(),
                    partial: Arc::clone(&m),
                },
            },
            AgentEvent::MessageEnd {
                message: AgentMessage::Assistant(Box::new((*m).clone())),
            },
            AgentEvent::ToolExecutionStart {
                tool_call_id: "call_1".into(),
                tool_name: "bash".into(),
                args: json!({"command": "cargo test", "timeout": 1.5}),
            },
            AgentEvent::ToolExecutionEnd {
                tool_call_id: "call_1".into(),
                tool_name: "bash".into(),
                result: AgentToolResult::text("ok"),
                is_error: false,
            },
            AgentEvent::TurnEnd {
                message: AgentMessage::Assistant(Box::new((*m).clone())),
                tool_results: vec![],
            },
        ] {
            out.extend(mapper.map(&e));
        }
        let json: Vec<Value> = out
            .iter()
            .map(|e| serde_json::to_value(e).unwrap())
            .collect();
        assert!(json.iter().all(has_no_floats), "{json:?}");
        assert_eq!(
            json,
            [
                json!({"type": "message_start", "message_id": "msg_1", "model": "claude-opus-5-5",
                       "provider": "anthropic"}),
                json!({"type": "thinking_delta", "message_id": "msg_1", "delta": "plan"}),
                json!({"type": "text_delta", "message_id": "msg_1", "delta": "Running"}),
                json!({"type": "message_end", "message_id": "msg_1", "stop_reason": "tool_use",
                       "error": null, "model": "claude-opus-5-5", "provider": "anthropic",
                       "usage": {"input_tokens": 12034, "output_tokens": 311,
                                 "cache_read_tokens": 90000, "cache_write_tokens": 0,
                                 "cost_usd_micros": 41250}}),
                json!({"type": "tool_start", "message_id": "msg_1", "tool_call_id": "call_1",
                       "name": "bash",
                       "arguments_json": "{\"command\":\"cargo test\",\"timeout\":1.5}",
                       "truncated": false}),
                json!({"type": "tool_end", "tool_call_id": "call_1", "name": "bash",
                       "is_error": false, "text": "ok", "truncated": false, "full_bytes": 2}),
            ]
        );
    }

    #[test]
    fn long_strings_are_cut_on_a_char_boundary() {
        let (s, cut_) = cut("aé", 2);
        assert_eq!((s, cut_), ("a", true));
        assert_eq!(cut("abc", 3), ("abc", false));
        let text = "é".repeat(DELTA_MAX); // 2 bytes each
        let pieces = chunks(&text, DELTA_MAX);
        assert_eq!(pieces.len(), 2);
        assert!(pieces.iter().all(|p| p.len() <= DELTA_MAX));
        assert_eq!(pieces.concat(), text);

        let big = "x".repeat(TOOL_TEXT_MAX + 10);
        let mut mapper = ChatMapper::live();
        let out = mapper.map(&AgentEvent::ToolExecutionEnd {
            tool_call_id: "c".into(),
            tool_name: "read".into(),
            result: AgentToolResult::text(big),
            is_error: false,
        });
        match &out[0] {
            ChatEvent::ToolEnd {
                text,
                truncated,
                full_bytes,
                ..
            } => {
                assert_eq!(text.len(), TOOL_TEXT_MAX);
                assert!(truncated);
                assert_eq!(*full_bytes, (TOOL_TEXT_MAX + 10) as u64);
            }
            other => panic!("{other:?}"),
        }
        let args = json!({"content": "y".repeat(ARGUMENTS_MAX)});
        match &mapper.map(&AgentEvent::ToolExecutionStart {
            tool_call_id: "c".into(),
            tool_name: "write".into(),
            args,
        })[0]
        {
            ChatEvent::ToolStart {
                arguments_json,
                truncated,
                message_id,
                ..
            } => {
                assert_eq!(arguments_json.len(), ARGUMENTS_MAX);
                assert!(truncated);
                assert_eq!(message_id, "msg_0", "no assistant message yet");
            }
            other => panic!("{other:?}"),
        }
        match ChatEvent::user_message("p", &"z".repeat(USER_TEXT_MAX + 1), &PromptOrigin::Local) {
            ChatEvent::UserMessage { text, .. } => assert_eq!(text.len(), USER_TEXT_MAX),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn a_session_file_resyncs_as_chat_events() {
        let text = [
            r#"{"kind":"entry","type":"message","timestamp":900,"message":{"kind":"user","role":"user","content":"go","timestamp":900}}"#,
            r#"{"kind":"entry","type":"message","timestamp":2500,"message":{"kind":"assistant","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"on it"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}],"api":"openai-completions","provider":"faux","model":"m","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0.0,"output":0.0,"cacheRead":0.0,"cacheWrite":0.0,"total":0.0000015}},"stopReason":"tooluse","timestamp":1000}}"#,
            r#"{"kind":"entry","type":"message","timestamp":20500,"message":{"kind":"tool_result","role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"ok"}],"details":null,"isError":false,"timestamp":20500}}"#,
        ]
        .join("\n");
        let tags: Vec<&str> = chat_events_from_session_text(&text)
            .iter()
            .map(ChatEvent::type_tag)
            .collect();
        assert_eq!(
            tags,
            [
                "user_message",
                "message_start",
                "thinking_delta",
                "text_delta",
                "message_end",
                "tool_start",
                "tool_end"
            ]
        );
        let events = chat_events_from_session_text(&text);
        assert_eq!(
            events[0],
            ChatEvent::user_message("p_1", "go", &PromptOrigin::Local)
        );
        match &events[4] {
            ChatEvent::MessageEnd { usage, .. } => assert_eq!(usage.cost_usd_micros, 2),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn worker_chips_follow_the_supervisor() {
        let base = WorkerSessionInfo {
            task_id: 7,
            agent_id: "amp:amp-worker-backend".into(),
            phase: Some(WorkerPhase::Running),
            model: Some("anthropic/claude".into()),
            cwd: None,
            since: UNIX_EPOCH + std::time::Duration::from_millis(1_792_945_200_000),
            ended: None,
            totals: Default::default(),
            outcome: None,
            error: None,
            stopped_by: None,
            session_file: None,
            branch: None,
            context_window: None,
        };
        let done = WorkerSessionInfo {
            task_id: 3,
            phase: None,
            outcome: Some(WorkerOutcome::Timeout),
            ..base.clone()
        };
        let e = ChatEvent::worker_status(&[base, done], |id| (id == 7).then(|| "Build it".into()));
        assert_eq!(
            serde_json::to_value(&e).unwrap(),
            json!({"type": "worker_status", "workers": [
                {"task_id": 3, "name": "#3", "role": "amp-worker-backend", "state": "failed",
                 "model": "anthropic/claude", "started_at_ms": 1_792_945_200_000_i64},
                {"task_id": 7, "name": "Build it", "role": "amp-worker-backend", "state": "running",
                 "model": "anthropic/claude", "started_at_ms": 1_792_945_200_000_i64},
            ]})
        );
    }
}
