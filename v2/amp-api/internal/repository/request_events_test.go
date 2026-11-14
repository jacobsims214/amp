package repository

// Tests for the request history read (AMP #4271, epic 202 story 549).
//
// The writes that append request_events rows are covered where they live
// (request_create_test.go, request_claims_test.go, request_edits_test.go,
// request_links_test.go), and each of those asserts the row it wrote by reading
// the table directly. This file asks only the questions the READ adds:
//
//  1. ID ORDER IS HISTORY ORDER, AND THE PAGE IS STABLE. The rows come back in
//     append order (by id, never created_at, which several rows in one
//     transaction share), a cursor pages forward through every row exactly once
//     whether or not rows are landing while the reader pages, and the answer
//     carries next_after_id only when there really is another page. A limit of
//     0 is the default, one above the ceiling is clamped, and a page with
//     nothing after the cursor is an empty slice rather than nil.
//  2. THE TENANT IS THE REQUEST'S, ASKED FIRST. A sibling team's request, another
//     org's request and an id that never existed are all ErrNotFound — not an
//     empty page — because "not yours" and "has no acts" must not be one
//     answer. A bare id with no tenant is refused outright.
//
// Both run only when TEST_DATABASE_URL is set, like the rest of this package:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/repository -run 'RequestEvent' -count=1 -v
//
// Every fixture row carries a run-unique suffix (requestReadsFixture's), so
// repeated runs and other tests in the same database never see each other's rows.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// appendEvent writes one history row for requestID in its own tenant and
// returns the stored id. It writes the table directly rather than through one
// of the act functions: this file is about the READ's order and tenancy, and
// going through ClaimRequest would make "twelve rows, in this order" a
// twelve-step lifecycle the test would then have to keep legal. The act
// functions' own tests already prove they write these rows.
func appendEvent(t *testing.T, f *requestReadsFixture, requestID int, action domain.RequestEventAction, actor string, detail string) int64 {
	t.Helper()
	org, team := f.tenantOf(requestID)
	var id int64
	if err := f.r.db.QueryRow(f.ctx, `
		INSERT INTO request_events (request_id, org_id, team_id, action, actor, detail)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		requestID, org, team, string(action), actor, detail).Scan(&id); err != nil {
		t.Fatalf("append %s event to request %d: %v", action, requestID, err)
	}
	return id
}

// eventThread appends n `claim`/`release` rows alternately to requestID and
// returns their ids in append order.
func eventThread(t *testing.T, f *requestReadsFixture, requestID, n int) []int64 {
	t.Helper()
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		action := domain.RequestActionClaim
		if i%2 == 1 {
			action = domain.RequestActionRelease
		}
		out = append(out, appendEvent(t, f, requestID, action, fmt.Sprintf("actor-%d", i), fmt.Sprintf(`{"n":%d}`, i)))
	}
	return out
}

// pageEvents walks a history forward from its first act, asserting on the way
// that Count agrees with the page, that the cursor moves strictly forward, and
// that a page carrying a cursor is never empty.
func pageEvents(t *testing.T, f *requestReadsFixture, requestID, pageSize int) []domain.RequestEvent {
	t.Helper()
	var out []domain.RequestEvent
	seen := map[int64]bool{}
	var cursor int64
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatalf("paging did not terminate after 100 pages: cursor %d", cursor)
		}
		got, err := f.r.ListRequestEventPage(f.ctx, requestID, f.org, f.team, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListRequestEventPage (after %d): %v", cursor, err)
		}
		if got == nil {
			t.Fatalf("page %d is nil: an empty page is an answer, not a missing one", page)
		}
		if got.Count != len(got.Items) {
			t.Fatalf("page %d: Count %d != len(Items) %d", page, got.Count, len(got.Items))
		}
		if pageSize > 0 && len(got.Items) > pageSize {
			t.Fatalf("page %d holds %d rows for a limit of %d: the limit was not honored", page, len(got.Items), pageSize)
		}
		for _, e := range got.Items {
			if seen[e.ID] {
				t.Fatalf("event %d appeared on more than one page: paging is not stable", e.ID)
			}
			seen[e.ID] = true
			if e.ID <= cursor {
				t.Fatalf("event %d arrives at or below the cursor %d: the pages are out of order", e.ID, cursor)
			}
			out = append(out, e)
		}
		if got.NextAfterID == 0 {
			return out
		}
		if got.NextAfterID <= cursor {
			t.Fatalf("cursor did not advance: next_after_id %d, previous %d", got.NextAfterID, cursor)
		}
		if len(got.Items) == 0 {
			t.Fatalf("a page with no rows carries a cursor (%d): a reader would loop forever", got.NextAfterID)
		}
		cursor = got.NextAfterID
	}
}

// TestListRequestEventsOrderAndBounds is criterion 1's static half: the rows
// arrive in append order with their own fields, the cursor appears only when
// there is more, and the bounds behave.
func TestListRequestEventsOrderAndBounds(t *testing.T) {
	f := newRequestReadsFixture(t)
	requestID := f.newRequest(f.team, domain.RequestStatusTriaging)
	stored := eventThread(t, f, requestID, 5)

	// The plain read (no envelope) is the same rows, in id order.
	rows, err := f.r.ListRequestEvents(f.ctx, requestID, f.org, f.team, 0, 0)
	if err != nil {
		t.Fatalf("ListRequestEvents: %v", err)
	}
	if len(rows) != len(stored) {
		t.Fatalf("ListRequestEvents returned %d rows, want %d", len(rows), len(stored))
	}
	for i := range stored {
		if rows[i].ID != stored[i] {
			t.Fatalf("row %d is event %d, want %d (append order, by id)", i, rows[i].ID, stored[i])
		}
	}
	// The row's own fields, not a projection of them: the request, the tenant,
	// the action, the actor and the detail all come back as stored.
	first := rows[0]
	if first.RequestID != requestID || first.OrgID != f.org || first.TeamID != f.team {
		t.Fatalf("row 0 lost its identity: %+v", first)
	}
	if first.Action != domain.RequestActionClaim || first.Actor != "actor-0" || first.Detail != `{"n":0}` {
		t.Fatalf("row 0 lost its own fields: %+v", first)
	}
	if first.CreatedAt.IsZero() {
		t.Fatalf("row 0 has no stored time: %+v", first)
	}
	if rows[1].Action != domain.RequestActionRelease {
		t.Fatalf("row 1 action = %q, want release (the append order, not a re-sort)", rows[1].Action)
	}

	// Small pages: every row once, in append order.
	got := pageEvents(t, f, requestID, 2)
	if len(got) != len(stored) {
		t.Fatalf("paging with limit 2 yielded %d rows, want %d", len(got), len(stored))
	}
	for i := range stored {
		if got[i].ID != stored[i] {
			t.Fatalf("paged row %d is event %d, want %d", i, got[i].ID, stored[i])
		}
	}

	// A page exactly the size of the history: there is no next page, so there is
	// no cursor — a reader must not be sent after a page that does not exist.
	exact, err := f.r.ListRequestEventPage(f.ctx, requestID, f.org, f.team, 0, len(stored))
	if err != nil {
		t.Fatalf("exact page: %v", err)
	}
	if exact.Count != len(stored) || exact.NextAfterID != 0 {
		t.Fatalf("a page that is exactly the history = %+v, want %d rows and no cursor", exact, len(stored))
	}

	// One row short: there IS another page, and the cursor is this page's last row.
	short, err := f.r.ListRequestEventPage(f.ctx, requestID, f.org, f.team, 0, len(stored)-1)
	if err != nil {
		t.Fatalf("short page: %v", err)
	}
	if short.Count != len(stored)-1 {
		t.Fatalf("short page holds %d rows, want %d", short.Count, len(stored)-1)
	}
	if short.NextAfterID != stored[len(stored)-2] {
		t.Fatalf("next_after_id = %d, want the page's last row %d", short.NextAfterID, stored[len(stored)-2])
	}

	// after_id past the end is an empty page, not an error and not nil.
	tail, err := f.r.ListRequestEventPage(f.ctx, requestID, f.org, f.team, stored[len(stored)-1], 10)
	if err != nil {
		t.Fatalf("past the end: %v", err)
	}
	if tail.Items == nil || len(tail.Items) != 0 || tail.Count != 0 || tail.NextAfterID != 0 {
		t.Fatalf("a page past the end = %+v, want an empty non-nil page with no cursor", tail)
	}

	// A limit of 0 is the default (all five fit); a limit above the ceiling is
	// clamped rather than refused, and the page still answers the whole history.
	for _, tc := range []struct {
		name  string
		limit int
	}{{"default", 0}, {"clamped", 10_000}} {
		page, err := f.r.ListRequestEventPage(f.ctx, requestID, f.org, f.team, 0, tc.limit)
		if err != nil {
			t.Fatalf("%s limit: %v", tc.name, err)
		}
		if page.Count != len(stored) || page.NextAfterID != 0 {
			t.Fatalf("%s limit = %+v, want the whole 5-row history and no cursor", tc.name, page)
		}
	}

	// A request with no acts is an empty non-nil page: "nothing has happened yet"
	// is an answer the rail must be able to render.
	empty := f.newRequest(f.team, domain.RequestStatusNew)
	silent, err := f.r.ListRequestEventPage(f.ctx, empty, f.org, f.team, 0, 0)
	if err != nil {
		t.Fatalf("history of a request with no acts: %v", err)
	}
	if silent.Items == nil || silent.Count != 0 || silent.NextAfterID != 0 {
		t.Fatalf("a request with no acts = %+v, want an empty non-nil page", silent)
	}
}

// TestListRequestEventsIsStableUnderConcurrentAppends is criterion 1's live
// half: acts keep landing while someone pages, and every row that existed when
// paging started is seen exactly once. A forward cursor makes this the easy
// direction — a new row sorts after the cursor, never before it — and the test
// pins the invariant rather than assuming it.
func TestListRequestEventsIsStableUnderConcurrentAppends(t *testing.T) {
	f := newRequestReadsFixture(t)
	requestID := f.newRequest(f.team, domain.RequestStatusTriaging)
	const existing = 12
	stored := eventThread(t, f, requestID, existing)

	var wg sync.WaitGroup
	appendErrs := make(chan error, 6)
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		org, team := f.org, f.team
		for i := 0; i < 6; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := f.r.db.Exec(f.ctx, `
				INSERT INTO request_events (request_id, org_id, team_id, action, actor, detail)
				VALUES ($1, $2, $3, 'claim', 'arrived-mid-page', '')`,
				requestID, org, team); err != nil {
				appendErrs <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	seen := pageEvents(t, f, requestID, 4)
	close(stop)
	wg.Wait()
	close(appendErrs)
	for err := range appendErrs {
		t.Fatalf("concurrent append: %v", err)
	}

	got := map[int64]bool{}
	for _, e := range seen {
		got[e.ID] = true
	}
	for _, id := range stored {
		if !got[id] {
			t.Errorf("event %d existed before paging started and was skipped: the cursor lost a row", id)
		}
	}
}

// TestListRequestEventsIsTenantScoped is criterion 2: a request in another team
// or another org is ErrNotFound — the same answer an id that never existed
// gives — rather than an empty page. "Not yours" and "has no acts" must not be
// one answer, or a caller could not tell them apart.
func TestListRequestEventsIsTenantScoped(t *testing.T) {
	f := newRequestReadsFixture(t)
	mine := f.newRequest(f.team, domain.RequestStatusNew)
	sibling := f.newRequest(f.team2, domain.RequestStatusNew)
	foreign := f.newRequest(f.otherTeam, domain.RequestStatusNew)
	eventThread(t, f, mine, 2)
	eventThread(t, f, sibling, 2)
	eventThread(t, f, foreign, 2)

	if rows, err := f.r.ListRequestEvents(f.ctx, mine, f.org, f.team, 0, 50); err != nil || len(rows) != 2 {
		t.Fatalf("own history = %d rows, %v; want 2", len(rows), err)
	}

	// A sibling team's request, read as this team: not found, exactly as a
	// missing request answers — never an empty list.
	if _, err := f.r.ListRequestEvents(f.ctx, sibling, f.org, f.team, 0, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a sibling team's request: err = %v, want ErrNotFound (an empty page would hide the request)", err)
	}
	// Another org's request, read as this org and team: the same.
	if _, err := f.r.ListRequestEvents(f.ctx, foreign, f.org, f.team, 0, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another org's request: err = %v, want ErrNotFound", err)
	}
	// The same two answers through the paged read: the tenant check is not the
	// plain read's own.
	if _, err := f.r.ListRequestEventPage(f.ctx, sibling, f.org, f.team, 0, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a sibling team's request (paged): err = %v, want ErrNotFound", err)
	}
	// The correct tenant still reads the sibling's own history, so the refusal
	// above was the tenant's and not the request's.
	if rows, err := f.r.ListRequestEvents(f.ctx, sibling, f.org, f.team2, 0, 50); err != nil || len(rows) != 2 {
		t.Fatalf("the sibling's own history = %d rows, %v; want 2", len(rows), err)
	}
	if rows, err := f.r.ListRequestEvents(f.ctx, foreign, f.other, f.otherTeam, 0, 50); err != nil || len(rows) != 2 {
		t.Fatalf("the foreign request's own history = %d rows, %v; want 2", len(rows), err)
	}

	// The tenant is required: a missing half is refused, never answered with
	// every tenant's rows.
	for _, tc := range []struct {
		name          string
		orgID, teamID int
	}{
		{"no org", 0, f.team},
		{"no team", f.org, 0},
		{"neither", 0, 0},
	} {
		if _, err := f.r.ListRequestEvents(f.ctx, mine, tc.orgID, tc.teamID, 0, 50); !errors.Is(err, ErrRequestTenantScope) {
			t.Fatalf("%s: err = %v, want ErrRequestTenantScope", tc.name, err)
		}
	}
	// A non-positive id names no row: an ordinary missing request, not a
	// tenant-scope bug (the serial starts at 1).
	if _, err := f.r.ListRequestEvents(f.ctx, 0, f.org, f.team, 0, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("request 0: err = %v, want ErrNotFound", err)
	}
}

// TestRequestEventColumnsStayInStepWithTheTable pins the read's column list
// against the table, the way the thread read's does: a column a later migration
// adds must not be silently missing from history rows, and a column the read
// names that does not exist is a broken statement nobody would notice until it
// ran.
func TestRequestEventColumnsStayInStepWithTheTable(t *testing.T) {
	f := newRequestReadsFixture(t)
	want := splitCols(requestEventCols)
	rows, err := f.r.db.Query(f.ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'request_events'`)
	if err != nil {
		t.Fatalf("columns of request_events: %v", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		have[name] = true
	}
	rows.Close()
	for _, name := range want {
		if !have[name] {
			t.Errorf("requestEventCols names %q, which request_events does not have", name)
		}
	}
	for name := range have {
		found := false
		for _, w := range want {
			if w == name {
				found = true
			}
		}
		if !found {
			t.Errorf("request_events.%s is not in requestEventCols: the history read would lose the column", name)
		}
	}
	if n := len(requestEventFields(&domain.RequestEvent{})); n != len(want) {
		t.Errorf("requestEventFields returns %d targets for %d columns", n, len(want))
	}
}

// splitCols splits a column list that may be wrapped across lines, which
// requestEventCols is (154 characters on one line would be a worse list to
// read than one per field group).
func splitCols(list string) []string {
	var out []string
	for _, part := range strings.Split(list, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
