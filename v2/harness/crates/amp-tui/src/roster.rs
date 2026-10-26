//! What each worker is for and how it is doing (#2347), wherever the chat mentions one.
//!
//! The person sees `run_ticket #2142` and wants to know what that worker is for. The shell
//! keeps one [`Roster`]: an entry per worker this chat runs (live, or finished in this
//! process), per worker session read from disk, and per ticket in progress elsewhere. Each
//! entry joins the supervisor's live numbers (state, elapsed time, tokens, context in use) with
//! what the AMP metadata poller has about the ticket (name, story › epic, dependencies, the last
//! recorded run; [`amp_core::meta::MetaHandle::set_workers`]). Without AMP the entry still has
//! the id, agent and live numbers, so lines fall back to the bare id.
//!
//! Lines that read the roster on every frame:
//! - [`WorkerStrip`], above the editor while workers run: one chip per worker
//!   (`▸ #2142 Fix installer setup hand-off · worker-backend (wf-1) · running 3m12s · 48k tok ·
//!   ctx 31%`) and the hint `2 running · click one or Ctrl+W to watch`. Ctrl+W focuses it
//!   (←/→ choose, Enter watch, o open in AMP, Esc back).
//! - [`WorkerNotice`]: `▶ worker started` / `■ worker finished` lines in the transcript.
//! - the dispatch line's second row (`run_ticket #2142`, `dispatch_task #2142`), through
//!   [`Roster::detail`].
//! - the session view's header ([`crate::workers::SessionBar`], [`header_lines`]) and the
//!   `/workers` rows ([`crate::overlay`]).
//!
//! [`chip`] fits a line to the width: the ticket name is shortened first (with `…`), then the
//! lowest-ranked parts go (epic, cost, model, ...); the id, the agent and the state stay.

use std::{
    any::Any,
    collections::BTreeMap,
    sync::{
        Arc, Mutex,
        atomic::{AtomicU64, Ordering},
    },
    time::{Duration, SystemTime},
};

use amp_core::events::UsageJson;
use amp_core::tickets::Status;
use rpi_tui::{Component, theme, theme::Color, truncate_to_width, visible_width};

use crate::chrome::{clean, shorten};
use crate::overlay::tokens;
use crate::theme::palette;

/// Where a worker is in its run.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum Phase {
    /// Waiting for a worker slot.
    Queued,
    Running,
    /// Asked to stop from the chat, not stopped yet.
    Stopping,
    /// Finished in this chat (or read from disk).
    Finished,
    /// In progress in another chat or for another person: AMP's record only.
    #[default]
    Elsewhere,
}

/// One worker as the chat shows it.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct WorkerEntry {
    pub task_id: i64,
    /// The ticket's name (empty until AMP has answered).
    pub name: String,
    pub epic: String,
    pub story: String,
    /// `amp:amp-worker-backend`.
    pub agent_id: String,
    /// `provider/model`.
    pub model: String,
    pub phase: Phase,
    /// `running 3m12s`, `queued 4s`, `completed after 3m10s`, `in progress 12m`.
    pub state: String,
    /// How it ended, for the colour (`completed`, `cancelled`, ...).
    pub outcome: Option<String>,
    pub usage: UsageJson,
    /// The latest response's context size.
    pub context_tokens: i64,
    /// The model's context window (0: unknown).
    pub context_window: u64,
    pub branch: Option<String>,
    /// Dependencies not completed yet.
    pub waiting_on: Vec<i64>,
    /// `Some`: the usage is AMP's record of the last run, made then (a worker elsewhere); `None`:
    /// live from this process.
    pub usage_recorded: Option<SystemTime>,
    /// Whether its session can be opened here (a worker of this chat, or a session file).
    pub viewable: bool,
    /// A worker of this chat.
    pub local: bool,
}

impl WorkerEntry {
    pub fn is_live(&self) -> bool {
        matches!(self.phase, Phase::Queued | Phase::Running | Phase::Stopping)
    }

    /// Every token the run has used: input, output and cache.
    pub fn tokens(&self) -> i64 {
        let u = &self.usage;
        u.input_tokens + u.output_tokens + u.cache_read_tokens + u.cache_write_tokens
    }

    /// The context in use as a percentage of the model's window, once both are known.
    pub fn context_percent(&self) -> Option<f64> {
        (self.context_tokens > 0 && self.context_window > 0)
            .then(|| self.context_tokens as f64 / self.context_window as f64 * 100.0)
    }

    /// `worker-backend` for `amp:amp-worker-backend`.
    pub fn agent(&self) -> String {
        clean(crate::shell::short_role(&self.agent_id))
    }

