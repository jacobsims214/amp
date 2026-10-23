//! The AMP chrome (M3-3, #2106): the two-line header above the transcript and the one-line AMP
//! status line between the editor and pi's footer (`research/harness-tui-theme-and-metadata.md`
//! §2, D10).
//!
//! ```text
//! ▀▀▀▀▀▀▀▀ amp.simstech.cloud · ada@example.com · simstech › AMP #2      ● connected 2026-07-28
//! ■ In progress #2106 M3-3 Header and AMP status line                    ▌ AMP Harness › M3
//! ...transcript, editor...
//! ■  2 QUESTIONS  │ workers 1 ■ running · 1 queued │ plan: 2 waiting for your approval │ ■ 3 In progress · □ 5 Ready   reported ✓ #2106
//! ```
//!
//! - **Header line 1:** the brand bands, the AMP host, the user, `org › project` (an OSC 8 link
//!   to the project's Mission page) and, right-aligned, the MCP health dot, its word and the
//!   protocol version.
//! - **Header line 2** (only while a ticket is current): the web UI's state mark, `#id` (a link to
//!   the board with the ticket's drawer open), the name, and `▌ Epic › Story` in the epic's cat
//!   colour.
//! - **Status line:** the red questions chip, this chat's workers (from its supervisor only;
//!   tickets in progress elsewhere are in the board counts), plan approval ("N of yours waiting
//!   for your approval", and others' tickets apart since per-ticket approval (#2198), links to
//!   the Flight plan page), the board counts in the web UI's words, the
//!   current ticket's artifacts in review, and, right-aligned, whether this session reported its
//!   run to AMP.
//!
//! Everything AMP sends (names, emails, reasons) is untrusted terminal text: [`clean`] drops
//! control characters (so no escape sequence gets through) and bidi overrides, and collapses
//! whitespace. Links are only built from [`Instance`]'s URL helpers, and only for `http(s)` URLs
//! without control characters ([`link`]).
//!
//! When a line is too wide, segments are dropped by rank (the board counts first, the questions
//! chip and the ticket id never); on the status line a dropped chip that still fits in what is
//! left comes back (#2230). Only the ticket name is shortened, as plain text before it is
//! styled, so a cut never lands inside an escape sequence or a link. An item whose last refresh
//! failed keeps its last value, drawn dimmed. Without a metadata source both components render no
//! lines at all.

use std::{
    any::Any,
    borrow::Borrow,
    collections::{BTreeMap, HashMap, HashSet},
    hash::{Hash, Hasher},
    sync::{Arc, Mutex},
};

use amp_core::{
    meta::{AmpMeta, Connection, Fetched, HealthLevel, Instance, MetaHandle},
    supervisor::{WorkerPhase, WorkerStatus},
    tickets::Status,
};
use rpi_agent::AgentEvent;
use rpi_tui::{Component, theme::Color, visible_width};
use serde_json::Value;

use crate::theme::{AmpPalette, palette};

/// AMP's tools whose success means "this run is on the ticket".
const REPORT_TOOLS: &[&str] = &["amp_complete_task", "amp_report_run"];
/// Cells per brand band (four bands).
const BAND_CELLS: usize = 2;
/// Header line 2 keeps `▌ Epic` beside a shortened ticket name only while the name keeps at least
/// this many cells (or all of it, when shorter); below that the label goes and the name gets the
/// room.
pub const MIN_NAME_CELLS: usize = 40;

// ------------------------------------------------------------------------------------------------
// Untrusted text and links
// ------------------------------------------------------------------------------------------------

/// Makes AMP text safe to draw: control characters (C0, DEL, C1: so no escape sequence) and bidi
/// overrides are dropped, and whitespace runs (newlines included) become one space.
pub fn clean(raw: &str) -> String {
    let mut out = String::with_capacity(raw.len());
    let mut space = false;
    for c in raw.chars() {
        if c.is_whitespace() {
            space = !out.is_empty();
            continue;
        }
        if c.is_control() || is_bidi_control(c) {
            continue;
        }
        if space {
            out.push(' ');
            space = false;
        }
        out.push(c);
    }
    out
}

fn is_bidi_control(c: char) -> bool {
    matches!(c, '\u{200E}' | '\u{200F}' | '\u{202A}'..='\u{202E}' | '\u{2066}'..='\u{2069}')
}

/// An OSC 8 hyperlink around already-styled `text`, or `text` alone when `url` is not a plain
/// `http(s)` URL (no controls, no spaces). Pass only URLs from [`Instance`]'s helpers.
pub fn link(url: &str, text: &str) -> String {
    let safe = (url.starts_with("https://") || url.starts_with("http://"))
        && url.chars().all(|c| c.is_ascii_graphic());
    if safe {
        rpi_tui::hyperlink(url, text)
    } else {
        text.to_owned()
    }
}

/// A rendered line without styling or links, right-trimmed.
pub fn plain(line: &str) -> String {
    rpi_tui::strip_ansi(line).trim_end().to_owned()
}

/// `text` cut to `max` cells with a trailing `…` (plain text only).
pub fn shorten(text: &str, max: usize) -> String {
    if visible_width(text) <= max {
        return text.to_owned();
    }
    if max == 0 {
        return String::new();
    }
    let mut out = String::new();
    let mut used = 0;
    for c in text.chars() {
        let w = visible_width(c.encode_utf8(&mut [0; 4]));
        if used + w > max - 1 {
            break;
        }
        out.push(c);
        used += w;
    }
    out.push('…');
    out
}

