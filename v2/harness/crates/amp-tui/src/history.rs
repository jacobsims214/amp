//! Prompt history (#2348): ↑ in the editor recalls the prompts sent earlier in this chat, ↓ goes
//! forward again, and ↓ past the newest gives back the draft that was there before ↑.
//!
//! rpi-tui's `Editor` keeps a history of its own but has no keys for it and no way to read it,
//! so the shell keeps this one ([`crate::Shell::submit`] records, [`crate::Shell::key`] recalls).
//!
//! When the keys recall rather than move the cursor ([`PromptHistory::key`]):
//! - ↑ with a one-line editor (empty, or a one-line draft): the newest prompt, the draft kept;
//! - while a recalled prompt is shown unchanged: ↑ on its first row goes older, ↓ on its last
//!   row goes newer (past the newest, the draft comes back);
//! - anything else (a multi-line draft, a recalled prompt that was edited) is the editor's: ↑↓
//!   move the cursor. Editing a recalled prompt makes it the draft; the next ↑ starts over.

use rpi_tui::Editor;

/// At most this many prompts are kept; the oldest go first.
pub const HISTORY_LIMIT: usize = 500;

/// Which way a history key goes.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Recall {
    /// ↑: an older prompt.
    Older,
    /// ↓: a newer prompt, or the draft past the newest.
    Newer,
}

/// The prompts sent in this chat, oldest first, and where ↑↓ are among them.
#[derive(Debug, Default)]
pub struct PromptHistory {
    entries: Vec<String>,
    /// The entry the editor shows while browsing.
    index: Option<usize>,
    /// The editor's text when browsing started; ↓ past the newest gives it back.
    draft: String,
}

impl PromptHistory {
    pub fn new() -> Self {
        Self::default()
    }

    /// The prompts kept, oldest first.
    pub fn entries(&self) -> &[String] {
        &self.entries
    }

    /// Records a sent prompt and stops browsing. Blank text is skipped, and so is a prompt
    /// equal to the newest one (sending the same thing twice keeps one entry).
    pub fn record(&mut self, text: &str) {
        self.index = None;
        self.draft.clear();
        if text.trim().is_empty() || self.entries.last().map(String::as_str) == Some(text) {
            return;
        }
        self.entries.push(text.to_owned());
        if self.entries.len() > HISTORY_LIMIT {
            self.entries.remove(0);
        }
    }

    /// The recalled entry while the editor still shows it unchanged.
    fn browsing(&self, text: &str) -> Option<usize> {
        self.index
            .filter(|&i| self.entries.get(i).map(String::as_str) == Some(text))
    }

    /// One ↑ or ↓ given the editor's `text` and cursor row (`row` of `rows` lines). Returns the
    /// text to put in the editor, or `None` when the key is the editor's (it moves the cursor),
    /// or `Some` of the same text at the ends of the history (the key is used, nothing changes).
    pub fn key(&mut self, recall: Recall, text: &str, row: usize, rows: usize) -> Option<String> {
        let browsing = self.browsing(text);
        if browsing.is_none() {
            // Not browsing, or the recalled prompt was edited: it is now the draft.
            self.index = None;
        }
        match (recall, browsing) {
            (Recall::Older, Some(i)) => {
                if row != 0 {
                    return None;
                }
                let i = i.saturating_sub(1);
                self.index = Some(i);
                Some(self.entries[i].clone())
            }
            (Recall::Older, None) => {
                if rows > 1 || self.entries.is_empty() {
                    return None;
                }
                self.draft = text.to_owned();
                let i = self.entries.len() - 1;
                self.index = Some(i);
                Some(self.entries[i].clone())
            }
            (Recall::Newer, Some(i)) => {
                if row + 1 < rows {
                    return None;
                }
                if i + 1 < self.entries.len() {
                    self.index = Some(i + 1);
                    Some(self.entries[i + 1].clone())
                } else {
                    self.index = None;
                    Some(std::mem::take(&mut self.draft))
                }
            }
            (Recall::Newer, None) => None,
        }
    }

    /// Whether the editor shows a recalled prompt unchanged (↑↓ are the history's, even while
    /// the `/` menu shows for a recalled command).
    pub fn is_browsing(&self, text: &str) -> bool {
        self.browsing(text).is_some()
    }

