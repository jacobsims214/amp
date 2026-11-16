package repository

// Tests for the request thread's paged read (AMP #4270, epic 202 story 549).
//
// request_reads_test.go already covers appending a comment: the attribution, the
// validation, the retry, the append-only surface, and reading a thread back as
// rows. This file asks only the questions the PAGE adds:
//
//  1. THE CURSOR IS STABLE AND THE PAGE IS BOUNDED. Walking a thread forward by
//     after_id sees every row exactly once, in append order, whether or not rows
//     are landing above the cursor while the reader pages — and the answer says
//     next_after_id only when there really is another page, so a reader never
//     asks for an empty one to learn it is done. A limit of 0 is the default, one
//     above the ceiling is clamped, and a page with nothing after the cursor is
//     an empty slice rather than nil.
//  2. THE TENANT IS THE JOIN, NOT A POST-FILTER. A sibling team's thread and
//     another org's thread are empty pages, byte for byte the same answer an
//     unknown request gives — and a request with no comments is that same empty
//     page, so a reader cannot tell the three apart (which is the surface's one
//     404's job, not this read's).
//
// Both run only when TEST_DATABASE_URL is set, like the rest of this package:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/repository -run 'RequestComment' -count=1 -v
//
// Every fixture row carries a run-unique suffix (requestReadsFixture's), so
// repeated runs and other tests in the same database never see each other's rows.

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// commentThread appends n comments to requestID in its own tenant and returns
// the stored rows in append order. It goes through the repository's own write,
// so the ids are the ones the database minted rather than numbers this test
// guessed — and an append into the wrong tenant would be refused, which is the
// write's own guarantee, not this test's.
func commentThread(t *testing.T, f *requestReadsFixture, requestID int, n int) []*domain.RequestComment {
	t.Helper()
	org, team := f.tenantOf(requestID)
	out := make([]*domain.RequestComment, 0, n)
	for i := 0; i < n; i++ {
		c, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
			RequestID: requestID, OrgID: org, TeamID: team,
			Kind: domain.RequestCommentDiscussion, Body: fmt.Sprintf("message %d", i), Author: "someone",
		})
		if err != nil {
			t.Fatalf("append comment %d: %v", i, err)
		}
		out = append(out, c)
	}
	return out
}

// tenantOf reads one request's own (org, team) pair, so a fixture that appends
// into a foreign request's thread uses that request's tenant rather than this
// fixture's — the write is tenant-scoped, so a mismatched pair is refused and
// the read under test would then be testing a thread nobody wrote.
func (f *requestReadsFixture) tenantOf(requestID int) (orgID, teamID int) {
	f.t.Helper()
	if err := f.r.db.QueryRow(f.ctx,
		`SELECT org_id, team_id FROM team_requests WHERE id = $1`, requestID).Scan(&orgID, &teamID); err != nil {
		f.t.Fatalf("tenant of request %d: %v", requestID, err)
	}
	return orgID, teamID
}

