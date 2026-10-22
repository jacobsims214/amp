//! Which AMP theme to use: dark or light, truecolor or 256 colours, or a theme file.
//!
//! - **Theme:** `--theme`, then `AMP_HARNESS_THEME`, then config.toml `[tui] theme`, then `auto`.
//!   Values: `auto`, `dark` / `amp-dark`, `light` / `amp-light`, or a path to a theme file in our
//!   format. `auto` asks the terminal for its background colour (OSC 11) and picks light on a light
//!   background, else dark (also when the terminal does not answer).
//! - **Colour depth:** `AMP_HARNESS_COLOR`, then `[tui] color`, then `auto`. Values: `auto`,
//!   `truecolor` (or `24bit`), `256`. `auto` is truecolor when `COLORTERM` is `truecolor`/`24bit`
//!   (or `TERM` ends in `-direct`), else 256.

use std::{
    path::{Path, PathBuf},
    time::Duration,
};

use serde::Deserialize;

use super::{AmpTheme, ThemeError, relative_luminance};

pub const ENV_THEME: &str = "AMP_HARNESS_THEME";
pub const ENV_COLOR: &str = "AMP_HARNESS_COLOR";
pub const ENV_SHOW_THINKING: &str = "AMP_HARNESS_SHOW_THINKING";

/// Dark or light.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Variant {
    Dark,
    Light,
}

/// How many colours the terminal shows.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ColorDepth {
    /// 24-bit `38;2;r;g;b` colours: the theme's hex values.
    TrueColor,
    /// xterm-256 indexes: the theme's `*256` values.
    Ansi256,
}

impl ColorDepth {
    pub fn word(self) -> &'static str {
        match self {
            Self::TrueColor => "truecolor",
            Self::Ansi256 => "256",
        }
    }
}

/// The theme the user asked for.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ThemeChoice {
    /// Dark or light by the terminal's background.
    Auto,
    Builtin(Variant),
    /// A theme file in our format.
    File(PathBuf),
}

impl ThemeChoice {
    /// Parses a `--theme` / `AMP_HARNESS_THEME` / `[tui] theme` value. `home` expands a leading
    /// `~/` in a path.
    pub fn parse(raw: &str, home: Option<&str>) -> Result<Self, ThemeError> {
        let v = raw.trim();
        Ok(match v.to_ascii_lowercase().as_str() {
            "" | "auto" => Self::Auto,
            "dark" | "amp-dark" => Self::Builtin(Variant::Dark),
            "light" | "amp-light" => Self::Builtin(Variant::Light),
            _ if v.ends_with(".json") || v.contains('/') => {
                Self::File(match (v.strip_prefix("~/"), home) {
                    (Some(rest), Some(home)) => Path::new(home).join(rest),
                    _ => PathBuf::from(v),
                })
            }
            _ => {
                return Err(ThemeError(format!(
                    "unknown theme `{v}`: use auto, dark, light, or a path to a .json theme file"
                )));
            }
        })
    }
}

fn parse_depth(raw: &str) -> Result<Option<ColorDepth>, ThemeError> {
    Ok(match raw.trim().to_ascii_lowercase().as_str() {
        "" | "auto" => None,
        "truecolor" | "24bit" | "24-bit" => Some(ColorDepth::TrueColor),
        "256" | "ansi256" => Some(ColorDepth::Ansi256),
        other => {
            return Err(ThemeError(format!(
                "unknown colour depth `{other}`: use auto, truecolor or 256"
            )));
        }
    })
}