    /// The model's own id: `wf-1` for `wf/wf-1`.
    pub fn model_short(&self) -> String {
        clean(self.model.rsplit('/').next().unwrap_or(&self.model))
    }

    /// `AMP Harness › Chat TUI`, or whichever of the two is known.
    pub fn epic_story(&self) -> String {
        match (self.epic.trim(), self.story.trim()) {
            ("", "") => String::new(),
            (e, "") => clean(e),
            ("", s) => clean(s),
            (e, s) => format!("{} › {}", clean(e), clean(s)),
        }
    }

    fn state_color(&self) -> Color {
        let c = theme().colors;
        match self.phase {
            Phase::Running | Phase::Queued => c.accent,
            Phase::Stopping => c.warning,
            Phase::Elsewhere => palette().state(Status::InProgress).text,
            Phase::Finished => match self.outcome.as_deref() {
                Some("completed") => c.success,
                Some("blocked") => c.warning,
                Some(_) => c.error,
                None => c.muted,
            },
        }
    }
}

/// `48k tok`.
pub fn tok_label(n: i64) -> String {
    format!("{} tok", tokens(n))
}

/// `ctx 31%` (`ctx <1%` for a sliver).
pub fn ctx_label(percent: f64) -> String {
    crate::footer::context_label(percent)
}

/// `3m12s`.
pub fn duration(d: Duration) -> String {
    let s = d.as_secs();
    match s {
        0..60 => format!("{s}s"),
        60..3600 => format!("{}m{:02}s", s / 60, s % 60),
        _ => format!("{}h{:02}m", s / 3600, (s % 3600) / 60),
    }
}

/// `just now`, `42s ago`, `3m ago`, `5h ago`, `2d ago`.
pub fn ago(then: SystemTime, now: SystemTime) -> String {
    let d = now.duration_since(then).unwrap_or(Duration::ZERO).as_secs();
    match d {
        0..5 => "just now".to_owned(),
        5..60 => format!("{d}s ago"),
        60..3600 => format!("{}m ago", d / 60),
        3600..86_400 => format!("{}h ago", d / 3600),
        _ => format!("{}d ago", d / 86_400),
    }
}

/// `███░░░░░░░` for `percent` in `cells` cells.
pub fn bar(percent: f64, cells: usize) -> String {
    let filled = ((percent / 100.0) * cells as f64)
        .round()
        .clamp(0.0, cells as f64) as usize;
    let filled = if percent > 0.0 { filled.max(1) } else { 0 };
    format!("{}{}", "█".repeat(filled), "░".repeat(cells - filled))
}

/// The bar's colour: the accent, then warning from 80%, error from 95%.
pub fn ctx_color(percent: f64) -> Color {
    let c = theme().colors;
    if percent >= 95.0 {
        c.error
    } else if percent >= 80.0 {
        c.warning
    } else {
        c.accent
    }
}

// ------------------------------------------------------------------------------------------------
// The roster
// ------------------------------------------------------------------------------------------------

/// The chat's workers, shared by everything that draws one. Clones share it.
#[derive(Clone, Default)]
pub struct Roster {
    entries: Arc<Mutex<BTreeMap<i64, WorkerEntry>>>,
    revision: Arc<AtomicU64>,
}

impl std::fmt::Debug for Roster {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Roster")
            .field("revision", &self.revision())
            .finish()
    }
}

impl Roster {
    /// Replaces every entry; returns whether anything shown changed.
    pub fn set(&self, entries: BTreeMap<i64, WorkerEntry>) -> bool {
        let mut cur = self.entries.lock().unwrap_or_else(|e| e.into_inner());
        if *cur == entries {
            return false;
        }
        *cur = entries;
        self.revision.fetch_add(1, Ordering::Relaxed);
        true
    }

    pub fn get(&self, task_id: i64) -> Option<WorkerEntry> {
        self.entries
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(&task_id)
            .cloned()
    }

    /// Every entry, by ticket.
    pub fn all(&self) -> Vec<WorkerEntry> {
        self.entries
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .cloned()
            .collect()
    }

    /// This chat's workers: the live ones (by start), then the finished ones.
    pub fn local(&self) -> Vec<WorkerEntry> {
        let mut out: Vec<WorkerEntry> = self.all().into_iter().filter(|e| e.local).collect();
        out.sort_by_key(|e| !e.is_live());
        out
    }

    /// Bumped on every change.
    pub fn revision(&self) -> u64 {
        self.revision.load(Ordering::Relaxed)
    }

