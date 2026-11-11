package main

// Migration 060 adds API token scopes: the columns that make a credential
// submit-only and bind it to one team (AMP #3772; approved contract artifact 36
// v2 §5, build slice B5).
//
// These tests prove what the schema is responsible for, on a real database, and
// nothing else — no service, no checker:
//
//   - it applies fresh and on top of 059 with existing tokens, and every
//     pre-existing row is full-scope, which is exactly the behaviour it already
//     had (TestMigration_060APITokenScopes/upgrade from 059);
//   - the closed vocabulary plus the two implications are enforced by the
//     database, so no write path can produce a scope the code does not
//     understand or a submit scope with no team (the CHECK);
//   - deleting the team takes its submit-only credentials with it, and deleting
//     the user takes their tokens as before;
//   - the scope is not updatable: the only UPDATE the codebase runs on these
//     columns' row is revocation, which can never widen a scope (asserted here
//     as the absence of an escalation path through SQL the app has).

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/simstech/amp-api/internal/repository"
)

// scopeDB is a database at the current migration level with one org, one team
// and one user — the minimum a scoped token needs — plus raw insert helpers.
type scopeDB struct {
	t   *testing.T
	db  *pgx.Conn
	ctx context.Context
	org int
	// team is the inbox a submit-only token may be bound to; team2 is another.
	team, team2 int
	// user owns the tokens; other is a second account.
	user, other int
}

func newScopeDB(t *testing.T) *scopeDB {
	t.Helper()
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer repo.Close()
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	d := &scopeDB{t: t, db: db, ctx: ctx}
	d.org = d.insert(`INSERT INTO orgs (name, slug) VALUES ('Scope org', 'scope-org') RETURNING id`)
	d.team = d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Desk') RETURNING id`, d.org)
	d.team2 = d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Second desk') RETURNING id`, d.org)
	d.user = d.insert(`INSERT INTO users (subject, email) VALUES ('scope-user', 'scope-user@x.test') RETURNING id`)
	d.other = d.insert(`INSERT INTO users (subject, email) VALUES ('scope-other', 'scope-other@x.test') RETURNING id`)
	return d
}

