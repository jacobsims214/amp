//! Session ends the relay hasn't heard yet (#2396).
//!
//! `/remote off` (or quitting the chat) while AMP's relay is down turns remote control off on the
//! laptop at once, but the relay never gets the `DELETE`, so AMP keeps listing the session as live
//! and offline until its idle limit. The link records such an end here, in
//! `<state>/remote-pending-ends.json` (0600), and sends it later: in the background while the chat
//! runs, and on the next start. Sending it is a token refresh signed with the harness key
//! (protocol 8.2), then `DELETE` (8.2). No connect token is ever written to the file.
//!
//! An entry is dropped once the relay answers that the session is over (204, `410`, `404`), when
//! it can never succeed (another `4xx`), or once its deadline has passed: by then the relay has
//! ended the session itself (the idle limit or the 12 hour lifetime).

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use super::relay::RelayError;

/// The file, in the harness's state directory.
pub const PENDING_ENDS_FILE: &str = "remote-pending-ends.json";

/// At most this many entries are kept (the oldest go first).
pub const MAX_PENDING: usize = 32;

/// One session end to send.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct PendingEnd {
    /// The AMP origin the session lives on: an end is only ever sent there.
    pub api_base: String,
    pub session_id: String,
    /// The harness key the session was registered with (only that key can prove the end).
    pub harness_id: String,
    /// The `DELETE` reason (`harness_ended` or `detached`).
    pub reason: String,
    /// When the relay will have ended the session by itself (its idle deadline or lifetime).
    pub until_ms: i64,
}

#[derive(Debug, Default, Serialize, Deserialize)]
struct PendingFile {
    v: u32,
    ends: Vec<PendingEnd>,
}

/// What became of one attempt.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    /// The relay ended it, or says it's already over: drop it.
    Done,
    /// It can never succeed (refused proof, bad request): drop it.
    GiveUp,
    /// The relay is still out of reach (network, `5xx`, `429`) or the login needs renewing
    /// (`401`): keep it.
    Retry,
}

/// How an error from the refresh or the `DELETE` counts.
pub fn outcome(e: &RelayError) -> Outcome {
    if e.ended_reason().is_some() || e.status == 404 {
        return Outcome::Done;
    }
    // The org switch is off: the relay ended every session of the org already.
    if e.status == 403 && e.code.as_deref() == Some("remote_disabled") {
        return Outcome::Done;
    }
    match e.status {
        0 | 401 | 408 | 429 => Outcome::Retry,
        s if s >= 500 => Outcome::Retry,
        _ => Outcome::GiveUp,
    }
}

/// Where the entries are kept: the file, or memory (tests, and a link without a state dir).
#[derive(Debug)]
pub struct PendingStore {
    path: Option<PathBuf>,
    mem: Vec<PendingEnd>,
}

impl PendingStore {
    pub fn new(path: Option<PathBuf>) -> Self {
        Self {
            path,
            mem: Vec::new(),
        }
    }

    pub fn path(&self) -> Option<&Path> {
        self.path.as_deref()
    }

    /// Every entry still before its deadline. A file that can't be read counts as empty.
    pub fn load(&self, now_ms: i64) -> Vec<PendingEnd> {
        let all = match &self.path {
            None => self.mem.clone(),
            Some(path) => match std::fs::read_to_string(path) {
                Ok(text) => serde_json::from_str::<PendingFile>(&text)
                    .map(|f| f.ends)
                    .unwrap_or_else(|e| {
                        tracing::warn!(error = %e, "remote: the pending ends file is unreadable");
                        Vec::new()
                    }),
                Err(_) => Vec::new(),
            },
        };
        all.into_iter().filter(|e| e.until_ms > now_ms).collect()
    }

