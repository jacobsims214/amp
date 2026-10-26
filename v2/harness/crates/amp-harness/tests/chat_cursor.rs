//! #2344: the chat's cursor on a real pty (120x40 and 80x24). The terminal is asked for a
//! blinking bar (`ESC[5 q`) and shown (`ESC[?25h`) at the editor's insertion point, inside the
//! padding; it follows typed wide characters, hides while the `/model` picker has the keys and
//! comes back when it closes; a click (SGR mouse report) into the editor moves the insertion
//! point; and quitting puts back the terminal's default cursor shape (`ESC[0 q`) and shows it.
//!
//! The chat runs on the faux model against the in-process mock AMP, with HOME, XDG dirs and the
//! file credential store under cargo's target tmp dir.
#![cfg(unix)]

mod common;

use std::{
    fs::File,
    io::{Read, Write},
    path::Path,
    process::{Command, Stdio},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use common::{MockAmp, spawn, workdir};
use nix::{pty::Winsize, pty::openpty};

/// The sizes checked, and the editor's first column (1-based) at each: 2 columns of padding
/// at 120, 1 at 80.
const SIZES: [(u16, u16, usize); 2] = [(120, 40, 3), (80, 24, 2)];

const CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]
[providers.faux.models."faux-2"]

[roles.manager]
model = "faux/faux-1"
"#;

/// Where the last frame left the cursor: shown at `(row, col)` (1-based), or hidden.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Cursor {
    Shown(usize, usize),
    Hidden,
}

struct Chat {
    child: std::process::Child,
    master: File,
    seen: Arc<Mutex<Vec<u8>>>,
}

impl Chat {
    fn start(dir: &Path, url: &str, cols: u16, rows: u16) -> Self {
        let size = Winsize {
            ws_row: rows,
            ws_col: cols,
            ws_xpixel: 0,
            ws_ypixel: 0,
        };
        let pty = openpty(Some(&size), None).expect("openpty");
        let stdio = || Stdio::from(pty.slave.try_clone().unwrap());
        let child = Command::new(env!("CARGO_BIN_EXE_amp-harness"))
            .args(["--no-session", "--no-setup"])
            .current_dir(dir.join("project"))
            .env("HOME", dir.join("home"))
            .env("XDG_CONFIG_HOME", dir.join("home/.config"))
            .env("XDG_STATE_HOME", dir.join("home/.local/state"))
            .env("AMP_CREDENTIAL_STORE", "file")
            .env("AMP_MCP_URL", url)
            .env("AMP_HARNESS_CEILING_DIR", dir)
            .env("AMP_HARNESS_FAUX_SCRIPT", dir.join("script.json"))
            .env("TERM", "xterm-256color")
            .env_remove("AMP_TOKEN")
            .env_remove("AMP_CA_FILE")
            .env_remove("AMP_WEB_URL")
            .env_remove("AMP_MCP_PROTOCOL")
            .env_remove("AMP_HARNESS_PROFILE")
            .env_remove("AMP_HARNESS_MOUSE")
            .env_remove("ANTHROPIC_API_KEY")
            .env_remove("OPENROUTER_API_KEY")
            .env_remove("RUST_LOG")
            .stdin(stdio())
            .stdout(stdio())
            .stderr(stdio())
            .spawn()
            .expect("spawn amp-harness");
        drop(pty.slave);
        let master = File::from(pty.master);
        let seen = Arc::new(Mutex::new(Vec::new()));
        let mut reader = master.try_clone().unwrap();
        let sink = seen.clone();
        std::thread::spawn(move || {
            let mut buf = [0u8; 8192];
            while let Ok(n) = reader.read(&mut buf) {
                if n == 0 {
                    break;
                }
                sink.lock().unwrap().extend_from_slice(&buf[..n]);
            }
        });
        Self {
            child,
            master,
            seen,
        }
    }

    fn text(&self) -> String {
        String::from_utf8_lossy(&self.seen.lock().unwrap()).into_owned()
    }

    /// The output from byte `from` on.
    fn since(&self, from: usize) -> String {
        let seen = self.seen.lock().unwrap();
        String::from_utf8_lossy(&seen[from.min(seen.len())..]).into_owned()
    }

    fn len(&self) -> usize {
        self.seen.lock().unwrap().len()
    }

    fn send(&mut self, bytes: &[u8]) {
        let _ = self.master.write_all(bytes);
        let _ = self.master.flush();
    }

    /// Waits (at most 20 s) until `ok` holds for the output from byte `from` on.
    async fn wait_for(&self, from: usize, what: &str, ok: impl Fn(&str) -> bool) -> String {
        let start = Instant::now();
        loop {
            let text = self.since(from);
            if ok(&text) {
                return text;
            }
            assert!(
                start.elapsed() < Duration::from_secs(20),
                "waited for {what}:\n{}",
                printable(&text)
            );
            tokio::time::sleep(Duration::from_millis(30)).await;
        }
    }

    /// Waits until the last frame from byte `from` on leaves the cursor `want`.
    async fn wait_cursor(&self, from: usize, want: Cursor) -> String {
        self.wait_for(from, &format!("{want:?}"), |t| last_cursor(t) == Some(want))
            .await
    }

    fn exit_code(&mut self) -> i32 {
        let start = Instant::now();
        loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                return status.code().unwrap_or(-1);
            }
            if start.elapsed() > Duration::from_secs(10) {
                let _ = self.child.kill();
                panic!(
                    "still running 10 s after quitting:\n{}",
                    printable(&self.text())
                );
            }
            std::thread::sleep(Duration::from_millis(20));
        }
    }
}

impl Drop for Chat {
    fn drop(&mut self) {
        let _ = self.child.kill();
    }
}

