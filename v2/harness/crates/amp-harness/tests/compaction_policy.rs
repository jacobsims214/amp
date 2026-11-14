//! AMP #4262: automatic-compaction edge cases, pinned against rpi-harness 0.8.0 as it is.
//!
//! Spec: KB `specs/harness-auto-compaction-policy.md` §7, rows T-A3, T-F1, T-F2, T-F3, T-F6 and
//! T-I3 (rule IDs A/F/I/K are that spec's). These tests document what rpi does today. A failure
//! here means rpi's behaviour moved, and the spec (and the shell's notices from #4263) must move
//! with it. rpi is not patched.
//!
//! The rig is #4229's (`tests/chat_sessions.rs`, "Compaction end to end") one level down: the
//! harness itself on a real session file, without the shell. Like #4229's rig, the faux model
//! answers rpi's summary calls and the chat's turns from two queues and records every request. It
//! adds three things:
//! - a real tool (`fetch`), so a tool window has a start and an end;
//! - one timeline where the agent events (tool start/end) and the harness bus's
//!   `CompactionProgress` events are pushed in emit order;
//! - summary failures that are scripted per call.
//!
//! Every test uses its own state under cargo's target tmp dir and the faux provider. There is no
//! network, keychain, installed binary or process environment change.

mod common;

use std::{
    collections::VecDeque,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};

use amp_core::session_store;
use async_trait::async_trait;
use rpi_agent::{AgentEmitter, AgentEvent, AgentTool, AgentToolResult};
use rpi_ai::{
    providers::faux::{FauxProvider, FauxScript, FauxStep, faux_assistant_message},
    types::{Context, Schema, StopReason, Tool},
};
use rpi_harness::{
    agent_harness::{AgentHarness, AgentLane, HarnessRunOutcome, RunResult},
    events::HarnessEvent,
    session::types::{
        BranchBounds, CompactionEntry, CompactionReason, Entry, EntryOrder, EntryQuery, LaneRecord,
        RecordQuery,
    },
    types::{AgentHarnessOptions, CompactionSettings, HarnessTool},
};
use serde_json::{Value, json};

// ---------------------------------------------------------------------------------------------
// The rig

/// rpi compacts once its estimate passes 30 000 - 16 384 (default reserve) = 13 616.
const WINDOW: u64 = 30_000;
const THRESHOLD: i64 = WINDOW as i64 - 16_384;
/// Small, so earlier turns are what gets summarized (as in #4229's rig).
const KEEP: i64 = 2_000;

fn settings() -> CompactionSettings {
    CompactionSettings {
        keep_recent_tokens: KEEP,
        ..CompactionSettings::default()
    }
}

/// A faux turn or summary that fails. The message is not one rpi's retry classifier treats as
/// transient, so any second request a test sees is a replay, not a retry.
fn failure(message: &str) -> rpi_ai::types::AssistantMessage {
    let mut m = faux_assistant_message("", StopReason::Error);
    m.error_message = Some(message.into());
    m
}

/// The faux model: summary calls and turns from separate queues, every request recorded.
#[derive(Default)]
struct Router {
    /// One per summary call, in order: `Ok(text)` or `Err(provider error)`. Empty: `Ok("SUMMARY")`.
    summaries: VecDeque<Result<String, String>>,
    turns: VecDeque<FauxStep>,
    /// Used when `turns` is empty (T-I3's model that keeps calling the tool). None: an error turn.
    otherwise: Option<FauxStep>,
    /// Every model call in order: `"summary"` or `"turn"`, and the messages it was sent.
    calls: Vec<(&'static str, Value)>,
}

impl Router {
    fn of(&self, kind: &str) -> Vec<&Value> {
        self.calls
            .iter()
            .filter(|(k, _)| *k == kind)
            .map(|(_, v)| v)
            .collect()
    }

    fn order(&self) -> Vec<&'static str> {
        self.calls.iter().map(|(k, _)| *k).collect()
    }
}

