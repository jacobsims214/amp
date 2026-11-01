//! #3004: the session ownership lock across *processes*, which is the whole point of taking an OS
//! advisory lock. An in-process unit test can only show that two handles conflict; these spawn a
//! second copy of this test binary that really holds a session, and prove:
//!
//! - a second process is refused while the first owns the session, and the refusal names the live
//!   holder (the pid the sidecar records, read across processes);
//! - the OS releases the lock when the holder is killed outright (SIGKILL), so a crashed run's
//!   session is claimable straight away — with no cleanup step, no timestamp and no stale-lock
//!   sweep, and without the sidecar ever being deleted.
//!
//! The child is this binary, run with `--exact` on [`holds_a_session_until_told_to_stop`], which is
//! `#[ignore]`d so an ordinary `cargo test` never runs it as a test. It is told where the session
//! is and how to say it is ready through the environment.

use std::{
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    time::{Duration, Instant},
};

use amp_core::session_lock::{HolderStamp, SessionBusy, claim_session_lock_as, lock_path};

/// The child's session, and the two files it talks to us through.
const SESSION: &str = "AMP_SESSION_LOCK_TEST_SESSION";
const READY: &str = "AMP_SESSION_LOCK_TEST_READY";
const STOP: &str = "AMP_SESSION_LOCK_TEST_STOP";
/// The name of the child test, run with `--exact`.
const CHILD_TEST: &str = "holds_a_session_until_told_to_stop";
/// How long any single wait may take. Every wait in this file is bounded: a test that hangs is a
/// test nobody sees fail.
const LIMIT: Duration = Duration::from_secs(20);

fn workdir(name: &str) -> PathBuf {
    // D13: never /tmp; cargo gives integration tests CARGO_TARGET_TMPDIR.
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-core-session-lock-process")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::canonicalize(&dir).unwrap()
}

/// A session file in `dir` (its content does not matter: a lock protects a name, not bytes).
fn session(dir: &Path) -> PathBuf {
    let path = dir.join("sessions/amp-manager/1767225600000-sess-1.jsonl");
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(&path, "{\"kind\":\"header\",\"version\":4}\n").unwrap();
    path
}

/// A child process owning a session until it is told to stop.
struct Holder {
    child: Child,
    ready: PathBuf,
    stop: PathBuf,
    log: PathBuf,
    pid: u32,
}

impl Holder {
    /// Spawns the child on `session_path` as `agent_id` and waits until it holds the lock.
    fn spawn(dir: &Path, session_path: &Path, agent_id: &str) -> Self {
        let ready = dir.join("ready");
        let stop = dir.join("stop");
        // The child's own output goes to a file: the test harness's chatter would otherwise land in
        // this test's output. It is shown only if the child fails.
        let log = dir.join("child.log");
        let out = std::fs::File::create(&log).unwrap();
        let child = Command::new(std::env::current_exe().unwrap())
            // `--include-ignored`: the child test is `#[ignore]`d so `cargo test` skips it.
            .args([
                CHILD_TEST,
                "--exact",
                "--include-ignored",
                "--nocapture",
                "--test-threads=1",
            ])
            .env(SESSION, session_path)
            .env(READY, &ready)
            .env(STOP, &stop)
            .env("AMP_SESSION_LOCK_TEST_AGENT", agent_id)
            .stdout(Stdio::from(out.try_clone().unwrap()))
            .stderr(Stdio::from(out))
            .spawn()
            .expect("spawn the holding process");
        let pid = child.id();
        let mut holder = Self {
            child,
            ready,
            stop,
            log,
            pid,
        };
        // The child writes `ready` only after its claim returned Ok: now it really owns the file.
        let started = Instant::now();
        while !holder.ready.exists() {
            if let Some(status) = holder.child.try_wait().expect("poll the child") {
                panic!(
                    "the holding process exited early ({status}) without taking the session:\n{}",
                    holder.log_text()
                );
            }
            assert!(
                started.elapsed() < LIMIT,
                "timed out after {LIMIT:?} waiting for the child to take the session"
            );
            std::thread::sleep(Duration::from_millis(25));
        }
        holder
    }

    /// The child's own output, for a failure message.
    fn log_text(&self) -> String {
        std::fs::read_to_string(&self.log).unwrap_or_default()
    }

    /// Asks it to stop and waits for it to exit normally.
    fn stop(mut self) {
        std::fs::write(&self.stop, b"stop").unwrap();
        let status = self.child.wait().expect("wait for the child");
        assert!(
            status.success(),
            "the child exited with {status}:\n{}",
            self.log_text()
        );
    }

