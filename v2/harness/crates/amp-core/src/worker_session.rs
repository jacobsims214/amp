//! A worker's session as the chat shows it (#2252): the live event feed the supervisor keeps for
//! every worker it runs, and a replay of a finished worker's JSONL session file.
//!
//! - [`WorkerFeed`] records each [`AgentEvent`] the worker emits, so a view opened at any time
//!   can catch up from the start and then follow it. Streaming deltas (`MessageUpdate`,
//!   `ToolExecutionUpdate`) carry a snapshot of the whole partial message; only the latest of a
//!   run of them is kept, so memory stays linear in the output. Each entry has a sequence number
//!   and a replaced entry gets a new one, so [`WorkerFeed::since`] never misses an update.
//!   Each entry also keeps when it happened (#2355), so a view that opens mid-run shows how long
//!   earlier tool calls and thinking took instead of `0 ms`: a run of updates keeps the time of
//!   its first one, and an update that changes the message's shape (a text block after the
//!   thinking, say) starts a new entry, so the moment the thinking ended is kept.
//! - [`replay`] turns a session JSONL (rpi-harness v4: `{"kind":"entry","type":"message",
//!   "message":{...}}` lines) back into the events a live view would have seen: the prompt, each
//!   assistant message, and each tool call with its result. Streaming frames and records are
//!   skipped; a line that doesn't parse is skipped too (a torn tail). Each event carries the
//!   file's own time for it (#2355): a tool call starts when its assistant message was saved and
//!   ends at its result's timestamp; an assistant message starts at its own timestamp (when the
//!   request went out) and ends when it was saved.
//! - [`latest_session`] finds a ticket's newest session file under `<state>/sessions/<id>/`.

use std::{
    collections::HashMap,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{Duration, SystemTime},
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
    entries: Vec<(u64, Option<SystemTime>, AgentEvent)>,
    next: u64,
}

/// An event and when it happened (`None`: not known, so no duration is shown for it).
pub type TimedEvent = (Option<SystemTime>, AgentEvent);

/// The kinds of an assistant message's content blocks, in order: an update that changes them
/// (the answer's text starts after the thinking) is kept as its own entry.
fn shape(event: &AgentEvent) -> Vec<std::mem::Discriminant<Content>> {
    match event {
        AgentEvent::MessageUpdate { message, .. } => {
            message.content.iter().map(std::mem::discriminant).collect()
        }
        _ => Vec::new(),
    }
}

impl WorkerFeed {
    pub fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    /// A feed holding `events` (a replayed session).
    pub fn from_events(events: Vec<TimedEvent>) -> Arc<Self> {
        let feed = Self::new();
        for (at, e) in &events {
            feed.push_at(e, *at);
        }
        feed
    }

    /// Records one event, as happening now.
    pub fn push(&self, event: &AgentEvent) {
        self.push_at(event, Some(SystemTime::now()));
    }