// ------------------------------------------------------------------------------------------------
// Inputs
// ------------------------------------------------------------------------------------------------

/// Live workers for the status line (the manager's `Supervisor::workers`).
pub type WorkersFn = Arc<dyn Fn() -> Vec<WorkerStatus> + Send + Sync>;

/// The MCP hub's health for the status line (#2245): `mcp 3/4` (ready of those that should run).
pub type McpFn = Arc<dyn Fn() -> amp_core::mcp_hub::HubSummary + Send + Sync>;

/// Whether this session's run is on a ticket (§2 item 7).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum ReportState {
    /// `amp_complete_task` / `amp_report_run` succeeded.
    Reported,
    /// The call failed: the run is not on the ticket.
    Failed,
}

#[derive(Default)]
struct Inputs {
    handle: Option<MetaHandle>,
    fixed: Option<Arc<AmpMeta>>,
    workers: Option<WorkersFn>,
    mcp: Option<McpFn>,
    /// Report outcomes by ticket, and the most recent one.
    reports: BTreeMap<i64, ReportState>,
    last_report: Option<i64>,
    /// Report calls in flight: call id → ticket.
    pending: HashMap<String, i64>,
}

/// Everything one frame of the chrome shows.
#[derive(Clone)]
pub struct ChromeData {
    pub meta: Arc<AmpMeta>,
    pub workers: Vec<WorkerStatus>,
    pub reports: BTreeMap<i64, ReportState>,
    pub last_report: Option<i64>,
    /// The external MCP servers' health (`None`: no hub, or no server that should run).
    pub mcp: Option<amp_core::mcp_hub::HubSummary>,
}

impl ChromeData {
    pub fn new(meta: AmpMeta) -> Self {
        Self {
            meta: Arc::new(meta),
            workers: Vec::new(),
            reports: BTreeMap::new(),
            last_report: None,
            mcp: None,
        }
    }
}

/// The chrome's shared inputs. Clones share them; the header and status-line components read
/// them on every render.
#[derive(Clone, Default)]
pub struct Chrome(Arc<Mutex<Inputs>>);

impl Chrome {
    pub fn new() -> Self {
        Self::default()
    }

    /// Shows the poller's snapshot (read at every render).
    pub fn set_meta(&self, handle: MetaHandle) {
        self.with(|i| {
            i.handle = Some(handle);
            i.fixed = None;
        });
    }

    /// Shows a fixed snapshot (`render-chrome`, tests).
    pub fn set_snapshot(&self, meta: AmpMeta) {
        self.with(|i| {
            i.fixed = Some(Arc::new(meta));
            i.handle = None;
        });
    }

    /// Where local workers come from (the manager's supervisor).
    pub fn set_workers(&self, workers: WorkersFn) {
        self.with(|i| i.workers = Some(workers));
    }

    /// Where the MCP servers' health comes from (the session's hub, #2245).
    pub fn set_mcp(&self, mcp: McpFn) {
        self.with(|i| i.mcp = Some(mcp));
    }

    /// Records a report outcome for `ticket` directly.
    pub fn record_report(&self, ticket: i64, state: ReportState) {
        self.with(|i| {
            i.reports.insert(ticket, state);
            i.last_report = Some(ticket);
        });
    }

    /// Feeds one agent event: the session's own `amp_complete_task` / `amp_report_run` calls
    /// decide the "reported" segment.
    pub fn observe(&self, event: &AgentEvent) {
        match event {
            AgentEvent::ToolExecutionStart {
                tool_call_id,
                tool_name,
                args,
            } if REPORT_TOOLS.contains(&tool_name.as_str()) => {
                if let Some(id) = ticket_arg(args) {
                    self.with(|i| {
                        i.pending.insert(tool_call_id.clone(), id);
                    });
                }
            }
            AgentEvent::ToolExecutionEnd {
                tool_call_id,
                tool_name,
                is_error,
                ..
            } if REPORT_TOOLS.contains(&tool_name.as_str()) => {
                let ticket = self.with(|i| i.pending.remove(tool_call_id));
                if let Some(ticket) = ticket {
                    let state = if *is_error {
                        ReportState::Failed
                    } else {
                        ReportState::Reported
                    };
                    self.record_report(ticket, state);
                }
            }
            _ => {}
        }
    }

    /// This frame's data, or `None` without a metadata source (the chrome is then hidden).
    pub fn data(&self) -> Option<ChromeData> {
        let (meta, workers, mcp, reports, last_report) = self.with(|i| {
            let meta = match (&i.fixed, &i.handle) {
                (Some(m), _) => Some(Arc::clone(m)),
                (None, Some(h)) => Some(h.snapshot()),
                (None, None) => None,
            };
            (
                meta,
                i.workers.clone(),
                i.mcp.clone(),
                i.reports.clone(),
                i.last_report,
            )
        });
        Some(ChromeData {
            meta: meta?,
            workers: workers.map(|f| f()).unwrap_or_default(),
            reports,
            last_report,
            mcp: mcp.map(|f| f()).filter(|s| s.total > 0),
        })
    }

