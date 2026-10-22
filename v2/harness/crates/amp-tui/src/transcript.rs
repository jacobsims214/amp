//! The transcript's components, word-wrapped (#2176).
//!
//! rpi-tui 0.8.0 wraps every component's text with a per-character hard wrap (see
//! [`crate::wrap`]), so the transcript uses these instead of pi's `Text`,
//! `AssistantMessageComponent`, `UserMessageComponent` and `ToolExecutionComponent`. They look
//! the same; only the wrapping differs:
//!
//! - [`WrapText`]: plain or styled lines (the banner, notices, `/help`, errors).
//! - [`WrapMarkdown`]: rpi's own `Markdown` renders the text, but prose is rendered at an
//!   unbounded width (rpi's markdown is line-based, so each source line becomes one row) and then
//!   word-wrapped here, with list continuations under the item's text and quote borders
//!   repeated. Fenced code and rules are width-dependent and keep rpi's rendering at the real
//!   width (a long code line still wraps, so it stays copyable). Tables are drawn by
//!   [`crate::table`] (#2180): box glyphs, padded cells, wrapped to fit or listed per row.
//! - [`AssistantBlock`]: an assistant message's text and thinking blocks, and its error line.
//! - [`ThinkingBlock`]: a thinking section, collapsed to one live `Thinking · 18s ▸` line by
//!   default (#2177); expanded, plain dim text behind a `│` gutter ([`ThinkingDisplay`]).
//! - [`UserBlock`]: the user's message on its surface, with the OSC 133 prompt markers.
//! - [`ToolBlock`]: a tool call on pi's tool panel (status glyph, title, summary, elapsed,
//!   result preview), with the header and result word-wrapped.

