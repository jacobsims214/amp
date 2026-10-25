package relay

// The token-refresh proof (protocol.md 8.2, #2390): renewing a session's
// connect token takes, besides the owner's OAuth login, a signature by the
// session's registered harness key. Many programs hold the person's own
// OAuth token (Claude Code with the AMP plugin, a manager on another
// machine); without the proof any of them could mint a connect token and take
// over the session's command stream (review #2381, M1).

import (
	"crypto/ecdsa"
	"regexp"
	"time"
)

const sigPrefixToken = "amp-rc/1 token\n"

// tokenProofMembers are the exactly 7 members of a refresh proof.
var tokenProofMembers = []string{"v", "alg", "session_id", "harness_id", "ts", "nonce", "sig"}

var nonceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// TokenProofReceiver is the state the proof check needs: the session in the
// path, its registered harness key and the clock.
type TokenProofReceiver struct {
	NowMS     int64
	SessionID string
	HarnessID string
	Harness   *ecdsa.PublicKey
	// Seen reports whether the nonce was already used within the window
	// (and records it when it wasn't); nil skips the replay check.
	Seen func(nonce string) bool
}

// CheckTokenProof checks the body of POST /relay/v1/sessions/:id/token,
// {"proof": {...}}, and returns "ok" or the first failing code: malformed,
// unsupported_version, unsupported_alg, wrong_session, wrong_harness,
// bad_signature, stale_ts, replayed_nonce (protocol.md 8.2).
func CheckTokenProof(body []byte, rx TokenProofReceiver) string {
	if len(body) > MaxSmallBody {
		return CodeTooLarge
	}
	v, err := parseStrict(body)
	if err != nil {
		return CodeMalformed
	}
	outer, ok := v.(map[string]any)
	if !ok || len(outer) != 1 {
		return CodeMalformed
	}
	p, ok := outer["proof"].(map[string]any)
	if !ok || len(p) != len(tokenProofMembers) {
		return CodeMalformed
	}
	for _, k := range tokenProofMembers {
		if _, ok := p[k]; !ok {
			return CodeMalformed
		}
	}
	ver, ok1 := p["v"].(int64)
	alg, ok2 := p["alg"].(string)
	sid, ok3 := p["session_id"].(string)
	hid, ok4 := p["harness_id"].(string)
	ts, ok5 := p["ts"].(int64)
	nonce, ok6 := p["nonce"].(string)
	sig, ok7 := p["sig"].(string)
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7) || !nonceRE.MatchString(nonce) {
		return CodeMalformed
	}
	switch {
	case ver != ProtocolVersion:
		return CodeUnsupportedVersion
	case alg != Alg:
		return CodeUnsupportedAlg
	case sid != rx.SessionID:
		return CodeWrongSession
	case hid != rx.HarnessID:
		return CodeWrongHarness
	}
	unsigned := make(map[string]any, len(p)-1)
	for k, val := range p {
		if k != "sig" {
			unsigned[k] = val
		}
	}
	if !verifyES256(rx.Harness, canonical([]byte(sigPrefixToken), unsigned), sig) {
		return CodeBadSignature
	}
	if d := ts - rx.NowMS; d > TSWindow.Milliseconds() || d < -TSWindow.Milliseconds() {
		return CodeStaleTS
	}
	if rx.Seen != nil && rx.Seen(nonce) {
		return CodeReplayedNonce
	}
	return CodeOK
}

// SignTokenProof builds a signed refresh body {"proof": {...}} for tests and
// tools that play the harness. The relay never signs anything itself.
func SignTokenProof(priv *ecdsa.PrivateKey, sessionID, harnessID string, ts int64, nonce string) ([]byte, error) {
	env := map[string]any{"v": int64(ProtocolVersion), "alg": Alg, "session_id": sessionID, "harness_id": harnessID,
		"ts": ts, "nonce": nonce}
	signed, err := signWithPrefix(priv, sigPrefixToken, env)
	if err != nil {
		return nil, err
	}
	out := append([]byte(`{"proof":`), signed...)
	return append(out, '}'), nil
}

// nonceCache remembers refresh-proof nonces for twice the ts window, so a
// captured proof can't be replayed while its ts is still accepted. It only
// grows with proofs whose signature verified (the harness's own).
type nonceCache struct {
	seen map[string]time.Time
}

// seenOrAdd reports whether key was recorded in the last ttl, and records it.
// The caller holds the lock that guards the cache.
func (c *nonceCache) seenOrAdd(key string, now time.Time, ttl time.Duration) bool {
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	for k, at := range c.seen {
		if now.Sub(at) > ttl {
			delete(c.seen, k)
		}
	}
	if _, ok := c.seen[key]; ok {
		return true
	}
	c.seen[key] = now
	return false
}
