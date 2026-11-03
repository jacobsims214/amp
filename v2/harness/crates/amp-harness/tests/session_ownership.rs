//! #3006: every session writer owns its file for the storage's whole lifetime, and a resume takes
//! a saved session the same way.
//!
//! `amp-core`'s unit tests cover the mechanics (`session_store`); this is the end-to-end shape the
//! acceptance criteria are about, through `Runtime::build` — which is the one path both the chat's
//! manager session (`amp-harness/src/session.rs`) and a worker's (`amp-core/src/supervisor.rs`,
//! under `<state>/sessions/<ticket>/`) take:
//!
//! - a built session's file is owned from the moment it exists, and stays owned while the harness
//!   that writes it is alive — including after the `BuiltAgent` is dropped, because the guard lives
//!   in the session's storage and the harness keeps that;
//! - two sessions in one directory do not contend (the lock is per file);
//! - resuming a file a live harness is writing is refused, and the file is not even read for repair;
//! - resuming a *different* saved session onto a live harness moves it there and releases the old
//!   one — never two writers, never a file held by a harness that no longer writes it.
//!
//! Faux providers and the in-process mock AMP; every HOME, state dir and repo is under cargo's
//! target tmp dir (never /tmp: D13).

mod common;

use std::{
    path::{Path, PathBuf},
    sync::Arc,
};

use amp_core::{
    agent::{AgentSpec, Runtime, RuntimeOptions, SessionKind},
    project::ProjectMarker,
    providers::StaticKeys,
    session_lock::{SessionBusy, claim_session_lock},
    session_store::{self, OpenError},
};
use amp_mcp::{AmpMcpClient, ClientOptions};
use common::{MockAmp, Running, spawn};
use serde_json::json;

const CONFIG: &str = r#"
[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]

[roles.worker-backend]
model = "wf/wf-1"
"#;

struct Fixture {
    dir: PathBuf,
    repo: PathBuf,
    /// Kept alive for the test's lifetime: dropping it aborts the mock AMP's server.
    _amp: Running,
    rt: Arc<Runtime>,
}

/// A throwaway repo with `.amp.json`, its own HOME (the config above) and state dir, and a runtime
/// connected to a fresh mock AMP.
async fn fixture(name: &str) -> Fixture {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-session-ownership")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    let repo = dir.join("repo");
    std::fs::create_dir_all(&repo).unwrap();
    std::fs::create_dir_all(dir.join("home/.config/amp-harness")).unwrap();
    std::fs::write(dir.join("home/.config/amp-harness/config.toml"), CONFIG).unwrap();
    std::fs::write(
        repo.join(".amp.json"),
        json!({"project_id": 2, "project_name": "AMP v2"}).to_string(),
    )
    .unwrap();
    let repo = std::fs::canonicalize(&repo).unwrap();

    let amp = spawn(MockAmp::new(), None).await;
    let client = Arc::new(
        AmpMcpClient::connect(ClientOptions::new(amp.url.clone()))
            .await
            .expect("connect to the mock AMP"),
    );
    let home = dir.join("home");
    let env = {
        let home = home.clone();
        move |k: &str| match k {
            "HOME" => Some(home.to_string_lossy().into_owned()),
            "XDG_CONFIG_HOME" => Some(home.join(".config").to_string_lossy().into_owned()),
            "XDG_STATE_HOME" => Some(home.join(".local/state").to_string_lossy().into_owned()),
            _ => None,
        }
    };
    let rt = Runtime::load(
        client,
        RuntimeOptions {
            repo_root: Some(repo.clone()),
            models: None,
            keys: &StaticKeys::default(),
            env: &env,
            state_dir: None,
        },
    )
    .expect("load the runtime");
    Fixture {
        dir,
        repo,
        _amp: amp,
        rt: Arc::new(rt),
    }
}

impl Fixture {
    /// A worker's session directory, exactly as the supervisor picks it: `<state>/sessions/<id>/`.
    fn sessions_dir(&self, role_or_ticket: &str) -> PathBuf {
        self.rt.state_dir.join("sessions").join(role_or_ticket)
    }

