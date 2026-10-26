//! #2347: watching a running worker from the chat on a real pty, at 120x40 and 80x24.
//!
//! The faux manager dispatches two tickets at once (#2085, which the mock AMP knows as "M1-4
//! CLI" in AMP Harness › M1 CLI, and #9002, which it doesn't know: its lines fall back to the bare
//! id). Each worker runs two `sleep` turns, so its elapsed time, tokens and context move while the
//! test watches. The test checks:
//!
//! - the worker strip above the editor: one chip per worker with the ticket name (shortened at 80
//!   columns), agent, model, state, tokens and `ctx N%`, and the hint `2 running · click one or
//!   Ctrl+W to watch`; the transcript's `▶ worker started` notices and the dispatch lines' rows;
//! - the numbers updating: elapsed time ticks, tokens grow;
//! - an SGR 1006 left click on a chip opens that worker's live session: its header names the
//!   ticket, story › epic, agent, model and state, then tokens and the context bar;
//! - Esc goes back to the chat; Ctrl+W, → and Enter open the other worker; Esc again;
//! - once both finish, a click on a `■ worker finished` notice opens a finished session,
//!   read-only.
//! - #2355: the dispatches refresh the board at once; the session opened mid-run shows the
//!   earlier `sleep` call's duration (never `0 ms`); when the manager's reply shows, both
//!   finished notices are above it and the strip no longer says running.
//!
//! The screen is rebuilt from the escape stream by a small emulator ([`Screen`]). HOME, XDG dirs
//! and the file credential store live under cargo's target tmp dir; AMP is the in-process mock.
//! `AMP_TEST_DUMP=1` prints the screens (the renderings in the ticket).
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
use serde_json::json;

const CONFIG: &str = r#"
[providers.boss]
api = "faux"
[providers.boss.models."boss-1"]

[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]
context_window = 100000

[roles.manager]
model = "boss/boss-1"
[roles.worker-backend]
model = "wf/wf-1"
"#;

const KNOWN: i64 = 2085;
const UNKNOWN: i64 = 9002;

/// The faux script: the manager dispatches both tickets in one message; each worker sleeps twice
/// (a turn each), then answers.
fn script(sleep: u32) -> serde_json::Value {
    let run = |id: i64| {
        json!({"name": "amp_run_ticket",
               "arguments": {"task_id": id, "agent_id": "amp:amp-worker-backend", "worktree": false}})
    };
    let nap = json!({
        "thinking": "I should look around first.",
        "text": "Looking around.",
        "tool_calls": [{"name": "bash", "arguments": {"command": format!("sleep {sleep}")}}]
    });
    json!({
        "boss": [{"tool_calls": [run(KNOWN), run(UNKNOWN)]}, {"text": "Both workers are done."}],
        "wf": [nap.clone(), nap.clone(), nap.clone(), nap, {"text": "done"}, {"text": "done"}]
    })
}

/// The visible screen, rebuilt from what the chat writes (absolute and relative cursor moves,
/// erase in line and display; every character one cell, which holds for the chat's glyphs).
struct Screen {
    rows: Vec<Vec<char>>,
    row: usize,
    col: usize,
    width: usize,
}

impl Screen {
    fn new(width: usize, height: usize) -> Self {
        Self {
            rows: vec![vec![' '; width]; height],
            row: 0,
            col: 0,
            width,
        }
    }

    fn feed(&mut self, text: &str) {
        let mut it = text.chars().peekable();
        while let Some(ch) = it.next() {
            match ch {
                '\x1b' => match it.next() {
                    Some('[') => {
                        let mut params = String::new();
                        let mut fin = ' ';
                        for x in it.by_ref() {
                            if x.is_ascii_alphabetic() || x == '~' || x == '@' {
                                fin = x;
                                break;
                            }
                            params.push(x);
                        }
                        self.csi(&params, fin);
                    }
                    // OSC (links, titles) and other strings: up to BEL or ST.
                    Some(']' | '_' | 'P' | '^') => {
                        while let Some(x) = it.next() {
                            if x == '\x07' {
                                break;
                            }
                            if x == '\x1b' && it.peek() == Some(&'\\') {
                                it.next();
                                break;
                            }
                        }
                    }
                    _ => {}
                },
                '\r' => self.col = 0,
                '\n' => {
                    if self.row + 1 >= self.rows.len() {
                        self.rows.remove(0);
                        self.rows.push(vec![' '; self.width]);
                    } else {
                        self.row += 1;
                    }
                }
                c if (c as u32) < 0x20 => {}
                c => {
                    if let Some(cell) = self
                        .rows
                        .get_mut(self.row)
                        .and_then(|r| r.get_mut(self.col))
                    {
                        *cell = c;
                    }
                    self.col += 1;
                }
            }
        }
    }

