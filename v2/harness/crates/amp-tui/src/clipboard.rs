//! Copying text to the clipboard and opening a link in the browser (#2423), for `/remote`'s
//! pairing link: the person can't always select it out of the chat (mouse capture, wrapped rows)
//! and not every terminal makes a hyperlink clickable.
//!
//! [`Desktop::copy`] does both of what works where: an OSC 52 escape to the terminal (which
//! copies on the machine the terminal runs on, even over SSH, where the terminal allows it) and
//! the system's own tool when there is one: `pbcopy` on macOS, `wl-copy` under Wayland,
//! `xclip`/`xsel` under X11. [`Desktop::open`] runs `open` (macOS) or `xdg-open` (Linux and
//! the BSDs) with an http(s) link.
//!
//! The tools are looked up in a search path ([`Desktop::system`]: `$PATH`), so tests put
//! recording stand-ins first and never touch the real clipboard or browser. A chat starts with
//! [`Desktop::off`]; only the terminal loop ([`crate::terminal::run`]) turns the real one on,
//! with the terminal's writer for OSC 52.
//!
//! Nothing here logs or returns the text it copies (the pairing link holds a secret); errors name
//! the tool and its exit, never the text.

use std::{
    ffi::OsString,
    io::Write,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::{Arc, mpsc},
    time::Duration,
};

/// Writes raw bytes to the terminal (OSC 52), between frames.
pub type TerminalWrite = Arc<dyn Fn(&str) + Send + Sync>;

/// Which tools to use.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Platform {
    /// `pbcopy`, `open`.
    Mac,
    /// `wl-copy` (Wayland), `xclip`/`xsel` (X11), `xdg-open`.
    Unix,
    /// No tools (Windows): OSC 52 only.
    Other,
}

impl Platform {
    pub fn current() -> Self {
        if cfg!(target_os = "macos") {
            Self::Mac
        } else if cfg!(unix) {
            Self::Unix
        } else {
            Self::Other
        }
    }
}

/// How long a tool may take before the chat stops waiting for it (it keeps running: `xclip`
/// can stay to serve the selection).
const TOOL_WAIT: Duration = Duration::from_secs(2);

/// The clipboard and the browser, as the chat may use them.
#[derive(Clone)]
pub struct Desktop {
    on: bool,
    platform: Platform,
    /// Where tools are looked up (`$PATH`'s form); `None` = no tools.
    path: Option<OsString>,
    wayland: bool,
    x11: bool,
    terminal: Option<TerminalWrite>,
}

/// What [`Desktop::copy`] did.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Copied {
    /// The tool that copied it (`pbcopy`), if one did.
    pub tool: Option<&'static str>,
    /// Whether the OSC 52 escape was sent to the terminal.
    pub osc52: bool,
    /// Why a tool that was found failed.
    pub tool_error: Option<String>,
}

impl Copied {
    /// Whether anything was tried at all.
    pub fn any(&self) -> bool {
        self.tool.is_some() || self.osc52
    }
}

impl Desktop {
    /// No clipboard and no browser (tests and snapshots, until the terminal loop starts).
    pub fn off() -> Self {
        Self {
            on: false,
            platform: Platform::Other,
            path: None,
            wayland: false,
            x11: false,
            terminal: None,
        }
    }

    /// This computer's tools, found in `$PATH`.
    pub fn system() -> Self {
        Self {
            on: true,
            platform: Platform::current(),
            path: std::env::var_os("PATH"),
            wayland: std::env::var_os("WAYLAND_DISPLAY").is_some_and(|v| !v.is_empty()),
            x11: std::env::var_os("DISPLAY").is_some_and(|v| !v.is_empty()),
            terminal: None,
        }
    }

    /// `platform`'s tools found only in `path` (tests: a directory of recording stand-ins).
    /// Wayland and X11 tools are both allowed.
    pub fn with_path(platform: Platform, path: impl Into<OsString>) -> Self {
        Self {
            on: true,
            platform,
            path: Some(path.into()),
            wayland: true,
            x11: true,
            terminal: None,
        }
    }

    /// Also sends OSC 52 through `write`.
    pub fn with_terminal(mut self, write: TerminalWrite) -> Self {
        self.terminal = Some(write);
        self
    }

    pub fn is_on(&self) -> bool {
        self.on
    }

    /// Copies `text`: OSC 52 to the terminal and the first system tool that works.
    pub fn copy(&self, text: &str) -> Copied {
        let mut out = Copied::default();
        if !self.on {
            return out;
        }
        if let Some(write) = &self.terminal {
            write(&osc52(text));
            out.osc52 = true;
        }
        for (name, args) in self.copy_tools() {
            let Some(path) = self.find(name) else {
                continue;
            };
            match run(&path, args, Some(text)) {
                Ok(()) => {
                    out.tool = Some(name);
                    out.tool_error = None;
                    break;
                }
                Err(e) => out.tool_error = Some(format!("{name}: {e}")),
            }
        }
        out
    }