fn routed(router: &Arc<Mutex<Router>>) -> Vec<FauxStep> {
    (0..60)
        .map(|_| {
            let router = Arc::clone(router);
            FauxStep::factory(move |ctx: &Context| {
                let mut r = router.lock().unwrap();
                let summary = ctx
                    .system_prompt
                    .as_deref()
                    .is_some_and(|s| s.starts_with("You are a context summarization"));
                let messages = serde_json::to_value(&ctx.messages).expect("messages serialize");
                if summary {
                    r.calls.push(("summary", messages));
                    return match r.summaries.pop_front() {
                        Some(Err(e)) => failure(&e),
                        Some(Ok(text)) => faux_assistant_message(text, StopReason::Stop),
                        None => faux_assistant_message("SUMMARY", StopReason::Stop),
                    };
                }
                r.calls.push(("turn", messages));
                let step = r.turns.pop_front().or_else(|| r.otherwise.clone());
                match step {
                    Some(FauxStep::Message(m)) => m,
                    Some(FauxStep::Factory(f)) => f(ctx),
                    None => failure("no more scripted turns"),
                }
            })
        })
        .collect()
}

/// What the timeline records, in emit order.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Mark {
    ToolStart(String),
    ToolEnd(String),
    Compacting { active: bool, manual: bool },
}

type Timeline = Arc<Mutex<Vec<Mark>>>;

/// Pushes the agent events the tests read onto the timeline. rpi awaits `emit`, so the order is
/// the loop's order.
struct Recorder(Timeline);

impl AgentEmitter for Recorder {
    fn emit(&self, event: AgentEvent) -> futures::future::BoxFuture<'static, ()> {
        let mark = match event {
            AgentEvent::ToolExecutionStart { tool_call_id, .. } => {
                Some(Mark::ToolStart(tool_call_id))
            }
            AgentEvent::ToolExecutionEnd { tool_call_id, .. } => Some(Mark::ToolEnd(tool_call_id)),
            _ => None,
        };
        if let Some(mark) = mark {
            self.0.lock().unwrap().push(mark);
        }
        Box::pin(async {})
    }
}

/// A tool that takes a little time (so its window is real) and returns `size` characters.
struct Fetch {
    schema: Tool,
    size: usize,
}