    async fn build(&self, dir: &Path) -> amp_core::agent::BuiltAgent {
        let mut spec = AgentSpec::new("worker-backend", self.repo.clone());
        spec.project = ProjectMarker::find_within(&self.repo, Some(&self.dir)).unwrap();
        spec.session = SessionKind::Jsonl {
            dir: dir.to_path_buf(),
        };
        self.rt.build(spec).await.expect("build the session")
    }
}

/// Whether this process can take `path`'s lock right now.
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

/// The criterion `Runtime::build` has to satisfy: the file exists and is owned as long as the
/// harness that writes it can write it — not just as long as the `BuiltAgent` value lives.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_built_session_owns_its_file_for_the_harness_lifetime() {
    let f = fixture("build").await;
    let dir = f.sessions_dir("4242");
    let built = f.build(&dir).await;
    let path = built.session_file.clone().expect("a JSONL session");
    assert!(path.starts_with(&dir), "{path:?}");
    assert!(path.is_file(), "the header is on disk");
    assert!(!free(&path), "the created session owns its file");

    // The supervisor's shape: it keeps `Arc::clone(&built.harness)` and drops the rest of the
    // built agent. A guard kept beside the harness would be released here, and the worker's
    // appends would go into a file another process may already be writing.
    let harness = Arc::clone(&built.harness);
    drop(built);
    assert!(
        !free(&path),
        "the file is owned by the storage the harness writes through"
    );

    // A second session in the same directory is a different file, so it is not blocked.
    let other = f.build(&dir).await;
    let other_path = other.session_file.clone().expect("a JSONL session");
    assert_ne!(other_path, path);
    assert!(!free(&other_path));

    drop(harness);
    assert!(free(&path), "the last writer released it");
    drop(other);
    assert!(free(&other_path));
}

/// The real-world contention: a second chat (here, this test's own process standing in for another
/// one) resuming the session the live harness is writing. It is refused, and — because the claim
/// comes before anything reads the file — the session's bytes are untouched.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn resuming_the_session_a_live_harness_writes_is_refused_and_rewrites_nothing() {
    let f = fixture("contend").await;
    let dir = f.sessions_dir("4243");
    let built = f.build(&dir).await;
    let path = built.session_file.clone().expect("a JSONL session");
    let before = std::fs::read(&path).unwrap();

    let err = session_store::load(&path, &f.repo, "amp-manager")
        .await
        .unwrap_err();
    assert!(matches!(err, OpenError::Busy(_)), "{err}");
    assert!(
        err.held_by_this_process(),
        "this process is the holder, and the message says so: {err}"
    );
    assert_eq!(std::fs::read(&path).unwrap(), before, "nothing written");
    assert!(
        !free(&path),
        "and the live harness still owns it: a refusal never steals the file"
    );
}

/// Resuming another saved session onto a live harness: the harness moves to it, the old file is
/// released, and the loaded session keeps its guard.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_resume_moves_a_live_harness_and_releases_the_session_it_left() {
    let f = fixture("resume").await;
    let dir = f.sessions_dir("4244");
    let first = f.build(&dir).await;
    let first_path = first.session_file.clone().expect("a JSONL session");
    let first_bytes = std::fs::read(&first_path).unwrap();

    // A saved conversation to resume: written the same way a session is created, then released
    // (as a chat that exited would).
    let second_path = {
        let opened = session_store::create(&dir, &f.repo, "worker-backend")
            .await
            .expect("create the session to resume");
        assert!(opened.session.get_metadata().await.unwrap().created_at > 0);
        opened.path
    };
    assert!(free(&second_path), "its creator is gone");

    let resumed =
        amp_core::agent::resume_session(&first.harness, &second_path, &f.repo, "amp-manager")
            .await
            .expect("resume");
    assert_eq!(resumed, second_path);
    assert!(!free(&second_path), "the resumed session is owned now");
    assert!(
        free(&first_path),
        "the session it left is released: nothing is held that is not written"
    );
    assert_eq!(
        std::fs::read(&first_path).unwrap(),
        first_bytes,
        "and it is never written again"
    );
    // The harness is really on the second session: a run's entries land in that file.
    let model = first.harness.session().get_metadata().await.unwrap();
    let on_disk = std::fs::read_to_string(&second_path).unwrap();
    assert!(
        on_disk.contains(&model.id),
        "the harness writes the resumed file: {on_disk}"
    );
}
