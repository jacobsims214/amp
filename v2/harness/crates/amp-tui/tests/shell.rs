//! M2-7 (#2097): the chat shell on a real rpi `AgentHarness` with the faux provider, rendered
//! without a terminal. Covers the transcript (user, assistant text, a tool call and its error),
//! the conversation carrying on across prompts, the footer (model • thinking, usage, cwd, role),
//! the slash commands and the empty M3 slots.

use std::{sync::Arc, time::Duration};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{ModelChoice, Shell, ShellInfo, Submit};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    Model, ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
    types::Context,
};
use rpi_harness::{
    agent_harness::AgentHarness,
    types::{AgentHarnessOptions, CompactionSettings},
};
use serde_json::json;
use tokio::sync::mpsc::UnboundedReceiver;

const W: usize = 100;
const H: usize = 30;
/// The blank row above and below the screen (#2344).
const V: usize = amp_tui::VERTICAL;
const WAIT: Duration = Duration::from_secs(20);

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
    model: Model,
}

async fn rig(steps: Vec<FauxStep>) -> Rig {
    rig_with(steps, None, CompactionSettings::default()).await
}

/// `window`: the model's context window (`None`: the faux default, shown as 200 000), so a
/// small one makes rpi compact on its own (AMP #4263).
async fn rig_with(
    steps: Vec<FauxStep>,
    window: Option<u64>,
    compaction: CompactionSettings,
) -> Rig {
    rig_system(steps, window, compaction, None).await
}

/// [`rig_with`] with a system prompt, which the faux provider's reported usage includes, as
/// the real manager's system prompt and tools are (AMP #4280).
async fn rig_system(
    steps: Vec<FauxStep>,
    window: Option<u64>,
    compaction: CompactionSettings,
    system_prompt: Option<String>,
) -> Rig {
    let script = FauxScript::new();
    script.set_responses(steps);
    let provider = FauxProvider::new(script);
    let mut model = provider.default_model().clone();
    if let Some(w) = window {
        model.context_window = w;
    }
    let (emitter, events) = ChannelEmitter::new();
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(emitter.into_emitter()),
        compaction,
        system_prompt,
        ..Default::default()
    })
    .await
    .expect("harness");
    let info = ShellInfo {
        role: "amp-manager".into(),
        provider: model.provider.to_string(),
        model_id: model.id.clone(),
        thinking: ThinkingLevel::Off,
        requested_thinking: None,
        context_window: window.unwrap_or(200_000),
        cwd: "/work/amp".into(),
        git_branch: Some("harness-m2".into()),
        session_file: None,
        banner: vec![
            "AMP harness · manager".into(),
            "project #2 AMP · http://amp.test/mcp".into(),
        ],
    };
    let mut alt = model.clone();
    alt.id = "faux-2".into();
    let models = vec![
        ModelChoice {
            reference: format!("{}/{}", model.provider, model.id),
            model: model.clone(),
        },
        ModelChoice {
            reference: format!("{}/faux-2", model.provider),
            model: alt,
        },
    ];
    let (shell, done) = Shell::new(Arc::new(harness), info, models);
    Rig {
        shell,
        events,
        done,
        model,
    }
}

impl Rig {
    async fn say(&mut self, text: &str) {
        assert_eq!(self.shell.submit(text).await, Submit::Continue);
        self.shell
            .drive_until_idle(&mut self.events, &mut self.done, WAIT)
            .await
            .expect("idle");
        // `/compact` (AMP #4222) reports back through the shell's command queue.
        self.shell.settle(WAIT).await.expect("settled");
    }

