package repository

// Tests for request_rows.go against a real Postgres (AMP #3870, epic 202 story
// 535). They run only when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/... -run 'RequestRows|GetRequest' -count=1
//
// The three things this layer promises are what the tests assert, in order:
//
//  1. every typed field of domain.TeamRequest round-trips — a value written to
//     each of migration 053's columns is read back into the same field, stated
//     field by field so a mis-ordered scan names the field it broke;
//  2. requestCols is exactly the table's column set, so a column added by a
//     later migration can never be silently missing from the scanner (the
//     failure mode the rest of the repository's *Cols constants exist to
//     prevent);
//  3. the lookup is tenant-scoped and its errors are distinguishable: another
//     team's request and an unknown id are the same ErrNotFound, a lookup with
//     no tenant is its own error, and a real load failure is neither — it keeps
//     its own identity so a handler cannot report a database problem as 404.
//
// A fourth test covers the reason the scanner is a package internal rather than
// a method body: the claim and update slices load the row they are about to
// change inside their own transaction, so getRequest takes the repository's
// queryRower and is exercised through a pgx.Tx, FOR UPDATE included.
//
// Every guard was mutation-tested by hand: swapping two same-typed scan targets,
// dropping the tenant from the predicate, dropping one half of it, letting a
// missing tenant through, collapsing a load error into ErrNotFound, and
// removing a column or a scan target each fail the test that owns it.
//
// Every test creates its own org, teams and users with a random suffix, and its
// public_refs carry that suffix too, so repeated runs and tests running in
// parallel don't see each other's rows.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// requestRowsTestRepo returns a Repo whose database has every migration
// applied. It is deliberately separate from kbTestRepo so a failure here names
// this subsystem, not the KB's.
func requestRowsTestRepo(t *testing.T) *Repo {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the request-row tests against Postgres")
	}
	ctx := context.Background()
	r, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(r.Close)
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "server", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	var ms []Migration
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ms = append(ms, Migration{Version: strings.TrimSuffix(filepath.Base(f), ".sql"), SQL: string(b)})
	}
	if err := r.RunMigrations(ctx, ms); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return r
}

// requestRowsFixture is one test's two orgs, their teams, and the users a
// request's nullable columns point at.
type requestRowsFixture struct {
	r   *Repo
	ctx context.Context
	t   *testing.T
	// suffix makes this fixture's public_ref values unique across repeated runs
	// and concurrent tests: public_ref is unique across the whole table.
	suffix int
	org    int
	other  int // a second org, for cross-tenant checks
	team   int
	team2  int // a second team in org, for cross-tenant checks
	user   int
	other2 int // a user in the other org
}

