package relay

// The shared protocol vectors (v2/testdata/remote-control, protocol.md 16),
// run through the relay's share of the checks.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadVectors(t *testing.T, file string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "remote-control", file))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

func TestJCSVectors(t *testing.T) {
	var f struct {
		Canonical []struct{ Name, Input, Canonical string }
		Reject    []struct{ Name, Input string }
	}
	loadVectors(t, "jcs.json", &f)
	if len(f.Canonical) == 0 || len(f.Reject) == 0 {
		t.Fatal("no jcs vectors")
	}
	for _, c := range f.Canonical {
		v, err := parseStrict([]byte(c.Input))
		if err != nil {
			t.Errorf("%s: parse: %v", c.Name, err)
			continue
		}
		if got := string(canonical(nil, v)); got != c.Canonical {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, c.Canonical)
		}
	}
	for _, c := range f.Reject {
		if _, err := parseStrict([]byte(c.Input)); err == nil {
			t.Errorf("%s: accepted %q", c.Name, c.Input)
		}
	}
}

func TestJCSProfileEdges(t *testing.T) {
	for _, bad := range []string{
		``, `{`, `{"a":1,}`, `[1,]`, `{"a":01}`, `{"a":-}`, `{"a":1}x`, `{"a":"\x01"}`, `{"a":"\q"}`,
		`{"a":"\udc00"}`, `{"a":"\ud800\u0041"}`, `{"a":1E2}`, `{"a":-9007199254740992}`, `{"a":tru}`,
		"{\"a\":\"\xff\"}",
	} {
		if _, err := parseStrict([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	deep := ""
	for i := 0; i < 100; i++ {
		deep += "["
	}
	if _, err := parseStrict([]byte(deep)); err == nil {
		t.Error("accepted unbounded nesting")
	}
	v, err := parseStrict([]byte(` {"b" : [true,false,null, -0], "a":"\/x"} `))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(canonical(nil, v)); got != `{"a":"/x","b":[true,false,null,0]}` {
		t.Errorf("got %s", got)
	}
}

func TestFingerprintVectors(t *testing.T) {
	var f struct {
		Cases []struct {
			PublicRawB64u      string `json:"public_raw_b64u"`
			Fingerprint        string `json:"fingerprint"`
			FingerprintDisplay string `json:"fingerprint_display"`
		}
	}
	loadVectors(t, "fingerprints.json", &f)
	for _, c := range f.Cases {
		raw, err := b64uDecode(c.PublicRawB64u)
		if err != nil {
			t.Fatal(err)
		}
		if got := Fingerprint(raw); got != c.Fingerprint {
			t.Errorf("fp %s, want %s", got, c.Fingerprint)
		}
		if got := FingerprintDisplay(c.PublicRawB64u); got != c.FingerprintDisplay {
			t.Errorf("display %s, want %s", got, c.FingerprintDisplay)
		}
	}
}

type pinnedDevice struct {
	DeviceID      string `json:"device_id"`
	DevicePub     string `json:"device_pub"`
	UserID        int    `json:"user_id"`
	PinnedUntilMS int64  `json:"pinned_until_ms"`
}

// relayVerdict maps the harness's expected code to the relay's: the relay
// runs steps 1-10 only, so the harness-only refusals pass it (and are
// forwarded for the harness to refuse).
func relayVerdict(code string) string {
	switch code {
	case "replayed_nonce", "seq_not_increasing", "unknown_op", "invalid_body":
		return CodeOK
	}
	return code
}

func TestCommandVectors(t *testing.T) {
	var f struct {
		Vectors []struct {
			Name, Wire, Expect string
			SigningInput       string `json:"signing_input"`
			Receiver           struct {
				NowMS         int64          `json:"now_ms"`
				UserID        int            `json:"user_id"`
				SessionID     string         `json:"session_id"`
				HarnessID     string         `json:"harness_id"`
				PinnedDevices []pinnedDevice `json:"pinned_devices"`
			}
		}
	}
	loadVectors(t, "commands.json", &f)
	if len(f.Vectors) < 20 {
		t.Fatalf("only %d command vectors", len(f.Vectors))
	}
	for _, v := range f.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			rx := CommandReceiver{
				NowMS: v.Receiver.NowMS, UserID: v.Receiver.UserID,
				SessionID: v.Receiver.SessionID, HarnessID: v.Receiver.HarnessID,
				Pin: func(id string) *DevicePin {
					for _, d := range v.Receiver.PinnedDevices {
						if d.DeviceID == id {
							return &DevicePin{DevicePub: d.DevicePub, UserID: d.UserID, PinnedUntilMS: d.PinnedUntilMS}
						}
					}
					return nil
				},
			}
			got, env := CheckCommand([]byte(v.Wire), rx)
			if want := relayVerdict(v.Expect); got != want {
				t.Fatalf("got %q, want %q (harness verdict %q)", got, want, v.Expect)
			}
			if v.SigningInput != "" && string(env.signingInput()) != v.SigningInput {
				t.Fatalf("signing input\n got %q\nwant %q", env.signingInput(), v.SigningInput)
			}
		})
	}
}

