//! GFM tables for the transcript (#2180).
//!
//! rpi-tui 0.8.0 draws a table with no cell padding, a rule between every body row, widths from
//! its own width count and a proportional shrink that can overrun the row, and it only knows the
//! `| a | b |` form (a `a | b` table stays prose with ASCII pipes). [`crate::transcript::WrapMarkdown`]
//! renders tables here instead:
//!
//! - box-drawing glyphs only (`┌┬┐ ├┼┤ └┴┘ │ ─`), one space of padding each side of a cell;
//! - a rule under the header and none between body rows;
//! - column alignment from the separator (`:--`, `:-:`, `--:`);
//! - a table wider than the row shrinks its widest columns and word-wraps their cells (every
//!   row of a record keeps its `│` joints aligned);
//! - when even that can't fit (too many columns for the width), each body row becomes a
//!   `• Header: value` list, so nothing is cut off.
//!
//! Widths are measured the same way [`crate::wrap`] measures them (Unicode width of the visible
//! text), so the joints always meet.

use unicode_width::UnicodeWidthChar;

use crate::wrap::{plain, reflow_row, wrap};

/// A column's alignment, from the separator row.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Align {
    Left,
    Center,
    Right,
}

/// A parsed table: source cells (still markdown), one alignment per header column.
#[derive(Debug, PartialEq)]
pub struct Table {
    pub header: Vec<String>,
    pub align: Vec<Align>,
    pub rows: Vec<Vec<String>>,
}

/// The narrowest a shrunk column gets, unless its content is narrower.
const MIN_COL: usize = 6;

/// Visible width of a styled string.
pub fn width(s: &str) -> usize {
    plain(s).chars().map(|c| c.width().unwrap_or(0)).sum()
}

/// Splits a row into trimmed cells. Escaped pipes (`\|`) and pipes inside code spans don't
/// separate cells; leading and trailing pipes are optional.
pub fn split_cells(row: &str) -> Vec<String> {
    let mut t = row.trim();
    if let Some(rest) = t.strip_prefix('|') {
        t = rest;
    }
    if t.ends_with('|') && !t.ends_with("\\|") {
        t = &t[..t.len() - 1];
    }
    let mut cells = vec![String::new()];
    let mut escaped = false;
    let mut code = false;
    for ch in t.chars() {
        let cell = cells.last_mut().expect("one cell");
        if escaped {
            // `\|` is a literal pipe; other escapes stay for the inline renderer.
            if ch != '|' {
                cell.push('\\');
            }
            cell.push(ch);
            escaped = false;
        } else if ch == '\\' {
            escaped = true;
        } else if ch == '`' {
            code = !code;
            cell.push(ch);
        } else if ch == '|' && !code {
            cells.push(String::new());
        } else {
            cell.push(ch);
        }
    }
    if escaped {
        cells.last_mut().expect("one cell").push('\\');
    }
    cells.into_iter().map(|c| c.trim().to_owned()).collect()
}

/// Whether `line` has a pipe that separates cells (not escaped, not in a code span).
pub fn has_pipe(line: &str) -> bool {
    split_cells(&format!("|{}|", line.trim())).len() > 1
}

/// Whether `line` is a separator row (`|---|:--:|`, pipes optional when there are two or more
/// columns).
pub fn is_separator(line: &str) -> bool {
    let t = line.trim();
    if t.is_empty() || !(t.contains('|') || t.starts_with([':', '-'])) {
        return false;
    }
    let cells = split_cells(t);
    (t.contains('|') || cells.len() > 1)
        && cells.iter().all(|c| {
            !c.is_empty()
                && c.contains('-')
                && c.chars().all(|ch| matches!(ch, '-' | ':' | ' ' | '\t'))
        })
}

/// The alignment a separator cell asks for.
fn align_of(cell: &str) -> Align {
    match (cell.starts_with(':'), cell.ends_with(':')) {
        (true, true) => Align::Center,
        (false, true) => Align::Right,
        _ => Align::Left,
    }
}

/// Parses a table block: a header row, a separator, then body rows (short rows are padded,
/// extra cells dropped). `None` without a header and separator.
pub fn parse(lines: &[&str]) -> Option<Table> {
    let header = split_cells(lines.first()?);
    let mut sep = split_cells(lines.get(1)?);
    let n = header.len();
    sep.resize(n, String::new());
    let rows = lines[2..]
        .iter()
        .map(|l| {
            let mut cells = split_cells(l);
            cells.resize(n, String::new());
            cells
        })
        .collect();
    Some(Table {
        header,
        align: sep.iter().map(|c| align_of(c)).collect(),
        rows,
    })
}

/// The widest word of a styled string.
fn longest_word(s: &str) -> usize {
    plain(s)
        .split(' ')
        .map(|w| w.chars().map(|c| c.width().unwrap_or(0)).sum())
        .max()
        .unwrap_or(0)
}

