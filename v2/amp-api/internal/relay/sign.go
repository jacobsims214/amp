package relay

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// SignEnvelope signs env (the 10 unsigned members; any "sig" is replaced)
// with priv and returns the signed envelope as canonical JSON. The relay never
// signs anything itself; this is for tests and tools that play the harness or
// the browser. Values must be nil, bool, int, int64, string, []any or
// map[string]any.
func SignEnvelope(priv *ecdsa.PrivateKey, env map[string]any) ([]byte, error) {
	return signWithPrefix(priv, sigPrefixEnvelope, env)
}

// signWithPrefix signs JCS(env without "sig") under a domain prefix and
// returns env with its "sig" as canonical JSON.
func signWithPrefix(priv *ecdsa.PrivateKey, prefix string, env map[string]any) ([]byte, error) {
	unsigned := make(map[string]any, len(env))
	for k, v := range env {
		if k != "sig" {
			unsigned[k] = v
		}
	}
	sum := sha256.Sum256(canonical([]byte(prefix), unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	unsigned["sig"] = base64.RawURLEncoding.EncodeToString(sig)
	return canonical(nil, unsigned), nil
}
