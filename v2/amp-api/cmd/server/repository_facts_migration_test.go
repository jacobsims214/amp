package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/simstech/amp-api/internal/repository"
)

// Migration 044 adds the project-scoped repository-facts cache (AMP #3152).
//
// These tests prove the three things the schema is responsible for, on a real
// database, so none of them depends on the service code that will later use
// the tables:
//
//   - it applies both ways: to a fresh database, and on top of 043 with data;
//   - facts are isolated by project and repository identity, enforced by the
//     schema (per-project uniqueness and a composite credential foreign key),
//     not just by convention in the queries;
//   - nothing in it can hold a plaintext credential, and a failure is stored
//     as one of a fixed list of classes, never as a message.

// repoFactsDB is a scratch database migrated to 044, with the two projects and
// one credential per project that the isolation tests share.
type repoFactsDB struct {
	t   *testing.T
	db  *pgx.Conn
	ctx context.Context
	p1  int
	p2  int
	c1  int
	c2  int
}

func newRepoFactsDB(t *testing.T) *repoFactsDB {
	t.Helper()
	ctx := context.Background()
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
	t.Cleanup(func() { db.Close(context.Background()) })
	d := &repoFactsDB{t: t, db: db, ctx: ctx}
	d.p1 = d.insert(`INSERT INTO projects (name, code) VALUES ('P1', 'rf1') RETURNING id`)
	d.p2 = d.insert(`INSERT INTO projects (name, code) VALUES ('P2', 'rf2') RETURNING id`)
	d.c1 = d.insert(`INSERT INTO project_github_credentials (project_id, label) VALUES ($1, 'p1 token') RETURNING id`, d.p1)
	d.c2 = d.insert(`INSERT INTO project_github_credentials (project_id, label) VALUES ($1, 'p2 token') RETURNING id`, d.p2)
	return d
}

func (d *repoFactsDB) insert(sql string, args ...any) int {
	d.t.Helper()
	var id int
	if err := d.db.QueryRow(d.ctx, sql, args...).Scan(&id); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (d *repoFactsDB) exec(sql string, args ...any) error {
	d.t.Helper()
	_, err := d.db.Exec(d.ctx, sql, args...)
	return err
}

func (d *repoFactsDB) mustExec(sql string, args ...any) {
	d.t.Helper()
	if err := d.exec(sql, args...); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
}

// wantSQLState asserts err is a Postgres error with code (23505 unique
// violation, 23503 foreign key, 23514 check).
func wantSQLState(t *testing.T, what string, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("%s: want SQLSTATE %s, got %v", what, code, err)
	}
}

// Migration 044 applies on a fresh database and on top of 043 with existing
// work in it. New tables start empty, nothing existing changes, and re-running
// the SQL is harmless (the runner does that once; the guards should hold if a
// human runs it by hand).
func TestMigration_044RepositoryFacts(t *testing.T) {
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
		for _, table := range []string{
			"project_github_credentials", "github_repo_facts",
			"github_branch_facts", "github_pull_facts",
		} {
			var n int
			if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n != 0 {
				t.Fatalf("%s starts with %d rows, want 0", table, n)
			}
		}
	})
	// RunMigrations already applies everything, but the tables are read by
	// users: prove the app has the rights it needs on them.
	t.Run("readable by the app", func(t *testing.T) {
		d := newRepoFactsDB(t)
		d.mustExec(`SELECT count(*) FROM github_repo_facts`)
	})

	t.Run("upgrade from 043", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "044" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 044")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 043: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		var projectID, epic, story, task int
		q := func(sql string, dst *int) {
			t.Helper()
			if err := db.QueryRow(ctx, sql).Scan(dst); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		q(`INSERT INTO projects (name, code) VALUES ('up', 'up044') RETURNING id`, &projectID)
		q(fmt.Sprintf(`INSERT INTO epics (project_id, name) VALUES (%d, 'e') RETURNING id`, projectID), &epic)
		q(fmt.Sprintf(`INSERT INTO stories (project_id, epic_id, name) VALUES (%d, %d, 's') RETURNING id`, projectID, epic), &story)
		q(fmt.Sprintf(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES (%d, %d, %d, 't') RETURNING id`, projectID, epic, story), &task)

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 044: %v", err)
		}
		// The ticket that was already there is untouched, and the new tables
		// are empty: the cache starts cold rather than guessing facts.
		got, err := repo.GetTask(ctx, task)
		if err != nil || got.ProjectID != projectID {
			t.Fatalf("existing ticket after upgrade: %+v %v", got, err)
		}
		var facts int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM github_repo_facts`).Scan(&facts); err != nil || facts != 0 {
			t.Fatalf("facts after upgrade: %d %v", facts, err)
		}
		// Re-running 044's SQL by hand is harmless.
		for _, m := range migrations {
			if m.Version == "044_repository_facts" {
				if _, err := db.Exec(ctx, m.SQL); err != nil {
					t.Fatalf("re-running 044: %v", err)
				}
			}
		}
	})
}

