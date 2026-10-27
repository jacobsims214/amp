//! #2425: after the manager asks for plan approval, the chat waits for it in AMP and resumes the
//! manager by itself once a person approves in the web UI: no typing in the chat. On a real pty
//! (120x40), against the in-process mock AMP whose `amp_check_plan` answer the test changes the
//! way a person's click on "Approve plan" changes AMP's (the harness never approves: the mock
//! plays the person). The faux manager checks the plan and asks for approval; the status line
//! then says "⏸ waiting for plan approval in AMP" with the My work link and the transcript
//! says so too. Once "approved", the transcript says who approved, the manager's next turn runs
//! exactly once, and the waiting segment goes. Esc stops the wait: an approval after it starts
//! nothing.
//!
//! The screen is rebuilt from the bytes the chat writes by a small terminal model ([`Screen`],
//! as in tests/chat_relink.rs).
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
use serde_json::{Value, json};

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
    fn start(dir: &Path, url: &str, cols: u16, rows: u16, env: &[(&str, &str)]) -> Self {
        let size = Winsize {
            ws_row: rows,
            ws_col: cols,
            ws_xpixel: 0,
            ws_ypixel: 0,
        };
        let pty = openpty(Some(&size), None).expect("openpty");
        let stdio = || Stdio::from(pty.slave.try_clone().unwrap());
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_amp-harness"));
        cmd.args(["--no-session", "--no-setup"])
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
            .stderr(stdio());
        for (k, v) in env {
            cmd.env(k, v);
        }
        let child = cmd.spawn().expect("spawn amp-harness");
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

    /// Waits (at most 30 s) until `ok` holds for the screen, and returns it.
    async fn wait_for(&self, what: &str, ok: impl Fn(&[String]) -> bool) -> Vec<String> {
        let start = Instant::now();
        loop {
            let screen = self.screen();
            if ok(&screen) {
                return screen;
            }
            assert!(
                start.elapsed() < Duration::from_secs(30),
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

/// `APPROVAL_SCREENS=1` prints the screens checked (for a look at them).
fn show(what: &str, screen: &[String]) {
    if std::env::var_os("APPROVAL_SCREENS").is_some() {
        eprintln!("--- {what}\n{}\n", shown(screen));
    }
}

/// What `amp_check_plan` answers while #4050 (the person's own) waits for approval.
fn awaiting() -> Value {
    json!({
        "project_id": 2, "require_plan_approval": true,
        "awaiting_approval": [4050],
        "awaiting_approval_tickets": [{"task_id": 4050, "owner_user_id": 7, "yours": true}],
        "awaiting_yours": [4050],
        "plan_approved_at": "2026-10-26T10:00:00Z", "plan_approved_by": 7,
        "approve_url": "/project/2/my-work?approve=all"
    })
}

/// The same after the person clicked Approve plan in the web UI.
fn approved() -> Value {
    json!({
        "project_id": 2, "require_plan_approval": true,
        "awaiting_approval": [], "awaiting_approval_tickets": [], "awaiting_yours": [],
        "plan_approved_at": "2026-10-26T14:02:00Z", "plan_approved_by": 7
    })
}

/// A chat whose manager checks the plan and asks for approval, with `config` added.
async fn asked_for_approval(
    name: &str,
    extra_config: &str,
) -> (Chat, Arc<Mutex<Value>>, common::Running) {
    let plan = Arc::new(Mutex::new(awaiting()));
    let amp = spawn(
        MockAmp {
            plan: Some(plan.clone()),
            ..MockAmp::new()
        },
        None,
    )
    .await;
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(
        config.join("config.toml"),
        format!("{CONFIG}\n{extra_config}"),
    )
    .unwrap();
    std::fs::write(
        dir.join("script.json"),
        json!([
            {"tool_calls": [{"name": "amp_check_plan", "id": "c1", "arguments": {"project_id": 2}}]},
            {"text": "Plan ready: approve it in AMP."},
            {"text": "Resumed: dispatching the approved tickets."},
            {"text": "SECOND RESUME"}
        ])
        .to_string(),
    )
    .unwrap();
    // 140 columns: the status line keeps the approval link's text (`/my-work?approve=all`, #2495).
    let mut chat = Chat::start(&dir, &amp.url, 140, 40, &[]);
    chat.wait_for("the editor", |s| shown(s).contains("/ for commands"))
        .await;
    chat.send(b"plan the work\r");
    chat.wait_for("the manager's answer", |s| {
        shown(s).contains("Plan ready: approve it in AMP.")
    })
    .await;
    (chat, plan, amp)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_manager_resumes_once_a_person_approves() {
    let (mut chat, plan, amp) = asked_for_approval("chat-approval-resume", "").await;
    let screen = chat
        .wait_for("the waiting state", |s| {
            let t = shown(s);
            t.contains("⏸ waiting for plan approval in AMP")
                && t.contains("Waiting for plan approval in AMP (#4050)")
        })
        .await;
    show("waiting", &screen);
    let t = shown(&screen);
    // The status line names the My work tab (built from the instance, not the server's text).
    let status = screen
        .iter()
        .find(|l| l.contains("⏸ waiting for plan approval in AMP"))
        .unwrap();
    assert!(status.contains("/project/2/my-work?approve=all"), "{t}");
    assert!(!t.contains("Resumed"), "{t}");

    // Still waiting a while later: no approval, no turn.
    tokio::time::sleep(Duration::from_secs(2)).await;
    assert!(!shown(&chat.screen()).contains("Resumed"));

    // A person approves in the web UI.
    *plan.lock().unwrap() = approved();
    let screen = chat
        .wait_for("the resume", |s| {
            let t = shown(s);
            t.contains("Resumed: dispatching the approved tickets.")
                && !t.contains("⏸ waiting for plan approval in AMP")
        })
        .await;
    show("resumed", &screen);
    let t = shown(&screen);
    assert!(
        t.contains("✓ Plan approved in AMP by local-dev@example.com at"),
        "{t}"
    );
    assert!(t.contains("(#4050). The manager carries on."), "{t}");
    assert!(
        t.contains("[amp-harness] The plan was approved in AMP"),
        "{t}"
    );
    // Exactly once: more plan reads after the resume start nothing.
    tokio::time::sleep(Duration::from_secs(7)).await;
    let t = shown(&chat.screen());
    assert!(!t.contains("SECOND RESUME"), "{t}");
    assert_eq!(t.matches("✓ Plan approved in AMP").count(), 1, "{t}");
    // The harness never approved anything itself.
    assert!(amp.mock.calls_to("amp_approve_plan").is_empty());
    for _ in 0..2 {
        chat.send(b"\x03");
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    assert_eq!(chat.exit_code(), 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn esc_stops_the_wait_and_an_approval_then_starts_nothing() {
    let (mut chat, plan, _amp) = asked_for_approval("chat-approval-esc", "").await;
    chat.wait_for("the waiting state", |s| {
        shown(s).contains("⏸ waiting for plan approval in AMP")
    })
    .await;
    chat.send(b"\x1b");
    let screen = chat
        .wait_for("the wait to stop", |s| {
            let t = shown(s);
            t.contains("Stopped waiting for plan approval.")
                && !t.contains("⏸ waiting for plan approval in AMP")
        })
        .await;
    show("stopped", &screen);
    *plan.lock().unwrap() = approved();
    tokio::time::sleep(Duration::from_secs(7)).await;
    let t = shown(&chat.screen());
    assert!(!t.contains("Resumed"), "{t}");
    assert!(!t.contains("✓ Plan approved"), "{t}");
    for _ in 0..2 {
        chat.send(b"\x03");
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    assert_eq!(chat.exit_code(), 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn turned_off_the_chat_doesnt_wait() {
    let (mut chat, plan, _amp) = asked_for_approval(
        "chat-approval-off",
        "[manager]\nauto_resume_on_approval = false\n",
    )
    .await;
    tokio::time::sleep(Duration::from_secs(2)).await;
    let t = shown(&chat.screen());
    assert!(!t.contains("waiting for plan approval in AMP"), "{t}");
    // The usual chip still says the plan waits for the person.
    assert!(
        t.contains("plan: 1 of yours waiting for your approval"),
        "{t}"
    );
    *plan.lock().unwrap() = approved();
    tokio::time::sleep(Duration::from_secs(7)).await;
    let t = shown(&chat.screen());
    assert!(!t.contains("Resumed"), "{t}");
    for _ in 0..2 {
        chat.send(b"\x03");
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    assert_eq!(chat.exit_code(), 0);
}
