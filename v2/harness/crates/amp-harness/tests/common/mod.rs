//! A mock AMP for the CLI tests: rmcp's Streamable HTTP server in-process, serving `amp_*` tools
//! whose results are shaped like AMP's (`v2/amp-api/internal/mcp`), plus a runner for the binary
//! with an isolated HOME, a file credential store and no AMP_* variables leaking in.
#![allow(dead_code, reason = "each test binary uses a different subset")]

use std::{
    borrow::Cow,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

use axum::{
    extract::Request,
    http::{HeaderValue, StatusCode, header},
    middleware::{self, Next},
    response::{IntoResponse, Response},
};
use rmcp::{
    ErrorData, RoleServer, ServerHandler,
    model::{
        CallToolRequestParams, CallToolResponse, CallToolResult, ContentBlock, DiscoverResult,
        ErrorCode, Implementation, ListToolsResult, PaginatedRequestParams, ProtocolVersion,
        ServerCapabilities, ServerConfig, Tool,
    },
    service::{MaybeSendFuture, RequestContext},
    transport::streamable_http_server::{
        StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
    },
};
use serde_json::{Value, json};

/// The D11 contract (kept in sync by `doctor_reports_missing_contract_tools`).
pub const CONTRACT: &[&str] = &[
    "amp_whoami",
    "amp_get_project",
    "amp_list_projects",
    "amp_list_tasks",
    "amp_get_task",
    "amp_get_plan",
    "amp_check_plan",
    "amp_dispatch_task",
    "amp_complete_task",
    "amp_block_task",
    "amp_report_run",
    "amp_answer_question",
    "amp_list_artifacts",
    "amp_create_project",
    "amp_kb_write",
];

#[derive(Clone)]
pub struct MockAmp {
    /// Answer `server/discover` with "method not found" (an AMP older than 2026-07-28).
    pub legacy_only: bool,
    /// Tools to leave out of `tools/list`.
    pub omit: Vec<&'static str>,
    /// Every `tools/call` as (name, arguments).
    pub calls: Arc<Mutex<Vec<(String, Value)>>>,
}

impl MockAmp {
    pub fn new() -> Self {
        Self {
            legacy_only: false,
            omit: Vec::new(),
            calls: Arc::default(),
        }
    }

    pub fn calls_to(&self, tool: &str) -> Vec<Value> {
        self.calls
            .lock()
            .unwrap()
            .iter()
            .filter(|(name, _)| name == tool)
            .map(|(_, args)| args.clone())
            .collect()
    }
}

fn tool(name: &str) -> Tool {
    let Value::Object(schema) = json!({"type": "object", "properties": {}}) else {
        unreachable!()
    };
    Tool::new(name.to_owned(), format!("{name} (mock)"), Arc::new(schema))
}

fn ok_json(value: Value) -> Result<CallToolResponse, ErrorData> {
    Ok(CallToolResult::success(vec![ContentBlock::text(value.to_string())]).into())
}

fn tool_error(message: &str) -> Result<CallToolResponse, ErrorData> {
    Ok(CallToolResult::error(vec![ContentBlock::text(message.to_owned())]).into())
}

pub fn whoami_json() -> Value {
    json!({
        "user_id": 7, "email": "local-dev@example.com", "global_admin": false, "account": "human",
        "via_api_token": false,
        "orgs": [{
            "id": 3, "name": "simstech", "slug": "simstech", "created_by": 1,
            "created_at": "2026-10-16T19:21:01Z", "my_role": "owner",
            "teams": [{"user_id": 4, "email": "Engineering", "display_name": "", "role": "lead"}]
        }]
    })
}

fn task(id: i64, name: &str, state: &str, extra: Value) -> Value {
    let mut t = json!({
        "id": id, "project_id": 2, "epic_id": 161, "story_id": 391, "name": name,
        "description": "", "acceptance_criteria": "", "state": state, "priority": "2",
        "dependency_ids": null, "created_by": 7, "owner_user_id": 7, "performer": "agent",
        "metadata": {}, "created_at": "2026-10-22T13:17:39Z", "updated_at": "2026-10-22T14:17:16Z"
    });
    if let (Value::Object(t), Value::Object(extra)) = (&mut t, extra) {
        t.extend(extra);
    }
    t
}

/// `amp_list_tasks` with `format: "full"`, like AMP's buckets.
pub fn list_tasks_json(owner: &str) -> Value {
    let question = task(
        2084,
        "M1-3 OAuth",
        "blocked",
        json!({"block_reason": "Keychain or file?", "assigned_to": "amp:amp-worker-backend"}),
    );
    json!({
        "ready_to_dispatch": [task(2086, "M1-5 Smoke test", "backlog", json!({"assigned_to": "amp:amp-worker-backend"}))],
        "in_progress": [task(2085, "M1-4 CLI", "in_progress", json!({"assigned_to": "amp:amp-worker-backend", "dependency_ids": [2084]}))],
        "blocked": [
            task(2087, "M1-6 KB how-to", "blocked", json!({"assigned_to": "amp:amp-worker-docs", "dependency_ids": [2086], "blocked_by_ids": [2086]})),
            question.clone()
        ],
        "scheduled": [task(2090, "Later", "blocked", json!({"block_reason": "scheduled: 2026-11-01", "start_at": "2026-11-01T09:00:00Z"}))],
        "questions": [question],
        "completed": [task(2083, "M1-2 connect", "completed", json!({"assigned_to": "amp:amp-worker-backend", "completed_at": "2026-10-22T12:00:00Z"}))],
        "quiet": [],
        "waiting_on_people": [{"id": 2092, "name": "Merge PR M1", "reason": "merge", "owner": "jacob@example.com", "state": "ready", "waiting_hours": 3}],
        "count": 7,
        "owner_filter": owner
    })
}

/// `amp_list_tasks(owner=any, format=compact)`: the board the metadata poller reads.
pub fn compact_tasks_json() -> Value {
    json!({
        "ready_to_dispatch": [2086], "in_progress": [2085],
        "questions": [{"id": 2084, "name": "M1-3 OAuth", "question": "Keychain or file?"}],
        "waiting": 1, "scheduled": 1, "completed": 1, "quiet": [], "waiting_on_people": [2092],
        "by_epic": {"AMP Harness": [
            {"id": 2085, "name": "M1-4 CLI", "state": "in_progress", "assigned_to": "amp:amp-worker-backend"}
        ]},
        "count": 7, "owner_filter": "any"
    })
}

pub fn get_task_json(with_comments: bool) -> Value {
    let mut out = json!({
        "task": task(2085, "M1-4 CLI", "in_progress", json!({
            "description": "Build the CLI.\nSecond line.",
            "acceptance_criteria": "- doctor works",
            "assigned_to": "amp:amp-worker-backend",
            "dependency_ids": [2084],
            "dispatched_at": "2026-10-22T14:17:16Z"
        })),
        "effective_metadata": {}
    });
    if with_comments {
        out["comments"] = json!([
            {"id": 1, "task_id": 2085, "body": "Starting work.", "author": "amp-worker", "created_at": "2026-10-22T14:20:00Z"}
        ]);
        out["comments_total"] = json!(1);
    }
    out
}

impl ServerHandler for MockAmp {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
            .with_server_info(Implementation::new("amp-mock", "1.2.3"))
            .with_instructions("AMP mock instructions.")
    }

    fn supported_protocol_versions(&self) -> Cow<'static, [ProtocolVersion]> {
        Cow::Borrowed(ProtocolVersion::KNOWN_VERSIONS)
    }

    fn discover(
        &self,
        _context: RequestContext<RoleServer>,
    ) -> impl Future<Output = Result<DiscoverResult, ErrorData>> + MaybeSendFuture + '_ {
        std::future::ready(if self.legacy_only {
            Err(ErrorData::new(
                ErrorCode::METHOD_NOT_FOUND,
                "Method not found",
                None,
            ))
        } else {
            Ok(DiscoverResult::from_server_info(
                self.supported_protocol_versions().into_owned(),
                self.get_info(),
            ))
        })
    }

    async fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        _context: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        let tools = CONTRACT
            .iter()
            .chain(&["amp_kb_search", "amp_fail"])
            .filter(|name| !self.omit.contains(name))
            .map(|name| tool(name))
            .collect();
        Ok(ListToolsResult::with_all_items(tools))
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
        match request.name.as_ref() {
            "amp_whoami" => ok_json(whoami_json()),
            "amp_get_project" => match args["project_id"].as_i64() {
                Some(2) => ok_json(
                    json!({"project": {"id": 2, "name": "AMP v2", "code": "amp-v2", "state": "active"}}),
                ),
                _ => tool_error("project not found"),
            },
            "amp_list_tasks" => {
                // M3-3: the metadata poller's board (render-chrome --live).
                if args["format"] == "compact" && args["owner"] == "any" && args["project_id"] == 2
                {
                    return ok_json(compact_tasks_json());
                }
                if args["format"] != "full" {
                    return tool_error("mock: the CLI must ask for format=full");
                }
                if args["project_id"] != 2 {
                    return tool_error("project not found");
                }
                ok_json(list_tasks_json(args["owner"].as_str().unwrap_or("mine")))
            }
            "amp_get_task" => match args["task_id"].as_i64() {
                Some(2085) => {
                    let with_comments = args["include"]
                        .as_array()
                        .is_some_and(|a| a.iter().any(|v| v == "comments"));
                    ok_json(get_task_json(with_comments))
                }
                _ => tool_error("task not found"),
            },
            // M2-5: ticket 4040 belongs to someone else, like AMP's refusal.
            "amp_dispatch_task" => match args["task_id"].as_i64() {
                Some(4040) => tool_error("task 4040 is owned by someone else"),
                Some(id) => ok_json(json!({"task_id": id, "dispatched_to": args["agent_id"]})),
                None => tool_error("task_id is required"),
            },
            "amp_complete_task" | "amp_block_task" => {
                ok_json(json!({"task_id": args["task_id"], "ok": true}))
            }
            // M2-6: ticket 4041's runs are refused, like AMP refusing a run it can't record.
            "amp_report_run" => match args["task_id"].as_i64() {
                Some(4041) => tool_error("task 4041 not found"),
                Some(id) => ok_json(json!({"task_id": id, "outcome": args["outcome"]})),
                None => tool_error("task_id is required"),
            },
            "amp_get_plan" => ok_json(json!({"project_id": 2, "epics": [
                {"id": 150, "name": "Earlier", "stories": []},
                {"id": 161, "name": "AMP Harness", "stories": [{"id": 391, "name": "M1 CLI"}]}
            ]})),
            "amp_list_artifacts" => ok_json(json!({"artifacts": []})),
            // M3-4: /questions answers through the metadata poller's connection.
            "amp_answer_question" => match args["task_id"].as_i64() {
                Some(2084) => ok_json(json!({"task_id": 2084, "state": "backlog", "ok": true})),
                _ => tool_error("task is not blocked with a question"),
            },
            // M4-1 setup: the project picker and project creation.
            "amp_list_projects" => ok_json(json!({"projects": [
                {"id": 2, "name": "AMP v2", "code": "AV", "state": "active", "org_id": 1},
                {"id": 9, "name": "Old", "code": "OLD", "state": "archived", "org_id": 1}
            ]})),
            "amp_list_orgs" => ok_json(json!({"orgs": [{"id": 1, "name": "Default"}]})),
            "amp_create_project" => match (args["name"].as_str(), args["code"].as_str()) {
                (Some(name), Some(code)) if !name.is_empty() && !code.is_empty() => ok_json(
                    json!({"id": 77, "name": name, "code": code, "state": "active", "org_id": 1}),
                ),
                _ => tool_error("name and code are required"),
            },
            "amp_fail" => tool_error("it broke"),
            "amp_kb_search" => Ok(CallToolResult::success(vec![ContentBlock::text(
                "plain text, not JSON",
            )])
            .into()),
            other => Err(ErrorData::invalid_params(
                format!("unknown tool {other}"),
                None,
            )),
        }
    }
}

