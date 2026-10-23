//! A mock model provider (#2163): an OpenAI-compatible endpoint that serves `GET /v1/models` and a
//! streamed `POST /v1/chat/completions`, accepting only one key (and, optionally, one required
//! header). It records the credentials it was sent, so tests can prove which key a run used.

use std::sync::{Arc, Mutex};

use axum::{
    Json, Router,
    extract::State,
    http::{HeaderMap, StatusCode, header},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use serde_json::{Value, json};
use tokio::net::TcpListener;

/// One request: `(path, authorization, required header value)`.
pub type Seen = (String, Option<String>, Option<String>);

/// One request's target (path and query) and headers, lowercase names (#2277).
pub type Request = (String, std::collections::BTreeMap<String, String>);

#[derive(Clone, Default)]
pub struct ProviderState {
    /// The bearer key it accepts (`None`: any, or none).
    pub key: Option<String>,
    /// A header it requires: (name, value).
    pub header: Option<(String, String)>,
    pub models: Vec<String>,
    /// The reply text of every chat completion.
    pub reply: String,
    /// `(path, authorization, required header value)` of every request.
    pub seen: Arc<Mutex<Vec<Seen>>>,
    /// The `model` of every chat completion request (#2275: which model a role used).
    pub requested_models: Arc<Mutex<Vec<String>>>,
    /// #2277: the key is expected in this header instead of `Authorization: Bearer`.
    pub key_header: Option<String>,
    /// #2277: every request's target (path and query) and headers (lowercase names).
    pub requests: Arc<Mutex<Vec<Request>>>,
}

pub struct MockProvider {
    /// The base URL (`http://127.0.0.1:N/v1`).
    pub url: String,
    pub state: ProviderState,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for MockProvider {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl MockProvider {
    pub fn seen(&self) -> Vec<Seen> {
        self.state.seen.lock().unwrap().clone()
    }

    /// The `model` of every chat completion it served.
    pub fn requested_models(&self) -> Vec<String> {
        self.state.requested_models.lock().unwrap().clone()
    }

    /// Every request's target and headers (#2277).
    pub fn requests(&self) -> Vec<Request> {
        self.state.requests.lock().unwrap().clone()
    }
}

fn record(s: &ProviderState, uri: &axum::extract::OriginalUri, headers: &HeaderMap) {
    let map = headers
        .iter()
        .map(|(k, v)| (k.as_str().to_owned(), v.to_str().unwrap_or("").to_owned()))
        .collect();
    s.requests.lock().unwrap().push((uri.0.to_string(), map));
}

fn authorized(s: &ProviderState, path: &str, headers: &HeaderMap) -> bool {
    let auth = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .map(str::to_owned);
    let extra = s.header.as_ref().and_then(|(name, _)| {
        headers
            .get(name.as_str())
            .and_then(|v| v.to_str().ok())
            .map(str::to_owned)
    });
    s.seen
        .lock()
        .unwrap()
        .push((path.to_owned(), auth.clone(), extra.clone()));
    let key_ok = match &s.key_header {
        Some(name) => s.key.as_ref().is_none_or(|k| {
            headers.get(name.as_str()).and_then(|v| v.to_str().ok()) == Some(k.as_str())
        }),
        None => s
            .key
            .as_ref()
            .is_none_or(|k| auth.as_deref() == Some(format!("Bearer {k}").as_str())),
    };
    let header_ok = s
        .header
        .as_ref()
        .is_none_or(|(_, v)| extra.as_deref() == Some(v.as_str()));
    key_ok && header_ok
}

async fn models(
    State(s): State<ProviderState>,
    uri: axum::extract::OriginalUri,
    headers: HeaderMap,
) -> Response {
    record(&s, &uri, &headers);
    if !authorized(&s, "/v1/models", &headers) {
        return (StatusCode::UNAUTHORIZED, Json(json!({"error": "bad key"}))).into_response();
    }
    let data: Vec<Value> = s
        .models
        .iter()
        .map(|id| json!({"id": id, "object": "model"}))
        .collect();
    Json(json!({"object": "list", "data": data})).into_response()
}

async fn completions(
    State(s): State<ProviderState>,
    uri: axum::extract::OriginalUri,
    headers: HeaderMap,
    body: axum::body::Bytes,
) -> Response {
    record(&s, &uri, &headers);
    if !authorized(&s, "/v1/chat/completions", &headers) {
        return (StatusCode::UNAUTHORIZED, Json(json!({"error": "bad key"}))).into_response();
    }
    if let Some(model) = serde_json::from_slice::<Value>(&body)
        .ok()
        .and_then(|v| v["model"].as_str().map(str::to_owned))
    {
        s.requested_models.lock().unwrap().push(model);
    }
    let chunk = |delta: Value, finish: Value| {
        json!({
            "id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 0,
            "model": "m-1",
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
        })
    };
    let mut body = String::new();
    for event in [
        chunk(
            json!({"role": "assistant", "content": s.reply}),
            Value::Null,
        ),
        chunk(json!({}), json!("stop")),
        json!({
            "id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 0,
            "model": "m-1", "choices": [],
            "usage": {"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13},
        }),
    ] {
        body.push_str(&format!("data: {event}\n\n"));
    }
    body.push_str("data: [DONE]\n\n");
    ([(header::CONTENT_TYPE, "text/event-stream")], body).into_response()
}

pub async fn spawn_provider(state: ProviderState) -> MockProvider {
    let app = Router::new()
        .route("/v1/models", get(models))
        .route("/v1/chat/completions", post(completions))
        .with_state(state.clone());
    let tcp = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = tcp.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(tcp, app).await;
    });
    MockProvider {
        url: format!("http://{addr}/v1"),
        state,
        task,
    }
}

/// Every file under `dir` that contains `needle` (keys must never land in a file).
pub fn files_containing(dir: &std::path::Path, needle: &str) -> Vec<std::path::PathBuf> {
    let mut out = Vec::new();
    let mut stack = vec![dir.to_path_buf()];
    while let Some(d) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&d) else {
            continue;
        };
        for e in entries.flatten() {
            let p = e.path();
            if p.is_dir() {
                stack.push(p);
            } else if std::fs::read(&p)
                .ok()
                .is_some_and(|b| String::from_utf8_lossy(&b).contains(needle))
            {
                out.push(p);
            }
        }
    }
    out
}

