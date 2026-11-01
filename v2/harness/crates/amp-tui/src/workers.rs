//! A worker's session in the chat (#2252), like opening a subagent's session in opencode or
//! Claude Code: the person watches what a worker is doing, switches between workers, goes back to
//! the manager, and stops a worker that is going the wrong way.
//!
//! - [`SessionView`] is one worker's transcript. It is drawn by a second [`ChatView`] (so thinking
//!   collapses, tool calls are one line each, and everything wraps exactly as in the main chat),
//!   fed from the supervisor's [`WorkerFeed`] for a worker of this chat (live, or finished in
//!   this process) or from a replay of its session JSONL ([`amp_core::worker_session::replay`])
//!   for an older one. Only its transcript is shown: [`ChatView::show_session`] swaps it into the
//!   main scroll view.
//! - [`SessionBar`] is the header above it (#2347): `worker #2252 name` and its state, the
//!   agent, model, story › epic, branch and what it waits on, then its tokens, cost and context
//!   (`ctx ███░░░░░░░ 31% of 128k`), all live.
//! - [`KeysBar`] takes the editor's place while a session is shown: the keys, or the stop
//!   confirmation ([`stop_question`]).
//!
//! The keys themselves are [`crate::Shell`]'s (`Shell::session_key`).

use std::{
    sync::{Arc, Mutex},
    time::{Duration, Instant, SystemTime},
};

use amp_core::{
    events::{RunEvent, Totals},
    supervisor::{WorkerOutcome, WorkerPhase, WorkerSessionInfo},
    worker_session::WorkerFeed,
};
use rpi_agent::{AgentEvent, AgentMessage};
use rpi_ai::types::{Content, UserContent};
use rpi_tui::{Component, theme};

use crate::roster::WorkerEntry;
use crate::transcript::ThinkingDisplay;
use crate::view::ChatView;

/// The keys while a session is shown.
pub const SESSION_KEYS: &str = "Esc back to the chat · Tab/→ next worker · Shift+Tab/← previous · x stop · o open ticket in AMP · PgUp/PgDn scroll";
/// The keys for a session that can't be stopped (finished, or read from disk).
pub const READ_ONLY_KEYS: &str = "Esc back to the chat · Tab/→ next worker · Shift+Tab/← previous · o open ticket in AMP · PgUp/PgDn scroll · read-only";

/// The question before a stop: one key stops, anything else keeps the worker going.
pub fn stop_question(task_id: i64) -> String {
    format!(
        "stop the worker on #{task_id}? its run is reported abandoned and the ticket goes back to \
         ready · y / Enter stop · n / Esc keep it running"
    )
}

/// Where a session's events come from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Source {
    /// A worker of this chat (live, or finished in this process): its feed.
    Chat,
    /// A session file on disk (an earlier chat), read once.
    File(String),
}

/// The header above a worker's transcript (#2347): the ticket and its state, who runs it on
/// what for which story, and its live tokens and context ([`crate::roster::header_lines`]),
/// then a rule.
#[derive(Debug, Default)]
pub struct SessionBar {
    entry: Mutex<WorkerEntry>,
}

impl SessionBar {
    pub fn set(&self, entry: WorkerEntry) {
        *self.entry.lock().unwrap_or_else(|e| e.into_inner()) = entry;
    }

    pub fn entry(&self) -> WorkerEntry {
        self.entry.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }
}

impl Component for SessionBar {
    fn render(&self, width: usize) -> Vec<String> {
        let e = self.entry();
        let c = theme().colors;
        let mut lines = crate::roster::header_lines(&e, width, SystemTime::now());
        lines.push(c.border_muted.fg(&"─".repeat(width)));
        lines
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}

/// The editor's place while a session is shown: the keys (or the stop question), in the Message
/// box (#2882; its edges replaced the rules above and below).
#[derive(Debug, Default)]
pub struct KeysBar {
    text: Mutex<(String, bool)>,
}

impl KeysBar {
    /// `warning`: the stop question, drawn in the warning colour.
    pub fn set(&self, text: &str, warning: bool) {
        *self.text.lock().unwrap_or_else(|e| e.into_inner()) = (text.to_owned(), warning);
    }

