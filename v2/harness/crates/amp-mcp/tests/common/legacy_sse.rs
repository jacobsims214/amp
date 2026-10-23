//! A mock MCP 2024-11-05 server on the legacy HTTP+SSE transport (#2238): `GET <path>` opens an
//! event stream whose first event is `endpoint` (`/messages?sessionId=..`), and every client
//! message is a `POST` to it, answered 202 with the reply on the stream as a `message` event.
//! rmcp 3.6.0 has no SSE server, so the JSON-RPC is written by hand: `initialize`, `ping`,
//! `tools/list` (one `echo` tool) and `tools/call`.

use std::{
    collections::HashMap,
    convert::Infallible,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use axum::{
    Json, Router,
    extract::{Query, State},
    http::StatusCode,
    response::{
        IntoResponse, Response,
        sse::{Event, KeepAlive, Sse},
    },
    routing::{get, post},
};
use futures::Stream;
use serde_json::{Value, json};
use tokio::sync::mpsc;

/// What the `endpoint` event says.
#[derive(Debug, Clone)]
pub enum Endpoint {
    /// `/messages?sessionId=<id>`, relative like most servers send it.
    Relative,
    /// This URL verbatim (e.g. another origin, which the client must refuse).
    Literal(String),
    /// No endpoint event at all: the stream stays open and silent.
    Never,
}

pub struct LegacyState {
    endpoint: Mutex<Endpoint>,
    sessions: Mutex<HashMap<String, mpsc::UnboundedSender<Value>>>,
    seq: AtomicUsize,
    /// Event stream GETs.
    pub gets: AtomicUsize,
    /// Message POSTs that reached a session.
    pub posts: AtomicUsize,
    /// JSON-RPC methods received, in order.
    pub methods: Mutex<Vec<String>>,
    /// The protocol version the server answers `initialize` with.
    pub protocol_version: &'static str,
}

impl LegacyState {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            endpoint: Mutex::new(Endpoint::Relative),
            sessions: Mutex::new(HashMap::new()),
            seq: AtomicUsize::new(0),
            gets: AtomicUsize::new(0),
            posts: AtomicUsize::new(0),
            methods: Mutex::new(Vec::new()),
            protocol_version: "2024-11-05",
        })
    }

    pub fn set_endpoint(&self, endpoint: Endpoint) {
        *self.endpoint.lock().unwrap() = endpoint;
    }

    /// Ends every open event stream (the server restarting).
    pub fn close_all(&self) {
        self.sessions.lock().unwrap().clear();
    }

    /// Sends a server notification on every open stream.
    pub fn notify(&self, method: &str) {
        for tx in self.sessions.lock().unwrap().values() {
            let _ = tx.send(json!({"jsonrpc": "2.0", "method": method}));
        }
    }

    pub fn open_sessions(&self) -> usize {
        self.sessions.lock().unwrap().len()
    }
}

/// The legacy server: the event stream at `path` (GET only, so a Streamable HTTP POST there gets
/// axum's 405) and the message endpoint at `/messages`.
pub fn router(state: Arc<LegacyState>, path: &str) -> Router {
    Router::new()
        .route(path, get(open_stream))
        .route("/messages", post(message))
        .with_state(state)
}

async fn open_stream(
    State(state): State<Arc<LegacyState>>,
) -> Sse<impl Stream<Item = Result<Event, Infallible>>> {
    state.gets.fetch_add(1, Ordering::SeqCst);
    let id = format!("s{}", state.seq.fetch_add(1, Ordering::SeqCst));
    let (tx, rx) = mpsc::unbounded_channel::<Value>();
    let first = match state.endpoint.lock().unwrap().clone() {
        Endpoint::Relative => Some(format!("/messages?sessionId={id}")),
        Endpoint::Literal(url) => Some(url),
        Endpoint::Never => None,
    };
    if first.is_some() {
        state.sessions.lock().unwrap().insert(id, tx);
    } else {
        // Keep the stream open but silent.
        std::mem::forget(tx);
    }
    let first =
        futures::stream::iter(first.map(|url| Ok(Event::default().event("endpoint").data(url))));
    let rest = futures::stream::unfold(rx, |mut rx| async move {
        let message = rx.recv().await?;
        Some((
            Ok(Event::default().event("message").data(message.to_string())),
            rx,
        ))
    });
    use futures::StreamExt as _;
    // Keep-alive comments exercise the client's handling of non-message lines.
    Sse::new(first.chain(rest)).keep_alive(KeepAlive::new().interval(Duration::from_millis(200)))
}

#[derive(serde::Deserialize)]
struct SessionQuery {
    #[serde(rename = "sessionId")]
    session_id: String,
}

async fn message(
    State(state): State<Arc<LegacyState>>,
    Query(query): Query<SessionQuery>,
    Json(request): Json<Value>,
) -> Response {
    let Some(tx) = state
        .sessions
        .lock()
        .unwrap()
        .get(&query.session_id)
        .cloned()
    else {
        return (StatusCode::NOT_FOUND, "unknown session").into_response();
    };
    state.posts.fetch_add(1, Ordering::SeqCst);
    let method = request["method"].as_str().unwrap_or("").to_owned();
    if !method.is_empty() {
        state.methods.lock().unwrap().push(method.clone());
    }
    if let Some(id) = request.get("id").filter(|_| !method.is_empty()) {
        let reply = match reply(&state, &method, &request["params"]) {
            Ok(result) => json!({"jsonrpc": "2.0", "id": id, "result": result}),
            Err((code, msg)) => {
                json!({"jsonrpc": "2.0", "id": id, "error": {"code": code, "message": msg}})
            }
        };
        let _ = tx.send(reply);
    }
    StatusCode::ACCEPTED.into_response()
}

fn reply(state: &LegacyState, method: &str, params: &Value) -> Result<Value, (i64, String)> {
    match method {
        "initialize" => Ok(json!({
            "protocolVersion": state.protocol_version,
            "capabilities": {"tools": {"listChanged": true}},
            "serverInfo": {"name": "legacy-sse", "version": "0.9.0"},
            "instructions": "A legacy SSE server.",
        })),
        "ping" => Ok(json!({})),
        "tools/list" => Ok(json!({"tools": [{
            "name": "echo",
            "description": "Echoes `text`.",
            "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}},
        }]})),
        "tools/call" if params["name"] == "echo" => Ok(json!({
            "content": [{"type": "text", "text": format!("echo: {}", params["arguments"]["text"].as_str().unwrap_or(""))}],
            "isError": false,
        })),
        "tools/call" => Err((-32602, "unknown tool".into())),
        other => Err((-32601, format!("method not found: {other}"))),
    }
}
