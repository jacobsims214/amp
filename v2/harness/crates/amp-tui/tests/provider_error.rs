//! #2207: a provider's HTTP error shows as one sentence, once. QA pass 1 (#2173) saw OpenRouter's
//! whole 400 body (its message five times over and the account's `user_id`) printed as the reply's
//! error and again as "run failed: error: http error 400: {...}".
//!
//! A real rpi `AgentHarness` against a provider that refuses every request with that body; the
//! transcript is compared with `tests/snapshots/provider-error-100.txt` (UPDATE_SNAPSHOTS=1 to
//! regenerate).

use std::{path::PathBuf, sync::Arc, time::Duration};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{Shell, ShellInfo, Submit};
use async_trait::async_trait;
use rpi_ai::{
    AssistantMessage, AssistantMessageEvent, AssistantMessageEventStream, Context, ErrorReason,
    Model, Provider, SimpleStreamOptions, StopReason, ThinkingLevel,
    event_stream::create_assistant_message_event_stream,
    providers::faux::{FauxProvider, FauxScript},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};

const W: usize = 100;
const H: usize = 30;

/// QA pass 1's body, as rpi's OpenAI-compatible provider reports it.
const QA_400: &str = r#"http error 400: {"error":{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400,"metadata":{"provider_name":null,"previous_errors":[{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400},{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400},{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400},{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400}]}},"user_id":"user_3FPq9ZxAbCdEf"}"#;

const SENTENCE: &str = "OpenRouter refused the request (400): messages[9]: tool messages must \
                        include a non-empty string tool_call_id";

/// Refuses every request the way rpi reports an OpenRouter 400.
struct Refusing {
    models: Vec<Model>,
}

#[async_trait]
impl Provider for Refusing {
    fn id(&self) -> &str {
        "openrouter"
    }
    fn models(&self) -> &[Model] {
        &self.models
    }
    async fn stream_simple(
        &self,
        model: &Model,
        _ctx: &Context,
        _opts: &SimpleStreamOptions,
    ) -> AssistantMessageEventStream {
        let (mut producer, consumer) = create_assistant_message_event_stream();
        producer.push(AssistantMessageEvent::Error {
            reason: ErrorReason::Error,
            error: AssistantMessage::terminal(
                model.api.clone(),
                "openrouter",
                model.id.clone(),
                StopReason::Error,
                QA_400,
                0,
            ),
        });
        consumer
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_openrouter_400_is_one_sentence_shown_once() {
    let mut model = FauxProvider::new(FauxScript::new()).default_model().clone();
    model.provider = "openrouter".into();
    model.id = "xiaomi/mimo-v2.6-pro".into();
    let provider: Arc<dyn Provider> = Arc::new(Refusing {
        models: vec![model.clone()],
    });
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
        provider: "openrouter".into(),
        model_id: model.id.clone(),
        thinking: ThinkingLevel::Off,
        requested_thinking: None,
        context_window: 200_000,
        cwd: "/work/amp".into(),
        git_branch: Some("harness-m4".into()),
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, mut done) = Shell::new(Arc::new(harness), info, Vec::new());
    assert_eq!(shell.submit("go").await, Submit::Continue);
    shell
        .drive_until_idle(&mut events, &mut done, Duration::from_secs(20))
        .await
        .expect("idle");

    let screen = shell.view().render_plain(W, H).join("\n");
    let flat = screen.split_whitespace().collect::<Vec<_>>().join(" ");
    assert_eq!(flat.matches(SENTENCE).count(), 1, "shown once\n{screen}");
    for unwanted in [
        "user_id",
        "user_3FP",
        "http error",
        "run failed",
        "previous_errors",
        "{",
    ] {
        assert!(!screen.contains(unwanted), "{unwanted:?}\n{screen}");
    }

    // The transcript rows, from the prompt down (the chrome around them is chrome.rs's).
    let rows: Vec<&str> = screen.lines().map(str::trim_end).collect();
    let start = rows
        .iter()
        .position(|l| l.contains("go"))
        .expect("the prompt");
    let end = rows
        .iter()
        .rposition(|l| l.contains("tool_call_id"))
        .expect("the error");
    let text = rows[start..=end].join("\n") + "\n";
    let path =
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/snapshots/provider-error-100.txt");
    if std::env::var_os("UPDATE_SNAPSHOTS").is_some() {
        std::fs::write(&path, &text).unwrap();
    } else {
        let want = std::fs::read_to_string(&path).unwrap_or_default();
        assert_eq!(
            text, want,
            "snapshot differs (UPDATE_SNAPSHOTS=1 to regenerate)"
        );
    }
}
