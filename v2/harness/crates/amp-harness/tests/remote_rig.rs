//! #2376: a stand-in AMP for driving `/remote` by hand or from a pty script. It does nothing
//! under `cargo test` unless `AMP_REMOTE_RIG=1` is set.
//!
//! On one loopback origin it serves:
//! - an OAuth authorization server (`amp-harness login --no-browser` works against it; every
//!   token it issues is the one access token the relay accepts),
//! - the mock AMP MCP server at `/mcp` (tests/common), behind that bearer,
//! - #2373's in-process mock relay at `/relay/v1/...`.
//!
//! It then plays the browser, one command per stdin line, answering on stdout:
//! - `pair <link>`: pairs "Safari on iPhone" (jsims@example.test) with the link the chat showed;
//!   prints `CODE <nnnn-nnnn>` (what the browser would show), then `PAIRED accepted|rejected`
//!   once the harness answered (or `PAIRED timeout`).
//! - `prompt <text>`: a signed prompt; prints `ACK started|queued|...`.
//! - `stop`, `detach`: signed commands; print `ACK ...`.
//! - `viewers <n>`: the relay's viewer count. `drop`: cuts the command stream (reconnect).
//! - `revoke`: AMP revokes the paired device. `end <reason>`: the relay ends the session
//!   (`disconnected`, `revoked`, `policy_off`).
//! - `slow <ms>`: every MCP call waits that long (a busy run, for the queue); `slow 0` undoes it.
//! - `events`: the accepted event ops, one per line.
//! - `quit`.
//!
//! `AMP_REMOTE_RIG_REFUSE=404|403` answers every register with 404, or 403 `remote_disabled`;
//! `AMP_REMOTE_RIG_IDLE=<s>` sets an idle timeout (by default there is none, #2424).
//!
//! First line out: `URL http://127.0.0.1:<port>/mcp`.

mod common;

use std::{
    collections::{HashMap, HashSet},
    io::Write,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use amp_core::remote::{
    crypto::{self, KeyPair},
    envelope,
    mock_relay::{MockOptions, MockRelay},
    pairing,
    relay::now_ms,
};
use axum::{
    Form, Json, Router,
    extract::{Query, Request, State},
    http::{HeaderValue, StatusCode, header},
    middleware::{self, Next},
    response::{IntoResponse, Redirect, Response},
    routing::{get, post},
};
use serde_json::{Value, json};
use tokio::io::AsyncBufReadExt;

const TOKEN: &str = "rig-oauth-access-token-0123456789";
/// `slow <ms>`: every MCP call waits this long (keeps a run busy for the queue).
static SLOW_MS: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
const USER: i64 = 7;

#[derive(Default)]
struct AsState {
    base: String,
    seq: AtomicUsize,
    clients: Mutex<HashMap<String, Vec<String>>>,
    codes: Mutex<HashSet<String>>,
}

async fn gate(State(s): State<Arc<AsState>>, req: Request, next: Next) -> Response {
    let path = req.uri().path().to_owned();
    if !path.starts_with("/mcp") {
        return next.run(req).await;
    }
    let ok = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v == format!("Bearer {TOKEN}"));
    if ok {
        let slow = SLOW_MS.load(Ordering::SeqCst);
        if slow > 0 && req.method() == axum::http::Method::POST {
            tokio::time::sleep(Duration::from_millis(slow)).await;
        }
        return next.run(req).await;
    }
    let mut r = (StatusCode::UNAUTHORIZED, "unauthorized").into_response();
    let challenge = format!(
        r#"Bearer resource_metadata="{}/.well-known/oauth-protected-resource/mcp", scope="read""#,
        s.base
    );
    r.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_str(&challenge).unwrap(),
    );
    r
}

async fn prm(State(s): State<Arc<AsState>>) -> Json<Value> {
    Json(json!({
        "resource": format!("{}/mcp", s.base),
        "authorization_servers": [s.base],
        "scopes_supported": ["read"],
        "bearer_methods_supported": ["header"],
    }))
}

async fn as_metadata(State(s): State<Arc<AsState>>) -> Json<Value> {
    Json(json!({
        "issuer": s.base,
        "authorization_endpoint": format!("{}/authorize", s.base),
        "token_endpoint": format!("{}/token", s.base),
        "registration_endpoint": format!("{}/register", s.base),
        "response_types_supported": ["code"],
        "grant_types_supported": ["authorization_code", "refresh_token"],
        "code_challenge_methods_supported": ["S256"],
        "token_endpoint_auth_methods_supported": ["none"],
        "scopes_supported": ["read"],
    }))
}

