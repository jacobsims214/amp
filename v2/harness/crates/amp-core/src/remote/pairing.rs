//! Pairing (protocol.md section 7): the link secret, its derivations, the harness's check of a
//! pair request in the normative order of section 7.5, and the device pins (section 7.7).

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::crypto::{self, PAIR_REQUEST_PREFIX};
use super::envelope::{Code, DevicePin, TS_WINDOW_MS, VERSION};
use super::jcs;

/// A pairing secret lasts 5 minutes and allows 5 tries.
pub const PAIRING_TTL_MS: i64 = 5 * 60 * 1000;
pub const PAIRING_TRIES: u32 = 5;
/// D1: a device stays pinned 30 days at most.
pub const MAX_PIN_DAYS: i64 = 30;
pub const DAY_MS: i64 = 24 * 60 * 60 * 1000;

const REQUEST_FIELDS: [&str; 10] = [
    "type",
    "v",
    "session_id",
    "harness_id",
    "pairing_id",
    "device_id",
    "device_pub",
    "device_name",
    "ts",
    "nonce",
];

/// `pairing_id = b64u(SHA-256("amp-rc/1 pairing-id\n" ‖ secret)[0..16])`.
pub fn pairing_id(secret: &[u8]) -> String {
    let mut input = b"amp-rc/1 pairing-id\n".to_vec();
    input.extend_from_slice(secret);
    crypto::b64u(&crypto::sha256(&input)[..16])
}

pub fn k_mac(secret: &[u8], session_id: &str) -> [u8; 32] {
    crypto::hkdf32(secret, session_id.as_bytes(), b"amp-rc/1 pair-mac")
}

pub fn k_sas(secret: &[u8], session_id: &str) -> [u8; 32] {
    crypto::hkdf32(secret, session_id.as_bytes(), b"amp-rc/1 sas")
}

/// The 8-digit matching code, shown `NNNN-NNNN`.
pub fn sas_code(secret: &[u8], session_id: &str, harness_pub: &[u8], device_pub: &[u8]) -> String {
    let mut msg = harness_pub.to_vec();
    msg.extend_from_slice(device_pub);
    let sas = crypto::hmac_sha256(&k_sas(secret, session_id), &msg);
    let n = u64::from_be_bytes(sas[..8].try_into().expect("8 bytes")) % 100_000_000;
    let digits = format!("{n:08}");
    format!("{}-{}", &digits[..4], &digits[4..])
}

/// The link: `<web origin>/sessions/<session_id>#p=<secret>&h=<harness_id>`.
pub fn pairing_link(web_origin: &str, session_id: &str, secret: &[u8], harness_id: &str) -> String {
    format!(
        "{}/sessions/{session_id}#p={}&h={harness_id}",
        web_origin.trim_end_matches('/'),
        crypto::b64u(secret)
    )
}

/// Whether `c` must never reach the terminal from a remote or relay-supplied string: C0 and C1
/// controls and DEL (so ESC goes, and no escape sequence is interpreted), the bidi controls, and
/// every other invisible format character (Unicode category Cf: zero-width space/joiners, word
/// joiner, invisible operators, BOM, soft hyphen, tag characters, …) plus the line and paragraph
/// separators U+2028/U+2029 (Zl, Zp), which can fake or hide a line such as the y/N question
/// (#2391, L5).
pub fn is_terminal_unsafe(c: char) -> bool {
    let u = c as u32;
    u < 0x20
        || (0x7f..=0x9f).contains(&u)
        || matches!(
            u,
            0x00ad
                | 0x0600..=0x0605
                | 0x061c
                | 0x06dd
                | 0x070f
                | 0x0890..=0x0891
                | 0x08e2
                | 0x180e
                | 0x200b..=0x200f
                | 0x2028..=0x202e
                | 0x2060..=0x2064
                | 0x2066..=0x206f
                | 0xfeff
                | 0xfff9..=0xfffb
                | 0x110bd
                | 0x110cd
                | 0x13430..=0x1343f
                | 0x1bca0..=0x1bca3
                | 0x1d173..=0x1d17a
                | 0xe0001
                | 0xe0020..=0xe007f
        )
}

