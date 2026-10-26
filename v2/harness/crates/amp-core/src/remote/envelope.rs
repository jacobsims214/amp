//! The envelope (protocol.md section 4), signed events, and the harness's command check in the
//! normative order of section 5.6.

use std::collections::{BTreeMap, HashMap, VecDeque};

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};

use super::crypto::{self, ENVELOPE_PREFIX, KeyPair};
use super::jcs;

pub const VERSION: i64 = 1;
pub const ALG: &str = "ES256";
/// The command ops an M1 harness accepts (section 5.1, closed).
pub const COMMAND_OPS: [&str; 3] = ["prompt", "stop", "detach"];
/// Section 10.
pub const MAX_COMMAND_BYTES: usize = 65_536;
pub const MAX_EVENT_BYTES: usize = 1_048_576;
pub const MAX_PROMPT_TEXT_BYTES: usize = 49_152;
pub const TS_WINDOW_MS: i64 = 60_000;
/// Nonces are kept at least twice the ts window, with room for this many.
pub const NONCE_TTL_MS: i64 = 120_000;
pub const NONCE_CAPACITY: usize = 10_000;

const FIELDS: [&str; 11] = [
    "v",
    "alg",
    "session_id",
    "harness_id",
    "device_id",
    "seq",
    "ts",
    "nonce",
    "op",
    "body",
    "sig",
];

/// A refusal code of section 9.2 (commands) or 9.3 (pairing).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Code {
    TooLarge,
    Malformed,
    UnsupportedVersion,
    UnsupportedAlg,
    WrongSession,
    WrongHarness,
    UnknownDevice,
    DeviceExpired,
    BadSignature,
    StaleTs,
    ReplayedNonce,
    SeqNotIncreasing,
    UnknownOp,
    InvalidBody,
    RateLimited,
    QueueFull,
    InternalError,
    // Pairing only (9.3).
    UnknownPairing,
    PairingExpired,
    PairingLocked,
    WrongUser,
    DeviceIdMismatch,
    BadMac,
    Rejected,
}

impl Code {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::TooLarge => "too_large",
            Self::Malformed => "malformed",
            Self::UnsupportedVersion => "unsupported_version",
            Self::UnsupportedAlg => "unsupported_alg",
            Self::WrongSession => "wrong_session",
            Self::WrongHarness => "wrong_harness",
            Self::UnknownDevice => "unknown_device",
            Self::DeviceExpired => "device_expired",
            Self::BadSignature => "bad_signature",
            Self::StaleTs => "stale_ts",
            Self::ReplayedNonce => "replayed_nonce",
            Self::SeqNotIncreasing => "seq_not_increasing",
            Self::UnknownOp => "unknown_op",
            Self::InvalidBody => "invalid_body",
            Self::RateLimited => "rate_limited",
            Self::QueueFull => "queue_full",
            Self::InternalError => "internal_error",
            Self::UnknownPairing => "unknown_pairing",
            Self::PairingExpired => "pairing_expired",
            Self::PairingLocked => "pairing_locked",
            Self::WrongUser => "wrong_user",
            Self::DeviceIdMismatch => "device_id_mismatch",
            Self::BadMac => "bad_mac",
            Self::Rejected => "rejected",
        }
    }

    /// The short sentence of `refused.message` (≤ 200 chars, never command content).
    pub fn message(self) -> &'static str {
        match self {
            Self::TooLarge => "The command is too large.",
            Self::Malformed => "The command is not a valid envelope.",
            Self::UnsupportedVersion => "This laptop speaks another protocol version.",
            Self::UnsupportedAlg => "Only ES256 signatures are accepted.",
            Self::WrongSession => "The command is for another session.",
            Self::WrongHarness => "The command is for another laptop.",
            Self::UnknownDevice => "This device is not paired with the laptop.",
            Self::DeviceExpired => "This device's pairing has expired; pair it again.",
            Self::BadSignature => "The command's signature does not verify.",
            Self::StaleTs => "The command's time is outside the 60 s window.",
            Self::ReplayedNonce => "The command was already received.",
            Self::SeqNotIncreasing => "The command's sequence number is not increasing.",
            Self::UnknownOp => "The laptop does not accept that command remotely.",
            Self::InvalidBody => "The command's body is not valid for its op.",
            Self::RateLimited => "Too many commands; wait a moment.",
            Self::QueueFull => "Five prompts are already queued.",
            Self::InternalError => "The laptop could not handle the command.",
            Self::UnknownPairing => "That pairing link is not current.",
            Self::PairingExpired => "The pairing link expired.",
            Self::PairingLocked => "Too many pairing attempts.",
            Self::WrongUser => "The device belongs to another AMP user.",
            Self::DeviceIdMismatch => "The device id does not match its key.",
            Self::BadMac => "The pairing code check failed.",
            Self::Rejected => "Pairing was declined at the laptop.",
        }
    }
}

