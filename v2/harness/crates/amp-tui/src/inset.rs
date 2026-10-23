//! Left and right padding around the chat (#2251): `[tui] padding` columns on each side.
//!
//! [`Inset`] wraps a component: it renders the child [`Padding::columns`] narrower and indents
//! every row, so the right edge keeps the same gap (the child wraps, truncates and right-aligns
//! at the narrower width). [`crate::ChatView`] wraps every area in one: the header, the
//! transcript (inside its scroll view, so the scroll view still spans the screen and the
//! "↓ new output" pill stays centred), the status slot, the AMP panels, the `/` menu, the editor,
//! the AMP status line and the footer. They share one [`Padding`], so changing it moves them all.
//!
//! Narrow terminals keep [`MIN_INNER`] columns for the content: the padding shrinks (to 0 at
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

    /// The columns on each side at `width`: the configured padding, less what would leave the
    /// content under [`MIN_INNER`] columns.
    pub fn at(&self, width: usize) -> usize {
        self.columns().min(width.saturating_sub(MIN_INNER) / 2)
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
                if line.is_empty() {
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
        let lines = inset.render(80);
        assert_eq!(lines[0], format!("  {}", "x".repeat(76)));
        assert_eq!(lines[1], "", "blank rows stay blank");
    }

    #[test]
    fn shrinks_on_narrow_terminals() {
        let p = Padding::new(2);
        assert_eq!(p.at(127), 2);
        assert_eq!(p.at(44), 2);
        assert_eq!(p.at(43), 1);
        assert_eq!(p.at(42), 1);
        assert_eq!(p.at(41), 0);
        assert_eq!(p.at(10), 0);
        assert_eq!(p.at(0), 0);
        let inset = Inset::new(Arc::new(Fill), p.clone());
        assert_eq!(inset.render(30)[0], "x".repeat(30));
        p.set(0);
        assert_eq!(inset.render(80)[0], "x".repeat(80));
    }

    #[test]
    fn padding_is_capped() {
        assert_eq!(Padding::new(100).columns(), MAX_PADDING);
    }
}
