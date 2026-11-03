//! AMP #3014: `/sessions` end to end — closing a chat, reopening it, resuming a saved session, and
//! every way a switch must refuse or roll back — through the real binary.
//!
//! #3010 built the picker and #3012 the switch transaction, each tested at its own layer
//! (`amp-tui/tests/sessions.rs` drives the shell against a stand-in `SessionControl`;
//! `amp-core/src/session_store.rs` and `amp-harness/src/chat_sessions.rs` drive the transaction
//! in-process). What neither can cover is what a person does: chat, quit, start the chat again,
//! pick the earlier conversation, carry on — with a **real session file**, a **real lock sidecar**,
//! and a **real model client**. That is this file, and it is where #3012's acceptance criteria are
//! finally exercised against a running binary rather than a rig.
//!
//! Driving: the hidden `--snapshot` mode (the same shell, no terminal) with `--input` for prompts,
//! slash commands and keys, and `--input exec:` to change the state directory between two inputs
//! exactly as another process would. The model is the in-process mock OpenAI-compatible provider
//! (`tests/common/provider.rs`), whose recorded request bodies are how "the next turn sees the
//! restored context" is *proved*: the messages the model was sent are read, not inferred from the
//! screen. One test needs to type while a reply is still streaming, which `--input` cannot do (it
//! waits for each run), so it drives a real pty with a small screen emulator, as
//! `tests/chat_approval_wait.rs` does. One more needs a remote link that is *on*, which needs a
//! relay: it builds the shell in-process against `amp-core`'s mock relay, the way
//! `tests/chat_queue.rs` does.
//!
//! Nothing historical may execute, and every resume test asserts it: no recorded dispatch or tool
//! call runs, no prompt the old file still had queued runs, and this machine's dispatch ledger is
//! untouched.
//!
//! State: every test gets its own HOME, XDG dirs, repo and `.amp.json` under cargo's target tmp dir
//! (D13: never /tmp) and the file credential store — no keychain, no installed harness binary, no
//! running chat.

mod common;

use std::{
    path::{Path, PathBuf},
    sync::Arc,
    time::{Duration, SystemTime},
};

use amp_core::{
    session_lock::{self, SessionBusy},
    session_store::{self, OpenError},
};
use common::{
    MockAmp, Output, Running,
    provider::{MockProvider, ProviderState, spawn_provider},
    run, spawn, workdir,
};
use rpi_agent::AgentMessage;
use rpi_ai::types::{UserContent, UserMessage};
use rpi_harness::{
    agent_harness::AgentLane,
    session::{
        jsonl::JsonlV4Header,
        types::{ProvisionedEntry, ProvisionedKind},
    },
    types::AgentHarnessOptions,
};
use serde_json::{Value, json};

// ---------------------------------------------------------------------------------------------
// The fixture

const KEY: &str = "sk-chat-sessions-3014-0123456789abcdef";
/// The editor width every snapshot runs at: wide enough that a whole refusal is one row.
const W: usize = 200;
const GEOMETRY: &str = "200x44";

/// `mock/m-1` is a real HTTP endpoint (so a request body can be inspected); `faux/faux-1` is the
/// scripted provider the cases that need a *slow* or a *live* model use.
fn config(provider_url: &str, manager: &str) -> String {
    format!(
        r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[providers.mock]
api = "openai-completions"
base_url = "{provider_url}"
key_env = "MOCK_KEY"
[providers.mock.models."m-1"]

[roles.manager]
model = "{manager}"
"#
    )
}

/// The `boss`/`wf` pair for the worker test: the manager dispatches, the worker runs slowly.
const WORKER_CONFIG: &str = r#"
[providers.boss]
api = "faux"
[providers.boss.models."boss-1"]

[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]

[roles.manager]
model = "boss/boss-1"
[roles.worker-backend]
model = "wf/wf-1"
"#;

/// One test's world: a repo whose chats write sessions, an isolated HOME, the mock AMP and the
/// mock provider.
struct World {
    dir: PathBuf,
    /// The directory the chat runs in — what a session's header records and what `/sessions` scopes
    /// its listing to (canonical: the header is written canonical).
    repo: PathBuf,
    /// `<XDG_STATE_HOME>/amp-harness` (the harness's own state dir, a level below the variable):
    /// session files, their lock sidecars, the dispatch ledger.
    state: PathBuf,
    /// What `XDG_STATE_HOME` is set to — the directory *above* `state`.
    xdg_state: PathBuf,
    amp: Running,
    provider: MockProvider,
}

impl World {
    async fn new(name: &str) -> Self {
        let dir = workdir(name, Some(2));
        let repo = std::fs::canonicalize(dir.join("project")).expect("the repo");
        std::fs::create_dir_all(dir.join("home/.config/amp-harness")).unwrap();
        let amp = spawn(MockAmp::new(), None).await;
        let provider = spawn_provider(ProviderState {
            key: Some(KEY.to_owned()),
            models: vec!["m-1".into()],
            reply: "the mock's answer".into(),
            ..ProviderState::default()
        })
        .await;
        std::fs::write(
            dir.join("home/.config/amp-harness/config.toml"),
            config(&provider.url, "mock/m-1"),
        )
        .unwrap();
        let xdg_state = dir.join("home/.local/state");
        let state = xdg_state.join("amp-harness");
        Self {
            dir,
            repo,
            state,
            xdg_state,
            amp,
            provider,
        }
    }

    /// `<state>/sessions/amp-manager`: where a chat in `repo` files its conversations.
    fn sessions(&self) -> PathBuf {
        self.state.join("sessions/amp-manager")
    }

    /// This machine's dispatch ledger — the records (`t<ticket>.json`), which a resume must never
    /// add to or change. The `live-<pid>.lock` files are each running chat's own liveness handle,
    /// created by every chat that opens the ledger at all, so they are not evidence of a dispatch.
    fn dispatch_records(&self) -> Vec<(PathBuf, Vec<u8>)> {
        let mut out: Vec<(PathBuf, Vec<u8>)> = std::fs::read_dir(self.state.join("dispatches"))
            .map(|entries| {
                entries
                    .flatten()
                    .map(|e| e.path())
                    .filter(|p| {
                        p.extension().is_some_and(|x| x == "json")
                            && p.file_name()
                                .and_then(|n| n.to_str())
                                .is_some_and(|n| n.starts_with('t'))
                    })
                    .map(|path| {
                        let bytes = std::fs::read(&path).unwrap_or_default();
                        (path, bytes)
                    })
                    .collect()
            })
            .unwrap_or_default();
        out.sort();
        out
    }

