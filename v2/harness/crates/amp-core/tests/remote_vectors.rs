//! #2373: the shared remote control test vectors (v2/testdata/remote-control, #2369) against
//! the harness's protocol code: keys and fingerprints, JCS, every command vector through the
//! section 5.6 check, every event vector through the section 5.7 check, and the pairing
//! derivations and rejections of section 7.

use std::{collections::BTreeMap, path::PathBuf};

use amp_core::remote::{
    crypto::{self, KeyPair},
    envelope::{self, CommandContext},
    jcs,
    pairing::{self, PairCheckInput, PairingWindow},
};
use serde_json::Value;

fn load(file: &str) -> Value {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/remote-control")
        .join(file);
    serde_json::from_str(&std::fs::read_to_string(&path).expect("read vectors")).expect("json")
}

fn hex(s: &str) -> Vec<u8> {
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
        .collect()
}

#[test]
fn keys_and_fingerprints() {
    for k in load("keys.json")["keys"].as_array().unwrap() {
        let key = KeyPair::from_scalar(&hex(k["private_scalar_hex"].as_str().unwrap())).unwrap();
        assert_eq!(key.public_b64u(), k["public_raw_b64u"], "{}", k["label"]);
        assert_eq!(key.id(), k["fingerprint"], "{}", k["label"]);
        assert_eq!(key.display_fingerprint(), k["fingerprint_display"]);
        // Round trip: the harness's signatures verify.
        let sig = key.sign(b"amp-rc/1 envelope\n{}");
        assert!(crypto::verify(
            key.public_raw(),
            b"amp-rc/1 envelope\n{}",
            &sig
        ));
    }
    for c in load("fingerprints.json")["cases"].as_array().unwrap() {
        let raw = crypto::b64u_decode(c["public_raw_b64u"].as_str().unwrap()).unwrap();
        assert_eq!(crypto::fingerprint(&raw), c["fingerprint"]);
        assert_eq!(crypto::fingerprint_display(&raw), c["fingerprint_display"]);
    }
}

#[test]
fn jcs_vectors() {
    let file = load("jcs.json");
    for c in file["canonical"].as_array().unwrap() {
        let v = jcs::parse_strict(c["input"].as_str().unwrap().as_bytes())
            .unwrap_or_else(|e| panic!("{}: {e}", c["name"]));
        assert_eq!(
            jcs::canonical(&v),
            c["canonical"].as_str().unwrap(),
            "{}",
            c["name"]
        );
    }
    for c in file["reject"].as_array().unwrap() {
        assert!(
            jcs::parse_strict(c["input"].as_str().unwrap().as_bytes()).is_err(),
            "{} must be refused",
            c["name"]
        );
    }
}

#[test]
fn command_vectors() {
    let file = load("commands.json");
    let vectors = file["vectors"].as_array().unwrap();
    assert!(vectors.len() >= 30);
    for v in vectors {
        let rx = &v["receiver"];
        let now = rx["now_ms"].as_i64().unwrap();
        let pins = envelope::pins_from_json(rx["pinned_devices"].as_array().unwrap());
        let last: BTreeMap<String, i64> = serde_json::from_value(rx["last_seq"].clone()).unwrap();
        let nonces: Vec<String> = serde_json::from_value(rx["seen_nonces"].clone()).unwrap();
        let mut replay = envelope::replay_from_json(&last, &nonces, now);
        let ctx = CommandContext {
            now_ms: now,
            user_id: rx["user_id"].as_i64().unwrap(),
            session_id: rx["session_id"].as_str().unwrap(),
            harness_id: rx["harness_id"].as_str().unwrap(),
            pins: &pins,
        };
        let wire = v["wire"].as_str().unwrap();
        let got = match envelope::check_command(wire.as_bytes(), &ctx, &mut replay) {
            Ok(_) => "ok".to_owned(),
            Err(r) => r.code.as_str().to_owned(),
        };
        assert_eq!(got, v["expect"].as_str().unwrap(), "vector {}", v["name"]);
        if let Some(input) = v.get("signing_input").and_then(Value::as_str) {
            let parsed = jcs::parse_strict(wire.as_bytes()).unwrap();
            let ours = envelope::signing_input(&parsed).unwrap();
            assert_eq!(
                String::from_utf8(ours).unwrap(),
                input,
                "vector {}",
                v["name"]
            );
        }
        // An accepted command can't be replayed: the same text is refused the second time.
        if got == "ok" {
            let again = envelope::check_command(wire.as_bytes(), &ctx, &mut replay)
                .expect_err("a replay is refused");
            assert_eq!(
                again.code.as_str(),
                "replayed_nonce",
                "vector {}",
                v["name"]
            );
        }
    }
}

