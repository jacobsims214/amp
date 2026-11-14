//! Select-to-copy: the selection model and its text extraction (story #580, #4211).
//!
//! Pure logic, with no terminal I/O. A [`Selection`] lives in transcript **content** coordinates
//! (a rendered row index and a visible column within it), not screen coordinates. That way it
//! survives scrolling and new output. [`extract`] turns the selected cells of the rendered rows
//! back into the text the user saw (KB `discoveries/harness-select-to-copy.md` §4):
//!
//! - Columns are measured with `unicode-width` (the version [`crate::wrap`] measures with). A
//!   start column on the right half of a wide glyph snaps left, and an end column snaps right,
//!   so a CJK character or emoji is never split. A width-0 combining mark stays with its base.
//! - Escapes are stripped with [`wrap::plain`] (SGR, OSC 8 links, APC row markers). Tabs come
//!   out as four spaces. Control characters are **not** sanitised here; that is
//!   `clipboard::sanitize_for_copy`'s job.
//! - On rows after the first, the row's [`RowMeta::prefix`] decoration columns (hanging list
//!   indent, repeated quote borders) are skipped. Each row is right-trimmed (background fill,
//!   padding), except inside a hard-wrapped run, where trailing spaces are content.
//! - Rows rejoin by the previous row's [`Join`]: `Space` gives one space, `None` gives nothing
//!   and `Newline` gives `"\n"`.
//! - The smallest indent shared by the selected lines is removed (Inset, `padding_x`, the user
//!   bubble, the code panel). Indentation relative to that minimum is kept.
//! - Blank rows become empty lines. The result is right-trimmed, and an empty result is `None`
//!   (no copy).
//!
//! [`Press`] classifies a mouse press. A Down then Up in the same cell is a [`Press::Click`]
//! (the old click behaviour, no text). A drag to a different cell is [`Press::Selecting`].

use unicode_width::UnicodeWidthChar;

use crate::wrap::{self, Join, RowMeta};

/// A cell in transcript content coordinates: rendered row index and visible column.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Default)]
pub struct Pos {
    /// Rendered content row (0 = the transcript's first row).
    pub row: usize,
    /// Visible column within the row (escapes have no width).
    pub col: usize,
}

impl Pos {
    pub const fn new(row: usize, col: usize) -> Self {
        Self { row, col }
    }
}

/// A selection from the cell where the drag started (`anchor`) to the cell under the pointer
/// (`head`). Both end cells are included, and the drag may go in any direction.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct Selection {
    pub anchor: Pos,
    pub head: Pos,
}

impl Selection {
    pub const fn new(anchor: Pos, head: Pos) -> Self {
        Self { anchor, head }
    }

    /// `(start, end)` in reading order, whichever way the drag went (up, left or across rows).
    /// Both cells are inclusive.
    pub fn normalized(&self) -> (Pos, Pos) {
        if self.head < self.anchor {
            (self.head, self.anchor)
        } else {
            (self.anchor, self.head)
        }
    }
}

/// The state of a left-button press in the transcript: the click-vs-drag classifier.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Press {
    /// Button down, with no movement to another cell yet. Nothing has happened.
    Pending(Pos),
    /// The pointer moved to another cell, so this is a text selection.
    Selecting(Selection),
    /// Down then Up in the same cell. The caller runs its click action, and nothing is copied.
    Click(Pos),
}

impl Press {
    /// Button down at `at`.
    pub fn down(at: Pos) -> Self {
        Press::Pending(at)
    }

    /// The pointer dragged to `to`. A drag within the pressed cell stays pending.
    #[must_use]
    pub fn drag(self, to: Pos) -> Self {
        match self {
            Press::Pending(at) if at == to => Press::Pending(at),
            Press::Pending(at) => Press::Selecting(Selection::new(at, to)),
            Press::Selecting(sel) => Press::Selecting(Selection::new(sel.anchor, to)),
            click @ Press::Click(_) => click,
        }
    }

    /// Button released at `at`. A press still pending (same cell) becomes a click.
    #[must_use]
    pub fn up(self, at: Pos) -> Self {
        match self.drag(at) {
            Press::Pending(p) => Press::Click(p),
            other => other,
        }
    }

    /// The selection, while selecting.
    pub fn selection(&self) -> Option<Selection> {
        match self {
            Press::Selecting(sel) => Some(*sel),
            _ => None,
        }
    }
}