#[async_trait]
impl AgentTool for Fetch {
    fn schema(&self) -> &Tool {
        &self.schema
    }
    fn label(&self) -> &str {
        "fetch"
    }
    async fn execute(
        &self,
        _id: &str,
        _params: Value,
        _signal: tokio_util::sync::CancellationToken,
        _on_update: Arc<dyn Fn(rpi_agent::ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, rpi_agent::AgentError> {
        tokio::time::sleep(Duration::from_millis(30)).await;
        Ok(AgentToolResult::text(format!(
            "FETCHED {}",
            "z".repeat(self.size)
        )))
    }
}

fn fetch(size: usize) -> HarnessTool {
    HarnessTool::new(Arc::new(Fetch {
        schema: Tool {
            name: "fetch".into(),
            description: "fetch a page".into(),
            parameters: Schema(json!({"type": "object", "properties": {}})),
            constrained_sampling: None,
        },
        size,
    }))
}

/// A harness on a real session file, with a timeline of its tool and compaction events.
struct Rig {
    dir: PathBuf,
    harness: AgentHarness,
    path: PathBuf,
    timeline: Timeline,
    _watch: rpi_harness::watcher::HarnessWatcher,
}

impl Rig {
    /// `tool_size`: how many characters `fetch` returns.
    async fn new(name: &str, router: &Arc<Mutex<Router>>, tool_size: usize) -> Self {
        let dir = common::workdir(&format!("compaction-policy-{name}"), None);
        let script = FauxScript::new();
        script.set_responses(routed(router));
        let provider = FauxProvider::new(script);
        let mut model = provider.default_model().clone();
        model.context_window = WINDOW;
        let opened =
            session_store::create(&dir.join("sessions"), &dir.join("project"), "amp-manager")
                .await
                .expect("a new session file");
        let timeline: Timeline = Arc::default();
        let harness = AgentHarness::create(AgentHarnessOptions {
            model,
            session: opened.session,
            models: vec![provider],
            tools: vec![fetch(tool_size)],
            agent_emitter: Some(Arc::new(Recorder(Arc::clone(&timeline)))),
            compaction: settings(),
            ..Default::default()
        })
        .await
        .expect("harness");
        let watch = watch(&harness, &timeline);
        Self {
            dir,
            harness,
            path: opened.path,
            timeline,
            _watch: watch,
        }
    }

    async fn say(&self, text: &str) -> RunResult {
        tokio::time::timeout(WAIT, self.harness.prompt_text(text, Vec::new()))
            .await
            .expect("the run ends")
            .expect("the run")
    }

    fn timeline(&self) -> Vec<Mark> {
        self.timeline.lock().unwrap().clone()
    }
}

const WAIT: Duration = Duration::from_secs(60);

/// Pushes the bus's `CompactionProgress` events onto `timeline`.
fn watch(harness: &AgentHarness, timeline: &Timeline) -> rpi_harness::watcher::HarnessWatcher {
    let timeline = Arc::clone(timeline);
    harness.watch(move |event| {
        if let HarnessEvent::CompactionProgress(p) = event {
            timeline.lock().unwrap().push(Mark::Compacting {
                active: p.active,
                manual: p.manual,
            });
        }
    })
}

/// The compaction entries on `harness`'s branch, oldest first, each with the reason rpi recorded.
async fn compactions(harness: &AgentHarness) -> Vec<(CompactionEntry, Option<CompactionReason>)> {
    let reasons: std::collections::HashMap<String, _> = harness
        .session()
        .find_records(&RecordQuery {
            record_type: Some("step_attempt"),
            ..Default::default()
        })
        .await
        .expect("records")
        .into_iter()
        .filter_map(|r| match r {
            LaneRecord::StepAttempt(a) => Some((a.result_entry_id, a.compaction_reason)),
            _ => None,
        })
        .collect();
    harness
        .find_entries_on_branch(
            &EntryQuery {
                entry_type: Some("compaction"),
                order: Some(EntryOrder::OldestFirst),
                ..Default::default()
            },
            &BranchBounds::default(),
        )
        .await
        .expect("branch")
        .into_iter()
        .filter_map(|e| match e {
            Entry::Compaction(c) => {
                let reason = reasons.get(&c.base.id).copied().flatten();
                Some((c, reason))
            }
            _ => None,
        })
        .collect()
}

/// Each line of a session file as (`type`, line).
fn file_entries(path: &Path) -> Vec<(String, String)> {
    std::fs::read_to_string(path)
        .expect("the session file")
        .lines()
        .map(|l| {
            let v: Value = serde_json::from_str(l).expect("a JSON line");
            (
                v["type"].as_str().unwrap_or_default().to_owned(),
                l.to_owned(),
            )
        })
        .collect()
}

/// How many `message` lines of the file contain `needle`.
fn messages_with(path: &Path, needle: &str) -> usize {
    file_entries(path)
        .iter()
        .filter(|(t, l)| t == "message" && l.contains(needle))
        .count()
}

fn failed(result: &RunResult) -> &rpi_harness::session::types::OperationError {
    match &result.outcome {
        HarnessRunOutcome::Failed { error, .. } => error,
        other => panic!("the run failed: {other:?}"),
    }
}

fn completed(result: &RunResult) -> &rpi_ai::types::AssistantMessage {
    match &result.outcome {
        HarnessRunOutcome::Completed { final_message, .. } => final_message,
        other => panic!("the run completed: {other:?}"),
    }
}

/// The first two prompts stay under the trigger; the third takes the pre-run estimate over it
/// (faux usage is chars/4 of the request). Same sizes as #4229's T-A1.
fn early() -> String {
    format!("EARLY-TURN {}", "alpha ".repeat(3_000))
}
fn middle() -> String {
    format!("MIDDLE-TURN {}", "beta ".repeat(6_000))
}
fn newest() -> String {
    format!("NEW-PROMPT {}", "gamma ".repeat(2_000))
}

/// Two prompts under the trigger, answered.
async fn two_turns(rig: &Rig, router: &Arc<Mutex<Router>>) {
    {
        let mut r = router.lock().unwrap();
        r.turns.push_back(FauxStep::text("first answer"));
        r.turns.push_back(FauxStep::text("second answer"));
    }
    completed(&rig.say(&early()).await);
    completed(&rig.say(&middle()).await);
    assert!(
        compactions(&rig.harness).await.is_empty(),
        "under the trigger"
    );
    assert_eq!(router.lock().unwrap().order(), ["turn", "turn"]);
}

// ---------------------------------------------------------------------------------------------
// T-A3 (spec A3, A4, I1)

/// Automatic compaction never happens while a tool is executing. A tool loop crosses the trigger
/// between turns (A2), and the next prompt crosses it again before its run (A1). Every
/// `CompactionProgress{active:true}` falls outside every `ToolExecutionStart..End` window, and no
/// tool starts while a compaction is in progress.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn automatic_compaction_never_overlaps_a_tool_execution() {
    let router = Arc::new(Mutex::new(Router::default()));
    {
        let mut r = router.lock().unwrap();
        r.turns.push_back(FauxStep::text("first answer"));
        // ~10 000 tokens of arguments: after its result, the estimate is over the trigger.
        r.turns.push_back(FauxStep::tool_call(
            "fetch",
            json!({ "pad": "x".repeat(40_000) }),
        ));
        r.turns.push_back(FauxStep::tool_call("fetch", json!({})));
        r.turns.push_back(FauxStep::text("AFTER-THE-LOOP"));
        r.turns.push_back(FauxStep::text("LAST-ANSWER"));
    }
    let rig = Rig::new("a3", &router, 200).await;
    completed(
        &rig.say(&format!("EARLY-TURN {}", "alpha ".repeat(4_000)))
            .await,
    );
    completed(&rig.say("go").await);
    // ~15 000 tokens on its own: over the trigger before the run (A1).
    completed(
        &rig.say(&format!("BIG-PROMPT {}", "gamma ".repeat(10_000)))
            .await,
    );

    let found = compactions(&rig.harness).await;
    assert!(
        found
            .iter()
            .all(|(_, r)| *r == Some(CompactionReason::Threshold)),
        "{found:?}"
    );
    let order = router.lock().unwrap().order();
    // A2: a summary call between two turns of the tool loop (turns 2 and 3 are its tool calls).
    assert_eq!(&order[..3], ["turn", "turn", "summary"], "{order:?}");
    assert!(found.len() >= 2, "an A2 and an A1 compaction: {order:?}");

    let timeline = rig.timeline();
    let starts = timeline
        .iter()
        .filter(|m| matches!(m, Mark::ToolStart(_)))
        .count();
    let compacting = timeline
        .iter()
        .filter(|m| matches!(m, Mark::Compacting { active: true, .. }))
        .count();
    assert_eq!(starts, 2, "two tool windows: {timeline:?}");
    assert_eq!(
        compacting,
        found.len(),
        "one progress per compaction: {timeline:?}"
    );

    let mut open: Vec<&str> = Vec::new();
    let mut in_compaction = false;
    for mark in &timeline {
        match mark {
            Mark::ToolStart(id) => {
                assert!(
                    !in_compaction,
                    "{id} started during a compaction: {timeline:?}"
                );
                open.push(id);
            }
            Mark::ToolEnd(id) => {
                assert!(
                    open.contains(&id.as_str()),
                    "{id} ended unopened: {timeline:?}"
                );
                open.retain(|o| o != id);
            }
            Mark::Compacting { active, manual } => {
                assert!(!manual, "automatic only: {timeline:?}");
                if *active {
                    assert!(
                        open.is_empty(),
                        "a compaction started inside tool window(s) {open:?}: {timeline:?}"
                    );
                }
                in_compaction = *active;
            }
        }
    }
    assert!(open.is_empty() && !in_compaction, "{timeline:?}");
}

// ---------------------------------------------------------------------------------------------
// T-F1 (spec F1, I4)

/// The pre-run summary call fails. The run ends `Failed{code:"compaction_failed"}`, no compaction
/// entry is written, the prompt is persisted exactly once without a reply, and nothing re-sends it:
/// there is no turn request after the failed summary call. (#4263 maps this failure to the shell's
/// own notice, G1. This test pins only rpi's side.)
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_failed_pre_run_compaction_fails_the_run_and_never_replays_the_prompt() {
    let router = Arc::new(Mutex::new(Router::default()));
    let rig = Rig::new("f1", &router, 200).await;
    two_turns(&rig, &router).await;
    {
        let mut r = router.lock().unwrap();
        r.summaries
            .push_back(Err("summarizer rejected the request".into()));
        r.turns.push_back(FauxStep::text("NEVER-SENT"));
    }

    let result = rig.say(&newest()).await;
    let error = failed(&result);
    assert_eq!(error.code, "compaction_failed", "{error:?}");
    assert!(
        error.message.contains("summarizer rejected the request"),
        "{error:?}"
    );
    assert!(
        compactions(&rig.harness).await.is_empty(),
        "no compaction entry"
    );

    // Let anything that would retry or replay have its chance.
    tokio::time::sleep(Duration::from_millis(300)).await;
    let r = router.lock().unwrap();
    assert_eq!(
        r.order(),
        ["turn", "turn", "summary"],
        "no turn request after the failed summary, and no second summary call"
    );
    drop(r);
    assert_eq!(messages_with(&rig.path, "NEW-PROMPT"), 1, "persisted once");
    assert_eq!(messages_with(&rig.path, "NEVER-SENT"), 0);
    let entries = file_entries(&rig.path);
    assert!(!entries.iter().any(|(t, _)| t == "compaction"));
    // The prompt is the last message: there is no reply to it.
    let last_message = entries
        .iter()
        .rev()
        .find(|(t, _)| t == "message")
        .expect("a message");
    assert!(last_message.1.contains("NEW-PROMPT"), "{}", last_message.1);
}

// ---------------------------------------------------------------------------------------------
// T-F2 (spec F2)

/// The between-turn summary call fails. rpi skips the compaction without an error: the loop goes
/// on with the uncompacted context, no compaction entry is written, and the progress events still
/// show it starting and stopping. This active->inactive pair with no new entry is what #4263's G2
/// notice detects.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_failed_between_turn_compaction_is_skipped_and_the_loop_continues() {
    let router = Arc::new(Mutex::new(Router::default()));
    {
        let mut r = router.lock().unwrap();
        r.turns.push_back(FauxStep::text("first answer"));
        r.turns.push_back(FauxStep::tool_call(
            "fetch",
            json!({ "pad": "x".repeat(40_000) }),
        ));
        r.turns.push_back(FauxStep::text("AFTER-THE-LOOP"));
        r.summaries
            .push_back(Err("summarizer rejected the request".into()));
    }
    let rig = Rig::new("f2", &router, 200).await;
    completed(
        &rig.say(&format!("EARLY-TURN {}", "alpha ".repeat(4_000)))
            .await,
    );
    let reply = completed(&rig.say("go").await).clone();
    assert!(
        format!("{:?}", reply.content).contains("AFTER-THE-LOOP"),
        "{reply:?}"
    );

