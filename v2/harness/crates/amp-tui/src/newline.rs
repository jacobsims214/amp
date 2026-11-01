//! New lines in the editor without the kitty keyboard protocol (#2349).
//!
//! Enter sends. A new line needs a key the terminal reports differently from Enter:
//! - **Shift+Enter** with the kitty keyboard protocol (`CSI 13;2u`: kitty, Ghostty, WezTerm, foot,
//!   iTerm2 when it answers the query), with xterm's modifyOtherKeys (`CSI 27;2;13~`, decoded by
//!   the patched crossterm in `v2/harness/vendor/crossterm`), and in a local Terminal.app, where
//!   rpi-tui reads the Shift key's state from macOS ([`rpi_tui::terminal::normalize_key_event`]).
//!   Everywhere else a terminal sends Shift+Enter as a plain CR, the same as Enter, so it sends.
//! - **Ctrl+J** everywhere (a line feed, never Enter's CR).
//! - **Alt+Enter** (`ESC CR`) wherever Option/Alt sends Esc+ (iTerm2's "Esc+", Terminal.app's
//!   "Use Option as Meta key", most Linux terminals). [`normalize`] turns it into Shift+Enter.
//! - **`\` then Enter** in every terminal: Enter right after a backslash replaces it with a new
//!   line instead of sending ([`continue_line`]).
//!
//! [`NewlineMode`] says which of these the terminal in use has, for the empty editor's hint and
//! `/help` ([`NewlineMode::key`]).

use crossterm::event::{KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use rpi_tui::Editor;

/// How this terminal starts a new line, for the hints.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum NewlineMode {
    /// The kitty keyboard protocol is on: Shift+Enter is its own key.
    #[default]
    Kitty,
    /// A local Terminal.app (no kitty protocol): rpi-tui checks the Shift key's state with macOS,
    /// so Shift+Enter still starts a new line.
    AppleTerminal,
    /// Legacy keys: Shift+Enter sends like Enter; Ctrl+J, Alt+Enter and `\` Enter start a line.
    Legacy,
}

impl NewlineMode {
    /// The mode for this process's terminal, given whether the kitty protocol was negotiated.
    pub fn detect(kitty: bool) -> Self {
        let ssh = ["SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"]
            .iter()
            .any(|name| std::env::var_os(name).is_some());
        Self::from_parts(
            kitty,
            cfg!(target_os = "macos"),
            std::env::var("TERM_PROGRAM").ok().as_deref(),
            ssh,
        )
    }

    /// [`Self::detect`] from its inputs. Over SSH the Mac's own Shift key isn't the remote
    /// person's, so rpi-tui doesn't ask macOS (its `local_apple_terminal`).
    pub fn from_parts(kitty: bool, macos: bool, term_program: Option<&str>, ssh: bool) -> Self {
        if kitty {
            Self::Kitty
        } else if macos && term_program == Some("Apple_Terminal") && !ssh {
            Self::AppleTerminal
        } else {
            Self::Legacy
        }
    }

    /// Whether Shift+Enter starts a new line here.
    pub fn shift_enter(self) -> bool {
        !matches!(self, Self::Legacy)
    }

    /// The key to name in hints for a new line here.
    pub fn key(self) -> &'static str {
        if self.shift_enter() {
            "Shift+Enter"
        } else {
            "Ctrl+J"
        }
    }
}

/// Alt+Enter (`ESC CR`, or kitty's `CSI 13;3u`) as Shift+Enter, which rpi-tui's editor and the
/// shell's keys treat as a new line. Every other key is unchanged.
pub fn normalize(key: KeyEvent) -> KeyEvent {
    let enter = matches!(key.code, KeyCode::Enter | KeyCode::Char('\r'));
    if enter && key.modifiers == KeyModifiers::ALT {
        let mut key = key;
        key.code = KeyCode::Enter;
        key.modifiers = KeyModifiers::SHIFT;
        return key;
    }
    key
}

/// Whether `key` starts a new line in the editor (after [`normalize`]): Shift+Enter, Ctrl+J.
pub fn is_new_line(key: &KeyEvent) -> bool {
    matches!(
        (key.modifiers, key.code),
        (
            KeyModifiers::SHIFT,
            KeyCode::Enter | KeyCode::Char('\r') | KeyCode::Char('\n')
        ) | (KeyModifiers::CONTROL, KeyCode::Char('j'))
    )
}

