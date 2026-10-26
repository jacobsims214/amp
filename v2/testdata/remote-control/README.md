# Remote control protocol v1: shared test vectors

The vectors for [`v2/docs/remote-control/protocol.md`](../../docs/remote-control/protocol.md), shared by the
Rust harness, the Go relay in amp-api and the TypeScript web app. Ticket #2369.

**Every key, secret and nonce here is derived from a public label. They are test-only: never reuse them.**

## Regenerating

```sh
python3 v2/testdata/remote-control/gen_vectors.py          # rewrites the *.json files
python3 v2/testdata/remote-control/gen_vectors.py --check  # exit 1 if a committed file is out of date
```

The generator uses only the Python 3.9+ standard library and is deterministic: P-256 ECDSA with RFC 6979
nonces, RFC 8785 JCS, HKDF and HMAC are all implemented in the script. It also contains the **reference
checks** (`check_command`, `check_event`, the pairing check). It refuses to write a vector whose expected
result disagrees with them, so the order of checks in protocol.md sections 5.6, 5.7 and 7.5 is pinned down by
code.

On generation, the output was cross-checked against three independent implementations: pyca/cryptography 46
(same RFC 6979 signatures byte for byte), Node 26 WebCrypto (verification, JCS, HKDF, HMAC, SAS), and Go 1.25
`crypto/ecdsa` (verification).

Never edit a JSON file by hand. Change the generator (or the spec), regenerate and commit both.

## Files

Every file has a `_meta` member and a `description`. Byte strings are base64url without padding unless the name
ends in `_hex`.

| File | Shape | How to test |
|---|---|---|
| `keys.json` | `keys[]`: `{label, private_scalar_hex, public_raw_b64u, public_raw_hex, fingerprint, fingerprint_display}` | Derive the public key from the scalar; check both fingerprints. Use the private keys for round-trip signing tests. |
| `jcs.json` | `canonical[]`: `{name, input, canonical}`, `reject[]`: `{name, input}` | `canonicalize(parse(input)) == canonical` byte for byte; every `reject` input fails (see the TS note below) |
| `fingerprints.json` | `cases[]`: `{public_raw_b64u, sha256_hex, fingerprint, fingerprint_display}` | Exact match |
| `commands.json` | `vectors[]`: `{name, note, receiver, wire, expect, signing_input?}` | The harness (and the relay, for steps 1-10) runs `wire` through its command check with `receiver` and expects `expect` |
| `events.json` | `vectors[]`: the same shape, with a browser `receiver` | The browser (and the relay, for steps 1-7) runs its event check |
| `pairing.json` | `inputs`, `link`, `derived`, `request`, `signing_input`, `message`, `reject[]` | Reproduce `derived` and `message.mac` exactly; verify `message.sig`; each `reject[]` gives the harness code |

`receiver` in `commands.json`:

```json
{"now_ms": 1792948800000, "user_id": 1001, "session_id": "rs_…", "harness_id": "…",
 "pinned_devices": [{"device_id", "device_pub", "user_id", "pinned_until_ms"}],
 "last_seq": {"<device_id>": 1792948700000}, "seen_nonces": ["…"]}
```

`receiver` in `events.json`: `{"now_ms", "session_id", "pinned_harness": {"harness_id", "harness_pub"}, "last_applied_seq"}`.

`expect` values:

- **Commands:** `ok` or a refusal code from protocol.md 9.2.
- **Events:** `ok`, `duplicate`, `ignored_unknown_op` or a code from 9.4.

The receiver's clock is `now_ms`: inject a clock, don't read the system time.

**`wire` is deliberately non-canonical:** `sig` comes first, the field order is reversed, there are spaces, and
some vectors escape every non-ASCII character as `\uXXXX`. An implementation that verifies the received bytes
instead of JCS of the parsed value fails these vectors. That is intended.

