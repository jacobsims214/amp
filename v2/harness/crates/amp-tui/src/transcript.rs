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
//!   repeated. Inline code chips lose rpi's space padding: the chip's background alone marks
//!   it, so no gap shows before punctuation (#2188). Fenced code is drawn here the way rpi draws
//!   it (rpi's highlighter, its surface and indent), hard-wrapped by [`crate::wrap::hard_wrap`]
//!   so a long line still copies whole: rpi's own wrap replays a truecolor colour's `r;g;b` as
//!   separate SGR codes on the continuation row (dim, a bright background...), #2188. Rules
//!   and Mermaid keep rpi's rendering at the real width. Tables are drawn by [`crate::table`]
//!   (#2180): box glyphs, padded cells, wrapped to fit or listed per row.
//! - [`AssistantBlock`]: an assistant message's text and thinking blocks, and its error line.
//! - [`ThinkingBlock`]: a thinking section, collapsed to one live `▸ Thinking · 18s` line by
//!   default (#2177), `▸ Thought for 18s` once it ends (#2345); expanded, plain dim text behind
//!   a `│` gutter ([`ThinkingDisplay`]). A click on its line or Ctrl+T opens and closes it.
//! - [`UserBlock`]: the user's message on its surface; plain text, shown exactly as typed
//!   (#2350), with no OSC 133 prompt markers (#2357).
//! - [`ToolBlock`]: a tool call on one line (status glyph, title, argument, the result's
//!   summary, `12 ms`), its details under it when expanded (Ctrl+O, [`ToolDisplay`]); a failed
//!   call shows its error. [`ToolGroup`] holds consecutive calls as one block (#2183).

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
use crate::wrap::{hard_wrap, open_sgr as open_style, plain, reflow_row};

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
    /// The blank rows above it too (else only below).
    padding_top: bool,
    /// The blank rows below it too (else only above, #2346).
    padding_bottom: bool,
    cache: Cache,
}

impl WrapText {
    pub fn new(text: impl Into<String>, padding_x: usize, padding_y: usize) -> Self {
        Self {
            text: text.into(),
            padding_x,
            padding_y,
            padding_top: true,
            padding_bottom: true,
            cache: Cache::default(),
        }
    }

    /// Without the blank rows above, when what comes before already ends with one (#2334).
    pub fn with_top_padding(mut self, top: bool) -> Self {
        self.padding_top = top;
        self
    }

    /// Without the blank rows below (compact notices, #2346).
    pub fn with_bottom_padding(mut self, bottom: bool) -> Self {
        self.padding_bottom = bottom;
        self
    }
}

