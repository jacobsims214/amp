//! How setup talks to the person: questions on stderr, answers from the terminal.
//!
//! - [`TerminalUi`]: stdin and stderr are a terminal. Secrets are read in raw mode and never
//!   echoed (not even as `*`).
//! - [`ScriptedUi`]: `AMP_HARNESS_PROMPTS=stdin` (tests and scripted installs). Every answer,
//!   secrets included, is one line of stdin; nothing read is ever printed.
//! - [`NoUi`]: not interactive. Any question is an error that says which flag answers it.

use std::io::{self, BufRead, IsTerminal, Write};

use crate::conn::CliError;

/// Makes setup read its answers line by line from stdin, even without a terminal.
pub const ENV_PROMPTS: &str = "AMP_HARNESS_PROMPTS";

pub trait Ui {
    /// Whether questions can be asked at all.
    fn interactive(&self) -> bool;
    /// One line of information for the person (stderr).
    fn say(&mut self, text: &str);
    /// A question with an optional default (taken on an empty answer). Answers are trimmed.
    fn ask(&mut self, prompt: &str, default: Option<&str>) -> Result<String, CliError>;
    /// A secret: never echoed, never printed back.
    fn secret(&mut self, prompt: &str) -> Result<String, CliError>;

    /// A yes/no question; Enter takes `default`.
    fn confirm(&mut self, prompt: &str, default: bool) -> Result<bool, CliError> {
        let hint = if default { "Y/n" } else { "y/N" };
        loop {
            let answer = self.ask(&format!("{prompt} [{hint}]"), Some(""))?;
            match answer.to_ascii_lowercase().as_str() {
                "" => return Ok(default),
                "y" | "yes" => return Ok(true),
                "n" | "no" => return Ok(false),
                _ => self.say("  Please answer y or n."),
            }
        }
    }
}

/// The command a cancelled prompt names ("setup cancelled", "connect cancelled"): set once per
/// process by [`detect_for`]; setup's when nothing set it.
static CANCELLED_COMMAND: std::sync::OnceLock<&'static str> = std::sync::OnceLock::new();

fn cancelled() -> CliError {
    CliError {
        message: format!(
            "{} cancelled",
            CANCELLED_COMMAND.get().copied().unwrap_or("setup")
        ),
        code: 130,
        offline: false,
    }
}

fn prompt_text(prompt: &str, default: Option<&str>) -> String {
    match default {
        Some(d) if !d.is_empty() => format!("{prompt} [{d}]: "),
        _ if prompt.ends_with(']') => format!("{prompt} "),
        _ => format!("{prompt}: "),
    }
}

/// The UI for this process: scripted when `AMP_HARNESS_PROMPTS=stdin`, the terminal when stdin
/// and stderr are one, else none. `allow` = false forces [`NoUi`] (`init --yes`).
pub fn detect(allow: bool) -> Box<dyn Ui> {
    detect_for(allow, "setup")
}

/// [`detect`] for `command`: a cancelled prompt says "`command` cancelled" (#2340: Ctrl+C in
/// `connect` said "setup cancelled").
pub fn detect_for(allow: bool, command: &'static str) -> Box<dyn Ui> {
    let _ = CANCELLED_COMMAND.set(command);
    if !allow {
        return Box::new(NoUi);
    }
    if std::env::var(ENV_PROMPTS).is_ok_and(|v| v.trim() == "stdin") {
        return Box::new(ScriptedUi);
    }
    if io::stdin().is_terminal() && io::stderr().is_terminal() {
        return Box::new(TerminalUi);
    }
    Box::new(NoUi)
}

/// No questions: setup runs on flags and what it finds.
pub struct NoUi;

impl Ui for NoUi {
    fn interactive(&self) -> bool {
        false
    }
    fn say(&mut self, text: &str) {
        eprintln!("{text}");
    }
    fn ask(&mut self, prompt: &str, _default: Option<&str>) -> Result<String, CliError> {
        Err(CliError::usage(format!(
            "setup needs an answer (\"{prompt}\") but there is no terminal: pass it as a flag \
             (see `amp-harness init --help`)"
        )))
    }
    fn secret(&mut self, prompt: &str) -> Result<String, CliError> {
        self.ask(prompt, None)
    }
}

fn read_line() -> Result<Option<String>, CliError> {
    let mut line = String::new();
    match io::stdin().lock().read_line(&mut line) {
        Ok(0) => Ok(None),
        Ok(_) => Ok(Some(line.trim_end_matches(['\r', '\n']).to_owned())),
        Err(e) => Err(CliError::new(format!("read answer: {e}"))),
    }
}

/// Answers are lines of stdin (`AMP_HARNESS_PROMPTS=stdin`). End of input cancels.
pub struct ScriptedUi;