/// `[tui]` in config.toml. Other tables are ignored; unknown keys here are errors.
#[derive(Debug, Default, Deserialize)]
struct ConfigFile {
    #[serde(default)]
    tui: Option<TuiTable>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct TuiTable {
    theme: Option<String>,
    color: Option<String>,
    show_thinking: Option<String>,
}

/// How the chat shows thinking (#2177): `AMP_HARNESS_SHOW_THINKING`, then config.toml `[tui]
/// show_thinking`, then `collapsed`. Values: `collapsed`, `full`, `hidden`.
pub fn resolve_show_thinking(
    env: &dyn Fn(&str) -> Option<String>,
    config: Option<&str>,
) -> Result<crate::ThinkingDisplay, ThemeError> {
    let table = match config {
        Some(text) => toml::from_str::<ConfigFile>(text)
            .map_err(|e| ThemeError(format!("config.toml [tui]: {e}")))?
            .tui
            .unwrap_or_default(),
        None => TuiTable::default(),
    };
    match env(ENV_SHOW_THINKING)
        .filter(|v| !v.trim().is_empty())
        .map(|v| (v, ENV_SHOW_THINKING))
        .or_else(|| {
            table
                .show_thinking
                .filter(|v| !v.trim().is_empty())
                .map(|v| (v, "[tui] show_thinking"))
        }) {
        Some((v, from)) => {
            crate::ThinkingDisplay::parse(&v).map_err(|e| ThemeError(format!("{from}: {e}")))
        }
        None => Ok(crate::ThinkingDisplay::default()),
    }
}

/// The resolved theme settings, before the terminal is asked anything.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ThemeSettings {
    pub theme: ThemeChoice,
    /// `None` = detect from the environment.
    pub depth: Option<ColorDepth>,
}

impl ThemeSettings {
    /// `flag` (`--theme`), then the environment, then config.toml's text (`config`), then auto.
    pub fn resolve(
        flag: Option<&str>,
        env: &dyn Fn(&str) -> Option<String>,
        config: Option<&str>,
    ) -> Result<Self, ThemeError> {
        let table = match config {
            Some(text) => toml::from_str::<ConfigFile>(text)
                .map_err(|e| ThemeError(format!("config.toml [tui]: {e}")))?
                .tui
                .unwrap_or_default(),
            None => TuiTable::default(),
        };
        let set = |v: Option<String>| v.filter(|v| !v.trim().is_empty());
        let home = env("HOME");
        let theme = match set(flag.map(str::to_owned))
            .map(|v| (v, "--theme"))
            .or_else(|| set(env(ENV_THEME)).map(|v| (v, ENV_THEME)))
            .or_else(|| set(table.theme).map(|v| (v, "[tui] theme")))
        {
            Some((v, from)) => ThemeChoice::parse(&v, home.as_deref())
                .map_err(|e| ThemeError(format!("{from}: {}", e.0)))?,
            None => ThemeChoice::Auto,
        };
        let depth = match set(env(ENV_COLOR))
            .map(|v| (v, ENV_COLOR))
            .or_else(|| set(table.color).map(|v| (v, "[tui] color")))
        {
            Some((v, from)) => {
                parse_depth(&v).map_err(|e| ThemeError(format!("{from}: {}", e.0)))?
            }
            None => None,
        };
        Ok(Self { theme, depth })
    }

    /// Loads the theme. `background` is only called for [`ThemeChoice::Auto`] (it is
    /// [`query_background`] in a terminal).
    pub fn load(
        &self,
        env: &dyn Fn(&str) -> Option<String>,
        background: impl FnOnce() -> Option<(u8, u8, u8)>,
    ) -> Result<AmpTheme, ThemeError> {
        let depth = self.depth.unwrap_or_else(|| detect_depth(env));
        match &self.theme {
            ThemeChoice::Builtin(v) => Ok(AmpTheme::builtin(*v, depth)),
            ThemeChoice::File(path) => AmpTheme::from_file(path, depth),
            ThemeChoice::Auto => {
                let variant = background().map_or(Variant::Dark, variant_for_background);
                Ok(AmpTheme::builtin(variant, depth))
            }
        }
    }
}

/// Truecolor when the environment says so (`COLORTERM=truecolor|24bit`, or a `*-direct` TERM),
/// else 256 colours.
pub fn detect_depth(env: &dyn Fn(&str) -> Option<String>) -> ColorDepth {
    let colorterm = env("COLORTERM").unwrap_or_default().to_ascii_lowercase();
    let term = env("TERM").unwrap_or_default().to_ascii_lowercase();
    if matches!(colorterm.trim(), "truecolor" | "24bit") || term.ends_with("-direct") {
        ColorDepth::TrueColor
    } else {
        ColorDepth::Ansi256
    }
}