    assert!(
        compactions(&rig.harness).await.is_empty(),
        "no compaction entry"
    );
    assert!(
        !file_entries(&rig.path)
            .iter()
            .any(|(t, _)| t == "compaction")
    );

    let r = router.lock().unwrap();
    // No retry of the summary (A2 passes `retry: None`) and no replay of the tool turn.
    assert_eq!(r.order(), ["turn", "turn", "summary", "turn"]);
    let next = r.of("turn")[2].to_string();
    assert!(
        next.contains("EARLY-TURN") && !next.contains("<summary>"),
        "the next turn is sent the uncompacted context"
    );
    drop(r);

    let progress: Vec<Mark> = rig
        .timeline()
        .into_iter()
        .filter(|m| matches!(m, Mark::Compacting { .. }))
        .collect();
    assert_eq!(
        progress,
        [
            Mark::Compacting {
                active: true,
                manual: false
            },
            Mark::Compacting {
                active: false,
                manual: false
            },
        ]
    );
}

// ---------------------------------------------------------------------------------------------
// T-F3 (spec F3, I5)

/// A single prompt larger than the window. The provider rejects the request and that ends the run
/// as a failure that names the provider's error. The prompt is in the file once and in full, and
/// nothing is trimmed, dropped or retried with a smaller context.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_prompt_over_the_window_fails_on_the_provider_error_without_trimming() {
    let router = Arc::new(Mutex::new(Router::default()));
    {
        // The faux provider enforces the window as a real one would.
        let mut r = router.lock().unwrap();
        r.turns.push_back(FauxStep::factory(|ctx: &Context| {
            let chars = serde_json::to_string(&ctx.messages).unwrap().len();
            if chars / 4 > WINDOW as usize {
                failure(&format!(
                    "prompt is too long: {} tokens > {WINDOW} maximum",
                    chars / 4
                ))
            } else {
                faux_assistant_message("UNEXPECTED-REPLY", StopReason::Stop)
            }
        }));
    }
    let rig = Rig::new("f3", &router, 200).await;
    let huge = format!("HUGE-PROMPT {}", "omega ".repeat(25_000));
    assert!(huge.len() / 4 > WINDOW as usize);

    let result = rig.say(&huge).await;
    let error = failed(&result);
    assert!(error.message.contains("prompt is too long"), "{error:?}");

    tokio::time::sleep(Duration::from_millis(300)).await;
    let order = {
        let r = router.lock().unwrap();
        let turns = r.of("turn");
        assert_eq!(
            turns.len(),
            1,
            "one model request, no retry: {:?}",
            r.order()
        );
        // The request carried the whole prompt: nothing was trimmed to fit.
        assert!(
            turns[0].to_string().contains(&huge),
            "the prompt was sent whole"
        );
        // Pinned: the pre-run trigger does fire (the estimate is over W - R). With the prompt as
        // the only message there is no earlier turn to summarize, so the prompt is kept whole and the
        // compaction frees nothing (the I3 cut-point fallback).
        r.order()
    };
    let found = compactions(&rig.harness).await;
    assert_eq!(order, ["summary", "turn"], "{order:?}");
    assert_eq!(found.len(), 1, "{found:?}");
    assert_eq!(found[0].1, Some(CompactionReason::Threshold));
    let tail = serde_json::to_string(&found[0].0.retained_tail).unwrap();
    assert!(tail.contains(&huge), "the prompt is kept whole in the tail");

    // On disk: the prompt once and in full, and no reply.
    assert_eq!(messages_with(&rig.path, "HUGE-PROMPT"), 1);
    let entries = file_entries(&rig.path);
    let line = &entries
        .iter()
        .find(|(t, l)| t == "message" && l.contains("HUGE-PROMPT"))
        .unwrap()
        .1;
    assert!(line.contains(&huge), "the persisted prompt is not trimmed");
    assert_eq!(messages_with(&rig.path, "UNEXPECTED-REPLY"), 0);
}