    /// The dispatch line's second row for `task_id` (#2347): its chip without the id, which the
    /// line's title already shows. `None` while nothing is known about it.
    pub fn detail(&self, task_id: i64, width: usize) -> Option<String> {
        let e = self.get(task_id)?;
        Some(chip(&e, width, ChipStyle::NoId))
    }
}

// ------------------------------------------------------------------------------------------------
// Fitting a line
// ------------------------------------------------------------------------------------------------

/// One piece of a line: its text, colour, and rank (0: always kept; higher goes first).
struct Piece {
    text: String,
    color: Color,
    rank: u8,
    /// Joined to the previous piece with a space instead of ` · `.
    tight: bool,
}

impl Piece {
    fn new(text: impl Into<String>, color: Color, rank: u8) -> Self {
        Self {
            text: text.into(),
            color,
            rank,
            tight: false,
        }
    }

    fn tight(mut self) -> Self {
        self.tight = true;
        self
    }
}

/// How [`chip`] starts.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ChipStyle {
    /// `#2142 Name · …`
    WithId,
    /// `Name · …` (the dispatch line's title has the id).
    NoId,
    /// `#2142 Name · epic · agent (model)`: what it is, not how it is doing (a start notice,
    /// which stays in the transcript after the run).
    Identity,
}

/// The fewest cells a shortened ticket name keeps before lower-ranked parts give way to it.
const NAME_MIN: usize = 14;

/// `#2142 Fix installer setup hand-off · AMP Harness › M4 · worker-backend (wf-1) · running
/// 3m12s · 48k tok · ctx 31% · $0.04`, fitted to `width` (see the module docs).
pub fn chip(e: &WorkerEntry, width: usize, style: ChipStyle) -> String {
    let c = theme().colors;
    let mut pieces = Vec::new();
    let epic = e.epic_story();
    if !epic.is_empty() {
        pieces.push(Piece::new(epic, c.muted, 6));
    }
    let agent = e.agent();
    if !agent.is_empty() {
        pieces.push(Piece::new(agent, c.muted, 0));
    }
    if !e.model.is_empty() {
        pieces.push(Piece::new(format!("({})", e.model_short()), c.dim, 4).tight());
    }
    // A start notice says what the worker is, not how it is doing.
    if style != ChipStyle::Identity {
        if !e.state.is_empty() {
            pieces.push(Piece::new(clean(&e.state), e.state_color(), 0));
        }
        if e.tokens() > 0 {
            pieces.push(Piece::new(tok_label(e.tokens()), c.dim, 1));
        }
        if let Some(p) = e.context_percent() {
            pieces.push(Piece::new(ctx_label(p), ctx_color(p), 2));
        }
        if e.usage.cost_usd > 0.0 {
            pieces.push(Piece::new(format!("${:.2}", e.usage.cost_usd), c.dim, 5));
        }
        if let Some(at) = e.usage_recorded {
            pieces.push(Piece::new(
                format!("recorded {}", ago(at, SystemTime::now())),
                c.dim,
                3,
            ));
        }
    }
    let head = match style {
        ChipStyle::WithId | ChipStyle::Identity => format!("#{}", e.task_id),
        ChipStyle::NoId => String::new(),
    };
    fit(&head, &clean(&e.name), pieces, width)
}

/// Lays out `head` (bold accent), `name` and `pieces` in `width` cells.
fn fit(head: &str, name: &str, mut pieces: Vec<Piece>, width: usize) -> String {
    let c = theme().colors;
    let sep_w = 3;
    let piece_w = |p: &Piece| visible_width(&p.text) + if p.tight { 1 } else { sep_w };
    let head_w = visible_width(head);
    let name_w = visible_width(name);
    // The name's own room: what is left after the head, its space and the kept pieces.
    let lead = |name_room: usize| -> usize {
        match (head_w, name_room) {
            (0, 0) => 0,
            (0, n) => n,
            (h, 0) => h,
            (h, n) => h + 1 + n,
        }
    };
    let total = |pieces: &[Piece], name_room: usize| -> usize {
        let mut w = lead(name_room);
        for (i, p) in pieces.iter().enumerate() {
            w += if i == 0 && lead(name_room) == 0 {
                visible_width(&p.text)
            } else {
                piece_w(p)
            };
        }
        w
    };
    // Drop the highest-ranked pieces while even a shortened name doesn't fit.
    let want_name = name_w.min(NAME_MIN);
    while total(&pieces, want_name) > width {
        let Some((at, _)) = pieces
            .iter()
            .enumerate()
            .filter(|(_, p)| p.rank > 0)
            .max_by_key(|(i, p)| (p.rank, *i))
        else {
            break;
        };
        pieces.remove(at);
    }
    // The name takes what is left (whole when it fits), or goes when too little is left.
    // `total` grows one cell per name cell from one cell on.
    let base = total(&pieces, 1).saturating_sub(1);
    let name_room = name_w.min(width.saturating_sub(base));
    let name_text = if name_room > 0 && (name_room >= 4 || name_room == name_w) {
        shorten(name, name_room)
    } else {
        String::new()
    };

    let mut out = String::new();
    if head_w > 0 {
        out.push_str(&rpi_tui::bold(&c.accent.fg(head)));
    }
    if !name_text.is_empty() {
        if !out.is_empty() {
            out.push(' ');
        }
        out.push_str(&c.text.fg(&name_text));
    }
    for p in &pieces {
        if !out.is_empty() {
            if p.tight {
                out.push(' ');
            } else {
                out.push_str(&c.dim.fg(" · "));
            }
        }
        out.push_str(&p.color.fg(&p.text));
    }
    truncate_to_width(&out, width, "")
}