    /// Every `.jsonl` conversation in this directory, newest activity first (the picker's order).
    fn session_files(&self) -> Vec<PathBuf> {
        let mut files: Vec<(SystemTime, PathBuf)> = std::fs::read_dir(self.sessions())
            .map(|entries| {
                entries
                    .flatten()
                    .map(|e| e.path())
                    .filter(|p| p.extension().is_some_and(|x| x == "jsonl"))
                    .map(|p| {
                        let at = std::fs::metadata(&p)
                            .and_then(|m| m.modified())
                            .unwrap_or(SystemTime::UNIX_EPOCH);
                        (at, p)
                    })
                    .collect()
            })
            .unwrap_or_default();
        files.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| b.1.cmp(&a.1)));
        files.into_iter().map(|(_, p)| p).collect()
    }

    /// Writes `config` (which names this run's manager model) and runs the chat with `inputs`;
    /// `env` adds variables (a faux script, a slow stream).
    async fn chat_with(&self, config: &str, inputs: &[&str], env: &[(&str, &str)]) -> Output {
        self.chat_memory(config, inputs, env, false).await
    }

    /// A run that keeps its own conversation in memory (`--no-session`): the listing is drawn
    /// exactly as always, but the run adds no row of its own to it — what a test wants when it is
    /// about the rows already on disk.
    async fn chat_listing(&self, inputs: &[&str]) -> Output {
        let config = config(&self.provider.url, "mock/m-1");
        self.chat_memory(&config, inputs, &[], true).await
    }

    async fn chat_memory(
        &self,
        config: &str,
        inputs: &[&str],
        env: &[(&str, &str)],
        memory: bool,
    ) -> Output {
        std::fs::write(
            self.dir.join("home/.config/amp-harness/config.toml"),
            config,
        )
        .unwrap();
        let mut args = vec!["--no-setup", "--snapshot", GEOMETRY];
        if memory {
            args.push("--no-session");
        }
        for input in inputs {
            args.extend(["--input", input]);
        }
        let xdg_state = self.xdg_state.to_string_lossy().into_owned();
        let mut vars: Vec<(&str, &str)> =
            vec![("XDG_STATE_HOME", xdg_state.as_str()), ("MOCK_KEY", KEY)];
        vars.extend(env.iter().copied());
        run(&self.dir, &self.amp.url, &args, &vars).await
    }

    /// The chat on the mock provider (whose requests are recorded).
    async fn chat(&self, inputs: &[&str]) -> Output {
        let config = config(&self.provider.url, "mock/m-1");
        self.chat_with(&config, inputs, &[]).await
    }

    /// A run that must have succeeded, its screen unpadded to readable rows.
    fn screen(out: &Output) -> String {
        assert_eq!(
            out.code, 0,
            "stderr: {}\nstdout: {}",
            out.stderr, out.stdout
        );
        let rows: Vec<String> = out.stdout.lines().map(str::to_owned).collect();
        common::unpad(rows, W).join("\n")
    }

    /// The plain text of the `nth` chat completion request (0-based) the provider served: the
    /// messages in order, each flattened to its text.
    fn asked(&self, nth: usize) -> String {
        let bodies = self.provider.bodies();
        let body = bodies
            .get(nth)
            .unwrap_or_else(|| panic!("only {} model requests were made", bodies.len()));
        body["messages"]
            .as_array()
            .unwrap_or_else(|| panic!("no messages in {body}"))
            .iter()
            .map(|m| match &m["content"] {
                Value::String(s) => s.clone(),
                Value::Array(blocks) => blocks
                    .iter()
                    .filter_map(|b| b["text"].as_str())
                    .collect::<Vec<_>>()
                    .join("\n"),
                other => other.to_string(),
            })
            .collect::<Vec<_>>()
            .join("\n")
    }

    /// Everything the model was ever sent, requests separated.
    fn asked_all(&self) -> String {
        (0..self.provider.bodies().len())
            .map(|n| self.asked(n))
            .collect::<Vec<_>>()
            .join("\n===\n")
    }

    /// [`World::asked`] with the per-request clock stamp removed (#2250: every prompt carries one,
    /// and it is not part of the conversation), so a test can look at the ends of a message list.
    fn asked_no_stamp(&self, nth: usize) -> String {
        amp_core::request_time::strip(&self.asked(nth))
    }
}

/// The short id the banner and the picker show for `path` (`01a1…c795`), derived as `chat.rs` does.
fn short_id(path: &Path) -> String {
    let stem = path
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default();
    let id = match stem.split_once('-') {
        Some((millis, rest))
            if !millis.is_empty() && millis.bytes().all(|b| b.is_ascii_digit()) =>
        {
            rest.to_owned()
        }
        _ => stem,
    };
    let chars: Vec<char> = id.chars().collect();
    if chars.len() <= 9 {
        return id;
    }
    format!(
        "{}…{}",
        chars[..4].iter().collect::<String>(),
        chars[chars.len() - 4..].iter().collect::<String>()
    )
}

/// The lock sidecar beside `path` (`session_lock::lock_path`).
fn sidecar(path: &Path) -> PathBuf {
    session_lock::lock_path(path)
}

/// Whether this process can take `path`'s lock right now. `flock` is per open file description, so
/// another handle in this same process conflicts too — which is how "released" is observable.
fn free(path: &Path) -> bool {
    match session_lock::claim_session_lock(path) {
        Ok(guard) => {
            drop(guard);
            true
        }
        Err(SessionBusy::Held { .. }) => false,
        Err(other) => panic!("{other}"),
    }
}

/// The header line a session file starts with, written as the harness writes it (the encoder
/// already ends it with a newline, which is why nothing below adds a second one).
fn header_line(id: &str, created: i64, cwd: &Path) -> String {
    let header = JsonlV4Header::new(
        id.to_owned(),
        created,
        cwd.to_string_lossy().into_owned(),
        None,
        None,
        None,
    );
    rpi_harness::session::jsonl::encode_header(&header)
}

/// One `{"kind":"entry", …}` line, parented to `parent` so the replay follows the main lane.
fn entry_line(seq: u64, at: i64, parent: Option<&str>, message: Value) -> String {
    json!({
        "kind": "entry",
        "lane": "main",
        "type": "message",
        "id": format!("e-{seq}"),
        "seq": seq,
        "parentId": parent,
        "timestamp": at,
        "message": message,
    })
    .to_string()
}

fn user_message(text: &str, at: i64) -> Value {
    json!({"kind": "user", "role": "user", "content": text, "timestamp": at})
}

fn assistant_message(text: &str, at: i64) -> Value {
    let mut message = assistant_with(vec![json!({"type": "text", "text": text})], at);
    message["content"] = json!([{"type": "text", "text": text}]);
    message
}

/// An assistant message whose content blocks are `blocks` (text, thinking, tool calls).
fn assistant_with(blocks: Vec<Value>, at: i64) -> Value {
    json!({
        "kind": "assistant",
        "role": "assistant",
        "content": blocks,
        "api": "openai-completions",
        "provider": "mock",
        "model": "m-1",
        "usage": {"input": 10, "output": 5, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 15,
                  "cost": {"input": 0.0, "output": 0.0, "cacheRead": 0.0, "cacheWrite": 0.0,
                           "total": 0.0}},
        "stopReason": "stop",
        "timestamp": at,
    })
}

/// A saved conversation in `dir`, written straight to disk as a previous chat left it: the same
/// header, entry lines and `<created ms>-<id>.jsonl` name, and no lock behind.
fn saved_session(dir: &Path, created: i64, id: &str, cwd: &Path, lines: &[String]) -> PathBuf {
    std::fs::create_dir_all(dir).unwrap();
    let path = dir.join(format!("{created}-{id}.jsonl"));
    let mut text = header_line(id, created, cwd);
    for line in lines {
        text.push_str(line);
        text.push('\n');
    }
    std::fs::write(&path, text).unwrap();
    path
}

/// A session holding one exchange: `prompt` asked, `reply` answered.
fn exchange(dir: &Path, created: i64, id: &str, cwd: &Path, prompt: &str, reply: &str) -> PathBuf {
    saved_session(
        dir,
        created,
        id,
        cwd,
        &[
            entry_line(2, created, None, user_message(prompt, created)),
            entry_line(
                3,
                created + 1,
                Some("e-2"),
                assistant_message(reply, created + 1),
            ),
        ],
    )
}

/// The `Sessions · this directory` panel onward — the part of a screen the picker owns.
fn panel_of(screen: &str) -> &str {
    let start = screen
        .find("Sessions · this directory")
        .unwrap_or_else(|| panic!("no sessions panel in\n{screen}"));
    &screen[start..]
}

/// The one line of `text` that holds `needle`, whitespace-flattened (a wrapped sentence reads as
/// one line again). Panics with the whole screen when there is none.
fn line_with(screen: &str, needle: &str) -> String {
    screen
        .lines()
        .map(|l| {
            l.trim_matches(|c: char| c == '│' || c.is_whitespace())
                .split_whitespace()
                .collect::<Vec<_>>()
                .join(" ")
        })
        .find(|l| l.contains(needle))
        .unwrap_or_else(|| panic!("no line with {needle:?} in\n{screen}"))
}

// ---------------------------------------------------------------------------------------------
// A close, a reopen and a resume — the ticket's first criterion

