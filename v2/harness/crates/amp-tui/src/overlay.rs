//! The AMP command panels (M3-4, #2108): what `/amp`, `/ticket`, `/workers`, `/questions`,
//! `/plan` and `/project` show, docked between the transcript and the editor. `/model` uses the
//! same panel as a picker (#2188): Enter switches to the selected model.
//!
//! The configuration panels (#2245: `/mcp`, `/agents`, `/skills`, `/model roles`, `/config`) are
//! [`PanelKind::Info`] panels: the harness hands over an [`InfoPanel`] whose rows it builds (from
//! live state, at every render, so `/mcp` follows the servers' health) and whose actions are slash
//! commands: Enter and the letter keys run `"/mcp restart {}"`-style commands with `{}` the
//! selected row's key, so a key and the typed command take the same path.
//!
//! ```text
//! ── AMP · questions ───────────────────────────────────────────────────────────
//!  › ■  QUESTION  #2107 M3-5 Inline ticket chips
//!       Dark or light?
//!    ■  QUESTION  #2110 M3-6 Docs
//!       README or KB?
//!  ↑↓ select · Enter answer · o open in browser · Esc close
//! ```
//!
//! The panel is part of the layout (not a floating TUI overlay), so the headless renders that the
//! tests and `--snapshot` use show it exactly as the terminal does. Its rows are built from the
//! chrome's data at every render, so a panel follows the metadata poller live (a `/ticket 2108`
//! panel fills in when the ticket loads).
//!
//! Text from AMP goes through [`chrome::clean`]; links come only from [`Instance`]'s helpers
//! ([`chrome::link`]); a row is laid out from whole styled pieces plus at most one plain piece that
//! is shortened before it is styled, so a cut never lands inside an escape sequence or a link.

use std::{
    any::Any,
    collections::{HashMap, HashSet},
    sync::{Arc, Mutex},
    time::{Duration, SystemTime},
};

use amp_core::{
    meta::{AmpMeta, Connection, HealthLevel, Instance},
    supervisor::WorkerPhase,
    tickets::Status,
};
use rpi_tui::{Component, theme::Color, visible_width};
use serde_json::Value;

use crate::{
    chrome::{self, Chrome, ChromeData, ReportState, clean, link, shorten},
    theme::{AmpPalette, palette},
};

/// Rows shown at once; the list scrolls to keep the selection visible.
pub const MAX_ROWS: usize = 14;
/// Rows a configuration panel shows at once (#2245); ↓ past its last selectable row scrolls on.
pub const MAX_INFO_ROWS: usize = 18;

/// Which panel is open.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PanelKind {
    /// `/amp`: the connection, user, instance and how to log in again.
    Amp,
    /// `/ticket`: the current ticket.
    Ticket,
    /// `/workers`: local workers and in-progress tickets elsewhere.
    Workers,
    /// `/questions`: tickets waiting on a person; Enter answers one.
    Questions,
    /// `/plan`: the project's plan tree (`amp_get_plan`) and plan approval.
    Plan,
    /// `/project`: the project.
    Project,
    /// `/model`: the models this chat can switch to; Enter switches (no AMP data needed).
    Model,
    /// A configuration panel the harness describes ([`InfoPanel`], #2245): `/mcp`, `/agents`,
    /// `/skills`, `/model roles`, `/config`. No AMP data needed.
    Info,
}

impl PanelKind {
    pub fn title(self) -> &'static str {
        match self {
            Self::Amp => "AMP · connection",
            Self::Ticket => "AMP · ticket",
            Self::Workers => "AMP · workers",
            Self::Questions => "AMP · questions",
            Self::Plan => "AMP · plan",
            Self::Project => "AMP · project",
            Self::Model => "model",
            Self::Info => "settings",
        }
    }
}

/// Where `/questions` is in answering one.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Stage {
    /// Browsing the rows.
    Browse,
    /// The person types the answer in the editor; Enter takes it to [`Stage::Confirm`].
    Answer { ticket: i64, question: String },
    /// Shows exactly what will be sent; `y`/Enter sends it with `amp_answer_question`.
    Confirm {
        ticket: i64,
        question: String,
        answer: String,
    },
    /// The answer is on its way.
    Sending { ticket: i64 },
}

/// The open panel.
#[derive(Debug, Clone)]
pub struct PanelState {
    pub kind: PanelKind,
    pub selected: usize,
    pub stage: Stage,
    /// `/plan`'s `amp_get_plan` result (`None` while it loads).
    pub plan: Option<Result<Value, String>>,
    /// `/plan`'s blocked tickets (`amp_list_tasks(owner=any, state=blocked, format=full)`), which
    /// split the plan's stored `blocked` counts into Question, Scheduled and Waiting.
    pub plan_blocked: Option<Result<Value, String>>,
    /// A one-line note at the bottom (`answered #2107`, an error).
    pub note: Option<(String, bool)>,
    /// `/model`'s choices, in the order shown.
    pub models: Vec<ModelRow>,
    /// [`PanelKind::Info`]'s content.
    pub info: Option<Arc<InfoPanel>>,
    /// Lines scrolled past the selection (a configuration panel longer than it shows).
    pub scroll: usize,
}

/// One model in the `/model` picker.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ModelRow {
    /// `provider/model`, what `/model <reference>` takes.
    pub reference: String,
    /// The session's model now.
    pub current: bool,
    pub context_window: u64,
    /// Whether the model thinks (has thinking levels).
    pub reasoning: bool,
}

impl PanelState {
    pub fn new(kind: PanelKind) -> Self {
        Self {
            kind,
            selected: 0,
            stage: Stage::Browse,
            plan: None,
            plan_blocked: None,
            note: None,
            models: Vec::new(),
            info: None,
            scroll: 0,
        }
    }

    /// A configuration panel ([`PanelKind::Info`]); the row whose key is `select` is selected.
    pub fn info(panel: Arc<InfoPanel>, select: Option<&str>) -> Self {
        let selected = select
            .and_then(|key| {
                (panel.rows)()
                    .iter()
                    .filter(|r| r.key.is_some())
                    .position(|r| r.key.as_deref() == Some(key))
            })
            .unwrap_or(0);
        Self {
            selected,
            info: Some(panel),
            ..Self::new(PanelKind::Info)
        }
    }

    /// The panel's title rule text.
    pub fn title(&self) -> String {
        match &self.info {
            Some(info) if self.kind == PanelKind::Info => info.title.clone(),
            _ => self.kind.title().to_owned(),
        }
    }

    /// The `/model` picker over `models`, with the current one selected.
    pub fn models(models: Vec<ModelRow>) -> Self {
        let selected = models.iter().position(|m| m.current).unwrap_or(0);
        Self {
            selected,
            models,
            ..Self::new(PanelKind::Model)
        }
    }
}

// ------------------------------------------------------------------------------------------------
// Configuration panels (#2245)
// ------------------------------------------------------------------------------------------------

/// How a cell of an [`InfoRow`] is coloured (the theme's own colours).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Tone {
    /// Ink: names, values.
    Text,
    /// Secondary detail.
    Muted,
    /// Labels and hints.
    Dim,
    Accent,
    Success,
    Warning,
    Error,
}

/// One piece of an [`InfoRow`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Cell {
    pub text: String,
    pub tone: Tone,
    /// Shortened to the room left (at most one per row; the rest are kept whole or dropped).
    pub flex: bool,
    /// Padded with spaces to this many cells (a column), when it fits.
    pub width: Option<usize>,
    /// A note about the value before it (where it was set): drawn after the value when the row
    /// fits, else on its own line under the value, so the value is never shortened for it (#2351).
    pub note: bool,
}

impl Cell {
    pub fn new(text: impl Into<String>, tone: Tone) -> Self {
        Self {
            text: text.into(),
            tone,
            flex: false,
            width: None,
            note: false,
        }
    }

    /// A note about the row's value (`(config ~/.config/…)`): it moves to its own line under the
    /// value rather than shortening it when the row is too wide (#2351).
    pub fn note(text: impl Into<String>, tone: Tone) -> Self {
        Self {
            note: true,
            ..Self::new(text, tone)
        }
    }

    /// A cell that is shortened when the row is too wide.
    pub fn flex(text: impl Into<String>, tone: Tone) -> Self {
        Self {
            flex: true,
            ..Self::new(text, tone)
        }
    }

    /// Pads the cell to `width` cells (a column).
    pub fn pad(mut self, width: usize) -> Self {
        self.width = Some(width);
        self
    }
}

/// One line of an [`InfoPanel`].
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct InfoRow {
    pub cells: Vec<Cell>,
    /// Selectable rows have a key: what `{}` stands for in the panel's commands.
    pub key: Option<String>,
    /// Which of the panel's letter keys apply to this row (`None`: all of them).
    pub keys: Option<Vec<char>>,
    /// Indent in cells. An indented row with one flex cell is wrapped text.
    pub indent: usize,
    /// A row of one flex cell that wraps at word boundaries instead of being shortened.
    pub wrap: bool,
}

impl InfoRow {
    pub fn new(cells: Vec<Cell>) -> Self {
        Self {
            cells,
            ..Self::default()
        }
    }

    /// A row of wrapped text, indented by `indent`.
    pub fn text(text: impl Into<String>, tone: Tone, indent: usize) -> Self {
        Self {
            cells: vec![Cell::flex(text, tone)],
            indent,
            wrap: true,
            ..Self::default()
        }
    }

    /// `label  value`: a dim label padded to `width`, the value shortened to fit.
    pub fn field(label: &str, width: usize, value: impl Into<String>, tone: Tone) -> Self {
        Self::new(vec![
            Cell::new(label, Tone::Dim).pad(width),
            Cell::flex(value, tone),
        ])
    }

    /// Makes the row selectable; `{}` in the panel's commands is `key`.
    pub fn key(mut self, key: impl Into<String>) -> Self {
        self.key = Some(key.into());
        self
    }

    /// Only these letter keys apply to this row.
    pub fn keys(mut self, keys: &[char]) -> Self {
        self.keys = Some(keys.to_vec());
        self
    }

    pub fn indent(mut self, n: usize) -> Self {
        self.indent = n;
        self
    }
}

/// A letter key of an [`InfoPanel`]: runs `command` (with `{}` the selected row's key).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PanelKey {
    pub key: char,
    /// What the hint says (`restart`).
    pub label: String,
    pub command: String,
}

impl PanelKey {
    pub fn new(key: char, label: &str, command: &str) -> Self {
        Self {
            key,
            label: label.to_owned(),
            command: command.to_owned(),
        }
    }
}

/// The rows of an [`InfoPanel`], built at every render.
pub type RowsFn = Arc<dyn Fn() -> Vec<InfoRow> + Send + Sync>;

/// A configuration panel the harness describes (#2245). Rows come from `rows` at every render
/// (live state); Enter runs `enter` and a letter key runs its [`PanelKey::command`] for the
/// selected row, as slash commands.
pub struct InfoPanel {
    pub title: String,
    pub rows: RowsFn,
    /// Enter: (hint word, command), `{}` the selected row's key.
    pub enter: Option<(String, String)>,
    pub keys: Vec<PanelKey>,
}

