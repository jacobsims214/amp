//! The dispatches this machine made, so a chat that dies can't leave tickets in progress
//! (AMP #2802, story 444).
//!
//! AMP's own watchdog returns a dispatched ticket nobody *started*, but a harness worker that
//! started its ticket and then lost its chat (the process died, was killed, or the machine
//! rebooted) leaves a **started** ticket with nobody on it, and AMP has no way to know. The
//! harness therefore keeps a small ledger of the tickets it dispatched:
//!
//! - Every `amp_run_ticket` that got as far as `amp_dispatch_task` writes one record under
//!   `<state>/dispatches/t<ticket>.json`: the ticket, the dispatch token, the agent, the process
//!   that dispatched it and when. The record is removed when the worker's ticket is closed or
//!   returned.
//! - Each harness process holds an exclusive lock on `<state>/dispatches/live-<pid>.lock` for as
//!   long as it runs ([`Ledger::open`], released by the OS when the process ends, however it
//!   ends). That lock is the liveness test: a record naming a pid whose lock can be taken belongs
//!   to a process that is gone.
//! - On startup ([`Supervisor::release_orphans`]) every record whose process is gone is handed
//!   back to ready in AMP and told to the person — a ticket is never left started with nobody on
//!   it just because a chat ended.
//!
//! It is deliberately per machine, not per AMP project: the state dir is already per user, and a
//! ticket id is unique across projects. A record whose pid was reused by another harness is
//! treated as live (the safe way round: AMP's watchdog still catches it later).
//!
//! **Per server (AMP #2948).** A ticket id is unique only within one AMP server, and one person
//! runs the harness against several (prod and a local cluster, switched with `amp-harness server
//! set` or `/server`). So every record names the server it was dispatched on ([`server_key`]: the
//! normalised MCP URL), its file name carries a hash of that server (`t<ticket>-<hash>.json`, so
//! prod's #5 and the local cluster's #5 are two records), and the startup sweep only looks at the
//! records of the server it is connected to ([`Ledger::orphans`]). Another server's records are
//! left on disk, untouched, for the harness that next talks to that server.
//!
//! A record written before #2948 has no server (`server: ""`, file `t<ticket>.json`): nothing
//! says which server its ticket is on. [`Ledger::orphans`] still returns it, and the sweep treats
//! it conservatively (see `Supervisor::release_orphans`): only its dispatch token can release it,
//! through `amp_return_task` alone, because a server that didn't issue that token refuses it.

use std::{
    fs::{File, OpenOptions},
    io,
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};

use serde::{Deserialize, Serialize};

/// Where the ledger lives under the harness state dir.
pub const LEDGER_DIR: &str = "dispatches";

/// One ticket this machine dispatched and hasn't seen closed or returned.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DispatchRecord {
    pub task_id: i64,
    /// The AMP server it was dispatched on, as [`server_key`] writes it
    /// (`https://amp.example.com/mcp`). Empty on a record written before AMP #2948: then nobody
    /// knows which server the ticket is on.
    #[serde(default)]
    pub server: String,
    /// The dispatch token AMP gave out (`d_…`); empty on a server too old to have one.
    #[serde(default)]
    pub dispatch_id: String,
    /// `amp:amp-worker-backend`.
    pub agent_id: String,
    /// The harness process that dispatched it. Its `live-<pid>.lock` is what says it still runs.
    pub pid: u32,
    /// When the dispatch was made (Unix milliseconds).
    pub at_ms: i64,
}

impl DispatchRecord {
    /// When this was dispatched, as a [`SystemTime`] (out of range: now).
    pub fn at(&self) -> SystemTime {
        u64::try_from(self.at_ms)
            .ok()
            .and_then(|ms| UNIX_EPOCH.checked_add(std::time::Duration::from_millis(ms)))
            .unwrap_or_else(SystemTime::now)
    }

    /// Written before AMP #2948, without the server it was dispatched on.
    pub fn is_legacy(&self) -> bool {
        self.server.is_empty()
    }
}

