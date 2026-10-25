//! The harness leg of the relay API (protocol.md section 8.2): register, token refresh, the
//! command stream (SSE), event batches, heartbeat, pairing windows and the session's end.
//!
//! Nothing here logs a token, an envelope or a body.

use std::{
    path::Path,
    sync::{
        Arc, RwLock,
        atomic::{AtomicI64, Ordering},
    },
    time::Duration,
};

use futures::StreamExt as _;
use serde::Deserialize;
use serde_json::{Value, json};

/// One HTTP call's bound (the command stream has none: it lives for hours).
pub const CALL_TIMEOUT: Duration = Duration::from_secs(30);

/// A relay answer that isn't a success: the status, the `code` member and the message.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RelayError {
    /// 0: the request never got an answer (network, timeout).
    pub status: u16,
    pub code: Option<String>,
    pub message: String,
    /// `expected_seq` / `accepted_through` / `reason` members, when present.
    pub body: Box<Value>,
}

impl std::fmt::Display for RelayError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match (self.status, &self.code) {
            (0, _) => write!(f, "{}", self.message),
            (s, Some(c)) => write!(f, "{s} {c}: {}", self.message),
            (s, None) => write!(f, "{s}: {}", self.message),
        }
    }
}

impl RelayError {
    fn network(e: impl std::fmt::Display) -> Self {
        Self {
            status: 0,
            code: None,
            message: amp_mcp::redact_urls(&e.to_string()),
            body: Box::new(Value::Null),
        }
    }

    /// An answer that isn't the relay's (see [`NOT_RELAY`]).
    fn not_relay(status: u16) -> Self {
        Self {
            status,
            code: Some(NOT_RELAY.to_owned()),
            message: "the answer isn't from AMP's remote control relay".to_owned(),
            body: Box::new(Value::Null),
        }
    }

    pub fn is_network(&self) -> bool {
        self.status == 0
    }

    /// The session is over (`410 session_ended`); `reason` from the body.
    pub fn ended_reason(&self) -> Option<String> {
        (self.status == 410).then(|| {
            self.body
                .get("reason")
                .and_then(Value::as_str)
                .unwrap_or("ended")
                .to_owned()
        })
    }

    /// What to tell the person when registration fails (decision section 9).
    pub fn register_message(&self) -> String {
        match (self.status, self.code.as_deref()) {
            (404, _) | (_, Some(NOT_RELAY)) => {
                "Remote control isn't available on this AMP server.".to_owned()
            }
            (403, Some("remote_disabled")) => {
                "Remote control is turned off for this project's organization (an org admin can \
                 turn it on in the org settings)."
                    .to_owned()
            }
            (403, Some("token_not_allowed")) => {
                "AMP refused an API token for remote control; run `amp-harness login`.".to_owned()
            }
            (403, Some("agent_not_allowed")) => {
                "Remote control is for people: this login is an agent account.".to_owned()
            }
            (401, _) => "AMP did not accept the login; run `amp-harness login`.".to_owned(),
            (429, Some("too_many_sessions")) => {
                "You already have 5 remote sessions; turn one off first.".to_owned()
            }
            (429, _) => "Too many remote control requests; try again in a minute.".to_owned(),
            (400, Some("unsupported_version")) => {
                "This AMP server speaks another remote control version; update amp-harness."
                    .to_owned()
            }
            (503, Some("audit_unavailable")) => {
                "AMP couldn't record the audit entry, so it refused; try again shortly.".to_owned()
            }
            // No relay behind the proxy: amp-relay is its own service (#2387), and with it
            // scaled down or not deployed Envoy answers 502/503 without a relay `code`.
            (502..=504, None) => {
                "Remote control is unavailable: AMP's relay service isn't running \
                 right now. Try again later, or ask your AMP admin."
                    .to_owned()
            }
            (0, _) => format!("AMP could not be reached: {}", self.message),
            _ => format!("AMP refused remote control: {self}"),
        }
    }
}

/// The `code` the client gives an answer that isn't the relay's (#2390): a redirect, or a success
/// whose body isn't a JSON object, such as the web app's HTML when the relay is off.
pub const NOT_RELAY: &str = "not_relay";

/// Whether an answer is the relay's: never a redirect (the client doesn't follow them), and a
/// success always carries a JSON object.
fn is_relay_answer(status: u16, value: &Value) -> bool {
    !(300..400).contains(&status) && (!(200..300).contains(&status) || value.is_object())
}