/// Strips everything [`is_terminal_unsafe`] names before a relay-supplied string is printed in
/// the terminal (section 7.5 step 13). Also caps it.
pub fn sanitize_for_terminal(s: &str, max_chars: usize) -> String {
    s.chars()
        .filter(|&c| !is_terminal_unsafe(c))
        .take(max_chars)
        .collect::<String>()
        .trim()
        .to_owned()
}

/// Remote prompt text as the laptop shows it (#2391, L2): [`sanitize_for_terminal`] without the
/// cap, keeping line breaks and tabs (`\r\n` becomes `\n`). Display only: the model gets the text
/// as sent.
pub fn sanitize_text_for_terminal(s: &str) -> String {
    s.replace("\r\n", "\n")
        .chars()
        .filter(|&c| c == '\n' || c == '\t' || !is_terminal_unsafe(c))
        .collect()
}

/// What the relay adds to a forwarded pair request (section 7.4 step 6): from its verified
/// token, never from the client.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct RelayInfo {
    pub user_id: i64,
    #[serde(default)]
    pub email: String,
    #[serde(default)]
    pub user_agent: String,
}

/// A request that passed steps 1-12: the person decides.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PairCandidate {
    pub pairing_id: String,
    pub device_id: String,
    pub device_pub: Vec<u8>,
    /// Sanitized for the terminal.
    pub device_name: String,
    pub email: String,
    pub user_agent: String,
    /// The matching code (`NNNN-NNNN`) the browser shows too.
    pub code: String,
}

/// `pair_result.body` (section 6.2).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PairResult {
    pub pairing_id: String,
    pub device_id: Option<String>,
    pub result: &'static str,
    pub code: Option<Code>,
    pub tries_left: Option<u32>,
    pub pinned_until_ms: Option<i64>,
}

impl PairResult {
    pub fn body(&self) -> Value {
        json!({
            "pairing_id": self.pairing_id,
            "result": self.result,
            "code": self.code.map(Code::as_str),
            "tries_left": self.tries_left,
            "pinned_until_ms": self.pinned_until_ms,
        })
    }
}

/// The current pairing secret (one at a time; `/remote pair` replaces it).
#[derive(Debug)]
pub struct PairingWindow {
    secret: Vec<u8>,
    id: String,
    created_ms: i64,
    tries_left: u32,
    /// Spent: accepted, rejected, expired or burned.
    consumed: bool,
    /// A request passed the checks and waits for the person's y/N.
    pending: bool,
}

impl std::fmt::Debug for PairCheckInput<'_> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("PairCheckInput")
    }
}

/// What the check needs besides the window.
pub struct PairCheckInput<'a> {
    pub now_ms: i64,
    pub session_id: &'a str,
    pub harness_id: &'a str,
    pub harness_pub: &'a [u8],
    pub user_id: i64,
}

impl PairingWindow {
    /// A new random secret.
    pub fn new(now_ms: i64) -> Self {
        Self::with_secret(crypto::random_bytes(32), now_ms)
    }

    /// A window over a given secret (tests and the vectors).
    pub fn with_secret(secret: Vec<u8>, now_ms: i64) -> Self {
        let id = pairing_id(&secret);
        Self {
            secret,
            id,
            created_ms: now_ms,
            tries_left: PAIRING_TRIES,
            consumed: false,
            pending: false,
        }
    }

    pub fn id(&self) -> &str {
        &self.id
    }

    pub fn secret(&self) -> &[u8] {
        &self.secret
    }

    pub fn expires_at_ms(&self) -> i64 {
        self.created_ms + PAIRING_TTL_MS
    }

    pub fn is_open(&self, now_ms: i64) -> bool {
        !self.consumed && now_ms < self.expires_at_ms()
    }

    pub fn tries_left(&self) -> u32 {
        self.tries_left
    }

    /// Marks the window spent (the person answered, or the prompt expired).
    pub fn consume(&mut self) {
        self.consumed = true;
        self.pending = false;
    }

