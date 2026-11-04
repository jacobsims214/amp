//! Titled boxes around the chat's sections (AMP #2882, the layout approved in artifact #26).
//!
//! ```text
//! ┌─ ▀▀▀▀▀▀▀▀ AMP #2 · simstech · ada@example.com ───────────────── ● connected ─┐
//! │ Checking the board first.                                                    │
//! └──────────────────────────────────────────────────────────────────────────────┘
//! ┌─ Workers · 2 running ────────────────────────── Ctrl+W watch · /workers all ─┐
//! │ ▸ #2142 Fix installer setup ha… · worker-backend · running 3m12s · 48.0k tok │
//! └──────────────────────────────────────────────────────────────────────────────┘
//! ┌─ Message ─────────────────────────────── ⠋ manager replying 12s · Esc stops ─┐
//! │ / for commands · Ctrl+J new line                                             │
//! └──────────────────────────────────────────────────────────────────────────────┘
//! ```
//!
//! - **Glyphs:** sharp corners `┌ ┐ └ ┘ ─ │`, or `+ - |` in ASCII mode ([`set_ascii`]: a
//!   locale that isn't UTF-8, `TERM=dumb`, `AMP_HARNESS_ASCII=1` or `[tui] ascii = true`). The
//!   geometry is the same, so nothing reflows.
//! - **Titles:** `┌─ Title ─…─ right ─┐`, one space either side; the right-hand title carries live
//!   status (the reply's spinner, a count, the keys). A bottom title carries a key hint.
//! - **Focus:** the box with the keys draws its border in the theme's `muted` colour and its title
//!   bold; the others in `border`, title `dim` (`border_muted` is too faint for a box edge).
//! - **Margin:** a box sits [`margin`] columns from the screen's edge: the shared
//!   [`Padding`] less the border's own column, so the text inside stays where it was before the
//!   boxes (column 2 at 80 columns, 3 above). Inside, one column of padding.
//!
//! [`Framed`] boxes a leaf component (the editor, the worker list, a panel). The conversation's
//! box can't be a leaf (its scroll view needs the layout engine), so [`crate::view::ChatView`]
//! builds it from a [`BorderRow`] above and below an rpi-tui `HStack` of [`Gutter`], [`Edge`],
//! the transcript, [`Edge`], [`Gutter`].

use std::{
    any::Any,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};

use rpi_tui::{Component, theme, truncate_to_width, visible_width};

use crate::chrome::shorten;
use crate::inset::Padding;

static ASCII: AtomicBool = AtomicBool::new(false);

/// Draws every box with `+ - |` instead of the box-drawing characters (before the shell is
/// built; the boxes read it on every render).
pub fn set_ascii(on: bool) {
    ASCII.store(on, Ordering::Relaxed);
}

/// Whether the boxes are drawn in ASCII.
pub fn ascii() -> bool {
    ASCII.load(Ordering::Relaxed)
}

/// The box-drawing characters in use.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Glyphs {
    pub top_left: &'static str,
    pub top_right: &'static str,
    pub bottom_left: &'static str,
    pub bottom_right: &'static str,
    pub horizontal: &'static str,
    pub vertical: &'static str,
}

pub const SHARP: Glyphs = Glyphs {
    top_left: "┌",
    top_right: "┐",
    bottom_left: "└",
    bottom_right: "┘",
    horizontal: "─",
    vertical: "│",
};

pub const ASCII_GLYPHS: Glyphs = Glyphs {
    top_left: "+",
    top_right: "+",
    bottom_left: "+",
    bottom_right: "+",
    horizontal: "-",
    vertical: "|",
};

/// The glyphs for the current mode.
pub fn glyphs() -> Glyphs {
    if ascii() { ASCII_GLYPHS } else { SHARP }
}

/// Columns between the screen's edge and a box at `width`: the padding less the border's
/// column (0 at 80 columns or fewer by default, 1 above).
pub fn margin(padding: &Padding, width: usize) -> usize {
    padding.at(width).saturating_sub(1)
}

/// The narrowest box drawn; anything narrower shows its content without the frame.
pub const MIN_BOX: usize = 12;

/// The border colour for a box with (`focused`) or without the keys.
fn border_color(focused: bool) -> rpi_tui::theme::Color {
    let c = theme().colors;
    if focused { c.muted } else { c.border }
}

/// A plain title in the box's style: bold `text` with the keys, `dim` without.
pub fn label(text: &str, focused: bool) -> String {
    let c = theme().colors;
    if focused {
        rpi_tui::bold(&c.text.fg(text))
    } else {
        c.dim.fg(text)
    }
}