/// **A restart continuation fixture passes.** Chat in a directory, quit, start again in the same
/// directory, `/sessions`, pick the earlier conversation, and carry on: the next prompt's request
/// carries the restored history (read back from the provider's request body, not inferred from the
/// screen), the screen shows that conversation instead of an empty one, and the chat keeps writing
/// that same file.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_saved_conversation_is_reopened_and_continued_with_its_own_history() {
    let w = World::new("resume-continues").await;

    // First run: a prompt, answered by the real provider. Then quit, as a person would.
    let first = w
        .chat(&["what did we decide about the session lock?", "/quit"])
        .await;
    let first_screen = World::screen(&first);
    assert!(
        first_screen.contains("the mock's answer"),
        "the first chat answered:\n{first_screen}"
    );
    let files = w.session_files();
    assert_eq!(files.len(), 1, "one saved conversation: {files:?}");
    let saved = files[0].clone();
    let saved_bytes = std::fs::read(&saved).unwrap();
    assert!(
        sidecar(&saved).exists(),
        "the writer left its lock sidecar beside the file"
    );
    assert!(free(&saved), "and released it on quit");
    assert_eq!(
        w.provider.bodies().len(),
        1,
        "one model call in the first run"
    );
    // The chat connected to AMP (its banner names the project's server, and the listing is drawn
    // from the session store the chat wired): the fixture is a real chat, not a stub.
    assert!(
        first_screen.contains("project #2 AMP v2") && first_screen.contains("session "),
        "the first chat ran the real shell:\n{first_screen}"
    );

    // Second run, same directory and state dir. First the listing on its own (the picker closes
    // when a resume succeeds, so this is where the row is read), then the resume and a new prompt.
    let listed = World::screen(&w.chat_listing(&["/sessions"]).await);
    let id = short_id(&saved);
    assert!(
        panel_of(&listed).contains(&id),
        "the saved conversation is a row:\n{listed}"
    );
    // This listing run keeps its own conversation in memory, so the saved one is the only row
    // and nothing is marked current or un-resumable — the scan is clean.
    let panel = panel_of(&listed);
    assert!(
        panel.contains("Sessions · this directory · 1"),
        "exactly the one saved conversation:\n{listed}"
    );
    assert!(!panel.contains("· current"), "{listed}");
    assert!(
        !panel.contains("can't be resumed") && !panel.contains("locked"),
        "nothing in the list is a problem row:\n{listed}"
    );

    let second = w
        .chat(&[
            "/sessions",
            "key:enter",
            "key:enter",
            "and what follows from that?",
            "/quit",
        ])
        .await;
    let screen = World::screen(&second);

    // The resume says where it went and on which model, and the chat keeps writing the original
    // file.
    let resumed_line = line_with(&screen, &format!("✓ resumed {id}"));
    assert!(
        resumed_line.contains("continues in the original file"),
        "the resume line: {resumed_line}"
    );
    assert!(
        resumed_line.contains("runs on mock/m-1"),
        "the model it runs on is disclosed: {resumed_line}"
    );
    // The banner now names the file the chat writes (`set_session_file` re-points it), so the
    // process no longer claims the session it started.
    assert!(
        screen.contains(&format!("session {id} ")),
        "the banner names the resumed file:\n{screen}"
    );
    // The transcript is the resumed conversation, plus the new turn.
    assert!(
        screen.contains("what did we decide about the session lock?"),
        "the resumed prompt is drawn:\n{screen}"
    );
    assert!(
        screen.contains("the mock's answer"),
        "and its reply:\n{screen}"
    );
    assert!(
        screen.contains("and what follows from that?"),
        "the new prompt ran:\n{screen}"
    );

    // The substance: the new turn's request carried the resumed history, which exists only in the
    // file the person picked.
    let third = w.provider.bodies().len() - 1;
    let asked = w.asked(third);
    assert!(
        asked.contains("what did we decide about the session lock?"),
        "the model was sent the resumed prompt:\n{asked}"
    );
    assert!(
        asked.contains("the mock's answer"),
        "and its answer, so the conversation continues rather than restarts:\n{asked}"
    );
    assert!(
        w.asked_no_stamp(third)
            .ends_with("and what follows from that?"),
        "with the new prompt last:\n{asked}"
    );

    // The saved file is the one the chat writes now: appended to in place, the new turn on disk.
    let after = std::fs::read(&saved).unwrap();
    assert!(
        after.len() > saved_bytes.len() && after.starts_with(&saved_bytes),
        "the resumed file was appended to, never rewritten"
    );
    assert!(
        String::from_utf8_lossy(&after).contains("and what follows from that?"),
        "the new prompt is in the resumed file"
    );
    assert!(free(&saved), "the chat let go of it when it quit");
}

/// After a resume the chat's own answer to "which file am I writing?" is the resumed one:
/// `/usage` names it and the banner's short id is the resumed session's. `chat.rs`'s exit line
/// reads the same handle (it is printed after the alternate screen, which `--snapshot` does not
/// reach), so this is the same fact at the point a run can observe it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn after_a_resume_the_chat_names_the_file_it_continues() {
    let w = World::new("resume-exit-line").await;
    let older = exchange(
        &w.sessions(),
        1_600_000_000_000,
        "older-conversation",
        &w.repo,
        "the conversation to continue",
        "its answer",
    );

    let out = w
        .chat(&["/sessions", "key:enter", "key:enter", "/usage", "/quit"])
        .await;
    let screen = World::screen(&out);
    assert!(
        screen.contains(&format!("✓ resumed {}", short_id(&older))),
        "{screen}"
    );
    // `/usage`'s last line names the file (`session: ~/.local/state/.../<id>.jsonl`), under this
    // run's own state dir.
    let usage = screen
        .lines()
        .map(|l| {
            l.trim_matches(|c: char| c == '│' || c.is_whitespace())
                .to_owned()
        })
        .find(|l| l.starts_with("session: "))
        .unwrap_or_else(|| panic!("no `session: ` line in\n{screen}"));
    let name = older.file_name().unwrap().to_string_lossy().into_owned();
    assert!(
        usage.ends_with(&name),
        "/usage names the continued file: {usage}"
    );
    assert!(
        usage.starts_with("session: ~/.local/state/amp-harness/sessions/amp-manager/"),
        "with the path the banner shortens: {usage}"
    );
}

/// The listing is this directory's, and the session this chat is writing is `· current`: present
/// and well-formed, just not a target. A session another directory started is not listed at all
/// (its tools, policy and prompt belong to that directory), and nothing of its text leaks into the
/// panel.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_listing_is_this_directory_and_marks_the_one_being_written() {
    let w = World::new("listing-scope").await;
    let elsewhere = w.dir.join("elsewhere");
    std::fs::create_dir_all(&elsewhere).unwrap();
    let elsewhere = std::fs::canonicalize(&elsewhere).unwrap();
    let mine = exchange(
        &w.sessions(),
        1_600_000_000_000,
        "mine",
        &w.repo,
        "this directory's question",
        "this directory's answer",
    );
    let theirs = exchange(
        &w.sessions(),
        1_600_000_100_000,
        "theirs",
        &elsewhere,
        "another directory's question",
        "another directory's answer",
    );

    let out = w.chat(&["/sessions"]).await;
    let screen = World::screen(&out);
    let panel = panel_of(&screen);
    assert!(
        panel.contains(&short_id(&mine)),
        "this directory's session is listed:\n{screen}"
    );
    assert!(
        !panel.contains(&short_id(&theirs)),
        "another directory's is not:\n{screen}"
    );
    assert!(
        !screen.contains("another directory's question"),
        "and none of its text leaks in:\n{screen}"
    );
    // The current row is a state, not a problem: one `· current`, and no "un-resumable" tally.
    assert_eq!(screen.matches("· current").count(), 1, "{screen}");
    assert!(!panel.contains("can't be resumed"), "{screen}");
    assert!(
        !panel.contains("locked"),
        "the chat's own file is not a problem row:\n{screen}"
    );

    // The confirm stage says what a resume does and does not do before Enter is pressed twice.
    let confirmed = World::screen(&w.chat_listing(&["/sessions", "key:enter"]).await);
    assert!(
        confirmed.contains("does not re-run its tool calls"),
        "the confirm stage promises display-only:\n{confirmed}"
    );
    assert!(
        confirmed.contains("stays saved as this chat"),
        "and says the file stays:\n{confirmed}"
    );
}

// ---------------------------------------------------------------------------------------------
// Nothing historical executes