    pub fn text(&self) -> String {
        self.text
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .0
            .clone()
    }
}

impl Component for KeysBar {
    fn render(&self, width: usize) -> Vec<String> {
        let (text, warning) = self.text.lock().unwrap_or_else(|e| e.into_inner()).clone();
        let c = theme().colors;
        let color = if warning { c.warning } else { c.dim };
        crate::wrap::wrap(&text, width.max(1))
            .iter()
            .map(|row| color.fg(row))
            .collect()
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}

/// One worker's session, shown on demand.
pub struct SessionView {
    pub task_id: i64,
    pub source: Source,
    view: ChatView,
    feed: Arc<WorkerFeed>,
    seen: u64,
    bar: Arc<SessionBar>,
    keys: Arc<KeysBar>,
    /// The worker is live (its feed still grows).
    live: bool,
    /// Its first event's wall-clock time and [`Instant`]: how event times map to the view's
    /// clock (#2355).
    anchor: Option<(SystemTime, Instant)>,
    /// Usage counted from the events (sessions read from disk; the supervisor has the rest).
    totals: Totals,
    model: String,
}

impl SessionView {
    pub fn new(
        task_id: i64,
        source: Source,
        feed: Arc<WorkerFeed>,
        thinking: ThinkingDisplay,
    ) -> Self {
        let mut view = ChatView::new();
        view.set_show_thinking(thinking);
        // Its transcript is padded by the main view's scroll slot already.
        view.set_padding(0);
        let mut s = Self {
            task_id,
            source,
            view,
            feed,
            seen: 0,
            bar: Arc::new(SessionBar::default()),
            keys: Arc::new(KeysBar::default()),
            live: false,
            anchor: None,
            totals: Totals::default(),
            model: String::new(),
        };
        s.keys.set(READ_ONLY_KEYS, false);
        s.pump();
        s
    }

    /// Applies the events the feed got since the last call; returns whether there were any.
    pub fn pump(&mut self) -> bool {
        let (events, seen) = self.feed.since(self.seen);
        self.seen = seen;
        let (now, wall) = (Instant::now(), SystemTime::now());
        for (at, event) in &events {
            let at = at.map(|at| {
                let anchor = *self.anchor.get_or_insert_with(|| anchor(at, now, wall));
                instant(at, anchor)
            });
            self.apply(event, at);
        }
        !events.is_empty()
    }

    fn apply(&mut self, event: &AgentEvent, at: Option<Instant>) {
        if let Some(e) = RunEvent::from_agent(event) {
            self.totals.observe(&e);
        }
        match event {
            AgentEvent::MessageStart {
                message: AgentMessage::User(m),
            } => {
                let text = match &m.content {
                    UserContent::Text(t) => t.clone(),
                    UserContent::Blocks(blocks) => blocks
                        .iter()
                        .filter_map(|b| match b {
                            Content::Text(t) => Some(t.text.as_str()),
                            _ => None,
                        })
                        .collect::<Vec<_>>()
                        .join("\n"),
                };
                if !text.trim().is_empty() {
                    self.view.add_user(text.trim());
                }
            }
            AgentEvent::MessageStart {
                message: AgentMessage::Assistant(m),
            } if self.model.is_empty() => {
                self.model = format!("{}/{}", m.provider, m.model);
                self.view.apply_at(event, at);
            }
            _ => self.view.apply_at(event, at),
        }
    }

