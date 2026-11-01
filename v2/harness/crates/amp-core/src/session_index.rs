//! Directory-aware session discovery for `/sessions` (#3000): which saved chats belong to the
//! working directory the harness is in now.
//!
//! The harness writes one JSONL file per session under `<state>/sessions/<directory>/`, named
//! `<created ms>-<session id>.jsonl`, whose first line is a v4 header carrying the canonical
//! working directory ([`crate::agent::jsonl_session`]). Chat sessions live in their role id's
//! directory (`amp-manager/`, `amp-worker-backend/`, …); worker sessions live in their ticket id's
//! ([`crate::worker_session::sessions_dir`]). [`list`] reads exactly that layout, so a session is
//! found where it was written: nothing is moved, renamed or rewritten, and no second layout is
//! introduced.
//!
//! **Read-only and bounded.** A listing needs the header and a hint of the transcript, never the
//! transcript: [`list`] reads at most [`HEAD_MAX_BYTES`] from the start of a file (the header line
//! plus the opening prompt) and at most [`TAIL_MAX_BYTES`] from its end (the latest text). It never
//! calls `JsonlSessionStorage::load`, so it never repairs, rewrites or extends a file, and it takes
//! no lock: the ownership contract of `discoveries/harness-local-session-storage.md` §10 requires a
//! lock only for access that may write. A multi-GB session costs the same two reads as a new one.
//!
//! **One directory.** A session belongs to the directory it was started in, and the header's `cwd`
//! is that canonical directory (`session::build` canonicalizes before the session is created). So
//! [`list`] compares the header's `cwd` with the canonical current directory **exactly**, and does
//! it before taking a preview: another directory's session is neither listed nor read past its
//! header window, and none of its text ever reaches the preview.
//!
//! **What a listing says about a broken file** ([`EntryStatus`]): a session file's *name* carries
//! its id and creation time, so a file whose header cannot be read — a torn first line, or a format
//! this harness does not read (v3) — is still listed with the reason it cannot be opened. It is
//! listed with no preview text at all, because its directory cannot be checked and no part of a
//! session that might belong elsewhere may be shown.
//!
//! Ordering is newest activity first (the file's modification time — its last append), with a
//! deterministic tie-break ([`SessionEntry`]'s sort) so two listings of the same directory never
//! disagree.

use std::{
    io::{self, Read, Seek, SeekFrom},
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};

use rpi_harness::session::jsonl::{JsonlDecodeErrorKind, parse_header};
use serde_json::Value;

/// The most of a session file's start [`list`] reads: enough for the header line and the opening
/// prompt, never the transcript.
pub const HEAD_MAX_BYTES: u64 = 16 * 1024;

/// The most of a session file's end [`list`] reads: the latest line of text in the session.
pub const TAIL_MAX_BYTES: u64 = 16 * 1024;

/// The longest preview [`list`] keeps, in characters (an elided string ends in `…`).
pub const PREVIEW_MAX_CHARS: usize = 120;

/// Where every session directory lives under the harness state dir.
pub const SESSIONS_DIR: &str = "sessions";

/// One saved session as a listing shows it: who it was, when it was last active, and (only when it
/// belongs to the directory being listed) what it was about.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionEntry {
    /// The session file itself: what a resume opens.
    pub path: PathBuf,
    /// The directory it was found in (`amp-manager`, `amp-worker-backend`, …); empty when the
    /// caller inspected a file by path. Worker (ticket id) directories are never listed.
    pub role: String,
    /// The session id (`JsonlV4Header::id` when the header reads, else the file name's).
    pub id: String,
    /// When the session was created (ms since the epoch): the header's `createdAt` when it reads,
    /// else the file name's prefix.
    pub created_at: i64,
    /// The canonical working directory the session was started in; `None` when the header cannot
    /// be read (then nothing about its directory is known).
    pub cwd: Option<String>,
    /// Its last activity: the file's modification time (each append touches it).
    pub modified_at: SystemTime,
    /// The file's size in bytes (shown by the picker; never read in full).
    pub size: u64,
    /// The session's opening prompt, one line, at most [`PREVIEW_MAX_CHARS`] characters: the first
    /// user message (else the first assistant text) in the bounded head read. `None` for a session
    /// with no messages yet, a session whose opening prompt is past the head bound, and a session
    /// whose header cannot be read (no preview is ever taken from one).
    pub opening: Option<String>,
    /// The latest thing said in the session, one line, bounded: the last message in the bounded
    /// tail read, whether the person's or the model's.
    pub latest: Option<String>,
    /// Whether this harness can open the file, and if not, why.
    pub status: EntryStatus,
}

