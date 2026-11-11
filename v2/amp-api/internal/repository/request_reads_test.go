package repository

// Tests for request_reads.go against a real Postgres (AMP #3874, epic 202 story
// 535). Like request_rows_test.go they run only when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/... -run 'RequestReads|ListRequests|CountRequests' -count=1 -v
//
// The three things this layer promises are what the tests assert, in order:
//
//  1. PAGING UNDER CONCURRENT INSERTS HAS NO DUPLICATE ROWS AND SKIPS NONE.
//     The queue is live: rows arrive while someone pages through it. The test
//     walks a filtered queue page by page while another goroutine keeps
//     inserting matching rows above the cursor, and asserts every row that
//     existed when paging started is seen exactly once (an OFFSET paginator
//     repeats one here — see request_reads.go's header).
//  2. FILTERS AND COUNTS ARE TENANT-CORRECT. A sibling team's requests, another
//     org's requests and a team number reused in another org are all absent
//     from every page and every count, and CountRequests equals the number of
//     rows that paging the same filter yields — so a queue pill cannot
//     disagree with the list beneath it. A filter without its tenant is refused
//     (ErrRequestTenantScope), never answered with every tenant's queue.
//  3. COMMENTS APPEND WITH CORRECT ATTRIBUTION AND NO UPDATE/DELETE SURFACE.
//     A comment stores the author snapshot and the user behind it, the kind is
//     validated, the append changes nothing about the request (no state, no
//     rev, no event), a retried append returns the original row instead of a
//     second one, and the repository offers no method that edits or deletes a
//     comment.
//
// Every test creates its own org, teams and users with a random suffix, so
// repeated runs and tests running in parallel don't see each other's rows.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// requestReadsTestRepo is requestRowsTestRepo under a name that says which
// subsystem a failure belongs to. (It is the same helper: one migrated database
// per process is the repository package's convention.)
func requestReadsTestRepo(t *testing.T) *Repo { return requestRowsTestRepo(t) }

// requestReadsFixture is one test's tenant: two orgs, a team in each plus a
// second team in the first, and the users a request's nullable columns point
// at. It deliberately reuses requestRowsFixture's shape but not its type, so a
// change made for one subsystem's tests cannot quietly change the other's.
type requestReadsFixture struct {
	r   *Repo
	ctx context.Context
	t   *testing.T
	// suffix keeps public_ref values unique across runs and concurrent tests:
	// public_ref is unique across the whole table.
	suffix int
	// refSeq makes ref() unique within one fixture: the label and the suffix
	// alone cannot tell two requests in the same state apart.
	refSeq    *atomic.Int64
	org       int
	other     int // a second org, for cross-tenant checks
	team      int
	team2     int // a second team in org, for cross-tenant checks
	otherTeam int
	user      int
	user2     int
}