impl std::fmt::Display for Code {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// One envelope, parsed under the profile.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Envelope {
    pub v: i64,
    pub alg: String,
    pub session_id: String,
    pub harness_id: String,
    pub device_id: Option<String>,
    pub seq: i64,
    pub ts: i64,
    pub nonce: String,
    pub op: String,
    pub body: Map<String, Value>,
    pub sig: String,
}

/// The signing input of an envelope value (the object with or without `sig`).
pub fn signing_input(envelope: &Value) -> Result<Vec<u8>, jcs::Malformed> {
    let mut v = envelope.clone();
    if let Value::Object(map) = &mut v {
        map.remove("sig");
    }
    let canonical = jcs::canonical_checked(&v)?;
    let mut out = ENVELOPE_PREFIX.to_vec();
    out.extend_from_slice(canonical.as_bytes());
    Ok(out)
}

/// The parsed envelope, if `value` has exactly the 11 members with the right types.
fn shape(value: &Value) -> Option<Envelope> {
    let map = value.as_object()?;
    if map.len() != FIELDS.len() || !FIELDS.iter().all(|f| map.contains_key(*f)) {
        return None;
    }
    let int = |k: &str| map.get(k).and_then(Value::as_i64);
    let string = |k: &str| map.get(k).and_then(Value::as_str).map(str::to_owned);
    let device_id = match map.get("device_id")? {
        Value::Null => None,
        Value::String(s) => Some(s.clone()),
        _ => return None,
    };
    Some(Envelope {
        v: int("v")?,
        alg: string("alg")?,
        session_id: string("session_id")?,
        harness_id: string("harness_id")?,
        device_id,
        seq: int("seq")?,
        ts: int("ts")?,
        nonce: string("nonce")?,
        op: string("op")?,
        body: map.get("body")?.as_object()?.clone(),
        sig: string("sig")?,
    })
}

/// A pinned device, as the command check needs it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DevicePin {
    pub device_id: String,
    pub device_pub: Vec<u8>,
    pub name: String,
    pub user_id: i64,
    pub pinned_until_ms: i64,
}

/// What the harness knows when it checks a command.
#[derive(Debug, Clone)]
pub struct CommandContext<'a> {
    pub now_ms: i64,
    pub user_id: i64,
    pub session_id: &'a str,
    pub harness_id: &'a str,
    pub pins: &'a [DevicePin],
}

/// Per-session replay state: the last seq per device and the nonce cache.
#[derive(Debug, Default)]
pub struct ReplayState {
    last_seq: HashMap<String, i64>,
    nonces: HashMap<String, i64>,
    order: VecDeque<(i64, String)>,
}

impl ReplayState {
    pub fn last_seq(&self, device_id: &str) -> i64 {
        self.last_seq.get(device_id).copied().unwrap_or(0)
    }

    /// For tests and vectors: a device's last seq.
    pub fn set_last_seq(&mut self, device_id: &str, seq: i64) {
        self.last_seq.insert(device_id.to_owned(), seq);
    }

