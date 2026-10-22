//! #2171: the steps only a person takes, in the AMP web UI. No role approves a plan or changes the
//! person-only project settings (role policy); the manager touches only the tickets its user named
//! or it dispatched; and a refusal that needs a person ends the manager's turn inside a real rpi
//! agent loop instead of letting it retry another way.

use std::{
    collections::VecDeque,
    sync::{Arc, Mutex},
};

use amp_core::{
    approval::{self, GuardDenial, ManagerGuard, RefusalKind, TURN_ENDED},
    assets::Assets,
    policy::{DenialReason, ToolPolicy},
    roles::RoleRegistry,
};
use rpi_agent::{AgentContext, AgentMessage, AgentTool, AgentToolResult, ToolResultPartial};
use rpi_ai::{
    event_stream::create_assistant_message_event_stream,
    types::{
        Api, AssistantMessage, AssistantMessageEvent, Content, DoneReason, ErrorReason, Message,
        StopReason, Tool, ToolCall, ToolCallType, Usage, UserContent, UserMessage,
    },
};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

const ROOT: &str = "/work/repo";

/// What amp-api (feat/human-plan-approval) returns for `amp_approve_plan` over MCP.
const PLAN_REFUSAL: &str = "Plan approval is done by a person in the AMP web UI, not by an agent \
     or over MCP: http://localhost:5173/project/2/plan. Stop and ask your person to do it there. \
     {\"approve_url\":\"http://localhost:5173/project/2/plan\",\"awaiting_approval\":[7]}";
/// The dispatch gate's refusal.
const GATE_REFUSAL: &str = "ticket 7 was planned after the plan was last approved; this project \
     needs the plan approved before agents start new tickets. A person approves it in the AMP web \
     UI (Approve plan): http://localhost:5173/project/2/plan. Agents can't approve: stop and ask \
     your person to approve it there.";

fn registry() -> RoleRegistry {
    RoleRegistry::from_assets(&Assets::embedded().unwrap()).unwrap()
}

fn policy(id: &str) -> ToolPolicy {
    registry().get(id).unwrap().tool_policy(ROOT)
}

// ------------------------------------------------------------------------------------------------
// The role policy: no role approves or changes the person-only settings.
// ------------------------------------------------------------------------------------------------

#[test]
fn no_role_approves_a_plan_or_changes_the_person_only_settings() {
    for role in ["amp-manager", "amp-worker-backend", "amp-reviewer"] {
        let p = policy(role);
        let denial = p
            .check("amp_approve_plan", &json!({"project_id": 2}))
            .unwrap_err();
        assert_eq!(denial.reason, DenialReason::PersonOnly, "{role}");
        let text = denial.to_string();
        assert!(
            text.contains("person's step, in the AMP web UI") && text.contains("approve_url"),
            "{role}: {text}"
        );
        for args in [
            json!({"project_id": 2, "require_plan_approval": false}),
            json!({"project_id": 2, "require_plan_approval": true}),
            json!({"project_id": 2, "open_reassign": true, "name": "x"}),
        ] {
            let d = p
                .check("amp_update_project_settings", &args)
                .expect_err(&format!("{role} {args}"));
            assert_eq!(d.reason, DenialReason::PersonOnly);
        }
        // The project facts the manager prompt records stay allowed.
        for args in [
            json!({"project_id": 2, "metadata": {"jira": "AMP"}}),
            json!({"project_id": 2, "name": "AMP v2", "description": "d"}),
            json!({"project_id": 2, "require_plan_approval": null, "metadata": {}}),
        ] {
            assert!(
                p.check("amp_update_project_settings", &args).is_ok(),
                "{role} {args}"
            );
        }
        // Still in the tool list, so a call shows the denial instead of "no such tool".
        assert!(p.exposes("amp_approve_plan") && p.exposes("amp_update_project_settings"));
    }
}

// ------------------------------------------------------------------------------------------------
// The manager guard.
// ------------------------------------------------------------------------------------------------

fn user(text: &str) -> AgentMessage {
    AgentMessage::User(UserMessage::new(UserContent::Text(text.into()), 0))
}