    /// Kills it outright: what a crashed or power-cut run looks like, with no chance to release
    /// the lock or tidy up.
    fn kill(mut self) {
        self.child.kill().expect("kill the child");
        self.child.wait().expect("reap the child");
    }
}

/// The child half: own the session until `stop` appears, then let the guard drop normally.
///
/// Ignored so `cargo test` never runs it as a test; [`Holder::spawn`] runs it by name.
#[test]
#[ignore = "spawned by the parent test in this file, never run on its own"]
fn holds_a_session_until_told_to_stop() {
    let session = PathBuf::from(std::env::var_os(SESSION).expect("session path"));
    let ready = PathBuf::from(std::env::var_os(READY).expect("ready file"));
    let stop = PathBuf::from(std::env::var_os(STOP).expect("stop file"));
    let agent = std::env::var("AMP_SESSION_LOCK_TEST_AGENT").unwrap_or_default();

    let guard = claim_session_lock_as(&session, &agent).expect("the child is the first writer");
    // Once this file exists the lock is held; the parent stops waiting on it.
    std::fs::write(&ready, std::process::id().to_string()).unwrap();

    // Bounded, so an abandoned child can never outlive the suite.
    let started = Instant::now();
    while !stop.exists() {
        assert!(
            started.elapsed() < LIMIT * 3,
            "the parent never asked this process to stop"
        );
        std::thread::sleep(Duration::from_millis(25));
    }
    drop(guard);
}

#[test]
fn a_second_process_is_refused_while_another_owns_the_session() {
    let dir = workdir("refused");
    let path = session(&dir);
    let holder = Holder::spawn(&dir, &path, "amp-manager");

    // The real cross-process case: this process is not the holder, and the OS says so.
    let refused = claim_session_lock_as(&path, "amp:amp-worker-backend").unwrap_err();
    assert!(
        matches!(refused, SessionBusy::Held { .. }),
        "a second process must be refused, not allowed to append: {refused:?}"
    );
    let stamp = refused
        .holder()
        .expect("the holding process's stamp, read across processes");
    assert_eq!(stamp.pid, holder.pid, "the stamp names the live process");
    assert_eq!(stamp.agent_id, "amp-manager");
    // The message a person sees names the holder and says what to do about it.
    let text = refused.to_string();
    assert!(text.contains("owned by another live process"), "{text}");
    assert!(text.contains("amp-manager"), "{text}");
    assert!(
        text.contains("stop that run or pick another session"),
        "{text}"
    );
    // The sidecar exists and is not the session itself.
    assert!(lock_path(&path).exists());
    assert_eq!(
        std::fs::read_to_string(&path).unwrap(),
        "{\"kind\":\"header\",\"version\":4}\n",
        "a refused writer never touched the session"
    );

    // The holder exits normally: the session is free, and the sidecar stayed put.
    holder.stop();
    let guard = claim_session_lock_as(&path, "amp:amp-worker-backend")
        .expect("a stopped holder releases its session");
    assert_eq!(
        HolderStamp::read(&path).map(|h| h.agent_id),
        Some("amp:amp-worker-backend".into())
    );
    drop(guard);
    assert!(lock_path(&path).exists(), "the lock inode is never removed");
}

#[test]
fn a_killed_holder_releases_the_session_with_no_cleanup() {
    let dir = workdir("killed");
    let path = session(&dir);
    let holder = Holder::spawn(&dir, &path, "amp-manager");
    assert!(matches!(
        claim_session_lock_as(&path, "other").unwrap_err(),
        SessionBusy::Held { .. }
    ));

    // SIGKILL: the process had no chance to release the lock or remove anything.
    holder.kill();
    assert!(
        lock_path(&path).exists(),
        "a crash leaves the sidecar behind, and it is still there afterwards"
    );

    // Claimable at once — no stale-lock sweep, no timestamp, no cleanup call. The kernel dropped
    // the lock when the holder died, and that is the whole test.
    let guard = claim_session_lock_as(&path, "amp:amp-worker-backend")
        .expect("a crashed run's session is claimable immediately");
    // The dead holder's stamp is replaced by the new one; nothing ever consulted it to decide.
    assert_eq!(
        guard.lock_path(),
        lock_path(&path),
        "one inode, still the same sidecar the killed process used"
    );
    assert_eq!(
        HolderStamp::read(&path).map(|h| h.pid),
        Some(std::process::id())
    );
    drop(guard);

    // And it is free again afterwards, so a crash costs nothing to recover from.
    let guard = claim_session_lock_as(&path, "amp-manager")
        .expect("free again after the second writer left");
    drop(guard);
}
