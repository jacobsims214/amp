package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"

	"github.com/simstech/amp-api/internal/repository"
)

// AMP API tokens: "amp_" followed by 43 characters of random base64url.
// Dex tokens are JWTs ("eyJ..."), so the prefix tells the two apart. Only
// the SHA-256 of a token is stored.

const apiTokenPrefix = "amp_"

// IsAPIToken reports whether a bearer token is an AMP API token.
func IsAPIToken(raw string) bool { return strings.HasPrefix(raw, apiTokenPrefix) }

// NewAPIToken generates a token, its hash for storage, and the prefix shown
// to help people recognise it.
func NewAPIToken() (token, hash, display string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", err
	}
	token = apiTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashAPIToken(token), token[:len(apiTokenPrefix)+6], nil
}

// HashAPIToken is how a token is stored and looked up.
func HashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewAPITokenVerifier returns a Verifier that accepts only AMP API tokens
// (no Dex). For tests, and for listeners that serve agents alone.
func NewAPITokenVerifier(repo *repository.Repo) *Verifier {
	return &Verifier{enabled: true, repo: repo, label: "api-token"}
}