/// Column widths that fit `avail` columns of content, or `None` when they can't.
///
/// Natural widths when they fit. Otherwise every column gets a floor and the rest of the room
/// goes to the columns that still want it, in proportion to what they lack. The floor is the
/// column's widest word, so no word breaks, when those all fit; failing that, the widest word
/// up to [`MIN_COL`] (or the header's widest word, if wider), so only long tokens break.
fn layout(cells: &[Vec<String>], n: usize, avail: usize) -> Option<Vec<usize>> {
    let natural: Vec<usize> = (0..n)
        .map(|c| cells.iter().map(|r| width(&r[c])).max().unwrap_or(0).max(1))
        .collect();
    if natural.iter().sum::<usize>() <= avail {
        return Some(natural);
    }
    let words: Vec<usize> = (0..n)
        .map(|c| {
            let w = cells.iter().map(|r| longest_word(&r[c])).max().unwrap_or(0);
            w.min(natural[c]).max(1)
        })
        .collect();
    let floor: Vec<usize> = if words.iter().sum::<usize>() <= avail {
        words
    } else {
        (0..n)
            .map(|c| {
                let head = longest_word(&cells[0][c]);
                words[c].min(MIN_COL).max(head).min(natural[c]).max(1)
            })
            .collect()
    };
    let base: usize = floor.iter().sum();
    if base > avail {
        return None;
    }
    let mut widths = floor.clone();
    let mut spare = avail - base;
    let want: Vec<usize> = (0..n).map(|c| natural[c] - floor[c]).collect();
    let total_want: usize = want.iter().sum();
    for c in 0..n {
        let share = (want[c] * spare) / total_want.max(1);
        widths[c] += share.min(want[c]);
    }
    spare = avail - widths.iter().sum::<usize>();
    // Hand out what rounding left, one column at a time, to those still short.
    while spare > 0 {
        let Some(c) = (0..n)
            .filter(|&c| widths[c] < natural[c])
            .max_by_key(|&c| natural[c] - widths[c])
        else {
            break;
        };
        widths[c] += 1;
        spare -= 1;
    }
    Some(widths)
}

/// Pads `s` (already at most `w` wide) to `w` columns.
fn pad(s: &str, w: usize, align: Align) -> String {
    let gap = w.saturating_sub(width(s));
    let (l, r) = match align {
        Align::Left => (0, gap),
        Align::Right => (gap, 0),
        Align::Center => (gap / 2, gap - gap / 2),
    };
    format!("{}{s}{}", " ".repeat(l), " ".repeat(r))
}

/// How a rendered table looks: its left margin, how to draw borders and header text.
pub struct Style<'a> {
    pub margin: &'a str,
    pub border: &'a dyn Fn(&str) -> String,
    pub header: &'a dyn Fn(&str) -> String,
}

/// Renders `table` into rows at most `cols` wide (margin included). `inline` renders a cell's
/// markdown to one styled line.
pub fn render(
    table: &Table,
    cols: usize,
    style: &Style<'_>,
    inline: &dyn Fn(&str) -> String,
) -> Vec<String> {
    let n = table.header.len();
    let header: Vec<String> = table
        .header
        .iter()
        .map(|c| (style.header)(&inline(c)))
        .collect();
    let body: Vec<Vec<String>> = table
        .rows
        .iter()
        .map(|r| r.iter().map(|c| inline(c)).collect())
        .collect();
    let content = cols.saturating_sub(width(style.margin));
    // `│ a │ b │`: a bar before every column and one after, and a space each side of a cell.
    let overhead = 3 * n + 1;
    let mut all = vec![header.clone()];
    all.extend(body.iter().cloned());
    let Some(widths) = content
        .checked_sub(overhead)
        .and_then(|avail| layout(&all, n, avail))
    else {
        return records(&header, &body, content, style);
    };

    let rule = |l: &str, m: &str, r: &str| {
        let segs: Vec<String> = widths.iter().map(|w| "─".repeat(w + 2)).collect();
        format!(
            "{}{}",
            style.margin,
            (style.border)(&format!("{l}{}{r}", segs.join(m)))
        )
    };
    let bar = (style.border)("│");
    let row = |cells: &[String], out: &mut Vec<String>| {
        let wrapped: Vec<Vec<String>> = cells
            .iter()
            .zip(&widths)
            .map(|(c, &w)| wrap(c, w))
            .collect();
        let height = wrapped.iter().map(Vec::len).max().unwrap_or(1);
        for i in 0..height {
            let mut line = format!("{}{bar}", style.margin);
            for (c, w) in widths.iter().enumerate() {
                let piece = wrapped[c].get(i).map(String::as_str).unwrap_or("");
                line.push(' ');
                line.push_str(&pad(piece, *w, table.align[c]));
                line.push(' ');
                line.push_str(&bar);
            }
            out.push(line);
        }
    };

    let mut out = vec![rule("┌", "┬", "┐")];
    row(&header, &mut out);
    out.push(rule("├", "┼", "┤"));
    for r in &body {
        row(r, &mut out);
    }
    out.push(rule("└", "┴", "┘"));
    out
}