/// A recorded `amp_run_ticket` is drawn in the transcript and **never runs**: resuming the
/// conversation does not dispatch its ticket, claim it, record a run or complete it, and this
/// machine's dispatch ledger is untouched. Its recorded tool result is shown as the run left it —
/// the "transcript and tool results are display-only" half of the criterion.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_recorded_dispatch_is_shown_but_never_runs_again() {
    let w = World::new("no-historical-dispatch").await;
    let at = 1_600_000_000_000_i64;
    let call = assistant_with(
        vec![
            json!({"type": "text", "text": "Dispatching the CLI ticket."}),
            json!({"type": "toolCall", "id": "call-dispatch-1", "name": "amp_run_ticket",
                   "arguments": {"task_id": 2085, "agent_id": "amp:amp-worker-backend"}}),
        ],
        at,
    );
    let result = json!({
        "kind": "tool_result",
        "role": "toolResult",
        "toolCallId": "call-dispatch-1",
        "toolName": "amp_run_ticket",
        "content": [{"type": "text", "text": "worker amp:amp-worker-backend finished #2085"}],
        "details": Value::Null,
        "isError": false,
        "timestamp": at + 2,
    });
    let saved = saved_session(
        &w.sessions(),
        at,
        "dispatched-before",
        &w.repo,
        &[
            entry_line(2, at, None, user_message("run ticket 2085", at)),
            entry_line(3, at, Some("e-2"), call),
            entry_line(4, at + 2, Some("e-3"), result),
        ],
    );
    let ledger_before = w.dispatch_records();

    let out = w
        .chat(&[
            "/sessions",
            "key:enter",
            "key:enter",
            "what happened to 2085?",
            "/quit",
        ])
        .await;
    let screen = World::screen(&out);
    assert!(
        screen.contains(&format!("✓ resumed {}", short_id(&saved))),
        "{screen}"
    );
    // The recorded turn is drawn: the call as a one-line tool row naming its ticket, and its
    // result — the events a replay produces, not a re-run.
    assert!(
        screen.contains("run_ticket #2085"),
        "the recorded call is a tool row:\n{screen}"
    );
    assert!(
        screen.contains("worker amp:amp-worker-backend finished #2085"),
        "and its recorded result is shown:\n{screen}"
    );
    assert!(
        screen.contains("Dispatching the CLI ticket."),
        "with the sentence that preceded it:\n{screen}"
    );

    // Nothing ran it again: no dispatch, claim, run report, completion or hand-back anywhere, and
    // no call so much as names ticket 2085.
    for tool in [
        "amp_run_ticket",
        "amp_dispatch_task",
        "amp_claim_task",
        "amp_report_run",
        "amp_complete_task",
        "amp_return_task",
        "amp_set_task_state",
        "amp_block_task",
    ] {
        assert!(
            w.amp.mock.calls_to(tool).is_empty(),
            "{tool} was called:\n{:?}",
            w.amp.mock.calls
        );
    }
    assert!(
        w.amp
            .mock
            .calls
            .lock()
            .unwrap()
            .iter()
            .all(|(_, args)| args["task_id"] != 2085),
        "no call names #2085: {:?}",
        w.amp.mock.calls
    );
    assert_eq!(
        w.dispatch_records(),
        ledger_before,
        "this machine's dispatch ledger did not gain an entry"
    );
    // The next turn starts from the conversation, and the new prompt is what it was asked to do.
    let asked = w.asked(w.provider.bodies().len() - 1);
    assert!(
        asked.contains("what happened to 2085?"),
        "the new prompt is what was asked:\n{asked}"
    );
}

/// A prompt the saved file still had **queued** never runs in the resumed chat. A session saved
/// mid-reply has durable `queue_enqueued` records, and `set_session` rebuilds rpi's queues from
/// them — so `resume_session` cancels every one first, on the loaded session, before the swap. The
/// next turn must see none of them, and a later reopen must find nothing pending either.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn prompts_the_saved_file_still_had_queued_never_run() {
    let w = World::new("no-historical-queue").await;
    let at = 1_600_000_000_000_i64;
    let saved = saved_session(
        &w.sessions(),
        at,
        "queued-before",
        &w.repo,
        &[
            entry_line(2, at, None, user_message("what should I do next?", at)),
            entry_line(
                3,
                at + 1,
                Some("e-2"),
                assistant_message("Wait for the reply.", at + 1),
            ),
        ],
    );
    // The dead chat was mid-reply: two prompts of different queue kinds are on the file. Queued
    // with the real API, so the records are exactly the ones `rebuild_queues` replays.
    {
        let opened = session_store::load(&saved, &w.repo, "amp-manager")
            .await
            .expect("the saved file loads");
        let harness = rpi_harness::agent_harness::AgentHarness::create(AgentHarnessOptions {
            session: opened.session,
            ..Default::default()
        })
        .await
        .expect("a harness on it");
        harness
            .steer(for_prompt("a prompt steered into the dead run"))
            .await
            .expect("steer");
        harness
            .next_run(for_prompt("a prompt queued for the dead run's next turn"))
            .await
            .expect("next_run");
    }
    assert!(
        free(&saved),
        "the chat that queued them is gone, as a crashed chat's file is"
    );

    let out = w
        .chat(&[
            "/sessions",
            "key:enter",
            "key:enter",
            "carry on please",
            "/quit",
        ])
        .await;
    let screen = World::screen(&out);
    assert!(
        screen.contains(&format!("✓ resumed {}", short_id(&saved))),
        "{screen}"
    );

    // Neither queued prompt ran: not as a turn of its own, and not folded into the new turn.
    let asked = w.asked_all();
    assert!(
        !asked.contains("a prompt steered into the dead run"),
        "the steered prompt never reached the model:\n{asked}"
    );
    assert!(
        !asked.contains("a prompt queued for the dead run's next turn"),
        "nor did the next-run prompt:\n{asked}"
    );
    assert!(
        asked.contains("carry on please"),
        "the prompt the person typed did:\n{asked}"
    );
    assert_eq!(
        w.provider.bodies().len(),
        1,
        "exactly one turn ran after the resume"
    );

    // Durable, not just in memory: every enqueue on the file now has its cancellation, so a later
    // reopen of the same file finds nothing pending either.
    let text = std::fs::read_to_string(&saved).unwrap();
    let enqueued = text
        .lines()
        .filter(|l| l.contains("\"queue_enqueued\""))
        .count();
    let cancelled = text
        .lines()
        .filter(|l| l.contains("\"queue_cancelled\""))
        .count();
    assert_eq!(enqueued, 2, "there were two to cancel:\n{text}");
    assert_eq!(
        enqueued, cancelled,
        "every enqueue the file still had was cancelled on disk:\n{text}"
    );
}

/// A compacted conversation resumes from its summary. The compaction entry is the boundary rpi's
/// context builder honours: what the model gets on the next turn is the summary plus what came
/// after it, and not the turns it summarized — the same rule as a live run, applied to a file that
/// was loaded rather than written.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_compacted_conversation_resumes_from_its_summary_not_its_summarized_turns() {
    let w = World::new("compaction").await;
    let at = 1_600_000_000_000_i64;
    let saved = saved_session(
        &w.sessions(),
        at,
        "compacted-before",
        &w.repo,
        &[
            entry_line(
                2,
                at,
                None,
                user_message("an early question about the lock order", at),
            ),
            entry_line(
                3,
                at + 1,
                Some("e-2"),
                assistant_message("an early answer the summary replaces", at + 1),
            ),
        ],
    );
    // Compact it the way the harness does, then continue: the summary, the retained tail, and a
    // later exchange that is *after* the boundary.
    {
        let opened = session_store::load(&saved, &w.repo, "amp-manager")
            .await
            .expect("the saved file loads");
        opened
            .session
            .append_entry(
                ProvisionedEntry {
                    id: "compaction-1".to_owned(),
                    kind: ProvisionedKind::Compaction {
                        summary: "the earlier part was about the lock order".to_owned(),
                        retained_tail: vec![for_prompt("a message kept beside the summary")],
                        tokens_before: 4_000,
                        details: None,
                        usage: None,
                    },
                },
                "main",
            )
            .await
            .expect("a compaction entry");
        opened
            .session
            .append_message(for_prompt("a question asked after the compaction"))
            .await
            .expect("a later prompt");
        opened
            .session
            .append_message(AgentMessage::Assistant(Box::new(faux_reply(
                "a reply written after the compaction",
            ))))
            .await
            .expect("a later reply");
    }
    assert!(free(&saved), "the chat that wrote it is gone");

    let out = w
        .chat(&["/sessions", "key:enter", "key:enter", "and now?", "/quit"])
        .await;
    let screen = World::screen(&out);
    assert!(
        screen.contains(&format!("✓ resumed {}", short_id(&saved))),
        "{screen}"
    );

    let asked = w.asked(w.provider.bodies().len() - 1);
    assert!(
        asked.contains("the earlier part was about the lock order"),
        "the summary reached the model:\n{asked}"
    );
    assert!(
        asked.contains("a message kept beside the summary"),
        "so did the retained tail:\n{asked}"
    );
    assert!(
        asked.contains("a question asked after the compaction"),
        "and what came after the boundary:\n{asked}"
    );
    assert!(
        !asked.contains("an early question about the lock order"),
        "but not the turns the summary replaced:\n{asked}"
    );
    assert!(
        !asked.contains("an early answer the summary replaces"),
        "including their answers:\n{asked}"
    );
    assert!(
        w.asked_no_stamp(w.provider.bodies().len() - 1)
            .ends_with("and now?"),
        "{asked}"
    );
}