    /// Follows the supervisor's view of the worker (`None`: a session read from disk), with the
    /// rest of what the chat knows about it in `entry` (the roster's, or a bare one). Returns
    /// whether the worker is still live.
    pub fn update(&mut self, info: Option<&WorkerSessionInfo>, mut entry: WorkerEntry) -> bool {
        entry.task_id = self.task_id;
        match info {
            Some(i) => {
                if entry.model.is_empty() {
                    entry.model = i.model.clone().unwrap_or_else(|| self.model.clone());
                }
                self.live = i.is_live();
            }
            None => {
                if entry.model.is_empty() {
                    entry.model = self.model.clone();
                }
                if entry.tokens() == 0 {
                    entry.usage = self.totals.usage;
                }
                if entry.context_tokens == 0 {
                    entry.context_tokens = self.totals.context_tokens;
                }
                if entry.state.is_empty() {
                    entry.state = match &self.source {
                        Source::File(name) => format!("from {name}"),
                        Source::Chat => "finished".to_owned(),
                    };
                }
                self.live = false;
            }
        }
        self.bar.set(entry);
        self.live
    }

    /// The model the session's own events name (`provider/model`).
    pub fn model(&self) -> &str {
        &self.model
    }

    /// Usage counted from its own events (a session read from disk).
    pub fn totals(&self) -> &Totals {
        &self.totals
    }

    pub fn is_live(&self) -> bool {
        self.live
    }

    /// Whether it follows `feed` (a new run of the same ticket has a new feed).
    pub fn feed_is(&self, feed: &Arc<WorkerFeed>) -> bool {
        Arc::ptr_eq(&self.feed, feed)
    }

    /// How its transcript shows thinking (`/config thinking`, #2345).
    pub fn set_show_thinking(&mut self, display: ThinkingDisplay) {
        self.view.set_show_thinking(display);
    }

    /// The transcript's view (thinking and tool toggles act on it while it is shown).
    pub fn view(&self) -> &ChatView {
        &self.view
    }

    pub fn bar(&self) -> &Arc<SessionBar> {
        &self.bar
    }

    pub fn keys(&self) -> &Arc<KeysBar> {
        &self.keys
    }

    /// The keys line for its state (no stop question pending).
    pub fn reset_keys(&self) {
        self.keys.set(
            if self.live {
                SESSION_KEYS
            } else {
                READ_ONLY_KEYS
            },
            false,
        );
    }

