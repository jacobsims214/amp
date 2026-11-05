//! Who owns a saved session file: one live writer per session, refused rather than waited for
//! (AMP #3004, story 454).
//!
//! `discoveries/harness-local-session-storage.md` §10 is the design this module implements. Its
//! problem: a session is one JSONL file appended to by whoever opened it, and nothing today says
//! one process owns it. Two harness processes can open the same file and append concurrently
//! (§7), and a restore repairs a file in place (torn-tail and duplicate-seq republishing through
//! tmp+rename, §6), which can drop a live writer's appends. Neither is acceptable once `/sessions`
//! can load an existing session and continue it.
//!
//! The contract:
//!
//! - **One live writer per session file.** Before the first byte is written to a session — create,
//!   append, or repair — the writer takes that file's ownership lock and holds the guard for as
//!   long as it can write (§10.3: releasing it between `load` and continued appends would reopen
//!   the race). Reading never locks: the `/sessions` listing and previews take no lock and never
//!   repair ([`crate::session_index`]), so a listing is instant and deadlock-free.
//! - **Refuse, don't block.** One non-blocking `try_lock` and nothing else: the rpi contract is
//!   that a second process is refused (§10.1), and a session lock is held for a whole run, so
//!   waiting for it would be unbounded. The refusal says who holds it and what to do.
//! - **Per file, not per directory.** One directory holds many independent sessions (a new file
//!   per run, and forks add more); a per-directory lock would serialize unrelated sessions. The
//!   lock is the sidecar [`lock_path`] next to the real file, reached through symlinks, and it is
//!   **never deleted** — removing it would let two writers lock two different inodes
//!   (`setup/config_file.rs`'s `lock_config` states the same rule).
//! - **The OS lock is the only authority.** It is released when the holder dies, however it dies,
//!   so a sidecar left behind by a crashed run is simply claimable and needs no cleanup. Timestamps
//!   and process ids are *never* consulted to decide whether a session is free
//!   ([`dispatch_ledger`](crate::dispatch_ledger) uses the same testing idiom for its liveness
//!   locks). The [`HolderStamp`] written into the sidecar at acquire is a diagnostic for the
//!   refusal message only: a pid can be reused, so it is worded as "last claimed by", not as proof.
//! - **Fail closed.** Any failure to take the lock — including an I/O error opening the sidecar —
//!   is a refusal ([`SessionBusy`]), because writing without proven exclusivity is the bug this
//!   module exists to prevent.
//!
//! Precedent in this crate: [`crate::dispatch_ledger`] (a per-process `live-<pid>.lock` whose
//! `try_lock` is the liveness test) and `amp_harness::setup::config_file::lock_config` (a
//! sidecar lock that is left in place). Unlike the ledger's, this lock is held for the length of a
//! run, so it never waits. Its unit tests run in-process because `flock` is per open file
//! description, so two handles conflict within one process too; cross-process refusal, and release
//! when the holder is killed, live in `tests/session_lock.rs`.