/// The fallback for a table too wide to draw: each body row as a bulleted `Header: value` list,
/// word-wrapped under the value.
fn records(header: &[String], body: &[Vec<String>], cols: usize, style: &Style<'_>) -> Vec<String> {
    let mut out = Vec::new();
    for (i, r) in body.iter().enumerate() {
        if i > 0 {
            out.push(String::new());
        }
        for (c, value) in r.iter().enumerate() {
            let lead = if c == 0 { "• " } else { "  " };
            let line = format!("{lead}{}: {value}", header[c]);
            out.extend(
                reflow_row(&line, cols)
                    .into_iter()
                    .map(|l| format!("{}{l}", style.margin)),
            );
        }
    }
    if body.is_empty() {
        let line = header.join(" · ");
        out.extend(
            reflow_row(&line, cols)
                .into_iter()
                .map(|l| format!("{}{l}", style.margin)),
        );
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ident(s: &str) -> String {
        s.to_owned()
    }

    fn style<'a>(f: &'a dyn Fn(&str) -> String) -> Style<'a> {
        Style {
            margin: " ",
            border: f,
            header: f,
        }
    }

    fn draw(src: &str, cols: usize) -> Vec<String> {
        let lines: Vec<&str> = src.lines().collect();
        let t = parse(&lines).expect("table");
        let f = ident;
        render(&t, cols, &style(&f), &ident)
    }

    #[test]
    fn split_cells_handles_optional_pipes_escapes_and_code() {
        assert_eq!(split_cells("| a | b |"), vec!["a", "b"]);
        assert_eq!(split_cells("a | b"), vec!["a", "b"]);
        assert_eq!(split_cells(r"| a \| b | c |"), vec!["a | b", "c"]);
        assert_eq!(split_cells("| `x|y` | c |"), vec!["`x|y`", "c"]);
        assert_eq!(split_cells(r"| a\*b |"), vec![r"a\*b"]);
    }

    #[test]
    fn separators() {
        assert!(is_separator("|---|:--:|"));
        assert!(is_separator("--- | ---:"));
        assert!(!is_separator("---"));
        assert!(!is_separator("| a | b |"));
        assert!(!is_separator("|  | --- |"));
    }

    #[test]
    fn parse_reads_alignment_and_pads_short_rows() {
        let t = parse(&["| a | b | c |", "|:--|:-:|--:|", "| 1 |"]).unwrap();
        assert_eq!(t.align, vec![Align::Left, Align::Center, Align::Right]);
        assert_eq!(t.rows, vec![vec!["1", "", ""]]);
        assert_eq!(parse(&["| a | b |", "|---|"]).unwrap().align.len(), 2);
        assert!(parse(&["| a |"]).is_none());
    }

    #[test]
    fn natural_layout_pads_one_space_and_rules_only_the_header() {
        let rows = draw(
            "| id | name |\n|---|--:|\n| 1 | one |\n| 22 | two two |",
            80,
        );
        assert_eq!(
            rows,
            vec![
                " ┌────┬─────────┐",
                " │ id │    name │",
                " ├────┼─────────┤",
                " │ 1  │     one │",
                " │ 22 │ two two │",
                " └────┴─────────┘",
            ]
        );
    }

    #[test]
    fn narrow_width_wraps_cells_and_keeps_joints_aligned() {
        let src =
            "| key | description |\n|---|---|\n| a | the quick brown fox jumps over the lazy dog |";
        let rows = draw(src, 30);
        for r in &rows {
            assert!(width(r) <= 30, "{r:?}");
        }
        let joints: Vec<usize> = rows[0]
            .char_indices()
            .filter(|(_, c)| *c == '┬')
            .map(|(i, _)| rows[0][..i].chars().count())
            .collect();
        for r in rows.iter().skip(1).filter(|r| r.contains('│')) {
            let bars: Vec<usize> = r
                .chars()
                .enumerate()
                .filter(|(_, c)| matches!(c, '│' | '┼'))
                .map(|(i, _)| i)
                .collect();
            assert!(joints.iter().all(|j| bars.contains(j)), "{rows:#?}");
        }
        assert!(rows.len() > 5, "the description wraps: {rows:#?}");
    }

    #[test]
    fn too_many_columns_fall_back_to_records() {
        let src = "| alpha | bravo | charlie | delta |\n|---|---|---|---|\n| 1 | 2 | 3 | 4 |\n| 5 | 6 | 7 | 8 |";
        let rows = draw(src, 20);
        assert_eq!(
            rows,
            vec![
                " • alpha: 1",
                "   bravo: 2",
                "   charlie: 3",
                "   delta: 4",
                "",
                " • alpha: 5",
                "   bravo: 6",
                "   charlie: 7",
                "   delta: 8",
            ]
        );
    }

    #[test]
    fn wide_glyphs_are_measured_like_the_terminal() {
        let rows = draw("| s | n |\n|---|---|\n| ✅ | x |\n| ok | y |", 80);
        let w: Vec<usize> = rows.iter().map(|r| width(r)).collect();
        assert!(w.iter().all(|&x| x == w[0]), "{rows:#?}");
    }
}
