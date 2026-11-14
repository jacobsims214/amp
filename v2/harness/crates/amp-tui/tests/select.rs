//! Select-to-copy's highlight layer and `ChatView` selection API (story #580, #4215).
//!
//! The transcript's rows carry #4210's in-band row markers; the selection layer between the
//! scroll view and the inset takes them off, paints the selection in reverse video in content
//! space (so it scrolls with the text), and extracts the selected text as shown. Everything here
//! goes through `ChatView::render_frame`: no terminal, no clipboard.

use std::sync::Arc;

use amp_tui::scroll::WHEEL_LINES;
use amp_tui::selection::Pos;
use amp_tui::wrap::{encode_row_meta, hard_wrap_marked, reflow_row_marked, wrap_marked};
use amp_tui::{ChatView, SessionParts};
use rpi_tui::{Component, Container, visible_width};

const W: usize = 80;
const H: usize = 30;
const REV: &str = "\x1b[7m";

/// How a test block wraps its text.
#[derive(Clone, Copy)]
enum Wrap {
    Words,
    Hard,
    Reflow,
}

/// A transcript block that wraps its text the way the transcript does and marks each row,
/// as #4214's components do.
struct Marked(String, Wrap);

impl Component for Marked {
    fn render(&self, width: usize) -> Vec<String> {
        let (rows, meta) = match self.1 {
            Wrap::Words => wrap_marked(&self.0, width),
            Wrap::Hard => hard_wrap_marked(&self.0, width),
            Wrap::Reflow => reflow_row_marked(&self.0, width),
        };
        rows.into_iter()
            .zip(meta)
            .map(|(row, m)| format!("{row}{}", encode_row_meta(&m)))
            .collect()
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}

fn block(text: &str, wrap: Wrap) -> Arc<dyn Component> {
    Arc::new(Marked(text.to_owned(), wrap))
}

fn view_with(blocks: &[(&str, Wrap)]) -> ChatView {
    let view = ChatView::new();
    for (text, wrap) in blocks {
        view.add_to_transcript(block(text, *wrap));
    }
    view
}

/// The content cell of the first screen cell showing `needle`.
fn locate(view: &ChatView, width: usize, needle: &str) -> Pos {
    let frame = view.render_plain(width, H);
    let (row, line) = frame
        .iter()
        .enumerate()
        .find(|(_, l)| l.contains(needle))
        .unwrap_or_else(|| panic!("{needle:?} not on screen:\n{}", frame.join("\n")));
    let col = visible_width(&line[..line.find(needle).unwrap()]);
    view.content_pos(width, H, row, col)
        .unwrap_or_else(|| panic!("{needle:?} at ({row}, {col}) is outside the transcript"))
}

fn right(p: Pos, cols: usize) -> Pos {
    Pos::new(p.row, p.col + cols)
}

fn select(view: &ChatView, from: Pos, to: Pos) {
    view.select_start(from);
    view.select_extend(to);
}

/// The screen rows showing the highlight.
fn highlighted(view: &ChatView, width: usize) -> Vec<usize> {
    view.render_frame(width, H)
        .iter()
        .enumerate()
        .filter(|(_, l)| l.contains(REV))
        .map(|(i, _)| i)
        .collect()
}

fn frame_has(view: &ChatView, needle: &str) -> bool {
    view.render_frame(W, H).iter().any(|l| l.contains(needle))
}

#[test]
fn ascii_selection_paints_and_copies_exact_text() {
    let view = view_with(&[("hello world, said the transcript", Wrap::Words)]);
    let at = locate(&view, W, "world");
    select(&view, at, right(at, 4));
    assert_eq!(view.select_text().as_deref(), Some("world"));
    assert!(frame_has(&view, "\x1b[7mworld\x1b[27m"));
    assert_eq!(highlighted(&view, W).len(), 1);
    // Backwards drag selects the same cells.
    select(&view, right(at, 4), at);
    assert_eq!(view.select_text().as_deref(), Some("world"));
}

#[test]
fn cjk_selection_snaps_to_whole_glyphs() {
    let view = view_with(&[("前 日本語テキスト 後", Wrap::Words)]);
    let at = locate(&view, W, "日本語");
    // From the right half of 日 to the right half of 本.
    select(&view, right(at, 1), right(at, 3));
    assert_eq!(view.select_text().as_deref(), Some("日本"));
    assert!(frame_has(&view, "\x1b[7m日本\x1b[27m"));
}

#[test]
fn emoji_selection_snaps_to_whole_glyphs() {
    let view = view_with(&[("go 😀 now", Wrap::Words)]);
    let at = locate(&view, W, "😀");
    // The emoji's right half to the `n` after it.
    select(&view, right(at, 1), right(at, 3));
    assert_eq!(view.select_text().as_deref(), Some("😀 n"));
    assert!(frame_has(&view, "\x1b[7m😀 n\x1b[27m"));
}

#[test]
fn wrapped_paragraph_rejoins_and_highlights_every_row() {
    let words: Vec<String> = (0..40).map(|i| format!("w{i:02}")).collect();
    let text = words.join(" ");
    let view = view_with(&[(&text, Wrap::Words)]);
    let start = locate(&view, W, "w00");
    let end = locate(&view, W, "w39");
    assert!(end.row > start.row + 1, "the paragraph wraps");
    select(&view, start, right(end, 2));
    assert_eq!(view.select_text().as_deref(), Some(text.as_str()));
    assert_eq!(highlighted(&view, W).len(), end.row - start.row + 1);

    // A hard-wrapped (code) line rejoins byte-exact, inner spaces included.
    let code = format!("let  x = \"{}\";", "a".repeat(120));
    let view = view_with(&[(&code, Wrap::Hard)]);
    let start = locate(&view, W, "let  x");
    select(&view, start, Pos::new(start.row + 10, 0));
    assert_eq!(view.select_text().as_deref(), Some(code.as_str()));

    // A list item's continuation rows skip their hanging indent.
    let item = format!("• {}", words[..30].join(" "));
    let view = view_with(&[(&item, Wrap::Reflow)]);
    let start = locate(&view, W, "• w00");
    select(&view, start, Pos::new(start.row + 10, 0));
    assert_eq!(view.select_text().as_deref(), Some(item.as_str()));
}

#[test]
fn highlight_moves_with_scroll() {
    let lines: Vec<String> = (0..120)
        .map(|i| format!("line {i:03} of the log"))
        .collect();
    let blocks: Vec<(&str, Wrap)> = lines.iter().map(|l| (l.as_str(), Wrap::Words)).collect();
    let view = view_with(&blocks);
    view.render_frame(W, H);
    view.scroll().scroll_by(-20);
    let at = locate(&view, W, "line 090");
    select(&view, at, right(at, 7));
    let before = highlighted(&view, W);
    assert_eq!(before.len(), 1);

    view.scroll().scroll_by(-WHEEL_LINES);
    let after = highlighted(&view, W);
    assert_eq!(after, vec![before[0] + WHEEL_LINES as usize]);
    let row = &view.render_plain(W, H)[after[0]];
    assert!(row.contains("line 090"), "{row:?}");
    assert_eq!(view.select_text().as_deref(), Some("line 090"));
    // The screen-to-content mapping follows the scroll too.
    assert_eq!(locate(&view, W, "line 090"), at);
}

#[test]
fn worker_session_selection_extracts_that_workers_text() {
    let view = view_with(&[("manager text here", Wrap::Words)]);
    let worker = view_with(&[("worker says hello", Wrap::Words)]);
    view.show_session(Some(SessionParts {
        transcript: worker.transcript_component(),
        bar: Arc::new(Container::new()),
        keys: Arc::new(Container::new()),
    }));
    let at = locate(&view, W, "worker says");
    select(&view, at, right(at, 10));
    assert_eq!(view.select_text().as_deref(), Some("worker says"));
    assert!(frame_has(&view, "\x1b[7mworker says\x1b[27m"));
}

#[test]
fn no_row_marker_reaches_the_frame() {
    let view = view_with(&[
        ("plain words wrapped across a row or two", Wrap::Words),
        ("code_row_that_is_hard_wrapped", Wrap::Hard),
        ("• a list item", Wrap::Reflow),
    ]);
    for width in [W, 50] {
        let frame = view.render_frame(width, H);
        assert!(frame.iter().all(|l| !l.contains("\x1b_amp:")), "{frame:?}");
        assert!(
            view.render_plain(width, H)
                .iter()
                .any(|l| l.contains("plain words"))
        );
    }
    let at = locate(&view, W, "plain");
    select(&view, at, right(at, 4));
    assert!(
        view.render_frame(W, H)
            .iter()
            .all(|l| !l.contains("\x1b_amp:"))
    );
}

#[test]
fn a_press_without_a_drag_selects_nothing() {
    let view = view_with(&[("hello world", Wrap::Words)]);
    let at = locate(&view, W, "world");
    select(&view, at, at);
    assert_eq!(view.selection(), None);
    assert_eq!(view.select_text(), None);
    assert!(highlighted(&view, W).is_empty());
    // A selection of blanks copies nothing.
    let view = view_with(&[("zq", Wrap::Words)]);
    let at = locate(&view, W, "zq");
    select(&view, right(at, 3), right(at, 9));
    assert_eq!(view.select_text(), None);
}

#[test]
fn reflowing_the_transcript_clears_the_selection() {
    let view = view_with(&[("hello world", Wrap::Words)]);
    let selected = |view: &ChatView| {
        let at = locate(view, W, "world");
        select(view, at, right(at, 4));
        assert!(view.selection().is_some());
    };

    selected(&view);
    view.clear_selection();
    assert_eq!(view.selection(), None);

    selected(&view);
    view.toggle_tools();
    assert_eq!(view.selection(), None);
    assert!(highlighted(&view, W).is_empty());

    selected(&view);
    view.show_session(None);
    assert_eq!(view.selection(), None);

    selected(&view);
    view.render_frame(W + 10, H);
    assert_eq!(view.selection(), None, "a width change clears it");

    let mut view = view;
    selected(&view);
    view.clear_transcript();
    assert_eq!(view.selection(), None);
}

#[test]
fn content_pos_is_none_outside_the_transcript_and_clamps_for_drags() {
    let view = view_with(&[("hello", Wrap::Words)]);
    let at = locate(&view, W, "hello");
    // The last screen row is the footer, outside the transcript.
    assert_eq!(view.content_pos(W, H, H - 1, 0), None);
    let clamped = view.content_pos_clamped(W, H, 0, 0).expect("a transcript");
    assert_eq!(clamped.col, 0);
    assert_eq!(clamped.row, view.scroll().top());
    assert!(clamped.row <= at.row);
}