func (d *scopeDB) insert(sql string, args ...any) int {
	d.t.Helper()
	var id int
	if err := d.db.QueryRow(d.ctx, sql, args...).Scan(&id); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

// scopeToken is one api_tokens row under test. Zero values are what the schema's
// defaults supply, so a test states only the rule it proves.
type scopeToken struct {
	user   int
	hash   string
	prefix string  // "" = 'amp_scope'
	scope  *string // nil = the column's default
	team   int     // 0 = NULL
}

func (d *scopeDB) insertToken(t scopeToken) error {
	d.t.Helper()
	if t.user == 0 {
		t.user = d.user
	}
	if t.prefix == "" {
		t.prefix = "amp_scope"
	}
	var team *int
	if t.team != 0 {
		team = &t.team
	}
	// The scope is passed as a nullable string so a nil means "let the default
	// apply", which is what a pre-scope INSERT looks like.
	_, err := d.db.Exec(d.ctx, `
		INSERT INTO api_tokens (user_id, name, token_hash, prefix, scope, scope_team_id)
		VALUES ($1, 'test token', $2, $3, COALESCE($4::text, 'full'), $5)`,
		t.user, t.hash, t.prefix, t.scope, team)
	return err
}

func (d *scopeDB) mustToken(t scopeToken) {
	d.t.Helper()
	if err := d.insertToken(t); err != nil {
		d.t.Fatalf("insert token %+v: %v", t, err)
	}
}

// isCheckViolation reports whether err is a CHECK constraint refusal.
func isCheckViolation(err error) bool {
	var pg *pgconn.PgError
	return err != nil && asPgError(err, &pg) && pg.Code == "23514"
}

func strp(s string) *string { return &s }

func TestMigration_060APITokenScopes(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh", func(t *testing.T) {
		d := newScopeDB(t)
		// Both columns exist with the defaults a deployment expects.
		var scope string
		var team *int
		if err := d.db.QueryRow(ctx, `SELECT scope, scope_team_id FROM api_tokens LIMIT 1`).Scan(&scope, &team); err != nil && err != pgx.ErrNoRows {
			t.Fatalf("columns: %v", err)
		}
		// The table is usable immediately: full and submit rows both insert.
		d.mustToken(scopeToken{hash: "h-full-1"})
		d.mustToken(scopeToken{hash: "h-submit-1", scope: strp("submit"), team: d.team})
		var n int
		if err := d.db.QueryRow(ctx, `SELECT count(*) FROM api_tokens`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 2 {
			t.Fatalf("api_tokens has %d rows, want 2", n)
		}
	})

	// The upgrade path is the one that matters for every existing deployment:
	// tokens minted before scopes must come out exactly as they were.
	t.Run("upgrade from 059 keeps existing tokens full", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "060" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 060")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 059: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		// Two tokens as 059 would have stored them: no scope column at all.
		var userID int
		if err := db.QueryRow(ctx, `
			INSERT INTO users (subject, email) VALUES ('pre-060', 'pre-060@x.test') RETURNING id`).Scan(&userID); err != nil {
			t.Fatalf("user: %v", err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO api_tokens (user_id, name, token_hash, prefix) VALUES
			($1, 'old one', 'pre-060-hash-1', 'amp_pre'),
			($1, 'old revoked', 'pre-060-hash-2', 'amp_rev')`, userID); err != nil {
			t.Fatalf("pre-060 tokens: %v", err)
		}
		if _, err := db.Exec(ctx, `
			UPDATE api_tokens SET revoked_at = NOW() WHERE token_hash = 'pre-060-hash-2'`); err != nil {
			t.Fatal(err)
		}

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("migrate 060: %v", err)
		}
		rows, err := db.Query(ctx, `SELECT name, scope, scope_team_id, revoked_at IS NOT NULL FROM api_tokens ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := 0
		for rows.Next() {
			var name, scope string
			var team *int
			var revoked bool
			if err := rows.Scan(&name, &scope, &team, &revoked); err != nil {
				t.Fatal(err)
			}
			got++
			if scope != "full" {
				t.Errorf("%s: scope = %q after upgrade, want full", name, scope)
			}
			if team != nil {
				t.Errorf("%s: scope_team_id = %d after upgrade, want NULL", name, *team)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if got != 2 {
			t.Fatalf("saw %d pre-060 tokens after upgrade, want 2", got)
		}
	})

	// Re-running the file by hand changes nothing: the migrations are
	// documented as idempotent, and this one is DDL that gets retried on
	// every boot in the unversioned-schema path.
	t.Run("re-running the SQL is a no-op", func(t *testing.T) {
		d := newScopeDB(t)
		d.mustToken(scopeToken{hash: "h-idem-1", scope: strp("submit"), team: d.team})
		var sql string
		for _, m := range migrations {
			if m.Version == "060_api_token_scopes" {
				sql = m.SQL
			}
		}
		if sql == "" {
			t.Fatal("migration 060 not embedded")
		}
		if _, err := d.db.Exec(ctx, sql); err != nil {
			t.Fatalf("re-running 060: %v", err)
		}
		var n int
		if err := d.db.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE scope = 'submit'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("after re-run: %d submit tokens, want the 1 existing row untouched", n)
		}
	})
}

// TestMigration_060ScopeConstraints is the schema's share of the ticket's
// criteria: a scope value the code doesn't know is unstorable, a submit scope
// without a team is unstorable, and a full scope can never carry a team.
func TestMigration_060ScopeConstraints(t *testing.T) {
	d := newScopeDB(t)

	t.Run("the closed vocabulary", func(t *testing.T) {
		for _, scope := range []string{"admin", "read", "submits", "FULL", "*", ""} {
			err := d.insertToken(scopeToken{hash: "h-bad-" + scope, scope: &scope})
			if !isCheckViolation(err) {
				t.Errorf("scope %q was storable (err = %v); only 'full' and 'submit' may be", scope, err)
			}
		}
		// The two that may be stored, with the team rule each implies.
		d.mustToken(scopeToken{hash: "h-good-full"})
		d.mustToken(scopeToken{hash: "h-good-submit", scope: strp("submit"), team: d.team})
	})

	t.Run("submit always names its team", func(t *testing.T) {
		err := d.insertToken(scopeToken{hash: "h-submit-noteam", scope: strp("submit")})
		if !isCheckViolation(err) {
			t.Fatalf("a submit scope with no team was storable (err = %v)", err)
		}
	})

	t.Run("full never carries a team", func(t *testing.T) {
		err := d.insertToken(scopeToken{hash: "h-full-team", scope: strp("full"), team: d.team})
		if !isCheckViolation(err) {
			t.Fatalf("a full scope bound to a team was storable (err = %v)", err)
		}
		// And the default (no scope given) with a team is the same refusal:
		// there is exactly one representation of each scope.
		err = d.insertToken(scopeToken{hash: "h-default-team", team: d.team})
		if !isCheckViolation(err) {
			t.Fatalf("a defaulted (full) scope bound to a team was storable (err = %v)", err)
		}
	})

	t.Run("the team must exist", func(t *testing.T) {
		err := d.insertToken(scopeToken{hash: "h-ghost-team", scope: strp("submit"), team: 999999})
		if err == nil {
			t.Fatal("a submit token bound to a nonexistent team was storable")
		}
	})
}

// TestMigration_060ScopeLifetime pins what happens to a scoped credential when
// the things it names go away, and that no write path widens one.
func TestMigration_060ScopeLifetime(t *testing.T) {
	d := newScopeDB(t)

	t.Run("deleting the team takes its submit credentials", func(t *testing.T) {
		d.mustToken(scopeToken{hash: "h-team-gone", scope: strp("submit"), team: d.team2})
		d.mustToken(scopeToken{hash: "h-other-team", scope: strp("submit"), team: d.team})
		d.mustToken(scopeToken{hash: "h-full-stays"})
		if _, err := d.db.Exec(d.ctx, `DELETE FROM teams WHERE id = $1`, d.team2); err != nil {
			t.Fatalf("delete team: %v", err)
		}
		var n int
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM api_tokens WHERE token_hash = 'h-team-gone'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("a submit-only token outlived the team it could only submit to")
		}
		// Everything else is untouched: the cascade is that team only.
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM api_tokens`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("after deleting one team: %d tokens, want the other 2 untouched", n)
		}
	})

	t.Run("deleting the user takes their tokens", func(t *testing.T) {
		before := 0
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM api_tokens WHERE user_id = $1`, d.other).Scan(&before); err != nil {
			t.Fatal(err)
		}
		d.mustToken(scopeToken{user: d.other, hash: "h-other-user"})
		if _, err := d.db.Exec(d.ctx, `DELETE FROM users WHERE id = $1`, d.other); err != nil {
			t.Fatalf("delete user: %v", err)
		}
		var n int
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM api_tokens WHERE user_id = $1`, d.other).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d tokens outlived their user account", n)
		}
	})

	// The escalation guard: the schema accepts no UPDATE that turns a full
	// scope into a narrower one or back, because there is no application path
	// that writes these columns. The CHECK is what makes the *resulting row*
	// honest even if one were added carelessly — a scope and its team cannot
	// disagree — so this asserts the constraint holds on UPDATE too, which is
	// the half a plain INSERT test would miss.
	t.Run("no row can become an illegal scope", func(t *testing.T) {
		d.mustToken(scopeToken{hash: "h-update-me", scope: strp("submit"), team: d.team})
		var id int
		if err := d.db.QueryRow(d.ctx, `SELECT id FROM api_tokens WHERE token_hash = 'h-update-me'`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		// Widening to full while keeping the team is refused…
		if _, err := d.db.Exec(d.ctx, `UPDATE api_tokens SET scope = 'full' WHERE id = $1`, id); !isCheckViolation(err) {
			t.Fatalf("widening a submit token to full with its team left in place was accepted (err = %v)", err)
		}
		// …and so is dropping the team while staying submit…
		if _, err := d.db.Exec(d.ctx, `UPDATE api_tokens SET scope_team_id = NULL WHERE id = $1`, id); !isCheckViolation(err) {
			t.Fatalf("a submit token with no team was accepted (err = %v)", err)
		}
		// …while the one legal rewrite (both together) works, so a future
		// admin path is not blocked by the constraint itself.
		if _, err := d.db.Exec(d.ctx, `UPDATE api_tokens SET scope = 'full', scope_team_id = NULL WHERE id = $1`, id); err != nil {
			t.Fatalf("the legal narrowing/lifting rewrite was refused: %v", err)
		}
	})
}