fn assistant(content: Vec<Content>, stop: StopReason) -> AssistantMessage {
    AssistantMessage {
        role: rpi_ai::types::AssistantRole,
        content,
        api: Api::Other("mock".into()),
        provider: "mock".into(),
        model: "mock".into(),
        response_model: None,
        response_id: None,
        usage: Usage::zero(),
        stop_reason: stop,
        deferred: None,
        error_message: None,
        raw_stop_reason: None,
        end_turn: None,
        timestamp: 0,
    }
}

fn call(id: &str, name: &str, args: Value) -> Content {
    Content::ToolCall(ToolCall {
        kind: ToolCallType,
        id: id.into(),
        name: name.into(),
        arguments: args,
        thought_signature: None,
        namespace: None,
    })
}

fn tool_result(name: &str, text: &str, is_error: bool) -> AgentMessage {
    AgentMessage::ToolResult(Box::new(rpi_ai::types::ToolResultMessage {
        role: rpi_ai::types::ToolResultRole,
        tool_call_id: "x".into(),
        tool_name: name.into(),
        content: vec![Content::text(text)],
        details: None,
        usage: None,
        added_tool_names: Vec::new(),
        is_error,
        timestamp: 0,
    }))
}

fn context(messages: Vec<AgentMessage>) -> AgentContext {
    AgentContext {
        system_prompt: String::new(),
        messages,
        tools: Vec::new(),
    }
}

#[test]
fn the_manager_completes_or_claims_only_tickets_it_was_asked_about() {
    let guard = ManagerGuard;
    let check = |tool: &str, id: Value, messages: Vec<AgentMessage>| {
        guard.check(tool, &json!({"task_id": id}), &context(messages))
    };
    // A status question names no ticket: completing or claiming one is out of scope.
    let status = vec![user("what's on the board?")];
    for tool in approval::TICKET_SCOPED_TOOLS {
        let err = check(tool, json!(41), status.clone()).unwrap_err();
        assert_eq!(
            err,
            GuardDenial::OutOfScope {
                tool: (*tool).into(),
                ticket: Some(41)
            }
        );
        let text = err.to_string();
        assert!(
            text.contains("#41") && text.contains("status questions are read-only"),
            "{text}"
        );
    }
    // Named by the user (any form), or dispatched in this session: allowed.
    assert!(
        check(
            "amp_claim_task",
            json!(41),
            vec![user("I'm on #41, claim it")]
        )
        .is_ok()
    );
    assert!(check("amp_complete_task", json!("#41"), vec![user("41 is done")]).is_ok());
    let ran = AgentMessage::Assistant(Box::new(assistant(
        vec![call(
            "r",
            "amp_run_ticket",
            json!({"task_id": 52, "agent_id": "amp:amp-worker-backend"}),
        )],
        StopReason::ToolUse,
    )));
    assert!(
        check(
            "amp_complete_task",
            json!(52),
            vec![user("run epic 3"), ran]
        )
        .is_ok()
    );
    // A number inside a word isn't a ticket (v41), and a missing task_id is out of scope.
    assert!(check("amp_complete_task", json!(41), vec![user("bump to v41")]).is_err());
    assert!(
        guard
            .check("amp_complete_task", &json!({}), &context(vec![user("41")]))
            .is_err()
    );
    // Other tools are not the guard's business.
    assert!(check("amp_add_task_comment", json!(41), status).is_ok());
}

#[test]
fn after_a_refusal_nothing_else_runs_until_the_user_speaks() {
    let guard = ManagerGuard;
    let refused = vec![
        user("plan epic 9"),
        tool_result("amp_dispatch_task", GATE_REFUSAL, true),
    ];
    let err = guard
        .check("amp_list_tasks", &json!({}), &context(refused.clone()))
        .unwrap_err();
    assert_eq!(err, GuardDenial::AfterRefusal);
    // The same words in a successful result (a KB doc quoting them) don't count.
    let quoted = vec![user("x"), tool_result("amp_kb_get", GATE_REFUSAL, false)];
    assert!(
        guard
            .check("amp_list_tasks", &json!({}), &context(quoted))
            .is_ok()
    );
    // A new user message starts a new turn.
    let mut next = refused;
    next.push(user("approved it, go on"));
    assert!(
        guard
            .check("amp_check_plan", &json!({}), &context(next))
            .is_ok()
    );
}