async fn register(State(s): State<Arc<AsState>>, Json(body): Json<Value>) -> Response {
    let uris: Vec<String> = body["redirect_uris"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(str::to_owned))
                .collect()
        })
        .unwrap_or_default();
    let id = format!("client-{}", s.seq.fetch_add(1, Ordering::SeqCst));
    s.clients.lock().unwrap().insert(id.clone(), uris.clone());
    (
        StatusCode::CREATED,
        Json(json!({"client_id": id, "redirect_uris": uris, "token_endpoint_auth_method": "none"})),
    )
        .into_response()
}

async fn authorize(
    State(s): State<Arc<AsState>>,
    Query(q): Query<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| q.get(k).cloned().unwrap_or_default();
    let known = s
        .clients
        .lock()
        .unwrap()
        .get(&get("client_id"))
        .is_some_and(|uris| uris.contains(&get("redirect_uri")));
    if !known || get("response_type") != "code" {
        return (StatusCode::BAD_REQUEST, "bad authorization request").into_response();
    }
    let code = format!("code-{}", s.seq.fetch_add(1, Ordering::SeqCst));
    s.codes.lock().unwrap().insert(code.clone());
    Redirect::to(&format!(
        "{}?code={code}&state={}",
        get("redirect_uri"),
        get("state")
    ))
    .into_response()
}

async fn token(
    State(s): State<Arc<AsState>>,
    Form(form): Form<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| form.get(k).cloned().unwrap_or_default();
    let ok = match get("grant_type").as_str() {
        "authorization_code" => s.codes.lock().unwrap().remove(&get("code")),
        "refresh_token" => true,
        _ => false,
    };
    if !ok {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error": "invalid_grant"})),
        )
            .into_response();
    }
    Json(json!({
        "access_token": TOKEN, "token_type": "Bearer", "expires_in": 36000,
        "refresh_token": format!("rt-{}", s.seq.fetch_add(1, Ordering::SeqCst)),
    }))
    .into_response()
}

fn say(line: impl AsRef<str>) {
    let mut out = std::io::stdout().lock();
    let _ = writeln!(out, "{}", line.as_ref());
    let _ = out.flush();
}

struct Browser {
    mock: MockRelay,
    device: KeyPair,
    seq: i64,
}

impl Browser {
    fn hello(&self) -> Option<Value> {
        self.mock
            .events()
            .into_iter()
            .rev()
            .find(|e| e["op"] == "hello")
    }

    async fn command(&mut self, op: &str, body: Value) {
        let (Some(sid), Some(hello)) = (self.mock.session_id(), self.hello()) else {
            say("ERR no session");
            return;
        };
        let hid = hello["harness_id"].as_str().unwrap_or_default().to_owned();
        let before = self.mock.events().len();
        self.seq = self.seq.max(now_ms()) + 1;
        let text = envelope::sign_command(
            &self.device,
            &sid,
            &hid,
            self.seq,
            now_ms(),
            &crypto::new_nonce(),
            op,
            body,
        );
        if !self.mock.send_command(&text) {
            say("ERR no command stream");
            return;
        }
        let found = self
            .mock
            .wait_until(Duration::from_secs(10), |m| {
                m.events()
                    .iter()
                    .skip(before)
                    .any(|e| e["op"] == "ack" || e["op"] == "refused")
            })
            .await;
        let last = self
            .mock
            .events()
            .into_iter()
            .skip(before)
            .find(|e| e["op"] == "ack" || e["op"] == "refused");
        match (found, last) {
            (true, Some(e)) if e["op"] == "ack" => say(format!("ACK {}", e["body"]["result"])),
            (true, Some(e)) => say(format!("REFUSED {}", e["body"]["code"])),
            _ => say("ACK timeout"),
        }
    }