/// Light text needs a dark ground: light when black text would contrast more than white text
/// (WCAG luminance above ~0.18).
pub fn variant_for_background(rgb: (u8, u8, u8)) -> Variant {
    // (L + 0.05) / 0.05 > 1.05 / (L + 0.05)  <=>  L > sqrt(1.05 * 0.05) - 0.05.
    if relative_luminance(rgb) > (1.05f64 * 0.05).sqrt() - 0.05 {
        Variant::Light
    } else {
        Variant::Dark
    }
}

/// The OSC 11 query (with ST) followed by a primary device attributes query. Every terminal
/// answers DA1, so its reply marks the end of the answers even when OSC 11 is not supported.
pub const BACKGROUND_QUERY: &str = "\x1b]11;?\x1b\\\x1b[c";

/// Whether `reply` holds the DA1 answer (`ESC [ ? ... c`), i.e. the terminal has answered
/// everything [`BACKGROUND_QUERY`] asked.
pub fn reply_complete(reply: &[u8]) -> bool {
    let Some(start) = find(reply, b"\x1b[?") else {
        return false;
    };
    reply[start + 3..]
        .iter()
        .find(|b| !(b.is_ascii_digit() || **b == b';'))
        == Some(&b'c')
}

/// The background colour in a terminal's answer to [`BACKGROUND_QUERY`], if it sent one.
pub fn parse_background_reply(reply: &[u8]) -> Option<(u8, u8, u8)> {
    let start = find(reply, b"\x1b]11;")?;
    let rest = &reply[start..];
    let end = rest
        .iter()
        .position(|b| *b == 0x07)
        .map(|i| i + 1)
        .or_else(|| find(rest, b"\x1b\\").map(|i| i + 2))?;
    let text = std::str::from_utf8(&rest[..end]).ok()?;
    let c = rpi_tui::terminal_colors::parse_osc11_background_color(text)?;
    Some((c.r, c.g, c.b))
}

fn find(hay: &[u8], needle: &[u8]) -> Option<usize> {
    hay.windows(needle.len()).position(|w| w == needle)
}

/// Asks the terminal (`/dev/tty`) for its background colour. Call it before the TUI takes the
/// terminal. `None` when there is no terminal, it doesn't answer, or the answer has no OSC 11
/// colour.
///
/// `timeout` bounds the wait for the answer to start; once part of it has arrived, the rest
/// (up to the DA1 answer, which every terminal sends) gets [`REPLY_DRAIN`] more. The answer is
/// read on the calling thread with `select(2)`, one byte at a time, so when this returns nothing
/// is left reading the terminal and no byte after the DA1 answer (type-ahead) has been taken.
#[cfg(unix)]
pub fn query_background(timeout: Duration) -> Option<(u8, u8, u8)> {
    let tty = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open("/dev/tty")
        .ok()?;

    struct Raw(bool);
    impl Drop for Raw {
        fn drop(&mut self) {
            if self.0 {
                let _ = crossterm::terminal::disable_raw_mode();
            }
        }
    }
    let was_raw = crossterm::terminal::is_raw_mode_enabled().unwrap_or(false);
    if !was_raw {
        crossterm::terminal::enable_raw_mode().ok()?;
    }
    let _raw = Raw(!was_raw);

    let reply = exchange(&tty, timeout, REPLY_DRAIN)?;
    parse_background_reply(&reply)
}

/// How long [`query_background`] keeps reading once part of the answer has arrived, so a slow
/// link's late bytes are taken here rather than reaching the TUI as key presses.
pub const REPLY_DRAIN: Duration = Duration::from_millis(500);

