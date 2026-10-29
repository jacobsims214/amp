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

    fn path(&self, task_id: i64) -> PathBuf {
        self.dir.join(format!("t{task_id}.json"))
    }

    /// Records that this process dispatched `task_id`. Last write wins (a re-dispatch of the same
    /// ticket updates the record rather than adding one).
    pub fn record(&self, task_id: i64, dispatch_id: &str, agent_id: &str) -> io::Result<()> {
        std::fs::create_dir_all(&self.dir)?;
        let rec = DispatchRecord {
            task_id,
            dispatch_id: dispatch_id.to_owned(),
            agent_id: agent_id.to_owned(),
            pid: self.pid,
            at_ms: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map_or(0, |d| d.as_millis() as i64),
        };
        let text = serde_json::to_string(&rec).map_err(io::Error::other)?;
        std::fs::write(self.path(task_id), text)
    }

    /// Forgets `task_id`: its worker closed the ticket, or it was handed back.
    pub fn clear(&self, task_id: i64) {
        let _ = std::fs::remove_file(self.path(task_id));
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
        out.sort_by_key(|r| r.task_id);
        out
    }

    /// The records whose process is no longer running: the tickets this machine dispatched that
    /// nobody is on any more. This process's own records are never orphans.
    pub fn orphans(&self) -> Vec<DispatchRecord> {
        self.records()
            .into_iter()
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

    #[test]
    fn a_record_is_written_read_and_cleared() {
        let state = tmp("record");
        let ledger = Ledger::open(&state);
        assert!(
            ledger.hold.is_some(),
            "this process holds its liveness lock"
        );
        ledger.record(7, "d_abc", "amp:amp-worker-backend").unwrap();
        let recs = ledger.records();
        assert_eq!(recs.len(), 1);
        assert_eq!(
            (recs[0].task_id, recs[0].dispatch_id.as_str()),
            (7, "d_abc")
        );
        assert_eq!(recs[0].pid, std::process::id());
        assert!(recs[0].at_ms > 0);
        // A re-dispatch updates the one record.
        ledger.record(7, "d_def", "amp:amp-worker-backend").unwrap();
        assert_eq!(ledger.records()[0].dispatch_id, "d_def");
        assert_eq!(ledger.records().len(), 1);
        // This process's own dispatch is never an orphan.
        assert!(ledger.orphans().is_empty());
        ledger.clear(7);
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
                dispatch_id: "d_live".into(),
                agent_id: "amp:amp-worker-docs".into(),
                pid: 4242,
                at_ms: 1,
            })
            .unwrap(),
        )
        .unwrap();
        let orphans = ledger.orphans();
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
        assert!(ledger.orphans().is_empty());
        assert!(ledger.record(1, "d_x", "amp:a").is_err());
    }
}