    fn csi(&mut self, params: &str, fin: char) {
        if params.starts_with('?') || params.starts_with('<') || params.starts_with('>') {
            return;
        }
        let nums: Vec<usize> = params.split(';').map(|p| p.parse().unwrap_or(0)).collect();
        let n = |i: usize| nums.get(i).copied().filter(|v| *v > 0).unwrap_or(1);
        let h = self.rows.len();
        match fin {
            'H' | 'f' => {
                self.row = (n(0) - 1).min(h - 1);
                self.col = n(1) - 1;
            }
            'A' => self.row = self.row.saturating_sub(n(0)),
            'B' => self.row = (self.row + n(0)).min(h - 1),
            'C' => self.col += n(0),
            'D' => self.col = self.col.saturating_sub(n(0)),
            'G' => self.col = n(0) - 1,
            'K' => {
                let mode = nums.first().copied().unwrap_or(0);
                let row = &mut self.rows[self.row];
                let range = match mode {
                    1 => 0..(self.col + 1).min(row.len()),
                    2 => 0..row.len(),
                    _ => self.col.min(row.len())..row.len(),
                };
                for c in range {
                    row[c] = ' ';
                }
            }
            'J' if nums.first().copied().unwrap_or(0) >= 2 => {
                for r in &mut self.rows {
                    r.fill(' ');
                }
            }
            _ => {}
        }
    }

    fn lines(&self) -> Vec<String> {
        self.rows
            .iter()
            .map(|r| r.iter().collect::<String>().trim_end().to_owned())
            .collect()
    }

    fn text(&self) -> String {
        self.lines().join("\n")
    }
}

struct Chat {
    child: std::process::Child,
    master: File,
    seen: Arc<Mutex<Vec<u8>>>,
    width: usize,
    height: usize,
}

impl Chat {
    fn start(dir: &Path, url: &str, cols: u16, rows: u16) -> Self {
        Self::start_with_path(
            dir,
            url,
            cols,
            rows,
            &std::env::var("PATH").unwrap_or_default(),
        )
    }

    /// [`Self::start`] with the chat's PATH (#2395: a fake browser opener in front).
    fn start_with_path(dir: &Path, url: &str, cols: u16, rows: u16, path: &str) -> Self {
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
            .env("PATH", path)
            .env_remove("AMP_TOKEN")
            .env_remove("AMP_CA_FILE")
            .env_remove("AMP_WEB_URL")
            .env_remove("AMP_MCP_PROTOCOL")
            .env_remove("AMP_HARNESS_PROFILE")
            .env_remove("AMP_HARNESS_MOUSE")
            .env_remove("AMP_HARNESS_MAX_WORKERS")
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
            width: usize::from(cols),
            height: usize::from(rows),
        }
    }

    /// The screen as everything written so far leaves it.
    fn screen(&self) -> Screen {
        let mut s = Screen::new(self.width, self.height);
        s.feed(&String::from_utf8_lossy(&self.seen.lock().unwrap()));
        s
    }

    fn send(&mut self, bytes: &[u8]) {
        let _ = self.master.write_all(bytes);
        let _ = self.master.flush();
    }

    /// An SGR 1006 left click (press and release) at the 0-based `row` and `col`.
    fn click(&mut self, row: usize, col: usize) {
        let (r, c) = (row + 1, col + 1);
        self.send(format!("\x1b[<0;{c};{r}M\x1b[<0;{c};{r}m").as_bytes());
    }

    /// Waits (at most 30 s) until the screen satisfies `ok`; returns it.
    async fn wait(&self, what: &str, ok: impl Fn(&Screen) -> bool) -> Screen {
        let start = Instant::now();
        loop {
            let screen = self.screen();
            if ok(&screen) {
                return screen;
            }
            assert!(
                start.elapsed() < Duration::from_secs(30),
                "waited for {what}:\n{}",
                screen.text()
            );
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }
}