func newRequestRowsFixture(t *testing.T) *requestRowsFixture {
	t.Helper()
	r := requestRowsTestRepo(t)
	ctx := context.Background()
	// A random suffix keeps concurrent tests and repeated runs apart, the same
	// way kb_test.go's newScope does.
	suffix := rand.IntN(1_000_000_000)
	f := &requestRowsFixture{r: r, ctx: ctx, t: t, suffix: suffix}
	insert := func(sql string, dst *int, args ...any) {
		t.Helper()
		if err := r.db.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	insert(`INSERT INTO orgs (name, slug) VALUES ('rows org', $1) RETURNING id`, &f.org, fmt.Sprintf("rows-%d", suffix))
	insert(`INSERT INTO orgs (name, slug) VALUES ('rows other org', $1) RETURNING id`, &f.other, fmt.Sprintf("rows-other-%d", suffix))
	insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Desk') RETURNING id`, &f.team, f.org)
	insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Second desk') RETURNING id`, &f.team2, f.org)
	insert(`INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`,
		&f.user, fmt.Sprintf("rows-user-%d", suffix), fmt.Sprintf("rows-user-%d@rows.test", suffix))
	insert(`INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`,
		&f.other2, fmt.Sprintf("rows-other-%d", suffix), fmt.Sprintf("rows-other-%d@rows.test", suffix))
	return f
}

// ref builds a public_ref this fixture owns: the label plus the run's random
// suffix, which is long enough to satisfy the schema's 16-character floor and
// unique across runs against a database that is not reset between them.
func (f *requestRowsFixture) ref(label string) string {
	return fmt.Sprintf("%s%09d", label, f.suffix)
}

// requestTime is a fixture timestamp: whole microseconds, which is the precision
// timestamptz stores, so a round-trip comparison needs no tolerance.
func requestTime(offset time.Duration) time.Time {
	return time.Now().UTC().Add(offset).Truncate(time.Microsecond)
}

// insertRequest writes r's columns explicitly (a fixture may name them; the
// production reads may not) and returns the id the database assigned.
func (f *requestRowsFixture) insertRequest(r *domain.TeamRequest) int {
	f.t.Helper()
	var id int
	err := f.r.db.QueryRow(f.ctx, `
		INSERT INTO team_requests (
			public_ref, org_id, team_id, requester_user_id, requester_contact,
			source_kind, source_external_id, subject, input_brief, outcome_brief, state,
			claim_user_id, claimed_at, owner_user_id, duplicate_of, reject_reason,
			needs_info_since, planned_at, delivered_at, accepted_at, accepted_by,
			closed_at, reopened_at, reopened_count, rev, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			$12, $13, $14, $15, $16, $17, $18, $19, $20, $21,
			$22, $23, $24, $25, $26, $27, $28)
		RETURNING id`,
		r.PublicRef, r.OrgID, r.TeamID, r.RequesterUserID, r.RequesterContact,
		string(r.SourceKind), r.SourceExternalID, r.Subject, r.InputBrief, r.OutcomeBrief, string(r.State),
		r.ClaimUserID, r.ClaimedAt, r.OwnerUserID, r.DuplicateOf, r.RejectReason,
		r.NeedsInfoSince, r.PlannedAt, r.DeliveredAt, r.AcceptedAt, r.AcceptedBy,
		r.ClosedAt, r.ReopenedAt, r.ReopenedCount, r.Rev, r.Metadata, r.CreatedAt, r.UpdatedAt,
	).Scan(&id)
	if err != nil {
		f.t.Fatalf("insert request %q: %v", r.PublicRef, err)
	}
	return id
}

// assertRequestEqual compares every field of got against want, naming the field
// on a mismatch so a scan that is out of step with requestCols fails with the
// field it broke rather than "structs differ". Times are compared as instants
// (pgx returns them in the session's location, which is not part of the value)
// and everything else by value.
func assertRequestEqual(t *testing.T, what string, got, want *domain.TeamRequest) {
	t.Helper()
	gv, wv := reflect.ValueOf(*got), reflect.ValueOf(*want)
	typ := gv.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		g, w := gv.Field(i).Interface(), wv.Field(i).Interface()
		var same bool
		switch value := g.(type) {
		case time.Time:
			same = value.Equal(w.(time.Time))
		case *time.Time:
			other := w.(*time.Time)
			same = (value == nil) == (other == nil) && (value == nil || value.Equal(*other))
		default:
			same = reflect.DeepEqual(g, w)
		}
		if !same {
			t.Errorf("%s: field %s = %s, want %s", what, field.Name, showRequestValue(g), showRequestValue(w))
		}
	}
}