    /// Changes whenever what the chrome shows may have changed (the terminal repaints then).
    pub fn fingerprint(&self) -> u64 {
        let mut h = std::collections::hash_map::DefaultHasher::new();
        if let Some(d) = self.data() {
            d.meta.revision.hash(&mut h);
            for w in &d.workers {
                (w.task_id, w.phase == WorkerPhase::Running).hash(&mut h);
            }
            d.reports.hash(&mut h);
            d.last_report.hash(&mut h);
            if let Some(m) = d.mcp {
                (m.ready, m.total, m.level as u8).hash(&mut h);
            }
        }
        h.finish()
    }

    /// The header component (for `ChatView::header_slot`).
    pub fn header(&self) -> Arc<dyn Component> {
        Arc::new(ChromeLine {
            chrome: self.clone(),
            part: Part::Header,
        })
    }

    /// The status-line component (for `ChatView::amp_status_slot`).
    pub fn status_line(&self) -> Arc<dyn Component> {
        Arc::new(ChromeLine {
            chrome: self.clone(),
            part: Part::Status,
        })
    }

    fn with<R>(&self, f: impl FnOnce(&mut Inputs) -> R) -> R {
        let mut guard = match self.0.lock() {
            Ok(g) => g,
            Err(poisoned) => poisoned.into_inner(),
        };
        f(&mut guard)
    }
}

fn ticket_arg(args: &Value) -> Option<i64> {
    match &args["task_id"] {
        Value::Number(n) => n.as_i64(),
        Value::String(s) => s.trim().trim_start_matches('#').parse().ok(),
        _ => None,
    }
}

#[derive(Clone, Copy)]
enum Part {
    Header,
    Status,
}

struct ChromeLine {
    chrome: Chrome,
    part: Part,
}

impl Component for ChromeLine {
    fn render(&self, width: usize) -> Vec<String> {
        let Some(data) = self.chrome.data() else {
            return Vec::new();
        };
        let p = palette();
        match self.part {
            Part::Header => render_header(&data, &p, width),
            Part::Status => render_status(&data, &p, width),
        }
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

// ------------------------------------------------------------------------------------------------
// Layout
// ------------------------------------------------------------------------------------------------

/// What goes before a segment when it is not the first one on its side.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Lead {
    Space,
    Dot,
    Bar,
}

/// One styled piece of a line. `rank` 0 is never dropped; higher ranks are dropped first.
struct Seg {
    text: String,
    width: usize,
    lead: Lead,
    rank: u8,
    /// The next segment follows after a plain space whatever its own lead (the brand bands).
    then_space: bool,
}

impl Seg {
    fn new(text: String, lead: Lead, rank: u8) -> Self {
        let width = visible_width(&text);
        Self {
            text,
            width,
            lead,
            rank,
            then_space: false,
        }
    }

    fn then_space(mut self) -> Self {
        self.then_space = true;
        self
    }
}

/// The lead actually drawn before `segs[i]` (`None` for the first).
fn lead_of<S: Borrow<Seg>>(segs: &[S], i: usize) -> Option<Lead> {
    match i {
        0 => None,
        _ if segs[i - 1].borrow().then_space => Some(Lead::Space),
        _ => Some(segs[i].borrow().lead),
    }
}

struct Style {
    colors: rpi_tui::theme::ThemeColors,
    bar: &'static str,
}

impl Style {
    fn current() -> Self {
        let t = rpi_tui::theme();
        Self {
            bar: t.border_style.chars().vertical,
            colors: t.colors,
        }
    }

    fn lead(&self, lead: Lead) -> (String, usize) {
        match lead {
            Lead::Space => (" ".to_owned(), 1),
            Lead::Dot => (self.colors.dim.fg(" · "), 3),
            Lead::Bar => (format!(" {} ", self.colors.border_muted.fg(self.bar)), 3),
        }
    }