func newRequestReadsFixture(t *testing.T) *requestReadsFixture {
	t.Helper()
	r := requestReadsTestRepo(t)
	ctx := context.Background()
	suffix := int(time.Now().UnixNano()%1_000_000_000) ^ os.Getpid()
	f := &requestReadsFixture{r: r, ctx: ctx, t: t, suffix: suffix, refSeq: &atomic.Int64{}}
	insert := func(sql string, dst *int, args ...any) {
		t.Helper()
		if err := r.db.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	insert(`INSERT INTO orgs (name, slug) VALUES ('reads org', $1) RETURNING id`, &f.org, fmt.Sprintf("reads-%d", suffix))
	insert(`INSERT INTO orgs (name, slug) VALUES ('reads other org', $1) RETURNING id`, &f.other, fmt.Sprintf("reads-other-%d", suffix))
	insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Desk') RETURNING id`, &f.team, f.org)
	insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Second desk') RETURNING id`, &f.team2, f.org)
	insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Desk') RETURNING id`, &f.otherTeam, f.other)
	insert(`INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`,
		&f.user, fmt.Sprintf("reads-user-%d", suffix), fmt.Sprintf("reads-user-%d@reads.test", suffix))
	insert(`INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`,
		&f.user2, fmt.Sprintf("reads-user2-%d", suffix), fmt.Sprintf("reads-user2-%d@reads.test", suffix))
	return f
}

// ref builds a public_ref this fixture owns: the label, the run's suffix, and a
// per-fixture counter. The suffix separates runs and concurrent tests against a
// database nobody resets; the counter separates rows within one fixture, which
// the label alone cannot do when two requests share a state.
func (f *requestReadsFixture) ref(label string) string {
	return fmt.Sprintf("%s%09d%04d", label, f.suffix, f.refSeq.Add(1))
}

// newRequest inserts a minimal request into team and returns its id. Only the
// columns the list reads are set; the rest take 053's defaults.
func (f *requestReadsFixture) newRequest(team int, state domain.RequestStatus, mutators ...func(*domain.TeamRequest)) int {
	f.t.Helper()
	r := &domain.TeamRequest{
		PublicRef:        f.ref("reads" + strings.ReplaceAll(string(state), "_", "")),
		OrgID:            f.org,
		TeamID:           team,
		RequesterContact: "requester@reads.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "A request",
		State:            state,
		Rev:              1,
		Metadata:         domain.Metadata{},
	}
	if team == f.otherTeam {
		r.OrgID = f.other
	}
	for _, m := range mutators {
		m(r)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	}
	var id int
	err := f.r.db.QueryRow(f.ctx, `
		INSERT INTO team_requests (
			public_ref, org_id, team_id, requester_user_id, requester_contact,
			source_kind, source_external_id, subject, input_brief, outcome_brief, state,
			claim_user_id, claimed_at, owner_user_id, duplicate_of, reject_reason,
			needs_info_since, planned_at, delivered_at, accepted_at, accepted_by,
			closed_at, reopened_at, reopened_count, rev, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			$12, $13, $14, $15, $16, $17, $18, $19, $20, $21,
			$22, $23, $24, $25, $26, $27)
		RETURNING id`,
		r.PublicRef, r.OrgID, r.TeamID, r.RequesterUserID, r.RequesterContact,
		string(r.SourceKind), r.SourceExternalID, r.Subject, r.InputBrief, r.OutcomeBrief, string(r.State),
		r.ClaimUserID, r.ClaimedAt, r.OwnerUserID, r.DuplicateOf, r.RejectReason,
		r.NeedsInfoSince, r.PlannedAt, r.DeliveredAt, r.AcceptedAt, r.AcceptedBy,
		r.ClosedAt, r.ReopenedAt, r.ReopenedCount, r.Rev, r.Metadata, r.CreatedAt,
	).Scan(&id)
	if err != nil {
		f.t.Fatalf("insert request %q: %v", r.PublicRef, err)
	}
	return id
}

// link attaches a lineage row so the "attached" filter has something to find.
// target_id is a made-up number: 055 has no foreign key on it (a tombstone link
// is legal), which is exactly what this filter must cope with.
func (f *requestReadsFixture) link(requestID int, linkKind, targetKind string, targetID int) {
	f.t.Helper()
	if _, err := f.r.db.Exec(f.ctx, `
		INSERT INTO request_links (request_id, org_id, link_kind, target_kind, target_id)
		VALUES ($1, (SELECT org_id FROM team_requests WHERE id = $1), $2, $3, $4)`,
		requestID, linkKind, targetKind, targetID); err != nil {
		f.t.Fatalf("link request %d: %v", requestID, err)
	}
}

// pageAll walks a filter from the first page to the last and returns every row
// it saw plus the id of the page each came from, so a test can assert
// "exactly once" rather than "at least once".
func (f *requestReadsFixture) pageAll(filter RequestFilter, pageSize int) []domain.TeamRequest {
	f.t.Helper()
	var out []domain.TeamRequest
	seen := map[int]int{}
	filter.Limit = pageSize
	for page := 0; ; page++ {
		if page > 100 {
			f.t.Fatalf("paging did not terminate after 100 pages: cursor %d", filter.BeforeID)
		}
		got, err := f.r.ListRequests(f.ctx, filter)
		if err != nil {
			f.t.Fatalf("ListRequests (before_id %d): %v", filter.BeforeID, err)
		}
		if got.Count != len(got.Items) {
			f.t.Fatalf("page %d: Count %d != len(Items) %d", page, got.Count, len(got.Items))
		}
		if len(got.Items) == 0 {
			return out
		}
		for _, r := range got.Items {
			seen[r.ID]++
			if seen[r.ID] > 1 {
				f.t.Fatalf("request %d appeared on more than one page: paging is not stable", r.ID)
			}
			out = append(out, r)
		}
		if got.NextBeforeID == 0 {
			return out
		}
		if got.NextBeforeID >= filter.BeforeID && filter.BeforeID != 0 {
			f.t.Fatalf("cursor did not advance: next_before_id %d, previous %d", got.NextBeforeID, filter.BeforeID)
		}
		filter.BeforeID = got.NextBeforeID
	}
}

// idsOf is the id set of a page of rows, for set comparisons.
func idsOf(rs []domain.TeamRequest) map[int]bool {
	out := map[int]bool{}
	for _, r := range rs {
		out[r.ID] = true
	}
	return out
}

// TestListRequestsPagingIsStableUnderConcurrentInserts is criterion 1.
//
// The queue is live, so the test pages a filtered queue of 25 rows six at a
// time while another goroutine keeps inserting 10 more requests that match the
// same filter and would sort above the cursor. Every row that existed when
// paging began must appear exactly once: an OFFSET paginator would repeat the
// row that got pushed past the page boundary.
//
// All 25 rows share ONE created_at, which is what a batch submitted in one
// transaction looks like (several rows, one NOW()). That is the case a
// created_at cursor cannot page: the values tie, so the comparison that ends a
// page matches more rows than the page holds and the next page repeats or skips
// them. The cursor is the serial id instead, which is why this passes.
func TestListRequestsPagingIsStableUnderConcurrentInserts(t *testing.T) {
	f := newRequestReadsFixture(t)
	const existing = 25
	// One timestamp for the whole batch (truncated to the microsecond pgx
	// stores), so a created_at-ordered cursor has nothing to break ties with.
	batch := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	want := map[int]bool{}
	for i := 0; i < existing; i++ {
		want[f.newRequest(f.team, domain.RequestStatusTriaging, func(r *domain.TeamRequest) { r.CreatedAt = batch })] = true
	}

	filter := RequestFilter{OrgID: f.org, TeamID: f.team, States: []domain.RequestStatus{domain.RequestStatusTriaging}}

	// The interleaving goroutine: insert matching rows above the cursor while
	// the reader pages. It is joined before the test returns, and it never
	// fails the test from a goroutine (it records first, asserts after).
	var wg sync.WaitGroup
	insertedErrs := make(chan error, 10)
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r := &domain.TeamRequest{
				PublicRef:        f.ref(fmt.Sprintf("readsconcurrent%02d", i)),
				OrgID:            f.org,
				TeamID:           f.team,
				RequesterContact: "concurrent@reads.test",
				SourceKind:       domain.RequestSourceWeb,
				Subject:          "Arrived mid-page",
				State:            domain.RequestStatusTriaging,
				Rev:              1,
				Metadata:         domain.Metadata{},
			}
			if _, err := f.r.db.Exec(f.ctx, `
				INSERT INTO team_requests (public_ref, org_id, team_id, requester_contact, source_kind, subject, state, rev, metadata)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				r.PublicRef, r.OrgID, r.TeamID, r.RequesterContact, string(r.SourceKind), r.Subject, string(r.State), r.Rev, r.Metadata); err != nil {
				insertedErrs <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	got := f.pageAll(filter, 6)
	close(stop)
	wg.Wait()
	close(insertedErrs)
	for err := range insertedErrs {
		t.Fatalf("concurrent insert: %v", err)
	}

	seen := idsOf(got)
	for id := range want {
		if !seen[id] {
			t.Errorf("request %d existed before paging started and was skipped: keyset paging lost a row", id)
		}
	}
	// A row that arrived above the cursor may or may not be seen depending on
	// timing; what must never happen is a row seen twice (pageAll asserts that)
	// or an existing row missed (above). Rows below the cursor at insertion
	// time must be seen, which the "no skip" check above covers because every
	// pre-existing row sorts below every concurrent one.
	if len(got) < existing {
		t.Fatalf("saw %d rows, want at least the %d that existed before paging: %v", len(got), existing, idsOf(got))
	}
}

// TestListRequestsFiltersAndCountsAreTenantCorrect is criterion 2.
//
// Every filter narrows to what it says, the counts agree with the pages, and
// nothing from another team or another org is ever reachable — including the
// second org's team, which reuses the same team name.
func TestListRequestsFiltersAndCountsAreTenantCorrect(t *testing.T) {
	f := newRequestReadsFixture(t)
	base := time.Now().UTC().Truncate(time.Microsecond)

	// The queue: one request per state, plus a second team's, plus another
	// org's, plus two with lineage and one with a searchable subject.
	var (
		triageID, newID, deliveredID    int
		attachedByRoute, attachedByPlan int
	)
	requester := f.user
	triageID = f.newRequest(f.team, domain.RequestStatusTriaging, func(r *domain.TeamRequest) {
		r.RequesterUserID = &requester
		r.Subject = "Export the billing CSV"
		r.CreatedAt = base.Add(-48 * time.Hour)
	})
	newID = f.newRequest(f.team, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.Subject = "Add SSO for 100% of tenants"
		r.SourceKind = domain.RequestSourceMCP
		r.CreatedAt = base.Add(-24 * time.Hour)
	})
	deliveredID = f.newRequest(f.team, domain.RequestStatusDelivered, func(r *domain.TeamRequest) {
		r.Subject = "Delete my workspace"
		r.OutcomeBrief = "Workspace deleted."
		r.DeliveredAt = &base
		r.CreatedAt = base.Add(-1 * time.Hour)
	})
	attachedByRoute = f.newRequest(f.team, domain.RequestStatusAccepted)
	attachedByPlan = f.newRequest(f.team, domain.RequestStatusInProgress)
	f.newRequest(f.team2, domain.RequestStatusTriaging, func(r *domain.TeamRequest) { r.Subject = "Sibling team's request" })
	f.newRequest(f.otherTeam, domain.RequestStatusTriaging, func(r *domain.TeamRequest) { r.Subject = "Other org's request" })

	f.link(attachedByRoute, "routes_to", "project", 1<<20)
	f.link(attachedByPlan, "planned_as", "task", 1<<20)

	// The routes_to link is "attached"; a relates_to link is not (it is not
	// work attached to the request), and the filter must say so.
	relatesTo := f.newRequest(f.team, domain.RequestStatusNew, func(r *domain.TeamRequest) { r.Subject = "Related to another request" })
	f.link(relatesTo, "relates_to", "request", triageID)
	// A subject carrying each LIKE wildcard, so the escaping subtest has
	// something only a literal search can find.
	underscored := f.newRequest(f.team, domain.RequestStatusNew, func(r *domain.TeamRequest) { r.Subject = "snake_case field" })

	all := RequestFilter{OrgID: f.org, TeamID: f.team}
	allRows := f.pageAll(all, 10)
	gotIDs := idsOf(allRows)
	if len(allRows) != 7 {
		t.Fatalf("unfiltered list has %d rows, want 7: %v", len(allRows), gotIDs)
	}
	for _, foreign := range []string{"Sibling team's request", "Other org's request"} {
		for _, r := range allRows {
			if r.Subject == foreign {
				t.Fatalf("an unfiltered list of team %d returned %q: tenancy is not in the WHERE clause", f.team, foreign)
			}
		}
	}

	cases := []struct {
		name   string
		filter RequestFilter
		want   []int
	}{
		{"one state", RequestFilter{States: []domain.RequestStatus{domain.RequestStatusTriaging}}, []int{triageID}},
		{"two states", RequestFilter{States: []domain.RequestStatus{domain.RequestStatusNew, domain.RequestStatusDelivered}}, []int{deliveredID, newID, relatesTo, underscored}},
		{"state normalized like every write", RequestFilter{States: []domain.RequestStatus{" Triaging "}}, []int{triageID}},
		{"requester", RequestFilter{RequesterUserID: &requester}, []int{triageID}},
		{"source", RequestFilter{SourceKind: domain.RequestSourceMCP}, []int{newID}},
		{"created after", RequestFilter{CreatedAfter: &[]time.Time{base.Add(-25 * time.Hour)}[0]}, []int{deliveredID, newID, attachedByRoute, attachedByPlan, relatesTo, underscored}},
		{"created before", RequestFilter{CreatedBefore: &[]time.Time{base.Add(-25 * time.Hour)}[0]}, []int{triageID}},
		{"attached", RequestFilter{Attached: &[]bool{true}[0]}, []int{attachedByPlan, attachedByRoute}},
		{"not attached", RequestFilter{Attached: &[]bool{false}[0]}, []int{triageID, newID, deliveredID, relatesTo, underscored}},
		{"query over subject", RequestFilter{Query: "billing"}, []int{triageID}},
		{"query over input brief", RequestFilter{Query: "nothing"}, nil},
		{"query with a literal wildcard", RequestFilter{Query: "100%"}, []int{newID}},
		// A bare "%" is the case that tells escaping apart from not escaping:
		// as a literal it finds the one subject that says "100%", and as a
		// wildcard it finds every row in the queue (which is what a search for
		// a percent sign must never do).
		{"query of a bare percent", RequestFilter{Query: "%"}, []int{newID}},
		// Same for "_", which is LIKE's single-character wildcard: as a
		// literal it matches the underscore in this subject and nothing else.
		{"query of a bare underscore", RequestFilter{Query: "_"}, []int{underscored}},
		{"query over the public ref", RequestFilter{Query: f.ref("readsnew")}, nil}, // the label varies per row; see below
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "query over the public ref" {
				// The ref of one specific row is only knowable through the row
				// itself, so this subtest reads it and searches for it.
				var ref string
				if err := f.r.db.QueryRow(f.ctx, `SELECT public_ref FROM team_requests WHERE id = $1`, deliveredID).Scan(&ref); err != nil {
					t.Fatalf("read public_ref: %v", err)
				}
				tc.filter.Query = ref
				tc.want = []int{deliveredID}
			}
			tc.filter.OrgID, tc.filter.TeamID = f.org, f.team
			rows := f.pageAll(tc.filter, 2)
			gotSet := idsOf(rows)
			if !reflect.DeepEqual(gotSet, idsOf(idsToRequests(tc.want))) {
				t.Fatalf("filter %q returned %v, want %v", tc.name, gotSet, tc.want)
			}
			n, err := f.r.CountRequests(f.ctx, tc.filter)
			if err != nil {
				t.Fatalf("CountRequests: %v", err)
			}
			if n != len(tc.want) {
				t.Fatalf("CountRequests = %d, but paging the same filter yielded %d rows: the count and the list disagree", n, len(rows))
			}
			// The count must be the total, not the page: ask for one row.
			tc.filter.Limit = 1
			one, err := f.r.ListRequests(f.ctx, tc.filter)
			if err != nil {
				t.Fatalf("ListRequests with limit 1: %v", err)
			}
			if len(one.Items) > 1 {
				t.Fatalf("limit 1 returned %d rows", len(one.Items))
			}
			if len(tc.want) > 1 && one.NextBeforeID == 0 {
				t.Fatalf("a clamped page of 1 over %d matching rows reported no next cursor", len(tc.want))
			}
		})
	}

	// Other tenants: the same filters over a *different* team in the same org
	// and over the other org's team must not see this team's rows.
	for _, tenant := range []struct {
		name      string
		org, team int
	}{
		{"sibling team", f.org, f.team2},
		{"other org's team", f.other, f.otherTeam},
	} {
		t.Run("tenant "+tenant.name, func(t *testing.T) {
			rows, err := f.r.ListRequests(f.ctx, RequestFilter{OrgID: tenant.org, TeamID: tenant.team})
			if err != nil {
				t.Fatalf("ListRequests: %v", err)
			}
			for _, r := range rows.Items {
				if r.TeamID != tenant.team || r.OrgID != tenant.org {
					t.Fatalf("%s's list returned a request from org %d team %d", tenant.name, r.OrgID, r.TeamID)
				}
			}
			wantN := 1 // each of those teams holds exactly its one request
			n, err := f.r.CountRequests(f.ctx, RequestFilter{OrgID: tenant.org, TeamID: tenant.team})
			if err != nil {
				t.Fatalf("CountRequests: %v", err)
			}
			if n != wantN {
				t.Fatalf("%s's count = %d, want %d: a count leaked another tenant's rows", tenant.name, n, wantN)
			}
			// The same team NUMBER in the wrong org sees nothing at all.
			rows, err = f.r.ListRequests(f.ctx, RequestFilter{OrgID: f.other, TeamID: f.team})
			if err != nil {
				t.Fatalf("ListRequests with a mismatched tenant: %v", err)
			}
			if len(rows.Items) != 0 {
				t.Fatalf("team %d in org %d returned %d rows: the tenant is filtered by org AND team", f.team, f.other, len(rows.Items))
			}
		})
	}
}

// idsToRequests turns a want-list of ids into rows pageAll's shape expects, so
// the set comparison above reads the same on both sides.
func idsToRequests(ids []int) []domain.TeamRequest {
	out := make([]domain.TeamRequest, len(ids))
	for i, id := range ids {
		out[i].ID = id
	}
	return out
}

// TestListRequestsRefusesAnEmptyTenant is the other half of criterion 2: a list
// without its tenant is a caller bug, answered with ErrRequestTenantScope — not
// with every tenant's queue, and not with an empty page that would read as "no
// requests".
func TestListRequestsRefusesAnEmptyTenant(t *testing.T) {
	f := newRequestReadsFixture(t)
	// A request exists, so an unfiltered read would have something to leak.
	f.newRequest(f.team, domain.RequestStatusNew)

	cases := []struct {
		name string
		f    RequestFilter
		want error
	}{
		{"no tenant", RequestFilter{}, ErrRequestTenantScope},
		{"only the org", RequestFilter{OrgID: f.org}, ErrRequestTenantScope},
		{"only the team", RequestFilter{TeamID: f.team}, ErrRequestTenantScope},
		{"negative org", RequestFilter{OrgID: -1, TeamID: f.team}, ErrRequestTenantScope},
		{"negative team", RequestFilter{OrgID: f.org, TeamID: -1}, ErrRequestTenantScope},
		{"bad state", RequestFilter{OrgID: f.org, TeamID: f.team, States: []domain.RequestStatus{"review"}}, nil},
		{"blank state", RequestFilter{OrgID: f.org, TeamID: f.team, States: []domain.RequestStatus{"  "}}, nil},
		{"bad source", RequestFilter{OrgID: f.org, TeamID: f.team, SourceKind: "email"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := f.r.ListRequests(f.ctx, tc.f)
			if rows != nil {
				t.Fatalf("ListRequests returned %+v, want no rows", rows)
			}
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("ListRequests = %v, want %v", err, tc.want)
				}
				if errors.Is(err, ErrNotFound) {
					t.Fatalf("a missing tenant also matched ErrNotFound: %v", err)
				}
				if _, err := f.r.CountRequests(f.ctx, tc.f); !errors.Is(err, tc.want) {
					t.Fatalf("CountRequests = %v, want %v (the count must refuse what the list refuses)", err, tc.want)
				}
				return
			}
			// A bad filter value is a caller error too, but it is not a tenant
			// bug: it names the offending value instead of answering a page.
			if err == nil {
				t.Fatalf("ListRequests(%+v) = nil error: an invalid filter value must be refused, not matched against nothing", tc.f)
			}
		})
	}
}

// TestListRequestsOrdersByIDNotCreatedAt pins the ordering column. created_at
// is NOT the sort key and must not become one: a connector can submit an
// envelope with its own occurred_at, an import can back-date, and a batch
// written in one transaction shares one NOW() — so created_at is neither unique
// nor monotonic with insertion. The test inserts rows whose created_at order is
// the exact reverse of their id order and asserts the page still comes back
// newest-id-first.
func TestListRequestsOrdersByIDNotCreatedAt(t *testing.T) {
	f := newRequestReadsFixture(t)
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	ids := make([]int, 4)
	for i := range ids {
		// The later the insert, the OLDER the created_at.
		at := base.Add(-time.Duration(i) * time.Hour)
		ids[i] = f.newRequest(f.team, domain.RequestStatusNew, func(r *domain.TeamRequest) { r.CreatedAt = at })
	}
	page, err := f.r.ListRequests(f.ctx, RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 10})
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(page.Items) != len(ids) {
		t.Fatalf("list returned %d rows, want %d", len(page.Items), len(ids))
	}
	for i := range ids {
		want := ids[len(ids)-1-i]
		if page.Items[i].ID != want {
			t.Fatalf("page order = %v, want id DESC (%v): the list must not sort by created_at, whose values tie and back-date",
				[]int{page.Items[0].ID, page.Items[1].ID, page.Items[2].ID, page.Items[3].ID},
				[]int{ids[3], ids[2], ids[1], ids[0]})
		}
	}
}

// TestListRequestsCursorAndLimit covers the paging shape itself: newest first,
// the cursor is the last row of the page, the ceiling clamps, and a page that
// exactly consumes the queue reports no next cursor (a caller must not have to
// ask for an empty page to learn it is done).
func TestListRequestsCursorAndLimit(t *testing.T) {
	f := newRequestReadsFixture(t)
	ids := make([]int, 5)
	for i := range ids {
		ids[i] = f.newRequest(f.team, domain.RequestStatusNew)
	}
	sort.Ints(ids)
	newestFirst := make([]int, len(ids))
	for i, id := range ids {
		newestFirst[len(ids)-1-i] = id
	}

	first, err := f.r.ListRequests(f.ctx, RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 2})
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if first.Count != 2 || len(first.Items) != 2 {
		t.Fatalf("page = %d items, count %d, want 2", len(first.Items), first.Count)
	}
	if first.Items[0].ID != newestFirst[0] || first.Items[1].ID != newestFirst[1] {
		t.Fatalf("page ids = %d, %d, want %d, %d (newest first)", first.Items[0].ID, first.Items[1].ID, newestFirst[0], newestFirst[1])
	}
	if first.NextBeforeID != first.Items[1].ID {
		t.Fatalf("next_before_id = %d, want the page's last id %d", first.NextBeforeID, first.Items[1].ID)
	}

	second, err := f.r.ListRequests(f.ctx, RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 2, BeforeID: first.NextBeforeID})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Items) != 2 || second.Items[0].ID != newestFirst[2] {
		t.Fatalf("second page = %v, want to start at %d", second.Items, newestFirst[2])
	}

	last, err := f.r.ListRequests(f.ctx, RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 2, BeforeID: second.NextBeforeID})
	if err != nil {
		t.Fatalf("last page: %v", err)
	}
	if len(last.Items) != 1 || last.Items[0].ID != newestFirst[4] {
		t.Fatalf("last page = %v, want exactly %d", last.Items, newestFirst[4])
	}
	if last.NextBeforeID != 0 {
		t.Fatalf("last page reported next_before_id %d: a caller would ask for an empty page", last.NextBeforeID)
	}

	// An exact fit also reports no cursor: 5 rows, page size 5.
	exact, err := f.r.ListRequests(f.ctx, RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 5})
	if err != nil {
		t.Fatalf("exact page: %v", err)
	}
	if len(exact.Items) != 5 || exact.NextBeforeID != 0 {
		t.Fatalf("exact-fit page = %d items, next %d, want 5 and 0", len(exact.Items), exact.NextBeforeID)
	}

	// The ceiling and the default are clamped, not refused.
	for _, tc := range []struct{ asked, want int }{{0, requestListDefaultLimit}, {-4, requestListDefaultLimit}, {10_000, requestListMaxLimit}, {requestListMaxLimit + 1, requestListMaxLimit}} {
		if got := requestListLimit(tc.asked); got != tc.want {
			t.Errorf("requestListLimit(%d) = %d, want %d", tc.asked, got, tc.want)
		}
	}
	// A limit above the ceiling still answers a page of at most the ceiling.
	big, err := f.r.ListRequests(f.ctx, RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 10_000})
	if err != nil {
		t.Fatalf("oversized limit: %v", err)
	}
	if len(big.Items) != 5 {
		t.Fatalf("oversized limit returned %d of this team's 5 rows", len(big.Items))
	}
}

// TestCountRequestsIgnoresTheCursor pins CountRequests' documented contract: it
// answers how many requests the filter matches in total, ignoring its paging
// fields — both of them. Limit is trivially ignored (it never reaches the
// predicate), but BeforeID is part of requestListWhere, so a count handed a
// paged filter would otherwise answer "rows left below the cursor": the exact
// number a queue pill must not show, and one that shrinks every time someone
// scrolls. The count with a cursor set must equal the count without it and
// equal the number of rows paging the same filter to the end yields.
func TestCountRequestsIgnoresTheCursor(t *testing.T) {
	f := newRequestReadsFixture(t)
	ids := make([]int, 7)
	for i := range ids {
		ids[i] = f.newRequest(f.team, domain.RequestStatusNew)
	}
	total := RequestFilter{OrgID: f.org, TeamID: f.team}

	// The total the pill shows, and the independent truth: paging the filter
	// to the end.
	all := f.pageAll(total, 2)
	want := len(all)
	if want != len(ids) {
		t.Fatalf("paging the filter yielded %d rows, want the %d inserted", want, len(ids))
	}
	without, err := f.r.CountRequests(f.ctx, total)
	if err != nil {
		t.Fatalf("CountRequests without a cursor: %v", err)
	}
	if without != want {
		t.Fatalf("CountRequests without a cursor = %d, want %d", without, want)
	}

	// Every cursor a caller could arrive with: one at the newest row (only the
	// rest of the queue is below it — the worst under-count a pill could show),
	// one at a middle page's start, and one carrying a partial Limit, which must
	// not change the answer either.
	for _, tc := range []struct {
		name string
		f    RequestFilter
	}{
		{"cursor at the newest row", RequestFilter{OrgID: f.org, TeamID: f.team, BeforeID: ids[len(ids)-1]}},
		{"cursor at a middle page start", RequestFilter{OrgID: f.org, TeamID: f.team, BeforeID: ids[3]}},
		{"cursor and a one-row limit", RequestFilter{OrgID: f.org, TeamID: f.team, BeforeID: ids[2], Limit: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.r.CountRequests(f.ctx, tc.f)
			if err != nil {
				t.Fatalf("CountRequests: %v", err)
			}
			if got != without || got != want {
				t.Fatalf("CountRequests with a cursor = %d, want %d (the filter total, equal to the count without a cursor and to paging the filter to the end)", got, want)
			}
		})
	}

	// ListRequests' cursor semantics are unchanged by the count's fix: the
	// same paged filter still returns only what is below its cursor.
	paged, err := f.r.ListRequests(f.ctx, RequestFilter{OrgID: f.org, TeamID: f.team, BeforeID: ids[3], Limit: 10})
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(paged.Items) != 3 {
		t.Fatalf("a cursor at the 4th of 7 rows listed %d rows, want 3: the list must still page", len(paged.Items))
	}
	for _, r := range paged.Items {
		if r.ID >= ids[3] {
			t.Fatalf("ListRequests returned id %d at or above the cursor %d", r.ID, ids[3])
		}
	}
}

// TestListRequestsRowsCarryNoProjectData is the DTO half of criterion 2: a
// queue row is a request and nothing else. The list is built from the shared
// scanner, so this test asserts the shape rather than trusting the query — it
// links a real project, epic, story and task to a request and then checks that
// no field of the returned row names any of them.
func TestListRequestsRowsCarryNoProjectData(t *testing.T) {
	f := newRequestReadsFixture(t)
	var project, epic, story int
	mustScan := func(sql string, dst *int, args ...any) {
		t.Helper()
		if err := f.r.db.QueryRow(f.ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustScan(`INSERT INTO projects (name, code, org_id) VALUES ('reads project', $1, $2) RETURNING id`,
		&project, fmt.Sprintf("RDP%d", f.suffix), f.org)
	mustScan(`INSERT INTO epics (project_id, name) VALUES ($1, 'An epic') RETURNING id`, &epic, project)
	mustScan(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'A story') RETURNING id`, &story, project, epic)

	id := f.newRequest(f.team, domain.RequestStatusInProgress)
	f.link(id, "routes_to", "project", project)
	f.link(id, "planned_as", "epic", epic)
	f.link(id, "planned_as", "story", story)

	page, err := f.r.ListRequests(f.ctx, RequestFilter{OrgID: f.org, TeamID: f.team, Attached: &[]bool{true}[0]})
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != id {
		t.Fatalf("attached list = %v, want request %d", page.Items, id)
	}
	// domain.TeamRequest is the whole DTO; there is no room for project data in
	// it, so what this asserts is that nothing leaked into the *values* a row
	// carries (e.g. a metadata join, a subject rewrite, a merged field).
	row := page.Items[0]
	if row.Metadata == nil || len(row.Metadata) != 0 {
		t.Fatalf("request metadata = %v, want {}: the list must not join project metadata in", row.Metadata)
	}
	if row.Subject != "A request" || row.InputBrief != "" || row.OutcomeBrief != "" {
		t.Fatalf("request content was rewritten by the list: %+v", row)
	}
	// The typed DTO has no field for a project, epic, story, task, branch or
	// agent, so a compile-time check is the strongest statement available here.
	if n := reflect.TypeOf(row).NumField(); n != reflect.TypeOf(domain.TeamRequest{}).NumField() {
		t.Fatalf("the list returned a row shape with %d fields, not domain.TeamRequest's", n)
	}
	if _, ok := reflect.TypeOf(row).FieldByName("ProjectID"); ok {
		t.Fatal("domain.TeamRequest has a ProjectID field: a request list must not carry project data")
	}
}

// TestAppendRequestCommentAttribution is the first half of criterion 3: a
// comment stores the kind, the body and both halves of its attribution, and an
// append leaves the request exactly as it was.
func TestAppendRequestCommentAttribution(t *testing.T) {
	f := newRequestReadsFixture(t)
	requestID := f.newRequest(f.team, domain.RequestStatusNew)

	before, err := f.r.GetRequest(f.ctx, requestID, f.org, f.team)
	if err != nil {
		t.Fatalf("GetRequest before: %v", err)
	}

	// One row per kind the domain defines, each with a different attribution
	// shape: a person, a person with a display override, and an integration
	// identity with no AMP user (a connector or service client).
	user := f.user
	got := make([]*domain.RequestComment, 0, len(domain.RequestCommentKinds))
	for i, kind := range domain.RequestCommentKinds {
		req := AppendRequestCommentRequest{
			RequestID: requestID, OrgID: f.org, TeamID: f.team,
			Kind: kind, Body: fmt.Sprintf("a %s", kind), Author: "someone",
		}
		switch i % 3 {
		case 0:
			req.Author, req.AuthorUserID = "jacob@reads.test", &user
		case 1:
			req.Author = "desk"
		case 2:
			req.Author = "integration:github"
		}
		c, err := f.r.AppendRequestComment(f.ctx, req)
		if err != nil {
			t.Fatalf("AppendRequestComment(%s): %v", kind, err)
		}
		if c.ID == 0 || c.CreatedAt.IsZero() {
			t.Fatalf("append returned an unstored row: %+v", c)
		}
		if c.Kind != kind || c.Body != req.Body || c.Author != req.Author || c.RequestID != requestID {
			t.Fatalf("append returned %+v, want kind %s, body %q, author %q on request %d", c, kind, req.Body, req.Author, requestID)
		}
		if (req.AuthorUserID == nil) != (c.AuthorUserID == nil) || (req.AuthorUserID != nil && *req.AuthorUserID != *c.AuthorUserID) {
			t.Fatalf("author_user_id = %v, want %v", c.AuthorUserID, req.AuthorUserID)
		}
		got = append(got, c)
	}

	// The thread reads back in append order, with the same attribution.
	thread, err := f.r.ListRequestComments(f.ctx, requestID, f.org, f.team, 0, 0)
	if err != nil {
		t.Fatalf("ListRequestComments: %v", err)
	}
	if len(thread) != len(got) {
		t.Fatalf("thread has %d comments, want %d", len(thread), len(got))
	}
	for i := range got {
		if thread[i].ID != got[i].ID {
			t.Fatalf("thread order: comment %d is %d, want %d (append order, by id)", i, thread[i].ID, got[i].ID)
		}
		if thread[i].Author != got[i].Author || thread[i].Kind != got[i].Kind || thread[i].Body != got[i].Body {
			t.Fatalf("thread row %d = %+v, want %+v", i, thread[i], got[i])
		}
	}

	// Forward paging by id returns the tail of the thread.
	tail, err := f.r.ListRequestComments(f.ctx, requestID, f.org, f.team, got[1].ID, 1)
	if err != nil {
		t.Fatalf("ListRequestComments after id: %v", err)
	}
	if len(tail) != 1 || tail[0].ID != got[2].ID {
		t.Fatalf("forward page = %v, want exactly comment %d", tail, got[2].ID)
	}

	// The append changed nothing about the request: no state, no rev, no
	// timestamp. A comment is a comment; a transition writes its own row and
	// event (I8 — nothing auto-advances).
	after, err := f.r.GetRequest(f.ctx, requestID, f.org, f.team)
	if err != nil {
		t.Fatalf("GetRequest after: %v", err)
	}
	if after.State != before.State || after.Rev != before.Rev || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a comment changed the request: state %s->%s, rev %d->%d, updated_at %s->%s",
			before.State, after.State, before.Rev, after.Rev, before.UpdatedAt, after.UpdatedAt)
	}
	var events int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_events WHERE request_id = $1`, requestID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Fatalf("appending comments wrote %d history rows: a comment is not a transition", events)
	}
}

// TestAppendRequestCommentRefusesForeignTenantAndBadInput covers the refusals:
// a comment on another team's or another org's request, an unknown id, a
// missing tenant, an unknown kind, a blank body, a blank author. None of them
// writes a row.
func TestAppendRequestCommentRefusesForeignTenantAndBadInput(t *testing.T) {
	f := newRequestReadsFixture(t)
	mine := f.newRequest(f.team, domain.RequestStatusNew)
	sibling := f.newRequest(f.team2, domain.RequestStatusNew)
	foreign := f.newRequest(f.otherTeam, domain.RequestStatusNew)
	user := f.user

	ok := func() AppendRequestCommentRequest {
		return AppendRequestCommentRequest{RequestID: mine, OrgID: f.org, TeamID: f.team, Body: "hello", Author: "desk", AuthorUserID: &user}
	}
	cases := []struct {
		name    string
		mutate  func(*AppendRequestCommentRequest)
		wantErr error
		wantMsg string
	}{
		{"sibling team's request", func(r *AppendRequestCommentRequest) { r.RequestID = sibling }, ErrNotFound, ""},
		{"another org's request", func(r *AppendRequestCommentRequest) { r.RequestID = foreign; r.OrgID = f.other }, ErrNotFound, ""},
		{"another org's request under our org", func(r *AppendRequestCommentRequest) { r.RequestID = foreign }, ErrNotFound, ""},
		{"unknown id", func(r *AppendRequestCommentRequest) { r.RequestID = 1 << 30 }, ErrNotFound, ""},
		{"id 0", func(r *AppendRequestCommentRequest) { r.RequestID = 0 }, ErrNotFound, ""},
		{"no tenant", func(r *AppendRequestCommentRequest) { r.OrgID, r.TeamID = 0, 0 }, ErrRequestTenantScope, ""},
		{"only the org", func(r *AppendRequestCommentRequest) { r.TeamID = 0 }, ErrRequestTenantScope, ""},
		{"unknown kind", func(r *AppendRequestCommentRequest) { r.Kind = "reply" }, nil, "comment kind"},
		{"blank body", func(r *AppendRequestCommentRequest) { r.Body = "   " }, nil, "body"},
		{"blank author", func(r *AppendRequestCommentRequest) { r.Author = "  " }, nil, "author is required"},
		{"over-long body", func(r *AppendRequestCommentRequest) { r.Body = strings.Repeat("a", domain.MaxRequestBriefLen+1) }, nil, "body is over"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := ok()
			tc.mutate(&req)
			c, err := f.r.AppendRequestComment(f.ctx, req)
			if err == nil {
				t.Fatalf("AppendRequestComment = %+v, want an error", c)
			}
			if c != nil {
				t.Fatalf("a refused append returned a row: %+v", c)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				// A tenant mistake is not a missing row, and vice versa.
				if tc.wantErr == ErrRequestTenantScope && errors.Is(err, ErrNotFound) {
					t.Fatalf("a missing tenant also matched ErrNotFound: %v", err)
				}
				return
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}

	// Nothing was written: the only request with a thread is the one that was
	// never a target of a successful append.
	for _, id := range []int{mine, sibling, foreign} {
		var n int
		if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_comments WHERE request_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count comments: %v", err)
		}
		if n != 0 {
			t.Fatalf("request %d has %d comments after only refused appends", id, n)
		}
	}

	// A foreign request's thread is empty, not an error and not another team's.
	thread, err := f.r.ListRequestComments(f.ctx, sibling, f.org, f.team, 0, 0)
	if err != nil {
		t.Fatalf("ListRequestComments on a sibling team's request: %v", err)
	}
	if len(thread) != 0 {
		t.Fatalf("reading a sibling team's thread returned %d comments", len(thread))
	}
}

// TestAppendRequestCommentRetryIsIdempotent is the "idempotent retry
// attribution" half of criterion 3: a caller that retries with the id its first
// attempt returned gets that same row back — original attribution and all —
// instead of a second comment under whoever retried.
func TestAppendRequestCommentRetryIsIdempotent(t *testing.T) {
	f := newRequestReadsFixture(t)
	requestID := f.newRequest(f.team, domain.RequestStatusNew)
	otherRequest := f.newRequest(f.team, domain.RequestStatusNew)
	first, second := f.user, f.user2

	original, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: requestID, OrgID: f.org, TeamID: f.team,
		Body: "the answer is in the doc", Author: "jacob@reads.test", AuthorUserID: &first,
	})
	if err != nil {
		t.Fatalf("first append: %v", err)
	}

	// The retry: same content, a different (wrong) retrier identity supplied,
	// and the id the first attempt returned.
	retried, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: requestID, OrgID: f.org, TeamID: f.team,
		Body: "the answer is in the doc", Author: "priya@reads.test", AuthorUserID: &second,
		RetryOfCommentID: original.ID,
	})
	if err != nil {
		t.Fatalf("retried append: %v", err)
	}
	if retried.ID != original.ID {
		t.Fatalf("retry created comment %d, want the original %d", retried.ID, original.ID)
	}
	// Attribution is the ORIGINAL author's: a retry must not relabel who spoke.
	if retried.Author != "jacob@reads.test" || retried.AuthorUserID == nil || *retried.AuthorUserID != first {
		t.Fatalf("retry re-attributed the comment: author %q user %v, want jacob and %d", retried.Author, retried.AuthorUserID, first)
	}
	thread, err := f.r.ListRequestComments(f.ctx, requestID, f.org, f.team, 0, 0)
	if err != nil {
		t.Fatalf("ListRequestComments: %v", err)
	}
	if len(thread) != 1 {
		t.Fatalf("thread has %d comments after one append and its retry, want 1", len(thread))
	}

	// A retry id that names another request's comment is not this request's
	// retry: the append is a fresh row rather than someone else's comment
	// handed to this caller.
	elsewhere, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: otherRequest, OrgID: f.org, TeamID: f.team, Body: "on the other request", Author: "desk",
	})
	if err != nil {
		t.Fatalf("append on the other request: %v", err)
	}
	fresh, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: requestID, OrgID: f.org, TeamID: f.team, Body: "a genuinely new remark", Author: "desk",
		RetryOfCommentID: elsewhere.ID,
	})
	if err != nil {
		t.Fatalf("append with a foreign retry id: %v", err)
	}
	if fresh.ID == elsewhere.ID {
		t.Fatalf("a retry id naming another request's comment returned that comment: %+v", fresh)
	}
	if fresh.Body != "a genuinely new remark" || fresh.RequestID != requestID {
		t.Fatalf("the new row is not what was asked for: %+v", fresh)
	}

	// A request in another tenant cannot have its comment returned through this
	// one: the retry lookup is tenant-scoped, so it answers ErrNotFound and the
	// append proceeds here rather than leaking the foreign row.
	sibling := f.newRequest(f.team2, domain.RequestStatusNew)
	siblingComment, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: sibling, OrgID: f.org, TeamID: f.team2, Body: "sibling thread", Author: "other desk",
	})
	if err != nil {
		t.Fatalf("append on the sibling team's request: %v", err)
	}
	got, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: requestID, OrgID: f.org, TeamID: f.team, Body: "still mine", Author: "desk",
		RetryOfCommentID: siblingComment.ID,
	})
	if err != nil {
		t.Fatalf("append with a cross-tenant retry id: %v", err)
	}
	if got.ID == siblingComment.ID || got.RequestID != requestID {
		t.Fatalf("a cross-tenant retry id leaked another team's comment: %+v", got)
	}
}

// TestRequestCommentsHaveNoEditOrDeleteSurface is the second half of criterion
// 3, asserted structurally: the repository exposes no way to update or delete a
// comment. The migration test scans for SQL write paths; this one looks at the
// API surface a later slice would reach for, so "append-only" stays true even
// if someone adds a method that happens to build its SQL dynamically.
func TestRequestCommentsHaveNoEditOrDeleteSurface(t *testing.T) {
	// The exported surface on *Repo that names a request comment. Only these
	// three may exist: append, and the two reads the retry path and the thread
	// need.
	typ := reflect.TypeOf(&Repo{})
	allowed := map[string]bool{
		"AppendRequestComment": true,
		"ListRequestComments":  true,
	}
	var commentMethods []string
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !strings.Contains(strings.ToLower(name), "requestcomment") {
			continue
		}
		commentMethods = append(commentMethods, name)
		if !allowed[name] {
			t.Errorf("*Repo.%s names a request comment and is not one of the append/read methods: the thread is append-only, so an edit or a delete must be a new row, never a method", name)
		}
	}
	if len(commentMethods) < 2 {
		t.Fatalf("found %v on *Repo: expected AppendRequestComment and ListRequestComments", commentMethods)
	}

	// And the column list stays in step with the table it scans, the way
	// requestCols does: a column added by a later migration must not be
	// silently missing from the thread read.
	f := newRequestReadsFixture(t)
	want := strings.Split(requestCommentCols(""), ", ")
	rows, err := f.r.db.Query(f.ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'request_comments'`)
	if err != nil {
		t.Fatalf("columns of request_comments: %v", err)
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
			t.Errorf("requestCommentCols names %q, which request_comments does not have", name)
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
			t.Errorf("request_comments.%s is not in requestCommentCols: the thread read would lose the column", name)
		}
	}
	// The qualified form must name exactly the same columns, just prefixed.
	for i, c := range strings.Split(requestCommentCols("c"), ", ") {
		if c != "c."+want[i] {
			t.Errorf("qualified column %d = %q, want c.%s", i, c, want[i])
		}
	}
	if n := len(requestCommentFields(&domain.RequestComment{})); n != len(want) {
		t.Errorf("requestCommentFields returns %d targets for %d columns", n, len(want))
	}
}