use std::{
    any::Any,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use rpi_tui::{
    Box as BgBox, Component, Container, Markdown, Spacer, apply_background_to_line, bold, theme,
    theme_revision, truncate_to_width,
};

use crate::table;
use crate::wrap::{open_sgr as open_style, plain, reflow_row, wrap};

/// The width prose is rendered at before it is word-wrapped: wide enough that rpi never wraps.
const UNBOUNDED: usize = 1 << 20;

/// A render cached per width and theme.
#[derive(Default)]
struct Cache(Mutex<Option<(usize, u64, Vec<String>)>>);

impl Cache {
    fn get_or(&self, width: usize, render: impl FnOnce() -> Vec<String>) -> Vec<String> {
        let revision = theme_revision();
        if let Ok(cache) = self.0.lock()
            && let Some((w, r, lines)) = cache.as_ref()
            && *w == width
            && *r == revision
        {
            return lines.clone();
        }
        let lines = render();
        if let Ok(mut cache) = self.0.lock() {
            *cache = Some((width, revision, lines.clone()));
        }
        lines
    }

    fn clear(&self) {
        if let Ok(mut cache) = self.0.lock() {
            *cache = None;
        }
    }
}

/// Word-wraps `text` (styled, several lines) to `width`, each row prefixed with `pad`. A style
/// left open at the end of one line carries on to the next, as it does on a terminal.
fn wrap_lines(text: &str, pad: &str, width: usize) -> Vec<String> {
    let mut rows = Vec::new();
    let mut carry = String::new();
    for line in text.split('\n') {
        let styled = format!("{carry}{line}");
        carry = open_style(&styled);
        rows.extend(
            reflow_row(&styled, width)
                .into_iter()
                .map(|r| format!("{pad}{r}")),
        );
    }
    rows
}

/// Lines of text, word-wrapped: rpi's `Text` (left padding, blank rows above and below), with
/// hanging indents under a line's leading spaces.
pub struct WrapText {
    text: String,
    padding_x: usize,
    padding_y: usize,
    cache: Cache,
}

impl WrapText {
    pub fn new(text: impl Into<String>, padding_x: usize, padding_y: usize) -> Self {
        Self {
            text: text.into(),
            padding_x,
            padding_y,
            cache: Cache::default(),
        }
    }
}

impl Component for WrapText {
    fn render(&self, width: usize) -> Vec<String> {
        self.cache.get_or(width, || {
            let mut lines = vec![String::new(); self.padding_y];
            let content_width = width.saturating_sub(self.padding_x).max(1);
            lines.extend(wrap_lines(
                &self.text,
                &" ".repeat(self.padding_x),
                content_width,
            ));
            lines.extend(std::iter::repeat_n(String::new(), self.padding_y));
            lines
        })
    }

    fn invalidate(&self) {
        self.cache.clear();
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// The markdown block styles [`WrapMarkdown`] supports.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MarkdownStyle {
    Plain,
    /// pi's thinking style: dim italic in the theme's thinking colour.
    Thinking,
}

/// Markdown rendered by rpi and word-wrapped here (see the module docs).
pub struct WrapMarkdown {
    text: Mutex<String>,
    padding_x: usize,
    style: MarkdownStyle,
    cache: Cache,
}

impl WrapMarkdown {
    pub fn new(text: impl Into<String>, padding_x: usize, style: MarkdownStyle) -> Self {
        Self {
            text: Mutex::new(text.into()),
            padding_x,
            style,
            cache: Cache::default(),
        }
    }

    pub fn set_text(&self, text: impl Into<String>) {
        if let Ok(mut t) = self.text.lock() {
            *t = text.into();
        }
        self.cache.clear();
    }

    fn markdown(&self, text: String) -> Markdown {
        self.markdown_padded(text, self.padding_x)
    }

    fn markdown_padded(&self, text: String, padding_x: usize) -> Markdown {
        let md = Markdown::new(text, padding_x, 0);
        match self.style {
            MarkdownStyle::Plain => md,
            MarkdownStyle::Thinking => {
                md.with_base_style(format!("\x1b[3m{}", theme().colors.thinking_text.to_fg()))
            }
        }
    }
}

impl WrapMarkdown {
    /// A table, drawn by [`crate::table`] at rpi's content width (the width less the padding on
    /// both sides), cells' inline markdown rendered by rpi.
    fn table(&self, src: &str, width: usize) -> Vec<String> {
        let lines: Vec<&str> = src.lines().collect();
        let Some(t) = table::parse(&lines) else {
            return self.markdown(src.to_owned()).render(width);
        };
        let colors = theme().colors;
        let border = |s: &str| colors.border_muted.fg(s);
        let header = |s: &str| bold(s);
        let inline = |cell: &str| {
            self.markdown_padded(inline_only(cell), 0)
                .render(UNBOUNDED)
                .iter()
                .map(|r| r.trim_end().to_owned())
                .filter(|r| !plain(r).trim().is_empty())
                .collect::<Vec<_>>()
                .join(" ")
        };
        let margin = " ".repeat(self.padding_x);
        let style = table::Style {
            margin: &margin,
            border: &border,
            header: &header,
        };
        table::render(&t, width.saturating_sub(self.padding_x), &style, &inline)
    }
}

/// Escapes what would make a cell's text render as a block (a heading, list item, quote, rule
/// or fence) rather than inline text.
fn inline_only(cell: &str) -> String {
    let t = cell.trim();
    let hashes = t.bytes().take_while(|&b| b == b'#').count();
    let digits = t.bytes().take_while(u8::is_ascii_digit).count();
    let block = (hashes > 0 && t[hashes..].starts_with(' '))
        || ["> ", "- ", "* ", "+ ", "```", "~~~", "---", "***", "___"]
            .iter()
            .any(|m| t.starts_with(m))
        || t == "-"
        || t == ">";
    if block {
        format!("\\{t}")
    } else if digits > 0
        && matches!(t.as_bytes().get(digits), Some(b'.' | b')'))
        && matches!(t.as_bytes().get(digits + 1), None | Some(b' '))
    {
        format!("{}\\{}", &t[..digits], &t[digits..])
    } else {
        t.to_owned()
    }
}

impl Component for WrapMarkdown {
    fn render(&self, width: usize) -> Vec<String> {
        let text = self.text.lock().map(|t| t.clone()).unwrap_or_default();
        self.cache.get_or(width, || {
            // rpi wraps markdown text at the width less the padding on both sides.
            let text_width = width.saturating_sub(self.padding_x).max(1);
            let mut lines = Vec::new();
            for segment in segments(&text) {
                match segment {
                    Segment::Prose(s) => {
                        for row in self.markdown(s).render(UNBOUNDED) {
                            lines.extend(reflow_row(&row, text_width));
                        }
                    }
                    Segment::Block(s) => lines.extend(self.markdown(s).render(width)),
                    Segment::Table(s) => lines.extend(self.table(&s, width)),
                }
            }
            lines
        })
    }

    fn invalidate(&self) {
        self.cache.clear();
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// A run of markdown source lines: prose (word-wrapped by us) or a width-dependent block.
#[derive(Debug, PartialEq)]
enum Segment {
    Prose(String),
    /// A fenced code block (or Mermaid diagram) or a rule.
    Block(String),
    /// A GFM table (#2180).
    Table(String),
}

/// The fence character of a ```` ``` ```` / `~~~` fence line (rpi's `fence_info`).
fn fence(line: &str) -> Option<char> {
    let t = line.trim_start();
    if t.starts_with("```") {
        Some('`')
    } else if t.starts_with("~~~") {
        Some('~')
    } else {
        None
    }
}

/// rpi's `is_table_row`.
fn is_table_row(line: &str) -> bool {
    let t = line.trim();
    t.starts_with('|') && t.ends_with('|') && t.len() > 1
}

/// rpi's `is_table_separator`.
fn is_table_separator(line: &str) -> bool {
    let t = line.trim();
    t.contains('|')
        && t.trim_matches('|').split('|').all(|cell| {
            cell.chars().all(|c| matches!(c, '-' | ':' | ' ' | '\t')) && cell.contains('-')
        })
}

/// A line rpi renders as a rule: `---`/`***` that is not a heading, list item or quote.
fn is_rule(line: &str) -> bool {
    let c = line.trim_start_matches([' ', '\t']);
    let t = line.trim();
    (t.starts_with("---") || t.starts_with("***"))
        && !line.starts_with('#')
        && !c.starts_with('>')
        && !["- ", "* ", "+ "].iter().any(|m| c.starts_with(m))
}

/// Where a table starts at `lines[i]`: rpi's `| a | b |` form, or GFM's pipe-optional form when
/// the separator has as many columns as the header.
fn table_start(lines: &[&str], i: usize) -> bool {
    let (line, Some(next)) = (lines[i], lines.get(i + 1)) else {
        return false;
    };
    (is_table_row(line) && is_table_separator(next))
        || (table::has_pipe(line)
            && table::is_separator(next)
            && table::split_cells(line).len() == table::split_cells(next).len())
}

/// A table body row: rpi's `| … |`, or any line with a cell-separating pipe that starts no
/// other block.
fn table_body(line: &str) -> bool {
    is_table_row(line)
        || (!line.trim().is_empty()
            && fence(line).is_none()
            && !is_rule(line)
            && table::has_pipe(line))
}

/// Splits markdown source into prose, tables and width-dependent blocks, the way rpi's renderer
/// walks it.
fn segments(text: &str) -> Vec<Segment> {
    #[derive(PartialEq)]
    enum Kind {
        Prose,
        Block,
        Table,
    }
    let lines: Vec<&str> = text.lines().collect();
    let mut out: Vec<Segment> = Vec::new();
    let mut push = |kind: Kind, chunk: &[&str]| {
        let chunk = chunk.join("\n");
        match (out.last_mut(), &kind) {
            (Some(Segment::Prose(s)), Kind::Prose) | (Some(Segment::Block(s)), Kind::Block) => {
                s.push('\n');
                s.push_str(&chunk);
            }
            _ => out.push(match kind {
                Kind::Prose => Segment::Prose(chunk),
                Kind::Block => Segment::Block(chunk),
                Kind::Table => Segment::Table(chunk),
            }),
        }
    };
    let mut i = 0;
    while i < lines.len() {
        let start = i;
        let line = lines[i];
        if let Some(ch) = fence(line) {
            i += 1;
            while i < lines.len() {
                i += 1;
                if fence(lines[i - 1]) == Some(ch) {
                    break;
                }
            }
            push(Kind::Block, &lines[start..i]);
        } else if table_start(&lines, i) {
            i += 2;
            while i < lines.len() && table_body(lines[i]) {
                i += 1;
            }
            push(Kind::Table, &lines[start..i]);
        } else if is_rule(line) {
            i += 1;
            push(Kind::Block, &lines[start..i]);
        } else {
            i += 1;
            push(Kind::Prose, &lines[start..i]);
        }
    }
    if out.is_empty() {
        out.push(Segment::Prose(String::new()));
    }
    out
}

/// One visible block of an assistant message.
#[derive(Debug, Clone, PartialEq)]
pub enum Block {
    Text(String),
    Thinking(String),
}

/// How the transcript shows the model's thinking (`[tui] show_thinking`, #2177).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum ThinkingDisplay {
    /// One line per thinking section: `Thinking · 18s ▸`, with the latest sentence while it
    /// streams. Ctrl+T expands the last one.
    #[default]
    Collapsed,
    /// The whole text, as plain dim text behind a `│` gutter.
    Full,
    /// Nothing (Ctrl+T still shows the last one).
    Hidden,
}

impl ThinkingDisplay {
    /// Parses `collapsed`, `full` or `hidden`.
    pub fn parse(raw: &str) -> Result<Self, String> {
        match raw.trim().to_ascii_lowercase().as_str() {
            "" | "collapsed" => Ok(Self::Collapsed),
            "full" => Ok(Self::Full),
            "hidden" => Ok(Self::Hidden),
            other => Err(format!(
                "unknown show_thinking `{other}`: use collapsed, full or hidden"
            )),
        }
    }

    pub fn word(self) -> &'static str {
        match self {
            Self::Collapsed => "collapsed",
            Self::Full => "full",
            Self::Hidden => "hidden",
        }
    }
}

struct ThinkingState {
    text: String,
    started: Instant,
    ended: Option<Instant>,
    /// Whether it was seen streaming (a replayed message has no duration to show).
    timed: bool,
    /// A blank row after it (something visible follows).
    spacer: bool,
}

/// One thinking section of an assistant message (#2177). Collapsed it is one dim line,
/// `Thinking · 18s ▸`, followed while it streams by the latest sentence; expanded, the text
/// follows as plain dim rows behind a `│` gutter, with no markdown styling, so it never reads
/// like the answer.
pub struct ThinkingBlock {
    state: Mutex<ThinkingState>,
    expanded: AtomicBool,
    display: ThinkingDisplay,
    /// The expanded body's rows.
    cache: Cache,
}

impl ThinkingBlock {
    /// Left padding, as the assistant's text has.
    const PAD: usize = 1;

    pub fn new(display: ThinkingDisplay, text: impl Into<String>) -> Self {
        Self {
            state: Mutex::new(ThinkingState {
                text: text.into(),
                started: Instant::now(),
                ended: None,
                timed: true,
                spacer: false,
            }),
            expanded: AtomicBool::new(display == ThinkingDisplay::Full),
            display,
            cache: Cache::default(),
        }
    }

    pub fn set_text(&self, text: &str) {
        if let Ok(mut s) = self.state.lock()
            && s.text != text
        {
            s.text = text.to_owned();
            self.cache.clear();
        }
    }

    /// The thinking is over (the answer started, or the message ended). `timed: false` when it
    /// was never seen streaming.
    pub fn end(&self, timed: bool) {
        if let Ok(mut s) = self.state.lock()
            && s.ended.is_none()
        {
            s.ended = Some(Instant::now());
            s.timed = timed;
        }
    }

    pub fn is_live(&self) -> bool {
        self.state.lock().is_ok_and(|s| s.ended.is_none())
    }

    fn set_spacer(&self, spacer: bool) {
        if let Ok(mut s) = self.state.lock() {
            s.spacer = spacer;
        }
    }

    /// Expands or collapses it; returns whether it is now expanded.
    pub fn toggle(&self) -> bool {
        !self.expanded.fetch_xor(true, Ordering::Relaxed)
    }

    pub fn is_expanded(&self) -> bool {
        self.expanded.load(Ordering::Relaxed)
    }

    fn header(&self, s: &ThinkingState, width: usize) -> String {
        let colors = theme().colors;
        let expanded = self.is_expanded();
        let mut label = "Thinking".to_owned();
        if s.timed {
            let secs = s
                .ended
                .unwrap_or_else(Instant::now)
                .duration_since(s.started)
                .as_secs();
            label.push_str(&format!(" · {secs}s"));
        }
        let glyph = if expanded { "▾" } else { "▸" };
        let pad = " ".repeat(Self::PAD);
        let head = format!("{label} {glyph}");
        let room = width.saturating_sub(Self::PAD + rpi_tui::visible_width(&head) + 1);
        let latest = if s.ended.is_none() && !expanded {
            tail_to_width(&latest_sentence(&s.text), room)
        } else {
            String::new()
        };
        let mut row = format!(
            "{pad}{} {}",
            colors.thinking_text.fg(&label),
            colors.muted.fg(glyph)
        );
        if !latest.is_empty() {
            row.push(' ');
            row.push_str(&colors.thinking_text.fg(&latest));
        }
        truncate_to_width(&row, width, "")
    }

    fn body(&self, text: &str, width: usize) -> Vec<String> {
        self.cache.get_or(width, || {
            let colors = theme().colors;
            let pad = " ".repeat(Self::PAD);
            let gutter = colors.dim.fg("│");
            let body_width = width.saturating_sub(Self::PAD + 2).max(1);
            text.trim()
                .replace("\r\n", "\n")
                .split('\n')
                .flat_map(|line| {
                    let line = plain(line);
                    if line.trim().is_empty() {
                        return vec![format!("{pad}{gutter}")];
                    }
                    wrap_lines(&line, "", body_width)
                        .into_iter()
                        .map(|r| format!("{pad}{gutter} {}", colors.thinking_text.fg(&r)))
                        .collect()
                })
                .collect()
        })
    }
}

impl Component for ThinkingBlock {
    fn render(&self, width: usize) -> Vec<String> {
        let shown = self.display != ThinkingDisplay::Hidden || self.is_expanded();
        let Ok(s) = self.state.lock() else {
            return Vec::new();
        };
        if !shown || s.text.trim().is_empty() {
            return Vec::new();
        }
        let mut rows = vec![self.header(&s, width)];
        if self.is_expanded() {
            rows.extend(self.body(&s.text, width));
        }
        if s.spacer {
            rows.push(String::new());
        }
        rows
    }

    fn invalidate(&self) {
        self.cache.clear();
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// The sentence the model is on: the last one of the last non-empty line, without markdown
/// markers.
fn latest_sentence(text: &str) -> String {
    let Some(line) = text.lines().rev().map(str::trim).find(|l| !l.is_empty()) else {
        return String::new();
    };
    let line = plain(line)
        .trim_start_matches(['#', '>', '-', '*', '+', ' '])
        .replace("**", "")
        .replace('`', "");
    let mut sentences = Vec::new();
    let mut current = String::new();
    let mut chars = line.chars().peekable();
    while let Some(c) = chars.next() {
        current.push(c);
        if matches!(c, '.' | '!' | '?') && chars.peek().is_some_and(|n| n.is_whitespace()) {
            sentences.push(std::mem::take(&mut current));
        }
    }
    sentences.push(current);
    sentences
        .into_iter()
        .map(|s| s.trim().to_owned())
        .rfind(|s| !s.is_empty())
        .unwrap_or_default()
}

/// The end of `s` that fits in `width` columns, behind `…` when cut.
fn tail_to_width(s: &str, width: usize) -> String {
    if rpi_tui::visible_width(s) <= width {
        return s.to_owned();
    }
    if width < 2 {
        return String::new();
    }
    let mut tail: Vec<char> = Vec::new();
    let mut used = 1;
    for c in s.chars().rev() {
        let w = rpi_tui::visible_width(c.encode_utf8(&mut [0; 4]));
        if used + w > width {
            break;
        }
        used += w;
        tail.push(c);
    }
    let tail: String = tail.into_iter().rev().collect();
    format!("…{}", tail.trim_start())
}

/// An assistant message: its text and thinking blocks (pi's `AssistantMessageComponent`
/// layout: consecutive thinking blocks joined into one section, a blank row between thinking
/// and what follows), word-wrapped. Thinking shows as [`ThinkingBlock`]s, per
/// [`ThinkingDisplay`].
pub struct AssistantBlock {
    content: Arc<Container>,
    blocks: Mutex<Vec<Block>>,
    display: ThinkingDisplay,
    /// One per thinking section, kept across rebuilds (they hold its timer and expansion).
    thinking: Mutex<Vec<Arc<ThinkingBlock>>>,
    finished: AtomicBool,
}

impl Default for AssistantBlock {
    fn default() -> Self {
        Self::new()
    }
}

impl AssistantBlock {
    /// pi's output padding for assistant text.
    const PAD: usize = 1;

    pub fn new() -> Self {
        Self::with_thinking(ThinkingDisplay::default())
    }

    pub fn with_thinking(display: ThinkingDisplay) -> Self {
        Self {
            content: Arc::new(Container::new()),
            blocks: Mutex::new(Vec::new()),
            display,
            thinking: Mutex::new(Vec::new()),
            finished: AtomicBool::new(false),
        }
    }

    pub fn update_blocks(&self, blocks: &[Block]) {
        if let Ok(mut b) = self.blocks.lock() {
            if *b == blocks {
                return;
            }
            *b = blocks.to_vec();
        }
        self.rebuild(blocks);
    }

    /// The message is complete: its thinking stops its timers.
    pub fn finish(&self) {
        self.finished.store(true, Ordering::Relaxed);
        if let Ok(t) = self.thinking.lock() {
            for block in t.iter() {
                block.end(true);
            }
        }
    }

    /// Whether it shows any thinking.
    pub fn has_thinking(&self) -> bool {
        self.thinking.lock().is_ok_and(|t| !t.is_empty())
    }

    /// Expands or collapses its last thinking section; `None` when it has none, else whether
    /// it is now expanded.
    pub fn toggle_thinking(&self) -> Option<bool> {
        let last = self.thinking.lock().ok()?.last().cloned()?;
        let expanded = last.toggle();
        self.content.invalidate();
        Some(expanded)
    }

    fn rebuild(&self, blocks: &[Block]) {
        self.content.clear();
        let visible = |b: &Block| match b {
            Block::Text(t) | Block::Thinking(t) => !t.trim().is_empty(),
        };
        let finished = self.finished.load(Ordering::Relaxed);
        let Ok(mut thinking) = self.thinking.lock() else {
            return;
        };
        let mut section = 0;
        let mut i = 0;
        while i < blocks.len() {
            match &blocks[i] {
                Block::Text(t) => {
                    if !t.trim().is_empty() {
                        self.content.add_child(Arc::new(WrapMarkdown::new(
                            t.trim(),
                            Self::PAD,
                            MarkdownStyle::Plain,
                        )));
                    }
                    i += 1;
                }
                Block::Thinking(_) => {
                    let mut joined = Vec::new();
                    while let Some(Block::Thinking(t)) = blocks.get(i) {
                        if !t.trim().is_empty() {
                            joined.push(t.trim());
                        }
                        i += 1;
                    }
                    if joined.is_empty() {
                        continue;
                    }
                    let text = joined.join("\n\n");
                    let followed = blocks[i..].iter().any(visible);
                    let block = match thinking.get(section) {
                        Some(b) => {
                            b.set_text(&text);
                            Arc::clone(b)
                        }
                        None => {
                            let b = Arc::new(ThinkingBlock::new(self.display, text));
                            // Seen complete at once (a replayed message): no duration.
                            if followed || finished {
                                b.end(false);
                            }
                            thinking.push(Arc::clone(&b));
                            b
                        }
                    };
                    if followed || finished {
                        block.end(true);
                    }
                    block.set_spacer(followed);
                    self.content.add_child(block);
                    section += 1;
                }
            }
        }
        thinking.truncate(section);
    }

    /// Replaces the content with pi's error line (`❌ Error: …`, `❌ Operation aborted: …`).
    pub fn update_error(&self, error: &str, stop_reason: &str) {
        if let Ok(mut b) = self.blocks.lock() {
            b.clear();
        }
        if let Ok(mut t) = self.thinking.lock() {
            t.clear();
        }
        self.content.clear();
        self.content.add_child(Arc::new(Spacer::new(1)));
        let text = match stop_reason {
            "aborted" => format!("❌ Operation aborted: {error}"),
            "error" => format!("❌ Error: {error}"),
            "length" => "⚠️ Response was truncated before completion.".to_owned(),
            _ => format!("❌ {error}"),
        };
        self.content
            .add_child(Arc::new(WrapText::new(text, Self::PAD, 0)));
    }
}

impl Component for AssistantBlock {
    fn render(&self, width: usize) -> Vec<String> {
        self.content.render(width)
    }

    fn invalidate(&self) {
        let blocks = self.blocks.lock().map(|b| b.clone()).unwrap_or_default();
        if !blocks.is_empty() {
            self.rebuild(&blocks);
        }
        self.content.invalidate();
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

const OSC133_ZONE_START: &str = "\x1b]133;A\x07";
const OSC133_ZONE_END: &str = "\x1b]133;B\x07";
const OSC133_ZONE_FINAL: &str = "\x1b]133;C\x07";

/// The user's message: markdown on the theme's surface colour inside a padded box, between OSC
/// 133 prompt-zone markers (pi's `UserMessageComponent`), word-wrapped.
pub struct UserBlock {
    text: String,
    inner: Mutex<(u64, BgBox)>,
}

fn user_box(text: &str) -> BgBox {
    let bg = theme().colors.surface;
    let content = BgBox::new(1, 1);
    content.set_bg_fn(Some(Arc::new(move |s: &str| bg.bg(s))));
    content.add_child(Arc::new(WrapMarkdown::new(text, 0, MarkdownStyle::Plain)));
    content
}

impl UserBlock {
    pub fn new(text: impl Into<String>) -> Self {
        let text = text.into();
        let inner = Mutex::new((theme_revision(), user_box(&text)));
        Self { text, inner }
    }
}

impl Component for UserBlock {
    fn render(&self, width: usize) -> Vec<String> {
        let Ok(mut inner) = self.inner.lock() else {
            return Vec::new();
        };
        // The surface colour is baked into the box: rebuild it after a theme change.
        let revision = theme_revision();
        if inner.0 != revision {
            *inner = (revision, user_box(&self.text));
        }
        let mut lines = inner.1.render(width);
        if let Some(first) = lines.first_mut() {
            first.insert_str(0, OSC133_ZONE_START);
        }
        if let Some(last) = lines.last_mut() {
            last.insert_str(0, &format!("{OSC133_ZONE_END}{OSC133_ZONE_FINAL}"));
        }
        lines
    }

    fn invalidate(&self) {
        if let Ok(mut inner) = self.inner.lock() {
            *inner = (theme_revision(), user_box(&self.text));
        }
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// Where a tool call is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ToolState {
    Pending,
    Running,
    Completed,
    Failed,
}

/// Result rows shown under a tool's header (pi's collapsed preview).
const RESULT_PREVIEW_LINES: usize = 12;

struct ToolInner {
    title: String,
    summary: String,
    state: ToolState,
    started: Option<Instant>,
    finished: Option<Instant>,
    result: Option<String>,
}

/// A tool call on pi's tool panel: `✓ title summary 1.2s ▸` and up to 12 result rows behind a
/// `│` gutter, on the status-tinted background, word-wrapped.
pub struct ToolBlock {
    inner: Mutex<ToolInner>,
    cache: Cache,
}

impl ToolBlock {
    /// `title` is shown bold; `summary` is the call's most telling argument (one line).
    pub fn new(title: impl Into<String>, summary: impl Into<String>) -> Self {
        Self {
            inner: Mutex::new(ToolInner {
                title: title.into(),
                summary: summary.into(),
                state: ToolState::Pending,
                started: None,
                finished: None,
                result: None,
            }),
            cache: Cache::default(),
        }
    }

    pub fn set_running(&self) {
        if let Ok(mut i) = self.inner.lock() {
            i.state = ToolState::Running;
            i.started = Some(Instant::now());
        }
        self.cache.clear();
    }

    pub fn set_result(&self, result: &str, is_error: bool) {
        if let Ok(mut i) = self.inner.lock() {
            i.result = Some(result.to_owned());
            i.state = if is_error {
                ToolState::Failed
            } else {
                ToolState::Completed
            };
            i.finished = Some(Instant::now());
        }
        self.cache.clear();
    }

    pub fn state(&self) -> ToolState {
        self.inner
            .lock()
            .map(|i| i.state)
            .unwrap_or(ToolState::Pending)
    }

    fn render_panel(&self, width: usize) -> Vec<String> {
        let Ok(inner) = self.inner.lock() else {
            return Vec::new();
        };
        let t = theme();
        let colors = t.colors;
        let width = width.max(1);
        let (icon, icon_color, bg) = match inner.state {
            ToolState::Pending => ("●", colors.muted, colors.tool_pending_bg),
            ToolState::Running => ("●", colors.accent, colors.tool_pending_bg),
            ToolState::Completed => ("✓", colors.success, colors.tool_success_bg),
            ToolState::Failed => ("✗", colors.error, colors.tool_error_bg),
        };
        // pi's ToolPanel insets.
        let padding_x = t.spacing.tool_panel_padding_x.min((width - 1) / 2);
        let content_width = width - padding_x * 2;
        let pad = " ".repeat(padding_x);
        let line = |content: &str| {
            let content = truncate_to_width(content, content_width, "");
            apply_background_to_line(&format!("{pad}{content}"), width, |s| bg.bg(s))
        };

        // pi's top spacer, then the panel's own vertical padding.
        let mut lines = vec![String::new()];
        lines.extend(std::iter::repeat_n(
            line(""),
            t.spacing.tool_panel_padding_y,
        ));

        let elapsed = match (inner.state, inner.started, inner.finished) {
            (ToolState::Running, Some(s), _) => format_elapsed(s.elapsed()),
            (ToolState::Completed | ToolState::Failed, Some(s), Some(f)) => {
                format_elapsed(f.duration_since(s))
            }
            _ => String::new(),
        };
        let mut head = vec![colors.tool_title.fg(&bold(&inner.title))];
        if !inner.summary.is_empty() {
            head.push(colors.tool_output.fg(&inner.summary));
        }
        if !elapsed.is_empty() {
            head.push(colors.muted.fg(&elapsed));
        }
        head.push(colors.muted.fg("▸"));
        // Continuation rows of a long header hang under the title, after the status glyph.
        let glyph = icon_color.fg(icon);
        // (A panel too narrow for the hang truncates its rows, as pi's does.)
        let hang = 2.min(content_width.saturating_sub(1));
        for (i, row) in wrap(&head.join(" "), content_width - hang)
            .iter()
            .enumerate()
        {
            let row = if i == 0 {
                format!("{glyph} {row}")
            } else {
                format!("{}{row}", " ".repeat(hang))
            };
            lines.push(line(&row));
        }

        if let Some(result) = &inner.result {
            let gutter = content_width > 2;
            let body_width = content_width - if gutter { 2 } else { 0 };
            let rows: Vec<String> = result
                .replace("\r\n", "\n")
                .replace('\r', "\n")
                .split('\n')
                .flat_map(|l| wrap_lines(&plain(l), "", body_width))
                .collect();
            for row in rows.iter().take(RESULT_PREVIEW_LINES) {
                let row = if gutter {
                    format!("{} {}", colors.dim.fg("│"), colors.tool_output.fg(row))
                } else {
                    colors.tool_output.fg(row)
                };
                lines.push(line(&row));
            }
            let hidden = rows.len().saturating_sub(RESULT_PREVIEW_LINES);
            if hidden > 0 {
                let hint = format!("… {hidden} more line{}", if hidden == 1 { "" } else { "s" });
                lines.push(line(&colors.muted.fg(&hint)));
            }
        }
        lines.extend(std::iter::repeat_n(
            line(""),
            t.spacing.tool_panel_padding_y,
        ));
        lines
    }
}

/// pi's elapsed format: `0.4s`, `12.0s`, `1m5s`.
fn format_elapsed(d: Duration) -> String {
    let secs = d.as_secs_f64();
    if secs < 60.0 {
        format!("{secs:.1}s")
    } else {
        format!(
            "{}m{}s",
            (secs / 60.0).floor() as u64,
            (secs % 60.0).floor() as u64
        )
    }
}

impl Component for ToolBlock {
    fn render(&self, width: usize) -> Vec<String> {
        // A running call's timer changes every frame; a finished one is fixed.
        if self.state() == ToolState::Running {
            return self.render_panel(width);
        }
        self.cache.get_or(width, || self.render_panel(width))
    }

    fn invalidate(&self) {
        self.cache.clear();
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn segments_split_prose_from_fences_tables_and_rules() {
        let src = "intro\n```rust\nlet a = 1;\n```\nmiddle\n| a | b |\n|---|---|\n| 1 | 2 |\n---\n- item\nend";
        assert_eq!(
            segments(src),
            vec![
                Segment::Prose("intro".into()),
                Segment::Block("```rust\nlet a = 1;\n```".into()),
                Segment::Prose("middle".into()),
                Segment::Table("| a | b |\n|---|---|\n| 1 | 2 |".into()),
                Segment::Block("---".into()),
                Segment::Prose("- item\nend".into()),
            ]
        );
    }

    #[test]
    fn segments_find_pipe_optional_tables_and_end_them_at_a_blank_line() {
        let src =
            "a | b\n--- | ---\n1 | 2\n| 3 | 4 |\n\nafter | pipe\n| x | y |\n|---|---|\n| z | w |";
        assert_eq!(
            segments(src),
            vec![
                Segment::Table("a | b\n--- | ---\n1 | 2\n| 3 | 4 |".into()),
                Segment::Prose("\nafter | pipe".into()),
                Segment::Table("| x | y |\n|---|---|\n| z | w |".into()),
            ]
        );
        // A one-column separator without pipes is a setext heading or a rule, not a table.
        assert_eq!(segments("a\n---").len(), 2);
    }

    #[test]
    fn markdown_tables_use_box_glyphs_padding_and_a_header_rule_only() {
        let md = WrapMarkdown::new(
            "| # | Ticket | State |\n|--:|---|---|\n| 1 | **Say hello** | `done` |\n| 22 | Plan | ready |",
            1,
            MarkdownStyle::Plain,
        );
        let rows = plain_rows(&md.render(80));
        assert_eq!(
            rows,
            vec![
                " ┌────┬───────────┬────────┐",
                " │  # │ Ticket    │ State  │",
                " ├────┼───────────┼────────┤",
                " │  1 │ Say hello │  done  │",
                " │ 22 │ Plan      │ ready  │",
                " └────┴───────────┴────────┘",
            ]
        );
    }

    #[test]
    fn table_cells_that_look_like_blocks_stay_inline() {
        for cell in ["# 1", "- a", "1. x", "> q", "---"] {
            assert_eq!(
                inline_only(cell).chars().filter(|&c| c == '\\').count(),
                1,
                "{cell}"
            );
        }
        let md = WrapMarkdown::new(
            "| a | b |\n|---|---|\n| # 1 | - x |",
            1,
            MarkdownStyle::Plain,
        );
        let rows = plain_rows(&md.render(80));
        assert_eq!(rows[3], " │ # 1 │ - x │", "{rows:#?}");
    }

    #[test]
    fn an_unclosed_fence_runs_to_the_end() {
        assert_eq!(
            segments("a\n~~~\ncode\n```\nmore"),
            vec![
                Segment::Prose("a".into()),
                Segment::Block("~~~\ncode\n```\nmore".into()),
            ]
        );
    }

    fn plain_rows(rows: &[String]) -> Vec<String> {
        rows.iter()
            .map(|r| plain(r).trim_end().to_owned())
            .collect()
    }

    #[test]
    fn tool_blocks_wrap_their_header_and_result_at_words() {
        let block = ToolBlock::new(
            "AMP · kb_search",
            "how the harness wraps transcript text in narrow terminals",
        );
        block.set_result(
            "query: how the harness wraps transcript text · results: 3 items · top: architecture/amp-harness-chat-tui.md",
            false,
        );
        let rows = plain_rows(&block.render(50));
        // pi's top spacer and the panel's own padding (1 row in the default theme).
        assert_eq!(rows[0], "");
        let body: Vec<&str> = rows
            .iter()
            .map(String::as_str)
            .filter(|r| !r.is_empty())
            .collect();
        assert!(
            body[0].starts_with("  ✓ AMP · kb_search how the harness"),
            "{body:?}"
        );
        for row in &body {
            assert!(visible_width_of(row) <= 50, "{row:?}");
        }
        // Header continuations hang under the title; result rows keep the gutter.
        assert!(
            body[1].starts_with("    ") && !body[1].starts_with("     "),
            "{body:?}"
        );
        let result: Vec<&&str> = body.iter().filter(|r| r.starts_with("  │ ")).collect();
        assert!(result.len() >= 3, "{body:?}");
        assert!(result.iter().all(|r| !r.starts_with("  │  ")), "{body:?}");
        let joined: Vec<String> = result.iter().map(|r| r[6..].to_owned()).collect();
        assert_eq!(
            joined.join(" "),
            "query: how the harness wraps transcript text · results: 3 items · top: architecture/amp-harness-chat-tui.md"
        );
    }

    fn visible_width_of(s: &str) -> usize {
        rpi_tui::visible_width(s)
    }

    #[test]
    fn long_tool_results_show_a_preview() {
        let block = ToolBlock::new("read", "src/lib.rs");
        let text: Vec<String> = (1..=20).map(|i| format!("line {i}")).collect();
        block.set_result(&text.join("\n"), false);
        let rows = plain_rows(&block.render(60));
        assert!(
            rows.iter().any(|r| r.trim() == "… 8 more lines"),
            "{rows:?}"
        );
        assert!(!rows.iter().any(|r| r.contains("line 13")), "{rows:?}");
    }

    // #2177: thinking.

    const THOUGHT: &str = "## Plan\nFirst I **check** the guide. Then I write the `ticket` and ask";

    #[test]
    fn live_thinking_is_one_line_with_the_latest_sentence() {
        let block = AssistantBlock::new();
        block.update_blocks(&[Block::Thinking(THOUGHT.into())]);
        let rows = plain_rows(&block.render(80));
        assert_eq!(
            rows,
            vec![" Thinking · 0s ▸ Then I write the ticket and ask"]
        );

        // The answer starts: the line collapses to its duration and the timer stops.
        block.update_blocks(&[
            Block::Thinking(THOUGHT.into()),
            Block::Text("The answer.".into()),
        ]);
        let rows = plain_rows(&block.render(80));
        assert_eq!(rows, vec![" Thinking · 0s ▸", "", " The answer."]);
        let thinking = block.thinking.lock().unwrap()[0].clone();
        assert!(!thinking.is_live());
    }

    #[test]
    fn the_latest_sentence_keeps_its_end_when_narrow() {
        let block = ThinkingBlock::new(ThinkingDisplay::Collapsed, THOUGHT);
        let rows = plain_rows(&block.render(30));
        assert_eq!(rows, vec![" Thinking · 0s ▸ …cket and ask"]);
        assert!(rpi_tui::visible_width(&block.render(30)[0]) <= 30);
        assert_eq!(latest_sentence("- one. two? **three"), "three");
        assert_eq!(latest_sentence("a.\n\n"), "a.");
    }

    #[test]
    fn expanded_thinking_is_plain_dim_text_behind_a_gutter() {
        let block = AssistantBlock::with_thinking(ThinkingDisplay::Full);
        block.update_blocks(&[
            Block::Thinking(THOUGHT.into()),
            Block::Text("## Answer\nThe **answer**.".into()),
        ]);
        block.finish();
        let styled = block.render(80);
        let rows = plain_rows(&styled);
        // Seen complete at once: no duration.
        assert_eq!(rows[0], " Thinking ▾");
        // Markdown stays literal text: no heading, no bold.
        assert_eq!(rows[1], " │ ## Plan");
        assert_eq!(
            rows[2],
            " │ First I **check** the guide. Then I write the `ticket` and ask"
        );
        let thinking_rows = &styled[1..3];
        for row in thinking_rows {
            assert!(!row.contains("\x1b[1m"), "bold in thinking: {row:?}");
            assert!(
                row.contains(&theme().colors.thinking_text.fg("x")[..4]),
                "{row:?}"
            );
        }
        // The answer below is markdown, without the gutter.
        let answer: Vec<&String> = rows.iter().skip(3).filter(|r| !r.is_empty()).collect();
        assert!(answer.iter().all(|r| !r.contains('│')), "{rows:?}");
        assert!(
            styled.iter().skip(3).any(|r| r.contains("\x1b[1m")),
            "{styled:?}"
        );
    }

    #[test]
    fn toggling_expands_and_collapses_the_last_section() {
        let block = AssistantBlock::new();
        assert_eq!(block.toggle_thinking(), None);
        block.update_blocks(&[
            Block::Thinking("one".into()),
            Block::Text("a".into()),
            Block::Thinking("two".into()),
            Block::Text("b".into()),
        ]);
        assert_eq!(block.toggle_thinking(), Some(true));
        let rows = plain_rows(&block.render(40));
        assert_eq!(
            rows,
            vec![" Thinking ▸", "", " a", " Thinking ▾", " │ two", "", " b"]
        );
        assert_eq!(block.toggle_thinking(), Some(false));
        assert!(!plain_rows(&block.render(40)).contains(&" │ two".to_owned()));
    }

    #[test]
    fn hidden_thinking_renders_nothing_until_toggled() {
        let block = AssistantBlock::with_thinking(ThinkingDisplay::Hidden);
        block.update_blocks(&[Block::Thinking("why".into()), Block::Text("a".into())]);
        assert_eq!(plain_rows(&block.render(40)), vec![" a"]);
        block.toggle_thinking();
        assert_eq!(
            plain_rows(&block.render(40)),
            vec![" Thinking ▾", " │ why", "", " a"]
        );
    }

    #[test]
    fn a_message_seen_complete_has_no_duration() {
        let block = AssistantBlock::new();
        block.finish();
        block.update_blocks(&[Block::Thinking("why".into()), Block::Text("a".into())]);
        assert_eq!(plain_rows(&block.render(40))[0], " Thinking ▸");
    }

    #[test]
    fn show_thinking_values() {
        assert_eq!(ThinkingDisplay::parse("FULL"), Ok(ThinkingDisplay::Full));
        assert_eq!(ThinkingDisplay::parse(""), Ok(ThinkingDisplay::Collapsed));
        assert_eq!(ThinkingDisplay::parse("hidden").unwrap().word(), "hidden");
        assert!(ThinkingDisplay::parse("on").is_err());
    }

    #[test]
    fn list_markers_are_not_rules() {
        assert!(!is_rule("- --- not a rule"));
        assert!(!is_rule("* ** x"));
        assert!(is_rule("---"));
        assert!(is_rule("  ***"));
    }
}
