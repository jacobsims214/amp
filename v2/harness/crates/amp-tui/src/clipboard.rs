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
//! the tool and its exit, never the text. Nothing here reads the clipboard.
//!
//! [`Desktop::copy_async`] is the event-loop form (select-to-copy, #4212). It sanitises the text
//! ([`sanitize_for_copy`]), refuses an empty or oversize copy ([`MAX_COPY_BYTES`]) before sending
//! anything, writes OSC 52 right away, and runs the system tool on tokio's blocking pool, so the
//! caller never waits on `pbcopy`. [`copy_notice`] words the result without overstating it.

use std::{
    ffi::OsString,
    fmt,
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

/// The most [`Desktop::copy_async`] copies, in bytes of sanitised UTF-8 (1 MiB). A design limit,
/// not a measured terminal one: OSC 52 sends it base64-encoded in one escape (a third larger), and
/// terminals cap or drop large ones (kitty's limit is 512 MB, most don't document one).
pub const MAX_COPY_BYTES: usize = 1 << 20;

/// Why [`Desktop::copy_async`] sent nothing.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CopyError {
    /// Nothing but whitespace (after [`sanitize_for_copy`]).
    Empty,
    /// More than `max` bytes; nothing was sent, not even part of it.
    TooLarge { bytes: usize, max: usize },
}

impl fmt::Display for CopyError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Empty => f.write_str("Nothing to copy."),
            Self::TooLarge { bytes, max } => write!(
                f,
                "Selection too large to copy ({} KB, limit {} KB).",
                bytes.div_ceil(1024),
                max / 1024
            ),
        }
    }
}

impl std::error::Error for CopyError {}

