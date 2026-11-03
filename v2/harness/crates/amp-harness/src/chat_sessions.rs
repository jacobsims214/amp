//! `/sessions`'s binary side (AMP #3012): the scan the picker draws, and the one session-switch
//! transaction a confirmed pick asks for.
//!
//! The picker itself is `amp-tui`'s (`PanelKind::Sessions`, #3010): it browses a listing and hands a
//! confirmed `ResumeRequest` to whatever implements [`amp_tui::SessionControl`]. This module is that
//! implementation, and deliberately the only place in the binary that opens a saved session for
//! writing, so "refuse the busy states, load under the lock, swap the harness, hand back the
//! transcript" cannot drift apart into two half-transactions.
//!
//! **The transaction.** [`ChatSessions::switch`] runs these in order, and every step refuses rather
//! than half-way does its job:
//!
//! 1. **Busy probe** — [`session_lock::claim_session_lock_as`], one non-blocking `try_lock`, dropped
//!    at once. A file another live harness is writing is refused in the picker's own words *before*
//!    anything about the harness is touched. It is the same lock [`amp_core::session_store`] takes
//!    for real below, so it is not a second opinion: it is the cheap probe that makes the refusal
//!    cost nothing and leaves the file unopened. If something wins the race between the two, `load`
//!    refuses with the same variant.
//! 2. **Read the transcript back** — display only ([`restore`]): `worker_session::replay` turns the
//!    file's messages into `AgentEvent`s for the blocks the chat already draws. It only reads, so it
//!    runs **before** the swap: a read that fails is then a plain refusal, never a switched context
//!    under the old view. Nothing is executed: an `AgentEvent` is what a viewer sees, so a recorded
//!    `amp_run_ticket` cannot dispatch anything, a recorded tool call cannot run, and the dispatch
//!    ledger is never touched. The next turn's *context* comes from the same file through rpi's own
//!    per-run read (`run_core` → `build_session_context`), so the transcript shown and the history
//!    the model gets agree.
//! 3. **Load and swap** — [`amp_core::agent::resume_session`]: `session_store::load` validates the
//!    header, refuses another directory, a foreign name, an unreadable or older-format file, claims
//!    the guard, and only then replays (so a repair runs under it). It then **cancels every prompt
//!    the file still had queued** — `set_session` rebuilds rpi's queues from the durable records, so
//!    a session saved mid-reply would otherwise hand the next turn prompts typed in a chat that is
//!    over. Finally `AgentHarness::set_session` publishes the swap: it rejects with `LaneBusy` while
//!    a run is active, rebuilds the (now empty) queues, and clears `pending_resume`. The guard
//!    travels inside the new storage, so it is held for exactly as long as the harness may write
//!    that file, and the file this chat was writing is released as the old `Session` drops.
//!
//! Nothing is rebuilt: no `Runtime`, no `AgentHarness::create`, no `SessionKind` load variant. The
//! harness keeps the model, tools, policy and system prompt it was built with for this directory —
//! which is why a session started elsewhere is refused — and [`Resumed::model`] reports the *live*
//! harness's model (read at switch time, so a `/model` change is reflected), so the resumed line says
//! what the conversation will actually run on rather than what the old file recorded. No automatic
//! continuation of a dead run crosses over either: `set_session` clears `pending_resume` rather than
//! re-issuing it.

