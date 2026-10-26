//! Left and right padding around the chat (#2251): `[tui] padding` columns on each side.
//!
//! [`Inset`] wraps a component: it renders the child [`Padding::columns`] narrower and indents
//! every row (except a [`FLUSH_LEFT`] one), so the right edge keeps the same gap (the child
//! wraps, truncates and right-aligns at the narrower width). [`crate::ChatView`] wraps every area in one: the header, the
//! transcript (inside its scroll view, so the scroll view still spans the screen and the
//! "↓ new output" pill stays centred), the status slot, the AMP panels, the `/` menu, the editor,
//! the AMP status line and the footer. They share one [`Padding`], so changing it moves them all.
//!
//! A blank row above and below the whole chat too (#2344, [`Gap`]): [`VERTICAL`] rows on
//! terminals at least [`SHORT`] rows tall, none on shorter ones.
//!
//! At [`NARROW`] columns or fewer the padding halves (rounded up: the default 2 becomes 1, #2344),
//! and narrow terminals keep [`MIN_INNER`] columns for the content: the padding shrinks (to 0 at
//! `MIN_INNER + 1` columns or fewer) before any area does.

use std::sync::{
    Arc,
    atomic::{AtomicUsize, Ordering},
};

use rpi_tui::Component;

/// Columns on each side by default.
pub const DEFAULT_PADDING: usize = 2;
/// The most `[tui] padding` accepts.
pub const MAX_PADDING: usize = 8;
/// The padding shrinks rather than leave the content fewer columns than this.
pub const MIN_INNER: usize = 40;
/// At this many columns or fewer the padding is halved, rounded up (#2344): 1 column each side
/// by default on an 80-column terminal, 2 on wider ones.
pub const NARROW: usize = 80;

/// A row that starts with this is not indented (#2423): the pairing link's rows start at the
/// screen's left edge, so a terminal selection of them has no padding to remove. It is an OSC 8
/// end-of-hyperlink with no hyperlink open: the terminal draws and does nothing.
pub const FLUSH_LEFT: &str = "\x1b]8;;\x07";

/// Blank rows above and below the chat (#2344).
pub const VERTICAL: usize = 1;
/// Terminals shorter than this get no blank rows above and below.
pub const SHORT: usize = 24;

/// The blank rows at the top and bottom of the screen (#2344): one [`Gap`] component above the
/// header and one below the footer share it; [`Gap::set_height`] picks the rows for the
/// terminal's height before each frame, so the layout (the transcript's viewport, the cursor,
/// clicks) accounts for them.
#[derive(Debug, Clone)]
pub struct Gap(Arc<AtomicUsize>);

impl Default for Gap {
    fn default() -> Self {
        Self(Arc::new(AtomicUsize::new(VERTICAL)))
    }
}

impl Gap {
    /// The rows for a terminal `height` rows tall.
    pub fn rows_at(height: usize) -> usize {
        if height >= SHORT { VERTICAL } else { 0 }
    }

    pub fn set_height(&self, height: usize) {
        self.0.store(Self::rows_at(height), Ordering::Relaxed);
    }

    pub fn rows(&self) -> usize {
        self.0.load(Ordering::Relaxed)
    }
}

impl Component for Gap {
    fn render(&self, _width: usize) -> Vec<String> {
        vec![String::new(); self.rows()]
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}

/// The padding every [`Inset`] of one screen shares.
#[derive(Debug, Clone)]
pub struct Padding(Arc<AtomicUsize>);

impl Default for Padding {
    fn default() -> Self {
        Self::new(DEFAULT_PADDING)
    }
}

impl Padding {
    pub fn new(columns: usize) -> Self {
        Self(Arc::new(AtomicUsize::new(columns.min(MAX_PADDING))))
    }

    /// The configured columns on each side.
    pub fn columns(&self) -> usize {
        self.0.load(Ordering::Relaxed)
    }

    pub fn set(&self, columns: usize) {
        self.0.store(columns.min(MAX_PADDING), Ordering::Relaxed);
    }

