//! Owning the file a session writes (AMP #3006, story 454): the one way a saved session is opened
//! for writing, and the guard that travels with it.
//!
//! [`crate::session_lock`] (#3004) is the primitive: it takes a per-file OS lock and refuses a
//! second writer. It is only useful if every writer actually takes it, and this module is what
//! makes that structural instead of a rule someone has to remember. `discoveries/
//! harness-local-session-storage.md` §10 is the contract; §10.5's placement note is "every entry
//! point that appends to or repairs a session file takes the guard first".
//!
//! # The guard travels inside the storage, not beside it
//!
//! A `Session` is a thin `Arc<RwLock<Arc<dyn SessionStorage>>>` over its storage, so a storage
//! value *is* the thing that writes. [`LockedStorage`] is that storage with the [`SessionLock`]
//! inside it: it implements [`SessionStorage`] by delegating to the real one, and owns the guard
//! for exactly as long as it can be written. Every append, record, lane change and name update
//! goes through it, so "the file is owned while this process may write it" holds by construction
//! rather than by a comment.
//!
//! Keeping the guard in a field of the session's owner instead would be weaker in a way that is
//! easy to miss: a caller can clone the `Session` (or the `Arc<AgentHarness>`) and drop the
//! struct the guard lived in — the supervisor does exactly that with a worker's `BuiltAgent`,
//! keeping only `Arc::clone(&built.harness)` — and the lock would be released while the harness
//! goes on appending. Inside the storage there is no such gap: the last writer's storage holds the
//! last reference to its own lock.
//!
//! One file, one guard ([`create`] makes a new file and claims it; [`load`] claims a saved one).
//! Two sessions in one directory never contend: the lock is per file, not per directory (§10.2).
//!
//! # Where repair is allowed
//!
//! [`rpi_harness::session::jsonl::JsonlSessionStorage::load`] is not a pure read: it repairs a
//! torn tail and duplicate-sequence appends by republishing the file through tmp+rename, which
//! **replaces** it and can drop a concurrent writer's lines that landed after the read (§6, §10.3).
//! So [`load`] runs in this order, and the order is the safety argument:
//!
//! 1. **Validate without opening for repair** — [`crate::session_index::inspect`] reads only the
//!    name and a bounded head of the file, takes no lock and never writes. A file that is not one
//!    of this harness's sessions, or whose header is damaged, or was written in a format this
//!    harness does not read, is refused here, untouched.
//! 2. **Refuse a live writer before reading its file** — the claim comes before `load`, so another
//!    process's session is never opened for repair at all.
//! 3. **Load under the guard** — the repair, if any, runs while this process is the only writer
//!    (§10.3). The guard is then held for every append that follows, by [`LockedStorage`].
//!
//! A failure at any step drops the guard it took and leaves whatever session was active alone:
//! nothing is swapped in until the loaded storage is in hand ([`crate::agent::resume_session`]).
//!
//! # What a resumed session is not
//!
//! It continues a *conversation file*. It never records a dispatch, never re-runs a recorded tool
//! call (`ToolReplay::Never` is rpi's default and nothing here widens it), and never continues an
//! interrupted request: `AgentHarness::set_session` clears `pending_resume`, so a resumed session
//! waits for the next prompt instead of re-issuing the one a dead run was in the middle of (§10.4,
//! §6.4). The caller's own state (model snapshot, run numbering, transcript) is the caller's to
//! reset — that is #3012, not this module.
//!
//! # Reads stay lock-free
//!
//! [`crate::session_index`] and [`crate::worker_session::replay`] take no lock and never repair, so
//! a listing is instant and cannot deadlock against a live run (§10.2). Only access that may write
//! needs the guard, and this module is the only door to it in [`crate::agent`].

use std::{
    path::{Path, PathBuf},
    sync::Arc,
    time::{SystemTime, UNIX_EPOCH},
};

use async_trait::async_trait;
use rpi_harness::{
    error::SessionResult,
    session::{
        DefaultIdGenerator, Session, SystemClock,
        jsonl::{JsonlV4Header, storage::JsonlSessionStorage},
        types::{
            BranchBounds, Entry, EntryQuery, IdGenerator, LanePointer, LaneRecord, LogItem,
            LogOptions, OperationStartedRecord, ProvisionedEntry, RecordQuery, SessionMetadata,
            SessionStats, SessionStorage,
        },
    },
};
use rpi_tools::OsExecutionEnv;

use crate::{
    session_index::{self, EntryStatus},
    session_lock::{self, SessionBusy, SessionLock},
};

/// A session storage that owns its file: the [`SessionLock`] is held for as long as this storage
/// can be written, because this storage is what writes.
///
/// Every method delegates to the storage it wraps; the wrapper adds no behaviour to the data. It
/// makes the ownership contract structural: whoever holds the `Session` holds the guard, and the
/// guard is released only when the last clone of the `Session` — and so the last possible writer —
/// is gone. See the module docs for why the guard lives here rather than beside the writer.
pub struct LockedStorage {
    /// Never read: holding it *is* the point. Dropping it releases the OS lock.
    _lock: SessionLock,
    inner: Arc<dyn SessionStorage>,
}