/// A mock AMP on a loopback port; stops when dropped.
pub struct Running {
    pub url: String,
    pub mock: MockAmp,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Running {
    fn drop(&mut self) {
        self.task.abort();
    }
}

/// Serves `mock` at `/mcp`. With `bearer`, requests without that token get a 401 challenge.
pub async fn spawn(mock: MockAmp, bearer: Option<&'static str>) -> Running {
    let app = app(&mock, bearer);
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

/// Serves `app` (built from `mock`, e.g. [`app`] plus more routes) on a loopback port.
pub async fn serve(app: axum::Router, mock: MockAmp) -> Running {
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

/// The mock's axum app (`/mcp`), for tests that serve it themselves (tests/doctor.rs: over TLS).
pub fn app(mock: &MockAmp, bearer: Option<&'static str>) -> axum::Router {
    let served = mock.clone();
    let service: StreamableHttpService<MockAmp, LocalSessionManager> = StreamableHttpService::new(
        move || Ok(served.clone()),
        Default::default(),
        StreamableHttpServerConfig::default().with_json_response(true),
    );
    let mut app = axum::Router::new().nest_service("/mcp", service);
    if let Some(token) = bearer {
        app = app.layer(middleware::from_fn(move |req: Request, next: Next| {
            require_bearer(token, req, next)
        }));
    }
    app
}

async fn require_bearer(token: &'static str, req: Request, next: Next) -> Response {
    let ok = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v == format!("Bearer {token}"));
    if ok {
        return next.run(req).await;
    }
    let mut response = (StatusCode::UNAUTHORIZED, "unauthorized").into_response();
    response.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_static(
            r#"Bearer resource_metadata="http://127.0.0.1/.well-known/oauth-protected-resource""#,
        ),
    );
    response
}

/// A per-test directory under cargo's target tmp dir (never /tmp: D13). `marker` writes a
/// `.amp.json` (with a stale `amp_api` that must be ignored).
pub fn workdir(name: &str, marker: Option<i64>) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-cli")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(dir.join("home")).unwrap();
    std::fs::create_dir_all(dir.join("project")).unwrap();
    if let Some(id) = marker {
        std::fs::write(
            dir.join("project/.amp.json"),
            json!({"project_id": id, "project_name": "AMP v2", "project_code": "amp-v2",
                   "amp_api": "https://stale.invalid"})
            .to_string(),
        )
        .unwrap();
    }
    dir
}

