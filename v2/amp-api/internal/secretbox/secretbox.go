// Package secretbox encrypts secrets AMP stores in the database (webhook
// auth headers and signing secrets, #2441) with AES-256-GCM under a key
// that lives only in a Kubernetes Secret, never in the database, so a
// pg_dump or a backup holds ciphertext.
//
// A sealed value is text: "enc:v1:<key id>:<base64url(nonce || ciphertext)>".
// The key id lets the key rotate: values sealed under a retired key still
// open (AMP_SECRETS_RETIRED_KEYS) and are re-sealed under the current key
// at startup. Each value is bound to its purpose (the column it lives in)
// as additional data, so a ciphertext copied into another column won't
// open.
//
// Values without the prefix are legacy plaintext: Open returns them as
// they are, so rows written before encryption keep working until they are
// re-sealed.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Environment variables the keys come from (base64 of 32 random bytes,
// e.g. `openssl rand -base64 32`).
const (
	// EnvKey is the current key: it seals, and opens what it sealed.
	EnvKey = "AMP_SECRETS_KEY"
	// EnvRetiredKeys are earlier keys (comma-separated), kept only to open
	// values sealed before a rotation until they are re-sealed.
	EnvRetiredKeys = "AMP_SECRETS_RETIRED_KEYS"
)

// Prefix marks a sealed value.
const Prefix = "enc:v1:"

// Purposes: what a value is, bound into its ciphertext.
const (
	PurposeWebhookAuthHeader = "webhooks.auth_header_value"
	PurposeWebhookSecret     = "webhooks.secret"
	// PurposeGitHubHookSecret is a project's inbound GitHub webhook
	// secret (AMP #2479).
	PurposeGitHubHookSecret = "project_github_hooks.secret"
)

// ErrNoKey: there is no current key to seal with.
var ErrNoKey = errors.New("no encryption key is configured (" + EnvKey + ")")

// Keyring holds the current key and any retired ones. A nil or empty
// Keyring can't seal, and opens only plaintext.
type Keyring struct {
	current string // key id, "" if none
	keys    map[string]cipher.AEAD
}

// KeyID is the id of a key: the first 8 bytes of a hash of it, in hex.
// It names the key in sealed values and logs; it reveals nothing about it.
func KeyID(key []byte) string {
	h := sha256.Sum256(append([]byte("amp-secretbox-key-id\x00"), key...))
	return hex.EncodeToString(h[:8])
}

// New makes a keyring from a current key (nil for none) and retired keys.
// Each key must be 32 bytes.
func New(current []byte, retired ...[]byte) (*Keyring, error) {
	k := &Keyring{keys: map[string]cipher.AEAD{}}
	add := func(key []byte) (string, error) {
		if len(key) != 32 {
			return "", fmt.Errorf("an encryption key must be 32 bytes, got %d", len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return "", err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return "", err
		}
		id := KeyID(key)
		k.keys[id] = aead
		return id, nil
	}
	for _, r := range retired {
		if _, err := add(r); err != nil {
			return nil, fmt.Errorf("retired key: %w", err)
		}
	}
	if current != nil {
		id, err := add(current)
		if err != nil {
			return nil, err
		}
		k.current = id
	}
	return k, nil
}

// FromEnv reads the keys from EnvKey and EnvRetiredKeys. Both unset gives
// an empty keyring (nothing can be sealed); a malformed key is an error
// whose text never contains the key.
func FromEnv() (*Keyring, error) {
	decode := func(name, v string) ([]byte, error) {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("%s is not valid base64", name)
		}
		return b, nil
	}
	var current []byte
	if v := strings.TrimSpace(os.Getenv(EnvKey)); v != "" {
		b, err := decode(EnvKey, v)
		if err != nil {
			return nil, err
		}
		current = b
	}
	var retired [][]byte
	for _, v := range strings.FieldsFunc(os.Getenv(EnvRetiredKeys), func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		b, err := decode(EnvRetiredKeys, v)
		if err != nil {
			return nil, err
		}
		retired = append(retired, b)
	}
	k, err := New(current, retired...)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", EnvKey, EnvRetiredKeys, err)
	}
	return k, nil
}

// CanSeal reports whether there is a current key.
func (k *Keyring) CanSeal() bool { return k != nil && k.current != "" }

// CurrentID is the current key's id ("" if none).
func (k *Keyring) CurrentID() string {
	if k == nil {
		return ""
	}
	return k.current
}

// IsSealed reports whether v is a sealed value (not legacy plaintext).
func IsSealed(v string) bool { return strings.HasPrefix(v, Prefix) }

// SealedKeyID is the key id a sealed value was sealed under ("" if v isn't
// sealed).
func SealedKeyID(v string) string {
	if !IsSealed(v) {
		return ""
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(v, Prefix), ":")
	return id
}

// Seal encrypts plaintext for purpose under the current key. "" stays ""
// (nothing to hide, and "is it set" checks keep working).
func (k *Keyring) Seal(purpose, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if !k.CanSeal() {
		return "", ErrNoKey
	}
	aead := k.keys[k.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, []byte(plaintext), []byte(purpose))
	return Prefix + k.current + ":" + base64.RawURLEncoding.EncodeToString(ct), nil
}

// Open decrypts a value sealed for purpose. Legacy plaintext (no prefix)
// comes back unchanged. Errors never contain the value.
func (k *Keyring) Open(purpose, v string) (string, error) {
	if !IsSealed(v) {
		return v, nil
	}
	id, data, ok := strings.Cut(strings.TrimPrefix(v, Prefix), ":")
	if !ok {
		return "", errors.New("malformed sealed value")
	}
	var aead cipher.AEAD
	if k != nil {
		aead = k.keys[id]
	}
	if aead == nil {
		return "", fmt.Errorf("sealed under key %s, which isn't configured (%s, or %s for a retired key)", id, EnvKey, EnvRetiredKeys)
	}
	raw, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil || len(raw) < aead.NonceSize() {
		return "", errors.New("malformed sealed value")
	}
	pt, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(purpose))
	if err != nil {
		return "", fmt.Errorf("sealed value under key %s doesn't open (altered, or sealed for something else)", id)
	}
	return string(pt), nil
}

// NeedsReseal reports whether v should be sealed again: it is plaintext,
// or sealed under a key other than the current one. Always false without a
// current key.
func (k *Keyring) NeedsReseal(v string) bool {
	if v == "" || !k.CanSeal() {
		return false
	}
	return SealedKeyID(v) != k.current
}