// ------------------------------------------------------------------------------------------------
// The session view's header
// ------------------------------------------------------------------------------------------------

/// The worker session's header (#2347), each line within `width`:
///
/// ```text
/// worker #2142 Fix installer setup hand-off                              running 3m12s
/// amp:amp-worker-backend · openrouter/deepseek/deepseek-v4.1 · AMP Harness › M4 · branch amp-2142
/// in 12.3k · out 1.2k · cached 9.8k · $0.04                    ctx ███░░░░░░░ 31% of 128k
/// ```
pub fn header_lines(e: &WorkerEntry, width: usize, now: SystemTime) -> Vec<String> {
    let c = theme().colors;
    let mut lines = Vec::new();

    // 1: the ticket, and its state on the right.
    let head = format!("worker #{}", e.task_id);
    let state = clean(&e.state);
    let state_w = visible_width(&state);
    let head_w = visible_width(&head);
    let room = width.saturating_sub(head_w + 1 + if state_w > 0 { state_w + 2 } else { 0 });
    let name = clean(&e.name);
    let name = if room >= 6 {
        shorten(&name, room)
    } else {
        String::new()
    };
    let mut left = rpi_tui::bold(&c.accent.fg(&head));
    let mut used = head_w;
    if !name.is_empty() {
        left.push(' ');
        left.push_str(&rpi_tui::bold(&c.text.fg(&name)));
        used += 1 + visible_width(&name);
    }
    if state_w > 0 && used + 2 + state_w <= width {
        let gap = width - used - state_w;
        left.push_str(&" ".repeat(gap));
        left.push_str(&e.state_color().fg(&state));
    }
    lines.push(truncate_to_width(&left, width, ""));

    // 2: who runs it, on what, for which story, where, and what it waits on.
    let mut pieces = Vec::new();
    if !e.agent_id.is_empty() {
        pieces.push(Piece::new(clean(&e.agent_id), c.muted, 0));
    }
    if !e.model.is_empty() {
        pieces.push(Piece::new(clean(&e.model), c.muted, 2));
    }
    let epic = e.epic_story();
    if !epic.is_empty() {
        pieces.push(Piece::new(epic, c.muted, 4));
    }
    if let Some(b) = e.branch.as_deref().filter(|b| !b.is_empty()) {
        pieces.push(Piece::new(format!("branch {}", clean(b)), c.dim, 3));
    }
    if !e.waiting_on.is_empty() {
        let ids: Vec<String> = e.waiting_on.iter().map(|id| format!("#{id}")).collect();
        pieces.push(Piece::new(
            format!("waits on {}", ids.join(", ")),
            c.warning,
            1,
        ));
    }
    if !pieces.is_empty() {
        lines.push(fit("", "", pieces, width));
    }

    // 3: tokens and cost on the left, context on the right.
    let u = &e.usage;
    let mut usage = Vec::new();
    let recorded = e.usage_recorded.map(|at| ago(at, now));
    if e.tokens() > 0 || e.local {
        let prefix = if recorded.is_some() { "last run " } else { "" };
        usage.push(Piece::new(
            format!("{prefix}in {}", tokens(u.input_tokens)),
            c.text,
            0,
        ));
        usage.push(Piece::new(
            format!("out {}", tokens(u.output_tokens)),
            c.text,
            0,
        ));
        usage.push(Piece::new(
            format!(
                "cached {}",
                tokens(u.cache_read_tokens + u.cache_write_tokens)
            ),
            c.muted,
            2,
        ));
        if u.cost_usd > 0.0 {
            usage.push(Piece::new(format!("${:.2}", u.cost_usd), c.text, 1));
        }
    } else {
        usage.push(Piece::new("no run recorded yet", c.dim, 0));
    }
    if let Some(r) = &recorded {
        usage.push(Piece::new(format!("recorded {r}"), c.dim, 3));
    }
    let ctx = context_text(e, width);
    let ctx_w = visible_width(&rpi_tui::strip_ansi(&ctx));
    let left_room = if ctx_w > 0 && width > ctx_w + 2 {
        width - ctx_w - 2
    } else {
        width
    };
    let mut row = fit("", "", usage, left_room);
    let row_w = visible_width(&row);
    if ctx_w > 0 && row_w + 2 + ctx_w <= width {
        row.push_str(&" ".repeat(width - row_w - ctx_w));
        row.push_str(&ctx);
    }
    lines.push(row);
    lines
}