    fn save(&mut self, mut ends: Vec<PendingEnd>) -> Result<(), String> {
        if ends.len() > MAX_PENDING {
            let drop = ends.len() - MAX_PENDING;
            ends.drain(..drop);
        }
        match &self.path {
            None => {
                self.mem = ends;
                Ok(())
            }
            Some(path) if ends.is_empty() => match std::fs::remove_file(path) {
                Err(e) if e.kind() != std::io::ErrorKind::NotFound => {
                    Err(format!("remove {}: {e}", path.display()))
                }
                _ => Ok(()),
            },
            Some(path) => {
                if let Some(dir) = path.parent() {
                    let _ = std::fs::create_dir_all(dir);
                }
                let text = serde_json::to_string_pretty(&PendingFile { v: 1, ends })
                    .map_err(|e| e.to_string())?;
                super::crypto::write_private(path, text.as_bytes())
            }
        }
    }

    /// Records `end` (replacing an entry for the same session).
    pub fn add(&mut self, end: PendingEnd, now_ms: i64) -> Result<(), String> {
        let mut ends = self.load(now_ms);
        ends.retain(|e| e.session_id != end.session_id);
        ends.push(end);
        self.save(ends)
    }

    /// Drops the entries for `sessions` (and every expired one).
    pub fn remove(&mut self, sessions: &[String], now_ms: i64) -> Result<(), String> {
        let mut ends = self.load(now_ms);
        ends.retain(|e| !sessions.contains(&e.session_id));
        self.save(ends)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{Value, json};

    fn err(status: u16, code: Option<&str>, body: Value) -> RelayError {
        RelayError {
            status,
            code: code.map(str::to_owned),
            message: String::new(),
            body: Box::new(body),
        }
    }

    fn end(id: &str, until_ms: i64) -> PendingEnd {
        PendingEnd {
            api_base: "https://amp.example".into(),
            session_id: id.into(),
            harness_id: "h".into(),
            reason: "harness_ended".into(),
            until_ms,
        }
    }

    #[test]
    fn what_each_answer_means() {
        use Outcome::*;
        for (e, want) in [
            (err(0, None, Value::Null), Retry),
            (err(503, None, Value::Null), Retry),
            (err(502, None, Value::Null), Retry),
            (err(503, Some("relay_unavailable"), json!({})), Retry),
            (err(401, Some("token_expired"), json!({})), Retry),
            (err(401, Some("unauthenticated"), json!({})), Retry),
            (err(429, Some("rate_limited"), json!({})), Retry),
            (
                err(410, Some("session_ended"), json!({"reason": "idle"})),
                Done,
            ),
            (err(404, Some("not_found"), json!({})), Done),
            (err(403, Some("remote_disabled"), json!({})), Done),
            (err(403, Some("bad_signature"), json!({})), GiveUp),
            (err(400, Some("wrong_harness"), json!({})), GiveUp),
        ] {
            assert_eq!(outcome(&e), want, "{e}");
        }
    }

    #[test]
    fn the_file_keeps_entries_until_their_deadline_and_is_private() {
        let dir = std::env::temp_dir().join(format!(
            "amp-pending-ends-{}",
            super::super::crypto::b64u(&super::super::crypto::random_bytes(6))
        ));
        let path = dir.join(PENDING_ENDS_FILE);
        let mut store = PendingStore::new(Some(path.clone()));
        assert!(store.load(0).is_empty());
        store.add(end("rs_a", 1_000), 0).unwrap();
        store.add(end("rs_b", 5_000), 0).unwrap();
        store.add(end("rs_a", 2_000), 0).unwrap(); // replaces
        let again = PendingStore::new(Some(path.clone()));
        assert_eq!(again.load(0), vec![end("rs_b", 5_000), end("rs_a", 2_000)]);
        assert_eq!(again.load(3_000), vec![end("rs_b", 5_000)]);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode, 0o600);
        }
        let text = std::fs::read_to_string(&path).unwrap();
        assert!(!text.contains("rct_"), "no connect token is stored");
        store.remove(&["rs_b".into()], 3_000).unwrap();
        // rs_a expired and rs_b removed: the file is gone.
        assert!(!path.exists());
        let mut many = PendingStore::new(None);
        for i in 0..(MAX_PENDING + 3) {
            many.add(end(&format!("rs_{i}"), 10), 0).unwrap();
        }
        let kept = many.load(0);
        assert_eq!(kept.len(), MAX_PENDING);
        assert_eq!(kept[0].session_id, "rs_3");
        let _ = std::fs::remove_dir_all(dir);
    }
}
