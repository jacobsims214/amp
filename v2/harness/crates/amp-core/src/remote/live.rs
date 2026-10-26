//! Remote sessions this harness install is running, and the ones a dead process left behind
//! (#2424).
//!
//! Since #2424 a remote session has no idle limit or lifetime by default: it lives until a person
//! ends it. A chat that is killed (SIGKILL, a crash, a reboot) never says goodbye, so its session
//! stays listed in AMP as live and offline. The chat can't be resumed (the harness has no chat
//! resume), so nothing would ever end it but Disconnect on the web page, and enough of them would
//! use up the person's 5 live sessions.
//!
//! So each running session holds a record, `<state>/remote-live/<session_id>.json` (0600: the AMP
//! origin, session id and harness id; never a token), with an exclusive lock on the open file for
//! as long as the process lives. The OS drops the lock when the process dies. When the person
//! turns `/remote` on again on this computer, every record nobody holds a lock on (for the same
//! AMP origin) is a session left behind: the link records its end in the pending ends
//! ([`super::pending`]), which a token refresh signed with the harness key then `DELETE` send to
//! the relay, and removes the record. A session still running in another chat keeps its lock and
//! is left alone. A clean end removes the record itself.

use std::{
    fs::{File, OpenOptions},
    io::Write as _,
    path::{Path, PathBuf},
};

use serde::{Deserialize, Serialize};

/// The directory, in the harness's state directory.
pub const LIVE_DIR: &str = "remote-live";

/// What a record holds.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LiveEntry {
    pub v: u32,
    /// The AMP origin the session lives on: only that origin is ever told it ended.
    pub api_base: String,
    pub session_id: String,
    /// The harness key the session was registered with.
    pub harness_id: String,
    pub started_at_ms: i64,
}

/// A running session's record: the lock lives as long as this value (and the process).
#[derive(Debug)]
pub struct LiveRecord {
    path: PathBuf,
    _file: File,
}

impl LiveRecord {
    /// The record's path.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// The session ended cleanly (or its end is now a pending end): remove the record.
    pub fn release(self) {
        if let Err(e) = std::fs::remove_file(&self.path)
            && e.kind() != std::io::ErrorKind::NotFound
        {
            tracing::warn!(error = %e, "remote: the live session record could not be removed");
        }
    }
}

fn safe_name(session_id: &str) -> bool {
    !session_id.is_empty()
        && session_id.len() <= 64
        && session_id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

fn open_private(path: &Path) -> std::io::Result<File> {
    let mut o = OpenOptions::new();
    o.create(true).write(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        o.mode(0o600);
    }
    o.open(path)
}

/// Writes and locks the record of a session this process is now running. The file is locked
/// before it gets its final name, so another process never sees it unlocked.
pub fn claim(dir: &Path, entry: &LiveEntry) -> Result<LiveRecord, String> {
    if !safe_name(&entry.session_id) {
        return Err("the session id isn't a file name".to_owned());
    }
    std::fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
    let path = dir.join(format!("{}.json", entry.session_id));
    let tmp = dir.join(format!("{}.json.tmp", entry.session_id));
    let mut file = open_private(&tmp).map_err(|e| format!("create {}: {e}", tmp.display()))?;
    file.try_lock()
        .map_err(|e| format!("lock {}: {e}", tmp.display()))?;
    let text = serde_json::to_vec(entry).map_err(|e| e.to_string())?;
    file.write_all(&text)
        .and_then(|()| file.sync_all())
        .map_err(|e| format!("write {}: {e}", tmp.display()))?;
    std::fs::rename(&tmp, &path).map_err(|e| format!("rename {}: {e}", path.display()))?;
    Ok(LiveRecord { path, _file: file })
}

/// The sessions for `api_base` whose process is gone (nobody holds their record's lock). Their
/// records are removed: the caller records their ends. Records for other AMP origins, and ones a
/// running chat holds, are left alone; an unreadable one is removed.
pub fn take_orphans(dir: &Path, api_base: &str) -> Vec<LiveEntry> {
    let Ok(read) = std::fs::read_dir(dir) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for item in read.flatten() {
        let path = item.path();
        if path.extension().and_then(|e| e.to_str()) != Some("json") {
            continue;
        }
        let Ok(file) = File::open(&path) else {
            continue;
        };
        // A running chat holds this lock; a dead one doesn't.
        if file.try_lock().is_err() {
            continue;
        }
        let entry = std::fs::read(&path)
            .ok()
            .and_then(|b| serde_json::from_slice::<LiveEntry>(&b).ok());
        match entry {
            Some(e) if e.api_base != api_base => continue,
            Some(e) => out.push(e),
            None => tracing::warn!("remote: an unreadable live session record was removed"),
        }
        let _ = std::fs::remove_file(&path);
        drop(file);
    }
    out.sort_by_key(|e| e.started_at_ms);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry(id: &str, base: &str) -> LiveEntry {
        LiveEntry {
            v: 1,
            api_base: base.into(),
            session_id: id.into(),
            harness_id: "h".into(),
            started_at_ms: 1,
        }
    }

    fn dir(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!(
            "amp-live-{tag}-{}-{}",
            std::process::id(),
            crate::remote::crypto::b64u(&crate::remote::crypto::random_bytes(6))
        ));
        let _ = std::fs::remove_dir_all(&d);
        d
    }

    #[test]
    fn a_held_record_is_not_an_orphan_and_a_dropped_one_is() {
        let d = dir("held");
        let held = claim(&d, &entry("rs_held", "https://amp.example")).unwrap();
        let dead = claim(&d, &entry("rs_dead", "https://amp.example")).unwrap();
        let other = claim(&d, &entry("rs_other", "https://other.example")).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(held.path()).unwrap().permissions().mode();
            assert_eq!(mode & 0o777, 0o600);
        }
        // The process "dies": its lock goes with the handle, the file stays.
        drop(dead);
        drop(other);
        let found = take_orphans(&d, "https://amp.example");
        assert_eq!(
            found
                .iter()
                .map(|e| e.session_id.as_str())
                .collect::<Vec<_>>(),
            ["rs_dead"]
        );
        assert!(!d.join("rs_dead.json").exists());
        assert!(
            d.join("rs_held.json").exists(),
            "a live session is left alone"
        );
        assert!(
            d.join("rs_other.json").exists(),
            "another AMP's is left alone"
        );
        assert!(take_orphans(&d, "https://amp.example").is_empty());
        held.release();
        assert!(!d.join("rs_held.json").exists());
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn a_bad_session_id_is_not_a_path() {
        let d = dir("bad");
        assert!(claim(&d, &entry("../x", "b")).is_err());
        assert!(claim(&d, &entry("", "b")).is_err());
        let _ = std::fs::remove_dir_all(&d);
    }
}
