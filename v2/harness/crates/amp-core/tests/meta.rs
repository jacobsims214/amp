//! The AMP metadata poller (#2104) against a mock AMP: rmcp's Streamable HTTP server in-process,
//! answering the tools the poller calls with AMP-shaped JSON (`v2/amp-api/internal/mcp`).
//!
//! No model is involved anywhere here: the poller calls AMP on its own client, so polling costs
//! no tokens. Intervals are shortened to milliseconds; waits are bounded.

use std::{
    borrow::Cow,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::Duration,
};

use amp_core::meta::{
    AmpMeta, Connection, Connector, HealthLevel, Intervals, MetaHandle, MetaOptions, MetaPoller,
};
use amp_core::tickets::Status;
use amp_mcp::{AmpMcpClient, ClientOptions};
use axum::{
    extract::Request,
    http::StatusCode,
    middleware::{self, Next},
    response::{IntoResponse, Response},
};
use rmcp::{
    ErrorData, RoleServer, ServerHandler,
    model::{
        CallToolRequestParams, CallToolResponse, CallToolResult, ContentBlock, DiscoverResult,
        Implementation, ListToolsResult, PaginatedRequestParams, ProtocolVersion,
        ServerCapabilities, ServerConfig, Tool,
    },
    service::{MaybeSendFuture, RequestContext},
    transport::streamable_http_server::{
        StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
    },
};
use rpi_agent::{AgentEvent, AgentToolResult};
use serde_json::{Value, json};

const TOOLS: &[&str] = &[
    "amp_whoami",
    "amp_get_project",
    "amp_list_tasks",
    "amp_get_task",
    "amp_get_plan",
    "amp_check_plan",
    "amp_list_artifacts",
    "amp_answer_question",
];

/// What the mock answers; tests change it while the poller runs.
#[derive(Debug, Clone)]
struct Board {
    /// The current ticket's AMP state.
    ticket_state: &'static str,
    require_plan_approval: bool,
    artifact_status: &'static str,
}

#[derive(Clone)]
struct Mock {
    board: Arc<Mutex<Board>>,
    calls: Arc<Mutex<Vec<(String, Value)>>>,
    /// Answer every call with a JSON-RPC error.
    fail: Arc<AtomicBool>,
    /// Delay every call by this many ms.
    delay_ms: Arc<AtomicU64>,
    /// Reject every HTTP request with 401.
    unauthorized: Arc<AtomicBool>,
    /// HTTP requests seen, rejected ones included.
    requests: Arc<AtomicU64>,
    /// The plan was approved (in the web UI): `amp_check_plan` reports nothing awaiting.
    approved: Arc<AtomicBool>,
    /// The awaiting tickets are someone else's (#2198 per-ticket answer, `yours: false`).
    others: Arc<AtomicBool>,
}

impl Mock {
    fn new(board: Board) -> Self {
        Self {
            board: Arc::new(Mutex::new(board)),
            calls: Arc::default(),
            fail: Arc::default(),
            delay_ms: Arc::default(),
            unauthorized: Arc::default(),
            requests: Arc::default(),
            approved: Arc::default(),
            others: Arc::default(),
        }
    }

    fn count(&self, tool: &str) -> usize {
        self.calls
            .lock()
            .unwrap()
            .iter()
            .filter(|(n, _)| n == tool)
            .count()
    }

    fn total(&self) -> usize {
        self.calls.lock().unwrap().len()
    }

    fn args_of(&self, tool: &str) -> Vec<Value> {
        self.calls
            .lock()
            .unwrap()
            .iter()
            .filter(|(n, _)| n == tool)
            .map(|(_, a)| a.clone())
            .collect()
    }
}

fn ok(v: Value) -> Result<CallToolResponse, ErrorData> {
    Ok(CallToolResult::success(vec![ContentBlock::text(v.to_string())]).into())
}

fn task_json(id: i64, state: &str) -> Value {
    json!({"task": {
        "id": id, "project_id": 2, "epic_id": 161, "story_id": 393,
        "name": format!("Ticket {id}"), "state": state,
        "block_reason": if state == "blocked" { "Keychain or file?" } else { "" },
        "assigned_to": "amp:amp-worker-backend", "dispatched_at": "2026-10-22T17:36:06Z"
    }, "effective_metadata": {}})
}

