//! The token refresh proof (protocol.md 8.2, #2390): renewing a session's connect token takes,
//! besides the person's OAuth login, a signature by the harness key the session was registered
//! with. Other programs holding the same login (Claude Code with the AMP plugin, a manager on
//! another machine) can't renew the token, so they can't take over the command stream.

use serde_json::{Value, json};

use super::{
    crypto::{self, KeyPair},
    jcs,
};

/// Signing-input prefix of a token refresh proof.
pub const TOKEN_PREFIX: &[u8] = b"amp-rc/1 token\n";

/// The members of a proof, `sig` included.
const FIELDS: [&str; 7] = ["v", "alg", "session_id", "harness_id", "ts", "nonce", "sig"];

/// The body of `POST /relay/v1/sessions/:id/token`: `{"proof": {...}}`, signed with `key`, with
/// a fresh nonce.
pub fn refresh_body(key: &KeyPair, session_id: &str, now_ms: i64) -> Value {
    let mut proof = json!({
        "v": 1, "alg": "ES256", "session_id": session_id, "harness_id": key.id(),
        "ts": now_ms, "nonce": crypto::new_nonce(),
    });
    let sig = key.sign(&signing_input(&proof));
    proof["sig"] = Value::String(sig);
    json!({ "proof": proof })
}

/// `"amp-rc/1 token\n"` + JCS(proof without `sig`).
fn signing_input(proof: &Value) -> Vec<u8> {
    let mut unsigned = proof.clone();
    if let Value::Object(map) = &mut unsigned {
        map.remove("sig");
    }
    let mut out = TOKEN_PREFIX.to_vec();
    out.extend_from_slice(jcs::canonical(&unsigned).as_bytes());
    out
}

/// What the relay knows when it checks a proof.
pub struct ProofReceiver<'a> {
    pub now_ms: i64,
    pub session_id: &'a str,
    pub harness_id: &'a str,
    pub harness_pub: &'a [u8],
    /// Whether the nonce was already used (and records it when not).
    pub seen: &'a mut dyn FnMut(&str) -> bool,
}

/// The relay's proof check in protocol.md 8.2's order (for the mock relay and the vectors):
/// `"ok"` or the first failing code.
pub fn check(body: &[u8], rx: ProofReceiver<'_>) -> &'static str {
    let Ok(v) = jcs::parse_strict(body) else {
        return "malformed";
    };
    let Some(outer) = v.as_object() else {
        return "malformed";
    };
    let Some(p) = outer.get("proof").and_then(Value::as_object) else {
        return "malformed";
    };
    if outer.len() != 1 || p.len() != FIELDS.len() || !FIELDS.iter().all(|f| p.contains_key(*f)) {
        return "malformed";
    }
    let int = |k: &str| p.get(k).and_then(Value::as_i64);
    let string = |k: &str| p.get(k).and_then(Value::as_str);
    let (Some(ver), Some(alg), Some(sid), Some(hid), Some(ts), Some(nonce), Some(sig)) = (
        int("v"),
        string("alg"),
        string("session_id"),
        string("harness_id"),
        int("ts"),
        string("nonce"),
        string("sig"),
    ) else {
        return "malformed";
    };
    let nonce_ok = nonce.len() == 22
        && nonce
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_');
    if !nonce_ok {
        return "malformed";
    }
    if ver != 1 {
        return "unsupported_version";
    }
    if alg != "ES256" {
        return "unsupported_alg";
    }
    if sid != rx.session_id {
        return "wrong_session";
    }
    if hid != rx.harness_id {
        return "wrong_harness";
    }
    let proof = Value::Object(p.clone());
    if !crypto::verify(rx.harness_pub, &signing_input(&proof), sig) {
        return "bad_signature";
    }
    if (ts - rx.now_ms).abs() > 60_000 {
        return "stale_ts";
    }
    if (rx.seen)(nonce) {
        return "replayed_nonce";
    }
    "ok"
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run(key: &KeyPair, body: &str, used: &mut Vec<String>) -> &'static str {
        check(
            body.as_bytes(),
            ProofReceiver {
                now_ms: 1_000,
                session_id: "rs_x",
                harness_id: key.id(),
                harness_pub: key.public_raw(),
                seen: &mut |n: &str| {
                    let hit = used.iter().any(|u| u == n);
                    used.push(n.to_owned());
                    hit
                },
            },
        )
    }

    #[test]
    fn round_trip_and_replay() {
        let key = KeyPair::generate();
        let body = refresh_body(&key, "rs_x", 1_000).to_string();
        let mut used = Vec::new();
        assert_eq!(run(&key, &body, &mut used), "ok");
        assert_eq!(run(&key, &body, &mut used), "replayed_nonce");
        let other = KeyPair::generate();
        let forged = refresh_body(&other, "rs_x", 1_000)
            .to_string()
            .replace(other.id(), key.id());
        assert_eq!(run(&key, &forged, &mut used), "bad_signature");
        let stale = refresh_body(&key, "rs_x", 1_000 + 61_000).to_string();
        assert_eq!(run(&key, &stale, &mut used), "stale_ts");
    }
}