/// `ctx ███░░░░░░░ 31% of 128k`, shorter on narrow screens; empty while unknown.
fn context_text(e: &WorkerEntry, width: usize) -> String {
    let c = theme().colors;
    match e.context_percent() {
        Some(p) => {
            let cells = if width >= 100 { 10 } else { 6 };
            let label = if p > 0.0 && p < 1.0 {
                "<1%".to_owned()
            } else {
                format!("{p:.0}%")
            };
            format!(
                "{} {} {} {}",
                c.muted.fg("ctx"),
                ctx_color(p).fg(&bar(p, cells)),
                c.text.fg(&label),
                c.dim.fg(&format!("of {}", tokens(e.context_window as i64)))
            )
        }
        None if e.context_tokens > 0 => {
            format!(
                "{} {}",
                c.muted.fg("ctx"),
                c.text.fg(&tokens(e.context_tokens))
            )
        }
        None => String::new(),
    }
}

// ------------------------------------------------------------------------------------------------
// The strip above the editor
// ------------------------------------------------------------------------------------------------

/// The most worker chips the strip shows; more are counted on the hint line.
pub const STRIP_ROWS: usize = 4;

#[derive(Debug, Default)]
struct StripState {
    /// Ctrl+W gave it the keys.
    focused: bool,
    selected: usize,
    /// The worker whose session is shown.
    viewing: Option<i64>,
}

/// The workers above the editor (#2347): one chip per worker and a hint. Shown while this
/// chat's workers run, while it has the keys, and while a session is shown.
#[derive(Debug)]
pub struct WorkerStrip {
    roster: Roster,
    state: Mutex<StripState>,
}

impl WorkerStrip {
    pub fn new(roster: Roster) -> Self {
        Self {
            roster,
            state: Mutex::default(),
        }
    }

    fn with<R>(&self, f: impl FnOnce(&mut StripState) -> R) -> R {
        f(&mut self.state.lock().unwrap_or_else(|e| e.into_inner()))
    }

    /// The workers it lists, in order: this chat's live ones; with the keys or a session shown,
    /// the finished ones too.
    pub fn shown(&self) -> Vec<WorkerEntry> {
        let all = self.with(|s| s.focused || s.viewing.is_some());
        self.roster
            .local()
            .into_iter()
            .filter(|e| all || e.is_live())
            .collect()
    }

    pub fn is_focused(&self) -> bool {
        self.with(|s| s.focused)
    }

    /// Gives it the keys (selecting the shown worker, else the first); false with no worker.
    pub fn focus(&self) -> bool {
        let viewing = self.with(|s| {
            s.focused = true;
            s.viewing
        });
        let shown = self.shown();
        if shown.is_empty() {
            self.with(|s| s.focused = false);
            return false;
        }
        let at = viewing
            .and_then(|v| shown.iter().position(|e| e.task_id == v))
            .unwrap_or(0);
        self.with(|s| s.selected = at);
        true
    }

    pub fn blur(&self) {
        self.with(|s| s.focused = false);
    }

    /// Moves the selection by `step`, wrapping.
    pub fn move_by(&self, step: isize) {
        let n = self.shown().len();
        if n == 0 {
            return;
        }
        self.with(|s| {
            s.selected = (s.selected as isize + step).rem_euclid(n as isize) as usize;
        });
    }

    /// The selected worker's ticket.
    pub fn selected(&self) -> Option<i64> {
        let shown = self.shown();
        let at = self.with(|s| s.selected).min(shown.len().saturating_sub(1));
        shown.get(at).map(|e| e.task_id)
    }

    pub fn set_viewing(&self, task_id: Option<i64>) {
        self.with(|s| s.viewing = task_id);
    }