impl Ui for ScriptedUi {
    fn interactive(&self) -> bool {
        true
    }
    fn say(&mut self, text: &str) {
        eprintln!("{text}");
    }
    fn ask(&mut self, prompt: &str, default: Option<&str>) -> Result<String, CliError> {
        eprintln!("{}", prompt_text(prompt, default));
        let line = read_line()?.ok_or_else(cancelled)?;
        let line = line.trim();
        Ok(match (line.is_empty(), default) {
            (true, Some(d)) => d.to_owned(),
            _ => line.to_owned(),
        })
    }
    fn secret(&mut self, prompt: &str) -> Result<String, CliError> {
        eprintln!("{prompt} (hidden): ");
        Ok(read_line()?.ok_or_else(cancelled)?.trim().to_owned())
    }
}

/// The person at a terminal. Every answer is read in raw mode (#2227), so Ctrl+C, Esc and Ctrl+D
/// on an empty line are keys that cancel setup ("setup cancelled", exit 130) whatever the process's
/// SIGINT disposition is: a parent that ignores SIGINT (a pexpect driver, `nohup`, a background job
/// of a script) used to leave a cooked-mode prompt echoing ^C forever. The terminal is restored on
/// every way out.
pub struct TerminalUi;

impl Ui for TerminalUi {
    fn interactive(&self) -> bool {
        true
    }
    fn say(&mut self, text: &str) {
        eprintln!("{text}");
    }
    fn ask(&mut self, prompt: &str, default: Option<&str>) -> Result<String, CliError> {
        let line = read_terminal(&prompt_text(prompt, default), Echo::Visible)?;
        let line = line.trim();
        Ok(match (line.is_empty(), default) {
            (true, Some(d)) => d.to_owned(),
            _ => line.to_owned(),
        })
    }
    fn secret(&mut self, prompt: &str) -> Result<String, CliError> {
        Ok(
            read_terminal(&format!("{prompt} (hidden): "), Echo::Hidden)?
                .trim()
                .to_owned(),
        )
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Echo {
    Visible,
    /// Nothing is echoed, not even `*`.
    Hidden,
}

/// Writes the prompt and reads one answer at the terminal. Raw mode when it can be had, else a
/// cooked line (where only Ctrl+D cancels).
fn read_terminal(prompt: &str, echo: Echo) -> Result<String, CliError> {
    let mut err = io::stderr();
    let _ = write!(err, "{prompt}");
    let _ = err.flush();
    let result = match RawMode::enable() {
        Ok(raw) => {
            let result = read_raw(echo);
            drop(raw);
            result
        }
        Err(_) if echo == Echo::Visible => read_line().and_then(|l| l.ok_or_else(cancelled)),
        Err(e) => Err(e),
    };
    // Cooked again: end the prompt's line (the Enter or ^C left the cursor on it).
    let _ = writeln!(err);
    result
}

/// Restores cooked mode when dropped, even on an error or a panic.
struct RawMode;

impl RawMode {
    fn enable() -> Result<Self, CliError> {
        crossterm::terminal::enable_raw_mode()
            .map_err(|e| CliError::new(format!("terminal raw mode: {e}")))?;
        Ok(Self)
    }
}

impl Drop for RawMode {
    fn drop(&mut self) {
        let _ = crossterm::terminal::disable_raw_mode();
    }
}

/// What a key does to the answer being typed.
#[derive(Debug, PartialEq, Eq)]
enum Edit {
    Done,
    Cancel,
    /// Characters appended (to echo).
    Typed(String),
    /// Characters removed from the end (to rub out).
    Erased(usize),
    Nothing,
}

/// Applies one key to `out`. Ctrl-C, Esc and Ctrl-D on an empty answer cancel; Backspace, Ctrl-U
/// and Ctrl-W edit; pasted text arrives as key presses and is accepted.
fn apply_key(out: &mut String, key: crossterm::event::KeyEvent) -> Edit {
    use crossterm::event::{KeyCode, KeyEventKind, KeyModifiers};

    if key.kind == KeyEventKind::Release {
        return Edit::Nothing;
    }
    let ctrl = key.modifiers.contains(KeyModifiers::CONTROL);
    let erase = |out: &mut String, keep: usize| {
        let n = out[keep..].chars().count();
        out.truncate(keep);
        if n == 0 {
            Edit::Nothing
        } else {
            Edit::Erased(n)
        }
    };
    match key.code {
        KeyCode::Enter => Edit::Done,
        KeyCode::Char('j' | 'm') if ctrl => Edit::Done,
        KeyCode::Esc => Edit::Cancel,
        KeyCode::Char('c') if ctrl => Edit::Cancel,
        KeyCode::Char('d') if ctrl && out.is_empty() => Edit::Cancel,
        KeyCode::Char('u') if ctrl => erase(out, 0),
        KeyCode::Char('w') if ctrl => {
            let trimmed = out.trim_end_matches(char::is_whitespace);
            let keep = trimmed.rfind(char::is_whitespace).map_or(0, |i| {
                i + trimmed[i..].chars().next().map_or(1, char::len_utf8)
            });
            erase(out, keep)
        }
        KeyCode::Backspace => match out.char_indices().next_back() {
            Some((i, _)) => erase(out, i),
            None => Edit::Nothing,
        },
        KeyCode::Char(c) if !ctrl => {
            out.push(c);
            Edit::Typed(c.to_string())
        }
        _ => Edit::Nothing,
    }
}

/// Reads one line in raw mode, echoing it unless hidden (#2113: a secret shows nothing).
fn read_raw(echo: Echo) -> Result<String, CliError> {
    use crossterm::event::{self, Event};

    let mut err = io::stderr();
    let mut out = String::new();
    loop {
        let ev = event::read().map_err(|e| CliError::new(format!("read key: {e}")))?;
        let edit = match ev {
            Event::Key(key) => apply_key(&mut out, key),
            Event::Paste(text) => {
                let text = text.trim_end_matches(['\r', '\n']).to_owned();
                out.push_str(&text);
                Edit::Typed(text)
            }
            _ => Edit::Nothing,
        };
        match edit {
            Edit::Done => return Ok(out),
            Edit::Cancel => {
                if echo == Echo::Visible {
                    let _ = write!(err, "^C");
                }
                return Err(cancelled());
            }
            Edit::Typed(text) if echo == Echo::Visible => {
                let _ = write!(err, "{text}");
            }
            Edit::Erased(n) if echo == Echo::Visible => {
                let _ = write!(err, "{}", "\x08 \x08".repeat(n));
            }
            _ => continue,
        }
        let _ = err.flush();
    }
}

#[cfg(test)]
pub mod testing {
    //! A UI with canned answers for unit tests.
    use super::*;

    pub struct Canned {
        pub answers: std::collections::VecDeque<String>,
        pub said: Vec<String>,
        pub asked: Vec<String>,
    }

    impl Canned {
        pub fn new(answers: &[&str]) -> Self {
            Self {
                answers: answers.iter().map(|s| (*s).to_owned()).collect(),
                said: Vec::new(),
                asked: Vec::new(),
            }
        }
    }

    impl Ui for Canned {
        fn interactive(&self) -> bool {
            true
        }
        fn say(&mut self, text: &str) {
            self.said.push(text.to_owned());
        }
        fn ask(&mut self, prompt: &str, default: Option<&str>) -> Result<String, CliError> {
            self.asked.push(prompt.to_owned());
            let a = self.answers.pop_front().ok_or_else(cancelled)?;
            Ok(match (a.is_empty(), default) {
                (true, Some(d)) => d.to_owned(),
                _ => a,
            })
        }
        fn secret(&mut self, prompt: &str) -> Result<String, CliError> {
            self.asked.push(prompt.to_owned());
            self.answers.pop_front().ok_or_else(cancelled)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{testing::Canned, *};

    #[test]
    fn confirm_defaults_and_retries() {
        let mut ui = Canned::new(&["", "maybe", "n"]);
        assert!(ui.confirm("go?", true).unwrap());
        assert!(!ui.confirm("go?", true).unwrap());
        assert_eq!(ui.said, ["  Please answer y or n."]);
    }

    #[test]
    fn no_ui_names_the_question() {
        let e = NoUi.ask("Project", None).unwrap_err();
        assert!(e.message.contains("Project") && e.message.contains("init --help"));
    }

    #[test]
    fn keys_edit_and_cancel() {
        use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
        let key = |c| KeyEvent::new(KeyCode::Char(c), KeyModifiers::NONE);
        let ctrl = |c| KeyEvent::new(KeyCode::Char(c), KeyModifiers::CONTROL);
        let mut out = String::new();
        for c in "two wörds".chars() {
            apply_key(&mut out, key(c));
        }
        assert_eq!(
            apply_key(&mut out, KeyEvent::from(KeyCode::Backspace)),
            Edit::Erased(1)
        );
        assert_eq!(out, "two wörd");
        assert_eq!(apply_key(&mut out, ctrl('w')), Edit::Erased(4));
        assert_eq!(out, "two ");
        // Ctrl-D only cancels an empty answer, like end of input in cooked mode.
        assert_eq!(apply_key(&mut out, ctrl('d')), Edit::Nothing);
        assert_eq!(apply_key(&mut out, ctrl('u')), Edit::Erased(4));
        assert_eq!(apply_key(&mut out, ctrl('d')), Edit::Cancel);
        assert_eq!(apply_key(&mut out, ctrl('c')), Edit::Cancel);
        assert_eq!(
            apply_key(&mut out, KeyEvent::from(KeyCode::Esc)),
            Edit::Cancel
        );
        assert_eq!(
            apply_key(&mut out, KeyEvent::from(KeyCode::Enter)),
            Edit::Done
        );
    }

    #[test]
    fn prompt_shapes() {
        assert_eq!(prompt_text("AMP server", Some("x")), "AMP server [x]: ");
        assert_eq!(prompt_text("Log in? [Y/n]", Some("")), "Log in? [Y/n] ");
        assert_eq!(prompt_text("Name", None), "Name: ");
    }
}