/// A box's top or bottom edge `width` columns wide: `left` and `right` (styled, either may be
/// empty) set into the rule. When both don't fit, `right` goes first, then `left` is cut.
pub fn edge(width: usize, top: bool, left: &str, right: &str, focused: bool) -> String {
    edge_in(glyphs(), width, top, left, right, focused)
}

fn edge_in(g: Glyphs, width: usize, top: bool, left: &str, right: &str, focused: bool) -> String {
    let color = border_color(focused);
    let (open, close) = if top {
        (g.top_left, g.top_right)
    } else {
        (g.bottom_left, g.bottom_right)
    };
    if width < 2 {
        return color.fg(&g.horizontal.repeat(width));
    }
    let rule = |n: usize| color.fg(&g.horizontal.repeat(n));
    // `┌─ left ` … ` right ─┐`: 3 + L + 1 before the fill, 1 + R + 2 after it.
    let lw = visible_width(left);
    let rw = visible_width(right);
    let room = width.saturating_sub(2);
    let (left, lw, right, rw) = if lw > 0 && rw > 0 && lw + rw + 8 <= width {
        (left.to_owned(), lw, right.to_owned(), rw)
    } else if lw > 0 && lw + 5 <= width {
        (left.to_owned(), lw, String::new(), 0)
    } else if lw > 0 && room >= 6 {
        let cut = truncate_to_width(left, width - 5, "…");
        let w = visible_width(&cut);
        (cut, w, String::new(), 0)
    } else if lw == 0 && rw > 0 && rw + 5 <= width {
        (String::new(), 0, right.to_owned(), rw)
    } else {
        (String::new(), 0, String::new(), 0)
    };
    let mut out = color.fg(open);
    match (lw > 0, rw > 0) {
        (false, false) => out.push_str(&rule(room)),
        (true, false) => {
            out.push_str(&rule(1));
            out.push(' ');
            out.push_str(&left);
            out.push(' ');
            out.push_str(&rule(room - lw - 3));
        }
        (false, true) => {
            out.push_str(&rule(room - rw - 3));
            out.push(' ');
            out.push_str(&right);
            out.push(' ');
            out.push_str(&rule(1));
        }
        (true, true) => {
            out.push_str(&rule(1));
            out.push(' ');
            out.push_str(&left);
            out.push(' ');
            out.push_str(&rule(room - lw - rw - 6));
            out.push(' ');
            out.push_str(&right);
            out.push(' ');
            out.push_str(&rule(1));
        }
    }
    out.push_str(&color.fg(close));
    out
}

/// One row inside a box `width` columns wide: `│ text… │`, `text` padded (or cut) to the inner
/// width.
pub fn side(width: usize, text: &str, focused: bool) -> String {
    side_in(glyphs(), width, text, focused)
}

fn side_in(g: Glyphs, width: usize, text: &str, focused: bool) -> String {
    let color = border_color(focused);
    let inner = width.saturating_sub(4);
    let w = visible_width(text);
    let body = if w > inner {
        truncate_to_width(text, inner, "")
    } else {
        format!("{text}{}", " ".repeat(inner - w))
    };
    format!("{} {body} {}", color.fg(g.vertical), color.fg(g.vertical))
}

/// What a [`Framed`] box shows around its content, read at every render.
#[derive(Default, Clone)]
pub struct Titles {
    /// Styled; see [`label`].
    pub left: String,
    pub right: String,
    /// In the bottom edge (a key hint).
    pub bottom: String,
    pub focused: bool,
}

/// Builds a box's titles at the content's inner width.
pub type TitlesFn = Arc<dyn Fn(usize) -> Titles + Send + Sync>;

/// A leaf component in a titled box, [`margin`] columns in from each edge. Renders nothing when
/// its content renders nothing (an empty worker list, a closed panel).
pub struct Framed {
    child: Arc<dyn Component>,
    titles: TitlesFn,
    padding: Padding,
}

impl Framed {
    pub fn new(child: Arc<dyn Component>, titles: TitlesFn, padding: Padding) -> Self {
        Self {
            child,
            titles,
            padding,
        }
    }

    pub fn child(&self) -> &Arc<dyn Component> {
        &self.child
    }

    /// The columns before the content's first cell at `width` (margin, border, padding).
    pub fn content_left(&self, width: usize) -> usize {
        let m = margin(&self.padding, width);
        if width.saturating_sub(2 * m) < MIN_BOX {
            return self.padding.at(width);
        }
        m + 2
    }