/// `POST /relay/v1/sessions`'s answer (201).
#[derive(Debug, Clone, Deserialize)]
pub struct Registered {
    pub session_id: String,
    pub protocol_version: i64,
    pub connect_token: String,
    pub connect_token_expires_at_ms: i64,
    pub started_at_ms: i64,
    pub expires_at_ms: i64,
    pub idle_timeout_s: i64,
    #[serde(default = "default_pin_days")]
    pub device_pin_max_days: i64,
    #[serde(default = "default_interval")]
    pub heartbeat_interval_s: i64,
    #[serde(default)]
    pub session_url: Option<String>,
}

fn default_pin_days() -> i64 {
    30
}

fn default_interval() -> i64 {
    20
}

/// `POST …/token`'s answer.
#[derive(Debug, Clone, Deserialize)]
pub struct Refreshed {
    pub connect_token: String,
    pub connect_token_expires_at_ms: i64,
}

/// `POST …/events`' answer.
#[derive(Debug, Clone, Copy, Deserialize)]
pub struct Accepted {
    pub accepted_through: i64,
}

/// One frame of the command stream.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Frame {
    Command(String),
    PairRequest(String),
    Viewers(u32),
    DeviceRevoked {
        device_id: Option<String>,
        all: bool,
    },
    End(String),
    /// An event the harness doesn't know (ignored).
    Other(String),
}

/// The relay client for one AMP server.
#[derive(Clone)]
pub struct RelayClient {
    http: reqwest::Client,
    base: String,
}

impl std::fmt::Debug for RelayClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RelayClient")
            .field("base", &self.base)
            .finish()
    }
}

impl RelayClient {
    /// `base` is the AMP server's origin (`https://amp.example`); `ca_file` as for MCP. The client
    /// keeps its connections open (#2380): the link posts an event batch about every 100 ms while
    /// a reply streams, and a new connection per batch cost a DNS lookup and TLS handshake each
    /// time (5 s a batch on a Mac resolving `amp.local`).
    ///
    /// It never follows a redirect (#2390): every `/relay/` call carries the OAuth or connect token,
    /// and a redirect means the answer isn't the relay's (with the relay off, `/relay/` reaches the
    /// web app, which sends a login redirect). Such an answer reads as "isn't available".
    pub fn new(base: &str, ca_file: Option<&Path>) -> Result<Self, String> {
        let mut builder = reqwest::Client::builder()
            .pool_max_idle_per_host(usize::MAX)
            .pool_idle_timeout(amp_mcp::KEEPALIVE_IDLE)
            .redirect(reqwest::redirect::Policy::none());
        if let Some(path) = ca_file {
            builder =
                builder.tls_certs_merge(amp_mcp::load_ca_file(path).map_err(|e| e.to_string())?);
        }
        let http = builder.build().map_err(|e| e.to_string())?;
        Ok(Self {
            http,
            base: base.trim_end_matches('/').to_owned(),
        })
    }

    fn url(&self, path: &str) -> String {
        format!("{}/relay/v1/sessions{path}", self.base)
    }

    async fn call(
        &self,
        method: reqwest::Method,
        path: &str,
        bearer: &str,
        body: Option<Value>,
    ) -> Result<Value, RelayError> {
        let mut req = self
            .http
            .request(method, self.url(path))
            .bearer_auth(bearer);
        if let Some(body) = body {
            req = req
                .header(
                    reqwest::header::CONTENT_TYPE,
                    "application/json; charset=utf-8",
                )
                .body(body.to_string());
        }
        let send = async {
            let resp = req
                .send()
                .await
                .map_err(|e| RelayError::network(e.without_url()))?;
            let status = resp.status().as_u16();
            let text = resp
                .text()
                .await
                .map_err(|e| RelayError::network(e.without_url()))?;
            let value: Value = serde_json::from_str(&text).unwrap_or(Value::Null);
            if !is_relay_answer(status, &value) {
                return Err(RelayError::not_relay(status));
            }
            if (200..300).contains(&status) {
                return Ok(value);
            }
            Err(RelayError {
                status,
                code: value.get("code").and_then(Value::as_str).map(str::to_owned),
                message: value
                    .get("error")
                    .and_then(Value::as_str)
                    .unwrap_or("no error message")
                    .chars()
                    .take(200)
                    .collect(),
                body: Box::new(value),
            })
        };
        tokio::time::timeout(CALL_TIMEOUT, send)
            .await
            .unwrap_or_else(|_| Err(RelayError::network("the request timed out")))
    }

    fn parse<T: serde::de::DeserializeOwned>(v: Value) -> Result<T, RelayError> {
        serde_json::from_value(v).map_err(|e| RelayError {
            status: 0,
            code: Some("malformed".into()),
            message: format!("AMP's answer has an unexpected shape: {e}"),
            body: Box::new(Value::Null),
        })
    }