impl std::fmt::Debug for InfoPanel {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("InfoPanel")
            .field("title", &self.title)
            .field("enter", &self.enter)
            .field("keys", &self.keys)
            .finish_non_exhaustive()
    }
}

impl InfoPanel {
    /// A panel with fixed rows.
    pub fn fixed(title: impl Into<String>, rows: Vec<InfoRow>) -> Self {
        Self::live(title, Arc::new(move || rows.clone()))
    }

    /// A panel whose rows are rebuilt at every render.
    pub fn live(title: impl Into<String>, rows: RowsFn) -> Self {
        Self {
            title: title.into(),
            rows,
            enter: None,
            keys: Vec::new(),
        }
    }

    /// Enter runs `command` (hint: `word`).
    pub fn on_enter(mut self, word: &str, command: &str) -> Self {
        self.enter = Some((word.to_owned(), command.to_owned()));
        self
    }

    pub fn with_key(mut self, key: char, label: &str, command: &str) -> Self {
        self.keys.push(PanelKey::new(key, label, command));
        self
    }

    /// The letter keys that apply to `row`.
    pub fn keys_for(&self, row: &InfoRow) -> Vec<&PanelKey> {
        self.keys
            .iter()
            .filter(|k| row.keys.as_ref().is_none_or(|only| only.contains(&k.key)))
            .collect()
    }
}

/// `command` with `{}` replaced by `key`.
pub fn fill_command(command: &str, key: &str) -> String {
    command.replace("{}", key)
}

/// The panel slot's shared state. Clones share it; the component reads it at every render.
#[derive(Clone)]
pub struct Panel {
    state: Arc<Mutex<Option<PanelState>>>,
    chrome: Chrome,
    /// The width the panel was last drawn at, so scrolling counts the lines it really takes
    /// (notes under values and wrapped hints add lines at narrow widths, #2351). 0: not drawn yet.
    width: Arc<std::sync::atomic::AtomicUsize>,
}

impl Panel {
    pub fn new(chrome: Chrome) -> Self {
        Self {
            state: Arc::default(),
            chrome,
            width: Arc::default(),
        }
    }

    pub fn open(&self, state: PanelState) {
        *self.lock() = Some(state);
    }

    pub fn close(&self) {
        *self.lock() = None;
    }

    pub fn is_open(&self) -> bool {
        self.lock().is_some()
    }

    /// The open panel's state (a copy).
    pub fn state(&self) -> Option<PanelState> {
        self.lock().clone()
    }

    /// Changes the open panel's state; returns false when no panel is open.
    pub fn update(&self, f: impl FnOnce(&mut PanelState)) -> bool {
        match self.lock().as_mut() {
            Some(s) => {
                f(s);
                true
            }
            None => false,
        }
    }

    /// The open panel's rows for the current data (what the keys act on).
    pub fn rows(&self) -> Vec<Row> {
        let Some(state) = self.state() else {
            return Vec::new();
        };
        let data = self.chrome.data();
        build_rows(&state, data.as_ref(), &palette())
    }

    /// The selectable rows' targets, in order.
    pub fn targets(&self) -> Vec<Target> {
        self.rows().into_iter().filter_map(|r| r.target).collect()
    }

    /// Moves the selection by `delta` among the selectable rows. In a configuration panel, ↓ at
    /// the last selectable row scrolls the rest into view, and ↑ scrolls back first.
    pub fn move_selection(&self, delta: isize) {
        let rows = self.rows();
        let n = rows.iter().filter(|r| r.target.is_some()).count();
        // Never further than there are lines below the selection: the lines as last drawn, else
        // the rows (wrapped rows may then need a few more presses, never an endless run back up).
        let width = self.width.load(std::sync::atomic::Ordering::Relaxed);
        let lines = match self.state() {
            Some(state) if width > 0 => {
                let data = self.chrome.data();
                layout(&state, data.as_ref(), &palette(), width).0.len()
            }
            _ => rows.len(),
        };
        let room = lines.saturating_sub(MAX_INFO_ROWS.min(lines));
        self.update(|s| {
            let info = s.kind == PanelKind::Info;
            if info && delta < 0 && s.scroll > 0 {
                s.scroll = s.scroll.saturating_sub(delta.unsigned_abs());
                return;
            }
            if n == 0 {
                s.selected = 0;
                if info && delta > 0 {
                    s.scroll = (s.scroll + delta as usize).min(room + 2);
                }
                return;
            }
            let last = n - 1;
            let at = s.selected.min(last) as isize + delta;
            if info && at > last as isize {
                s.scroll = (s.scroll + (at - last as isize) as usize).min(room + 2);
            }
            s.selected = at.clamp(0, last as isize) as usize;
        });
    }

    /// The selected row's target.
    pub fn selected(&self) -> Option<Target> {
        let state = self.state()?;
        let targets = self.targets();
        targets
            .get(state.selected.min(targets.len().saturating_sub(1)))
            .cloned()
    }

    /// The component for the panel slot.
    pub fn component(&self) -> Arc<dyn Component> {
        Arc::new(PanelView(self.clone()))
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Option<PanelState>> {
        match self.state.lock() {
            Ok(g) => g,
            Err(poisoned) => poisoned.into_inner(),
        }
    }
}

struct PanelView(Panel);

impl Component for PanelView {
    fn render(&self, width: usize) -> Vec<String> {
        let Some(state) = self.0.state() else {
            return Vec::new();
        };
        self.0
            .width
            .store(width, std::sync::atomic::Ordering::Relaxed);
        let data = self.0.chrome.data();
        render_panel(&state, data.as_ref(), &palette(), width)
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

// ------------------------------------------------------------------------------------------------
// Rows
// ------------------------------------------------------------------------------------------------

/// What a selectable row points at.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Target {
    /// A ticket: `o` opens it; in `/questions`, Enter answers it.
    Ticket { id: i64, question: Option<String> },
    /// A web page from [`Instance`]'s helpers (the project's Mission page, AMP itself).
    Page(String),
    /// A model in `/model` (`provider/model`): Enter switches to it.
    Model(String),
    /// A row of a configuration panel ([`InfoRow::key`]).
    Item(String),
}

/// One piece of a row.
#[derive(Debug, Clone)]
pub enum Piece {
    /// Styled text drawn whole (or dropped when it can't fit).
    Fixed(String),
    /// Plain text shortened to the room left, then drawn in `Color`.
    Flex(String, Color),
    /// Plain text about the value before it: kept on the row when everything fits, else drawn
    /// on a line of its own under the value (see [`render_panel`]).
    Note(String, Color),
}

/// One line of a panel.
#[derive(Debug, Clone, Default)]
pub struct Row {
    pub pieces: Vec<Piece>,
    pub target: Option<Target>,
    /// Indent in cells (continuation lines).
    pub indent: usize,
    /// A single flex piece wraps at word boundaries (as an indented one does).
    pub wrap: bool,
}

impl Row {
    fn new(pieces: Vec<Piece>) -> Self {
        Self {
            pieces,
            ..Self::default()
        }
    }

    fn target(mut self, t: Target) -> Self {
        self.target = Some(t);
        self
    }

