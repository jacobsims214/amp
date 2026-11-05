package main

// Sealed project GitHub credentials (AMP #3154, epic 183 story 453).
//
// Three things the ticket asks for, each proven against a real database and
// the real service:
//
//   - only a project owner can attach, rotate or delete a credential;
//   - the token is stored encrypted and never read back — not by the API, not
//     by the audit log, not by an error message;
//   - every attach, rotation and deletion is audited without the token.
//
// No test here talks to GitHub: the token is an isolated fixture string, and
// nothing in this path makes a network call.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/secretbox"
	"github.com/simstech/amp-api/internal/service"
)

// ghToken is a fixture token shaped like a real one. It is never sent
// anywhere; the assertions are that it can't be read back.
func ghToken(seed string) string {
	s := seed + strings.Repeat("x", 30)
	return "ghp_" + s[:30]
}

// credSvc is the service over the world's repository (which has the test
// encryption key from setupMCP).
func credSvc(w *rbacWorld) *service.Service { return service.New(w.s.repo, w.s.reg, w.s.kb) }

// credAs is a web-UI principal for one of the world's users.
func credAs(w *rbacWorld, who string) authz.Principal {
	ids := map[string]*auth.Identity{
		"owner": w.owner, "alice": w.alice, "bob": w.bob, "lead": w.lead, "carol": w.carol,
		"dave": w.dave, "orgadmin": w.orgadmin, "outsider": w.outsider, "root": w.root,
	}
	return uiPrincipal(ids[who])
}

// storedCredentialSecret reads a project's credential column exactly as the
// database holds it.
func storedCredentialSecret(t *testing.T, dsn string, projectID int) string {
	t.Helper()
	db, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	var stored string
	if err := db.QueryRow(context.Background(), `SELECT sealed_secret FROM project_github_credentials
		WHERE project_id = $1 AND host = 'github.com'`, projectID).Scan(&stored); err != nil {
		t.Fatalf("read sealed_secret: %v", err)
	}
	return stored
}

// Only a project owner attaches, rotates or deletes: a member and a lead are
// refused with 403, someone outside the project gets 404, and an org admin
// (owner level or above) may. A refused attach writes nothing.
func TestGitHubCredential_OwnerOnly(t *testing.T) {
	w := newRBACWorld(t)
	svc, ctx := credSvc(w), context.Background()
	pid, token := w.s.projectID, ghToken("a")

	for _, who := range []string{"alice", "bob", "carol", "lead"} {
		if _, err := svc.AttachGitHubCredential(ctx, credAs(w, who), pid, "", "ci", token); err == nil {
			t.Fatalf("%s attached a credential", who)
		} else if s := authz.StatusOf(err, 500); s != 403 {
			t.Fatalf("%s attaching: status %d (%v), want 403", who, s, err)
		}
		if _, err := svc.GitHubCredential(ctx, credAs(w, who), pid, ""); err == nil || authz.StatusOf(err, 500) != 403 {
			t.Fatalf("%s read the credential: %v", who, err)
		}
		if _, err := svc.DeleteGitHubCredential(ctx, credAs(w, who), pid, ""); err == nil || authz.StatusOf(err, 500) != 403 {
			t.Fatalf("%s deleted the credential: %v", who, err)
		}
	}
	// Not on the project at all: not found, so its existence isn't leaked.
	for _, who := range []string{"dave", "outsider"} {
		if _, err := svc.AttachGitHubCredential(ctx, credAs(w, who), pid, "", "ci", token); authz.StatusOf(err, 500) != 404 {
			t.Fatalf("%s attaching: %v, want 404", who, err)
		}
	}
	// Nothing was written by any of those attempts.
	if _, err := svc.Repo.GetGitHubCredential(ctx, pid, ""); !errors.Is(err, repository.ErrNoGitHubCredential) {
		t.Fatalf("a refused attach left a credential behind: %v", err)
	}

	// The owner team's owner, and an org admin (owner level or above), may.
	info, err := svc.AttachGitHubCredential(ctx, credAs(w, "owner"), pid, "", "ci", token)
	if err != nil {
		t.Fatalf("owner attaching: %v", err)
	}
	if !info.Configured || info.Credential == nil || info.Credential.Label != "ci" || info.Host != "github.com" {
		t.Fatalf("after attach = %+v", info)
	}
	if _, err := svc.RotateGitHubCredential(ctx, credAs(w, "orgadmin"), pid, "", "ci", ghToken("b")); err != nil {
		t.Fatalf("org admin rotating: %v", err)
	}
	if had, err := svc.DeleteGitHubCredential(ctx, credAs(w, "owner"), pid, ""); err != nil || !had {
		t.Fatalf("owner deleting: %v %v", had, err)
	}
	if _, err := svc.GitHubCredential(ctx, credAs(w, "owner"), pid, ""); err != nil {
		t.Fatalf("reading after delete: %v", err)
	}
}

