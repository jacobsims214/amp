//! An in-process mock of the relay's harness leg (protocol.md section 8.2), for the remote
//! link's end-to-end tests (feature `mock-relay`). It follows the spec's checks that matter to
//! the harness: OAuth vs connect-token auth, `amp_` tokens refused, `fp(harness_pub) ==
//! harness_id`, every event envelope verified with the registered key (section 5.7 steps 1-7)
//! and contiguous seqs (`409 seq_gap`), `410 session_ended` after the end. A test plays the
//! browser by pushing frames (`command`, `pair_request`, `viewers`, `device_revoked`, `end`) onto
//! the command stream.

#![allow(
    clippy::result_large_err,
    reason = "axum responses as errors keep the mock short"
)]

use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::Duration,
};

use axum::{
    Router,
    body::{Body, Bytes},
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::{delete, get, post},
};
use serde_json::{Value, json};
use tokio::sync::{Notify, mpsc};

use super::crypto;
use super::envelope;
use super::relay::now_ms;

/// What the mock answers with.
#[derive(Debug, Clone)]
pub struct MockOptions {
    /// The OAuth access token the mock accepts for register and refresh.
    pub oauth_token: String,
    pub idle_timeout_s: i64,
    pub lifetime_ms: i64,
    pub device_pin_max_days: i64,
    pub connect_token_ttl_ms: i64,
    pub heartbeat_interval_s: i64,
    /// Answer every register with this status and code (a disabled org, an old AMP).
    pub refuse_register: Option<(u16, &'static str)>,
}

impl Default for MockOptions {
    fn default() -> Self {
        Self {
            oauth_token: "oauth-access-token-for-tests".into(),
            idle_timeout_s: 1800,
            lifetime_ms: 12 * 3600 * 1000,
            device_pin_max_days: 30,
            connect_token_ttl_ms: 15 * 60 * 1000,
            heartbeat_interval_s: 20,
            refuse_register: None,
        }
    }
}

#[derive(Debug, Default)]
struct Session {
    harness_id: String,
    harness_pub: Vec<u8>,
    tokens: Vec<String>,
    events: Vec<Value>,
    accepted_through: i64,
    stream: Option<mpsc::UnboundedSender<Bytes>>,
    streams_opened: u32,
    ended: Option<String>,
    pairing: Option<(String, i64)>,
    heartbeats: u32,
    viewers: u32,
}

#[derive(Debug, Default)]
struct Inner {
    sessions: HashMap<String, Session>,
    last: Option<String>,
    requests: Vec<String>,
}

#[derive(Clone)]
struct App {
    opts: MockOptions,
    inner: Arc<Mutex<Inner>>,
    changed: Arc<Notify>,
}

/// The running mock. Dropping it stops the server.
pub struct MockRelay {
    /// `http://127.0.0.1:<port>` (the "AMP origin").
    pub base: String,
    app: App,
    server: tokio::task::JoinHandle<()>,
}

impl Drop for MockRelay {
    fn drop(&mut self) {
        self.server.abort();
    }
}

fn err(status: u16, code: &str, extra: Value) -> Response {
    let mut body = json!({"error": format!("mock relay: {code}"), "code": code});
    if let (Some(b), Some(e)) = (body.as_object_mut(), extra.as_object()) {
        for (k, v) in e {
            b.insert(k.clone(), v.clone());
        }
    }
    (
        StatusCode::from_u16(status).unwrap_or(StatusCode::BAD_REQUEST),
        [("content-type", "application/json")],
        body.to_string(),
    )
        .into_response()
}

fn ok(status: u16, body: Value) -> Response {
    (
        StatusCode::from_u16(status).unwrap_or(StatusCode::OK),
        [("content-type", "application/json")],
        body.to_string(),
    )
        .into_response()
}

fn bearer(h: &HeaderMap) -> String {
    h.get("authorization")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or_default()
        .to_owned()
}

fn frame(event: &str, data: &str) -> Bytes {
    Bytes::from(format!("event: {event}\ndata: {data}\n\n"))
}

impl App {
    fn lock(&self) -> std::sync::MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn note(&self, what: String) {
        self.lock().requests.push(what);
        self.changed.notify_waiters();
    }

