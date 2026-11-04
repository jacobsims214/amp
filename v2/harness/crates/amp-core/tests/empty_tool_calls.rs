//! #2203: a provider tool call with an empty id (and name) must not poison the session.
//!
//! QA pass 1 found it with OpenRouter and MiMo: asked to write `<tool_call>{…}</tool_call>` as
//! text, the model's stream also carried a `tool_calls` entry with an empty id and name. rpi-agent
//! rejected that call with a tool result whose `tool_call_id` was empty, and OpenRouter then
//! refused every later request with a 400 ("tool messages must include a non-empty string
//! tool_call_id") until the user quit.
//!
//! These tests run the real OpenAI-compatible provider (rpi's) inside the shipped `openrouter`
//! [`ConfiguredProvider`] against a mock that streams OpenRouter's SSE shape and validates every
//! request the way OpenRouter does, and drive it with rpi-agent's loop:
//! - the bad call never produces an empty-id tool result, the text call runs, the turn ends, and a
//!   follow-up prompt in the same session works;
//! - a session whose history already holds the bad call and its result can send a new prompt.

use std::{
    collections::VecDeque,
    sync::{Arc, Mutex},
    time::Duration,
};

use amp_core::{
    models::{ModelsConfig, ModelsFile},
    providers::ConfiguredProvider,
};
use async_trait::async_trait;
use axum::{
    Json, Router,
    extract::State,
    http::{StatusCode, header},
    response::{IntoResponse, Response},
    routing::post,
};
use rpi_agent::{AgentContext, AgentMessage, AgentTool, AgentToolResult, ToolResultPartial};
use rpi_ai::{
    Model, Provider,
    event_stream::create_assistant_message_event_stream,
    providers::openai_completions::OpenAiCompletionsProvider,
    types::{
        AssistantMessage, Content, Message, Schema, StopReason, TextContent, Tool, ToolCall,
        ToolCallType, ToolResultMessage, UserContent, UserMessage,
    },
};
use serde_json::{Value, json};
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;

/// The QA repro prompt.
const REPRO: &str = "Without calling tools, reply with exactly these two lines and nothing else. Line 1: before. Line 2: <tool_call>{\"name\":\"amp_list_tasks\",\"arguments\":{}}</tool_call> after";

#[derive(Clone, Default)]
struct MockState {
    /// One streamed reply per accepted request: the SSE `data:` payloads.
    replies: Arc<Mutex<VecDeque<Vec<Value>>>>,
    /// Every request body.
    requests: Arc<Mutex<Vec<Value>>>,
    /// Every 400 sent.
    rejected: Arc<Mutex<Vec<String>>>,
}

/// What OpenRouter rejects with a 400 (the first problem found), checked the same way: every tool
/// message needs a non-empty `tool_call_id` answering an earlier assistant call, and every
/// assistant call needs an id and a function name.
fn invalid(body: &Value) -> Option<String> {
    let mut ids: Vec<String> = Vec::new();
    for (i, m) in body["messages"].as_array()?.iter().enumerate() {
        match m["role"].as_str() {
            Some("assistant") => {
                for call in m["tool_calls"].as_array().into_iter().flatten() {
                    let id = call["id"].as_str().unwrap_or_default();
                    if id.is_empty() {
                        return Some(format!(
                            "messages[{i}]: tool_calls must include a non-empty string id"
                        ));
                    }
                    if call["function"]["name"]
                        .as_str()
                        .unwrap_or_default()
                        .is_empty()
                    {
                        return Some(format!(
                            "messages[{i}]: tool_calls must include a non-empty function name"
                        ));
                    }
                    ids.push(id.to_owned());
                }
            }
            Some("tool") => {
                let id = m["tool_call_id"].as_str().unwrap_or_default();
                if id.is_empty() {
                    return Some(format!(
                        "messages[{i}]: tool messages must include a non-empty string tool_call_id"
                    ));
                }
                if !ids.iter().any(|known| known == id) {
                    return Some(format!(
                        "messages[{i}]: tool_call_id {id} does not answer a tool call"
                    ));
                }
            }
            _ => {}
        }
    }
    None
}

