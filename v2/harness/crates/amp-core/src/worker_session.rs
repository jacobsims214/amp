//! A worker's session as the chat shows it (#2252): the live event feed the supervisor keeps for
//! every worker it runs, and a replay of a finished worker's JSONL session file.
//!
//! - [`WorkerFeed`] records each [`AgentEvent`] the worker emits, so a view opened at any time
//!   can catch up from the start and then follow it. Streaming deltas (`MessageUpdate`,
//!   `ToolExecutionUpdate`) carry a snapshot of the whole partial message; only the latest of a
//!   run of them is kept, so memory stays linear in the output. Each entry has a sequence number
//!   and a replaced entry gets a new one, so [`WorkerFeed::since`] never misses an update.
//! - [`replay`] turns a session JSONL (rpi-harness v4: `{"kind":"entry","type":"message",
//!   "message":{...}}` lines) back into the events a live view would have seen: the prompt, each
//!   assistant message, and each tool call with its result. Streaming frames and records are
//!   skipped; a line that doesn't parse is skipped too (a torn tail).
//! - [`latest_session`] finds a ticket's newest session file under `<state>/sessions/<id>/`.

use std::{
    collections::HashMap,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

use rpi_agent::{AgentEvent, AgentMessage, AgentToolResult};
use rpi_ai::types::{Content, ToolCall};
use serde_json::Value;

/// What a tool call that never got a result shows in a replayed session.
pub const NO_RESULT: &str = "no result: the run ended before this call finished";

/// One worker's events, as emitted. Cheap to share (`Arc`).
#[derive(Debug, Default)]
pub struct WorkerFeed {
    inner: Mutex<FeedInner>,
}

#[derive(Debug, Default)]
struct FeedInner {
    entries: Vec<(u64, AgentEvent)>,
    next: u64,
}

impl WorkerFeed {
    pub fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    /// A feed holding `events` (a replayed session).
    pub fn from_events(events: Vec<AgentEvent>) -> Arc<Self> {
        let feed = Self::new();
        for e in &events {
            feed.push(e);
        }
        feed
    }

    /// Records one event. A streaming delta replaces the delta right before it of the same kind
    /// (the same message, or the same tool call), with a new sequence number.
    pub fn push(&self, event: &AgentEvent) {
        let mut inner = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        inner.next += 1;
        let seq = inner.next;
        let replace = match (inner.entries.last().map(|(_, e)| e), event) {
            (Some(AgentEvent::MessageUpdate { .. }), AgentEvent::MessageUpdate { .. }) => true,
            (
                Some(AgentEvent::ToolExecutionUpdate {
                    tool_call_id: a, ..
                }),
                AgentEvent::ToolExecutionUpdate {
                    tool_call_id: b, ..
                },
            ) => a == b,
            _ => false,
        };
        if replace {
            inner.entries.pop();
        }
        inner.entries.push((seq, event.clone()));
    }

    /// The events after sequence number `seen` (0 = from the start), and the sequence number to
    /// pass next time. An update that replaced one already seen comes again (applying it twice
    /// is harmless: it carries the whole partial message).
    pub fn since(&self, seen: u64) -> (Vec<AgentEvent>, u64) {
        let inner = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        let start = inner
            .entries
            .iter()
            .rposition(|(seq, _)| *seq <= seen)
            .map_or(0, |i| i + 1);
        let events = inner.entries[start..]
            .iter()
            .map(|(_, e)| e.clone())
            .collect();
        (events, inner.next)
    }

    /// How many entries the feed holds (tests).
    pub fn len(&self) -> usize {
        self.inner
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .entries
            .len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

/// The directory a ticket's worker sessions are written to.
pub fn sessions_dir(state_dir: &Path, task_id: i64) -> PathBuf {
    state_dir.join("sessions").join(task_id.to_string())
}

/// The newest `*.jsonl` session of ticket `task_id` under `state_dir`, if any. Session files are
/// named `<created ms>-<id>.jsonl`, so the newest sorts last; the modification time breaks ties.
pub fn latest_session(state_dir: &Path, task_id: i64) -> Option<PathBuf> {
    let dir = sessions_dir(state_dir, task_id);
    let mut files: Vec<(String, PathBuf)> = std::fs::read_dir(&dir)
        .ok()?
        .filter_map(Result::ok)
        .map(|e| e.path())
        .filter(|p| p.extension().is_some_and(|x| x == "jsonl"))
        .map(|p| {
            let name = p
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            (name, p)
        })
        .collect();
    files.sort();
    files.pop().map(|(_, p)| p)
}

/// The events a live view of the session in `path` would have applied. `Err` only when the file
/// can't be read.
pub fn replay(path: &Path) -> Result<Vec<AgentEvent>, String> {
    let text =
        std::fs::read_to_string(path).map_err(|e| format!("read {}: {e}", path.display()))?;
    Ok(replay_text(&text))
}

/// [`replay`] on the file's text.
pub fn replay_text(text: &str) -> Vec<AgentEvent> {
    let mut events = Vec::new();
    // Tool calls seen in assistant messages that have no result yet, in order.
    let mut open: Vec<(String, String)> = Vec::new();
    let mut names: HashMap<String, String> = HashMap::new();
    for line in text.lines() {
        let Ok(value) = serde_json::from_str::<Value>(line) else {
            continue;
        };
        if value["kind"] != "entry" || value["type"] != "message" {
            continue;
        }
        let Ok(message) = serde_json::from_value::<AgentMessage>(value["message"].clone()) else {
            continue;
        };
        match &message {
            AgentMessage::User(_) | AgentMessage::Custom(_) => {
                events.push(AgentEvent::MessageStart {
                    message: message.clone(),
                });
                events.push(AgentEvent::MessageEnd { message });
            }
            AgentMessage::Assistant(m) => {
                let calls: Vec<ToolCall> = m
                    .content
                    .iter()
                    .filter_map(|c| match c {
                        Content::ToolCall(call) => Some(call.clone()),
                        _ => None,
                    })
                    .collect();
                events.push(AgentEvent::TurnStart);
                events.push(AgentEvent::MessageStart {
                    message: message.clone(),
                });
                events.push(AgentEvent::MessageEnd {
                    message: message.clone(),
                });
                for call in calls {
                    names.insert(call.id.clone(), call.name.clone());
                    open.push((call.id.clone(), call.name.clone()));
                    events.push(AgentEvent::ToolExecutionStart {
                        tool_call_id: call.id,
                        tool_name: call.name,
                        args: call.arguments,
                    });
                }
            }
            AgentMessage::ToolResult(r) => {
                open.retain(|(id, _)| id != &r.tool_call_id);
                let text = r
                    .content
                    .iter()
                    .filter_map(|c| match c {
                        Content::Text(t) => Some(t.text.as_str()),
                        _ => None,
                    })
                    .collect::<Vec<_>>()
                    .join("\n");
                let mut result = AgentToolResult::text(text);
                if let Some(details) = &r.details {
                    result.details = details.clone();
                }
                events.push(AgentEvent::ToolExecutionEnd {
                    tool_call_id: r.tool_call_id.clone(),
                    tool_name: names
                        .get(&r.tool_call_id)
                        .cloned()
                        .unwrap_or_else(|| r.tool_name.clone()),
                    result,
                    is_error: r.is_error,
                });
            }
        }
    }
    // Calls the run never finished: closed, so they don't spin forever in a read-only view.
    for (id, name) in open {
        events.push(AgentEvent::ToolExecutionEnd {
            tool_call_id: id,
            tool_name: name,
            result: AgentToolResult::text(NO_RESULT),
            is_error: true,
        });
    }
    events
}

#[cfg(test)]
mod tests {
    use super::*;
    use rpi_ai::{
        providers::faux::{FauxBlock, faux_assistant_message},
        types::StopReason,
    };

    fn update(text: &str) -> AgentEvent {
        let m = Arc::new(faux_assistant_message(
            vec![FauxBlock::text(text)],
            StopReason::Stop,
        ));
        AgentEvent::MessageUpdate {
            message: Arc::clone(&m),
            assistant_message_event: rpi_ai::types::AssistantMessageEvent::Start { partial: m },
        }
    }

    #[test]
    fn streaming_updates_are_coalesced_and_never_missed() {
        let feed = WorkerFeed::new();
        feed.push(&AgentEvent::TurnStart);
        feed.push(&update("a"));
        let (first, seen) = feed.since(0);
        assert_eq!(first.len(), 2);
        feed.push(&update("ab"));
        feed.push(&update("abc"));
        assert_eq!(feed.len(), 2, "one update kept");
        let (next, seen2) = feed.since(seen);
        assert_eq!(next.len(), 1, "the replaced update comes again");
        assert!(seen2 > seen);
        let (none, _) = feed.since(seen2);
        assert!(none.is_empty());
        feed.push(&AgentEvent::TurnStart);
        assert_eq!(feed.since(seen2).0.len(), 1);
        assert_eq!(feed.since(0).0.len(), 3);
    }

    #[test]
    fn a_session_file_replays_as_prompt_messages_and_tool_calls() {
        let text = [
            r#"{"kind":"header","version":4,"id":"x","createdAt":1,"cwd":"/w"}"#,
            r#"{"kind":"entry","lane":"main","type":"message","id":"1","seq":2,"parentId":null,"timestamp":1,"message":{"kind":"user","role":"user","content":"You are working AMP ticket #7","timestamp":1}}"#,
            r#"{"kind":"record","type":"assistant_frame","id":"2","seq":3}"#,
            r#"{"kind":"entry","lane":"main","type":"message","id":"3","seq":4,"parentId":"1","timestamp":2,"message":{"kind":"assistant","role":"assistant","content":[{"type":"text","text":"reading it"},{"type":"toolCall","id":"c1","name":"amp_get_task","arguments":{"task_id":7}},{"type":"toolCall","id":"c2","name":"bash","arguments":{"command":"ls"}}],"api":"openai-completions","provider":"faux","model":"m","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0.0,"output":0.0,"cacheRead":0.0,"cacheWrite":0.0,"total":0.0}},"stopReason":"tooluse","timestamp":2}}"#,
            r#"{"kind":"entry","lane":"main","type":"message","id":"4","seq":5,"parentId":"3","timestamp":3,"message":{"kind":"tool_result","role":"toolResult","toolCallId":"c1","toolName":"amp_get_task","content":[{"type":"text","text":"{\"task\":{\"id\":7}}"}],"details":null,"isError":false,"timestamp":3}}"#,
            "{\"kind\":\"entry\",\"torn",
        ]
        .join("\n");
        let events = replay_text(&text);
        let kinds: Vec<&str> = events
            .iter()
            .map(|e| match e {
                AgentEvent::MessageStart {
                    message: AgentMessage::User(_),
                } => "user",
                AgentEvent::MessageStart { .. } => "start",
                AgentEvent::MessageEnd { .. } => "end",
                AgentEvent::TurnStart => "turn",
                AgentEvent::ToolExecutionStart { .. } => "tool",
                AgentEvent::ToolExecutionEnd {
                    is_error: false, ..
                } => "result",
                AgentEvent::ToolExecutionEnd { is_error: true, .. } => "unfinished",
                _ => "other",
            })
            .collect();
        assert_eq!(
            kinds,
            [
                "user",
                "end",
                "turn",
                "start",
                "end",
                "tool",
                "tool",
                "result",
                "unfinished"
            ]
        );
    }
}
