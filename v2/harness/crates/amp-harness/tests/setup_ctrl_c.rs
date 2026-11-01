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
    /// The terminal's device path. When the child led a session on it (`start_dev_tty`), macOS
    /// revokes every open handle as it exits (ENOTTY), so the mode is read through a fresh open.
    slave_path: Option<std::path::PathBuf>,
    seen: Arc<Mutex<Vec<u8>>>,
}

impl Pty {
    fn start(dir: &Path, args: &[&str], env: &[(&str, &str)]) -> Self {
        Self::spawn(dir, args, env, false)
    }

    /// Like [`Pty::start`], but the pty is the child's controlling terminal and its stdin is
    /// opened from `/dev/tty`, as install.sh hands off to setup (`setup </dev/tty`, #2330). On
    /// macOS kqueue can't poll that fd, so this is the case a plain pty stdin can't catch.
    fn start_dev_tty(dir: &Path, args: &[&str]) -> Self {
        Self::spawn(dir, args, &[], true)
    }

    fn spawn(dir: &Path, args: &[&str], env: &[(&str, &str)], dev_tty: bool) -> Self {
        let pty = openpty(None, None).expect("openpty");
        let stdio = || Stdio::from(pty.slave.try_clone().unwrap());
        let bin = env!("CARGO_BIN_EXE_amp-harness");
        let slave_path = dev_tty.then(|| nix::unistd::ttyname(&pty.slave).expect("ttyname"));
        let mut cmd = if dev_tty {
            // A new session whose controlling terminal is the pty (stdin is the slave here), so
            // /dev/tty is it; then `exec BIN ARGS </dev/tty`. Python does the setsid + TIOCSCTTY
            // because this crate forbids `unsafe` (Command::pre_exec).
            let mut py = Command::new("python3");
            py.args([
                "-c",
                "import fcntl, os, sys, termios\n\
                 os.setsid()\n\
                 fcntl.ioctl(0, termios.TIOCSCTTY, 0)\n\
                 os.execv('/bin/sh', ['sh', '-c', 'exec \"$0\" \"$@\" </dev/tty'] + sys.argv[1:])",
                bin,
            ]);
            py
        } else {
            Command::new(bin)
        };
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
            slave_path,
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

    /// Types `bytes`. A child that already exited (EIO) shows up in the exit-code check, with
    /// what it printed.
    fn send(&mut self, bytes: &[u8]) {
        let _ = self.master.write_all(bytes);
        let _ = self.master.flush();
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
    fn ctrl_c_cancels(self, prompt: &str) -> String {
        self.ctrl_c_cancels_command(prompt, "setup")
    }

    /// [`Pty::ctrl_c_cancels`] for `command` ("connect cancelled", #2340).
    fn ctrl_c_cancels_command(mut self, prompt: &str, command: &str) -> String {
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
        let said = format!("{command} cancelled");
        assert!(text.contains(&format!("amp-harness: {said}")), "{text}");
        assert_eq!(text.matches(&said).count(), 1, "one line: {text}");
        assert_eq!(
            text.matches(" cancelled").count(),
            1,
            "only `{said}`: {text}"
        );
        let mode = match &self.slave_path {
            Some(path) => tcgetattr(File::open(path).expect("reopen the pty")),
            None => tcgetattr(&self.slave),
        }
        .expect("tcgetattr");
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

/// #2330: install.sh runs `setup </dev/tty` (stdin may be the `curl | sh` pipe). On macOS the key
/// reader failed at once with "read key: Failed to initialize input reader". The prompt must
/// take typed input, and Ctrl+C must still cancel.
#[test]
fn the_server_prompt_reads_keys_from_dev_tty() {
    let dir = workdir("dev-tty-server", None);
    let pty = Pty::start_dev_tty(&dir, &["setup"]);
    let text = pty.ctrl_c_cancels("AMP server [");
    assert!(text.contains("abc"), "typed input echoes: {text}");
    assert!(!text.contains("read key"), "{text}");
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

/// #2340: Ctrl+C in `connect`'s prompts said "setup cancelled". (Its hidden key prompt needs a
/// keychain since #2340, so the picker is the prompt here; the real-keychain cases are in
/// tests/keychain.rs.)
#[test]
fn ctrl_c_in_connect_says_connect_cancelled() {
    let dir = workdir("ctrl-c-connect", None);
    let pty = Pty::start(&dir, &["connect"], &[]);
    let text = pty.ctrl_c_cancels_command("Provider [", "connect");
    assert!(text.contains("abc"), "a visible prompt echoes: {text}");
    assert!(!text.contains("setup"), "{text}");
    nothing_written(&dir);
}

/// #2340: in a terminal with the keychain off, `connect` asks for no key it could not keep: it
/// names the variable to set instead, before any hidden prompt.
#[test]
fn connect_in_a_terminal_without_a_keychain_asks_for_no_key() {
    let dir = workdir("pty-connect-no-keychain", None);
    let mut pty = Pty::start(&dir, &["connect", "openrouter"], &[]);
    let code = pty.exit_code();
    std::thread::sleep(Duration::from_millis(100));
    let text = pty.text();
    assert_eq!(code, 2, "{text}");
    assert!(!text.contains("(hidden)"), "{text}");
    assert!(
        text.contains("set OPENROUTER_API_KEY in your environment"),
        "{text}"
    );
    nothing_written(&dir);
}
