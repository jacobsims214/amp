package auditchain

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Checkpoint kinds.
const (
	// KindPeriodic is the hourly signature of a chain's head.
	KindPeriodic = "periodic"
	// KindAnchor is written just before retention deletes the rows up to
	// UptoSeq; verification of what's left starts from it.
	KindAnchor = "anchor"
)

// Checkpoint is one row of audit_checkpoints: "chain ChainKey's row UptoSeq
// has hash Hash", signed by the key KeyID (Signature nil when no key was
// configured).
type Checkpoint struct {
	ID        int64     `json:"id"`
	ChainKey  string    `json:"chain_key"`
	UptoSeq   int64     `json:"upto_seq"`
	Hash      []byte    `json:"-"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	KeyID     string    `json:"key_id,omitempty"`
	Signature []byte    `json:"-"`
	// RowAt is when the anchored row was written (anchors written by
	// audit_purge since migration 035 only): verify checks the row was past
	// retention when it was purged. Not part of the signed message;
	// audit_purge takes it from the row itself.
	RowAt *time.Time `json:"row_at,omitempty"`
}

// Message is the exact byte string a checkpoint's signature covers.
func (c Checkpoint) Message() []byte {
	return []byte("amp-audit-checkpoint/v1\n" + c.ChainKey + "\n" + strconv.FormatInt(c.UptoSeq, 10) + "\n" +
		hex.EncodeToString(c.Hash) + "\n" + c.At.UTC().Format("2006-01-02T15:04:05.000000Z") + "\n" + c.Kind)
}

// Environment variables the keyring is read from.
const (
	// EnvSigningKey is the Ed25519 private key, base64 (a 32-byte seed or a
	// 64-byte private key). Helm fills it from a Secret.
	EnvSigningKey = "AUDIT_SIGNING_KEY"
	// EnvSigningKeyFile is a file holding the same, e.g. a mounted Secret.
	EnvSigningKeyFile = "AUDIT_SIGNING_KEY_FILE"
	// EnvPublicKeys lists more public keys (base64, comma-separated) that
	// verification accepts: retired signing keys after a rotation.
	EnvPublicKeys = "AUDIT_SIGNING_PUBLIC_KEYS"
	// EnvSigningSince is when signing started (RFC 3339, or a date): once a
	// key is configured, every checkpoint must be signed, except ones
	// written before this time (history from before there was a key). Unset:
	// no unsigned checkpoint is accepted when a key is configured.
	EnvSigningSince = "AUDIT_SIGNING_SINCE"
)

// Keyring signs checkpoints with the configured key, and checks them
// against it and any retired public keys.
type Keyring struct {
	signer   ed25519.PrivateKey
	signerID string
	public   map[string]ed25519.PublicKey
	// since: unsigned checkpoints written before it are pre-key history
	// (EnvSigningSince); zero means none are.
	since time.Time
}

// KeyID names a public key: the first 8 bytes of its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// NewKeyring builds a keyring that signs with priv (nil: unsigned) and also
// accepts the public keys in extra.
func NewKeyring(priv ed25519.PrivateKey, extra ...ed25519.PublicKey) *Keyring {
	k := &Keyring{public: map[string]ed25519.PublicKey{}}
	if priv != nil {
		pub := priv.Public().(ed25519.PublicKey)
		k.signer, k.signerID = priv, KeyID(pub)
		k.public[k.signerID] = pub
	}
	for _, p := range extra {
		k.public[KeyID(p)] = p
	}
	return k
}

// LoadKeyring reads the keyring from the environment. It never fails: with
// no key, or a key it can't read, checkpoints are written unsigned (and the
// returned error, which never contains key material, says why the key was
// not used). getenv is os.Getenv outside tests.
func LoadKeyring(getenv func(string) string) (*Keyring, error) {
	var errs []error
	var priv ed25519.PrivateKey
	keyText := strings.TrimSpace(getenv(EnvSigningKey))
	if keyText == "" {
		if path := getenv(EnvSigningKeyFile); path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: can't read the key file: %w", EnvSigningKeyFile, err))
			}
			keyText = strings.TrimSpace(string(b))
		}
	}
	if keyText != "" {
		p, err := ParsePrivateKey(keyText)
		if err != nil {
			errs = append(errs, fmt.Errorf("signing key: %w", err))
		} else {
			priv = p
		}
	}
	var extra []ed25519.PublicKey
	for _, s := range strings.Split(getenv(EnvPublicKeys), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := ParsePublicKey(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvPublicKeys, err))
			continue
		}
		extra = append(extra, p)
	}
	k := NewKeyring(priv, extra...)
	if v := strings.TrimSpace(getenv(EnvSigningSince)); v != "" {
		t, err := ParseSince(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvSigningSince, err))
		} else {
			k.since = t
		}
	}
	return k, errors.Join(errs...)
}

// ParseSince reads an RFC 3339 time or a YYYY-MM-DD date (midnight UTC).
func ParseSince(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, errors.New("want an RFC 3339 time or a YYYY-MM-DD date")
	}
	return t, nil
}

// WithSince returns a copy of k that accepts unsigned checkpoints written
// before t as pre-key history (EnvSigningSince).
func (k *Keyring) WithSince(t time.Time) *Keyring {
	c := NewKeyring(nil)
	if k != nil {
		*c = *k
	}
	c.since = t
	return c
}

// Since is EnvSigningSince (zero when unset).
func (k *Keyring) Since() time.Time {
	if k == nil {
		return time.Time{}
	}
	return k.since
}

// HasKeys reports whether the keyring can check signatures at all: a
// signing key or a public key is configured. Without one, every chain is
// unproven: anyone with database access could have rewritten it.
func (k *Keyring) HasKeys() bool { return k != nil && len(k.public) > 0 }

func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not base64")
}

// ParsePrivateKey reads a base64 Ed25519 seed (32 bytes) or private key (64
// bytes). Errors never include the input.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := decodeB64(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		priv := ed25519.PrivateKey(b)
		if !ed25519.NewKeyFromSeed(b[:ed25519.SeedSize]).Equal(priv) {
			return nil, errors.New("the 64-byte key's public half doesn't match its seed")
		}
		return priv, nil
	}
	return nil, fmt.Errorf("want a 32-byte seed or a 64-byte Ed25519 private key, got %d bytes", len(b))
}

// ParsePublicKey reads a base64 32-byte Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeB64(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("want a 32-byte Ed25519 public key, got %d bytes", len(b))
	}
	return ed25519.PublicKey(b), nil
}

// GenerateKey makes a new signing key: the base64 seed to store in the
// Secret, and the base64 public key and its id to publish.
func GenerateKey() (seed, public, keyID string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), base64.StdEncoding.EncodeToString(pub), KeyID(pub), nil
}

// CanSign reports whether a signing key is configured.
func (k *Keyring) CanSign() bool { return k != nil && k.signer != nil }

// SignerID is the configured key's id ("" without one).
func (k *Keyring) SignerID() string {
	if k == nil {
		return ""
	}
	return k.signerID
}

// PublicKey is the configured key's public half, base64 ("" without one).
func (k *Keyring) PublicKey() string {
	if !k.CanSign() {
		return ""
	}
	return base64.StdEncoding.EncodeToString(k.signer.Public().(ed25519.PublicKey))
}

// Sign fills c's KeyID and Signature, or leaves them empty without a key.
// c.At is truncated to the microseconds Postgres keeps.
func (k *Keyring) Sign(c *Checkpoint) {
	c.At = c.At.UTC().Truncate(time.Microsecond)
	c.KeyID, c.Signature = "", nil
	if !k.CanSign() {
		return
	}
	c.KeyID = k.signerID
	c.Signature = ed25519.Sign(k.signer, c.Message())
}

// Signature check results.
const (
	SigOK         = "ok"
	SigUnsigned   = "unsigned"
	SigBad        = "bad_signature"
	SigUnknownKey = "unknown_key"
)

// Check says whether c's signature is valid.
func (k *Keyring) Check(c Checkpoint) string {
	if len(c.Signature) == 0 {
		return SigUnsigned
	}
	if k == nil {
		return SigUnknownKey
	}
	pub, ok := k.public[c.KeyID]
	if !ok {
		return SigUnknownKey
	}
	if !ed25519.Verify(pub, c.Message(), c.Signature) {
		return SigBad
	}
	return SigOK
}