/// The clipboard and the browser, as the chat may use them.
#[derive(Clone)]
pub struct Desktop {
    on: bool,
    platform: Platform,
    /// Where tools are looked up (`$PATH`'s form); `None` = no tools.
    path: Option<OsString>,
    wayland: bool,
    x11: bool,
    /// An SSH session: a native tool would write the remote machine's clipboard, so
    /// [`Desktop::copy_async`] skips macOS's `pbcopy` (OSC 52 reaches the person's own machine).
    remote: bool,
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
            remote: false,
            terminal: None,
        }
    }

    /// This computer's tools, found in `$PATH`; remote when `SSH_TTY` or `SSH_CONNECTION` is set.
    pub fn system() -> Self {
        let set = |k: &str| std::env::var_os(k).is_some_and(|v| !v.is_empty());
        Self {
            on: true,
            platform: Platform::current(),
            path: std::env::var_os("PATH"),
            wayland: set("WAYLAND_DISPLAY"),
            x11: set("DISPLAY"),
            remote: false,
            terminal: None,
        }
        .with_remote(set("SSH_TTY") || set("SSH_CONNECTION"))
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
            remote: false,
            terminal: None,
        }
    }

    /// Also sends OSC 52 through `write`.
    pub fn with_terminal(mut self, write: TerminalWrite) -> Self {
        self.terminal = Some(write);
        self
    }

    /// Marks an SSH session: [`Desktop::copy_async`] then skips macOS's `pbcopy`, which would
    /// copy on the remote Mac, not the person's. ([`Desktop::copy`] is unchanged.)
    pub fn with_remote(mut self, remote: bool) -> Self {
        self.remote = remote;
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
        self.copy_with_tools(&self.copy_tools(), text, &mut out);
        out
    }

    /// Copies `text` without making the caller wait on a tool (select-to-copy on the event loop).
    ///
    /// The text is first [sanitised](sanitize_for_copy); an empty or oversize result is refused
    /// before anything is sent. Otherwise OSC 52 is written to the terminal now, before this
    /// returns, and the first working system tool runs on tokio's blocking pool (at most
    /// [`TOOL_WAIT`] each, stdin included); the handle resolves to what was done. Over SSH
    /// ([`Desktop::with_remote`]) macOS's `pbcopy` is skipped. With the desktop off, nothing is
    /// sent and the handle resolves to an empty [`Copied`].
    ///
    /// Must be called inside a tokio runtime.
    pub fn copy_async(&self, text: String) -> Result<tokio::task::JoinHandle<Copied>, CopyError> {
        let text = sanitize_for_copy(&text);
        if text.trim().is_empty() {
            return Err(CopyError::Empty);
        }
        if text.len() > MAX_COPY_BYTES {
            return Err(CopyError::TooLarge {
                bytes: text.len(),
                max: MAX_COPY_BYTES,
            });
        }
        let mut out = Copied::default();
        if !self.on {
            return Ok(tokio::task::spawn_blocking(move || out));
        }
        if let Some(write) = &self.terminal {
            write(&osc52(&text));
            out.osc52 = true;
        }
        let tools: Vec<_> = self
            .copy_tools()
            .into_iter()
            .filter(|(name, _)| !(self.remote && *name == "pbcopy"))
            .collect();
        let desktop = self.clone();
        Ok(tokio::task::spawn_blocking(move || {
            desktop.copy_with_tools(&tools, &text, &mut out);
            out
        }))
    }

    /// Tries `tools` in order with `text` on stdin, recording the first that works in `out`.
    fn copy_with_tools(
        &self,
        tools: &[(&'static str, &'static [&'static str])],
        text: &str,
        out: &mut Copied,
    ) {
        for &(name, args) in tools {
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

/// Runs `path args`, feeding `stdin`; waits at most [`TOOL_WAIT`] in all, writing stdin included
/// (a tool still running then counts as done, as does one that stopped reading). Its output goes
/// nowhere: the chat owns the terminal.
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
    // The write happens on the waiting thread, so a tool that never reads its stdin can't hold
    // the caller past TOOL_WAIT once the text outgrows the pipe's buffer.
    let pipe = child.stdin.take();
    let text = stdin.map(|t| t.as_bytes().to_vec());
    let (tx, rx) = mpsc::channel();
    std::thread::spawn(move || {
        let written = match (text, pipe) {
            // Dropping the pipe after the write closes it: the tool sees end of input.
            (Some(text), Some(mut pipe)) => pipe.write_all(&text),
            _ => Ok(()),
        };
        let status = child.wait();
        let _ = tx.send((written, status));
    });
    match rx.recv_timeout(TOOL_WAIT) {
        Ok((_, Ok(status))) if !status.success() => Err(format!("exited with {status}")),
        Ok((Err(e), _)) => Err(e.to_string()),
        Ok((Ok(()), Ok(_))) => Ok(()),
        Ok((Ok(()), Err(e))) => Err(e.to_string()),
        Err(_) => Ok(()),
    }
}

/// `text` without control characters, so a copy can't carry an escape into the terminal or a
/// later paste (`ESC[201~` would end bracketed paste early): C0 (ESC, CR, BEL...), DEL and C1 are
/// dropped, except `\n`, which is kept, and tab, which becomes four spaces (as the editor's paste
/// does) so indentation survives.
pub fn sanitize_for_copy(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        match c {
            '\n' => out.push('\n'),
            '\t' => out.push_str("    "),
            c if c.is_control() => {}
            c => out.push(c),
        }
    }
    out
}

/// What to tell the person after a [`Desktop::copy_async`] of `chars` characters, worded so it
/// never claims more than happened: "Copied" only when a system tool took it; OSC 52 alone is
/// unacknowledged, so it is "sent" with a hint. `None` when nothing was tried (desktop off).
/// Never includes the copied text.
pub fn copy_notice(chars: usize, copied: &Copied) -> Option<String> {
    let n = if chars == 1 {
        "1 char".to_owned()
    } else {
        format!("{chars} chars")
    };
    let why = copied
        .tool_error
        .as_deref()
        .map(|e| format!(" ({e})"))
        .unwrap_or_default();
    if copied.tool.is_some() {
        Some(format!("Copied {n}."))
    } else if copied.osc52 {
        Some(format!(
            "Sent {n} to the terminal's clipboard (OSC 52){why}. If paste is empty your terminal \
             blocks it (iTerm2: allow clipboard access; tmux: set -g set-clipboard on)."
        ))
    } else if copied.tool_error.is_some() {
        Some(format!(
            "Nothing copied: no clipboard here{why}. Use the terminal's selection (Option/Fn-drag, \
             or [tui] mouse = false)."
        ))
    } else {
        None
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

    /// A recording OSC 52 sink.
    fn sink() -> (TerminalWrite, Arc<std::sync::Mutex<Vec<String>>>) {
        let seen = Arc::new(std::sync::Mutex::new(Vec::new()));
        let s = Arc::clone(&seen);
        (
            Arc::new(move |seq: &str| s.lock().unwrap().push(seq.to_owned())),
            seen,
        )
    }

    /// A fresh directory for stand-in tools (never the real `$PATH`).
    #[cfg(unix)]
    fn stub_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "amp-clipboard-{tag}-{}-{:?}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// An executable `sh` stand-in called `name` in `dir`, run once (as a no-op) before it is
    /// returned: macOS assesses a new executable on its first run, which can take longer than
    /// TOOL_WAIT under parallel tests and would turn every stand-in into a "still running" success.
    #[cfg(unix)]
    fn stub(dir: &Path, name: &str, body: &str) {
        use std::os::unix::fs::PermissionsExt;
        let p = dir.join(name);
        std::fs::write(
            &p,
            format!("#!/bin/sh\n[ -n \"$AMP_STUB_WARM\" ] && exit 0\n{body}\n"),
        )
        .unwrap();
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
        let warm = Command::new(&p)
            .env("AMP_STUB_WARM", "1")
            .stdin(Stdio::null())
            .status()
            .unwrap();
        assert!(warm.success());
    }

    /// Records its stdin into `<name>.<pid>.out` next to itself.
    #[cfg(unix)]
    const RECORD: &str = "cat > \"$0.$$.out\"";

    /// What the stand-in `name` recorded, one entry per run.
    #[cfg(unix)]
    fn recorded(dir: &Path, name: &str) -> Vec<String> {
        let prefix = format!("{name}.");
        std::fs::read_dir(dir)
            .unwrap()
            .filter_map(|e| {
                let f = e.unwrap().file_name().into_string().unwrap();
                (f.starts_with(&prefix) && f.ends_with(".out"))
                    .then(|| std::fs::read_to_string(dir.join(f)).unwrap())
            })
            .collect()
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn copy_async_too_large() {
        let dir = stub_dir("large");
        stub(&dir, "pbcopy", RECORD);
        let (write, seen) = sink();
        let d = Desktop::with_path(Platform::Mac, &dir).with_terminal(write);
        let err = d.copy_async("a".repeat(MAX_COPY_BYTES + 1)).unwrap_err();
        assert_eq!(
            err,
            CopyError::TooLarge {
                bytes: MAX_COPY_BYTES + 1,
                max: MAX_COPY_BYTES
            }
        );
        assert_eq!(
            err.to_string(),
            "Selection too large to copy (1025 KB, limit 1024 KB)."
        );
        assert!(
            seen.lock().unwrap().is_empty(),
            "nothing sent to the terminal"
        );
        assert!(recorded(&dir, "pbcopy").is_empty(), "tool never run");

        // The cap is measured after sanitising: dropped controls don't count.
        let d = Desktop::with_path(Platform::Mac, "/nonexistent");
        let mut text = "a".repeat(MAX_COPY_BYTES);
        text.push_str("\x07\x07\x07");
        assert!(d.copy_async(text).is_ok());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[tokio::test]
    async fn copy_async_empty() {
        let d = Desktop::with_path(Platform::Mac, "/nonexistent");
        assert_eq!(d.copy_async(String::new()).unwrap_err(), CopyError::Empty);
        assert_eq!(
            d.copy_async(" \n\x1b\x07\r ".into()).unwrap_err(),
            CopyError::Empty
        );
    }

    #[tokio::test]
    async fn copy_async_off_sends_nothing() {
        let (write, seen) = sink();
        let d = Desktop::off().with_terminal(write);
        let copied = d.copy_async("hi".into()).unwrap().await.unwrap();
        assert_eq!(copied, Copied::default());
        assert!(seen.lock().unwrap().is_empty());
        assert_eq!(copy_notice(2, &copied), None);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn copy_async_writes_osc52_before_returning() {
        let dir = stub_dir("osc52");
        stub(&dir, "pbcopy", "sleep 5");
        let (write, seen) = sink();
        let d = Desktop::with_path(Platform::Mac, &dir).with_terminal(write);
        let handle = d.copy_async("hello \x1b[201~world".into()).unwrap();
        // Before awaiting the tool: exactly one escape, of the sanitised text.
        assert_eq!(*seen.lock().unwrap(), vec![osc52("hello [201~world")]);
        assert!(handle.await.unwrap().osc52);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn copy_async_runs_stub_tool() {
        let dir = stub_dir("runs");
        stub(&dir, "pbcopy", RECORD);
        let d = Desktop::with_path(Platform::Mac, &dir);
        let text = "line one\nline two ✓";
        let copied = d.copy_async(text.into()).unwrap().await.unwrap();
        assert_eq!(copied.tool, Some("pbcopy"));
        assert_eq!(copied.tool_error, None);
        assert!(!copied.osc52, "no terminal attached");
        assert_eq!(recorded(&dir, "pbcopy"), vec![text.to_owned()]);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn copy_async_does_not_block() {
        let dir = stub_dir("block");
        stub(&dir, "pbcopy", "sleep 5");
        let d = Desktop::with_path(Platform::Mac, &dir);
        let start = std::time::Instant::now();
        let handle = d.copy_async("x".into()).unwrap();
        assert!(
            start.elapsed() < TOOL_WAIT / 4,
            "copy_async returned after {:?}",
            start.elapsed()
        );
        let copied = handle.await.unwrap();
        assert!(
            start.elapsed() < TOOL_WAIT * 2,
            "resolved after {:?}",
            start.elapsed()
        );
        // Still running at TOOL_WAIT counts as done (xclip/wl-copy stay to serve the selection).
        assert_eq!(copied.tool, Some("pbcopy"));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn stdin_write_is_bounded() {
        let dir = stub_dir("stdin");
        // Never reads stdin; 256 KiB is well past any pipe buffer.
        stub(&dir, "pbcopy", "sleep 5");
        let d = Desktop::with_path(Platform::Mac, &dir);
        let start = std::time::Instant::now();
        let handle = d.copy_async("y".repeat(256 * 1024)).unwrap();
        assert!(start.elapsed() < TOOL_WAIT / 4);
        let copied = handle.await.unwrap();
        assert!(
            start.elapsed() < TOOL_WAIT * 2,
            "resolved after {:?}",
            start.elapsed()
        );
        assert_eq!(copied.tool, Some("pbcopy"));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn failing_tool_reports_error_without_text() {
        let dir = stub_dir("fail");
        stub(&dir, "pbcopy", "exit 3");
        let (write, _seen) = sink();
        let d = Desktop::with_path(Platform::Mac, &dir).with_terminal(write);
        let text = "s3cret-pairing-token";
        let copied = d.copy_async(text.into()).unwrap().await.unwrap();
        assert_eq!(copied.tool, None);
        assert_eq!(
            copied.tool_error.as_deref(),
            Some("pbcopy: exited with exit status: 3")
        );
        let notice = copy_notice(text.chars().count(), &copied).unwrap();
        assert!(notice.contains("OSC 52") && notice.contains("exit status: 3"));
        assert!(!notice.contains(text) && !format!("{copied:?}").contains(text));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn remote_mac_skips_pbcopy() {
        let dir = stub_dir("remote");
        stub(&dir, "pbcopy", RECORD);
        let (write, seen) = sink();
        let d = Desktop::with_path(Platform::Mac, &dir)
            .with_terminal(write)
            .with_remote(true);
        let copied = d.copy_async("over ssh".into()).unwrap().await.unwrap();
        assert!(copied.osc52);
        assert_eq!(copied.tool, None);
        assert_eq!(copied.tool_error, None);
        assert!(
            recorded(&dir, "pbcopy").is_empty(),
            "remote pbcopy never run"
        );
        assert_eq!(seen.lock().unwrap().len(), 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn unix_order() {
        let dir = stub_dir("unix");
        stub(&dir, "wl-copy", "exit 1");
        stub(&dir, "xclip", RECORD);
        stub(&dir, "xsel", RECORD);
        let d = Desktop::with_path(Platform::Unix, &dir);
        let copied = d.copy_async("unix".into()).unwrap().await.unwrap();
        assert_eq!(copied.tool, Some("xclip"), "failing wl-copy falls through");
        assert_eq!(copied.tool_error, None);
        assert_eq!(recorded(&dir, "xclip"), vec!["unix".to_owned()]);
        assert!(
            recorded(&dir, "xsel").is_empty(),
            "stops at the first success"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn copy_notice_table() {
        let text = "the copied text";
        let tool = Copied {
            tool: Some("pbcopy"),
            osc52: true,
            tool_error: None,
        };
        assert_eq!(copy_notice(15, &tool).as_deref(), Some("Copied 15 chars."));
        assert_eq!(
            copy_notice(
                1,
                &Copied {
                    tool: Some("xclip"),
                    ..Copied::default()
                }
            )
            .as_deref(),
            Some("Copied 1 char.")
        );
        let osc = Copied {
            osc52: true,
            ..Copied::default()
        };
        let n = copy_notice(15, &osc).unwrap();
        assert!(n.starts_with("Sent 15 chars to the terminal's clipboard (OSC 52)."));
        assert!(!n.contains("Copied"));
        let neither = Copied {
            tool_error: Some("xclip: exited with exit status: 1".into()),
            ..Copied::default()
        };
        let n2 = copy_notice(15, &neither).unwrap();
        assert!(n2.starts_with("Nothing copied: no clipboard here (xclip: exited"));
        assert_eq!(copy_notice(15, &Copied::default()), None);
        for n in [copy_notice(15, &tool).unwrap(), n, n2] {
            assert!(!n.contains(text));
        }
    }

    #[test]
    fn sanitize_for_copy_strips_controls() {
        assert_eq!(
            sanitize_for_copy("a\x1b[201~b\r\n\x07c\x7f\u{9b}d\tė ✓\n"),
            "a[201~b\ncd    ė ✓\n"
        );
        assert!(!sanitize_for_copy("x\x1b]52;c;?\x07").contains('\x1b'));
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