impl ServerHandler for Mock {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
            .with_server_info(Implementation::new("amp-mock", "9.9.9"))
    }

    fn supported_protocol_versions(&self) -> Cow<'static, [ProtocolVersion]> {
        Cow::Borrowed(ProtocolVersion::KNOWN_VERSIONS)
    }

    fn discover(
        &self,
        _context: RequestContext<RoleServer>,
    ) -> impl Future<Output = Result<DiscoverResult, ErrorData>> + MaybeSendFuture + '_ {
        std::future::ready(Ok(DiscoverResult::from_server_info(
            self.supported_protocol_versions().into_owned(),
            self.get_info(),
        )))
    }

    async fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        _context: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        let Value::Object(schema) = json!({"type": "object"}) else {
            unreachable!()
        };
        let schema = Arc::new(schema);
        Ok(ListToolsResult::with_all_items(
            TOOLS
                .iter()
                .map(|n| Tool::new(n.to_string(), "mock", schema.clone()))
                .collect(),
        ))
    }

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        _context: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        let args = Value::Object(request.arguments.clone().unwrap_or_default());
        self.calls
            .lock()
            .unwrap()
            .push((request.name.to_string(), args.clone()));
        let delay = self.delay_ms.load(Ordering::SeqCst);
        if delay > 0 {
            tokio::time::sleep(Duration::from_millis(delay)).await;
        }
        if self.fail.load(Ordering::SeqCst) {
            return Err(ErrorData::internal_error("database is down", None));
        }
        let board = self.board.lock().unwrap().clone();
        match request.name.as_ref() {
            "amp_whoami" => ok(json!({
                "user_id": 36199, "email": "jacob@example.com", "account": "human",
                "via_api_token": false, "global_admin": true,
                "orgs": [{"id": 3, "name": "simstech", "slug": "simstech", "teams": []}]
            })),
            "amp_get_project" => ok(json!({"project": {
                "id": 2, "name": "AMP", "code": "amp", "state": "active", "org_id": 3,
                "require_plan_approval": board.require_plan_approval
            }})),
            "amp_list_tasks" => {
                assert_eq!(
                    args["owner"], "any",
                    "board counts cover everyone's tickets"
                );
                assert_eq!(args["format"], "compact");
                ok(json!({
                    "ready_to_dispatch": [2105, 2106], "in_progress": [2104],
                    "questions": [{"id": 2107, "name": "M3-5", "question": "Dark or light?"}],
                    "waiting": 4, "scheduled": 1, "completed": 30, "quiet": [],
                    "waiting_on_people": [],
                    "by_epic": {"AMP Harness": [
                        {"id": 2104, "name": "M3-2 AMP metadata poller", "state": "in_progress",
                         "assigned_to": "amp:amp-worker-backend"}
                    ]},
                    "count": 38, "owner_filter": "any"
                }))
            }
            "amp_get_task" if args["task_id"] == 99_999 => {
                Err(ErrorData::invalid_params("task 99999 not found", None))
            }
            "amp_answer_question" => ok(json!({"ok": true, "task_id": args["task_id"]})),
            "amp_get_task" => {
                let id = args["task_id"].as_i64().unwrap_or(0);
                let state = if id == 2104 {
                    board.ticket_state
                } else {
                    "in_progress"
                };
                ok(task_json(id, state))
            }
            "amp_get_plan" => ok(json!({"project_id": 2, "tickets": 40, "epics": [
                {"id": 150, "name": "Earlier", "state": "done", "tickets": {}, "stories": []},
                {"id": 161, "name": "AMP Harness", "state": "active", "tickets": {}, "stories": [
                    {"id": 390, "name": "Research", "state": "done", "tickets": {}},
                    {"id": 393, "name": "M3 theme + metadata", "state": "active", "tickets": {}}
                ]}
            ]})),
            "amp_check_plan" => {
                let awaiting: Vec<i64> = if self.approved.load(Ordering::SeqCst) {
                    vec![]
                } else {
                    vec![2105, 2106]
                };
                let mut plan = json!({
                "ok": true,
                "awaiting_approval": awaiting,
                "require_plan_approval": board.require_plan_approval, "tasks": 40,
                "open_tasks": 10, "dependencies": 30, "ready_now": [], "longest_chain": 5,
                "issues": [], "human_gates": [], "summary": "GO"
                });
                if self.others.load(Ordering::SeqCst) {
                    plan["awaiting_approval_tickets"] = awaiting
                        .iter()
                        .map(|id| json!({"task_id": id, "owner_user_id": 9, "yours": false}))
                        .collect();
                    plan["awaiting_yours"] = json!([]);
                }
                ok(plan)
            }
            "amp_list_artifacts" => ok(json!({"artifacts": [
                {"id": 3, "project_id": 2, "task_id": args["task_id"], "title": "Header mock",
                 "kind": "html", "status": board.artifact_status, "current_version": 2}
            ], "count": 1})),
            other => Err(ErrorData::invalid_params(
                format!("unknown tool {other}"),
                None,
            )),
        }
    }
}

