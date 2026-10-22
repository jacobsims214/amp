//! Word wrapping for transcript text (#2176).
//!
//! rpi-tui 0.8.0's `wrap_text_with_ansi` breaks a line at exactly the width, in the middle of a
//! word ("comple / tes"), and keeps the separating space, so a continuation line can start with
//! one. Every rpi component calls it internally, so the transcript components in
//! [`crate::transcript`] wrap with [`wrap`] instead:
//!
//! - breaks happen at spaces; the spaces at a break are dropped, so no continuation line starts
//!   with a space;
//! - only a token wider than the line (a URL, a path) is broken inside, filling each row;
//! - ANSI styling is carried across a break: a row closes its open SGR state with a reset and
//!   the next row re-opens it, so every row renders on its own, like rpi's own rows;
//! - the first row keeps its leading spaces (indentation is meaningful there).
//!
//! [`reflow_row`] wraps one row that rpi rendered unwrapped, with a hanging indent under the text
//! of a list item (`• `, `1. `, `☐ `) or behind a quote's borders (`│ `).

use unicode_width::UnicodeWidthChar;

const RESET: &str = "\x1b[0m";

/// One piece of a styled string: an escape sequence (no width) or a visible character.
#[derive(Debug, Clone, Copy)]
enum Item<'a> {
    Esc(&'a str),
    Char(char, usize),
}

/// Splits `s` into escape sequences and visible characters. Control characters other than tab
/// are dropped (as rpi does); a tab counts as four spaces.
fn items(s: &str) -> Vec<Item<'_>> {
    let mut out = Vec::with_capacity(s.len());
    let bytes = s.as_bytes();
    let mut i = 0;
    while i < s.len() {
        if bytes[i] == 0x1b {
            let end = escape_end(s, i);
            out.push(Item::Esc(&s[i..end]));
            i = end;
            continue;
        }
        let c = s[i..].chars().next().expect("char boundary");
        i += c.len_utf8();
        match c {
            '\t' => out.extend(std::iter::repeat_n(Item::Char(' ', 1), 4)),
            c if c < ' ' || c == '\x7f' => {}
            c => out.push(Item::Char(c, c.width().unwrap_or(0))),
        }
    }
    out
}

/// The end (exclusive) of the escape sequence starting at `start`: CSI up to its final byte,
/// OSC/DCS/APC up to BEL or ST, otherwise ESC plus one character.
fn escape_end(s: &str, start: usize) -> usize {
    let bytes = s.as_bytes();
    let n = bytes.len();
    let Some(&kind) = bytes.get(start + 1) else {
        return n;
    };
    match kind {
        b'[' => {
            let mut j = start + 2;
            while j < n {
                if (0x40..=0x7e).contains(&bytes[j]) {
                    return j + 1;
                }
                j += 1;
            }
            n
        }
        b']' | b'P' | b'_' | b'^' => {
            let mut j = start + 2;
            while j < n {
                if bytes[j] == 0x07 {
                    return j + 1;
                }
                if bytes[j] == 0x1b && bytes.get(j + 1) == Some(&b'\\') {
                    return j + 2;
                }
                j += 1;
            }
            n
        }
        _ => {
            let c = s[start + 1..].chars().next().map_or(0, char::len_utf8);
            start + 1 + c
        }
    }
}

/// Is `esc` an SGR sequence (`ESC [ … m`)? Returns its parameters.
fn sgr_params(esc: &str) -> Option<&str> {
    esc.strip_prefix("\x1b[")?.strip_suffix('m')
}

/// The attribute a single-parameter SGR sets or clears, and whether it clears it.
fn sgr_attr(params: &str) -> Option<(u8, bool)> {
    let first = params.split(';').next()?.parse::<u16>().ok()?;
    let simple = !params.contains(';');
    Some(match first {
        1 if simple => (1, false),
        2 if simple => (2, false),
        // Clears bold and dim (see `track_sgr`).
        22 if simple => (1, true),
        3 if simple => (3, false),
        23 if simple => (3, true),
        4 if simple => (4, false),
        24 if simple => (4, true),
        7 if simple => (7, false),
        27 if simple => (7, true),
        9 if simple => (9, false),
        29 if simple => (9, true),
        30..=37 | 90..=97 if simple => (38, false),
        38 => (38, false),
        39 if simple => (38, true),
        40..=47 | 100..=107 if simple => (48, false),
        48 => (48, false),
        49 if simple => (48, true),
        _ => return None,
    })
}