    /// Waits (bounded) for the shell's look-up of an automatic compaction's entry, which is not
    /// counted in `settle` (AMP #4226): it takes up to ~0.5 s when no entry lands.
    async fn settle_landed(&mut self) {
        for _ in 0..30 {
            tokio::time::sleep(Duration::from_millis(50)).await;
            self.shell.poll();
        }
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(W, H).join("\n")
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_prompt_renders_the_reply_and_the_footer() {
    let mut rig = rig(vec![FauxStep::text("Hello, I am the manager.")]).await;
    let first = rig.screen();
    assert!(first.contains("AMP harness · manager"), "{first}");
    assert!(first.contains("project #2 AMP"), "{first}");

    rig.say("hi there").await;
    let screen = rig.screen();
    assert!(screen.contains("hi there"), "{screen}");
    assert!(screen.contains("Hello, I am the manager."), "{screen}");

    let lines = rig.shell.view().render_plain(W, H);
    assert_eq!(lines.len(), H, "the frame fills the screen");
    // The one-row footer (compact, #2346) is the last line (#2882: no blank row below it since
    // the boxes): cwd (branch) • role, then stats … model.
    let footer = &lines[H - 1];
    assert!(
        footer.contains("/work/amp (harness-m2) • manager"),
        "{footer}"
    );
    assert!(
        footer.ends_with(&format!("{} • thinking off", rig.model.id)),
        "{footer}"
    );
    assert!(!rig.shell.is_running());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_conversation_carries_on_between_prompts() {
    // The second reply echoes how many messages the model received: user, assistant, user.
    let mut rig = rig(vec![
        FauxStep::text("first answer"),
        FauxStep::factory(|ctx: &Context| {
            rpi_ai::providers::faux::faux_assistant_message(
                format!("I can see {} messages", ctx.messages.len()),
                rpi_ai::types::StopReason::Stop,
            )
        }),
    ])
    .await;
    rig.say("one").await;
    rig.say("two").await;
    let screen = rig.screen();
    assert!(screen.contains("first answer"), "{screen}");
    assert!(screen.contains("I can see 3 messages"), "{screen}");
    assert_eq!(rig.shell.totals().turns, 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn tool_calls_render_with_their_result() {
    let mut rig = rig(vec![
        FauxStep::tool_call("amp_whoami", json!({})),
        FauxStep::text("done checking"),
    ])
    .await;
    rig.say("who am I?").await;
    let screen = rig.screen();
    // No such tool in this harness: the call renders, with the error the model saw.
    assert!(screen.contains("amp_whoami"), "{screen}");
    assert!(screen.contains("done checking"), "{screen}");
    assert_eq!(rig.shell.totals().tool_calls, 1);
    assert_eq!(rig.shell.totals().tool_errors, 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_failed_run_shows_the_error_and_the_shell_stays_usable() {
    let mut rig = rig(vec![]).await;
    rig.say("anyone there?").await;
    let screen = rig.screen();
    assert!(screen.contains("No more faux responses"), "{screen}");
    assert!(!rig.shell.is_running());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn slash_commands() {
    let mut rig = rig(vec![FauxStep::text("ok")]).await;
    rig.say("hello").await;

    rig.say("/usage").await;
    let screen = rig.screen();
    // #2188: proper plurals; no failed count when nothing failed.
    assert!(
        screen.contains("usage this session: 1 prompt, 1 turn, 0 tool calls"),
        "{screen}"
    );
    assert!(!screen.contains("failed"), "{screen}");
    assert!(!screen.contains("(s)"), "{screen}");

    // #2188: /model opens a picker with the current model selected; ↓ Enter switches.
    rig.say("/model").await;
    let screen = rig.screen();
    assert!(screen.contains("┌─ model "), "{screen}");
    let current = format!("› ● {}/{}", rig.model.provider, rig.model.id);
    assert!(screen.contains(&current), "{screen}");
    assert!(
        screen.contains(&format!("○ {}/faux-2", rig.model.provider)),
        "{screen}"
    );
    assert!(screen.contains("current"), "{screen}");
    assert!(screen.contains("Enter switch"), "{screen}");
    assert!(!screen.contains("available:"), "no comma list: {screen}");
    let key = |code| KeyEvent::new(code, KeyModifiers::NONE);
    assert!(rig.shell.key(&key(KeyCode::Down), true).await);
    assert!(
        rig.screen()
            .contains(&format!("› ○ {}/faux-2", rig.model.provider))
    );
    assert!(rig.shell.key(&key(KeyCode::Enter), true).await);
    assert!(!rig.shell.view().panel().is_open());
    assert_eq!(rig.shell.info().model_id, "faux-2");
    let lines = rig.shell.view().render_plain(W, H);
    assert!(
        lines[H - 1].ends_with("faux-2 • thinking off"),
        "{}",
        lines[H - 1]
    );
    assert!(rig.screen().contains("model: faux/faux-2"));

    // Typing a reference still switches directly.
    rig.say(&format!("/model {}/{}", rig.model.provider, rig.model.id))
        .await;
    assert_eq!(rig.shell.info().model_id, rig.model.id);

    rig.say("/model nope/nope").await;
    assert!(rig.screen().contains("unknown model `nope/nope`"));

    rig.say("/bogus").await;
    assert!(rig.screen().contains("unknown command: /bogus"));

    rig.say("/help").await;
    // Tall enough for the whole list.
    let help = rig.shell.view().render_plain(W, 90).join("\n");
    assert!(help.contains("/model [p/m|roles|role]"), "{help}");

    assert_eq!(rig.shell.submit("/quit").await, Submit::Quit);
    assert_eq!(rig.shell.submit("/exit").await, Submit::Quit);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_chrome_slots_render_nothing_without_metadata() {
    let rig = rig(vec![]).await;
    let view = rig.shell.view();
    // M3-3: each slot holds its chrome component, which draws nothing until there is metadata.
    assert_eq!(view.header_slot().child_count(), 1);
    assert_eq!(view.amp_status_slot().child_count(), 1);
    assert!(rig.shell.meta().is_none() && view.chrome().data().is_none());
    // Without a header the banner is the first content (after its one-line top padding).
    let lines = view.render_plain(W, H);
    assert!(
        lines[..3 + V].iter().any(|l| l.contains("AMP harness")),
        "{lines:#?}"
    );
    // A component in the header slot shows above the transcript, inside the Conversation box
    // (#2882), whose top edge is under the top padding row (#2344).
    view.header_slot()
        .add_child(Arc::new(rpi_tui::Text::new("HEADER", 0, 0)));
    let lines = view.render_plain(W, H);
    assert_eq!(lines[0], "");
    assert!(
        lines[V].trim_start().starts_with("┌─ Conversation"),
        "{lines:#?}"
    );
    // In the box's text column: the padding's columns, then the edge's padding (#2251).
    assert_eq!(
        lines[V + 1],
        format!(
            "{}│ HEADER{}│",
            " ".repeat(amp_tui::DEFAULT_PADDING - 1),
            " ".repeat(W - 2 * (amp_tui::DEFAULT_PADDING - 1) - 9)
        )
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn narrow_terminals_still_render_a_full_frame() {
    let mut rig = rig(vec![FauxStep::text(
        "a long reply that has to wrap on a narrow terminal ".repeat(4),
    )])
    .await;
    rig.say("wrap please").await;
    for (w, h) in [(40, 12), (80, 24), (20, 8)] {
        let lines = rig.shell.view().render_plain(w, h);
        assert_eq!(lines.len(), h, "{w}x{h}");
        assert!(
            lines.iter().all(|l| rpi_tui::visible_width(l) <= w),
            "{w}x{h}: {lines:#?}"
        );
    }
}

/// The rendered frame as one line of words: box edges and wrapping gone.
fn flat(rig: &Rig) -> String {
    rig.shell
        .view()
        .render_plain(W, 80)
        .iter()
        .map(|l| l.trim().trim_matches('│').trim())
        .collect::<Vec<_>>()
        .join(" ")
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
}

/// AMP #4226 (spec E1, review #3881): `/usage` says when rpi compacts on its own (window −
/// reserve), what it keeps, how long the summary may be, and how many compactions this session
/// had, by reason; the trigger estimate is its own line, apart from the measured context.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn usage_shows_compaction_threshold_and_count() {
    let mut rig = rig(vec![
        FauxStep::text("first answer"),
        FauxStep::text("SUMMARY-OF-EARLIER-TURNS"),
    ])
    .await;
    // Before any reply: no compaction yet, and the estimate leaves out the system prompt.
    rig.say("/usage").await;
    let screen = flat(&rig);
    // Defaults: reserve 16 384 of a 200 000 window; keep 20 000; summary 0.8 × reserve.
    let window = amp_tui::footer::format_tokens(200_000);
    let trigger = amp_tui::footer::format_tokens(200_000 - 16_384);
    assert!(
        screen.contains(&format!(
            "auto-compaction: on · triggers at ~{trigger} of {window}"
        )),
        "{screen}"
    );
    assert!(
        screen.contains(&format!(
            "keeps ~{} recent",
            amp_tui::footer::format_tokens(20_000)
        )),
        "{screen}"
    );
    assert!(
        screen.contains(&format!(
            "summary ≤ {}",
            amp_tui::footer::format_tokens(13_107)
        )),
        "{screen}"
    );
    assert!(screen.contains("0 this session"), "{screen}");
    assert!(
        screen.contains("excludes system prompt and tools"),
        "{screen}"
    );

    rig.say("hello").await;
    rig.say("/compact").await;
    assert!(rig.screen().contains("compacted: ~"), "{}", rig.screen());
    rig.say("/usage").await;
    let screen = flat(&rig);
    assert!(
        screen.contains("1 this session (1 manual, 0 automatic)"),
        "{screen}"
    );
    assert!(screen.contains("trigger estimate: ~"), "{screen}");
}

/// AMP #4226 (spec E2/E3): right after a completed `/compact` the footer's context is the
/// summary + kept tail estimate, below the figure before it and not the stale usage the kept
/// tail still carries; the remote status's figure follows.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn footer_context_drops_after_compaction() {
    // The faux provider reports chars/4 of the request as its usage, so long prompts make a
    // large context. rpi keeps at least `keep_recent_tokens` (20k): the ~25k second turn and
    // the short third are the kept tail, and the third reply still carries the large (stale)
    // usage; the ~75k first turn becomes the summary.
    let mut rig = rig(vec![
        FauxStep::text("first answer"),
        FauxStep::text("second answer"),
        FauxStep::text("third answer"),
        FauxStep::text("SUMMARY-OF-EARLIER-TURNS"),
    ])
    .await;
    rig.say(&"lots of earlier context ".repeat(12_500)).await;
    rig.say(&"some recent context ".repeat(5_000)).await;
    rig.say("short follow-up").await;
    let stale = rig.shell.view().footer().context().expect("ctx").tokens;
    assert!(stale > 90_000, "{stale}");
    let before_label = amp_tui::footer::context_label(stale as f64 / 2_000.0);
    assert!(flat(&rig).contains(&before_label), "{}", flat(&rig));

    rig.say("/compact").await;
    assert!(flat(&rig).contains("compacted: ~"), "{}", flat(&rig));
    let after = rig.shell.view().footer().context().expect("ctx").tokens;
    assert!(
        after < stale,
        "after {after} before {stale}: {}",
        flat(&rig)
    );
    assert_ne!(after, stale);
    assert!(after < stale / 2, "the summary and the kept tail: {after}");
    assert_eq!(rig.shell.totals().context_tokens, after);
    assert!(!flat(&rig).contains(&before_label), "{}", flat(&rig));

    // A declined compaction (the leaf is already one) leaves the footer as it was.
    rig.say("/compact").await;
    assert!(flat(&rig).contains("nothing to compact"), "{}", flat(&rig));
    assert_eq!(rig.shell.view().footer().context().unwrap().tokens, after);
}

/// AMP #4280: `/compact` on a short chat (the whole conversation is the kept tail) shows no
/// large drop. Before, "before" was the reported usage (system prompt + tools + messages) and
/// "after" chars/4 of the summary and tail only, so it printed `~11k → ~21` and the footer fell
/// below the system prompt's own size. Now both include the request overhead.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn compact_on_a_short_chat_shows_no_large_drop() {
    let mut rig = rig_system(
        vec![
            FauxStep::text("Got it: 42."),
            FauxStep::text("SUMMARY: the user asked to remember 42."),
        ],
        None,
        CompactionSettings::default(),
        // ~45k characters: about the default manager's ~11k tokens of system prompt and tools.
        Some("You are the AMP manager; follow the protocol. ".repeat(1_000)),
    )
    .await;
    rig.say("hello there, please remember the number 42").await;
    let before = rig.shell.view().footer().context().expect("ctx").tokens;
    assert!(
        before > 10_000,
        "the usage includes the system prompt: {before}"
    );

    rig.say("/compact").await;
    let screen = flat(&rig);
    let after = rig.shell.view().footer().context().expect("ctx").tokens;
    // Nothing was summarized away (the tail is the whole chat) and a summary was added.
    assert!(after >= before, "after {after} before {before}: {screen}");
    assert!(
        screen.contains(&format!(
            "compacted: ~{} → ~{} tokens (estimate)",
            amp_tui::footer::format_tokens(before),
            amp_tui::footer::format_tokens(after)
        )),
        "{screen}"
    );
    assert_eq!(rig.shell.totals().context_tokens, after);
}

/// AMP #4263: a faux model that answers rpi's summarization requests from one queue and the
/// chat's own turns from another, so a test controls each whatever order rpi calls them in
/// (a split turn makes two summary calls).
#[derive(Default)]
struct Router {
    /// `Err(message)`: the summary call fails. Empty: a short summary.
    summaries: std::collections::VecDeque<Result<String, String>>,
    turns: std::collections::VecDeque<FauxStep>,
    summary_calls: usize,
    turn_calls: usize,
}

fn routed(router: &Arc<std::sync::Mutex<Router>>) -> Vec<FauxStep> {
    use rpi_ai::{providers::faux::faux_assistant_message, types::StopReason};
    (0..40)
        .map(|_| {
            let router = Arc::clone(router);
            FauxStep::factory(move |ctx: &Context| {
                let mut r = router.lock().unwrap();
                let summary = ctx
                    .system_prompt
                    .as_deref()
                    .is_some_and(|s| s.starts_with("You are a context summarization"));
                let failed = |msg: &str| {
                    let mut m = faux_assistant_message("", StopReason::Error);
                    m.error_message = Some(msg.into());
                    m
                };
                if summary {
                    r.summary_calls += 1;
                    return match r.summaries.pop_front() {
                        Some(Err(e)) => failed(&e),
                        Some(Ok(text)) => faux_assistant_message(text, StopReason::Stop),
                        None => faux_assistant_message("SUMMARY", StopReason::Stop),
                    };
                }
                r.turn_calls += 1;
                match r.turns.pop_front() {
                    Some(FauxStep::Message(m)) => m,
                    Some(FauxStep::Factory(f)) => f(ctx),
                    None => failed("no more scripted turns"),
                }
            })
        })
        .collect()
}

/// A tool call whose arguments make ~15 000 tokens of output (faux usage is chars/4).
fn big_tool_call() -> FauxStep {
    FauxStep::tool_call("amp_whoami", json!({ "pad": "x".repeat(60_000) }))
}

/// A 30 000 window: rpi compacts above 30 000 − 16 384 = 13 616 (default reserve).
const SMALL_WINDOW: u64 = 30_000;

/// AMP #4263 G1 (spec F1, I4): a run whose pre-run compaction fails says so in its own words,
/// the prompt is kept but never re-sent, and G2's skipped notice stays quiet for it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_failed_pre_run_compaction_has_its_own_notice_and_no_resend() {
    let router = Arc::new(std::sync::Mutex::new(Router::default()));
    router
        .lock()
        .unwrap()
        .turns
        .push_back(FauxStep::text("first answer"));
    let mut rig = rig_with(
        routed(&router),
        Some(SMALL_WINDOW),
        CompactionSettings::default(),
    )
    .await;
    // ~12 000 tokens: below the trigger before the first call.
    rig.say(&"earlier context ".repeat(3_000)).await;
    assert_eq!(router.lock().unwrap().summary_calls, 0, "{}", rig.screen());
    router
        .lock()
        .unwrap()
        .summaries
        .push_back(Err("summary model exploded".into()));
    // ~4 000 more: the pre-run estimate crosses the trigger, and the summary call fails.
    rig.say(&format!("SECOND-PROMPT {}", "more ".repeat(3_200)))
        .await;
    rig.settle_landed().await;
    let screen = flat(&rig);
    assert!(
        screen.contains("automatic compaction failed (summary model exploded); context unchanged, your prompt is saved but was not answered"),
        "{screen}"
    );
    assert!(
        !screen.contains(amp_tui::shell::COMPACTION_SKIPPED),
        "{screen}"
    );
    assert!(!rig.shell.is_running());
    let r = router.lock().unwrap();
    assert!(r.summary_calls >= 1);
    assert_eq!(
        r.turn_calls, 1,
        "the prompt was never sent to the model, nor re-sent"
    );
}

/// AMP #4263 G2 (spec F2): a between-turn compaction whose summary fails is skipped by rpi; the
/// chat says so and the run carries on.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_skipped_between_turn_compaction_is_shown() {
    let router = Arc::new(std::sync::Mutex::new(Router::default()));
    {
        let mut r = router.lock().unwrap();
        r.turns.push_back(big_tool_call());
        r.turns.push_back(FauxStep::text("carried on"));
        for _ in 0..4 {
            r.summaries.push_back(Err("summary model exploded".into()));
        }
    }
    let keep = CompactionSettings {
        keep_recent_tokens: 2_000,
        ..CompactionSettings::default()
    };
    let mut rig = rig_with(routed(&router), Some(SMALL_WINDOW), keep).await;
    rig.say("go").await;
    rig.settle_landed().await;
    let screen = flat(&rig);
    assert!(router.lock().unwrap().summary_calls >= 1, "{screen}");
    assert!(screen.contains("carried on"), "{screen}");
    assert!(
        screen.contains(amp_tui::shell::COMPACTION_SKIPPED),
        "{screen}"
    );
    assert!(!screen.contains("compaction freed nothing"), "{screen}");
}

/// AMP #4263 G3 (spec I3/S4): when one tool result alone is larger than keep_recent_tokens,
/// automatic compaction frees nothing and fires again next turn: the chat warns, once per run.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_compaction_that_frees_nothing_warns_once_per_run() {
    let router = Arc::new(std::sync::Mutex::new(Router::default()));
    {
        let mut r = router.lock().unwrap();
        r.turns.push_back(big_tool_call());
        r.turns.push_back(big_tool_call());
        r.turns.push_back(FauxStep::text("finally done"));
    }
    let keep = CompactionSettings {
        keep_recent_tokens: 2_000,
        ..CompactionSettings::default()
    };
    let mut rig = rig_with(routed(&router), Some(SMALL_WINDOW), keep).await;
    rig.say("go").await;
    rig.settle_landed().await;
    let screen = flat(&rig);
    assert!(screen.contains("finally done"), "{screen}");
    assert!(router.lock().unwrap().summary_calls >= 2, "{screen}");
    assert_eq!(
        screen
            .matches(amp_tui::shell::COMPACTION_NO_PROGRESS)
            .count(),
        1,
        "{screen}"
    );
    assert!(
        !screen.contains(amp_tui::shell::COMPACTION_SKIPPED),
        "{screen}"
    );
}

// AMP #4259: self-management confirmations as a y/N question in the chat.

mod confirm {
    use super::*;
    use amp_core::approval::{
        ChannelConfirmer, ConfirmRequest, Confirmed, DenyReason, confirm_or_deny,
    };

    fn request(name: &str) -> ConfirmRequest {
        ConfirmRequest::self_management(
            "amp_manage_agent",
            format!("/home/me/.config/amp-harness/agents/{name}.md"),
            "user",
            format!("create agent {name}"),
        )
        .detail("+ description: reviews Go code")
    }

    /// The rig with the session's confirmer wired to its shell, as the chat does.
    async fn wired() -> (Rig, Arc<ChannelConfirmer>) {
        let mut rig = rig(vec![]).await;
        let (confirmer, confirms) = ChannelConfirmer::new();
        rig.shell.set_confirms(confirms);
        (rig, Arc::new(confirmer))
    }

    /// A tool asking with `timeout`, as `confirm_or_deny` is called from a manager tool.
    fn ask(
        c: &Arc<ChannelConfirmer>,
        name: &str,
        timeout: Duration,
    ) -> tokio::task::JoinHandle<Confirmed> {
        let c = Arc::clone(c);
        let req = request(name);
        tokio::spawn(async move { confirm_or_deny(Some(c.as_ref()), req, timeout).await })
    }

    /// Polls (bounded) until a question is on screen.
    async fn until_shown(rig: &mut Rig) {
        for _ in 0..200 {
            rig.shell.poll();
            if rig.shell.confirm_pending() {
                return;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        panic!("no confirmation shown:\n{}", rig.screen());
    }

    async fn outcome(h: tokio::task::JoinHandle<Confirmed>) -> Confirmed {
        tokio::time::timeout(WAIT, h)
            .await
            .expect("the tool got an answer")
            .unwrap()
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_request_shows_a_y_n_question_and_y_approves() {
        let (mut rig, c) = wired().await;
        let h = ask(&c, "amp-foo", WAIT);
        until_shown(&mut rig).await;
        let screen = rig.screen();
        assert!(
            screen.contains("Allow amp_manage_agent to create agent amp-foo? [y/N]"),
            "{screen}"
        );
        assert!(screen.contains("user scope:"), "{screen}");
        assert!(screen.contains("agents/amp-foo.md"), "{screen}");
        assert!(
            screen.contains("+ description: reviews Go code"),
            "{screen}"
        );
        assert!(screen.contains("Press y to allow it"), "{screen}");
        assert!(
            !rig.shell.input_focused(),
            "the editor does not take the key"
        );
        assert!(rig.shell.press("y").await.unwrap());
        assert_eq!(outcome(h).await, Confirmed::Approved);
        assert!(!rig.shell.confirm_pending());
        assert!(rig.screen().contains("Allowed: amp_manage_agent may write"));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn only_y_approves_esc_n_and_enter_refuse() {
        let (mut rig, c) = wired().await;
        for key in ["n", "esc", "enter", "x"] {
            let h = ask(&c, "amp-foo", WAIT);
            until_shown(&mut rig).await;
            assert!(rig.shell.press(key).await.unwrap());
            assert_eq!(
                outcome(h).await,
                Confirmed::Denied(DenyReason::Declined),
                "key {key}"
            );
            assert!(!rig.shell.confirm_pending());
        }
        assert!(
            rig.screen()
                .contains("Refused: amp_manage_agent did not write")
        );
        // A modified y (Ctrl+Y) is not a yes either.
        let h = ask(&c, "amp-foo", WAIT);
        until_shown(&mut rig).await;
        let ctrl_y = KeyEvent::new(KeyCode::Char('y'), KeyModifiers::CONTROL);
        assert!(rig.shell.key(&ctrl_y, true).await);
        assert_eq!(outcome(h).await, Confirmed::Denied(DenyReason::Declined));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_timed_out_question_closes_and_a_late_y_writes_nothing() {
        let (mut rig, c) = wired().await;
        // On screen, then the tool gives up: the next poll closes it.
        let h = ask(&c, "amp-slow", Duration::from_millis(200));
        until_shown(&mut rig).await;
        assert_eq!(outcome(h).await, Confirmed::Denied(DenyReason::TimedOut));
        rig.shell.poll();
        assert!(!rig.shell.confirm_pending());
        let screen = rig.screen();
        assert!(
            screen.contains(
                "The request from amp_manage_agent timed out before an answer; nothing was written."
            ),
            "{screen}"
        );

        // On screen, the tool gives up, and a `y` lands before the next poll: it says so.
        let h = ask(&c, "amp-late", Duration::from_millis(200));
        until_shown(&mut rig).await;
        assert_eq!(outcome(h).await, Confirmed::Denied(DenyReason::TimedOut));
        assert!(rig.shell.press("y").await.unwrap());
        let screen = rig.screen();
        assert!(
            screen.contains("That request already timed out; nothing was written."),
            "{screen}"
        );
        assert!(!screen.contains("Allowed:"), "{screen}");

        // Stale before it was ever shown: skipped.
        let h = ask(&c, "amp-never", Duration::from_millis(50));
        assert_eq!(outcome(h).await, Confirmed::Denied(DenyReason::TimedOut));
        rig.shell.poll();
        assert!(!rig.shell.confirm_pending());
        assert!(!rig.screen().contains("amp-never"), "{}", rig.screen());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn one_question_at_a_time_the_next_waits_its_turn() {
        let (mut rig, c) = wired().await;
        let first = ask(&c, "amp-one", WAIT);
        until_shown(&mut rig).await;
        let second = ask(&c, "amp-two", WAIT);
        for _ in 0..20 {
            rig.shell.poll();
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(rig.screen().contains("create agent amp-one"));
        assert!(
            !rig.screen().contains("create agent amp-two"),
            "{}",
            rig.screen()
        );
        assert!(rig.shell.press("n").await.unwrap());
        assert_eq!(
            outcome(first).await,
            Confirmed::Denied(DenyReason::Declined)
        );
        until_shown(&mut rig).await;
        assert!(
            rig.screen().contains("create agent amp-two"),
            "{}",
            rig.screen()
        );
        assert!(rig.shell.press("y").await.unwrap());
        assert_eq!(outcome(second).await, Confirmed::Approved);
    }

    // AMP #4317: nothing cut from a request can be allowed unseen.

    /// A request for `name` with `rows` as its detail lines.
    fn request_with(name: &str, rows: &[String]) -> ConfirmRequest {
        let mut req = ConfirmRequest::self_management(
            "amp_manage_agent",
            format!("/home/me/.config/amp-harness/agents/{name}.md"),
            "user",
            format!("override agent {name}"),
        );
        for row in rows {
            req = req.detail(row.clone());
        }
        req
    }

    fn ask_with(
        c: &Arc<ChannelConfirmer>,
        req: ConfirmRequest,
    ) -> tokio::task::JoinHandle<Confirmed> {
        let c = Arc::clone(c);
        tokio::spawn(async move { confirm_or_deny(Some(c.as_ref()), req, WAIT).await })
    }

    /// The whole transcript, unstyled, not only what fits the screen.
    fn transcript(rig: &Rig) -> String {
        rig.shell
            .view()
            .transcript_component()
            .render(W)
            .iter()
            .map(|l| rpi_tui::strip_ansi(l).trim_end().to_owned())
            .collect::<Vec<_>>()
            .join("\n")
    }

    /// `y` on a request with unseen rows says to press `v` and leaves it open.
    async fn y_waits_for_v(rig: &mut Rig, h: &tokio::task::JoinHandle<Confirmed>) {
        assert!(rig.shell.press("y").await.unwrap());
        assert!(rig.shell.confirm_pending(), "y did not allow it unseen");
        assert!(rig.shell.confirm_has_unseen());
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(!h.is_finished(), "the tool got no answer yet");
        let t = transcript(rig);
        assert!(t.contains(amp_tui::shell::CONFIRM_VIEW_FIRST), "{t}");
        assert!(!t.contains("Allowed:"), "{t}");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_long_request_needs_v_before_y_allows_it() {
        let (mut rig, c) = wired().await;
        let rows: Vec<String> = (1..=60)
            .map(|i| format!("+ row-{i:02} of the agent"))
            .collect();
        let h = ask_with(&c, request_with("amp-long", &rows));
        until_shown(&mut rig).await;
        let t = transcript(&rig);
        assert!(t.contains("+ row-24 of the agent"), "{t}");
        assert!(!t.contains("+ row-25 of the agent"), "{t}");
        assert!(
            t.contains("… 36 more lines, press v to show them all"),
            "{t}"
        );
        assert!(t.contains("Press v to see the whole change, then y"), "{t}");

        y_waits_for_v(&mut rig, &h).await;

        assert!(rig.shell.press("v").await.unwrap());
        assert!(rig.shell.confirm_pending(), "v does not answer");
        assert!(!rig.shell.confirm_has_unseen());
        let t = transcript(&rig);
        for row in &rows {
            assert!(t.contains(row.as_str()), "{row} missing:\n{t}");
        }
        assert!(
            t.trim_end().ends_with("it is refused."),
            "the y/N line again:\n{t}"
        );
        assert!(rig.shell.press("y").await.unwrap());
        assert_eq!(outcome(h).await, Confirmed::Approved);
        assert!(!rig.shell.confirm_pending());
        assert!(transcript(&rig).contains("Allowed: amp_manage_agent may write"));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_short_request_is_allowed_by_the_first_y() {
        let (mut rig, c) = wired().await;
        let rows: Vec<String> = (1..=10)
            .map(|i| format!("+ row-{i:02} of the agent"))
            .collect();
        let h = ask_with(&c, request_with("amp-short", &rows));
        until_shown(&mut rig).await;
        assert!(!rig.shell.confirm_has_unseen());
        let t = transcript(&rig);
        for row in &rows {
            assert!(t.contains(row.as_str()), "{row} missing:\n{t}");
        }
        assert!(!t.contains("press v"), "{t}");
        assert!(rig.shell.press("y").await.unwrap());
        assert_eq!(outcome(h).await, Confirmed::Approved);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_wide_row_is_cut_visibly_and_shown_in_full_after_v() {
        let (mut rig, c) = wired().await;
        // 500 characters: 100 numbered words of five.
        let wide: String = (0..100).map(|i| format!("w{i:03} ")).collect();
        assert_eq!(wide.chars().count(), 500);
        let rows = vec!["+ short row".to_owned(), wide.trim_end().to_owned()];
        let h = ask_with(&c, request_with("amp-wide", &rows));
        until_shown(&mut rig).await;
        let t = transcript(&rig);
        assert!(t.contains("w059"), "{t}");
        assert!(!t.contains("w060"), "cut at 300 characters:\n{t}");
        assert!(t.contains("more characters)"), "the cut is marked:\n{t}");
        assert!(t.contains("press v to show them in full"), "{t}");

        y_waits_for_v(&mut rig, &h).await;

        assert!(rig.shell.press("v").await.unwrap());
        let t = transcript(&rig);
        for i in 0..100 {
            assert!(t.contains(&format!("w{i:03}")), "w{i:03} missing:\n{t}");
        }
        // Any other key still refuses, unseen or not.
        assert!(rig.shell.press("n").await.unwrap());
        assert_eq!(outcome(h).await, Confirmed::Denied(DenyReason::Declined));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn any_other_key_refuses_a_request_with_unseen_rows() {
        let (mut rig, c) = wired().await;
        let rows: Vec<String> = (1..=30).map(|i| format!("+ row-{i:02}")).collect();
        let h = ask_with(&c, request_with("amp-esc", &rows));
        until_shown(&mut rig).await;
        assert!(rig.shell.press("esc").await.unwrap());
        assert_eq!(outcome(h).await, Confirmed::Denied(DenyReason::Declined));
        assert!(!rig.shell.confirm_pending());
    }
}
