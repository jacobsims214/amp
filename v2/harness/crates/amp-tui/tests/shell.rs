//! M2-7 (#2097): the chat shell on a real rpi `AgentHarness` with the faux provider, rendered
//! without a terminal. Covers the transcript (user, assistant text, a tool call and its error),
//! the conversation carrying on across prompts, the footer (model • thinking, usage, cwd, role),
//! the slash commands and the empty M3 slots.

use std::{sync::Arc, time::Duration};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{ModelChoice, Shell, ShellInfo, Submit};
use rpi_agent::AgentEvent;
use rpi_ai::{
    Model, ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
    types::Context,
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use serde_json::json;
use tokio::sync::mpsc::UnboundedReceiver;

const W: usize = 100;
const H: usize = 30;
const WAIT: Duration = Duration::from_secs(20);

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
    model: Model,
}

async fn rig(steps: Vec<FauxStep>) -> Rig {
    let script = FauxScript::new();
    script.set_responses(steps);
    let provider = FauxProvider::new(script);
    let model = provider.default_model().clone();
    let (emitter, events) = ChannelEmitter::new();
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(emitter.into_emitter()),
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
        context_window: 200_000,
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
    // pi's two-line footer is the last two lines: cwd (branch) • role, then stats … model.
    let footer1 = &lines[H - 2];
    let footer2 = &lines[H - 1];
    assert!(
        footer1.contains("/work/amp (harness-m2) • manager"),
        "{footer1}"
    );
    assert!(
        footer2.ends_with(&format!("{} • thinking off", rig.model.id)),
        "{footer2}"
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
    assert!(
        screen.contains("usage this session: 1 prompt(s), 1 turn(s), 0 tool call(s)"),
        "{screen}"
    );

    rig.say("/model").await;
    let screen = rig.screen();
    assert!(screen.contains("available: faux/"), "{screen}");

    rig.say(&format!("/model {}/faux-2", rig.model.provider))
        .await;
    let lines = rig.shell.view().render_plain(W, H);
    assert!(
        lines[H - 1].ends_with("faux-2 • thinking off"),
        "{}",
        lines[H - 1]
    );
    assert_eq!(rig.shell.info().model_id, "faux-2");

    rig.say("/model nope/nope").await;
    assert!(rig.screen().contains("unknown model `nope/nope`"));

    rig.say("/bogus").await;
    assert!(rig.screen().contains("unknown command: /bogus"));

    rig.say("/help").await;
    assert!(rig.screen().contains("/model [provider/model]"));

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
        lines[..3].iter().any(|l| l.contains("AMP harness")),
        "{lines:#?}"
    );
    // A component in the header slot shows above the transcript.
    view.header_slot()
        .add_child(Arc::new(rpi_tui::Text::new("HEADER", 0, 0)));
    let lines = view.render_plain(W, H);
    assert_eq!(lines[0], "HEADER");
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
