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
    time::{Duration, SystemTime},
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

/// The editor's place while a session is shown: a rule, the keys (or the stop question), a rule.
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
        let rule = c.border_muted.fg(&"─".repeat(width));
        let color = if warning { c.warning } else { c.dim };
        let mut lines = vec![rule.clone()];
        for row in crate::wrap::wrap(&text, width.max(1)) {
            lines.push(color.fg(&row));
        }
        lines.push(rule);
        lines
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
        for event in &events {
            self.apply(event);
        }
        !events.is_empty()
    }

    fn apply(&mut self, event: &AgentEvent) {
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
                self.view.apply(event);
            }
            _ => self.view.apply(event),
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