impl Drop for Chat {
    fn drop(&mut self) {
        let _ = self.child.kill();
    }
}

fn dump(title: &str, screen: &Screen) {
    if std::env::var_os("AMP_TEST_DUMP").is_some() {
        eprintln!(
            "----- {title} ({}x{}) -----\n{}\n-----",
            screen.width,
            screen.rows.len(),
            screen.text()
        );
    }
}

/// The row showing `needle`, and the column it starts at.
fn find(screen: &Screen, needle: &str) -> Option<(usize, usize)> {
    screen
        .lines()
        .iter()
        .enumerate()
        .find_map(|(r, line)| line.find(needle).map(|at| (r, line[..at].chars().count())))
}

/// The worker strip's chip row for `id`: `▸ #id …`.
fn chip(screen: &Screen, id: i64) -> Option<String> {
    let needle = format!("▸ #{id}");
    screen
        .lines()
        .into_iter()
        .find(|l| l.trim_start().starts_with(&needle))
}

/// `running 12s` → 12 seconds (minutes too).
fn running_secs(line: &str) -> Option<u64> {
    let at = line.find("running ")? + "running ".len();
    let word: String = line[at..]
        .chars()
        .take_while(|c| !c.is_whitespace())
        .collect();
    let (m, s) = match word.split_once('m') {
        Some((m, s)) => (m.parse::<u64>().ok()?, s.trim_end_matches('s')),
        None => (0, word.trim_end_matches('s')),
    };
    Some(m * 60 + s.parse::<u64>().ok()?)
}