/// Why a session file cannot be opened (or that it can).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EntryStatus {
    /// A v4 header this harness read, in this directory.
    Ok,
    /// The header line could not be read or parsed as JSON: a torn first line, or damage before
    /// the session's first append completed.
    Unreadable(String),
    /// A syntactically valid session header in a format this harness does not read (a version
    /// other than 4, or an invalid combination of its fields).
    Incompatible(String),
}

impl EntryStatus {
    /// Whether the session can be opened at all.
    pub fn readable(&self) -> bool {
        matches!(self, Self::Ok)
    }

    /// Why it cannot be opened, in words for a person; `None` when it can.
    pub fn reason(&self) -> Option<&str> {
        match self {
            Self::Ok => None,
            Self::Unreadable(why) | Self::Incompatible(why) => Some(why),
        }
    }
}

impl SessionEntry {
    /// Whether [`crate::agent`]'s resume path can continue this session: a v4 header that was read
    /// and, for a listing, matched the directory being listed.
    pub fn resumable(&self) -> bool {
        self.status.readable()
    }
}

/// [`SESSIONS_DIR`] under the harness state dir: the root [`list`] scans.
pub fn sessions_root(state_dir: &Path) -> PathBuf {
    state_dir.join(SESSIONS_DIR)
}

/// Every session under `sessions_root` ([`sessions_root`]) that was started in `cwd`, newest
/// activity first.
///
/// `cwd` is canonicalized first (a symlinked or `.`-relative path finds the same sessions); the
/// header's `cwd`, written canonical by [`crate::agent::jsonl_session`], is compared with it
/// exactly. Session files whose header cannot be read are listed too (their name still identifies
/// them, [`EntryStatus`] says why they cannot be opened) but never with preview text.
///
/// A missing `sessions_root` is an empty listing (no session has been saved yet); only failing to
/// read the root itself is an error. A directory under it that cannot be read contributes nothing.
pub fn list(sessions_root: &Path, cwd: &Path) -> Result<Vec<SessionEntry>, String> {
    let cwd = canonical(cwd);
    let dir = match std::fs::read_dir(sessions_root) {
        Ok(dir) => dir,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(format!("read {}: {e}", sessions_root.display())),
    };
    let mut entries: Vec<SessionEntry> = Vec::new();
    for role_dir in dir.flatten() {
        // Worker sessions live under their numeric ticket id; they belong to a ticket's transcript
        // view, not to a person's conversations.
        let role = role_dir.file_name().to_string_lossy().into_owned();
        if role.is_empty() || role.chars().all(|c| c.is_ascii_digit()) {
            continue;
        }
        if !role_dir.path().is_dir() {
            continue;
        }
        let Ok(files) = std::fs::read_dir(role_dir.path()) else {
            continue;
        };
        for file in files.flatten() {
            let path = file.path();
            if !path.is_file() {
                continue;
            }
            let Some(named) = named(&path) else {
                // Not one of this harness's files (`<ms>-<id>.jsonl`): a sidecar `.lock`, an
                // editor's leftover, another tool's JSONL.
                continue;
            };
            if let Some(entry) = read_entry(&path, role.clone(), named, Some(&cwd)) {
                entries.push(entry);
            }
        }
    }
    sort(&mut entries);
    Ok(entries)
}

/// One session file, whatever directory it was started in: the single-file counterpart of [`list`],
/// for a caller that already chose a file (a resume validates [`SessionEntry::cwd`] itself).
///
/// `Err` when `path` is not named like one of this harness's sessions, or cannot be stat'd; a file
/// that is named right but damaged comes back as an entry with a non-[`EntryStatus::Ok`] status.
pub fn inspect(path: &Path) -> Result<SessionEntry, String> {
    let named = named(path)
        .ok_or_else(|| format!("{}: not a session file (<ms>-<id>.jsonl)", path.display()))?;
    let role = path
        .parent()
        .and_then(Path::file_name)
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    read_entry(path, role, named, None).ok_or_else(|| format!("{}: not readable", path.display()))
}

/// The identity a session file's name carries, whatever its header says.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Named {
    created_at: i64,
    id: String,
}

/// `<unix ms>-<session id>.jsonl` (the name [`crate::agent::jsonl_session`] writes), if that is what
/// `path` is.
fn named(path: &Path) -> Option<Named> {
    let name = path.file_name()?.to_str()?.strip_suffix(".jsonl")?;
    let (created, id) = name.split_once('-')?;
    if id.is_empty() {
        return None;
    }
    Some(Named {
        created_at: created.parse::<i64>().ok()?,
        id: id.to_owned(),
    })
}

