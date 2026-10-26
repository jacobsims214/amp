#!/usr/bin/env python3
"""Generates the AMP remote control protocol v1 test vectors (ticket #2369).

    python3 v2/testdata/remote-control/gen_vectors.py          # rewrite the *.json files next to this script
    python3 v2/testdata/remote-control/gen_vectors.py --check  # regenerate in memory, fail if a file differs

Spec: v2/docs/remote-control/protocol.md. How to load the vectors: README.md in this directory.

Python 3.9+ standard library only. ECDSA P-256 with RFC 6979 deterministic nonces, RFC 8785 (JCS),
HKDF-SHA256 (RFC 5869) and HMAC-SHA256 are implemented here, so the output is byte-for-byte
reproducible on any machine. Every key, secret, nonce and id is derived from a fixed public label,
so none of it is a real secret. Never reuse these keys outside tests.

The implementations under test must not use deterministic signing: browsers (WebCrypto) sign with
random nonces. Test signing as a round trip (sign, then verify), and use these vectors to test
verification, canonicalization, fingerprints and the pairing derivations byte for byte.
"""

import base64
import hashlib
import hmac
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# ---------------------------------------------------------------------------------------------
# Protocol constants (protocol.md section 3)
# ---------------------------------------------------------------------------------------------

VERSION = 1
ALG = "ES256"
SIG_PREFIX_ENVELOPE = b"amp-rc/1 envelope\n"
SIG_PREFIX_PAIR = b"amp-rc/1 pair-request\n"
SIG_PREFIX_TOKEN = b"amp-rc/1 token\n"
PAIRING_ID_PREFIX = b"amp-rc/1 pairing-id\n"
HKDF_INFO_MAC = b"amp-rc/1 pair-mac"
HKDF_INFO_SAS = b"amp-rc/1 sas"

MAX_COMMAND_BYTES = 64 * 1024
MAX_EVENT_BYTES = 1024 * 1024
MAX_PROMPT_TEXT_BYTES = 48 * 1024
TS_WINDOW_MS = 60_000
MAX_SAFE_INT = 2**53 - 1

COMMAND_OPS = {"prompt", "stop", "detach"}
EVENT_OPS = {"hello", "status", "chat_event", "ack", "refused", "pair_result", "bye"}

# A fixed "now" for every vector: 2026-10-25T17:20:00Z.
NOW_MS = 1792948800000

# ---------------------------------------------------------------------------------------------
# Encoding helpers
# ---------------------------------------------------------------------------------------------


def b64u(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def b64u_dec(text: str) -> bytes:
    if "=" in text:
        raise ValueError("padding is not allowed")
    raw = base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))
    if b64u(raw) != text:
        raise ValueError("non-canonical base64url")
    return raw


def label_bytes(label: str, n: int = 32) -> bytes:
    """Deterministic test bytes for a public label. Not secret."""
    return hashlib.sha256(("amp-rc-v1 test vectors: " + label).encode()).digest()[:n]


# ---------------------------------------------------------------------------------------------
# P-256 ECDSA (FIPS 186-4 / SEC 1) with RFC 6979 nonces
# ---------------------------------------------------------------------------------------------

P = 0xFFFFFFFF00000001000000000000000000000000FFFFFFFFFFFFFFFFFFFFFFFF
A = P - 3
B = 0x5AC635D8AA3A93E7B3EBBD55769886BC651D06B0CC53B0F63BCE3C3E27D2604B
N = 0xFFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551
G = (
    0x6B17D1F2E12C4247F8BCE6E563A440F277037D812DEB33A0F4A13945D898C296,
    0x4FE342E2FE1A7F9B8EE7EB4A7C0F9E162BCE33576B315ECECBB6406837BF51F5,
)


def _add(p1, p2):
    if p1 is None:
        return p2
    if p2 is None:
        return p1
    x1, y1 = p1
    x2, y2 = p2
    if x1 == x2 and (y1 + y2) % P == 0:
        return None
    if p1 == p2:
        lam = (3 * x1 * x1 + A) * pow(2 * y1, -1, P) % P
    else:
        lam = (y2 - y1) * pow(x2 - x1, -1, P) % P
    x3 = (lam * lam - x1 - x2) % P
    return (x3, (lam * (x1 - x3) - y1) % P)


def _mul(k, pt):
    acc = None
    while k:
        if k & 1:
            acc = _add(acc, pt)
        pt = _add(pt, pt)
        k >>= 1
    return acc


def _on_curve(pt) -> bool:
    x, y = pt
    return (y * y - (x * x * x + A * x + B)) % P == 0


class Key:
    def __init__(self, label: str):
        self.label = label
        d = int.from_bytes(label_bytes("private key " + label), "big") % N
        assert d != 0
        self.d = d
        self.pub = _mul(d, G)

    @property
    def pub_raw(self) -> bytes:
        x, y = self.pub
        return b"\x04" + x.to_bytes(32, "big") + y.to_bytes(32, "big")

    @property
    def fp(self) -> str:
        return fingerprint(self.pub_raw)

    def sign(self, msg: bytes) -> bytes:
        h = hashlib.sha256(msg).digest()
        z = int.from_bytes(h, "big")
        k = _rfc6979_k(self.d, h)
        while True:
            r = _mul(k, G)[0] % N
            s = pow(k, -1, N) * (z + r * self.d) % N
            if r and s:
                return r.to_bytes(32, "big") + s.to_bytes(32, "big")
            k = (k + 1) % N  # unreachable in practice


def _rfc6979_k(d: int, h1: bytes) -> int:
    x = d.to_bytes(32, "big")
    h = (int.from_bytes(h1, "big") % N).to_bytes(32, "big")
    V = b"\x01" * 32
    K = b"\x00" * 32
    K = hmac.new(K, V + b"\x00" + x + h, hashlib.sha256).digest()
    V = hmac.new(K, V, hashlib.sha256).digest()
    K = hmac.new(K, V + b"\x01" + x + h, hashlib.sha256).digest()
    V = hmac.new(K, V, hashlib.sha256).digest()
    while True:
        V = hmac.new(K, V, hashlib.sha256).digest()
        k = int.from_bytes(V, "big")
        if 1 <= k < N:
            return k
        K = hmac.new(K, V + b"\x00", hashlib.sha256).digest()
        V = hmac.new(K, V, hashlib.sha256).digest()


def parse_pub(raw: bytes):
    if len(raw) != 65 or raw[0] != 4:
        raise ValueError("public key must be a 65-byte uncompressed SEC1 point")
    x = int.from_bytes(raw[1:33], "big")
    y = int.from_bytes(raw[33:], "big")
    if x >= P or y >= P or not _on_curve((x, y)):
        raise ValueError("point is not on P-256")
    return (x, y)


def verify(pub_raw: bytes, msg: bytes, sig: bytes) -> bool:
    if len(sig) != 64:
        return False
    try:
        q = parse_pub(pub_raw)
    except ValueError:
        return False
    r = int.from_bytes(sig[:32], "big")
    s = int.from_bytes(sig[32:], "big")
    if not (1 <= r < N and 1 <= s < N):
        return False
    z = int.from_bytes(hashlib.sha256(msg).digest(), "big")
    w = pow(s, -1, N)
    pt = _add(_mul(z * w % N, G), _mul(r * w % N, q))
    return pt is not None and pt[0] % N == r


def fingerprint(pub_raw: bytes) -> str:
    return b64u(hashlib.sha256(pub_raw).digest())