use std::{
    ffi::OsString,
    fs::{File, OpenOptions, TryLockError},
    io::{self, Write},
    path::{Path, PathBuf},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use serde::{Deserialize, Serialize};

/// What a session's ownership sidecar adds to the session file's name: `…-<id>.jsonl.lock`.
///
/// A sidecar is not a session: [`crate::session_index`] and
/// [`crate::worker_session::latest_session`] both skip it, because it does not end in `.jsonl`.
pub const LOCK_SUFFIX: &str = ".lock";

/// The file a session's ownership lock lives in: the session's own path with [`LOCK_SUFFIX`]
/// appended, next to the *real* file.
///
/// `session_path` is resolved through symlinks first ([`crate::policy::resolve_symlinks`]: the
/// longest existing ancestor canonicalized, `..` collapsed), so every spelling of one session — the
/// real path, a link to the file, a path through a linked directory — reaches one sidecar and
/// therefore one lock. `setup/config_file.rs`'s `write_target` resolves links for the same reason;
/// this crate's resolver is used because it also follows a link whose target does not exist yet,
/// which a session being created can have.
///
/// The session file itself need not exist: a writer owns a session *before* it creates it.
pub fn lock_path(session_path: &Path) -> PathBuf {
    let target = crate::policy::resolve_symlinks(session_path);
    let name = match target.file_name() {
        Some(name) => {
            let mut name = name.to_os_string();
            name.push(LOCK_SUFFIX);
            name
        }
        // A path with no file name (`/`, `..`) has no sidecar beside it; this names one that the
        // open in [`claim_session_lock`] then fails on, which is a refusal, not a silent success.
        None => OsString::from(format!("session{LOCK_SUFFIX}")),
    };
    target.with_file_name(name)
}

/// A session's ownership lock, held until it is dropped.
///
/// Dropping it releases the lock, and so does the process ending (the OS drops advisory locks when
/// the last handle to the file closes, however the process died). It is a guard: the session is
/// owned only while it is alive, so keep it next to the writer for the writer's whole lifetime —
/// for a resumed session that means across `load` (whose repair must run under the lock) and every
/// append after it. Creating a new session file is exclusive by its new name, but its writer takes
/// the lock all the same, so the rule is uniform.
///
/// The sidecar it locked is deliberately left on disk after the guard is dropped: it is the inode
/// two later writers must agree on.
#[derive(Debug)]
#[must_use = "the session is owned only while the guard is held"]
pub struct SessionLock {
    session: PathBuf,
    lock: PathBuf,
    /// The locked handle. Its lock is released **explicitly** on drop ([`SessionLock::release`]),
    /// never merely by closing it — see that method for why that distinction is load-bearing.
    file: File,
}

impl SessionLock {
    /// Drops the lock before the handle is closed.
    ///
    /// Closing the file is not enough here, and the reason is `fork`. A `flock` belongs to the
    /// open file description, and `fork` shares open file descriptions with the child: if any
    /// thread of this process spawns a child (`git`, `sh`, a hook — the test suite does it
    /// constantly) while this guard is alive, that child holds a duplicate of *this* description.
    /// Closing our handle only drops our reference; the lock survives in the child until it
    /// `exec`s and `CLOEXEC` closes the duplicate. So a dropped guard can stay "held" for the
    /// length of that window, and the next claim of the session is refused — as a *busy* session
    /// owned by a process that already let it go. That is exactly the cross-module flake in AMP
    /// #3220.
    ///
    /// An explicit `LOCK_UN` acts on the description itself, so it releases the lock for every
    /// duplicate at once, whatever the children are doing.
    ///
    /// Failure is ignored: `drop` cannot report it, and the only way `unlock` fails is the lock
    /// not being held, which leaves the state this guard promised (nothing of ours is locked).
    fn release(&mut self) {
        let _ = self.file.unlock();
    }

    /// The session file this owns, as the caller named it (not resolved through symlinks).
    pub fn session(&self) -> &Path {
        &self.session
    }

    /// The sidecar that carries the lock ([`lock_path`] of [`SessionLock::session`]).
    pub fn lock_path(&self) -> &Path {
        &self.lock
    }
}

impl Drop for SessionLock {
    fn drop(&mut self) {
        self.release();
    }
}

/// Why a session could not be owned. Both variants mean the same thing to a caller: **nothing may
/// be written**, and a person was told why rather than left waiting.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SessionBusy {
    /// Another live process holds the session's lock: its transcript is being written right now.
    Held {
        /// The session, as the caller named it.
        session: PathBuf,
        /// Its sidecar ([`lock_path`]).
        lock: PathBuf,
        /// Who the sidecar says last claimed it — diagnostics only, see [`HolderStamp`].
        holder: Option<HolderStamp>,
    },
    /// The lock could not be taken at all: its sidecar could not be opened or the OS refused the
    /// lock. Failing closed, because writing without proven exclusivity is what this module
    /// prevents. The session's directory must exist (a writer creates it before claiming).
    Unavailable {
        /// The session, as the caller named it.
        session: PathBuf,
        /// Its sidecar ([`lock_path`]).
        lock: PathBuf,
        /// What went wrong, for the person reading the message.
        error: String,
    },
}