/// The identity of an AMP server in the ledger: its MCP URL normalised so two spellings of one
/// server compare equal: scheme and host lower-cased, a default port dropped, `127.0.0.1` and
/// `[::1]` written as `localhost`, any `user:password@` dropped, then the path without a trailing
/// `/` (query and fragment dropped). A URL with no scheme or host is kept as written (trimmed).
pub fn server_key(url: &str) -> String {
    let url = url.trim();
    let Some(origin) = crate::project::origin(url) else {
        return url.trim_end_matches('/').to_owned();
    };
    let rest = url.split_once("://").map_or("", |(_, r)| r);
    let path = rest
        .find('/')
        .map_or("", |at| &rest[at..])
        .split(['?', '#'])
        .next()
        .unwrap_or("")
        .trim_end_matches('/');
    format!("{origin}{path}")
}

/// The file a record of `task_id` on `server` is kept in, under the ledger dir:
/// `t<ticket>-<server tag>.json`; a legacy record (no server) is `t<ticket>.json`.
pub fn record_file_name(server: &str, task_id: i64) -> String {
    if server.is_empty() {
        format!("t{task_id}.json")
    } else {
        format!("t{task_id}-{}.json", server_tag(server))
    }
}

/// A short, stable name for `server` in a record's file name: FNV-1a 64 of [`server_key`]'s
/// output, in hex. (Not `DefaultHasher`, whose output may change between Rust releases: the
/// name must be the same for the next build that reads the ledger.)
fn server_tag(server: &str) -> String {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in server.as_bytes() {
        h ^= u64::from(*b);
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    format!("{h:016x}")
}

/// This machine's dispatch ledger. One per process, shared by that process's supervisors.
#[derive(Debug)]
pub struct Ledger {
    dir: PathBuf,
    pid: u32,
    /// Held for the life of the process: taking this lock is how another process learns we are
    /// gone. `None` when the lock file couldn't be created (the ledger then treats its own pid as
    /// live and reads no liveness from it).
    hold: Option<File>,
}

impl Ledger {
    /// Opens the ledger under `state_dir` (creating it) and takes this process's liveness lock.
    /// Failure is never fatal: a ledger that can't be written just doesn't help the next startup.
    pub fn open(state_dir: &Path) -> Self {
        let dir = state_dir.join(LEDGER_DIR);
        let pid = std::process::id();
        let hold = (|| {
            std::fs::create_dir_all(&dir).ok()?;
            let file = OpenOptions::new()
                .create(true)
                .truncate(false)
                .write(true)
                .open(dir.join(lock_name(pid)))
                .ok()?;
            file.try_lock().ok()?;
            Some(file)
        })();
        Self { dir, pid, hold }
    }

    /// The ledger's directory (`<state>/dispatches`).
    pub fn dir(&self) -> &Path {
        &self.dir
    }

    /// This process's id.
    pub fn pid(&self) -> u32 {
        self.pid
    }

    fn path(&self, server: &str, task_id: i64) -> PathBuf {
        self.dir.join(record_file_name(server, task_id))
    }

    /// Records that this process dispatched `task_id` on `server` (a [`server_key`]). Last write
    /// wins (a re-dispatch of the same ticket on the same server updates the record rather than
    /// adding one); the same id on another server is another record.
    pub fn record(
        &self,
        server: &str,
        task_id: i64,
        dispatch_id: &str,
        agent_id: &str,
    ) -> io::Result<()> {
        std::fs::create_dir_all(&self.dir)?;
        let rec = DispatchRecord {
            task_id,
            server: server.to_owned(),
            dispatch_id: dispatch_id.to_owned(),
            agent_id: agent_id.to_owned(),
            pid: self.pid,
            at_ms: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map_or(0, |d| d.as_millis() as i64),
        };
        let text = serde_json::to_string(&rec).map_err(io::Error::other)?;
        std::fs::write(self.path(server, task_id), text)
    }

    /// Forgets `task_id` on `server`: its worker closed the ticket, or it was handed back. Another
    /// server's record of the same id is not touched.
    pub fn clear(&self, server: &str, task_id: i64) {
        let _ = std::fs::remove_file(self.path(server, task_id));
    }

    /// Where `rec` is kept on disk.
    pub fn record_path(&self, rec: &DispatchRecord) -> PathBuf {
        self.path(&rec.server, rec.task_id)
    }

    /// Forgets the record `rec` (legacy or not).
    pub fn clear_record(&self, rec: &DispatchRecord) {
        self.clear(&rec.server, rec.task_id);
    }

    /// Every record on disk that can still be read (a torn or foreign file is skipped).
    pub fn records(&self) -> Vec<DispatchRecord> {
        let Ok(entries) = std::fs::read_dir(&self.dir) else {
            return Vec::new();
        };
        let mut out = Vec::new();
        for entry in entries.flatten() {
            let path = entry.path();
            if path.extension().is_none_or(|e| e != "json") {
                continue;
            }
            if let Ok(text) = std::fs::read_to_string(&path)
                && let Ok(rec) = serde_json::from_str::<DispatchRecord>(&text)
            {
                out.push(rec);
            }
        }
        out.sort_by(|a, b| (a.task_id, &a.server).cmp(&(b.task_id, &b.server)));
        out
    }

    /// The records on `server` (a [`server_key`]) whose process is no longer running: the
    /// tickets this machine dispatched there that nobody is on any more. This process's own
    /// records are never orphans. Another server's records are not returned (they are that
    /// server's to release, by a harness connected to it); legacy records with no server are
    /// ([`DispatchRecord::is_legacy`]), for the caller to treat with care.
    pub fn orphans(&self, server: &str) -> Vec<DispatchRecord> {
        self.records()
            .into_iter()
            .filter(|r| r.is_legacy() || r.server == server)
            .filter(|r| r.pid != self.pid && !self.pid_is_live(r.pid))
            .collect()
    }

    /// Whether the harness process `pid` still holds its liveness lock. Unreadable lock file:
    /// treated as live, the safe way round.
    fn pid_is_live(&self, pid: u32) -> bool {
        if pid == self.pid {
            return self.hold.is_some();
        }
        let Ok(file) = OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .open(self.dir.join(lock_name(pid)))
        else {
            return true;
        };
        // Free: nobody holds it, so that process ended (and released it however it ended).
        match file.try_lock() {
            Ok(()) => {
                let _ = file.unlock();
                let _ = std::fs::remove_file(self.dir.join(lock_name(pid)));
                false
            }
            Err(_) => true,
        }
    }
}

fn lock_name(pid: u32) -> String {
    format!("live-{pid}.lock")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmp(name: &str) -> PathBuf {
        // D13: never /tmp. cargo sets this for integration tests and benches; a unit test in the
        // library gets the same place through the env var, and a manual `cargo test` falls back
        // to the workspace's own target dir first.
        let base = std::env::var_os("CARGO_TARGET_TMPDIR")
            .map(PathBuf::from)
            .or_else(|| {
                std::env::var_os("CARGO_MANIFEST_DIR")
                    .map(|d| PathBuf::from(d).join("../../target").join("tmp"))
            })
            .unwrap_or_else(|| PathBuf::from("target/tmp"));
        let dir = base.join("amp-core-dispatch-ledger").join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    const PROD: &str = "https://amp.example.com/mcp";
    const LOCAL: &str = "http://localhost:18000/mcp";

    #[test]
    fn a_record_is_written_read_and_cleared() {
        let state = tmp("record");
        let ledger = Ledger::open(&state);
        assert!(
            ledger.hold.is_some(),
            "this process holds its liveness lock"
        );
        ledger
            .record(PROD, 7, "d_abc", "amp:amp-worker-backend")
            .unwrap();
        let recs = ledger.records();
        assert_eq!(recs.len(), 1);
        assert_eq!(
            (recs[0].task_id, recs[0].dispatch_id.as_str()),
            (7, "d_abc")
        );
        assert_eq!(recs[0].pid, std::process::id());
        assert_eq!(recs[0].server, PROD);
        assert!(recs[0].at_ms > 0);
        // A re-dispatch updates the one record.
        ledger
            .record(PROD, 7, "d_def", "amp:amp-worker-backend")
            .unwrap();
        assert_eq!(ledger.records()[0].dispatch_id, "d_def");
        assert_eq!(ledger.records().len(), 1);
        // This process's own dispatch is never an orphan.
        assert!(ledger.orphans(PROD).is_empty());
        ledger.clear(PROD, 7);
        assert!(ledger.records().is_empty());
    }

    #[test]
    fn a_dead_process_record_is_an_orphan_and_a_live_one_is_not() {
        let state = tmp("orphans");
        let ledger = Ledger::open(&state);
        // A record from a process that can't be running: pid 0 has no harness lock.
        std::fs::write(
            state.join(LEDGER_DIR).join("t11.json"),
            serde_json::to_string(&DispatchRecord {
                task_id: 11,
                server: PROD.into(),
                dispatch_id: "d_gone".into(),
                agent_id: "amp:amp-worker-docs".into(),
                pid: 0,
                at_ms: 1,
            })
            .unwrap(),
        )
        .unwrap();
        // A live one: a lock held by this process under another pid's name is still held.
        let held = OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .open(state.join(LEDGER_DIR).join(lock_name(4242)))
            .unwrap();
        held.try_lock().unwrap();
        std::fs::write(
            state.join(LEDGER_DIR).join("t12.json"),
            serde_json::to_string(&DispatchRecord {
                task_id: 12,
                server: PROD.into(),
                dispatch_id: "d_live".into(),
                agent_id: "amp:amp-worker-docs".into(),
                pid: 4242,
                at_ms: 1,
            })
            .unwrap(),
        )
        .unwrap();
        let orphans = ledger.orphans(PROD);
        assert_eq!(
            orphans.iter().map(|r| r.task_id).collect::<Vec<_>>(),
            vec![11],
            "{orphans:?}"
        );
        // A torn file is skipped, not fatal.
        std::fs::write(state.join(LEDGER_DIR).join("t13.json"), "{not json").unwrap();
        assert_eq!(ledger.records().len(), 2);
    }

    #[test]
    fn a_ledger_beside_an_unwritable_dir_does_not_fail() {
        let state = tmp("nowrite");
        let blocked = state.join("file-not-dir");
        std::fs::write(&blocked, "x").unwrap();
        let ledger = Ledger::open(&blocked);
        assert!(ledger.hold.is_none());
        assert!(ledger.records().is_empty());
        assert!(ledger.orphans(PROD).is_empty());
        assert!(ledger.record(PROD, 1, "d_x", "amp:a").is_err());
    }

    /// Writes `rec` where the ledger keeps it, as a process `rec.pid` would have.
    fn put(ledger: &Ledger, rec: &DispatchRecord) {
        std::fs::write(
            ledger.path(&rec.server, rec.task_id),
            serde_json::to_string(rec).unwrap(),
        )
        .unwrap();
    }

    fn dead(server: &str, task_id: i64) -> DispatchRecord {
        DispatchRecord {
            task_id,
            server: server.into(),
            dispatch_id: format!("d_{task_id}"),
            agent_id: "amp:amp-worker-backend".into(),
            pid: 0,
            at_ms: 1,
        }
    }

    /// AMP #2948: the same ticket id on two servers is two records; the sweep for one server sees
    /// only its own (and the legacy ones), and clearing one leaves the other.
    #[test]
    fn records_are_per_server_and_orphans_only_name_the_current_one() {
        let state = tmp("per-server");
        let ledger = Ledger::open(&state);
        put(&ledger, &dead(PROD, 5));
        put(&ledger, &dead(LOCAL, 5));
        put(&ledger, &dead(LOCAL, 6));
        // Written by a harness before #2948: no server field at all.
        std::fs::write(
            ledger.dir().join("t9.json"),
            r#"{"task_id":9,"dispatch_id":"d_old","agent_id":"amp:a","pid":0,"at_ms":1}"#,
        )
        .unwrap();
        assert_eq!(ledger.records().len(), 4, "{:?}", ledger.records());

        let on_prod = ledger.orphans(PROD);
        let names = |v: &[DispatchRecord]| {
            v.iter()
                .map(|r| (r.task_id, r.server.clone()))
                .collect::<Vec<_>>()
        };
        assert_eq!(
            names(&on_prod),
            vec![(5, PROD.to_owned()), (9, String::new())],
            "the local cluster's records are not prod's orphans"
        );
        assert!(on_prod[1].is_legacy());
        assert_eq!(on_prod[1].dispatch_id, "d_old");
        // A spelling of the same server finds the same records.
        assert_eq!(
            names(&ledger.orphans(&server_key("HTTP://127.0.0.1:18000/mcp/"))),
            vec![
                (5, LOCAL.to_owned()),
                (6, LOCAL.to_owned()),
                (9, String::new())
            ]
        );

        // Releasing prod's #5 leaves the local cluster's #5 where it is.
        ledger.clear(PROD, 5);
        assert_eq!(
            names(&ledger.records()),
            vec![
                (5, LOCAL.to_owned()),
                (6, LOCAL.to_owned()),
                (9, String::new())
            ]
        );
        let legacy = ledger.orphans(PROD).pop().unwrap();
        ledger.clear_record(&legacy);
        assert_eq!(ledger.records().len(), 2);
        assert!(ledger.orphans(PROD).is_empty());

        // A re-dispatch on one server overwrites only that server's record.
        ledger.record(LOCAL, 6, "d_new", "amp:a").unwrap();
        ledger.record(PROD, 6, "d_prod", "amp:a").unwrap();
        let sixes: Vec<_> = ledger
            .records()
            .into_iter()
            .filter(|r| r.task_id == 6)
            .map(|r| (r.server, r.dispatch_id))
            .collect();
        assert_eq!(
            sixes,
            vec![
                (LOCAL.to_owned(), "d_new".to_owned()),
                (PROD.to_owned(), "d_prod".to_owned())
            ]
        );
    }

    #[test]
    fn a_server_key_is_one_spelling_per_server() {
        for (input, want) in [
            ("https://AMP.example.com/mcp", "https://amp.example.com/mcp"),
            (
                "https://amp.example.com:443/mcp/",
                "https://amp.example.com/mcp",
            ),
            (
                "  https://amp.example.com/mcp?x=1#y ",
                "https://amp.example.com/mcp",
            ),
            (
                "https://user:secret@amp.example.com/mcp",
                "https://amp.example.com/mcp",
            ),
            ("http://127.0.0.1:18000/mcp", "http://localhost:18000/mcp"),
            ("http://[::1]:18000/mcp", "http://localhost:18000/mcp"),
            ("http://localhost:80", "http://localhost"),
            ("https://amp.example.com", "https://amp.example.com"),
            ("not a url/", "not a url"),
        ] {
            assert_eq!(server_key(input), want, "{input}");
        }
        // Different servers stay different, in the key and in the file name.
        assert_ne!(server_key(PROD), server_key(LOCAL));
        assert_ne!(server_tag(PROD), server_tag(LOCAL));
        assert_ne!(
            server_key("http://localhost:18000/mcp"),
            server_key("http://localhost:18001/mcp")
        );
        // The file-name tag is stable across builds (FNV-1a 64), not a per-process hash.
        assert_eq!(server_tag(""), "cbf29ce484222325");
        assert_eq!(server_tag("a"), "af63dc4c8601ec8c");
    }
}