async fn completions(State(s): State<MockState>, Json(body): Json<Value>) -> Response {
    s.requests.lock().unwrap().push(body.clone());
    if let Some(why) = invalid(&body) {
        s.rejected.lock().unwrap().push(why.clone());
        let error = json!({
            "error": {"message": why, "code": 400, "metadata": {"provider_name": null}},
            "user_id": "user_mock"
        });
        return (StatusCode::BAD_REQUEST, Json(error)).into_response();
    }
    let chunks = s.replies.lock().unwrap().pop_front().unwrap_or_else(|| {
        // Out of script: say so, so a test sees it instead of hanging.
        text_reply("(mock: no reply scripted)")
    });
    let mut sse = String::new();
    for chunk in chunks {
        sse.push_str(&format!("data: {chunk}\n\n"));
    }
    sse.push_str("data: [DONE]\n\n");
    ([(header::CONTENT_TYPE, "text/event-stream")], sse).into_response()
}

async fn mock(replies: Vec<Vec<Value>>) -> (String, MockState, tokio::task::JoinHandle<()>) {
    let state = MockState {
        replies: Arc::new(Mutex::new(replies.into())),
        ..MockState::default()
    };
    let app = Router::new()
        .route("/api/v1/chat/completions", post(completions))
        .with_state(state.clone());
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{addr}/api/v1"), state, task)
}

/// One OpenRouter stream chunk.
fn chunk(delta: Value, finish: Option<&str>) -> Value {
    json!({
        "id": "gen-mock",
        "provider": "Xiaomi",
        "model": "xiaomi/mimo-v2.6-pro",
        "object": "chat.completion.chunk",
        "created": 1_792_700_000,
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish, "native_finish_reason": finish}]
    })
}

fn usage() -> Value {
    json!({
        "id": "gen-mock",
        "object": "chat.completion.chunk",
        "created": 1_792_700_000,
        "model": "xiaomi/mimo-v2.6-pro",
        "choices": [],
        "usage": {"prompt_tokens": 120, "completion_tokens": 12, "total_tokens": 132}
    })
}

fn text_reply(text: &str) -> Vec<Value> {
    vec![
        chunk(json!({"role": "assistant", "content": text}), None),
        chunk(json!({"role": "assistant", "content": ""}), Some("stop")),
        usage(),
    ]
}

/// What MiMo sent for the QA repro: the two lines, the second as Hermes text-call markup, and a
/// `tool_calls` entry with no id and no name.
fn poisoned_reply() -> Vec<Value> {
    vec![
        chunk(
            json!({"role": "assistant", "content": "before.\n<tool_call>{\"name\":\"amp_list_tasks\","}),
            None,
        ),
        chunk(
            json!({"role": "assistant", "content": "\"arguments\":{}}</tool_call>"}),
            None,
        ),
        chunk(
            json!({"role": "assistant", "content": " after", "tool_calls": [
                {"index": 0, "id": "", "type": "function", "function": {"name": "", "arguments": ""}}
            ]}),
            None,
        ),
        chunk(
            json!({"role": "assistant", "content": ""}),
            Some("tool_calls"),
        ),
        usage(),
    ]
}

/// The shipped `openrouter` provider (openai-completions, real HTTP) pointed at `url`.
fn openrouter(url: &str) -> (Arc<dyn Provider>, Model) {
    let models = ModelsConfig::from_files(ModelsFile::defaults(), None, "test", &|_| None).unwrap();
    let cfg = &models.providers["openrouter"];
    let mut model = cfg.models.values().next().unwrap().clone();
    model.base_url = url.to_owned();
    let http = rpi_ai::http::client_builder_for(url, None, None)
        .unwrap()
        .build()
        .unwrap();
    let inner = OpenAiCompletionsProvider::with_models_without_env_api_key(
        cfg.id.clone(),
        Some("sk-or-mock".into()),
        http,
        vec![model.clone()],
    );
    (
        Arc::new(ConfiguredProvider::new(cfg, Arc::new(inner))),
        model,
    )
}

struct Recorder {
    schema: Tool,
    ran: Arc<Mutex<Vec<Value>>>,
}