// ---------------------------------------------------------------------------------------------
// T-F6 (spec F6, K4)

/// A1 compacts, then the run's model call fails. Pinned on rpi 0.8.0: the next prompt does
/// **not** compact again. The spec's F6 row expected a second compaction from stale usage, but the
/// stale usage never reaches the estimate. After A1 the context is the summary plus the tail (the
/// failed prompt and its error reply). rpi's estimate skips assistant messages that ended in
/// `Error`/`Aborted` (`last_assistant_usage_info`, rpi:compaction/tokens.rs:297), so it falls back
/// to chars/4 of that small context, which is under the trigger. (A stale estimate does re-trigger
/// when the kept tail holds a *successful* reply, as after a manual `/compact`. That is #4229's
/// T-K4.) The next run is sent the first summary, then the failed prompt, then the new one. The
/// failed prompt is not re-run as a prompt of its own, and nothing is retried.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_run_failing_after_a_pre_run_compaction_does_not_compact_again_on_the_next_prompt() {
    let router = Arc::new(Mutex::new(Router::default()));
    let rig = Rig::new("f6", &router, 200).await;
    two_turns(&rig, &router).await;
    {
        let mut r = router.lock().unwrap();
        r.summaries.push_back(Ok("SUMMARY-ONE".into()));
        r.turns
            .push_back(FauxStep::message(failure("model refused the request")));
    }
    let result = rig.say(&newest()).await;
    let error = failed(&result);
    assert!(
        error.message.contains("model refused the request"),
        "{error:?}"
    );
    let found = compactions(&rig.harness).await;
    assert_eq!(found.len(), 1, "A1 compacted before the failing call");
    assert_eq!(found[0].1, Some(CompactionReason::Threshold));
    assert!(found[0].0.summary.contains("SUMMARY-ONE"));
    assert_eq!(
        router.lock().unwrap().order(),
        ["turn", "turn", "summary", "turn"],
        "the failed call is not retried"
    );

    {
        let mut r = router.lock().unwrap();
        r.summaries.push_back(Ok("UNEXPECTED-SUMMARY".into()));
        r.turns.push_back(FauxStep::text("RECOVERED"));
    }
    let reply = completed(&rig.say("try again").await).clone();
    assert!(format!("{:?}", reply.content).contains("RECOVERED"));

    let found = compactions(&rig.harness).await;
    assert_eq!(found.len(), 1, "no second compaction: {found:?}");
    let r = router.lock().unwrap();
    assert_eq!(
        r.order(),
        ["turn", "turn", "summary", "turn", "turn"],
        "no summary call before the next reply"
    );
    // The first summary is still what the model starts from, with the kept tail after it.
    let asked = r.of("turn")[3];
    let first = asked[0].to_string();
    assert!(
        first.contains("<summary>") && first.contains("SUMMARY-ONE"),
        "the request starts with the first summary: {first}"
    );
    let asked = asked.to_string();
    assert!(asked.contains("NEW-PROMPT") && asked.contains("try again"));
    assert!(!asked.contains("EARLY-TURN") && !asked.contains("MIDDLE-TURN"));
    drop(r);
    // No replay: the failed prompt is in the file once, never re-submitted.
    assert_eq!(messages_with(&rig.path, "NEW-PROMPT"), 1);
}

