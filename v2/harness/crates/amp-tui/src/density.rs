//! How tightly the chat is laid out (`[tui] density`, #2346).
//!
//! A terminal app can't change the terminal's font size, so "everything is a smidge too big"
//! comes from spacing and weight. **Compact** (the default) packs the same conversation into
//! about a third fewer rows:
//!
//! - the person's message is one surface row per line (no padding rows above and below it), with
//!   one blank row between it and the reply;
//! - a collapsed `▸ Thought for Ns` line is followed straight by the answer;
//! - a reply's text follows its tool calls with no blank row; a failed call's error goes on its
//!   own line (details with Ctrl+O);
//! - markdown headings are bold text (no underline) with at most one blank row after them, and
//!   runs of blank lines in a reply show as one;
//! - notices have one blank row above them, none below; the start banner is three dim lines and
//!   `/help` has no blank rows between its groups;
//! - tool titles and the banner are regular weight: bold is for headings;
//! - the editor's rules are drawn in the muted border colour; the footer is one row at any width.
//!
//! **Comfortable** is the layout before #2346. The setting is process-wide (like the theme): set
//! it once with [`set`] before the shell is built. [`with`] renders at another density on the
//! calling thread only (tests compare the two in one process).

use std::{
    cell::Cell,
    sync::atomic::{AtomicU8, Ordering},
};

/// The chat's density.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Density {
    #[default]
    Compact,
    Comfortable,
}

impl Density {
    /// Parses `compact` or `comfortable`.
    pub fn parse(raw: &str) -> Result<Self, String> {
        match raw.trim().to_ascii_lowercase().as_str() {
            "" | "compact" => Ok(Self::Compact),
            "comfortable" => Ok(Self::Comfortable),
            other => Err(format!(
                "unknown density `{other}`: use compact or comfortable"
            )),
        }
    }

    pub fn word(self) -> &'static str {
        match self {
            Self::Compact => "compact",
            Self::Comfortable => "comfortable",
        }
    }
}

static DENSITY: AtomicU8 = AtomicU8::new(0);

thread_local! {
    /// [`with`]'s density for this thread, over the process-wide one.
    static OVERRIDE: Cell<Option<Density>> = const { Cell::new(None) };
}

/// The density every component renders at.
pub fn get() -> Density {
    if let Some(d) = OVERRIDE.with(Cell::get) {
        return d;
    }
    match DENSITY.load(Ordering::Relaxed) {
        1 => Density::Comfortable,
        _ => Density::Compact,
    }
}

/// Runs `f` with `density` on this thread only (renders made inside it use it), then puts the
/// previous one back.
pub fn with<R>(density: Density, f: impl FnOnce() -> R) -> R {
    struct Restore(Option<Density>);
    impl Drop for Restore {
        fn drop(&mut self) {
            OVERRIDE.with(|o| o.set(self.0));
        }
    }
    let _restore = Restore(OVERRIDE.with(|o| o.replace(Some(density))));
    f()
}

/// Sets the density for every component (before the shell is built).
pub fn set(density: Density) {
    DENSITY.store(
        match density {
            Density::Compact => 0,
            Density::Comfortable => 1,
        },
        Ordering::Relaxed,
    );
}

/// Whether the chat is compact.
pub fn compact() -> bool {
    get() == Density::Compact
}

/// `text` in bold when comfortable, regular weight when compact (which keeps bold for headings):
/// tool titles, the banner's first line, notice titles.
pub fn strong(text: &str) -> String {
    if compact() {
        text.to_owned()
    } else {
        rpi_tui::bold(text)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_both_words() {
        assert_eq!(Density::parse("compact"), Ok(Density::Compact));
        assert_eq!(Density::parse(" Comfortable "), Ok(Density::Comfortable));
        assert_eq!(Density::parse(""), Ok(Density::Compact));
        assert!(
            Density::parse("cosy")
                .unwrap_err()
                .contains("compact or comfortable")
        );
        assert_eq!(Density::default().word(), "compact");
    }

    #[test]
    fn with_applies_on_this_thread_and_restores() {
        assert!(compact());
        let inner = with(Density::Comfortable, || {
            let nested = with(Density::Compact, compact);
            (get(), nested)
        });
        assert_eq!(inner, (Density::Comfortable, true));
        assert!(compact());
    }
}