impl SessionBusy {
    /// The session that could not be owned, as the caller named it.
    pub fn session(&self) -> &Path {
        match self {
            Self::Held { session, .. } | Self::Unavailable { session, .. } => session,
        }
    }

    /// Its ownership sidecar.
    pub fn lock_path(&self) -> &Path {
        match self {
            Self::Held { lock, .. } | Self::Unavailable { lock, .. } => lock,
        }
    }

    /// The holder the sidecar named, when one could be read ([`SessionBusy::Held`] only).
    pub fn holder(&self) -> Option<&HolderStamp> {
        match self {
            Self::Held { holder, .. } => holder.as_ref(),
            Self::Unavailable { .. } => None,
        }
    }
}

impl std::fmt::Display for SessionBusy {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Held {
                session,
                lock,
                holder,
            } => {
                write!(
                    f,
                    "session {} is owned by another live process",
                    session.display()
                )?;
                match holder {
                    Some(holder) => write!(
                        f,
                        ": last claimed by {} started {}",
                        holder.who(),
                        elapsed(holder.at(), SystemTime::now())
                    )?,
                    None => write!(f, " (its lock {} carries no holder stamp)", lock.display())?,
                }
                write!(f, "; stop that run or pick another session")
            }
            Self::Unavailable {
                session,
                lock,
                error,
            } => write!(
                f,
                "session {}: cannot take its ownership lock {}: {error}; nothing was written",
                session.display(),
                lock.display()
            ),
        }
    }
}

impl std::error::Error for SessionBusy {}

/// Who took a session's lock, as the sidecar records it.
///
/// **Diagnostics only.** The lock itself is the authority on whether a session is free; this stamp
/// exists so a refusal can say who is on the session and what to do about it. It is written best
/// effort on acquire, so it can be missing (an old sidecar, a failed write) or name a process that
/// has since exited or whose pid was reused — the message says "last claimed by" for that reason.
/// Nothing in this module ever decides ownership from it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct HolderStamp {
    /// The process that claimed the session.
    pub pid: u32,
    /// The role or agent on the other end (`amp-manager`, `amp:amp-worker-backend`), when the
    /// caller named itself; empty when it didn't.
    #[serde(default)]
    pub agent_id: String,
    /// When it claimed (Unix milliseconds).
    pub at_ms: i64,
}

impl HolderStamp {
    /// A stamp for this process, claiming on behalf of `agent_id` (empty for none).
    pub fn now(agent_id: &str) -> Self {
        Self {
            pid: std::process::id(),
            agent_id: agent_id.to_owned(),
            at_ms: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map_or(0, |d| d.as_millis() as i64),
        }
    }

    /// When the claim was made (out of range: the epoch).
    pub fn at(&self) -> SystemTime {
        u64::try_from(self.at_ms)
            .ok()
            .and_then(|ms| UNIX_EPOCH.checked_add(Duration::from_millis(ms)))
            .unwrap_or(UNIX_EPOCH)
    }

    /// Who holds it, for the refusal message: `amp:amp-worker-backend pid 4321`, or just
    /// `pid 4321` when the caller named no agent.
    pub fn who(&self) -> String {
        if self.agent_id.is_empty() {
            format!("pid {}", self.pid)
        } else {
            format!("{} pid {}", self.agent_id, self.pid)
        }
    }

    /// The stamp on `session_path`'s sidecar, if one can be read: what a refusal would say.
    ///
    /// Reading it says nothing about whether the session is free — only [`claim_session_lock`]
    /// answers that — and it never takes the lock.
    pub fn read(session_path: &Path) -> Option<Self> {
        let text = std::fs::read_to_string(lock_path(session_path)).ok()?;
        serde_json::from_str(text.trim()).ok()
    }

    /// Records this stamp in the sidecar we have locked (best effort: it is diagnostics, so a
    /// failure must never fail the claim).
    fn record(&self, file: &File) -> io::Result<()> {
        let mut line = serde_json::to_string(self).map_err(io::Error::other)?;
        line.push('\n');
        // The sidecar is ours while we hold the lock, so rewriting it wholesale is safe; the
        // truncate keeps a longer previous stamp from trailing behind this one.
        file.set_len(0)?;
        let mut writer: &File = file;
        writer.write_all(line.as_bytes())
    }
}