/// `\` then Enter: when `key` is a plain Enter press and the character before the cursor is a
/// backslash, the backslash becomes a new line and this returns true (the editor doesn't get
/// the Enter, so nothing is sent). Otherwise it does nothing and returns false.
pub fn continue_line(editor: &Editor, key: &KeyEvent) -> bool {
    if key.code != KeyCode::Enter || !key.modifiers.is_empty() || key.kind == KeyEventKind::Release
    {
        return false;
    }
    let (row, col) = editor.cursor_position();
    let text = editor.get_text();
    let Some(line) = text.split('\n').nth(row) else {
        return false;
    };
    // The cursor column is a byte offset into its line.
    if !line.get(..col).is_some_and(|before| before.ends_with('\\')) {
        return false;
    }
    editor.handle_key(KeyEvent::new(KeyCode::Backspace, KeyModifiers::NONE));
    editor.handle_key(KeyEvent::new(KeyCode::Enter, KeyModifiers::SHIFT));
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(code: KeyCode, modifiers: KeyModifiers) -> KeyEvent {
        KeyEvent::new(code, modifiers)
    }

    #[test]
    fn alt_enter_becomes_shift_enter_and_nothing_else_changes() {
        let cases = [
            (
                key(KeyCode::Enter, KeyModifiers::ALT),
                key(KeyCode::Enter, KeyModifiers::SHIFT),
            ),
            (
                key(KeyCode::Char('\r'), KeyModifiers::ALT),
                key(KeyCode::Enter, KeyModifiers::SHIFT),
            ),
            (
                key(KeyCode::Enter, KeyModifiers::NONE),
                key(KeyCode::Enter, KeyModifiers::NONE),
            ),
            (
                key(KeyCode::Enter, KeyModifiers::SHIFT),
                key(KeyCode::Enter, KeyModifiers::SHIFT),
            ),
            // Ctrl+Alt+Enter is someone else's key.
            (
                key(KeyCode::Enter, KeyModifiers::ALT | KeyModifiers::CONTROL),
                key(KeyCode::Enter, KeyModifiers::ALT | KeyModifiers::CONTROL),
            ),
            (
                key(KeyCode::Char('b'), KeyModifiers::ALT),
                key(KeyCode::Char('b'), KeyModifiers::ALT),
            ),
        ];
        for (input, want) in cases {
            assert_eq!(normalize(input), want, "{input:?}");
        }
    }

    #[test]
    fn new_line_keys() {
        assert!(is_new_line(&key(KeyCode::Enter, KeyModifiers::SHIFT)));
        assert!(is_new_line(&key(KeyCode::Char('j'), KeyModifiers::CONTROL)));
        assert!(is_new_line(&normalize(key(
            KeyCode::Enter,
            KeyModifiers::ALT
        ))));
        assert!(!is_new_line(&key(KeyCode::Enter, KeyModifiers::NONE)));
        assert!(!is_new_line(&key(KeyCode::Char('j'), KeyModifiers::NONE)));
    }

    #[test]
    fn mode_from_the_terminal() {
        use NewlineMode::*;
        let cases = [
            ((true, true, Some("Apple_Terminal"), false), Kitty),
            ((true, false, Some("xterm"), true), Kitty),
            ((false, true, Some("Apple_Terminal"), false), AppleTerminal),
            // Over SSH rpi-tui doesn't ask macOS about Shift.
            ((false, true, Some("Apple_Terminal"), true), Legacy),
            ((false, false, Some("Apple_Terminal"), false), Legacy),
            ((false, true, Some("iTerm.app"), false), Legacy),
            ((false, true, Some("tmux"), false), Legacy),
            ((false, false, None, false), Legacy),
        ];
        for ((kitty, macos, program, ssh), want) in cases {
            assert_eq!(
                NewlineMode::from_parts(kitty, macos, program, ssh),
                want,
                "{kitty} {macos} {program:?} {ssh}"
            );
        }
        assert_eq!(Kitty.key(), "Shift+Enter");
        assert_eq!(AppleTerminal.key(), "Shift+Enter");
        assert_eq!(Legacy.key(), "Ctrl+J");
    }

    fn editor(text: &str) -> Editor {
        let editor = Editor::simple();
        editor.set_text(text);
        let lines: Vec<&str> = text.split('\n').collect();
        editor.set_cursor(lines.len() - 1, lines.last().map_or(0, |l| l.len()));
        editor
    }

    #[test]
    fn backslash_enter_continues_the_line() {
        let enter = key(KeyCode::Enter, KeyModifiers::NONE);
        let e = editor("first line\\");
        assert!(continue_line(&e, &enter));
        assert_eq!(e.get_text(), "first line\n");
        assert_eq!(e.cursor_position(), (1, 0));

        // Typing on and doing it again on the second line.
        e.insert("second\\");
        assert!(continue_line(&e, &enter));
        assert_eq!(e.get_text(), "first line\nsecond\n");
    }

    #[test]
    fn enter_without_a_backslash_before_the_cursor_is_left_alone() {
        let enter = key(KeyCode::Enter, KeyModifiers::NONE);
        for text in ["", "plain", "a \\ b", "ends with\\ "] {
            let e = editor(text);
            assert!(!continue_line(&e, &enter), "{text:?}");
            assert_eq!(e.get_text(), text);
        }
        // The backslash is not right before the cursor.
        let e = editor("x\\y");
        e.set_cursor(0, 3);
        assert!(!continue_line(&e, &enter));
        // Shift+Enter and Ctrl+J are the editor's own; only a plain Enter is looked at.
        let e = editor("x\\");
        assert!(!continue_line(
            &e,
            &key(KeyCode::Enter, KeyModifiers::SHIFT)
        ));
        assert_eq!(e.get_text(), "x\\");
    }

    #[test]
    fn backslash_in_the_middle_of_a_line() {
        let e = editor("ab\\cd");
        e.set_cursor(0, 3);
        assert!(continue_line(&e, &key(KeyCode::Enter, KeyModifiers::NONE)));
        assert_eq!(e.get_text(), "ab\ncd");
    }
}