impl LockedStorage {
    /// Wraps `inner`, which writes `lock`'s session file, in `lock`.
    pub fn new(lock: SessionLock, inner: Arc<dyn SessionStorage>) -> Self {
        Self { _lock: lock, inner }
    }
}

#[async_trait]
impl SessionStorage for LockedStorage {
    fn metadata(&self) -> SessionMetadata {
        self.inner.metadata()
    }

    async fn get_metadata(&self) -> SessionMetadata {
        self.inner.get_metadata().await
    }

    async fn get_lanes(&self) -> SessionResult<Vec<LanePointer>> {
        self.inner.get_lanes().await
    }

    async fn create_lane(&self, lane: &str, at: Option<&str>) -> SessionResult<()> {
        self.inner.create_lane(lane, at).await
    }

    async fn move_lane(&self, lane: &str, to: Option<&str>) -> SessionResult<()> {
        self.inner.move_lane(lane, to).await
    }

    async fn append_entry(&self, entry: ProvisionedEntry, lane: &str) -> SessionResult<Entry> {
        self.inner.append_entry(entry, lane).await
    }

    async fn append_record(&self, record: LaneRecord) -> SessionResult<LaneRecord> {
        self.inner.append_record(record).await
    }

    async fn get_entry(&self, id: &str) -> SessionResult<Option<Entry>> {
        self.inner.get_entry(id).await
    }

    async fn find_entries(&self, query: &EntryQuery) -> SessionResult<Vec<Entry>> {
        self.inner.find_entries(query).await
    }

    async fn find_entries_on_branch(
        &self,
        query: &EntryQuery,
        bounds: &BranchBounds,
        start: &str,
    ) -> SessionResult<Vec<Entry>> {
        self.inner
            .find_entries_on_branch(query, bounds, start)
            .await
    }

    async fn find_records(&self, query: &RecordQuery) -> SessionResult<Vec<LaneRecord>> {
        self.inner.find_records(query).await
    }

    async fn find_open_operations(
        &self,
        lane: &str,
        limit: Option<usize>,
    ) -> SessionResult<Vec<OperationStartedRecord>> {
        self.inner.find_open_operations(lane, limit).await
    }

    async fn get_log(&self, options: &LogOptions) -> SessionResult<Vec<LogItem>> {
        self.inner.get_log(options).await
    }

    async fn get_name(&self) -> SessionResult<Option<String>> {
        self.inner.get_name().await
    }

    async fn set_name(&self, name: Option<&str>) -> SessionResult<()> {
        self.inner.set_name(name).await
    }

    async fn get_label(&self, id: &str) -> SessionResult<Option<String>> {
        self.inner.get_label(id).await
    }

    async fn set_label(&self, id: &str, label: Option<&str>) -> SessionResult<()> {
        self.inner.set_label(id, label).await
    }

    async fn get_stats(&self) -> SessionResult<SessionStats> {
        self.inner.get_stats().await
    }
}

/// A session opened for writing, with its file owned by this process until the session is dropped.
///
/// There is deliberately no accessor for the guard: it is handed to [`LockedStorage`] and travels
/// with the session. A caller that wants the file to stay owned must keep the session (or the
/// harness driving it) alive — which is the same thing as "may still write it".
pub struct OpenedSession {
    /// Ready to drive (append, `AgentHarness::create`/`set_session`).
    pub session: Session,
    /// The file it writes.
    pub path: PathBuf,
}

impl std::fmt::Debug for OpenedSession {
    /// `Session` is an opaque facade over its storage, so a session in a test failure message is
    /// just the file that identifies it.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("OpenedSession")
            .field("path", &self.path)
            .finish_non_exhaustive()
    }
}