    /// [`PromptHistory::key`] on `editor`: puts the recalled text in it with the cursor at its
    /// end. Returns whether the key was used.
    pub fn apply(&mut self, recall: Recall, editor: &Editor) -> bool {
        let text = editor.get_text();
        let rows = text.split('\n').count();
        let (row, _) = editor.cursor_position();
        let Some(next) = self.key(recall, &text, row, rows) else {
            return false;
        };
        if next != text {
            editor.set_text(&next);
            let last = next.split('\n').count() - 1;
            let col = next.rsplit('\n').next().map_or(0, str::len);
            editor.set_cursor(last, col);
        }
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn with(entries: &[&str]) -> PromptHistory {
        let mut h = PromptHistory::new();
        for e in entries {
            h.record(e);
        }
        h
    }

    #[test]
    fn up_up_down_down_walks_back_to_the_draft() {
        let mut h = with(&["first", "second"]);
        assert_eq!(h.key(Recall::Older, "", 0, 1).as_deref(), Some("second"));
        assert_eq!(
            h.key(Recall::Older, "second", 0, 1).as_deref(),
            Some("first")
        );
        // At the oldest, ↑ stays there.
        assert_eq!(
            h.key(Recall::Older, "first", 0, 1).as_deref(),
            Some("first")
        );
        assert_eq!(
            h.key(Recall::Newer, "first", 0, 1).as_deref(),
            Some("second")
        );
        assert_eq!(h.key(Recall::Newer, "second", 0, 1).as_deref(), Some(""));
        // Not browsing any more: ↓ is the editor's.
        assert_eq!(h.key(Recall::Newer, "", 0, 1), None);
    }

    #[test]
    fn a_one_line_draft_is_kept_and_given_back() {
        let mut h = with(&["first"]);
        assert_eq!(
            h.key(Recall::Older, "half typed", 0, 1).as_deref(),
            Some("first")
        );
        assert_eq!(
            h.key(Recall::Newer, "first", 0, 1).as_deref(),
            Some("half typed")
        );
    }

    #[test]
    fn multi_line_drafts_keep_their_cursor_keys() {
        let mut h = with(&["first"]);
        assert_eq!(h.key(Recall::Older, "a\nb", 0, 2), None);
        assert_eq!(h.key(Recall::Older, "a\nb", 1, 2), None);
        assert_eq!(h.key(Recall::Newer, "a\nb", 0, 2), None);
    }

    #[test]
    fn a_multi_line_prompt_moves_its_cursor_before_going_on() {
        let mut h = with(&["old", "two\nlines"]);
        assert_eq!(
            h.key(Recall::Older, "", 0, 1).as_deref(),
            Some("two\nlines")
        );
        // The cursor is at the end (row 1): ↑ moves it up first.
        assert_eq!(h.key(Recall::Older, "two\nlines", 1, 2), None);
        assert_eq!(
            h.key(Recall::Older, "two\nlines", 0, 2).as_deref(),
            Some("old")
        );
        assert_eq!(
            h.key(Recall::Newer, "old", 0, 1).as_deref(),
            Some("two\nlines")
        );
        // On its first row, ↓ moves the cursor down; on the last it goes back to the draft.
        assert_eq!(h.key(Recall::Newer, "two\nlines", 0, 2), None);
        assert_eq!(
            h.key(Recall::Newer, "two\nlines", 1, 2).as_deref(),
            Some("")
        );
    }

    #[test]
    fn editing_a_recalled_prompt_makes_it_the_draft() {
        let mut h = with(&["first", "second"]);
        h.key(Recall::Older, "", 0, 1);
        // "second" edited to "second!": ↑ starts over from the newest, keeping the edit.
        assert_eq!(
            h.key(Recall::Older, "second!", 0, 1).as_deref(),
            Some("second")
        );
        assert_eq!(
            h.key(Recall::Newer, "second", 0, 1).as_deref(),
            Some("second!")
        );
    }

    #[test]
    fn empty_history_and_repeats() {
        let mut h = PromptHistory::new();
        assert_eq!(h.key(Recall::Older, "", 0, 1), None);
        h.record("  ");
        h.record("same");
        h.record("same");
        assert_eq!(h.entries(), ["same"]);
        for i in 0..HISTORY_LIMIT + 5 {
            h.record(&i.to_string());
        }
        assert_eq!(h.entries().len(), HISTORY_LIMIT);
        assert_eq!(h.entries()[0], "5");
    }
}