    /// Section 7.5 for one forwarded `pair_request` (`data` is the SSE event's JSON).
    /// `Ok` asks the person; `Err` is the `pair_result` to send. The device id in the result is
    /// the requester's when it could be read.
    pub fn check(
        &mut self,
        data: &[u8],
        input: &PairCheckInput<'_>,
    ) -> Result<PairCandidate, PairResult> {
        let failed = |pairing_id: &str, device_id: Option<String>, code: Code| PairResult {
            pairing_id: pairing_id.to_owned(),
            device_id,
            result: "failed",
            code: Some(code),
            tries_left: None,
            pinned_until_ms: None,
        };
        // 1. Malformed.
        let Some(msg) = jcs::parse_strict(data).ok().and_then(|v| parse_message(&v)) else {
            return Err(failed("", None, Code::Malformed));
        };
        let req = &msg.request;
        let pid = req.pairing_id.clone();
        let dev = Some(req.device_id.clone());
        // 2-3.
        if req.v != VERSION {
            return Err(failed(&pid, dev, Code::UnsupportedVersion));
        }
        if req.session_id != input.session_id {
            return Err(failed(&pid, dev, Code::WrongSession));
        }
        if req.harness_id != input.harness_id {
            return Err(failed(&pid, dev, Code::WrongHarness));
        }
        // 4-6.
        if req.pairing_id != self.id || self.consumed {
            return Err(failed(&pid, dev, Code::UnknownPairing));
        }
        if input.now_ms >= self.expires_at_ms() {
            return Err(failed(&pid, dev, Code::PairingExpired));
        }
        if self.tries_left == 0 || self.pending {
            return Err(failed(&pid, dev, Code::PairingLocked));
        }
        // 7. Count one try.
        self.tries_left -= 1;
        let spent = |this: &mut Self, code: Code| {
            if this.tries_left == 0 {
                this.consumed = true;
                PairResult {
                    pairing_id: pid.clone(),
                    device_id: dev.clone(),
                    result: "locked",
                    code: Some(Code::PairingLocked),
                    tries_left: Some(0),
                    pinned_until_ms: None,
                }
            } else {
                PairResult {
                    pairing_id: pid.clone(),
                    device_id: dev.clone(),
                    result: "failed",
                    code: Some(code),
                    tries_left: Some(this.tries_left),
                    pinned_until_ms: None,
                }
            }
        };
        // 8-12.
        if msg.relay.user_id != input.user_id {
            return Err(spent(self, Code::WrongUser));
        }
        if crypto::fingerprint(&msg.device_pub) != req.device_id {
            return Err(spent(self, Code::DeviceIdMismatch));
        }
        if (req.ts - input.now_ms).abs() > TS_WINDOW_MS {
            return Err(spent(self, Code::StaleTs));
        }
        let mut signing_input = PAIR_REQUEST_PREFIX.to_vec();
        signing_input.extend_from_slice(msg.request_jcs.as_bytes());
        if !crypto::hmac_sha256_verify(
            &k_mac(&self.secret, input.session_id),
            &signing_input,
            &msg.mac,
        ) {
            return Err(spent(self, Code::BadMac));
        }
        if !crypto::verify(&msg.device_pub, &signing_input, &msg.sig) {
            return Err(spent(self, Code::BadSignature));
        }
        self.pending = true;
        Ok(PairCandidate {
            pairing_id: pid,
            device_id: req.device_id.clone(),
            code: sas_code(
                &self.secret,
                input.session_id,
                input.harness_pub,
                &msg.device_pub,
            ),
            device_pub: msg.device_pub,
            device_name: sanitize_for_terminal(&req.device_name, 64),
            email: sanitize_for_terminal(&msg.relay.email, 254),
            user_agent: sanitize_for_terminal(&msg.relay.user_agent, 200),
        })
    }
}

#[derive(Debug, Clone)]
struct PairRequest {
    v: i64,
    session_id: String,
    harness_id: String,
    pairing_id: String,
    device_id: String,
    device_name: String,
    ts: i64,
}

struct PairMessage {
    request: PairRequest,
    request_jcs: String,
    device_pub: Vec<u8>,
    mac: Vec<u8>,
    sig: String,
    relay: RelayInfo,
}