    /// Records one event that happened `at`. A streaming delta replaces the delta right before
    /// it of the same kind (the same message with the same blocks, or the same tool call), with a
    /// new sequence number and the time of the first delta of the run.
    pub fn push_at(&self, event: &AgentEvent, at: Option<SystemTime>) {
        let mut inner = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        inner.next += 1;
        let seq = inner.next;
        let last = inner.entries.last().map(|(_, t, e)| (*t, e));
        let replace = match (last.map(|(_, e)| e), event) {
            (Some(prev @ AgentEvent::MessageUpdate { .. }), AgentEvent::MessageUpdate { .. }) => {
                shape(prev) == shape(event)
            }
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
        let at = match last {
            Some((first, _)) if replace => first,
            _ => at,
        };
        if replace {
            inner.entries.pop();
        }
        inner.entries.push((seq, at, event.clone()));
    }

    /// The events after sequence number `seen` (0 = from the start), and the sequence number to
    /// pass next time. An update that replaced one already seen comes again (applying it twice
    /// is harmless: it carries the whole partial message).
    pub fn since(&self, seen: u64) -> (Vec<TimedEvent>, u64) {
        let inner = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        let start = inner
            .entries
            .iter()
            .rposition(|(seq, _, _)| *seq <= seen)
            .map_or(0, |i| i + 1);
        let events = inner.entries[start..]
            .iter()
            .map(|(_, at, e)| (*at, e.clone()))
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
pub fn replay(path: &Path) -> Result<Vec<TimedEvent>, String> {
    let text =
        std::fs::read_to_string(path).map_err(|e| format!("read {}: {e}", path.display()))?;
    Ok(replay_text(&text))
}

/// A session file's milliseconds since the epoch.
fn ms(value: &Value) -> Option<SystemTime> {
    let ms = u64::try_from(value.as_i64()?).ok()?;
    Some(SystemTime::UNIX_EPOCH + Duration::from_millis(ms))
}

/// [`replay`] on the file's text.
pub fn replay_text(text: &str) -> Vec<TimedEvent> {
    let mut events: Vec<TimedEvent> = Vec::new();
    // A line without a time takes the one before it.
    let mut now: Option<SystemTime> = None;
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
        // When it was saved (an assistant message: when it ended).
        let saved = ms(&value["timestamp"])
            .or_else(|| ms(&value["message"]["timestamp"]))
            .or(now);
        now = saved;
        match &message {
            AgentMessage::User(_) | AgentMessage::Custom(_) => {
                events.push((
                    saved,
                    AgentEvent::MessageStart {
                        message: message.clone(),
                    },
                ));
                events.push((saved, AgentEvent::MessageEnd { message }));
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
                // The request went out at the message's own timestamp.
                let started = ms(&value["message"]["timestamp"])
                    .filter(|t| saved.is_none_or(|s| *t <= s))
                    .or(saved);
                events.push((started, AgentEvent::TurnStart));
                events.push((
                    started,
                    AgentEvent::MessageStart {
                        message: message.clone(),
                    },
                ));
                events.push((
                    saved,
                    AgentEvent::MessageEnd {
                        message: message.clone(),
                    },
                ));
                for call in calls {
                    names.insert(call.id.clone(), call.name.clone());
                    open.push((call.id.clone(), call.name.clone()));
                    events.push((
                        saved,
                        AgentEvent::ToolExecutionStart {
                            tool_call_id: call.id,
                            tool_name: call.name,
                            args: call.arguments,
                        },
                    ));
                }
            }
            AgentMessage::ToolResult(r) => {
                // The result's own timestamp is when the call returned.
                let ended = ms(&value["message"]["timestamp"]).or(saved);
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
                events.push((
                    ended,
                    AgentEvent::ToolExecutionEnd {
                        tool_call_id: r.tool_call_id.clone(),
                        tool_name: names
                            .get(&r.tool_call_id)
                            .cloned()
                            .unwrap_or_else(|| r.tool_name.clone()),
                        result,
                        is_error: r.is_error,
                    },
                ));
            }
        }
    }
    // Calls the run never finished: closed, so they don't spin forever in a read-only view, with
    // no duration (they never ended).
    for (id, name) in open {
        events.push((
            None,
            AgentEvent::ToolExecutionEnd {
                tool_call_id: id,
                tool_name: name,
                result: AgentToolResult::text(NO_RESULT),
                is_error: true,
            },
        ));
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
            .map(|(_, e)| match e {
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

    fn at(ms: u64) -> Option<SystemTime> {
        Some(SystemTime::UNIX_EPOCH + Duration::from_millis(ms))
    }

    #[test]
    fn a_replayed_session_keeps_the_files_times() {
        // The request went out at 1000, the message was saved at 2500, the bash call returned at
        // 20500 (an 18s call); the other call never returned.
        let text = [
            r#"{"kind":"entry","type":"message","timestamp":900,"message":{"kind":"user","role":"user","content":"go","timestamp":900}}"#,
            r#"{"kind":"entry","type":"message","timestamp":2500,"message":{"kind":"assistant","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"sleep 18"}},{"type":"toolCall","id":"c2","name":"bash","arguments":{"command":"sleep 99"}}],"api":"openai-completions","provider":"faux","model":"m","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0.0,"output":0.0,"cacheRead":0.0,"cacheWrite":0.0,"total":0.0}},"stopReason":"tooluse","timestamp":1000}}"#,
            r#"{"kind":"entry","type":"message","timestamp":20500,"message":{"kind":"tool_result","role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"ok"}],"details":null,"isError":false,"timestamp":20500}}"#,
        ]
        .join("\n");
        let times: Vec<(Option<SystemTime>, &str)> = replay_text(&text)
            .iter()
            .map(|(t, e)| {
                let kind = match e {
                    AgentEvent::MessageStart { .. } => "start",
                    AgentEvent::MessageEnd { .. } => "end",
                    AgentEvent::TurnStart => "turn",
                    AgentEvent::ToolExecutionStart { .. } => "tool",
                    AgentEvent::ToolExecutionEnd { .. } => "result",
                    _ => "other",
                };
                (*t, kind)
            })
            .collect();
        assert_eq!(
            times,
            [
                (at(900), "start"),
                (at(900), "end"),
                (at(1000), "turn"),
                (at(1000), "start"),
                (at(2500), "end"),
                (at(2500), "tool"),
                (at(2500), "tool"),
                (at(20500), "result"),
                // Never returned: no time, so no duration.
                (None, "result"),
            ]
        );
    }

    fn update_blocks(blocks: Vec<FauxBlock>) -> AgentEvent {
        let m = Arc::new(faux_assistant_message(blocks, StopReason::Stop));
        AgentEvent::MessageUpdate {
            message: Arc::clone(&m),
            assistant_message_event: rpi_ai::types::AssistantMessageEvent::Start { partial: m },
        }
    }

    #[test]
    fn a_run_of_updates_keeps_its_first_time_and_a_new_block_starts_a_new_entry() {
        let feed = WorkerFeed::new();
        feed.push_at(&update_blocks(vec![FauxBlock::thinking("a")]), at(1000));
        feed.push_at(&update_blocks(vec![FauxBlock::thinking("ab")]), at(4000));
        // The answer starts at 9000: the moment the thinking ended is kept.
        let answer = |t: &str| update_blocks(vec![FauxBlock::thinking("ab"), FauxBlock::text(t)]);
        feed.push_at(&answer("x"), at(9000));
        feed.push_at(&answer("xy"), at(9500));
        let (events, _) = feed.since(0);
        let times: Vec<_> = events.iter().map(|(t, _)| *t).collect();
        assert_eq!(times, [at(1000), at(9000)]);
    }
}