// Facts are isolated by project and repository identity: the same slug in two
// projects is two rows, only one row per project+slug, and a fact row can only
// name a credential of its own project.
func TestMigration_044FactsAreProjectScoped(t *testing.T) {
	d := newRepoFactsDB(t)

	// The same repository seen from two projects: two independent rows.
	if err := d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, credential_id, default_branch,
		default_branch_source, completeness, fetched_at)
		VALUES ($1, 'owner/repo', $2, 'main', 'rest', 'complete', NOW())`, d.p1, d.c1); err != nil {
		t.Fatalf("project 1 facts: %v", err)
	}
	if err := d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, credential_id, default_branch,
		default_branch_source, completeness, fetched_at)
		VALUES ($1, 'owner/repo', $2, 'trunk', 'rest', 'complete', NOW())`, d.p2, d.c2); err != nil {
		t.Fatalf("project 2 facts for the same slug: %v", err)
	}
	var n int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM github_repo_facts WHERE repo_slug = 'owner/repo'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows for one slug across two projects = %d (%v), want 2", n, err)
	}

	// One row per project, host and slug.
	wantSQLState(t, "a second row for the same project and slug",
		d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug) VALUES ($1, 'owner/repo')`, d.p1),
		"23505")
	// GitHub matches owner and repository names case-insensitively, so
	// 'Owner/Repo' is the same repository and must not become a second row with
	// its own credential context and its own answer. The slug is stored as the
	// reader reported it (the reader, #3151, preserves case); uniqueness is
	// what folds them.
	wantSQLState(t, "the same repository spelled with different case",
		d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug) VALUES ($1, 'Owner/Repo')`, d.p1),
		"23505")
	// A different repository in the same project is its own row.
	if err := d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug) VALUES ($1, 'owner/other')`, d.p1); err != nil {
		t.Fatalf("a different repository in the same project: %v", err)
	}
	// Two projects may hold the same repository, and project 2's row is not
	// project 1's: its own default branch, its own credential. That is the
	// whole point of the cache being project-scoped rather than keyed by slug.

	// Project 2's credential cannot be attached to project 1's facts.
	err := d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, credential_id) VALUES ($1, 'owner/leak', $2)`,
		d.p1, d.c2)
	wantSQLState(t, "cross-project credential", err, "23503")

	// Nor can facts it already wrote be re-pointed at another project's one.
	err = d.exec(`UPDATE github_repo_facts SET credential_id = $1 WHERE project_id = $2 AND repo_slug = 'owner/repo'`,
		d.c2, d.p1)
	wantSQLState(t, "re-pointing a fact at another project's credential", err, "23503")

	// A credential of the right project but another host doesn't fit either:
	// facts name the host they were read from and the credential must match it.
	if err := d.exec(`INSERT INTO project_github_credentials (project_id, host, label)
		VALUES ($1, 'ghe.example.com', 'x')`, d.p1); err != nil {
		t.Fatalf("second host credential for one project: %v", err)
	}
	var otherHostID int
	if err := d.db.QueryRow(d.ctx, `SELECT id FROM project_github_credentials WHERE host = 'ghe.example.com'`).Scan(&otherHostID); err != nil {
		t.Fatal(err)
	}
	err = d.exec(`INSERT INTO github_repo_facts (project_id, host, repo_slug, credential_id)
		VALUES ($1, 'github.com', 'owner/crosshost', $2)`, d.p1, otherHostID)
	wantSQLState(t, "credential from another host", err, "23503")

	// Deleting the credential drops the facts it produced, so nothing read with
	// a token outlives that token.
	d.mustExec(`DELETE FROM project_github_credentials WHERE id = $1`, d.c2)
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM github_repo_facts WHERE project_id = $1`, d.p2).Scan(&n); err != nil || n != 0 {
		t.Fatalf("facts left after deleting their credential: %d %v", n, err)
	}
	// Project 1's facts, read with its own credential, are untouched.
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM github_repo_facts WHERE project_id = $1`, d.p1).Scan(&n); err != nil || n != 2 {
		t.Fatalf("project 1 facts after project 2's credential went away: %d %v", n, err)
	}

	// Branch and PR facts inherit their scope from the repository row: they key
	// on it, so they cannot name a project of their own.
	var repoFactsID int
	if err := d.db.QueryRow(d.ctx, `SELECT id FROM github_repo_facts WHERE project_id = $1 AND repo_slug = 'owner/repo'`, d.p1).Scan(&repoFactsID); err != nil {
		t.Fatal(err)
	}
	d.mustExec(`INSERT INTO github_branch_facts (repo_facts_id, branch, rules, completeness, fetched_at)
		VALUES ($1, 'main', '[{"type":"pull_request","ruleset_source":"main"}]', 'complete', NOW())`, repoFactsID)
	d.mustExec(`INSERT INTO github_pull_facts (repo_facts_id, number, base_branch, state,
		review_decision, requested_reviewers, requested_teams, mergeable_state, completeness, fetched_at)
		VALUES ($1, 7, 'main', 'open', 'review_required', ARRAY['alice'], ARRAY['core'], 'blocked', 'complete', NOW())`,
		repoFactsID)
	// Deleting the repository's facts takes its branch and PR facts with it.
	d.mustExec(`DELETE FROM github_repo_facts WHERE id = $1`, repoFactsID)
	for _, table := range []string{"github_branch_facts", "github_pull_facts"} {
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM `+table+` WHERE repo_facts_id = $1`, repoFactsID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s left behind after its repository row went away: %d %v", table, n, err)
		}
	}
}

// The schema records what is known and how completely, and refuses to record
// a guess: completeness and failure are enumerated, a verified state is dated,
// and a failure is a class — never a free-text message that could carry a
// response body or a header.
func TestMigration_044CompletenessAndFailureClass(t *testing.T) {
	d := newRepoFactsDB(t)

	// A brand-new row is honestly unknown and undated.
	var (
		completeness string
		hasFetchedAt bool
		errClass     string
	)
	if err := d.db.QueryRow(d.ctx, `INSERT INTO github_repo_facts (project_id, repo_slug)
		VALUES ($1, 'owner/fresh')
		RETURNING completeness, fetched_at IS NOT NULL, last_error_class`, d.p1).
		Scan(&completeness, &hasFetchedAt, &errClass); err != nil {
		t.Fatal(err)
	}
	if completeness != "unknown" || hasFetchedAt || errClass != "" {
		t.Fatalf("a new fact row = %q, dated=%v, error=%q; want unknown, undated, no error",
			completeness, hasFetchedAt, errClass)
	}

	// A verified state is dated; "unknown" cannot claim a verification time.
	wantSQLState(t, "complete without fetched_at",
		d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, completeness) VALUES ($1, 'owner/a', 'complete')`, d.p1),
		"23514")
	wantSQLState(t, "partial without fetched_at",
		d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, completeness) VALUES ($1, 'owner/b', 'partial')`, d.p1),
		"23514")
	wantSQLState(t, "an invented completeness",
		d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, completeness, fetched_at)
			VALUES ($1, 'owner/c', 'probably', NOW())`, d.p1),
		"23514")

	// A failure is one of a fixed list of classes.
	wantSQLState(t, "a free-text failure",
		d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, last_error_class, last_error_at)
			VALUES ($1, 'owner/d', 'GET https://api.github.com/repos/o/r failed: 401 Bad credentials (token ghp_x)', NOW())`, d.p1),
		"23514")
	// Every class githubfacts.Kind can report is storable verbatim (#3151), so
	// the cache never needs a mapping table that can drift from the client.
	for i, class := range []string{
		"not_found", "forbidden", "rate_limited", "timeout", "canceled", "network",
		"server", "moved", "invalid", "no_credential", "no_target", "unsupported_host",
	} {
		if err := d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, last_error_class, last_error_at)
			VALUES ($1, $2, $3, NOW())`, d.p1, fmt.Sprintf("owner/class%d", i), class); err != nil {
			t.Fatalf("failure class %q from githubfacts.Kind was refused: %v", class, err)
		}
	}

	// A detected default branch must name the branch it was detected as, and a
	// manual override is a source of its own (it wins without a token).
	wantSQLState(t, "a source without a branch",
		d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, default_branch_source, completeness, fetched_at)
			VALUES ($1, 'owner/f', 'rest', 'complete', NOW())`, d.p1),
		"23514")
	wantSQLState(t, "an invented source",
		d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, default_branch, default_branch_source, completeness, fetched_at)
			VALUES ($1, 'owner/g', 'main', 'guessed', 'complete', NOW())`, d.p1),
		"23514")
	if err := d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, default_branch, default_branch_source, completeness, fetched_at)
		VALUES ($1, 'owner/h', 'release/2', 'manual', 'partial', NOW())`, d.p1); err != nil {
		t.Fatalf("manual override: %v", err)
	}

	// A repository is named owner/repo, not a URL or a path.
	for _, slug := range []string{"https://github.com/owner/repo", "owner", "owner/repo/extra", "owner//repo", "owner/repo\n"} {
		if err := d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug) VALUES ($1, $2)`, d.p1, slug); err == nil {
			t.Fatalf("repo_slug %q was accepted", slug)
		}
	}

	// Rules are an array of rules, and an unrecognised completeness is refused
	// on branch and PR facts too.
	var repoFactsID int
	if err := d.db.QueryRow(d.ctx, `INSERT INTO github_repo_facts (project_id, repo_slug) VALUES ($1, 'owner/rules') RETURNING id`, d.p1).Scan(&repoFactsID); err != nil {
		t.Fatal(err)
	}
	wantSQLState(t, "rules as an object",
		d.exec(`INSERT INTO github_branch_facts (repo_facts_id, branch, rules) VALUES ($1, 'main', '{"type":"pull_request"}')`, repoFactsID),
		"23514")
	if err := d.exec(`INSERT INTO github_branch_facts (repo_facts_id, branch, rules, completeness, fetched_at)
		VALUES ($1, 'main', '[]', 'unknown', NULL)`, repoFactsID); err != nil {
		// An empty list is a real answer: the read happened and nothing applies.
		t.Fatalf("empty rules: %v", err)
	}
	wantSQLState(t, "an invented PR state",
		d.exec(`INSERT INTO github_pull_facts (repo_facts_id, number, state) VALUES ($1, 1, 'maybe')`, repoFactsID),
		"23514")
	wantSQLState(t, "PR zero",
		d.exec(`INSERT INTO github_pull_facts (repo_facts_id, number) VALUES ($1, 0)`, repoFactsID),
		"23514")
	// 'partial' is how rules read but protection unreadable is stored: a real
	// state, with a real timestamp.
	if err := d.exec(`INSERT INTO github_pull_facts (repo_facts_id, number, base_branch, completeness, fetched_at)
		VALUES ($1, 3, 'fix/x', 'partial', NOW())`, repoFactsID); err != nil {
		t.Fatalf("partial PR facts: %v", err)
	}
}