fn parse_message(v: &Value) -> Option<PairMessage> {
    let m = v.as_object()?;
    let req = m.get("request")?.as_object()?;
    if req.len() != REQUEST_FIELDS.len() || !REQUEST_FIELDS.iter().all(|f| req.contains_key(*f)) {
        return None;
    }
    let s = |k: &str| req.get(k).and_then(Value::as_str).map(str::to_owned);
    if s("type")? != "pair_request" {
        return None;
    }
    let device_name = s("device_name")?;
    let name_len = device_name.chars().count();
    if !(1..=64).contains(&name_len) {
        return None;
    }
    let request = PairRequest {
        v: req.get("v")?.as_i64()?,
        session_id: s("session_id")?,
        harness_id: s("harness_id")?,
        pairing_id: s("pairing_id")?,
        device_id: s("device_id")?,
        device_name,
        ts: req.get("ts")?.as_i64()?,
    };
    s("nonce")?;
    let device_pub = crypto::b64u_decode(&s("device_pub")?)?;
    let mac = crypto::b64u_decode(m.get("mac")?.as_str()?)?;
    let sig = m.get("sig")?.as_str()?.to_owned();
    crypto::b64u_decode(&sig)?;
    let relay: RelayInfo =
        serde_json::from_value(m.get("relay").cloned().unwrap_or(Value::Null)).unwrap_or_default();
    Some(PairMessage {
        request,
        request_jcs: jcs::canonical_checked(&Value::Object(req.clone())).ok()?,
        device_pub,
        mac,
        sig,
        relay,
    })
}

/// When a pin made now ends: `min(30, org days)` days, or `session_end_ms` for an org value of
/// 0 ("re-pair every session").
pub fn pin_until(now_ms: i64, org_pin_days: i64, session_end_ms: i64) -> i64 {
    let days = org_pin_days.clamp(0, MAX_PIN_DAYS);
    if days == 0 {
        session_end_ms
    } else {
        now_ms + days * DAY_MS
    }
}

/// One pinned device as stored (public keys aren't secret).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct StoredPin {
    pub device_id: String,
    pub device_pub: String,
    pub name: String,
    pub user_id: i64,
    pub paired_at_ms: i64,
    pub pinned_until_ms: i64,
    /// A pin for this session only (org pin days 0): never written to disk.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub session_only: bool,
}

impl StoredPin {
    pub fn to_pin(&self) -> Option<DevicePin> {
        Some(DevicePin {
            device_id: self.device_id.clone(),
            device_pub: crypto::b64u_decode(&self.device_pub)?,
            name: self.name.clone(),
            user_id: self.user_id,
            pinned_until_ms: self.pinned_until_ms,
        })
    }
}

#[derive(Debug, Default, Serialize, Deserialize)]
struct PinFile {
    v: u32,
    devices: Vec<StoredPin>,
}

/// The harness's pins: `<state dir>/remote-devices.json`, mode 0600 (section 7.7), plus
/// session-only pins in memory. `None` path: memory only (tests).
#[derive(Debug)]
pub struct PinStore {
    path: Option<PathBuf>,
    pins: Vec<StoredPin>,
}

/// The pin file's name in the harness's state directory.
pub const PIN_FILE: &str = "remote-devices.json";

impl PinStore {
    pub fn memory() -> Self {
        Self {
            path: None,
            pins: Vec::new(),
        }
    }