impl Component for WrapText {
    fn render(&self, width: usize) -> Vec<String> {
        self.cache.get_or(width, || {
            let above = if self.padding_top { self.padding_y } else { 0 };
            let mut lines = vec![String::new(); above];
            let content_width = width.saturating_sub(self.padding_x).max(1);
            lines.extend(wrap_lines(
                &self.text,
                &" ".repeat(self.padding_x),
                content_width,
            ));
            if self.padding_bottom {
                lines.extend(std::iter::repeat_n(String::new(), self.padding_y));
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
                .map(|r| unpad_code_chips(&r))
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

impl WrapMarkdown {
    /// Fenced code, rules and Mermaid at `width`. Code fences (not Mermaid) are drawn by
    /// [`code_rows`]; the rest is left to rpi's `Markdown`.
    fn block(&self, src: &str, width: usize) -> Vec<String> {
        let lines: Vec<&str> = src.lines().collect();
        let mut out = Vec::new();
        let mut other: Vec<&str> = Vec::new();
        let flush = |other: &mut Vec<&str>, out: &mut Vec<String>| {
            if !other.is_empty() {
                out.extend(self.markdown(other.join("\n")).render(width));
                other.clear();
            }
        };
        let mut i = 0;
        while i < lines.len() {
            let line = lines[i];
            let Some(ch) = fence(line) else {
                other.push(line);
                i += 1;
                continue;
            };
            let lang = line.trim_start()[3..].trim().to_ascii_lowercase();
            // The block runs to the matching fence (or the end, while it streams).
            let mut end = i + 1;
            while end < lines.len() && fence(lines[end]) != Some(ch) {
                end += 1;
            }
            let close = (end + 1).min(lines.len());
            if lang == "mermaid" {
                other.extend_from_slice(&lines[i..close]);
            } else {
                flush(&mut other, &mut out);
                out.extend(code_rows(&lang, &lines[i + 1..end], width, self.padding_x));
            }
            i = close;
        }
        flush(&mut other, &mut out);
        out
    }
}

/// rpi's `code_block_indent` (Markdown's default).
const CODE_INDENT: usize = 2;

/// A fenced code block's rows, as rpi's `Markdown` draws them (`push_code_line`): each line
/// highlighted by rpi's highlighter, then `pad` + indent + the code surface (one cell of
/// inset, filled to the width). A line wider than the surface is hard-wrapped by
/// [`hard_wrap`], which re-opens truecolor styles whole on the continuation row (rpi 0.8.0's
/// `wrap_text_with_ansi` replays `38;2;r;g;b` as separate codes there, #2188). Streamed
/// partial closing fences (a line of 1-2 fence characters) are skipped, as rpi does.
fn code_rows(lang: &str, code: &[&str], width: usize, padding_x: usize) -> Vec<String> {
    let colors = theme().colors;
    let mut highlighter = rpi_tui::syntax_highlight::Highlighter::new(lang);
    let pad = " ".repeat(padding_x);
    let indent = " ".repeat(CODE_INDENT);
    let cwidth = width.saturating_sub(padding_x * 2);
    let panel_width = cwidth.saturating_sub(CODE_INDENT).max(1);
    let body_width = panel_width.saturating_sub(2).max(1);
    let mut out = Vec::new();
    for line in code {
        let t = line.trim();
        if !t.is_empty() && t.len() <= 2 && t.chars().all(|c| c == '`' || c == '~') {
            continue;
        }
        let expanded = line.replace('\t', "    ");
        let styled = highlighter.highlight(&expanded, &colors);
        for chunk in hard_wrap(&styled, body_width) {
            let body = apply_background_to_line(&format!(" {chunk}"), panel_width, |text| {
                colors.md_code_block_bg.bg(text)
            });
            out.push(format!("{pad}{indent}{body}"));
        }
    }
    out
}

/// Drops the space rpi's `Markdown` puts on each side of an inline code chip
/// (`bg(" " + fg(code) + " ")`), so the chip is marked by its background alone and no gap shows
/// before the punctuation after it (`amp_check_plan:`), #2188.
pub fn unpad_code_chips(row: &str) -> String {
    let colors = theme().colors;
    let bg = colors.md_code_bg.to_bg();
    let fg = colors.md_code.to_fg();
    if !row.contains(&bg) {
        return row.to_owned();
    }
    row.replace(&format!("{bg} {fg}"), &format!("{bg}{fg}"))
        .replace(&format!("\x1b[0m{bg} \x1b[0m"), "\x1b[0m")
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
                        let s = if self.style == MarkdownStyle::Plain && crate::density::compact() {
                            compact_prose(&s)
                        } else {
                            s
                        };
                        for row in self.markdown(s).render(UNBOUNDED) {
                            lines.extend(reflow_row(&unpad_code_chips(&row), text_width));
                        }
                    }
                    Segment::Block(s) => lines.extend(self.block(&s, width)),
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

/// Compact prose (#2346): headings of every level render as bold text (`#`/`##` lose rpi's
/// underline: they become `###`), with no blank line under them, and a run of blank lines
/// shows as one.
fn compact_prose(src: &str) -> String {
    let mut out: Vec<String> = Vec::new();
    let mut after_heading = false;
    let mut blank_run = false;
    for line in src.lines() {
        if line.trim().is_empty() {
            if after_heading || blank_run {
                continue;
            }
            blank_run = true;
            out.push(String::new());
            continue;
        }
        blank_run = false;
        let t = line.trim_start();
        let hashes = t.bytes().take_while(|&b| b == b'#').count();
        let heading = (1..=6).contains(&hashes) && t[hashes..].starts_with(' ');
        after_heading = heading;
        if heading && hashes < 3 {
            out.push(format!("###{}", &t[hashes..]));
        } else {
            out.push(line.to_owned());
        }
    }
    out.join("\n")
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
    /// One line per thinking section: `▸ Thinking · 18s`, with the latest sentence while it
    /// streams, then `▸ Thought for 18s`. A click or Ctrl+T opens one.
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
    /// When it started (`None`: not known, #2355).
    started: Option<Instant>,
    ended: Option<Instant>,
    /// Whether it was seen streaming with known times (a replayed message has no duration to
    /// show).
    timed: bool,
    /// A blank row after it (something visible follows).
    spacer: bool,
    /// How it shows (`/config thinking` changes it for blocks already in the transcript, #2345).
    display: ThinkingDisplay,
    /// The transcript's last thinking: collapsed, its header says how to open it
    /// ([`THINKING_HINT`]).
    hint: Option<&'static str>,
}

/// What the transcript's last collapsed thinking header says after its duration (#2345).
pub const THINKING_HINT: &str = "Ctrl+T or click to open";
/// [`THINKING_HINT`] with mouse capture off (`[tui] mouse = false`): clicks don't reach the chat.
pub const THINKING_HINT_KEY: &str = "Ctrl+T to open";

/// One thinking section of an assistant message (#2177). Collapsed it is one dim line,
/// `▸ Thinking · 18s`, followed while it streams by the latest sentence, and `▸ Thought for 18s`
/// once it ends (#2345); expanded (`▾`), the text follows as plain dim rows behind a `│` gutter,
/// with no markdown styling, so it never reads like the answer. A click on the header, or Ctrl+T,
/// opens and closes it.
pub struct ThinkingBlock {
    state: Mutex<ThinkingState>,
    expanded: AtomicBool,
    /// The expanded body's rows.
    cache: Cache,
}

impl ThinkingBlock {
    /// Left padding, as the assistant's text has.
    const PAD: usize = 1;

    pub fn new(display: ThinkingDisplay, text: impl Into<String>) -> Self {
        Self::new_at(display, text, Some(Instant::now()))
    }

    /// A section that started `at` (`None`: not known, so it shows no duration; #2355).
    pub fn new_at(display: ThinkingDisplay, text: impl Into<String>, at: Option<Instant>) -> Self {
        Self {
            state: Mutex::new(ThinkingState {
                text: text.into(),
                started: at,
                ended: None,
                timed: true,
                spacer: false,
                display,
                hint: None,
            }),
            expanded: AtomicBool::new(display == ThinkingDisplay::Full),
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
        self.end_at(timed, Some(Instant::now()));
    }

    /// [`Self::end`] at `at` (`None`: not known, so no duration; #2355).
    pub fn end_at(&self, timed: bool, at: Option<Instant>) {
        if let Ok(mut s) = self.state.lock()
            && s.ended.is_none()
        {
            s.ended = Some(at.unwrap_or_else(Instant::now));
            s.timed = timed && at.is_some();
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

    /// A new display mode (`/config thinking`, #2345): it opens for `full`, closes otherwise.
    pub fn set_display(&self, display: ThinkingDisplay) {
        if let Ok(mut s) = self.state.lock() {
            s.display = display;
        }
        self.expanded
            .store(display == ThinkingDisplay::Full, Ordering::Relaxed);
    }

    /// What its collapsed header says about opening it, if anything.
    pub fn set_hint(&self, hint: Option<&'static str>) {
        if let Ok(mut s) = self.state.lock() {
            s.hint = hint;
        }
    }

    /// Expands or collapses it; returns whether it is now expanded.
    pub fn toggle(&self) -> bool {
        !self.expanded.fetch_xor(true, Ordering::Relaxed)
    }

    pub fn is_expanded(&self) -> bool {
        self.expanded.load(Ordering::Relaxed)
    }

    /// `Thinking · 18s` while it streams, `Thought for 18s` after, `Thinking` without a duration.
    fn label(s: &ThinkingState) -> String {
        let Some(started) = s.started.filter(|_| s.timed) else {
            return "Thinking".to_owned();
        };
        let secs = s
            .ended
            .unwrap_or_else(Instant::now)
            .saturating_duration_since(started)
            .as_secs();
        if s.ended.is_some() {
            format!("Thought for {secs}s")
        } else {
            format!("Thinking · {secs}s")
        }
    }

    fn header(&self, s: &ThinkingState, width: usize) -> String {
        let colors = theme().colors;
        let expanded = self.is_expanded();
        let label = Self::label(s);
        let glyph = if expanded { "▾" } else { "▸" };
        let pad = " ".repeat(Self::PAD);
        let head = format!("{glyph} {label}");
        let room = width.saturating_sub(Self::PAD + rpi_tui::visible_width(&head) + 1);
        let tail = if s.ended.is_none() && !expanded {
            let latest = tail_to_width(&latest_sentence(&s.text), room);
            (!latest.is_empty()).then(|| colors.thinking_text.fg(&latest))
        } else {
            s.hint
                .filter(|h| !expanded && room >= rpi_tui::visible_width(h) + 2)
                .map(|hint| colors.dim.fg(&format!("· {hint}")))
        };
        let mut row = format!(
            "{pad}{} {}",
            colors.muted.fg(glyph),
            colors.thinking_text.fg(&label)
        );
        if let Some(tail) = tail {
            row.push(' ');
            row.push_str(&tail);
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
        let Ok(s) = self.state.lock() else {
            return Vec::new();
        };
        let shown = s.display != ThinkingDisplay::Hidden || self.is_expanded();
        if !shown || s.text.trim().is_empty() {
            return Vec::new();
        }
        let mut rows = vec![self.header(&s, width)];
        if self.is_expanded() {
            rows.extend(self.body(&s.text, width));
        }
        // Compact (#2346): the answer follows a collapsed header straight away.
        if s.spacer && (self.is_expanded() || !crate::density::compact()) {
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
    display: Mutex<ThinkingDisplay>,
    /// One per thinking section, kept across rebuilds (they hold its timer and expansion).
    thinking: Mutex<Vec<Arc<ThinkingBlock>>>,
    finished: AtomicBool,
    /// It follows a tool group: a blank row above it when it shows anything (#2183).
    gap_above: AtomicBool,
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
            display: Mutex::new(display),
            thinking: Mutex::new(Vec::new()),
            finished: AtomicBool::new(false),
            gap_above: AtomicBool::new(false),
        }
    }

    pub fn update_blocks(&self, blocks: &[Block]) {
        self.update_blocks_at(blocks, Some(Instant::now()));
    }

    /// [`Self::update_blocks`] for an update that happened `at` (a session caught up from its
    /// feed or file, #2355; `None`: not known).
    pub fn update_blocks_at(&self, blocks: &[Block], at: Option<Instant>) {
        if let Ok(mut b) = self.blocks.lock() {
            if *b == blocks {
                return;
            }
            *b = blocks.to_vec();
        }
        self.rebuild(blocks, at);
    }

    /// It comes right after a tool group: a blank row separates them when it shows anything
    /// (the group itself ends at its last call, #2183).
    pub fn set_gap_above(&self) {
        self.gap_above.store(true, Ordering::Relaxed);
    }

    /// The message is complete: its thinking stops its timers.
    pub fn finish(&self) {
        self.finish_at(Some(Instant::now()));
    }

    /// [`Self::finish`] for a message that ended `at` (#2355; `None`: not known).
    pub fn finish_at(&self, at: Option<Instant>) {
        self.finished.store(true, Ordering::Relaxed);
        if let Ok(t) = self.thinking.lock() {
            for block in t.iter() {
                block.end_at(true, at);
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
        let last = self.thinking.lock().ok()?.len().checked_sub(1)?;
        self.toggle_section(last)
    }

    /// Expands or collapses its thinking section `index` (a click on its header, #2345).
    pub fn toggle_section(&self, index: usize) -> Option<bool> {
        let block = self.thinking.lock().ok()?.get(index).cloned()?;
        let expanded = block.toggle();
        self.content.invalidate();
        Some(expanded)
    }

    /// Shows its thinking, and any section added later, as `display` (`/config thinking`,
    /// #2345): every section opens for `full` and closes otherwise.
    pub fn set_display(&self, display: ThinkingDisplay) {
        if let Ok(mut d) = self.display.lock() {
            *d = display;
        }
        if let Ok(t) = self.thinking.lock() {
            for block in t.iter() {
                block.set_display(display);
            }
        }
        self.content.invalidate();
    }

    /// What its last section's collapsed header says about opening it (the transcript's last
    /// thinking, #2345); the others say nothing.
    pub fn set_hint(&self, hint: Option<&'static str>) {
        if let Ok(t) = self.thinking.lock() {
            let last = t.len().saturating_sub(1);
            for (i, block) in t.iter().enumerate() {
                block.set_hint(hint.filter(|_| i == last));
            }
        }
    }

    /// The rows (of [`Component::render`] at `width`) holding a thinking header, with its
    /// section's index (#2345): what a click maps to.
    pub fn thinking_headers(&self, width: usize) -> Vec<(usize, usize)> {
        let Ok(thinking) = self.thinking.lock().map(|t| t.clone()) else {
            return Vec::new();
        };
        let children = self.content.get_children();
        let mut rows = Vec::new();
        let mut at = 0;
        for child in &children {
            let rendered = child.render(width);
            if !rendered.is_empty()
                && let Some(index) = thinking
                    .iter()
                    .position(|t| std::ptr::addr_eq(Arc::as_ptr(t), Arc::as_ptr(child)))
            {
                rows.push((at, index));
            }
            at += rendered.len();
        }
        // The blank row above it (after a tool group) moves everything down one.
        let first_shown = children
            .iter()
            .flat_map(|c| c.render(width))
            .next()
            .is_some_and(|r| !r.trim().is_empty());
        if self.gap_above.load(Ordering::Relaxed) && first_shown {
            for row in &mut rows {
                row.0 += 1;
            }
        }
        rows
    }

    fn rebuild(&self, blocks: &[Block], at: Option<Instant>) {
        self.content.clear();
        let visible = |b: &Block| match b {
            Block::Text(t) | Block::Thinking(t) => !t.trim().is_empty(),
        };
        let finished = self.finished.load(Ordering::Relaxed);
        let display = self.display.lock().map(|d| *d).unwrap_or_default();
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
                            let b = Arc::new(ThinkingBlock::new_at(display, text, at));
                            // Seen complete at once (a replayed message): no duration.
                            if followed || finished {
                                b.end_at(false, at);
                            }
                            thinking.push(Arc::clone(&b));
                            b
                        }
                    };
                    if followed || finished {
                        block.end_at(true, at);
                    }
                    block.set_spacer(followed);
                    self.content.add_child(block);
                    section += 1;
                }
            }
        }
        thinking.truncate(section);
    }

    /// Replaces the content with the error line: pi's (`❌ Error: …`), with the emoji mapped to
    /// the theme's `✗` in the error colour (#2188).
    pub fn update_error(&self, error: &str, stop_reason: &str) {
        if let Ok(mut b) = self.blocks.lock() {
            b.clear();
        }
        if let Ok(mut t) = self.thinking.lock() {
            t.clear();
        }
        self.content.clear();
        self.content.add_child(Arc::new(Spacer::new(1)));
        let colors = theme().colors;
        let text = match stop_reason {
            "aborted" => colors.error.fg(&format!("✗ Operation aborted: {error}")),
            "error" => colors.error.fg(&format!("✗ Error: {error}")),
            "length" => colors
                .warning
                .fg("! Response was truncated before completion."),
            _ => colors.error.fg(&format!("✗ {error}")),
        };
        self.content
            .add_child(Arc::new(WrapText::new(text, Self::PAD, 0)));
    }
}

impl Component for AssistantBlock {
    fn render(&self, width: usize) -> Vec<String> {
        let mut rows = self.content.render(width);
        if self.gap_above.load(Ordering::Relaxed)
            && rows.first().is_some_and(|r| !r.trim().is_empty())
        {
            rows.insert(0, String::new());
        }
        rows
    }

    fn invalidate(&self) {
        let blocks = self.blocks.lock().map(|b| b.clone()).unwrap_or_default();
        if !blocks.is_empty() {
            self.rebuild(&blocks, Some(Instant::now()));
        }
        self.content.invalidate();
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// The user's message: plain text on the theme's surface colour inside a padded box,
/// word-wrapped. It is shown exactly as typed, not as markdown (#2350): paths like
/// `mathx/__init__.py`, `snake_case` and `*.rs` globs are what engineers type, and emphasis
/// parsing would change them on screen.
///
/// No OSC 133 prompt-zone markers (pi's `UserMessageComponent` has them, #2357): `133;A` means
/// "prompt start, fresh-line", and iTerm2 (by default) and other terminals following the
/// semantic-prompts spec answer it with CR+LF when the cursor isn't in column 0. A frame row
/// never starts the marker there (the inset's padding comes first, and the differ redraws from
/// mid-row), so the bubble's first row landed one row down and was drawn over: in compact
/// density that row is the first line of the message, which vanished. On the alternate screen
/// the marks have no use (no scrollback to jump through), so they are not sent at all.
pub struct UserBlock {
    text: String,
    inner: Mutex<(u64, BgBox)>,
    /// A blank row above the bubble, when what comes before it doesn't end with one (#2188).
    margin_above: bool,
}

fn user_box(text: &str) -> BgBox {
    let bg = theme().colors.surface;
    // Compact (#2346): no padding rows above and below the text.
    let content = BgBox::new(1, if crate::density::compact() { 0 } else { 1 });
    content.set_bg_fn(Some(Arc::new(move |s: &str| bg.bg(s))));
    content.add_child(Arc::new(WrapText::new(verbatim(text), 0, 0)));
    content
}

/// The user's text as it shows: tabs as spaces, and control characters (an escape sequence
/// pasted in) dropped so they can't restyle the terminal; everything else as typed.
fn verbatim(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.replace("\r\n", "\n").chars() {
        match c {
            '\t' => out.push_str("    "),
            '\n' => out.push('\n'),
            c if c.is_control() => {}
            c => out.push(c),
        }
    }
    out
}

impl UserBlock {
    pub fn new(text: impl Into<String>) -> Self {
        let text = text.into();
        let inner = Mutex::new((theme_revision(), user_box(&text)));
        Self {
            text,
            inner,
            margin_above: false,
        }
    }

    /// Puts a blank row above the bubble (see [`crate::view::ChatView::add_user`]).
    pub fn with_margin_above(mut self, margin: bool) -> Self {
        self.margin_above = margin;
        self
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
        if self.margin_above && !lines.is_empty() {
            lines.insert(0, String::new());
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

/// Result rows shown under an expanded call (pi's preview).
const RESULT_PREVIEW_LINES: usize = 12;
/// Error rows shown under a failed call while collapsed.
const ERROR_PREVIEW_LINES: usize = 4;
/// A group longer than this folds its earliest successful calls into one line.
const GROUP_FOLD_AFTER: usize = 5;
/// The calls a folded group still shows one by one, at its end.
const GROUP_KEEP_LAST: usize = 3;

/// How tool calls show, shared by every call in the transcript: one line each (the default), or
/// with their arguments and result (Ctrl+O, pi's `app.tools.expand`).
#[derive(Debug, Default)]
pub struct ToolDisplay {
    expanded: AtomicBool,
}

impl ToolDisplay {
    pub fn is_expanded(&self) -> bool {
        self.expanded.load(Ordering::Relaxed)
    }

    /// Flips expanded/collapsed; returns whether calls are now expanded.
    pub fn toggle(&self) -> bool {
        !self.expanded.fetch_xor(true, Ordering::Relaxed)
    }
}

struct ToolInner {
    title: String,
    /// The call's most telling argument (one line), shown muted after the title.
    args: String,
    /// The call's whole arguments (compact JSON), shown when expanded.
    args_detail: String,
    state: ToolState,
    started: Option<Instant>,
    finished: Option<Instant>,
    /// What the result says, in one line (`3 tickets: 1 ready, 1 waiting`).
    summary: String,
    /// The result itself: the error's text, or what the expanded view shows.
    detail: Option<String>,
}

/// A tool call's rendered rows, for (width, theme revision, expanded).
type ToolRows = (usize, u64, bool, Vec<String>);

/// One tool call, on one line (#2183):
///
/// ```text
///  ✓ list_tasks · 3 tickets: 1 ready, 1 waiting · 12 ms
///  ● get_task #67 · 2s                                  (running: on the pending surface)
///  ✗ complete_task #2078                                (failed: on the error surface,
///    │ the plan was last approved before #2078 …         with the error under it)
/// ```
///
/// Expanded ([`ToolDisplay`], Ctrl+O), the arguments and up to 12 result rows follow behind a
/// `│` gutter, word-wrapped. Calls in a row share one [`ToolGroup`].
pub struct ToolBlock {
    inner: Mutex<ToolInner>,
    display: Arc<ToolDisplay>,
    cache: Mutex<Option<ToolRows>>,
    /// A row under the line drawn fresh on every frame (#2347: what a dispatched worker is for
    /// and how it is doing).
    live_row: Mutex<Option<LiveRow>>,
}

/// [`ToolBlock::set_live_row`]'s source: the row's text at a width, or `None` for no row.
pub type LiveRow = Arc<dyn Fn(usize) -> Option<String> + Send + Sync>;

impl ToolBlock {
    /// `title` is shown bold; `args` is the call's most telling argument (one line).
    pub fn new(title: impl Into<String>, args: impl Into<String>) -> Self {
        Self::with_display(title, args, Arc::new(ToolDisplay::default()))
    }

    /// A call that expands and collapses with `display` (the transcript's, shared).
    pub fn with_display(
        title: impl Into<String>,
        args: impl Into<String>,
        display: Arc<ToolDisplay>,
    ) -> Self {
        Self {
            inner: Mutex::new(ToolInner {
                title: title.into(),
                args: args.into(),
                args_detail: String::new(),
                state: ToolState::Pending,
                started: None,
                finished: None,
                summary: String::new(),
                detail: None,
            }),
            display,
            cache: Mutex::new(None),
            live_row: Mutex::new(None),
        }
    }

    /// A row under the call's line, drawn fresh every frame behind a `│` gutter (#2347).
    pub fn set_live_row(&self, row: LiveRow) {
        *self.live_row.lock().unwrap_or_else(|e| e.into_inner()) = Some(row);
    }

    /// The live row at `width` (with its gutter), if there is one now.
    fn live_rows(&self, width: usize) -> Option<String> {
        let row = self
            .live_row
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()?;
        let indent = 3.min(width.saturating_sub(1));
        let text = row(width.saturating_sub(indent + 2).max(1))?;
        let colors = theme().colors;
        Some(truncate_to_width(
            &format!("{}{} {text}", " ".repeat(indent), colors.dim.fg("│")),
            width,
            "",
        ))
    }

    /// The whole arguments, shown when expanded.
    pub fn set_args_detail(&self, detail: impl Into<String>) {
        if let Ok(mut i) = self.inner.lock() {
            i.args_detail = detail.into();
        }
        self.clear_cache();
    }

    pub fn set_running(&self) {
        self.set_running_at(Some(Instant::now()));
    }

    /// The call started `at` (a session caught up from its feed or file, #2355; `None`: not
    /// known, so it shows no duration).
    pub fn set_running_at(&self, at: Option<Instant>) {
        if let Ok(mut i) = self.inner.lock() {
            i.state = ToolState::Running;
            i.started = at;
        }
        self.clear_cache();
    }

    /// The call ended: `summary` goes on its line, `detail` (the result, or the error) under it
    /// when expanded, and always for an error.
    pub fn set_result(&self, summary: &str, detail: &str, is_error: bool) {
        self.set_result_at(summary, detail, is_error, Some(Instant::now()));
    }

    /// [`Self::set_result`] for a call that ended `at` (#2355; `None`: not known, so no
    /// duration rather than a wrong one).
    pub fn set_result_at(&self, summary: &str, detail: &str, is_error: bool, at: Option<Instant>) {
        if let Ok(mut i) = self.inner.lock() {
            i.summary = summary.to_owned();
            i.detail = Some(detail.to_owned());
            i.state = if is_error {
                ToolState::Failed
            } else {
                ToolState::Completed
            };
            i.finished = at;
        }
        self.clear_cache();
    }

    pub fn state(&self) -> ToolState {
        self.inner
            .lock()
            .map(|i| i.state)
            .unwrap_or(ToolState::Pending)
    }

    /// The tool's name (the title's first word): what a folded group counts.
    fn kind(&self) -> String {
        self.inner
            .lock()
            .map(|i| i.title.split_whitespace().next().unwrap_or("").to_owned())
            .unwrap_or_default()
    }

    /// How long the call took, once it has ended.
    fn took(&self) -> Option<Duration> {
        let i = self.inner.lock().ok()?;
        Some(i.finished?.saturating_duration_since(i.started?))
    }

    fn clear_cache(&self) {
        if let Ok(mut c) = self.cache.lock() {
            *c = None;
        }
    }

    /// The call's rows: its line, then its error or (expanded) its details.
    pub fn rows(&self, width: usize) -> Vec<String> {
        let mut rows = self.cached_rows(width);
        if let Some(live) = self.live_rows(width) {
            rows.insert(1.min(rows.len()), live);
        }
        rows
    }

    fn cached_rows(&self, width: usize) -> Vec<String> {
        let expanded = self.display.is_expanded();
        // A running call's timer changes every frame; a finished one is fixed.
        if self.state() == ToolState::Running {
            return self.render_rows(width, expanded);
        }
        let revision = theme_revision();
        if let Ok(c) = self.cache.lock()
            && let Some((w, r, e, rows)) = c.as_ref()
            && (*w, *r, *e) == (width, revision, expanded)
        {
            return rows.clone();
        }
        let rows = self.render_rows(width, expanded);
        if let Ok(mut c) = self.cache.lock() {
            *c = Some((width, revision, expanded, rows.clone()));
        }
        rows
    }

    fn render_rows(&self, width: usize, expanded: bool) -> Vec<String> {
        let Ok(inner) = self.inner.lock() else {
            return Vec::new();
        };
        let colors = theme().colors;
        let width = width.max(1);
        let (icon, icon_color, bg) = match inner.state {
            ToolState::Pending => ("●", colors.muted, Some(colors.tool_pending_bg)),
            ToolState::Running => ("●", colors.accent, Some(colors.tool_pending_bg)),
            ToolState::Completed => ("✓", colors.success, None),
            ToolState::Failed => ("✗", colors.error, Some(colors.tool_error_bg)),
        };
        // The surface colour only where it means something: running and failed calls.
        let paint = |content: &str| {
            let content = truncate_to_width(content, width, "");
            match bg {
                Some(bg) => apply_background_to_line(&content, width, |s| bg.bg(s)),
                None => content,
            }
        };

        let elapsed = match (inner.state, inner.started, inner.finished) {
            (ToolState::Running, Some(s), _) => running_elapsed(s.elapsed()),
            (ToolState::Completed | ToolState::Failed, Some(s), Some(f)) => {
                format_elapsed(f.saturating_duration_since(s))
            }
            _ => String::new(),
        };
        // Compact (#2346): a failed call is one line too, its error's first line on it; the
        // whole error shows with Ctrl+O.
        let error_inline =
            inner.state == ToolState::Failed && !expanded && crate::density::compact();
        let first_error = inner
            .detail
            .as_deref()
            .and_then(|d| d.lines().map(str::trim).find(|l| !l.is_empty()))
            .map(plain)
            .unwrap_or_default();
        let summary = match inner.state {
            // A failed call's error is under it, whole.
            ToolState::Completed => inner.summary.as_str(),
            ToolState::Failed if error_inline => first_error.as_str(),
            _ => "",
        };
        let mut rows = vec![paint(&tool_line(
            &ToolLine {
                glyph: &icon_color.fg(icon),
                title: &inner.title,
                args: &inner.args,
                summary,
                error: error_inline,
                elapsed: &elapsed,
            },
            width,
        ))];

        // Rows behind a `│` gutter under the line, indented to the title.
        let gutter_rows = |text: &str, limit: usize, style: &dyn Fn(&str) -> String| {
            let indent = 3.min(width.saturating_sub(1));
            let body_width = width.saturating_sub(indent + 2).max(1);
            let all: Vec<String> = text
                .replace("\r\n", "\n")
                .replace('\r', "\n")
                .split('\n')
                .flat_map(|l| wrap_lines(&plain(l), "", body_width))
                .collect();
            let mut out: Vec<String> = all
                .iter()
                .take(limit)
                .map(|row| {
                    format!(
                        "{}{} {}",
                        " ".repeat(indent),
                        colors.dim.fg("│"),
                        style(row)
                    )
                })
                .collect();
            let hidden = all.len().saturating_sub(limit);
            if hidden > 0 {
                let hint = format!(
                    "… {hidden} more line{}{}",
                    if hidden == 1 { "" } else { "s" },
                    if expanded { "" } else { " (Ctrl+O)" }
                );
                out.push(format!("{}{}", " ".repeat(indent), colors.muted.fg(&hint)));
            }
            out
        };
        let output = |s: &str| colors.tool_output.fg(s);
        let error = |s: &str| colors.error.fg(s);
        if expanded && !inner.args_detail.is_empty() {
            for row in gutter_rows(&inner.args_detail, 4, &|s| colors.muted.fg(s)) {
                rows.push(paint(&row));
            }
        }
        match (&inner.detail, inner.state, expanded) {
            (Some(_), ToolState::Failed, _) if error_inline => {}
            (Some(detail), ToolState::Failed, _) => {
                let limit = if expanded {
                    RESULT_PREVIEW_LINES
                } else {
                    ERROR_PREVIEW_LINES
                };
                for row in gutter_rows(detail, limit, &error) {
                    rows.push(paint(&row));
                }
            }
            (Some(detail), _, true) if !detail.trim().is_empty() => {
                for row in gutter_rows(detail, RESULT_PREVIEW_LINES, &output) {
                    rows.push(paint(&row));
                }
            }
            _ => {}
        }
        rows
    }
}

/// The parts of a tool line, plain text but for the glyph.
struct ToolLine<'a> {
    glyph: &'a str,
    title: &'a str,
    args: &'a str,
    summary: &'a str,
    /// The summary is a failed call's error (compact, #2346): drawn in the error colour.
    error: bool,
    elapsed: &'a str,
}

/// ` ✓ title args · summary · 12 ms`, fitted to `width`: the arguments give way first (down to
/// a third of the room), then the summary; the title and the duration stay whole when they can.
fn tool_line(l: &ToolLine, width: usize) -> String {
    let colors = theme().colors;
    let w = rpi_tui::visible_width;
    // ` ✓ ` + title.
    let title = crate::chrome::shorten(l.title, width.saturating_sub(3).max(1));
    let mut room = width.saturating_sub(3 + w(&title));
    let elapsed = if !l.elapsed.is_empty() && room >= 3 + w(l.elapsed) {
        room -= 3 + w(l.elapsed);
        l.elapsed
    } else {
        ""
    };
    let arg_need = if l.args.is_empty() { 0 } else { 1 + w(l.args) };
    let sum_need = if l.summary.is_empty() {
        0
    } else {
        3 + w(l.summary)
    };
    let (arg_room, sum_room) = if arg_need + sum_need <= room {
        (arg_need, sum_need)
    } else {
        let arg_cap = arg_need.min(room / 3);
        let sum_room = room.saturating_sub(arg_cap).min(sum_need);
        (room - sum_room, sum_room)
    };
    let mut line = format!(
        " {} {}",
        l.glyph,
        colors.tool_title.fg(&crate::density::strong(&title))
    );
    if arg_room > 2 {
        let args = crate::chrome::shorten(l.args, arg_room - 1);
        line.push_str(&format!(" {}", colors.muted.fg(&args)));
    }
    let sep = colors.dim.fg(" · ");
    if sum_room > 4 {
        let summary = crate::chrome::shorten(l.summary, sum_room - 3);
        let color = if l.error {
            colors.error
        } else {
            colors.tool_output
        };
        line.push_str(&format!("{sep}{}", color.fg(&summary)));
    }
    if !elapsed.is_empty() {
        line.push_str(&format!("{sep}{}", colors.dim.fg(elapsed)));
    }
    line
}

/// A finished call's duration: `12 ms` under a second (#2183), then `1.2s`, `1m5s`.
pub fn format_elapsed(d: Duration) -> String {
    let secs = d.as_secs_f64();
    if secs < 1.0 {
        format!("{} ms", d.as_millis())
    } else if secs < 60.0 {
        format!("{secs:.1}s")
    } else {
        format!(
            "{}m{}s",
            (secs / 60.0).floor() as u64,
            (secs % 60.0).floor() as u64
        )
    }
}

/// A running call's timer: nothing for the first second (most AMP calls are done by then), then
/// whole seconds.
fn running_elapsed(d: Duration) -> String {
    match d.as_secs() {
        0 => String::new(),
        s if s < 60 => format!("{s}s"),
        s => format!("{}m{}s", s / 60, s % 60),
    }
}

impl Component for ToolBlock {
    /// A call on its own: a blank row, then its rows (in the transcript, calls sit in a
    /// [`ToolGroup`]).
    fn render(&self, width: usize) -> Vec<String> {
        let mut rows = vec![String::new()];
        rows.extend(self.rows(width));
        rows
    }

    fn invalidate(&self) {
        self.clear_cache();
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// Consecutive tool calls as one block (#2183): a blank row, then one line per call. A group
/// longer than [`GROUP_FOLD_AFTER`] calls folds its earliest successful ones into one line
/// (`✓ 4 calls · get_task ×3, list_tasks · 140 ms`) unless calls are expanded; failed and running
/// calls are never folded.
pub struct ToolGroup {
    calls: Mutex<Vec<Arc<ToolBlock>>>,
    display: Arc<ToolDisplay>,
}

impl ToolGroup {
    pub fn new(display: Arc<ToolDisplay>) -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
            display,
        }
    }

    pub fn push(&self, call: Arc<ToolBlock>) {
        if let Ok(mut c) = self.calls.lock() {
            c.push(call);
        }
    }

    pub fn len(&self) -> usize {
        self.calls.lock().map(|c| c.len()).unwrap_or(0)
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// The folded calls' line: `✓ 4 calls · get_task ×3, list_tasks · 140 ms`.
    fn folded_line(calls: &[&Arc<ToolBlock>], width: usize) -> String {
        let mut kinds: Vec<(String, usize)> = Vec::new();
        // Unknown when any call's duration is (#2355): no total rather than a short one.
        let mut took = Some(Duration::ZERO);
        for call in calls {
            let kind = call.kind();
            match kinds.iter_mut().find(|(k, _)| *k == kind) {
                Some((_, n)) => *n += 1,
                None => kinds.push((kind, 1)),
            }
            took = took.zip(call.took()).map(|(a, b)| a + b);
        }
        let names: Vec<String> = kinds
            .iter()
            .map(|(k, n)| {
                if *n > 1 {
                    format!("{k} ×{n}")
                } else {
                    k.clone()
                }
            })
            .collect();
        let colors = theme().colors;
        tool_line(
            &ToolLine {
                glyph: &colors.success.fg("✓"),
                title: &format!("{} calls", calls.len()),
                args: "",
                summary: &names.join(", "),
                error: false,
                elapsed: &took.map(format_elapsed).unwrap_or_default(),
            },
            width,
        )
    }
}

impl Component for ToolGroup {
    fn render(&self, width: usize) -> Vec<String> {
        let Ok(calls) = self.calls.lock() else {
            return Vec::new();
        };
        if calls.is_empty() {
            return Vec::new();
        }
        let mut rows = vec![String::new()];
        let fold_before = if !self.display.is_expanded() && calls.len() > GROUP_FOLD_AFTER {
            calls.len() - GROUP_KEEP_LAST
        } else {
            0
        };
        let folded: Vec<&Arc<ToolBlock>> = calls[..fold_before]
            .iter()
            .filter(|c| c.state() == ToolState::Completed)
            .collect();
        let mut folded_drawn = false;
        for (i, call) in calls.iter().enumerate() {
            if i < fold_before && call.state() == ToolState::Completed && folded.len() > 1 {
                if !folded_drawn {
                    rows.push(Self::folded_line(&folded, width));
                    folded_drawn = true;
                }
                continue;
            }
            rows.extend(call.rows(width));
        }
        rows
    }

    fn invalidate(&self) {
        if let Ok(calls) = self.calls.lock() {
            for call in calls.iter() {
                call.invalidate();
            }
        }
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
                // #2188: the `done` chip has no space padding.
                " ┌────┬───────────┬───────┐",
                " │  # │ Ticket    │ State │",
                " ├────┼───────────┼───────┤",
                " │  1 │ Say hello │ done  │",
                " │ 22 │ Plan      │ ready │",
                " └────┴───────────┴───────┘",
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

    fn visible_width_of(s: &str) -> usize {
        rpi_tui::visible_width(s)
    }

    /// A finished call (its timer set by hand, so the duration is known).
    fn finished(title: &str, args: &str, summary: &str, ms: u64) -> Arc<ToolBlock> {
        let block = ToolBlock::new(title, args);
        block.set_result(summary, "{\"ok\": true}", false);
        if let Ok(mut i) = block.inner.lock() {
            let end = Instant::now();
            i.started = Some(end - Duration::from_millis(ms));
            i.finished = Some(end);
        }
        Arc::new(block)
    }

    #[test]
    fn a_tool_call_is_one_line_with_its_summary_and_ms() {
        let block = finished("list_tasks", "", "3 tickets: 1 ready, 1 waiting", 12);
        let rows = plain_rows(&block.render(80));
        assert_eq!(
            rows,
            vec!["", " ✓ list_tasks · 3 tickets: 1 ready, 1 waiting · 12 ms"]
        );
        // A completed call has no surface colour; nothing else is styled as a background.
        let styled = block.rows(80).join("");
        assert!(!styled.contains("\x1b[48"), "{styled:?}");
    }

    #[test]
    fn a_narrow_line_shortens_the_argument_then_the_summary() {
        let block = finished(
            "kb_search",
            "how the harness wraps transcript text in narrow terminals",
            "3 results: architecture/amp-harness-transcript-wrapping.md",
            40,
        );
        for width in [30, 50, 80, 120] {
            let rows = plain_rows(&block.rows(width));
            assert_eq!(rows.len(), 1, "{rows:?}");
            assert!(visible_width_of(&rows[0]) <= width, "{width}: {rows:?}");
            assert!(rows[0].starts_with(" ✓ kb_search"), "{rows:?}");
            assert!(rows[0].ends_with("· 40 ms"), "{width}: {rows:?}");
        }
        let at50 = &plain_rows(&block.rows(50))[0];
        assert!(at50.contains("· 3 results"), "{at50:?}");
        assert!(at50.contains('…'), "{at50:?}");
    }

    #[test]
    fn durations_under_a_second_are_ms() {
        assert_eq!(format_elapsed(Duration::from_millis(0)), "0 ms");
        assert_eq!(format_elapsed(Duration::from_millis(12)), "12 ms");
        assert_eq!(format_elapsed(Duration::from_millis(999)), "999 ms");
        assert_eq!(format_elapsed(Duration::from_millis(1200)), "1.2s");
        assert_eq!(format_elapsed(Duration::from_secs(65)), "1m5s");
        // A running call shows no timer for its first second.
        assert_eq!(running_elapsed(Duration::from_millis(400)), "");
        assert_eq!(running_elapsed(Duration::from_millis(2400)), "2s");
    }

    #[test]
    fn running_and_failed_calls_keep_the_surface_and_errors_show() {
        let running = ToolBlock::new("get_task #67", "");
        running.set_running();
        let rows = running.rows(60);
        assert_eq!(rows.len(), 1);
        assert!(rows[0].contains("\x1b[48"), "{rows:?}");
        assert!(plain(&rows[0]).starts_with(" ● get_task #67"), "{rows:?}");

        let failed = ToolBlock::new("complete_task #2078", "all done");
        failed.set_result(
            "the plan was last approved before #2078",
            "the plan was last approved before #2078 was planned\nApprove it in AMP",
            true,
        );
        // Compact (#2346): one line, the error's first line on it.
        let rows = failed.rows(100);
        assert!(rows.iter().all(|r| r.contains("\x1b[48")), "{rows:?}");
        let rows = plain_rows(&rows);
        assert_eq!(rows.len(), 1, "{rows:?}");
        assert!(
            rows[0].starts_with(
                " ✗ complete_task #2078 all done · the plan was last approved before #2078 was"
            ),
            "{rows:?}"
        );
        // Comfortable: the error under it.
        failed.clear_cache();
        let rows = crate::density::with(crate::Density::Comfortable, || failed.rows(60));
        failed.clear_cache();
        assert!(rows.iter().all(|r| r.contains("\x1b[48")), "{rows:?}");
        let rows = plain_rows(&rows);
        assert!(
            rows[0].starts_with(" ✗ complete_task #2078 all done"),
            "{rows:?}"
        );
        assert!(
            rows[1].starts_with("   │ the plan was last approved"),
            "{rows:?}"
        );
        assert!(
            rows.iter().any(|r| r.contains("Approve it in AMP")),
            "{rows:?}"
        );
    }

    #[test]
    fn expanded_calls_show_their_arguments_and_result() {
        let display = Arc::new(ToolDisplay::default());
        let block = ToolBlock::with_display("read", "src/lib.rs", Arc::clone(&display));
        block.set_args_detail("{\"path\":\"src/lib.rs\"}");
        let text: Vec<String> = (1..=20).map(|i| format!("line {i}")).collect();
        block.set_result("20 lines", &text.join("\n"), false);
        assert_eq!(plain_rows(&block.rows(60)).len(), 1);
        assert!(display.toggle());
        let rows = plain_rows(&block.rows(60));
        assert!(
            rows[1].starts_with("   │ {\"path\":\"src/lib.rs\"}"),
            "{rows:?}"
        );
        assert!(rows.iter().any(|r| r.ends_with("│ line 12")), "{rows:?}");
        assert!(!rows.iter().any(|r| r.contains("line 13")), "{rows:?}");
        assert!(
            rows.iter().any(|r| r.trim() == "… 8 more lines"),
            "{rows:?}"
        );
        for row in &rows {
            assert!(visible_width_of(row) <= 60, "{row:?}");
        }
        assert!(!display.toggle());
        assert_eq!(plain_rows(&block.rows(60)).len(), 1);
    }

    #[test]
    fn three_calls_in_a_group_take_four_rows() {
        let group = ToolGroup::new(Arc::new(ToolDisplay::default()));
        group.push(finished(
            "list_tasks",
            "",
            "3 tickets: 1 ready, 1 waiting",
            12,
        ));
        group.push(finished("get_plan", "", "1 epic, 2 stories, 3 tickets", 8));
        group.push(finished("get_task #67", "", "Card form · in progress", 5));
        let rows = plain_rows(&group.render(80));
        assert_eq!(
            rows,
            vec![
                "",
                " ✓ list_tasks · 3 tickets: 1 ready, 1 waiting · 12 ms",
                " ✓ get_plan · 1 epic, 2 stories, 3 tickets · 8 ms",
                " ✓ get_task #67 · Card form · in progress · 5 ms",
            ]
        );
    }

    #[test]
    fn a_long_group_folds_its_earliest_successful_calls() {
        let display = Arc::new(ToolDisplay::default());
        let group = ToolGroup::new(Arc::clone(&display));
        for i in 0..4 {
            group.push(finished(&format!("get_task #{i}"), "", "x", 10));
        }
        let failed = ToolBlock::new("get_task #99", "");
        failed.set_result("no such ticket", "task 99 not found", true);
        group.push(Arc::new(failed));
        group.push(finished("list_tasks", "", "no tickets", 10));
        group.push(finished("get_plan", "", "no epics", 10));
        group.push(finished("check_plan", "", "plan OK", 10));
        let rows = plain_rows(&group.render(80));
        assert_eq!(rows[1], " ✓ 4 calls · get_task ×4 · 40 ms", "{rows:?}");
        // The failed call is never folded; the last three are shown one by one.
        assert!(rows[2].starts_with(" ✗ get_task #99"), "{rows:?}");
        assert!(
            rows.iter().any(|r| r.contains("task 99 not found")),
            "{rows:?}"
        );
        assert!(
            rows.iter().rev().take(3).all(|r| r.starts_with(" ✓ ")),
            "{rows:?}"
        );
        // Expanded, every call is shown.
        display.toggle();
        let rows = plain_rows(&group.render(80));
        assert!(
            rows.iter().any(|r| r.starts_with(" ✓ get_task #0")),
            "{rows:?}"
        );
    }

    // #2350: the user's own message is shown as typed, not as markdown.

    const TYPED: &str = "export it from mathx/__init__.py, rename a_b_c, match *.rs and keep **x**";

    /// #2357: what a long message looks like after sending, at the chat's widths and both
    /// densities: every character is on the bubble's rows, in order, and every row fits.
    #[test]
    fn a_long_message_shows_every_character_and_every_row_fits() {
        let paragraph = (0..120)
            .map(|i| format!("w{i:03}"))
            .collect::<Vec<_>>()
            .join(" ");
        let url = format!(
            "https://example.com/{}/END",
            (0..60).map(|i| format!("seg{i:03}")).collect::<String>()
        );
        let lines = (0..30)
            .map(|i| format!("line{i:02} text"))
            .collect::<Vec<_>>()
            .join("\n");
        let pasted = format!("{lines}\n{paragraph}");
        let wide = (0..40)
            .map(|i| format!("e{i:02}🚀中文字"))
            .collect::<Vec<_>>()
            .join(" ");
        for text in [&paragraph, &url, &lines, &pasted, &wide] {
            for density in [crate::Density::Compact, crate::Density::Comfortable] {
                // The transcript's width inside the inset at 80, 120 and 200 columns, and narrow.
                for width in [40, 78, 116, 196] {
                    let rows = crate::density::with(density, || {
                        UserBlock::new(text.as_str()).render(width)
                    });
                    let raw = rows.join("\n");
                    assert!(!raw.contains("\x1b]133"), "{density:?} {width}: {raw:?}");
                    for row in &rows {
                        assert!(
                            visible_width_of(row) <= width,
                            "{density:?} {width}: {row:?} is {} wide",
                            visible_width_of(row)
                        );
                        assert!(!plain(row).contains('…'), "{density:?} {width}: {row:?}");
                    }
                    let shown: String = plain_rows(&rows).concat().split_whitespace().collect();
                    let sent: String = text.split_whitespace().collect();
                    assert_eq!(
                        shown,
                        sent,
                        "{density:?} {width}:\n{}",
                        plain_rows(&rows).join("\n")
                    );
                    // The first line of the message is on the bubble's first text row (compact:
                    // its first row), the row a fresh-line from `133;A` used to lose.
                    let first_text = plain_rows(&rows)
                        .into_iter()
                        .find(|r| !r.trim().is_empty())
                        .unwrap();
                    let start: String = text.chars().take(3).collect();
                    assert!(
                        first_text.trim_start().starts_with(&start),
                        "{density:?} {width}: {first_text:?}"
                    );
                    if density == crate::Density::Compact {
                        assert!(!plain(&rows[0]).trim().is_empty(), "{rows:?}");
                    }
                }
            }
        }
    }

    #[test]
    fn the_users_message_is_shown_exactly_as_typed() {
        let rows = plain_rows(&UserBlock::new(TYPED).render(120));
        assert!(rows.iter().any(|r| r.trim() == TYPED), "{rows:#?}");

        // Wrapped at 40 columns, the words are the same and in order.
        let rows = plain_rows(&UserBlock::new(TYPED).render(40));
        assert!(rows.len() >= 3, "{rows:#?}");
        assert!(rows.iter().all(|r| r.chars().count() <= 40), "{rows:#?}");
        let words: Vec<String> = rows
            .iter()
            .flat_map(|r| r.split_whitespace().map(str::to_owned))
            .collect();
        assert_eq!(words.join(" "), TYPED);

        // No bold or italic from the `**x**` and `__init__` either.
        let raw = UserBlock::new("**x** and __init__").render(80).join("\n");
        assert!(
            !raw.contains("\x1b[1m") && !raw.contains("\x1b[3m"),
            "{raw:?}"
        );
    }

    #[test]
    fn the_users_message_keeps_its_lines_and_drops_control_characters() {
        let rows =
            plain_rows(&UserBlock::new("# not a heading\n- not a list\tx\x1b[31m").render(80));
        let text: Vec<&str> = rows
            .iter()
            .map(|r| r.trim())
            .filter(|r| !r.is_empty())
            .collect();
        assert_eq!(text, vec!["# not a heading", "- not a list    x[31m"]);
    }

    #[test]
    fn assistant_replies_still_render_markdown() {
        let block = AssistantBlock::new();
        block.update_blocks(&[Block::Text("see mathx/__init__.py and **x**".into())]);
        let raw = block.render(80).join("\n");
        let rows = plain_rows(&block.render(80));
        assert!(
            rows.iter().any(|r| r.trim() == "see mathx/init.py and x"),
            "{rows:#?}"
        );
        assert!(raw.contains("\x1b[1m"), "{raw:?}");
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
            vec![" ▸ Thinking · 0s Then I write the ticket and ask"]
        );

        // The answer starts: the line collapses to "Thought for Ns" and the timer stops (#2345).
        block.update_blocks(&[
            Block::Thinking(THOUGHT.into()),
            Block::Text("The answer.".into()),
        ]);
        let rows = plain_rows(&block.render(80));
        assert_eq!(rows, vec![" ▸ Thought for 0s", " The answer."]);
        // Comfortable: a blank row between them.
        let rows = crate::density::with(crate::Density::Comfortable, || {
            block.invalidate();
            plain_rows(&block.render(80))
        });
        block.invalidate();
        assert_eq!(rows, vec![" ▸ Thought for 0s", "", " The answer."]);
        let thinking = block.thinking.lock().unwrap()[0].clone();
        assert!(!thinking.is_live());
    }

    #[test]
    fn the_latest_sentence_keeps_its_end_when_narrow() {
        let block = ThinkingBlock::new(ThinkingDisplay::Collapsed, THOUGHT);
        let rows = plain_rows(&block.render(30));
        assert_eq!(rows, vec![" ▸ Thinking · 0s …cket and ask"]);
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
        assert_eq!(rows[0], " ▾ Thinking");
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
            vec![" ▸ Thinking", " a", " ▾ Thinking", " │ two", "", " b"]
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
            vec![" ▾ Thinking", " │ why", "", " a"]
        );
    }

    #[test]
    fn a_message_seen_complete_has_no_duration() {
        let block = AssistantBlock::new();
        block.finish();
        block.update_blocks(&[Block::Thinking("why".into()), Block::Text("a".into())]);
        assert_eq!(plain_rows(&block.render(40))[0], " ▸ Thinking");
    }

    // #2345: per-section toggles, the hint, the display mode changed in place, header rows.

    /// A message streamed live: thinking, then an answer, then thinking, then an answer.
    fn two_sections(display: ThinkingDisplay) -> AssistantBlock {
        let block = AssistantBlock::with_thinking(display);
        block.update_blocks(&[Block::Thinking("one".into())]);
        block.update_blocks(&[Block::Thinking("one".into()), Block::Text("a".into())]);
        block.update_blocks(&[
            Block::Thinking("one".into()),
            Block::Text("a".into()),
            Block::Thinking("two".into()),
        ]);
        block.update_blocks(&[
            Block::Thinking("one".into()),
            Block::Text("a".into()),
            Block::Thinking("two".into()),
            Block::Text("b".into()),
        ]);
        block.finish();
        block
    }

    #[test]
    fn each_section_opens_and_closes_on_its_own() {
        let block = two_sections(ThinkingDisplay::Collapsed);
        assert_eq!(
            plain_rows(&block.render(40)),
            vec![" ▸ Thought for 0s", " a", " ▸ Thought for 0s", " b"]
        );
        assert_eq!(block.toggle_section(0), Some(true));
        assert_eq!(
            plain_rows(&block.render(40)),
            vec![
                " ▾ Thought for 0s",
                " │ one",
                "",
                " a",
                " ▸ Thought for 0s",
                " b"
            ]
        );
        assert_eq!(block.toggle_section(0), Some(false));
        assert_eq!(block.toggle_section(2), None);
    }

    #[test]
    fn the_last_collapsed_section_names_its_key() {
        let block = two_sections(ThinkingDisplay::Collapsed);
        block.set_hint(Some(THINKING_HINT));
        let rows = plain_rows(&block.render(60));
        assert_eq!(rows[0], " ▸ Thought for 0s");
        assert_eq!(rows[2], " ▸ Thought for 0s · Ctrl+T or click to open");
        // Open, it says nothing; too narrow, neither.
        block.toggle_thinking();
        assert_eq!(plain_rows(&block.render(60))[2], " ▾ Thought for 0s");
        block.toggle_thinking();
        assert_eq!(plain_rows(&block.render(30))[2], " ▸ Thought for 0s");
        block.set_hint(None);
        assert_eq!(plain_rows(&block.render(60))[2], " ▸ Thought for 0s");
    }

    #[test]
    fn a_new_display_mode_applies_to_sections_already_shown() {
        let block = two_sections(ThinkingDisplay::Collapsed);
        block.set_display(ThinkingDisplay::Full);
        assert_eq!(
            plain_rows(&block.render(40)),
            vec![
                " ▾ Thought for 0s",
                " │ one",
                "",
                " a",
                " ▾ Thought for 0s",
                " │ two",
                "",
                " b"
            ]
        );
        block.set_display(ThinkingDisplay::Hidden);
        assert_eq!(plain_rows(&block.render(40)), vec![" a", " b"]);
        assert!(block.thinking_headers(40).is_empty());
        // A section added later takes the new mode.
        block.set_display(ThinkingDisplay::Collapsed);
        assert_eq!(plain_rows(&block.render(40))[0], " ▸ Thought for 0s");
    }

    #[test]
    fn header_rows_count_the_gap_and_open_sections() {
        let block = two_sections(ThinkingDisplay::Collapsed);
        assert_eq!(block.thinking_headers(40), vec![(0, 0), (2, 1)]);
        block.toggle_section(0);
        assert_eq!(block.thinking_headers(40), vec![(0, 0), (4, 1)]);
        let after_tools = two_sections(ThinkingDisplay::Collapsed);
        after_tools.set_gap_above();
        assert_eq!(after_tools.render(40)[0], "");
        assert_eq!(after_tools.thinking_headers(40), vec![(1, 0), (3, 1)]);
        // Comfortable: a blank row under each collapsed header.
        let rows = crate::density::with(crate::Density::Comfortable, || {
            after_tools.thinking_headers(40)
        });
        assert_eq!(rows, vec![(1, 0), (4, 1)]);
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