/// Writes [`BACKGROUND_QUERY`] to `tty` (already in raw mode) and reads the answer up to the end
/// of the DA1 reply. Gives up after `first` if nothing arrives, or after `first + drain` if the
/// answer is incomplete. Reads byte by byte so nothing past the DA1 reply is consumed.
#[cfg(unix)]
pub(crate) fn exchange(tty: &std::fs::File, first: Duration, drain: Duration) -> Option<Vec<u8>> {
    use std::{
        io::{ErrorKind, Read, Write},
        os::fd::AsFd,
        time::Instant,
    };

    use nix::{
        errno::Errno,
        sys::{
            select::{FdSet, select},
            time::{TimeVal, TimeValLike},
        },
    };

    let mut w = tty;
    w.write_all(BACKGROUND_QUERY.as_bytes()).ok()?;
    w.flush().ok()?;

    let start = Instant::now();
    let mut reply = Vec::new();
    let mut r = tty;
    while !reply_complete(&reply) {
        let bound = if reply.is_empty() {
            first
        } else {
            first + drain
        };
        let left = bound.saturating_sub(start.elapsed());
        if left.is_zero() {
            break;
        }
        // select(2), not poll(2): macOS poll reports POLLNVAL for /dev/tty.
        let mut fds = FdSet::new();
        fds.insert(tty.as_fd());
        let mut tv = TimeVal::microseconds(i64::try_from(left.as_micros()).unwrap_or(i64::MAX));
        match select(None, &mut fds, None, None, &mut tv) {
            Ok(0) => continue, // the loop re-checks the bound
            Ok(_) => {}
            Err(Errno::EINTR) => continue,
            Err(_) => break,
        }
        let mut byte = [0u8; 1];
        match r.read(&mut byte) {
            Ok(1) => reply.push(byte[0]),
            Err(e) if e.kind() == ErrorKind::Interrupted => {}
            _ => break, // end of file or an error
        }
    }
    Some(reply)
}