def fingerprint_display(pub_raw: bytes) -> str:
    h = hashlib.sha256(pub_raw).digest()[:8].hex().upper()
    return "-".join(h[i : i + 4] for i in range(0, 16, 4))


# ---------------------------------------------------------------------------------------------
# RFC 8785 JSON Canonicalization Scheme, restricted to the protocol's I-JSON profile:
# integers only (|n| <= 2^53-1), no floats, no lone surrogates, no duplicate keys.
# ---------------------------------------------------------------------------------------------


class Malformed(Exception):
    pass


def _jcs_str(s: str) -> str:
    out = ['"']
    for ch in s:
        c = ord(ch)
        if ch == '"':
            out.append('\\"')
        elif ch == "\\":
            out.append("\\\\")
        elif ch == "\b":
            out.append("\\b")
        elif ch == "\f":
            out.append("\\f")
        elif ch == "\n":
            out.append("\\n")
        elif ch == "\r":
            out.append("\\r")
        elif ch == "\t":
            out.append("\\t")
        elif c < 0x20:
            out.append("\\u%04x" % c)
        elif 0xD800 <= c <= 0xDFFF:
            raise Malformed("lone surrogate in string")
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def _utf16_key(k: str) -> bytes:
    return k.encode("utf-16-be", "surrogatepass")


def jcs(v) -> str:
    if v is None:
        return "null"
    if v is True:
        return "true"
    if v is False:
        return "false"
    if isinstance(v, int):
        if abs(v) > MAX_SAFE_INT:
            raise Malformed("integer outside +-(2^53-1)")
        return str(v)
    if isinstance(v, float):
        raise Malformed("non-integer numbers are not allowed")
    if isinstance(v, str):
        return _jcs_str(v)
    if isinstance(v, list):
        return "[" + ",".join(jcs(x) for x in v) + "]"
    if isinstance(v, dict):
        for k in v:
            _jcs_str(k)  # rejects lone surrogates in keys
        keys = sorted(v.keys(), key=_utf16_key)
        return "{" + ",".join(_jcs_str(k) + ":" + jcs(v[k]) for k in keys) + "}"
    raise TypeError(type(v))


def _no_dupes(pairs):
    out = {}
    for k, val in pairs:
        if k in out:
            raise Malformed("duplicate key %r" % k)
        out[k] = val
    return out


def _no_float(text):
    raise Malformed("non-integer number %s" % text)


def strict_parse(text: str):
    """Parses the wire JSON the way every receiver must: duplicate keys, non-integer numbers and
    NaN/Infinity are malformed."""
    try:
        return json.loads(
            text, object_pairs_hook=_no_dupes, parse_float=_no_float, parse_constant=_no_float
        )
    except json.JSONDecodeError as e:
        raise Malformed(str(e))


# ---------------------------------------------------------------------------------------------
# HKDF-SHA256 (RFC 5869), pairing derivations (protocol.md section 7)
# ---------------------------------------------------------------------------------------------


def hkdf(ikm: bytes, salt: bytes, info: bytes, length: int = 32) -> bytes:
    prk = hmac.new(salt, ikm, hashlib.sha256).digest()
    okm, t, i = b"", b"", 1
    while len(okm) < length:
        t = hmac.new(prk, t + info + bytes([i]), hashlib.sha256).digest()
        okm += t
        i += 1
    return okm[:length]


def pairing_id(secret: bytes) -> str:
    return b64u(hashlib.sha256(PAIRING_ID_PREFIX + secret).digest()[:16])


def k_mac(secret: bytes, session_id: str) -> bytes:
    return hkdf(secret, session_id.encode(), HKDF_INFO_MAC)


def k_sas(secret: bytes, session_id: str) -> bytes:
    return hkdf(secret, session_id.encode(), HKDF_INFO_SAS)


def sas_code(secret: bytes, session_id: str, harness_pub: bytes, device_pub: bytes) -> str:
    h = hmac.new(k_sas(secret, session_id), harness_pub + device_pub, hashlib.sha256).digest()
    n = int.from_bytes(h[:8], "big") % 100_000_000
    s = "%08d" % n
    return s[:4] + "-" + s[4:]


# ---------------------------------------------------------------------------------------------
# Test identities
# ---------------------------------------------------------------------------------------------

HARNESS = Key("harness-1")  # the laptop the browser paired with
HARNESS_OTHER = Key("harness-2")  # some other harness install
DEVICE_A = Key("device-a")  # pinned at HARNESS
DEVICE_B = Key("device-b")  # never paired (a stolen-cookie browser)
DEVICE_C = Key("device-c")  # pinned, but the 30-day pin has lapsed

OWNER_USER_ID = 1001
OTHER_USER_ID = 2002


def session_id(label: str) -> str:
    return "rs_" + b64u(label_bytes("session " + label, 16))


def nonce(label: str) -> str:
    return b64u(label_bytes("nonce " + label, 16))


SESSION = session_id("1")
SESSION_OTHER = session_id("2")
DAY_MS = 86_400_000


def key_record(k: Key) -> dict:
    return {
        "label": k.label,
        "private_scalar_hex": "%064x" % k.d,
        "public_raw_b64u": b64u(k.pub_raw),
        "public_raw_hex": k.pub_raw.hex(),
        "fingerprint": k.fp,
        "fingerprint_display": fingerprint_display(k.pub_raw),
    }


# ---------------------------------------------------------------------------------------------
# Envelopes
# ---------------------------------------------------------------------------------------------

ENVELOPE_FIELDS = ["v", "alg", "session_id", "harness_id", "device_id", "seq", "ts", "nonce", "op", "body"]


def signing_input(env: dict) -> bytes:
    unsigned = {k: v for k, v in env.items() if k != "sig"}
    return SIG_PREFIX_ENVELOPE + jcs(unsigned).encode("utf-8")


def sign_env(key: Key, env: dict) -> dict:
    env = dict(env)
    env.pop("sig", None)
    env["sig"] = b64u(key.sign(signing_input(env)))
    return env


def wire(env: dict, ascii_only: bool = False) -> str:
    """A deliberately non-canonical serialization (sig first, reversed field order, spaces), so a
    receiver that signs or verifies the bytes it received instead of JCS of the parsed value fails."""
    order = ["sig"] + [f for f in reversed(ENVELOPE_FIELDS)]
    ordered = {k: env[k] for k in order if k in env}
    for k in env:
        if k not in ordered:
            ordered[k] = env[k]
    return json.dumps(ordered, ensure_ascii=ascii_only, separators=(", ", ": "))


def command(op, body, *, seq, ts=NOW_MS - 1500, nonce_label, device=DEVICE_A, session=SESSION, harness=HARNESS):
    return {
        "v": VERSION,
        "alg": ALG,
        "session_id": session,
        "harness_id": harness.fp,
        "device_id": device.fp,
        "seq": seq,
        "ts": ts,
        "nonce": nonce(nonce_label),
        "op": op,
        "body": body,
    }


def event(op, body, *, seq, ts=NOW_MS - 800, nonce_label, device_id=None, session=SESSION, harness=HARNESS):
    return {
        "v": VERSION,
        "alg": ALG,
        "session_id": session,
        "harness_id": harness.fp,
        "device_id": device_id,
        "seq": seq,
        "ts": ts,
        "nonce": nonce(nonce_label),
        "op": op,
        "body": body,
    }


# ---------------------------------------------------------------------------------------------
# Reference receivers (normative order: protocol.md sections 5.6 and 5.7)
# ---------------------------------------------------------------------------------------------