/// Why a session could not be opened for writing. Every variant says what to fix, and none of them
/// leaves a repair, a header or an append behind.
#[derive(Debug, thiserror::Error)]
pub enum OpenError {
    /// The session's directory does not exist and could not be created.
    #[error("session directory {}: {why}", dir.display())]
    Directory {
        /// The directory that could not be created.
        dir: PathBuf,
        /// What the filesystem said.
        why: String,
    },
    /// Another live writer owns the file — possibly this very process (resuming the session it is
    /// already writing). Nothing was opened and nothing was written.
    #[error("{}", held_message(.0))]
    Busy(SessionBusy),
    /// Not one of this harness's session files (its name is not `<created ms>-<session id>.jsonl`),
    /// so nothing can say which session it is.
    #[error("{} is not a session file: {why}", path.display())]
    NotASession {
        /// The file the caller named.
        path: PathBuf,
        /// What about the name, from [`crate::session_index::inspect`].
        why: String,
    },
    /// The session's header cannot be read: a torn first line, or damage from before its first
    /// append completed. It cannot be resumed, and nothing past the header is read.
    #[error("{} cannot be resumed: {why}", path.display())]
    Unreadable {
        /// The session file.
        path: PathBuf,
        /// What the header scan found.
        why: String,
    },
    /// The session was written in a format this harness does not read (v3, or a version other than
    /// 4). Loading it would need a converter that does not exist yet.
    #[error("{} cannot be resumed: {why}", path.display())]
    Incompatible {
        /// The session file.
        path: PathBuf,
        /// What the header scan found.
        why: String,
    },
    /// The session was started in another working directory. A harness's tools, policy and system
    /// prompt belong to *its* directory, so continuing this conversation here would append to a
    /// session whose transcript claims a directory its tools never ran in.
    #[error(
        "{} was started in {saved}, not {cwd}: resume it from there (the harness's tools and \
         policy belong to the directory it runs in)",
        path.display()
    )]
    OtherDirectory {
        /// The session file.
        path: PathBuf,
        /// The canonical directory the session was started in.
        saved: String,
        /// The canonical directory asked for.
        cwd: String,
    },
    /// The file could not be read, replayed or created: I/O trouble, or a transcript damaged
    /// somewhere `load` will not repair (a syntax error before the last line). The file is left
    /// exactly as it was — `load` repairs only what it can publish safely.
    #[error("{}: {why}", path.display())]
    Load {
        /// The session file.
        path: PathBuf,
        /// What the storage said.
        why: String,
    },
}

impl OpenError {
    /// Whether this process already owns the session it was refused: **true** when the refusal
    /// means "the session you asked to resume is the one you have open".
    ///
    /// The lock is the only authority on ownership ([`crate::session_lock`]), so this reads it the
    /// same way: a `try_lock` that fails while the holder stamp names *this* pid cannot be another
    /// process (a leftover sidecar naming a live pid is claimable, and our own pid is ours alone).
    /// It is a wording decision for the message, never a second opinion about ownership.
    pub fn held_by_this_process(&self) -> bool {
        match self {
            Self::Busy(busy) => busy
                .holder()
                .is_some_and(|holder| holder.pid == std::process::id()),
            _ => false,
        }
    }
}

/// The person-facing text for a refusal: the lock's own message normally, and a plainer one for
/// the case where the "other" writer is this process (resuming the session it is writing).
fn held_message(busy: &SessionBusy) -> String {
    let mine = busy
        .holder()
        .is_some_and(|holder| holder.pid == std::process::id());
    if !mine {
        return busy.to_string();
    }
    format!(
        "session {} is already open in this process: it is the session being written right now. \
         Switch to a different session, or continue this one where you are.",
        busy.session().display()
    )
}

/// A new session file in `dir`, named `<unix ms>-<session id>.jsonl`, claimed **before** the header
/// is written and owned for as long as the returned session can be written.
///
/// `cwd` is recorded in the header **canonical**, the form [`crate::session_index::list`] and
/// [`load`] compare against; a session written from a symlinked or `.`-relative path is therefore
/// still listed and resumable under the directory it was started in, whatever the caller passed.
/// `agent_id` goes into the lock's holder stamp, so a refusal elsewhere can name who opened it
/// (`amp-manager`, `amp-worker-backend`, …); it is diagnostics only.
pub async fn create(dir: &Path, cwd: &Path, agent_id: &str) -> Result<OpenedSession, OpenError> {
    std::fs::create_dir_all(dir).map_err(|why| OpenError::Directory {
        dir: dir.to_path_buf(),
        why: why.to_string(),
    })?;
    let ids = Arc::new(DefaultIdGenerator::new());
    let id = IdGenerator::next(ids.as_ref());
    let created = now_ms();
    let path = dir.join(format!("{created}-{id}.jsonl"));
    // Before the first byte. A fresh name is exclusive only by inspection, and `create` writes the
    // header unconditionally; the rule is uniform for every writer (§10.5), so a second writer can
    // never find a half-written file it is allowed to repair.
    let lock = session_lock::claim_session_lock_as(&path, agent_id).map_err(OpenError::Busy)?;
    // Canonical, so `create` -> `list`/`load` always round-trips (see `canonical_cwd`): the header
    // is what a listing matches on and what `load` compares, and a caller passing a symlink or a
    // relative path must still produce a session it can find again.
    let header = JsonlV4Header::new(id, created, canonical_cwd(cwd), None, None, None);
    let fs = Arc::new(OsExecutionEnv::with_cwd(dir.to_path_buf()));
    let storage = JsonlSessionStorage::create(
        fs,
        &path.to_string_lossy(),
        header,
        Arc::new(SystemClock),
        ids.clone(),
    )
    .await
    .map_err(|e| OpenError::Load {
        path: path.clone(),
        why: e.to_string(),
    })?;
    let session = Session::new(
        Arc::new(LockedStorage::new(lock, Arc::new(storage))),
        Some(ids),
    );
    Ok(OpenedSession { session, path })
}