// The token is stored sealed, is never returned by any read, and a rotation
// replaces it in place. The only way back to the plaintext is the storage
// call the facts reader uses.
func TestGitHubCredential_EncryptedAndNoReadback(t *testing.T) {
	w := newRBACWorld(t)
	svc, ctx := credSvc(w), context.Background()
	pid, token := w.s.projectID, ghToken("c")
	owner := credAs(w, "owner")

	info, err := svc.AttachGitHubCredential(ctx, owner, pid, "", "ci token", token)
	if err != nil {
		t.Fatal(err)
	}
	// At rest: sealed, and the token is nowhere in the stored text.
	stored := storedCredentialSecret(t, testDSN(t), pid)
	if !secretbox.IsSealed(stored) {
		t.Fatalf("stored value is not sealed: %q", stored)
	}
	if strings.Contains(stored, token) {
		t.Fatalf("the stored value contains the token: %q", stored)
	}
	if secretbox.SealedKeyID(stored) != defaultTestSecretsKeys(t).CurrentID() {
		t.Fatalf("stored value sealed under %q, want the configured key", secretbox.SealedKeyID(stored))
	}

	// No readback: the answer carries metadata only, and the credential type
	// has no field a token could hide in.
	if b := mustJSON(info); strings.Contains(b, token) {
		t.Fatalf("attach returned the token: %s", b)
	}
	read, err := svc.GitHubCredential(ctx, owner, pid, "")
	if err != nil {
		t.Fatal(err)
	}
	if b := mustJSON(read); strings.Contains(b, token) || strings.Contains(b, "ghp_") {
		t.Fatalf("a read returned secret material: %s", b)
	}
	if read.Credential.CreatedAt.IsZero() || read.Credential.RotatedAt != nil {
		t.Fatalf("first attach = %+v, want a created_at and no rotated_at", read.Credential)
	}

	// The storage layer is the one way to the plaintext, and it opens only
	// for the purpose it was sealed with.
	got, err := svc.Repo.GitHubCredentialSecret(ctx, pid, "")
	if err != nil || got != token {
		t.Fatalf("opening the stored token = %q %v", got, err)
	}
	ring := defaultTestSecretsKeys(t)
	if _, err := ring.Open(secretbox.PurposeGitHubHookSecret, stored); err == nil {
		t.Fatal("a credential opened as a webhook secret: the purpose binding is gone")
	}

	// Rotation replaces the value in place, on the same row, and dates it.
	rotated := ghToken("d")
	after, err := svc.RotateGitHubCredential(ctx, owner, pid, "", "ci token v2", rotated)
	if err != nil {
		t.Fatal(err)
	}
	if after.Credential.ID != info.Credential.ID || after.Credential.RotatedAt == nil {
		t.Fatalf("rotation = %+v, want the same row with rotated_at set", after.Credential)
	}
	if stored2 := storedCredentialSecret(t, testDSN(t), pid); stored2 == stored || strings.Contains(stored2, rotated) {
		t.Fatalf("after rotation the stored value = %q", stored2)
	}
	if got, err := svc.Repo.GitHubCredentialSecret(ctx, pid, ""); err != nil || got != rotated {
		t.Fatalf("after rotation the token = %q %v", got, err)
	}
	if b := mustJSON(after); strings.Contains(b, rotated) {
		t.Fatalf("rotate returned the token: %s", b)
	}

	// A rotate of nothing is a mistake, reported as one rather than becoming
	// a quiet attach.
	if _, err := svc.RotateGitHubCredential(ctx, owner, w.s.projectID+1_000_000, "", "x", ghToken("e")); err == nil {
		t.Fatal("rotating a project with no credential succeeded")
	}

	// Deleting removes the row (and with it, in the schema, the facts it
	// produced) and reports that there was one.
	if had, err := svc.DeleteGitHubCredential(ctx, owner, pid, ""); err != nil || !had {
		t.Fatalf("delete = %v %v", had, err)
	}
	if had, err := svc.DeleteGitHubCredential(ctx, owner, pid, ""); err != nil || had {
		t.Fatalf("second delete = %v %v, want false", had, err)
	}
	if _, err := svc.Repo.GitHubCredentialSecret(ctx, pid, ""); !errors.Is(err, repository.ErrNoGitHubCredential) {
		t.Fatalf("the token survived deletion: %v", err)
	}
}