// ---------------------------------------------------------------------------------------------
// T-I3 (spec I3, I2; pinned behaviour, S4 = G3)

/// rpi 0.8.0's no-progress loop (spec I3, `specs/harness-auto-compaction-policy.md` §5). Every
/// turn of a tool loop returns one tool result larger than keep_recent_tokens. `find_cut_point`
/// then finds no valid cut at or after the keep index and falls back to the first cut point. So
/// each between-turn compaction summarizes nothing: the summary request has no conversation in it,
/// `retained_tail` is the whole context, and a summary message is only *added*. The estimate stays
/// over the trigger, so the next turn compacts again. Nothing bounds this except the turn budget.
///
/// The budget is rpi's per-harness `max_turns` (a child harness from `create_child`), not
/// `RPI_MAX_TURNS_PER_RUN`. That env var is process-wide and would race with parallel tests. The
/// test asserts that the loop compacts once per turn and stops at the budget.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_tool_result_over_keep_recent_compacts_every_turn_until_the_turn_budget() {
    const BUDGET: u32 = 4;
    // ~15 000 tokens: over keep_recent (2 000) and over the trigger (13 616) on its own.
    const RESULT: usize = 60_000;
    let router = Arc::new(Mutex::new(Router::default()));
    router.lock().unwrap().otherwise = Some(FauxStep::tool_call("fetch", json!({})));
    let parent = Rig::new("i3", &router, RESULT).await;

    let opened = session_store::create(
        &parent.dir.join("child-sessions"),
        &parent.dir.join("project"),
        "amp-manager",
    )
    .await
    .expect("a child session file");
    let child = parent
        .harness
        .create_child(
            opened.session,
            Arc::new(Recorder(Arc::clone(&parent.timeline))),
            BUDGET,
            None,
        )
        .await
        .expect("a child harness with a turn budget");
    let _watch = watch(&child.harness, &parent.timeline);

    let result = tokio::time::timeout(WAIT, child.harness.prompt_text("I3-PROMPT", Vec::new()))
        .await
        .expect("the turn budget ends the run")
        .expect("the run");
    // A budget stop ends the run as rpi's normal exit, with a notice in the transcript.
    assert!(
        !matches!(result.outcome, HarnessRunOutcome::Failed { .. }),
        "{:?}",
        result.outcome
    );
    assert_eq!(
        messages_with(&opened.path, "runBudget"),
        1,
        "the budget notice"
    );

    {
        let r = router.lock().unwrap();
        let turns = r.of("turn").len();
        assert_eq!(turns, BUDGET as usize, "{:?}", r.order());
        // One summary call per turn: the loop compacts after every turn's tool result.
        assert_eq!(
            r.order(),
            ["turn", "summary"].repeat(BUDGET as usize),
            "a compaction after every turn"
        );
        // Each summary request summarizes nothing of the conversation.
        for (n, s) in r.of("summary").iter().enumerate() {
            let s = s.to_string();
            assert!(
                !s.contains("I3-PROMPT") && !s.contains("FETCHED"),
                "summary call {n} had conversation to summarize: {}",
                &s[..s.len().min(500)]
            );
        }
        // Each turn after the first is sent everything again, plus one more summary message.
        for (n, t) in r.of("turn").iter().enumerate() {
            let t = t.to_string();
            assert!(t.contains("I3-PROMPT"), "turn {n} still has the prompt");
            assert_eq!(
                t.matches("FETCHED").count(),
                n,
                "turn {n}: every result is kept"
            );
            assert_eq!(
                t.matches("<summary>").count(),
                n,
                "turn {n}: summaries pile up"
            );
        }
    }

    let found = compactions(&child.harness).await;
    assert_eq!(found.len(), BUDGET as usize, "bounded by the turn budget");
    let mut before = 0;
    for (n, (entry, reason)) in found.iter().enumerate() {
        assert_eq!(*reason, Some(CompactionReason::Threshold));
        let tail = serde_json::to_value(&entry.retained_tail).unwrap();
        let tail_text = tail.to_string();
        // retained_tail == the whole context: the prompt is still its first real message.
        assert!(
            tail_text.contains("I3-PROMPT"),
            "compaction {n} kept the prompt"
        );
        assert_eq!(
            tail_text.matches("FETCHED").count(),
            n + 1,
            "compaction {n} kept every result"
        );
        // Compacting freed nothing: each one starts bigger than the last.
        assert!(
            entry.tokens_before > THRESHOLD,
            "compaction {n}: {}",
            entry.tokens_before
        );
        assert!(
            entry.tokens_before > before,
            "compaction {n} grew: {} <= {before}",
            entry.tokens_before
        );
        before = entry.tokens_before;
    }
}