    /// The content's width inside the box at `width`.
    pub fn content_width(&self, width: usize) -> usize {
        let m = margin(&self.padding, width);
        let bw = width.saturating_sub(2 * m);
        if bw < MIN_BOX {
            return width.saturating_sub(2 * self.padding.at(width));
        }
        bw - 4
    }
}

impl Component for Framed {
    fn render(&self, width: usize) -> Vec<String> {
        let m = margin(&self.padding, width);
        let bw = width.saturating_sub(2 * m);
        if bw < MIN_BOX {
            // Too narrow for a frame: the content alone, as before the boxes.
            let pad = self.padding.at(width);
            let indent = " ".repeat(pad);
            return self
                .child
                .render(width.saturating_sub(2 * pad))
                .into_iter()
                .map(|l| format!("{indent}{l}"))
                .collect();
        }
        let inner = bw - 4;
        let body = self.child.render(inner);
        if body.is_empty() {
            return body;
        }
        let t = (self.titles)(inner);
        let indent = " ".repeat(m);
        let mut out = Vec::with_capacity(body.len() + 2);
        out.push(format!(
            "{indent}{}",
            edge(bw, true, &t.left, &t.right, t.focused)
        ));
        for line in &body {
            out.push(format!("{indent}{}", side(bw, line, t.focused)));
        }
        out.push(format!(
            "{indent}{}",
            edge(bw, false, &t.bottom, "", t.focused)
        ));
        out
    }

    fn handle_input(&self, data: &str) -> bool {
        self.child.handle_input(data)
    }