// showRequestValue prints a field for a test message without the pointer
// addresses %v would otherwise show.
func showRequestValue(v any) string {
	switch value := v.(type) {
	case *time.Time:
		if value == nil {
			return "nil"
		}
		return value.UTC().Format(time.RFC3339Nano)
	case *int:
		if value == nil {
			return "nil"
		}
		return fmt.Sprintf("%d", *value)
	case time.Time:
		return value.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%#v", v)
}

// TestRequestRowsRoundTrip is criterion 1: every typed field of
// domain.TeamRequest survives a write and a read. Three rows cover the shape —
// a requester_accepted request with every nullable column set, a duplicate that
// points at it, and a bare `new` request with every nullable column NULL — so a
// null that turned into a zero value, or a value that landed in the wrong field,
// fails here rather than in a service three slices from now.
func TestRequestRowsRoundTrip(t *testing.T) {
	f := newRequestRowsFixture(t)
	// A fixed base so the fixture's timestamps are distinct per column: a scan
	// that swapped two of them then fails with the fields named.
	base := requestTime(-2 * time.Hour)
	at := func(minutes int) *time.Time {
		v := base.Add(time.Duration(minutes) * time.Minute)
		return &v
	}
	user, otherUser := f.user, f.other2

	full := &domain.TeamRequest{
		PublicRef:        f.ref("rowsfullrequest"),
		OrgID:            f.org,
		TeamID:           f.team,
		RequesterUserID:  &user,
		RequesterContact: "requester@rows.test",
		SourceKind:       domain.RequestSourceConnector,
		// Distinct from public_ref and from every timestamp, so a column swap
		// between the two string columns is visible.
		SourceExternalID: "gh-issue-4821",
		Subject:          "Add SSO to the customer portal",
		InputBrief:       "We need SAML for our enterprise customers.\n\nToday they sign in with a password.",
		OutcomeBrief:     "SSO ships for SAML identity providers; SCIM is not included.",
		State:            domain.RequestStatusRequesterAccepted,
		ClaimUserID:      &user,
		ClaimedAt:        at(1),
		OwnerUserID:      &otherUser,
		RejectReason:     "",
		NeedsInfoSince:   at(2),
		PlannedAt:        at(3),
		DeliveredAt:      at(4),
		AcceptedAt:       at(5),
		AcceptedBy:       &user,
		ClosedAt:         at(6),
		ReopenedAt:       at(7),
		ReopenedCount:    3,
		Rev:              7,
		// JSONB numbers come back as float64, so the expectation is float64 too.
		Metadata:  domain.Metadata{"integration": "github", "issue": float64(4821), "vip": true},
		CreatedAt: base,
		UpdatedAt: base.Add(9 * time.Minute),
	}
	full.ID = f.insertRequest(full)

	duplicate := &domain.TeamRequest{
		PublicRef:        f.ref("rowsduplicate00"),
		OrgID:            f.org,
		TeamID:           f.team,
		RequesterUserID:  &otherUser,
		RequesterContact: "another@rows.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "Same thing again",
		State:            domain.RequestStatusDuplicate,
		DuplicateOf:      &full.ID,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        base.Add(10 * time.Minute),
		UpdatedAt:        base.Add(10 * time.Minute),
	}
	duplicate.ID = f.insertRequest(duplicate)

	bare := &domain.TeamRequest{
		PublicRef:        f.ref("rowsbarerequest0"),
		OrgID:            f.org,
		TeamID:           f.team2,
		RequesterContact: "service@rows.test",
		SourceKind:       domain.RequestSourceAPI,
		Subject:          "Just landed",
		State:            domain.RequestStatusNew,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        base.Add(11 * time.Minute),
		UpdatedAt:        base.Add(11 * time.Minute),
	}
	bare.ID = f.insertRequest(bare)

	for _, want := range []*domain.TeamRequest{full, duplicate, bare} {
		got, err := f.r.GetRequest(f.ctx, want.ID, want.OrgID, want.TeamID)
		if err != nil {
			t.Fatalf("GetRequest(%d): %v", want.ID, err)
		}
		assertRequestEqual(t, fmt.Sprintf("request %d", want.ID), got, want)
	}
}

// TestRequestRowColumnsMatchSchema is the guard behind requestCols: the list the
// scanner uses must be exactly the table's columns, no more and no fewer. A
// migration that adds a column to team_requests and forgets request_rows.go
// fails here, which is the whole point of having one list.
func TestRequestRowColumnsMatchSchema(t *testing.T) {
	f := newRequestRowsFixture(t)
	want := strings.Split(strings.ReplaceAll(requestCols, "\n", " "), ",")
	wantSet := map[string]bool{}
	for _, c := range want {
		name := strings.TrimSpace(c)
		if name == "" {
			t.Fatalf("requestCols has an empty entry: %q", requestCols)
		}
		if wantSet[name] {
			t.Errorf("requestCols lists %q twice", name)
		}
		wantSet[name] = true
	}

	rows, err := f.r.db.Query(f.ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'team_requests'`)
	if err != nil {
		t.Fatalf("columns of team_requests: %v", err)
	}
	haveSet := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		haveSet[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns of team_requests: %v", err)
	}
	rows.Close()

	for name := range haveSet {
		if !wantSet[name] {
			t.Errorf("team_requests.%s is not in requestCols: the scanner would read the wrong column", name)
		}
	}
	for name := range wantSet {
		if !haveSet[name] {
			t.Errorf("requestCols names %q, which team_requests does not have", name)
		}
	}
	if len(wantSet) != len(haveSet) {
		t.Errorf("requestCols covers %d columns but team_requests has %d", len(wantSet), len(haveSet))
	}
	// The scan target list is positional against the column list, so the two
	// lengths are a contract too.
	if n := len(requestFields(&domain.TeamRequest{})); n != len(wantSet) {
		t.Errorf("requestFields returns %d targets for %d columns", n, len(wantSet))
	}
}

// TestGetRequestRefusesForeignTenant is criterion 2: the lookup is scoped by the
// request's tenant, and a request outside it is answered exactly like one that
// does not exist — so a handler's single 404 leaks neither existence nor the
// team a request lives in.
func TestGetRequestRefusesForeignTenant(t *testing.T) {
	f := newRequestRowsFixture(t)
	base := requestTime(-time.Hour)
	req := &domain.TeamRequest{
		PublicRef:        f.ref("rowstenantscoped"),
		OrgID:            f.org,
		TeamID:           f.team,
		RequesterContact: "someone@rows.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "In the first team",
		State:            domain.RequestStatusTriaging,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        base,
		UpdatedAt:        base,
	}
	req.ID = f.insertRequest(req)

	cases := []struct {
		name          string
		id, org, team int
		want          error
	}{
		{"its own tenant", req.ID, f.org, f.team, nil},
		{"sibling team in the same org", req.ID, f.org, f.team2, ErrNotFound},
		{"the same team number in another org", req.ID, f.other, f.team, ErrNotFound},
		{"another org's own team", req.ID, f.other, f.team2, ErrNotFound},
		{"an id that does not exist", 1 << 30, f.org, f.team, ErrNotFound},
		// An unparseable route parameter is the same 404, not a scope error: it
		// names no row, and the answer must not tell the caller its id was
		// malformed rather than missing.
		{"a request id of zero", 0, f.org, f.team, ErrNotFound},
		{"a negative request id", -5, f.org, f.team, ErrNotFound},
		{"no tenant at all", req.ID, 0, 0, ErrRequestTenantScope},
		{"only the org given", req.ID, f.org, 0, ErrRequestTenantScope},
		{"only the team given", req.ID, 0, f.team, ErrRequestTenantScope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.r.GetRequest(f.ctx, tc.id, tc.org, tc.team)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("GetRequest(%d, org %d, team %d) = (%v, %v), want %v",
						tc.id, tc.org, tc.team, got, err, tc.want)
				}
				if got != nil {
					t.Fatalf("GetRequest returned a request with %v: a foreign tenant must load nothing", err)
				}
				// A tenant-scope mistake is not a missing row: the two must stay
				// distinguishable, or a caller cannot tell its own bug from a 404.
				if tc.want == ErrRequestTenantScope && errors.Is(err, ErrNotFound) {
					t.Fatalf("a missing tenant also matched ErrNotFound: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetRequest: %v", err)
			}
			if got == nil || got.ID != req.ID {
				t.Fatalf("GetRequest = %+v, want request %d", got, req.ID)
			}
		})
	}
}

// TestGetRequestLoadErrorIsNotNotFound is criterion 3: a load failure keeps its
// own identity. The cancelled context is a real failure of the statement, not a
// missing row, and a handler that mapped it to 404 would report a database
// problem as "no such request" — the exact confusion the two errors exist to
// prevent.
func TestGetRequestLoadErrorIsNotNotFound(t *testing.T) {
	f := newRequestRowsFixture(t)
	base := requestTime(-time.Hour)
	req := &domain.TeamRequest{
		PublicRef:        f.ref("rowsloaderror000"),
		OrgID:            f.org,
		TeamID:           f.team,
		RequesterContact: "someone@rows.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "Will fail to load",
		State:            domain.RequestStatusNew,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        base,
		UpdatedAt:        base,
	}
	req.ID = f.insertRequest(req)

	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	got, err := f.r.GetRequest(ctx, req.ID, f.org, f.team)
	if err == nil {
		t.Fatalf("GetRequest on a cancelled context returned %+v, want an error", got)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("a load failure matched ErrNotFound: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a load failure lost its cause: %v (errors.Is(context.Canceled) = false)", err)
	}

	// The same row still loads, so the failure above was the context and not the
	// row: the two answers differ for the same id.
	if _, err := f.r.GetRequest(f.ctx, req.ID, f.org, f.team); err != nil {
		t.Fatalf("GetRequest after the cancelled call: %v", err)
	}
}

// TestGetRequestInsideTransaction proves the scan contract is reusable where the
// later slices need it: a claim or a versioned update loads the row it is about
// to change inside its own transaction, after locking it. getRequest takes the
// repository's queryRower (the auditchain.go pattern), which pgx.Tx satisfies,
// so those callers reuse one scanner instead of copying the column list.
func TestGetRequestInsideTransaction(t *testing.T) {
	f := newRequestRowsFixture(t)
	base := requestTime(-time.Hour)
	req := &domain.TeamRequest{
		PublicRef:        f.ref("rowsintransactio"),
		OrgID:            f.org,
		TeamID:           f.team,
		RequesterContact: "someone@rows.test",
		SourceKind:       domain.RequestSourceMCP,
		Subject:          "Locked and loaded",
		State:            domain.RequestStatusTriaging,
		Rev:              2,
		Metadata:         domain.Metadata{},
		CreatedAt:        base,
		UpdatedAt:        base,
	}
	req.ID = f.insertRequest(req)

	tx, err := f.r.db.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	got, err := getRequest(f.ctx, tx, req.ID, f.org, f.team)
	if err != nil {
		t.Fatalf("getRequest in a transaction: %v", err)
	}
	assertRequestEqual(t, "request in transaction", got, req)

	// A locked read of the same row also works, which is the shape the claim
	// takes: the predicate is the shared constant, so the lock cannot be scoped
	// differently from the read.
	locked := &domain.TeamRequest{}
	if err := scanRequest(tx.QueryRow(f.ctx, requestSelect+requestTenantWhere+` FOR UPDATE`,
		req.ID, f.org, f.team), locked); err != nil {
		t.Fatalf("locked read: %v", err)
	}
	assertRequestEqual(t, "locked request", locked, req)
	if err := tx.Commit(f.ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("commit: %v", err)
	}
}