/// Reads one file's identity, header state and (when `only_cwd` matches) its previews. `None` only
/// when `only_cwd` is given and the session was started somewhere else.
fn read_entry(
    path: &Path,
    role: String,
    named: Named,
    only_cwd: Option<&str>,
) -> Option<SessionEntry> {
    let mut entry = SessionEntry {
        path: path.to_path_buf(),
        role,
        id: named.id.clone(),
        created_at: named.created_at,
        cwd: None,
        modified_at: UNIX_EPOCH,
        size: 0,
        opening: None,
        latest: None,
        status: EntryStatus::Unreadable(String::new()),
    };
    match std::fs::metadata(path) {
        Ok(meta) => {
            entry.size = meta.len();
            entry.modified_at = meta.modified().unwrap_or(UNIX_EPOCH);
        }
        Err(e) => {
            entry.status = EntryStatus::Unreadable(format!("the file cannot be read: {e}"));
            return Some(entry);
        }
    }
    let head = match head_bytes(path, HEAD_MAX_BYTES) {
        Ok(head) => head,
        Err(e) => {
            entry.status = EntryStatus::Unreadable(format!("the file cannot be read: {e}"));
            return Some(entry);
        }
    };
    let head_text = String::from_utf8_lossy(&head);
    let Some((header_line, transcript)) = split_header(&head_text, head.len() as u64) else {
        entry.status = EntryStatus::Unreadable(format!(
            "the session header is longer than the {HEAD_MAX_BYTES} bytes this listing reads"
        ));
        return Some(entry);
    };
    let header = match parse_header(header_line) {
        Ok(header) => header,
        Err(e) => {
            let why = match e.kind {
                JsonlDecodeErrorKind::Syntax => EntryStatus::Unreadable(format!(
                    "the session header cannot be read: {}",
                    e.message
                )),
                JsonlDecodeErrorKind::Schema => EntryStatus::Incompatible(format!(
                    "the session was written in a format this harness does not read ({})",
                    e.message
                )),
            };
            entry.status = why;
            return Some(entry);
        }
    };
    if let Some(cwd) = only_cwd
        && header.cwd != cwd
    {
        // Another directory's session: not listed, and its preview window is never read.
        return None;
    }
    entry.id = header.id.clone();
    entry.created_at = header.created_at;
    entry.cwd = Some(header.cwd);
    entry.status = EntryStatus::Ok;
    entry.opening = opening_text(transcript);
    entry.latest = latest_text(path, entry.size);
    Some(entry)
}

/// The header line and the text after it. A file shorter than the head bound whose last line is not
/// newline-terminated is a header-only file a crash left behind: that is the header. `None` when no
/// newline was found in a full head read (a header longer than [`HEAD_MAX_BYTES`]).
fn split_header(head: &str, read: u64) -> Option<(&str, &str)> {
    match head.split_once('\n') {
        Some((line, rest)) => Some((line.trim_end_matches('\r'), rest)),
        None if read < HEAD_MAX_BYTES => Some((head.trim_end_matches('\r'), "")),
        None => None,
    }
}

/// The session's opening prompt: the first user message's text, else the first assistant text, in
/// the bounded head read.
fn opening_text(transcript: &str) -> Option<String> {
    let mut fallback = None;
    for line in transcript.lines() {
        if let Some((is_user, text)) = message_text(line) {
            if is_user {
                return Some(text);
            }
            fallback.get_or_insert(text);
        }
    }
    fallback
}

/// What was last said in the session, from the bounded tail read: the last message's text.
fn latest_text(path: &Path, size: u64) -> Option<String> {
    let tail = tail_bytes(path, size, TAIL_MAX_BYTES).ok()?;
    let text = String::from_utf8_lossy(&tail);
    let mut lines = text.lines();
    // A file longer than the window starts mid-line, and that partial line is not a line at all.
    if size > TAIL_MAX_BYTES {
        lines.next();
    }
    lines
        .rev()
        .find_map(|line| message_text(line).map(|(_, text)| text))
}

/// The text of one JSONL line, when it is a `{"kind":"entry","type":"message"}` line: whether the
/// message is the user's, and its text. Records, lanes, facts, streaming frames and lines that do
/// not parse (a torn tail) are `None`.
fn message_text(line: &str) -> Option<(bool, String)> {
    let value: Value = serde_json::from_str(line).ok()?;
    if value.get("kind")?.as_str()? != "entry" || value.get("type")?.as_str()? != "message" {
        return None;
    }
    let message = value.get("message")?;
    let kind = message.get("kind")?.as_str()?;
    if kind != "user" && kind != "assistant" {
        return None;
    }
    let text = content_text(message.get("content")?)?;
    Some((kind == "user", text))
}