    fn invalidate(&self) {
        self.child.invalidate();
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// The top or bottom edge of a box whose rows are other components (the conversation's), the
/// screen's full width with the margin left blank.
pub struct BorderRow {
    top: bool,
    titles: TitlesFn,
    padding: Padding,
}

impl BorderRow {
    pub fn new(top: bool, titles: TitlesFn, padding: Padding) -> Self {
        Self {
            top,
            titles,
            padding,
        }
    }
}

impl Component for BorderRow {
    fn render(&self, width: usize) -> Vec<String> {
        let m = margin(&self.padding, width);
        let bw = width.saturating_sub(2 * m);
        if bw < MIN_BOX {
            return Vec::new();
        }
        let t = (self.titles)(bw.saturating_sub(8));
        let line = if self.top {
            edge(bw, true, &t.left, &t.right, t.focused)
        } else {
            edge(bw, false, &t.bottom, "", t.focused)
        };
        vec![format!("{}{line}", " ".repeat(m))]
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// The rows the side edges draw: more than any terminal is tall; the layout clips them to the
/// rows the box has.
const EDGE_ROWS: usize = 1000;

/// A box's left or right edge beside a component the layout engine places (an `HStack` column,
/// one cell wide; never focused: the conversation's box). Nothing when the box is too narrow to
/// draw ([`MIN_BOX`]).
pub struct Edge {
    on: Arc<AtomicBool>,
}

impl Edge {
    /// `on` says whether the box is drawn at all (the screen is wide enough); the
    /// conversation's left [`Gutter`] sets it.
    pub fn new(on: Arc<AtomicBool>) -> Self {
        Self { on }
    }
}

impl Component for Edge {
    fn render(&self, _width: usize) -> Vec<String> {
        if !self.on.load(Ordering::Relaxed) {
            return Vec::new();
        }
        vec![border_color(false).fg(glyphs().vertical); EDGE_ROWS]
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// The blank margin beside a box whose rows the layout places: measured at the row's full width
/// (where it picks [`margin`] columns); drawn blank. It also records whether the box's sides are
/// drawn ([`Edge`]): not when the box is too narrow, nor while `flush` is set (rows that must
/// start at the screen's edge are in the box: the pairing link, #2423).
pub struct Gutter {
    padding: Padding,
    on: Arc<AtomicBool>,
    flush: Arc<AtomicBool>,
    /// The left gutter decides; the right one follows it.
    decides: bool,
}

impl Gutter {
    pub fn new(
        padding: Padding,
        on: Arc<AtomicBool>,
        flush: Arc<AtomicBool>,
        decides: bool,
    ) -> Self {
        Self {
            padding,
            on,
            flush,
            decides,
        }
    }
}

impl Component for Gutter {
    fn render(&self, width: usize) -> Vec<String> {
        // The layout measures a column at the stack's full width, then draws it at the width it
        // was given (a few cells): only the measurement decides anything.
        if width < MIN_BOX {
            return vec![" ".repeat(width)];
        }
        if self.flush.load(Ordering::Relaxed) {
            if self.decides {
                self.on.store(false, Ordering::Relaxed);
            }
            return vec![String::new()];
        }
        let m = margin(&self.padding, width);
        let drawn = width.saturating_sub(2 * m) >= MIN_BOX;
        if self.decides {
            self.on.store(drawn, Ordering::Relaxed);
        }
        let cols = if drawn { m } else { self.padding.at(width) };
        vec![" ".repeat(cols)]
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// One blank column inside a box's right edge, beside a component the layout places (the
/// transcript, whose entries don't all keep a column clear on the right). None while the box has
/// no sides (`on` off, or `flush`).
pub struct Spacer {
    on: Arc<AtomicBool>,
    flush: Arc<AtomicBool>,
}

impl Spacer {
    pub fn new(on: Arc<AtomicBool>, flush: Arc<AtomicBool>) -> Self {
        Self { on, flush }
    }
}

impl Component for Spacer {
    fn render(&self, _width: usize) -> Vec<String> {
        if self.on.load(Ordering::Relaxed) && !self.flush.load(Ordering::Relaxed) {
            vec![" ".to_owned()]
        } else {
            vec![String::new()]
        }
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// `text` shortened (as plain text) to `max` cells and drawn in `color`.
pub fn fitted(text: &str, max: usize, color: rpi_tui::theme::Color) -> String {
    color.fg(&shorten(text, max))
}

#[cfg(test)]
mod tests {
    use super::*;
    use rpi_tui::strip_ansi;

    #[test]
    fn edges_carry_titles_and_fill_the_width() {
        let top = strip_ansi(&edge(40, true, "Message", "⠋ replying 3s", true));
        assert_eq!(top, "┌─ Message ──────────── ⠋ replying 3s ─┐");
        assert_eq!(visible_width(&top), 40);
        let bottom = strip_ansi(&edge(20, false, "", "", false));
        assert_eq!(bottom, format!("└{}┘", "─".repeat(18)));
        // The right title goes first, then the left is cut.
        let tight = strip_ansi(&edge(20, true, "Message", "a long right title", false));
        assert_eq!(tight, "┌─ Message ────────┐");
        let cut = strip_ansi(&edge(12, true, "Conversation", "", false));
        assert_eq!(visible_width(&cut), 12, "{cut}");
        assert!(cut.starts_with("┌─ Con") && cut.ends_with("┐"), "{cut}");
    }

    #[test]
    fn sides_pad_and_cut_to_the_inner_width() {
        assert_eq!(strip_ansi(&side(10, "abc", false)), "│ abc    │");
        assert_eq!(strip_ansi(&side(10, "abcdefghij", false)), "│ abcdef │");
    }

    #[test]
    fn ascii_mode_keeps_the_geometry() {
        // The mode is process-wide; the glyph sets are tested directly so tests don't race.
        let top = strip_ansi(&edge_in(ASCII_GLYPHS, 20, true, "Message", "", true));
        let row = strip_ansi(&side_in(ASCII_GLYPHS, 20, "x", true));
        assert_eq!(top, "+- Message --------+");
        assert_eq!(row, "| x                |");
    }

    #[test]
    fn a_framed_component_sits_in_its_margin() {
        struct Two;
        impl Component for Two {
            fn render(&self, width: usize) -> Vec<String> {
                vec!["x".repeat(width), "y".into()]
            }
            fn invalidate(&self) {}
            fn as_any(&self) -> &dyn Any {
                self
            }
        }
        let titles: TitlesFn = Arc::new(|_| Titles {
            left: "T".into(),
            ..Titles::default()
        });
        let framed = Framed::new(Arc::new(Two), titles, Padding::new(2));
        let at80: Vec<String> = framed.render(80).iter().map(|l| strip_ansi(l)).collect();
        assert_eq!(at80.len(), 4);
        assert!(at80[0].starts_with("┌─ T ─"), "{at80:?}");
        assert_eq!(at80[1], format!("│ {} │", "x".repeat(76)));
        assert_eq!(visible_width(&at80[3]), 80);
        assert_eq!(framed.content_left(80), 2);
        let at120: Vec<String> = framed.render(120).iter().map(|l| strip_ansi(l)).collect();
        assert!(at120[1].starts_with(" │ x"), "margin 1 above 80: {at120:?}");
        assert_eq!(visible_width(&at120[1]), 119);
        assert_eq!(framed.content_left(120), 3);
        assert_eq!(framed.content_width(120), 114);
    }
}
