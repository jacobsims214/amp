package receipts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"
)

// UploadTokenTTL is how long a one-time upload URL is valid.
const UploadTokenTTL = 15 * time.Minute

// NewUploadToken mints a one-time upload token: 32 random bytes, base64url
// (no padding). Only its hash (HashToken) is stored, like the relay's
// connect tokens: the database never holds a usable credential.
func NewUploadToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken is a token's stored form: the lowercase hex sha256 of it.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ValidTokenShape reports whether s could be a token NewUploadToken made
// (43 base64url characters), so junk is refused before any lookup.
func ValidTokenShape(s string) bool {
	if len(s) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil
}
