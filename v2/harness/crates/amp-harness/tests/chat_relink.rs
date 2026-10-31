//! #2383: the manager writes `.amp.json` in the chat (amp-init, #2363) and the chat follows it
//! without a restart. On a real pty (120x40): the chat starts in a folder with no marker (header
//! and status line say "no project"); a scripted manager turn writes `.amp.json` naming project
//! #2 with the `write` tool; then the transcript says `Linked this folder to project #2 AMP v2`,
//! the header names the project, the status line shows its board, and the manager's next request
//! carries `project_id: 2` in its `<amp_context>` (read back through the faux model's
//! `{{context:KEY}}` placeholder).
//!
//! The screen is rebuilt from the bytes the chat writes by a small terminal model ([`Screen`],
//! as in tests/chat_thinking.rs). The chat runs on the faux model against the in-process mock
//! AMP, with HOME, XDG dirs and the file credential store under cargo's target tmp dir.
#![cfg(unix)]

mod common;

use std::{
    fs::File,
    io::{Read, Write},
    path::Path,
    process::Stdio,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use common::{MockAmp, pty_command, spawn, workdir};
use nix::{pty::Winsize, pty::openpty};
use serde_json::json;

const CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
"#;

/// A terminal's character grid, fed the chat's output.
struct Screen {
    rows: Vec<Vec<char>>,
    width: usize,
    row: usize,
    col: usize,
}

impl Screen {
    fn new(width: usize, height: usize) -> Self {
        Self {
            rows: vec![vec![' '; width]; height],
            width,
            row: 0,
            col: 0,
        }
    }

    fn feed(&mut self, text: &str) {
        let mut chars = text.chars().peekable();
        while let Some(c) = chars.next() {
            match c {
                '\x1b' => match chars.next() {
                    Some('[') => {
                        let mut params = String::new();
                        let mut fin = ' ';
                        for p in chars.by_ref() {
                            if ('@'..='~').contains(&p) {
                                fin = p;
                                break;
                            }
                            params.push(p);
                        }
                        self.csi(&params, fin);
                    }
                    // OSC, APC, DCS: up to BEL or ST.
                    Some(']' | '_' | 'P') => {
                        while let Some(p) = chars.next() {
                            if p == '\x07' {
                                break;
                            }
                            if p == '\x1b' && chars.peek() == Some(&'\\') {
                                chars.next();
                                break;
                            }
                        }
                    }
                    _ => {}
                },
                '\r' => self.col = 0,
                '\n' => self.down(1),
                '\x07' | '\x08' => {}
                c if c >= ' ' => {
                    if self.col >= self.width {
                        self.col = 0;
                        self.down(1);
                    }
                    if let Some(r) = self.rows.get_mut(self.row) {
                        r[self.col] = c;
                    }
                    self.col += 1;
                }
                _ => {}
            }
        }
    }

    fn down(&mut self, n: usize) {
        self.row += n;
        while self.row >= self.rows.len() {
            self.rows.remove(0);
            self.rows.push(vec![' '; self.width]);
            self.row -= 1;
        }
    }

    fn csi(&mut self, params: &str, fin: char) {
        if params.starts_with(['?', '>', '<', '=']) {
            return;
        }
        let nums: Vec<usize> = params.split(';').map(|p| p.parse().unwrap_or(0)).collect();
        let n = |i: usize| nums.get(i).copied().filter(|v| *v > 0).unwrap_or(1);
        let height = self.rows.len();
        match fin {
            'H' | 'f' => {
                self.row = (n(0) - 1).min(height - 1);
                self.col = (n(1) - 1).min(self.width - 1);
            }
            'A' => self.row = self.row.saturating_sub(n(0)),
            'B' => self.row = (self.row + n(0)).min(height - 1),
            'C' => self.col = (self.col + n(0)).min(self.width - 1),
            'D' => self.col = self.col.saturating_sub(n(0)),
            'G' => self.col = (n(0) - 1).min(self.width - 1),
            'd' => self.row = (n(0) - 1).min(height - 1),
            'K' => {
                let r = &mut self.rows[self.row];
                match nums.first().copied().unwrap_or(0) {
                    0 => r[self.col.min(self.width)..].fill(' '),
                    1 => r[..=self.col.min(self.width - 1)].fill(' '),
                    _ => r.fill(' '),
                }
            }
            'J' => match nums.first().copied().unwrap_or(0) {
                0 => {
                    self.rows[self.row][self.col.min(self.width)..].fill(' ');
                    for r in &mut self.rows[self.row + 1..] {
                        r.fill(' ');
                    }
                }
                _ => {
                    for r in &mut self.rows {
                        r.fill(' ');
                    }
                }
            },
            _ => {}
        }
    }

    fn lines(&self) -> Vec<String> {
        self.rows
            .iter()
            .map(|r| r.iter().collect::<String>().trim_end().to_owned())
            .collect()
    }
}

struct Chat {
    child: std::process::Child,
    master: File,
    seen: Arc<Mutex<Vec<u8>>>,
    size: (usize, usize),
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
        let child = pty_command()
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
            .env_remove("AMP_HARNESS_SHOW_THINKING")
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
            size: (usize::from(cols), usize::from(rows)),
        }
    }

    /// The screen after everything written so far.
    fn screen(&self) -> Vec<String> {
        let mut screen = Screen::new(self.size.0, self.size.1);
        screen.feed(&String::from_utf8_lossy(&self.seen.lock().unwrap()));
        screen.lines()
    }

    fn send(&mut self, bytes: &[u8]) {
        let _ = self.master.write_all(bytes);
        let _ = self.master.flush();
    }

    /// Waits (at most 20 s) until `ok` holds for the screen, and returns it.
    async fn wait_for(&self, what: &str, ok: impl Fn(&[String]) -> bool) -> Vec<String> {
        let start = Instant::now();
        loop {
            let screen = self.screen();
            if ok(&screen) {
                return screen;
            }
            assert!(
                start.elapsed() < Duration::from_secs(20),
                "waited for {what}:\n{}",
                screen.join("\n")
            );
            tokio::time::sleep(Duration::from_millis(30)).await;
        }
    }

    fn exit_code(&mut self) -> i32 {
        let start = Instant::now();
        loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                return status.code().unwrap_or(-1);
            }
            if start.elapsed() > Duration::from_secs(10) {
                let _ = self.child.kill();
                panic!("still running 10 s after quitting");
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

fn shown(screen: &[String]) -> String {
    screen.join("\n")
}

/// `RELINK_SCREENS=1` prints the screens checked (for a look at them).
fn show(what: &str, screen: &[String]) {
    if std::env::var_os("RELINK_SCREENS").is_some() {
        eprintln!("--- {what}\n{}\n", shown(screen));
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_chat_follows_an_amp_json_the_manager_writes() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("chat-relink", None);
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), CONFIG).unwrap();
    let marker = json!({"project_id": 2, "project_name": "AMP v2", "project_code": "amp-v2"});
    std::fs::write(
        dir.join("script.json"),
        json!([
            {"text": "Before: project {{context:project_id}}."},
            {"tool_calls": [{"name": "write", "id": "w1", "arguments": {
                "path": ".amp.json", "content": format!("{marker:#}\n")}}]},
            {"text": "Wrote .amp.json for project 2."},
            {"text": "After: project {{context:project_id}} {{context:project_name}}."}
        ])
        .to_string(),
    )
    .unwrap();
    let mut chat = Chat::start(&dir, &amp.url, 120, 40);
    let screen = chat
        .wait_for("the editor", |s| shown(s).contains("/ for commands"))
        .await;
    show("start", &screen);
    // Nothing linked: the header and the AMP status line say so.
    let start = shown(&screen);
    assert!(start.matches("no project").count() >= 2, "{start}");

    chat.send(b"which project?\r");
    chat.wait_for("the first answer", |s| {
        shown(s).contains("Before: project none")
    })
    .await;

    chat.send(b"set up a new project here\r");
    let screen = chat
        .wait_for("the notice and the board", |s| {
            let t = shown(s);
            t.contains("Wrote .amp.json for project 2.")
                && t.contains("Linked this folder to project #2 AMP v2.")
                && !t.contains("no project")
                && t.contains("1 In progress")
        })
        .await;
    show("linked", &screen);
    assert!(
        dir.join("project/.amp.json").is_file(),
        "the manager's write landed"
    );
    // The header names the project (the poller fetched it), and so does the banner.
    let top: String = screen[..4].join("\n");
    // (#2882: the project leads the Conversation box's top edge, after the brand bands.)
    assert!(top.contains("▀▀ AMP v2 #2"), "{}", shown(&screen));
    assert!(
        top.contains("project #2 AMP v2 · http://"),
        "{}",
        shown(&screen)
    );

    // The manager's next request names the project in its <amp_context>.
    chat.send(b"and now?\r");
    let screen = chat
        .wait_for("the manager's context", |s| {
            shown(s).contains("After: project 2 AMP v2.")
        })
        .await;
    show("context", &screen);

    // Quit: Ctrl+C twice.
    for _ in 0..2 {
        chat.send(b"\x03");
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    assert_eq!(chat.exit_code(), 0);
}