/// Opens the saved session at `path` for writing, in `cwd`: the load half of
/// [`crate::agent::jsonl_session`], for continuing a conversation instead of starting one.
///
/// Validation and the ownership claim both happen before the file is read for replay, so (a) a
/// damaged or foreign session is refused without being touched and (b) a session a live writer owns
/// is never opened for the repairing `load` (see the module docs). Everything this refuses, it
/// refuses with the file byte-identical.
///
/// `cwd` must be the canonical working directory the harness runs in; a session started elsewhere
/// is [`OpenError::OtherDirectory`]. `agent_id` is the holder stamp, diagnostics only.
pub async fn load(path: &Path, cwd: &Path, agent_id: &str) -> Result<OpenedSession, OpenError> {
    // 1. Read-only validation. `inspect` takes no lock and never writes: it reads the name and at
    //    most `session_index::HEAD_MAX_BYTES` from the start.
    let entry = session_index::inspect(path).map_err(|why| OpenError::NotASession {
        path: path.to_path_buf(),
        why,
    })?;
    match &entry.status {
        EntryStatus::Ok => {}
        EntryStatus::Unreadable(why) => {
            return Err(OpenError::Unreadable {
                path: path.to_path_buf(),
                why: why.clone(),
            });
        }
        EntryStatus::Incompatible(why) => {
            return Err(OpenError::Incompatible {
                path: path.to_path_buf(),
                why: why.clone(),
            });
        }
    }
    let want = canonical_cwd(cwd);
    // `inspect` fills `cwd` from the header it just parsed, so it is Some whenever the status is Ok.
    let saved = entry.cwd.clone().unwrap_or_default();
    if saved != want {
        return Err(OpenError::OtherDirectory {
            path: path.to_path_buf(),
            saved,
            cwd: want,
        });
    }

    // 2. The guard, before the file is read. Refusing here is what keeps a live writer's appends
    //    out of reach of `load`'s repairing republish.
    let lock = session_lock::claim_session_lock_as(path, agent_id).map_err(OpenError::Busy)?;

    // 3. Load — and repair, if the tail needs it — while this process is the only writer.
    let ids = Arc::new(DefaultIdGenerator::new());
    let dir = path.parent().unwrap_or(Path::new(".")).to_path_buf();
    let fs = Arc::new(OsExecutionEnv::with_cwd(dir));
    let storage = JsonlSessionStorage::load(
        fs,
        &path.to_string_lossy(),
        Arc::new(SystemClock),
        ids.clone(),
    )
    .await
    .map_err(|e| OpenError::Load {
        path: path.to_path_buf(),
        why: e.to_string(),
    })?;
    let session = Session::new(
        Arc::new(LockedStorage::new(lock, Arc::new(storage))),
        Some(ids),
    );
    Ok(OpenedSession {
        session,
        path: path.to_path_buf(),
    })
}