    /// Loads the file (missing: no pins; unreadable: an error, so pins are never silently lost
    /// by a later save).
    pub fn load(path: &Path) -> Result<Self, String> {
        let pins = match std::fs::read_to_string(path) {
            Ok(text) => {
                serde_json::from_str::<PinFile>(&text)
                    .map_err(|e| format!("{}: {e}", path.display()))?
                    .devices
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Vec::new(),
            Err(e) => return Err(format!("read {}: {e}", path.display())),
        };
        Ok(Self {
            path: Some(path.to_owned()),
            pins,
        })
    }

    /// Re-reads the file (on every reconnect), keeping the session-only pins.
    pub fn reload(&mut self) -> Result<(), String> {
        let Some(path) = self.path.clone() else {
            return Ok(());
        };
        let session: Vec<StoredPin> = self
            .pins
            .iter()
            .filter(|p| p.session_only)
            .cloned()
            .collect();
        let mut fresh = Self::load(&path)?;
        fresh.pins.extend(session);
        self.pins = fresh.pins;
        Ok(())
    }

    fn save(&self) -> Result<(), String> {
        let Some(path) = &self.path else {
            return Ok(());
        };
        let file = PinFile {
            v: 1,
            devices: self
                .pins
                .iter()
                .filter(|p| !p.session_only)
                .cloned()
                .collect(),
        };
        let text = serde_json::to_string_pretty(&file).map_err(|e| e.to_string())?;
        crypto::write_private(path, text.as_bytes())
    }

    /// The live pins of `user_id` (unexpired at `now_ms`).
    pub fn active(&self, user_id: i64, now_ms: i64) -> Vec<DevicePin> {
        self.pins
            .iter()
            .filter(|p| p.user_id == user_id && p.pinned_until_ms > now_ms)
            .filter_map(StoredPin::to_pin)
            .collect()
    }

    /// Every pin (expired ones too), for the device list.
    pub fn all(&self) -> &[StoredPin] {
        &self.pins
    }

    /// Pins a device (replacing an older pin of the same device), and drops expired pins.
    pub fn pin(&mut self, pin: StoredPin, now_ms: i64) -> Result<(), String> {
        self.pins
            .retain(|p| p.device_id != pin.device_id && p.pinned_until_ms > now_ms);
        self.pins.push(pin);
        self.save()
    }

    /// Unpins one device. Returns whether it was pinned.
    pub fn unpin(&mut self, device_id: &str) -> Result<bool, String> {
        let before = self.pins.len();
        self.pins.retain(|p| p.device_id != device_id);
        let removed = self.pins.len() != before;
        if removed {
            self.save()?;
        }
        Ok(removed)
    }

    /// Unpins every device. Returns how many there were.
    pub fn unpin_all(&mut self) -> Result<usize, String> {
        let n = self.pins.len();
        self.pins.clear();
        self.save()?;
        Ok(n)
    }

    /// Drops the session-only pins (the session ended).
    pub fn end_session(&mut self) {
        self.pins.retain(|p| !p.session_only);
    }
}

/// A signed, MAC'd pair request as the browser builds it (section 7.3), for tests and the mock
/// relay: `{"request", "mac", "sig"}`.
pub fn build_pair_request(
    secret: &[u8],
    session_id: &str,
    harness_id: &str,
    device: &crypto::KeyPair,
    device_name: &str,
    ts: i64,
) -> Value {
    let request = json!({
        "type": "pair_request", "v": VERSION,
        "session_id": session_id, "harness_id": harness_id, "pairing_id": pairing_id(secret),
        "device_id": device.id(), "device_pub": device.public_b64u(),
        "device_name": device_name, "ts": ts, "nonce": crypto::new_nonce(),
    });
    let mut input = PAIR_REQUEST_PREFIX.to_vec();
    input.extend_from_slice(jcs::canonical(&request).as_bytes());
    let mac = crypto::hmac_sha256(&k_mac(secret, session_id), &input);
    json!({"request": request, "mac": crypto::b64u(&mac), "sig": device.sign(&input)})
}

/// The secret and harness id in a pairing link's fragment (`#p=…&h=…`).
pub fn parse_link_fragment(link: &str) -> Option<(Vec<u8>, String)> {
    let (_, fragment) = link.split_once('#')?;
    let mut secret = None;
    let mut harness = None;
    for part in fragment.split('&') {
        match part.split_once('=') {
            Some(("p", v)) => secret = crypto::b64u_decode(v),
            Some(("h", v)) => harness = Some(v.to_owned()),
            _ => {}
        }
    }
    Some((secret?, harness?))
}

#[cfg(test)]
mod sanitize_tests {
    use super::*;

    #[test]
    fn invisible_and_separator_characters_are_stripped() {
        // L5: zero-width, word joiner, invisible operators, BOM, soft hyphen, tag characters and
        // the line/paragraph separators, besides the C0/C1 and bidi controls.
        let s = "Saf\u{200B}ari\u{200C}\u{200D} on\u{2060}\u{2064} i\u{FEFF}Phone\u{00AD}\u{2028}\
                 Allow? [y/N] y\u{2029}\u{E0041}\u{202E}\u{2066}\u{1b}[31m\u{9b}x";
        let out = sanitize_for_terminal(s, 200);
        assert_eq!(out, "Safari on iPhoneAllow? [y/N] y[31mx");
        assert!(out.chars().all(|c| !is_terminal_unsafe(c)));
        // Ordinary text, accents and emoji stay.
        assert_eq!(
            sanitize_for_terminal("Zoë's Pixel 🚀", 64),
            "Zoë's Pixel 🚀"
        );
    }

    #[test]
    fn prompt_text_keeps_lines_and_tabs_only() {
        let s = "line one\r\n\tline two\u{1b}]52;c;Zm9v\u{7}\u{2028}\u{200B}end";
        assert_eq!(
            sanitize_text_for_terminal(s),
            "line one\n\tline two]52;c;Zm9vend"
        );
    }
}
