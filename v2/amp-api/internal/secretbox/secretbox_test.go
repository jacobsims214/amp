package secretbox

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestSealOpen(t *testing.T) {
	k, err := New(key(1))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "Splunk 0f8b-hec-token"
	s, err := k.Seal(PurposeWebhookAuthHeader, secret)
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(s) || strings.Contains(s, secret) || SealedKeyID(s) != KeyID(key(1)) {
		t.Fatalf("sealed = %q", s)
	}
	// A fresh nonce each time.
	if s2, _ := k.Seal(PurposeWebhookAuthHeader, secret); s2 == s {
		t.Error("two seals of one value are identical")
	}
	if got, err := k.Open(PurposeWebhookAuthHeader, s); err != nil || got != secret {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Bound to its purpose: the same ciphertext in another column won't open.
	if _, err := k.Open(PurposeWebhookSecret, s); err == nil {
		t.Error("opened under another purpose")
	}
	// Tampering is detected, and errors never carry the value.
	raw := []byte(s)
	raw[len(raw)-2] ^= 1
	if _, err := k.Open(PurposeWebhookAuthHeader, string(raw)); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("tampered open err = %v", err)
	}
	// "" stays "", plaintext passes through (legacy rows).
	if s, _ := k.Seal(PurposeWebhookSecret, ""); s != "" {
		t.Errorf("seal(\"\") = %q", s)
	}
	if got, _ := k.Open(PurposeWebhookSecret, "legacy-plain"); got != "legacy-plain" {
		t.Errorf("legacy open = %q", got)
	}
}

func TestNoKey(t *testing.T) {
	var nilRing *Keyring
	empty, _ := New(nil)
	for _, k := range []*Keyring{nilRing, empty} {
		if k.CanSeal() {
			t.Error("can seal without a key")
		}
		if _, err := k.Seal(PurposeWebhookAuthHeader, "x"); err != ErrNoKey {
			t.Errorf("seal err = %v", err)
		}
		if k.NeedsReseal("plain") {
			t.Error("NeedsReseal without a key")
		}
	}
	sealed, _ := mustRing(t, key(1)).Seal(PurposeWebhookSecret, "s")
	if _, err := empty.Open(PurposeWebhookSecret, sealed); err == nil || !strings.Contains(err.Error(), EnvKey) {
		t.Errorf("open without its key err = %v", err)
	}
}

func mustRing(t *testing.T, current []byte, retired ...[]byte) *Keyring {
	t.Helper()
	k, err := New(current, retired...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Rotation: the new key opens nothing old by itself; with the old key
// retired, old values open and are due for re-sealing.
func TestRotation(t *testing.T) {
	old := mustRing(t, key(1))
	s, _ := old.Seal(PurposeWebhookSecret, "whsec")
	rotated := mustRing(t, key(2), key(1))
	if got, err := rotated.Open(PurposeWebhookSecret, s); err != nil || got != "whsec" {
		t.Fatalf("open after rotation = %q, %v", got, err)
	}
	if !rotated.NeedsReseal(s) || !rotated.NeedsReseal("plain") || old.NeedsReseal(s) {
		t.Error("NeedsReseal wrong")
	}
	s2, _ := rotated.Seal(PurposeWebhookSecret, "whsec")
	if SealedKeyID(s2) != KeyID(key(2)) || rotated.NeedsReseal(s2) {
		t.Errorf("re-sealed under %s", SealedKeyID(s2))
	}
	if _, err := mustRing(t, key(2)).Open(PurposeWebhookSecret, s); err == nil {
		t.Error("opened without the retired key")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvKey, base64.StdEncoding.EncodeToString(key(3)))
	t.Setenv(EnvRetiredKeys, base64.StdEncoding.EncodeToString(key(1))+", "+base64.StdEncoding.EncodeToString(key(2)))
	k, err := FromEnv()
	if err != nil || k.CurrentID() != KeyID(key(3)) || len(k.keys) != 3 {
		t.Fatalf("FromEnv = %+v, %v", k, err)
	}
	t.Setenv(EnvKey, base64.StdEncoding.EncodeToString([]byte("short")))
	if _, err := FromEnv(); err == nil {
		t.Error("short key accepted")
	}
	t.Setenv(EnvKey, "not base64 !!")
	if _, err := FromEnv(); err == nil || strings.Contains(err.Error(), "not base64 !!") {
		t.Errorf("bad key err = %v", err)
	}
	t.Setenv(EnvKey, "")
	t.Setenv(EnvRetiredKeys, "")
	if k, err := FromEnv(); err != nil || k.CanSeal() {
		t.Errorf("empty env = %v, %v", k, err)
	}
}