    #[allow(clippy::too_many_arguments)]
    pub async fn register(
        &self,
        oauth_token: &str,
        harness_id: &str,
        harness_pub: &str,
        host: &str,
        os: &str,
        version: &str,
        project_id: Option<i64>,
    ) -> Result<Registered, RelayError> {
        let body = json!({
            "protocol_versions": [1],
            "harness_id": harness_id, "harness_pub": harness_pub,
            "host": host, "os": os, "harness_version": version,
            "project_id": project_id,
        });
        let v = self
            .call(reqwest::Method::POST, "", oauth_token, Some(body))
            .await?;
        Self::parse(v)
    }

    /// Renews the connect token: the OAuth login plus a proof signed by the harness key the
    /// session was registered with (protocol 8.2, #2390).
    pub async fn refresh(
        &self,
        session_id: &str,
        oauth_token: &str,
        key: &crate::remote::crypto::KeyPair,
    ) -> Result<Refreshed, RelayError> {
        let body = crate::remote::token::refresh_body(key, session_id, now_ms());
        let v = self
            .call(
                reqwest::Method::POST,
                &format!("/{session_id}/token"),
                oauth_token,
                Some(body),
            )
            .await?;
        Self::parse(v)
    }

    /// Posts envelopes (compact JSON texts) as one batch.
    pub async fn post_events(
        &self,
        session_id: &str,
        token: &str,
        envelopes: &[String],
    ) -> Result<Accepted, RelayError> {
        // The envelopes are already JSON: splice them in without re-serializing.
        let body = format!("{{\"events\":[{}]}}", envelopes.join(","));
        let value: Value = serde_json::from_str(&body).map_err(RelayError::network)?;
        let v = self
            .call(
                reqwest::Method::POST,
                &format!("/{session_id}/events"),
                token,
                Some(value),
            )
            .await?;
        Self::parse(v)
    }

    pub async fn heartbeat(&self, session_id: &str, token: &str) -> Result<Value, RelayError> {
        self.call(
            reqwest::Method::POST,
            &format!("/{session_id}/heartbeat"),
            token,
            Some(json!({})),
        )
        .await
    }

    pub async fn open_pairing(
        &self,
        session_id: &str,
        token: &str,
        pairing_id: &str,
        expires_at_ms: i64,
    ) -> Result<(), RelayError> {
        self.call(
            reqwest::Method::POST,
            &format!("/{session_id}/pairings"),
            token,
            Some(json!({"pairing_id": pairing_id, "expires_at_ms": expires_at_ms})),
        )
        .await
        .map(|_| ())
    }

    pub async fn end(&self, session_id: &str, token: &str, reason: &str) -> Result<(), RelayError> {
        self.call(
            reqwest::Method::DELETE,
            &format!("/{session_id}"),
            token,
            Some(json!({"reason": reason})),
        )
        .await
        .map(|_| ())
    }