    /// For tests and vectors: a nonce already seen at `at_ms`.
    pub fn remember_nonce(&mut self, nonce: &str, at_ms: i64) {
        if self.nonces.insert(nonce.to_owned(), at_ms).is_none() {
            self.order.push_back((at_ms, nonce.to_owned()));
        }
    }

    fn seen(&self, nonce: &str) -> bool {
        self.nonces.contains_key(nonce)
    }

    fn evict(&mut self, now_ms: i64) {
        while let Some((at, _)) = self.order.front() {
            let old = now_ms - at > NONCE_TTL_MS;
            if !old && self.order.len() <= NONCE_CAPACITY {
                break;
            }
            // Over capacity, the oldest goes even inside the TTL (a flood can't grow memory).
            if let Some((_, nonce)) = self.order.pop_front() {
                self.nonces.remove(&nonce);
            }
        }
    }
}

/// An accepted command.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Command {
    Prompt { text: String, client_msg_id: String },
    Stop,
    Detach,
}

impl Command {
    pub fn op(&self) -> &'static str {
        match self {
            Self::Prompt { .. } => "prompt",
            Self::Stop => "stop",
            Self::Detach => "detach",
        }
    }
}

/// A command that passed every check, with who sent it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VerifiedCommand {
    pub device_id: String,
    pub device_name: String,
    pub seq: i64,
    pub command: Command,
}

/// A refused command: the code and what could be parsed for `refused.ref`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Refusal {
    pub code: Code,
    pub device_id: Option<String>,
    pub seq: Option<i64>,
    pub op: Option<String>,
    pub last_seq: Option<i64>,
}

impl Refusal {
    fn bare(code: Code) -> Self {
        Self {
            code,
            device_id: None,
            seq: None,
            op: None,
            last_seq: None,
        }
    }
}

/// The body of `prompt` (section 5.1), unknown fields refused.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PromptBody {
    text: String,
    client_msg_id: String,
}