/// A message's `content` as text: a user prompt is a plain string or a block list, an assistant
/// message a block list; only `text` blocks count (an image or a tool call is not a preview).
fn content_text(content: &Value) -> Option<String> {
    let text = match content {
        Value::String(s) => s.clone(),
        Value::Array(blocks) => blocks
            .iter()
            .filter(|b| b.get("type").and_then(Value::as_str) == Some("text"))
            .filter_map(|b| b.get("text").and_then(Value::as_str))
            .collect::<Vec<_>>()
            .join("\n"),
        _ => return None,
    };
    let text = one_line(&text);
    (!text.is_empty()).then_some(text)
}

/// `text` on one line, every run of whitespace collapsed to a space, at most
/// [`PREVIEW_MAX_CHARS`] characters (elided with `…`).
fn one_line(text: &str) -> String {
    let joined = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if joined.chars().count() <= PREVIEW_MAX_CHARS {
        return joined;
    }
    let mut elided: String = joined.chars().take(PREVIEW_MAX_CHARS - 1).collect();
    elided.push('…');
    elided
}

/// The canonical form of `cwd` as the header records it, falling back to `cwd` itself when it cannot
/// be canonicalized (a directory that no longer exists still lists the sessions that were started
/// in it, by the path it was given).
fn canonical(cwd: &Path) -> String {
    cwd.canonicalize()
        .unwrap_or_else(|_| cwd.to_path_buf())
        .to_string_lossy()
        .into_owned()
}

/// At most `limit` bytes from the file's start. Never the whole file.
fn head_bytes(path: &Path, limit: u64) -> io::Result<Vec<u8>> {
    let mut file = std::fs::File::open(path)?;
    let mut buf = Vec::new();
    file.by_ref().take(limit).read_to_end(&mut buf)?;
    Ok(buf)
}

/// At most `limit` bytes from the file's end: the tail of a large session costs one seek and one
/// bounded read, whatever its size.
fn tail_bytes(path: &Path, size: u64, limit: u64) -> io::Result<Vec<u8>> {
    let mut file = std::fs::File::open(path)?;
    file.seek(SeekFrom::Start(size.saturating_sub(limit)))?;
    let mut buf = Vec::new();
    file.by_ref().take(limit).read_to_end(&mut buf)?;
    Ok(buf)
}