/// No terminal query off unix.
#[cfg(not(unix))]
pub fn query_background(_timeout: Duration) -> Option<(u8, u8, u8)> {
    None
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;

    use super::*;

    fn env_of(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let map: HashMap<String, String> = pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        move |k| map.get(k).cloned()
    }

    #[test]
    fn choices() {
        let home = Some("/home/me");
        assert_eq!(ThemeChoice::parse("auto", home), Ok(ThemeChoice::Auto));
        assert_eq!(ThemeChoice::parse("", home), Ok(ThemeChoice::Auto));
        assert_eq!(
            ThemeChoice::parse("Dark", home),
            Ok(ThemeChoice::Builtin(Variant::Dark))
        );
        assert_eq!(
            ThemeChoice::parse("amp-light", home),
            Ok(ThemeChoice::Builtin(Variant::Light))
        );
        assert_eq!(
            ThemeChoice::parse("~/themes/x.json", home),
            Ok(ThemeChoice::File("/home/me/themes/x.json".into()))
        );
        assert_eq!(
            ThemeChoice::parse("mine.json", home),
            Ok(ThemeChoice::File("mine.json".into()))
        );
        assert!(ThemeChoice::parse("solarized", home).is_err());
    }

    #[test]
    fn precedence_flag_env_config() {
        let config = "[server]\nurl = \"x\"\n[tui]\ntheme = \"light\"\ncolor = \"truecolor\"\n";
        let none = env_of(&[]);
        let s = ThemeSettings::resolve(None, &none, Some(config)).unwrap();
        assert_eq!(s.theme, ThemeChoice::Builtin(Variant::Light));
        assert_eq!(s.depth, Some(ColorDepth::TrueColor));

        let env = env_of(&[(ENV_THEME, "dark"), (ENV_COLOR, "256")]);
        let s = ThemeSettings::resolve(None, &env, Some(config)).unwrap();
        assert_eq!(s.theme, ThemeChoice::Builtin(Variant::Dark));
        assert_eq!(s.depth, Some(ColorDepth::Ansi256));

        let s = ThemeSettings::resolve(Some("light"), &env, Some(config)).unwrap();
        assert_eq!(s.theme, ThemeChoice::Builtin(Variant::Light));

        let s = ThemeSettings::resolve(None, &none, None).unwrap();
        assert_eq!(
            s,
            ThemeSettings {
                theme: ThemeChoice::Auto,
                depth: None
            }
        );
        // A config without [tui] is fine; a typo inside [tui] is not.
        assert!(ThemeSettings::resolve(None, &none, Some("[workers]\nmax_parallel = 2\n")).is_ok());
        let e = ThemeSettings::resolve(None, &none, Some("[tui]\nthme = \"dark\"\n")).unwrap_err();
        assert!(e.0.contains("[tui]"), "{e}");
        let e = ThemeSettings::resolve(None, &env_of(&[(ENV_COLOR, "16")]), None).unwrap_err();
        assert!(e.0.contains(ENV_COLOR), "{e}");
        let e = ThemeSettings::resolve(Some("neon"), &none, None).unwrap_err();
        assert!(e.0.contains("--theme"), "{e}");
    }

    #[test]
    fn show_thinking_from_env_then_config_then_collapsed() {
        use crate::ThinkingDisplay;
        let none = env_of(&[]);
        assert_eq!(
            resolve_show_thinking(&none, None).unwrap(),
            ThinkingDisplay::Collapsed
        );
        let config = "[tui]\ntheme = \"dark\"\nshow_thinking = \"full\"\n";
        assert_eq!(
            resolve_show_thinking(&none, Some(config)).unwrap(),
            ThinkingDisplay::Full
        );
        // The theme settings accept the key too.
        assert!(ThemeSettings::resolve(None, &none, Some(config)).is_ok());
        let env = env_of(&[(ENV_SHOW_THINKING, "Hidden")]);
        assert_eq!(
            resolve_show_thinking(&env, Some(config)).unwrap(),
            ThinkingDisplay::Hidden
        );
        let e =
            resolve_show_thinking(&none, Some("[tui]\nshow_thinking = \"some\"\n")).unwrap_err();
        assert!(e.0.contains("[tui] show_thinking"), "{}", e.0);
    }

    #[test]
    fn depth_detection() {
        assert_eq!(
            detect_depth(&env_of(&[("COLORTERM", "truecolor")])),
            ColorDepth::TrueColor
        );
        assert_eq!(
            detect_depth(&env_of(&[("COLORTERM", "24bit")])),
            ColorDepth::TrueColor
        );
        assert_eq!(
            detect_depth(&env_of(&[("TERM", "xterm-direct")])),
            ColorDepth::TrueColor
        );
        assert_eq!(
            detect_depth(&env_of(&[("TERM", "xterm-256color")])),
            ColorDepth::Ansi256
        );
        assert_eq!(detect_depth(&env_of(&[])), ColorDepth::Ansi256);
    }

    #[test]
    fn auto_follows_the_background() {
        assert_eq!(variant_for_background((0x06, 0x0A, 0x0C)), Variant::Dark);
        assert_eq!(variant_for_background((0xFF, 0xFF, 0xFF)), Variant::Light);
        assert_eq!(variant_for_background((0x1E, 0x1E, 0x1E)), Variant::Dark);
        assert_eq!(variant_for_background((0xEF, 0xEF, 0xEC)), Variant::Light);

        let env = env_of(&[("COLORTERM", "truecolor")]);
        let auto = ThemeSettings {
            theme: ThemeChoice::Auto,
            depth: None,
        };
        let t = auto.load(&env, || Some((255, 255, 255))).unwrap();
        assert_eq!(
            (t.name.as_str(), t.depth),
            ("amp-light", ColorDepth::TrueColor)
        );
        let t = auto.load(&env, || None).unwrap();
        assert_eq!(t.name, "amp-dark");
        // A fixed choice never asks the terminal.
        let fixed = ThemeSettings {
            theme: ThemeChoice::Builtin(Variant::Light),
            depth: Some(ColorDepth::Ansi256),
        };
        let t = fixed
            .load(&env, || panic!("must not query the terminal"))
            .unwrap();
        assert_eq!(
            (t.name.as_str(), t.depth),
            ("amp-light", ColorDepth::Ansi256)
        );
    }

    #[test]
    fn osc11_replies() {
        let xterm = b"\x1b]11;rgb:0606/0a0a/0c0c\x1b\\\x1b[?62;22c";
        assert!(reply_complete(xterm));
        assert_eq!(parse_background_reply(xterm), Some((0x06, 0x0A, 0x0C)));
        let bel = b"\x1b]11;rgb:ff/ff/ff\x07\x1b[?1;2c";
        assert_eq!(parse_background_reply(bel), Some((0xFF, 0xFF, 0xFF)));
        // No OSC 11 support: only the DA1 answer.
        let da1_only = b"\x1b[?65;1;9c";
        assert!(reply_complete(da1_only));
        assert_eq!(parse_background_reply(da1_only), None);
        // Partial answers.
        assert!(!reply_complete(b"\x1b]11;rgb:ff/ff/ff\x07\x1b[?6"));
        assert_eq!(parse_background_reply(b"\x1b]11;rgb:ff/ff"), None);
    }
}