/// `cwd` in the form the session header records it: canonical, falling back to the path as given
/// when it cannot be canonicalized (a directory that has since been removed still has sessions
/// filed under the path it had). [`crate::session_index::list`] compares the same way, so a session
/// a listing shows under a directory is one this will resume.
fn canonical_cwd(cwd: &Path) -> String {
    cwd.canonicalize()
        .unwrap_or_else(|_| cwd.to_path_buf())
        .to_string_lossy()
        .into_owned()
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use rpi_agent::AgentMessage;
    use rpi_ai::types::{UserContent, UserMessage};
    use rpi_harness::{
        agent_harness::{AgentHarness, AgentLane},
        session::types::{Entry, EntryOrder, EntryQuery, LogOptions},
        types::AgentHarnessOptions,
    };

    use super::*;
    use crate::{
        agent::{ResumeError, memory_session, resume_session},
        session_lock::{SessionBusy, claim_session_lock, lock_path},
    };

    /// A directory of its own under cargo's target tmp dir (D13: never /tmp).
    fn tmpdir(name: &str) -> PathBuf {
        let base = std::env::var_os("CARGO_TARGET_TMPDIR")
            .map(PathBuf::from)
            .or_else(|| {
                std::env::var_os("CARGO_MANIFEST_DIR")
                    .map(|d| PathBuf::from(d).join("../../target").join("tmp"))
            })
            .unwrap_or_else(|| PathBuf::from("target/tmp"));
        let dir = base.join("amp-core-session-store").join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::canonicalize(&dir).unwrap()
    }

    fn user(text: &str) -> AgentMessage {
        AgentMessage::User(UserMessage {
            role: Default::default(),
            content: UserContent::Text(text.to_owned()),
            timestamp: 1_767_225_600_000,
        })
    }

    /// A session file in `dir` with one user message in it, and its path.
    async fn saved(dir: &Path, cwd: &Path, prompt: &str) -> PathBuf {
        let opened = create(dir, cwd, "amp-manager").await.expect("create");
        opened
            .session
            .append_message(user(prompt))
            .await
            .expect("append");
        opened.path
    }

    /// Whether this process can take `path`'s lock right now: a second `try_lock` fails while a
    /// guard this process holds is alive (`flock` is per open file description), which is how a
    /// test sees "still owned" without an accessor a caller could abuse.
    fn free(path: &Path) -> bool {
        match claim_session_lock(path) {
            Ok(guard) => {
                drop(guard);
                true
            }
            Err(SessionBusy::Held { .. }) => false,
            Err(other) => panic!("{other}"),
        }
    }

    fn content(message: &AgentMessage) -> Option<&str> {
        match message {
            AgentMessage::User(u) => u.content.as_text(),
            _ => None,
        }
    }

    async fn harness_on(session: Session) -> AgentHarness {
        AgentHarness::create(AgentHarnessOptions {
            session,
            ..Default::default()
        })
        .await
        .expect("harness")
    }

    // -- Creation -------------------------------------------------------------

    /// The whole point of the wrapper: the file is owned as long as the *storage* is alive, not as
    /// long as some Runtime or `BuiltAgent` is.
    #[tokio::test]
    async fn a_created_session_owns_its_file_until_the_storage_is_dropped() {
        let dir = tmpdir("created");
        let opened = create(&dir, &dir, "amp-manager").await.expect("create");
        assert!(opened.path.is_file(), "the header is on disk");
        assert!(
            !free(&opened.path),
            "a session this process created is owned by it"
        );
        assert!(
            lock_path(&opened.path).exists(),
            "the sidecar is created beside it"
        );
        // A clone of the session is another writer handle: ownership must survive it.
        let clone = opened.session.clone();
        drop(opened.session);
        assert!(
            !free(&opened.path),
            "a cloned session still writes the file"
        );
        drop(clone);
        assert!(free(&opened.path), "the last writer is gone");
    }

    /// A session created from a symlinked or relative path is filed under the directory it was
    /// really started in, so a listing finds it and `load` resumes it. Without this, such a session
    /// is write-only: created successfully, and unresumable forever after.
    #[cfg(unix)]
    #[tokio::test]
    async fn a_session_created_through_a_symlink_is_listed_and_resumable() {
        let dir = tmpdir("symlink");
        // The real layout: `<sessions_root>/<role>/<ms>-<id>.jsonl`.
        let real = dir.join("sessions/amp-manager");
        std::fs::create_dir_all(&real).unwrap();
        let link = dir.join("link");
        std::os::unix::fs::symlink(&real, &link).unwrap();

        // Created through the link, resumed through the real path: same directory.
        let created = create(&link, &link, "amp-manager")
            .await
            .expect("create through the link");
        assert_eq!(
            session_index::inspect(&created.path).expect("inspect").cwd,
            Some(canonical_cwd(&real)),
            "the header records where it was really started"
        );
        // Dropped, as a chat that exited would: the file is free to resume.
        drop(created.session);

        let root = session_index::sessions_root(&dir);
        let listed = session_index::list(&root, &link).expect("list");
        assert_eq!(
            listed.len(),
            1,
            "listed under the directory it was started in"
        );

        let opened = load(&created.path, &link, "amp-manager")
            .await
            .expect("resume through the link");
        assert_eq!(opened.path, created.path);
    }

    /// One guard per file: sessions in the same directory never contend.
    #[tokio::test]
    async fn each_session_file_has_its_own_guard() {
        let dir = tmpdir("per-file");
        let a = create(&dir, &dir, "amp-manager").await.expect("create a");
        let b = create(&dir, &dir, "amp-manager").await.expect("create b");
        assert_ne!(a.path, b.path);
        assert!(!free(&a.path));
        assert!(!free(&b.path));
        // Independent: dropping one releases only its own file.
        drop(a.session);
        assert!(free(&a.path));
        assert!(!free(&b.path), "b is untouched by a's release");
    }

    // -- Loading --------------------------------------------------------------

    #[tokio::test]
    async fn a_saved_session_loads_with_its_history_and_stays_owned() {
        let dir = tmpdir("loaded");
        let file = saved(&dir, &dir, "what were we doing?").await;

        let opened = load(&file, &dir, "amp-manager").await.expect("load");
        assert_eq!(opened.path, file);
        assert!(
            !free(&file),
            "the loaded session owns the file it will append to"
        );
        let entries = opened
            .session
            .find_entries(&EntryQuery {
                order: Some(EntryOrder::OldestFirst),
                ..Default::default()
            })
            .await
            .expect("entries");
        assert!(
            entries.iter().any(|e| match e {
                Entry::Message(m) => content(&m.message) == Some("what were we doing?"),
                _ => false,
            }),
            "{entries:?}"
        );
        // Continuing it appends to the same file, under the same guard.
        opened
            .session
            .append_message(user("we were resuming"))
            .await
            .expect("append");
        let text = std::fs::read_to_string(&file).unwrap();
        assert!(text.contains("we were resuming"), "{text}");
        assert!(!free(&file));
    }

    /// Storage §10.3: the repairing republish may only run while the guard is held. A torn last
    /// line is dropped by `load`, and the file is owned throughout.
    #[tokio::test]
    async fn a_torn_tail_is_repaired_under_the_guard() {
        let dir = tmpdir("torn");
        let file = saved(&dir, &dir, "hello").await;
        // A crash mid-append: a partial JSON line, not newline-terminated.
        let mut text = std::fs::read_to_string(&file).unwrap();
        text.push_str("{\"kind\":\"entry\",\"seq\":2,\"ty");
        std::fs::write(&file, &text).unwrap();

        let opened = load(&file, &dir, "amp-manager").await.expect("load");
        let repaired = std::fs::read_to_string(&file).unwrap();
        assert!(
            !repaired.contains("{\"kind\":\"entry\",\"seq\":2,\"ty"),
            "the torn line is dropped: {repaired}"
        );
        assert!(
            repaired.ends_with('\n'),
            "the repaired file is line-terminated: {repaired}"
        );
        assert!(
            !free(&file),
            "the repair ran under the guard, and the guard is still held for the appends after it"
        );
        assert!(repaired.contains("hello"), "the valid prefix is kept");
        drop(opened.session);
        assert!(free(&file));
    }

    /// The refusal comes *before* `load`, so a live writer's file is never opened for repair: its
    /// bytes are exactly as they were, torn tail included.
    #[tokio::test]
    async fn a_live_writers_session_is_refused_before_any_repair() {
        let dir = tmpdir("refused");
        let file = saved(&dir, &dir, "mine").await;
        let mut text = std::fs::read_to_string(&file).unwrap();
        text.push_str("{\"kind\":\"entry\",\"seq\":2,\"ty");
        std::fs::write(&file, &text).unwrap();
        let live = claim_session_lock(&file).expect("the live writer takes it first");

        let err = load(&file, &dir, "amp-manager").await.unwrap_err();
        assert!(matches!(err, OpenError::Busy(_)), "{err}");
        assert!(
            err.held_by_this_process(),
            "the refusal names this process as the holder: {err}"
        );
        assert_eq!(
            std::fs::read_to_string(&file).unwrap(),
            text,
            "the refused file was not even opened for repair"
        );
        drop(live);
    }

    #[tokio::test]
    async fn a_session_from_another_directory_is_refused() {
        let dir = tmpdir("other-cwd");
        let elsewhere = dir.join("elsewhere");
        std::fs::create_dir_all(&elsewhere).unwrap();
        let file = saved(&dir, &elsewhere, "over there").await;
        let before = std::fs::read(&file).unwrap();

        let err = load(&file, &dir, "amp-manager").await.unwrap_err();
        assert!(matches!(err, OpenError::OtherDirectory { .. }), "{err}");
        let text = err.to_string();
        assert!(text.contains("was started in"), "{text}");
        assert!(text.contains(&canonical_cwd(&elsewhere)), "{text}");
        assert_eq!(std::fs::read(&file).unwrap(), before, "nothing written");
        assert!(free(&file), "and no guard was left behind");
    }

    #[tokio::test]
    async fn a_format_this_harness_does_not_read_is_refused_without_writing() {
        let dir = tmpdir("v3");
        let file = dir.join("1767225600000-sess-v3.jsonl");
        let v3 = format!(
            "{{\"kind\":\"header\",\"version\":3,\"id\":\"sess-v3\",\"createdAt\":1767225600000,\
             \"cwd\":\"{}\"}}\n",
            canonical_cwd(&dir)
        );
        std::fs::write(&file, &v3).unwrap();

        let err = load(&file, &dir, "amp-manager").await.unwrap_err();
        assert!(matches!(err, OpenError::Incompatible { .. }), "{err}");
        assert!(
            err.to_string()
                .contains("format this harness does not read"),
            "{err}"
        );
        assert_eq!(std::fs::read_to_string(&file).unwrap(), v3);
        assert!(free(&file));
    }

    #[tokio::test]
    async fn a_damaged_header_and_a_foreign_name_are_refused() {
        let dir = tmpdir("damaged");
        let torn = dir.join("1767225600000-sess-torn.jsonl");
        std::fs::write(&torn, "{\"kind\":\"head").unwrap();
        let err = load(&torn, &dir, "amp-manager").await.unwrap_err();
        assert!(matches!(err, OpenError::Unreadable { .. }), "{err}");

        let foreign = dir.join("notes.jsonl");
        std::fs::write(&foreign, "{}\n").unwrap();
        let err = load(&foreign, &dir, "amp-manager").await.unwrap_err();
        assert!(matches!(err, OpenError::NotASession { .. }), "{err}");
        assert!(free(&torn) && free(&foreign));
    }

    /// A syntax error before the last line is hard corruption for rpi (`load` repairs only a torn
    /// *last* line and duplicate seqs), so it is refused; the file is left alone and no guard is
    /// retained.
    #[tokio::test]
    async fn a_corrupt_transcript_stays_untouched_and_releases_the_guard() {
        let dir = tmpdir("corrupt");
        let file = saved(&dir, &dir, "first").await;
        let mut text = std::fs::read_to_string(&file).unwrap();
        text.push_str("oops not json\n");
        text.push_str("{\"kind\":\"lane\",\"seq\":9,\"lane\":\"side\",\"leafId\":null}\n");
        std::fs::write(&file, &text).unwrap();

        let err = load(&file, &dir, "amp-manager").await.unwrap_err();
        assert!(matches!(err, OpenError::Load { .. }), "{err}");
        assert_eq!(std::fs::read_to_string(&file).unwrap(), text);
        assert!(free(&file), "the failed load dropped its guard");
    }

    // -- The harness switch ---------------------------------------------------

    #[tokio::test]
    async fn a_resume_switches_the_harness_onto_the_loaded_session() {
        let dir = tmpdir("switch");
        let file = saved(&dir, &dir, "before the switch").await;
        let harness = harness_on(memory_session()).await;
        let before = harness.session().get_metadata().await.expect("metadata").id;

        let resumed = resume_session(&harness, &file, &dir, "amp-manager")
            .await
            .expect("resume");
        assert_eq!(resumed, file);
        let after = harness.session().get_metadata().await.expect("metadata").id;
        assert_ne!(after, before, "the harness is on the loaded session now");
        assert!(
            !free(&file),
            "and holds its guard: the harness can append to it"
        );
        let log = harness
            .session()
            .get_log(&LogOptions::default())
            .await
            .expect("log");
        assert!(!log.is_empty(), "the loaded history is visible: {log:?}");
    }

    /// The acceptance criterion: a loader failure leaves the active session — and its guard —
    /// exactly as they were.
    #[tokio::test]
    async fn a_failed_resume_leaves_the_active_session_and_its_guard_intact() {
        let dir = tmpdir("failure-keeps");
        let active = create(&dir, &dir, "amp-manager").await.expect("create");
        let active_path = active.path.clone();
        let harness = harness_on(active.session).await;
        let id = harness.session().get_metadata().await.expect("metadata").id;
        let active_bytes = std::fs::read(&active_path).unwrap();

        // Every refusal shape in turn: another directory, a foreign name, a damaged header.
        let elsewhere = dir.join("elsewhere");
        std::fs::create_dir_all(&elsewhere).unwrap();
        let other_cwd = saved(&dir, &elsewhere, "not here").await;
        let foreign = dir.join("foreign.jsonl");
        std::fs::write(&foreign, "{}\n").unwrap();
        let damaged = dir.join("1767225600000-sess-damaged.jsonl");
        std::fs::write(&damaged, "{\"kind\":\"head").unwrap();
        for path in [&other_cwd, &foreign, &damaged] {
            let err = resume_session(&harness, path, &dir, "amp-manager")
                .await
                .unwrap_err();
            assert!(matches!(err, ResumeError::Open(_)), "{path:?}: {err}");
        }
        // …and a session this process already owns: refused, naming this process.
        let busy_path = saved(&dir, &dir, "held").await;
        let held = claim_session_lock(&busy_path).expect("hold it elsewhere");
        let err = resume_session(&harness, &busy_path, &dir, "amp-manager")
            .await
            .unwrap_err();
        assert!(err.held_by_this_process(), "{err}");
        assert!(
            err.to_string().contains("already open in this process"),
            "{err}"
        );
        drop(held);

        // The active session is untouched: same identity, same bytes, still owned by this process.
        assert_eq!(
            harness.session().get_metadata().await.expect("metadata").id,
            id
        );
        assert_eq!(std::fs::read(&active_path).unwrap(), active_bytes);
        assert!(
            !free(&active_path),
            "the active session kept its guard through every failed resume"
        );
        // And the refused sessions were left exactly as they were, with no guard behind.
        for path in [&other_cwd, &foreign, &damaged, &busy_path] {
            assert!(free(path), "{path:?} is not left locked");
        }
    }

    /// A switch releases the session it left, so a process never holds a file it is not writing.
    #[tokio::test]
    async fn a_switch_releases_the_previous_session_and_appends_in_place() {
        let dir = tmpdir("switch-release");
        let first = create(&dir, &dir, "amp-manager").await.expect("create");
        let first_path = first.path.clone();
        let harness = harness_on(first.session).await;
        let first_bytes = std::fs::read(&first_path).unwrap();
        let second = saved(&dir, &dir, "second one").await;

        resume_session(&harness, &second, &dir, "amp-manager")
            .await
            .expect("resume");
        assert!(!free(&second), "the resumed session is owned");
        assert!(
            free(&first_path),
            "the session it left is released, so nothing is held that is not written"
        );
        // The harness writes the resumed file, in place, and never the old one.
        let before = std::fs::read(&second).unwrap();
        harness
            .session()
            .append_message(user("after the switch"))
            .await
            .expect("append");
        let after = std::fs::read(&second).unwrap();
        assert!(
            after.len() > before.len() && after.starts_with(&before),
            "appended to the resumed file"
        );
        assert_eq!(
            std::fs::read(&first_path).unwrap(),
            first_bytes,
            "the file it left is never written again"
        );
    }

    // -- What a resume must drop (AMP #3012) ----------------------------------

    /// A prompt the file still had queued must not run in the resumed chat. `set_session` rebuilds
    /// rpi's in-memory queues from the durable `queue_enqueued` records, so without an explicit
    /// cancel a session saved mid-reply hands the next turn prompts the person queued in a chat
    /// that is over — including `next_run`, which `clear_queue` never touches.
    #[tokio::test]
    async fn a_resume_drops_the_prompts_the_file_still_had_queued() {
        let dir = tmpdir("resume-drops-queues");
        let file = saved(&dir, &dir, "before the switch").await;
        // Queue two prompts of different kinds, exactly as a chat does mid-reply: one steered into
        // the running turn and one for the next. Both are durable records with reserved entries.
        {
            let writer = harness_on(load_session(&file).await).await;
            writer
                .steer(user("steered into the dead run"))
                .await
                .expect("steer");
            writer
                .next_run(user("queued for the next turn"))
                .await
                .expect("next_run");
            let queued = writer.queued_messages().await.expect("queued");
            assert_eq!(queued.steering.len(), 1, "{queued:?}");
            drop(writer);
        }
        assert!(free(&file), "the writer that queued them is gone");

        // A fresh harness resumes it. Nothing of the old queues may come with it.
        let harness = harness_on(memory_session()).await;
        resume_session(&harness, &file, &dir, "amp-manager")
            .await
            .expect("resume");
        let queued = harness.queued_messages().await.expect("queued");
        assert!(
            queued.is_empty(),
            "the resumed chat starts with nothing queued: {queued:?}"
        );

        // And it is durable, not just in memory: the records on the file itself say every enqueue
        // was cancelled, so a later reopen of this file finds nothing pending either.
        let reopened = harness.session();
        let records = reopened
            .find_records(&RecordQuery {
                record_type: Some("queue_enqueued"),
                order: Some(EntryOrder::OldestFirst),
                ..Default::default()
            })
            .await
            .expect("records");
        let cancelled = reopened
            .find_records(&RecordQuery {
                record_type: Some("queue_cancelled"),
                ..Default::default()
            })
            .await
            .expect("records");
        assert_eq!(
            records.len(),
            cancelled.len(),
            "every enqueue was cancelled on disk"
        );
        assert!(!records.is_empty(), "there were enqueues to cancel");
    }

    /// The other half: a prompt the run already consumed is history, not a queue. Resuming must not
    /// rewrite a conversation's records for messages the model already saw.
    #[tokio::test]
    async fn a_resume_leaves_prompts_a_run_already_consumed_alone() {
        let dir = tmpdir("resume-keeps-consumed");
        let file = saved(&dir, &dir, "a prompt a run took").await;
        let cancellations = {
            let session = load_session(&file).await;
            session
                .find_records(&RecordQuery {
                    record_type: Some("queue_cancelled"),
                    ..Default::default()
                })
                .await
                .expect("records")
                .len()
        };
        let harness = harness_on(memory_session()).await;
        resume_session(&harness, &file, &dir, "amp-manager")
            .await
            .expect("resume");
        let after = harness
            .session()
            .find_records(&RecordQuery {
                record_type: Some("queue_cancelled"),
                ..Default::default()
            })
            .await
            .expect("records")
            .len();
        assert_eq!(
            after, cancellations,
            "a plain conversation has no queue records to cancel"
        );
        // Its history is intact and still there — the resume did not drop the conversation.
        let entries = harness
            .session()
            .find_entries(&EntryQuery::default())
            .await
            .expect("entries");
        assert!(
            entries.iter().any(|e| matches!(e, Entry::Message { .. })),
            "the saved prompt is still in the session: {entries:?}"
        );
    }

    /// A session file opened for writing under its own guard (what `load` does, without a harness).
    async fn load_session(path: &Path) -> Session {
        load(path, path.parent().unwrap(), "amp-manager")
            .await
            .expect("load")
            .session
    }
}
