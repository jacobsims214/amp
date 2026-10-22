//! #2179: text-format tool calls (`<tool_call><function=…>`, as MiMo writes them) never reach the
//! transcript. Through the real provider wrapper ([`ConfiguredProvider`]), streamed across chunk
//! boundaries:
//! - with a structured call next to it, the text is dropped and the call runs once;
//! - alone, it becomes a real tool call;
//! - that call goes through the role's tool policy (`before_tool_call`) like any other call.

use std::{
    collections::VecDeque,
    sync::{Arc, Mutex},
};

use amp_core::{
    assets::Assets,
    models::{ModelsConfig, ModelsFile},
    providers::ConfiguredProvider,
    roles::RoleRegistry,
};
use async_trait::async_trait;
use rpi_agent::{AgentContext, AgentMessage, AgentTool, AgentToolResult, ToolResultPartial};
use rpi_ai::{
    AssistantMessageEventStream, Model, Provider, SimpleStreamOptions,
    event_stream::create_assistant_message_event_stream,
    types::{
        AssistantMessage, AssistantMessageEvent, Content, Context, DoneReason, Message, Schema,
        StopReason, TextContent, ThinkingContent, Tool, ToolCall, ToolCallType, UserContent,
        UserMessage,
    },
};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

/// The UX review's line (#2175 finding 4).
const MIMO_LS: &str = "<tool_call><function=ls><parameter=path>/Volumes/x/harness-play</parameter></function></tool_call>";