struct Running {
    url: String,
    mock: Mock,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Running {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn serve(mock: Mock) -> Running {
    let served = mock.clone();
    let service: StreamableHttpService<Mock, LocalSessionManager> = StreamableHttpService::new(
        move || Ok(served.clone()),
        Default::default(),
        StreamableHttpServerConfig::default().with_json_response(true),
    );
    let flag = mock.unauthorized.clone();
    let requests = mock.requests.clone();
    let app = axum::Router::new()
        .nest_service("/mcp", service)
        .layer(middleware::from_fn(move |req: Request, next: Next| {
            let flag = flag.clone();
            requests.fetch_add(1, Ordering::SeqCst);
            async move {
                if flag.load(Ordering::SeqCst) {
                    let mut r: Response = (StatusCode::UNAUTHORIZED, "no").into_response();
                    r.headers_mut().insert(
                        "www-authenticate",
                        "Bearer resource_metadata=\"http://127.0.0.1/.well-known/x\""
                            .parse()
                            .unwrap(),
                    );
                    return r;
                }
                next.run(req).await
            }
        }));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Running {
        url: format!("http://{addr}/mcp"),
        mock,
        task,
    }
}

fn default_board() -> Board {
    Board {
        ticket_state: "in_progress",
        require_plan_approval: true,
        artifact_status: "in_review",
    }
}

/// Intervals in milliseconds, so a test sees several cycles in about a second.
fn fast(board_ms: u64) -> Intervals {
    Intervals {
        board: Duration::from_millis(board_ms),
        ticket: Duration::from_millis(board_ms),
        plan: Duration::from_millis(board_ms),
        plan_pending: Duration::from_millis(board_ms),
        artifacts: Duration::from_millis(board_ms),
        idle_ping: Duration::from_secs(3600),
        call_timeout: Duration::from_secs(5),
        slow_after: Duration::from_secs(2),
        reconnect: Duration::from_millis(200),
    }
}

async fn client(url: &str) -> Arc<AmpMcpClient> {
    Arc::new(
        AmpMcpClient::connect(ClientOptions::new(url))
            .await
            .expect("connect to the mock"),
    )
}

/// Waits (at most 5 s) until `cond` holds for the snapshot.
async fn wait_for(meta: &MetaHandle, what: &str, cond: impl Fn(&AmpMeta) -> bool) -> Arc<AmpMeta> {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    loop {
        let snap = meta.snapshot();
        if cond(&snap) {
            return snap;
        }
        assert!(
            tokio::time::Instant::now() < deadline,
            "timed out waiting for {what}; last snapshot: {snap:#?}"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

/// Waits (at most 5 s) until `cond` holds for the mock's calls.
async fn wait_calls(mock: &Mock, what: &str, cond: impl Fn(&Mock) -> bool) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    while !cond(mock) {
        assert!(
            tokio::time::Instant::now() < deadline,
            "timed out waiting for {what}; calls: {:?}",
            mock.calls.lock().unwrap()
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

fn loaded(m: &AmpMeta) -> bool {
    m.user.get().is_some()
        && m.project.get().is_some()
        && m.board.get().is_some()
        && m.ticket.get().is_some()
        && m.plan.get().is_some()
        && m.artifacts.get().is_some()
}

fn tool_call(meta: &MetaHandle, id: &str, name: &str, args: Value, is_error: bool) {
    meta.observe(&AgentEvent::ToolExecutionStart {
        tool_call_id: id.into(),
        tool_name: name.into(),
        args,
    });
    meta.observe(&AgentEvent::ToolExecutionEnd {
        tool_call_id: id.into(),
        tool_name: name.into(),
        result: AgentToolResult::text("{}"),
        is_error,
    });
}

#[tokio::test(flavor = "multi_thread")]
async fn loads_every_item_on_start_and_polls_on_the_intervals() {
    let server = serve(Mock::new(default_board())).await;
    let meta = MetaPoller::spawn(
        Some(client(&server.url).await),
        MetaOptions {
            project_marker: None,
            project_id: Some(2),
            ticket_id: Some(2104),
            intervals: fast(150),
            connector: None,
            web_url: None,
        },
    );
    let m = wait_for(&meta, "every item", loaded).await;

    let instance = m.instance.as_ref().unwrap();
    assert_eq!(instance.web_base, server.url.trim_end_matches("/mcp"));
    assert_eq!(instance.protocol, "2026-07-28");
    assert_eq!(m.user.get().unwrap().email, "jacob@example.com");
    let project = m.project.get().unwrap();
    assert_eq!((project.id, project.name.as_str()), (2, "AMP"));
    assert_eq!(m.org_name(), Some("simstech"));
    let board = m.board.get().unwrap();
    assert_eq!(
        board.nonzero(),
        vec![
            (Status::InProgress, 1),
            (Status::Ready, 2),
            (Status::Question, 1),
            (Status::Scheduled, 1)
        ]
    );
    assert_eq!(board.questions[0].question, "Dark or light?");
    assert_eq!(board.in_progress_tickets[0].id, 2104);
    let ticket = m.ticket.get().unwrap();
    assert_eq!(ticket.status, Status::InProgress);
    assert_eq!(ticket.epic.as_ref().unwrap().name, "AMP Harness");
    assert_eq!(ticket.story.as_ref().unwrap().name, "M3 theme + metadata");
    assert_eq!(ticket.epic_index, Some(1), "cat colour by plan order");
    assert_eq!(m.plan.get().unwrap().awaiting_approval, vec![2105, 2106]);
    assert_eq!(m.artifacts_in_review().len(), 1);
    assert_eq!(m.health.connection, Connection::Ok);
    assert_eq!(m.health.connection.level(), HealthLevel::Ok);

    // Over ~1.2 s at 150 ms: several more cycles of every periodic item, whoami only once
    // (on connect), and the plan tree read once (cached).
    let before = server.mock.count("amp_list_tasks");
    tokio::time::sleep(Duration::from_millis(1200)).await;
    let polled = server.mock.count("amp_list_tasks") - before;
    assert!((3..=12).contains(&polled), "board polled {polled} times");
    assert!(server.mock.count("amp_get_task") >= 4);
    assert!(server.mock.count("amp_check_plan") >= 4);
    assert!(server.mock.count("amp_list_artifacts") >= 4);
    assert_eq!(server.mock.count("amp_whoami"), 1);
    assert_eq!(server.mock.count("amp_get_project"), 1);
    assert_eq!(server.mock.count("amp_get_plan"), 1);
    assert!(meta.snapshot().revision > m.revision);
}

/// #2230: only others' tickets wait, and a lead approves them on their owners' behalf. The others'
/// chip clears on the fast `plan_pending` poll too, not after the 60 s plan poll.
#[tokio::test(flavor = "multi_thread")]
async fn an_on_behalf_approval_clears_the_others_chip_on_the_fast_poll() {
    let mock = Mock::new(default_board());
    mock.others.store(true, Ordering::SeqCst);
    let server = serve(mock).await;
    let meta = MetaPoller::spawn(
        Some(client(&server.url).await),
        MetaOptions {
            project_marker: None,
            project_id: Some(2),
            ticket_id: None,
            intervals: Intervals {
                plan: Duration::from_secs(60),
                plan_pending: Duration::from_millis(150),
                ..fast(150)
            },
            connector: None,
            web_url: None,
        },
    );
    let m = wait_for(&meta, "others' tickets waiting", |m| m.plan_awaiting()).await;
    let plan = m.plan.get().unwrap();
    assert_eq!((plan.yours(), plan.others()), (vec![], vec![2105, 2106]));
    assert!(!m.plan_pending(), "none of them are yours");
    wait_calls(&server.mock, "a fast plan poll", |m| {
        m.count("amp_check_plan") >= 3
    })
    .await;

    // A lead approves on the owners' behalf; nothing tells this harness.
    server.mock.approved.store(true, Ordering::SeqCst);
    let started = tokio::time::Instant::now();
    let m = wait_for(&meta, "the others' chip to clear", |m| !m.plan_awaiting()).await;
    assert!(
        started.elapsed() < Duration::from_secs(2),
        "cleared after {:?}",
        started.elapsed()
    );
    assert!(m.plan.get().unwrap().others().is_empty());
}

/// #2209: approving in the web UI clears the "waiting for your approval" chip on the fast
/// `plan_pending` poll, not the 60 s plan poll; once nothing is pending the plan poll slows down.
#[tokio::test(flavor = "multi_thread")]
async fn an_approval_in_the_web_ui_clears_the_pending_plan_on_the_fast_poll() {
    let server = serve(Mock::new(default_board())).await;
    let meta = MetaPoller::spawn(
        Some(client(&server.url).await),
        MetaOptions {
            project_marker: None,
            project_id: Some(2),
            ticket_id: None,
            intervals: Intervals {
                plan: Duration::from_secs(60),
                plan_pending: Duration::from_millis(150),
                ..fast(150)
            },
            connector: None,
            web_url: None,
        },
    );
    wait_for(&meta, "the plan waiting for approval", |m| m.plan_pending()).await;
    // While pending, the plan is polled on the fast interval, not the 60 s one.
    wait_calls(&server.mock, "a fast plan poll", |m| {
        m.count("amp_check_plan") >= 3
    })
    .await;

    // A person approves in the web UI; nothing tells the harness.
    server.mock.approved.store(true, Ordering::SeqCst);
    let started = tokio::time::Instant::now();
    let m = wait_for(&meta, "the chip to clear", |m| !m.plan_pending()).await;
    assert!(
        started.elapsed() < Duration::from_secs(2),
        "cleared after {:?}",
        started.elapsed()
    );
    assert!(m.plan.get().unwrap().awaiting_approval.is_empty());

    // Nothing pending: back on the 60 s plan poll.
    let before = server.mock.count("amp_check_plan");
    tokio::time::sleep(Duration::from_millis(700)).await;
    assert_eq!(server.mock.count("amp_check_plan"), before);
}

#[tokio::test(flavor = "multi_thread")]
async fn polls_only_what_applies() {
    // A done ticket, no plan approval, an approved artifact: only the board keeps polling.
    let server = serve(Mock::new(Board {
        ticket_state: "completed",
        require_plan_approval: false,
        artifact_status: "approved",
    }))
    .await;
    let meta = MetaPoller::spawn(
        Some(client(&server.url).await),
        MetaOptions {
            project_marker: None,
            project_id: Some(2),
            ticket_id: Some(2104),
            intervals: fast(100),
            connector: None,
            web_url: None,
        },
    );
    let m = wait_for(&meta, "the start-up load", |m| {
        m.ticket.get().is_some() && m.artifacts.get().is_some() && m.board.get().is_some()
    })
    .await;
    assert_eq!(m.ticket.get().unwrap().status, Status::Done);
    assert!(
        m.plan.get().is_none(),
        "no approval required: never checked"
    );
    tokio::time::sleep(Duration::from_millis(800)).await;
    assert!(server.mock.count("amp_list_tasks") >= 4);
    assert_eq!(server.mock.count("amp_get_task"), 1);
    assert_eq!(server.mock.count("amp_list_artifacts"), 1);
    assert_eq!(server.mock.count("amp_check_plan"), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn amp_tool_calls_refresh_at_once() {
    let server = serve(Mock::new(default_board())).await;
    let mock = &server.mock;
    let meta = MetaPoller::spawn(
        Some(client(&server.url).await),
        MetaOptions {
            project_marker: None,
            project_id: Some(2),
            ticket_id: Some(2104),
            // Nothing is due again for an hour: every later call comes from a tool trigger.
            intervals: fast(3_600_000),
            connector: None,
            web_url: None,
        },
    );
    wait_for(&meta, "every item", loaded).await;
    let start = mock.total();
    let (board, ticket) = (mock.count("amp_list_tasks"), mock.count("amp_get_task"));

    // Not AMP, an AMP read, and a failed AMP call: nothing.
    tool_call(&meta, "c1", "bash", json!({"command": "ls"}), false);
    tool_call(&meta, "c2", "amp_get_task", json!({"task_id": 2104}), false);
    tool_call(
        &meta,
        "c3",
        "amp_complete_task",
        json!({"task_id": 2104}),
        true,
    );
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert_eq!(mock.total(), start, "nothing refreshed");

    // The model completes the current ticket: board and ticket are refetched right away.
    mock.board.lock().unwrap().ticket_state = "completed";
    tool_call(
        &meta,
        "c4",
        "amp_complete_task",
        json!({"task_id": 2104}),
        false,
    );
    wait_for(&meta, "the ticket to read done", |m| {
        m.ticket.get().is_some_and(|t| t.status == Status::Done)
    })
    .await;
    wait_calls(mock, "a board refresh", |m| {
        m.count("amp_list_tasks") == board + 1
    })
    .await;
    assert_eq!(mock.count("amp_get_task"), ticket + 1);

    // Publishing an artifact refetches the artifacts only.
    let artifacts = mock.count("amp_list_artifacts");
    tool_call(
        &meta,
        "c5",
        "amp_publish_artifact",
        json!({"task_id": 2104}),
        false,
    );
    wait_calls(mock, "an artifacts refresh", |m| {
        m.count("amp_list_artifacts") == artifacts + 1
    })
    .await;
    assert_eq!(mock.count("amp_list_tasks"), board + 1);

    // Claiming a ticket makes it the current one.
    tool_call(
        &meta,
        "c6",
        "amp_claim_task",
        json!({"task_id": 2105}),
        false,
    );
    let m = wait_for(&meta, "ticket 2105", |m| {
        m.ticket.get().is_some_and(|t| t.id == 2105)
    })
    .await;
    assert_eq!(m.ticket_id, Some(2105));
    assert!(
        mock.args_of("amp_list_artifacts")
            .iter()
            .any(|a| a["task_id"] == 2105)
    );

    // The TUI's own switches.
    meta.set_ticket(None);
    let m = wait_for(&meta, "no ticket", |m| m.ticket_id.is_none()).await;
    assert!(m.ticket.get().is_none() && m.artifacts.get().is_none());
    let projects = mock.count("amp_get_project");
    meta.refresh(amp_core::meta::Topics::PROJECT);
    wait_calls(mock, "a project refresh", |m| {
        m.count("amp_get_project") == projects + 1
    })
    .await;
}

#[tokio::test(flavor = "multi_thread")]
async fn health_follows_failures_slowness_and_auth() {
    let server = serve(Mock::new(default_board())).await;
    let mock = &server.mock;
    // No project or ticket: the only calls are idle health probes (amp_whoami) every 100 ms.
    let mut iv = fast(3_600_000);
    iv.idle_ping = Duration::from_millis(100);
    iv.slow_after = Duration::from_millis(150);
    iv.call_timeout = Duration::from_millis(500);
    let meta = MetaPoller::spawn(
        Some(client(&server.url).await),
        MetaOptions {
            intervals: iv,
            ..MetaOptions::default()
        },
    );
    wait_for(&meta, "a user", |m| m.user.get().is_some()).await;
    wait_calls(mock, "idle pings", |m| m.count("amp_whoami") >= 3).await;
    assert_eq!(mock.count("amp_list_tasks"), 0);

    // #2108: AMP answering with an error (a JSON-RPC error here) is the item's error, not the
    // connection's: the user item goes stale and the health stays connected.
    mock.fail.store(true, Ordering::SeqCst);
    let m = wait_for(&meta, "a stale user", |m| m.user.error.is_some()).await;
    assert!(m.user.get().is_some(), "the last good value is kept");
    assert_eq!(m.health.connection, Connection::Ok);
    let calls = mock.count("amp_whoami");
    wait_calls(mock, "more failing pings", |m| {
        m.count("amp_whoami") >= calls + 2
    })
    .await;
    let m = meta.snapshot();
    assert_eq!(m.health.connection, Connection::Ok, "still connected");
    assert!(
        m.health
            .last_ok
            .is_some_and(|at| at >= m.health.last_call.unwrap())
    );
    mock.fail.store(false, Ordering::SeqCst);
    wait_for(&meta, "a fresh user", |m| m.user.error.is_none()).await;

    // No answer in time (a transport-level failure) degrades the connection.
    mock.delay_ms.store(800, Ordering::SeqCst);
    let m = wait_for(&meta, "degraded", |m| {
        matches!(m.health.connection, Connection::Degraded { .. })
    })
    .await;
    assert_eq!(m.health.connection.level(), HealthLevel::Degraded);
    assert!(m.user.get().is_some(), "the last good value is kept");
    assert!(m.user.error.is_some());

    mock.delay_ms.store(0, Ordering::SeqCst);
    wait_for(&meta, "recovered", |m| {
        m.health.connection == Connection::Ok
    })
    .await;

    mock.delay_ms.store(250, Ordering::SeqCst);
    wait_for(&meta, "slow", |m| {
        matches!(m.health.connection, Connection::Slow { .. })
    })
    .await;
    mock.delay_ms.store(0, Ordering::SeqCst);

    mock.unauthorized.store(true, Ordering::SeqCst);
    let m = wait_for(&meta, "auth required", |m| {
        m.health.connection == Connection::AuthRequired
    })
    .await;
    assert_eq!(m.health.connection.level(), HealthLevel::Down);
    mock.unauthorized.store(false, Ordering::SeqCst);
    wait_for(&meta, "recovered", |m| {
        m.health.connection == Connection::Ok
    })
    .await;
}

/// #2156: `amp-harness login` registers a new OAuth client, which the open connection can't
/// refresh for, so re-login needed on an open client must heal through the connector (a fresh
/// connect picks up the new login), not by waiting for the old client to work again. Server A
/// plays the old login (401 for good), server B the connection made after the re-login.
#[tokio::test(flavor = "multi_thread")]
async fn re_login_needed_heals_by_reconnecting_through_the_connector() {
    let old = serve(Mock::new(default_board())).await;
    let new = serve(Mock::new(default_board())).await;
    let logged_in = Arc::new(AtomicBool::new(false));
    let attempts = Arc::new(AtomicU64::new(0));
    let connector: Connector = {
        let (old_url, new_url) = (old.url.clone(), new.url.clone());
        let (logged_in, attempts) = (logged_in.clone(), attempts.clone());
        Arc::new(move || {
            attempts.fetch_add(1, Ordering::SeqCst);
            let url = if logged_in.load(Ordering::SeqCst) {
                new_url.clone()
            } else {
                old_url.clone()
            };
            Box::pin(async move {
                AmpMcpClient::connect(
                    ClientOptions::new(url).with_connect_timeout(Duration::from_secs(2)),
                )
                .await
            })
        })
    };
    let reconnect = Duration::from_millis(250);
    let mut iv = fast(50);
    iv.idle_ping = Duration::from_millis(50);
    iv.reconnect = reconnect;
    let meta = MetaPoller::spawn(
        Some(client(&old.url).await),
        MetaOptions {
            project_marker: None,
            project_id: Some(2),
            ticket_id: Some(2107),
            intervals: iv,
            connector: Some(connector),
            web_url: None,
        },
    );
    wait_for(&meta, "loaded from the old login", loaded).await;
    assert_eq!(
        attempts.load(Ordering::SeqCst),
        0,
        "an open client needs no connect"
    );

    // The old login's tokens can't be refreshed any more: every request is a 401.
    old.mock.unauthorized.store(true, Ordering::SeqCst);
    wait_for(&meta, "re-login needed", |m| {
        m.health.connection == Connection::AuthRequired
    })
    .await;
    // While waiting for the login, the old client is not polled (topics are due every 50 ms):
    // the only requests are the reconnect attempts, at most one per `reconnect`.
    let (requests, tries) = (
        old.mock.requests.load(Ordering::SeqCst),
        attempts.load(Ordering::SeqCst),
    );
    let waited = Duration::from_millis(1_000);
    tokio::time::sleep(waited).await;
    let tries = attempts.load(Ordering::SeqCst) - tries;
    let most = waited.as_millis() / reconnect.as_millis() + 1;
    assert!(
        (1..=most as u64).contains(&tries),
        "{tries} reconnect attempts in {waited:?} (at most {most})"
    );
    let per_try = (old.mock.requests.load(Ordering::SeqCst) - requests) / tries;
    assert!(
        per_try <= 1,
        "{per_try} requests per attempt: the old client is still polled"
    );
    assert_eq!(meta.snapshot().health.connection, Connection::AuthRequired);

    // `amp-harness login` in another terminal: the next attempt connects with the new login.
    logged_in.store(true, Ordering::SeqCst);
    let m = wait_for(&meta, "reconnected with the new login", |m| {
        m.health.connection == Connection::Ok && m.user.error.is_none()
    })
    .await;
    assert!(m.board.get().is_some());
    wait_calls(&new.mock, "everything refetched on the new client", |m| {
        m.count("amp_whoami") >= 1 && m.count("amp_list_tasks") >= 1 && m.count("amp_get_task") >= 1
    })
    .await;
    // The old client is never called again, even once its server would answer.
    old.mock.unauthorized.store(false, Ordering::SeqCst);
    let (old_requests, new_calls) = (old.mock.requests.load(Ordering::SeqCst), new.mock.total());
    wait_calls(&new.mock, "more polls on the new client", |m| {
        m.total() >= new_calls + 6
    })
    .await;
    assert_eq!(old.mock.requests.load(Ordering::SeqCst), old_requests);
    assert_eq!(meta.snapshot().health.connection, Connection::Ok);
}

/// #2159: the session's own client (the model's amp_* tools, run reporting, the supervisor all
/// share it) heals after `amp-harness login` the same way the poller does: the next tool call
/// after the login reconnects through its reconnector and runs on the new connection.
#[tokio::test(flavor = "multi_thread")]
async fn the_models_amp_tools_heal_after_a_re_login() {
    use amp_core::mcp_tools::McpTool;
    use rpi_agent::{AgentTool, ToolResultPartial};
    use tokio_util::sync::CancellationToken;

    let old = serve(Mock::new(default_board())).await;
    let new = serve(Mock::new(default_board())).await;
    let shared = client(&old.url).await;
    let logged_in = Arc::new(AtomicBool::new(false));
    let attempts = Arc::new(AtomicU64::new(0));
    let reconnector: Connector = {
        let (old_url, new_url) = (old.url.clone(), new.url.clone());
        let (logged_in, attempts) = (logged_in.clone(), attempts.clone());
        Arc::new(move || {
            attempts.fetch_add(1, Ordering::SeqCst);
            let url = if logged_in.load(Ordering::SeqCst) {
                new_url.clone()
            } else {
                old_url.clone()
            };
            Box::pin(async move {
                AmpMcpClient::connect(
                    ClientOptions::new(url).with_connect_timeout(Duration::from_secs(2)),
                )
                .await
            })
        })
    };
    // The poller's Connector and the client's Reconnector are the same closure type.
    shared.set_reconnector(Some(reconnector));
    let interval = Duration::from_millis(300);
    shared.set_reconnect_interval(interval);
    let catalog = shared.catalog().await.expect("catalog");
    let tool = McpTool::new(
        catalog.get("amp_whoami").expect("amp_whoami"),
        shared.clone(),
    );
    async fn run(tool: &McpTool) -> Result<AgentToolResult, rpi_agent::AgentError> {
        tool.execute(
            "call-1",
            json!({}),
            CancellationToken::new(),
            Arc::new(|_: ToolResultPartial| {}),
        )
        .await
    }
    run(&tool).await.expect("works before");

    old.mock.unauthorized.store(true, Ordering::SeqCst);
    let err = run(&tool).await.unwrap_err();
    assert!(err.to_string().contains("amp-harness login"), "{err}");

    logged_in.store(true, Ordering::SeqCst);
    tokio::time::sleep(interval).await;
    let result = tokio::time::timeout(Duration::from_secs(10), run(&tool))
        .await
        .expect("no hang")
        .expect("the same tool works again after the login");
    assert!(!format!("{:?}", result.content).is_empty());
    assert_eq!(new.mock.count("amp_whoami"), 1);
    assert_eq!(attempts.load(Ordering::SeqCst), 2);
    assert_eq!(shared.generation(), 1);
}

/// #2108: a ticket that doesn't exist is that item's error (the header shows it unavailable); the
/// connection stays healthy. Slash commands' own calls run on the poller's connection.
#[tokio::test(flavor = "multi_thread")]
async fn tool_errors_mark_the_item_not_the_connection_and_commands_call_through_the_poller() {
    let server = serve(Mock::new(default_board())).await;
    let mock = &server.mock;
    let meta = MetaPoller::spawn(
        Some(client(&server.url).await),
        MetaOptions {
            project_marker: None,
            project_id: Some(2),
            ticket_id: Some(99_999),
            intervals: fast(3_600_000),
            connector: None,
            web_url: None,
        },
    );
    let m = wait_for(&meta, "the missing ticket's error", |m| {
        m.ticket.error.is_some() && m.board.get().is_some()
    })
    .await;
    assert!(m.ticket.get().is_none());
    assert!(
        m.ticket.error.as_deref().unwrap().contains("not found"),
        "{:?}",
        m.ticket.error
    );
    assert_eq!(m.health.connection, Connection::Ok);
    assert_eq!(m.health.connection.level(), HealthLevel::Ok);

    // A read through the poller: answered, nothing refreshed.
    let boards = mock.count("amp_list_tasks");
    let plan = meta
        .call(
            "amp_get_plan",
            json!({"project_id": 2}),
            Duration::from_secs(5),
        )
        .await
        .unwrap();
    assert_eq!(plan["epics"][1]["name"], "AMP Harness");
    // A failed call: its error comes back, the connection stays healthy.
    let err = meta
        .call(
            "amp_get_task",
            json!({"task_id": 99_999}),
            Duration::from_secs(5),
        )
        .await
        .unwrap_err();
    assert!(err.contains("not found"), "{err}");
    assert_eq!(meta.snapshot().health.connection, Connection::Ok);
    // A write: answered, then the board it touched is refetched.
    let answered = meta
        .call(
            "amp_answer_question",
            json!({"task_id": 2107, "answer": "Dark"}),
            Duration::from_secs(5),
        )
        .await
        .unwrap();
    assert_eq!(answered["ok"], true);
    assert_eq!(
        mock.args_of("amp_answer_question"),
        vec![json!({"task_id": 2107, "answer": "Dark"})]
    );
    wait_calls(mock, "the board refetched after the answer", |m| {
        m.count("amp_list_tasks") > boards
    })
    .await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_stub_handle_answers_without_amp() {
    let meta = MetaHandle::stub(
        AmpMeta::default(),
        Arc::new(|tool, args| match tool {
            "amp_get_plan" => Ok(json!({"epics": [], "project_id": args["project_id"]})),
            other => Err(format!("{other}: no")),
        }),
    );
    meta.set_project(Some(5));
    meta.set_ticket(Some(7));
    let m = wait_for(&meta, "the ids", |m| {
        m.project_id == Some(5) && m.ticket_id == Some(7)
    })
    .await;
    assert!(m.revision >= 2);
    let plan = meta
        .call(
            "amp_get_plan",
            json!({"project_id": 5}),
            Duration::from_secs(1),
        )
        .await
        .unwrap();
    assert_eq!(plan["project_id"], 5);
    assert!(
        meta.call("amp_x", json!({}), Duration::from_secs(1))
            .await
            .is_err()
    );
    assert_eq!(meta.stub_calls().len(), 2);
}

#[tokio::test(flavor = "multi_thread")]
async fn connects_in_the_background_and_retries() {
    let server = serve(Mock::new(default_board())).await;
    let url = server.url.clone();
    let attempts = Arc::new(AtomicU64::new(0));
    let counter = attempts.clone();
    // The first attempt fails (as if AMP were down at start); the next one connects.
    let connector: Connector = Arc::new(move || {
        let url = url.clone();
        let n = counter.fetch_add(1, Ordering::SeqCst);
        Box::pin(async move {
            let url = if n == 0 {
                "http://127.0.0.1:9/mcp".to_owned()
            } else {
                url
            };
            AmpMcpClient::connect(
                ClientOptions::new(url).with_connect_timeout(Duration::from_secs(2)),
            )
            .await
        })
    });
    let meta = MetaPoller::spawn(
        None,
        MetaOptions {
            project_id: Some(2),
            intervals: fast(3_600_000),
            connector: Some(connector),
            web_url: None,
            ..MetaOptions::default()
        },
    );
    let m = meta.snapshot();
    assert!(m.instance.is_none());
    assert_eq!(m.health.connection.level(), HealthLevel::Degraded);
    let m = wait_for(&meta, "a down connection", |m| {
        matches!(m.health.connection, Connection::Down { .. })
    })
    .await;
    assert!(m.user.get().is_none());
    let m = wait_for(&meta, "connected and loaded", |m| {
        m.user.get().is_some() && m.board.get().is_some() && m.project.get().is_some()
    })
    .await;
    assert!(attempts.load(Ordering::SeqCst) >= 2);
    assert!(m.instance.is_some());
    assert_eq!(m.health.connection, Connection::Ok);
}

#[tokio::test(flavor = "multi_thread")]
async fn the_poller_stops_with_its_last_handle() {
    let server = serve(Mock::new(default_board())).await;
    let mut iv = fast(3_600_000);
    iv.idle_ping = Duration::from_millis(50);
    let meta = MetaPoller::spawn(
        Some(client(&server.url).await),
        MetaOptions {
            intervals: iv,
            ..MetaOptions::default()
        },
    );
    let clone = meta.clone();
    wait_calls(&server.mock, "pings", |m| m.count("amp_whoami") >= 2).await;
    drop(meta);
    wait_calls(&server.mock, "more pings", |m| m.count("amp_whoami") >= 4).await;
    drop(clone);
    tokio::time::sleep(Duration::from_millis(150)).await;
    let after = server.mock.count("amp_whoami");
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert_eq!(
        server.mock.count("amp_whoami"),
        after,
        "no calls after the last handle drops"
    );
}

/// Against a real AMP, when asked: `AMP_META_LIVE_URL=http://localhost:18000/mcp
/// AMP_META_LIVE_PROJECT=<id> cargo test -p amp-core --test meta live -- --nocapture`.
/// Skips (passes) without the variables, so CI never needs an AMP.
#[tokio::test(flavor = "multi_thread")]
async fn live_amp_when_asked() {
    let Ok(url) = std::env::var("AMP_META_LIVE_URL") else {
        return;
    };
    let project_id = std::env::var("AMP_META_LIVE_PROJECT")
        .ok()
        .and_then(|p| p.parse().ok());
    let meta = MetaPoller::spawn(
        Some(client(&url).await),
        MetaOptions {
            project_id,
            ..MetaOptions::default()
        },
    );
    let m = wait_for(&meta, "the live load", |m| {
        m.user.get().is_some() && (project_id.is_none() || m.board.get().is_some())
    })
    .await;
    let instance = m.instance.as_ref().unwrap();
    println!(
        "{} ({}) as {} · health {} in {:?}",
        instance.host(),
        instance.protocol,
        m.user.get().unwrap().email,
        m.health.connection,
        m.health.latency.unwrap_or_default()
    );
    if let (Some(p), Some(b)) = (m.project.get(), m.board.get()) {
        println!(
            "project #{} {} (org {:?}, plan approval {}) · board {:?} · questions {}",
            p.id,
            p.name,
            m.org_name(),
            p.require_plan_approval,
            b.nonzero(),
            b.questions.len()
        );
    }
    assert_eq!(m.health.connection.level(), HealthLevel::Ok);

    // #2108: a ticket that doesn't exist is that item's error; the connection stays healthy.
    meta.set_ticket(Some(999_999_999));
    let m = wait_for(&meta, "the missing ticket's error", |m| {
        m.ticket_id == Some(999_999_999) && m.ticket.error.is_some()
    })
    .await;
    println!(
        "missing ticket: {:?} · health {}",
        m.ticket.error, m.health.connection
    );
    assert_eq!(m.health.connection.level(), HealthLevel::Ok);
}