/// Claims `session_path` for this process: the lock that every writer of a session takes.
///
/// One non-blocking attempt, never a wait. `Ok` returns the guard, which must be held for as long
/// as this process may write that session (see [`SessionLock`]). `Err(SessionBusy::Held)` means a
/// live process is writing it: refuse and tell the person. `Err(SessionBusy::Unavailable)` means
/// the lock could not be taken at all: also refuse (fail closed).
///
/// The session's directory must exist; claiming does not create it (a writer creates the directory
/// before it claims, exactly as [`crate::agent::jsonl_session`] does). Nothing else is required of
/// the session file: it need not exist yet.
pub fn claim_session_lock(session_path: &Path) -> Result<SessionLock, SessionBusy> {
    claim_session_lock_as(session_path, "")
}

/// [`claim_session_lock`] on behalf of `agent_id`, which the holder stamp records so a refusal can
/// name who is on the session (`amp-manager`, `amp:amp-worker-backend`, …).
pub fn claim_session_lock_as(
    session_path: &Path,
    agent_id: &str,
) -> Result<SessionLock, SessionBusy> {
    let lock = lock_path(session_path);
    let unavailable = |error: String| SessionBusy::Unavailable {
        session: session_path.to_path_buf(),
        lock: lock.clone(),
        error,
    };
    let mut options = OpenOptions::new();
    options.read(true).write(true).create(true).truncate(false);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let file = options
        .open(&lock)
        .map_err(|e| unavailable(e.to_string()))?;
    match file.try_lock() {
        Ok(()) => {
            let _ = HolderStamp::now(agent_id).record(&file);
            Ok(SessionLock {
                session: session_path.to_path_buf(),
                lock,
                file,
            })
        }
        // Someone else's live writer: the lock says so, and only the lock does.
        Err(TryLockError::WouldBlock) => Err(SessionBusy::Held {
            session: session_path.to_path_buf(),
            lock,
            holder: HolderStamp::read(session_path),
        }),
        Err(TryLockError::Error(e)) => Err(unavailable(e.to_string())),
    }
}