    fn hint(&self, shown: &[WorkerEntry], hidden: usize) -> String {
        let (focused, viewing) = self.with(|s| (s.focused, s.viewing));
        let live = shown.iter().filter(|e| e.is_live()).count();
        let more = if hidden > 0 {
            format!(" · +{hidden} more in /workers")
        } else {
            String::new()
        };
        if focused {
            return format!("←/→ choose · Enter watch · o open in AMP · Esc back{more}");
        }
        if let Some(v) = viewing {
            return format!("watching #{v} · click another worker to switch{more}");
        }
        let running = if live == 1 {
            "1 running".to_owned()
        } else {
            format!("{live} running")
        };
        format!("{running} · click one or Ctrl+W to watch{more}")
    }
}

impl Component for WorkerStrip {
    fn render(&self, width: usize) -> Vec<String> {
        let shown = self.shown();
        if shown.is_empty() {
            return Vec::new();
        }
        let c = theme().colors;
        let p = palette();
        let (focused, selected, viewing) = self.with(|s| (s.focused, s.selected, s.viewing));
        let selected = selected.min(shown.len() - 1);
        // Keep the selection in sight when there are more than fit.
        let start = if focused && selected >= STRIP_ROWS {
            selected + 1 - STRIP_ROWS
        } else {
            0
        };
        let mut lines = Vec::new();
        for (i, e) in shown.iter().enumerate().skip(start).take(STRIP_ROWS) {
            let (glyph, color) = if focused && i == selected {
                ("❯", c.accent)
            } else if viewing == Some(e.task_id) {
                ("◆", c.accent)
            } else if e.is_live() {
                ("▸", p.state(Status::InProgress).mark)
            } else {
                ("■", c.dim)
            };
            let body = chip(e, width.saturating_sub(2), ChipStyle::WithId);
            let body = if focused && i == selected {
                rpi_tui::bold(&body)
            } else {
                body
            };
            lines.push(format!("{} {body}", color.fg(glyph)));
        }
        let hidden = shown.len().saturating_sub(STRIP_ROWS);
        let hint = self.hint(&shown, hidden);
        let hint_color = if focused { c.accent } else { c.dim };
        lines.push(hint_color.fg(&shorten(&hint, width)));
        lines
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

// ------------------------------------------------------------------------------------------------
// Transcript notices
// ------------------------------------------------------------------------------------------------

/// Which notice.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NoticeKind {
    Started,
    Finished,
}

/// `▶ worker started  #2142 Fix installer… · worker-backend (wf-1) · running 3s`, or
/// `■ worker finished #2142 … · completed after 3m10s · 48k tok`: a transcript line that reads
/// the roster on every frame, so the name fills in once AMP answers and the numbers stay current.
pub struct WorkerNotice {
    task_id: i64,
    kind: NoticeKind,
    roster: Roster,
    /// What it showed last, for when the roster no longer has the worker.
    last: Mutex<Option<WorkerEntry>>,
}

impl WorkerNotice {
    pub fn new(task_id: i64, kind: NoticeKind, roster: Roster) -> Self {
        Self {
            task_id,
            kind,
            roster,
            last: Mutex::new(None),
        }
    }