/// One block of a scripted reply. Text and thinking come as the chunks the provider streams.
#[derive(Clone)]
enum Block {
    Thinking(Vec<&'static str>),
    Text(Vec<&'static str>),
    Call(&'static str, &'static str, Value),
}

/// Split `s` into chunks of `n` bytes (ASCII fixtures), so tags break mid-name.
fn chunks(s: &'static str, n: usize) -> Vec<&'static str> {
    (0..s.len())
        .step_by(n)
        .map(|i| &s[i..(i + n).min(s.len())])
        .collect()
}

/// A provider that streams scripted replies, one per request, the way rpi's providers do: each
/// event carries the message so far.
struct Scripted {
    replies: Mutex<VecDeque<Vec<Block>>>,
}

impl Scripted {
    fn new(replies: Vec<Vec<Block>>) -> Self {
        Self {
            replies: Mutex::new(replies.into()),
        }
    }
}

#[async_trait]
impl Provider for Scripted {
    fn id(&self) -> &str {
        "scripted"
    }
    fn models(&self) -> &[Model] {
        &[]
    }
    async fn stream_simple(
        &self,
        model: &Model,
        _ctx: &Context,
        _opts: &SimpleStreamOptions,
    ) -> AssistantMessageEventStream {
        use AssistantMessageEvent as E;
        let (mut p, stream) = create_assistant_message_event_stream();
        let blocks = self.replies.lock().unwrap().pop_front().unwrap_or_default();
        let mut m = AssistantMessage::empty(model.api.clone(), "scripted", model.id.clone(), 0);
        let snap = |m: &AssistantMessage| Arc::new(m.clone());
        p.push(E::Start { partial: snap(&m) });
        let mut structured = false;
        for block in blocks {
            let i = m.content.len();
            match block {
                Block::Text(parts) => {
                    m.content.push(Content::Text(TextContent {
                        kind: Default::default(),
                        text: String::new(),
                        text_signature: None,
                    }));
                    p.push(E::TextStart {
                        content_index: i,
                        partial: snap(&m),
                    });
                    for part in parts {
                        if let Content::Text(t) = &mut m.content[i] {
                            t.text.push_str(part);
                        }
                        p.push(E::TextDelta {
                            content_index: i,
                            delta: part.into(),
                            partial: snap(&m),
                        });
                    }
                    let Content::Text(t) = &m.content[i] else {
                        unreachable!()
                    };
                    p.push(E::TextEnd {
                        content_index: i,
                        content: t.text.clone(),
                        partial: snap(&m),
                    });
                }
                Block::Thinking(parts) => {
                    m.content.push(Content::Thinking(ThinkingContent {
                        kind: Default::default(),
                        thinking: String::new(),
                        thinking_signature: None,
                        redacted: false,
                    }));
                    p.push(E::ThinkingStart {
                        content_index: i,
                        partial: snap(&m),
                    });
                    for part in parts {
                        if let Content::Thinking(t) = &mut m.content[i] {
                            t.thinking.push_str(part);
                        }
                        p.push(E::ThinkingDelta {
                            content_index: i,
                            delta: part.into(),
                            partial: snap(&m),
                        });
                    }
                    let Content::Thinking(t) = &m.content[i] else {
                        unreachable!()
                    };
                    p.push(E::ThinkingEnd {
                        content_index: i,
                        content: t.thinking.clone(),
                        partial: snap(&m),
                    });
                }
                Block::Call(id, name, arguments) => {
                    structured = true;
                    let call = ToolCall {
                        kind: ToolCallType,
                        id: id.into(),
                        name: name.into(),
                        arguments,
                        thought_signature: None,
                        namespace: None,
                    };
                    m.content.push(Content::ToolCall(call.clone()));
                    p.push(E::ToolCallStart {
                        content_index: i,
                        partial: snap(&m),
                    });
                    p.push(E::ToolCallEnd {
                        content_index: i,
                        tool_call: call,
                        partial: snap(&m),
                    });
                }
            }
        }
        let reason = if structured {
            DoneReason::ToolUse
        } else {
            DoneReason::Stop
        };
        m.stop_reason = reason.into();
        p.push(E::Done { reason, message: m });
        stream
    }
}

/// The shipped openrouter provider (openai-completions) around a scripted inner provider.
fn provider(replies: Vec<Vec<Block>>) -> (Arc<dyn Provider>, Model) {
    let models = ModelsConfig::from_files(ModelsFile::defaults(), None, "test", &|_| None).unwrap();
    let cfg = &models.providers["openrouter"];
    let model = cfg.models.values().next().unwrap().clone();
    let wrapped: Arc<dyn Provider> = Arc::new(ConfiguredProvider::new(
        cfg,
        Arc::new(Scripted::new(replies)),
    ));
    (wrapped, model)
}

fn tool(name: &str, props: Value) -> Tool {
    Tool {
        name: name.into(),
        description: name.into(),
        parameters: Schema(json!({"type": "object", "properties": props})),
        constrained_sampling: None,
    }
}

/// Everything the transcript could render from one streamed reply, and the final message.
struct Seen {
    rendered: Vec<String>,
    deltas: String,
    message: AssistantMessage,
}

async fn stream(provider: &Arc<dyn Provider>, model: &Model, tools: Vec<Tool>) -> Seen {
    use AssistantMessageEvent as E;
    let ctx = Context {
        system_prompt: None,
        messages: vec![Message::User(UserMessage::new("go", 0))],
        tools,
    };
    let mut s = provider
        .stream_simple(model, &ctx, &SimpleStreamOptions::default())
        .await;
    let mut rendered = Vec::new();
    let mut deltas = String::new();
    let texts = |m: &AssistantMessage| -> Vec<String> {
        m.content
            .iter()
            .filter_map(|c| match c {
                Content::Text(t) => Some(t.text.clone()),
                Content::Thinking(t) => Some(t.thinking.clone()),
                _ => None,
            })
            .collect()
    };
    let mut message = None;
    while let Some(event) = s.next().await {
        match &event {
            E::TextDelta { delta, partial, .. } => {
                deltas.push_str(delta);
                rendered.extend(texts(partial));
            }
            E::ThinkingDelta { delta, partial, .. } => {
                rendered.push(delta.clone());
                rendered.extend(texts(partial));
            }
            E::TextEnd {
                content, partial, ..
            }
            | E::ThinkingEnd {
                content, partial, ..
            } => {
                rendered.push(content.clone());
                rendered.extend(texts(partial));
            }
            E::Done { message: m, .. } => {
                rendered.extend(texts(m));
                message = Some(m.clone());
            }
            E::Error { error, .. } => panic!("stream error: {:?}", error.error_message),
            _ => {}
        }
    }
    Seen {
        rendered,
        deltas,
        message: message.expect("a Done event"),
    }
}

fn assert_no_markup(seen: &Seen) {
    for text in &seen.rendered {
        for tag in ["<tool", "</tool", "<function", "</function", "<parameter"] {
            assert!(!text.contains(tag), "markup would render: {text:?}");
        }
    }
}

fn calls(m: &AssistantMessage) -> Vec<&ToolCall> {
    m.content
        .iter()
        .filter_map(|c| match c {
            Content::ToolCall(t) => Some(t),
            _ => None,
        })
        .collect()
}

#[tokio::test]
async fn text_call_next_to_a_structured_call_is_dropped_and_the_call_runs_once() {
    for n in [1, 3, 7, 16] {
        let (p, model) = provider(vec![vec![
            Block::Thinking(chunks("I'll list the directory first.", n)),
            Block::Text(chunks(MIMO_LS, n)),
            Block::Call("call_1", "ls", json!({"path": "/Volumes/x/harness-play"})),
        ]]);
        let seen = stream(
            &p,
            &model,
            vec![tool("ls", json!({"path": {"type": "string"}}))],
        )
        .await;
        assert_no_markup(&seen);
        assert_eq!(seen.deltas, "");
        let calls = calls(&seen.message);
        assert_eq!(calls.len(), 1, "chunk {n}");
        assert_eq!(calls[0].id, "call_1");
        assert_eq!(seen.message.stop_reason, StopReason::ToolUse);
        // The text block held only markup, so it is gone from the stored message.
        assert!(
            !seen
                .message
                .content
                .iter()
                .any(|c| matches!(c, Content::Text(_))),
            "{:?}",
            seen.message.content
        );
    }
}

#[tokio::test]
async fn text_call_alone_becomes_a_tool_call() {
    for n in [1, 2, 5, 11] {
        let (p, model) = provider(vec![vec![Block::Text(chunks(
            "Let me look around.\n<tool_call><function=ls><parameter=path>/Volumes/x/harness-play</parameter></function></tool_call>",
            n,
        ))]]);
        let seen = stream(
            &p,
            &model,
            vec![tool("ls", json!({"path": {"type": "string"}}))],
        )
        .await;
        assert_no_markup(&seen);
        assert_eq!(seen.deltas, "Let me look around.\n", "chunk {n}");
        let calls = calls(&seen.message);
        assert_eq!(calls.len(), 1, "chunk {n}");
        assert_eq!(calls[0].name, "ls");
        assert_eq!(
            calls[0].arguments,
            json!({"path": "/Volumes/x/harness-play"})
        );
        assert!(calls[0].id.starts_with("call_text_"));
        assert_eq!(seen.message.stop_reason, StopReason::ToolUse);
    }
}

#[tokio::test]
async fn plain_text_passes_through_untouched() {
    let text = "Use Vec<String> when a < b; <tool> is not a tag.";
    let (p, model) = provider(vec![vec![Block::Text(chunks(text, 3))]]);
    let seen = stream(&p, &model, Vec::new()).await;
    assert_eq!(seen.deltas, text);
    assert!(calls(&seen.message).is_empty());
    assert_eq!(seen.message.stop_reason, StopReason::Stop);
}

/// Records every call that actually executes.
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
        Ok(AgentToolResult::text("ran"))
    }
}

fn recorder(name: &str, props: Value, ran: &Arc<Mutex<Vec<Value>>>) -> Arc<dyn AgentTool> {
    Arc::new(Recorder {
        schema: tool(name, props),
        ran: Arc::clone(ran),
    })
}

fn loop_config(model: Model, hook: rpi_agent::BeforeToolCall) -> rpi_agent::AgentLoopConfig {
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
        before_tool_call: Some(hook),
        after_tool_call: None,
        tool_execution: rpi_agent::ToolExecutionMode::Sequential,
        thinking_level: rpi_ai::types::ThinkingLevel::Off,
        api_key: None,
        timeout: None,
        max_retries: None,
        max_retry_delay: None,
        cache_retention: rpi_ai::provider::CacheRetention::default(),
        session_id: None,
        signal: CancellationToken::new(),
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_parsed_text_call_goes_through_the_tool_policy() {
    // A reviewer (edit: deny) writes two text-format calls and no structured one: a write (denied
    // by its policy) and an allowed bash command. Only the bash call may execute.
    let registry = RoleRegistry::from_assets(&Assets::embedded().unwrap()).unwrap();
    let policy = Arc::new(
        registry
            .get("amp-reviewer")
            .unwrap()
            .tool_policy("/work/repo/.worktrees/42"),
    );
    let ran = Arc::new(Mutex::new(Vec::new()));
    let tools = vec![
        recorder(
            "write",
            json!({"path": {"type": "string"}, "content": {"type": "string"}}),
            &ran,
        ),
        recorder("bash", json!({"command": {"type": "string"}}), &ran),
    ];
    let (p, model) = provider(vec![
        vec![Block::Text(chunks(
            "<tool_call><function=write><parameter=path>src/a.rs</parameter><parameter=content>x</parameter></function></tool_call>\n<tool_call>{\"name\": \"bash\", \"arguments\": {\"command\": \"git diff main\"}}</tool_call>",
            4,
        ))],
        vec![Block::Text(vec!["done"])],
    ]);
    // The production path: the agent loop streams from the wrapped provider.
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
        messages: Vec::new(),
        tools,
    };
    let prompt = AgentMessage::User(UserMessage::new(UserContent::Text("go".into()), 0));
    let (collector, _events) = rpi_agent::CollectorEmitter::new();
    let messages = tokio::time::timeout(
        std::time::Duration::from_secs(30),
        rpi_agent::run_agent_loop(
            vec![prompt],
            context,
            loop_config(model, policy.before_tool_call()),
            Arc::new(collector),
            stream_fn,
        ),
    )
    .await
    .expect("the loop finished")
    .unwrap();

    assert_eq!(
        ran.lock().unwrap().clone(),
        vec![json!({"command": "git diff main"})]
    );
    let result = |name: &str| {
        messages
            .iter()
            .find_map(|m| match m {
                AgentMessage::ToolResult(t) if t.tool_name == name => Some(t),
                _ => None,
            })
            .unwrap_or_else(|| panic!("no {name} result"))
    };
    let body = |t: &rpi_ai::types::ToolResultMessage| -> String {
        t.content
            .iter()
            .filter_map(|c| match c {
                Content::Text(t) => Some(t.text.clone()),
                _ => None,
            })
            .collect()
    };
    let write = result("write");
    assert!(write.is_error);
    assert!(
        body(write).contains("Denied by the amp-reviewer tool policy"),
        "{}",
        body(write)
    );
    let bash = result("bash");
    assert!(!bash.is_error && body(bash) == "ran");
    assert!(matches!(
        messages.last(),
        Some(AgentMessage::Assistant(a)) if a.stop_reason == StopReason::Stop
    ));
}