fn client_msg_id_ok(s: &str) -> bool {
    (1..=64).contains(&s.len())
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

fn parse_body(op: &str, body: &Map<String, Value>) -> Option<Command> {
    match op {
        "prompt" => {
            let b: PromptBody = serde_json::from_value(Value::Object(body.clone())).ok()?;
            if b.text.trim().is_empty()
                || b.text.len() > MAX_PROMPT_TEXT_BYTES
                || !client_msg_id_ok(&b.client_msg_id)
            {
                return None;
            }
            Some(Command::Prompt {
                text: b.text,
                client_msg_id: b.client_msg_id,
            })
        }
        "stop" if body.is_empty() => Some(Command::Stop),
        "detach" if body.is_empty() => Some(Command::Detach),
        _ => None,
    }
}

/// Section 5.6, steps 1-13: one refusal code, the first check that fails. On success (and on
/// `unknown_op` / `invalid_body`) the nonce and seq are committed to `replay`. Rate limits and
/// the queue (step 14) are the caller's.
pub fn check_command(
    text: &[u8],
    ctx: &CommandContext<'_>,
    replay: &mut ReplayState,
) -> Result<VerifiedCommand, Refusal> {
    if text.len() > MAX_COMMAND_BYTES {
        return Err(Refusal::bare(Code::TooLarge));
    }
    let value = jcs::parse_strict(text).map_err(|_| Refusal::bare(Code::Malformed))?;
    let env = shape(&value).ok_or_else(|| Refusal::bare(Code::Malformed))?;
    let refuse = |code: Code| Refusal {
        code,
        device_id: env.device_id.clone(),
        seq: Some(env.seq),
        op: Some(env.op.clone()),
        last_seq: None,
    };
    if env.v != VERSION {
        return Err(refuse(Code::UnsupportedVersion));
    }
    if env.alg != ALG {
        return Err(refuse(Code::UnsupportedAlg));
    }
    if env.session_id != ctx.session_id {
        return Err(refuse(Code::WrongSession));
    }
    if env.harness_id != ctx.harness_id {
        return Err(refuse(Code::WrongHarness));
    }
    let pin = env
        .device_id
        .as_deref()
        .and_then(|id| ctx.pins.iter().find(|p| p.device_id == id))
        .filter(|p| p.user_id == ctx.user_id)
        .ok_or_else(|| refuse(Code::UnknownDevice))?;
    if pin.pinned_until_ms <= ctx.now_ms {
        return Err(refuse(Code::DeviceExpired));
    }
    let input = signing_input(&value).map_err(|_| refuse(Code::Malformed))?;
    if !crypto::verify(&pin.device_pub, &input, &env.sig) {
        return Err(refuse(Code::BadSignature));
    }
    if (env.ts - ctx.now_ms).abs() > TS_WINDOW_MS {
        return Err(refuse(Code::StaleTs));
    }
    replay.evict(ctx.now_ms);
    if replay.seen(&env.nonce) {
        return Err(refuse(Code::ReplayedNonce));
    }
    let last = replay.last_seq(&pin.device_id);
    if env.seq <= last {
        let mut r = refuse(Code::SeqNotIncreasing);
        r.last_seq = Some(last);
        return Err(r);
    }
    replay.remember_nonce(&env.nonce, ctx.now_ms);
    replay.set_last_seq(&pin.device_id, env.seq);
    if !COMMAND_OPS.contains(&env.op.as_str()) {
        return Err(refuse(Code::UnknownOp));
    }
    let command = parse_body(&env.op, &env.body).ok_or_else(|| refuse(Code::InvalidBody))?;
    Ok(VerifiedCommand {
        device_id: pin.device_id.clone(),
        device_name: pin.name.clone(),
        seq: env.seq,
        command,
    })
}

/// Builds and signs envelopes for one session: the harness side of section 6.
#[derive(Debug)]
pub struct EventSigner {
    key: KeyPair,
    session_id: String,
    next_seq: i64,
}

impl EventSigner {
    pub fn new(key: KeyPair, session_id: impl Into<String>) -> Self {
        Self {
            key,
            session_id: session_id.into(),
            next_seq: 1,
        }
    }

    pub fn session_id(&self) -> &str {
        &self.session_id
    }

    pub fn harness_id(&self) -> &str {
        self.key.id()
    }

    /// The seq the next event gets.
    pub fn next_seq(&self) -> i64 {
        self.next_seq
    }

    /// The next event, signed, as compact JSON (the wire form). `body` must be in the profile.
    pub fn sign(
        &mut self,
        op: &str,
        device_id: Option<&str>,
        body: Value,
        now_ms: i64,
    ) -> Result<String, jcs::Malformed> {
        let mut env = json!({
            "v": VERSION,
            "alg": ALG,
            "session_id": self.session_id,
            "harness_id": self.key.id(),
            "device_id": device_id,
            "seq": self.next_seq,
            "ts": now_ms,
            "nonce": crypto::new_nonce(),
            "op": op,
            "body": body,
        });
        let input = signing_input(&env)?;
        let sig = self.key.sign(&input);
        env["sig"] = Value::String(sig);
        self.next_seq += 1;
        Ok(jcs::canonical(&env))
    }

    /// As [`Self::sign`], but an event over `max_bytes` is not returned and takes no seq
    /// (`Ok(None)`), so leaving it out never opens a gap in the seqs (section 6.1).
    pub fn sign_at_most(
        &mut self,
        op: &str,
        device_id: Option<&str>,
        body: Value,
        now_ms: i64,
        max_bytes: usize,
    ) -> Result<Option<String>, jcs::Malformed> {
        let seq = self.next_seq;
        let env = self.sign(op, device_id, body, now_ms)?;
        if env.len() > max_bytes {
            self.next_seq = seq;
            return Ok(None);
        }
        Ok(Some(env))
    }
}

/// `refused.body` (section 6.2).
pub fn refused_body(r: &Refusal) -> Value {
    let reference = match (&r.device_id, r.seq) {
        (Some(d), Some(s)) => json!({"device_id": d, "seq": s}),
        _ => Value::Null,
    };
    json!({
        "ref": reference,
        "op": r.op,
        "code": r.code.as_str(),
        "message": r.code.message(),
        "last_seq": r.last_seq,
    })
}

/// `ack.body` (section 6.2).
pub fn ack_body(
    device_id: &str,
    seq: i64,
    op: &str,
    result: &str,
    prompt_id: Option<&str>,
    queue_position: Option<usize>,
) -> Value {
    json!({
        "ref": {"device_id": device_id, "seq": seq},
        "op": op,
        "result": result,
        "prompt_id": prompt_id,
        "queue_position": queue_position,
    })
}

/// The browser's event check (section 5.7), for tests and the vectors: `ok`, `duplicate`,
/// `ignored_unknown_op` or a code.
pub fn check_event_for_test(
    text: &[u8],
    session_id: &str,
    harness_id: &str,
    harness_pub: &[u8],
    last_applied_seq: i64,
) -> &'static str {
    const EVENT_OPS: [&str; 7] = [
        "hello",
        "status",
        "chat_event",
        "ack",
        "refused",
        "pair_result",
        "bye",
    ];
    if text.len() > MAX_EVENT_BYTES {
        return "too_large";
    }
    let Ok(value) = jcs::parse_strict(text) else {
        return "malformed";
    };
    let Some(env) = shape(&value) else {
        return "malformed";
    };
    if env.v != VERSION {
        return "unsupported_version";
    }
    if env.alg != ALG {
        return "unsupported_alg";
    }
    if env.session_id != session_id {
        return "wrong_session";
    }
    if env.harness_id != harness_id {
        return "wrong_harness";
    }
    let Ok(input) = signing_input(&value) else {
        return "malformed";
    };
    if !crypto::verify(harness_pub, &input, &env.sig) {
        return "bad_signature";
    }
    if env.seq <= last_applied_seq {
        return "duplicate";
    }
    if !EVENT_OPS.contains(&env.op.as_str()) {
        return "ignored_unknown_op";
    }
    "ok"
}