    /// `color`, or the dim colour for a stale item.
    fn paint(&self, color: Color, text: &str, stale: bool) -> String {
        if stale {
            self.colors.dim.fg(text)
        } else {
            color.fg(text)
        }
    }
}

fn side_width<S: Borrow<Seg>>(style: &Style, segs: &[S]) -> usize {
    segs.iter()
        .enumerate()
        .map(|(i, s)| s.borrow().width + lead_of(segs, i).map_or(0, |l| style.lead(l).1))
        .sum()
}

/// The fewest blank cells between the left group and a right-aligned item, so the two never read
/// as one phrase (#2148). When they don't fit with this gap, `fit` drops segments by rank.
pub const MIN_GAP: usize = 3;

fn line_width<S: Borrow<Seg>>(style: &Style, left: &[S], right: &[S]) -> usize {
    let (l, r) = (side_width(style, left), side_width(style, right));
    if right.is_empty() {
        l
    } else if left.is_empty() {
        r
    } else {
        l + MIN_GAP + r
    }
}

/// The highest-ranked droppable segment of `left` (`false`) or `right` (`true`), the rightmost of
/// equal rank.
fn worst(left: &[(usize, Seg)], right: &[(usize, Seg)]) -> Option<(bool, usize)> {
    left.iter()
        .enumerate()
        .map(|(i, (_, s))| (s.rank, false, i))
        .chain(
            right
                .iter()
                .enumerate()
                .map(|(i, (_, s))| (s.rank, true, i)),
        )
        .filter(|(rank, _, _)| *rank > 0)
        .max_by_key(|(rank, on_right, i)| (*rank, *on_right, *i))
        .map(|(_, on_right, i)| (on_right, i))
}

fn segs(side: &[(usize, Seg)]) -> Vec<&Seg> {
    side.iter().map(|(_, s)| s).collect()
}

/// Puts `seg` back at its place (by original index) in `side`. A `·`-led segment whose original
/// predecessor is gone leads with the bar: it starts a group of its own (#2230: `□ 1 Ready` kept
/// after `■ 1 In progress` was dropped).
fn restore(side: &mut Vec<(usize, Seg)>, at: usize, mut seg: Seg) {
    let pos = side.partition_point(|(i, _)| *i < at);
    let follows_its_own = pos > 0 && side[pos - 1].0 + 1 == at;
    if matches!(seg.lead, Lead::Dot) && !follows_its_own {
        seg.lead = Lead::Bar;
    }
    side.insert(pos, (at, seg));
}

fn join(style: &Style, segs: &[Seg]) -> String {
    let mut out = String::new();
    for (i, s) in segs.iter().enumerate() {
        if let Some(lead) = lead_of(segs, i) {
            out.push_str(&style.lead(lead).0);
        }
        out.push_str(&s.text);
    }
    out
}

/// Lays `left` and right-aligned `right` out in `width` cells, dropping segments by rank until
/// they fit. The result is never wider than `width`.
fn fit(style: &Style, left: Vec<Seg>, right: Vec<Seg>, width: usize) -> String {
    fit_with(style, left, right, width, false)
}

/// [`fit`], then each dropped segment, most important (lowest rank) first, comes back if it still
/// fits in what is left. The status line's chips are independent facts, so a narrow one outlasts
/// a wide one of higher priority that had to go (#2230: `□ 1 Ready` after the others' plan chip
/// was dropped). The header keeps plain drop order: its segments read as one phrase.
fn fit_keeping(style: &Style, left: Vec<Seg>, right: Vec<Seg>, width: usize) -> String {
    fit_with(style, left, right, width, true)
}

fn fit_with(style: &Style, left: Vec<Seg>, right: Vec<Seg>, width: usize, keep: bool) -> String {
    let mut l: Vec<(usize, Seg)> = left.into_iter().enumerate().collect();
    let mut r: Vec<(usize, Seg)> = right.into_iter().enumerate().collect();
    let width_of = |l: &[(usize, Seg)], r: &[(usize, Seg)]| {
        let (ls, rs) = (segs(l), segs(r));
        line_width(style, &ls, &rs)
    };
    let mut dropped: Vec<(bool, usize, Seg)> = Vec::new();
    while width_of(&l, &r) > width {
        let Some((on_right, i)) = worst(&l, &r) else {
            break;
        };
        let (at, seg) = if on_right { r.remove(i) } else { l.remove(i) };
        dropped.push((on_right, at, seg));
    }
    if keep && !dropped.is_empty() && width_of(&l, &r) <= width {
        // Lowest rank first; on equal ranks the left side, then the leftmost.
        dropped.sort_by_key(|(on_right, at, s)| (s.rank, *on_right, *at));
        for (on_right, at, seg) in dropped {
            let side = if on_right { &mut r } else { &mut l };
            restore(side, at, seg);
            if width_of(&l, &r) > width {
                let side = if on_right { &mut r } else { &mut l };
                let pos = side
                    .iter()
                    .position(|(i, _)| *i == at)
                    .expect("just restored");
                side.remove(pos);
            }
        }
    }
    let left: Vec<Seg> = l.into_iter().map(|(_, s)| s).collect();
    let right: Vec<Seg> = r.into_iter().map(|(_, s)| s).collect();
    let used = line_width(style, &left, &right);
    if used > width {
        // Only unbreakable segments are left and they still don't fit: plain text, cut.
        let gap = " ".repeat(MIN_GAP);
        let plain = rpi_tui::strip_ansi(&format!(
            "{}{gap}{}",
            join(style, &left),
            join(style, &right)
        ));
        return shorten(plain.trim(), width);
    }
    let l = join(style, &left);
    if right.is_empty() {
        return l;
    }
    let gap = width - side_width(style, &left) - side_width(style, &right);
    format!("{l}{}{}", " ".repeat(gap), join(style, &right))
}

fn stale<T>(f: &Fetched<T>) -> bool {
    f.value.is_some() && f.error.is_some()
}

/// The poller's connection is closed (its first attempt failed, or it dropped): nothing is
/// loading, so the chrome says "not connected" / "retrying…" rather than an ellipsis that never
/// ends next to "● disconnected" (#2215).
fn disconnected(meta: &AmpMeta) -> bool {
    matches!(meta.health.connection, Connection::Down { .. })
}

// ------------------------------------------------------------------------------------------------
// Header
// ------------------------------------------------------------------------------------------------

/// The header: line 1 always, line 2 while a ticket is current. Each line fits `width` cells.
pub fn render_header(data: &ChromeData, p: &AmpPalette, width: usize) -> Vec<String> {
    let style = Style::current();
    let mut lines = vec![header_line1(&style, data, p, width)];
    if let Some(id) = data.meta.ticket_id {
        lines.push(ticket_line(&style, data, p, id, width));
    }
    lines
}

fn header_line1(style: &Style, data: &ChromeData, p: &AmpPalette, width: usize) -> String {
    let c = &style.colors;
    let meta = &data.meta;
    let mut left = vec![Seg::new(p.brand_bands(BAND_CELLS), Lead::Space, 4).then_space()];
    match &meta.instance {
        Some(inst) => left.push(Seg::new(c.muted.fg(&clean(inst.host())), Lead::Space, 8)),
        None => {
            let word = if disconnected(meta) {
                "retrying…"
            } else {
                "connecting…"
            };
            left.push(Seg::new(c.dim.fg(word), Lead::Space, 8));
        }
    }
    if let Some(user) = meta.user.get() {
        let email = clean(&user.email);
        if !email.is_empty() {
            left.push(Seg::new(
                style.paint(c.text, &email, stale(&meta.user)),
                Lead::Dot,
                6,
            ));
        }
    }
    if let Some(seg) = project_seg(style, meta) {
        left.push(seg);
    }

    let health = &meta.health.connection;
    let dot = match health.level() {
        HealthLevel::Ok => c.success,
        HealthLevel::Degraded => c.warning,
        HealthLevel::Down => c.error,
    };
    // The dot and its word ("connected") outlast everything else on the line (#2186): the
    // protocol goes first, then the left side from the host to the project.
    let mut right = vec![
        Seg::new(dot.fg("●"), Lead::Space, 1),
        Seg::new(c.muted.fg(&health_word(health)), Lead::Space, 1),
    ];
    if let Some(inst) = &meta.instance {
        let protocol = clean(&inst.protocol);
        if !protocol.is_empty() {
            right.push(Seg::new(c.dim.fg(&protocol), Lead::Space, 9));
        }
    }
    fit(style, left, right, width)
}

/// `org › project` linked to the project's Mission page, plus a dim `#id`.
fn project_seg(style: &Style, meta: &AmpMeta) -> Option<Seg> {
    let c = &style.colors;
    let Some(pid) = meta.project_id else {
        return Some(Seg::new(c.dim.fg("no project"), Lead::Dot, 2));
    };
    let is_stale = stale(&meta.project) || stale(&meta.user);
    let name = match meta.project.get() {
        Some(p) => {
            let name = clean(&p.name);
            let name = if name.is_empty() {
                clean(&p.code)
            } else {
                name
            };
            match meta.org_name().map(clean).filter(|o| !o.is_empty()) {
                Some(org) => format!(
                    "{} {} {}",
                    style.paint(c.muted, &org, is_stale),
                    c.dim.fg("›"),
                    if is_stale {
                        c.dim.fg(&name)
                    } else {
                        rpi_tui::bold(&c.text.fg(&name))
                    }
                ),
                None => style.paint(c.text, &name, is_stale),
            }
        }
        None => c.dim.fg("project"),
    };
    let text = match &meta.instance {
        Some(inst) => link(&inst.project_url(pid), &name),
        None => name,
    };
    Some(Seg::new(
        format!("{text} {}", c.dim.fg(&format!("#{pid}"))),
        Lead::Dot,
        2,
    ))
}

fn health_word(c: &Connection) -> String {
    match c {
        Connection::Connecting => "connecting".to_owned(),
        Connection::Ok => "connected".to_owned(),
        Connection::Slow { latency } => format!("slow {} ms", latency.as_millis()),
        Connection::Degraded { .. } => "degraded".to_owned(),
        Connection::AuthRequired => "re-login needed".to_owned(),
        Connection::Down { .. } => "disconnected".to_owned(),
    }
}

fn ticket_line(style: &Style, data: &ChromeData, p: &AmpPalette, id: i64, width: usize) -> String {
    let c = &style.colors;
    let meta = &data.meta;
    let id_text = rpi_tui::bold(&c.text.fg(&format!("#{id}")));
    let id_text = match (&meta.instance, meta.project_id) {
        (Some(inst), Some(pid)) => link(&inst.ticket_url(pid, id), &id_text),
        _ => id_text,
    };
    let Some(ticket) = meta.ticket.get() else {
        let note = match &meta.ticket.error {
            Some(_) => "unavailable",
            None if disconnected(meta) => "not connected",
            None => "loading…",
        };
        let left = vec![
            Seg::new(id_text, Lead::Space, 0),
            Seg::new(c.dim.fg(note), Lead::Space, 3),
        ];
        return fit(style, left, Vec::new(), width);
    };
    let is_stale = stale(&meta.ticket);
    let mark = if is_stale {
        let s = p.state(ticket.status);
        c.dim.fg(&format!("{} {}", s.glyph, s.label))
    } else {
        p.state_mark(ticket.status)
    };
    let mark = Seg::new(mark, Lead::Space, 1);
    let id_seg = Seg::new(id_text, Lead::Space, 0);

    // `▌ Epic › Story`, `▌ Epic`, or nothing. The name comes first: the story is shown only
    // beside the whole name, and the epic only while the name keeps MIN_NAME_CELLS.
    let epic_index = ticket.epic_index.unwrap_or(0);
    let epic = ticket.epic.as_ref().map(|e| clean(&e.name));
    let story = ticket.story.as_ref().map(|s| clean(&s.name));
    let (mut with_story, mut epic_only) = (None, None);
    if let Some(epic) = epic.filter(|e| !e.is_empty()) {
        let bar = if is_stale {
            c.dim.fg("▌")
        } else {
            p.epic(epic_index).fg("▌")
        };
        with_story = story
            .filter(|s| !s.is_empty())
            .map(|story| format!("{bar} {}", c.muted.fg(&format!("{epic} › {story}"))));
        epic_only = Some(format!("{bar} {}", c.muted.fg(&epic)));
    }

    let name = clean(&ticket.name);
    let name_w = visible_width(&name);
    let fixed = mark.width + 1 + id_seg.width + 1;
    let room_with = |label: &str| {
        let label_w = visible_width(label);
        width
            .saturating_sub(fixed)
            .saturating_sub(if label_w > 0 { label_w + MIN_GAP } else { 0 })
    };
    // 1. Epic › Story beside the whole name; 2. Epic beside the whole name; 3. Epic beside a
    // name shortened to no less than MIN_NAME_CELLS; 4. no label.
    let whole = |l: &&String| room_with(l) >= name_w;
    let label = with_story
        .as_ref()
        .filter(whole)
        .or_else(|| epic_only.as_ref().filter(whole))
        .or_else(|| {
            epic_only
                .as_ref()
                .filter(|l| room_with(l) >= MIN_NAME_CELLS.min(name_w))
        })
        .cloned()
        .unwrap_or_default();
    let short = shorten(&name, room_with(&label));
    let mut left = vec![mark, id_seg];
    if !short.is_empty() {
        left.push(Seg::new(
            style.paint(c.text, &short, is_stale),
            Lead::Space,
            5,
        ));
    }
    let right = if label.is_empty() {
        Vec::new()
    } else {
        vec![Seg::new(label, Lead::Space, 6)]
    };
    fit(style, left, right, width)
}

// ------------------------------------------------------------------------------------------------
// Status line
// ------------------------------------------------------------------------------------------------

/// The AMP status line: one line that fits `width` cells.
pub fn render_status(data: &ChromeData, p: &AmpPalette, width: usize) -> Vec<String> {
    let style = Style::current();
    let c = &style.colors;
    let meta = &data.meta;
    let board = meta.board.get();
    let board_stale = stale(&meta.board);
    let mut left = Vec::new();

    // The questions chip (never dropped).
    if let Some(b) = board.filter(|b| b.question > 0) {
        let q = p.state(Status::Question);
        let word = if b.question == 1 {
            "QUESTION"
        } else {
            "QUESTIONS"
        };
        let label = format!(" {} {word} ", b.question);
        let text = match (q.chip, board_stale) {
            (Some((fg, bg)), false) => format!("{} {}", q.mark.fg(&q.glyph), bg.bg(&fg.fg(&label))),
            _ => c.dim.fg(&format!("{}{label}", q.glyph)),
        };
        left.push(Seg::new(text, Lead::Bar, 0));
    }

    left.push(workers_seg(&style, data, p));

    // #2245: the external MCP servers, `mcp 3/4` (/mcp lists them).
    if let Some(m) = data.mcp {
        use amp_core::mcp_hub::HubLevel;
        let color = match m.level {
            HubLevel::Ok => c.muted,
            HubLevel::Degraded => c.warning,
            HubLevel::Down => c.error,
        };
        left.push(Seg::new(
            color.fg(&format!("mcp {}/{}", m.ready, m.total)),
            Lead::Bar,
            9,
        ));
    }

    // Plan approval.
    if meta.plan_required()
        && let Some(plan) = meta.plan.get()
        && !plan.awaiting_approval.is_empty()
    {
        let plan_link = |painted: String| match (&meta.instance, meta.project_id) {
            (Some(inst), Some(pid)) => link(&inst.plan_url(pid), &painted),
            _ => painted,
        };
        // #2171: the plan waits on the person at the keyboard; the chip links to the Flight plan
        // page where they approve it (`/open plan` opens it too). Their next step outranks the
        // workers count when the line is narrow (only the questions chip stays longer).
        // #2198: approval is per ticket. The chip counts the person's own tickets; others' wait
        // for their owners (or a lead), so they get a quieter segment that goes first.
        let yours = plan.yours().len();
        if yours > 0 {
            let text = if plan.per_ticket() {
                format!("plan: {yours} of yours waiting for your approval")
            } else {
                format!("plan: {yours} waiting for your approval")
            };
            let painted = style.paint(c.warning, &text, stale(&meta.plan));
            left.push(Seg::new(plan_link(painted), Lead::Bar, 2));
        }
        let others = plan.others().len();
        if others > 0 {
            let text = if yours > 0 {
                format!("{others} others' waiting for their owners")
            } else {
                format!("plan: {others} others' waiting for their owners' approval")
            };
            let painted = style.paint(c.muted, &text, stale(&meta.plan));
            left.push(Seg::new(plan_link(painted), Lead::Bar, 7));
        }
    }

    // Artifacts of the current ticket in review.
    let in_review = meta.artifacts_in_review();
    if let Some(first) = in_review.first() {
        let text = if in_review.len() == 1 {
            let t = c.info.fg(&format!("artifact #{} in review", first.id));
            match &meta.instance {
                Some(inst) => link(&inst.artifact_url(first.id), &t),
                None => t,
            }
        } else {
            c.info
                .fg(&format!("{} artifacts in review", in_review.len()))
        };
        left.push(Seg::new(text, Lead::Bar, 5));
    }

    // Board counts, in the web UI's words (the questions are the chip above).
    match board {
        Some(b) if b.nonzero().is_empty() => {
            left.push(Seg::new(
                style.paint(c.dim, "no open tickets", board_stale),
                Lead::Bar,
                10,
            ));
        }
        Some(b) => {
            let mut first = true;
            for (i, (status, n)) in b
                .nonzero()
                .into_iter()
                .filter(|(s, _)| *s != Status::Question)
                .enumerate()
            {
                let s = p.state(status);
                let text = if board_stale {
                    c.dim.fg(&format!("{} {n} {}", s.glyph, s.label))
                } else {
                    format!(
                        "{} {}",
                        s.mark.fg(&s.glyph),
                        s.text.fg(&format!("{n} {}", s.label))
                    )
                };
                let lead = if first { Lead::Bar } else { Lead::Dot };
                first = false;
                left.push(Seg::new(text, lead, 10 + i as u8));
            }
        }
        None if meta.project_id.is_some() => {
            let note = if meta.board.error.is_some() {
                "board unavailable"
            } else if disconnected(meta) {
                "board: not connected"
            } else {
                "board…"
            };
            left.push(Seg::new(c.dim.fg(note), Lead::Bar, 10));
        }
        None => left.push(Seg::new(c.dim.fg("no project"), Lead::Bar, 10)),
    }

    let right: Vec<Seg> = report_seg(&style, data).into_iter().collect();
    vec![fit_keeping(&style, left, right, width)]
}

/// `workers 1 ■ running · 1 queued · 1 ■ question`: the workers **this chat** runs (its
/// supervisor). Tickets in progress elsewhere (another chat, another person, or a run this chat
/// abandoned) are not workers of this chat: they stay in the board's `In progress` count, and
/// `/workers` lists them as "elsewhere" (#2204: a fresh chat used to count a dead ticket as a
/// running worker).
fn workers_seg(style: &Style, data: &ChromeData, p: &AmpPalette) -> Seg {
    let c = &style.colors;
    let meta = &data.meta;
    let board = meta.board.get();
    let questions: HashSet<i64> = board
        .map(|b| b.questions.iter().map(|q| q.id).collect())
        .unwrap_or_default();
    let mut running = 0;
    let mut queued = 0;
    let mut asking = 0;
    for w in &data.workers {
        if questions.contains(&w.task_id) {
            asking += 1;
        } else if w.phase == WorkerPhase::Queued {
            queued += 1;
        } else {
            running += 1;
        }
    }

    let is_stale = stale(&meta.board);
    let prog = p.state(Status::InProgress);
    let q = p.state(Status::Question);
    let mut parts = vec![c.muted.fg("workers")];
    if running + queued + asking == 0 {
        parts.push(style.paint(c.dim, "0", is_stale));
    } else {
        let mut groups = Vec::new();
        if running > 0 {
            groups.push(format!(
                "{} {} {}",
                style.paint(c.text, &running.to_string(), is_stale),
                style.paint(prog.mark, &prog.glyph, is_stale),
                style.paint(c.muted, "running", is_stale)
            ));
        }
        if queued > 0 {
            groups.push(style.paint(c.muted, &format!("{queued} queued"), is_stale));
        }
        if asking > 0 {
            groups.push(format!(
                "{} {} {}",
                style.paint(c.text, &asking.to_string(), is_stale),
                style.paint(q.mark, &q.glyph, is_stale),
                style.paint(q.text, "question", is_stale)
            ));
        }
        parts.push(groups.join(&c.dim.fg(" · ")));
    }
    Seg::new(parts.join(" "), Lead::Bar, 3)
}

/// `reported ✓ #id`, `report failed #id`, or `unreported #id` while the current ticket is in
/// progress and this session has not reported it.
fn report_seg(style: &Style, data: &ChromeData) -> Option<Seg> {
    let c = &style.colors;
    let meta = &data.meta;
    let current = meta.ticket_id;
    let (ticket, state) = match current.and_then(|id| data.reports.get(&id).map(|s| (id, Some(*s))))
    {
        Some(found) => found,
        None => {
            let in_progress = meta
                .ticket
                .get()
                .is_some_and(|t| t.status == Status::InProgress);
            match (current, data.last_report) {
                (Some(id), _) if in_progress => (id, None),
                (_, Some(id)) => (id, data.reports.get(&id).copied()),
                _ => return None,
            }
        }
    };
    let id = format!("#{ticket}");
    let id = match (&meta.instance, meta.project_id) {
        (Some(inst), Some(pid)) => link(&inst.ticket_url(pid, ticket), &id),
        _ => id,
    };
    let text = match state {
        Some(ReportState::Reported) => format!("{} {id}", c.success.fg("reported ✓")),
        Some(ReportState::Failed) => format!("{} {id}", c.error.fg("report failed")),
        None => format!("{} {id}", c.warning.fg("unreported")),
    };
    Some(Seg::new(text, Lead::Space, 5))
}

/// A link helper for callers that hold an [`Instance`]: the ticket's board drawer.
pub fn ticket_link(inst: &Instance, project_id: i64, ticket_id: i64, text: &str) -> String {
    link(&inst.ticket_url(project_id, ticket_id), text)
}

/// A representative chrome for `amp-harness debug render-chrome` and the render tests: an
/// in-progress current ticket, two questions, plan approval pending, workers here and elsewhere.
pub mod sample {
    use std::{
        path::PathBuf,
        time::{Duration, Instant, SystemTime},
    };