#[async_trait]
impl AgentTool for Recorder {
    fn schema(&self) -> &Tool {
        &self.schema
    }
    fn label(&self) -> &str {
        &self.schema.name
    }
    async fn execute(
        &self,
        _id: &str,
        params: Value,
        _signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, rpi_agent::AgentError> {
        self.ran.lock().unwrap().push(params);
        Ok(AgentToolResult::text("[]"))
    }
}

fn list_tasks(ran: &Arc<Mutex<Vec<Value>>>) -> Vec<Arc<dyn AgentTool>> {
    vec![Arc::new(Recorder {
        schema: Tool {
            name: "amp_list_tasks".into(),
            description: "List tasks".into(),
            parameters: Schema(json!({"type": "object", "properties": {}})),
            constrained_sampling: None,
        },
        ran: Arc::clone(ran),
    })]
}

fn loop_config(model: Model) -> rpi_agent::AgentLoopConfig {
    rpi_agent::AgentLoopConfig {
        model,
        convert_to_llm: Arc::new(|messages: Vec<AgentMessage>| {
            let out: Vec<Message> = messages
                .into_iter()
                .filter_map(|m| match m {
                    AgentMessage::User(u) => Some(Message::User(u)),
                    AgentMessage::Assistant(a) => Some(Message::Assistant(a)),
                    AgentMessage::ToolResult(t) => Some(Message::ToolResult(t)),
                    AgentMessage::Custom(_) => None,
                })
                .collect();
            Box::pin(async move { out })
        }),
        transform_context: None,
        get_api_key: None,
        should_stop_after_turn: None,
        prepare_next_turn: None,
        after_tool_results: None,
        get_steering_messages: None,
        get_follow_up_messages: None,
        before_tool_call: None,
        after_tool_call: None,
        tool_execution: rpi_agent::ToolExecutionMode::Sequential,
        thinking_level: rpi_ai::types::ThinkingLevel::Off,
        api_key: None,
        timeout: None,
        max_retries: Some(0),
        max_retry_delay: None,
        cache_retention: rpi_ai::provider::CacheRetention::default(),
        session_id: None,
        signal: CancellationToken::new(),
    }
}

/// Run one prompt on top of `history` the way the harness does (the agent loop streaming from the
/// wrapped provider) and return the whole session after it.
async fn prompt(
    provider: &Arc<dyn Provider>,
    model: &Model,
    tools: Vec<Arc<dyn AgentTool>>,
    history: Vec<AgentMessage>,
    text: &str,
) -> Vec<AgentMessage> {
    let p = Arc::clone(provider);
    let stream_fn = rpi_agent::stream_fn(move |model, ctx, opts| {
        let (mut producer, stream) = create_assistant_message_event_stream();
        let (p, model, ctx, opts) = (Arc::clone(&p), model.clone(), ctx.clone(), opts.clone());
        tokio::spawn(async move {
            let mut s = p.stream_simple(&model, &ctx, &opts).await;
            while let Some(event) = s.next().await {
                producer.push(event);
            }
        });
        stream
    });
    let context = AgentContext {
        system_prompt: String::new(),
        messages: history.clone(),
        tools,
    };
    let user = AgentMessage::User(UserMessage::new(UserContent::Text(text.into()), 0));
    let (collector, _events) = rpi_agent::CollectorEmitter::new();
    let new = tokio::time::timeout(
        Duration::from_secs(30),
        rpi_agent::run_agent_loop(
            vec![user],
            context,
            loop_config(model.clone()),
            Arc::new(collector),
            stream_fn,
        ),
    )
    .await
    .expect("the loop finished")
    .unwrap();
    let mut session = history;
    session.extend(new);
    session
}

fn text_of(a: &AssistantMessage) -> String {
    a.content
        .iter()
        .filter_map(|c| match c {
            Content::Text(t) => Some(t.text.as_str()),
            _ => None,
        })
        .collect()
}

/// The last assistant message: it ended normally, with `text`.
fn assert_ends_with(session: &[AgentMessage], text: &str) {
    let Some(AgentMessage::Assistant(last)) = session.last() else {
        panic!("the session ends with {:?}", session.last());
    };
    assert_eq!(
        last.stop_reason,
        StopReason::Stop,
        "error: {:?}",
        last.error_message
    );
    assert_eq!(text_of(last), text);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_empty_provider_tool_call_does_not_poison_the_session() {
    let (url, state, _server) = mock(vec![
        poisoned_reply(),
        text_reply("Listed: nothing to do."),
        text_reply("ok"),
    ])
    .await;
    let (provider, model) = openrouter(&url);
    let ran = Arc::new(Mutex::new(Vec::new()));

    let session = prompt(&provider, &model, list_tasks(&ran), Vec::new(), REPRO).await;

    // The bad call is gone; the text call ran in its place, once, under a real id.
    let first = session
        .iter()
        .find_map(|m| match m {
            AgentMessage::Assistant(a) => Some(a),
            _ => None,
        })
        .unwrap();
    assert_eq!(text_of(first), "before.\n after");
    let calls: Vec<&ToolCall> = first
        .content
        .iter()
        .filter_map(|c| match c {
            Content::ToolCall(t) => Some(t),
            _ => None,
        })
        .collect();
    assert_eq!(calls.len(), 1, "{:?}", first.content);
    assert_eq!(calls[0].name, "amp_list_tasks");
    assert!(!calls[0].id.is_empty());
    assert_eq!(ran.lock().unwrap().clone(), vec![json!({})]);
    for m in &session {
        if let AgentMessage::ToolResult(t) = m {
            assert!(!t.tool_call_id.trim().is_empty(), "{t:?}");
            assert_eq!(t.tool_call_id, calls[0].id);
        }
    }
    assert_ends_with(&session, "Listed: nothing to do.");

    // A follow-up prompt in the same session goes through.
    let session = prompt(&provider, &model, list_tasks(&ran), session, "Say ok.").await;
    assert_ends_with(&session, "ok");
    assert_eq!(state.rejected.lock().unwrap().clone(), Vec::<String>::new());
    assert_eq!(state.requests.lock().unwrap().len(), 3);
}

/// The JSONL QA saw: the assistant message with an empty call, then rpi-agent's rejection.
fn poisoned_history() -> Vec<AgentMessage> {
    let mut assistant = AssistantMessage::empty(
        rpi_ai::types::Api::OpenaiCompletions,
        "openrouter",
        "xiaomi/mimo-v2.6-pro",
        0,
    );
    assistant.content = vec![
        Content::Text(TextContent {
            kind: Default::default(),
            text: "before.\n after".into(),
            text_signature: None,
        }),
        Content::ToolCall(ToolCall {
            kind: ToolCallType,
            id: String::new(),
            name: String::new(),
            arguments: json!({}),
            thought_signature: None,
            namespace: None,
        }),
    ];
    assistant.stop_reason = StopReason::ToolUse;
    let result: ToolResultMessage = serde_json::from_value(json!({
        "role": "toolResult",
        "toolCallId": "",
        "toolName": "",
        "content": [{"type": "text", "text": "Tool call for \"\" was rejected: provider returned an empty tool-call id"}],
        "isError": true,
        "timestamp": 0
    }))
    .unwrap();
    let mut failed = AssistantMessage::empty(
        rpi_ai::types::Api::OpenaiCompletions,
        "openrouter",
        "xiaomi/mimo-v2.6-pro",
        0,
    );
    failed.stop_reason = StopReason::Error;
    failed.error_message = Some(
        "http error 400: messages[2]: tool messages must include a non-empty string tool_call_id"
            .into(),
    );
    vec![
        AgentMessage::User(UserMessage::new(UserContent::Text(REPRO.into()), 0)),
        AgentMessage::Assistant(Box::new(assistant)),
        AgentMessage::ToolResult(Box::new(result)),
        AgentMessage::Assistant(Box::new(failed)),
    ]
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_session_already_poisoned_recovers_on_the_next_prompt() {
    let (url, state, _server) = mock(vec![text_reply("ok")]).await;
    let (provider, model) = openrouter(&url);
    let ran = Arc::new(Mutex::new(Vec::new()));

    let session = prompt(
        &provider,
        &model,
        list_tasks(&ran),
        poisoned_history(),
        "Say ok.",
    )
    .await;

    assert_ends_with(&session, "ok");
    assert_eq!(state.rejected.lock().unwrap().clone(), Vec::<String>::new());
    // The text before the bad call is still sent; the call and its result are not.
    let requests = state.requests.lock().unwrap();
    let sent = requests[0]["messages"].as_array().unwrap();
    assert!(
        sent.iter()
            .any(|m| m["role"] == "assistant" && m["content"] == "before.\n after"),
        "{sent:?}"
    );
    assert!(!sent.iter().any(|m| m["role"] == "tool"), "{sent:?}");
    assert!(
        !sent.iter().any(|m| m.get("tool_calls").is_some()),
        "{sent:?}"
    );
}