// Attaching, rotating and deleting are audited by who did it and for which
// host, and no audit row — nor an error message — ever holds the token.
func TestGitHubCredential_AuditNeverHoldsTheToken(t *testing.T) {
	w := newRBACWorld(t)
	svc, ctx := credSvc(w), context.Background()
	pid, first, second := w.s.projectID, ghToken("f"), ghToken("9")
	owner := credAs(w, "owner")

	if _, err := svc.AttachGitHubCredential(ctx, owner, pid, "", "ci", first); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RotateGitHubCredential(ctx, owner, pid, "", "ci v2", second); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteGitHubCredential(ctx, owner, pid, ""); err != nil {
		t.Fatal(err)
	}

	rows := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.owner.User.ID, Action: "project.github_credential."})
	got := map[string]domain.AuditEntry{}
	for _, r := range rows {
		got[r.Action] = r
	}
	for _, action := range []string{
		"project.github_credential.attach", "project.github_credential.rotate", "project.github_credential.delete",
	} {
		e, ok := got[action]
		if !ok {
			t.Fatalf("no %s audit row: %v", action, rows)
		}
		if e.ProjectID == nil || *e.ProjectID != pid || e.TargetType != "project" {
			t.Fatalf("%s = %+v", action, e)
		}
		if b := mustJSON(e); strings.Contains(b, first) || strings.Contains(b, second) || strings.Contains(b, "ghp_") {
			t.Fatalf("%s audit row holds secret material: %s", action, b)
		}
	}
	if e := got["project.github_credential.rotate"]; e.Before["label"] != "ci" || e.After["label"] != "ci v2" ||
		e.Before["host"] != "github.com" || e.Before["credential_id"] == nil {
		t.Fatalf("rotate before/after = %v / %v", e.Before, e.After)
	}
	if e := got["project.github_credential.attach"]; e.After["label"] != "ci" || e.Details["host"] != "github.com" {
		t.Fatalf("attach = %+v", e)
	}
	if e := got["project.github_credential.delete"]; e.Details["host"] != "github.com" {
		t.Fatalf("delete = %+v", e)
	}

	// A refused attach is refused, and says why without repeating what was
	// pasted: a value with whitespace can't be a token.
	err := func() error {
		_, err := svc.AttachGitHubCredential(ctx, owner, pid, "", "ci", "Authorization: Bearer "+ghToken("h"))
		return err
	}()
	if err == nil || authz.StatusOf(err, 500) != 400 {
		t.Fatalf("a header line as a token: %v", err)
	}
	if strings.Contains(err.Error(), ghToken("h")) {
		t.Fatalf("the error echoes the token: %v", err)
	}
}

// With no encryption key a token is refused rather than stored in the clear,
// and the refusal names the setting to fix — never the token.
func TestGitHubCredential_NoKeyRefusesRatherThanStoringPlaintext(t *testing.T) {
	w := newRBACWorld(t)
	svc, ctx := credSvc(w), context.Background()
	pid, token := w.s.projectID, ghToken("j")
	w.s.repo.SetSecretsKeys(nil)
	t.Cleanup(func() { w.s.repo.SetSecretsKeys(defaultTestSecretsKeys(t)) })

	_, err := svc.AttachGitHubCredential(ctx, credAs(w, "owner"), pid, "", "ci", token)
	if err == nil {
		t.Fatal("attached a credential with no encryption key")
	}
	if authz.StatusOf(err, 500) != 400 || !strings.Contains(err.Error(), secretbox.EnvKey) {
		t.Fatalf("no-key refusal = %v, want a 400 naming %s", err, secretbox.EnvKey)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the refusal echoes the token: %v", err)
	}
	// Nothing was written: not plaintext, not sealed.
	if _, err := svc.Repo.GetGitHubCredential(ctx, pid, ""); !errors.Is(err, repository.ErrNoGitHubCredential) {
		t.Fatalf("a refused attach left a row: %v", err)
	}
	// A read still works and reports that this server can't hold one.
	info, err := svc.GitHubCredential(ctx, credAs(w, "owner"), pid, "")
	if err != nil || info.Configured || info.Encrypted {
		t.Fatalf("read with no key = %+v %v", info, err)
	}
}