    use amp_core::{
        events::Totals,
        meta::{
            AmpMeta, Board, BoardTicket, Connection, Fetched, Instance, MetaHealth, Named, Org,
            PlanApproval, Project, Question, Ticket, User,
        },
        supervisor::{WorkerPhase, WorkerStatus},
        tickets::Status,
    };

    use super::ChromeData;

    fn ok<T>(value: T) -> Fetched<T> {
        Fetched {
            value: Some(value),
            updated: Some(Instant::now()),
            error: None,
        }
    }

    fn in_progress(id: i64, name: &str) -> BoardTicket {
        BoardTicket {
            id,
            name: name.to_owned(),
            assigned_to: Some("amp:amp-worker-backend".to_owned()),
            epic: "AMP Harness".to_owned(),
        }
    }

    /// The sample metadata. `ticket`: whether #2106 is the current ticket.
    pub fn meta(ticket: bool) -> AmpMeta {
        AmpMeta {
            revision: 1,
            instance: Some(Instance {
                mcp_url: "https://amp.simstech.cloud/mcp".to_owned(),
                web_base: "https://amp.simstech.cloud".to_owned(),
                protocol: "2026-07-28".to_owned(),
                server_name: Some("amp".to_owned()),
                server_version: Some("2.0.0".to_owned()),
                auth: "oauth".to_owned(),
            }),
            health: MetaHealth {
                connection: Connection::Ok,
                latency: Some(Duration::from_millis(4)),
                last_ok: Some(Instant::now()),
                last_call: Some(Instant::now()),
                calls: 6,
            },
            user: ok(User {
                user_id: 7,
                email: "ada@example.com".to_owned(),
                account: "human".to_owned(),
                via_api_token: false,
                global_admin: false,
                orgs: vec![Org {
                    id: 3,
                    name: "simstech".to_owned(),
                    slug: "simstech".to_owned(),
                }],
            }),
            project_id: Some(2),
            project: ok(Project {
                id: 2,
                name: "AMP".to_owned(),
                code: "amp".to_owned(),
                state: "active".to_owned(),
                org_id: Some(3),
                require_plan_approval: true,
            }),
            board: ok(Board {
                in_progress: 3,
                ready: 5,
                question: 2,
                scheduled: 1,
                waiting: 4,
                done: 30,
                questions: vec![
                    Question {
                        id: 2107,
                        name: "M3-4 Slash commands".to_owned(),
                        question: "Overlay or inline?".to_owned(),
                    },
                    Question {
                        id: 2110,
                        name: "M3-6 Docs".to_owned(),
                        question: "README or KB?".to_owned(),
                    },
                ],
                in_progress_tickets: vec![
                    in_progress(2104, "M3-2 AMP metadata poller"),
                    in_progress(2105, "M3-5 Inline ticket chips"),
                    in_progress(2106, "M3-3 Header and AMP status line"),
                ],
                quiet: Vec::new(),
                waiting_on_people: 1,
            }),
            ticket_id: ticket.then_some(2106),
            ticket: if ticket {
                ok(Ticket {
                    id: 2106,
                    project_id: 2,
                    name: "M3-3 Header and AMP status line".to_owned(),
                    state: "in_progress".to_owned(),
                    block_reason: None,
                    status: Status::InProgress,
                    assigned_to: Some("amp:amp-worker-backend".to_owned()),
                    dispatched_at: Some("2026-10-22T17:54:26Z".to_owned()),
                    epic: Some(Named {
                        id: 161,
                        name: "AMP Harness".to_owned(),
                    }),
                    story: Some(Named {
                        id: 393,
                        name: "M3 theme + metadata".to_owned(),
                    }),
                    epic_index: Some(1),
                })
            } else {
                Fetched::default()
            },
            plan: ok(PlanApproval {
                require_plan_approval: true,
                awaiting_approval: vec![2108, 2109],
                plan_approved_at: None,
                approve_url: Some("https://amp.simstech.cloud/project/2/plan".into()),
                tickets: None,
            }),
            artifacts: ok(Vec::new()),
        }
    }

    /// A live worker of this chat on `task_id`.
    pub fn worker(task_id: i64, phase: WorkerPhase) -> WorkerStatus {
        WorkerStatus {
            task_id,
            agent_id: "amp:amp-worker-backend".to_owned(),
            phase,
            model: Some("claude-opus-5-5".to_owned()),
            cwd: Some(PathBuf::from(format!(".worktrees/{task_id}"))),
            since: SystemTime::UNIX_EPOCH,
            totals: Totals::default(),
        }
    }

    /// The sample chrome: [`meta`] plus two local workers (#2105 running, #2111 queued).
    pub fn data(ticket: bool) -> ChromeData {
        let mut d = ChromeData::new(meta(ticket));
        d.workers = vec![
            worker(2105, WorkerPhase::Running),
            worker(2111, WorkerPhase::Queued),
        ];
        d
    }
}