/// Updates `active` (the SGR sequences open, in order) with `esc`: a reset clears it, and a
/// sequence that sets or clears one attribute (bold, italic, colour, ...) drops what it replaces,
/// so the state re-opened on a continuation row stays short.
fn track_sgr(active: &mut Vec<String>, esc: &str) {
    let Some(params) = sgr_params(esc) else {
        return;
    };
    if params.is_empty() || params == "0" {
        active.clear();
        return;
    }
    match sgr_attr(params) {
        Some((attr, off)) => {
            active.retain(|a| {
                sgr_params(a)
                    .and_then(sgr_attr)
                    .is_none_or(|(other, _)| other != attr && !(off && attr == 1 && other == 2))
            });
            if !off {
                active.push(esc.to_owned());
            }
        }
        None => active.push(esc.to_owned()),
    }
}

/// Builds rows, tracking the SGR state that is open at the end of the current row.
struct Rows {
    width: usize,
    rows: Vec<String>,
    cur: String,
    cur_w: usize,
    /// SGR sequences since the last reset, in order.
    active: Vec<String>,
    /// Whether the current row is the first of its logical line.
    first: bool,
}

impl Rows {
    fn new(width: usize) -> Self {
        Self {
            width: width.max(1),
            rows: Vec::new(),
            cur: String::new(),
            cur_w: 0,
            active: Vec::new(),
            first: true,
        }
    }

    fn esc(&mut self, esc: &str) {
        track_sgr(&mut self.active, esc);
        self.cur.push_str(esc);
    }

    fn char(&mut self, c: char, w: usize) {
        self.cur.push(c);
        self.cur_w += w;
    }

