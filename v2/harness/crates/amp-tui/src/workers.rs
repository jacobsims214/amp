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
//! - [`SessionBar`] is the line above it: `worker #2252 name · amp:amp-worker-backend · model ·
//!   running 1m03s · ↑12.3k ↓1.2k $0.04`.
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
use rpi_tui::{Component, theme, visible_width};

use crate::chrome::{clean, shorten};
use crate::transcript::ThinkingDisplay;
use crate::view::ChatView;

/// The keys while a session is shown.
pub const SESSION_KEYS: &str = "Esc back to the manager · Tab/→ next worker · Shift+Tab/← previous · x stop · PgUp/PgDn scroll";
/// The keys for a session that can't be stopped (finished, or read from disk).
pub const READ_ONLY_KEYS: &str = "Esc back to the manager · Tab/→ next worker · Shift+Tab/← previous · PgUp/PgDn scroll · read-only";

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

/// What the session bar shows.
#[derive(Debug, Clone, Default)]
pub struct BarInfo {
    pub task_id: i64,
    /// The ticket's name, when the board knows it.
    pub name: String,
    pub agent_id: String,
    pub model: String,
    /// `running 1m03s`, `queued 4s`, `completed after 3m10s`, `stopped by … from the chat`.
    pub state: String,
    /// Whether it is still running (the state is drawn in the accent colour).
    pub live: bool,
    pub totals: Totals,
}

/// The line above a worker's transcript.
#[derive(Debug, Default)]
pub struct SessionBar {
    info: Mutex<BarInfo>,
}

impl SessionBar {
    pub fn set(&self, info: BarInfo) {
        *self.info.lock().unwrap_or_else(|e| e.into_inner()) = info;
    }

    pub fn info(&self) -> BarInfo {
        self.info.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }
}

impl Component for SessionBar {
    fn render(&self, width: usize) -> Vec<String> {
        let i = self.info();
        let c = theme().colors;
        let u = &i.totals.usage;
        let usage = format!(
            "↑{} ↓{} ${:.2}",
            crate::overlay::tokens(u.input_tokens + u.cache_read_tokens + u.cache_write_tokens),
            crate::overlay::tokens(u.output_tokens),
            u.cost_usd
        );
        let head = format!("worker #{}", i.task_id);
        // Kept whole while they fit, in this order; the ticket name takes what is left.
        let tail: Vec<(String, rpi_tui::theme::Color)> = [
            (clean(&i.agent_id), c.muted),
            (clean(&i.model), c.muted),
            (clean(&i.state), if i.live { c.accent } else { c.text }),
            (usage, c.dim),
        ]
        .into_iter()
        .filter(|(t, _)| !t.is_empty())
        .collect();
        let sep = " · ";
        let mut used = visible_width(&head);
        let mut parts = vec![rpi_tui::bold(&c.accent.fg(&head))];
        let mut kept = Vec::new();
        for (text, color) in &tail {
            let w = visible_width(sep) + visible_width(text);
            if used + w > width {
                break;
            }
            used += w;
            kept.push(c.dim.fg(sep) + &color.fg(text));
        }
        let name = clean(&i.name);
        let room = width.saturating_sub(used + 1);
        if !name.is_empty() && room >= 8 {
            parts.push(" ".to_owned() + &c.text.fg(&shorten(&name, room)));
        }
        parts.extend(kept);
        let line = parts.concat();
        let rule = c.border_muted.fg(&"─".repeat(width));
        vec![line, rule]
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

    /// Follows the supervisor's view of the worker (`None`: a session read from disk). Returns
    /// whether the worker is still live.
    pub fn update(&mut self, info: Option<&WorkerSessionInfo>, name: &str) -> bool {
        let mut bar = BarInfo {
            task_id: self.task_id,
            name: name.to_owned(),
            ..BarInfo::default()
        };
        match info {
            Some(i) => {
                bar.agent_id = i.agent_id.clone();
                bar.model = i.model.clone().unwrap_or_else(|| self.model.clone());
                bar.totals = i.totals.clone();
                bar.live = i.is_live();
                bar.state = session_state(i, SystemTime::now());
                self.live = bar.live;
            }
            None => {
                bar.model = self.model.clone();
                bar.totals = self.totals.clone();
                bar.state = match &self.source {
                    Source::File(name) => format!("from {name}"),
                    Source::Chat => "finished".to_owned(),
                };
                self.live = false;
            }
        }
        self.bar.set(bar);
        self.live
    }

    pub fn is_live(&self) -> bool {
        self.live
    }

    /// Whether it follows `feed` (a new run of the same ticket has a new feed).
    pub fn feed_is(&self, feed: &Arc<WorkerFeed>) -> bool {
        Arc::ptr_eq(&self.feed, feed)
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
    let s = d.as_secs();
    match s {
        0..60 => format!("{s}s"),
        60..3600 => format!("{}m{:02}s", s / 60, s % 60),
        _ => format!("{}h{:02}m", s / 3600, (s % 3600) / 60),
    }
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
        bar.set(BarInfo {
            task_id: 2252,
            name: "Open a worker's live session from the chat, and stop it from there".into(),
            agent_id: "amp:amp-worker-backend".into(),
            model: "openrouter/deepseek/deepseek-v4.1".into(),
            state: "running 1m03s".into(),
            live: true,
            totals: Totals::default(),
        });
        for w in [30, 60, 80, 140] {
            for line in bar.render(w) {
                assert!(visible_width(&line) <= w, "{w}: {line}");
            }
        }
        let wide = rpi_tui::strip_ansi(&bar.render(140)[0]);
        assert!(wide.starts_with("worker #2252 Open a worker"), "{wide}");
        assert!(wide.contains("running 1m03s"), "{wide}");
        let narrow = rpi_tui::strip_ansi(&bar.render(30)[0]);
        assert!(narrow.starts_with("worker #2252"), "{narrow}");
    }
}