    /// The columns on each side at `width`: the configured padding (halved, rounded up, at
    /// [`NARROW`] columns or fewer), less what would leave the content under [`MIN_INNER`]
    /// columns.
    pub fn at(&self, width: usize) -> usize {
        let columns = self.columns();
        let columns = if width <= NARROW {
            columns.div_ceil(2)
        } else {
            columns
        };
        columns.min(width.saturating_sub(MIN_INNER) / 2)
    }
}

/// A component drawn [`Padding`] columns in from the left and right edges.
pub struct Inset {
    child: Arc<dyn Component>,
    padding: Padding,
}

impl Inset {
    pub fn new(child: Arc<dyn Component>, padding: Padding) -> Self {
        Self { child, padding }
    }

    pub fn child(&self) -> &Arc<dyn Component> {
        &self.child
    }
}

impl Component for Inset {
    fn render(&self, width: usize) -> Vec<String> {
        let pad = self.padding.at(width);
        let lines = self.child.render(width - 2 * pad);
        if pad == 0 {
            return lines;
        }
        let indent = " ".repeat(pad);
        lines
            .into_iter()
            .map(|line| {
                if line.is_empty() || line.starts_with(FLUSH_LEFT) {
                    line
                } else {
                    format!("{indent}{line}")
                }
            })
            .collect()
    }

    fn handle_input(&self, data: &str) -> bool {
        self.child.handle_input(data)
    }

    fn wants_key_release(&self) -> bool {
        self.child.wants_key_release()
    }

    fn invalidate(&self) {
        self.child.invalidate();
    }

    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Fill;

    impl Component for Fill {
        fn render(&self, width: usize) -> Vec<String> {
            vec!["x".repeat(width), String::new()]
        }
        fn invalidate(&self) {}
        fn as_any(&self) -> &dyn std::any::Any {
            self
        }
    }

    #[test]
    fn indents_and_narrows_the_child() {
        let inset = Inset::new(Arc::new(Fill), Padding::new(2));
        let lines = inset.render(120);
        assert_eq!(lines[0], format!("  {}", "x".repeat(116)));
        let lines = inset.render(80);
        assert_eq!(lines[0], format!(" {}", "x".repeat(78)));
        assert_eq!(lines[1], "", "blank rows stay blank");
    }

    /// #2423: a [`FLUSH_LEFT`] row (the pairing link) keeps the left edge.
    #[test]
    fn flush_left_rows_are_not_indented() {
        struct Link;
        impl Component for Link {
            fn render(&self, _: usize) -> Vec<String> {
                vec![format!("{FLUSH_LEFT}https://a"), "b".into()]
            }
            fn invalidate(&self) {}
            fn as_any(&self) -> &dyn std::any::Any {
                self
            }
        }
        let lines = Inset::new(Arc::new(Link), Padding::new(2)).render(120);
        assert_eq!(lines, vec![format!("{FLUSH_LEFT}https://a"), "  b".into()]);
    }

    #[test]
    fn shrinks_on_narrow_terminals() {
        let p = Padding::new(2);
        assert_eq!(p.at(127), 2);
        assert_eq!(p.at(81), 2);
        assert_eq!(p.at(80), 1, "halved at 80 columns or fewer");
        assert_eq!(p.at(44), 1);
        assert_eq!(p.at(43), 1);
        assert_eq!(p.at(42), 1);
        assert_eq!(p.at(41), 0);
        assert_eq!(p.at(10), 0);
        assert_eq!(p.at(0), 0);
        let inset = Inset::new(Arc::new(Fill), p.clone());
        assert_eq!(inset.render(30)[0], "x".repeat(30));
        p.set(0);
        assert_eq!(inset.render(80)[0], "x".repeat(80));
        p.set(4);
        assert_eq!(p.at(80), 2);
        assert_eq!(p.at(120), 4);
        p.set(1);
        assert_eq!(p.at(80), 1, "one column stays one");
    }

    #[test]
    fn a_blank_row_above_and_below_unless_the_terminal_is_short() {
        let gap = Gap::default();
        gap.set_height(40);
        assert_eq!(gap.render(80), vec![String::new()]);
        gap.set_height(24);
        assert_eq!(gap.rows(), 1);
        gap.set_height(23);
        assert!(gap.render(80).is_empty());
    }

    #[test]
    fn padding_is_capped() {
        assert_eq!(Padding::new(100).columns(), MAX_PADDING);
    }
}