/// The visible text of `plain` in the cells `[from, to)`. A wide glyph is included whole if
/// either half is in range, and a width-0 mark goes with its base character.
fn cells(plain: &str, from: usize, to: usize) -> String {
    let mut out = String::new();
    let mut col = 0;
    // Marks before any base character belong to the row's first cell.
    let mut take = from == 0 && to > 0;
    for c in plain.chars() {
        let w = c.width().unwrap_or(0);
        if w > 0 {
            take = col + w > from && col < to;
            col += w;
        }
        if take {
            out.push(c);
        }
    }
    out
}

/// One selected row's text, before dedent and joining.
struct Piece {
    text: String,
    /// The column `text` starts at.
    from: usize,
    /// Starts a logical line: the first selected row, or the previous row ended with `Newline`.
    line_start: bool,
    /// Taken from the row's own start (column 0, or just after its skipped prefix).
    whole_left: bool,
}

fn leading_spaces(s: &str) -> usize {
    s.len() - s.trim_start_matches(' ').len()
}

/// The text a selection covers in `rows` (rendered content rows, ANSI included), with `meta`
/// giving each row's [`RowMeta`] (missing entries default to a plain `Newline` row). Returns
/// `None` when nothing but whitespace is selected, which means nothing gets copied.
pub fn extract(rows: &[String], meta: &[RowMeta], sel: Selection) -> Option<String> {
    let (start, end) = sel.normalized();
    if rows.is_empty() || start.row >= rows.len() {
        return None;
    }
    let (last, end_col) = if end.row >= rows.len() {
        (rows.len() - 1, usize::MAX)
    } else {
        (end.row, end.col.saturating_add(1))
    };
    let meta_at = |r: usize| meta.get(r).copied().unwrap_or_default();

    let mut pieces = Vec::with_capacity(last + 1 - start.row);
    for (r, row) in rows.iter().enumerate().take(last + 1).skip(start.row) {
        let m = meta_at(r);
        let mut from = if r == start.row { start.col } else { 0 };
        let mut whole_left = from == 0;
        if r > start.row && from < m.prefix {
            from = m.prefix;
            whole_left = true;
        }
        let to = if r == last { end_col } else { usize::MAX };
        let mut text = cells(&wrap::plain(row), from, to);
        // Trailing spaces inside a hard-wrapped run are content (code), not padding.
        if m.join != Join::None || r == last {
            text.truncate(text.trim_end().len());
        }
        let line_start = r == start.row || meta_at(r - 1).join == Join::Newline;
        pieces.push(Piece {
            text,
            from,
            line_start,
            whole_left,
        });
    }

    // Dedent: the smallest absolute indent column shared by the selected lines' starts.
    let indent = |p: &Piece| p.from + leading_spaces(&p.text);
    let starts = || {
        pieces
            .iter()
            .filter(|p| p.line_start && !p.text.trim().is_empty())
    };
    let min_col = starts()
        .filter(|p| p.whole_left)
        .map(indent)
        .min()
        .or_else(|| starts().map(indent).min())
        .unwrap_or(0);
    for p in pieces.iter_mut().filter(|p| p.line_start) {
        let cut = leading_spaces(&p.text).min(min_col.saturating_sub(p.from));
        p.text.drain(..cut);
    }

    let mut out = String::new();
    for (i, p) in pieces.iter().enumerate() {
        if i > 0 {
            out.push_str(match meta_at(start.row + i - 1).join {
                Join::Newline => "\n",
                Join::Space => " ",
                Join::None => "",
            });
        }
        out.push_str(&p.text);
    }
    out.truncate(out.trim_end().len());
    if out.trim().is_empty() {
        None
    } else {
        Some(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn s(rows: &[&str]) -> Vec<String> {
        rows.iter().map(|r| r.to_string()).collect()
    }

    fn sel(a: (usize, usize), h: (usize, usize)) -> Selection {
        Selection::new(Pos::new(a.0, a.1), Pos::new(h.0, h.1))
    }

    const NL: RowMeta = RowMeta {
        prefix: 0,
        join: Join::Newline,
    };
    const SP: RowMeta = RowMeta {
        prefix: 0,
        join: Join::Space,
    };

    fn meta(prefix: usize, join: Join) -> RowMeta {
        RowMeta { prefix, join }
    }

    #[test]
    fn ascii_single_row_partial() {
        let rows = s(&["hello world"]);
        assert_eq!(
            extract(&rows, &[], sel((0, 6), (0, 10))).as_deref(),
            Some("world")
        );
        assert_eq!(
            extract(&rows, &[], sel((0, 1), (0, 3))).as_deref(),
            Some("ell")
        );
    }

    #[test]
    fn ascii_multi_row_keeps_real_breaks() {
        let rows = s(&["first line", "second line", "third line"]);
        let got = extract(&rows, &[NL, NL, NL], sel((0, 6), (2, 4)));
        assert_eq!(got.as_deref(), Some("line\nsecond line\nthird"));
    }

    #[test]
    fn backward_drag_normalises() {
        let rows = s(&["first line", "second line", "third line"]);
        let fwd = extract(&rows, &[], sel((0, 6), (2, 4)));
        let back = extract(&rows, &[], sel((2, 4), (0, 6)));
        assert_eq!(fwd, back);
        // Right to left on one row.
        let rows = s(&["hello world"]);
        assert_eq!(
            extract(&rows, &[], sel((0, 10), (0, 6))).as_deref(),
            Some("world")
        );
        assert_eq!(
            sel((3, 1), (1, 9)).normalized(),
            (Pos::new(1, 9), Pos::new(3, 1))
        );
    }

    #[test]
    fn cjk_wide_glyphs_never_split() {
        let rows = s(&["日本語"]);
        // Columns 1..=3 touch the right half of 日 and the left half of 本.
        assert_eq!(
            extract(&rows, &[], sel((0, 1), (0, 3))).as_deref(),
            Some("日本")
        );
        // Start on the right half of 本 snaps left; end on the left half of 語 snaps right.
        assert_eq!(
            extract(&rows, &[], sel((0, 3), (0, 4))).as_deref(),
            Some("本語")
        );
        assert_eq!(
            extract(&rows, &[], sel((0, 1), (0, 1))).as_deref(),
            Some("日")
        );
    }

    #[test]
    fn emoji_snaps_like_cjk() {
        // a=0, 😀=1..2, b=3
        let rows = s(&["a😀b"]);
        assert_eq!(
            extract(&rows, &[], sel((0, 2), (0, 3))).as_deref(),
            Some("😀b")
        );
        assert_eq!(
            extract(&rows, &[], sel((0, 0), (0, 1))).as_deref(),
            Some("a😀")
        );
        assert_eq!(
            extract(&rows, &[], sel((0, 2), (0, 2))).as_deref(),
            Some("😀")
        );
    }

    #[test]
    fn combining_mark_stays_with_base() {
        // c=0 a=1 f=2 e=3 (U+0301 has width 0) space=4 x=5
        let rows = s(&["cafe\u{301} x"]);
        assert_eq!(
            extract(&rows, &[], sel((0, 3), (0, 3))).as_deref(),
            Some("e\u{301}")
        );
        assert_eq!(
            extract(&rows, &[], sel((0, 0), (0, 3))).as_deref(),
            Some("cafe\u{301}")
        );
        assert_eq!(
            extract(&rows, &[], sel((0, 4), (0, 5))).as_deref(),
            Some("x")
        );
    }

    #[test]
    fn ansi_styled_rows_are_stripped() {
        let rows = s(&[
            "\x1b[1mbold\x1b[0m \x1b[38;5;1mred\x1b[0m",
            "\x1b]8;;https://example.com\x07link\x1b]8;;\x07 \x1b_amp:r;0;n\x07",
        ]);
        let got = extract(&rows, &[], sel((0, 0), (1, 20)));
        assert_eq!(got.as_deref(), Some("bold red\nlink"));
        // Columns ignore the escapes: 'red' is at 5..=7.
        assert_eq!(
            extract(&rows, &[], sel((0, 5), (0, 7))).as_deref(),
            Some("red")
        );
    }

    #[test]
    fn word_wrapped_paragraph_rejoins_to_source() {
        let src = "the quick brown fox jumps over the lazy dog";
        let rows = wrap::wrap(src, 12);
        assert!(rows.len() > 2, "wraps onto several rows: {rows:?}");
        let mut m = vec![SP; rows.len()];
        *m.last_mut().unwrap() = NL;
        let got = extract(&rows, &m, sel((0, 0), (rows.len() - 1, 99)));
        assert_eq!(got.as_deref(), Some(src));
    }

    #[test]
    fn hard_wrapped_code_rejoins_byte_exact() {
        let src = "let x = a  +  b;   // ok";
        let rows: Vec<String> = wrap::hard_wrap(src, 5)
            .into_iter()
            .map(|r| format!("    {r}"))
            .collect();
        let mut m = vec![meta(4, Join::None); rows.len()];
        m.last_mut().unwrap().join = Join::Newline;
        let got = extract(&rows, &m, sel((0, 0), (rows.len() - 1, 99)));
        assert_eq!(got.as_deref(), Some(src));
    }

    #[test]
    fn wrapped_rows_with_unicode_rejoin() {
        let rows = s(&["日本語の", "テキスト"]);
        let got = extract(&rows, &[meta(0, Join::None), NL], sel((0, 4), (1, 3)));
        assert_eq!(got.as_deref(), Some("語のテキ"));
    }

    #[test]
    fn continuation_prefix_skipped_after_first_row() {
        // List hang: "• " then continuation indented 2.
        let rows = s(&["• one two", "  three four"]);
        let m = [SP, meta(2, Join::Newline)];
        assert_eq!(
            extract(&rows, &m, sel((0, 0), (1, 20))).as_deref(),
            Some("• one two three four")
        );
        // Quote borders repeat on the continuation.
        let rows = s(&["│ quoted text", "│ goes on"]);
        let m = [SP, meta(2, Join::Newline)];
        assert_eq!(
            extract(&rows, &m, sel((0, 0), (1, 20))).as_deref(),
            Some("│ quoted text goes on")
        );
    }

    #[test]
    fn trailing_padding_trimmed() {
        let rows = s(&[
            "\x1b[48;5;236m  hi          \x1b[0m",
            "\x1b[48;5;236m  there       \x1b[0m",
        ]);
        assert_eq!(
            extract(&rows, &[], sel((0, 0), (1, 13))).as_deref(),
            Some("hi\nthere")
        );
    }

    #[test]
    fn dedented_user_bubble() {
        let rows = s(&[
            "  hello there   ",
            "  second line   ",
            "                ",
            "  third   ",
        ]);
        let got = extract(&rows, &[], sel((0, 0), (3, 15)));
        assert_eq!(got.as_deref(), Some("hello there\nsecond line\n\nthird"));
    }

    #[test]
    fn dedent_with_prefix_on_every_row() {
        // Inset pad in every row's prefix: the first row isn't skipped, later ones are.
        let rows = s(&["  hello", "  world"]);
        let m = [meta(2, Join::Newline), meta(2, Join::Newline)];
        assert_eq!(
            extract(&rows, &m, sel((0, 0), (1, 9))).as_deref(),
            Some("hello\nworld")
        );
    }

    #[test]
    fn relative_code_indent_kept() {
        let rows = s(&["    fn main() {", "        go();", "    }"]);
        let got = extract(&rows, &[], sel((0, 0), (2, 9)));
        assert_eq!(got.as_deref(), Some("fn main() {\n    go();\n}"));
    }

    #[test]
    fn tool_line_copies_with_ellipsis() {
        let rows = s(&[" \x1b[32m✓\x1b[0m read src/very/long/pa…"]);
        let got = extract(&rows, &[], sel((0, 0), (0, 40)));
        assert_eq!(got.as_deref(), Some("✓ read src/very/long/pa…"));
    }

    #[test]
    fn tabs_copy_as_four_spaces() {
        let rows = s(&["a\tb"]);
        assert_eq!(
            extract(&rows, &[], sel((0, 0), (0, 9))).as_deref(),
            Some("a    b")
        );
    }

    #[test]
    fn blank_or_out_of_range_is_none() {
        let rows = s(&["      ", ""]);
        assert_eq!(extract(&rows, &[], sel((0, 0), (1, 5))), None);
        assert_eq!(extract(&s(&["x"]), &[], sel((3, 0), (4, 0))), None);
        assert_eq!(extract(&[], &[], sel((0, 0), (0, 0))), None);
        // An end past the last row clamps to the content.
        assert_eq!(
            extract(&s(&["ab", "cd"]), &[], sel((0, 1), (9, 0))).as_deref(),
            Some("b\ncd")
        );
    }

    #[test]
    fn press_same_cell_is_click() {
        let p = Pos::new(2, 5);
        assert_eq!(Press::down(p).up(p), Press::Click(p));
        // A drag that never leaves the cell is still a click.
        assert_eq!(Press::down(p).drag(p).up(p), Press::Click(p));
        assert_eq!(Press::down(p).up(p).selection(), None);
    }

    #[test]
    fn press_drag_to_other_cell_selects() {
        let a = Pos::new(2, 5);
        let b = Pos::new(3, 1);
        let dragging = Press::down(a).drag(b);
        assert_eq!(dragging, Press::Selecting(Selection::new(a, b)));
        // Up keeps selecting, and the head follows the last position.
        let c = Pos::new(4, 0);
        assert_eq!(dragging.up(c).selection(), Some(Selection::new(a, c)));
        // Down then Up in a different cell with no drag events is a selection too.
        assert_eq!(Press::down(a).up(b).selection(), Some(Selection::new(a, b)));
        assert_eq!(Press::down(a).drag(a), Press::Pending(a));
    }
}