#[test]
fn event_vectors() {
    for v in load("events.json")["vectors"].as_array().unwrap() {
        let rx = &v["receiver"];
        let pinned = &rx["pinned_harness"];
        let got = envelope::check_event_for_test(
            v["wire"].as_str().unwrap().as_bytes(),
            rx["session_id"].as_str().unwrap(),
            pinned["harness_id"].as_str().unwrap(),
            &crypto::b64u_decode(pinned["harness_pub"].as_str().unwrap()).unwrap(),
            rx["last_applied_seq"].as_i64().unwrap(),
        );
        assert_eq!(got, v["expect"].as_str().unwrap(), "vector {}", v["name"]);
    }
}

#[test]
fn harness_signed_events_pass_the_browser_check() {
    let keys = load("keys.json");
    let k = &keys["keys"][0];
    let key = KeyPair::from_scalar(&hex(k["private_scalar_hex"].as_str().unwrap())).unwrap();
    let mut signer = envelope::EventSigner::new(key.clone(), "rs_YIL0d-r7J-AHLPpZ00o2bA");
    let body = serde_json::json!({"type": "notice", "level": "info", "code": null, "text": "é\u{2028}\"x\""});
    let env = signer.sign("chat_event", None, body, 1).unwrap();
    assert_eq!(
        envelope::check_event_for_test(
            env.as_bytes(),
            "rs_YIL0d-r7J-AHLPpZ00o2bA",
            key.id(),
            key.public_raw(),
            0
        ),
        "ok"
    );
    // Floats are not in the profile: signing refuses them.
    assert!(
        signer
            .sign("status", None, serde_json::json!({"x": 1.5}), 1)
            .is_err()
    );
}

#[test]
fn pairing_vectors() {
    let p = load("pairing.json");
    let inputs = &p["inputs"];
    let secret = crypto::b64u_decode(inputs["pairing_secret_b64u"].as_str().unwrap()).unwrap();
    let session = inputs["session_id"].as_str().unwrap();
    let harness_pub = crypto::b64u_decode(inputs["harness_pub_b64u"].as_str().unwrap()).unwrap();
    let device_pub = crypto::b64u_decode(inputs["device_pub_b64u"].as_str().unwrap()).unwrap();
    let harness_id = inputs["harness_id"].as_str().unwrap();
    let now = inputs["now_ms"].as_i64().unwrap();
    let owner = inputs["owner_user_id"].as_i64().unwrap();
    let derived = &p["derived"];
    assert_eq!(pairing::pairing_id(&secret), derived["pairing_id"]);
    assert_eq!(
        pairing::k_mac(&secret, session).to_vec(),
        hex(derived["k_mac_hex"].as_str().unwrap())
    );
    assert_eq!(
        pairing::k_sas(&secret, session).to_vec(),
        hex(derived["k_sas_hex"].as_str().unwrap())
    );
    assert_eq!(
        pairing::sas_code(&secret, session, &harness_pub, &device_pub),
        derived["sas_code"]
    );
    let link = pairing::pairing_link("https://amp.example.test", session, &secret, harness_id);
    assert_eq!(link, p["link"]);
    let (s2, h2) = pairing::parse_link_fragment(&link).unwrap();
    assert_eq!(
        (s2.as_slice(), h2.as_str()),
        (secret.as_slice(), harness_id)
    );
    // The request's signing input and MAC, byte for byte.
    let req = jcs::canonical(&p["request"]);
    let input = format!("amp-rc/1 pair-request\n{req}");
    assert_eq!(input, p["signing_input"].as_str().unwrap());
    let mac = crypto::hmac_sha256(&pairing::k_mac(&secret, session), input.as_bytes());
    assert_eq!(crypto::b64u(&mac), p["message"]["mac"]);
    assert!(crypto::verify(
        &device_pub,
        input.as_bytes(),
        p["message"]["sig"].as_str().unwrap()
    ));

    let check = |message: &Value, relay_user: i64| {
        let mut window = PairingWindow::with_secret(secret.clone(), now - 1000);
        let mut m = message.clone();
        m["relay"] = serde_json::json!({"user_id": relay_user, "email": "jsims@example.test"});
        let input = PairCheckInput {
            now_ms: now,
            session_id: session,
            harness_id,
            harness_pub: &harness_pub,
            user_id: owner,
        };
        match window.check(m.to_string().as_bytes(), &input) {
            Ok(c) => {
                assert_eq!(c.code, derived["sas_code"].as_str().unwrap());
                "ok".to_owned()
            }
            Err(r) => r
                .code
                .map_or(r.result.to_owned(), |c| c.as_str().to_owned()),
        }
    };
    assert_eq!(check(&p["message"], owner), "ok");
    for r in p["reject"].as_array().unwrap() {
        let relay_user = r["relay_user_id"].as_i64().unwrap_or(owner);
        assert_eq!(
            check(&r["message"], relay_user),
            r["expect"].as_str().unwrap(),
            "pairing vector {}",
            r["name"]
        );
    }
}