#[test]
fn refusals_are_parsed_for_the_notice() {
    let r = approval::refusal(&format!("{PLAN_REFUSAL}\n{TURN_ENDED}")).unwrap();
    assert_eq!(r.kind, RefusalKind::PlanApproval);
    assert_eq!(
        r.link.as_deref(),
        Some("http://localhost:5173/project/2/plan")
    );
    assert!(!r.message.contains("[amp-harness]") && !r.message.contains('{'));
}

// ------------------------------------------------------------------------------------------------
// In a real rpi agent loop.
// ------------------------------------------------------------------------------------------------

/// A tool that records its calls and answers with `reply` (an error when `refuse`).
struct Fake {
    schema: Tool,
    ran: Arc<Mutex<Vec<String>>>,
    reply: &'static str,
    refuse: bool,
}

#[async_trait::async_trait]
impl AgentTool for Fake {
    fn schema(&self) -> &Tool {
        &self.schema
    }
    fn label(&self) -> &str {
        &self.schema.name
    }
    async fn execute(
        &self,
        _id: &str,
        _params: Value,
        _signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, rpi_agent::AgentError> {
        self.ran.lock().unwrap().push(self.schema.name.clone());
        if self.refuse {
            Err(rpi_agent::AgentError::Tool(self.reply.into()))
        } else {
            Ok(AgentToolResult::text(self.reply))
        }
    }
}

fn fake(
    name: &str,
    reply: &'static str,
    refuse: bool,
    ran: &Arc<Mutex<Vec<String>>>,
) -> Arc<dyn AgentTool> {
    Arc::new(Fake {
        schema: Tool {
            name: name.into(),
            description: name.into(),
            parameters: json!({"type": "object", "additionalProperties": true}).into(),
            constrained_sampling: None,
        },
        ran: Arc::clone(ran),
        reply,
        refuse,
    })
}

/// A scripted model; counts the LLM calls it answered.
fn scripted(messages: Vec<AssistantMessage>, calls: Arc<Mutex<usize>>) -> rpi_agent::StreamFn {
    let queue = Arc::new(Mutex::new(VecDeque::from(messages)));
    rpi_agent::stream_fn(move |_model, _ctx, _opts| {
        *calls.lock().unwrap() += 1;
        let (mut prod, stream) = create_assistant_message_event_stream();
        let next = queue.lock().unwrap().pop_front();
        tokio::spawn(async move {
            match next {
                Some(message) => {
                    prod.push(AssistantMessageEvent::Start {
                        partial: Arc::new(message.clone()),
                    });
                    let reason = if message.stop_reason == StopReason::ToolUse {
                        DoneReason::ToolUse
                    } else {
                        DoneReason::Stop
                    };
                    prod.push(AssistantMessageEvent::Done { reason, message });
                }
                None => {
                    let error = AssistantMessage::terminal(
                        Api::Other("mock".into()),
                        "mock",
                        "mock",
                        StopReason::Error,
                        "script exhausted",
                        0,
                    );
                    prod.push(AssistantMessageEvent::Error {
                        reason: ErrorReason::Error,
                        error,
                    });
                }
            }
        });
        stream
    })
}

/// The manager's hooks, as `Runtime::build` installs them (the guard, then the policy).
fn manager_config(mode: rpi_agent::ToolExecutionMode) -> rpi_agent::AgentLoopConfig {
    let policy = Arc::new(policy("amp-manager"));
    let guard = Arc::new(ManagerGuard);
    rpi_agent::AgentLoopConfig {
        model: rpi_ai::Model::new(
            "mock",
            "mock",
            Api::Other("mock".into()),
            "mock",
            "https://x.invalid",
        ),
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
        before_tool_call: Some(
            guard.before_tool_call_then(Some(policy.before_tool_call_then(None))),
        ),
        after_tool_call: Some(approval::end_turn_on_refusal()),
        tool_execution: mode,
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

async fn run(
    script: Vec<AssistantMessage>,
    tools: Vec<Arc<dyn AgentTool>>,
    mode: rpi_agent::ToolExecutionMode,
) -> (Vec<AgentMessage>, usize) {
    let calls = Arc::new(Mutex::new(0));
    let context = AgentContext {
        system_prompt: String::new(),
        messages: Vec::new(),
        tools,
    };
    let (collector, _events) = rpi_agent::CollectorEmitter::new();
    let messages = tokio::time::timeout(
        std::time::Duration::from_secs(30),
        rpi_agent::run_agent_loop(
            vec![user("plan and start epic 9")],
            context,
            manager_config(mode),
            Arc::new(collector),
            scripted(script, Arc::clone(&calls)),
        ),
    )
    .await
    .expect("the loop finished")
    .unwrap();
    let n = *calls.lock().unwrap();
    (messages, n)
}

fn results(messages: &[AgentMessage]) -> Vec<(String, bool, String)> {
    messages
        .iter()
        .filter_map(|m| match m {
            AgentMessage::ToolResult(t) => Some((
                t.tool_name.clone(),
                t.is_error,
                t.content
                    .iter()
                    .filter_map(|c| match c {
                        Content::Text(t) => Some(t.text.clone()),
                        _ => None,
                    })
                    .collect::<String>(),
            )),
            _ => None,
        })
        .collect()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_refusal_ends_the_managers_turn_without_another_model_call() {
    let ran = Arc::new(Mutex::new(Vec::new()));
    let tools = vec![
        fake("amp_dispatch_task", GATE_REFUSAL, true, &ran),
        fake("amp_list_tasks", "{}", false, &ran),
    ];
    let script = vec![
        assistant(
            vec![call("c1", "amp_dispatch_task", json!({"task_id": 7}))],
            StopReason::ToolUse,
        ),
        // Never asked for: the refusal ended the run.
        assistant(
            vec![call("c2", "amp_list_tasks", json!({}))],
            StopReason::ToolUse,
        ),
    ];
    let (messages, llm_calls) = run(script, tools, rpi_agent::ToolExecutionMode::Sequential).await;
    assert_eq!(
        llm_calls, 1,
        "the model was not called again after the refusal"
    );
    assert_eq!(ran.lock().unwrap().clone(), vec!["amp_dispatch_task"]);
    let r = results(&messages);
    assert_eq!(r.len(), 1);
    assert!(r[0].1 && r[0].2.contains(approval::PLAN_GATE_MARK) && r[0].2.contains(TURN_ENDED));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_refusal_in_a_mixed_batch_blocks_the_retry_and_ends_the_run() {
    let ran = Arc::new(Mutex::new(Vec::new()));
    let tools = vec![
        fake("amp_dispatch_task", GATE_REFUSAL, true, &ran),
        fake("amp_list_tasks", "{}", false, &ran),
        fake("amp_approve_plan", PLAN_REFUSAL, true, &ran),
    ];
    let script = vec![
        // The dispatch is refused, the read runs: not every call terminated, so rpi asks the
        // model again...
        assistant(
            vec![
                call("c1", "amp_dispatch_task", json!({"task_id": 7})),
                call("c2", "amp_list_tasks", json!({})),
            ],
            StopReason::ToolUse,
        ),
        // ...and every retry is blocked with terminate, so the run ends here.
        assistant(
            vec![
                call("c3", "amp_approve_plan", json!({"project_id": 2})),
                call("c4", "amp_dispatch_task", json!({"task_id": 7})),
            ],
            StopReason::ToolUse,
        ),
        assistant(vec![Content::text("never")], StopReason::Stop),
    ];
    for mode in [
        rpi_agent::ToolExecutionMode::Sequential,
        rpi_agent::ToolExecutionMode::Parallel,
    ] {
        ran.lock().unwrap().clear();
        let (messages, llm_calls) = run(script.clone(), tools.clone(), mode).await;
        assert_eq!(llm_calls, 2, "{mode:?}");
        let mut got = ran.lock().unwrap().clone();
        got.sort();
        assert_eq!(got, vec!["amp_dispatch_task", "amp_list_tasks"], "{mode:?}");
        let r = results(&messages);
        assert_eq!(r.len(), 4, "{mode:?}");
        for blocked in &r[2..] {
            assert!(
                blocked.1 && blocked.2.contains("the harness ended this turn"),
                "{blocked:?}"
            );
        }
    }
}