func TestEventVectors(t *testing.T) {
	var f struct {
		Vectors []struct {
			Name, Wire, Expect string
			Receiver           struct {
				SessionID     string `json:"session_id"`
				PinnedHarness struct {
					HarnessID  string `json:"harness_id"`
					HarnessPub string `json:"harness_pub"`
				} `json:"pinned_harness"`
			}
		}
	}
	loadVectors(t, "events.json", &f)
	if len(f.Vectors) < 10 {
		t.Fatalf("only %d event vectors", len(f.Vectors))
	}
	for _, v := range f.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			pub, ok := parsePub(v.Receiver.PinnedHarness.HarnessPub)
			if !ok {
				t.Fatal("bad harness key")
			}
			got, _ := CheckEvent([]byte(v.Wire), EventReceiver{
				SessionID: v.Receiver.SessionID, HarnessID: v.Receiver.PinnedHarness.HarnessID, Harness: pub,
			})
			// Steps 8-9 (duplicates, unknown ops) aren't part of the
			// signature-level check: the relay sequences events itself and
			// buffers unknown ops for the browser to ignore.
			want := v.Expect
			if want == "duplicate" || want == "ignored_unknown_op" {
				want = CodeOK
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// testKey loads a keys.json key as an ECDSA private key.
func testKey(t *testing.T, label string) *ecdsa.PrivateKey {
	t.Helper()
	var f struct {
		Keys []struct {
			Label            string `json:"label"`
			PrivateScalarHex string `json:"private_scalar_hex"`
			PublicRawB64u    string `json:"public_raw_b64u"`
			Fingerprint      string `json:"fingerprint"`
		}
	}
	loadVectors(t, "keys.json", &f)
	for _, k := range f.Keys {
		if k.Label != label {
			continue
		}
		d, err := hex.DecodeString(k.PrivateScalarHex)
		if err != nil {
			t.Fatal(err)
		}
		priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := priv.PublicKey.Bytes()
		if Fingerprint(raw) != k.Fingerprint || b64(raw) != k.PublicRawB64u {
			t.Fatalf("%s: key mismatch", label)
		}
		return priv
	}
	t.Fatalf("no key %s", label)
	return nil
}

// Signing is a round trip (production ECDSA is randomized): sign with a
// vector key, verify with the relay's check.
func TestSignRoundTrip(t *testing.T) {
	dev := testKey(t, "device-a")
	devRaw, _ := dev.PublicKey.Bytes()
	devID := Fingerprint(devRaw)
	env := map[string]any{
		"v": 1, "alg": "ES256", "session_id": "rs_AAAAAAAAAAAAAAAAAAAAAA", "harness_id": "h", "device_id": devID,
		"seq": int64(5), "ts": int64(1000), "nonce": "n", "op": "prompt",
		"body": map[string]any{"text": "ünïcode \u2028 </script>", "client_msg_id": "m"},
	}
	wire, err := SignEnvelope(dev, env)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := b64(devRaw)
	code, _ := CheckCommand(wire, CommandReceiver{
		NowMS: 1000, UserID: 1, SessionID: "rs_AAAAAAAAAAAAAAAAAAAAAA", HarnessID: "h",
		Pin: func(string) *DevicePin { return &DevicePin{DevicePub: pubB64, UserID: 1, PinnedUntilMS: 2000} },
	})
	if code != CodeOK {
		t.Fatalf("round trip: %s", code)
	}
}

// token.json: the refresh proof (protocol.md 8.2, #2390).
func TestTokenProofVectors(t *testing.T) {
	var f struct {
		SigningInput string `json:"signing_input"`
		Vectors      []struct {
			Name     string
			Body     string
			Expect   string
			Receiver struct {
				NowMS      int64    `json:"now_ms"`
				SessionID  string   `json:"session_id"`
				HarnessID  string   `json:"harness_id"`
				HarnessPub string   `json:"harness_pub"`
				SeenNonces []string `json:"seen_nonces"`
			}
		}
	}
	loadVectors(t, "token.json", &f)
	if len(f.Vectors) < 10 || f.SigningInput == "" {
		t.Fatal("no token vectors")
	}
	for _, v := range f.Vectors {
		pub, ok := parsePub(v.Receiver.HarnessPub)
		if !ok {
			t.Fatalf("%s: bad harness_pub", v.Name)
		}
		seen := v.Receiver.SeenNonces
		got := CheckTokenProof([]byte(v.Body), TokenProofReceiver{
			NowMS: v.Receiver.NowMS, SessionID: v.Receiver.SessionID, HarnessID: v.Receiver.HarnessID, Harness: pub,
			Seen: func(n string) bool {
				for _, s := range seen {
					if s == n {
						return true
					}
				}
				return false
			},
		})
		if got != v.Expect {
			t.Errorf("%s: got %s, want %s", v.Name, got, v.Expect)
		}
	}
}

// SignTokenProof round-trips through CheckTokenProof.
func TestTokenProofRoundTrip(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := priv.PublicKey.Bytes()
	id := Fingerprint(raw)
	body, err := SignTokenProof(priv, "rs_x", id, 1000, "AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	rx := TokenProofReceiver{NowMS: 1000, SessionID: "rs_x", HarnessID: id, Harness: &priv.PublicKey,
		Seen: func(string) bool { calls++; return calls > 1 }}
	if got := CheckTokenProof(body, rx); got != CodeOK {
		t.Fatalf("round trip: %s", got)
	}
	if got := CheckTokenProof(body, rx); got != CodeReplayedNonce {
		t.Fatalf("replay: %s", got)
	}
}