    /// Opens the command stream. Frames go to `on_frame`; `last_byte_ms` is updated on every
    /// chunk (keepalive comments included), so the caller can spot a silent stream. Returns when
    /// the stream ends (`Ok`) or fails to open (`Err`).
    pub async fn command_stream(
        &self,
        session_id: &str,
        token: &str,
        last_byte_ms: Arc<AtomicI64>,
        on_open: impl FnOnce(),
        mut on_frame: impl FnMut(Frame) -> bool,
    ) -> Result<(), RelayError> {
        let open = self
            .http
            .get(self.url(&format!("/{session_id}/commands")))
            .bearer_auth(token)
            .header(reqwest::header::ACCEPT, "text/event-stream")
            .send();
        let resp = tokio::time::timeout(CALL_TIMEOUT, open)
            .await
            .map_err(|_| RelayError::network("opening the command stream timed out"))?
            .map_err(|e| RelayError::network(e.without_url()))?;
        let status = resp.status().as_u16();
        let event_stream = resp
            .headers()
            .get(reqwest::header::CONTENT_TYPE)
            .and_then(|v| v.to_str().ok())
            .is_some_and(|v| v.starts_with("text/event-stream"));
        if (300..400).contains(&status) || ((200..300).contains(&status) && !event_stream) {
            return Err(RelayError::not_relay(status));
        }
        if !(200..300).contains(&status) {
            let text = resp.text().await.unwrap_or_default();
            let value: Value = serde_json::from_str(&text).unwrap_or(Value::Null);
            return Err(RelayError {
                status,
                code: value.get("code").and_then(Value::as_str).map(str::to_owned),
                message: value
                    .get("error")
                    .and_then(Value::as_str)
                    .unwrap_or("the command stream was refused")
                    .chars()
                    .take(200)
                    .collect(),
                body: Box::new(value),
            });
        }
        last_byte_ms.store(now_ms(), Ordering::Relaxed);
        on_open();
        let marker = Arc::clone(&last_byte_ms);
        let bytes = resp.bytes_stream().map(move |chunk| {
            marker.store(now_ms(), Ordering::Relaxed);
            chunk
        });
        let mut events = Box::pin(sse_stream::SseStream::from_bytes_stream(bytes));
        while let Some(event) = events.next().await {
            let Ok(event) = event else { break };
            let data = event.data.unwrap_or_default();
            let frame = match event.event.as_deref() {
                Some("command") => Frame::Command(data),
                Some("pair_request") => Frame::PairRequest(data),
                Some("viewers") => Frame::Viewers(
                    serde_json::from_str::<Value>(&data)
                        .ok()
                        .and_then(|v| v.get("count").and_then(Value::as_u64))
                        .unwrap_or(0) as u32,
                ),
                Some("device_revoked") => {
                    let v: Value = serde_json::from_str(&data).unwrap_or(Value::Null);
                    Frame::DeviceRevoked {
                        device_id: v
                            .get("device_id")
                            .and_then(Value::as_str)
                            .map(str::to_owned),
                        all: v.get("all").and_then(Value::as_bool).unwrap_or(false),
                    }
                }
                Some("end") => Frame::End(
                    serde_json::from_str::<Value>(&data)
                        .ok()
                        .and_then(|v| v.get("reason").and_then(Value::as_str).map(str::to_owned))
                        .unwrap_or_else(|| "ended".to_owned()),
                ),
                other => Frame::Other(other.unwrap_or("message").to_owned()),
            };
            let end = matches!(frame, Frame::End(_));
            if !on_frame(frame) || end {
                break;
            }
        }
        Ok(())
    }
}

/// Unix milliseconds now.
pub fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// The connect token, shared by the link's tasks (never printed).
#[derive(Clone, Default)]
pub struct ConnectToken(Arc<RwLock<String>>);

impl std::fmt::Debug for ConnectToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("ConnectToken(<redacted>)")
    }
}

impl ConnectToken {
    pub fn set(&self, token: &str) {
        *self.0.write().unwrap_or_else(|e| e.into_inner()) = token.to_owned();
    }

    pub fn get(&self) -> String {
        self.0.read().unwrap_or_else(|e| e.into_inner()).clone()
    }
}

#[cfg(test)]
mod register_message_tests {
    use super::*;

    fn err(status: u16, code: Option<&str>) -> RelayError {
        RelayError {
            status,
            code: code.map(str::to_owned),
            message: "x".into(),
            body: Box::new(Value::Null),
        }
    }

    #[test]
    fn non_relay_answers_read_as_not_available() {
        assert!(!is_relay_answer(302, &json!({})));
        assert!(!is_relay_answer(200, &Value::Null)); // HTML
        assert!(!is_relay_answer(200, &json!([1])));
        assert!(is_relay_answer(200, &json!({"accepted_through": 1})));
        assert!(is_relay_answer(503, &Value::Null)); // Envoy without an upstream
        assert!(
            RelayError::not_relay(302)
                .register_message()
                .contains("isn't available on this AMP")
        );
    }

    #[test]
    fn relay_service_down_reads_as_unavailable() {
        for s in [502, 503, 504] {
            assert!(
                err(s, None)
                    .register_message()
                    .starts_with("Remote control is unavailable"),
                "{s}"
            );
        }
        assert!(
            err(503, Some("audit_unavailable"))
                .register_message()
                .contains("audit entry")
        );
        assert!(
            err(404, None)
                .register_message()
                .contains("isn't available")
        );
    }

    /// Live probe of what a person is told when registering against a real AMP (#2387's
    /// kill-switch check): `AMP_RELAY_PROBE_URL=https://amp.local AMP_RELAY_PROBE_CA=<pem>
    /// cargo test -p amp-core live_register_probe -- --ignored --nocapture`.
    #[tokio::test]
    #[ignore]
    async fn live_register_probe() {
        let url = std::env::var("AMP_RELAY_PROBE_URL").expect("AMP_RELAY_PROBE_URL");
        let ca = std::env::var("AMP_RELAY_PROBE_CA").ok();
        let c = RelayClient::new(&url, ca.as_deref().map(Path::new)).unwrap();
        let e = c
            .register("not-a-real-token", "h", "p", "host", "os", "0", None)
            .await
            .unwrap_err();
        println!("PROBE status={} code={:?}", e.status, e.code);
        println!("PROBE message: {}", e.register_message());
    }
}