    /// Opens an http(s) `url` in the default browser; returns the tool that did.
    pub fn open(&self, url: &str) -> Result<&'static str, String> {
        if !self.on {
            return Err("no browser in this session".to_owned());
        }
        if !(url.starts_with("https://") || url.starts_with("http://")) {
            return Err("not an http(s) link; refusing to open it".to_owned());
        }
        let name = match self.platform {
            Platform::Mac => "open",
            Platform::Unix => "xdg-open",
            Platform::Other => return Err("no browser opener on this system".to_owned()),
        };
        let path = self
            .find(name)
            .ok_or_else(|| format!("`{name}` not found"))?;
        run(&path, &[url], None)
            .map(|()| name)
            .map_err(|e| format!("{name}: {e}"))
    }

    /// The copy tools to try, in order.
    fn copy_tools(&self) -> Vec<(&'static str, &'static [&'static str])> {
        match self.platform {
            Platform::Mac => vec![("pbcopy", &[][..])],
            Platform::Unix => {
                let mut v: Vec<(&'static str, &'static [&'static str])> = Vec::new();
                if self.wayland {
                    v.push(("wl-copy", &[][..]));
                }
                if self.x11 {
                    v.push(("xclip", &["-selection", "clipboard"][..]));
                    v.push(("xsel", &["--clipboard", "--input"][..]));
                }
                v
            }
            Platform::Other => Vec::new(),
        }
    }

    fn find(&self, name: &str) -> Option<PathBuf> {
        let path = self.path.as_ref()?;
        std::env::split_paths(path)
            .map(|dir| dir.join(name))
            .find(|p| executable(p))
    }
}

#[cfg(unix)]
fn executable(p: &Path) -> bool {
    use std::os::unix::fs::PermissionsExt;
    p.metadata()
        .is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
}

#[cfg(not(unix))]
fn executable(p: &Path) -> bool {
    p.is_file()
}

/// Runs `path args`, feeding `stdin`; waits at most [`TOOL_WAIT`] (a tool still running then
/// counts as done). Its output goes nowhere: the chat owns the terminal.
fn run(path: &Path, args: &[&str], stdin: Option<&str>) -> Result<(), String> {
    let mut child = Command::new(path)
        .args(args)
        .stdin(if stdin.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|e| e.to_string())?;
    if let (Some(text), Some(mut pipe)) = (stdin, child.stdin.take()) {
        pipe.write_all(text.as_bytes()).map_err(|e| e.to_string())?;
    }
    let (tx, rx) = mpsc::channel();
    std::thread::spawn(move || {
        let _ = tx.send(child.wait());
    });
    match rx.recv_timeout(TOOL_WAIT) {
        Ok(Ok(status)) if status.success() => Ok(()),
        Ok(Ok(status)) => Err(format!("exited with {status}")),
        Ok(Err(e)) => Err(e.to_string()),
        Err(_) => Ok(()),
    }
}

/// The OSC 52 escape that puts `text` on the terminal's clipboard.
pub fn osc52(text: &str) -> String {
    format!("\x1b]52;c;{}\x07", base64(text.as_bytes()))
}

/// Standard base64 with padding (OSC 52's encoding).
pub fn base64(data: &[u8]) -> String {
    const ABC: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(data.len().div_ceil(3) * 4);
    for chunk in data.chunks(3) {
        let b = [
            chunk[0],
            chunk.get(1).copied().unwrap_or(0),
            chunk.get(2).copied().unwrap_or(0),
        ];
        let n = (u32::from(b[0]) << 16) | (u32::from(b[1]) << 8) | u32::from(b[2]);
        for i in 0..4 {
            if i <= chunk.len() {
                out.push(ABC[(n >> (18 - 6 * i)) as usize & 63] as char);
            } else {
                out.push('=');
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base64_matches_the_standard() {
        assert_eq!(base64(b""), "");
        assert_eq!(base64(b"f"), "Zg==");
        assert_eq!(base64(b"fo"), "Zm8=");
        assert_eq!(base64(b"foo"), "Zm9v");
        assert_eq!(base64(b"foobar"), "Zm9vYmFy");
        assert_eq!(osc52("hi"), "\x1b]52;c;aGk=\x07");
    }

    #[test]
    fn off_does_nothing() {
        let d = Desktop::off();
        assert!(!d.copy("x").any());
        assert!(d.open("https://amp.local/").is_err());
    }

    #[test]
    fn open_refuses_other_schemes() {
        let d = Desktop::with_path(Platform::Mac, "/nonexistent");
        assert!(
            d.open("file:///etc/passwd")
                .unwrap_err()
                .contains("refusing")
        );
        assert!(
            d.open("https://amp.local/")
                .unwrap_err()
                .contains("not found")
        );
    }
}
