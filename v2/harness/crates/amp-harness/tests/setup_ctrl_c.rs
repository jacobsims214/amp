//! #2227: Ctrl+C at a setup prompt cancels setup ("setup cancelled", exit 130), nothing written,
//! and the terminal is left in cooked mode with echo on. Driven through a real pty. The child has
//! no controlling terminal, so ^C can only work as a key: exactly the case QA hit (a driver that
//! ignored SIGINT), where a cooked-mode prompt echoed ^C and waited forever.
//!
//! Every child runs in **its own session** (#3691), never in the test runner's:
//! a child that shares the runner's terminal keeps that terminal as its controlling terminal, and
//! setup's `terminal_width` opens `/dev/tty` to size its output, so from a background process
//! group (how a test runner is started under a shell that owns the terminal) the open stops the
//! *whole process group* with SIGTTOU/SIGTTIN. A stopped group runs no code at all: the 30 s
//! bound in [`Pty::expect`] cannot fire and the binary's own checks never run, which looked like
//! an infinite hang and cost the suite its whole time budget on macOS. `setsid` leaves the child
//! with no controlling terminal (`/dev/tty` opens ENXIO, crossterm falls back to the pty on
//! stdin/stdout), so the pty stays the only terminal it can see and ^C is still only ever a key
//! (#2227), as this file means to test.
//!
//! A session also makes an interrupted run harmless: [`Pty`] kills and reaps its child on drop, and
//! with no shared session a killed run hangs its child up instead of orphaning it.
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
    /// The terminal's device path. When the child led a session on it (every case since #3691, and
    /// `start_dev_tty` before that), macOS revokes every open handle as it exits (ENOTTY), so the
    /// mode is read through a fresh open of this path.
    slave_path: std::path::PathBuf,
    seen: Arc<Mutex<Vec<u8>>>,
}

/// The python3 shim that detaches the child before `exec`: `setsid` (its own session, so the
/// runner's terminal is not its controlling terminal) and, for [`Pty::start_dev_tty`], `TIOCSCTTY`
/// plus stdin from `/dev/tty`. Python does it because this crate forbids `unsafe`, so
/// `Command::pre_exec` is not available (the same shim as `common::pty_command`, #2906).
fn shim(dev_tty: bool) -> &'static str {
    if dev_tty {
        "import fcntl, os, sys, termios\n\
         os.setsid()\n\
         fcntl.ioctl(0, termios.TIOCSCTTY, 0)\n\
         os.execv('/bin/sh', ['sh', '-c', 'exec \"$0\" \"$@\" </dev/tty'] + sys.argv[1:])"
    } else {
        "import os, sys\n\
         os.setsid()\n\
         os.execv(sys.argv[1], sys.argv[1:])"
    }
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
        let slave_path = nix::unistd::ttyname(&pty.slave).expect("ttyname");
        // Never the runner's terminal: `setsid` before `exec` in both cases (#3691).
        let mut cmd = Command::new("python3");
        cmd.args(["-c", shim(dev_tty), bin]);
        cmd.args(args)
            .current_dir(dir.join("project"))
            .env("HOME", dir.join("home"))
            .env("XDG_CONFIG_HOME", dir.join("home/.config"))
            .env("XDG_STATE_HOME", dir.join("home/.local/state"))
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

    /// The terminal's mode. Through a fresh open of the device path when the test's own handle is
    /// gone: the child leads a session on it, so macOS revokes every handle held when it exits
    /// (ENOTTY).
    fn mode(&self) -> nix::sys::termios::Termios {
        tcgetattr(&self.slave)
            .or_else(|_| tcgetattr(File::open(&self.slave_path).expect("reopen the pty")))
            .expect("tcgetattr")
    }

    /// Kills and reaps the child, so no test path (a panic, [`Drop`]) leaves `amp-harness setup`
    /// running. Its own session has no controlling terminal to hang it up (#3691), so waiting is
    /// the process's end, not a race.
    fn kill(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
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
                self.kill();
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
        let mode = self.mode();
        assert!(
            mode.local_flags
                .contains(LocalFlags::ECHO | LocalFlags::ICANON | LocalFlags::ISIG),
            "terminal left in raw mode: {:?}",
            mode.local_flags
        );
        text
    }
}

impl Drop for Pty {
    /// An interrupted test (a panic, a killed run) must not leave `amp-harness setup` running.
    fn drop(&mut self) {
        self.kill();
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