    async fn pair(&mut self, link: &str) {
        let Some((secret, h)) = pairing::parse_link_fragment(link) else {
            say("ERR not a pairing link");
            return;
        };
        let (Some(sid), Some(hello)) = (self.mock.session_id(), self.hello()) else {
            say("ERR no session");
            return;
        };
        let hid = hello["harness_id"].as_str().unwrap_or_default().to_owned();
        if hid != h {
            say("ERR the link's harness key is not the session's");
            return;
        }
        let harness_pub = crypto::b64u_decode(
            hello["body"]["harness"]["harness_pub"]
                .as_str()
                .unwrap_or_default(),
        )
        .unwrap_or_default();
        say(format!(
            "CODE {}",
            pairing::sas_code(&secret, &sid, &harness_pub, self.device.public_raw())
        ));
        let msg = pairing::build_pair_request(
            &secret,
            &sid,
            &hid,
            &self.device,
            "Safari on iPhone",
            now_ms(),
        );
        let before = self.mock.events().len();
        if !self.mock.send_pair_request(msg, USER, "jsims@example.test") {
            say("ERR no command stream");
            return;
        }
        let done = self
            .mock
            .wait_until(Duration::from_secs(120), |m| {
                m.events()
                    .iter()
                    .skip(before)
                    .any(|e| e["op"] == "pair_result")
            })
            .await;
        let result = self
            .mock
            .events()
            .into_iter()
            .skip(before)
            .find(|e| e["op"] == "pair_result");
        match (done, result) {
            (true, Some(e)) => say(format!("PAIRED {}", e["body"]["result"])),
            _ => say("PAIRED timeout"),
        }
    }
}

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() {
    if std::env::var("AMP_REMOTE_RIG").as_deref() != Ok("1") {
        return;
    }
    let refuse: Option<(u16, &'static str)> =
        match std::env::var("AMP_REMOTE_RIG_REFUSE").ok().as_deref() {
            Some("404") => Some((404, "not_found")),
            Some("403") => Some((403, "remote_disabled")),
            _ => None,
        };
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    let state = Arc::new(AsState {
        base: base.clone(),
        ..Default::default()
    });
    let mcp = common::app(&common::MockAmp::new(), None);
    let extra = Router::new()
        .route("/.well-known/oauth-protected-resource/mcp", get(prm))
        .route("/.well-known/oauth-authorization-server", get(as_metadata))
        .route("/register", post(register))
        .route("/authorize", get(authorize))
        .route("/token", post(token))
        .with_state(state.clone())
        .merge(mcp)
        .layer(middleware::from_fn_with_state(state.clone(), gate));
    let mock = MockRelay::serve(
        MockOptions {
            oauth_token: TOKEN.into(),
            refuse_register: refuse,
            // No idle limit unless asked (#2424: the relay's default).
            idle_timeout_s: std::env::var("AMP_REMOTE_RIG_IDLE")
                .ok()
                .and_then(|v| v.parse().ok()),
            ..MockOptions::default()
        },
        listener,
        extra,
    );
    say(format!("URL {base}/mcp"));
    let mut b = Browser {
        mock,
        device: KeyPair::generate(),
        seq: 0,
    };
    let mut lines = tokio::io::BufReader::new(tokio::io::stdin()).lines();
    while let Ok(Some(line)) = lines.next_line().await {
        let line = line.trim().to_owned();
        let (cmd, arg) = line.split_once(' ').unwrap_or((line.as_str(), ""));
        match cmd {
            "pair" => b.pair(arg.trim()).await,
            "prompt" => {
                b.command(
                    "prompt",
                    json!({"text": arg, "client_msg_id": format!("m-{}", now_ms())}),
                )
                .await
            }
            "stop" => b.command("stop", json!({})).await,
            "detach" => b.command("detach", json!({})).await,
            "viewers" => {
                let n = arg.trim().parse().unwrap_or(1);
                say(format!("VIEWERS {}", b.mock.set_viewers(n)));
            }
            "slow" => {
                SLOW_MS.store(arg.trim().parse().unwrap_or(0), Ordering::SeqCst);
                say("SLOW");
            }
            "drop" => {
                b.mock.drop_stream();
                say("DROPPED");
            }
            "revoke" => {
                let ok = b.mock.push(
                    "device_revoked",
                    &json!({"device_id": b.device.id(), "all": false}).to_string(),
                );
                say(format!("REVOKED {ok}"));
            }
            "end" => {
                b.mock.end_session(arg.trim());
                say("ENDED");
            }
            "events" => {
                for e in b.mock.events() {
                    say(format!("EVENT {} {}", e["op"], e["body"]["type"]));
                }
            }
            "quit" => break,
            "" => {}
            other => say(format!("ERR unknown command {other}")),
        }
    }
}
