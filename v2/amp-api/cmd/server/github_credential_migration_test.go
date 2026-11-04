package main

// Migration 045 adds the sealed project GitHub credential (AMP #3154). The
// test is about the schema's own guarantee, independent of the Go code that
// writes it:
//
//   - it applies on a fresh database and on top of 044 with credential rows
//     already in it, and existing rows read as "no token" rather than breaking;
//   - the column can hold a sealed value and refuses anything else, so a
//     plaintext token can't be stored even by a writer that forgets to seal;
//   - the facts cache's composite foreign key still points at the same row,
//     so attaching a secret doesn't disturb the isolation 044 established.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/secretbox"
)

// migration045 is 045's SQL, read from the embedded set.
func migration045(t *testing.T) repository.Migration {
	t.Helper()
	for _, m := range migrations {
		if strings.HasPrefix(m.Version, "045") {
			return m
		}
	}
	t.Fatal("no migration 045 in the embedded set")
	return repository.Migration{}
}

func TestMigration_045SealedGitHubCredential(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer repo.Close()
		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("fresh migrate: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		// The column exists, is NOT NULL and defaults to '' (the 044 state).
		var nullable, def string
		if err := db.QueryRow(ctx, `SELECT is_nullable, column_default FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'project_github_credentials'
			  AND column_name = 'sealed_secret'`).Scan(&nullable, &def); err != nil {
			t.Fatalf("sealed_secret column: %v", err)
		}
		if nullable != "NO" || !strings.Contains(def, "''") {
			t.Fatalf("sealed_secret is nullable=%s default=%s, want NOT NULL default ''", nullable, def)
		}
	})

	t.Run("upgrade from 044 keeps existing credentials and their facts", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "045" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 045")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 044: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		var projectID, credID int
		if err := db.QueryRow(ctx, `INSERT INTO projects (name, code) VALUES ('gc', 'gc045') RETURNING id`).Scan(&projectID); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO project_github_credentials (project_id, label) VALUES ($1, 'pre-3154')
			RETURNING id`, projectID).Scan(&credID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO github_repo_facts (project_id, repo_slug, credential_id, default_branch,
			default_branch_source, completeness, fetched_at) VALUES ($1, 'owner/repo', $2, 'main', 'rest', 'complete', NOW())`,
			projectID, credID); err != nil {
			t.Fatal(err)
		}

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 045: %v", err)
		}
		// The credential that was there reads as "attached, no token": its
		// row and the facts pointing at it are untouched.
		var sealed string
		var label string
		if err := db.QueryRow(ctx, `SELECT sealed_secret, label FROM project_github_credentials WHERE id = $1`, credID).
			Scan(&sealed, &label); err != nil {
			t.Fatalf("existing credential after upgrade: %v", err)
		}
		if sealed != "" || label != "pre-3154" {
			t.Fatalf("existing credential = sealed %q label %q", sealed, label)
		}
		var facts int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM github_repo_facts WHERE credential_id = $1`, credID).Scan(&facts); err != nil || facts != 1 {
			t.Fatalf("facts after upgrade = %d (%v), want 1", facts, err)
		}
		// Re-running 045 by hand is harmless.
		if _, err := db.Exec(ctx, migration045(t).SQL); err != nil {
			t.Fatalf("re-running 045: %v", err)
		}
	})

	t.Run("sealed only", func(t *testing.T) {
		d := newRepoFactsDB(t)
		// Anything that isn't a sealed value is refused: a plaintext token,
		// a token with a prefix that only looks sealed, an empty-ish value.
		for _, bad := range []string{
			"ghp_abcdef0123456789",
			"github_pat_11ABCDEFG",
			"enc:v2:deadbeef:c2VhbGVk", // a different scheme
			"enc:v1",                   // prefix only
		} {
			if err := d.exec(`UPDATE project_github_credentials SET sealed_secret = $1 WHERE id = $2`, bad, d.c1); err == nil {
				t.Fatalf("stored %q in sealed_secret, want the CHECK to refuse it", bad)
			}
		}
		// '' stays allowed: it is the 044 "reference metadata, no token" state.
		d.mustExec(`UPDATE project_github_credentials SET sealed_secret = '' WHERE id = $1`, d.c1)
		// And a value the storage layer produces is accepted.
		key := []byte(strings.Repeat("k", 32))
		ring, err := secretbox.New(key)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := ring.Seal(secretbox.PurposeGitHubCredential, "ghp_abcdef0123456789")
		if err != nil {
			t.Fatal(err)
		}
		d.mustExec(`UPDATE project_github_credentials SET sealed_secret = $1 WHERE id = $2`, sealed, d.c1)
		var stored string
		if err := d.db.QueryRow(d.ctx, `SELECT sealed_secret FROM project_github_credentials WHERE id = $1`, d.c1).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != sealed || strings.Contains(stored, "ghp_") {
			t.Fatalf("stored value = %q, want the sealed text and no token in it", stored)
		}
	})

	t.Run("facts isolation survives the new column", func(t *testing.T) {
		d := newRepoFactsDB(t)
		// Each project's facts still only name its own credential, now that
		// both rows can carry a secret.
		d.mustExec(`UPDATE project_github_credentials SET sealed_secret = 'enc:v1:aa:c2VhbGVk' WHERE id = $1`, d.c1)
		d.mustExec(`UPDATE project_github_credentials SET sealed_secret = 'enc:v1:bb:c2VhbGVk' WHERE id = $1`, d.c2)
		wantSQLState(t, "project 1's facts naming project 2's credential",
			d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, credential_id) VALUES ($1, 'owner/x', $2)`, d.p1, d.c2),
			"23503")
		// Deleting the credential still cascades to what it read.
		if err := d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, credential_id) VALUES ($1, 'owner/x', $2)`, d.p1, d.c1); err != nil {
			t.Fatal(err)
		}
		d.mustExec(`DELETE FROM project_github_credentials WHERE id = $1`, d.c1)
		var n int
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM github_repo_facts WHERE project_id = $1`, d.p1).Scan(&n); err != nil || n != 0 {
			t.Fatalf("facts after deleting the credential = %d (%v), want 0", n, err)
		}
	})
}