    fn emit(&mut self, items: &[Item<'_>]) {
        for item in items {
            match *item {
                Item::Esc(e) => self.esc(e),
                Item::Char(c, w) => self.char(c, w),
            }
        }
    }

    /// Applies only the escapes of `items` (the spaces at a break are dropped).
    fn emit_escapes(&mut self, items: &[Item<'_>]) {
        for item in items {
            if let Item::Esc(e) = *item {
                self.esc(e);
            }
        }
    }

    /// Ends the current row: closes its open styling and re-opens it on the next row.
    fn break_row(&mut self) {
        if !self.active.is_empty() {
            self.cur.push_str(RESET);
        }
        self.rows.push(std::mem::take(&mut self.cur));
        self.cur = self.active.concat();
        self.cur_w = 0;
        self.first = false;
    }

    /// Ends a logical line (a `\n` in the input, or the end).
    fn end_line(&mut self) {
        self.break_row();
        self.first = true;
    }

    /// Places a word after the pending `spaces`.
    fn word(&mut self, spaces: &[Item<'_>], spaces_w: usize, word: &[Item<'_>], word_w: usize) {
        if self.cur_w == 0 {
            if self.first {
                // Leading spaces of a logical line are its indentation.
                self.emit(spaces);
                if self.cur_w > 0 && self.cur_w + word_w > self.width {
                    // The indent leaves no room for the word: start it on a clean row.
                    self.break_row();
                }
            } else {
                self.emit_escapes(spaces);
            }
        } else if self.cur_w + spaces_w + word_w <= self.width {
            self.emit(spaces);
        } else {
            self.emit_escapes(spaces);
            self.break_row();
        }
        if self.cur_w + word_w <= self.width {
            self.emit(word);
            return;
        }
        // Wider than the room left: hard-break it, filling each row.
        for item in word {
            match *item {
                Item::Esc(e) => self.esc(e),
                Item::Char(c, w) => {
                    if self.cur_w > 0 && self.cur_w + w > self.width {
                        self.break_row();
                    }
                    self.char(c, w);
                }
            }
        }
    }
}

/// Word-wraps `s` (which may carry ANSI styling) to `width` columns. Each `\n` starts a new row.
/// Always returns at least one row.
pub fn wrap(s: &str, width: usize) -> Vec<String> {
    wrap_in(s, width, Vec::new())
}

/// [`wrap`], for text that continues a row whose styling `active` is open: continuation rows
/// re-open it too.
fn wrap_in(s: &str, width: usize, active: Vec<String>) -> Vec<String> {
    let mut rows = Rows::new(width);
    rows.active = active;
    for line in s.split('\n') {
        let mut spaces: Vec<Item<'_>> = Vec::new();
        let mut spaces_w = 0;
        let mut word: Vec<Item<'_>> = Vec::new();
        let mut word_w = 0;
        for item in items(line) {
            match item {
                Item::Char(' ', w) => {
                    if word_w > 0 {
                        rows.word(&spaces, spaces_w, &word, word_w);
                        spaces.clear();
                        spaces_w = 0;
                        word.clear();
                        word_w = 0;
                    }
                    spaces.push(item);
                    spaces_w += w;
                }
                Item::Char(_, w) => {
                    word.push(item);
                    word_w += w;
                }
                Item::Esc(_) => {
                    if word_w > 0 {
                        word.push(item);
                    } else {
                        spaces.push(item);
                    }
                }
            }
        }
        if word_w > 0 {
            rows.word(&spaces, spaces_w, &word, word_w);
        } else {
            // Trailing spaces are dropped (a blank line keeps nothing); their escapes still apply.
            rows.emit_escapes(&spaces);
        }
        rows.end_line();
    }
    rows.rows
}

/// The SGR sequences still open at the end of `s`.
fn open_sgr_list(s: &str) -> Vec<String> {
    let mut active = Vec::new();
    for item in items(s) {
        if let Item::Esc(e) = item {
            track_sgr(&mut active, e);
        }
    }
    active
}

/// The SGR state open at the end of `s`, as one string of sequences (empty when none).
pub fn open_sgr(s: &str) -> String {
    open_sgr_list(s).concat()
}

/// Splits `s` after `col` visible columns (escapes before the cut stay in the head).
pub fn split_at_col(s: &str, col: usize) -> (&str, &str) {
    let bytes = s.as_bytes();
    let mut i = 0;
    let mut w = 0;
    while i < s.len() {
        if bytes[i] == 0x1b {
            i = escape_end(s, i);
            continue;
        }
        if w >= col {
            break;
        }
        let c = s[i..].chars().next().expect("char boundary");
        w += c.width().unwrap_or(0);
        i += c.len_utf8();
    }
    (&s[..i], &s[i..])
}

/// The visible text of `s` (escapes removed).
pub fn plain(s: &str) -> String {
    items(s)
        .into_iter()
        .filter_map(|i| match i {
            Item::Char(c, _) => Some(c),
            Item::Esc(_) => None,
        })
        .collect()
}

/// How a row's continuation lines are indented.
#[derive(Debug, PartialEq)]
struct Hang {
    /// Columns of the row's prefix (indent, list marker, quote borders).
    cols: usize,
    /// Whether continuations repeat the prefix (quote borders) rather than indent under it.
    repeat: bool,
}

/// The prefix of a rendered prose row: its leading spaces, then a list marker (`• `, `☐ `, `☒ `,
/// `12. `, `3) `) or quote borders (`│ │ `).
fn hang(plain: &str) -> Hang {
    let lead = plain.len() - plain.trim_start_matches(' ').len();
    let rest = &plain[lead..];
    let mut borders = 0;
    let mut r = rest;
    while let Some(next) = r.strip_prefix("│ ") {
        borders += 1;
        r = next;
    }
    if borders > 0 {
        return Hang {
            cols: lead + borders * 2,
            repeat: true,
        };
    }
    for marker in ["• ", "☐ ", "☒ "] {
        if rest.starts_with(marker) {
            let w: usize = marker.chars().map(|c| c.width().unwrap_or(0)).sum();
            return Hang {
                cols: lead + w,
                repeat: false,
            };
        }
    }
    let digits = rest.bytes().take_while(u8::is_ascii_digit).count();
    if digits > 0
        && matches!(rest.as_bytes().get(digits), Some(b'.' | b')'))
        && rest.as_bytes().get(digits + 1) == Some(&b' ')
    {
        return Hang {
            cols: lead + digits + 2,
            repeat: false,
        };
    }
    Hang {
        cols: lead,
        repeat: false,
    }
}

/// Word-wraps one rendered row to `width`, with continuations hanging under the row's text: under
/// a list item's text, behind a quote's borders, or at the row's indentation.
pub fn reflow_row(row: &str, width: usize) -> Vec<String> {
    let width = width.max(1);
    let plain_row = plain(row);
    let mut h = hang(&plain_row);
    // Keep at least a third of the row (and 8 columns) for text; otherwise hang at the margin.
    if h.cols + (width / 3).max(8) > width {
        h = Hang {
            cols: 0,
            repeat: false,
        };
    }
    let (head, body) = split_at_col(row, h.cols);
    let head_style = open_sgr_list(head);
    let continuation = if h.repeat {
        let mut c = head.to_owned();
        if !head_style.is_empty() {
            c.push_str(RESET);
        }
        c
    } else {
        " ".repeat(h.cols)
    };
    // The body continues whatever styling the head left open.
    wrap_in(body, width - h.cols, head_style)
        .into_iter()
        .enumerate()
        .map(|(i, part)| {
            if i == 0 {
                format!("{head}{part}")
            } else {
                format!("{continuation}{part}")
            }
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn plain_rows(rows: &[String]) -> Vec<String> {
        rows.iter().map(|r| plain(r)).collect()
    }

    #[test]
    fn breaks_at_spaces_and_drops_them() {
        let rows = wrap("one two three four five", 9);
        assert_eq!(plain_rows(&rows), ["one two", "three", "four five"]);
    }

    #[test]
    fn a_word_that_fits_exactly_is_kept_whole() {
        assert_eq!(plain_rows(&wrap("abc defgh", 9)), ["abc defgh"]);
        assert_eq!(plain_rows(&wrap("abc defghi", 9)), ["abc", "defghi"]);
    }

    #[test]
    fn only_tokens_wider_than_the_row_break_inside() {
        let rows = wrap("see https://example.com/a/very/long/path ok", 12);
        assert_eq!(
            plain_rows(&rows),
            ["see", "https://exam", "ple.com/a/ve", "ry/long/path", "ok"]
        );
    }

    #[test]
    fn leading_indent_is_kept_on_the_first_row_only() {
        let rows = wrap("  alpha beta gamma", 12);
        assert_eq!(plain_rows(&rows), ["  alpha beta", "gamma"]);
    }

    #[test]
    fn newlines_start_rows_and_blank_lines_survive() {
        assert_eq!(plain_rows(&wrap("a\n\nb", 5)), ["a", "", "b"]);
    }

    #[test]
    fn styling_is_closed_and_reopened_across_a_break() {
        let rows = wrap("\x1b[1mbold words here\x1b[0m done", 10);
        assert_eq!(plain_rows(&rows), ["bold words", "here done"]);
        assert_eq!(rows[0], "\x1b[1mbold words\x1b[0m");
        assert!(rows[1].starts_with("\x1b[1mhere"), "{:?}", rows[1]);
    }

    #[test]
    fn wide_characters_count_two_columns() {
        assert_eq!(plain_rows(&wrap("日本語 です", 6)), ["日本語", "です"]);
    }

    #[test]
    fn reflow_hangs_list_items_under_their_text() {
        let rows = reflow_row("   • alpha beta gamma delta", 16);
        assert_eq!(plain_rows(&rows), ["   • alpha beta", "     gamma delta"]);
        let rows = reflow_row(" 12. alpha beta gamma", 14);
        assert_eq!(plain_rows(&rows), [" 12. alpha", "     beta", "     gamma"]);
    }

    #[test]
    fn reflow_repeats_quote_borders() {
        let rows = reflow_row("  │ alpha beta gamma", 14);
        assert_eq!(plain_rows(&rows), ["  │ alpha beta", "  │ gamma"]);
    }

    #[test]
    fn reflow_keeps_a_paragraph_at_its_indent() {
        let rows = reflow_row(" one two three", 9);
        assert_eq!(plain_rows(&rows), [" one two", " three"]);
    }

    #[test]
    fn reflow_carries_the_heads_style_into_the_body() {
        let rows = reflow_row("\x1b[3m one two three\x1b[0m", 9);
        assert_eq!(plain_rows(&rows), [" one two", " three"]);
        assert!(rows[1].starts_with(" \x1b[3m"), "{:?}", rows[1]);
    }

    #[test]
    fn closed_attributes_are_not_reopened() {
        let rows = wrap("\x1b[1mbold\x1b[22m \x1b[3mitalic words\x1b[23m tail", 12);
        assert_eq!(plain_rows(&rows), ["bold italic", "words tail"]);
        assert_eq!(rows[1], "\x1b[3mwords\x1b[23m tail");
        let mut active = Vec::new();
        for e in ["\x1b[31m", "\x1b[1m", "\x1b[32m", "\x1b[39m"] {
            track_sgr(&mut active, e);
        }
        assert_eq!(active, ["\x1b[1m"]);
    }

    #[test]
    fn split_at_col_keeps_escapes_in_the_head() {
        assert_eq!(
            split_at_col("\x1b[1mab\x1b[0mcd", 2),
            ("\x1b[1mab\x1b[0m", "cd")
        );
        assert_eq!(split_at_col("ab", 0), ("", "ab"));
    }
}
