//! #2370: the chat on a fan-out emitter. The TUI works as before off its channel, a second
//! consumer gets every rpi event, and the manager feed holds the chat as numbered ChatEvents:
//! prompts with their origin, the reply, the run's outcome and the notice lines.

use std::{sync::Arc, time::Duration};

use amp_core::{
    agent::memory_session,
    chat_event::{ChatEvent, NoticeLevel, Origin, PromptOrigin, RunOutcome},
    events::{ChannelEmitter, FanOutEmitter},
    manager_feed::ManagerFeed,
};
use amp_tui::{Shell, ShellInfo, Submit};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};

const WAIT: Duration = Duration::from_secs(20);

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_chat_reaches_a_second_consumer_and_the_feed() {
    let script = FauxScript::new();
    script.set_responses(vec![
        FauxStep::text("Hello from the manager."),
        FauxStep::text("Second reply."),
    ]);
    let provider = FauxProvider::new(script);
    let model = provider.default_model().clone();
    let (tui, mut events) = ChannelEmitter::new();
    let fanout = FanOutEmitter::new();
    fanout.add_sink(tui.into_emitter());
    let feed = ManagerFeed::new();
    fanout.add_sink(feed.emitter());
    let mut second = fanout.subscribe();
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(fanout.clone().into_emitter()),
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
        git_branch: None,
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, mut done) = Shell::new(Arc::new(harness), info, vec![]);
    shell.set_feed(Some(Arc::clone(&feed)));

    assert_eq!(shell.submit("hi there").await, Submit::Continue);
    shell
        .drive_until_idle(&mut events, &mut done, WAIT)
        .await
        .expect("idle");
    // The TUI is unchanged: the reply is on screen.
    let screen = shell.view().render_plain(100, 30).join("\n");
    assert!(screen.contains("Hello from the manager."), "{screen}");

    // A remote prompt goes in as a user turn, never as a command.
    let remote = PromptOrigin::Remote {
        device_id: "dev1".into(),
        device_name: "Safari on iPhone".into(),
    };
    assert!(shell.send_prompt("/quit", remote));
    shell
        .drive_until_idle(&mut events, &mut done, WAIT)
        .await
        .expect("idle");
    assert!(!shell.send_prompt("   ", PromptOrigin::Local), "empty");

    // A notice line reaches the feed too.
    assert_eq!(shell.submit("/nonsense").await, Submit::Continue);
    feed.flush();

    // The second consumer got every rpi event of both runs.
    let mut got = Vec::new();
    while let Ok(e) = second.try_recv() {
        got.push(e);
    }
    let ends = got
        .iter()
        .filter(|e| matches!(e, AgentEvent::AgentEnd { .. }))
        .count();
    assert_eq!(ends, 2, "{got:?}");

    let all = feed.since(0).expect("all held");
    let seqs: Vec<u64> = all.iter().map(|e| e.seq).collect();
    assert_eq!(seqs, (1..=all.len() as u64).collect::<Vec<_>>());
    let events: Vec<ChatEvent> = all.into_iter().map(|e| e.event).collect();
    let tags: Vec<&str> = events.iter().map(ChatEvent::type_tag).collect();
    assert_eq!(
        tags,
        [
            "user_message",
            "run_start",
            "message_start",
            "text_delta",
            "message_end",
            "run_end",
            "user_message",
            "run_start",
            "message_start",
            "text_delta",
            "message_end",
            "run_end",
            "notice",
        ],
        "{events:?}"
    );
    assert_eq!(
        events[0],
        ChatEvent::user_message("p_1", "hi there", &PromptOrigin::Local)
    );
    assert_eq!(
        events[1],
        ChatEvent::RunStart {
            run_id: "run_1".into(),
            prompt_id: "p_1".into()
        }
    );
    assert_eq!(
        events[3],
        ChatEvent::TextDelta {
            message_id: "msg_1".into(),
            delta: "Hello from the manager.".into()
        }
    );
    assert_eq!(
        events[5],
        ChatEvent::RunEnd {
            run_id: "run_1".into(),
            outcome: RunOutcome::Completed,
            error: None
        }
    );
    match &events[6] {
        ChatEvent::UserMessage {
            text,
            origin,
            device_id,
            device_name,
            ..
        } => {
            assert_eq!(text, "/quit");
            assert_eq!(*origin, Origin::Remote);
            assert_eq!(device_id.as_deref(), Some("dev1"));
            assert_eq!(device_name.as_deref(), Some("Safari on iPhone"));
        }
        other => panic!("{other:?}"),
    }
    match &events[12] {
        ChatEvent::Notice { level, text, .. } => {
            assert_eq!(*level, NoticeLevel::Info);
            assert!(text.contains("unknown command: /nonsense"), "{text}");
        }
        other => panic!("{other:?}"),
    }
}