    fn indent(mut self, n: usize) -> Self {
        self.indent = n;
        self
    }
}

fn fixed(s: String) -> Piece {
    Piece::Fixed(s)
}

fn flex(s: &str, c: Color) -> Piece {
    Piece::Flex(clean(s), c)
}

/// Lays `pieces` out in `width` cells: pieces joined by one space; the first [`Piece::Flex`] is
/// shortened to the room left; fixed pieces that still don't fit are dropped from the end.
/// Never wider than `width`.
pub fn fit_pieces(pieces: &[Piece], width: usize) -> String {
    let fixed_w: usize = pieces
        .iter()
        .map(|p| match p {
            Piece::Fixed(s) | Piece::Note(s, _) => visible_width(s),
            Piece::Flex(..) => 0,
        })
        .sum();
    let gaps = pieces.len().saturating_sub(1);
    let room = width.saturating_sub(fixed_w + gaps);
    let mut out = String::new();
    let mut used = 0;
    let mut flex_done = false;
    for p in pieces {
        let (text, w) = match p {
            Piece::Fixed(s) => (s.clone(), visible_width(s)),
            Piece::Note(s, c) => (c.fg(s), visible_width(s)),
            Piece::Flex(s, c) if !flex_done => {
                flex_done = true;
                let short = shorten_value(s, room);
                if short.is_empty() {
                    continue;
                }
                let w = visible_width(&short);
                (c.fg(&short), w)
            }
            Piece::Flex(s, c) => {
                let w = visible_width(s);
                (c.fg(s), w)
            }
        };
        let gap = usize::from(used > 0);
        if used + gap + w > width {
            break;
        }
        if gap == 1 {
            out.push(' ');
        }
        out.push_str(&text);
        used += gap + w;
    }
    out
}

/// Whether `pieces` fit `width` cells with every piece whole (one space between pieces).
fn fits_whole(pieces: &[Piece], width: usize) -> bool {
    let w: usize = pieces
        .iter()
        .map(|p| match p {
            Piece::Fixed(s) | Piece::Flex(s, _) | Piece::Note(s, _) => visible_width(s),
        })
        .sum();
    w + pieces.len().saturating_sub(1) <= width
}

/// The column the first [`Piece::Flex`] starts at when `pieces` are laid out whole.
fn flex_column(pieces: &[Piece]) -> usize {
    let mut col = 0;
    for p in pieces {
        match p {
            Piece::Flex(..) => return col,
            Piece::Fixed(s) | Piece::Note(s, _) => col += visible_width(s) + 1,
        }
    }
    0
}

/// A row with a [`Piece::Note`] laid out in `width` cells (#2351): one line when everything
/// fits; otherwise the row without its notes, then each note on its own line, lined up under the
/// value (or two cells in when that leaves too little room), shortened only if it must be.
pub fn fit_with_notes(pieces: &[Piece], width: usize) -> Vec<String> {
    if fits_whole(pieces, width) || !pieces.iter().any(|p| matches!(p, Piece::Note(..))) {
        return vec![fit_pieces(pieces, width)];
    }
    let main: Vec<Piece> = pieces
        .iter()
        .filter(|p| !matches!(p, Piece::Note(..)))
        .cloned()
        .collect();
    let mut col = flex_column(&main);
    let mut out = vec![fit_pieces(&main, width)];
    for p in pieces {
        if let Piece::Note(s, c) = p {
            if col + visible_width(s) > width && col > 2 {
                col = 2.min(width);
            }
            let room = width.saturating_sub(col);
            if room > 0 {
                out.push(format!("{}{}", " ".repeat(col), c.fg(&shorten(s, room))));
            }
        }
    }
    out
}

/// `text` shortened to `max` cells. A path (`/…`, `~/…`, `<repo>/…`, no spaces) keeps its tail,
/// the directory or file it names, behind a `…` (#2351); anything else is cut at the end.
pub fn shorten_value(text: &str, max: usize) -> String {
    let is_path = !text.contains(' ')
        && (text.starts_with('/') || text.starts_with("~/") || text.starts_with("<repo>/"));
    if !is_path || visible_width(text) <= max || max < 8 {
        return shorten(text, max);
    }
    // The longest tail that fits after `…`, starting at a `/` when one fits.
    let chars: Vec<char> = text.chars().collect();
    let mut start = chars.len();
    let mut used = 0;
    while start > 0 {
        let mut buf = [0; 4];
        let w = visible_width(chars[start - 1].encode_utf8(&mut buf));
        if used + w > max - 1 {
            break;
        }
        used += w;
        start -= 1;
    }
    let tail: String = chars[start..].iter().collect();
    let tail = match tail.find('/') {
        Some(i) if i > 0 && tail.len() - i > 1 => &tail[i..],
        _ => tail.as_str(),
    };
    format!("…{tail}")
}

/// A key hint (`↑↓ select · Enter use · s save · Esc close`) in rows of at most `width` cells,
/// broken between its ` · ` parts so no key is cut (#2351). A part wider than a row is shortened.
pub fn wrap_hint(hint: &str, width: usize) -> Vec<String> {
    let width = width.max(1);
    let mut rows: Vec<String> = Vec::new();
    let mut cur = String::new();
    for part in hint.split(" · ") {
        let part = shorten(part, width);
        if cur.is_empty() {
            cur = part;
        } else if visible_width(&cur) + 3 + visible_width(&part) <= width {
            cur.push_str(" · ");
            cur.push_str(&part);
        } else {
            rows.push(std::mem::take(&mut cur));
            cur = part;
        }
    }
    rows.push(cur);
    rows
}

/// Plain text wrapped at word boundaries to `width` cells (long words are cut).
pub fn wrap_plain(text: &str, width: usize) -> Vec<String> {
    let width = width.max(1);
    let mut lines = Vec::new();
    for para in text.split('\n') {
        let mut line = String::new();
        for word in clean(para).split(' ') {
            if word.is_empty() {
                continue;
            }
            let lw = visible_width(&line);
            let ww = visible_width(word);
            if lw > 0 && lw + 1 + ww <= width {
                line.push(' ');
                line.push_str(word);
                continue;
            }
            if lw > 0 {
                lines.push(std::mem::take(&mut line));
            }
            if ww <= width {
                line.push_str(word);
            } else {
                let mut rest = word.to_owned();
                while visible_width(&rest) > width {
                    let cut = shorten_exact(&rest, width);
                    rest = rest[cut.len()..].to_owned();
                    lines.push(cut);
                }
                line = rest;
            }
        }
        lines.push(line);
    }
    lines
}

/// The longest prefix of `text` that fits `width` cells (no ellipsis).
fn shorten_exact(text: &str, width: usize) -> String {
    let mut out = String::new();
    for c in text.chars() {
        let mut buf = [0; 4];
        if visible_width(&out) + visible_width(c.encode_utf8(&mut buf)) > width {
            break;
        }
        out.push(c);
    }
    if out.is_empty() {
        text.chars().take(1).collect()
    } else {
        out
    }
}

fn colors() -> rpi_tui::theme::ThemeColors {
    rpi_tui::theme().colors
}

fn label(text: &str) -> Piece {
    fixed(colors().dim.fg(text))
}

/// `#id` in bold, linked to the ticket's board drawer when the instance and project are known.
fn ticket_id(meta: &AmpMeta, id: i64) -> String {
    let c = colors();
    let text = rpi_tui::bold(&c.text.fg(&format!("#{id}")));
    match (&meta.instance, meta.project_id) {
        (Some(inst), Some(pid)) => link(&inst.ticket_url(pid, id), &text),
        _ => text,
    }
}

/// The state mark as the web UI shows it (`■ In progress`, the `QUESTION` chip, ...).
fn state_mark(p: &AmpPalette, status: Status) -> String {
    p.state_mark(status)
}

fn ticket_target(id: i64) -> Target {
    Target::Ticket { id, question: None }
}

fn build_rows(state: &PanelState, data: Option<&ChromeData>, p: &AmpPalette) -> Vec<Row> {
    let c = colors();
    if state.kind == PanelKind::Model {
        return model_rows(state);
    }
    if state.kind == PanelKind::Info {
        return info_rows(state);
    }
    let Some(data) = data else {
        return vec![Row::new(vec![flex(
            "no AMP metadata: the chat is not connected to AMP's metadata poller",
            c.dim,
        )])];
    };
    match state.kind {
        PanelKind::Amp => amp_rows(data),
        PanelKind::Ticket => ticket_rows(data, p),
        PanelKind::Workers => worker_rows(data, p),
        PanelKind::Questions => question_rows(state, data, p),
        PanelKind::Plan => plan_rows(state, data, p),
        PanelKind::Project => project_rows(data, p),
        PanelKind::Model => model_rows(state),
        PanelKind::Info => info_rows(state),
    }
}

/// A configuration panel's rows as [`Row`]s: each cell styled whole, the flex one shortened.
fn info_rows(state: &PanelState) -> Vec<Row> {
    let c = colors();
    let Some(info) = &state.info else {
        return Vec::new();
    };
    let rows = (info.rows)();
    if rows.is_empty() {
        return vec![Row::new(vec![flex("nothing to show", c.dim)])];
    }
    rows.into_iter()
        .map(|r| {
            let pieces = r
                .cells
                .iter()
                .map(|cell| {
                    let color = tone_color(cell.tone);
                    let mut text = clean(&cell.text);
                    if let Some(w) = cell.width {
                        let have = visible_width(&text);
                        if have < w {
                            text.push_str(&" ".repeat(w - have));
                        }
                    }
                    if cell.note {
                        Piece::Note(text, color)
                    } else if cell.flex {
                        Piece::Flex(text, color)
                    } else {
                        Piece::Fixed(color.fg(&text))
                    }
                })
                .collect();
            let mut row = Row::new(pieces).indent(r.indent);
            row.wrap = r.wrap;
            if let Some(key) = r.key {
                row = row.target(Target::Item(key));
            }
            row
        })
        .collect()
}

fn tone_color(tone: Tone) -> Color {
    let c = colors();
    match tone {
        Tone::Text => c.text,
        Tone::Muted => c.muted,
        Tone::Dim => c.dim,
        Tone::Accent => c.accent,
        Tone::Success => c.success,
        Tone::Warning => c.warning,
        Tone::Error => c.error,
    }
}

/// `/model`: one selectable row per model, `● provider/model  current · 1M ctx · thinking`.
/// The reference is the flex piece, so a long name is shortened rather than wrapped.
fn model_rows(state: &PanelState) -> Vec<Row> {
    let c = colors();
    if state.models.is_empty() {
        return vec![Row::new(vec![flex(
            "no other models: add a provider with /connect",
            c.dim,
        )])];
    }
    state
        .models
        .iter()
        .map(|m| {
            let mark = if m.current {
                c.success.fg("●")
            } else {
                c.dim.fg("○")
            };
            let mut detail = Vec::new();
            if m.current {
                detail.push(c.success.fg("current"));
            }
            if m.context_window > 0 {
                detail.push(c.dim.fg(&format!(
                    "{} ctx",
                    crate::footer::format_tokens(m.context_window as i64)
                )));
            }
            if m.reasoning {
                detail.push(c.dim.fg("thinking"));
            }
            let mut pieces = vec![fixed(mark), flex(&m.reference, c.text)];
            if !detail.is_empty() {
                pieces.push(fixed(detail.join(&c.dim.fg(" · "))));
            }
            Row::new(pieces).target(Target::Model(m.reference.clone()))
        })
        .collect()
}

fn health_word(c: &Connection) -> String {
    c.to_string()
}

fn ago(at: Option<std::time::Instant>) -> String {
    match at {
        Some(at) => format!("{} s ago", at.elapsed().as_secs()),
        None => "never".to_owned(),
    }
}

fn amp_rows(data: &ChromeData) -> Vec<Row> {
    let c = colors();
    let m = &data.meta;
    let mut rows = Vec::new();
    match &m.instance {
        Some(inst) => {
            let host = link(&inst.web_base, &c.text.fg(&clean(inst.host())));
            rows.push(
                Row::new(vec![
                    label("instance"),
                    fixed(host),
                    flex(&format!("({})", inst.mcp_url), c.dim),
                ])
                .target(Target::Page(inst.web_base.clone())),
            );
            let server = format!(
                "{} {}",
                inst.server_name.as_deref().unwrap_or("?"),
                inst.server_version.as_deref().unwrap_or("")
            );
            rows.push(Row::new(vec![
                label("server  "),
                flex(
                    &format!(
                        "{} · protocol {} · auth {}",
                        server.trim(),
                        inst.protocol,
                        inst.auth
                    ),
                    c.muted,
                ),
            ]));
        }
        None => rows.push(Row::new(vec![
            label("instance"),
            flex("connecting…", c.dim),
        ])),
    }
    match m.user.get() {
        Some(u) => {
            let mut text = format!("{} · {}", u.email, u.account);
            if u.via_api_token {
                text.push_str(" · API token");
            }
            if u.global_admin {
                text.push_str(" · admin");
            }
            rows.push(Row::new(vec![label("user    "), flex(&text, c.text)]));
        }
        None => rows.push(Row::new(vec![
            label("user    "),
            flex(m.user.error.as_deref().unwrap_or("loading…"), c.dim),
        ])),
    }
    let dot = match m.health.connection.level() {
        HealthLevel::Ok => c.success,
        HealthLevel::Degraded => c.warning,
        HealthLevel::Down => c.error,
    };
    let latency = m
        .health
        .latency
        .map_or("-".to_owned(), |l| format!("{} ms", l.as_millis()));
    rows.push(Row::new(vec![
        label("health  "),
        fixed(dot.fg("●")),
        flex(
            &format!(
                "{} · last call {latency} · {} calls · last answer {}",
                health_word(&m.health.connection),
                m.health.calls,
                ago(m.health.last_ok)
            ),
            c.muted,
        ),
    ]));
    rows.push(project_line(m));
    match &m.health.connection {
        Connection::AuthRequired => rows.push(Row::new(vec![flex(
            "re-login needed: run `amp-harness login` in another terminal; the header and the agent's AMP tools reconnect within 15 s (with AMP_TOKEN, set a new one and restart the chat)",
            c.warning,
        )])),
        Connection::Down { .. } => rows.push(Row::new(vec![flex(
            "disconnected: the poller reconnects every 15 s",
            c.warning,
        )])),
        _ => {}
    }
    rows
}

fn project_line(m: &AmpMeta) -> Row {
    let c = colors();
    let Some(pid) = m.project_id else {
        return Row::new(vec![
            label("project "),
            flex("none (no .amp.json; /project <id> picks one)", c.dim),
        ]);
    };
    let name = m.project.get().map_or_else(
        || {
            m.project
                .error
                .clone()
                .map_or("loading…".to_owned(), |e| format!("unavailable: {e}"))
        },
        |p| match m.org_name() {
            Some(org) => format!("{} › {}", clean(org), clean(&p.name)),
            None => clean(&p.name),
        },
    );
    let id = c.dim.fg(&format!("#{pid}"));
    let mut row = Row::new(vec![label("project "), flex(&name, c.text), fixed(id)]);
    if let Some(inst) = &m.instance {
        row = row.target(Target::Page(inst.project_url(pid)));
    }
    row
}

fn ticket_rows(data: &ChromeData, p: &AmpPalette) -> Vec<Row> {
    let c = colors();
    let m = &data.meta;
    let Some(id) = m.ticket_id else {
        return vec![Row::new(vec![flex(
            "no current ticket: /ticket <id> picks one (a claim also sets it)",
            c.dim,
        )])];
    };
    let Some(t) = m.ticket.get().filter(|t| t.id == id) else {
        let note = match &m.ticket.error {
            Some(e) => format!("unavailable: {e}"),
            None => "loading…".to_owned(),
        };
        return vec![
            Row::new(vec![fixed(ticket_id(m, id)), flex(&note, c.dim)]).target(ticket_target(id)),
        ];
    };
    let mut rows = vec![
        Row::new(vec![
            fixed(state_mark(p, t.status)),
            fixed(ticket_id(m, id)),
            flex(&t.name, c.text),
        ])
        .target(ticket_target(id)),
    ];
    if let Some(epic) = &t.epic {
        let bar = p.epic(t.epic_index.unwrap_or(0)).fg("▌");
        let path = match &t.story {
            Some(s) => format!("{} › {}", epic.name, s.name),
            None => epic.name.clone(),
        };
        rows.push(Row::new(vec![
            label("plan    "),
            fixed(bar),
            flex(&path, c.muted),
        ]));
    }
    // The state mark above carries the colour; the reason itself is ink, under the web's word.
    if let Some(reason) = &t.block_reason {
        let (word, color) = if t.status == Status::Question {
            ("question", c.text)
        } else {
            ("waiting ", c.muted)
        };
        rows.push(Row::new(vec![label(word), flex(reason, color)]));
    }
    // AMP's own fields; one left empty is left out rather than shown as "-" (#2187).
    let mut fields = vec![format!("state {}", t.state)];
    if let Some(a) = t.assigned_to.as_deref().filter(|a| !a.is_empty()) {
        fields.push(format!("assigned {a}"));
    }
    if let Some(d) = t.dispatched_at.as_deref().filter(|d| !d.is_empty()) {
        fields.push(format!("dispatched {}", since_text(d, SystemTime::now())));
    }
    rows.push(Row::new(vec![
        label("AMP     "),
        flex(&fields.join(" · "), c.muted),
    ]));
    let report = match data.reports.get(&id) {
        Some(ReportState::Reported) => Some(c.success.fg("this session reported its run ✓")),
        Some(ReportState::Failed) => Some(c.error.fg("this session's run report failed")),
        None if t.status == Status::InProgress => {
            Some(c.warning.fg("this session has not reported a run"))
        }
        None => None,
    };
    if let Some(r) = report {
        rows.push(Row::new(vec![label("run     "), fixed(r)]));
    }
    for a in m.artifacts.get().into_iter().flatten() {
        let text = c.info.fg(&format!("artifact #{}", a.id));
        let text = match &m.instance {
            Some(inst) => link(&inst.artifact_url(a.id), &text),
            None => text,
        };
        let mut row = Row::new(vec![
            label("        "),
            fixed(text),
            flex(
                &format!("{} · {}", a.title, a.status.replace('_', " ")),
                c.muted,
            ),
        ]);
        if let Some(inst) = &m.instance {
            row = row.target(Target::Page(inst.artifact_url(a.id)));
        }
        rows.push(row);
    }
    if m.ticket.error.is_some() {
        rows.push(Row::new(vec![flex(
            &format!("stale: {}", m.ticket.error.as_deref().unwrap_or("")),
            c.dim,
        )]));
    }
    rows
}

pub(crate) fn tokens(n: i64) -> String {
    if n >= 1_000_000 {
        format!("{:.1}M", n as f64 / 1e6)
    } else if n >= 1_000 {
        format!("{:.1}k", n as f64 / 1e3)
    } else {
        n.to_string()
    }
}

fn elapsed(since: SystemTime) -> String {
    let d = SystemTime::now()
        .duration_since(since)
        .unwrap_or(Duration::ZERO)
        .as_secs();
    match d {
        0..60 => format!("{d}s"),
        60..3600 => format!("{}m{:02}s", d / 60, d % 60),
        _ => format!("{}h{:02}m", d / 3600, (d % 3600) / 60),
    }
}

/// An AMP timestamp (RFC 3339, e.g. `2026-10-23T00:00:01.4477798Z`) as a relative time like the
/// rest of the chrome: `just now`, `42s ago`, `3m ago`, `5h ago`, `2d ago` (#2213). A time in the
/// future (clock skew) reads `just now`; one that doesn't parse is shown as AMP sent it.
fn since_text(at: &str, now: SystemTime) -> String {
    let Some(then) = parse_rfc3339(at) else {
        return at.to_owned();
    };
    let d = now.duration_since(then).unwrap_or(Duration::ZERO).as_secs();
    match d {
        0..5 => "just now".to_owned(),
        5..60 => format!("{d}s ago"),
        60..3600 => format!("{}m ago", d / 60),
        3600..86_400 => format!("{}h ago", d / 3600),
        _ => format!("{}d ago", d / 86_400),
    }
}

/// `YYYY-MM-DDTHH:MM:SS[.frac](Z|±HH:MM)`; `None` for anything else. No time crate in amp-tui
/// for one field, so this is the civil-days arithmetic by hand.
pub(crate) fn parse_rfc3339(s: &str) -> Option<SystemTime> {
    let b = s.as_bytes();
    let num = |r: std::ops::Range<usize>| -> Option<i64> {
        let part = s.get(r)?;
        (!part.is_empty() && part.bytes().all(|c| c.is_ascii_digit()))
            .then(|| part.parse().ok())
            .flatten()
    };
    if b.len() < 20 || b[4] != b'-' || b[7] != b'-' || !matches!(b[10], b'T' | b't' | b' ') {
        return None;
    }
    if b[13] != b':' || b[16] != b':' {
        return None;
    }
    let (y, mo, d) = (num(0..4)?, num(5..7)?, num(8..10)?);
    let (h, mi, sec) = (num(11..13)?, num(14..16)?, num(17..19)?);
    if !(1..=12).contains(&mo) || !(1..=31).contains(&d) || h > 23 || mi > 59 || sec > 60 {
        return None;
    }
    let mut i = 19;
    let mut nanos = 0u32;
    if b[i] == b'.' {
        let start = i + 1;
        i = start;
        while i < b.len() && b[i].is_ascii_digit() {
            i += 1;
        }
        if i == start {
            return None;
        }
        let digits = &s[start..i.min(start + 9)];
        nanos = digits.parse::<u32>().ok()? * 10u32.pow(9 - digits.len() as u32);
    }
    let offset = match s.get(i..)? {
        "Z" | "z" => 0,
        tz if tz.len() == 6
            && matches!(tz.as_bytes()[0], b'+' | b'-')
            && tz.as_bytes()[3] == b':' =>
        {
            let secs = num(i + 1..i + 3)? * 3600 + num(i + 4..i + 6)? * 60;
            if tz.starts_with('-') { -secs } else { secs }
        }
        _ => return None,
    };
    // Days since 1970-01-01 (Howard Hinnant's days_from_civil).
    let yy = if mo <= 2 { y - 1 } else { y };
    let era = yy.div_euclid(400);
    let yoe = yy - era * 400;
    let mp = (mo + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146_097 + doe - 719_468;
    let secs = days * 86_400 + h * 3600 + mi * 60 + sec - offset;
    let secs = u64::try_from(secs).ok()?;
    Some(SystemTime::UNIX_EPOCH + Duration::new(secs, nanos))
}

/// `/workers` from the chat's roster (#2347): this chat's workers (live, then finished), then
/// tickets in progress elsewhere. Each is a row with the web's shape (state, id, name, agent,
/// where) and a detail row: model, state and elapsed time, tokens, context, cost, branch, what it
/// waits on, story › epic. A worker elsewhere shows AMP's record of its last run, and when it was
/// recorded.
fn roster_rows(data: &ChromeData, p: &AmpPalette) -> Vec<Row> {
    use crate::roster::Phase;
    let c = colors();
    let m = &data.meta;
    let board = m.board.get();
    let asking = |id: i64| board.is_some_and(|b| b.questions.iter().any(|q| q.id == id));
    let mut entries = data.roster.clone();
    entries.sort_by_key(|e| (!e.local, !e.is_live(), e.task_id));
    let mut rows = Vec::new();
    for e in &entries {
        let mark = if asking(e.task_id) {
            p.state_mark(Status::Question)
        } else {
            match e.phase {
                Phase::Queued => {
                    format!("{} {}", p.state_mark(Status::Ready), c.muted.fg("queued"))
                }
                Phase::Running => format!(
                    "{} {}",
                    p.state_mark(Status::InProgress),
                    p.state(Status::InProgress).text.fg("running")
                ),
                Phase::Stopping => format!(
                    "{} {}",
                    p.state_mark(Status::InProgress),
                    c.warning.fg("stopping")
                ),
                Phase::Finished => {
                    format!("{} {}", p.state_mark(Status::Done), c.muted.fg("finished"))
                }
                Phase::Elsewhere => p.state_mark(Status::InProgress),
            }
        };
        let where_ = if e.local {
            "· local"
        } else if e.viewable {
            "· elsewhere (session on disk)"
        } else {
            "· elsewhere"
        };
        let agent = if e.agent_id.is_empty() {
            "unassigned".to_owned()
        } else {
            clean(&e.agent_id)
        };
        rows.push(
            Row::new(vec![
                fixed(mark),
                fixed(ticket_id(m, e.task_id)),
                flex(&e.name, c.text),
                fixed(format!(
                    "{} {} {}",
                    c.dim.fg("·"),
                    c.muted.fg(&agent),
                    c.dim.fg(where_)
                )),
            ])
            .target(ticket_target(e.task_id)),
        );
        let mut detail: Vec<String> = Vec::new();
        if !e.model.is_empty() {
            detail.push(clean(&e.model));
        }
        if !e.state.is_empty() {
            detail.push(clean(&e.state));
        }
        let u = &e.usage;
        match e.usage_recorded {
            Some(at) => detail.push(format!(
                "last run {} · recorded {}",
                crate::roster::tok_label(e.tokens()),
                crate::roster::ago(at, SystemTime::now())
            )),
            None if e.tokens() > 0 => detail.push(format!(
                "{} (in {} · out {} · cached {})",
                crate::roster::tok_label(e.tokens()),
                tokens(u.input_tokens),
                tokens(u.output_tokens),
                tokens(u.cache_read_tokens + u.cache_write_tokens)
            )),
            None if !e.local => detail.push("no run recorded yet".to_owned()),
            None => {}
        }
        if let Some(pc) = e.context_percent() {
            detail.push(crate::roster::ctx_label(pc));
        }
        if u.cost_usd > 0.0 {
            detail.push(format!("${:.2}", u.cost_usd));
        }
        if let Some(b) = e.branch.as_deref().filter(|b| !b.is_empty()) {
            detail.push(format!("branch {}", clean(b)));
        }
        if !e.waiting_on.is_empty() {
            let ids: Vec<String> = e.waiting_on.iter().map(|id| format!("#{id}")).collect();
            detail.push(format!("waits on {}", ids.join(", ")));
        }
        let epic = e.epic_story();
        if !epic.is_empty() {
            detail.push(epic);
        }
        if !detail.is_empty() {
            rows.push(Row::new(vec![flex(&detail.join(" · "), c.dim)]).indent(2));
        }
    }
    rows
}

fn worker_rows(data: &ChromeData, p: &AmpPalette) -> Vec<Row> {
    let c = colors();
    let m = &data.meta;
    let board = m.board.get();
    // The roster when it has every local worker (a chat with a supervisor); else the workers
    // and the board as given (`render-chrome`, tests).
    let covered = data.workers.iter().all(|w| {
        data.roster
            .iter()
            .any(|e| e.task_id == w.task_id && e.local)
    });
    if !data.roster.is_empty() && covered {
        return roster_rows(data, p);
    }
    let name_of = |id: i64| -> String {
        board
            .and_then(|b| {
                b.in_progress_tickets
                    .iter()
                    .find(|t| t.id == id)
                    .map(|t| t.name.clone())
                    .or_else(|| {
                        b.questions
                            .iter()
                            .find(|q| q.id == id)
                            .map(|q| q.name.clone())
                    })
            })
            .unwrap_or_default()
    };
    let asking = |id: i64| board.is_some_and(|b| b.questions.iter().any(|q| q.id == id));
    let mut rows = Vec::new();
    for w in &data.workers {
        let status = if asking(w.task_id) {
            Status::Question
        } else if w.phase == WorkerPhase::Queued {
            Status::Ready
        } else {
            Status::InProgress
        };
        // The web word first (palette mark), then what the local worker is doing.
        let phase = match (status, w.phase) {
            (Status::Question, _) => p.state_mark(Status::Question),
            (_, WorkerPhase::Queued) => {
                format!("{} {}", p.state_mark(Status::Ready), c.muted.fg("queued"))
            }
            _ => format!(
                "{} {}",
                p.state_mark(Status::InProgress),
                p.state(Status::InProgress).text.fg("running")
            ),
        };
        let u = &w.totals.usage;
        let model = match w.model.as_deref().filter(|m| !m.is_empty()) {
            Some(m) => format!("{m} · "),
            None => String::new(),
        };
        let detail = format!(
            "{model}{} · ↑{} ↓{} ${:.2}",
            elapsed(w.since),
            tokens(u.input_tokens + u.cache_read_tokens + u.cache_write_tokens),
            tokens(u.output_tokens),
            u.cost_usd
        );
        rows.push(
            Row::new(vec![
                fixed(phase),
                fixed(ticket_id(m, w.task_id)),
                flex(&name_of(w.task_id), c.text),
                fixed(format!(
                    "{} {} {}",
                    c.dim.fg("·"),
                    c.muted.fg(&clean(&w.agent_id)),
                    c.dim.fg("· local")
                )),
            ])
            .target(ticket_target(w.task_id)),
        );
        // The run's detail on its own line, so the row keeps the web's shape at any width.
        rows.push(Row::new(vec![flex(&clean(&detail), c.dim)]).indent(2));
    }
    let local: Vec<i64> = data.workers.iter().map(|w| w.task_id).collect();
    for t in board
        .map(|b| b.in_progress_tickets.as_slice())
        .unwrap_or_default()
        .iter()
        .filter(|t| !local.contains(&t.id))
    {
        // The assignee exactly as AMP stores it (the web UI shows it unmodified).
        let who = t.assigned_to.as_deref().unwrap_or("unassigned");
        rows.push(
            Row::new(vec![
                fixed(p.state_mark(Status::InProgress)),
                fixed(ticket_id(m, t.id)),
                flex(&t.name, c.text),
                fixed(format!(
                    "{} {} {}",
                    c.dim.fg("·"),
                    c.muted.fg(&clean(who)),
                    c.dim.fg("· elsewhere")
                )),
            ])
            .target(ticket_target(t.id)),
        );
    }
    if rows.is_empty() {
        let note = if board.is_none() && m.project_id.is_some() {
            "no local workers (the board is loading)"
        } else {
            "no workers: nothing is in progress"
        };
        rows.push(Row::new(vec![flex(note, c.dim)]));
    }
    rows
}

fn question_rows(state: &PanelState, data: &ChromeData, p: &AmpPalette) -> Vec<Row> {
    let c = colors();
    let m = &data.meta;
    match &state.stage {
        Stage::Answer { ticket, question } => {
            let mut rows = vec![Row::new(vec![
                fixed(state_mark(p, Status::Question)),
                fixed(ticket_id(m, *ticket)),
                flex(&name_of_question(data, *ticket), c.text),
            ])];
            rows.push(Row::new(vec![Piece::Flex(question.clone(), c.text)]).indent(2));
            rows.push(Row::new(vec![flex(
                "type the answer in the editor below and press Enter; nothing is sent until you confirm",
                c.muted,
            )]));
            return rows;
        }
        Stage::Confirm { ticket, answer, .. } => {
            let mut rows = vec![Row::new(vec![
                fixed(c.warning.fg("send this answer to")),
                fixed(ticket_id(m, *ticket)),
                flex(
                    &format!(
                        "{} with amp_answer_question?",
                        name_of_question(data, *ticket)
                    ),
                    c.text,
                ),
            ])];
            rows.push(Row::new(vec![Piece::Flex(answer.clone(), c.text)]).indent(2));
            return rows;
        }
        Stage::Sending { ticket } => {
            return vec![Row::new(vec![
                fixed(c.muted.fg("sending the answer to")),
                fixed(ticket_id(m, *ticket)),
                fixed(c.muted.fg("…")),
            ])];
        }
        Stage::Browse => {}
    }
    let Some(board) = m.board.get() else {
        let note = match (&m.board.error, m.project_id) {
            (_, None) => "no project: /project <id> picks one",
            (Some(_), _) => "the board is unavailable",
            (None, _) => "loading…",
        };
        return vec![Row::new(vec![flex(note, c.dim)])];
    };
    if board.questions.is_empty() {
        return vec![Row::new(vec![flex(
            "no questions: nothing is waiting on a person",
            c.dim,
        )])];
    }
    let mut rows = Vec::new();
    for question in &board.questions {
        rows.push(
            Row::new(vec![
                fixed(state_mark(p, Status::Question)),
                fixed(ticket_id(m, question.id)),
                flex(&question.name, c.text),
            ])
            .target(Target::Ticket {
                id: question.id,
                question: Some(clean(&question.question)),
            }),
        );
        // The QUESTION chip carries the state; the question itself reads in ink, not red.
        rows.push(Row::new(vec![flex(&question.question, c.text)]).indent(5));
    }
    rows
}

fn name_of_question(data: &ChromeData, id: i64) -> String {
    data.meta
        .board
        .get()
        .and_then(|b| b.questions.iter().find(|q| q.id == id))
        .map(|q| q.name.clone())
        .unwrap_or_default()
}

/// How many of a plan node's blocked tickets are questions or scheduled (the rest wait).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct BlockedSplit {
    pub question: i64,
    pub scheduled: i64,
}

/// [`BlockedSplit`]s per story and per epic.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct PlanSplit {
    pub stories: HashMap<i64, BlockedSplit>,
    pub epics: HashMap<i64, BlockedSplit>,
}

/// Splits blocked tickets the way the web UI's `displayState` does ([`Status::of`]): a
/// `scheduled:` reason is Scheduled, any other reason is a Question, no reason is Waiting.
/// `list` is `amp_list_tasks(format=full)`; its `blocked` and `scheduled` buckets hold every
/// blocked ticket. `None` when it isn't that shape.
pub fn blocked_split(list: &Value) -> Option<PlanSplit> {
    let buckets = [
        list.get("blocked")?.as_array()?,
        list.get("scheduled")?.as_array()?,
    ];
    let mut seen = HashSet::new();
    let mut split = PlanSplit::default();
    for t in buckets.into_iter().flatten() {
        let Some(id) = t["id"].as_i64() else { continue };
        if !seen.insert(id) {
            continue;
        }
        let status = Status::of(
            t["state"].as_str().unwrap_or("blocked"),
            t["block_reason"].as_str(),
        );
        for (map, key) in [
            (&mut split.stories, t["story_id"].as_i64()),
            (&mut split.epics, t["epic_id"].as_i64()),
        ] {
            let Some(key) = key else { continue };
            let e = map.entry(key).or_default();
            match status {
                Status::Question => e.question += 1,
                Status::Scheduled => e.scheduled += 1,
                _ => {}
            }
        }
    }
    Some(split)
}

/// A plan node's counts in the six display states, in the status line's order (In progress,
/// Ready, Question, Scheduled, Waiting, Done), zeros left out. `tickets` is `amp_get_plan`'s
/// per-stored-state counts: backlog is Ready, and blocked is split by `split` (all Waiting
/// without it).
pub fn plan_states(tickets: &Value, split: Option<BlockedSplit>) -> Vec<(Status, i64)> {
    let n = |k: &str| tickets[k].as_i64().unwrap_or(0).max(0);
    let blocked = n("blocked");
    let sp = split.unwrap_or_default();
    let question = sp.question.clamp(0, blocked);
    let scheduled = sp.scheduled.clamp(0, blocked - question);
    [
        (Status::InProgress, n("in_progress")),
        (Status::Ready, n("backlog")),
        (Status::Question, question),
        (Status::Scheduled, n("scheduled") + scheduled),
        (Status::Waiting, blocked - question - scheduled),
        (Status::Done, n("completed")),
    ]
    .into_iter()
    .filter(|(_, k)| *k > 0)
    .collect()
}

/// `■ 2 In progress · □ 3 Ready · ■ 1 Question · ■ 10 Done`: [`plan_states`] with the palette's
/// state marks and the web UI's words.
fn plan_counts(p: &AmpPalette, tickets: &Value, split: Option<BlockedSplit>) -> String {
    let c = colors();
    let parts: Vec<String> = plan_states(tickets, split)
        .into_iter()
        .map(|(status, k)| {
            let s = p.state(status);
            format!(
                "{} {}",
                s.mark.fg(&s.glyph),
                s.text.fg(&format!("{k} {}", s.label))
            )
        })
        .collect();
    if parts.is_empty() {
        c.dim.fg("no tickets")
    } else {
        parts.join(&c.dim.fg(" · "))
    }
}

fn plan_rows(state: &PanelState, data: &ChromeData, p: &AmpPalette) -> Vec<Row> {
    let c = colors();
    let m = &data.meta;
    let mut rows = Vec::new();
    let Some(pid) = m.project_id else {
        return vec![Row::new(vec![flex(
            "no project: /project <id> picks one",
            c.dim,
        )])];
    };
    // Plan approval first: it is what a person acts on.
    if m.plan_required() {
        match m.plan.get() {
            Some(plan) if plan.awaiting_approval.is_empty() => rows.push(Row::new(vec![
                label("approval"),
                fixed(c.success.fg("plan approved")),
                // #2215: relative, like /ticket's dispatch time (#2213).
                flex(
                    &plan
                        .plan_approved_at
                        .as_deref()
                        .map(|at| since_text(at, SystemTime::now()))
                        .unwrap_or_default(),
                    c.dim,
                ),
            ])),
            Some(plan) => {
                // #2171: a person approves in the web UI; agents can't. Say so, and link the
                // Flight plan page (`o` on these rows, or `/open plan`).
                // #2198: approval is per ticket. The person's own tickets are theirs to approve;
                // others' wait for their owners (a project lead may approve them on their behalf).
                let ids = |list: &[i64]| -> String {
                    let mut out: Vec<String> =
                        list.iter().take(12).map(|id| format!("#{id}")).collect();
                    if list.len() > 12 {
                        out.push("…".to_owned());
                    }
                    out.join(" ")
                };
                let approve = m.instance.as_ref().map(|i| i.plan_url(pid));
                let target = |row: Row| match &approve {
                    Some(url) => row.target(Target::Page(url.clone())),
                    None => row,
                };
                let yours = plan.yours();
                let others = plan.others();
                if !yours.is_empty() {
                    let what = if plan.per_ticket() {
                        format!("{} of yours waiting for your approval", yours.len())
                    } else {
                        format!("{} waiting for your approval", yours.len())
                    };
                    rows.push(target(Row::new(vec![
                        label("approval"),
                        fixed(c.warning.fg(&what)),
                        flex(&ids(&yours), c.muted),
                    ])));
                    rows.push(target(
                        Row::new(vec![
                            fixed(c.dim.fg(if plan.per_ticket() {
                                "approve yours in AMP:"
                            } else {
                                "approve the plan in AMP:"
                            })),
                            match &approve {
                                Some(url) => fixed(link(url, &c.accent.fg(url))),
                                None => flex("the project's Flight plan page", c.dim),
                            },
                            fixed(c.dim.fg("(o or /open plan)")),
                        ])
                        .indent(2),
                    ));
                } else {
                    rows.push(Row::new(vec![
                        label("approval"),
                        fixed(c.success.fg("none of yours waiting")),
                    ]));
                }
                if !others.is_empty() {
                    rows.push(target(Row::new(vec![
                        label("others'"),
                        fixed(c.muted.fg(&format!(
                            "{} waiting for their owners' approval",
                            others.len()
                        ))),
                        flex(&ids(&others), c.dim),
                    ])));
                    rows.push(target(
                        Row::new(vec![flex(
                            "their owners approve them in AMP (a project lead may, on their behalf)",
                            c.dim,
                        )])
                        .indent(2),
                    ));
                }
            }
            None => rows.push(Row::new(vec![label("approval"), flex("loading…", c.dim)])),
        }
    } else {
        rows.push(Row::new(vec![
            label("approval"),
            flex("this project does not require plan approval", c.dim),
        ]));
    }
    let page = m
        .instance
        .as_ref()
        .map(|i| Target::Page(i.project_url(pid)));
    match &state.plan {
        None => rows.push(Row::new(vec![flex(
            "loading the plan (amp_get_plan)…",
            c.dim,
        )])),
        Some(Err(e)) => rows.push(Row::new(vec![flex(&format!("amp_get_plan: {e}"), c.error)])),
        Some(Ok(v)) => {
            let split = match &state.plan_blocked {
                Some(Ok(list)) => blocked_split(list).ok_or_else(|| "not a full list".to_owned()),
                Some(Err(e)) => Err(e.clone()),
                None => Err("not read".to_owned()),
            };
            let of = |map: fn(&PlanSplit) -> &HashMap<i64, BlockedSplit>, id: &Value| {
                split.as_ref().ok().map(|s| {
                    id.as_i64()
                        .and_then(|id| map(s).get(&id).copied())
                        .unwrap_or_default()
                })
            };
            let epics = v["epics"].as_array().cloned().unwrap_or_default();
            if epics.is_empty() {
                rows.push(Row::new(vec![flex("no epics yet", c.dim)]));
            }
            for (i, epic) in epics.iter().enumerate() {
                let mut row = Row::new(vec![
                    fixed(p.epic(i).fg("▌")),
                    flex(epic["name"].as_str().unwrap_or("?"), c.text),
                    fixed(plan_counts(
                        p,
                        &epic["tickets"],
                        of(|s| &s.epics, &epic["id"]),
                    )),
                ]);
                if let Some(t) = &page {
                    row = row.target(t.clone());
                }
                rows.push(row);
                for story in epic["stories"].as_array().into_iter().flatten() {
                    rows.push(
                        Row::new(vec![
                            flex(story["name"].as_str().unwrap_or("?"), c.muted),
                            fixed(plan_counts(
                                p,
                                &story["tickets"],
                                of(|s| &s.stories, &story["id"]),
                            )),
                        ])
                        .indent(2),
                    );
                }
            }
            // The split is a second read; without it, questions and scheduled tickets count as
            // Waiting, and the panel says so.
            if let Err(e) = &split {
                rows.push(Row::new(vec![flex(
                    &format!("Question and Scheduled counted as Waiting (amp_list_tasks: {e})"),
                    c.dim,
                )]));
            }
        }
    }
    rows
}

fn project_rows(data: &ChromeData, p: &AmpPalette) -> Vec<Row> {
    let c = colors();
    let m = &data.meta;
    let mut rows = vec![project_line(m)];
    if let Some(project) = m.project.get() {
        rows.push(Row::new(vec![
            label("        "),
            flex(
                &format!(
                    "code {} · {} · plan approval {}",
                    project.code,
                    project.state,
                    if project.require_plan_approval {
                        "required"
                    } else {
                        "not required"
                    }
                ),
                c.muted,
            ),
        ]));
    }
    // #2356: the session's project comes from a parent folder's .amp.json.
    if let Some((_, shown)) = m
        .project_marker
        .as_ref()
        .filter(|(id, _)| Some(*id) == m.project_id)
    {
        rows.push(Row::new(vec![label("marker  "), flex(shown, c.muted)]));
    }
    if let Some(board) = m.board.get() {
        let counts: Vec<String> = board
            .nonzero()
            .into_iter()
            .map(|(s, n)| {
                let st = p.state(s);
                format!(
                    "{} {}",
                    st.mark.fg(&st.glyph),
                    st.text.fg(&format!("{n} {}", st.label))
                )
            })
            .collect();
        let counts = if counts.is_empty() {
            c.dim.fg("no open tickets")
        } else {
            counts.join(&c.dim.fg(" · "))
        };
        rows.push(Row::new(vec![label("board   "), fixed(counts)]));
        rows.push(Row::new(vec![
            label("        "),
            flex(
                &format!(
                    "{} waiting · {} done · {} waiting on people",
                    board.waiting, board.done, board.waiting_on_people
                ),
                c.dim,
            ),
        ]));
    }
    rows
}

// ------------------------------------------------------------------------------------------------
// Rendering
// ------------------------------------------------------------------------------------------------

/// The open configuration panel's selected row (what its keys act on).
fn selected_info_row(state: &PanelState) -> Option<InfoRow> {
    let info = state.info.as_ref()?;
    let rows: Vec<InfoRow> = (info.rows)()
        .into_iter()
        .filter(|r| r.key.is_some())
        .collect();
    let at = state.selected.min(rows.len().checked_sub(1)?);
    rows.into_iter().nth(at)
}

/// A configuration panel's hint: Enter's word and the letter keys that apply to the selected row.
fn info_hint(state: &PanelState, has_targets: bool, overflow: bool) -> String {
    let mut parts = Vec::new();
    match (has_targets, overflow) {
        (true, _) => parts.push("↑↓ select".to_owned()),
        (false, true) => parts.push("↑↓ scroll".to_owned()),
        (false, false) => {}
    }
    if let Some(info) = &state.info
        && let Some(row) = selected_info_row(state)
    {
        if let Some((word, _)) = &info.enter {
            parts.push(format!("Enter {word}"));
        }
        for k in info.keys_for(&row) {
            parts.push(format!("{} {}", k.key, k.label));
        }
    }
    parts.push("Esc close".to_owned());
    parts.join(" · ")
}

/// The letter key's command for the selected row of a configuration panel, if `key` is one.
pub fn info_command(state: &PanelState, key: char) -> Option<String> {
    let info = state.info.as_ref()?;
    let row = selected_info_row(state)?;
    let k = info.keys_for(&row).into_iter().find(|k| k.key == key)?;
    Some(fill_command(&k.command, row.key.as_deref()?))
}

/// Enter's command for the selected row of a configuration panel.
pub fn info_enter(state: &PanelState) -> Option<String> {
    let info = state.info.as_ref()?;
    let row = selected_info_row(state)?;
    let (_, command) = info.enter.as_ref()?;
    Some(fill_command(command, row.key.as_deref()?))
}

fn hint(state: &PanelState, has_targets: bool, overflow: bool) -> String {
    if state.kind == PanelKind::Info && state.stage == Stage::Browse {
        return info_hint(state, has_targets, overflow);
    }
    match (&state.stage, state.kind) {
        (Stage::Answer { .. }, _) => {
            "Enter (in the editor) review the answer · Esc cancel".to_owned()
        }
        (Stage::Confirm { .. }, _) => "y / Enter send · n / Esc cancel".to_owned(),
        (Stage::Sending { .. }, _) => "sending…".to_owned(),
        (Stage::Browse, PanelKind::Model) if has_targets => {
            "↑↓ select · Enter switch · Esc close".to_owned()
        }
        (Stage::Browse, PanelKind::Workers) if has_targets => {
            "↑↓ select · Enter or click watch · x stop · o open in browser · Esc close".to_owned()
        }
        (Stage::Browse, PanelKind::Questions) if has_targets => {
            "↑↓ select · Enter answer · o open in browser · Esc close".to_owned()
        }
        (Stage::Browse, _) if has_targets => {
            "↑↓ select · Enter / o open in browser · Esc close".to_owned()
        }
        _ => "Esc close".to_owned(),
    }
}

/// Every row of the panel laid out at `width` (wrapped rows and notes take several lines), the
/// line the selection is on and how many rows are selectable.
fn layout(
    state: &PanelState,
    data: Option<&ChromeData>,
    p: &AmpPalette,
    width: usize,
) -> (Vec<String>, usize, usize) {
    let rows = build_rows(state, data, p);
    let n_targets = rows.iter().filter(|r| r.target.is_some()).count();
    let selected = state.selected.min(n_targets.saturating_sub(1));
    // Lay every row out (multi-line flex rows wrap), remembering where the selection is.
    let mut lines: Vec<String> = Vec::new();
    let mut selected_line = 0;
    let mut target_index = 0;
    let inner = width.saturating_sub(3);
    let c = colors();
    for row in &rows {
        let is_target = row.target.is_some();
        let is_selected = is_target && target_index == selected && state.stage == Stage::Browse;
        if is_target {
            target_index += 1;
        }
        let marker = if is_selected {
            c.accent.fg("›")
        } else {
            " ".to_owned()
        };
        if is_selected {
            selected_line = lines.len();
        }
        // An indented single-flex row is wrapped text (a question, an answer).
        if (row.indent > 0 || row.wrap)
            && let [Piece::Flex(text, color)] = row.pieces.as_slice()
        {
            for part in wrap_plain(text, inner.saturating_sub(row.indent).max(1)) {
                lines.push(format!(
                    "{marker} {}{}",
                    " ".repeat(row.indent),
                    color.fg(&part)
                ));
            }
            continue;
        }
        let room = inner.saturating_sub(row.indent);
        for (i, body) in fit_with_notes(&row.pieces, room).into_iter().enumerate() {
            let marker = if i == 0 { marker.as_str() } else { " " };
            lines.push(format!("{marker} {}{body}", " ".repeat(row.indent)));
        }
    }
    (lines, selected_line, n_targets)
}

/// The panel at `width`: a title rule, the rows (at most [`MAX_ROWS`], scrolled to the
/// selection), the note and the key hint. Every line fits `width`.
pub fn render_panel(
    state: &PanelState,
    data: Option<&ChromeData>,
    p: &AmpPalette,
    width: usize,
) -> Vec<String> {
    if width == 0 {
        return Vec::new();
    }
    let c = colors();
    let rule_char = rpi_tui::theme().border_style.chars().horizontal;
    let title = format!(" {} ", state.title());
    let title = shorten(&title, width.saturating_sub(2));
    let fill = width.saturating_sub(2 + visible_width(&title));
    let mut out = vec![format!(
        "{}{}{}",
        c.border_muted.fg(&rule_char.repeat(2.min(width))),
        rpi_tui::bold(&c.text.fg(&title)),
        c.border_muted.fg(&rule_char.repeat(fill))
    )];

    let (lines, selected_line, n_targets) = layout(state, data, p, width);
    let max_rows = if state.kind == PanelKind::Info {
        MAX_INFO_ROWS
    } else {
        MAX_ROWS
    };
    let start = (selected_line.saturating_sub(max_rows.saturating_sub(2)) + state.scroll)
        .min(lines.len().saturating_sub(max_rows));
    let end = (start + max_rows).min(lines.len());
    if start > 0 {
        out.push(c.dim.fg(&shorten(&format!("  ↑ {start} more"), width)));
    }
    out.extend(lines[start..end].iter().cloned());
    if end < lines.len() {
        out.push(
            c.dim
                .fg(&shorten(&format!("  ↓ {} more", lines.len() - end), width)),
        );
    }
    // The note and the key hint wrap rather than lose their ends (#2351: `l login` read `l l…`).
    if let Some((note, error)) = &state.note {
        let color = if *error { c.error } else { c.success };
        for part in wrap_plain(note, width.saturating_sub(2))
            .into_iter()
            .take(3)
        {
            out.push(format!("  {}", color.fg(&part)));
        }
    }
    let hint = hint(state, n_targets > 0, lines.len() > max_rows);
    for part in wrap_hint(&hint, width.saturating_sub(2)) {
        out.push(format!("  {}", c.dim.fg(&part)));
    }
    // A last guard: nothing wider than the terminal (the pieces are already fitted).
    out.into_iter()
        .map(|l| {
            if visible_width(&l) > width {
                shorten(&chrome::plain(&l), width)
            } else {
                l
            }
        })
        .collect()
}

/// A URL is opened only when it came from [`Instance`]'s helpers and is a plain `http(s)` URL
/// (the same rule as the header's links and login's browser guard).
pub fn openable(url: &str) -> bool {
    (url.starts_with("https://") || url.starts_with("http://"))
        && url.chars().all(|c| c.is_ascii_graphic())
}

/// The web page for `target`, from [`Instance`]'s helpers.
pub fn target_url(meta: &AmpMeta, target: &Target) -> Option<String> {
    match target {
        Target::Ticket { id, .. } => {
            let inst: &Instance = meta.instance.as_ref()?;
            Some(inst.ticket_url(meta.project_id?, *id))
        }
        Target::Page(url) => Some(url.clone()),
        Target::Model(_) | Target::Item(_) => None,
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn dispatched_times_read_relative() {
        use super::{parse_rfc3339, since_text};
        let at = |s: &str| parse_rfc3339(s).unwrap();
        let now = at("2026-10-23T00:03:01Z");
        assert_eq!(
            at("1970-01-01T00:00:00Z"),
            std::time::SystemTime::UNIX_EPOCH
        );
        assert_eq!(
            at("2026-10-23T00:00:01.4477798Z")
                .duration_since(std::time::SystemTime::UNIX_EPOCH)
                .unwrap()
                .as_nanos(),
            1_792_713_601_447_779_800
        );
        assert_eq!(at("2026-10-23T02:00:01+02:00"), at("2026-10-23T00:00:01Z"));
        assert_eq!(at("2026-10-22T19:00:01-05:00"), at("2026-10-23T00:00:01Z"));
        assert_eq!(since_text("2026-10-23T00:00:00.4477798Z", now), "3m ago");
        assert_eq!(since_text("2026-10-23T00:03:00Z", now), "just now");
        assert_eq!(
            since_text("2026-10-23T00:05:00Z", now),
            "just now",
            "future"
        );
        assert_eq!(since_text("2026-10-23T00:02:31Z", now), "30s ago");
        assert_eq!(since_text("2026-10-22T19:03:01Z", now), "5h ago");
        assert_eq!(since_text("2026-10-20T00:03:01Z", now), "3d ago");
        for bad in [
            "",
            "yesterday",
            "2026-10-23",
            "2026-13-01T00:00:00Z",
            "2026-10-23T00:00:00",
            "2026-10-23T00:00:00.Z",
        ] {
            assert_eq!(since_text(bad, now), bad);
        }
    }

    use super::*;

    #[test]
    fn pieces_fit_and_shorten_only_the_flex_text() {
        let c = Color::Default;
        let pieces = vec![
            Piece::Fixed("#2108".into()),
            Piece::Flex("a long ticket name that will not fit".into(), c),
            Piece::Fixed("tail".into()),
        ];
        for w in 0..60 {
            let line = fit_pieces(&pieces, w);
            assert!(visible_width(&line) <= w, "{w}: {line:?}");
        }
        let line = chrome::plain(&fit_pieces(&pieces, 24));
        assert!(line.starts_with("#2108 a long"), "{line}");
        assert!(line.ends_with("… tail") || line.ends_with("tail"), "{line}");
    }

    /// A `/config`-shaped panel and an `/mcp` details-shaped one (#2351).
    fn narrow_panels() -> (PanelState, PanelState) {
        let line = |label: &str, value: &str, note: &str| {
            let mut cells = vec![
                Cell::new(label, Tone::Dim).pad(18),
                Cell::flex(value, Tone::Text),
            ];
            if !note.is_empty() {
                cells.push(Cell::note(note, Tone::Muted));
            }
            InfoRow::new(cells).indent(2)
        };
        let config = InfoPanel::fixed(
            "configuration",
            vec![
                InfoRow::new(vec![Cell::new("files", Tone::Accent)]),
                line(
                    "repo",
                    "/Volumes/external-data/development/simstech/demo/harness-play/qa-3c",
                    "",
                ),
                InfoRow::new(vec![Cell::new("AMP server", Tone::Accent)]),
                line(
                    "url",
                    "http://localhost:8080",
                    "(config ~/.config/amp-harness/config.toml)",
                ),
                line(
                    "openrouter",
                    "openai-completions · https://openrouter.ai/api/v1",
                    "(key: env OPENROUTER_API_KEY)",
                ),
                line("protocol", "streamable", "(default)"),
            ],
        );
        let mcp = InfoPanel::fixed(
            "MCP server amp",
            vec![InfoRow::new(vec![Cell::flex("amp · ready", Tone::Text)]).key("amp")],
        )
        .on_enter("back to the list", "/mcp")
        .with_key('r', "restart", "/mcp restart {}")
        .with_key('t', "test", "/mcp test {}")
        .with_key('e', "enable", "/mcp enable {}")
        .with_key('d', "disable", "/mcp disable {}")
        .with_key('l', "login", "/mcp login {}")
        .with_key('T', "trust", "/mcp trust {}")
        .with_key('U', "untrust", "/mcp untrust {}");
        (
            PanelState::info(Arc::new(config), None),
            PanelState::info(Arc::new(mcp), Some("amp")),
        )
    }

    #[test]
    fn narrow_config_keeps_values_whole_and_moves_notes_under_them() {
        // #2351: at 80 columns (76 inside the default padding) the url read `http://l…` while its
        // source note took the rest of the row.
        let (config, _) = narrow_panels();
        let p = palette();
        let lines: Vec<String> = render_panel(&config, None, &p, 76)
            .iter()
            .map(|l| chrome::plain(l))
            .collect();
        assert!(lines.iter().all(|l| visible_width(l) <= 76), "{lines:#?}");
        let at = |needle: &str| {
            lines
                .iter()
                .position(|l| l.contains(needle))
                .unwrap_or_else(|| panic!("{needle:?} not in {lines:#?}"))
        };
        let url = at("url");
        assert!(
            lines[url].trim_end().ends_with("http://localhost:8080"),
            "{lines:#?}"
        );
        assert!(
            lines[url + 1].contains("(config ~/.config/amp-harness/config.toml)"),
            "{lines:#?}"
        );
        // The note lines up under the value.
        let value_col = lines[url].find("http").unwrap();
        assert_eq!(lines[url + 1].find('(').unwrap(), value_col, "{lines:#?}");
        let api = at("openrouter");
        assert!(
            lines[api].contains("openai-completions · https://openrouter.ai/api/v1"),
            "{lines:#?}"
        );
        assert!(lines[api + 1].contains("(key: env OPENROUTER_API_KEY)"));
        // A short row keeps its note on the same line.
        assert!(lines[at("protocol")].contains("streamable (default)"));
        // A long path keeps the folder it names.
        let repo = &lines[at("repo")];
        assert!(
            repo.contains("repo               …/")
                && repo.trim_end().ends_with("/harness-play/qa-3c"),
            "{repo}"
        );

        // Wide enough, every note stays on its row.
        let wide: Vec<String> = render_panel(&config, None, &p, 116)
            .iter()
            .map(|l| chrome::plain(l))
            .collect();
        assert!(wide.iter().any(|l| {
            l.contains("http://localhost:8080 (config ~/.config/amp-harness/config.toml)")
        }));
        assert!(!wide.iter().any(|l| l.contains('…')), "{wide:#?}");
    }

    #[test]
    fn narrow_key_hints_wrap_between_keys() {
        // #2351: the /mcp details hint ended in `l l…` at 80 columns.
        let (_, mcp) = narrow_panels();
        let lines: Vec<String> = render_panel(&mcp, None, &palette(), 76)
            .iter()
            .map(|l| chrome::plain(l))
            .collect();
        assert!(lines.iter().all(|l| visible_width(l) <= 76), "{lines:#?}");
        let hint = lines
            .iter()
            .skip_while(|l| !l.contains("↑↓ select"))
            .cloned()
            .collect::<Vec<_>>();
        assert!(hint.len() >= 2, "{lines:#?}");
        let joined = hint.join(" · ");
        for key in [
            "Enter back to the list",
            "l login",
            "T trust",
            "U untrust",
            "Esc close",
        ] {
            assert!(joined.contains(key), "{key:?} in {hint:#?}");
        }
        assert!(!joined.contains('…'), "{hint:#?}");
        assert_eq!(wrap_hint("a · b · c", 5), ["a · b", "c"]);
    }

    #[test]
    fn a_narrow_panel_scrolls_to_its_last_line() {
        // #2351: notes under values add lines at narrow widths; ↓ must still reach the end.
        let line = |i: usize| {
            InfoRow::new(vec![
                Cell::new(format!("row {i}"), Tone::Dim).pad(18),
                Cell::flex("http://localhost:8080/mcp", Tone::Text),
                Cell::note("(config ~/.config/amp-harness/config.toml)", Tone::Muted),
            ])
            .indent(2)
        };
        let rows: Vec<InfoRow> = (0..30).map(line).collect();
        let panel = Panel::new(Chrome::new());
        panel.open(PanelState::info(
            Arc::new(InfoPanel::fixed("config", rows)),
            None,
        ));
        let view = panel.component();
        let draw = || -> Vec<String> { view.render(76).iter().map(|l| chrome::plain(l)).collect() };
        let more = |l: &String| l.trim_start().starts_with("↓ ");
        assert!(draw().iter().any(more), "{:#?}", draw());
        for _ in 0..200 {
            panel.move_selection(1);
            draw();
        }
        let lines = draw();
        assert!(lines.iter().any(|l| l.contains("row 29")), "{lines:#?}");
        assert!(!lines.iter().any(more), "{lines:#?}");
    }

    #[test]
    fn path_values_keep_their_tail() {
        assert_eq!(shorten_value("/a/bb/ccc/dddd", 10), "…/ccc/dddd");
        assert_eq!(shorten_value("http://localhost:8080/x", 10), "http://lo…");
        assert_eq!(shorten_value("~/short", 10), "~/short");
        assert_eq!(shorten_value("a b/c/d/e/f/g", 6), "a b/c…");
    }

    #[test]
    fn wrap_plain_wraps_words_and_cuts_long_ones() {
        assert_eq!(wrap_plain("one two three", 7), vec!["one two", "three"]);
        assert_eq!(wrap_plain("abcdefghij", 4), vec!["abcd", "efgh", "ij"]);
        assert_eq!(wrap_plain("a\x1b[31mb", 10), vec!["a[31mb"]);
    }

    #[test]
    fn plan_counts_split_blocked_like_the_web() {
        use serde_json::json;
        // amp_list_tasks(format=full): a question, a waiting and a scheduled ticket in story 393.
        let list = json!({
            "blocked": [
                {"id": 1, "epic_id": 161, "story_id": 393, "state": "blocked", "block_reason": "Dark or light?"},
                {"id": 2, "epic_id": 161, "story_id": 393, "state": "blocked"}
            ],
            "scheduled": [
                {"id": 3, "epic_id": 161, "story_id": 393, "state": "blocked", "block_reason": "scheduled: 2026-11-01"}
            ],
            "questions": [{"id": 1, "epic_id": 161, "story_id": 393, "state": "blocked", "block_reason": "Dark or light?"}]
        });
        let split = blocked_split(&list).expect("full list");
        let story = split.stories[&393];
        assert_eq!(
            story,
            BlockedSplit {
                question: 1,
                scheduled: 1
            }
        );
        assert_eq!(split.epics[&161], story);
        let tickets = json!({"in_progress": 1, "backlog": 2, "blocked": 3, "completed": 4});
        assert_eq!(
            plan_states(&tickets, Some(story)),
            vec![
                (Status::InProgress, 1),
                (Status::Ready, 2),
                (Status::Question, 1),
                (Status::Scheduled, 1),
                (Status::Waiting, 1),
                (Status::Done, 4),
            ]
        );
        // Without the split every blocked ticket waits; zeros are left out.
        assert_eq!(
            plan_states(&json!({"blocked": 3}), None),
            vec![(Status::Waiting, 3)]
        );
        // A split larger than the plan's count (two reads, a moment apart) never goes negative.
        assert_eq!(
            plan_states(
                &json!({"blocked": 1}),
                Some(BlockedSplit {
                    question: 2,
                    scheduled: 2
                })
            ),
            vec![(Status::Question, 1)]
        );
        // The compact list (no buckets of tickets) is not a split.
        assert_eq!(blocked_split(&json!({"waiting": 1, "scheduled": 1})), None);
    }

    /// #2153: question text is ink (the theme's `text`), never the Question state's red; the
    /// QUESTION chip and the state mark carry the state. `/ticket` labels the row `question`.
    #[test]
    fn question_text_is_ink_and_the_ticket_row_says_question() {
        let p = crate::theme::palette();
        let c = colors();
        let red = p.state(Status::Question).text;
        assert_ne!(red, c.text, "the test needs ink and red to differ");
        let line_with = |lines: &[String], text: &str| {
            lines
                .iter()
                .find(|l| chrome::plain(l).contains(text))
                .cloned()
                .unwrap_or_else(|| panic!("{text}: {lines:?}"))
        };

        let data = chrome::sample::data(true);
        let lines = render_panel(&PanelState::new(PanelKind::Questions), Some(&data), &p, 100);
        for text in ["Overlay or inline?", "README or KB?"] {
            let line = line_with(&lines, text);
            assert!(line.contains(&c.text.fg(text)), "{line:?}");
            assert!(!line.contains(&red.fg(text)), "{line:?}");
        }
        // The chip still carries the state.
        assert!(chrome::plain(&line_with(&lines, "#2107")).contains("QUESTION"));

        // Answering shows the question in ink too.
        let mut answering = PanelState::new(PanelKind::Questions);
        answering.stage = Stage::Answer {
            ticket: 2107,
            question: "Overlay or inline?".to_owned(),
        };
        let lines = render_panel(&answering, Some(&data), &p, 100);
        let line = line_with(&lines, "Overlay or inline?");
        assert!(line.contains(&c.text.fg("Overlay or inline?")), "{line:?}");

        // /ticket on a question: the row is labelled `question`, its text in ink.
        let mut meta = chrome::sample::meta(true);
        let t = meta.ticket.value.as_mut().expect("sample ticket");
        t.state = "blocked".to_owned();
        t.status = Status::Question;
        t.block_reason = Some("Dark or light?".to_owned());
        let data = ChromeData::new(meta.clone());
        let lines = render_panel(&PanelState::new(PanelKind::Ticket), Some(&data), &p, 100);
        let line = line_with(&lines, "Dark or light?");
        assert!(line.contains(&c.dim.fg("question")), "{line:?}");
        assert!(line.contains(&c.text.fg("Dark or light?")), "{line:?}");
        assert!(!line.contains(&red.fg("Dark or light?")), "{line:?}");
        assert!(!chrome::plain(&line).contains("blocked"), "{line:?}");

        // A waiting ticket with a reason keeps a muted row, labelled `waiting`.
        let t = meta.ticket.value.as_mut().expect("sample ticket");
        t.status = Status::Waiting;
        t.block_reason = Some("needs #2152".to_owned());
        let data = ChromeData::new(meta);
        let lines = render_panel(&PanelState::new(PanelKind::Ticket), Some(&data), &p, 100);
        let line = line_with(&lines, "needs #2152");
        assert!(
            chrome::plain(&line).contains("waiting  needs #2152"),
            "{line:?}"
        );
        assert!(line.contains(&c.muted.fg("needs #2152")), "{line:?}");
    }

    /// #2215: `/plan`'s "plan approved" row shows the time relative (#2213's helper), not raw
    /// RFC 3339.
    #[test]
    fn plan_approved_time_is_relative() {
        let p = crate::theme::palette();
        let mut meta = chrome::sample::meta(true);
        meta.project
            .value
            .as_mut()
            .expect("sample project")
            .require_plan_approval = true;
        let plan = meta.plan.value.as_mut().expect("sample plan");
        plan.awaiting_approval.clear();
        plan.plan_approved_at = Some("2026-10-20T00:00:00.4477798Z".to_owned());
        let data = ChromeData::new(meta);
        let lines = render_panel(&PanelState::new(PanelKind::Plan), Some(&data), &p, 100);
        let line = lines
            .iter()
            .map(|l| chrome::plain(l))
            .find(|l| l.contains("plan approved"))
            .unwrap_or_else(|| panic!("{lines:?}"));
        assert!(line.trim_end().ends_with(" ago"), "{line}");
        assert!(!line.contains("2026-10-20"), "{line}");
    }

    /// #2356: `/project` says where a parent folder's `.amp.json` is, only while the panel shows
    /// that marker's project.
    #[test]
    fn project_panel_names_a_parent_folders_marker() {
        let p = crate::theme::palette();
        let mut meta = chrome::sample::meta(true);
        let text = |meta: &AmpMeta| {
            let data = ChromeData::new(meta.clone());
            render_panel(&PanelState::new(PanelKind::Project), Some(&data), &p, 100)
                .iter()
                .map(|l| chrome::plain(l))
                .collect::<Vec<_>>()
                .join("\n")
        };
        assert!(!text(&meta).contains("marker"), "{}", text(&meta));
        meta.project_marker = Some((
            2,
            "../.amp.json (a parent folder's: /work/.amp.json)".into(),
        ));
        assert!(
            text(&meta).contains("marker   ../.amp.json (a parent folder's: /work/.amp.json)"),
            "{}",
            text(&meta)
        );
        meta.project_id = Some(5);
        assert!(!text(&meta).contains("marker"), "{}", text(&meta));
    }

    #[test]
    fn only_plain_http_urls_open() {
        assert!(openable(
            "https://amp.simstech.cloud/project/2/board?task=2108"
        ));
        assert!(openable("http://localhost:18000/project/2"));
        for bad in [
            "file:///etc/passwd",
            "javascript:alert(1)",
            "https://amp.test/a b",
            "https://amp.test/\x1b]8;;x",
            "amp://x",
        ] {
            assert!(!openable(bad), "{bad}");
        }
    }
}
