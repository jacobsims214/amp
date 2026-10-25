package relay

// The envelope (protocol.md section 4) and the relay's share of the checks:
// commands get steps 1-10 of section 5.6 (the harness does 11-15), events get
// steps 1-7 of section 5.7. The relay is not trusted: it filters early so junk
// never reaches the laptop, but the harness and the browser verify for
// themselves.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"math/big"
	"time"
)

// Protocol constants (protocol.md sections 2, 3 and 10).
const (
	ProtocolVersion = 1
	Alg             = "ES256"

	MaxCommandBytes = 64 << 10 // a command envelope
	MaxEventBytes   = 1 << 20  // an event envelope
	MaxBatchBytes   = 2 << 20  // a POST .../events body
	MaxBatchEvents  = 64
	MaxSmallBody    = 8 << 10 // register and pair request bodies
	TSWindow        = 60 * time.Second

	sigPrefixEnvelope = "amp-rc/1 envelope\n"
)

// Refusal codes (protocol.md section 9).
const (
	CodeOK                 = "ok"
	CodeTooLarge           = "too_large"
	CodeMalformed          = "malformed"
	CodeUnsupportedVersion = "unsupported_version"
	CodeUnsupportedAlg     = "unsupported_alg"
	CodeWrongSession       = "wrong_session"
	CodeWrongHarness       = "wrong_harness"
	CodeUnknownDevice      = "unknown_device"
	CodeDeviceExpired      = "device_expired"
	CodeBadSignature       = "bad_signature"
	CodeStaleTS            = "stale_ts"
)

// envelopeMembers are the exactly 11 members of every envelope.
var envelopeMembers = []string{"v", "alg", "session_id", "harness_id", "device_id", "seq", "ts", "nonce", "op", "body", "sig"}

// Envelope is a parsed command or event.
type Envelope struct {
	V         int64
	Alg       string
	SessionID string
	HarnessID string
	DeviceID  *string
	Seq       int64
	TS        int64
	Nonce     string
	Op        string
	Body      map[string]any
	Sig       string

	value map[string]any // the parsed value, for the signing input
}

// parseEnvelope parses text under the profile (3.3) and checks the envelope
// shape: exactly the 11 members with the right types. Any failure is
// malformed.
func parseEnvelope(text []byte) (*Envelope, bool) {
	v, err := parseStrict(text)
	if err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != len(envelopeMembers) {
		return nil, false
	}
	for _, k := range envelopeMembers {
		if _, ok := m[k]; !ok {
			return nil, false
		}
	}
	e := &Envelope{value: m}
	var ok1, ok2, ok3, ok4, ok5, ok6, ok7, ok8, ok9, ok10 bool
	e.V, ok1 = m["v"].(int64)
	e.Alg, ok2 = m["alg"].(string)
	e.SessionID, ok3 = m["session_id"].(string)
	e.HarnessID, ok4 = m["harness_id"].(string)
	e.Seq, ok5 = m["seq"].(int64)
	e.TS, ok6 = m["ts"].(int64)
	e.Nonce, ok7 = m["nonce"].(string)
	e.Op, ok8 = m["op"].(string)
	e.Body, ok9 = m["body"].(map[string]any)
	e.Sig, ok10 = m["sig"].(string)
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7 && ok8 && ok9 && ok10) {
		return nil, false
	}
	switch d := m["device_id"].(type) {
	case nil:
	case string:
		e.DeviceID = &d
	default:
		return nil, false
	}
	return e, true
}

// signingInput is "amp-rc/1 envelope\n" + JCS(envelope without sig).
func (e *Envelope) signingInput() []byte {
	unsigned := make(map[string]any, len(e.value)-1)
	for k, v := range e.value {
		if k != "sig" {
			unsigned[k] = v
		}
	}
	return canonical([]byte(sigPrefixEnvelope), unsigned)
}

// b64uDecode is strict base64url without padding (protocol section 2).
func b64uDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

// parsePub parses a b64u raw uncompressed P-256 point.
func parsePub(b64 string) (*ecdsa.PublicKey, bool) {
	raw, err := b64uDecode(b64)
	if err != nil || len(raw) != 65 || raw[0] != 0x04 {
		return nil, false
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		return nil, false
	}
	return pub, true
}