    /// The session if `token` is one of its connect tokens; else the error response.
    fn with_session<T>(
        &self,
        id: &str,
        token: &str,
        f: impl FnOnce(&mut Session) -> T,
    ) -> Result<T, Response> {
        let mut inner = self.lock();
        let Some(s) = inner.sessions.get_mut(id) else {
            return Err(err(404, "not_found", json!({})));
        };
        if !s.tokens.iter().any(|t| t == token) {
            return Err(err(401, "unauthenticated", json!({})));
        }
        if let Some(reason) = &s.ended {
            return Err(err(410, "session_ended", json!({"reason": reason})));
        }
        Ok(f(s))
    }
}

async fn register(State(app): State<App>, headers: HeaderMap, body: Bytes) -> Response {
    app.note("register".into());
    let token = bearer(&headers);
    if token.starts_with("amp_") {
        return err(403, "token_not_allowed", json!({}));
    }
    if token != app.opts.oauth_token {
        return err(401, "unauthenticated", json!({}));
    }
    if let Some((status, code)) = app.opts.refuse_register {
        return err(status, code, json!({}));
    }
    let Ok(v) = serde_json::from_slice::<Value>(&body) else {
        return err(400, "malformed", json!({}));
    };
    let harness_id = v["harness_id"].as_str().unwrap_or_default().to_owned();
    let Some(harness_pub) = v["harness_pub"].as_str().and_then(crypto::b64u_decode) else {
        return err(400, "malformed", json!({}));
    };
    if crypto::fingerprint(&harness_pub) != harness_id {
        return err(400, "malformed", json!({}));
    }
    let session_id = format!("rs_{}", crypto::b64u(&crypto::random_bytes(16)));
    let connect = format!("rct_{}", crypto::b64u(&crypto::random_bytes(32)));
    let now = now_ms();
    {
        let mut inner = app.lock();
        inner.sessions.insert(
            session_id.clone(),
            Session {
                harness_id,
                harness_pub,
                tokens: vec![connect.clone()],
                ..Session::default()
            },
        );
        inner.last = Some(session_id.clone());
    }
    app.changed.notify_waiters();
    ok(
        201,
        json!({
            "session_id": session_id, "protocol_version": 1,
            "connect_token": connect,
            "connect_token_expires_at_ms": now + app.opts.connect_token_ttl_ms,
            "started_at_ms": now, "expires_at_ms": now + app.opts.lifetime_ms,
            "idle_timeout_s": app.opts.idle_timeout_s,
            "device_pin_max_days": app.opts.device_pin_max_days,
            "heartbeat_interval_s": app.opts.heartbeat_interval_s,
            "keepalive_interval_s": 20,
            // A different origin on purpose: the harness must build its link from its own.
            "session_url": format!("https://elsewhere.example/sessions/{session_id}"),
            "limits": {"command_bytes": 65536, "event_bytes": 1048576, "batch_bytes": 2097152,
                       "batch_events": 64, "prompts_per_min": 20, "ops_per_min": 120,
                       "viewers": 3, "pairing_tries": 5},
        }),
    )
}

async fn refresh(State(app): State<App>, Path(id): Path<String>, headers: HeaderMap) -> Response {
    app.note("token".into());
    let token = bearer(&headers);
    if token.starts_with("amp_") {
        return err(403, "token_not_allowed", json!({}));
    }
    if token != app.opts.oauth_token {
        return err(401, "unauthenticated", json!({}));
    }
    let connect = format!("rct_{}", crypto::b64u(&crypto::random_bytes(32)));
    let mut inner = app.lock();
    let Some(s) = inner.sessions.get_mut(&id) else {
        return err(404, "not_found", json!({}));
    };
    if let Some(reason) = &s.ended {
        return err(410, "session_ended", json!({"reason": reason}));
    }
    s.tokens.push(connect.clone());
    ok(
        200,
        json!({"connect_token": connect,
               "connect_token_expires_at_ms": now_ms() + app.opts.connect_token_ttl_ms}),
    )
}

async fn commands(State(app): State<App>, Path(id): Path<String>, headers: HeaderMap) -> Response {
    let token = bearer(&headers);
    let (tx, rx) = mpsc::unbounded_channel::<Bytes>();
    let r = app.with_session(&id, &token, |s| {
        if let Some(old) = s.stream.take() {
            let _ = old.send(frame("end", r#"{"reason":"replaced"}"#));
        }
        let _ = tx.send(frame("viewers", &json!({"count": s.viewers}).to_string()));
        s.stream = Some(tx);
        s.streams_opened += 1;
    });
    if let Err(resp) = r {
        return resp;
    }
    app.note("commands".into());
    let stream = futures::stream::unfold(rx, |mut rx| async move {
        rx.recv()
            .await
            .map(|b| (Ok::<_, std::convert::Infallible>(b), rx))
    });
    Response::builder()
        .status(200)
        .header("content-type", "text/event-stream")
        .header("cache-control", "no-store")
        .body(Body::from_stream(stream))
        .unwrap_or_else(|_| err(500, "internal", json!({})))
}

async fn events(
    State(app): State<App>,
    Path(id): Path<String>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let token = bearer(&headers);
    let Ok(v) = serde_json::from_slice::<Value>(&body) else {
        return err(400, "malformed", json!({}));
    };
    let Some(list) = v.get("events").and_then(Value::as_array).cloned() else {
        return err(400, "malformed", json!({}));
    };
    if list.is_empty() || list.len() > 64 || body.len() > 2 * 1024 * 1024 {
        return err(413, "too_large", json!({}));
    }
    let r = app.with_session(&id, &token, |s| {
        for (i, env) in list.iter().enumerate() {
            let text = env.to_string();
            let seq = env.get("seq").and_then(Value::as_i64).unwrap_or(0);
            if seq <= s.accepted_through {
                continue; // a retry
            }
            if seq != s.accepted_through + 1 {
                return Err(err(
                    409,
                    "seq_gap",
                    json!({"expected_seq": s.accepted_through + 1,
                           "accepted_through": s.accepted_through}),
                ));
            }
            let verdict = envelope::check_event_for_test(
                text.as_bytes(),
                &id,
                &s.harness_id,
                &s.harness_pub,
                s.accepted_through,
            );
            let op = env.get("op").and_then(Value::as_str).unwrap_or_default();
            let device_ok = env.get("device_id").is_some_and(Value::is_null)
                || matches!(op, "ack" | "refused" | "pair_result");
            if verdict != "ok" || !device_ok {
                let code = if verdict == "ok" {
                    "malformed"
                } else {
                    verdict
                };
                return Err(err(
                    400,
                    code,
                    json!({"accepted_through": s.accepted_through, "index": i}),
                ));
            }
            s.events.push(env.clone());
            s.accepted_through = seq;
        }
        Ok(s.accepted_through)
    });
    app.changed.notify_waiters();
    match r {
        Ok(Ok(through)) => ok(200, json!({"accepted_through": through})),
        Ok(Err(resp)) | Err(resp) => resp,
    }
}

async fn heartbeat(State(app): State<App>, Path(id): Path<String>, headers: HeaderMap) -> Response {
    let token = bearer(&headers);
    match app.with_session(&id, &token, |s| {
        s.heartbeats += 1;
        s.viewers
    }) {
        Ok(viewers) => {
            app.changed.notify_waiters();
            ok(200, json!({"viewers": viewers}))
        }
        Err(r) => r,
    }
}

async fn pairings(
    State(app): State<App>,
    Path(id): Path<String>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let token = bearer(&headers);
    let v: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
    let pid = v["pairing_id"].as_str().unwrap_or_default().to_owned();
    let exp = v["expires_at_ms"].as_i64().unwrap_or(0);
    if pid.len() != 22 || exp > now_ms() + 300_000 + 5_000 {
        return err(400, "malformed", json!({}));
    }
    match app.with_session(&id, &token, |s| s.pairing = Some((pid, exp))) {
        Ok(()) => {
            app.changed.notify_waiters();
            ok(201, json!({}))
        }
        Err(r) => r,
    }
}

async fn end(
    State(app): State<App>,
    Path(id): Path<String>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let token = bearer(&headers);
    let v: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
    let reason = v["reason"].as_str().unwrap_or("harness_ended").to_owned();
    match app.with_session(&id, &token, |s| {
        s.ended = Some(reason);
        s.stream = None;
    }) {
        Ok(()) => {
            app.note("delete".into());
            StatusCode::NO_CONTENT.into_response()
        }
        Err(r) => r,
    }
}

impl MockRelay {
    pub async fn start(opts: MockOptions) -> Self {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind the mock relay");
        Self::serve(opts, listener, Router::new())
    }

    /// Serves the relay on `listener` with `extra` routes on the same origin (#2376: a stand-in
    /// AMP with its MCP server and OAuth endpoints next to the relay, for driving the chat).
    pub fn serve(opts: MockOptions, listener: tokio::net::TcpListener, extra: Router) -> Self {
        let app = App {
            opts,
            inner: Default::default(),
            changed: Arc::new(Notify::new()),
        };
        let router = Router::new()
            .route("/relay/v1/sessions", post(register))
            .route("/relay/v1/sessions/{id}/token", post(refresh))
            .route("/relay/v1/sessions/{id}/commands", get(commands))
            .route("/relay/v1/sessions/{id}/events", post(events))
            .route("/relay/v1/sessions/{id}/heartbeat", post(heartbeat))
            .route("/relay/v1/sessions/{id}/pairings", post(pairings))
            .route("/relay/v1/sessions/{id}", delete(end))
            .with_state(app.clone())
            .merge(extra);
        let base = format!("http://{}", listener.local_addr().expect("addr"));
        let server = tokio::spawn(async move {
            let _ = axum::serve(listener, router).await;
        });
        Self { base, app, server }
    }

    /// The latest registered session id.
    pub fn session_id(&self) -> Option<String> {
        self.app.lock().last.clone()
    }

    fn with_last<T>(&self, f: impl FnOnce(&mut Session) -> T) -> Option<T> {
        let mut inner = self.app.lock();
        let id = inner.last.clone()?;
        inner.sessions.get_mut(&id).map(f)
    }

    /// Pushes one frame onto the live command stream. False when no stream is open.
    pub fn push(&self, event: &str, data: &str) -> bool {
        self.with_last(|s| {
            s.stream
                .as_ref()
                .is_some_and(|tx| tx.send(frame(event, data)).is_ok())
        })
        .unwrap_or(false)
    }

    /// A browser command (compact JSON, as the relay forwards it).
    pub fn send_command(&self, envelope: &str) -> bool {
        let compact: Value = serde_json::from_str(envelope).unwrap_or(Value::Null);
        self.push("command", &compact.to_string())
    }

    /// A pair request, with the relay's verified info added (section 7.4 step 6).
    pub fn send_pair_request(&self, mut message: Value, user_id: i64, email: &str) -> bool {
        message["relay"] =
            json!({"user_id": user_id, "email": email, "user_agent": "Mozilla/5.0 (iPhone)"});
        self.push("pair_request", &message.to_string())
    }

    pub fn set_viewers(&self, n: u32) -> bool {
        self.with_last(|s| s.viewers = n);
        self.push("viewers", &json!({"count": n}).to_string())
    }

    /// Cuts the command stream (a network drop).
    pub fn drop_stream(&self) {
        self.with_last(|s| s.stream = None);
    }

    /// Ends the session from the relay's side (Disconnect, revoke-all, policy off).
    pub fn end_session(&self, reason: &str) {
        self.with_last(|s| {
            if let Some(tx) = s.stream.take() {
                let _ = tx.send(frame("end", &json!({"reason": reason}).to_string()));
            }
            s.ended = Some(reason.to_owned());
        });
    }

    /// Every accepted event envelope, in seq order.
    pub fn events(&self) -> Vec<Value> {
        self.with_last(|s| s.events.clone()).unwrap_or_default()
    }

    pub fn ended(&self) -> Option<String> {
        self.with_last(|s| s.ended.clone()).flatten()
    }

    pub fn streams_opened(&self) -> u32 {
        self.with_last(|s| s.streams_opened).unwrap_or(0)
    }

    pub fn stream_open(&self) -> bool {
        self.with_last(|s| s.stream.is_some()).unwrap_or(false)
    }

    pub fn heartbeats(&self) -> u32 {
        self.with_last(|s| s.heartbeats).unwrap_or(0)
    }

    /// The open pairing window `(pairing_id, expires_at_ms)`.
    pub fn pairing(&self) -> Option<(String, i64)> {
        self.with_last(|s| s.pairing.clone()).flatten()
    }

    /// The requests seen, by kind (`register`, `token`, `commands`, `delete`).
    pub fn requests(&self) -> Vec<String> {
        self.app.lock().requests.clone()
    }

    /// Waits until `pred` holds for the accepted events (or the state), up to `timeout`.
    pub async fn wait_until(
        &self,
        timeout: Duration,
        mut pred: impl FnMut(&MockRelay) -> bool,
    ) -> bool {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            if pred(self) {
                return true;
            }
            let notified = self.app.changed.notified();
            if tokio::time::timeout_at(deadline, async {
                tokio::select! {
                    _ = notified => {}
                    _ = tokio::time::sleep(Duration::from_millis(50)) => {}
                }
            })
            .await
            .is_err()
            {
                return pred(self);
            }
        }
    }

    /// Waits for an accepted event matching `pred`; returns it.
    pub async fn wait_event(
        &self,
        timeout: Duration,
        pred: impl Fn(&Value) -> bool,
    ) -> Option<Value> {
        let mut found = None;
        self.wait_until(timeout, |m| {
            found = m.events().into_iter().find(|e| pred(e));
            found.is_some()
        })
        .await;
        found
    }
}

/// The body of a `chat_event` envelope whose `type` is `ty`.
pub fn is_chat(env: &Value, ty: &str) -> bool {
    env["op"] == "chat_event" && env["body"]["type"] == ty
}