/// Signs a command as a device would (tests and the mock relay's scripted browser).
#[allow(clippy::too_many_arguments)]
pub fn sign_command(
    device: &KeyPair,
    session_id: &str,
    harness_id: &str,
    seq: i64,
    ts: i64,
    nonce: &str,
    op: &str,
    body: Value,
) -> String {
    let mut env = json!({
        "v": VERSION, "alg": ALG, "session_id": session_id, "harness_id": harness_id,
        "device_id": device.id(), "seq": seq, "ts": ts, "nonce": nonce, "op": op, "body": body,
    });
    let input = signing_input(&env).expect("a command in the profile");
    env["sig"] = Value::String(device.sign(&input));
    jcs::canonical(&env)
}

/// Pins from a JSON list as the vectors write them (`device_pub` base64url).
pub fn pins_from_json(list: &[Value]) -> Vec<DevicePin> {
    list.iter()
        .filter_map(|p| {
            Some(DevicePin {
                device_id: p.get("device_id")?.as_str()?.to_owned(),
                device_pub: crypto::b64u_decode(p.get("device_pub")?.as_str()?)?,
                name: p
                    .get("name")
                    .and_then(Value::as_str)
                    .unwrap_or("device")
                    .to_owned(),
                user_id: p.get("user_id")?.as_i64()?,
                pinned_until_ms: p.get("pinned_until_ms")?.as_i64()?,
            })
        })
        .collect()
}

/// For the vectors: replay state from `last_seq` and `seen_nonces`.
pub fn replay_from_json(
    last_seq: &BTreeMap<String, i64>,
    nonces: &[String],
    now_ms: i64,
) -> ReplayState {
    let mut r = ReplayState::default();
    for (d, s) in last_seq {
        r.set_last_seq(d, *s);
    }
    for n in nonces {
        r.remember_nonce(n, now_ms);
    }
    r
}