// verifyES256 checks a b64u IEEE P1363 (r||s) signature over msg. High-S
// signatures are accepted (WebCrypto doesn't normalize s); r and s must be
// in [1, n-1], which ecdsa.Verify enforces.
func verifyES256(pub *ecdsa.PublicKey, msg []byte, sigB64 string) bool {
	sig, err := b64uDecode(sigB64)
	if err != nil || len(sig) != 64 || pub == nil {
		return false
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	sum := sha256.Sum256(msg)
	return ecdsa.Verify(pub, sum[:], r, s)
}

// DevicePin is what the relay knows about a paired device (remote_devices):
// enough for the early command checks, never for trust decisions.
type DevicePin struct {
	DevicePub     string
	UserID        int
	PinnedUntilMS int64
}

// CommandReceiver is the state the relay's command check needs.
type CommandReceiver struct {
	NowMS     int64
	UserID    int // the session owner
	SessionID string
	HarnessID string
	// Pin looks up the device's pin; nil when the relay knows none.
	Pin func(deviceID string) *DevicePin
}

// CheckCommand runs steps 1-10 of protocol.md 5.6 on a command's received
// text and returns the first failing code, or "ok". Steps 11-13 (nonce, seq,
// op allowlist, body schema) belong to the harness: a command that would
// fail only those is "ok" here and is forwarded.
func CheckCommand(text []byte, rx CommandReceiver) (string, *Envelope) {
	if len(text) > MaxCommandBytes {
		return CodeTooLarge, nil
	}
	env, ok := parseEnvelope(text)
	if !ok {
		return CodeMalformed, nil
	}
	switch {
	case env.V != ProtocolVersion:
		return CodeUnsupportedVersion, env
	case env.Alg != Alg:
		return CodeUnsupportedAlg, env
	case env.SessionID != rx.SessionID:
		return CodeWrongSession, env
	case env.HarnessID != rx.HarnessID:
		return CodeWrongHarness, env
	}
	var pin *DevicePin
	if env.DeviceID != nil && rx.Pin != nil {
		pin = rx.Pin(*env.DeviceID)
	}
	if pin == nil || pin.UserID != rx.UserID {
		return CodeUnknownDevice, env
	}
	if pin.PinnedUntilMS <= rx.NowMS {
		return CodeDeviceExpired, env
	}
	pub, ok := parsePub(pin.DevicePub)
	if !ok || !verifyES256(pub, env.signingInput(), env.Sig) {
		return CodeBadSignature, env
	}
	if d := env.TS - rx.NowMS; d > TSWindow.Milliseconds() || d < -TSWindow.Milliseconds() {
		return CodeStaleTS, env
	}
	return CodeOK, env
}

// EventReceiver is the state the relay's event check needs: the session and
// the harness key registered for it.
type EventReceiver struct {
	SessionID string
	HarnessID string
	Harness   *ecdsa.PublicKey
}

// CheckEvent runs steps 1-7 of protocol.md 5.7 on one event's received text,
// with the registered harness key, and returns the first failing code or
// "ok". Sequencing (step 8 and contiguity) and unknown ops are the caller's.
func CheckEvent(text []byte, rx EventReceiver) (string, *Envelope) {
	if len(text) > MaxEventBytes {
		return CodeTooLarge, nil
	}
	env, ok := parseEnvelope(text)
	if !ok {
		return CodeMalformed, nil
	}
	switch {
	case env.V != ProtocolVersion:
		return CodeUnsupportedVersion, env
	case env.Alg != Alg:
		return CodeUnsupportedAlg, env
	case env.SessionID != rx.SessionID:
		return CodeWrongSession, env
	case env.HarnessID != rx.HarnessID:
		return CodeWrongHarness, env
	case !verifyES256(rx.Harness, env.signingInput(), env.Sig):
		return CodeBadSignature, env
	}
	return CodeOK, env
}

// Fingerprint is b64u(SHA-256(raw public key)).
func Fingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// FingerprintDisplay is the people-facing form: the first 8 bytes of the
// SHA-256, upper-case hex in groups of 4 ("29CA-E6B5-00C2-66CB").
func FingerprintDisplay(pubB64 string) string {
	raw, err := b64uDecode(pubB64)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	const hexdig = "0123456789ABCDEF"
	out := make([]byte, 0, 19)
	for i := 0; i < 8; i++ {
		if i > 0 && i%2 == 0 {
			out = append(out, '-')
		}
		out = append(out, hexdig[sum[i]>>4], hexdig[sum[i]&0xF])
	}
	return string(out)
}

// b64 is base64url without padding.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