// A credential whose value can't be opened is reported as such, never handed
// out as if it were a token: the facts reader must fail rather than
// authenticate with garbage.
func TestGitHubCredential_UnopenableValueIsAnError(t *testing.T) {
	w := newRBACWorld(t)
	svc, ctx := credSvc(w), context.Background()
	pid := w.s.projectID
	if _, err := svc.AttachGitHubCredential(ctx, credAs(w, "owner"), pid, "", "ci", ghToken("k")); err != nil {
		t.Fatal(err)
	}
	// A key this install doesn't have: the stored value no longer opens.
	other, err := secretbox.New([]byte(strings.Repeat("z", 32)))
	if err != nil {
		t.Fatal(err)
	}
	w.s.repo.SetSecretsKeys(other)
	t.Cleanup(func() { w.s.repo.SetSecretsKeys(defaultTestSecretsKeys(t)) })
	if _, err := svc.Repo.GitHubCredentialSecret(ctx, pid, ""); err == nil {
		t.Fatal("opened a value sealed under a key that isn't configured")
	}
	// The metadata read is unaffected: the credential is still attached.
	if info, err := svc.GitHubCredential(ctx, credAs(w, "owner"), pid, ""); err != nil || !info.Configured {
		t.Fatalf("read under a foreign key = %+v %v", info, err)
	}
}

// An empty host means github.com, and one host's credential is not another's:
// attaching for github.com doesn't make an enterprise host configured.
func TestGitHubCredential_PerHost(t *testing.T) {
	w := newRBACWorld(t)
	svc, ctx := credSvc(w), context.Background()
	pid, owner := w.s.projectID, credAs(w, "owner")
	if _, err := svc.AttachGitHubCredential(ctx, owner, pid, "", "dotcom", ghToken("m")); err != nil {
		t.Fatal(err)
	}
	ent, err := svc.GitHubCredential(ctx, owner, pid, "ghe.example.com")
	if err != nil || ent.Configured {
		t.Fatalf("enterprise host = %+v %v, want not configured", ent, err)
	}
	if _, err := svc.AttachGitHubCredential(ctx, owner, pid, "ghe.example.com", "ghe", ghToken("n")); err != nil {
		t.Fatal(err)
	}
	dotcom, err := svc.GitHubCredential(ctx, owner, pid, "")
	if err != nil || !dotcom.Configured || dotcom.Credential.Label != "dotcom" {
		t.Fatalf("github.com after attaching enterprise = %+v %v", dotcom, err)
	}
	if got, err := svc.Repo.GitHubCredentialSecret(ctx, pid, "ghe.example.com"); err != nil || got != ghToken("n") {
		t.Fatalf("enterprise token = %q %v", got, err)
	}
	// Deleting one host leaves the other alone.
	if had, err := svc.DeleteGitHubCredential(ctx, owner, pid, "ghe.example.com"); err != nil || !had {
		t.Fatalf("delete enterprise = %v %v", had, err)
	}
	if dotcom, err := svc.GitHubCredential(ctx, owner, pid, ""); err != nil || !dotcom.Configured {
		t.Fatalf("github.com after deleting enterprise = %+v %v", dotcom, err)
	}
}

// A token that can't be a header value is refused before any write, and the
// message never repeats it. This is the shape check, not validation: only
// GitHub can say whether a token works.
func TestGitHubCredential_RejectsUnusableTokenShapes(t *testing.T) {
	w := newRBACWorld(t)
	svc, ctx := credSvc(w), context.Background()
	owner := credAs(w, "owner")
	bad := map[string]string{
		"empty":        "",
		"too short":    "ghp_12",
		"a newline":    "ghp_abcdef0123456789\n",
		"a header":     "Authorization: token ghp_abcdef0123456789",
		"a space":      "ghp_abcdef 0123456789",
		"a tab":        "ghp_abcdef\t0123456789",
		"too long":     strings.Repeat("g", 513),
		"a pasted url": "https://github.com/settings/tokens/ghp_abcdef",
	}
	for name, token := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := svc.AttachGitHubCredential(ctx, owner, w.s.projectID, "", "ci", token)
			if err == nil {
				t.Fatalf("accepted %q", token)
			}
			if token != "" && strings.Contains(err.Error(), token) {
				t.Fatalf("the error echoes the input: %v", err)
			}
			if authz.StatusOf(err, 500) != 400 && !errors.Is(err, service.ErrGitHubTokenRequired) {
				t.Fatalf("%q: %v, want a 400", token, err)
			}
		})
	}
	// A plausible token of every GitHub prefix is accepted.
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		if _, err := svc.AttachGitHubCredential(ctx, owner, w.s.projectID, "", "ci", prefix+strings.Repeat("Ab3", 12)); err != nil {
			t.Fatalf("%s token refused: %v", prefix, err)
		}
	}
}