use std::{
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

use amp_core::{
    agent::{ResumeError, resume_session},
    session_index::{self, EntryStatus},
    session_lock::{self, SessionBusy},
    worker_session::{self, TimedEvent},
};
use amp_tui::{
    ResumeFuture, ResumeRequest, Resumed, SessionControl, SessionList, SessionListFuture,
    SessionProblem, SessionRow,
};
use rpi_agent::AgentEvent;
use rpi_harness::agent_harness::{AgentHarness, AgentLane};

/// What a switch needs, and nothing else: cheap enough to clone into the future `resume` returns.
#[derive(Clone)]
struct Switch {
    harness: Arc<AgentHarness>,
    /// The canonical working directory. Both the listing (the header's `cwd` match) and the load
    /// (`session_store::load`'s `OtherDirectory` refusal) are scoped to it: a session started
    /// elsewhere was run with that directory's tools and policy, so it is refused by name.
    cwd: PathBuf,
    /// `<state>/sessions`, the root of every role directory (`session_index::list`'s scan root).
    sessions_root: PathBuf,
    /// The role directory this chat writes (`amp-manager`).
    role: String,
    /// The holder stamp's agent id (`amp-manager`, `amp-worker-backend`).
    agent_id: String,
    /// `provider/model` the live harness runs on. A hot-swap keeps it, so it is what the resumed
    /// conversation runs on — never the loaded file's recorded `model_change`.
    model: String,
    /// The file this chat writes right now, marked `· current` and unpickable. Behind a lock because
    /// a successful switch replaces it without rebuilding this control.
    current: Arc<Mutex<Option<PathBuf>>>,
}

/// The chat's `/sessions` control for one role in one directory (AMP #3012).
pub struct ChatSessions(Switch);

impl ChatSessions {
    /// The control for a chat writing `current` (the file `session::build` created), running `model`
    /// on `harness` in `cwd`.
    ///
    /// `current` is the shared handle the caller keeps: a successful switch writes the newly
    /// resumed file into it, so the rest of the chat (the exit line, which names the file the
    /// conversation was saved to) reads the same answer the picker's `· current` mark does, instead
    /// of the one it started with. `None` initially for `--no-session`, where a resume is what makes
    /// the chat write a file at all.
    pub fn new(
        harness: Arc<AgentHarness>,
        cwd: &Path,
        sessions_root: &Path,
        role: &str,
        agent_id: &str,
        model: String,
        current: Arc<Mutex<Option<PathBuf>>>,
    ) -> Self {
        Self(Switch {
            harness,
            cwd: cwd.to_path_buf(),
            sessions_root: sessions_root.to_path_buf(),
            role: role.to_owned(),
            agent_id: agent_id.to_owned(),
            model,
            current,
        })
    }

    /// This chat's own file, as the picker marks it.
    fn current(&self) -> Option<PathBuf> {
        self.0.current.lock().ok().and_then(|c| c.clone())
    }

    /// The scan, as the picker's rows: `session_index::list`'s entries (bounded, read-only, exact
    /// `cwd` match) with the one fact the scan cannot know — whether a **live writer** holds the file
    /// — filled in here.
    ///
    /// The liveness probe is one non-blocking `try_lock` per openable session, dropped at once; the
    /// scan itself takes no lock (the ownership contract requires one only for access that may
    /// write). That is what turns "this row would fail after you picked it" into a `locked` row
    /// carrying the reason, as artifact #28 draws it. A probe that fails as `Unavailable` leaves the
    /// row as scanned: a sidecar we could not open is not evidence that someone else has the file,
    /// and the load will say what is actually wrong.
    fn scan(&self) -> Result<SessionList, String> {
        let entries = session_index::list(&self.0.sessions_root, &self.0.cwd)?;
        let current = self.current();
        let rows = entries
            .into_iter()
            .map(|entry| {
                let problem = match &entry.status {
                    // #3014: the file *this* chat is writing is not a problem row. It is held, by
                    // this process, and it is offered as `· current` — which is a state, not a
                    // reason it cannot be resumed (it cannot: it is the one being resumed onto).
                    // Probing it would draw `· current · locked · running elsewhere`, and count
                    // it as un-resumable in the title, both of which are untrue of the session
                    // the person is sitting in (artifact #28: the current session is present and
                    // well, just not pickable). The `Held` wording still reaches a *transaction*
                    // that names this file — see [`busy`] — where "this chat has it open" is the
                    // useful sentence.
                    EntryStatus::Ok if current.as_ref() == Some(&entry.path) => None,
                    EntryStatus::Ok => busy(&entry.path).map(SessionProblem::Locked),
                    EntryStatus::Unreadable(why) => Some(SessionProblem::Unreadable(why.clone())),
                    EntryStatus::Incompatible(why) => {
                        Some(SessionProblem::Incompatible(why.clone()))
                    }
                };
                SessionRow {
                    path: entry.path,
                    id: short_id(&entry.id),
                    role: role_label(&entry.role, &self.0.role),
                    modified_at: entry.modified_at,
                    // The latest thing said, else the opening prompt: what the row's second line
                    // shows. `None` for a session with no messages yet.
                    preview: entry.latest.or(entry.opening),
                    problem,
                }
            })
            .collect();
        Ok(SessionList {
            scope: self.0.cwd.display().to_string(),
            rows,
            current,
        })
    }
}

impl SessionControl for ChatSessions {
    fn list(&self) -> SessionListFuture {
        let listed = self.scan();
        Box::pin(async move { listed })
    }

    fn resume(&self, request: ResumeRequest) -> ResumeFuture {
        let switch = self.0.clone();
        Box::pin(async move { switch.switch(&request.path).await })
    }
}

impl Switch {
    /// The one switch transaction. `Err` leaves the harness on the session it had, writing where it
    /// was writing, and the file that was being resumed byte-identical (every refusal happens before
    /// `load`'s repairing read).
    async fn switch(&self, path: &Path) -> Result<Resumed, String> {
        // 1. The cheap liveness refusal, before anything about the harness is touched.
        if let Some(why) = busy(path) {
            return Err(why);
        }
        // 2. Read the transcript **before** committing to the swap. `replay` only reads the file:
        //    no tool runs, no model call. Preparing it first is what makes a read failure a plain
        //    refusal — the harness, its guard and the chat's view all stay as they were. Reading it
        //    afterwards would mean a failed read could only be swallowed after the context had
        //    already moved, leaving the person looking at the old transcript while the next turn
        //    continued the new session.
        let transcript = restore(path)?;
        // 3. Load under the guard, then publish the swap. `resume_session` refuses another
        //    directory, a foreign or damaged file, a busy one, and drops what the file still had
        //    queued; `set_session` refuses while a run is active. Each leaves the old session and
        //    its queues untouched.
        let resumed = resume_session(&self.harness, path, &self.cwd, &self.agent_id)
            .await
            .map_err(|e| switch_message(&e))?;
        // The resumed file is the one this chat writes from now on.
        if let Ok(mut current) = self.current.lock() {
            *current = Some(resumed.clone());
        }
        Ok(Resumed {
            messages: message_count(&transcript),
            path: resumed,
            // The live harness keeps its model across a swap, and `/model` can have changed it
            // since this control was built: report what the next turn will actually run on. The
            // loaded file's own `model_change` entries are deliberately not applied (#3012).
            model: Some(self.live_model().await),
            transcript,
        })
    }

    /// The `provider/model` the live harness runs on now, falling back to the one it was built with
    /// when the harness cannot say (a closed harness fails the switch anyway).
    async fn live_model(&self) -> String {
        match self.harness.get_model().await {
            Ok(model) => format!("{}/{}", model.provider, model.id),
            Err(_) => self.model.clone(),
        }
    }
}

/// The reason a live writer holds `path`, in the picker's words, or `None` when the file is free.
///
/// One `try_lock`, dropped immediately — the same guard `session_store` takes for real later. A file
/// this process already owns says so: "switch to a different session, or keep talking here" is the
/// useful sentence there, not "another live process has it", which sends the person looking for a
/// process that isn't there. The stamp only decides the wording (a pid can be reused); the lock
/// result is what decides ownership.
fn busy(path: &Path) -> Option<String> {
    match session_lock::claim_session_lock_as(path, "") {
        Ok(guard) => {
            drop(guard);
            None
        }
        Err(SessionBusy::Held { holder, .. }) => Some(match holder {
            Some(stamp) if stamp.pid == std::process::id() => {
                "this chat already has it open · switch to a different session, or keep talking here"
                    .to_owned()
            }
            Some(stamp) => format!("{stamp} · running elsewhere", stamp = stamp.who()),
            None => "another live harness has it".to_owned(),
        }),
        // Not `Held`: the sidecar could not be opened or locked at all. Not evidence of a writer.
        Err(SessionBusy::Unavailable { .. }) => None,
    }
}

/// What a failed switch says in the transcript: the refusal in words, plus the next step for the
/// case that has one (the session this chat already has open).
fn switch_message(e: &ResumeError) -> String {
    let mut text = e.to_string();
    if e.held_by_this_process() {
        text.push_str(
            " (this chat has it open: switch to a different session, or keep talking here)",
        );
    }
    text
}

/// The loaded session's transcript for display: the replay a worker's saved session uses
/// ([`amp_core::worker_session::replay`]), so the blocks, tool results and durations render exactly
/// as they did live. Display only — see the module docs.
///
/// It is called *before* the swap (see [`Switch::switch`]), so a read that fails is a refusal rather
/// than a switch whose view and context disagree: the `Err` reaches the transcript as the reason the
/// chat is unchanged, and nothing was committed.
fn restore(path: &Path) -> Result<Vec<TimedEvent>, String> {
    worker_session::replay(path).map_err(|why| {
        format!(
            "its transcript could not be read ({why}); the session already open is unchanged and \
             nothing was written"
        )
    })
}

/// How many messages the transcript holds, for the resumed line: a message that ended and a tool
/// call that returned, which is what a person counts as messages. 0 when the replay produced nothing
/// (the caller falls back to the count the scan showed).
fn message_count(events: &[TimedEvent]) -> u64 {
    events
        .iter()
        .filter(|(_, e)| {
            matches!(
                e,
                AgentEvent::MessageEnd { .. } | AgentEvent::ToolExecutionEnd { .. }
            )
        })
        .count() as u64
}

/// `01a1655c-3f27-76e5-a3f9-a4e1bec7c795` → `01a1…c795`, the short id the rest of the chrome shows
/// (`chat.rs`'s banner draws the same from a file name).
fn short_id(id: &str) -> String {
    let chars: Vec<char> = id.chars().collect();
    if chars.len() <= 9 {
        return id.to_owned();
    }
    let head: String = chars[..4].iter().collect();
    let tail: String = chars[chars.len() - 4..].iter().collect();
    format!("{head}…{tail}")
}

/// The role a row shows: empty for this chat's own role (the row does not repeat it), the directory
/// name for another role's sessions.
fn role_label(found: &str, own: &str) -> String {
    if found == own {
        String::new()
    } else {
        found.to_owned()
    }
}

#[cfg(test)]
mod tests {
    use super::{ChatSessions, message_count, restore, role_label, short_id};
    use amp_tui::SessionControl;
    use rpi_agent::{AgentEvent, AgentMessage};

    #[test]
    fn the_row_names_a_session_the_way_the_banner_does() {
        assert_eq!(
            short_id("01a1655c-3f27-76e5-a3f9-a4e1bec7c795"),
            "01a1…c795"
        );
        // Too short to elide: shown whole (as chat.rs' session_id does).
        assert_eq!(short_id("abc"), "abc");
        assert_eq!(short_id("123456789"), "123456789");
    }

    #[test]
    fn a_row_repeats_the_role_only_when_it_is_not_this_chats() {
        assert_eq!(role_label("amp-manager", "amp-manager"), "");
        assert_eq!(
            role_label("amp-worker-backend", "amp-manager"),
            "amp-worker-backend"
        );
    }

    #[test]
    fn the_message_count_counts_ended_messages_and_returned_calls() {
        use rpi_ai::types::{UserContent, UserMessage};

        // What replay yields for one turn: the prompt, the reply, and a call it made.
        let user = AgentEvent::MessageEnd {
            message: AgentMessage::User(UserMessage {
                role: Default::default(),
                content: UserContent::Text("hello".into()),
                timestamp: 0,
            }),
        };
        let reply = AgentEvent::MessageEnd {
            message: reply_message(),
        };
        let call = AgentEvent::ToolExecutionEnd {
            tool_call_id: "c1".into(),
            tool_name: "bash".into(),
            result: rpi_agent::AgentToolResult::text("ok"),
            is_error: false,
        };
        // Streaming frames and turn boundaries are not messages.
        let streaming = AgentEvent::MessageStart {
            message: reply_message(),
        };
        let events = vec![(None, user), (None, streaming), (None, reply), (None, call)];
        assert_eq!(message_count(&events), 3);
        assert_eq!(message_count(&[]), 0);
    }

    /// Any message, for the streaming frame the count must not include.
    fn reply_message() -> AgentMessage {
        use rpi_ai::providers::faux::{FauxBlock, faux_assistant_message};
        use rpi_ai::types::StopReason;
        AgentMessage::Assistant(Box::new(faux_assistant_message(
            FauxBlock::text("hi"),
            StopReason::Stop,
        )))
    }

    // -- The switch transaction, end to end (AMP #3012) --------------------------------------

    /// A directory of its own under cargo's target tmp dir (D13: never /tmp).
    fn tmpdir(name: &str) -> std::path::PathBuf {
        let base = std::env::var_os("CARGO_TARGET_TMPDIR")
            .map(std::path::PathBuf::from)
            .or_else(|| {
                std::env::var_os("CARGO_MANIFEST_DIR")
                    .map(|d| std::path::PathBuf::from(d).join("../../target").join("tmp"))
            })
            .unwrap_or_else(|| std::path::PathBuf::from("target/tmp"));
        let dir = base.join("amp-harness-chat-sessions").join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::canonicalize(&dir).unwrap()
    }

    fn user(text: &str) -> AgentMessage {
        use rpi_ai::types::{UserContent, UserMessage};
        AgentMessage::User(UserMessage {
            role: Default::default(),
            content: UserContent::Text(text.to_owned()),
            timestamp: 1_767_225_600_000,
        })
    }

    /// A harness on an in-memory session (no file of its own to guard).
    async fn harness() -> std::sync::Arc<rpi_harness::agent_harness::AgentHarness> {
        use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
        std::sync::Arc::new(
            AgentHarness::create(AgentHarnessOptions {
                session: amp_core::agent::memory_session(),
                ..Default::default()
            })
            .await
            .expect("harness"),
        )
    }

    /// A saved session file in `dir` holding `prompts`, written the way a chat writes one, then
    /// released (as the chat that wrote it exiting would).
    async fn saved(dir: &std::path::Path, prompts: &[&str]) -> std::path::PathBuf {
        let opened = amp_core::session_store::create(dir, dir, "amp-manager")
            .await
            .expect("create");
        for prompt in prompts {
            opened
                .session
                .append_message(user(prompt))
                .await
                .expect("append");
        }
        opened.path
    }

    /// The criterion's happy path: a picked file becomes this chat's session, and the transcript
    /// handed back is *its* conversation, drawn without running anything.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_switch_moves_the_harness_and_hands_back_that_sessions_transcript() {
        let dir = tmpdir("switch");
        let file = saved(&dir, &["the question the old chat asked"]).await;
        let harness = harness().await;
        let before = harness.session().get_metadata().await.expect("metadata").id;
        let current = std::sync::Arc::new(std::sync::Mutex::new(None));
        let control = ChatSessions::new(
            std::sync::Arc::clone(&harness),
            &dir,
            &dir,
            "amp-manager",
            "amp-manager",
            "faux/faux-1".to_owned(),
            std::sync::Arc::clone(&current),
        );

        let resumed = control
            .resume(amp_tui::ResumeRequest {
                path: file.clone(),
                id: "01a1…c795".to_owned(),
                messages: 1,
                modified_at: std::time::SystemTime::now(),
            })
            .await
            .expect("the switch succeeds");

        // The harness is on the loaded session now, and the chat writes that file.
        let after = harness.session().get_metadata().await.expect("metadata").id;
        assert_ne!(after, before, "the harness switched sessions");
        assert_eq!(resumed.path, file);
        assert_eq!(
            current.lock().unwrap().as_ref(),
            Some(&file),
            "the exit line will name the file this chat writes now"
        );
        // The transcript is the resumed session's own, for display only: a prompt and no running.
        assert!(
            resumed.transcript.iter().any(|(_, e)| matches!(
                e,
                AgentEvent::MessageStart {
                    message: AgentMessage::User(_)
                }
            )),
            "the saved prompt is in the transcript: {:?}",
            resumed.transcript
        );
        // The model reported is the *live harness's* — `faux/faux`, what the harness was actually
        // built with — never the string this control was handed at startup. That is the point of
        // reading it at switch time: a `/model` change since would show up here.
        assert_eq!(
            resumed.model.as_deref(),
            Some("faux/faux"),
            "the disclosure is the live harness's model, not a captured one"
        );
    }

    /// The criterion's failure half: a switch that is refused leaves the harness on the session it
    /// had, and the chat's handle on it unchanged. Here the file belongs to another directory, so
    /// the loader refuses it.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_refused_switch_leaves_the_old_session_and_its_file_handle_alone() {
        let dir = tmpdir("refused");
        let elsewhere = dir.join("elsewhere");
        std::fs::create_dir_all(&elsewhere).unwrap();
        // Saved under another cwd: resuming it here would run this directory's tools on another
        // directory's conversation.
        let foreign = saved(&elsewhere, &["not this directory's"]).await;
        let harness = harness().await;
        let before = harness.session().get_metadata().await.expect("metadata").id;
        let current = std::sync::Arc::new(std::sync::Mutex::new(None));
        let control = ChatSessions::new(
            std::sync::Arc::clone(&harness),
            &dir,
            &dir,
            "amp-manager",
            "amp-manager",
            "faux/faux-1".to_owned(),
            std::sync::Arc::clone(&current),
        );

        let why = match control
            .resume(amp_tui::ResumeRequest {
                path: foreign.clone(),
                id: "dead…beef".to_owned(),
                messages: 1,
                modified_at: std::time::SystemTime::now(),
            })
            .await
        {
            Err(why) => why,
            Ok(_) => panic!("another directory's session must be refused"),
        };
        assert!(!why.is_empty(), "the refusal says why: {why:?}");

        // Nothing moved: same session, and the chat still names no file.
        assert_eq!(
            harness.session().get_metadata().await.expect("metadata").id,
            before,
            "the harness is on the session it had"
        );
        assert!(
            current.lock().unwrap().is_none(),
            "the chat's file handle was not touched"
        );
    }

    /// A transcript that cannot be read is a refusal, not a switch: `restore` runs before the
    /// harness is touched, so the context and the view never disagree.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_unreadable_transcript_refuses_before_anything_is_swapped() {
        let dir = tmpdir("unreadable");
        let file = saved(&dir, &["something"]).await;
        // Replace it with a file the loader accepts as a session but that cannot be replayed as
        // one: a directory named like the session would be unreadable *and* unresumable, so read
        // a path that simply is not there — the same `restore` failure a permission error gives.
        std::fs::remove_file(&file).unwrap();
        let err = restore(&file).expect_err("a missing file has no transcript");
        assert!(err.contains("could not be read"), "{err}");

        let harness = harness().await;
        let before = harness.session().get_metadata().await.expect("metadata").id;
        let current = std::sync::Arc::new(std::sync::Mutex::new(None));
        let control = ChatSessions::new(
            std::sync::Arc::clone(&harness),
            &dir,
            &dir,
            "amp-manager",
            "amp-manager",
            "faux/faux-1".to_owned(),
            std::sync::Arc::clone(&current),
        );
        let why = match control
            .resume(amp_tui::ResumeRequest {
                path: file,
                id: "gone…0000".to_owned(),
                messages: 1,
                modified_at: std::time::SystemTime::now(),
            })
            .await
        {
            Err(why) => why,
            Ok(_) => panic!("a transcript that cannot be read must refuse"),
        };
        assert!(why.contains("could not be read"), "{why}");
        assert_eq!(
            harness.session().get_metadata().await.expect("metadata").id,
            before,
            "nothing was swapped"
        );
        assert!(current.lock().unwrap().is_none());
    }
}