// pageThread walks a thread forward from the beginning and returns every row it
// saw, asserting on the way that each page is at most pageSize rows, that Count
// agrees with the page, and that the cursor moves strictly forward. It is the
// reader's loop, so "the pages add up to the thread" is the property under test
// rather than something the test re-derives.
func pageThread(t *testing.T, f *requestReadsFixture, requestID, pageSize int) []domain.RequestComment {
	t.Helper()
	var out []domain.RequestComment
	seen := map[int64]bool{}
	var cursor int64
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatalf("paging did not terminate after 100 pages: cursor %d", cursor)
		}
		got, err := f.r.ListRequestCommentPage(f.ctx, requestID, f.org, f.team, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListRequestCommentPage (after %d): %v", cursor, err)
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
		for _, c := range got.Items {
			if seen[c.ID] {
				t.Fatalf("comment %d appeared on more than one page: paging is not stable", c.ID)
			}
			seen[c.ID] = true
			if c.ID <= cursor {
				t.Fatalf("comment %d arrives at or below the cursor %d: the pages are out of order", c.ID, cursor)
			}
			out = append(out, c)
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

// TestListRequestCommentPageCursorAndBounds is criterion 1: the pages add up to
// the thread, the cursor appears only when there is more, and the bounds behave.
func TestListRequestCommentPageCursorAndBounds(t *testing.T) {
	f := newRequestReadsFixture(t)
	requestID := f.newRequest(f.team, domain.RequestStatusTriaging)
	stored := commentThread(t, f, requestID, 5)

	// Small pages: every row once, in append order (which is id order).
	got := pageThread(t, f, requestID, 2)
	if len(got) != len(stored) {
		t.Fatalf("paging with limit 2 yielded %d rows, want %d", len(got), len(stored))
	}
	for i := range stored {
		if got[i].ID != stored[i].ID {
			t.Fatalf("paged row %d is comment %d, want %d (append order, by id)", i, got[i].ID, stored[i].ID)
		}
	}

	// One page exactly the size of the thread: there is no next page, so there is
	// no cursor — a reader must not be sent after a page that does not exist.
	exact, err := f.r.ListRequestCommentPage(f.ctx, requestID, f.org, f.team, 0, len(stored))
	if err != nil {
		t.Fatalf("ListRequestCommentPage (exact page): %v", err)
	}
	if exact.Count != len(stored) || exact.NextAfterID != 0 {
		t.Fatalf("a page that is exactly the thread = %+v, want %d rows and no cursor", exact, len(stored))
	}

	// One row short of the thread: there IS another page, and the cursor is the
	// last row of this one.
	short, err := f.r.ListRequestCommentPage(f.ctx, requestID, f.org, f.team, 0, len(stored)-1)
	if err != nil {
		t.Fatalf("ListRequestCommentPage (short page): %v", err)
	}
	if short.Count != len(stored)-1 {
		t.Fatalf("short page holds %d rows, want %d", short.Count, len(stored)-1)
	}
	if short.NextAfterID != stored[len(stored)-2].ID {
		t.Fatalf("next_after_id = %d, want the page's last row %d", short.NextAfterID, stored[len(stored)-2].ID)
	}

	// after_id past the end is an empty page, not an error and not nil.
	tail, err := f.r.ListRequestCommentPage(f.ctx, requestID, f.org, f.team, stored[len(stored)-1].ID, 10)
	if err != nil {
		t.Fatalf("ListRequestCommentPage (past the end): %v", err)
	}
	if tail.Items == nil || len(tail.Items) != 0 || tail.Count != 0 || tail.NextAfterID != 0 {
		t.Fatalf("a page past the end = %+v, want an empty non-nil page with no cursor", tail)
	}

	// A limit of 0 is the default (all five fit), and a limit above the ceiling
	// is clamped rather than refused: the caller gets a page and a cursor.
	for _, tc := range []struct {
		name  string
		limit int
	}{{"default", 0}, {"clamped", 10_000}} {
		page, err := f.r.ListRequestCommentPage(f.ctx, requestID, f.org, f.team, 0, tc.limit)
		if err != nil {
			t.Fatalf("%s limit: %v", tc.name, err)
		}
		if page.Count != len(stored) || page.NextAfterID != 0 {
			t.Fatalf("%s limit = %+v, want the whole 5-row thread and no cursor", tc.name, page)
		}
	}

	// The rows read back with their own fields: the paged read is the same row
	// the append returned, not a projection of it.
	if got[0].RequestID != requestID || got[0].Body != "message 0" || got[0].Kind != domain.RequestCommentDiscussion {
		t.Fatalf("paged row 0 = %+v, want the first appended comment", got[0])
	}
}

// TestListRequestCommentPageIsStableUnderConcurrentAppends is criterion 1's
// live-thread half: comments keep arriving while someone pages, and every row
// that existed when paging started is seen exactly once. A forward cursor makes
// this the easy direction — a new row sorts after the cursor, never before it —
// and the test pins that the invariant holds rather than assuming it.
func TestListRequestCommentPageIsStableUnderConcurrentAppends(t *testing.T) {
	f := newRequestReadsFixture(t)
	requestID := f.newRequest(f.team, domain.RequestStatusTriaging)
	const existing = 12
	stored := commentThread(t, f, requestID, existing)

	var wg sync.WaitGroup
	appendErrs := make(chan error, 6)
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
				RequestID: requestID, OrgID: f.org, TeamID: f.team,
				Kind: domain.RequestCommentDiscussion,
				Body: fmt.Sprintf("arrived mid-page %d", i), Author: "someone-else",
			}); err != nil {
				appendErrs <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	seen := pageThread(t, f, requestID, 4)
	close(stop)
	wg.Wait()
	close(appendErrs)
	for err := range appendErrs {
		t.Fatalf("concurrent append: %v", err)
	}

	got := map[int64]bool{}
	for _, c := range seen {
		got[c.ID] = true
	}
	for _, c := range stored {
		if !got[c.ID] {
			t.Errorf("comment %d existed before paging started and was skipped: the cursor lost a row", c.ID)
		}
	}
}

// TestListRequestCommentPageIsTenantScoped is criterion 2: the tenant ride in
// the join, so another team's thread (in this org or another) is an empty page,
// indistinguishable from a request that simply has no comments. "Does this
// request exist?" is GetRequest's question, not this read's, and it answers it
// with the surface's one 404.
func TestListRequestCommentPageIsTenantScoped(t *testing.T) {
	f := newRequestReadsFixture(t)
	mine := f.newRequest(f.team, domain.RequestStatusNew)
	sibling := f.newRequest(f.team2, domain.RequestStatusNew)
	foreign := f.newRequest(f.otherTeam, domain.RequestStatusNew)
	empty := f.newRequest(f.team, domain.RequestStatusNew)
	commentThread(t, f, mine, 2)
	commentThread(t, f, sibling, 2)
	// The foreign request gets rows of its own in its own tenant; the reads
	// below name this org and this team, which is what must hide them.
	commentThread(t, f, foreign, 2)

	type answer struct {
		count  int
		cursor int64
	}
	read := func(requestID, orgID, teamID int) answer {
		t.Helper()
		page, err := f.r.ListRequestCommentPage(f.ctx, requestID, orgID, teamID, 0, 50)
		if err != nil {
			t.Fatalf("ListRequestCommentPage(%d, %d, %d): %v", requestID, orgID, teamID, err)
		}
		return answer{count: page.Count, cursor: page.NextAfterID}
	}

	if got := read(mine, f.org, f.team); got.count != 2 {
		t.Fatalf("own thread = %+v, want 2 rows", got)
	}
	// A sibling team's request, read as this team: empty.
	if got := read(sibling, f.org, f.team); got.count != 0 || got.cursor != 0 {
		t.Fatalf("a sibling team's request = %+v, want an empty page", got)
	}
	// Another org's request, read as this org and team: empty.
	if got := read(foreign, f.org, f.team); got.count != 0 {
		t.Fatalf("another org's request = %+v, want an empty page", got)
	}
	// A request with no comments: the same empty page. The three are one answer.
	if got := read(empty, f.org, f.team); got.count != 0 || got.cursor != 0 {
		t.Fatalf("a request with no comments = %+v, want an empty page", got)
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
		if _, err := f.r.ListRequestCommentPage(f.ctx, mine, tc.orgID, tc.teamID, 0, 50); err == nil {
			t.Fatalf("%s: a thread was read without its tenant", tc.name)
		} else if !errors.Is(err, ErrRequestTenantScope) {
			t.Fatalf("%s: err = %v, want ErrRequestTenantScope", tc.name, err)
		}
	}
	if _, err := f.r.ListRequestCommentPage(f.ctx, 0, f.org, f.team, 0, 50); err == nil {
		t.Fatal("request 0 answered a thread")
	} else if !errors.Is(err, ErrNotFound) {
		t.Fatalf("request 0: err = %v, want ErrNotFound", err)
	}
}
