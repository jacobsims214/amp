//! #2227: Ctrl+C at a setup prompt cancels setup ("setup cancelled", exit 130), nothing written,
//! and the terminal is left in cooked mode with echo on. Driven through a real pty. The child has
//! no controlling terminal, so ^C can only work as a key: exactly the case QA hit (a driver that
//! ignored SIGINT), where a cooked-mode prompt echoed ^C and waited forever.
#![cfg(unix)]

mod common;

use std::{
    fs::File,
    io::{Read, Write},
    os::fd::OwnedFd,
    path::Path,
    process::{Command, Stdio},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use common::{MockAmp, spawn, workdir};
use nix::{
    pty::openpty,
    sys::termios::{LocalFlags, tcgetattr},
};

struct Pty {
    child: std::process::Child,
    master: File,
    /// The test's own handle on the terminal, to read its mode after the child exits.
    slave: OwnedFd,
    seen: Arc<Mutex<Vec<u8>>>,
}

impl Pty {
    fn start(dir: &Path, args: &[&str], env: &[(&str, &str)]) -> Self {
        let pty = openpty(None, None).expect("openpty");
        let stdio = || Stdio::from(pty.slave.try_clone().unwrap());
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_amp-harness"));
        cmd.args(args)
            .current_dir(dir.join("project"))
            .env("HOME", dir.join("home"))
            .env("XDG_CONFIG_HOME", dir.join("home/.config"))
            .env("AMP_CREDENTIAL_STORE", "file")
            .env("AMP_HARNESS_CEILING_DIR", dir)
            .env("AMP_WEB_URL", "http://ui.invalid")
            .env("TERM", "xterm-256color")
            .env_remove("AMP_MCP_URL")
            .env_remove("AMP_TOKEN")
            .env_remove("AMP_CA_FILE")
            .env_remove("AMP_MCP_PROTOCOL")
            .env_remove("AMP_HARNESS_PROFILE")
            .env_remove("AMP_HARNESS_PROMPTS")
            .env_remove("AMP_HARNESS_KEYCHAIN_SERVICE")
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
        let master = File::from(pty.master);
        let seen = Arc::new(Mutex::new(Vec::new()));
        let mut reader = master.try_clone().unwrap();
        let sink = seen.clone();
        std::thread::spawn(move || {
            let mut buf = [0u8; 4096];
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
            slave: pty.slave,
            seen,
        }
    }

    fn text(&self) -> String {
        String::from_utf8_lossy(&self.seen.lock().unwrap()).into_owned()
    }

    /// Waits (at most 30 s) for `needle` on the terminal.
    fn expect(&self, needle: &str) {
        let start = Instant::now();
        while !self.text().contains(needle) {
            assert!(
                start.elapsed() < Duration::from_secs(30),
                "no {needle:?} on the terminal:\n{}",
                self.text()
            );
            std::thread::sleep(Duration::from_millis(50));
        }
    }

    fn send(&mut self, bytes: &[u8]) {
        self.master.write_all(bytes).unwrap();
        self.master.flush().unwrap();
    }

    /// The exit code; the child must exit within 5 s ("promptly").
    fn exit_code(&mut self) -> i32 {
        let start = Instant::now();
        loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                return status.code().unwrap_or(-1);
            }
            if start.elapsed() > Duration::from_secs(5) {
                let _ = self.child.kill();
                panic!("still running 5 s after Ctrl+C:\n{}", self.text());
            }
            std::thread::sleep(Duration::from_millis(20));
        }
    }

    /// Ctrl+C at the prompt `prompt`: setup cancelled, exit 130, terminal back in cooked mode.
    fn ctrl_c_cancels(mut self, prompt: &str) -> String {
        self.expect(prompt);
        // A partial answer is dropped too.
        self.send(b"abc");
        std::thread::sleep(Duration::from_millis(100));
        self.send(b"\x03");
        let code = self.exit_code();
        // Let the reader catch the last bytes.
        std::thread::sleep(Duration::from_millis(100));
        let text = self.text();
        assert_eq!(code, 130, "{text}");
        assert!(text.contains("amp-harness: setup cancelled"), "{text}");
        assert_eq!(
            text.matches("setup cancelled").count(),
            1,
            "one line: {text}"
        );
        let mode = tcgetattr(&self.slave).expect("tcgetattr");
        assert!(
            mode.local_flags
                .contains(LocalFlags::ECHO | LocalFlags::ICANON | LocalFlags::ISIG),
            "terminal left in raw mode: {:?}",
            mode.local_flags
        );
        text
    }
}

fn nothing_written(dir: &Path) {
    assert!(
        !dir.join("home/.config/amp-harness/config.toml").exists(),
        "config.toml written"
    );
    assert!(!dir.join("project/.amp.json").exists(), ".amp.json written");
}

#[test]
fn ctrl_c_at_the_first_run_server_prompt_cancels() {
    let dir = workdir("ctrl-c-server", None);
    let pty = Pty::start(&dir, &[], &[]);
    let text = pty.ctrl_c_cancels("AMP server [");
    assert!(text.contains("abc"), "a visible prompt echoes: {text}");
    nothing_written(&dir);
}

#[test]
fn ctrl_c_at_the_project_picker_cancels() {
    let rt = tokio::runtime::Runtime::new().unwrap();
    let amp = rt.block_on(spawn(MockAmp::new(), None));
    let dir = workdir("ctrl-c-picker", None);
    let pty = Pty::start(&dir, &[], &[("AMP_MCP_URL", &amp.url)]);
    pty.ctrl_c_cancels("Project (number, #id, n, or Enter to skip)");
    nothing_written(&dir);
}

#[test]
fn ctrl_c_at_the_hidden_key_prompt_cancels_and_restores_echo() {
    let dir = workdir("ctrl-c-key", None);
    let pty = Pty::start(&dir, &["connect", "openrouter"], &[]);
    let text = pty.ctrl_c_cancels("OpenRouter API key (openrouter.ai/keys) (hidden): ");
    assert!(!text.contains("abc"), "the hidden prompt echoed: {text}");
    nothing_written(&dir);
}
