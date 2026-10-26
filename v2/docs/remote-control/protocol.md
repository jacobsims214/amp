# AMP remote control: wire protocol v1 (M1)

**Status:** contract for milestone M1 (epic 165, story 402). Ticket #2369. Written 2026-10-25. Session limits
amended by #2424 (2026-10-26): no idle limit or lifetime by default (section 12.5).
**Implemented by:** the Rust harness (`v2/harness`), the Go relay service amp-relay (`v2/amp-api/cmd/relay`,
its own binary, image and Deployment since #2387; the code is shared in the `v2/amp-api` module) and the
TypeScript web app (`v2/ui`). All three test suites load the shared vectors in
[`v2/testdata/remote-control/`](../../testdata/remote-control/README.md).

**Design sources:** the KB docs `decisions/harness-remote-control.md` (the design; user decisions D1-D3),
`discoveries/remote-control-threat-model.md` (requirements R-M1..R-M16), `discoveries/remote-control-amp-fit.md`
and `discoveries/remote-control-prior-art.md`. The six open questions in the decision doc take its
recommended answers:

1. A prompt sent while the agent is busy is **queued** as a follow-up and runs when the current run ends.
2. AMP keeps **no transcript**, only an in-memory resync buffer.
3. Only the **owner** can watch or drive a session.
4. Remote control is **off by default** at the org level (and the instance level, `relay.enabled: false`).
5. M1 is chat, Stop, tool calls and status. Questions, workers, model switching and the device list come in M2.
6. Push notifications come later (M3).

The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119. Wherever this document disagrees with
the vectors, the vectors were generated from the reference checks in `gen_vectors.py`, so fix whichever one is
wrong and regenerate. Don't special-case either one.

---

## Contents

1. [Overview](#1-overview)
2. [Identifiers and encodings](#2-identifiers-and-encodings)
3. [Cryptography](#3-cryptography)
4. [The envelope](#4-the-envelope)
5. [Commands (browser to harness)](#5-commands-browser-to-harness)
6. [Events (harness to browser)](#6-events-harness-to-browser)
7. [Pairing](#7-pairing)
8. [HTTP API](#8-http-api)
9. [Error and refusal codes](#9-error-and-refusal-codes)
10. [Limits and timeouts](#10-limits-and-timeouts)
11. [Buffering, resume and presence](#11-buffering-resume-and-presence)
12. [Session lifecycle, policy and audit](#12-session-lifecycle-policy-and-audit)
13. [Versioning and compatibility](#13-versioning-and-compatibility)
14. [Sequence diagrams](#14-sequence-diagrams)
15. [Notes for implementers](#15-notes-for-implementers)
16. [Test vectors](#16-test-vectors)

---

## 1. Overview

```mermaid
flowchart LR
  B[Browser / phone<br/>amp-ui /sessions/:id<br/>non-extractable P-256 device key] -- "GET /api/sessions/:id/events (SSE)<br/>POST /api/sessions/:id/commands" --> R
  R[amp-relay<br/>owner authz, registry,<br/>in-memory buffer, audit] -- "GET /relay/v1/sessions/:id/commands (SSE)" --> H
  H[amp-harness on the laptop<br/>P-256 harness key in keychain<br/>verifies commands, signs events] -- "POST /relay/v1/sessions/:id/events" --> R
```

- **Transport:** HTTPS only. Server-sent events (SSE) carry streams, and POST carries sends, on both legs.
  There is no WebSocket and no listening socket on the laptop (R-M1).
- **Two legs:**
  - **Harness leg** `/relay/v1/...`: bearer auth. Envoy runs it with ext_authz off, and amp-relay verifies the
    token itself, with the same MCP-audience verifier amp-api uses for `/mcp`.
  - **Browser leg** `/api/sessions/...`: the normal oauth2-proxy cookie through Envoy ext_authz. Every call
    requires a person in the web UI (`Channel == ui`).
- **End-to-end authentication, not end-to-end encryption.** The browser signs every command with a key that the
  harness pinned during a pairing confirmed at the laptop. The harness signs every event with its own key, which
  the browser pinned from the pairing link. The relay can read the traffic, because it is TLS only, but it cannot
  forge, change or replay anything.
- **The relay never stores payloads.** Events live only in a bounded in-memory buffer (section 11). Logs and
  audit rows hold metadata only (D3). The only members of a harness event the relay writes to Postgres are the
  status's `model`, `ticket_id` and `ticket_name` (section 6.3), for the session list (section 8.3).

## 2. Identifiers and encodings

`b64u` means base64url (RFC 4648 section 5) **without padding**. Decoders MUST reject padding characters and
non-canonical trailing bits. In Go that is `base64.RawURLEncoding.Strict()`; in Rust, the `base64` crate's
`URL_SAFE_NO_PAD`; in TS, a strict decoder (`Uint8Array.fromBase64(s, {alphabet: 'base64url', lastChunkHandling: 'strict'})`
or an equivalent check).

| Name | Format | Example / notes |
|---|---|---|
| `session_id` | `rs_` + b64u(16 random bytes): 25 chars, `^rs_[A-Za-z0-9_-]{22}$` | Minted by the relay at registration. Unguessable, but **not** a bearer secret: authz never relies on it. |
| `harness_id` | Fingerprint of the harness public key (below), 43 chars | Stable for one harness install. |
| `device_id` | Fingerprint of the device (browser) public key, 43 chars | Stable for one browser profile on one AMP origin. |
| Fingerprint | b64u(SHA-256(public key raw bytes)), 43 chars | Self-certifying: a receiver MUST check `fp(pub) == id` wherever it learns a key. |
| Display fingerprint | The first 8 bytes of SHA-256(raw pub), upper-case hex, in groups of 4 joined by `-` | `29CA-E6B5-00C2-66CB` (`harness-1` in `keys.json`). For people only. |
| Public key ("raw") | 65-byte uncompressed SEC1 point `0x04 ‖ X ‖ Y`, carried as b64u (87 chars) | WebCrypto `exportKey('raw')`; Go `ecdsa.ParseUncompressedPublicKey` (Go 1.25+); Rust `p256::PublicKey::from_sec1_bytes`. Compressed points MUST be rejected. |
| `nonce` | b64u(16 random bytes), 22 chars | A CSPRNG for every envelope. |
| `seq` | Integer, 1 ≤ seq ≤ 2^53−1 | Section 5.3 (commands) and 6.1 (events). |
| `ts`, `*_ms` | Integer Unix time in **milliseconds** | |
| `*_s` | Integer seconds | |
| Pairing secret | 32 random bytes, b64u (43 chars) | Lives only in the link fragment and in harness memory. |
| `pairing_id` | b64u(SHA-256(`"amp-rc/1 pairing-id\n"` ‖ secret)[0..16]), 22 chars | Lets the relay count tries without knowing the secret. |
| `connect_token` | `rct_` + b64u(32 random bytes) | Opaque. Stored hashed (SHA-256) by the relay. Never logged; log a prefix only. |
| `prompt_id`, `message_id`, `run_id` | Harness-chosen strings ≤ 64 chars, `^[A-Za-z0-9_-]+$` | For example `p_12`, `msg_7`, `run_3`. |
| `client_msg_id` | Browser-chosen, `^[A-Za-z0-9_-]{1,64}$` | Correlates the UI's optimistic message with its `ack`. |

All JSON is UTF-8. Section 3.3 sets the profile every envelope must follow.

## 3. Cryptography

### 3.1 Algorithms

| Use | Algorithm |
|---|---|
| Signatures, both directions | **ECDSA over P-256 with SHA-256**, `alg: "ES256"`. The signature is IEEE P1363 `r ‖ s` (64 bytes, each half big-endian and left-padded to 32 bytes), carried as b64u (86 chars). It is **not** DER. |
| Fingerprints | SHA-256 |
| Pairing MAC | HMAC-SHA256 with a key from HKDF-SHA256 (RFC 5869) |
| Short authentication string (SAS) | HMAC-SHA256 with a key from HKDF-SHA256, reduced to 8 decimal digits |

**Browser key.** A WebCrypto `{name: "ECDSA", namedCurve: "P-256"}` key pair generated with
`extractable: false` and usages `["sign", "verify"]`. The `CryptoKeyPair` is stored in IndexedDB (structured
clone keeps it non-extractable). One device key per browser profile per AMP origin, reused across sessions and
harnesses. The public key is always exportable: `exportKey('raw', publicKey)` gives the 65 bytes.

**Harness key: also ECDSA P-256.** One key per harness install. The private key is kept in the OS keychain
(the keyring crate the harness already uses for OAuth), as the 32-byte scalar or PKCS#8. The reasons:

1. **The browser must verify harness signatures with WebCrypto.** ECDSA P-256 verification is in every
   WebCrypto implementation the AMP UI targets. Ed25519 in WebCrypto is recent (Chrome 137, Firefox 129,
   Safari 17) and absent on older iOS. A pure-JS fallback would add a dependency to the page whose script
   integrity matters most (T-UI-4).
2. **One algorithm everywhere.** All three codebases implement and test a single signature scheme against a
   single vector set. Go needs nothing beyond the standard library (`crypto/ecdsa`, `crypto/elliptic`).
3. **Forward path to E2E (R-L1).** Optional encryption later is ECDH(P-256) + HKDF + AES-256-GCM over the keys
   both sides already pinned. Both keys must be P-256 for that.
4. **Rust support is mature.** Use the RustCrypto `p256` crate (features `ecdsa`, `sha256`): its
   `ecdsa::Signature` is exactly the 64-byte P1363 form, and it verifies high-S signatures (see 3.4). The `ring`
   and `aws-lc-rs` crates already in the tree through rustls also work (`ECDSA_P256_SHA256_FIXED`). `p256` is
   recommended because it imports a raw scalar from the keychain directly and needs no PKCS#8 wrapper.

### 3.2 Domain-separated signing input

The bytes that are signed are a fixed ASCII prefix followed by the JCS canonical form (section 3.3):

| What | Signing input (bytes) |
|---|---|
| Envelope (command or event) | `"amp-rc/1 envelope\n"` ‖ UTF-8(JCS(envelope **without** the `sig` member)) |
| Pairing request | `"amp-rc/1 pair-request\n"` ‖ UTF-8(JCS(request object)) |
| Token refresh proof (8.2) | `"amp-rc/1 token\n"` ‖ UTF-8(JCS(proof **without** the `sig` member)) |

The prefixes end in one LF (0x0A) byte. The signer computes ECDSA-SHA256 over the whole signing input. The
WebCrypto, Go and Rust APIs hash internally, so pass them the signing input, not its hash. Go's `ecdsa.Verify`
takes the hash: `sha256.Sum256(input)`.

Example signing input (from `commands.json`, vector `prompt_valid`, wrapped here for readability):

```
amp-rc/1 envelope
{"alg":"ES256","body":{"client_msg_id":"m-01","text":"run the tests and tell me what fails"},"device_id":"X5Pp2GXb6NEr3Jh05qAy4qnPP9oOBKhSltxS5yIPExs",...,"v":1}
```

### 3.3 Canonical JSON: RFC 8785 (JCS) under the protocol's I-JSON profile

Signatures cover **JCS of the parsed value, never the received bytes.** A receiver parses the wire text,
removes `sig`, canonicalizes and verifies. Every envelope, and every pairing request, MUST satisfy this profile.
A receiver that finds a violation answers `malformed`.

- **Numbers are integers only**, with |n| ≤ 2^53−1. A number token with a fraction or an exponent (`1.0`,
  `1e3`) is malformed, and so are `NaN` and `Infinity`. **So no floats appear anywhere in signed content.**
  Money is an integer count of micro-USD (`cost_usd_micros`), percentages are integers, and arbitrary JSON from
  the model (tool arguments) travels as a **string** (`arguments_json`).
- **No duplicate object keys.**
- **No lone surrogates** in strings or keys.
- **JCS output.** No whitespace. Object members are sorted by their keys' **UTF-16 code units** (not code
  points; the `jcs.json` vector proves the difference). Strings are escaped exactly as ECMAScript's
  `JSON.stringify` escapes them: `\"`, `\\`, `\b`, `\f`, `\n`, `\r`, `\t`, other code points below U+0020 as
  `\u00XX` with **lower-case** hex, and everything else (including U+007F, U+2028, U+2029 and non-ASCII) as raw
  UTF-8. Integers are written in plain decimal.

Per language:

- **TS:** `JSON.parse`, then a recursive canonicalizer: `Object.keys(o).sort()` (the default JS sort is by
  UTF-16 code units, which is exactly right), `JSON.stringify` for strings, and `Number.isSafeInteger` for every
  number. `JSON.parse` can't see duplicate keys or tell `1.0` from `1`. That is acceptable only on the browser,
  which verifies harness events: the harness never emits either, and a relay that injects them changes nothing,
  because each side verifies and uses the same parsed value. The browser MUST still reject non-safe-integer
  numbers and lone surrogates.
- **Go:** **not `encoding/json`.** It escapes U+2028/U+2029 (and `<`, `>`, `&` unless told otherwise), so its
  output is not JCS. Use a JCS library (for example `github.com/gowebpki/jcs`), or write a canonicalizer over a
  `json.Decoder` with `UseNumber()`, rejecting duplicate keys and any `json.Number` containing `.`, `e` or `E`.
- **Rust:** `serde_jcs` over a `serde_json::Value`, or a small canonicalizer. Parse with a duplicate-key check
  (derive structs with `#[serde(deny_unknown_fields)]`, which also rejects duplicate fields) and reject
  `Number::is_f64()`.

### 3.4 Verification rules

- Verify with the public key that is **pinned** for the claimed id: the device pins the harness holds (section 7.7),
  or the harness pin the browser holds. Never verify with a key that came from the relay, except when the key's
  fingerprint equals an id the receiver already trusts.
- A signature that doesn't decode to exactly 64 bytes, or with r or s outside [1, n−1], is `bad_signature`.
- **Accept high-S signatures.** WebCrypto doesn't normalize s, so about half of browser signatures have s > n/2.
  A verifier MUST NOT require low-S. (RustCrypto's `k256` normalizes; `p256` does not. Don't copy secp256k1
  habits.) Replay protection never depends on signature uniqueness: it is `seq` plus `nonce`.
- Sign with a random or hedged nonce in production (WebCrypto always does). The vectors use RFC 6979 only so
  that they are reproducible.

## 4. The envelope

Every command and every event is one JSON object with **exactly** these 11 members. A missing or extra
top-level member makes it `malformed`.

| Field | Type | Command (browser → harness) | Event (harness → browser) |
|---|---|---|---|
| `v` | integer | `1` | `1` |
| `alg` | string | `"ES256"` | `"ES256"` |
| `session_id` | string | The target session | The source session |
| `harness_id` | string | The pinned harness's fingerprint | The signing harness's fingerprint |
| `device_id` | string or null | The signing device's fingerprint | `null` for broadcast events. For `ack`, `refused` and `pair_result`, the `device_id` the result is for. |
| `seq` | integer | Section 5.3 | Section 6.1 |
| `ts` | integer, ms | The sender's clock when signing | The sender's clock when signing |
| `nonce` | string | 16 random bytes, b64u | 16 random bytes, b64u |
| `op` | string | `prompt`, `stop`, `detach` (section 5.1) | `hello`, `status`, `chat_event`, `ack`, `refused`, `pair_result`, `bye` (section 6.2) |
| `body` | object | Per op, **unknown fields refused** | Per op, **unknown fields ignored** |
| `sig` | string | Signed by the device key | Signed by the harness key |

Example (a prompt as the wire may carry it; member order on the wire doesn't matter):

```json
{
  "v": 1, "alg": "ES256",
  "session_id": "rs_YIL0d-r7J-AHLPpZ00o2bA",
  "harness_id": "KcrmtQDCZstbc69koLwXJWQvwvQbZAufZBMxCffV6DY",
  "device_id": "X5Pp2GXb6NEr3Jh05qAy4qnPP9oOBKhSltxS5yIPExs",
  "seq": 1792948798500, "ts": 1792948798500, "nonce": "dPTjCr8zMqrmOeC3G1VLhA",
  "op": "prompt",
  "body": {"text": "run the tests and tell me what fails", "client_msg_id": "m-01"},
  "sig": "<86 chars>"
}
```

## 5. Commands (browser to harness)

### 5.1 The M1 allowlist (closed)

| `op` | `body` (exactly these members) | Harness action | `ack.result` |
|---|---|---|---|
| `prompt` | `text`: string, 1..49152 UTF-8 bytes, not whitespace-only. `client_msg_id`: string per section 2. | **Idle:** starts a run with `text` as the user turn. **Busy:** appends it to the follow-up queue (section 5.4). The text is **always literal**: a leading `/` is never a slash command (R-M6, T-H-2). The harness tags the turn `[remote: <device name>]` in its local transcript and in the model context (R-M16). | `started` or `queued` |
| `stop` | `{}` | Aborts the running turn and its workers, as Esc / `/abort` do, and **clears the follow-up queue**. Always allowed while the session is live, never rate limited below 120/min. | `done` |
| `detach` | `{}` | Ends remote control for this session, as `/remote off` does. The local session keeps running. The harness sends `bye {reason: "detached"}` and `DELETE`s the relay session. | `done` |

**Reserved names.** A v1 M1 harness MUST refuse these with `unknown_op`. They are listed so that nobody reuses
them with another meaning: `stop_worker`, `answer`, `set_model`, `cancel_queued` (M2); `approve_tool` (only if
an ask path ever exists, R-S9). Anything not in the table above is refused with `unknown_op`. That includes every
local-only action: config, providers and keys, MCP and repo trust, `/server`, `/open`, pairing, timeouts,
policy and plan approval.

A harness that supports more ops later advertises them in `hello.body.ops` (section 6.3). A browser MUST NOT send
an op that the session's latest `hello.ops` doesn't list.

### 5.2 Command results

The relay's `202` only means "forwarded". The harness's verdict arrives on the event stream as an `ack` or a
`refused` event whose `body.ref` is `{device_id, seq}` of the command. The UI SHOULD show "no answer from the
laptop" if neither arrives within 10 s.

### 5.3 `seq` for commands

- The scope is per `(session_id, device_id)`. The harness keeps `last_seq[device_id]` for the session, starting at 0.
- A command is accepted only if `seq > last_seq`. After a successful signature, ts and nonce check, the harness
  sets `last_seq = seq` and records the nonce **before** the op and body checks (section 5.6, step 11). A command
  refused for `unknown_op` or `invalid_body` can therefore never be replayed either.
- **Browser rule:** `seq = max(last_sent + 1, Date.now())`. Clock-based values survive page reloads without
  storage. With several tabs on one device, a collision makes the harness answer `refused {code:
  "seq_not_increasing", last_seq}`. The tab may then re-sign **once** with `seq = last_seq + 1`, a fresh nonce
  and a fresh `ts`. That is a new command, not a replay.

### 5.4 Follow-up queue (open question 1)

- The harness keeps at most **5** queued prompts per session. A 6th is refused with `queue_full`.
- Queued prompts run in order, each as a new run, when the current run ends with any outcome other than one
  caused by `stop`.
- `stop` (remote or local Esc / `/abort`) clears the queue.
- Every change sends a `chat_event` of type `queue` (section 6.4), so every viewer can show "queued (2)".
- A queued prompt is announced as `user_message` only when it starts, so the transcript order is the order of
  execution.
- Typing on the laptop while a run is busy keeps today's TUI behaviour in M1. The queue is for remote prompts.

### 5.5 Nonce cache and timestamps

- **Command `ts` window:** |ts − now| ≤ 60 000 ms, measured with the receiver's clock, inclusive.
- The harness keeps every nonce it has accepted for the session for at least **120 s** (twice the window), with
  room for at least 10 000 entries. A nonce older than 120 s may be evicted, because its `ts` can't pass the
  window any more.
- The relay MAY keep its own nonce cache. The harness's cache is the one that matters.

### 5.6 Harness command check (normative order)

Exactly one refusal code results, the first check that fails. The vectors in `commands.json` depend on this order.

1. Size of the received text > 65 536 bytes → `too_large`
2. Not a JSON object under section 3.3, or not exactly the 11 envelope members with the right types → `malformed`
3. `v != 1` → `unsupported_version`
4. `alg != "ES256"` → `unsupported_alg`
5. `session_id` isn't this session → `wrong_session`
6. `harness_id` isn't this harness → `wrong_harness`
7. `device_id` isn't pinned, or its pin belongs to another AMP user → `unknown_device`
8. The pin has expired (`pinned_until_ms <= now`) → `device_expired`
9. The signature fails, using the pinned key → `bad_signature`
10. |ts − now| > 60 000 → `stale_ts`
11. The nonce is in the cache → `replayed_nonce`. Then `seq <= last_seq[device_id]` → `seq_not_increasing`.
    **Then commit:** record the nonce and set `last_seq`.
12. `op` isn't on the allowlist → `unknown_op`
13. `body` doesn't match the op's schema exactly → `invalid_body`
14. The rate limit is exceeded (section 10) → `rate_limited`; a full queue → `queue_full`
15. Accept: act, then send an `ack`.

For codes from step 2 onwards, the harness sends a `refused` event when it could parse `device_id` and `seq`
(`ref` is null otherwise). It writes a local log line with no payload. It MAY rate-limit `refused` events to
one per second per device.

### 5.7 Browser event check (normative order)

1. Size > 1 048 576 bytes → `too_large`
2. Malformed under section 3.3 or the envelope shape → `malformed`
3. `v != 1` → `unsupported_version`. The UI shows "This laptop runs a newer harness; update AMP" and stops
   applying events.
4. `alg != "ES256"` → `unsupported_alg`
5. `session_id` isn't the session being viewed → `wrong_session`
6. `harness_id` isn't the pinned harness's id → `wrong_harness`
7. The signature fails with the pinned harness key → `bad_signature`
8. `seq <= last_applied_seq` → `duplicate`: drop it **silently** (this is normal after a reconnect)
9. Unknown `op` → `ignored_unknown_op`: advance `last_applied_seq` but don't render
10. Apply the event, and set `last_applied_seq = seq`

**Events have no `ts` window and no nonce check.** A reconnect can legitimately replay buffered events up to
10 minutes old. Replay is prevented by the `session_id` binding and `seq`. Snapshot frames (section 8.4,
`event: snapshot`) go through steps 1-7 only and **don't** move `last_applied_seq`.

A rejection at steps 2-7 is a security event: the UI shows a red "Untrusted data from the relay was dropped"
notice and never renders the payload.

### 5.8 Relay checks on `POST /api/sessions/:id/commands`

The relay is not trusted, but it filters early so that junk never reaches the laptop and refusals get audited.
It MUST check steps 1-10 of section 5.6, using the device keys it learned from `pair_result` events (section 7.6).
It MUST NOT change the command. It forwards the received bytes after `json.Compact` (whitespace removed,
everything else byte-identical) as one SSE `data:` line. It doesn't check `seq` or `nonce`, and it doesn't check
the op body beyond the size limits and the per-op rate limits.

## 6. Events (harness to browser)

### 6.1 `seq` for events

- The scope is per session. The first event is `seq = 1`, and each event is exactly the previous seq + 1. The
  harness keeps the counter in memory across reconnects for the session's whole life.
- The relay requires contiguity (section 8.3): a gap gets `409 seq_gap`. A batch that starts at or below the
  relay's `accepted_through` is a retry: the relay skips those envelopes and takes the rest.
- Deltas are **not** snapshots. Unlike `WorkerFeed`, events on the wire are never replaced or renumbered: the
  harness coalesces text and thinking deltas for about 100 ms **before** assigning a seq.

### 6.2 Event ops

| `op` | `device_id` | `body` | When |
|---|---|---|---|
| `hello` | null | Section 6.3 | On every connection of the commands stream (first connect and reconnect) |
| `status` | null | The `status` object of section 6.3 | When the status changes, no more than once a second, and at least every 60 s |
| `chat_event` | null | A ChatEvent (section 6.4) | Transcript activity |
| `ack` | The command's device | `{ref: {device_id, seq}, op, result: "started"\|"queued"\|"done", prompt_id: string\|null, queue_position: integer\|null}` | A command was accepted |
| `refused` | The command's device, or null | `{ref: {device_id, seq}\|null, op: string\|null, code, message, last_seq: integer\|null}` | A command was refused (codes in 9.2) |
| `pair_result` | The requesting device | `{pairing_id, result: "accepted"\|"rejected"\|"failed"\|"expired"\|"locked", code: string\|null, tries_left: integer\|null, pinned_until_ms: integer\|null}` | Section 7.5 |
| `bye` | null | `{reason}`. The reasons are in section 12.2. | The harness is ending remote control |

**Field presence in event bodies.** Every member listed is always present. A member typed `X|null` is `null`
when it doesn't apply (never omitted). This keeps the JCS form identical across serde, Go structs and TS. A
browser MUST ignore body members it doesn't know, because later v1 harnesses may add them.

`refused.message` is a short English sentence for people (≤ 200 chars) and never echoes command content.

### 6.3 `hello` and `status`

```json
{
  "harness": {"harness_id": "...", "harness_pub": "<b64u raw>", "host": "jacobs-mbp", "os": "macos", "version": "0.4.0"},
  "user_id": 1001,
  "ops": ["prompt", "stop", "detach"],
  "started_at_ms": 1792945200000,
  "expires_at_ms": null,
  "idle_timeout_s": null,
  "status": {
    "model": "anthropic/claude-opus-5-5",
    "busy": false,
    "context_pct": 37,
    "cost_usd_micros": 1834200,
    "project": {"id": 2, "name": "AMP"},
    "ticket_id": 2369,
    "ticket_name": "RC-M1 amp-relay: sessions list",
    "branch": "remote-m1-2369",
    "queue_len": 0,
    "workers_running": 1,
    "devices": [{"device_id": "...", "name": "Safari on iPhone"}],
    "ticket_state": "in_progress",
    "epic": "Remote control",
    "story": "M1",
    "usage": {"input_tokens": 120340, "output_tokens": 8311, "cache_read_tokens": 901200, "cache_write_tokens": 0},
    "thinking": "high",
    "role": "manager",
    "cwd": "amp",
    "board": {"in_progress": 3, "ready": 5, "question": 1, "scheduled": 0},
    "plan_approval": {"yours": 2, "others": 0},
    "amp_connection": {"state": "connected", "latency_ms": null}
  }
}
```

- `harness.harness_pub` MUST hash to `harness_id`. The browser checks this against its pin; the relay checks it
  against the registered key.
- `status.project` is `{id, name}|null`. `ticket_id` and `branch` are nullable. `ticket_name` (string|null, the
  current ticket's name; added by #2389) is optional within v1: a harness that predates it omits it, and the relay
  then shows the ticket's name from AMP when the ticket is in the session's project.
- **What the relay stores from a status** (#2389). From `hello.body.status` and from each `status` event, the relay
  stores `model`, `ticket_id` and `ticket_name` on the session row (`remote_sessions.model`,
  `current_ticket_id`, `current_ticket_name`) so that `GET /api/sessions` can list them, including for sessions
  that have ended. It stores nothing else from the status, and nothing from any other event. A member that is
  missing, null or of the wrong type is stored as null; `model` and `ticket_name` are trimmed and cut to 200
  bytes; `ticket_name` is kept only with a positive `ticket_id`. A status after the session ended changes
  nothing. `context_pct` is an integer
  0-100, or null. `devices` lists the devices **currently pinned at the harness** for this user. The relay uses it
  to reconcile its device table (section 7.6).
- `host` and `os` are display strings ≤ 64 chars. `os` is one of `macos`, `linux`, `windows`.
- **What the TUI shows besides** (#2388). These members are optional within v1: a harness that predates them
  omits them, and a browser treats a missing member like `null` ("not known"; draw nothing for it). A harness that
  has them always sends every one, `null` when unknown (for example before AMP answered, or without a project).
  They are display facts only: the relay stores none of them (the list above is the whole of what it stores).
  - `ticket_state` (string|null): the current ticket's board state, one of `ready`, `in_progress`, `question`,
    `scheduled`, `waiting`, `done` (the web UI's display states). `epic`, `story` (string|null): its epic and
    story names (≤ 200 chars each, like `ticket_name`).
  - `usage` (`{input_tokens, output_tokens, cache_read_tokens, cache_write_tokens}`|null): the session's token
    totals, as the harness counted them. `cost_usd_micros` above is the matching cost. A browser shows these and
    never sums `message_end` usage itself (that undercounts after a buffer gap). `null` before the first reply.
  - `thinking` (string|null): the thinking level (`off`, `minimal`, `low`, `medium`, `high`, `xhigh`). `role`
    (string|null): the agent role without its prefix (`manager`).
  - `cwd` (string|null): the **last component only** of the session's working directory (`amp`, never
    `/Users/ada/dev/amp`), ≤ 64 chars.
  - `board` (`{in_progress, ready, question, scheduled}`|null): the project board's counts the TUI's status line
    shows (`null` while unknown). `plan_approval` (`{yours, others}`|null): tickets awaiting plan approval, the
    person's own and others'; `null` when the project doesn't require plan approval or it isn't known. These are
    counts only: approving still happens in the AMP web UI, never from this page.
  - `amp_connection` (`{state, latency_ms}`|null): the harness's own connection to AMP, as the TUI header's dot
    says it. `state` is one of `connecting`, `connected`, `slow` (with `latency_ms`, an integer), `degraded`,
    `login_needed`, `disconnected` (the harness keeps retrying). `latency_ms` is `null` except for `slow`. This is
    the harness's MCP connection, independent of the remote link itself.

`hello.expires_at_ms` and `hello.idle_timeout_s` repeat the register answer (8.2): `null` when the org set no
lifetime or no idle limit, the default since #2424; numbers when it did.

### 6.4 ChatEvent (`op: "chat_event"`)

`body.type` selects the shape. All members are always present, and `X|null` members are null when unset. The
rpi source events and today's `RunEvent` (`v2/harness/crates/amp-core/src/events.rs`) map onto these types; the
harness keeps `run --json` unchanged (decision section 9).

| `type` | Members (besides `type`) | Source in the harness |
|---|---|---|
| `user_message` | `prompt_id`, `text`, `origin: "local"\|"remote"`, `device_id: string\|null`, `device_name: string\|null` | The `Shell` starts a run (local or remote prompt). `text` has no `[remote: …]` tag; the UI renders origin from the fields. |
| `run_start` | `run_id`, `prompt_id` | A run starts |
| `message_start` | `message_id`, `model`, `provider` | `AgentEvent::MessageStart` (assistant) |
| `text_delta` | `message_id`, `delta` | `MessageUpdate` / `TextDelta`, coalesced about every 100 ms |
| `thinking_delta` | `message_id`, `delta` | `MessageUpdate` / `ThinkingDelta`, coalesced. The UI folds it by default. |
| `tool_start` | `message_id`, `tool_call_id`, `name`, `arguments_json`, `truncated` | `ToolExecutionStart`. `arguments_json` is the arguments' JSON **text**, cut at ≤ 16 KiB on a UTF-8 boundary; `truncated: true` when it was cut. |
| `tool_end` | `tool_call_id`, `name`, `is_error`, `text`, `truncated`, `full_bytes`, `refused_line: string\|null` | `ToolExecutionEnd`. `text` is cut at ≤ 32 KiB; `full_bytes` is the original size. The UI shows "full output on the laptop" when `truncated`. `refused_line` (#2388): when AMP refused the call because a person must act in the AMP web UI (plan or human-ticket approval), the fixed line the TUI draws for the call's row (`AMP refused: this needs a person in the AMP web UI (below)`); the UI shows it in place of the error, next to the `approval_refused` notice. Otherwise `null`. |
| `message_end` | `message_id`, `stop_reason`, `error: string\|null`, `model`, `provider`, `usage`, `error_summary: string\|null` | `MessageEnd` (assistant). `usage` = `{input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd_micros}`, all integers. `cost_usd_micros = round(cost_usd × 1e6)`. `error` is the provider's raw text (the detail); `error_summary` (#2388) is the one sentence the TUI shows for it (`provider_error::summarize`), `null` without an error. |
| `retry` | `attempt`, `max_retries`, `delay_ms`, `error` | `RetryScheduled` (the error text already goes through `provider_error::redact`) |
| `run_end` | `run_id`, `outcome: "completed"\|"aborted"\|"failed"\|"suspended"`, `error: string\|null` | `finish_run` in the shell |
| `notice` | `level: "info"\|"warn"\|"error"`, `code: string\|null`, `text` | Notices the TUI shows: MCP hub changes, approval refusals, repeat guard, "Stopped.", offline |
| `queue` | `items: [{prompt_id, origin, device_id: string\|null, preview}]` | The follow-up queue changed. `preview` is the first 80 chars. |
| `worker_status` | `workers: [{task_id, name, role, state: "running"\|"done"\|"failed"\|"stopped"\|"blocked", model: string\|null, started_at_ms: integer\|null, ended_at_ms: integer\|null, usage: {…}\|null, context_tokens: integer\|null, context_window: integer\|null, epic: string\|null, story: string\|null, ticket_state: string\|null}]` | A snapshot from `Supervisor::workers()` when the roster changes (also when a worker's tokens change). M1 shows read-only chips. The members after `started_at_ms` are #2388's (below). |
| `compaction` | `active: boolean`, `manual: boolean` | #2388. The harness started (`active: true`) or finished (`false`) compacting the context; `manual` when the person asked for it, else automatic (the context crossed its threshold). The TUI shows `Auto-compacting…` / `Compacting context…` in its status slot meanwhile; the UI shows the same while the latest compaction is active. |

A browser that receives an unknown `type` ignores it (it may log it in development). It is not an error.

**Members added within v1 (#2388).** `message_end.error_summary`, `tool_end.refused_line`, the `worker_status`
members after `started_at_ms`, and the `compaction` type are additive: a harness that predates them leaves the
members out (and never sends `compaction`), so a browser reads a missing member as `null`. A harness that has them
always sends them (`null` when unset), as for every other member. In `worker_status`:
- `usage` is the worker run's `{input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  cost_usd_micros}` (the TUI chip's `48k tok` is the sum of the four token counts, and `$0.04` the cost), `null`
  before its first reply.
- `context_tokens` is the latest response's context size and `context_window` the model's window; the chip's
  `ctx N%` is `context_tokens × 100 / context_window` (`ctx <1%` for a sliver), shown only when both are known.
- `ended_at_ms` is when a finished run ended (`completed after 3m10s`); `null` while it runs.
- `epic`, `story`: the ticket's epic and story names (`Epic › Story`). `ticket_state`: its board state, as
  `status.ticket_state` (section 6.3).

**Redaction (R-M11, T-H-5).** Before signing, the harness MUST replace every known secret value with
`[redacted]` in every string of every event: keychain secrets, provider keys, MCP secret env values, OAuth
tokens. Redaction happens before truncation, so a secret can't be split across the cut. This covers the
`status` body too, and #2388's new text (`message_end.error` and `error_summary`, ticket, epic and story names).

**Rendering (R-M10).** `text`, `delta`, `arguments_json`, tool output, notices, error texts and the status's
names are untrusted. The UI renders
them as text or sanitized Markdown, with no raw HTML, no auto-loaded images, and links shown with their full
URL. Approvals and questions are never drawn from this text.

### 6.5 Size rules

- An event envelope is ≤ 1 MiB (1 048 576 bytes of compact JSON). With the per-field caps above, an envelope
  stays far below this. A harness that would exceed it MUST truncate further, never split an event.
- A `text_delta` or `thinking_delta` carries ≤ 16 KiB per event. A `user_message.text` is ≤ 49 152 bytes.

## 7. Pairing

### 7.1 The link

```
https://<amp-web-origin>/sessions/<session_id>#p=<pairing_secret>&h=<harness_id>
```

- The harness mints 32 random bytes as the pairing secret and keeps it **only in memory**.
- It builds the link from the AMP web origin it is logged in to, which is the origin of its MCP URL. If the
  `session_url` the relay returned has a different origin, the harness MUST ignore it and use its own origin
  (T-H-1, T-UI-8).
- The TUI prints the link and a QR code that encodes the same string.
- The fragment never reaches a server.
- **Browser:** parse the fragment with `URLSearchParams`, and **immediately** remove it with
  `history.replaceState(null, '', location.pathname)`. Keep the secret in memory only. Never log it, store it
  or send it. The page ignores every other URL parameter (T-UI-8).
- `/remote` prints a fresh link. `/remote pair` (local only) mints a new secret, which replaces any previous
  one.

### 7.2 Derivations

```
secret     = b64u_decode(p)                                  // 32 bytes
pairing_id = b64u( SHA-256("amp-rc/1 pairing-id\n" ‖ secret)[0..16] )
k_mac      = HKDF-SHA256(IKM = secret, salt = UTF-8(session_id), info = "amp-rc/1 pair-mac", L = 32)
k_sas      = HKDF-SHA256(IKM = secret, salt = UTF-8(session_id), info = "amp-rc/1 sas",      L = 32)
pair_input = "amp-rc/1 pair-request\n" ‖ UTF-8(JCS(request))
mac        = b64u( HMAC-SHA256(k_mac, pair_input) )
sig        = b64u( ECDSA-P256-SHA256(device_key, pair_input) )          // P1363
sas        = HMAC-SHA256(k_sas, harness_pub_raw ‖ device_pub_raw)        // 65 + 65 bytes
code       = uint64_big_endian(sas[0..8]) mod 100 000 000, as 8 digits with leading zeros, shown "NNNN-NNNN"
```

In WebCrypto, import the secret with `importKey('raw', secret, 'HKDF', false, ['deriveBits'])` and call
`deriveBits({name: 'HKDF', hash: 'SHA-256', salt, info}, key, 256)`. The vector values are in `pairing.json`.
The SAS binds both public keys and the secret, so a second requester who also saw the link (for example a
shoulder-surfed QR code) gets a different code from the one on the owner's phone.

### 7.3 The request

The browser sends `POST /api/sessions/:id/pair` with:

```json
{
  "request": {
    "type": "pair_request", "v": 1,
    "session_id": "rs_...", "harness_id": "<h from the link>", "pairing_id": "<derived>",
    "device_id": "<fp(device_pub)>", "device_pub": "<b64u raw>",
    "device_name": "Safari on iPhone",
    "ts": 1792948798000, "nonce": "<16 bytes b64u>"
  },
  "mac": "<b64u>",
  "sig": "<b64u>"
}
```

- `request` has exactly these 10 members.
- `device_name` is 1..64 Unicode scalar values, derived by the UI from the user agent ("Safari on iPhone",
  "Chrome on macOS").
- Before sending, the browser checks that `harness_id` equals the `harness_id` that `GET /api/sessions/:id`
  reports. If they differ, the page says "This link doesn't match that laptop" and stops.

### 7.4 Relay handling

1. Run the browser-leg authz (section 8.1).
2. Check `request.session_id` equals the path id.
3. Look up the pairing window that the harness registered with `POST /relay/v1/sessions/:id/pairings`:
   - none, or a different `pairing_id` → `409 pairing_unavailable`
   - expired → `409 pairing_expired`
   - 5 requests already counted against this `pairing_id` → `429 pairing_locked`
4. Count the attempt.
5. Write the audit row `remote.pair` (attempt, metadata only), **fail closed**: if the audit write fails, return
   `503 audit_unavailable` and don't forward the request (R-M12).
6. Forward the request to the harness on the commands stream as `event: pair_request` with data
   `{"request": …, "mac": …, "sig": …, "relay": {"user_id": <verified caller id>, "email": "<verified email>", "user_agent": "<UA header, ≤ 200 chars>"}}`.
   The relay sets the `relay` member from the verified token, never from the client.
7. Answer `202 {"status": "pending", "expires_at_ms": <window expiry>}`. The result arrives as a `pair_result`
   event on the session's event stream, so the browser opens the stream **before** it posts.

### 7.5 Harness check and the local prompt (normative order)

1. Malformed (section 3.3; exactly 10 request members; `mac` and `sig` are b64u) → `malformed`
2. `v != 1` → `unsupported_version`
3. `session_id` isn't this session → `wrong_session`. `harness_id` isn't this harness → `wrong_harness`.
4. `pairing_id` isn't the current, unconsumed secret's id → `unknown_pairing`
5. The secret is older than 5 minutes → `pairing_expired`
6. The secret has no tries left → `pairing_locked`
7. **Count one try** (5 per secret).
8. `relay.user_id` isn't the harness's own AMP user → `wrong_user`
9. `fp(device_pub) != device_id` → `device_id_mismatch`
10. |ts − now| > 60 000 → `stale_ts`
11. The MAC doesn't match (compare in constant time) → `bad_mac`
12. The signature doesn't verify with `device_pub` → `bad_signature`
13. **Prompt locally** in the TUI, never anywhere else:
    `Allow "Safari on iPhone" (jsims@…) to control this session? code 7324-7649 [y/N]`
    - The harness MUST strip C0 and C1 control characters, ESC sequences and bidi controls (U+202A..U+202E,
      U+2066..U+2069) from `device_name`, `email` and `user_agent` before printing them (terminal injection).
    - The prompt times out when the secret expires.

Results, sent as `pair_result` with `device_id` = the requester:

| Outcome | `result` | `code` | Secret |
|---|---|---|---|
| A failure at steps 1-6 | `failed` | that code | Unchanged (no try counted) |
| A failure at steps 8-12 | `failed` | that code, with `tries_left` | The try is spent. At 0 tries left, the secret is burned and the result becomes `locked`. Pairing stays locked until a local `/remote pair`. |
| The person answers `y` | `accepted` | null, with `pinned_until_ms` | Consumed |
| The person answers `N` | `rejected` | `rejected` | Consumed |
| No answer before expiry | `expired` | `pairing_expired` | Consumed |

The browser shows the same `code` (computed from its own secret) while it waits. After `accepted` it pins the
harness: it stores `{harness_id, harness_pub, host, pinned_until_ms}` in IndexedDB, but only after checking
`fp(harness_pub) == h` from the link.

### 7.6 What the relay learns

When the relay accepts a `pair_result` event with `result: "accepted"` (it verifies the harness signature like
any event), it upserts `remote_devices(user_id, harness_id, device_id, device_pub, name, pinned_until)`. It uses
this table only for the early checks of section 5.8, never for trust decisions. It reconciles the table from
`status.devices` and deletes rows for devices the harness no longer lists. The relay audits `remote.pair.reject`
for `rejected`, `failed` and `locked` results.

### 7.7 Pins (D1: 30 days)

- **Harness pin:** `{device_id, device_pub, name, user_id, paired_at_ms, pinned_until_ms}` per harness install,
  in a 0600 state file such as `<state>/remote-devices.json`. Public keys aren't secret; the harness private
  key stays in the keychain.
- `pinned_until_ms = accepted_at + min(30 days, org device_pin_days)`. An org value of 0 means "re-pair every
  session": the pin lasts until that session ends (the harness keeps it in memory only; with no session lifetime
  its `pinned_until_ms` is `accepted_at + 30 days`).
- A pin works for any later session of the same install, the same user and an unexpired pin, with no new pairing.
- The harness checks pins again on every reconnect.
- **Unpin:** locally with `/remote devices` (M1 only needs "unpin all"; the list is M2), or when the relay sends
  `device_revoked` (section 8.3), or when the pin expires.
- **Browser pin:** `{harness_id, harness_pub, host, pinned_until_ms}`. A session whose `harness_id` is pinned
  and unexpired opens directly. Otherwise the page shows "Pair this device from the laptop" with instructions.
  It MUST NOT trust on first use: a relay-supplied key is never pinned without the link's `h` (T-R-1).

## 8. HTTP API

### 8.1 Common rules

- **Errors** keep amp-api's existing shape and add a stable code:
  `{"error": "<human sentence>", "code": "<code from section 9.1>"}`, plus extra members where noted
  (`retry_after_s`, `supported_versions`, `expected_seq`, `accepted_through`). Every `429` also sets a
  `Retry-After` header in seconds.
- **Content types:** requests and responses are `application/json; charset=utf-8`. Streams are
  `text/event-stream` with `Cache-Control: no-store` and `X-Accel-Buffering: no`. A POST with another
  `Content-Type` gets `415 unsupported_media_type`.
- **Harness leg (`/relay/v1/…`).** Envoy routes `/relay/` with ext_authz off. The command stream
  (`…/commands`) has `timeout: 0s`, `idle_timeout: 120s` and `max_stream_duration: 12h`; every other harness call
  has a 30 s timeout (#2399). The 12 h cap is per stream, not per session: a stream Envoy cuts is reopened by the
  harness (or the browser) at once, and the session carries on (#2424).
  - `POST /relay/v1/sessions` and `POST …/token` take the harness's **OAuth access token**: a Dex JWT whose
    audience includes `amp-mcp`, verified as `/mcp` verifies it. `POST …/token` also takes a proof signed by
    the session's harness key (8.2): the OAuth login alone never renews a session's connect token.
  - Refused: `amp_` API tokens (`403 token_not_allowed`), agent accounts (`users.kind = 'agent'`,
    `403 agent_not_allowed`) and an auth-disabled server (the routes don't exist, so `404`).
  - Every other harness call takes `Authorization: Bearer rct_…`, the connect token, which is bound to one
    session.
- **Browser leg (`/api/sessions…`).** It goes through ext_authz with the cookie. The principal MUST be a person
  in the web UI (`Channel == ui`, not an agent account), else `403 not_ui_channel`. That rules out `amp_` tokens
  and MCP tokens.
  - Non-GET calls pass `RejectCrossSiteWrites`.
  - The event stream (a GET) MUST also refuse `Sec-Fetch-Site: cross-site`, and any `Origin` other than the
    AMP web origin, with `403 cross_site`.
- **Owner only (R-M3).** Every `/api/sessions/:id…` call checks `session.owner_user_id == principal.UserID`,
  taken from the verified token. Someone else's session, or a session that doesn't exist, gets the same
  `404 not_found`, so existence never leaks. An ended session of the caller's own gets `410 session_ended`.
- **Policy gates:**
  - Instance switch off (`relay.enabled: false`): amp-relay isn't deployed and Envoy has no relay route.
    amp-api serves no relay route either way, so `/api/sessions...` is amp-api's plain `404`, and `/relay/...`
    falls through to the web app's catch-all and never reaches a relay. With `relay.enabled: true` and
    amp-relay scaled to 0, Envoy has no upstream: both legs get `503` and remote control is unavailable.
  - Org switch off for the session's project: `403 remote_disabled`.
  - `AMP_HARNESS_DISABLE_REMOTE=1` on the laptop: the harness never calls the relay at all.

### 8.2 Harness leg

#### `POST /relay/v1/sessions`: register (enable)

Request (≤ 8 KiB):

```json
{
  "protocol_versions": [1],
  "harness_id": "<fp>", "harness_pub": "<b64u raw>",
  "host": "jacobs-mbp", "os": "macos", "harness_version": "0.4.0",
  "project_id": 2
}
```

`201 Created`:

```json
{
  "session_id": "rs_…",
  "protocol_version": 1,
  "connect_token": "rct_…", "connect_token_expires_at_ms": 1792949700000,
  "started_at_ms": 1792948800000, "expires_at_ms": null,
  "idle_timeout_s": null, "device_pin_max_days": 30,
  "heartbeat_interval_s": 20, "keepalive_interval_s": 20,
  "session_url": "https://amp.example/sessions/rs_…",
  "limits": {"command_bytes": 65536, "event_bytes": 1048576, "batch_bytes": 2097152, "batch_events": 64,
             "prompts_per_min": 20, "ops_per_min": 120, "viewers": 3, "pairing_tries": 5}
}
```

`expires_at_ms` is when the session's lifetime ends and `idle_timeout_s` its idle limit. Both are `null` unless the
project's org set them (section 12.5, #2424): by default a session has no time limit. A harness MUST treat `null`
(and a non-positive number) as "no limit". A harness built before #2424 can't read `null` and fails to turn remote
control on; remote-m1 is unreleased, so update the harness with the relay.

The relay checks:

- `fp(harness_pub) == harness_id` → else `400 malformed`
- the caller can see `project_id` → else `404 not_found`
- the project's org has remote control on → else `403 remote_disabled`
- the protocol versions overlap → else `400 unsupported_version` with `supported_versions`
- ≤ 5 live sessions for this user → else `429 too_many_sessions`
- ≤ 10 registrations a minute for this user → else `429 rate_limited`
- the `remote.enable` audit row is written → else `503 audit_unavailable` (fail closed)

A harness that gets `404` says "Remote control isn't available on this AMP server" (decision section 9). It
says the same when the answer isn't the relay's at all (#2390): any redirect (`3xx`), or a success whose body
isn't a JSON object (the web app's HTML, which is what `/relay/` reaches when the instance switch is off). The
harness never follows redirects on `/relay/` calls, so its tokens are only ever sent to the AMP origin's relay.

#### `POST /relay/v1/sessions/:id/token`: refresh the connect token

Auth: the OAuth access token, as for registration, **and** a proof that the caller holds the session's
harness key (#2390). Many programs hold the person's own OAuth token (Claude Code with the AMP plugin, a manager
on another machine), and the session id is not a secret; without the proof any of them could mint a connect
token, void the harness's own, take over the command stream and read every prompt (review #2381, M1).

Request (≤ 8 KiB, `application/json`), exactly one member:

```json
{"proof": {"v": 1, "alg": "ES256", "session_id": "rs_…", "harness_id": "<fp>",
           "ts": 1792948799500, "nonce": "<22 chars>", "sig": "<86 chars>"}}
```

`sig` is the harness key's ES256 signature over `"amp-rc/1 token\n"` ‖ JCS(proof without `sig`) (section 3.2);
a fresh random `nonce` (16 bytes, b64u) and the current `ts` every time. The relay checks, in this order:

1. the body is in the profile (3.3), has only `proof`, and the proof has exactly the 7 members with the right
   types and a 22-character nonce → else `400 malformed`
2. `v == 1` → else `400 unsupported_version`; `alg == "ES256"` → else `400 unsupported_alg`
3. `session_id` is the path's → else `400 wrong_session`; `harness_id` is the session's registered one → else
   `400 wrong_harness`
4. the signature verifies with the **registered** `harness_pub` (`remote_sessions.harness_pub`) → else
   `403 bad_signature`
5. `ts` within ±60 s → else `400 stale_ts`, whose body also carries the relay's clock, `now_ms`, so the harness
   can tell its person how far off the computer's clock is (#2399)
6. the nonce wasn't used for this session in the last 120 s → else `400 replayed_nonce`. The used nonces are kept
   in Postgres (`remote_proof_nonces`), so a proof used on one relay replica is refused on every other (#2399)

The harness tells its person once (a TUI notice, and a `notice` chat event with code `token_refresh_refused`
for the web page) when a refresh is refused with `stale_ts`, `bad_signature`, `wrong_harness`, `wrong_session`,
`malformed` or an unsupported version: retrying won't help, and remote control stops when the current token
runs out (#2399).

Ownership comes first (someone else's session `404`, an ended one `410`), then the proof, then the org policy
(`403 remote_disabled`). Response `200 {"connect_token", "connect_token_expires_at_ms"}`. The token lasts
**15 minutes**; the harness refreshes it when two thirds of that has passed. Every other harness call needs the
connect token, so a client without the harness key can't open the command stream, post events, open a pairing
window or end the session. The vectors are in `token.json`.

- An expired token on any harness call → `401 token_expired`.
- Streams that are already open are not cut when their token expires.
- An ended session → `410 session_ended {"reason"}`.

#### `GET /relay/v1/sessions/:id/commands`: the command stream (SSE)

Auth: the connect token. One stream per session: a new stream with a valid token for the session **replaces**
the old one, which gets `event: end` with `{"reason": "replaced"}` and is closed. Only the holder of the
session's connect token can do this; a second registration is a new session.

Frames (the relay never sends `id:`, because commands are never replayed):

| `event:` | `data:` |
|---|---|
| `command` | A browser envelope (section 4), compacted and otherwise byte-identical |
| `pair_request` | Section 7.4 |
| `viewers` | `{"count": n}` (the number of live browser event streams). Sent on every change and right after connect. It drives `REMOTE ON · n devices`. |
| `device_revoked` | `{"device_id": "<fp>"}` or `{"all": true}`. The harness unpins. It is unsigned, but it only reduces access. |
| `end` | `{"reason": "<section 12.2>"}`. Then the stream closes. The harness turns remote off locally. It is unsigned, but it only reduces access. |
| *(comment)* | `: ka` every 20 s |

The harness reconnects when the stream drops, or when it hears nothing for 60 s: backoff 1, 2, 4 … up to 30 s
with ±20% jitter. There is no deadline (#2424): after a sleep, a network loss, a relay restart or rollout, or a
relay outage of any length it keeps trying, and renews its connect token with the signed proof (`POST …/token`)
however long ago the token expired. It stops only when the session has ended (`410`, an `end` frame, the relay's
own `404 not_found`, `403 remote_disabled` on the refresh) or, when the org set them, once the session's
`expires_at_ms` or idle limit has passed. An answer that isn't the relay's (a bare `404`, a `503` from Envoy, the web
app's HTML when the relay is switched off) is a pause, not an end: the session comes back with the relay.

#### `POST /relay/v1/sessions/:id/events`: publish events

Auth: the connect token. Body `{"events": [envelope, …]}`: 1-64 envelopes, ≤ 2 MiB in total, each envelope
≤ 1 MiB. The harness flushes about every 100 ms, or sooner when a batch is full.

The relay checks each envelope in order:

- section 5.7 steps 1-7, with the registered harness key
- `device_id` is null except on `ack`, `refused` and `pair_result`
- the seq is contiguous

Responses:

- `200 {"accepted_through": N}`
- `409 seq_gap {"expected_seq": N, "accepted_through": N-1}`
- `400 malformed | bad_signature | wrong_session {"accepted_through": …, "index": i}`. The relay keeps the
  envelopes before index `i`. The harness treats this as a bug: it ends remote with a local notice.
- `413 too_large`
- `429 rate_limited {"retry_after_s"}`: the session's harness posted more than **2 MiB/s** of event bytes
  (burst 8 MiB, counted by request body). Nothing in the batch was accepted; the harness waits and sends it
  again (#2390).
- `410 session_ended`

The relay appends accepted envelopes to the buffer (section 11) and fans them out to viewers. It keeps the
latest `hello` and `status` in the snapshot slot.

#### `POST /relay/v1/sessions/:id/heartbeat`

Auth: the connect token. Empty body. Sent every 20 s.

- `200 {"viewers": n, "idle_deadline_ms": …|null, "expires_at_ms": …|null}` (`null`: no such limit, #2424)
- `410 session_ended {"reason"}`

The relay marks the harness **offline** after 60 s without a heartbeat, or when the command stream closes,
whichever comes first.

#### `POST /relay/v1/sessions/:id/pairings`: open a pairing window

Auth: the connect token. Body `{"pairing_id": "<22 chars>", "expires_at_ms": <≤ now + 300000>}`. Response
`201 {}`. It replaces any earlier window and resets the try count.

#### `DELETE /relay/v1/sessions/:id`: end (remote off, detach, exit)

Auth: the connect token. Optional body `{"reason": "harness_ended" | "detached" | "idle" | "expired"}`.
Response `204`. The relay ends the session, sends `relay {type: "ended"}` to the viewers, drops the buffer and
audits `remote.disable` (or `remote.expire` for `idle` and `expired`). The harness sends `bye` before
`DELETE` when it can.

**An end the relay missed (#2396).** When `/remote off`, `detach` or the harness exiting can't reach the relay
(no answer, a `5xx`, `429`, or `401` because the connect token lapsed during an outage), remote control is
off on the laptop at once, and the harness SHOULD still tell the relay. Otherwise AMP lists the session as
live and offline until its idle limit, or, with no limits (the default), until someone ends it. The harness records the end locally
(`<state>/remote-pending-ends.json`, mode 0600: the AMP origin, session id, harness id, reason and a
deadline; never a token). It then sends the end in the background while it runs, and again when it next
starts. Sending it is a token refresh signed with the harness key (`POST …/token`, above), then this
`DELETE` with the new token and the original reason. The wire is unchanged.

The harness drops the entry when:

- the relay ends the session (`204`), or says it's over (`410`, or `404`);
- the org switch is off (`403 remote_disabled`): the relay ended the org's sessions itself;
- another `4xx` means it can never succeed (a proof from another key, for example);
- the deadline passes. The deadline is the earlier of the session's idle deadline and its lifetime, because by
  then the relay ends the session by itself; with neither (the default), 30 days after the end (#2424).

An end with reason `idle` or `expired` is never recorded, for the same reason. Only the AMP origin that the
session belongs to is ever sent its end. The relay serves both calls on any replica: a holder that's gone is
taken over (section 11), as `TestRelay_EndAfterOutageWithSignedRefresh` checks.

### 8.3 Browser leg

#### `GET /api/sessions`

The caller's own sessions: the live ones first (newest started first), then the ones that **ended in the last 7
days** (most recently ended first, at most 100). Owner only, like every browser-leg call: nobody else's session
appears, global admins and org owners included.

```json
{"sessions": [{
  "session_id": "rs_…", "harness_id": "…", "harness_fp_display": "29CA-E6B5-00C2-66CB",
  "host": "jacobs-mbp", "os": "macos", "harness_version": "0.4.0",
  "project": {"id": 2, "name": "AMP"}, "online": true, "viewers": 1,
  "started_at_ms": …, "expires_at_ms": null, "last_activity_at_ms": …, "protocol_version": 1,
  "state": "active", "end_reason": null, "ended_at_ms": null,
  "model": "anthropic/claude-opus-5-5",
  "ticket": {"id": 2369, "name": "RC-M1 amp-relay: sessions list"},
  "devices": [{"device_id": "…", "name": "Safari on iPhone", "paired_at_ms": …, "expires_at_ms": …}]
}]}
```

`expires_at_ms` is `null` for a session with no lifetime, the default since #2424 (the UI shows "Never").

The members after `protocol_version` were added by #2389 (additive; a client that predates them ignores them):
- `state` is `active` or `ended`. `end_reason` is the reason of section 12.2 for an ended session, else null, and
  `ended_at_ms` is when it ended, else null. An ended session is always `online: false, viewers: 0`.
- `model` is the last `status.model` the harness sent, or null (section 6.3).
- `ticket` is `{id, name: string|null}` from the last status's `ticket_id` and `ticket_name`, or null when the
  status named no ticket. When the harness sent no name, `name` is the ticket's name in AMP if the ticket is in
  the session's project, else null.
- `devices` lists the browsers paired with this **harness install** (pins are per harness, section 7.7) that are
  neither revoked nor expired: `device_id`, `name`, `paired_at_ms`, `expires_at_ms`. For an ended session it is
  that harness's current pins.

`GET /api/sessions/:id/...` on an ended session stays `410 session_ended`; the list is the only place ended
sessions show.

#### `GET /api/sessions/:id`

The same object plus `"harness_pub": "<b64u raw>"`. The browser MUST NOT trust `harness_pub` unless its
fingerprint matches a pin or the link's `h`.

#### `GET /api/sessions/:id/events?from_seq=N`: the event stream (SSE)

Envoy route: `timeout: 0s`, `idle_timeout: 120s`, `max_stream_duration: 12h` (#2399). There are at most 3 concurrent streams per session (`429 too_many_viewers`) and
20 attaches a minute (`429 rate_limited`). Each attach is audited as `remote.attach`.

**Where the replay starts.** With `Last-Event-ID: L`, events with `seq ≥ L+1`. Otherwise with `from_seq=N`,
events with `seq ≥ N`. With neither, from the oldest buffered event. `Last-Event-ID` wins when both are present.

Frames, in this order:

1. `retry: 3000`
2. `event: relay` with `{"type": "attached", "online": bool, "oldest_seq": n|null, "latest_seq": n|null}`
3. `event: snapshot` with the latest `hello` envelope, then the latest `status` envelope, when the relay has them
4. If the requested start is below `oldest_seq`: `event: relay` with
   `{"type": "gap", "requested_seq": n, "oldest_seq": m}`. The UI shows "Earlier messages aren't available."
5. Buffered events: `id: <seq>`, `event: envelope`, `data: <envelope>`
6. Live events, in the same format
7. `event: relay` with `{"type": "online"}` / `{"type": "offline"}` when presence changes, and with
   `{"type": "ended", "reason": "…"}` just before the relay closes the stream for good
8. A `: ka` comment every 20 s

`relay` frames are **unsigned status from AMP**. The UI uses them for the online badge, the gap notice and the
end screen, and never as chat content or as a reason to trust anything. If the browser hears nothing for 60 s,
it closes the `EventSource` and reconnects (`EventSource` alone doesn't notice a silent stream).

#### `POST /api/sessions/:id/commands`

The body is one envelope, ≤ 64 KiB.

- `202 {"status": "forwarded"}`
- `409 harness_offline`: refused immediately, never queued (R-M8)
- `400 malformed | unsupported_version | unsupported_alg | wrong_session | wrong_harness`
- `403 unknown_device | device_expired | bad_signature`
- `400 stale_ts`
- `413 too_large`
- `429 rate_limited`
- `410 session_ended`

Audit: `remote.prompt` (`{len, sha256}` of the text, never the text), `remote.stop`, `remote.detach`, and
`remote.refused` for refusals, sampled at 1 per minute per session and code.

#### `POST /api/sessions/:id/pair`

Section 7.4. The body is ≤ 8 KiB. Responses: `202 {"status": "pending", "expires_at_ms"}`, `409
pairing_unavailable | pairing_expired | harness_offline`, `429 pairing_locked`, `503 audit_unavailable`.

#### `POST /api/sessions/:id/disconnect`

The **Disconnect** button. It ends the session (reason `disconnected`) whether or not the harness is online, and
sends `end` to the harness. Response `204`. Audited as `remote.detach`. When the device is paired and the
harness is online, the UI SHOULD first send a signed `detach`, so the harness turns off on authenticated
evidence, and then call this endpoint.

#### `POST /api/sessions/revoke-all`

**Revoke all my remote sessions.**

- It ends every live session of the caller (reason `revoked`) and sends `device_revoked {"all": true}` to each
  harness first.
- It deletes the caller's `remote_devices` rows.
- Response `200 {"ended": n, "devices_revoked": m}`.
- Audited as `remote.device.revoke` and `remote.disable`.

The per-device `DELETE /api/sessions/devices/:device_id` and the device list are M2.

#### Org policy (org admins, `Channel == ui`)

`GET /api/orgs/:id/remote-control` and `PUT /api/orgs/:id/remote-control` with

```json
{"enabled": false, "idle_timeout_min": 0, "max_lifetime_hours": 0, "device_pin_days": 30}
```

- Defaults: `enabled: false`; `idle_timeout_min` 0 (no idle limit) or 5..240, default 0; `max_lifetime_hours` 0
  (no lifetime) or 1..720, default 0; `device_pin_days` 0..30, default 30. Members left out of a `PUT` keep their
  values, so a client that predates `max_lifetime_hours` (#2424) never changes it.
- The idle limit applies to live sessions (the relay's sweep reads it every 30 s). The lifetime is fixed when a
  session registers, so a new value applies to sessions started after it.
- `PUT` audits `remote.policy.set`.
- Served by **amp-api** (not the relay). Turning `enabled` off ends every live session in the org within
  seconds (reason `policy_off`). amp-api ends the sessions in Postgres and tells each session's holding
  amp-relay replica with a control call on its internal `:3003` listener (`POST /internal/relay/ctl/end`,
  cluster token; section 11), which closes the streams. With no relay running there is nothing to close. If
  the call fails, the holder's own sweep finds the ended rows within seconds. Chat payloads never cross
  services this way, and nothing goes through Postgres NOTIFY.

#### Capability flag: `GET /api/me` (additive)

amp-api has no separate UI-config endpoint. The UI boots from `/api/me` (`v2/ui/src/contexts/AuthContext.tsx`),
so the flag is a new member there:

```json
"capabilities": {"remote_control": {"available": true, "org_ids": [3], "reason": null}}
```

- Served by **amp-api**. The instance switch here is `RELAY_ENABLED`, which Helm sets from `relay.enabled`
  (the same value that deploys amp-relay); amp-api uses it only for this flag.
- `available` is true when the instance switch is on, auth is enabled, the caller is a person (not an agent,
  not an API token), and at least one of the caller's orgs has the setting on.
- Otherwise `reason` is one of `"instance_disabled"`, `"auth_disabled"`, `"not_a_person"`, `"no_org_enabled"`.
- The UI shows the Sessions entry points only when `available` is true. An old UI ignores the member; an old
  API lacks it, and the UI treats that as unavailable.

## 9. Error and refusal codes

### 9.1 HTTP (`code` member)

| Status | `code` | Meaning |
|---|---|---|
| 400 | `malformed` | Bad JSON, a profile violation (3.3), a bad shape, or `fp(pub) != id` |
| 400 | `unsupported_version` | No common protocol version (`supported_versions` included) |
| 400 | `unsupported_alg` | `alg` isn't `ES256` |
| 400 | `wrong_session` / `wrong_harness` | The envelope doesn't match the session in the path |
| 400 | `stale_ts` | Outside ±60 s |
| 400 | `replayed_nonce` | A token refresh proof whose nonce was already used (8.2) |
| 401 | `unauthenticated` | No credential, or an invalid one |
| 401 | `token_expired` | The connect token has expired; refresh it |
| 403 | `token_not_allowed` | An `amp_` API token on the harness leg |
| 403 | `agent_not_allowed` | An agent account |
| 403 | `not_ui_channel` | The browser leg was called by something other than a person in the web UI |
| 403 | `cross_site` | A cross-site request, or a foreign Origin |
| 403 | `remote_disabled` | The org setting is off |
| 403 | `unknown_device` / `device_expired` / `bad_signature` | The relay's early command checks (5.8), or a token refresh proof not signed by the session's harness key (8.2) |
| 404 | `not_found` | No such session, or not the caller's (deliberately the same) |
| 409 | `harness_offline` | No live command stream, or the heartbeat is older than 60 s |
| 409 | `seq_gap` | Events aren't contiguous (`expected_seq`) |
| 409 | `pairing_unavailable` / `pairing_expired` | No matching pairing window |
| 410 | `session_ended` | The session has ended (`reason`) |
| 413 | `too_large` | Over the size limit |
| 415 | `unsupported_media_type` | Not JSON |
| 429 | `rate_limited` / `too_many_sessions` / `too_many_viewers` / `pairing_locked` | With `Retry-After` and `retry_after_s` |
| 503 | `audit_unavailable` | A fail-closed audit write failed (enable, pair) |

### 9.2 Harness command refusals (`refused.code`)

`too_large`, `malformed`, `unsupported_version`, `unsupported_alg`, `wrong_session`, `wrong_harness`,
`unknown_device`, `device_expired`, `bad_signature`, `stale_ts`, `replayed_nonce`, `seq_not_increasing`,
`unknown_op`, `invalid_body`, `rate_limited`, `queue_full`, `internal_error`.

### 9.3 Pairing (`pair_result.code`)

`malformed`, `unsupported_version`, `wrong_session`, `wrong_harness`, `unknown_pairing`, `pairing_expired`,
`pairing_locked`, `wrong_user`, `device_id_mismatch`, `stale_ts`, `bad_mac`, `bad_signature`, `rejected`.

### 9.4 Browser-side verdicts on events (local only, never sent)

`ok`, `duplicate`, `ignored_unknown_op`, `too_large`, `malformed`, `unsupported_version`, `unsupported_alg`,
`wrong_session`, `wrong_harness`, `bad_signature`.

Clients MUST treat an unknown `code` as a generic failure of that HTTP status. New codes are additive within v1.

## 10. Limits and timeouts

| Item | Value | Enforced by |
|---|---|---|
| Command envelope | ≤ 65 536 bytes | relay, harness |
| Prompt text | 1..49 152 UTF-8 bytes | harness, relay (via the envelope size); the UI checks before sending |
| Event envelope | ≤ 1 048 576 bytes | harness, relay, browser |
| Event batch | ≤ 64 envelopes and ≤ 2 MiB | relay |
| Event bytes per session | 2 MiB/s, burst 8 MiB (`429 rate_limited`) | relay |
| Viewer queue | 512 frames and 4 MiB per viewer; 16 MiB for all the viewers of one person's sessions on one relay replica; 64 MiB for all viewers on one relay replica. A viewer past any of them is dropped and resumes with `Last-Event-ID`. The per-person bound keeps one person's stalled viewers from using up the replica's and getting other people's viewers dropped (#2398) | relay |
| Stream write | 30 s per write; a client that stops reading is dropped | relay |
| `tool_start.arguments_json` / `tool_end.text` / deltas | ≤ 16 KiB / 32 KiB / 16 KiB | harness |
| Register and pair request bodies | ≤ 8 KiB | relay |
| Prompts | 20/min per session, burst 5 | relay and harness |
| Other ops | 120/min per session | relay and harness |
| Registrations | 10/min per user; 5 live sessions per user | relay |
| Viewers | 3 concurrent streams per session; 20 attaches/min | relay |
| Pairing | 5 tries per secret (harness), 5 requests per `pairing_id` (relay); secret TTL 5 min; single use | harness, relay |
| Follow-up queue | 5 prompts | harness |
| Command `ts` window | ±60 s; nonces kept ≥ 120 s | harness (relay checks `ts` too) |
| Command TTL while offline | none: refused immediately (`409 harness_offline`) | relay |
| SSE keepalive | a comment every 20 s, both streams | relay |
| Harness heartbeat | every 20 s; offline after 60 s | harness, relay |
| Silence reconnect | 60 s without bytes | harness, browser |
| Connect token | 15 min; refresh at 10 min | relay |
| Idle timeout | none by default; an org may set 5-240 min without remote activity (#2424) | harness (authoritative), relay (backstop: +60 s) |
| Max lifetime | none by default; an org may set 1-720 h from registration (#2424) | harness, relay |
| Device pin | 30 days (org: 0-30 days) | harness, browser |
| Resync buffer | 200 events or 2 MiB, whichever is smaller; 10 min TTL; in memory only | relay |
| Missing ack in the UI | 10 s | browser |
| Envoy `/relay/` | The command stream: `timeout: 0s`, `idle_timeout: 120s` (`relay.streamIdleTimeout`), `max_stream_duration: 12h` (`relay.maxStreamDuration`, per stream: the harness reopens it); the other calls `timeout: 30s` (`relay.requestTimeout`) | Helm |
| Envoy `/api/sessions/` | `…/events` as the command stream; the other calls `timeout: 30s` | Helm |

**Remote activity**, for the idle timer, is a command the harness accepted or a new viewer attach. Events the
harness sends don't count.

**Keepalives and proxies (#2424).** With no session limit a stream can stay open for days, so every hop must
keep it. The relay writes `: ka` every 20 s on both streams and the harness posts a heartbeat every 20 s, well
inside Envoy's 120 s stream idle timeout (#2399). Keepalives can't help with a proxy that caps a whole request:
Traefik v3's entry point `respondingTimeouts.readTimeout` defaults to 60 s and cuts SSE streams, so set it to 0
in front of AMP (KB how-to/remote-control-local-test.md). Anything else in front of Envoy must not cut a stream
that is quiet for less than 60 s. When a hop cuts one anyway (Envoy's 12 h
`max_stream_duration`, a proxy restart), the harness and the browser reopen it at once.

## 11. Buffering, resume and presence

- The relay keeps one ring per session: the newest events within **200 events / 2 MiB / 10 minutes**, evicting
  the oldest first. The latest `hello` and `status` are kept outside the ring (the snapshot slot).
- **Which service serves what (#2387).** The relay is its own service, **amp-relay** (`cmd/relay`, image
  `amp-relay`, Deployment `amp-relay`), sharing the database and the Go packages with amp-api:

  | Endpoint | Served by |
  |---|---|
  | `/relay/v1/...` (harness leg, all of 8.2) | amp-relay (public `:3002`) |
  | `/api/sessions`, `/api/sessions/...` (browser leg, all of 8.3, incl. `revoke-all`) | amp-relay (public `:3002`) |
  | `/internal/relay/...` (forwarding between relay replicas; `ctl/end`, `ctl/device-revoked`, `ctl/presence`) | amp-relay (internal `:3003`, cluster token) |
  | `GET`/`PUT /api/orgs/:id/remote-control` (org policy) | amp-api |
  | `GET /api/me` `capabilities.remote_control` | amp-api |
  | migrations, the idle/lifetime sweep | amp-api migrates; amp-relay runs the sweep |

  amp-api serves none of the relay's routes and doesn't link the relay code, so with no amp-relay pod
  running nobody can reach a harness. amp-relay never migrates the database: it waits for amp-api's
  remote control migration. Its replicas join the same replica group (`api_nodes`) as amp-api's, which is
  how relay replicas find a session's holder and how amp-api reaches it, but they never run project actors.
  Each `api_nodes` row records the database role that registered it (`registered_by`, filled by Postgres).
  Row-level security lets the relay's role write only its own rows, and amp-api sends its cluster token only
  to project owners registered by amp-api's role, so a compromised relay can't point an amp-api replica's
  address at itself to receive that token (#2398).
- The ring lives only in the memory of the relay replica that holds the harness connection. It is **never** written to
  Postgres, the logs or traces, and it is dropped when the session ends.
- **Multiple replicas.** The `remote_sessions` row names the replica that holds the session (`holder_node`):
  the replica that registered it, until that one is gone. Every call for a session, on either leg (the
  browser's stream, commands, pairing, disconnect; the harness's command stream, events, heartbeat, pairing
  window, `DELETE`), is served on the holder. A call that lands on another replica is proxied over the
  internal `:3003` listener with `X-AMP-Cluster-Token`, in the same way as project actors: SSE streams are
  proxied as they flow, and a request is sent again only when it certainly wasn't delivered (the connection
  failed, or the target answered that it doesn't hold the session). The Postgres NOTIFY relay is never used
  for events.
  - Ends started on another replica (the org switch, revoke-all, the idle/lifetime sweep) reach the holder by
    a control call, which closes its streams; revoke-all's `device_revoked` and the presence shown by
    `GET /api/sessions` come from the holder the same way. The holder also checks its sessions against
    Postgres every few seconds, as a backstop.
  - The per-session limits (prompt and op rates, viewers, attaches, pairing tries) are enforced on the holder,
    which sees all of the session's traffic, so they hold across replicas. The registration limit is also
    checked against the sessions started in the last minute in Postgres.
  - After an end, every replica answers the connect token with `410` and the reason (Postgres keeps the
    ended session's token hash).
  - A proxied call carries no caller credentials (#2390): the `Cookie` (the oauth2-proxy session), the
    headers ext_authz passes upstream and, on the browser leg, `Authorization` are removed. The holder trusts
    only the verified-principal header, and only behind the cluster token. The harness leg keeps
    `Authorization`: its connect token is checked again on the holder.
- **Failover.** A replica takes a session over only when the holder's cluster heartbeat is older than the
  cluster TTL (10 s; at once after a graceful shutdown). Until the harness reconnects, the session is offline
  there: viewers attach with `online: false` and commands get `409 harness_offline`. The harness reconnects to
  whichever replica it reaches, which passes it to the new holder. That replica has an empty buffer: it
  accepts the harness's next batch as the new start, and a viewer that asked to resume from an earlier seq
  gets a `gap` frame. A replica that was judged gone and comes back releases such sessions: their streams
  close without an `end`, and the harness and viewers reconnect to the new holder.
- **Browser reconnect.** It reconnects with `Last-Event-ID` (`EventSource` does that itself), or with `from_seq`
  after a manual reopen. It drops duplicates by seq.
- **Harness resync** ("replay from my own feed or JSONL" when the buffer can't cover a gap) is **M2**. In M1 a
  gap shows as a notice, and the `hello`/`status` snapshot restores the status bar.
- **Presence.** The harness is online while its command stream is open **and** a heartbeat has arrived within
  60 s. Viewers get `relay {online|offline}`, and offline sessions refuse commands.

## 12. Session lifecycle, policy and audit

### 12.1 States

`active` (online or offline), then `ended` (with a reason). An ended session never comes back: `/remote` on the
laptop registers a new session, and the device pins still apply.

### 12.2 End reasons (`bye.reason`, `end.reason`, `relay ended.reason`, `410` `reason`)

| Reason | Trigger |
|---|---|
| `harness_ended` | `/remote off`, harness exit, or `DELETE` without a reason |
| `detached` | A signed `detach` command |
| `disconnected` | The browser's Disconnect endpoint |
| `revoked` | Revoke-all, or the owner's AMP account was deactivated |
| `policy_off` | The org or instance switch was turned off |
| `idle` | The org's idle limit without remote activity (only when the org set one, #2424) |
| `expired` | The org's lifetime since registration (only when the org set one, #2424) |
| `replaced` | Command stream only: a newer stream of the same session took over (not a session end) |
| `killed` | Reserved for the M2 admin kill |

Every path ends in under 5 s. The relay closes all of the session's streams (R-M7).

### 12.3 Re-checks

- On every reconnect, the harness re-reads the local disable switch, re-checks its own idle and lifetime
  deadlines when the org set them (after sleep they may have passed, and then it ends locally), refreshes the
  token (the relay re-checks org policy) and reloads its pins.
- The relay re-checks org policy on register, on token refresh and on policy change.
- **Deactivating a person** (`POST /api/users/:id/deactivate`) ends all of their live sessions with reason
  `revoked`, audited as `remote.disable`; amp-api tells the holder as for the org switch, so the streams close
  at once. The connect token of a deactivated owner is refused (`401`) on every replica even if that end
  didn't happen (#2390).
- **Losing access to the project** ends the session the same way (reason `revoked`, audited as
  `remote.disable` with no actor when the relay finds it): amp-api checks right after a team member, a team, a
  project's team link, an org member or a global admin role is removed; the relay checks again on every token
  refresh (`410 session_ended`) and in its 30 s sweep, for access lost any other way (#2399).

### 12.4 Audit (D3: metadata only)

Every audit row has `project_id`, `actor_user_id` and `details = {session_id, device_id|null, key_fp|null,
harness_host, op}`. **No prompt text, tool output or secrets.**

| Action | When | Failure mode |
|---|---|---|
| `remote.enable` | Register | **Fail closed** (`503`) |
| `remote.disable` | `DELETE`, disconnect, revoke-all, policy off | Best effort |
| `remote.expire` | Idle or max lifetime | Best effort |
| `remote.pair` | Pair request forwarded | **Fail closed** (`503`) |
| `remote.pair.reject` | A `pair_result` of `rejected`, `failed` or `locked` | Best effort |
| `remote.device.revoke` | Revoke-all (M2: per device) | Best effort |
| `remote.attach` / `remote.detach` | Event stream opened / Disconnect or `detach` | Best effort |
| `remote.prompt` | Forwarded prompt (`{len, sha256}`) | Best effort |
| `remote.stop` | Forwarded stop | Best effort |
| `remote.refused` | Relay refusal (sampled) | Best effort |
| `remote.policy.set` | Org PUT | Best effort |

amp-relay writes these rows only through the database function `relay_record_audit` (#2399): it accepts only the
actions above that the relay records (not `remote.policy.set` or `remote.kill`), about a session the actor owns,
with the detail members above (`len`, `sha256`, `reason`, `code`, `channel`, `devices`, `sessions` too) as
short scalars, and fills the email, org, project, target and summary itself. The relay's database role can't
insert into `audit_log` or the webhook outbox. amp-api's webhook dispatcher takes the relay's rows from
`relay_audit_outbox` and renders and signs their deliveries from the stored row.

The harness also keeps a local log of remote ops (op, device, time, result), with no payloads.

### 12.5 What ends a session (#2424)

User decision, 2026-10-26: "I want the remote session to live, reconnect automatically and keep itself alive until
a user disconnects it." So by default a session has **no idle limit and no lifetime**. It ends only on:

- `/remote off` or quitting the chat on the laptop (`harness_ended`), or a signed `detach` (`detached`);
- **Disconnect** on the session page (`disconnected`), or **Revoke all** on Sessions (`revoked`);
- the org switch turned off (`policy_off`), the owner deactivated or losing access to the project (`revoked`);
- an org admin's opt-in idle limit (`idle`) or lifetime (`expired`), when set (8.3 "Org policy").

The **relay's kill switch** (amp-relay scaled to 0, or `relay.enabled: false`) is a pause, not an end: the session
stays `active`, the harness keeps trying (8.2) and the page shows "Laptop offline", and both resume when the relay
is back. Sleep, network loss, a relay restart or rollout, and a relay outage of any length are the same.

**Harness restart.** The harness has no chat resume: a chat started again is a new chat, with remote control
off until `/remote` (R-M2: it is turned on per session by a local action). A chat that exits normally ends its
session. A chat that is killed, or a reboot, leaves its session `active` and offline in AMP's list. Each running
session holds a locked record in `<state>/remote-live/<session_id>.json` (0600; the AMP origin, session id and
harness id, never a token); the OS drops the lock when the process dies. The next `/remote` on that computer
ends every session whose record nobody holds (for the same AMP origin) with a signed token refresh and
`DELETE` (reason `harness_ended`), the same way as an end the relay missed (8.2), and then registers the new
session. Until then the left-behind session stays listed, and Disconnect on the web page ends it. Devices paired
before the restart keep working without pairing again, because pins belong to the harness install (7.7).

**What mitigates a forgotten session** (threat model T-H-6, which the idle limit used to cover):

- the `REMOTE ON` badge in the TUI footer, always visible while remote control is on;
- the session list on `/sessions` (live sessions, online or offline) with Disconnect, and **Revoke all**;
- the org switch, which ends every session in the org within seconds, and deactivation or lost access;
- device trust of at most 30 days (`device_pin_days`): an unpaired or expired device can't drive the session;
- every command is signed by a device the owner paired on the laptop (R-M4, R-M5), so a live session is useless to
  anyone else;
- an org that wants the old behaviour sets an idle limit and a lifetime.

## 13. Versioning and compatibility

- **The envelope's `v` is the protocol major version.** This document is `v: 1`. A receiver MUST refuse any
  other major version: `unsupported_version` as a refusal, an HTTP 400 or a browser verdict. The UI shows a clear
  "update" message. Nothing is ever half-applied.
- **Additive within v1** (no bump):
  - new event ops (browsers ignore them, 5.7 step 9)
  - new ChatEvent `type`s (ignored)
  - new members in event bodies (ignored)
  - new HTTP response members
  - new error codes (treated generically)
  - new command ops, **only** when the harness advertises them in `hello.ops`
- **Breaking** (needs `v: 2`): any change to the signing input, canonicalization, the envelope members, the
  meaning of an existing field, or the strictness of command bodies.
- **Harness leg URL versioning:** `/relay/v1/`. A future `/relay/v2/` may live alongside it. Register negotiates
  with `protocol_versions`.
- **E2E later (R-L1)** is negotiated as a feature: the harness sets a flag in `hello`, and E2E traffic uses a new
  op whose body carries ciphertext under keys from ECDH over the pinned P-256 keys. It's invisible to clients
  that don't negotiate it.
- **Backward compatibility (decision section 9):**
  - Old harness with new AMP: nothing changes.
  - New harness with old AMP: `404`, and the harness says "not available".
  - #2424 made `expires_at_ms` and `idle_timeout_s` nullable (no limit). A harness from before it can't parse the
    `null` and fails to turn remote control on. M1 is unreleased (remote-m1), so this was amended within v1;
    update the harness together with amp-relay. A harness from after it reads the old numbers as before.
  - Instance off: `404`. Org off: `403 remote_disabled`.
  - `RunEvent` and `run --json` are unchanged. DB changes are new tables and a new org setting only.

## 14. Sequence diagrams

### 14.1 Enable

```mermaid
sequenceDiagram
  autonumber
  actor U as Person at laptop
  participant H as amp-harness
  participant R as amp-relay
  participant DB as Postgres
  U->>H: /remote
  H->>H: check AMP_HARNESS_DISABLE_REMOTE, real OAuth login (not amp_ token), TLS or loopback
  H->>H: load or create the P-256 harness key (keychain), harness_id = fp(pub)
  H->>R: POST /relay/v1/sessions {protocol_versions, harness_id, harness_pub, host, project_id} (Bearer OAuth)
  R->>R: verify token (aud amp-mcp, person, not amp_), org policy, limits
  R->>DB: insert remote_sessions, audit remote.enable (fail closed)
  R-->>H: 201 {session_id, connect_token, expires_at_ms, idle_timeout_s, ...}
  H->>R: GET /relay/v1/sessions/:id/commands (SSE, Bearer rct_)
  R-->>H: event: viewers {count: 0}
  H->>R: POST .../events [hello seq 1]
  H->>H: mint pairing secret, POST .../pairings {pairing_id, expires_at_ms}
  H-->>U: REMOTE ON · 0 devices, link + QR (#p=secret&h=harness_id)
  loop every 20 s
    H->>R: POST .../heartbeat
    R-->>H: ": ka" on the command stream
  end
```

### 14.2 Pair

```mermaid
sequenceDiagram
  autonumber
  actor U as Person
  participant B as Browser (phone)
  participant R as amp-relay
  participant H as amp-harness
  U->>B: open link / scan QR
  B->>B: read #p and #h, then history.replaceState (drop the fragment)
  B->>R: GET /api/sessions/:id (cookie, Channel ui, owner)
  R-->>B: {harness_id, harness_pub, host, online}
  B->>B: harness_id == h ? fp(harness_pub) == h ?
  B->>B: load or create the non-extractable P-256 device key (IndexedDB)
  B->>R: GET /api/sessions/:id/events (SSE)
  B->>B: derive pairing_id, k_mac, k_sas, build request, mac + sig, show code 7324-7649
  B->>R: POST /api/sessions/:id/pair {request, mac, sig}
  R->>R: owner check, pairing window, tries < 5, audit remote.pair (fail closed)
  R->>H: event: pair_request {request, mac, sig, relay:{user_id, email, user_agent}}
  R-->>B: 202 pending
  H->>H: checks 7.5 (pairing_id, user, fp, ts, MAC, sig)
  H-->>U: Allow "Safari on iPhone" (jsims@...)? code 7324-7649 [y/N]
  U->>H: y
  H->>H: pin {device_id, device_pub, user_id, pinned_until = now + 30 d}, consume secret
  H->>R: POST .../events [pair_result accepted (device_id = B), status]
  R->>R: verify, upsert remote_devices
  R-->>B: event: envelope pair_result accepted
  B->>B: verify with harness_pub (fp == h), pin harness 30 d
```

### 14.3 Chat (with a queued follow-up)

```mermaid
sequenceDiagram
  autonumber
  participant B as Browser
  participant R as Relay
  participant H as Harness
  B->>B: envelope{op: prompt, seq = max(last+1, now), ts, nonce}, sign (device key)
  B->>R: POST /api/sessions/:id/commands
  R->>R: 5.8 checks (size, shape, session, device known, sig, ts, rate)
  R->>H: event: command (compacted, unchanged)
  R-->>B: 202 forwarded
  H->>H: 5.6 checks, commit nonce + seq
  H->>R: events [ack started, chat_event user_message(origin remote), run_start, message_start]
  R-->>B: event: envelope ...
  loop streaming (about every 100 ms)
    H->>R: events [text_delta / thinking_delta / tool_start / tool_end]
    R-->>B: event: envelope ...
  end
  B->>R: POST commands {op: prompt} (while busy)
  R->>H: event: command
  H->>R: events [ack queued (position 1), chat_event queue]
  H->>R: events [message_end(usage), run_end completed]
  H->>R: events [user_message (queued prompt), run_start, ...]
```

### 14.4 Stop

```mermaid
sequenceDiagram
  autonumber
  participant B as Browser
  participant R as Relay
  participant H as Harness
  B->>R: POST commands {op: stop, body: {}}
  R->>H: event: command
  H->>H: verify, harness.abort() (manager + workers), clear the queue
  H->>R: events [ack done, chat_event queue{items: []}, notice "Stopped.", run_end aborted, status busy=false]
  R-->>B: event: envelope ...
  Note over B,H: A local Esc / /abort sends the same events, so every viewer sees the stop.
```

### 14.5 Reconnect (laptop sleep, network loss, browser reload)

```mermaid
sequenceDiagram
  autonumber
  participant B as Browser
  participant R as Relay
  participant H as Harness
  Note over H: laptop sleeps
  R->>R: no heartbeat for 60 s, so offline
  R-->>B: event: relay {type: offline}
  B->>R: POST commands
  R-->>B: 409 harness_offline (never queued)
  Note over H: laptop wakes
  H->>H: org limits set and passed? then end locally + DELETE (none by default, #2424). Else re-check the switch and pins
  H->>R: POST .../token (OAuth + signed proof, even hours after the token expired), the relay re-checks policy
  H->>R: GET .../commands (SSE, new stream replaces the old)
  H->>R: POST events [hello (seq continues)]
  R-->>B: event: relay {type: online}, event: envelope hello
  Note over B: page reload or network drop on the phone
  B->>R: GET /api/sessions/:id/events (Last-Event-ID: 57)
  R-->>B: attached, snapshot(hello, status), [gap if 58 was evicted], events 58..
  B->>B: drop seq <= last applied
```

### 14.6 Expire and other endings

```mermaid
sequenceDiagram
  autonumber
  participant B as Browser
  participant R as Relay
  participant H as Harness
  alt the org's opt-in idle limit or lifetime (harness clock; none by default, #2424)
    H->>R: POST events [bye {reason: idle|expired}]
    H->>R: DELETE /relay/v1/sessions/:id {reason}
    R->>R: audit remote.expire, drop the buffer
  else Disconnect or revoke-all in the web app
    B->>R: POST /api/sessions/:id/disconnect (or /api/sessions/revoke-all)
    R->>H: event: device_revoked (revoke-all), then event: end {reason}
    R->>R: audit remote.disable / remote.device.revoke
  else org switch off
    R->>H: event: end {reason: policy_off}
  end
  R-->>B: event: relay {type: ended, reason}, the stream closes
  H->>H: REMOTE OFF, the local session continues
  Note over R: relay backstop (org limits only): idle + 60 s or expires_at, it ends the session itself
```

## 15. Notes for implementers

**All teams**
- Run the vectors in CI (section 16). A mismatch is a contract bug: fix it and regenerate. Never edit a
  vector by hand.
- Never log envelopes, bodies, connect tokens, pairing secrets or keys. Log op, sizes, ids and codes.

**Go (amp-api module: amp-api and amp-relay)**
- **`Principal.Channel` / `InUI()` (epic 162) are on `main` but not on `remote-m1`**, which was cut from
  `harness-m4`. Before the browser leg can enforce `Channel == ui`, `main` has to be merged into `remote-m1`
  (the manager's call), or the relay ticket has to depend on that merge. Don't re-implement it differently.
- The REST listener (`:3001`) wraps everything in `authExceptHealth(restVerifier, …)`, whose audiences are the
  oauth2-proxy ones. The `/relay/v1/` routes need a **different** auth path: the MCP-audience verifier for
  register and token refresh, and the connect-token check for the rest. They must not sit behind
  `restVerifier`. `RejectCrossSiteWrites` is harmless there (the harness sends no `Origin`).
- Use a JCS implementation, not `encoding/json`, for canonical bytes. Parse with `UseNumber()` and reject
  duplicate keys and non-integer numbers.
- Signatures: `ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)` (Go 1.25) and
  `ecdsa.Verify(pub, sha256(input), r, s)` with r and s taken from the 64 bytes. `VerifyASN1` is the wrong
  function, because it expects DER.
- Store the forwarded command as `json.RawMessage` after `json.Compact`, so no SSE `data:` line contains a raw
  newline.
- The existing error helper `jsonErr` writes only `{"error"}`. Add the `code` member for these routes without
  changing the helper's behaviour for other routes.
- Rate-limit `refused` audit rows; audit `remote.enable` and `remote.pair` fail closed (the audit insert is
  best effort everywhere else today).

**Rust (harness)**
- Add `p256 = { version = "0.13", features = ["ecdsa", "sha256"] }` (or use ring/aws-lc-rs with the FIXED
  signature format). Don't add low-S normalization on verify.
- Command bodies: `#[serde(deny_unknown_fields)]` structs per op. The envelope: a struct with exactly 11
  fields, `deny_unknown_fields`, and `device_id: Option<String>` (serialized as `null`, never skipped).
  Canonicalize from a `serde_json::Value` parsed from the same text, reject `is_f64()` numbers, and check that
  every integer fits ±(2^53−1).
- The amp-core tee emitter turns rpi `AgentEvent`s into ChatEvents (section 6.4). Coalesce deltas **before**
  assigning a seq. Never replace an event that has been sent (unlike `WorkerFeed`).
- Remote prompt text goes to the agent as a user turn, **never** through `Shell::submit`'s slash-command
  routing.
- Sanitize `device_name`, `email` and `user_agent` before printing them in the y/N prompt (section 7.5).
- `cost_usd_micros = (cost_usd * 1e6).round() as i64`.

**TypeScript (amp-ui)**
- Device key: `crypto.subtle.generateKey({name: 'ECDSA', namedCurve: 'P-256'}, false, ['sign', 'verify'])`,
  stored as the `CryptoKeyPair` in IndexedDB. `subtle.sign({name: 'ECDSA', hash: 'SHA-256'}, …)` already
  returns P1363 `r‖s`.
- The canonicalizer is a few lines (section 3.3). Use `Object.keys().sort()`, not `localeCompare`.
- Treat every `relay` frame as unsigned status. Pin only from the link's `h` or an existing pin.
- `EventSource` doesn't notice silence: reopen it after 60 s without a keepalive.
- Strict CSP on `/sessions/:id` (no inline script, `connect-src 'self'`, no `unsafe-eval`). Render with the
  sanitizing Markdown component, without raw HTML or images.
- Baseline CSP on every other page (#2393): `script-src 'self'`, `object-src 'none'`, `base-uri 'none'`,
  `frame-ancestors 'none'`, same-origin connect. The device key can be used by any script on the AMP origin,
  so script injected on `/board` must not run either. Both policies live in `ui/src/remote/csp.ts`; nginx and
  `vite preview` serve them, and `csp.test.ts` keeps them in step.
- The strict policy only covers a **full load** of `/sessions/…`. Links to a session page are plain `<a href>`,
  never a router `<Link>`/`navigate()` (a test fails on one). The device key's private operations
  (`ensureDevice`, `signCommand`, `signPairRequest`, `pinHarness`) refuse unless the document was loaded at
  `/sessions/…` and is still there (`ui/src/remote/keyGate.ts`).
- Snapshots (`hello`/`status`) older than the one shown are ignored, and so are older `status` events, so a
  relay can't roll the status bar back with a replayed but genuine envelope.
- **Residual risk (T-UI-4/T-UI-5).** The key gate covers AMP's own code path, not the origin: the key is
  non-extractable, but script that does run on any AMP page can open the same IndexedDB store and sign with it.
  The baseline CSP is what keeps such script from running. The real fix is a step-up for prompts (R-S3:
  WebAuthn user verification per prompt or per short window, so a signature needs the person's presence),
  planned for a later milestone. Serving the session page from its own origin would also isolate the key.

## 16. Test vectors

The vectors are in `v2/testdata/remote-control/`. `gen_vectors.py` generates them (Python 3 standard library
only, deterministic). The `README.md` there shows how to load them from Rust, Go and TS.

| File | What it covers |
|---|---|
| `keys.json` | The five test key pairs (labelled test-only), raw public keys and fingerprints |
| `jcs.json` | Canonicalization cases (UTF-16 key order, escapes, U+2028, integers) and profile rejections |
| `fingerprints.json` | `fp` and the display fingerprint |
| `commands.json` | Valid prompt, stop and detach (including high-S and an ASCII-escaped wire), and every refusal of 5.6 with the receiver state |
| `events.json` | Valid hello, deltas, tool_start, message_end, ack, refused and pair_result, plus duplicate, bad signature, fake harness, wrong session, unknown op and unsupported version |
| `pairing.json` | Link, `pairing_id`, `k_mac`, `k_sas`, MAC, signature and SAS code, plus the pairing rejection cases |
| `token.json` | Token refresh proofs (8.2): valid (incl. high-S), and every refusal of the relay's proof check |

Each vector's `receiver` holds the state the check needs (`now_ms`, pins, `last_seq`, seen nonces). Signing is
tested as a round trip, because production ECDSA is randomized. Verification, canonical bytes, fingerprints and
pairing derivations are tested byte for byte.