// ---------------------------------------------------------------------------------------------
// Refusals and rollback

/// A switch that fails after the person picked a row leaves the chat exactly as it was — same
/// conversation, same history, same file — and says why. The file is removed between the scan and
/// the pick with `--input exec:`, which is a real race: the listing saw it, the load cannot.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_failed_switch_leaves_the_chat_and_its_conversation_usable() {
    let w = World::new("failed-switch").await;
    let doomed = exchange(
        &w.sessions(),
        1_600_000_000_000,
        "gone-by-then",
        &w.repo,
        "a conversation that is about to disappear",
        "its answer",
    );
    let remove = format!("rm -f {}", doomed.display());

    let out = w
        .chat(&[
            "a prompt the old session keeps",
            "/sessions",
            &format!("exec:{remove}"),
            "key:enter",
            "key:enter",
            "and the next turn still works",
            "/quit",
        ])
        .await;
    let screen = World::screen(&out);

    // The failure is reported as a refusal, on the row that was picked, with the picker back.
    let refused = line_with(&screen, "was not resumed");
    assert!(
        refused.contains(short_id(&doomed).as_str()) || refused.contains("could not be read"),
        "the refusal names the failure: {refused}"
    );
    assert!(
        !screen.contains("✓ resumed"),
        "nothing was resumed:\n{screen}"
    );
    // The chat is untouched and still usable: its own prompt is in the transcript, and the next
    // turn ran.
    assert!(
        screen.contains("a prompt the old session keeps"),
        "the old conversation is still there:\n{screen}"
    );
    assert!(
        screen.contains("and the next turn still works"),
        "and the chat keeps working after the failed switch:\n{screen}"
    );
    // Two model calls: the chat's own prompt, and the one after the failure — no third from a
    // half-completed switch.
    assert_eq!(
        w.provider.bodies().len(),
        2,
        "the refusal cost no turn and started none"
    );
    // The chat wrote its own session throughout; it never adopted the missing file.
    assert!(
        w.session_files().iter().all(|p| p != &doomed),
        "the missing file was not recreated"
    );
}

/// A session another directory started is refused, and the chat stays on the session it had. The
/// scan does not offer it (so a person never picks it), and the loader refuses it by name — the
/// second gate, for a request that names the path directly.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn another_directorys_session_is_refused_unopened() {
    let w = World::new("wrong-cwd").await;
    let elsewhere = w.dir.join("elsewhere");
    std::fs::create_dir_all(&elsewhere).unwrap();
    let elsewhere = std::fs::canonicalize(&elsewhere).unwrap();
    let foreign = exchange(
        &w.sessions(),
        1_600_000_000_000,
        "started-elsewhere",
        &elsewhere,
        "another directory's question",
        "another directory's answer",
    );
    let before = std::fs::read(&foreign).unwrap();

    let out = w.chat(&["/sessions", "and this one?", "/quit"]).await;
    let screen = World::screen(&out);
    assert!(
        !panel_of(&screen).contains(&short_id(&foreign)),
        "the list does not offer another directory's session:\n{screen}"
    );
    assert!(
        !screen.contains("another directory's question"),
        "and none of it is drawn:\n{screen}"
    );
    assert!(
        screen.contains("and this one?"),
        "the chat kept working:\n{screen}"
    );
    assert!(
        w.asked(w.provider.bodies().len() - 1)
            .contains("and this one?"),
        "on this directory's own session"
    );

    // The loader refuses it by name, and nothing about the file changed.
    let refused = session_store::load(&foreign, &w.repo, "amp-manager")
        .await
        .expect_err("another directory's session cannot be loaded here");
    assert!(
        matches!(refused, OpenError::OtherDirectory { .. }),
        "{refused}"
    );
    assert_eq!(std::fs::read(&foreign).unwrap(), before, "never written");
    assert!(free(&foreign), "and never locked");
}

/// A session a **live writer** holds is refused, and the row says so rather than silently going
/// missing. This test process stands in for the other live chat: it holds the sidecar while the
/// binary lists, so the row is a foreign holder, not "this chat has it open".
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_session_a_live_writer_holds_is_a_locked_row_and_is_refused() {
    let w = World::new("live-writer").await;
    let held = exchange(
        &w.sessions(),
        1_600_000_000_000,
        "being-written",
        &w.repo,
        "a conversation another chat is writing",
        "its answer",
    );
    let before = std::fs::read(&held).unwrap();
    let guard = session_lock::claim_session_lock_as(&held, "amp-manager").expect("lock it");
    assert!(guard.lock_path().exists(), "the sidecar is on disk");

    let out = w.chat_listing(&["/sessions"]).await;
    let screen = World::screen(&out);
    let panel = panel_of(&screen);
    assert!(
        panel.contains(&short_id(&held)),
        "the row is drawn, not silently missing:\n{screen}"
    );
    let row = line_with(panel, &short_id(&held));
    assert!(
        row.contains("locked · running elsewhere"),
        "with the reason on it: {row}"
    );
    assert!(
        !row.contains("this chat already has it open"),
        "and it is the *other* writer's refusal: {row}"
    );
    assert!(
        panel.contains("can't be resumed"),
        "the tally counts it:\n{screen}"
    );

    // The transaction refuses it too, before reading a byte: a resume never steals a live file.
    let refused = session_store::load(&held, &w.repo, "amp-manager")
        .await
        .expect_err("a live writer's session cannot be resumed");
    assert!(matches!(refused, OpenError::Busy(_)), "{refused}");
    assert_eq!(
        std::fs::read(&held).unwrap(),
        before,
        "a refusal never writes (not even a repair)"
    );
    assert!(!free(&held), "the live writer still owns it");
    drop(guard);
    assert!(free(&held), "and it is free once that writer goes");
}