import re

CLIENT_MSG_ID = re.compile(r"^[A-Za-z0-9_-]{1,64}$")
NONCE_RE = re.compile(r"^[A-Za-z0-9_-]{22}$")
ID_RE = re.compile(r"^[A-Za-z0-9_-]{43}$")
SESSION_RE = re.compile(r"^rs_[A-Za-z0-9_-]{22}$")


def _shape_ok(env) -> bool:
    if not isinstance(env, dict) or set(env.keys()) != set(ENVELOPE_FIELDS + ["sig"]):
        return False
    types = {
        "v": int, "alg": str, "session_id": str, "harness_id": str, "seq": int, "ts": int,
        "nonce": str, "op": str, "body": dict, "sig": str,
    }
    for k, t in types.items():
        if not isinstance(env[k], t) or isinstance(env[k], bool):
            return False
    if env["device_id"] is not None and not isinstance(env["device_id"], str):
        return False
    return True


def _body_ok(op, body) -> bool:
    if op == "prompt":
        if set(body.keys()) != {"text", "client_msg_id"}:
            return False
        t, c = body["text"], body["client_msg_id"]
        if not isinstance(t, str) or not isinstance(c, str):
            return False
        if not t.strip() or len(t.encode("utf-8")) > MAX_PROMPT_TEXT_BYTES:
            return False
        return bool(CLIENT_MSG_ID.match(c))
    if op in ("stop", "detach"):
        return body == {}
    return False


def check_command(text: str, rx: dict) -> str:
    """The harness's verdict on one command: "ok" or a refusal code."""
    if len(text.encode("utf-8")) > MAX_COMMAND_BYTES:
        return "too_large"
    try:
        env = strict_parse(text)
        if not _shape_ok(env):
            return "malformed"
        jcs(env)
    except Malformed:
        return "malformed"
    if env["v"] != VERSION:
        return "unsupported_version"
    if env["alg"] != ALG:
        return "unsupported_alg"
    if env["session_id"] != rx["session_id"]:
        return "wrong_session"
    if env["harness_id"] != rx["harness_id"]:
        return "wrong_harness"
    pin = next((d for d in rx["pinned_devices"] if d["device_id"] == env["device_id"]), None)
    if pin is None or pin["user_id"] != rx["user_id"]:
        return "unknown_device"
    if pin["pinned_until_ms"] <= rx["now_ms"]:
        return "device_expired"
    try:
        sig = b64u_dec(env["sig"])
    except ValueError:
        return "bad_signature"
    if not verify(b64u_dec(pin["device_pub"]), signing_input(env), sig):
        return "bad_signature"
    if abs(env["ts"] - rx["now_ms"]) > TS_WINDOW_MS:
        return "stale_ts"
    if env["nonce"] in rx["seen_nonces"]:
        return "replayed_nonce"
    if env["seq"] <= rx["last_seq"].get(env["device_id"], 0):
        return "seq_not_increasing"
    if env["op"] not in COMMAND_OPS:
        return "unknown_op"
    if not _body_ok(env["op"], env["body"]):
        return "invalid_body"
    return "ok"


def check_event(text: str, rx: dict) -> str:
    """The browser's verdict on one event: "ok", "duplicate", "ignored_unknown_op" or a code."""
    if len(text.encode("utf-8")) > MAX_EVENT_BYTES:
        return "too_large"
    try:
        env = strict_parse(text)
        if not _shape_ok(env):
            return "malformed"
        jcs(env)
    except Malformed:
        return "malformed"
    if env["v"] != VERSION:
        return "unsupported_version"
    if env["alg"] != ALG:
        return "unsupported_alg"
    if env["session_id"] != rx["session_id"]:
        return "wrong_session"
    if env["harness_id"] != rx["pinned_harness"]["harness_id"]:
        return "wrong_harness"
    try:
        sig = b64u_dec(env["sig"])
    except ValueError:
        return "bad_signature"
    if not verify(b64u_dec(rx["pinned_harness"]["harness_pub"]), signing_input(env), sig):
        return "bad_signature"
    if env["seq"] <= rx["last_applied_seq"]:
        return "duplicate"
    if env["op"] not in EVENT_OPS:
        return "ignored_unknown_op"
    return "ok"


# ---------------------------------------------------------------------------------------------
# Vector sets
# ---------------------------------------------------------------------------------------------


def keys_file():
    return {
        "description": "Test-only P-256 key pairs (private scalar derived from a public label: never use outside tests). "
        "public_raw is the 65-byte uncompressed SEC1 point; fingerprint = base64url(SHA-256(public_raw)) without padding; "
        "fingerprint_display = the first 8 bytes of that hash as upper-case hex in groups of 4.",
        "keys": [key_record(k) for k in (HARNESS, HARNESS_OTHER, DEVICE_A, DEVICE_B, DEVICE_C)],
    }


def jcs_file():
    cases = [
        ("key order is by UTF-16 code units, not code points", '{"\\ufb33":1,"\\ud83d\\ude00":2,"\\u20ac":3,"b":4,"a":5,"B":6}'),
        ("whitespace removed, nested objects sorted", '{ "z" : [ 1 , { "y" : true , "x" : null } ] , "a" : "s" }'),
        ("escapes: only \\\" \\\\ \\b \\f \\n \\r \\t and \\u00XX (lower-case hex) for other controls", '{"s":"q\\" b\\\\ \\b\\f\\n\\r\\t \\u0001 \\u001f \\u007f"}'),
        ("non-ASCII is raw UTF-8, never \\u-escaped (U+2028 and U+2029 included; Go's encoding/json gets this wrong)", '{"s":"\\u00e9 \\u2028 \\u2029 \\ud83d\\ude80 </script>"}'),
        ("integers", '{"a":0,"b":-1,"c":9007199254740991,"d":-9007199254740991}'),
        ("empty containers", '{"a":{},"b":[],"c":""}'),
    ]
    out = []
    for name, text in cases:
        out.append({"name": name, "input": text, "canonical": jcs(strict_parse(text))})
    rejects = [
        ("non-integer number", '{"a":1.5}'),
        ("exponent form is a non-integer number token", '{"a":1e3}'),
        ("integer outside +-(2^53-1)", '{"a":9007199254740992}'),
        ("duplicate key", '{"a":1,"a":2}'),
        ("lone surrogate", '{"a":"\\ud800"}'),
    ]
    bad = []
    for name, text in rejects:
        try:
            jcs(strict_parse(text))
            raise SystemExit("jcs reject case unexpectedly passed: " + name)
        except Malformed:
            pass
        bad.append({"name": name, "input": text})
    return {
        "description": "RFC 8785 canonicalization under the protocol's I-JSON profile. `canonical` is the exact UTF-8 text. "
        "`reject` inputs make an envelope malformed (receivers that cannot detect a case, e.g. JSON.parse and duplicate keys, "
        "see protocol.md 3.3).",
        "canonical": out,
        "reject": bad,
    }


def fingerprints_file():
    return {
        "description": "fingerprint = base64url(SHA-256(public_raw)) no padding (43 chars), used as harness_id and device_id. "
        "fingerprint_display = first 8 bytes of SHA-256(public_raw), upper-case hex, groups of 4 joined by '-'.",
        "cases": [
            {
                "public_raw_b64u": b64u(k.pub_raw),
                "sha256_hex": hashlib.sha256(k.pub_raw).hexdigest(),
                "fingerprint": k.fp,
                "fingerprint_display": fingerprint_display(k.pub_raw),
            }
            for k in (HARNESS, DEVICE_A)
        ],
    }


