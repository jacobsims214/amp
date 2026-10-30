//! #2384: while the model streams a large tool call (a plan's `amp_create_tasks`), the spinner
//! line says what it is writing and how many tickets it has begun, not only "Working…"; once the
//! call is complete it goes back to "Working…". Real rpi `AgentHarness`, a slow faux provider; no
//! terminal.

use std::{
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{Shell, ShellInfo, Submit};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use serde_json::json;

const W: usize = 100;
const H: usize = 30;
const WAIT: Duration = Duration::from_secs(30);

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_streaming_create_tasks_shows_what_it_writes_and_how_many() {
    let tasks: Vec<_> = (1..=13)
        .map(|i| {
            json!({
                "ref": format!("t{i}"),
                "name": format!("Ticket {i}"),
                "description": "word ".repeat(60),
                "depends_on": if i > 1 { vec![format!("t{}", i - 1)] } else { vec![] },
            })
        })
        .collect();
    let script = FauxScript::new().with_tokens_per_second(800.0);
    script.set_responses(vec![
        FauxStep::tool_call("amp_create_tasks", json!({"project_id": 2, "tasks": tasks})),
        FauxStep::text("Planned."),
    ]);
    let provider = FauxProvider::new(script);
    let model = provider.default_model().clone();
    let (emitter, mut events) = ChannelEmitter::new();
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
        git_branch: None,
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, mut done) = Shell::new(Arc::new(harness), info, Vec::new());

    assert_eq!(shell.submit("plan it").await, Submit::Continue);
    // Every spinner message seen while the call streamed, in order, without repeats.
    let mut seen: Vec<String> = Vec::new();
    let mut on_screen = false;
    let deadline = Instant::now() + WAIT;
    loop {
        assert!(
            Instant::now() < deadline,
            "the call never finished: {seen:?}"
        );
        let Ok(Some(event)) = tokio::time::timeout(Duration::from_millis(200), events.recv()).await
        else {
            continue;
        };
        let started = matches!(event, AgentEvent::ToolExecutionStart { .. });
        shell.handle_event(&event);
        let message = shell.view().working_message().unwrap_or_default();
        if seen.last() != Some(&message) {
            if message.contains("tickets") {
                let screen = shell.view().render_plain(W, H).join("\n");
                on_screen |= screen.contains(&message);
            }
            seen.push(message.clone());
        }
        if started {
            // (#2882: the Message box's title says who is replying instead of "Working…".)
            assert_eq!(
                message,
                amp_tui::view::REPLYING,
                "back to the plain reply once the call runs"
            );
            break;
        }
    }
    let counts: Vec<usize> = seen
        .iter()
        .filter_map(|m| {
            let rest = m.strip_prefix("Writing create_tasks ")?;
            rest.split(' ').next()?.parse().ok()
        })
        .collect();
    assert!(
        counts.len() >= 3,
        "the count should move as the tickets stream: {seen:?}"
    );
    assert!(counts.windows(2).all(|w| w[0] <= w[1]), "{seen:?}");
    // Throttled to a few updates a second, so the last ticket may not get its own.
    assert!(
        counts.last().is_some_and(|n| (10..=13).contains(n)),
        "{seen:?}"
    );
    assert!(
        seen.iter().any(|m| m.ends_with(" KB ·")),
        "the size shows once past 1 KB: {seen:?}"
    );
    assert!(on_screen, "the spinner line shows it");
    shell
        .drive_until_idle(&mut events, &mut done, WAIT)
        .await
        .expect("idle");
    assert!(shell.view().working_message().is_none());
}