/// A file whose last append was cut off mid-line (a killed writer) is *repaired*, not refused: the
/// loader drops the torn tail under its guard and the conversation before it survives, which is the
/// difference between truncated and corrupt.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_truncated_tail_is_repaired_and_the_conversation_before_it_kept() {
    let w = World::new("torn-tail").await;
    let at = 1_600_000_000_000_i64;
    let saved = saved_session(
        &w.sessions(),
        at,
        "torn-tail",
        &w.repo,
        &[
            entry_line(2, at, None, user_message("the question before the cut", at)),
            entry_line(
                3,
                at + 1,
                Some("e-2"),
                assistant_message("the answer before the cut", at + 1),
            ),
        ],
    );
    // A killed writer's last append: half a line, no newline.
    let mut bytes = std::fs::read(&saved).unwrap();
    bytes.extend_from_slice(br#"{"kind":"entry","lane":"main","type":"mess"#);
    std::fs::write(&saved, &bytes).unwrap();

    let out = w
        .chat(&[
            "/sessions",
            "key:enter",
            "key:enter",
            "after the repair",
            "/quit",
        ])
        .await;
    let screen = World::screen(&out);
    assert!(
        screen.contains(&format!("✓ resumed {}", short_id(&saved))),
        "a torn tail is repaired, not refused:\n{screen}"
    );
    assert!(
        screen.contains("the question before the cut")
            && screen.contains("the answer before the cut"),
        "the conversation before the tear survives:\n{screen}"
    );
    // The repair published the valid prefix: the half line is gone from the file.
    let repaired = std::fs::read_to_string(&saved).unwrap();
    assert!(
        !repaired.contains(r#"{"kind":"entry","lane":"main","type":"mess"}"#),
        "the torn append was dropped:\n{repaired}"
    );
    assert!(
        repaired.contains("the question before the cut"),
        "and the valid prefix kept:\n{repaired}"
    );
    assert!(
        w.asked(w.provider.bodies().len() - 1)
            .contains("the question before the cut"),
        "the resumed turn continued from it"
    );
}

/// A file whose **header** is corrupt cannot be resumed: the row is drawn with the reason (never
/// silently skipped), the tally counts it, and the load refuses it before reading past the header —
/// leaving the bytes alone.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_corrupt_header_is_a_row_that_says_why_and_a_refused_load() {
    let w = World::new("corrupt-header").await;
    std::fs::create_dir_all(w.sessions()).unwrap();
    let broken = w.sessions().join("1600000000000-broken-header.jsonl");
    std::fs::write(
        &broken,
        "{\"kind\":\"header\",\"version\":4,\"id\":\"broken-heade\n\
         {\"kind\":\"entry\",\"type\":\"message\",\"message\":{}}\n",
    )
    .unwrap();
    let before = std::fs::read(&broken).unwrap();

    let out = w.chat_listing(&["/sessions"]).await;
    let screen = World::screen(&out);
    let panel = panel_of(&screen);
    assert!(
        panel.contains(&short_id(&broken)),
        "the broken file is still a row:\n{screen}"
    );
    let row = line_with(panel, &short_id(&broken));
    assert!(
        row.contains("unreadable"),
        "and it says the header cannot be read: {row}"
    );
    assert!(
        panel.contains("can't be resumed"),
        "the tally counts it:\n{screen}"
    );

    let refused = session_store::load(&broken, &w.repo, "amp-manager")
        .await
        .expect_err("a corrupt header is not a session");
    assert!(matches!(refused, OpenError::Unreadable { .. }), "{refused}");
    assert_eq!(
        std::fs::read(&broken).unwrap(),
        before,
        "nothing was repaired, rewritten or locked"
    );
    assert!(free(&broken), "no guard was left behind");
}

/// A file written in a format this harness does not read is refused **by version** — a different
/// row from "unreadable", and one that names the file, because an older session is what someone
/// rescues by hand.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_unsupported_version_is_an_older_format_row_that_names_the_file() {
    let w = World::new("unsupported-version").await;
    std::fs::create_dir_all(w.sessions()).unwrap();
    let old = w.sessions().join("1600000000000-old-format.jsonl");
    std::fs::write(
        &old,
        format!(
            "{}\n{}\n",
            json!({"kind": "header", "version": 3, "id": "old-format",
                   "createdAt": 1_600_000_000_000_i64, "cwd": w.repo.to_string_lossy()}),
            entry_line(
                2,
                1_600_000_000_000,
                None,
                user_message("a v3 question", 1_600_000_000_000)
            ),
        ),
    )
    .unwrap();

    let out = w.chat_listing(&["/sessions"]).await;
    let screen = World::screen(&out);
    let panel = panel_of(&screen);
    let row = line_with(panel, &short_id(&old));
    assert!(
        row.contains("older format"),
        "the row says it is an older format, not damage: {row}"
    );
    assert!(
        panel.contains("old-format.jsonl"),
        "and names the file:\n{screen}"
    );
    assert!(
        !panel.contains("unreadable"),
        "it is not reported as damage:\n{screen}"
    );

    let refused = session_store::load(&old, &w.repo, "amp-manager")
        .await
        .expect_err("v3 is not this harness's format");
    assert!(
        matches!(refused, OpenError::Incompatible { .. }),
        "{refused}"
    );
}

/// A live worker of this chat blocks a switch: it belongs to the conversation. The refusal names
/// its ticket and `/workers`, and the worker keeps running — nothing is cancelled to make room.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_resume_while_a_worker_is_live_is_refused_and_the_worker_keeps_running() {
    let w = World::new("busy-workers").await;
    let other = exchange(
        &w.sessions(),
        1_600_000_000_000,
        "the-other-one",
        &w.repo,
        "the conversation that must not be entered with a worker live",
        "its answer",
    );
    // The manager dispatches #2085 and its turn ends at once (launch dispatch), so `/sessions`
    // is asked while the worker is still working; the worker's own `sleep` keeps it live.
    let script = json!({
        "boss": [
            {"tool_calls": [{"name": "amp_run_ticket",
                             "arguments": {"task_id": 2085,
                                           "agent_id": "amp:amp-worker-backend",
                                           "worktree": false}}]},
            {"text": "I dispatched it."}
        ],
        "wf": [
            {"tool_calls": [{"name": "bash", "arguments": {"command": "sleep 20"}}]},
            {"text": "the worker is done"}
        ]
    });
    let path = w.dir.join("script.json");
    std::fs::write(&path, script.to_string()).unwrap();
    let path = path.to_string_lossy().into_owned();

    let out = w
        .chat_with(
            WORKER_CONFIG,
            &["run ticket 2085", "/sessions", "/quit"],
            &[("AMP_HARNESS_FAUX_SCRIPT", path.as_str())],
        )
        .await;
    let screen = World::screen(&out);

    assert!(
        !screen.contains("Sessions · this directory"),
        "the picker never opened with a worker live:\n{screen}"
    );
    let refusal = line_with(&screen, "/sessions is unavailable:");
    assert!(
        refusal.contains("#2085") && refusal.contains("/workers shows it"),
        "the refusal names the running worker: {refusal}"
    );
    assert!(!screen.contains("✓ resumed"), "nothing switched:\n{screen}");
    assert!(
        free(&other),
        "the session it would have opened was never locked"
    );
    // The dispatch is this chat's own, and AMP saw exactly one.
    assert_eq!(
        w.amp.mock.calls_to("amp_run_ticket").len(),
        0,
        "harness dispatch is internal, not an AMP tool call"
    );
    assert_eq!(
        w.amp.mock.calls_to("amp_dispatch_task").len(),
        1,
        "the worker was dispatched once, by the chat that asked"
    );
}

// ---------------------------------------------------------------------------------------------
// A reply in flight, and a prompt queued behind it: what `--input` cannot do