def harness_rx(**over):
    rx = {
        "now_ms": NOW_MS,
        "user_id": OWNER_USER_ID,
        "session_id": SESSION,
        "harness_id": HARNESS.fp,
        "pinned_devices": [
            {"device_id": DEVICE_A.fp, "device_pub": b64u(DEVICE_A.pub_raw), "user_id": OWNER_USER_ID, "pinned_until_ms": NOW_MS + 29 * DAY_MS},
            {"device_id": DEVICE_C.fp, "device_pub": b64u(DEVICE_C.pub_raw), "user_id": OWNER_USER_ID, "pinned_until_ms": NOW_MS - 1},
        ],
        "last_seq": {DEVICE_A.fp: 1792948700000},
        "seen_nonces": [nonce("seen-1")],
    }
    rx.update(over)
    return rx


def commands_file():
    vectors = []
    base_seq = 1792948798500  # max(last+1, ts) as protocol.md 5.3 recommends

    def add(name, text, rx, expect, note, env=None, show_input=False):
        got = check_command(text, rx)
        if got != expect:
            raise SystemExit("command vector %s: reference receiver says %s, expected %s" % (name, got, expect))
        v = {"name": name, "note": note, "receiver": rx, "wire": text, "expect": expect}
        if show_input and env is not None:
            v["signing_input"] = signing_input(env).decode("utf-8")
        vectors.append(v)

    prompt = command("prompt", {"text": "run the tests and tell me what fails", "client_msg_id": "m-01"}, seq=base_seq, nonce_label="c1")
    sp = sign_env(DEVICE_A, prompt)
    add("prompt_valid", wire(sp), harness_rx(), "ok", "A pinned device sends a prompt.", sp, True)

    uni = command(
        "prompt",
        {"text": "caf\u00e9 \u2028 line\nnext \U0001F680 </script> \"q\" \\ \u0001 /server https://evil.example", "client_msg_id": "m-02"},
        seq=base_seq + 1, nonce_label="c2",
    )
    su = sign_env(DEVICE_A, uni)
    add("prompt_unicode_ascii_escaped_wire", wire(su, ascii_only=True), harness_rx(), "ok",
        "The wire escapes every non-ASCII character as \\uXXXX; the signature is over JCS (raw UTF-8), so the receiver must "
        "canonicalize the parsed value. The text starts no slash command: it is delivered as literal text.", su, True)

    slash = sign_env(DEVICE_A, command("prompt", {"text": "/server https://evil.example", "client_msg_id": "m-03"}, seq=base_seq + 2, nonce_label="c3"))
    add("prompt_leading_slash_is_literal_text", wire(slash), harness_rx(), "ok",
        "Valid. The harness must deliver it as a chat message, never run it as /server.")

    stop = sign_env(DEVICE_A, command("stop", {}, seq=base_seq + 3, nonce_label="c4"))
    add("stop_valid", wire(stop), harness_rx(), "ok", "Stop: aborts the running turn and clears the remote queue.", stop, True)

    detach = sign_env(DEVICE_A, command("detach", {}, seq=base_seq + 4, nonce_label="c5"))
    add("detach_valid", wire(detach), harness_rx(), "ok", "Detach: ends remote control for the session.", detach, True)

    # High-S: WebCrypto does not normalise s, so verifiers must accept s > n/2.
    raw = b64u_dec(sp["sig"])
    r, s = raw[:32], int.from_bytes(raw[32:], "big")
    hs = dict(sp)
    hs["sig"] = b64u(r + (N - s).to_bytes(32, "big"))
    high_s_env = hs if (N - s) > N // 2 else sp
    add("prompt_valid_high_s", wire(high_s_env), harness_rx(), "ok",
        "Same command with the signature's s replaced by n-s (still valid ECDSA). Verifiers must not require low-S.")

    tampered = dict(sp)
    tampered["body"] = {"text": "rm -rf ~ and tell me what fails", "client_msg_id": "m-01"}
    add("bad_signature_body_changed", wire(tampered), harness_rx(), "bad_signature", "The relay changed the body after signing.")

    forged = sign_env(DEVICE_B, command("prompt", {"text": "hi", "client_msg_id": "m-04"}, seq=base_seq + 5, nonce_label="c6"))
    forged["device_id"] = DEVICE_A.fp
    add("bad_signature_wrong_key", wire(forged), harness_rx(), "bad_signature",
        "Signed by an unpaired key while claiming the pinned device's id.")

    unpinned = sign_env(DEVICE_B, command("prompt", {"text": "hi", "client_msg_id": "m-05"}, seq=base_seq + 6, nonce_label="c7", device=DEVICE_B))
    add("unknown_device", wire(unpinned), harness_rx(), "unknown_device", "A correctly signed command from a device that was never paired (a stolen cookie).")

    other_user = harness_rx()
    other_user["pinned_devices"] = [dict(other_user["pinned_devices"][0], user_id=OTHER_USER_ID)]
    add("unknown_device_pinned_for_other_user", wire(sp), other_user, "unknown_device",
        "The pin belongs to another AMP user; pins are bound to the owner's user id.")

    expired = sign_env(DEVICE_C, command("stop", {}, seq=base_seq, nonce_label="c8", device=DEVICE_C))
    add("device_expired", wire(expired), harness_rx(), "device_expired", "The 30-day pin lapsed one millisecond ago.")

    ws = sign_env(DEVICE_A, command("prompt", {"text": "hi", "client_msg_id": "m-06"}, seq=base_seq + 7, nonce_label="c9", session=SESSION_OTHER))
    add("wrong_session", wire(ws), harness_rx(), "wrong_session", "Validly signed for another session of the same harness (cross-session replay).")

    wh = sign_env(DEVICE_A, command("prompt", {"text": "hi", "client_msg_id": "m-07"}, seq=base_seq + 8, nonce_label="c10", harness=HARNESS_OTHER))
    add("wrong_harness", wire(wh), harness_rx(), "wrong_harness", "Validly signed for another harness install.")

    old = sign_env(DEVICE_A, command("stop", {}, seq=base_seq + 9, ts=NOW_MS - 60_001, nonce_label="c11"))
    add("stale_ts_old", wire(old), harness_rx(), "stale_ts", "ts is 60001 ms in the past.")

    edge = sign_env(DEVICE_A, command("stop", {}, seq=base_seq + 10, ts=NOW_MS - 60_000, nonce_label="c12"))
    add("ts_window_edge_ok", wire(edge), harness_rx(), "ok", "ts exactly 60000 ms old is still inside the window (|ts - now| <= 60000).")

    fut = sign_env(DEVICE_A, command("stop", {}, seq=base_seq + 11, ts=NOW_MS + 60_001, nonce_label="c13"))
    add("stale_ts_future", wire(fut), harness_rx(), "stale_ts", "ts is 60001 ms in the future.")

    rep = sign_env(DEVICE_A, command("stop", {}, seq=base_seq + 12, nonce_label="seen-1"))
    add("replayed_nonce", wire(rep), harness_rx(), "replayed_nonce", "The nonce is already in the receiver's nonce cache.")

    same_seq = sign_env(DEVICE_A, command("stop", {}, seq=1792948700000, nonce_label="c14"))
    add("seq_not_increasing_equal", wire(same_seq), harness_rx(), "seq_not_increasing", "seq equals the last accepted seq for this device.")

    lower = sign_env(DEVICE_A, command("stop", {}, seq=5, nonce_label="c15"))
    add("seq_not_increasing_lower", wire(lower), harness_rx(), "seq_not_increasing", "seq is lower than the last accepted seq.")

    unk = sign_env(DEVICE_A, command("set_config", {"key": "tool_policy", "value": "allow"}, seq=base_seq + 13, nonce_label="c16"))
    add("unknown_op", wire(unk), harness_rx(), "unknown_op", "Not on the closed allowlist.")

    reserved = sign_env(DEVICE_A, command("set_model", {"model": "anthropic/claude-x"}, seq=base_seq + 14, nonce_label="c17"))
    add("reserved_op_refused_in_m1", wire(reserved), harness_rx(), "unknown_op", "set_model is reserved for M2; an M1 harness refuses it.")

    extra = sign_env(DEVICE_A, command("prompt", {"text": "hi", "client_msg_id": "m-08", "as_slash_command": True}, seq=base_seq + 15, nonce_label="c18"))
    add("invalid_body_unknown_field", wire(extra), harness_rx(), "invalid_body", "Bodies deny unknown fields.")

    blank = sign_env(DEVICE_A, command("prompt", {"text": " \n\t", "client_msg_id": "m-09"}, seq=base_seq + 16, nonce_label="c19"))
    add("invalid_body_blank_text", wire(blank), harness_rx(), "invalid_body", "Whitespace-only prompt text.")

    stopx = sign_env(DEVICE_A, command("stop", {"workers": True}, seq=base_seq + 17, nonce_label="c20"))
    add("invalid_body_stop_with_fields", wire(stopx), harness_rx(), "invalid_body", "stop takes an empty body in v1.")

    v2 = sign_env(DEVICE_A, dict(command("stop", {}, seq=base_seq + 18, nonce_label="c21"), v=2))
    add("unsupported_version", wire(v2), harness_rx(), "unsupported_version", "Major version 2 is unknown to a v1 receiver.")

    alg = sign_env(DEVICE_A, dict(command("stop", {}, seq=base_seq + 19, nonce_label="c22"), alg="EdDSA"))
    add("unsupported_alg", wire(alg), harness_rx(), "unsupported_alg", "Only ES256 is defined in v1.")

    nosig = {k: v for k, v in stop.items() if k != "sig"}
    add("malformed_missing_sig", json.dumps(nosig), harness_rx(), "malformed", "Unsigned.")

    extra_top = dict(stop)
    extra_top["reply_to"] = "x"
    add("malformed_unknown_top_level_field", wire(extra_top), harness_rx(), "malformed", "The envelope has exactly the 11 fields.")

    flt = wire(stop).replace('"seq": %d' % stop["seq"], '"seq": %d.0' % stop["seq"])
    add("malformed_float", flt, harness_rx(), "malformed", "seq written as a non-integer number token.")

    dup = wire(stop).replace('"op": "stop"', '"op": "prompt", "op": "stop"')
    add("malformed_duplicate_key", dup, harness_rx(), "malformed", "Duplicate key in the envelope.")

    big = sign_env(DEVICE_A, command("prompt", {"text": "a" * 70_000, "client_msg_id": "m-10"}, seq=base_seq + 20, nonce_label="c23"))
    add("too_large", wire(big), harness_rx(), "too_large", "The command is over 65536 bytes.")

    return {
        "description": "Browser -> harness commands. Run each `wire` text through the harness's command check with the "
        "`receiver` state (now_ms is the receiver's clock) and expect `expect` (\"ok\" or a refusal code from protocol.md 9.2). "
        "Checks run in the order of protocol.md 5.6, so exactly one code is expected. `signing_input` is the exact UTF-8 "
        "string that was signed (prefix + JCS).",
        "vectors": vectors,
    }