pub struct Output {
    pub code: i32,
    pub stdout: String,
    pub stderr: String,
}

impl Output {
    pub fn json(&self) -> Value {
        serde_json::from_str(&self.stdout)
            .unwrap_or_else(|e| panic!("stdout is not JSON ({e}):\n{}", self.stdout))
    }
}

/// Runs the binary in `dir/project` with HOME in `dir/home`, the file credential store,
/// `AMP_MCP_URL=url` and no other AMP_* settings. `env` adds variables.
pub async fn run(dir: &Path, url: &str, args: &[&str], env: &[(&str, &str)]) -> Output {
    let mut cmd = tokio::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"));
    cmd.args(args)
        .current_dir(dir.join("project"))
        .env("HOME", dir.join("home"))
        .env("XDG_CONFIG_HOME", dir.join("home/.config"))
        .env("AMP_CREDENTIAL_STORE", "file")
        .env("AMP_MCP_URL", url)
        // The worktree's own .amp.json sits above target/; never find it.
        .env("AMP_HARNESS_CEILING_DIR", dir)
        .env_remove("AMP_TOKEN")
        .env_remove("AMP_CA_FILE")
        .env_remove("AMP_MCP_PROTOCOL")
        .env_remove("AMP_HARNESS_PROFILE")
        .env_remove("ANTHROPIC_API_KEY")
        .env_remove("OPENROUTER_API_KEY")
        .env_remove("RUST_LOG");
    for (k, v) in env {
        cmd.env(k, v);
    }
    let out = tokio::time::timeout(std::time::Duration::from_secs(60), cmd.output())
        .await
        .expect("amp-harness finished within 60 s")
        .expect("run amp-harness");
    Output {
        code: out.status.code().unwrap_or(-1),
        stdout: String::from_utf8(out.stdout).unwrap(),
        stderr: String::from_utf8(out.stderr).unwrap(),
    }
}