    pub fn task_id(&self) -> i64 {
        self.task_id
    }
}

impl Component for WorkerNotice {
    fn render(&self, width: usize) -> Vec<String> {
        let c = theme().colors;
        let entry = {
            let mut last = self.last.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(e) = self.roster.get(self.task_id) {
                *last = Some(e);
            }
            last.clone().unwrap_or(WorkerEntry {
                task_id: self.task_id,
                ..WorkerEntry::default()
            })
        };
        let (glyph, verb, color) = match self.kind {
            NoticeKind::Started => ("▶", "worker started ", c.accent),
            NoticeKind::Finished => ("■", "worker finished", entry.state_color()),
        };
        let lead = format!(" {} {} ", color.fg(glyph), c.muted.fg(verb));
        let lead_w = visible_width(&lead);
        let style = match self.kind {
            NoticeKind::Started => ChipStyle::Identity,
            NoticeKind::Finished => ChipStyle::WithId,
        };
        let body = chip(&entry, width.saturating_sub(lead_w), style);
        vec![String::new(), truncate_to_width(&(lead + &body), width, "")]
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

// ------------------------------------------------------------------------------------------------
// Clicks
// ------------------------------------------------------------------------------------------------

/// The `#1234` mentions in a plain screen row: (first column, last column + 1, ticket).
pub fn ticket_mentions(row: &str) -> Vec<(usize, usize, i64)> {
    let mut out = Vec::new();
    let mut col = 0;
    let mut chars = row.chars().peekable();
    let mut prev: Option<char> = None;
    while let Some(ch) = chars.next() {
        let w = unicode_width::UnicodeWidthChar::width(ch).unwrap_or(0);
        if ch == '#' && !prev.is_some_and(|p| p.is_alphanumeric()) {
            let mut digits = String::new();
            while let Some(d) = chars.peek().filter(|d| d.is_ascii_digit()) {
                digits.push(*d);
                chars.next();
            }
            let end = col + 1 + digits.len();
            if let Ok(id) = digits.parse::<i64>()
                && id > 0
            {
                out.push((col, end, id));
            }
            prev = digits.chars().last().or(Some(ch));
            col = end;
            continue;
        }
        prev = Some(ch);
        col += w;
    }
    out
}

/// The ticket a click at `column` of `row` means: the mention under it, else the first mention
/// on the row that `wanted` accepts.
pub fn ticket_at(row: &str, column: usize, wanted: impl Fn(i64) -> bool) -> Option<i64> {
    let mentions = ticket_mentions(row);
    if let Some((_, _, id)) = mentions.iter().find(|(a, b, _)| (*a..*b).contains(&column)) {
        return Some(*id);
    }
    mentions
        .into_iter()
        .map(|(_, _, id)| id)
        .find(|id| wanted(*id))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry() -> WorkerEntry {
        WorkerEntry {
            task_id: 2142,
            name: "Fix installer setup hand-off so the person lands in their project".into(),
            epic: "AMP Harness".into(),
            story: "M4".into(),
            agent_id: "amp:amp-worker-backend".into(),
            model: "openrouter/deepseek/deepseek-v4.1".into(),
            phase: Phase::Running,
            state: "running 3m12s".into(),
            usage: UsageJson {
                input_tokens: 12_300,
                output_tokens: 1_200,
                cache_read_tokens: 34_500,
                cache_write_tokens: 0,
                cost_usd: 0.04,
            },
            context_tokens: 40_000,
            context_window: 128_000,
            branch: Some("amp-2142".into()),
            local: true,
            viewable: true,
            ..WorkerEntry::default()
        }
    }

    fn plain(s: &str) -> String {
        rpi_tui::strip_ansi(s)
    }

    #[test]
    fn a_chip_keeps_the_id_agent_and_state_and_shortens_the_name() {
        let e = entry();
        let wide = plain(&chip(&e, 200, ChipStyle::WithId));
        assert_eq!(
            wide,
            "#2142 Fix installer setup hand-off so the person lands in their project · AMP \
             Harness › M4 · worker-backend (deepseek-v4.1) · running 3m12s · 48.0k tok · ctx 31% \
             · $0.04"
        );
        for w in [40, 60, 76, 80, 100, 120] {
            let line = chip(&e, w, ChipStyle::WithId);
            let text = plain(&line);
            assert!(visible_width(&line) <= w, "{w}: {text}");
            assert!(text.starts_with("#2142 "), "{w}: {text}");
            assert!(text.contains("worker-backend"), "{w}: {text}");
            assert!(text.contains("running 3m12s"), "{w}: {text}");
        }
        let at80 = plain(&chip(&e, 78, ChipStyle::WithId));
        assert!(at80.contains("Fix installer"), "{at80}");
        assert!(at80.contains('…'), "the name is shortened: {at80}");
        assert!(
            at80.contains("48.0k tok") && at80.contains("ctx 31%"),
            "{at80}"
        );
        // Too narrow for the name: the id, agent and state still show.
        let tiny = plain(&chip(&e, 40, ChipStyle::WithId));
        assert!(tiny.starts_with("#2142"), "{tiny}");
    }

    #[test]
    fn without_amp_a_chip_is_the_bare_id_and_the_live_numbers() {
        let e = WorkerEntry {
            task_id: 9002,
            agent_id: "amp:amp-worker-backend".into(),
            phase: Phase::Running,
            state: "running 4s".into(),
            local: true,
            ..WorkerEntry::default()
        };
        assert_eq!(
            plain(&chip(&e, 80, ChipStyle::WithId)),
            "#9002 · worker-backend · running 4s"
        );
    }

    #[test]
    fn the_header_has_ticket_metadata_and_live_usage_within_the_width() {
        let e = WorkerEntry {
            waiting_on: vec![2140],
            ..entry()
        };
        let now = SystemTime::now();
        let wide: Vec<String> = header_lines(&e, 140, now)
            .iter()
            .map(|l| plain(l))
            .collect();
        assert!(
            wide[0].starts_with("worker #2142 Fix installer setup hand-off"),
            "{wide:?}"
        );
        assert!(wide[0].ends_with("running 3m12s"), "{wide:?}");
        assert!(wide[1].contains("amp:amp-worker-backend"), "{wide:?}");
        assert!(
            wide[1].contains("openrouter/deepseek/deepseek-v4.1"),
            "{wide:?}"
        );
        assert!(wide[1].contains("AMP Harness › M4"), "{wide:?}");
        assert!(wide[1].contains("branch amp-2142"), "{wide:?}");
        assert!(wide[1].contains("waits on #2140"), "{wide:?}");
        assert!(
            wide[2].starts_with("in 12.3k · out 1.2k · cached 34.5k · $0.04"),
            "{wide:?}"
        );
        assert!(
            wide[2].ends_with("ctx ███░░░░░░░ 31% of 128.0k"),
            "{wide:?}"
        );
        for w in [40, 60, 80, 100] {
            for line in header_lines(&e, w, now) {
                assert!(visible_width(&line) <= w, "{w}: {}", plain(&line));
            }
        }
        let narrow: Vec<String> = header_lines(&e, 80, now).iter().map(|l| plain(l)).collect();
        assert!(narrow[0].starts_with("worker #2142 Fix"), "{narrow:?}");
        assert!(narrow[0].ends_with("running 3m12s"), "{narrow:?}");
        assert!(narrow[1].contains("waits on #2140"), "{narrow:?}");
        assert!(narrow[2].contains("31%"), "{narrow:?}");
    }

    #[test]
    fn the_strip_lists_live_workers_with_a_hint_and_takes_the_keys() {
        let roster = Roster::default();
        let mut entries = BTreeMap::new();
        entries.insert(2142, entry());
        entries.insert(
            2143,
            WorkerEntry {
                task_id: 2143,
                name: "Thinking toggle".into(),
                state: "running 1m02s".into(),
                ..entry()
            },
        );
        entries.insert(
            2100,
            WorkerEntry {
                task_id: 2100,
                phase: Phase::Finished,
                outcome: Some("completed".into()),
                state: "completed after 2m".into(),
                ..entry()
            },
        );
        assert!(roster.set(entries.clone()));
        assert!(!roster.set(entries), "unchanged");
        let strip = WorkerStrip::new(roster);
        let lines: Vec<String> = strip.render(100).iter().map(|l| plain(l)).collect();
        assert_eq!(lines.len(), 3, "{lines:?}");
        assert!(lines[0].starts_with("▸ #2142 "), "{lines:?}");
        assert!(lines[1].starts_with("▸ #2143 Thinking toggle"), "{lines:?}");
        assert_eq!(lines[2], "2 running · click one or Ctrl+W to watch");
        // Focused: the finished one is listed too; ←/→ move, the selection is marked.
        assert!(strip.focus());
        assert_eq!(strip.selected(), Some(2142));
        strip.move_by(1);
        assert_eq!(strip.selected(), Some(2143));
        strip.move_by(1);
        assert_eq!(strip.selected(), Some(2100));
        let lines: Vec<String> = strip.render(100).iter().map(|l| plain(l)).collect();
        assert!(lines[2].starts_with("❯ #2100"), "{lines:?}");
        assert!(
            lines[3].starts_with("←/→ choose · Enter watch"),
            "{lines:?}"
        );
        strip.blur();
        strip.set_viewing(Some(2143));
        let lines: Vec<String> = strip.render(100).iter().map(|l| plain(l)).collect();
        assert!(lines[1].starts_with("◆ #2143"), "{lines:?}");
        assert!(
            lines.last().unwrap().starts_with("watching #2143"),
            "{lines:?}"
        );
        for line in strip.render(60) {
            assert!(visible_width(&line) <= 60, "{}", plain(&line));
        }
    }

    #[test]
    fn clicks_find_the_ticket_under_them() {
        let row = "  ▸ #2142 Fix · worker-backend · see #77";
        assert_eq!(ticket_mentions(row), vec![(4, 9, 2142), (37, 40, 77)]);
        assert_eq!(ticket_at(row, 6, |_| false), Some(2142));
        assert_eq!(ticket_at(row, 38, |_| false), Some(77));
        assert_eq!(ticket_at(row, 20, |id| id == 77), Some(77));
        assert_eq!(ticket_at(row, 20, |_| false), None);
        assert!(ticket_mentions("issue#12 and # and #x").is_empty());
    }

    #[test]
    fn small_helpers() {
        assert_eq!(bar(31.0, 10), "███░░░░░░░");
        assert_eq!(bar(0.2, 10), "█░░░░░░░░░");
        assert_eq!(bar(0.0, 4), "░░░░");
        assert_eq!(duration(Duration::from_secs(192)), "3m12s");
        let now = SystemTime::now();
        assert_eq!(ago(now - Duration::from_secs(180), now), "3m ago");
    }
}