def browser_rx(**over):
    rx = {
        "now_ms": NOW_MS,
        "session_id": SESSION,
        "pinned_harness": {"harness_id": HARNESS.fp, "harness_pub": b64u(HARNESS.pub_raw)},
        "last_applied_seq": 41,
    }
    rx.update(over)
    return rx


def events_file():
    vectors = []

    def add(name, text, rx, expect, note, env=None, show_input=False):
        got = check_event(text, rx)
        if got != expect:
            raise SystemExit("event vector %s: reference receiver says %s, expected %s" % (name, got, expect))
        v = {"name": name, "note": note, "receiver": rx, "wire": text, "expect": expect}
        if show_input and env is not None:
            v["signing_input"] = signing_input(env).decode("utf-8")
        vectors.append(v)

    hello = sign_env(HARNESS, event("hello", {
        "harness": {
            "harness_id": HARNESS.fp,
            "harness_pub": b64u(HARNESS.pub_raw),
            "host": "jacobs-mbp",
            "os": "macos",
            "version": "0.4.0",
        },
        "user_id": OWNER_USER_ID,
        "ops": ["prompt", "stop", "detach"],
        "started_at_ms": NOW_MS - 3_600_000,
        "expires_at_ms": NOW_MS + 39_600_000,
        "idle_timeout_s": 1800,
        "status": {
            "model": "anthropic/claude-opus-5-5",
            "busy": False,
            "context_pct": 37,
            "cost_usd_micros": 1834200,
            "project": {"id": 2, "name": "AMP"},
            "ticket_id": 2369,
            "branch": "remote-m1-2369",
            "queue_len": 0,
            "workers_running": 1,
            "devices": [{"device_id": DEVICE_A.fp, "name": "Safari on iPhone"}],
        },
    }, seq=42, nonce_label="e1"))
    add("hello_valid", wire(hello), browser_rx(), "ok", "The status snapshot sent at attach.", hello, True)

    delta = sign_env(HARNESS, event("chat_event", {"type": "text_delta", "message_id": "msg_7", "delta": "Running the tests\u2026 \U0001F9EA"}, seq=43, nonce_label="e2"))
    add("chat_text_delta_valid", wire(delta, ascii_only=True), browser_rx(last_applied_seq=42), "ok", "A coalesced text delta; wire is ASCII-escaped.", delta, True)

    tool = sign_env(HARNESS, event("chat_event", {
        "type": "tool_start", "message_id": "msg_7", "tool_call_id": "call_1", "name": "bash",
        "arguments_json": "{\"command\":\"cargo test -p amp-core\",\"timeout\":1.5}", "truncated": False,
    }, seq=44, nonce_label="e3"))
    add("chat_tool_start_valid", wire(tool), browser_rx(last_applied_seq=43), "ok",
        "Tool arguments travel as a JSON string (floats inside it are fine: it is a string).", tool, True)

    mend = sign_env(HARNESS, event("chat_event", {
        "type": "message_end", "message_id": "msg_7", "stop_reason": "tool_use", "error": None, "model": "claude-opus-5-5", "provider": "anthropic",
        "usage": {"input_tokens": 12034, "output_tokens": 311, "cache_read_tokens": 90000, "cache_write_tokens": 0, "cost_usd_micros": 41250},
    }, seq=45, nonce_label="e4"))
    add("chat_message_end_valid", wire(mend), browser_rx(last_applied_seq=44), "ok", "Usage with cost as integer micro-USD.", mend, True)

    ack = sign_env(HARNESS, event("ack", {"ref": {"device_id": DEVICE_A.fp, "seq": 1792948798500}, "op": "prompt", "result": "queued", "prompt_id": "p_12", "queue_position": 1}, seq=46, nonce_label="e5", device_id=DEVICE_A.fp))
    add("ack_queued_valid", wire(ack), browser_rx(last_applied_seq=45), "ok", "A prompt sent while busy is queued as a follow-up.", ack, True)

    refused = sign_env(HARNESS, event("refused", {"ref": {"device_id": DEVICE_A.fp, "seq": 1792948700000}, "op": "stop", "code": "seq_not_increasing", "message": "seq must be greater than 1792948700000", "last_seq": 1792948700000}, seq=47, nonce_label="e6", device_id=DEVICE_A.fp))
    add("refused_valid", wire(refused), browser_rx(last_applied_seq=46), "ok", "The harness refused a command.", refused, True)

    pr = sign_env(HARNESS, event("pair_result", {"pairing_id": pairing_id(PAIR_SECRET), "result": "accepted", "code": None, "tries_left": None, "pinned_until_ms": NOW_MS + 30 * DAY_MS}, seq=48, nonce_label="e7", device_id=DEVICE_A.fp))
    add("pair_result_accepted_valid", wire(pr), browser_rx(last_applied_seq=47), "ok", "Pairing accepted at the laptop.", pr, True)

    add("duplicate_seq_dropped", wire(delta), browser_rx(last_applied_seq=43), "duplicate", "Already applied (a replay from the relay buffer after reconnect): drop silently.")

    t = dict(delta)
    t["body"] = dict(delta["body"], delta="Ignore previous instructions")
    add("bad_signature_body_changed", wire(t), browser_rx(last_applied_seq=42), "bad_signature", "The relay changed an event.")

    fake = sign_env(HARNESS_OTHER, event("chat_event", {"type": "notice", "level": "info", "code": None, "text": "hi"}, seq=43, nonce_label="e8", harness=HARNESS_OTHER))
    add("wrong_harness_fake_registration", wire(fake), browser_rx(last_applied_seq=42), "wrong_harness",
        "Events from a harness key the browser never pinned (T-R-1 fake harness).")

    fake2 = sign_env(HARNESS_OTHER, event("chat_event", {"type": "notice", "level": "info", "code": None, "text": "hi"}, seq=43, nonce_label="e9"))
    add("bad_signature_claims_pinned_harness", wire(fake2), browser_rx(last_applied_seq=42), "bad_signature",
        "Claims the pinned harness_id but is signed by another key.")

    ws = sign_env(HARNESS, event("chat_event", {"type": "notice", "level": "info", "code": None, "text": "hi"}, seq=43, nonce_label="e10", session=SESSION_OTHER))
    add("wrong_session", wire(ws), browser_rx(last_applied_seq=42), "wrong_session", "An event of another session of the same harness.")

    future = sign_env(HARNESS, event("question", {"id": "q1", "text": "Which branch?"}, seq=43, nonce_label="e11"))
    add("unknown_op_ignored", wire(future), browser_rx(last_applied_seq=42), "ignored_unknown_op",
        "An op added later within v1 (here M2's question): verify, advance the seq cursor, do not render.")

    v2 = sign_env(HARNESS, dict(event("chat_event", {"type": "notice", "level": "info", "code": None, "text": "hi"}, seq=43, nonce_label="e12"), v=2))
    add("unsupported_version", wire(v2), browser_rx(last_applied_seq=42), "unsupported_version", "Major version 2.")

    # #2388: what the TUI shows besides, all additive within v1 (an older harness omits these members; the
    # vectors above keep the older shapes).
    st = sign_env(HARNESS, event("status", {
        "model": "anthropic/claude-opus-5-5",
        "busy": True,
        "context_pct": 41,
        "cost_usd_micros": 2104400,
        "project": {"id": 2, "name": "AMP"},
        "ticket_id": 2388,
        "branch": "remote-m1-2388",
        "queue_len": 0,
        "workers_running": 1,
        "devices": [{"device_id": DEVICE_A.fp, "name": "Safari on iPhone"}],
        "ticket_name": "RC-M1 harness + UI: ChatEvent carries everything the TUI shows",
        "ticket_state": "in_progress",
        "epic": "Remote control",
        "story": "M1",
        "usage": {"input_tokens": 120340, "output_tokens": 8311, "cache_read_tokens": 901200, "cache_write_tokens": 0},
        "thinking": "high",
        "role": "manager",
        "cwd": "amp",
        "board": {"in_progress": 3, "ready": 5, "question": 1, "scheduled": 0},
        "plan_approval": {"yours": 2, "others": 0},
        "amp_connection": {"state": "slow", "latency_ms": 812},
    }, seq=49, nonce_label="e13"))
    add("status_tui_facts_valid", wire(st), browser_rx(last_applied_seq=48), "ok",
        "#2388: a status with the TUI's header, footer and status-line facts (ticket name/state/epic/story, session "
        "token totals, thinking, role, cwd's last component, board counts, plan approval, the harness's AMP "
        "connection). Integers only.", st, True)

    ws2 = sign_env(HARNESS, event("chat_event", {"type": "worker_status", "workers": [
        {"task_id": 2390, "name": "Watch workers", "role": "amp-worker-backend", "state": "running",
         "model": "openrouter/deepseek/deepseek-v4.1", "started_at_ms": NOW_MS - 192_000, "ended_at_ms": None,
         "usage": {"input_tokens": 6100, "output_tokens": 1200, "cache_read_tokens": 500, "cache_write_tokens": 0, "cost_usd_micros": 40000},
         "context_tokens": 10240, "context_window": 128000, "epic": "QA3d epic", "story": "Watch workers",
         "ticket_state": "in_progress"},
        {"task_id": 2391, "name": "Docs", "role": "amp-worker-docs", "state": "done",
         "model": None, "started_at_ms": NOW_MS - 400_000, "ended_at_ms": NOW_MS - 210_000,
         "usage": None, "context_tokens": None, "context_window": None, "epic": None, "story": None,
         "ticket_state": "done"},
    ]}, seq=50, nonce_label="e14"))
    add("chat_worker_status_tui_facts_valid", wire(ws2), browser_rx(last_applied_seq=49), "ok",
        "#2388: worker chips with tokens/cost, context, ended time, epic \u203a story and the ticket's state.", ws2, True)

    comp = sign_env(HARNESS, event("chat_event", {"type": "compaction", "active": True, "manual": False}, seq=51, nonce_label="e15"))
    add("chat_compaction_valid", wire(comp), browser_rx(last_applied_seq=50), "ok",
        "#2388: the harness is compacting the context (automatic).", comp, True)

    merr = sign_env(HARNESS, event("chat_event", {
        "type": "message_end", "message_id": "msg_8", "stop_reason": "error",
        "error": "http error 429: {\"error\":{\"message\":\"Rate limit exceeded: free-models-per-day\",\"code\":429}}",
        "model": "deepseek/deepseek-v4.1", "provider": "openrouter",
        "usage": {"input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0, "cache_write_tokens": 0, "cost_usd_micros": 0},
        "error_summary": "OpenRouter rate-limited the request (429): Rate limit exceeded: free-models-per-day. Wait a minute and try again, or pick another model.",
    }, seq=52, nonce_label="e16"))
    add("chat_message_end_error_summary_valid", wire(merr), browser_rx(last_applied_seq=51), "ok",
        "#2388: a failed reply with the TUI's one sentence (error_summary) and the raw provider text (error) as detail.", merr, True)

    tref = sign_env(HARNESS, event("chat_event", {
        "type": "tool_end", "tool_call_id": "call_9", "name": "amp_approve_plan", "is_error": True,
        "text": "Approving the plan is done by a person in the AMP web UI.", "truncated": False, "full_bytes": 57,
        "refused_line": "AMP refused: this needs a person in the AMP web UI (below)",
    }, seq=53, nonce_label="e17"))
    add("chat_tool_end_refused_valid", wire(tref), browser_rx(last_applied_seq=52), "ok",
        "#2388: an AMP call refused for a person's step carries the TUI's fixed row line.", tref, True)

    # #2424: by default a session has no lifetime and no idle limit; hello says so with nulls.
    hello_nl = sign_env(HARNESS, event("hello", {
        "harness": {
            "harness_id": HARNESS.fp,
            "harness_pub": b64u(HARNESS.pub_raw),
            "host": "jacobs-mbp",
            "os": "macos",
            "version": "0.5.0",
        },
        "user_id": OWNER_USER_ID,
        "ops": ["prompt", "stop", "detach"],
        "started_at_ms": NOW_MS - 3_600_000,
        "expires_at_ms": None,
        "idle_timeout_s": None,
        "status": {"model": "anthropic/claude-opus-5-5", "busy": False, "queue_len": 0, "workers_running": 0,
                   "devices": [{"device_id": DEVICE_A.fp, "name": "Safari on iPhone"}]},
    }, seq=54, nonce_label="e18"))
    add("hello_no_limits_valid", wire(hello_nl), browser_rx(last_applied_seq=53), "ok",
        "#2424: a session with no lifetime and no idle limit (the default): expires_at_ms and idle_timeout_s are null.",
        hello_nl, True)

    return {
        "description": "Harness -> browser events. Run each `wire` text through the browser's event check with `receiver` and "
        "expect `expect`: \"ok\", \"duplicate\" (drop silently), \"ignored_unknown_op\" (verified, seq advanced, not rendered) or "
        "a rejection code. Order: protocol.md 5.7. Events have no ts window (they can be replayed from the 10-minute buffer).",
        "vectors": vectors,
    }


PAIR_SECRET = label_bytes("pairing secret 1", 32)


def pair_request(device: Key, *, secret=PAIR_SECRET, ts=NOW_MS - 2000, nonce_label="p1", name="Safari on iPhone"):
    return {
        "type": "pair_request",
        "v": VERSION,
        "session_id": SESSION,
        "harness_id": HARNESS.fp,
        "pairing_id": pairing_id(secret),
        "device_id": device.fp,
        "device_pub": b64u(device.pub_raw),
        "device_name": name,
        "ts": ts,
        "nonce": nonce(nonce_label),
    }


def pair_message(req: dict, signer: Key, mac_secret: bytes):
    si = SIG_PREFIX_PAIR + jcs(req).encode("utf-8")
    mac = hmac.new(k_mac(mac_secret, req["session_id"]), si, hashlib.sha256).digest()
    return {"request": req, "mac": b64u(mac), "sig": b64u(signer.sign(si))}


def pairing_file():
    secret = PAIR_SECRET
    req = pair_request(DEVICE_A)
    si = SIG_PREFIX_PAIR + jcs(req).encode("utf-8")
    msg = pair_message(req, DEVICE_A, secret)
    sas = sas_code(secret, SESSION, HARNESS.pub_raw, DEVICE_A.pub_raw)
    link = "https://amp.example.test/sessions/%s#p=%s&h=%s" % (SESSION, b64u(secret), HARNESS.fp)

    def check(m, *, expected_pairing=req["pairing_id"], relay_user=OWNER_USER_ID, now=NOW_MS):
        r = m["request"]
        if r["session_id"] != SESSION:
            return "wrong_session"
        if r["harness_id"] != HARNESS.fp:
            return "wrong_harness"
        if r["pairing_id"] != expected_pairing:
            return "unknown_pairing"
        if relay_user != OWNER_USER_ID:
            return "wrong_user"
        if fingerprint(b64u_dec(r["device_pub"])) != r["device_id"]:
            return "device_id_mismatch"
        if abs(r["ts"] - now) > TS_WINDOW_MS:
            return "stale_ts"
        s_in = SIG_PREFIX_PAIR + jcs(r).encode("utf-8")
        want = hmac.new(k_mac(secret, SESSION), s_in, hashlib.sha256).digest()
        if not hmac.compare_digest(want, b64u_dec(m["mac"])):
            return "bad_mac"
        if not verify(b64u_dec(r["device_pub"]), s_in, b64u_dec(m["sig"])):
            return "bad_signature"
        return "ok"

    assert check(msg) == "ok"
    rejects = []

    def rej(name, m, code, note, **kw):
        got = check(m, **kw)
        if got != code:
            raise SystemExit("pairing vector %s: got %s want %s" % (name, got, code))
        rejects.append({"name": name, "note": note, "message": m, "relay_user_id": kw.get("relay_user", OWNER_USER_ID), "expect": code})

    wrong_secret = label_bytes("a guessed secret", 32)
    rej("bad_mac_wrong_secret", pair_message(req, DEVICE_A, wrong_secret), "bad_mac", "MAC made with a secret that is not the link's.")
    swapped = dict(req, device_pub=b64u(DEVICE_B.pub_raw), device_id=DEVICE_B.fp)
    rej("bad_signature_key_not_held", {"request": swapped, "mac": pair_message(swapped, DEVICE_A, secret)["mac"], "sig": pair_message(swapped, DEVICE_A, secret)["sig"]},
        "bad_signature", "Valid MAC, but the signature is not by device_pub (no proof of possession).")
    mism = dict(req, device_id=DEVICE_B.fp)
    rej("device_id_mismatch", pair_message(mism, DEVICE_A, secret), "device_id_mismatch", "device_id is not the fingerprint of device_pub.")
    rej("unknown_pairing", pair_message(dict(req, pairing_id=pairing_id(wrong_secret)), DEVICE_A, secret), "unknown_pairing", "pairing_id of another (or a consumed) secret.")
    rej("stale_ts", pair_message(pair_request(DEVICE_A, ts=NOW_MS - 61_000, nonce_label="p2"), DEVICE_A, secret), "stale_ts", "Request ts 61 s old.")
    rej("wrong_user", msg, "wrong_user", "The relay says the requester is another AMP user.", relay_user=OTHER_USER_ID)

    return {
        "description": "Pairing derivations (protocol.md section 7). All byte strings are base64url without padding unless "
        "named *_hex. Reproduce pairing_id, k_mac, k_sas, mac, sas_code byte for byte; verify sig (ECDSA is randomized in "
        "browsers, so test your own signing as a round trip). `reject` cases list the harness's expected code.",
        "inputs": {
            "pairing_secret_b64u": b64u(secret),
            "session_id": SESSION,
            "harness_pub_b64u": b64u(HARNESS.pub_raw),
            "harness_id": HARNESS.fp,
            "device_pub_b64u": b64u(DEVICE_A.pub_raw),
            "device_id": DEVICE_A.fp,
            "now_ms": NOW_MS,
            "owner_user_id": OWNER_USER_ID,
        },
        "link": link,
        "derived": {
            "pairing_id": pairing_id(secret),
            "k_mac_hex": k_mac(secret, SESSION).hex(),
            "k_sas_hex": k_sas(secret, SESSION).hex(),
            "sas_code": sas,
        },
        "request": req,
        "signing_input": si.decode("utf-8"),
        "message": msg,
        "reject": rejects,
    }


# ---------------------------------------------------------------------------------------------
# Token refresh proof (protocol.md 8.2, #2390)
# ---------------------------------------------------------------------------------------------

TOKEN_PROOF_FIELDS = ["v", "alg", "session_id", "harness_id", "ts", "nonce", "sig"]


def token_proof(signer: Key, *, session=SESSION, harness=HARNESS, ts=NOW_MS - 500, nonce_label="t1", v=VERSION, alg=ALG):
    proof = {"v": v, "alg": alg, "session_id": session, "harness_id": harness.fp, "ts": ts, "nonce": nonce(nonce_label)}
    proof["sig"] = b64u(signer.sign(SIG_PREFIX_TOKEN + jcs(proof).encode("utf-8")))
    return proof


def _high_s(signed: dict) -> dict:
    """The same signature with s as n - s when that is the high one (verifiers must accept high-S)."""
    raw = b64u_dec(signed["sig"])
    r, s = raw[:32], int.from_bytes(raw[32:], "big")
    return signed if s > N // 2 else dict(signed, sig=b64u(r + (N - s).to_bytes(32, "big")))


def check_token(text: str, rx: dict) -> str:
    """The relay's refresh-proof check, in the normative order of protocol.md 8.2."""
    try:
        body = strict_parse(text)
    except (ValueError, Malformed):
        return "malformed"
    if not isinstance(body, dict) or set(body.keys()) != {"proof"}:
        return "malformed"
    p = body["proof"]
    if not isinstance(p, dict) or set(p.keys()) != set(TOKEN_PROOF_FIELDS):
        return "malformed"
    ints = ("v", "ts")
    for k in TOKEN_PROOF_FIELDS:
        if k in ints:
            if not isinstance(p[k], int) or isinstance(p[k], bool):
                return "malformed"
        elif not isinstance(p[k], str):
            return "malformed"
    if not NONCE_RE.match(p["nonce"]):
        return "malformed"
    if p["v"] != VERSION:
        return "unsupported_version"
    if p["alg"] != ALG:
        return "unsupported_alg"
    if p["session_id"] != rx["session_id"]:
        return "wrong_session"
    if p["harness_id"] != rx["harness_id"]:
        return "wrong_harness"
    unsigned = {k: v for k, v in p.items() if k != "sig"}
    try:
        sig = b64u_dec(p["sig"])
    except ValueError:
        return "bad_signature"
    if len(sig) != 64 or not verify(b64u_dec(rx["harness_pub"]), SIG_PREFIX_TOKEN + jcs(unsigned).encode("utf-8"), sig):
        return "bad_signature"
    if abs(p["ts"] - rx["now_ms"]) > TS_WINDOW_MS:
        return "stale_ts"
    if p["nonce"] in rx["seen_nonces"]:
        return "replayed_nonce"
    return "ok"


def token_file():
    rx = {"now_ms": NOW_MS, "session_id": SESSION, "harness_id": HARNESS.fp, "harness_pub": b64u(HARNESS.pub_raw), "seen_nonces": []}
    vectors = []

    def add(name, proof_or_text, expect, note, **over):
        text = proof_or_text if isinstance(proof_or_text, str) else json.dumps({"proof": proof_or_text}, separators=(", ", ": "))
        r = dict(rx, **over)
        got = check_token(text, r)
        if got != expect:
            raise SystemExit("token vector %s: got %s want %s" % (name, got, expect))
        vectors.append({"name": name, "note": note, "receiver": r, "body": text, "expect": expect})

    good = token_proof(HARNESS)
    unsigned = {k: v for k, v in good.items() if k != "sig"}
    add("valid", good, "ok", "Signed by the session's registered harness key.")
    add("valid_high_s", _high_s(good), "ok", "The same proof with s replaced by n - s: verifiers accept high-S.")
    add("malformed_extra_member", '{"proof": %s, "x": 1}' % json.dumps(good), "malformed", "A member besides proof.")
    add("malformed_missing_nonce", {k: v for k, v in good.items() if k != "nonce"}, "malformed", "No nonce.")
    add("malformed_bad_nonce", token_proof(HARNESS, nonce_label="t2") | {"nonce": "short"}, "malformed", "The nonce isn't 16 bytes of b64u.")
    add("malformed_float_ts", '{"proof": %s}' % json.dumps(good).replace('"ts": %d' % good["ts"], '"ts": %d.0' % good["ts"]), "malformed",
        "ts written as a float (profile 3.3).")
    add("unsupported_version", token_proof(HARNESS, v=2, nonce_label="t3"), "unsupported_version", "v is 2.")
    add("unsupported_alg", token_proof(HARNESS, alg="EdDSA", nonce_label="t4"), "unsupported_alg", "alg isn't ES256.")
    add("wrong_session", token_proof(HARNESS, session=SESSION_OTHER, nonce_label="t5"), "wrong_session", "A proof for another session.")
    add("wrong_harness", token_proof(HARNESS_OTHER, harness=HARNESS_OTHER, nonce_label="t6"), "wrong_harness",
        "Another harness's own proof: it isn't the session's registered key.")
    add("bad_signature_other_key", token_proof(HARNESS_OTHER, nonce_label="t7"), "bad_signature",
        "Claims the session's harness_id but is signed by another key (someone with the owner's login, not the key).")
    add("bad_signature_envelope_prefix", unsigned | {"sig": b64u(HARNESS.sign(SIG_PREFIX_ENVELOPE + jcs(unsigned).encode("utf-8")))},
        "bad_signature", "Signed under the envelope prefix, not the token prefix (domain separation).")
    add("stale_ts", token_proof(HARNESS, ts=NOW_MS - 61_000, nonce_label="t8"), "stale_ts", "61 s old.")
    add("replayed_nonce", good, "replayed_nonce", "A proof the relay already accepted within 120 s.", seen_nonces=[good["nonce"]])
    return {
        "description": "Token refresh proofs (protocol.md 8.2): the body of POST /relay/v1/sessions/:id/token. Run each `body` "
        "through the relay's proof check with `receiver` (now_ms, the session in the path, its registered harness key, the "
        "nonces seen in the last 120 s) and expect `expect`. Signing input: \"amp-rc/1 token\\n\" + JCS(proof without sig).",
        "signing_input": (SIG_PREFIX_TOKEN + jcs(unsigned).encode("utf-8")).decode("utf-8"),
        "vectors": vectors,
    }


def generate() -> dict:
    files = {
        "keys.json": keys_file(),
        "jcs.json": jcs_file(),
        "fingerprints.json": fingerprints_file(),
        "commands.json": commands_file(),
        "events.json": events_file(),
        "pairing.json": pairing_file(),
        "token.json": token_file(),
    }
    gen = {"generator": "v2/testdata/remote-control/gen_vectors.py", "protocol": "v2/docs/remote-control/protocol.md", "protocol_version": VERSION}
    return {name: json.dumps(dict(_meta=gen, **body), indent=2, ensure_ascii=False) + "\n" for name, body in files.items()}


def main() -> int:
    out = generate()
    if "--check" in sys.argv[1:]:
        bad = []
        for name, text in out.items():
            path = os.path.join(HERE, name)
            try:
                with open(path, encoding="utf-8") as f:
                    if f.read() != text:
                        bad.append(name)
            except FileNotFoundError:
                bad.append(name)
        if bad:
            print("out of date (rerun gen_vectors.py): " + ", ".join(bad), file=sys.stderr)
            return 1
        print("vectors are up to date")
        return 0
    for name, text in out.items():
        with open(os.path.join(HERE, name), "w", encoding="utf-8") as f:
            f.write(text)
        print("wrote", name)
    return 0


if __name__ == "__main__":
    sys.exit(main())
