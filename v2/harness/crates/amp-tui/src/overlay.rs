//! The AMP command panels (M3-4, #2108): what `/amp`, `/ticket`, `/workers`, `/questions`,
//! `/plan` and `/project` show, docked between the transcript and the editor.
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
    shell::short_role,
    theme::{AmpPalette, palette},
};

/// Rows shown at once; the list scrolls to keep the selection visible.
pub const MAX_ROWS: usize = 14;

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
        }
    }
}

/// The panel slot's shared state. Clones share it; the component reads it at every render.
#[derive(Clone)]
pub struct Panel {
    state: Arc<Mutex<Option<PanelState>>>,
    chrome: Chrome,
}

impl Panel {
    pub fn new(chrome: Chrome) -> Self {
        Self {
            state: Arc::default(),
            chrome,
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

    /// Moves the selection by `delta` among the selectable rows.
    pub fn move_selection(&self, delta: isize) {
        let n = self.targets().len();
        self.update(|s| {
            if n == 0 {
                s.selected = 0;
            } else {
                let at = s.selected.min(n - 1) as isize + delta;
                s.selected = at.clamp(0, n as isize - 1) as usize;
            }
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
}

/// One piece of a row.
#[derive(Debug, Clone)]
pub enum Piece {
    /// Styled text drawn whole (or dropped when it can't fit).
    Fixed(String),
    /// Plain text shortened to the room left, then drawn in `Color`.
    Flex(String, Color),
}

/// One line of a panel.
#[derive(Debug, Clone, Default)]
pub struct Row {
    pub pieces: Vec<Piece>,
    pub target: Option<Target>,
    /// Indent in cells (continuation lines).
    pub indent: usize,
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
            Piece::Fixed(s) => visible_width(s),
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
            Piece::Flex(s, c) if !flex_done => {
                flex_done = true;
                let short = shorten(s, room);
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
    }
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
            "re-login needed: run `amp-harness login` in another terminal (or set AMP_TOKEN), then reopen the chat",
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
    if let Some(reason) = &t.block_reason {
        let color = if t.status == Status::Question {
            p.state(Status::Question).text
        } else {
            c.muted
        };
        rows.push(Row::new(vec![label("blocked "), flex(reason, color)]));
    }
    rows.push(Row::new(vec![
        label("AMP     "),
        flex(
            &format!(
                "state {} · assigned {} · dispatched {}",
                t.state,
                t.assigned_to.as_deref().unwrap_or("-"),
                t.dispatched_at.as_deref().unwrap_or("-")
            ),
            c.muted,
        ),
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

fn tokens(n: i64) -> String {
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

fn worker_rows(data: &ChromeData, p: &AmpPalette) -> Vec<Row> {
    let c = colors();
    let m = &data.meta;
    let board = m.board.get();
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
        let phase = match (status, w.phase) {
            (Status::Question, _) => p.state_mark(Status::Question),
            (_, WorkerPhase::Queued) => format!(
                "{} {}",
                p.state(Status::Ready).mark.fg("□"),
                c.muted.fg("queued ")
            ),
            _ => format!(
                "{} {}",
                p.state(Status::InProgress).mark.fg("■"),
                p.state(Status::InProgress).text.fg("running")
            ),
        };
        let u = &w.totals.usage;
        let detail = format!(
            "{} · {} · {} · ↑{} ↓{} ${:.2}",
            short_role(&w.agent_id),
            w.model.as_deref().unwrap_or("-"),
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
                fixed(c.dim.fg(&clean(&detail))),
            ])
            .target(ticket_target(w.task_id)),
        );
    }
    let local: Vec<i64> = data.workers.iter().map(|w| w.task_id).collect();
    for t in board
        .map(|b| b.in_progress_tickets.as_slice())
        .unwrap_or_default()
        .iter()
        .filter(|t| !local.contains(&t.id))
    {
        let who = t
            .assigned_to
            .as_deref()
            .map(short_role)
            .unwrap_or("someone");
        rows.push(
            Row::new(vec![
                fixed(format!(
                    "{} {}",
                    p.state(Status::InProgress).mark.fg("■"),
                    c.muted.fg("elsewhere")
                )),
                fixed(ticket_id(m, t.id)),
                flex(&t.name, c.text),
                fixed(c.dim.fg(&clean(who))),
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
    let q = p.state(Status::Question);
    match &state.stage {
        Stage::Answer { ticket, question } => {
            let mut rows = vec![Row::new(vec![
                fixed(state_mark(p, Status::Question)),
                fixed(ticket_id(m, *ticket)),
                flex(&name_of_question(data, *ticket), c.text),
            ])];
            rows.push(Row::new(vec![Piece::Flex(question.clone(), q.text)]).indent(2));
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
        rows.push(Row::new(vec![flex(&question.question, q.text)]).indent(5));
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
                flex(plan.plan_approved_at.as_deref().unwrap_or(""), c.dim),
            ])),
            Some(plan) => {
                let ids: Vec<String> = plan
                    .awaiting_approval
                    .iter()
                    .take(12)
                    .map(|id| format!("#{id}"))
                    .collect();
                rows.push(Row::new(vec![
                    label("approval"),
                    fixed(c.warning.fg(&format!(
                        "{} awaiting approval",
                        plan.awaiting_approval.len()
                    ))),
                    flex(&ids.join(" "), c.muted),
                ]));
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

fn hint(state: &PanelState, has_targets: bool) -> String {
    match (&state.stage, state.kind) {
        (Stage::Answer { .. }, _) => {
            "Enter (in the editor) review the answer · Esc cancel".to_owned()
        }
        (Stage::Confirm { .. }, _) => "y / Enter send · n / Esc cancel".to_owned(),
        (Stage::Sending { .. }, _) => "sending…".to_owned(),
        (Stage::Browse, PanelKind::Questions) if has_targets => {
            "↑↓ select · Enter answer · o open in browser · Esc close".to_owned()
        }
        (Stage::Browse, _) if has_targets => {
            "↑↓ select · Enter / o open in browser · Esc close".to_owned()
        }
        _ => "Esc close".to_owned(),
    }
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
    let title = format!(" {} ", state.kind.title());
    let title = shorten(&title, width.saturating_sub(2));
    let fill = width.saturating_sub(2 + visible_width(&title));
    let mut out = vec![format!(
        "{}{}{}",
        c.border_muted.fg(&rule_char.repeat(2.min(width))),
        rpi_tui::bold(&c.text.fg(&title)),
        c.border_muted.fg(&rule_char.repeat(fill))
    )];

    let rows = build_rows(state, data, p);
    let n_targets = rows.iter().filter(|r| r.target.is_some()).count();
    let selected = state.selected.min(n_targets.saturating_sub(1));
    // Lay every row out (multi-line flex rows wrap), remembering where the selection is.
    let mut lines: Vec<String> = Vec::new();
    let mut selected_line = 0;
    let mut target_index = 0;
    let inner = width.saturating_sub(3);
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
        if row.indent > 0
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
        let body = fit_pieces(&row.pieces, inner.saturating_sub(row.indent));
        lines.push(format!("{marker} {}{body}", " ".repeat(row.indent)));
    }
    let start = selected_line.saturating_sub(MAX_ROWS.saturating_sub(2));
    let end = (start + MAX_ROWS).min(lines.len());
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
    if let Some((note, error)) = &state.note {
        let color = if *error { c.error } else { c.success };
        out.push(format!(
            "  {}",
            color.fg(&shorten(&clean(note), width.saturating_sub(2)))
        ));
    }
    out.push(format!(
        "  {}",
        c.dim.fg(&shorten(
            &hint(state, n_targets > 0),
            width.saturating_sub(2)
        ))
    ));
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
    }
}

#[cfg(test)]
mod tests {
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