/// `--input` waits for each run to finish, so the two states a switch most often meets — a reply
/// still streaming, a prompt queued behind it — need a real terminal. This drives the chat on a
/// pty at 120x40 (as `tests/chat_approval_wait.rs` does) against a model that never answers: the
/// turn stays in flight deterministically, and both refusals are read off the screen.
#[cfg(unix)]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_resume_during_a_reply_is_refused_and_the_queue_is_kept() {
    use std::{
        io::{Read, Write},
        process::Stdio,
    };

    use common::pty_command;
    use nix::{pty::Winsize, pty::openpty};

    /// A provider that accepts the connection and never answers: the turn stays in flight, so the
    /// test never races a stream's speed.
    async fn silent_provider() -> (String, tokio::task::JoinHandle<()>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let task = tokio::spawn(async move {
            let mut held = Vec::new();
            while let Ok((sock, _)) = listener.accept().await {
                // Hold the socket open (and unread): the client waits for a response.
                held.push(sock);
            }
        });
        (format!("http://{addr}/v1"), task)
    }

    let w = World::new("busy-turn").await;
    let other = exchange(
        &w.sessions(),
        1_600_000_000_000,
        "the-other-one",
        &w.repo,
        "the conversation that must not be entered mid-reply",
        "its answer",
    );
    let (url, provider) = silent_provider().await;
    let text = format!(
        r#"
[providers.silent]
api = "openai-completions"
base_url = "{url}"
key_env = "MOCK_KEY"
[providers.silent.models."s-1"]

[roles.manager]
model = "silent/s-1"
"#
    );
    std::fs::write(w.dir.join("home/.config/amp-harness/config.toml"), text).unwrap();

    let (cols, rows) = (120u16, 40u16);
    let size = Winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };
    let pty = openpty(Some(&size), None).expect("openpty");
    let stdio = || Stdio::from(pty.slave.try_clone().unwrap());
    let state = w.xdg_state.to_string_lossy().into_owned();
    let mut child = pty_command()
        .args(["--no-setup"])
        .current_dir(&w.repo)
        .env("HOME", w.dir.join("home"))
        .env("XDG_CONFIG_HOME", w.dir.join("home/.config"))
        .env("XDG_STATE_HOME", state)
        .env("AMP_CREDENTIAL_STORE", "file")
        .env("AMP_MCP_URL", &w.amp.url)
        .env("AMP_HARNESS_CEILING_DIR", &w.dir)
        .env("MOCK_KEY", KEY)
        .env("TERM", "xterm-256color")
        .env_remove("AMP_TOKEN")
        .env_remove("AMP_CA_FILE")
        .env_remove("AMP_WEB_URL")
        .env_remove("AMP_MCP_PROTOCOL")
        .env_remove("AMP_HARNESS_PROFILE")
        .env_remove("AMP_HARNESS_MOUSE")
        .env_remove("AMP_HARNESS_FAUX_SCRIPT")
        .env_remove("ANTHROPIC_API_KEY")
        .env_remove("OPENROUTER_API_KEY")
        .env_remove("RUST_LOG")
        .stdin(stdio())
        .stdout(stdio())
        .stderr(stdio())
        .spawn()
        .expect("spawn the chat");
    drop(pty.slave);
    let mut master = std::fs::File::from(pty.master);
    let mut screen = Screen::new(usize::from(cols), usize::from(rows));
    let mut seen: Vec<u8> = Vec::new();

    /// Pumps the pty until the screen holds `needle`, bounded, and returns the screen's text.
    macro_rules! wait_for {
        ($needle:expr) => {{
            let deadline = std::time::Instant::now() + Duration::from_secs(45);
            loop {
                let mut buf = [0u8; 8192];
                match master.read(&mut buf) {
                    Ok(0) => panic!("the chat exited while waiting for {}", $needle),
                    Ok(n) => {
                        seen.extend_from_slice(&buf[..n]);
                        screen.feed(&String::from_utf8_lossy(&buf[..n]));
                    }
                    Err(_) => {}
                }
                let text = screen.text();
                if text.contains($needle) {
                    break text;
                }
                assert!(
                    std::time::Instant::now() < deadline,
                    "waited for {:?}:\n{text}",
                    $needle
                );
            }
        }};
    }

    // The chat is up, then a prompt starts a turn that never ends.
    //
    // Wait for the *editor*, not the chrome: `AMP harness` and the model name are painted in the
    // first frame, which can land before the terminal is in raw mode — a key written then is
    // discarded by the mode switch (`TCSAFLUSH`), which is how an earlier version of this test
    // lost the first eight characters of `/sessions` and queued `ns` as a prompt. The editor's
    // placeholder is drawn by the first frame that also reads keys.
    wait_for!("/ for commands");
    let _ = master.write_all(b"a question that is never answered\r");
    let _ = master.flush();
    // In flight: the composer says so, and stays saying it (the model never answers).
    wait_for!("Esc stops");

    // Typed mid-reply: `/sessions` is refused by name, and the picker never opens. Written a
    // character at a time: the `/` completion menu is open while a command is typed, and it takes
    // the keys one event at a time.
    for c in b"/sessions" {
        let _ = master.write_all(&[*c]);
        let _ = master.flush();
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let _ = master.write_all(b"\r");
    let _ = master.flush();
    let text = wait_for!("/sessions is unavailable:");
    assert!(
        text.contains("the manager is replying"),
        "the refusal names the reply in flight:\n{text}"
    );
    assert!(
        text.contains("Your chat is unchanged"),
        "and promises nothing changed:\n{text}"
    );
    assert!(
        !text.contains("Sessions · this directory"),
        "the picker never opened:\n{text}"
    );

    // Typed while it still runs: queued behind the reply, not refused and not cancelled.
    for c in b"queued behind it" {
        let _ = master.write_all(&[*c]);
        let _ = master.flush();
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let _ = master.write_all(b"\r");
    let _ = master.flush();
    let text = wait_for!("queued behind it");
    assert!(
        text.contains("queued #1") || text.contains("1 queued"),
        "the prompt is queued behind the run:\n{text}"
    );
    assert!(
        !text.contains("✓ resumed"),
        "nothing was resumed, and nothing was cancelled to make room:\n{text}"
    );

    // Leave: Esc stops the reply, `/quit` ends the chat.
    let _ = master.write_all(b"\x1b");
    let _ = master.flush();
    tokio::time::sleep(Duration::from_millis(300)).await;
    let _ = master.write_all(b"/quit\r");
    let _ = master.flush();
    let deadline = std::time::Instant::now() + Duration::from_secs(30);
    loop {
        let mut buf = [0u8; 8192];
        match master.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                seen.extend_from_slice(&buf[..n]);
                screen.feed(&String::from_utf8_lossy(&buf[..n]));
            }
            Err(_) => {}
        }
        if child.try_wait().ok().flatten().is_some() || std::time::Instant::now() > deadline {
            break;
        }
    }
    let _ = child.kill();
    let _ = child.wait();
    provider.abort();
    assert!(
        free(&other),
        "the session it would have opened was never locked"
    );
    // The chat's own session was written; the one it never entered was not.
    assert!(
        w.session_files().iter().any(|p| p != &other),
        "the chat kept its own conversation"
    );
    let untouched = std::fs::metadata(&other).map(|m| m.len()).unwrap_or(0);
    assert_eq!(
        untouched,
        std::fs::metadata(&other).map(|m| m.len()).unwrap_or(1),
        "the other session's file was not written"
    );
}

/// A terminal's character grid, fed the chat's output (`tests/chat_approval_wait.rs`'s model,
/// trimmed to what the chat's glyphs need: absolute and relative cursor moves, erase in line and
/// display).
#[cfg(unix)]
struct Screen {
    rows: Vec<Vec<char>>,
    width: usize,
    row: usize,
    col: usize,
}

#[cfg(unix)]
impl Screen {
    fn new(width: usize, height: usize) -> Self {
        Self {
            rows: vec![vec![' '; width]; height],
            width,
            row: 0,
            col: 0,
        }
    }

    fn feed(&mut self, text: &str) {
        let mut chars = text.chars().peekable();
        while let Some(c) = chars.next() {
            match c {
                '\x1b' => match chars.next() {
                    Some('[') => {
                        let mut params = String::new();
                        let mut fin = ' ';
                        for p in chars.by_ref() {
                            if ('@'..='~').contains(&p) {
                                fin = p;
                                break;
                            }
                            params.push(p);
                        }
                        self.csi(&params, fin);
                    }
                    Some(']' | '_' | 'P' | '^') => {
                        while let Some(p) = chars.next() {
                            if p == '\x07' {
                                break;
                            }
                            if p == '\x1b' && chars.peek() == Some(&'\\') {
                                chars.next();
                                break;
                            }
                        }
                    }
                    _ => {}
                },
                '\r' => self.col = 0,
                '\n' => self.down(1),
                '\x07' | '\x08' => {}
                c if c >= ' ' => {
                    if self.col >= self.width {
                        self.col = 0;
                        self.down(1);
                    }
                    if let Some(r) = self.rows.get_mut(self.row) {
                        r[self.col] = c;
                    }
                    self.col += 1;
                }
                _ => {}
            }
        }
    }

    fn down(&mut self, n: usize) {
        self.row += n;
        while self.row >= self.rows.len() {
            self.rows.remove(0);
            self.rows.push(vec![' '; self.width]);
            self.row -= 1;
        }
    }