/// [`exchange`] against a fake terminal on a pty (#2155). Every wait is bounded: `exchange` by
/// its own bounds, the fake terminal by its script, and the follow-up reads by `select`.
#[cfg(all(test, unix))]
mod pty_tests {
    use std::{
        fs::File,
        io::{Read, Write},
        os::fd::AsFd,
        thread,
        time::{Duration, Instant},
    };

    use nix::{
        pty::openpty,
        sys::{
            select::{FdSet, select},
            termios::{SetArg, cfmakeraw, tcgetattr, tcsetattr},
            time::{TimeVal, TimeValLike},
        },
    };

    use super::*;

    const OSC11_REPLY: &[u8] = b"\x1b]11;rgb:ffff/ffff/ffff\x1b\\";
    const DA1_REPLY: &[u8] = b"\x1b[?62;22c";

    /// A raw-mode pty pair: (master = the fake terminal, slave = the tty `exchange` talks to).
    fn pty() -> (File, File) {
        let pair = openpty(None, None).expect("openpty");
        let mut t = tcgetattr(&pair.slave).unwrap();
        cfmakeraw(&mut t);
        tcsetattr(&pair.slave, SetArg::TCSANOW, &t).unwrap();
        (File::from(pair.master), File::from(pair.slave))
    }

    /// Plays `script` (delay from start, bytes) on the terminal side, in a thread that ends by
    /// itself once the script is done.
    fn terminal(master: &File, script: Vec<(u64, &'static [u8])>) -> thread::JoinHandle<()> {
        let mut master = master.try_clone().unwrap();
        let start = Instant::now();
        thread::spawn(move || {
            for (at, bytes) in script {
                let at = Duration::from_millis(at);
                thread::sleep(at.saturating_sub(start.elapsed()));
                master.write_all(bytes).unwrap();
            }
        })
    }

    /// Everything readable on `tty` within `wait` of the last byte.
    fn pending(tty: &File, wait: Duration) -> Vec<u8> {
        let mut out = Vec::new();
        let mut r = tty;
        loop {
            let mut fds = FdSet::new();
            fds.insert(tty.as_fd());
            let mut tv = TimeVal::milliseconds(wait.as_millis() as i64);
            if !matches!(select(None, &mut fds, None, None, &mut tv), Ok(n) if n > 0) {
                return out;
            }
            let mut buf = [0u8; 256];
            match r.read(&mut buf) {
                Ok(n) if n > 0 => out.extend_from_slice(&buf[..n]),
                _ => return out,
            }
        }
    }

    /// Writes a key press on the terminal side and returns what the next reader of the tty sees.
    fn next_reader_sees(master: &File, slave: &File) -> Vec<u8> {
        let mut m = master;
        m.write_all(b"x").unwrap();
        pending(slave, Duration::from_millis(300))
    }

    fn concat(parts: &[&[u8]]) -> Vec<u8> {
        parts.concat()
    }

    const FIRST: Duration = Duration::from_millis(300);
    const DRAIN: Duration = Duration::from_millis(700);

    #[test]
    fn osc11_and_da1_are_read_and_parsed() {
        let (master, slave) = pty();
        let reply = concat(&[OSC11_REPLY, DA1_REPLY]);
        let reply: &'static [u8] = Box::leak(reply.into_boxed_slice());
        let term = terminal(&master, vec![(20, reply)]);
        let started = Instant::now();
        let got = exchange(&slave, FIRST, DRAIN).unwrap();
        assert!(
            started.elapsed() < FIRST,
            "returns at the DA1 reply, not the bound"
        );
        term.join().unwrap();
        assert_eq!(got, reply);
        assert_eq!(parse_background_reply(&got), Some((255, 255, 255)));
        assert_eq!(next_reader_sees(&master, &slave), b"x");
    }

    #[test]
    fn type_ahead_after_da1_is_left_for_the_tui() {
        let (master, slave) = pty();
        let term = terminal(
            &master,
            vec![(20, b"\x1b]11;rgb:0000/0000/0000\x07\x1b[?1;2ck")],
        );
        let got = exchange(&slave, FIRST, DRAIN).unwrap();
        term.join().unwrap();
        assert!(got.ends_with(b"c"));
        assert_eq!(parse_background_reply(&got), Some((0, 0, 0)));
        assert_eq!(next_reader_sees(&master, &slave), b"kx");
    }

    #[test]
    fn da1_only_ends_the_exchange() {
        let (master, slave) = pty();
        let term = terminal(&master, vec![(20, DA1_REPLY)]);
        let started = Instant::now();
        let got = exchange(&slave, FIRST, DRAIN).unwrap();
        assert!(started.elapsed() < FIRST);
        term.join().unwrap();
        assert_eq!(got, DA1_REPLY);
        assert_eq!(parse_background_reply(&got), None);
        assert_eq!(next_reader_sees(&master, &slave), b"x");
    }

    #[test]
    fn no_answer_gives_up_at_the_first_bound_and_swallows_nothing() {
        let (master, slave) = pty();
        let started = Instant::now();
        let got = exchange(&slave, FIRST, DRAIN).unwrap();
        let took = started.elapsed();
        assert!(got.is_empty());
        assert!(took >= FIRST && took < FIRST + DRAIN, "took {took:?}");
        // The query went out to the terminal.
        assert_eq!(
            pending(&master, Duration::from_millis(100)),
            BACKGROUND_QUERY.as_bytes()
        );
        // Nothing is left reading the tty: the next key press reaches the next reader.
        assert_eq!(next_reader_sees(&master, &slave), b"x");
    }

    #[test]
    fn a_reply_finishing_past_the_first_bound_is_drained() {
        let (master, slave) = pty();
        // Starts inside the first bound, finishes (DA1) after it but inside the drain.
        let term = terminal(
            &master,
            vec![
                (100, &OSC11_REPLY[..8]),
                (500, &OSC11_REPLY[8..]),
                (550, DA1_REPLY),
            ],
        );
        let started = Instant::now();
        let got = exchange(&slave, FIRST, DRAIN).unwrap();
        let took = started.elapsed();
        term.join().unwrap();
        assert!(
            took >= Duration::from_millis(550) && took < FIRST + DRAIN,
            "took {took:?}"
        );
        assert!(reply_complete(&got));
        assert_eq!(parse_background_reply(&got), Some((255, 255, 255)));
        // No reply byte reaches the TUI.
        assert_eq!(next_reader_sees(&master, &slave), b"x");
    }

    #[test]
    fn an_incomplete_reply_stops_at_the_drain_bound() {
        let (master, slave) = pty();
        let term = terminal(&master, vec![(50, &OSC11_REPLY[..8])]);
        let started = Instant::now();
        let got = exchange(&slave, FIRST, DRAIN).unwrap();
        let took = started.elapsed();
        term.join().unwrap();
        assert_eq!(got, &OSC11_REPLY[..8]);
        assert!(
            took >= FIRST + DRAIN && took < FIRST + DRAIN + Duration::from_millis(500),
            "took {took:?}"
        );
        assert_eq!(next_reader_sees(&master, &slave), b"x");
    }
}