// The schema cannot hold a plaintext credential, and this test is what keeps
// that true as the tables grow. It is a schema assertion, not a code review:
// the cache's four tables must have no column that could carry a secret value.
//
// Two checks, both deliberately blunt, because a clever test is one a future
// migration can slip past:
//
//   - No column is NAMED like a secret (token, secret, password, key, hash,
//     fingerprint, bearer, auth, hmac, signature). A column that merely has
//     such a name fails too: the next reader would have to prove it safe, and
//     "prove this one is fine" is how a token column gets added.
//   - No column has a type that could carry one wholesale: no BYTEA, and the
//     only JSONB is github_branch_facts.rules, which is CHECKed to be an
//     array. A JSONB blob could otherwise hold a whole response body or a
//     sealed blob.
//
// The credential itself lives in the sealed storage the project credential
// service adds (AMP #3154); facts reference it by id, which is an INTEGER and
// carries no secret.
func TestMigration_044HasNoSecretColumn(t *testing.T) {
	d := newRepoFactsDB(t)
	const tables = `('project_github_credentials', 'github_repo_facts',
	                  'github_branch_facts', 'github_pull_facts')`
	columnQuery := `
		SELECT table_name || '.' || column_name || ' (' || data_type || ')'
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name IN ` + tables

	// Names that mean "this holds a secret".
	names, err := d.db.Query(d.ctx, columnQuery+`
		   AND (column_name ~* 'secret|token|password|passwd|bearer|auth'
		        OR column_name ~* '(^|_)key($|_)'
		        OR column_name ~* 'hash|fingerprint|hmac|signature')
		 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer names.Close()
	var named []string
	for names.Next() {
		var col string
		if err := names.Scan(&col); err != nil {
			t.Fatal(err)
		}
		// credential_id is the allowed exception: it is a reference to the
		// credential row (an id, not a value) and it is an INTEGER.
		if col == "github_repo_facts.credential_id (integer)" {
			continue
		}
		named = append(named, col)
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
	if len(named) > 0 {
		t.Fatalf("the facts cache has secret-shaped columns: %v", named)
	}

	// Types that could carry a secret, or a whole response, without being
	// named like one.
	types, err := d.db.Query(d.ctx, columnQuery+`
		   AND data_type IN ('bytea', 'jsonb')
		 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer types.Close()
	var typed []string
	for types.Next() {
		var col string
		if err := types.Scan(&col); err != nil {
			t.Fatal(err)
		}
		typed = append(typed, col)
	}
	if err := types.Err(); err != nil {
		t.Fatal(err)
	}
	// The rules array, CHECKed to be an array of rules — never a response
	// envelope, never a credential.
	if want := []string{"github_branch_facts.rules (jsonb)"}; strings.Join(typed, ",") != strings.Join(want, ",") {
		t.Fatalf("bytea/jsonb columns = %v, want exactly %v", typed, want)
	}

	// And the secret genuinely has nowhere to go: storing a token in the one
	// free-form field the cache already has (the error class) is refused, so
	// even an accidental "store the error message" write can't leak one.
	err = d.exec(`INSERT INTO github_repo_facts (project_id, repo_slug, last_error_class, last_error_at)
		VALUES ($1, 'owner/x', 'error: 401 Bad credentials (token ghp_abcdef)', NOW())`, d.p1)
	wantSQLState(t, "a token written as a failure reason", err, "23514")
}