/// A recording HTTP(S) proxy that refuses everything (#2275). Point `HTTPS_PROXY`/`HTTP_PROXY`
/// (and `ALL_PROXY`) at it with `NO_PROXY=127.0.0.1,localhost`: every request the harness would
/// send off this machine (to openrouter.ai, say) arrives here instead and is recorded by its first
/// line (`CONNECT openrouter.ai:443 HTTP/1.1`), so a test can assert there was none.
pub struct ProxyGuard {
    pub url: String,
    seen: Arc<Mutex<Vec<String>>>,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for ProxyGuard {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl ProxyGuard {
    pub fn seen(&self) -> Vec<String> {
        self.seen.lock().unwrap().clone()
    }

    /// The variables that send every off-machine request through the guard.
    pub fn env(&self) -> Vec<(&'static str, String)> {
        let no_proxy = "127.0.0.1,localhost,::1".to_owned();
        vec![
            ("HTTPS_PROXY", self.url.clone()),
            ("https_proxy", self.url.clone()),
            ("HTTP_PROXY", self.url.clone()),
            ("http_proxy", self.url.clone()),
            ("ALL_PROXY", self.url.clone()),
            ("all_proxy", self.url.clone()),
            ("NO_PROXY", no_proxy.clone()),
            ("no_proxy", no_proxy),
        ]
    }
}

pub async fn spawn_proxy_guard() -> ProxyGuard {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let tcp = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = tcp.local_addr().unwrap();
    let seen: Arc<Mutex<Vec<String>>> = Arc::default();
    let record = Arc::clone(&seen);
    let task = tokio::spawn(async move {
        while let Ok((mut sock, _)) = tcp.accept().await {
            let record = Arc::clone(&record);
            tokio::spawn(async move {
                let mut buf = vec![0u8; 4096];
                let n = sock.read(&mut buf).await.unwrap_or(0);
                let text = String::from_utf8_lossy(&buf[..n]);
                let first = text.lines().next().unwrap_or("").to_owned();
                record.lock().unwrap().push(first);
                let _ = sock
                    .write_all(
                        b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                    )
                    .await;
            });
        }
    });
    ProxyGuard {
        url: format!("http://{addr}"),
        seen,
        task,
    }
}