/// `just now`, `40s ago`, `2 min ago`, `3 h ago`, `2 d ago`: how long a claim has been held, for
/// the refusal message.
fn elapsed(then: SystemTime, now: SystemTime) -> String {
    let secs = now.duration_since(then).unwrap_or(Duration::ZERO).as_secs();
    match secs {
        0..5 => "just now".to_owned(),
        5..60 => format!("{secs}s ago"),
        60..3600 => format!("{} min ago", secs / 60),
        3600..86_400 => format!("{} h ago", secs / 3600),
        _ => format!("{} d ago", secs / 86_400),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmp(name: &str) -> PathBuf {
        // D13: never /tmp. cargo sets this for integration tests and benches; a unit test in the
        // library gets the same place through the env var, and a manual `cargo test` falls back to
        // the workspace's own target dir first.
        let base = std::env::var_os("CARGO_TARGET_TMPDIR")
            .map(PathBuf::from)
            .or_else(|| {
                std::env::var_os("CARGO_MANIFEST_DIR")
                    .map(|d| PathBuf::from(d).join("../../target").join("tmp"))
            })
            .unwrap_or_else(|| PathBuf::from("target/tmp"));
        let dir = base.join("amp-core-session-lock").join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        // [`lock_path`] canonicalizes, so compare against the canonical spelling of the root.
        std::fs::canonicalize(&dir).unwrap()
    }

    /// A session file name of the harness's own shape, in `dir`.
    fn session(dir: &Path) -> PathBuf {
        dir.join("1767225600000-sess-1.jsonl")
    }

    #[test]
    fn a_second_writer_is_refused_while_the_first_holds_the_session() {
        let dir = tmp("refused");
        let path = session(&dir);

        let first = claim_session_lock_as(&path, "amp-manager").unwrap();
        assert_eq!(first.session(), path);
        assert_eq!(first.lock_path(), path.with_extension("jsonl.lock"));

        let refused = claim_session_lock_as(&path, "amp:amp-worker-backend").unwrap_err();
        assert!(matches!(refused, SessionBusy::Held { .. }), "{refused:?}");
        assert_eq!(refused.session(), path);
        // The message names the holder (the live one, from the stamp the first claim wrote) and
        // says what to do. This is the whole point of the stamp.
        let text = refused.to_string();
        assert!(text.contains("owned by another live process"), "{text}");
        assert!(text.contains("amp-manager"), "{text}");
        assert!(
            text.contains(&format!("pid {}", std::process::id())),
            "{text}"
        );
        assert!(
            text.contains("stop that run or pick another session"),
            "{text}"
        );
        let holder = refused.holder().expect("the stamp the first claim wrote");
        assert_eq!(holder.agent_id, "amp-manager");
        assert_eq!(holder.pid, std::process::id());
        assert_eq!(
            holder.who(),
            format!("amp-manager pid {}", std::process::id())
        );

        assert!(first.lock_path().exists());
        drop(first);
        assert!(
            lock_path(&path).exists(),
            "the sidecar is left in place, never deleted: two later writers must agree on one inode"
        );
    }

    /// AMP #3220: dropping a guard must release the session **even while this process is
    /// spawning children**, because `fork` shares open file descriptions and therefore the
    /// `flock`. Releasing by closing the handle is not enough — the child's duplicate keeps the
    /// lock until it `exec`s, so the session stays refused as "busy" for a process that has
    /// already let it go. That is what made `cargo test -p amp-core --lib` fail at default
    /// parallelism: the git-backed `run_report` fixtures fork constantly while other modules'
    /// tests hold and drop session guards.
    ///
    /// The child here is `true`, spawned in a loop from another thread, which is the cheapest
    /// reproduction of the window. Without the explicit release in `Drop` this fails (measured
    /// on the pre-fix code: 11 of 2000 dropped guards were still locked).
    #[test]
    fn dropping_the_guard_releases_the_session_while_children_are_being_spawned() {
        use std::sync::{
            Arc,
            atomic::{AtomicBool, Ordering},
        };

        let dir = tmp("fork-window");
        let stop = Arc::new(AtomicBool::new(false));
        let spawning = {
            let stop = Arc::clone(&stop);
            std::thread::spawn(move || {
                while !stop.load(Ordering::Relaxed) {
                    // `output()` waits for the child, so the fork happens between our claims.
                    let _ = std::process::Command::new("true").output();
                }
            })
        };

        let mut still_held = Vec::new();
        for i in 0..500 {
            let path = dir.join(format!("1767225600000-sess-{i}.jsonl"));
            let guard = claim_session_lock(&path).expect("the first writer owns it");
            drop(guard);
            // The guard is gone, so nothing of ours may still be holding this session.
            match claim_session_lock(&path) {
                Ok(after) => drop(after),
                Err(SessionBusy::Held { .. }) => still_held.push(path),
                Err(other) => panic!("a claim after a release failed: {other}"),
            }
        }
        stop.store(true, Ordering::Relaxed);
        spawning.join().expect("the spawning thread");

        assert!(
            still_held.is_empty(),
            "a dropped guard left the session held (its lock outlived the close, so a forked \
             child kept it): {} of 500 were still refused, e.g. {:?}",
            still_held.len(),
            still_held.first()
        );
    }

    #[test]
    fn dropping_the_guard_releases_the_session_for_the_next_writer() {
        let dir = tmp("released");
        let path = session(&dir);
        for agent in ["amp-manager", "amp:amp-worker-backend", "amp-manager"] {
            let guard = claim_session_lock_as(&path, agent).unwrap();
            // While it is held, a second claim is refused.
            assert!(claim_session_lock(&path).is_err(), "{agent}");
            drop(guard);
        }
        // The sidecar outlived every guard and still guards the same inode: a claim now succeeds.
        assert!(lock_path(&path).exists());
        let _guard = claim_session_lock(&path).unwrap();
    }

    #[test]
    fn a_stale_sidecar_left_by_a_crashed_run_is_claimable() {
        let dir = tmp("stale");
        let path = session(&dir);
        // What a killed process leaves behind: a sidecar with its stamp, nobody holding the lock.
        let stale = HolderStamp {
            pid: 0,
            agent_id: "amp-manager".into(),
            at_ms: 1,
        };
        std::fs::write(
            lock_path(&path),
            serde_json::to_string(&stale).unwrap() + "\n",
        )
        .unwrap();
        // No cleanup step and no timestamp check: the OS says the lock is free, so it is.
        let guard = claim_session_lock_as(&path, "amp:amp-worker-backend").unwrap();
        assert_eq!(
            HolderStamp::read(&path).map(|h| h.agent_id),
            Some("amp:amp-worker-backend".into()),
            "the new holder's stamp replaces the stale one"
        );
        assert!(guard.lock_path().exists());
    }

    #[test]
    fn the_stamp_is_never_the_authority_on_ownership() {
        let dir = tmp("stamp");
        let path = session(&dir);
        let guard = claim_session_lock_as(&path, "amp-manager").unwrap();
        // A stamp saying nobody holds it, written behind the guard's back: the lock still refuses.
        std::fs::write(lock_path(&path), "{not a stamp").unwrap();
        let refused = claim_session_lock(&path).unwrap_err();
        // The garbled stamp cannot be read, so the message says so — but the refusal stands.
        assert_eq!(refused.holder(), None, "{refused:?}");
        assert!(refused.to_string().contains("no holder stamp"), "{refused}");
        drop(guard);

        // And the other way round: a sidecar naming a *live-looking* pid is claimable while
        // nobody holds the lock, because a pid is not evidence of anything.
        std::fs::write(
            lock_path(&path),
            serde_json::to_string(&HolderStamp {
                pid: std::process::id(),
                agent_id: "amp:amp-worker-backend".into(),
                at_ms: 1,
            })
            .unwrap(),
        )
        .unwrap();
        let _guard = claim_session_lock(&path).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn every_spelling_of_one_session_reaches_one_lock() {
        use std::os::unix::fs::symlink;

        let dir = tmp("symlinks");
        let real_dir = dir.join("sessions/amp-manager");
        std::fs::create_dir_all(&real_dir).unwrap();
        let real = session(&real_dir);
        std::fs::write(&real, "{\"kind\":\"header\"}\n").unwrap();

        // A link to the file and a link to its directory are the same session.
        let file_link = dir.join("linked.jsonl");
        symlink(&real, &file_link).unwrap();
        let dir_link = dir.join("linked-dir");
        symlink(&real_dir, &dir_link).unwrap();
        let through_dir = dir_link.join(real.file_name().unwrap());

        for spellings in [&file_link, &through_dir] {
            assert_eq!(
                lock_path(spellings),
                lock_path(&real),
                "{} resolves to the session's own sidecar",
                spellings.display()
            );
        }
        // The sidecar lands beside the real file, not beside the link.
        assert_eq!(
            lock_path(&file_link),
            real_dir.join("1767225600000-sess-1.jsonl.lock")
        );
        assert_ne!(
            lock_path(&file_link),
            file_link.with_file_name("linked.jsonl.lock"),
            "never beside the link itself"
        );

        // So a claim through one spelling refuses every other.
        let guard = claim_session_lock_as(&real, "amp-manager").unwrap();
        for spelling in [&file_link, &through_dir] {
            let refused = claim_session_lock(spelling).unwrap_err();
            assert!(matches!(refused, SessionBusy::Held { .. }), "{refused:?}");
        }
        drop(guard);
        // And after release, any spelling can take it.
        let _guard = claim_session_lock(&file_link).unwrap();
    }

    #[test]
    fn a_session_that_cannot_be_locked_fails_closed() {
        let dir = tmp("unavailable");
        let path = session(&dir);

        // A missing directory: nothing is created on the claim's behalf (a writer makes the
        // directory first), and no session is claimed without a lock.
        let missing = dir.join("no-such-dir/1-sess.jsonl");
        let refused = claim_session_lock(&missing).unwrap_err();
        assert!(
            matches!(refused, SessionBusy::Unavailable { .. }),
            "{refused:?}"
        );
        assert!(
            refused.to_string().contains("nothing was written"),
            "{refused}"
        );
        assert!(
            !dir.join("no-such-dir").exists(),
            "no directory was created"
        );
        assert!(!lock_path(&missing).exists(), "no sidecar was left behind");

        // A path that is not a directory at all (a file in the way): the open fails, refuse.
        let blocker = dir.join("a-file");
        std::fs::write(&blocker, "x").unwrap();
        let through_file = blocker.join("1-sess.jsonl");
        assert!(matches!(
            claim_session_lock(&through_file).unwrap_err(),
            SessionBusy::Unavailable { .. }
        ));

        // A sidecar that exists as a directory: the open fails, refuse.
        std::fs::create_dir_all(lock_path(&path)).unwrap();
        let refused = claim_session_lock(&path).unwrap_err();
        assert!(
            matches!(refused, SessionBusy::Unavailable { .. }),
            "{refused:?}"
        );
        assert!(refused.lock_path().exists());

        // Nothing above disturbed a session that can be claimed: the file need not exist yet.
        let guard = claim_session_lock(&path.with_file_name("2-sess.jsonl")).unwrap();
        assert!(!guard.session().exists());
    }

    /// A sidecar is not a session: the `/sessions` listing (which takes no lock, §10.2) neither
    /// lists it nor reads it, so owning a session cannot confuse discovery.
    #[test]
    fn a_sidecar_is_not_a_session() {
        let state = tmp("listing");
        let role = state.join("sessions/amp-manager");
        std::fs::create_dir_all(&role).unwrap();
        let path = role.join("1767225600000-sess-1.jsonl");
        let header = serde_json::json!({
            "kind": "header",
            "version": 4,
            "id": "sess-1",
            "createdAt": 1_767_225_600_000i64,
            "cwd": std::fs::canonicalize(&state).unwrap().to_string_lossy(),
        });
        std::fs::write(&path, format!("{header}\n")).unwrap();
        let _guard = claim_session_lock_as(&path, "amp-manager").unwrap();

        let listed = crate::session_index::list(&state.join("sessions"), &state).unwrap();
        assert_eq!(
            listed.iter().map(|e| e.id.as_str()).collect::<Vec<_>>(),
            vec!["sess-1"],
            "the session is listed; its sidecar is not"
        );
        // The worker transcript view's name test agrees (it looks for `*.jsonl`).
        assert_ne!(
            lock_path(&path).extension().and_then(|e| e.to_str()),
            Some("jsonl")
        );
    }

    #[test]
    fn the_lock_path_is_the_session_with_a_lock_suffix() {
        let dir = tmp("names");
        let path = session(&dir);
        assert_eq!(
            lock_path(&path),
            dir.join("1767225600000-sess-1.jsonl.lock")
        );
        // Resolution normalises an existing spelling, so `./` and `..` do not make a second lock.
        std::fs::write(&path, "{\"kind\":\"header\"}\n").unwrap();
        std::fs::create_dir_all(dir.join("sub")).unwrap();
        for spelling in [
            dir.join(".").join("1767225600000-sess-1.jsonl"),
            dir.join("sub/../1767225600000-sess-1.jsonl"),
        ] {
            assert_eq!(
                lock_path(&spelling),
                lock_path(&path),
                "{}",
                spelling.display()
            );
        }
    }

    #[test]
    fn a_refusal_ages_the_claim_in_words() {
        let now = UNIX_EPOCH + Duration::from_secs(10_000);
        let at = |secs| now - Duration::from_secs(secs);
        for (age, text) in [
            (0, "just now"),
            (40, "40s ago"),
            (120, "2 min ago"),
            (3 * 3600, "3 h ago"),
            (2 * 86_400, "2 d ago"),
        ] {
            assert_eq!(elapsed(at(age), now), text, "age {age}");
        }
        // A stamp from the future (a clock that went backwards) reads as just now, never negative.
        assert_eq!(elapsed(now + Duration::from_secs(60), now), "just now");
    }
}