    fn csi(&mut self, params: &str, fin: char) {
        if params.starts_with(['?', '>', '<', '=']) {
            return;
        }
        let nums: Vec<usize> = params.split(';').map(|p| p.parse().unwrap_or(0)).collect();
        let n = |i: usize| nums.get(i).copied().filter(|v| *v > 0).unwrap_or(1);
        let height = self.rows.len();
        match fin {
            'H' | 'f' => {
                self.row = (n(0) - 1).min(height - 1);
                self.col = (n(1) - 1).min(self.width - 1);
            }
            'A' => self.row = self.row.saturating_sub(n(0)),
            'B' => self.row = (self.row + n(0)).min(height - 1),
            'C' => self.col = (self.col + n(0)).min(self.width - 1),
            'D' => self.col = self.col.saturating_sub(n(0)),
            'G' => self.col = (n(0) - 1).min(self.width - 1),
            'd' => self.row = (n(0) - 1).min(height - 1),
            'K' => {
                let r = &mut self.rows[self.row];
                match nums.first().copied().unwrap_or(0) {
                    0 => r[self.col.min(self.width)..].fill(' '),
                    1 => r[..=self.col.min(self.width - 1)].fill(' '),
                    _ => r.fill(' '),
                }
            }
            'J' => match nums.first().copied().unwrap_or(0) {
                0 => {
                    self.rows[self.row][self.col.min(self.width)..].fill(' ');
                    for r in &mut self.rows[self.row + 1..] {
                        r.fill(' ');
                    }
                }
                _ => {
                    for r in &mut self.rows {
                        r.fill(' ');
                    }
                }
            },
            _ => {}
        }
    }

    fn text(&self) -> String {
        self.rows
            .iter()
            .map(|r| r.iter().collect::<String>().trim_end().to_owned())
            .collect::<Vec<_>>()
            .join("\n")
    }
}

// ---------------------------------------------------------------------------------------------
// Remote control: the one refusal that needs a live link

/// Remote control being **on** blocks a switch even with an empty queue, because a paired device
/// can send at any moment: an empty queue is not idleness. The TUI's own suite cannot reach this
/// (it has no relay to turn a link on), so it is checked here with a real `RemoteLink` enabled
/// against `amp-core`'s mock relay, in-process — the shape `tests/chat_queue.rs` uses.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn remote_control_on_refuses_a_switch_with_nothing_queued() {
    use amp_core::{
        events::ChannelEmitter,
        manager_feed::ManagerFeed,
        remote::{
            FixedToken, HostStatus, LinkTuning, RemoteDeps, RemoteHost, RemoteLink, RemoteSetup,
            crypto::KeyStore, mock_relay::MockRelay, redact::Redactor,
        },
    };
    use amp_tui::{ResumeFuture, ResumeRequest, SessionControl, SessionList, SessionListFuture};
    use rpi_ai::providers::faux::FauxProvider;

    /// The link's side of the chat. Nothing is ever sent to it in this test: the point is that the
    /// link is *on*, so what it would do with a prompt does not matter.
    struct Host;
    impl RemoteHost for Host {
        fn start_prompt(
            &self,
            _prompt_id: String,
            _text: String,
            _origin: amp_core::chat_event::PromptOrigin,
        ) -> futures::future::BoxFuture<'static, bool> {
            Box::pin(async { false })
        }
        fn stop(&self) -> futures::future::BoxFuture<'static, ()> {
            Box::pin(async {})
        }
        fn status(&self) -> HostStatus {
            HostStatus::default()
        }
    }

    /// The scan, recorded so the test can see whether the picker ever asked for one.
    #[derive(Clone, Default)]
    struct Control(Arc<std::sync::Mutex<usize>>);
    impl SessionControl for Control {
        fn list(&self) -> SessionListFuture {
            *self.0.lock().unwrap() += 1;
            Box::pin(async {
                Ok(SessionList {
                    scope: "/work".to_owned(),
                    rows: Vec::new(),
                    current: None,
                })
            })
        }
        fn resume(&self, _request: ResumeRequest) -> ResumeFuture {
            Box::pin(async { Err("a resume was asked for while remote control was on".to_owned()) })
        }
    }

    // The relay accepts exactly the one token it was configured with (as the real AMP accepts the
    // login's), so the link's `FixedToken` has to be that one.
    let options = amp_core::remote::mock_relay::MockOptions::default();
    let token = options.oauth_token.clone();
    let mock = MockRelay::start(options).await;
    let settings = RemoteSetup {
        api_base: mock.base.clone(),
        web_origin: "https://amp.example.test".into(),
        ca_file: None,
        project_id: Some(2),
        host: "test-host".into(),
        os: "macos".into(),
        version: "0.0.0-test".into(),
    };
    let feed = ManagerFeed::new();
    let provider = FauxProvider::new(Default::default());
    let model = provider.default_model().clone();
    let (emitter, mut events) = ChannelEmitter::new();
    let harness = rpi_harness::agent_harness::AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: amp_core::agent::memory_session(),
        models: vec![provider],
        agent_emitter: Some(emitter.into_emitter()),
        ..Default::default()
    })
    .await
    .expect("harness");
    let info = amp_tui::ShellInfo {
        role: "amp-manager".into(),
        provider: model.provider.to_string(),
        model_id: model.id.clone(),
        thinking: rpi_ai::ThinkingLevel::Off,
        requested_thinking: None,
        context_window: 200_000,
        cwd: "/work".into(),
        git_branch: None,
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, mut done) = amp_tui::Shell::new(Arc::new(harness), info, Vec::new());
    shell.set_feed(Some(Arc::clone(&feed)));
    let _flusher = feed.spawn_flusher();
    let control = Control::default();
    shell.set_session_control(Arc::new(control.clone()));

    // Remote control off: `/sessions` opens, and the picker asks for a scan (the scan runs on a
    // task, so the shell is polled until it lands).
    assert_eq!(shell.submit("/sessions").await, amp_tui::Submit::Continue);
    for _ in 0..200 {
        tokio::time::sleep(Duration::from_millis(5)).await;
        shell.poll();
        if *control.0.lock().unwrap() == 1 {
            break;
        }
    }
    assert_eq!(*control.0.lock().unwrap(), 1, "the scan was asked for");
    assert!(shell.view().panel().is_open(), "the picker opened");
    shell.press("esc").await.expect("close it");
    assert!(!shell.view().panel().is_open(), "Esc closed it");

    // Turn the link on (a real register against the mock relay), hand it to the shell, and try
    // the same command: refused, with no scan at all.
    let link = RemoteLink::new(RemoteDeps {
        setup: settings,
        tokens: Arc::new(FixedToken(token, 7)),
        host: Arc::new(Host),
        feed: Arc::clone(&feed),
        keys: KeyStore::memory(),
        pins: None,
        action_log: None,
        pending_ends: None,
        redactor: Redactor::new(),
        switches: Arc::new(|| Ok(())),
        tuning: LinkTuning::default(),
    });
    link.enable().await.expect("the link comes up");
    assert!(link.is_on(), "and it is on");
    assert_eq!(
        link.current(),
        amp_core::remote::RemoteState::Starting,
        "nothing is paired yet, and the queue is empty"
    );
    shell.set_remote(Ok(link), None);
    assert_eq!(shell.submit("/sessions").await, amp_tui::Submit::Continue);
    for _ in 0..20 {
        tokio::time::sleep(Duration::from_millis(5)).await;
        shell.poll();
    }
    assert_eq!(
        *control.0.lock().unwrap(),
        1,
        "no second scan: the picker never opened while remote control was on"
    );
    let screen = shell.view().render_plain(120, 40).join("\n");
    let flat = screen.split_whitespace().collect::<Vec<_>>().join(" ");
    assert!(
        flat.contains("remote control is on"),
        "the refusal names remote control:\n{screen}"
    );
    assert!(
        flat.contains("/remote off"),
        "and says what to do first:\n{screen}"
    );
    assert!(
        flat.contains("Your chat is unchanged"),
        "and that nothing changed:\n{screen}"
    );
    let _ = (&mut events, &mut done);
}

// ---------------------------------------------------------------------------------------------
// Small helpers

/// A user prompt as the harness stores it.
fn for_prompt(text: &str) -> AgentMessage {
    AgentMessage::User(UserMessage {
        role: Default::default(),
        content: UserContent::Text(text.to_owned()),
        timestamp: 1_767_225_600_000,
    })
}

/// An assistant message for a file written directly (the compaction test's later reply).
fn faux_reply(text: &str) -> rpi_ai::types::AssistantMessage {
    use rpi_ai::{
        providers::faux::{FauxBlock, faux_assistant_message},
        types::StopReason,
    };
    faux_assistant_message(vec![FauxBlock::text(text)], StopReason::Stop)
}