// A stored value that isn't sealed is reported, never returned: the CHECK in
// 045 makes this unreachable through SQL, and the storage layer still refuses
// to hand out a value it didn't seal. It runs in its own scratch database
// because it has to drop the constraint to create the drift.
func TestGitHubCredential_UnsealedRowIsRefused(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	repo.SetSecretsKeys(defaultTestSecretsKeys(t))

	var projectID int
	if err := repoDB(t, dsn).QueryRow(ctx, `INSERT INTO projects (name, code) VALUES ('drift', 'gc-drift') RETURNING id`).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetGitHubCredential(ctx, projectID, "", "ci", ghToken("p"), nil); err != nil {
		t.Fatal(err)
	}
	// Force the row past the CHECK, as a schema drift or a hand edit would.
	if _, err := repoDB(t, dsn).Exec(ctx, `ALTER TABLE project_github_credentials DROP CONSTRAINT project_github_credentials_sealed_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := repoDB(t, dsn).Exec(ctx, `UPDATE project_github_credentials SET sealed_secret = 'ghp_plaintext_leak' WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GitHubCredentialSecret(ctx, projectID, "")
	if !errors.Is(err, repository.ErrGitHubCredentialNotSealed) {
		t.Fatalf("unsealed row: %q %v, want ErrGitHubCredentialNotSealed", got, err)
	}
	if strings.Contains(err.Error(), "ghp_plaintext_leak") {
		t.Fatalf("the error echoes the stored value: %v", err)
	}
	// The metadata read is unaffected: the credential is still attached.
	if c, err := repo.GetGitHubCredential(ctx, projectID, ""); err != nil || c.Label != "ci" {
		t.Fatalf("metadata read = %+v %v", c, err)
	}
}

// repoDB opens a second connection to dsn, for statements the Repo has no
// method for (DDL and deliberate drift).
func repoDB(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	db, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	return db
}

// A rotation of the encryption key brings stored credentials under the new
// key at startup, and a value sealed under a key that is gone is counted as
// unopenable rather than silently dropped or re-sealed as garbage.
func TestGitHubCredential_ResealsOnKeyRotation(t *testing.T) {
	w := newRBACWorld(t)
	svc, ctx := credSvc(w), context.Background()
	pid, token := w.s.projectID, ghToken("r")
	if _, err := svc.AttachGitHubCredential(ctx, credAs(w, "owner"), pid, "", "ci", token); err != nil {
		t.Fatal(err)
	}
	before := storedCredentialSecret(t, testDSN(t), pid)

	// The new key becomes current, the old one retired: the value still opens
	// and is re-sealed under the new key.
	k2 := withSecretsKey(t, w.s.repo, testSecretsKey(2), defaultTestSecretsKey)
	resealed, unopenable, err := w.s.repo.ResealGitHubCredentialSecrets(ctx)
	if err != nil || resealed < 1 || unopenable != 0 {
		t.Fatalf("reseal = %d resealed, %d unopenable, %v", resealed, unopenable, err)
	}
	after := storedCredentialSecret(t, testDSN(t), pid)
	if after == before || secretbox.SealedKeyID(after) != k2.CurrentID() {
		t.Fatalf("after reseal the value is sealed under %q, want %q", secretbox.SealedKeyID(after), k2.CurrentID())
	}
	if got, err := w.s.repo.GitHubCredentialSecret(ctx, pid, ""); err != nil || got != token {
		t.Fatalf("token after reseal = %q %v", got, err)
	}
	// Idempotent.
	if n, _, err := w.s.repo.ResealGitHubCredentialSecrets(ctx); err != nil || n != 0 {
		t.Fatalf("second reseal = %d, %v", n, err)
	}

	// The key that sealed it goes away entirely: the value is counted, not
	// returned.
	withSecretsKey(t, w.s.repo, testSecretsKey(3))
	if _, unopenable, err := w.s.repo.ResealGitHubCredentialSecrets(ctx); err != nil || unopenable < 1 {
		t.Fatalf("reseal without the key = %d unopenable, %v", unopenable, err)
	}
	if got, err := w.s.repo.GitHubCredentialSecret(ctx, pid, ""); err == nil {
		t.Fatalf("opened a value sealed under a key that is gone: %q", got)
	}
	if strings.Contains(storedCredentialSecret(t, testDSN(t), pid), token) {
		t.Fatal("the stored value became plaintext")
	}
}