#[test]
fn pairing_tries_lock_and_terminal_text_is_sanitized() {
    let secret = vec![7u8; 32];
    let harness = KeyPair::generate();
    let device = KeyPair::generate();
    let session = "rs_AAAAAAAAAAAAAAAAAAAAAA";
    let now = 1_792_948_800_000;
    let mut window = PairingWindow::with_secret(secret.clone(), now);
    let input = PairCheckInput {
        now_ms: now,
        session_id: session,
        harness_id: harness.id(),
        harness_pub: harness.public_raw(),
        user_id: 1001,
    };
    let mut msg =
        pairing::build_pair_request(&secret, session, harness.id(), &device, "Safari", now);
    msg["relay"] = serde_json::json!({"user_id": 2002});
    // Five wrong-user tries spend the secret; the fifth locks it.
    for i in 1..=5 {
        let r = window
            .check(msg.to_string().as_bytes(), &input)
            .unwrap_err();
        if i < 5 {
            assert_eq!((r.result, r.tries_left), ("failed", Some(5 - i)));
        } else {
            assert_eq!(r.result, "locked");
        }
    }
    msg["relay"] = serde_json::json!({"user_id": 1001});
    let r = window
        .check(msg.to_string().as_bytes(), &input)
        .unwrap_err();
    assert_eq!(r.code.unwrap().as_str(), "unknown_pairing");

    // Terminal injection: control, ESC and bidi characters never reach the prompt.
    let mut window = PairingWindow::with_secret(secret.clone(), now);
    let mut msg = pairing::build_pair_request(
        &secret,
        session,
        harness.id(),
        &device,
        "Evil\u{1b}[2J\u{202e}name\u{7}",
        now,
    );
    msg["relay"] = serde_json::json!({"user_id": 1001, "email": "a@b\u{1b}]0;x\u{7}", "user_agent": "UA\u{2066}"});
    let c = window.check(msg.to_string().as_bytes(), &input).unwrap();
    assert_eq!(c.device_name, "Evil[2Jname");
    assert_eq!(c.email, "a@b]0;x");
    assert_eq!(c.user_agent, "UA");
    // Expired secret.
    let mut window = PairingWindow::with_secret(secret.clone(), now - 300_001);
    let r = window
        .check(msg.to_string().as_bytes(), &input)
        .unwrap_err();
    assert_eq!(r.code.unwrap().as_str(), "pairing_expired");
}

/// #2379, R-M1 / T-H-1: the harness side of remote control is outbound only. No source file of
/// the link (the mock relay aside, which is test-only behind the `mock-relay` feature) opens a
/// listening socket or serves HTTP.
#[test]
fn remote_link_opens_no_listening_socket() {
    let sources = [
        ("remote/mod.rs", include_str!("../src/remote/mod.rs")),
        ("remote/crypto.rs", include_str!("../src/remote/crypto.rs")),
        (
            "remote/envelope.rs",
            include_str!("../src/remote/envelope.rs"),
        ),
        ("remote/jcs.rs", include_str!("../src/remote/jcs.rs")),
        ("remote/link.rs", include_str!("../src/remote/link.rs")),
        (
            "remote/pairing.rs",
            include_str!("../src/remote/pairing.rs"),
        ),
        ("remote/policy.rs", include_str!("../src/remote/policy.rs")),
        ("remote/redact.rs", include_str!("../src/remote/redact.rs")),
        ("remote/relay.rs", include_str!("../src/remote/relay.rs")),
        (
            "amp-tui remote.rs",
            include_str!("../../amp-tui/src/remote.rs"),
        ),
        (
            "amp-harness remote.rs",
            include_str!("../../amp-harness/src/remote.rs"),
        ),
    ];
    for (name, src) in sources {
        for listener in [
            "TcpListener",
            "UdpSocket",
            "UnixListener",
            ".bind(",
            "axum::serve",
            "hyper::server",
        ] {
            assert!(
                !src.contains(listener),
                "{name} uses {listener}: remote control must stay outbound only"
            );
        }
    }
    let modules = include_str!("../src/remote/mod.rs");
    assert!(
        modules.contains("#[cfg(feature = \"mock-relay\")]\npub mod mock_relay;"),
        "the mock relay (which listens) is compiled only with the test feature"
    );
}