/// Newest activity first, deterministically: last modification, then creation, then path. Two
/// listings of one directory never disagree about order, even when a test (or a filesystem with
/// one-second timestamps) gives several files the same time.
fn sort(entries: &mut [SessionEntry]) {
    entries.sort_by(|a, b| {
        b.modified_at
            .cmp(&a.modified_at)
            .then_with(|| b.created_at.cmp(&a.created_at))
            .then_with(|| a.path.cmp(&b.path))
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use rpi_harness::session::jsonl::{JsonlV4Header, encode_header};
    use serde_json::json;
    use std::time::Duration;

    /// D13: never /tmp. `cargo` sets the target tmp dir for integration tests; a unit test in the
    /// library falls back to the workspace's own target dir rather than the system temp dir.
    fn tmp(name: &str) -> PathBuf {
        let base = std::env::var_os("CARGO_TARGET_TMPDIR")
            .map(PathBuf::from)
            .or_else(|| {
                std::env::var_os("CARGO_MANIFEST_DIR")
                    .map(|d| PathBuf::from(d).join("../../target").join("tmp"))
            })
            .unwrap_or_else(|| PathBuf::from("target/tmp"));
        let dir = base.join("amp-core-session-index").join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn header_line(id: &str, created: i64, cwd: &str) -> String {
        let header = JsonlV4Header::new(id.to_owned(), created, cwd.to_owned(), None, None, None);
        encode_header(&header)
    }

    fn user_line(text: &str, at: i64) -> String {
        let mut line = json!({
            "kind": "entry",
            "lane": "main",
            "type": "message",
            "id": format!("e-{at}"),
            "seq": 1,
            "parentId": null,
            "timestamp": at,
            "message": {"kind": "user", "role": "user", "content": text, "timestamp": at},
        })
        .to_string();
        line.push('\n');
        line
    }

    fn assistant_line(text: &str, at: i64) -> String {
        let mut line = json!({
            "kind": "entry",
            "lane": "main",
            "type": "message",
            "id": format!("a-{at}"),
            "seq": 2,
            "parentId": null,
            "timestamp": at,
            "message": {
                "kind": "assistant",
                "role": "assistant",
                "content": [{"type": "thinking", "text": "not a preview"},
                            {"type": "text", "text": text}],
                "api": "openai-completions",
                "provider": "openrouter",
                "model": "openai/gpt-6.1-sol",
                "usage": {"input": 1, "output": 1, "cacheRead": 0, "cacheWrite": 0},
                "stopReason": "stop",
                "timestamp": at,
            },
        })
        .to_string();
        line.push('\n');
        line
    }

    /// Writes `<root>/<role>/<created>-<id>.jsonl` with `lines` after its header.
    fn session(
        root: &Path,
        role: &str,
        created: i64,
        id: &str,
        cwd: &Path,
        lines: &[String],
    ) -> PathBuf {
        let dir = root.join(role);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join(format!("{created}-{id}.jsonl"));
        let mut text = header_line(id, created, &cwd.to_string_lossy());
        for line in lines {
            text.push_str(line);
        }
        std::fs::write(&path, text).unwrap();
        path
    }

    fn touch(path: &Path, at: SystemTime) {
        let file = std::fs::File::options().write(true).open(path).unwrap();
        file.set_modified(at).unwrap();
    }

    fn at(seconds: u64) -> SystemTime {
        UNIX_EPOCH + Duration::from_secs(seconds)
    }

    #[test]
    fn same_directory_sessions_are_listed_newest_activity_first() {
        let root = tmp("order");
        let cwd = tmp("order/cwd");
        let cwd = cwd.canonicalize().unwrap();
        let older = session(
            &root,
            "amp-manager",
            1_000,
            "id-older",
            &cwd,
            &[
                user_line("first question", 1_001),
                assistant_line("an answer", 1_002),
            ],
        );
        let newer = session(
            &root,
            "amp-manager",
            2_000,
            "id-newer",
            &cwd,
            &[user_line("second question", 2_001)],
        );
        let middle = session(
            &root,
            "amp-worker-backend",
            1_500,
            "id-middle",
            &cwd,
            &[user_line("third question", 1_501)],
        );
        // Activity is the file's own modification time, not its creation: the oldest session is the
        // one that was appended to last.
        touch(&older, at(9_000));
        touch(&middle, at(5_000));
        touch(&newer, at(1_000));

        let entries = list(&root, &cwd).unwrap();
        let ids: Vec<&str> = entries.iter().map(|e| e.id.as_str()).collect();
        assert_eq!(ids, ["id-older", "id-middle", "id-newer"]);
        assert_eq!(entries[0].role, "amp-manager");
        assert_eq!(entries[1].role, "amp-worker-backend");
        assert_eq!(entries[0].opening.as_deref(), Some("first question"));
        assert_eq!(entries[0].latest.as_deref(), Some("an answer"));
        assert_eq!(
            entries[0].cwd.as_deref(),
            Some(cwd.to_string_lossy().as_ref())
        );
        assert!(entries.iter().all(|e| e.resumable()));
        assert!(entries.iter().all(|e| e.size > 0));
    }

    #[test]
    fn another_directory_sessions_are_not_listed_and_their_previews_never_read() {
        let root = tmp("cross-cwd");
        let mine = tmp("cross-cwd/mine").canonicalize().unwrap();
        let other = tmp("cross-cwd/other").canonicalize().unwrap();
        session(
            &root,
            "amp-manager",
            1_000,
            "id-mine",
            &mine,
            &[user_line("my question", 1_001)],
        );
        session(
            &root,
            "amp-manager",
            2_000,
            "id-other",
            &other,
            &[user_line("SECRET other project question", 2_001)],
        );

        let entries = list(&root, &mine).unwrap();
        let ids: Vec<&str> = entries.iter().map(|e| e.id.as_str()).collect();
        assert_eq!(ids, ["id-mine"]);
        // Nothing from the other directory's transcript is anywhere in the result.
        let described = format!("{entries:?}");
        assert!(!described.contains("SECRET"), "{described}");
    }

    #[test]
    fn worker_ticket_directories_are_skipped() {
        let root = tmp("workers");
        let cwd = tmp("workers/cwd").canonicalize().unwrap();
        session(
            &root,
            "3000",
            1_000,
            "id-worker",
            &cwd,
            &[user_line("a worker's run", 1_001)],
        );
        session(
            &root,
            "amp-manager",
            2_000,
            "id-chat",
            &cwd,
            &[user_line("a chat", 2_001)],
        );
        let entries = list(&root, &cwd).unwrap();
        let ids: Vec<&str> = entries.iter().map(|e| e.id.as_str()).collect();
        assert_eq!(ids, ["id-chat"]);
    }

    #[test]
    fn a_large_session_is_read_only_at_its_ends() {
        let root = tmp("large");
        let cwd = tmp("large/cwd").canonicalize().unwrap();
        // 10 MB of transcript between the header and the last prompt: neither the opening prompt
        // (buried far past the head bound) nor the middle is looked at, and the last prompt is
        // found in the bounded tail read.
        let blob = "x".repeat(2_000);
        let mut lines = Vec::new();
        for n in 0..5_000 {
            lines.push(format!(
                "{{\"kind\":\"record\",\"type\":\"usage\",\"seq\":{n},\"blob\":\"{blob}\"}}\n"
            ));
        }
        lines.push(user_line("the last thing I asked", 9_000));
        let path = session(&root, "amp-manager", 1_000, "id-large", &cwd, &lines);
        let size = std::fs::metadata(&path).unwrap().len();
        assert!(size > 8 * 1024 * 1024, "{size} bytes");

        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries.len(), 1);
        assert!(entries[0].resumable());
        assert_eq!(entries[0].size, size);
        assert_eq!(entries[0].opening, None, "past the head bound");
        assert_eq!(
            entries[0].latest.as_deref(),
            Some("the last thing I asked"),
            "from the bounded tail read"
        );

        // The bound itself: the read helpers never hand back more than their limit.
        assert_eq!(
            head_bytes(&path, HEAD_MAX_BYTES).unwrap().len() as u64,
            HEAD_MAX_BYTES
        );
        assert_eq!(
            tail_bytes(&path, size, TAIL_MAX_BYTES).unwrap().len() as u64,
            TAIL_MAX_BYTES
        );
    }

    #[test]
    fn a_large_session_with_no_text_in_its_tail_window_has_no_latest() {
        let root = tmp("large-no-text");
        let cwd = tmp("large-no-text/cwd").canonicalize().unwrap();
        // Two enormous lines: the last one fills the whole tail window, and it is not a message.
        let dir = root.join("amp-manager");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("1000-id-blob.jsonl");
        let mut text = header_line("id-blob", 1_000, &cwd.to_string_lossy());
        for n in 0..2 {
            text.push_str(&format!(
                "{{\"kind\":\"record\",\"type\":\"usage\",\"seq\":{n},\"blob\":\"{}\"}}\n",
                "z".repeat(100_000)
            ));
        }
        std::fs::write(&path, text).unwrap();

        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries.len(), 1);
        assert!(entries[0].resumable());
        assert_eq!(entries[0].opening, None);
        assert_eq!(entries[0].latest, None);
    }

    #[test]
    fn a_session_of_one_enormous_line_is_listed() {
        let root = tmp("one-line");
        let cwd = tmp("one-line/cwd").canonicalize().unwrap();
        let dir = root.join("amp-manager");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("1000-id-huge.jsonl");
        let mut text = header_line("id-huge", 1_000, &cwd.to_string_lossy());
        text.push_str(&format!(
            "{{\"kind\":\"record\",\"type\":\"operation_started\",\"blob\":\"{}\"}}\n",
            "y".repeat(4 * 1024 * 1024)
        ));
        std::fs::write(&path, text).unwrap();

        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries.len(), 1);
        assert!(entries[0].resumable());
        assert_eq!(entries[0].opening, None);
        assert_eq!(entries[0].latest, None);
    }

    #[test]
    fn a_corrupt_header_is_listed_with_its_reason_and_no_preview() {
        let root = tmp("corrupt");
        let cwd = tmp("corrupt/cwd").canonicalize().unwrap();
        let dir = root.join("amp-manager");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("1000-id-torn.jsonl");
        std::fs::write(&path, "{\"kind\":\"header\",\"vers").unwrap();

        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries.len(), 1);
        let entry = &entries[0];
        assert_eq!(entry.id, "id-torn", "identity from the file name");
        assert_eq!(entry.created_at, 1_000);
        assert_eq!(entry.cwd, None);
        assert!(!entry.resumable());
        assert!(matches!(entry.status, EntryStatus::Unreadable(_)));
        assert!(entry.status.reason().unwrap().contains("header"));
        assert_eq!(entry.opening, None);
        assert_eq!(entry.latest, None);
    }

    #[test]
    fn a_newer_session_format_is_listed_as_incompatible() {
        let root = tmp("v3");
        let cwd = tmp("v3/cwd").canonicalize().unwrap();
        let dir = root.join("amp-manager");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("1000-id-v3.jsonl");
        std::fs::write(
            &path,
            format!(
                "{}\n{}",
                json!({"kind": "header", "version": 3, "id": "id-v3", "createdAt": 1_000,
                       "cwd": cwd.to_string_lossy()}),
                user_line("an old-format question", 1_001)
            ),
        )
        .unwrap();

        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries.len(), 1);
        let entry = &entries[0];
        assert!(matches!(entry.status, EntryStatus::Incompatible(_)));
        assert!(!entry.resumable());
        // Nothing from a file whose format is unknown is shown as a preview.
        assert_eq!(entry.opening, None);
        assert_eq!(entry.latest, None);
    }

    #[test]
    fn a_header_only_session_is_listed_with_no_preview() {
        let root = tmp("header-only");
        let cwd = tmp("header-only/cwd").canonicalize().unwrap();
        session(&root, "amp-manager", 1_000, "id-empty", &cwd, &[]);

        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries.len(), 1);
        assert!(
            entries[0].resumable(),
            "a real session with nothing in it yet"
        );
        assert_eq!(entries[0].opening, None);
        assert_eq!(entries[0].latest, None);
    }

    #[test]
    fn a_header_without_its_trailing_newline_still_reads() {
        let root = tmp("no-newline");
        let cwd = tmp("no-newline/cwd").canonicalize().unwrap();
        let dir = root.join("amp-manager");
        std::fs::create_dir_all(&dir).unwrap();
        let header = header_line("id-crash", 1_000, &cwd.to_string_lossy());
        std::fs::write(dir.join("1000-id-crash.jsonl"), header.trim_end()).unwrap();

        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries.len(), 1);
        assert!(entries[0].resumable());
        assert_eq!(entries[0].id, "id-crash");
    }

    #[test]
    fn a_torn_tail_does_not_break_the_latest_text() {
        let root = tmp("torn");
        let cwd = tmp("torn/cwd").canonicalize().unwrap();
        let mut lines = vec![
            user_line("what is this project", 1_001),
            assistant_line("it is a harness", 1_002),
        ];
        lines.push("{\"kind\":\"entry\",\"type\":\"message\",\"mess".to_owned());
        session(&root, "amp-manager", 1_000, "id-torn", &cwd, &lines);

        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].opening.as_deref(), Some("what is this project"));
        assert_eq!(entries[0].latest.as_deref(), Some("it is a harness"));
    }

    #[test]
    fn the_latest_text_is_the_last_thing_said_and_previews_stay_one_short_line() {
        let root = tmp("previews");
        let cwd = tmp("previews/cwd").canonicalize().unwrap();
        let long = "line one\n\nline two\tis very long ".repeat(40);
        session(
            &root,
            "amp-manager",
            1_000,
            "id-preview",
            &cwd,
            &[
                user_line(&long, 1_001),
                assistant_line("an answer", 1_002),
                user_line("the last thing I asked", 1_003),
                assistant_line("and the answer", 1_004),
            ],
        );

        let entries = list(&root, &cwd).unwrap();
        let entry = &entries[0];
        let opening = entry.opening.as_deref().unwrap();
        assert!(opening.chars().count() <= PREVIEW_MAX_CHARS, "{opening}");
        assert!(opening.ends_with('…'), "{opening}");
        assert!(!opening.contains('\n'));
        assert_eq!(entry.latest.as_deref(), Some("and the answer"));
    }

    #[test]
    fn a_prompt_said_last_is_the_latest_text() {
        let root = tmp("latest-prompt");
        let cwd = tmp("latest-prompt/cwd").canonicalize().unwrap();
        session(
            &root,
            "amp-manager",
            1_000,
            "id-latest",
            &cwd,
            &[
                user_line("first", 1_001),
                assistant_line("an answer", 1_002),
                user_line("my follow-up", 1_003),
            ],
        );
        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries[0].opening.as_deref(), Some("first"));
        assert_eq!(entries[0].latest.as_deref(), Some("my follow-up"));
    }

    #[test]
    fn entries_with_the_same_activity_time_are_ordered_deterministically() {
        let root = tmp("ties");
        let cwd = tmp("ties/cwd").canonicalize().unwrap();
        let first = session(
            &root,
            "amp-manager",
            1_000,
            "id-first",
            &cwd,
            &[user_line("one", 1_001)],
        );
        let second = session(
            &root,
            "amp-manager",
            2_000,
            "id-second",
            &cwd,
            &[user_line("two", 2_001)],
        );
        let third = session(
            &root,
            "amp-worker-backend",
            2_000,
            "id-third",
            &cwd,
            &[user_line("three", 2_001)],
        );
        for path in [&first, &second, &third] {
            touch(path, at(7_000));
        }

        let one: Vec<PathBuf> = list(&root, &cwd)
            .unwrap()
            .into_iter()
            .map(|e| e.path)
            .collect();
        let two: Vec<PathBuf> = list(&root, &cwd)
            .unwrap()
            .into_iter()
            .map(|e| e.path)
            .collect();
        assert_eq!(one, two, "same listing twice, same order");
        // Same mtime: newest creation first, then the path as the final tie-break.
        let ids: Vec<String> = list(&root, &cwd)
            .unwrap()
            .into_iter()
            .map(|e| e.id)
            .collect();
        assert_eq!(ids, ["id-second", "id-third", "id-first"]);
    }

    #[test]
    fn listing_never_changes_a_file_and_ignores_stray_files() {
        let root = tmp("read-only");
        let cwd = tmp("read-only/cwd").canonicalize().unwrap();
        let path = session(
            &root,
            "amp-manager",
            1_000,
            "id-still",
            &cwd,
            &[user_line("do not touch me", 1_001)],
        );
        touch(&path, at(4_000));
        // A future lock sidecar (#3004), an editor leftover and another tool's JSONL are not
        // sessions and must neither be listed nor confuse the scan.
        std::fs::write(root.join("amp-manager/1000-id-still.jsonl.lock"), "{}").unwrap();
        std::fs::write(root.join("amp-manager/notes.txt"), "hello").unwrap();
        std::fs::write(root.join("amp-manager/not-a-session.jsonl"), "hello").unwrap();
        std::fs::write(root.join("notes.txt"), "hello").unwrap();

        let before = (
            std::fs::read(&path).unwrap(),
            std::fs::metadata(&path).unwrap().modified().unwrap(),
            std::fs::metadata(&path).unwrap().len(),
            listing(&root),
        );
        let entries = list(&root, &cwd).unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].id, "id-still");
        let after = (
            std::fs::read(&path).unwrap(),
            std::fs::metadata(&path).unwrap().modified().unwrap(),
            std::fs::metadata(&path).unwrap().len(),
            listing(&root),
        );
        assert_eq!(
            before, after,
            "bytes, mtime, size and the file tree are unchanged"
        );
    }

    /// Every path under `root`, sorted: what a scan may not add to or remove from.
    fn listing(root: &Path) -> Vec<PathBuf> {
        let mut paths = Vec::new();
        let mut todo = vec![root.to_path_buf()];
        while let Some(dir) = todo.pop() {
            for entry in std::fs::read_dir(&dir).unwrap().flatten() {
                let path = entry.path();
                if path.is_dir() {
                    todo.push(path.clone());
                }
                paths.push(path);
            }
        }
        paths.sort();
        paths
    }

    #[test]
    fn a_missing_sessions_directory_is_an_empty_listing() {
        let root = tmp("missing");
        let cwd = tmp("missing/cwd").canonicalize().unwrap();
        assert_eq!(list(&root.join("nope"), &cwd).unwrap(), Vec::new());
    }

    #[test]
    fn a_relative_or_symlinked_cwd_finds_the_same_sessions() {
        let root = tmp("canonical");
        let cwd = tmp("canonical/cwd").canonicalize().unwrap();
        session(
            &root,
            "amp-manager",
            1_000,
            "id-canonical",
            &cwd,
            &[user_line("same directory", 1_001)],
        );
        // A path with a `.` in it is not the canonical string; the listing still matches.
        let dotty = cwd.join(".");
        assert_eq!(list(&root, &dotty).unwrap().len(), 1);

        #[cfg(unix)]
        {
            let link = cwd.parent().unwrap().join("link");
            std::os::unix::fs::symlink(&cwd, &link).unwrap();
            assert_eq!(list(&root, &link).unwrap().len(), 1, "through a symlink");
        }
    }

    #[test]
    fn inspect_reads_one_file_whatever_its_directory() {
        let root = tmp("inspect");
        let cwd = tmp("inspect/cwd").canonicalize().unwrap();
        let path = session(
            &root,
            "amp-manager",
            1_000,
            "id-one",
            &cwd,
            &[user_line("a single session", 1_001)],
        );
        let entry = inspect(&path).unwrap();
        assert_eq!(entry.id, "id-one");
        assert_eq!(entry.role, "amp-manager");
        assert_eq!(entry.cwd.as_deref(), Some(cwd.to_string_lossy().as_ref()));
        assert_eq!(entry.opening.as_deref(), Some("a single session"));
        assert!(entry.resumable());

        // A file that is not named like a session is an error, not an entry.
        assert!(inspect(&root.join("amp-manager/notes.txt")).is_err());
        assert!(inspect(&root.join("amp-manager/nope.jsonl")).is_err());
        // Named right but gone: an entry saying it cannot be read, not a silent miss.
        let gone = inspect(&root.join("amp-manager/1000-id-gone.jsonl")).unwrap();
        assert_eq!(gone.id, "id-gone");
        assert!(!gone.resumable());
        assert!(gone.status.reason().unwrap().contains("cannot be read"));
    }

    #[test]
    fn previews_are_bounded_one_liners() {
        assert_eq!(one_line("  a\n\n b\t c  "), "a b c");
        assert_eq!(one_line("   "), "");
        let long = "z".repeat(PREVIEW_MAX_CHARS * 2);
        let elided = one_line(&long);
        assert_eq!(elided.chars().count(), PREVIEW_MAX_CHARS);
        assert!(elided.ends_with('…'));
        assert_eq!(
            one_line(&"z".repeat(PREVIEW_MAX_CHARS)).chars().count(),
            PREVIEW_MAX_CHARS
        );
    }

    #[test]
    fn sessions_root_is_the_state_dirs_sessions_directory() {
        assert_eq!(
            sessions_root(Path::new("/state/amp-harness")),
            PathBuf::from("/state/amp-harness/sessions")
        );
    }
}