/// Escape bytes made visible, for failure messages.
fn printable(text: &str) -> String {
    let tail: String = text
        .chars()
        .rev()
        .take(4000)
        .collect::<Vec<_>>()
        .into_iter()
        .rev()
        .collect();
    tail.replace('\x1b', "␛")
}

/// The cursor each frame ends with: rpi-tui ends a frame (`ESC[?2026l`, the end of synchronized
/// output) with `ESC[row;colH ESC[?25h` where the editor's insertion point is, or `ESC[?25l`
/// when no component marks one.
fn last_cursor(text: &str) -> Option<Cursor> {
    let mut last = None;
    let mut rest = text;
    let mut offset = 0;
    while let Some(at) = rest.find("\x1b[?2026l") {
        let before = &text[..offset + at];
        if before.ends_with("\x1b[?25l") {
            last = Some(Cursor::Hidden);
        } else if let Some(head) = before.strip_suffix("\x1b[?25h")
            && let Some(head) = head.strip_suffix('H')
            && let Some(start) = head.rfind("\x1b[")
            && let Some((r, c)) = head[start + 2..].split_once(';')
            && let (Ok(r), Ok(c)) = (r.parse(), c.parse())
        {
            last = Some(Cursor::Shown(r, c));
        }
        offset += at + "\x1b[?2026l".len();
        rest = &text[offset..];
    }
    last
}

#[test]
fn frames_are_read_for_their_cursor() {
    assert_eq!(
        last_cursor("\x1b[?2026h\x1b[3;1Hab\x1b[37;5H\x1b[?25h\x1b[?2026l"),
        Some(Cursor::Shown(37, 5))
    );
    assert_eq!(
        last_cursor("\x1b[37;5H\x1b[?25h\x1b[?2026l\x1b[?2026h\x1b[?25l\x1b[?2026l"),
        Some(Cursor::Hidden)
    );
    assert_eq!(last_cursor("nothing"), None);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_input_shows_a_blinking_bar_cursor_at_the_insertion_point() {
    for (cols, rows, first_col) in SIZES {
        cursor_at(cols, rows, first_col).await;
    }
}

async fn cursor_at(cols: u16, rows: u16, first_col: usize) {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir(&format!("chat-cursor-{cols}x{rows}"), Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), CONFIG).unwrap();
    std::fs::write(dir.join("script.json"), "[]").unwrap();
    let mut chat = Chat::start(&dir, &amp.url, cols, rows);

    // On start: a blinking bar, shown at the start of the editor's text row, inside the left
    // padding and above the editor's bottom rule, the status line, the footer and the blank
    // bottom row.
    let text = chat
        .wait_for(0, "the editor's placeholder", |t| {
            t.contains("/ for commands")
        })
        .await;
    assert!(text.contains("\x1b[5 q"), "{}", printable(&text));
    let start = chat
        .wait_for(0, "a shown cursor", |t| {
            matches!(last_cursor(t), Some(Cursor::Shown(..)))
        })
        .await;
    let Some(Cursor::Shown(row, col)) = last_cursor(&start) else {
        unreachable!()
    };
    assert_eq!(col, first_col, "{}", printable(&start));
    assert!(
        (usize::from(rows) - 10..usize::from(rows) - 2).contains(&row),
        "row {row}: {}",
        printable(&start)
    );

    // Wide characters: 6 + 2×2 columns.
    let mark = chat.len();
    chat.send("héllo 世界".as_bytes());
    chat.wait_cursor(mark, Cursor::Shown(row, first_col + 10))
        .await;

    // The /model picker takes ↑↓ and Enter: no cursor while it is open with an empty editor.
    let mark = chat.len();
    chat.send(b"\x15"); // Ctrl+U: clear the line.
    chat.wait_cursor(mark, Cursor::Shown(row, first_col)).await;
    let mark = chat.len();
    chat.send(b"/model");
    chat.wait_for(mark, "the / menu", |t| t.contains("/model"))
        .await;
    let mark = chat.len();
    chat.send(b"\r");
    chat.wait_cursor(mark, Cursor::Hidden).await;
    // Esc closes it: the cursor is back where it was.
    let mark = chat.len();
    chat.send(b"\x1b");
    chat.wait_cursor(mark, Cursor::Shown(row, first_col)).await;

    // A click into the text moves the insertion point; typing goes there.
    let mark = chat.len();
    chat.send(b"hello world");
    chat.wait_cursor(mark, Cursor::Shown(row, first_col + 11))
        .await;
    let mark = chat.len();
    let at = first_col + 6;
    chat.send(format!("\x1b[<0;{at};{row}M\x1b[<0;{at};{row}m").as_bytes());
    chat.wait_cursor(mark, Cursor::Shown(row, at)).await;
    let mark = chat.len();
    chat.send(b"X");
    let text = chat.wait_cursor(mark, Cursor::Shown(row, at + 1)).await;
    assert!(
        text.contains("X") && chat.text().contains("Xworld"),
        "{}",
        printable(&text)
    );

    // Quit (Ctrl+C clears the editor, then twice quits): the default cursor shape, shown.
    for _ in 0..3 {
        chat.send(b"\x03");
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    let code = chat.exit_code();
    tokio::time::sleep(Duration::from_millis(100)).await;
    let text = chat.text();
    assert_eq!(code, 0, "{}", printable(&text));
    let bar = text.rfind("\x1b[5 q").expect("the bar");
    let default = text
        .rfind("\x1b[0 q")
        .expect("the default shape on the way out");
    assert!(default > bar, "{}", printable(&text));
    assert!(
        text[default..].contains("\x1b[?25h"),
        "shown after: {}",
        printable(&text[default..])
    );
}