/// `48.0k tok` / `812 tok` → tokens.
fn tokens_of(line: &str) -> Option<f64> {
    let at = line.find(" tok")?;
    let word = line[..at].rsplit(' ').next()?;
    match word.strip_suffix('k') {
        Some(k) => k.parse::<f64>().ok().map(|v| v * 1000.0),
        None => word.parse().ok(),
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn click_or_ctrl_w_opens_a_running_workers_session_with_its_metadata() {
    for (cols, rows) in [(120u16, 40u16), (80, 24)] {
        watch_at(cols, rows).await;
    }
}

async fn watch_at(cols: u16, rows: u16) {
    let width = usize::from(cols);
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir(&format!("chat-worker-watch-{cols}x{rows}"), Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), CONFIG).unwrap();
    std::fs::write(dir.join("script.json"), script(8).to_string()).unwrap();
    let mut chat = Chat::start(&dir, &amp.url, cols, rows);
    chat.wait("the editor", |s| s.text().contains("/ for commands"))
        .await;
    chat.send(b"run both tickets\r");

    // Both workers show above the editor, with what they are for, and the hint.
    let s = chat
        .wait("two chips, a name and the hint", |s| {
            let t = s.text();
            t.contains("2 running · click one or Ctrl+W to watch")
                && chip(s, KNOWN).is_some_and(|l| l.contains("M1-4 CLI") && l.contains("ctx "))
                && chip(s, UNKNOWN).is_some_and(|l| l.contains(" tok"))
        })
        .await;
    dump("chat with two running workers", &s);
    // #2355: the dispatches refresh the board (the status line's Ready count) at once, not on
    // the next 30s poll: AMP gets an amp_list_tasks after the last amp_dispatch_task.
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let calls = amp.mock.calls.lock().unwrap().clone();
        let last_dispatch = calls
            .iter()
            .rposition(|(name, _)| name == "amp_dispatch_task")
            .expect("dispatched");
        if calls[last_dispatch..]
            .iter()
            .any(|(name, _)| name == "amp_list_tasks")
        {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "no board refresh after the dispatches: {:?}",
            calls.iter().map(|(n, _)| n).collect::<Vec<_>>()
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    let known = chip(&s, KNOWN).unwrap();
    let unknown = chip(&s, UNKNOWN).unwrap();
    for line in s.lines() {
        assert!(line.chars().count() <= width, "{line:?}");
    }
    for (line, id) in [(&known, KNOWN), (&unknown, UNKNOWN)] {
        assert!(line.contains(&format!("#{id}")), "{line}");
        assert!(line.contains("worker-backend"), "{line}");
        assert!(line.contains("running "), "{line}");
    }
    // No ticket name from AMP: the bare id, then the agent.
    assert!(
        unknown.contains(&format!("#{UNKNOWN} · worker-backend")),
        "{unknown}"
    );
    if width >= 120 {
        assert!(known.contains("AMP Harness › M1 CLI"), "{known}");
        assert!(known.contains("(wf-1)"), "{known}");
    }
    let t = s.text();
    assert!(t.contains("▶ worker started"), "{t}");
    assert!(t.contains(&format!("run_ticket #{KNOWN}")), "{t}");

    // The numbers move: the elapsed time ticks, the tokens grow with the second turn.
    let secs0 = running_secs(&known).unwrap();
    let tok0 = tokens_of(&known).unwrap();
    chat.wait("the elapsed time to tick and the tokens to grow", |s| {
        chip(s, KNOWN).is_some_and(|l| {
            running_secs(&l).is_some_and(|v| v > secs0) && tokens_of(&l).is_some_and(|v| v > tok0)
        })
    })
    .await;

    // A click on the chip opens the worker's live session, with its metadata header.
    let s = chat.screen();
    let (row, col) = find(&s, &format!("▸ #{KNOWN}")).unwrap();
    chat.click(row, col + 4);
    let s = chat
        .wait("the session of #2085", |s| {
            let t = s.text();
            t.contains(&format!("worker #{KNOWN} M1-4 CLI")) && t.contains("Esc back to the chat")
        })
        .await;
    dump("worker session view", &s);
    let t = s.text();
    let (head, _) = find(&s, &format!("worker #{KNOWN}")).unwrap();
    let lines = s.lines();
    assert!(lines[head].contains("running "), "{t}");
    assert!(lines[head + 1].contains("amp:amp-worker-backend"), "{t}");
    if width >= 120 {
        assert!(lines[head + 1].contains("wf/wf-1"), "{t}");
        assert!(lines[head + 1].contains("AMP Harness › M1 CLI"), "{t}");
    }
    assert!(
        lines[head + 2].contains("in ") && lines[head + 2].contains("out "),
        "{t}"
    );
    assert!(
        lines[head + 2].contains("ctx ") && lines[head + 2].contains('%'),
        "{t}"
    );
    assert!(
        t.contains("Looking around."),
        "the worker's transcript: {t}"
    );
    assert!(t.contains(&format!("watching #{KNOWN}")), "{t}");
    // #2355: what the worker did before the view opened shows how long it took (its first
    // `sleep 8` call), never `0 ms`; its thinking says it thought, not a bare "Thinking".
    let done: Vec<&String> = lines.iter().filter(|l| l.contains("✓ bash")).collect();
    assert!(!done.is_empty(), "{t}");
    for line in done {
        let line = line.trim_end();
        assert!(!line.ends_with(" ms"), "{t}");
        assert!(line.ends_with('s'), "{t}");
    }
    assert!(
        !lines.iter().any(|l| {
            let l = l.trim();
            l == "▸ Thinking" || l.starts_with("▸ Thinking · Ctrl+T")
        }),
        "{t}"
    );
    assert!(
        !t.contains("run both tickets"),
        "the manager's transcript is hidden: {t}"
    );

    // Esc: back to the chat.
    chat.send(b"\x1b");
    chat.wait("the chat again", |s| {
        let t = s.text();
        t.contains("run both tickets") && t.contains("click one or Ctrl+W to watch")
    })
    .await;

    // The keyboard path: Ctrl+W, → to the other worker, Enter.
    chat.send(b"\x17");
    let s = chat
        .wait("the strip to take the keys", |s| {
            s.text().contains("←/→ choose · Enter watch")
        })
        .await;
    dump("worker strip focused (Ctrl+W)", &s);
    assert!(
        s.lines()
            .iter()
            .any(|l| l.trim_start().starts_with(&format!("❯ #{KNOWN}"))),
        "{}",
        s.text()
    );
    chat.send(b"\x1b[C");
    chat.wait("the selection to move", |s| {
        s.lines()
            .iter()
            .any(|l| l.trim_start().starts_with(&format!("❯ #{UNKNOWN}")))
    })
    .await;
    chat.send(b"\r");
    chat.wait("the session of #9002", |s| {
        s.text().contains(&format!("worker #{UNKNOWN}"))
    })
    .await;
    chat.send(b"\x1b");
    chat.wait("the chat again", |s| s.text().contains("run both tickets"))
        .await;

    // /workers lists both with the same details, and a click on a row watches it.
    chat.send(b"/workers\r");
    let s = chat
        .wait("the /workers panel", |s| {
            let t = s.text();
            t.contains("AMP · workers") && t.contains("Enter/click watch")
        })
        .await;
    dump("/workers panel", &s);
    let t = s.text();
    assert!(t.contains(&format!("#{KNOWN} M1-4 CLI")), "{t}");
    assert!(t.contains("amp:amp-worker-backend · local"), "{t}");
    for line in s.lines() {
        assert!(line.chars().count() <= width, "{line:?}");
    }
    let row = s
        .lines()
        .iter()
        .position(|l| l.contains(&format!("#{UNKNOWN} ")) && l.contains("· local"))
        .unwrap();
    chat.click(row, 6);
    chat.wait("the session of #9002 from /workers", |s| {
        s.text().contains(&format!("worker #{UNKNOWN}"))
    })
    .await;
    chat.send(b"\x1b");
    chat.wait("the chat again", |s| s.text().contains("run both tickets"))
        .await;

    // Both finish: notices say so, and a click on one opens its finished session, read-only.
    // #2355: by the time the manager's reply shows, both notices are above it and the strip no
    // longer says they are running.
    let s = chat
        .wait("the manager's reply", |s| {
            s.text().contains("Both workers are done.")
        })
        .await;
    dump("chat after both workers finished", &s);
    let t = s.text();
    let reply = find(&s, "Both workers are done.").unwrap().0;
    for id in [KNOWN, UNKNOWN] {
        let notice = find(&s, &format!("■ worker finished #{id}"))
            .or_else(|| {
                s.lines()
                    .iter()
                    .position(|l| l.contains("■ worker finished") && l.contains(&format!("#{id}")))
                    .map(|r| (r, 0))
            })
            .unwrap_or_else(|| panic!("no finished notice for #{id}: {t}"));
        assert!(
            notice.0 < reply,
            "#{id}'s notice comes before the reply: {t}"
        );
    }
    assert!(!t.contains("running · click one"), "{t}");
    assert!(
        !s.lines()
            .iter()
            .any(|l| l.trim_start().starts_with('▸') && l.contains("running ")),
        "{t}"
    );
    let (row, col) = find(&s, &format!("■ worker finished #{KNOWN}"))
        .or_else(|| find(&s, &format!("#{KNOWN}")))
        .unwrap();
    let col = s.lines()[row]
        .find(&format!("#{KNOWN}"))
        .map(|at| s.lines()[row][..at].chars().count())
        .unwrap_or(col);
    chat.click(row, col + 1);
    let s = chat
        .wait("the finished session", |s| {
            let t = s.text();
            t.contains(&format!("worker #{KNOWN}")) && t.contains("read-only")
        })
        .await;
    dump("finished worker session", &s);
    chat.send(b"\x1b");
    chat.wait("the chat again", |s| s.text().contains("run both tickets"))
        .await;

    for _ in 0..3 {
        chat.send(b"\x03");
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

/// #2395: while /workers is open every printable key is typing. On a real pty, with both workers
/// running: `xo hello` lands in the editor exactly, nothing is stopped or opened (a fake `open` /
/// `xdg-open` on PATH records any URL the chat opens); then Ctrl+G opens the selected row's ticket
/// and Ctrl+X asks to stop its worker, `y` stops it, and the typed text is still in the editor.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn typing_with_the_workers_panel_open_goes_to_the_editor() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("chat-workers-panel-typing", Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), CONFIG).unwrap();
    std::fs::write(dir.join("script.json"), script(20).to_string()).unwrap();
    let bin = dir.join("bin");
    std::fs::create_dir_all(&bin).unwrap();
    let opened = dir.join("opened.txt");
    for name in ["open", "xdg-open"] {
        let path = bin.join(name);
        std::fs::write(
            &path,
            format!("#!/bin/sh\necho \"$1\" >> '{}'\n", opened.display()),
        )
        .unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
    }
    let path = format!(
        "{}:{}",
        bin.display(),
        std::env::var("PATH").unwrap_or_default()
    );
    let mut chat = Chat::start_with_path(&dir, &amp.url, 120, 40, &path);
    chat.wait("the editor", |s| s.text().contains("/ for commands"))
        .await;
    chat.send(b"run both tickets\r");
    chat.wait("two running workers", |s| {
        s.text()
            .contains("2 running · click one or Ctrl+W to watch")
    })
    .await;

    chat.send(b"/workers\r");
    let s = chat
        .wait("the workers panel and its hint", |s| {
            let t = s.text();
            t.contains("AMP · workers")
                && t.contains("Enter/click watch · Ctrl+X stop · Ctrl+G open in AMP · Esc close")
        })
        .await;
    dump("workers panel", &s);

    chat.send(b"xo hello");
    let s = chat
        .wait("the typed text in the editor", |s| {
            s.text().contains("xo hello")
        })
        .await;
    // Give a wrongly taken key time to show its effect before checking for none.
    tokio::time::sleep(Duration::from_millis(500)).await;
    let s2 = chat.screen();
    dump("typed with the panel open", &s2);
    let t = s2.text();
    let typed: Vec<String> = s2
        .lines()
        .into_iter()
        .filter(|l| l.contains("xo hello"))
        .collect();
    assert_eq!(typed.len(), 1, "{t}");
    assert_eq!(
        typed[0].trim_matches(|c: char| !c.is_alphanumeric()),
        "xo hello",
        "exactly the typed text: {t}"
    );
    assert!(t.contains("AMP · workers"), "the panel stays open: {t}");
    assert!(!t.contains("stop the worker on"), "x asked nothing: {t}");
    assert!(!opened.exists(), "o opened nothing: {t}");
    assert!(amp.mock.calls_to("amp_set_task_state").is_empty(), "{t}");
    assert!(s.text().contains("2 running"), "{}", s.text());

    // Ctrl+G opens the selected row's ticket in AMP, whatever the editor holds.
    chat.send(b"\x07");
    let deadline = Instant::now() + Duration::from_secs(10);
    let url = loop {
        let text = std::fs::read_to_string(&opened).unwrap_or_default();
        if text.contains("task=") {
            break text.trim().to_owned();
        }
        assert!(
            Instant::now() < deadline,
            "Ctrl+G opened nothing:\n{}",
            chat.screen().text()
        );
        tokio::time::sleep(Duration::from_millis(50)).await;
    };
    assert_eq!(url.lines().count(), 1, "{url}");
    let id: i64 = url
        .rsplit("task=")
        .next()
        .and_then(|v| v.parse().ok())
        .unwrap_or_else(|| panic!("no task id in {url}"));
    assert!([KNOWN, UNKNOWN].contains(&id), "{url}");

    // Ctrl+X asks to stop that row's worker; y stops it.
    chat.send(b"\x18");
    let s = chat
        .wait("the stop question", |s| {
            s.text().contains(&format!("stop the worker on #{id}?"))
        })
        .await;
    dump("stop question in the workers panel", &s);
    chat.send(b"y");
    let deadline = Instant::now() + Duration::from_secs(20);
    while amp.mock.calls_to("amp_set_task_state").is_empty() {
        assert!(
            Instant::now() < deadline,
            "y stopped nothing:\n{}",
            chat.screen().text()
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    let states = amp.mock.calls_to("amp_set_task_state");
    assert_eq!(states.len(), 1, "{states:?}");
    assert_eq!(states[0]["task_id"], json!(id), "{states:?}");
    let s = chat.screen();
    assert!(
        s.text().contains("xo hello"),
        "the typed text is still in the editor: {}",
        s.text()
    );

    for _ in 0..3 {
        chat.send(b"\x03");
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}
