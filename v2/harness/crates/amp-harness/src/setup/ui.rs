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

fn cancelled() -> CliError {
    CliError {
        message: "setup cancelled".to_owned(),
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

/// The person at a terminal.
pub struct TerminalUi;

impl Ui for TerminalUi {
    fn interactive(&self) -> bool {
        true
    }
    fn say(&mut self, text: &str) {
        eprintln!("{text}");
    }
    fn ask(&mut self, prompt: &str, default: Option<&str>) -> Result<String, CliError> {
        let mut err = io::stderr();
        let _ = write!(err, "{}", prompt_text(prompt, default));
        let _ = err.flush();
        let line = read_line()?.ok_or_else(cancelled)?;
        let line = line.trim();
        Ok(match (line.is_empty(), default) {
            (true, Some(d)) => d.to_owned(),
            _ => line.to_owned(),
        })
    }
    fn secret(&mut self, prompt: &str) -> Result<String, CliError> {
        let mut err = io::stderr();
        let _ = write!(err, "{prompt} (hidden): ");
        let _ = err.flush();
        let result = read_hidden();
        let _ = writeln!(err);
        result
    }
}

/// Restores cooked mode when dropped, even on an error or a panic.
struct RawMode;

impl Drop for RawMode {
    fn drop(&mut self) {
        let _ = crossterm::terminal::disable_raw_mode();
    }
}

/// Reads one line in raw mode without echoing anything. Ctrl-C and Esc cancel; Backspace and
/// Ctrl-U edit; pasted text arrives as key presses and is accepted.
fn read_hidden() -> Result<String, CliError> {
    use crossterm::event::{self, Event, KeyCode, KeyEventKind, KeyModifiers};

    crossterm::terminal::enable_raw_mode()
        .map_err(|e| CliError::new(format!("terminal raw mode: {e}")))?;
    let _raw = RawMode;
    let mut out = String::new();
    loop {
        let ev = event::read().map_err(|e| CliError::new(format!("read key: {e}")))?;
        let Event::Key(key) = ev else {
            if let Event::Paste(text) = ev {
                out.push_str(text.trim_end_matches(['\r', '\n']));
            }
            continue;
        };
        if key.kind == KeyEventKind::Release {
            continue;
        }
        let ctrl = key.modifiers.contains(KeyModifiers::CONTROL);
        match key.code {
            KeyCode::Enter => return Ok(out.trim().to_owned()),
            KeyCode::Esc => return Err(cancelled()),
            KeyCode::Char('c') | KeyCode::Char('d') if ctrl => return Err(cancelled()),
            KeyCode::Char('u') if ctrl => out.clear(),
            KeyCode::Backspace => {
                out.pop();
            }
            KeyCode::Char(c) if !ctrl => out.push(c),
            _ => {}
        }
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
    fn prompt_shapes() {
        assert_eq!(prompt_text("AMP server", Some("x")), "AMP server [x]: ");
        assert_eq!(prompt_text("Log in? [Y/n]", Some("")), "Log in? [Y/n] ");
        assert_eq!(prompt_text("Name", None), "Name: ");
    }
}