    /// The parts [`ChatView::show_session`] swaps in.
    pub fn parts(&self) -> crate::view::SessionParts {
        crate::view::SessionParts {
            transcript: self.view.transcript_component(),
            bar: self.bar.clone(),
            keys: self.keys.clone(),
        }
    }
}

/// `running 1m03s`, `queued 4s`, `stopping…`, `completed after 3m10s`, `stopped by … from the chat`.
pub fn session_state(i: &WorkerSessionInfo, now: SystemTime) -> String {
    let since = now.duration_since(i.since).unwrap_or(Duration::ZERO);
    if let Some(phase) = i.phase {
        if i.stopped_by.is_some() {
            return "stopping…".to_owned();
        }
        return match phase {
            WorkerPhase::Queued => format!("queued {}", duration(since)),
            WorkerPhase::Running => format!("running {}", duration(since)),
        };
    }
    let took = i
        .ended
        .and_then(|e| e.duration_since(i.since).ok())
        .unwrap_or(Duration::ZERO);
    match (i.outcome, &i.stopped_by) {
        (Some(WorkerOutcome::Cancelled), Some(by)) => {
            format!(
                "{} after {}",
                amp_core::supervisor::stopped_from_chat(by),
                duration(took)
            )
        }
        (Some(o), _) => format!("{} after {}", o.as_str().replace('_', " "), duration(took)),
        (None, _) => "finished".to_owned(),
    }
}

/// The session's first event time `at` and its [`Instant`], given that `wall` is `now`
/// (#2355). An `at` the monotonic clock can't go back to (from before this machine started)
/// maps to `now`: only differences between event times matter then.
fn anchor(at: SystemTime, now: Instant, wall: SystemTime) -> (SystemTime, Instant) {
    let ago = wall.duration_since(at).unwrap_or(Duration::ZERO);
    (at, now.checked_sub(ago).unwrap_or(now))
}

/// The [`Instant`] of a wall-clock time `at`, by its distance from `anchor`.
fn instant(at: SystemTime, (wall, instant): (SystemTime, Instant)) -> Instant {
    match at.duration_since(wall) {
        Ok(after) => instant + after,
        Err(e) => instant.checked_sub(e.duration()).unwrap_or(instant),
    }
}

fn duration(d: Duration) -> String {
    crate::roster::duration(d)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn info(
        phase: Option<WorkerPhase>,
        outcome: Option<WorkerOutcome>,
        by: Option<&str>,
    ) -> WorkerSessionInfo {
        let since = SystemTime::UNIX_EPOCH + Duration::from_secs(1000);
        WorkerSessionInfo {
            task_id: 7,
            agent_id: "amp:amp-worker-backend".into(),
            phase,
            model: Some("wf/wf-1".into()),
            cwd: None,
            since,
            ended: outcome.map(|_| since + Duration::from_secs(190)),
            totals: Totals::default(),
            outcome,
            error: None,
            stopped_by: by.map(str::to_owned),
            returned: None,
            session_file: None,
            branch: None,
            context_window: None,
        }
    }

    #[test]
    fn the_state_reads_like_the_worker_list() {
        let now = SystemTime::UNIX_EPOCH + Duration::from_secs(1063);
        assert_eq!(
            session_state(&info(Some(WorkerPhase::Running), None, None), now),
            "running 1m03s"
        );
        assert_eq!(
            session_state(&info(Some(WorkerPhase::Queued), None, None), now),
            "queued 1m03s"
        );
        assert_eq!(
            session_state(&info(Some(WorkerPhase::Running), None, Some("jo")), now),
            "stopping…"
        );
        assert_eq!(
            session_state(&info(None, Some(WorkerOutcome::Cancelled), Some("jo")), now),
            "stopped by jo from the chat after 3m10s"
        );
        assert_eq!(
            session_state(&info(None, Some(WorkerOutcome::TurnBudget), None), now),
            "turn budget after 3m10s"
        );
    }

    fn message(
        blocks: Vec<rpi_ai::providers::faux::FauxBlock>,
    ) -> Arc<rpi_ai::types::AssistantMessage> {
        Arc::new(rpi_ai::providers::faux::faux_assistant_message(
            blocks,
            rpi_ai::types::StopReason::ToolUse,
        ))
    }

    fn rows(session: &SessionView) -> Vec<String> {
        session
            .view()
            .transcript_component()
            .render(100)
            .iter()
            .map(|r| rpi_tui::strip_ansi(r).trim_end().to_owned())
            .collect()
    }

    /// #2355: a session opened after its worker did some turns shows how long the earlier
    /// thinking and tool calls took, from the feed's times, not `Thinking` and `0 ms`.
    #[test]
    fn a_session_opened_mid_run_shows_earlier_durations_from_the_feed() {
        use rpi_ai::providers::faux::FauxBlock;
        let t0 = SystemTime::now() - Duration::from_secs(120);
        let at = |s: u64| Some(t0 + Duration::from_secs(s));
        let feed = WorkerFeed::new();
        let thinking = message(vec![FauxBlock::thinking("Looking at the tests first.")]);
        let answered = message(vec![
            FauxBlock::thinking("Looking at the tests first."),
            FauxBlock::text("Running them."),
        ]);
        let update = |m: &Arc<rpi_ai::types::AssistantMessage>| AgentEvent::MessageUpdate {
            message: Arc::clone(m),
            assistant_message_event: rpi_ai::types::AssistantMessageEvent::Start {
                partial: Arc::clone(m),
            },
        };
        let empty = message(Vec::new());
        feed.push_at(&AgentEvent::TurnStart, at(0));
        feed.push_at(
            &AgentEvent::MessageStart {
                message: AgentMessage::Assistant(Box::new((*empty).clone())),
            },
            at(0),
        );
        feed.push_at(&update(&thinking), at(1));
        feed.push_at(&update(&thinking), at(3));
        // The answer starts 7s in: the thinking took 6s.
        feed.push_at(&update(&answered), at(7));
        feed.push_at(&update(&answered), at(8));
        feed.push_at(
            &AgentEvent::MessageEnd {
                message: AgentMessage::Assistant(Box::new((*answered).clone())),
            },
            at(8),
        );
        feed.push_at(
            &AgentEvent::ToolExecutionStart {
                tool_call_id: "c1".into(),
                tool_name: "bash".into(),
                args: serde_json::json!({"command": "sleep 20"}),
            },
            at(8),
        );
        feed.push_at(
            &AgentEvent::ToolExecutionEnd {
                tool_call_id: "c1".into(),
                tool_name: "bash".into(),
                result: rpi_agent::AgentToolResult::text("ok"),
                is_error: false,
            },
            at(26),
        );
        let session = SessionView::new(7, Source::Chat, feed, ThinkingDisplay::Collapsed);
        let rows = rows(&session);
        let text = rows.join("\n");
        assert!(
            rows.iter()
                .any(|r| r.trim_start().starts_with("▸ Thought for 6s")),
            "{text}"
        );
        assert!(
            rows.iter()
                .any(|r| r.contains("bash") && r.ends_with("· 18.0s")),
            "{text}"
        );
        assert!(!text.contains("0 ms"), "{text}");
    }

    /// #2355: a session read from its file takes the file's times; a call that never returned
    /// shows no duration rather than `0 ms`.
    #[test]
    fn a_session_file_replays_with_its_own_durations() {
        let text = [
            r#"{"kind":"entry","type":"message","timestamp":1000,"message":{"kind":"user","role":"user","content":"go","timestamp":1000}}"#,
            r#"{"kind":"entry","type":"message","timestamp":2500,"message":{"kind":"assistant","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Running both."},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"sleep 18"}},{"type":"toolCall","id":"c2","name":"bash","arguments":{"command":"sleep 99"}}],"api":"openai-completions","provider":"faux","model":"m","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0.0,"output":0.0,"cacheRead":0.0,"cacheWrite":0.0,"total":0.0}},"stopReason":"tooluse","timestamp":1100}}"#,
            r#"{"kind":"entry","type":"message","timestamp":20500,"message":{"kind":"tool_result","role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"ok"}],"details":null,"isError":false,"timestamp":20500}}"#,
        ]
        .join("\n");
        let events = amp_core::worker_session::replay_text(&text);
        let session = SessionView::new(
            7,
            Source::File("x.jsonl".into()),
            WorkerFeed::from_events(events),
            ThinkingDisplay::Collapsed,
        );
        let rows = rows(&session);
        let all = rows.join("\n");
        assert!(
            rows.iter()
                .any(|r| r.contains("sleep 18") && r.ends_with("· 18.0s")),
            "{all}"
        );
        let unfinished = rows
            .iter()
            .find(|r| r.contains("sleep 99"))
            .unwrap_or_else(|| panic!("{all}"));
        assert!(!unfinished.ends_with(" ms"), "{all}");
        assert!(!all.contains("0 ms"), "{all}");
        // Seen complete at once: the thinking's own duration isn't in the file.
        assert!(
            rows.iter()
                .any(|r| r.trim_start().starts_with("▸ Thinking ·")),
            "{all}"
        );
    }

    #[test]
    fn the_bar_keeps_every_line_within_the_width() {
        let bar = SessionBar::default();
        bar.set(WorkerEntry {
            task_id: 2252,
            name: "Open a worker's live session from the chat, and stop it from there".into(),
            agent_id: "amp:amp-worker-backend".into(),
            model: "openrouter/deepseek/deepseek-v4.1".into(),
            state: "running 1m03s".into(),
            phase: crate::roster::Phase::Running,
            local: true,
            ..WorkerEntry::default()
        });
        for w in [30, 60, 80, 140] {
            for line in bar.render(w) {
                assert!(rpi_tui::visible_width(&line) <= w, "{w}: {line}");
            }
        }
        let wide = rpi_tui::strip_ansi(&bar.render(140)[0]);
        assert!(wide.starts_with("worker #2252 Open a worker"), "{wide}");
        assert!(wide.contains("running 1m03s"), "{wide}");
        let narrow = rpi_tui::strip_ansi(&bar.render(30)[0]);
        assert!(narrow.starts_with("worker #2252"), "{narrow}");
    }
}