**Signing** isn't compared byte for byte. Browsers sign with random nonces, so test your signer as a round trip
(sign with a `keys.json` key, then verify with your verifier and the vectors' verifier logic). Verification,
canonical bytes, fingerprints and the pairing derivations are compared byte for byte.

## Loading the vectors

The path from each package to the vectors:

| Package | Relative path |
|---|---|
| `v2/harness/crates/<crate>` | `../../../testdata/remote-control` |
| `v2/amp-api/internal/<pkg>` | `../../../testdata/remote-control` |
| `v2/ui` | `../testdata/remote-control` |

### Rust (harness, `cargo test`)

```rust
// crates/amp-core/tests/remote_vectors.rs (or wherever the protocol module lives)
use serde::Deserialize;
use std::path::PathBuf;

fn vectors_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/remote-control")
}

#[derive(Deserialize)]
struct CommandFile { vectors: Vec<CommandVector> }

#[derive(Deserialize)]
struct CommandVector { name: String, wire: String, expect: String, receiver: serde_json::Value }

#[test]
fn command_vectors() {
    let text = std::fs::read_to_string(vectors_dir().join("commands.json")).unwrap();
    let file: CommandFile = serde_json::from_str(&text).unwrap();
    for v in file.vectors {
        let rx = receiver_from_json(&v.receiver); // build your Receiver: clock = now_ms, pins, last_seq, nonces
        let got = match check_command(v.wire.as_bytes(), &rx) {
            Ok(_) => "ok".to_string(),
            Err(code) => code.as_str().to_string(),
        };
        assert_eq!(got, v.expect, "vector {}", v.name);
    }
}
```

Recommended crates: `p256` with features `ecdsa` and `sha256` (P1363 `Signature::from_slice`, and it doesn't
reject high-S), `hkdf` and `hmac` 0.13 (already in Cargo.lock), and `base64` with the `URL_SAFE_NO_PAD` engine.
For canonicalization, use `serde_jcs`, or a canonicalizer over `serde_json::Value` with the `preserve_order`
feature off.

### Go (amp-api, `go test`)

```go
// internal/relay/vectors_test.go
package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type commandVector struct {
	Name     string          `json:"name"`
	Wire     string          `json:"wire"`
	Expect   string          `json:"expect"`
	Receiver json.RawMessage `json:"receiver"`
}

func loadVectors[T any](t *testing.T, file string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "remote-control", file))
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCommandVectors(t *testing.T) {
	file := loadVectors[struct{ Vectors []commandVector }](t, "commands.json")
	for _, v := range file.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			got := checkCommandForTest([]byte(v.Wire), v.Receiver) // "ok" or the refusal code
			if got != v.Expect {
				t.Fatalf("got %q, want %q", got, v.Expect)
			}
		})
	}
}
```

The relay runs only steps 1-10 of the command check, so for vectors whose `expect` is `replayed_nonce`,
`seq_not_increasing`, `unknown_op` or `invalid_body` the relay's own check returns "ok" (it forwards them and
leaves those to the harness). Assert that explicitly instead of skipping the vectors.

Use `ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)` and `ecdsa.Verify(pub, sha256(input), r, s)`, with
`base64.RawURLEncoding.Strict()`. Don't use `encoding/json` to produce canonical bytes (its escaping of U+2028
isn't JCS; `jcs.json` catches it).

### TypeScript (amp-ui)

`v2/ui` has no unit-test runner yet. The ticket that adds the protocol module should add Vitest
(`npm i -D vitest`, a `"test": "vitest run"` script, and an `npm test` step in `.github/workflows/ui.yml`).
Vitest runs on Node, whose `globalThis.crypto.subtle` is the same WebCrypto API browsers have.

```ts
// src/remote/protocol.test.ts
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { checkEvent, jcs } from './protocol'

const dir = fileURLToPath(new URL('../../../testdata/remote-control/', import.meta.url))
const load = (f: string) => JSON.parse(readFileSync(dir + f, 'utf8'))

describe('event vectors', () => {
  for (const v of load('events.json').vectors) {
    it(v.name, async () => {
      expect(await checkEvent(v.wire, v.receiver)).toBe(v.expect)
    })
  }
})

describe('jcs', () => {
  for (const c of load('jcs.json').canonical) {
    it(c.name, () => expect(jcs(JSON.parse(c.input))).toBe(c.canonical))
  }
})
```

`JSON.parse` silently accepts the duplicate-key and `1e3` inputs in `jcs.json`'s `reject` list (protocol.md 3.3
explains why that's acceptable for the browser). The browser must still reject `1.5`, `9007199254740992` and
the lone surrogate, so assert those three.

Command signing in the UI uses a non-extractable key from
`crypto.subtle.generateKey({name: 'ECDSA', namedCurve: 'P-256'}, false, ['sign', 'verify'])`. To test the
signer against the harness's expectations, import a `keys.json` private key (build a JWK `{kty: 'EC', crv:
'P-256', d, x, y}` from the scalar and the raw point), sign a command, and check the result with the same
verifier the event vectors use. That proves the 64-byte P1363 output and the signing input are right.
