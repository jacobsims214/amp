package main

// Webhook secrets at rest (#2441, review #2432 M3 and L4): auth headers and
// signing secrets are stored encrypted under a key from a Secret, legacy
// plaintext is sealed at startup, the key can rotate, there are no
// plaintext auth headers without a key, and the audit row of a header
// change says the value changed.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/secretbox"
	"github.com/simstech/amp-api/internal/webhook"
)

func testSecretsKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// defaultTestSecretsKey is the key every test server starts with
// (setupMCP).
var defaultTestSecretsKey = testSecretsKey(0x42)

func defaultTestSecretsKeys(t *testing.T) *secretbox.Keyring {
	t.Helper()
	k, err := secretbox.New(defaultTestSecretsKey)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// withSecretsKey gives repo other encryption keys for the test (the
// default key comes back after it).
func withSecretsKey(t *testing.T, repo *repository.Repo, current []byte, retired ...[]byte) *secretbox.Keyring {
	t.Helper()
	k, err := secretbox.New(current, retired...)
	if err != nil {
		t.Fatal(err)
	}
	repo.SetSecretsKeys(k)
	t.Cleanup(func() { repo.SetSecretsKeys(defaultTestSecretsKeys(t)) })
	return k
}

// storedWebhookSecrets reads a webhook's secret columns as stored.
func storedWebhookSecrets(t *testing.T, id int) (secret, header string) {
	t.Helper()
	db, err := pgx.Connect(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	if err := db.QueryRow(context.Background(), `SELECT secret, auth_header_value FROM webhooks WHERE id = $1`, id).Scan(&secret, &header); err != nil {
		t.Fatal(err)
	}
	return secret, header
}

func execSQL(t *testing.T, sql string, args ...any) {
	t.Helper()
	db, err := pgx.Connect(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookSecrets_EncryptedAtRest(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	allowPrivateWebhooks(t)
	keys := withSecretsKey(t, w.s.repo, testSecretsKey(0x42))
	siem := newHeaderReceiver(t, "Authorization")
	const token = "Splunk 7c1e-at-rest-hec-token"

	hook := ra.want(t, 200, "root", "POST", "/api/webhooks", map[string]any{"url": siem.srv.URL, "auth_header": "Authorization: " + token})["webhook"].(map[string]any)
	id := int(hook["id"].(float64))
	t.Cleanup(func() { _ = w.s.repo.DeleteWebhook(context.Background(), id) })
	whsec := hook["secret"].(string)

	// What's stored is ciphertext under the current key: neither the
	// header nor the signing secret is readable from the database.
	secret, header := storedWebhookSecrets(t, id)
	for what, v := range map[string]string{"secret": secret, "auth_header_value": header} {
		if !secretbox.IsSealed(v) || secretbox.SealedKeyID(v) != keys.CurrentID() || strings.Contains(v, token) || strings.Contains(v, whsec) {
			t.Errorf("stored %s = %q, want sealed under %s", what, v, keys.CurrentID())
		}
	}

	// Deliveries still carry the real header and a signature made with the
	// real secret.
	startWebhooks(t, w, webhook.Options{AllowPrivateNetworks: true})
	tokenName := fmt.Sprintf("at-rest-%d", time.Now().UnixNano())
	ra.want(t, 200, "alice", "POST", "/api/users/me/tokens", map[string]any{"name": tokenName})
	d := siem.find(t, "token.create", func(d headerDelivery) bool { return strings.Contains(d.body, tokenName) })
	if d.auth != token {
		t.Errorf("auth header on delivery = %q", d.auth)
	}
	sig := strings.SplitN(d.signature, ",", 2)
	if len(sig) != 2 || sig[1] != "v1="+webhook.Sign(whsec, strings.TrimPrefix(sig[0], "t="), d.body) {
		t.Errorf("signature %q does not verify with the issued secret", d.signature)
	}

	// A rotated secret and a replaced header are sealed too.
	rot := ra.want(t, 200, "root", "POST", fmt.Sprintf("/api/webhooks/%d/rotate-secret", id), nil)
	newSecret := ""
	if wh, ok := rot["webhook"].(map[string]any); ok {
		newSecret, _ = wh["secret"].(string)
	}
	ra.want(t, 200, "root", "PATCH", fmt.Sprintf("/api/webhooks/%d", id), map[string]any{"auth_header": "Authorization: Splunk second"})
	secret, header = storedWebhookSecrets(t, id)
	if !secretbox.IsSealed(secret) || (newSecret != "" && strings.Contains(secret, newSecret)) || !secretbox.IsSealed(header) || strings.Contains(header, "second") {
		t.Errorf("after rotate/replace: secret %q, header %q", secret, header)
	}
}

// Without a key no auth header is stored (in plaintext or at all); the
// rest of webhooks works as before.
func TestWebhookSecrets_NoKeyRefusesAuthHeader(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	w.s.repo.SetSecretsKeys(nil)
	t.Cleanup(func() { w.s.repo.SetSecretsKeys(defaultTestSecretsKeys(t)) })

	out := ra.want(t, 400, "root", "POST", "/api/webhooks", map[string]any{"url": "https://siem.example.com/in", "auth_header": "DD-API-KEY: k"})
	if !strings.Contains(mustJSON(out), secretbox.EnvKey) {
		t.Errorf("refusal doesn't say what to set: %v", out)
	}
	hook := ra.want(t, 200, "root", "POST", "/api/webhooks", map[string]any{"url": "https://siem.example.com/in"})["webhook"].(map[string]any)
	id := int(hook["id"].(float64))
	t.Cleanup(func() { _ = w.s.repo.DeleteWebhook(context.Background(), id) })
	ra.want(t, 400, "root", "PATCH", fmt.Sprintf("/api/webhooks/%d", id), map[string]any{"auth_header": "DD-API-KEY: k"})
	// Removing one needs no key.
	ra.want(t, 200, "root", "PATCH", fmt.Sprintf("/api/webhooks/%d", id), map[string]any{"auth_header": ""})
	// Signing secrets stay plaintext without a key (unchanged behaviour).
	if secret, header := storedWebhookSecrets(t, id); secret != hook["secret"] || header != "" {
		t.Errorf("stored without a key: secret sealed=%v, header %q", secretbox.IsSealed(secret), header)
	}
	// The repository refuses too, whoever calls it.
	if _, err := w.s.repo.CreateWebhook(context.Background(), domain.Webhook{URL: "https://x.example.com", Format: domain.WebhookFormatJSON,
		Events: []string{"audit.*"}, AuthHeaderName: "X-Key", AuthHeaderValue: "v"}); !errors.Is(err, repository.ErrSecretsKeyMissing) {
		t.Errorf("repo create with header and no key: %v", err)
	}
}

// Rows written before encryption are sealed at startup; after a rotation
// they are re-sealed under the new key; a value whose key is gone isn't
// sent at all.
func TestWebhookSecrets_ResealAndRotate(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	allowPrivateWebhooks(t)
	siem := newHeaderReceiver(t, "Dd-Api-Key")
	// An org webhook on an event nothing here fires: it receives only the
	// pings this test sends.
	w.s.repo.SetSecretsKeys(nil)
	t.Cleanup(func() { w.s.repo.SetSecretsKeys(defaultTestSecretsKeys(t)) })
	hook := ra.want(t, 200, "orgadmin", "POST", fmt.Sprintf("/api/orgs/%d/webhooks", w.org.ID),
		map[string]any{"url": siem.srv.URL, "events": []string{"task.created"}})["webhook"].(map[string]any)
	id := int(hook["id"].(float64))
	t.Cleanup(func() { _ = w.s.repo.DeleteWebhook(context.Background(), id) })
	whsec := hook["secret"].(string)
	// A legacy (pre-#2441) plaintext header, as #85 stored them.
	execSQL(t, `UPDATE webhooks SET auth_header_name = 'Dd-Api-Key', auth_header_value = 'legacy-dd-key' WHERE id = $1`, id)
	if secret, _ := storedWebhookSecrets(t, id); secret != whsec {
		t.Fatalf("without a key the secret is stored as is; sealed=%v", secretbox.IsSealed(secret))
	}

	// No key: nothing changes, and the plaintext header is counted.
	rep, err := w.s.repo.ResealWebhookSecrets(context.Background(), id)
	if err != nil || rep.Resealed != 0 || rep.PlaintextHeaders != 1 {
		t.Fatalf("reseal without key = %+v, %v", rep, err)
	}

	// The key arrives (others' webhooks keep the default key, retired here).
	k1 := withSecretsKey(t, w.s.repo, testSecretsKey(1), defaultTestSecretsKey)
	if rep, err := w.s.repo.ResealWebhookSecrets(context.Background(), id); err != nil || rep.Resealed != 2 {
		t.Fatalf("reseal = %+v, %v", rep, err)
	}
	secret, header := storedWebhookSecrets(t, id)
	if secretbox.SealedKeyID(secret) != k1.CurrentID() || secretbox.SealedKeyID(header) != k1.CurrentID() {
		t.Fatalf("after reseal: secret %q, header %q", secret, header)
	}
	// Idempotent.
	if rep, _ := w.s.repo.ResealWebhookSecrets(context.Background(), id); rep.Resealed != 0 {
		t.Errorf("second reseal = %+v", rep)
	}

	// Rotation: the new key current, the old one retired.
	k2 := withSecretsKey(t, w.s.repo, testSecretsKey(2), testSecretsKey(1), defaultTestSecretsKey)
	if rep, err := w.s.repo.ResealWebhookSecrets(context.Background(), id); err != nil || rep.Resealed != 2 {
		t.Fatalf("reseal after rotation = %+v, %v", rep, err)
	}
	secret, header = storedWebhookSecrets(t, id)
	if secretbox.SealedKeyID(secret) != k2.CurrentID() || secretbox.SealedKeyID(header) != k2.CurrentID() {
		t.Fatalf("after rotation: secret %q, header %q", secret, header)
	}

	// The retired key can go now; deliveries still work.
	withSecretsKey(t, w.s.repo, testSecretsKey(2), defaultTestSecretsKey)
	startWebhooks(t, w, webhook.Options{AllowPrivateNetworks: true})
	ra.want(t, 200, "orgadmin", "POST", fmt.Sprintf("/api/webhooks/%d/test", id), nil)
	d := siem.find(t, "ping", func(d headerDelivery) bool { return d.event == "ping" })
	sig := strings.SplitN(d.signature, ",", 2)
	if d.auth != "legacy-dd-key" || len(sig) != 2 || sig[1] != "v1="+webhook.Sign(whsec, strings.TrimPrefix(sig[0], "t="), d.body) {
		t.Errorf("delivery after rotation: auth %q, signature %q", d.auth, d.signature)
	}

	// Without the key the values were sealed under, nothing is sent: no
	// unsigned delivery, no missing header.
	withSecretsKey(t, w.s.repo, testSecretsKey(3), defaultTestSecretsKey)
	ra.want(t, 200, "orgadmin", "POST", fmt.Sprintf("/api/webhooks/%d/test", id), nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		ds, _ := w.s.repo.ListDeliveries(context.Background(), id, 1)
		if len(ds) == 1 && ds[0].Attempts > 0 {
			if !strings.Contains(ds[0].LastError, "decrypt") || strings.Contains(ds[0].LastError, "legacy-dd-key") {
				t.Errorf("last error = %q", ds[0].LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the delivery was never attempted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	siem.mu.Lock()
	defer siem.mu.Unlock()
	if len(siem.got) != 1 {
		t.Errorf("a delivery was sent without its secrets: %d received", len(siem.got))
	}
}

// L4: replacing the header's value under the same name shows in the
// audit row, though the value itself never does.
func TestWebhookSecrets_HeaderChangeIsAudited(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	withSecretsKey(t, w.s.repo, testSecretsKey(0x42))
	hook := ra.want(t, 200, "root", "POST", "/api/webhooks", map[string]any{"url": "https://siem.example.com/in", "auth_header": "Authorization: first-value"})["webhook"].(map[string]any)
	id := int(hook["id"].(float64))
	t.Cleanup(func() { _ = w.s.repo.DeleteWebhook(context.Background(), id) })

	updates := func() []domain.AuditEntry {
		return auditRows(t, w.s.repo, domain.AuditFilter{Action: "webhook.update", TargetType: "webhook", TargetID: fmt.Sprint(id)})
	}
	ra.want(t, 200, "root", "PATCH", fmt.Sprintf("/api/webhooks/%d", id), map[string]any{"auth_header": "Authorization: second-value"})
	rows := updates()
	if len(rows) != 1 || rows[0].Details["auth_header_changed"] != true || rows[0].Details["auth_header_removed"] != nil {
		t.Fatalf("value swap row = %+v", rows)
	}
	if j := mustJSON(rows[0]); strings.Contains(j, "first-value") || strings.Contains(j, "second-value") {
		t.Errorf("the audit row carries the value: %s", j)
	}
	// An edit that leaves the header alone doesn't say it changed.
	ra.want(t, 200, "root", "PATCH", fmt.Sprintf("/api/webhooks/%d", id), map[string]any{"name": "renamed"})
	if rows = updates(); len(rows) != 2 || rows[0].Details["auth_header_changed"] != nil {
		t.Fatalf("rename row = %+v", rows[0])
	}
	// Removal says so.
	ra.want(t, 200, "root", "PATCH", fmt.Sprintf("/api/webhooks/%d", id), map[string]any{"auth_header": ""})
	if rows = updates(); len(rows) != 3 || rows[0].Details["auth_header_changed"] != true || rows[0].Details["auth_header_removed"] != true {
		t.Fatalf("removal row = %+v", rows[0])
	}
}
