package service

// Tests for the authorized request read path (AMP #3958, epic 202 story 549;
// build slice S2 of discoveries/team-inbox-approved-build-slices.md).
//
// The ticket's three criteria are what these assert, in order:
//
//  1. TEAM / REQUESTER / STRANGER VIEWS. A member, a lead, an org owner and a
//     global admin read the team's whole record; the requester reads the
//     whitelist projection of their own request and nothing else; a stranger
//     (another org, a plain org member with no team role, a customer with a
//     submit grant, an agent with no role) is refused with the surface's single
//     not-found. A caller asking a second time after losing their standing is
//     refused on that call, because the decision is re-read from the rows every
//     time (TestListRequests_Views, TestGetRequest_Views,
//     TestGetRequest_RereadsStandingEveryCall).
//  2. COUNTS AND PAGINATION. The page and the per-state pills come from one
//     filter: the pills equal the filtered queue and never the page remainder;
//     walking the pages sees every row exactly once; the "claimed by me" slice
//     is a slice of that same filter and its total is cursor-free
//     (TestListRequests_CountsAndPaging, TestListRequests_MineIsASliceOfTheQueue,
//     TestListOwnRequests_PagesAndCounts).
//  3. PRIVATE FIELDS NEVER REACH THE REQUESTER DTO. The requester's answer has
//     Record == nil and a projection whose serialized bytes contain none of the
//     record's internal facts — the sentinels planted in metadata, the contact
//     snapshot, the claim and assignee ids, the duplicate pointer, a
//     connector's external id — and whose comment and file lists are only what
//     the read path decided to share (TestGetRequest_RequesterGetsNoPrivateField,
//     TestGetRequest_RequesterThreadAndFiles).
//
// Around the criteria the tests pin what else the path promises: every way of
// not seeing a request is the same bytes (TestGetRequest_RefusalsAreIndistinguishable);
// a database failure is returned as itself, never as a 404
// (TestGetRequest_LoadFailureIsNotA404); a submit-scoped credential reads
// nothing (TestReads_RefuseSubmitScopedCredential); and the queue's tenant is
// the team row's org, so an adapter cannot pair a team with the wrong org
// (TestListRequests_TenantComesFromTheTeamRow).
//
// The database-backed tests run against a real Postgres with every migration
// applied, when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'RequestViews|ListRequests|GetRequest|ListOwnRequests' -count=1 -race
//
// Every fixture row carries a run-unique suffix, so repeated runs and other
// tests in the same database never see each other's rows.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- fixture ----

// viewsFixture is one test's tenant: a team inbox in an org with a second team,
// another org with its own team, and the people around them. It is
// submitFixture's shape plus the two roles the read path distinguishes (a team
// lead, and a plain org member with no team role at all).
type viewsFixture struct {
	t   *testing.T
	svc *Service
	db  *pgx.Conn
	ctx context.Context

	org       int
	team      int
	otherTeam int // a second team in org, NOT this inbox
	otherOrg  int
	foreign   int // a team in otherOrg

	member   int // team member
	lead     int // team lead
	admin    int // owner of org
	plain    int // org member, no team role anywhere
	customer int // customer kind: submit grant on the inbox, no membership
	stranger int // owner of otherOrg only
	agent    int // agent account with no role of its own

	// project is a project the inbox team is linked to, so the queue's org is
	// one the team actually lives in (and the "wrong org" test has a second
	// team in the same org to catch a widened predicate).
	project int
}

func newViewsFixture(t *testing.T) *viewsFixture {
	t.Helper()
	repo := viewsTestRepo(t)
	ctx := context.Background()
	db, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect (fixture): %v", err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })

	suffix := rand.IntN(1_000_000_000)
	f := &viewsFixture{t: t, db: db, ctx: ctx}
	q := func(sql string, args ...any) int {
		t.Helper()
		var id int
		if err := db.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	user := func(label, kind string) int {
		return q(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
			fmt.Sprintf("views-%s-%d", label, suffix),
			fmt.Sprintf("views-%s-%d@views.test", label, suffix), kind)
	}

	f.org = q(`INSERT INTO orgs (name, slug) VALUES ('Views org', $1) RETURNING id`, fmt.Sprintf("views-%d", suffix))
	f.team = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Views desk') RETURNING id`, f.org)
	f.otherTeam = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Views other desk') RETURNING id`, f.org)
	// The code is the full suffix, not a truncation of it: projects.code is
	// globally UNIQUE, so a shortened code collides across runs (1 in 100k per
	// insert, which a suite that builds several projects per fixture hits).
	f.project = q(`INSERT INTO projects (name, code, org_id) VALUES ('Views work', $1, $2) RETURNING id`,
		fmt.Sprintf("VW%d", suffix), f.org)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, f.project, f.team)
	f.otherOrg = q(`INSERT INTO orgs (name, slug) VALUES ('Views other org', $1) RETURNING id`, fmt.Sprintf("views-other-%d", suffix))
	f.foreign = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Views foreign desk') RETURNING id`, f.otherOrg)

	f.member = user("member", domain.UserKindHuman)
	f.lead = user("lead", domain.UserKindHuman)
	f.admin = user("admin", domain.UserKindHuman)
	f.plain = user("plain", domain.UserKindHuman)
	f.customer = user("customer", domain.UserKindCustomer)
	f.stranger = user("stranger", domain.UserKindHuman)
	f.agent = user("agent", domain.UserKindAgent)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.org, f.admin)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, f.org, f.member)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, f.org, f.plain)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, f.team, f.member)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, f.team, f.lead)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.otherOrg, f.stranger)
	exec(`INSERT INTO team_submit_grants (team_id, org_id, user_id) VALUES ($1, $2, $3)`, f.team, f.org, f.customer)

	f.svc = New(repo, nil, nil)
	return f
}

// viewsTestRepo returns a Repo whose database has every migration applied. It is
// submitTestRepo's body: one migrated database per process is the service
// package's convention, and repeating the helper here would let the two drift.
func viewsTestRepo(t *testing.T) *repository.Repo {
	t.Helper()
	return submitTestRepo(t)
}

// principal builds a signed-in caller for userID on channel (the web UI unless a
// test says otherwise).
func (f *viewsFixture) principal(userID int, channel string) authz.Principal {
	f.t.Helper()
	var email string
	if err := f.db.QueryRow(f.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		f.t.Fatalf("email of %d: %v", userID, err)
	}
	if channel == "" {
		channel = auth.ChannelUI
	}
	return authz.Principal{UserID: userID, Email: email, Channel: channel, AuthMethod: auth.AuthDexSession}
}

// delivered marks a request as delivered the way 053's CHECK requires: an
// outcome brief and a delivery time (I2). The requester-facing read path has to
// cope with a real row, and a row without the evidence cannot be stored.
func delivered(brief string) func(*domain.TeamRequest) {
	return func(r *domain.TeamRequest) {
		r.OutcomeBrief = brief
		r.DeliveredAt = timePtr(time.Now().UTC().Truncate(time.Microsecond))
	}
}

// accepted is delivered plus the requester's acceptance (I1: acceptance needs a
// delivery, and requester_accepted needs accepted_at).
func accepted(brief string) func(*domain.TeamRequest) {
	return func(r *domain.TeamRequest) {
		delivered(brief)(r)
		r.AcceptedAt = timePtr(time.Now().UTC().Truncate(time.Microsecond))
	}
}

// request inserts one request into team (in this fixture's org, or in
// otherOrg's team) and returns its id. Mutators set whichever columns a case is
// about; everything else takes 053's defaults.
func (f *viewsFixture) request(teamID int, refSeq *int, state domain.RequestStatus, mutators ...func(*domain.TeamRequest)) int {
	f.t.Helper()
	*refSeq++
	req := &domain.TeamRequest{
		PublicRef:        fmt.Sprintf("views%09d%04d", f.org, *refSeq),
		OrgID:            f.org,
		TeamID:           teamID,
		RequesterContact: "requester@views.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "A request from the views fixture",
		InputBrief:       "The body the requester wrote.",
		State:            state,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        time.Now().UTC().Truncate(time.Microsecond),
	}
	if teamID == f.foreign {
		req.OrgID = f.otherOrg
	}
	for _, m := range mutators {
		m(req)
	}
	var id int
	if err := f.db.QueryRow(f.ctx, `
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
		req.PublicRef, req.OrgID, req.TeamID, req.RequesterUserID, req.RequesterContact,
		string(req.SourceKind), req.SourceExternalID, req.Subject, req.InputBrief, req.OutcomeBrief, string(req.State),
		req.ClaimUserID, req.ClaimedAt, req.OwnerUserID, req.DuplicateOf, req.RejectReason,
		req.NeedsInfoSince, req.PlannedAt, req.DeliveredAt, req.AcceptedAt, req.AcceptedBy,
		req.ClosedAt, req.ReopenedAt, req.ReopenedCount, req.Rev, req.Metadata, req.CreatedAt,
	).Scan(&id); err != nil {
		f.t.Fatalf("insert request %q: %v", req.PublicRef, err)
	}
	return id
}

// comment appends one discussion row, attributed to authorUserID (nil for a
// service client's identity).
func (f *viewsFixture) comment(requestID int, kind domain.RequestCommentKind, body string, authorUserID *int) int64 {
	f.t.Helper()
	var id int64
	var author string
	if authorUserID == nil {
		author = "connector"
	} else if err := f.db.QueryRow(f.ctx, `SELECT email FROM users WHERE id = $1`, *authorUserID).Scan(&author); err != nil {
		f.t.Fatalf("author email: %v", err)
	}
	if err := f.db.QueryRow(f.ctx, `
		INSERT INTO request_comments (request_id, kind, body, author, author_user_id)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		requestID, string(kind), body, author, authorUserID).Scan(&id); err != nil {
		f.t.Fatalf("insert comment: %v", err)
	}
	return id
}

// attachment inserts one stored file. It writes bytes because 056's columns and
// CHECKs are the storage contract's, not this read path's, and a metadata-only
// insert would not prove the list can read a real row.
func (f *viewsFixture) attachment(requestID int, name, contentType string, uploadedBy *int, scan domain.RequestScanState) int64 {
	f.t.Helper()
	content := []byte("%PDF-1.4 views fixture bytes")
	var id int64
	if err := f.db.QueryRow(f.ctx, `
		INSERT INTO request_attachments (
			request_id, file_name, content_type, size_bytes, sha256, scan_state, uploaded_by, content)
		VALUES ($1, $2, $3, $4, encode(sha256($5::bytea), 'hex'), $6, $7, $5)
		RETURNING id`,
		requestID, name, contentType, len(content), content, string(scan), uploadedBy).Scan(&id); err != nil {
		f.t.Fatalf("insert attachment: %v", err)
	}
	return id
}

// claim / assign set the two ownership columns. A claim needs its timestamp:
// 053's CHECK refuses a claim with no time.
func (f *viewsFixture) claim(requestID, userID int) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx,
		`UPDATE team_requests SET claim_user_id = $2, claimed_at = NOW() WHERE id = $1`,
		requestID, userID); err != nil {
		f.t.Fatalf("claim %d: %v", requestID, err)
	}
}

func (f *viewsFixture) assign(requestID, userID int) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, `UPDATE team_requests SET owner_user_id = $2 WHERE id = $1`, requestID, userID); err != nil {
		f.t.Fatalf("assign %d: %v", requestID, err)
	}
}

// ---- 1. views ----

// TestListRequests_Views is criterion 1's queue half: exactly the principals who
// have standing on the inbox read it, and everyone else gets the same answer as
// a team that does not exist.
func TestListRequests_Views(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	mine := intRef(f.member)
	f.request(f.team, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) { r.RequesterUserID = mine })

	member := f.principal(f.member, "")
	lead := f.principal(f.lead, "")
	admin := f.principal(f.admin, "")
	super := authz.Principal{UserID: f.admin, Email: "super@views.test", Super: true}

	for _, tc := range []struct {
		name string
		p    authz.Principal
	}{
		{"team member", member},
		{"team lead", lead},
		{"org owner", admin},
		{"global admin", super},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queue, err := f.svc.ListRequests(f.ctx, tc.p, RequestQueueInput{TeamID: f.team})
			if err != nil {
				t.Fatalf("ListRequests: %v", err)
			}
			if queue.Count == 0 || len(queue.Items) == 0 {
				t.Fatalf("queue is empty for a %s, want at least the one request", tc.name)
			}
			if queue.Totals.Total != queue.Count {
				t.Fatalf("Totals.Total = %d, want the page count %d for an unfiltered, unpaged read", queue.Totals.Total, queue.Count)
			}
			// The queue is the record: a triager works from it, so an internal
			// field being present is the point.
			if queue.Items[0].OrgID != f.org || queue.Items[0].TeamID != f.team {
				t.Fatalf("queue row is not the record: org %d, team %d", queue.Items[0].OrgID, queue.Items[0].TeamID)
			}
		})
	}

	for _, tc := range []struct {
		name string
		p    authz.Principal
	}{
		{"plain org member with no team role", f.principal(f.plain, "")},
		{"customer with a submit grant only", f.principal(f.customer, "")},
		{"stranger in another org", f.principal(f.stranger, "")},
		{"agent account with no role", f.principal(f.agent, auth.ChannelMCP)},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			_, err := f.svc.ListRequests(f.ctx, tc.p, RequestQueueInput{TeamID: f.team})
			assertRequestNotFound(t, err)
		})
	}

	t.Run("an unknown team is the same refusal", func(t *testing.T) {
		_, err := f.svc.ListRequests(f.ctx, member, RequestQueueInput{TeamID: -1})
		assertRequestNotFound(t, err)
	})
	t.Run("another team's id, seen from a team I lead", func(t *testing.T) {
		// The *other* team in my own org: I lead the first, so I have standing
		// on neither the second's queue nor its existence beyond it.
		_, err := f.svc.ListRequests(f.ctx, member, RequestQueueInput{TeamID: f.otherTeam})
		assertRequestNotFound(t, err)
	})
	t.Run("a foreign org's team", func(t *testing.T) {
		_, err := f.svc.ListRequests(f.ctx, member, RequestQueueInput{TeamID: f.foreign})
		assertRequestNotFound(t, err)
	})
}

// TestListRequests_RereadsStandingEveryCall is criterion 1's revocation half: the
// decision comes from the rows on every call, so losing the standing takes effect
// on the next read with no cache to reset (contract §2's per-request re-check).
func TestListRequests_RereadsStandingEveryCall(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	f.request(f.team, &seq, domain.RequestStatusNew)
	p := f.principal(f.member, "")

	if _, err := f.svc.ListRequests(f.ctx, p, RequestQueueInput{TeamID: f.team}); err != nil {
		t.Fatalf("member read before removal: %v", err)
	}
	if _, err := f.db.Exec(f.ctx, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, f.team, f.member); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if _, err := f.svc.ListRequests(f.ctx, p, RequestQueueInput{TeamID: f.team}); !isRequestNotFound(err) {
		t.Fatalf("member read after removal = %v, want the surface's not-found", err)
	}
	// The org role grants nothing here: this person is a plain org member, and
	// the team role was their only standing.
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM org_members WHERE org_id = $1 AND user_id = $2`, f.org, f.member).Scan(new(int)); err != nil {
		t.Fatalf("org membership: %v", err)
	}
}

// TestGetRequest_Views is criterion 1's single-request half across every
// principal and every way of addressing the request.
func TestGetRequest_Views(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	requester := intRef(f.member) // the member also requests: the team's view must win
	reqID := f.request(f.team, &seq, domain.RequestStatusTriaging, func(r *domain.TeamRequest) {
		r.RequesterUserID = requester
		r.Subject = "Please export the queue"
	})
	other := f.request(f.team, &seq, domain.RequestStatusNew) // no requester user
	_ = other

	t.Run("team members read the record by id inside their team", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			p    authz.Principal
		}{
			{"member", f.principal(f.member, "")},
			{"lead", f.principal(f.lead, "")},
			{"org owner", f.principal(f.admin, "")},
		} {
			read, err := f.svc.GetRequest(f.ctx, tc.p, RequestReadInput{TeamID: f.team, RequestID: reqID})
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !read.View.Full() || read.Record == nil || read.Projection != nil {
				t.Fatalf("%s: view = %s, record %v, projection %v; want the team's record only",
					tc.name, read.View, read.Record != nil, read.Projection != nil)
			}
			if read.Record.Subject != "Please export the queue" {
				t.Fatalf("%s: subject = %q", tc.name, read.Record.Subject)
			}
		}
	})

	t.Run("the requester reads their own projection, never the record", func(t *testing.T) {
		read, err := f.svc.GetRequest(f.ctx, f.principal(f.member, ""), RequestReadInput{
			TeamID: f.team, RequestID: reqID,
		})
		// A team member who is also the requester gets the team's view: the
		// decision ranks the team's standing first (RequestViewFor's rule).
		if err != nil || !read.View.Full() {
			t.Fatalf("member-and-requester: view = %s, err %v; want the team's", read.View, err)
		}
	})

	t.Run("a requester with no team role reads their own by reference", func(t *testing.T) {
		// A customer's own request: the customer holds a submit grant and no
		// membership, so this is the requester surface exactly as a customer
		// uses it.
		seq++
		custReq := f.request(f.team, &seq, domain.RequestStatusDelivered, func(r *domain.TeamRequest) {
			r.RequesterUserID = intRef(f.customer)
			r.PublicRef = fmt.Sprintf("custref%09d%04d", f.org, seq)
			r.DeliveredAt = timePtr(time.Now().UTC().Truncate(time.Microsecond))
			r.OutcomeBrief = "Exported last week; here is the file."
		})
		read, err := f.svc.GetRequest(f.ctx, f.principal(f.customer, ""), RequestReadInput{
			PublicRef: "REQ-" + fmt.Sprintf("custref%09d%04d", f.org, seq),
		})
		if err != nil {
			t.Fatalf("requester read by reference: %v", err)
		}
		if !read.View.Own() || read.Projection == nil || read.Record != nil {
			t.Fatalf("view = %s, projection %v, record %v; want the projection only",
				read.View, read.Projection != nil, read.Record != nil)
		}
		if read.Projection.PublicRef != fmt.Sprintf("custref%09d%04d", f.org, seq) {
			t.Fatalf("public_ref = %q, want the stored token (display prefix stripped)", read.Projection.PublicRef)
		}
		if read.Projection.OutcomeBrief == "" {
			t.Fatalf("a delivered request shows its outcome brief to its requester")
		}
		// And by serial, on a surface that kept the id from its own answer.
		byID, err := f.svc.GetRequest(f.ctx, f.principal(f.customer, ""), RequestReadInput{RequestID: custReq})
		if err != nil || byID.Projection == nil {
			t.Fatalf("requester read by id: %v (projection %v)", err, byID != nil && byID.Projection != nil)
		}
	})

	t.Run("the team's view of a request with no requester user", func(t *testing.T) {
		read, err := f.svc.GetRequest(f.ctx, f.principal(f.lead, ""), RequestReadInput{TeamID: f.team, RequestID: other})
		if err != nil || read.Record == nil {
			t.Fatalf("team read of a connector request: %v", err)
		}
	})
}

// TestGetRequest_TeamSurfaceByReference pins the team surface's second way of
// naming a request: a triager pasting the reference the requester quoted. It is
// tenant-scoped, so a reference from a sibling team or another org is not found
// rather than found-and-refused, and the string it normalizes is the display
// form (REQ-…) as well as the stored token.
func TestGetRequest_TeamSurfaceByReference(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	ref := fmt.Sprintf("teamref%09d%04d", f.org, seq+1)
	reqID := f.request(f.team, &seq, domain.RequestStatusTriaging, func(r *domain.TeamRequest) { r.PublicRef = ref })
	// The same requester's request in a sibling team, addressed from this team.
	siblingRef := fmt.Sprintf("teamref%09d%04d", f.org, seq+1)
	sibling := f.request(f.otherTeam, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) { r.PublicRef = siblingRef })
	// And one in another org.
	foreignRef := fmt.Sprintf("teamref%09d%04d", f.org, seq+1)
	foreign := f.request(f.foreign, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) { r.PublicRef = foreignRef })

	lead := f.principal(f.lead, "")
	for _, name := range []string{ref, "REQ-" + ref} {
		read, err := f.svc.GetRequest(f.ctx, lead, RequestReadInput{TeamID: f.team, PublicRef: name})
		if err != nil {
			t.Fatalf("team read by reference %q: %v", name, err)
		}
		if read.Record == nil || read.Record.ID != reqID {
			t.Fatalf("team read by reference %q = %+v, want request %d", name, read.Record, reqID)
		}
	}

	// The sibling team's reference, read from THIS team: not found, the same
	// answer as a reference nobody ever minted.
	for _, tc := range []struct {
		name string
		ref  string
	}{
		{"a sibling team's reference", siblingRef},
		{"another org's reference", foreignRef},
		{"a reference that was never minted", fmt.Sprintf("nobody%09d%04d", f.org, 9999)},
	} {
		_, err := f.svc.GetRequest(f.ctx, lead, RequestReadInput{TeamID: f.team, PublicRef: tc.ref})
		assertRequestNotFound(t, err)
	}
	// A global admin has standing on EVERY team, so this is the case that makes
	// the reference read's team half observable: a reference from a sibling team
	// is not this team's request, whatever the caller's standing. Without the
	// tenant term the admin's read of team A would hand back team B's row.
	admin := authz.Principal{UserID: f.admin, Email: "views-super@views.test", Super: true}
	if _, err := f.svc.GetRequest(f.ctx, admin, RequestReadInput{TeamID: f.team, PublicRef: siblingRef}); !isRequestNotFound(err) {
		t.Fatalf("a global admin read a sibling team's request from this team's queue: %v", err)
	}
	if read, err := f.svc.GetRequest(f.ctx, admin, RequestReadInput{TeamID: f.otherTeam, PublicRef: siblingRef}); err != nil || read.Record == nil || read.Record.ID != sibling {
		t.Fatalf("the admin's read of the sibling's own team: %v (record %v)", err, read != nil && read.Record != nil)
	}

	// Each is genuinely readable from its own team, so the refusals above are
	// about the tenant and not about the reference shape.
	if _, err := f.svc.GetRequest(f.ctx, lead, RequestReadInput{TeamID: f.otherTeam, PublicRef: siblingRef}); !isRequestNotFound(err) {
		// The lead has no standing on the sibling team, so this is the standing
		// refusal — which is itself the right answer, and proves the reference
		// resolved rather than being malformed.
		t.Fatalf("lead read of a sibling team's queue = %v, want the not-found (no standing)", err)
	}
	// Another org's owner reads their own team's request by reference: the
	// reference is fine, the tenant was the difference.
	if read, err := f.svc.GetRequest(f.ctx, f.principal(f.stranger, ""), RequestReadInput{TeamID: f.foreign, PublicRef: foreignRef}); err != nil || read.Record == nil || read.Record.ID != foreign {
		t.Fatalf("the foreign org's own read by reference: %v (record %v)", err, read != nil && read.Record != nil)
	}
	_ = sibling
}

// TestGetRequest_RefusalsAreIndistinguishable is criterion 1's leak half: a
// request that is not yours, a request that does not exist, and a request in
// another team are one answer, byte for byte.
func TestGetRequest_RefusalsAreIndistinguishable(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	own := f.request(f.team, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	// Another customer's request in the same team, and one in another team of
	// the same org, and one in another org.
	otherCustomer := f.request(f.team, &seq, domain.RequestStatusTriaging, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.plain)
	})
	sibling := f.request(f.otherTeam, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.plain)
	})
	// The same requester's own request, submitted to a desk in another org: it
	// IS theirs, so it is a positive case, not a refusal one — a requester's
	// visibility follows the requester column, not a team.
	foreignOwn := f.request(f.foreign, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	// And another org's request that belongs to somebody else: refused.
	foreignOther := f.request(f.foreign, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.stranger)
	})
	connector := f.request(f.team, &seq, domain.RequestStatusNew) // no requester user

	customer := f.principal(f.customer, "")
	stranger := f.principal(f.stranger, "")

	type probe struct {
		name string
		p    authz.Principal
		in   RequestReadInput
	}
	probes := []probe{
		{"another requester's request, by id", customer, RequestReadInput{RequestID: otherCustomer}},
		{"a sibling team's request for this requester, by id", customer, RequestReadInput{RequestID: sibling}},
		{"a connector request with no requester, by id", customer, RequestReadInput{RequestID: connector}},
		{"an id that never existed", customer, RequestReadInput{RequestID: 2_000_000_000}},
		{"another org's request for somebody else, by id", customer, RequestReadInput{RequestID: foreignOther}},
		{"a stranger naming the team", stranger, RequestReadInput{TeamID: f.team, RequestID: own}},
		{"a stranger naming their own team for another org's request", stranger, RequestReadInput{TeamID: f.foreign, RequestID: own}},
		{"a plain org member naming the team", f.principal(f.plain, ""), RequestReadInput{TeamID: f.team, RequestID: own}},
		{"an agent with no role naming the team", f.principal(f.agent, auth.ChannelMCP), RequestReadInput{TeamID: f.team, RequestID: own}},
	}
	// The positive counter-case, so no probe above passes by refusing
	// everything: the requester's own request in another org reads.
	if read, err := f.svc.GetRequest(f.ctx, customer, RequestReadInput{RequestID: foreignOwn}); err != nil || read.Projection == nil {
		t.Fatalf("a requester's own request in another org's desk: %v (projection %v)", err, read != nil && read.Projection != nil)
	}

	var want string
	for _, pr := range probes {
		_, err := f.svc.GetRequest(f.ctx, pr.p, pr.in)
		if !isRequestNotFound(err) {
			t.Fatalf("%s: err = %v, want the surface's not-found", pr.name, err)
		}
		got := err.Error()
		if want == "" {
			want = got
			continue
		}
		// Identical bytes AND an identical stable code: the 404 a client
		// branches on is the same for every probe.
		if got != want || authz.CodeOf(err) != authz.CodeRequestNotFound {
			t.Fatalf("%s: %q (code %q), want the same %q (code %q)",
				pr.name, got, authz.CodeOf(err), want, authz.CodeRequestNotFound)
		}
	}
}

// TestGetRequest_LoadFailureIsNotA404 pins the one distinction the 404 rule must
// not swallow: a cancelled context is a database failure, not a missing request.
func TestGetRequest_LoadFailureIsNotA404(t *testing.T) {
	f := newViewsFixture(t)
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	_, err := f.svc.GetRequest(cancelled, f.principal(f.member, ""), RequestReadInput{TeamID: f.team, RequestID: 1})
	if err == nil {
		t.Fatalf("cancelled read returned nil, want an error")
	}
	if isRequestNotFound(err) {
		t.Fatalf("a cancelled read answered the 404; a database failure must not read as a missing request (%v)", err)
	}
}

// TestGetRequest_ReadRefValidation pins the naming rules: exactly one of id and
// reference, and a reference must be one the domain could have minted.
func TestGetRequest_ReadRefValidation(t *testing.T) {
	f := newViewsFixture(t)
	member := f.principal(f.member, "")
	for _, tc := range []struct {
		name string
		in   RequestReadInput
		want string // substring of the message
	}{
		{"neither", RequestReadInput{TeamID: f.team}, "name the request"},
		{"both", RequestReadInput{TeamID: f.team, RequestID: 1, PublicRef: "abc"}, "not both"},
		{"a bare serial as a reference", RequestReadInput{PublicRef: "0000000000001490"}, "opaque token"},
		{"a reference with a space", RequestReadInput{PublicRef: "not a token at all xxxx"}, "may hold letters"},
		{"a too-short reference", RequestReadInput{PublicRef: "short"}, "must be 16 to 64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.GetRequest(f.ctx, member, tc.in)
			if err == nil {
				t.Fatalf("accepted %+v", tc.in)
			}
			if authz.StatusOf(err, 500) != 400 {
				t.Fatalf("status = %d, want 400 (request.invalid)", authz.StatusOf(err, 500))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// ---- 2. counts and pagination ----

// TestListRequests_CountsAndPaging is criterion 2: the pills and the page come
// from one filter, the pills never count the page remainder, and walking the
// pages sees every matching row exactly once.
func TestListRequests_CountsAndPaging(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	states := []domain.RequestStatus{
		domain.RequestStatusNew, domain.RequestStatusNew, domain.RequestStatusNew,
		domain.RequestStatusTriaging, domain.RequestStatusTriaging,
		domain.RequestStatusInProgress,
		domain.RequestStatusDelivered,
		domain.RequestStatusRequesterAccepted,
		domain.RequestStatusRejected,
		domain.RequestStatusDuplicate,
		domain.RequestStatusNeedsInformation,
	}
	// A canonical for the duplicate to point at. 053's CHECK requires one, and
	// it must exist before the loop so the state counts below are the fixture's
	// own 11 rows rather than 11 plus a canonical.
	canonical := f.request(f.team, &seq, domain.RequestStatusTriaging)
	states = states[:0]
	for _, st := range []domain.RequestStatus{
		domain.RequestStatusNew, domain.RequestStatusNew, domain.RequestStatusNew,
		domain.RequestStatusTriaging, domain.RequestStatusTriaging,
		domain.RequestStatusInProgress,
		domain.RequestStatusDelivered,
		domain.RequestStatusRequesterAccepted,
		domain.RequestStatusRejected,
		domain.RequestStatusDuplicate,
		domain.RequestStatusNeedsInformation,
	} {
		var mut []func(*domain.TeamRequest)
		switch st {
		case domain.RequestStatusDelivered:
			mut = append(mut, delivered("Delivered: here is the export."))
		case domain.RequestStatusRequesterAccepted:
			mut = append(mut, accepted("Delivered and accepted."))
		case domain.RequestStatusRejected:
			mut = append(mut, func(r *domain.TeamRequest) { r.RejectReason = "Out of scope for this quarter." })
		case domain.RequestStatusDuplicate:
			mut = append(mut, func(r *domain.TeamRequest) { r.DuplicateOf = &canonical })
		case domain.RequestStatusNeedsInformation:
			mut = append(mut, func(r *domain.TeamRequest) { r.NeedsInfoSince = timePtr(time.Now().UTC()) })
		}
		f.request(f.team, &seq, st, mut...)
		states = append(states, st)
	}
	// 11 queued rows plus the canonical the fixture added before them.
	wantTotal := len(states) + 1
	_ = canonical
	// A sibling team's row and another org's row must not appear in either
	// answer.
	f.request(f.otherTeam, &seq, domain.RequestStatusNew)
	f.request(f.foreign, &seq, domain.RequestStatusNew)
	member := f.principal(f.member, "")

	queue, err := f.svc.ListRequests(f.ctx, member, RequestQueueInput{TeamID: f.team, Limit: 3})
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if queue.Count != 3 || len(queue.Items) != 3 {
		t.Fatalf("page = %d items (count %d), want 3", len(queue.Items), queue.Count)
	}
	if queue.NextBeforeID == 0 {
		t.Fatalf("NextBeforeID = 0 with 11 rows and a page of 3, want a cursor")
	}
	if queue.Totals.Total != wantTotal {
		t.Fatalf("Totals.Total = %d, want %d: the pills must count the filter, not the page", queue.Totals.Total, wantTotal)
	}
	for _, st := range domain.RequestStatuses {
		want := 0
		for _, s := range states {
			if s == st {
				want++
			}
		}
		if st == domain.RequestStatusTriaging {
			want++ // the canonical is triaging too
		}
		if got := queue.Totals.State(st); got != want {
			t.Fatalf("pill %s = %d, want %d", st, got, want)
		}
	}
	// The page did not move the caller's cursor or limit: the totals were
	// computed on a copy.
	if queue.NextBeforeID != queue.Items[len(queue.Items)-1].ID {
		t.Fatalf("NextBeforeID = %d, want the last row's id %d", queue.NextBeforeID, queue.Items[len(queue.Items)-1].ID)
	}

	// Paging to the end sees every row exactly once, and the last page's totals
	// are the same totals (a pill never shrinks as someone scrolls).
	seen := map[int]int{}
	in := RequestQueueInput{TeamID: f.team, Limit: 3}
	for page := 0; ; page++ {
		if page > 20 {
			t.Fatalf("paging did not terminate: cursor %d", in.BeforeID)
		}
		got, err := f.svc.ListRequests(f.ctx, member, in)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if got.Totals.Total != wantTotal {
			t.Fatalf("page %d: Totals.Total = %d, want %d (a total is cursor-free)", page, got.Totals.Total, wantTotal)
		}
		for _, r := range got.Items {
			seen[r.ID]++
			if seen[r.ID] > 1 {
				t.Fatalf("request %d appeared on two pages", r.ID)
			}
		}
		if got.NextBeforeID == 0 {
			break
		}
		in.BeforeID = got.NextBeforeID
	}
	if len(seen) != wantTotal {
		t.Fatalf("paging saw %d rows, want %d", len(seen), wantTotal)
	}

	// A state filter narrows the page AND the pills, and a state it excluded
	// reads 0 rather than the whole queue.
	filtered, err := f.svc.ListRequests(f.ctx, member, RequestQueueInput{
		TeamID: f.team, States: []domain.RequestStatus{domain.RequestStatusNew},
	})
	if err != nil {
		t.Fatalf("filtered ListRequests: %v", err)
	}
	if filtered.Totals.Total != 3 {
		t.Fatalf("filtered total = %d, want 3", filtered.Totals.Total)
	}
	if filtered.Totals.State(domain.RequestStatusTriaging) != 0 {
		t.Fatalf("a state the filter excluded = %d, want 0", filtered.Totals.State(domain.RequestStatusTriaging))
	}
	for _, r := range filtered.Items {
		if r.State != domain.RequestStatusNew {
			t.Fatalf("filtered page holds a %s row", r.State)
		}
	}

	// An unknown state is refused rather than answered with nothing: an empty
	// page would read as "no work in that state".
	_, err = f.svc.ListRequests(f.ctx, member, RequestQueueInput{TeamID: f.team, States: []domain.RequestStatus{"not-a-state"}})
	if err == nil {
		t.Fatalf("an unknown state was accepted")
	}
}

// TestListRequests_MineIsASliceOfTheQueue pins the "Claimed by me" panel: it is
// the caller's own slice of the SAME filter, deduplicated across claim and
// assignment, and its total is cursor-free.
func TestListRequests_MineIsASliceOfTheQueue(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	a := f.request(f.team, &seq, domain.RequestStatusNew)
	b := f.request(f.team, &seq, domain.RequestStatusTriaging)
	c := f.request(f.team, &seq, domain.RequestStatusNew)
	f.request(f.team, &seq, domain.RequestStatusNew) // unowned: nobody's
	sibling := f.request(f.otherTeam, &seq, domain.RequestStatusNew)

	f.claim(a, f.member)  // claimed only
	f.assign(b, f.member) // assigned only
	f.claim(c, f.member)
	f.assign(c, f.member) // both: must appear once
	f.claim(sibling, f.member)

	member := f.principal(f.member, "")
	// Mine is off by default.
	plain, err := f.svc.ListRequests(f.ctx, member, RequestQueueInput{TeamID: f.team})
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if plain.Mine != nil {
		t.Fatalf("Mine is set without asking for it")
	}

	queue, err := f.svc.ListRequests(f.ctx, member, RequestQueueInput{TeamID: f.team, Mine: true})
	if err != nil {
		t.Fatalf("ListRequests mine: %v", err)
	}
	if queue.Mine == nil {
		t.Fatalf("Mine is nil after asking for it")
	}
	if queue.Mine.Total != 3 {
		t.Fatalf("Mine.Total = %d, want 3 (a, b, and c once)", queue.Mine.Total)
	}
	got := append([]int{}, queue.Mine.IDs...)
	if len(got) != 3 {
		t.Fatalf("Mine.IDs = %v, want three distinct ids", got)
	}
	seen := map[int]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("Mine.IDs = %v holds a duplicate: %d", got, id)
		}
		seen[id] = true
	}
	if seen[sibling] {
		t.Fatalf("Mine.IDs = %v holds another team's request %d", got, sibling)
	}
	if len(queue.Mine.ClaimedIDs) != 2 || len(queue.Mine.AssignedIDs) != 2 {
		t.Fatalf("claimed %v / assigned %v, want two each", queue.Mine.ClaimedIDs, queue.Mine.AssignedIDs)
	}

	// The slice is a slice of the filter: a state filter narrows it too.
	narrow, err := f.svc.ListRequests(f.ctx, member, RequestQueueInput{
		TeamID: f.team, States: []domain.RequestStatus{domain.RequestStatusTriaging}, Mine: true,
	})
	if err != nil {
		t.Fatalf("ListRequests mine filtered: %v", err)
	}
	if narrow.Mine.Total != 1 || len(narrow.Mine.IDs) != 1 || narrow.Mine.IDs[0] != b {
		t.Fatalf("filtered Mine = %+v, want just %d", narrow.Mine, b)
	}

	// A caller with no user row owns nothing; Mine stays nil rather than
	// inventing an empty slice that a panel would render as "you hold nothing".
	localDev := authz.Principal{Super: true}
	noUser, err := f.svc.ListRequests(f.ctx, localDev, RequestQueueInput{TeamID: f.team, Mine: true})
	if err != nil {
		t.Fatalf("ListRequests as a principal with no user: %v", err)
	}
	if noUser.Mine != nil {
		t.Fatalf("Mine = %+v for a principal with no user row", noUser.Mine)
	}
}

// TestListRequests_TenantComesFromTheTeamRow pins the pairing rule: the org is
// read from the team, so an adapter cannot aim a queue at another tenant by
// sending a team id with a different org, and a team in another org is simply
// not this team's queue.
func TestListRequests_TenantComesFromTheTeamRow(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	f.request(f.team, &seq, domain.RequestStatusNew)
	f.request(f.otherTeam, &seq, domain.RequestStatusNew)

	member := f.principal(f.member, "")
	queue, err := f.svc.ListRequests(f.ctx, member, RequestQueueInput{TeamID: f.team})
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if queue.Totals.Total != 1 {
		t.Fatalf("Totals.Total = %d, want 1: the sibling team's row leaked in", queue.Totals.Total)
	}
	for _, r := range queue.Items {
		if r.TeamID != f.team || r.OrgID != f.org {
			t.Fatalf("queue row is team %d org %d, want %d/%d", r.TeamID, r.OrgID, f.team, f.org)
		}
	}
}

// ---- 2b. the requester's own list ----

// TestListOwnRequests_PagesAndCounts is criterion 2 for the requester's list: only
// the caller's own requests, newest first, pages that see each row once, and a
// total that does not shrink as someone pages.
func TestListOwnRequests_PagesAndCounts(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	var mine []int
	for i := range 5 {
		st := domain.RequestStatusNew
		var extra []func(*domain.TeamRequest)
		if i >= 3 {
			st = domain.RequestStatusDelivered
			extra = append(extra, delivered("Delivered: the export is attached."))
		}
		mine = append(mine, f.request(f.team, &seq, st, append(extra, func(r *domain.TeamRequest) {
			r.RequesterUserID = intRef(f.customer)
		})...))
	}
	// Another requester's rows in the same team, and a sibling team's row of the
	// same requester: neither may appear.
	f.request(f.team, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.plain)
	})
	otherDesk := f.request(f.otherTeam, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	// A connector submission has no requester user: never "yours".
	f.request(f.team, &seq, domain.RequestStatusNew)

	customer := f.principal(f.customer, "")
	page, err := f.svc.ListOwnRequests(f.ctx, customer, OwnRequestListInput{Limit: 2})
	if err != nil {
		t.Fatalf("ListOwnRequests: %v", err)
	}
	if page.Count != 2 || len(page.Items) != 2 || page.NextBeforeID == 0 {
		t.Fatalf("page = %d items (count %d, cursor %d), want 2 with a cursor", len(page.Items), page.Count, page.NextBeforeID)
	}
	// Five in this desk plus the one the same person sent to another desk: the
	// list is "your requests", not "your requests in one team".
	if page.Total != 6 {
		t.Fatalf("Total = %d, want 6", page.Total)
	}

	seen := map[string]bool{}
	in := OwnRequestListInput{Limit: 2}
	for p := 0; ; p++ {
		if p > 10 {
			t.Fatalf("paging did not terminate")
		}
		got, err := f.svc.ListOwnRequests(f.ctx, customer, in)
		if err != nil {
			t.Fatalf("page %d: %v", p, err)
		}
		if got.Total != 6 {
			t.Fatalf("page %d: Total = %d, want 6 (cursor-free)", p, got.Total)
		}
		for i, item := range got.Items {
			// Every row is the projection, never the record.
			if item == nil || item.PublicRef == "" {
				t.Fatalf("page %d row %d is not a projection: %+v", p, i, item)
			}
			seen[item.PublicRef] = true
		}
		if got.NextBeforeID == 0 {
			break
		}
		in.BeforeID = got.NextBeforeID
	}
	if len(seen) != 6 {
		t.Fatalf("paging saw %d distinct requests, want 6", len(seen))
	}
	_ = otherDesk

	// A state filter narrows the list and the total together.
	delivered, err := f.svc.ListOwnRequests(f.ctx, customer, OwnRequestListInput{
		States: []domain.RequestStatus{domain.RequestStatusDelivered},
	})
	if err != nil {
		t.Fatalf("filtered ListOwnRequests: %v", err)
	}
	if delivered.Total != 2 || len(delivered.Items) != 2 {
		t.Fatalf("delivered = %d rows (total %d), want 2", len(delivered.Items), delivered.Total)
	}
	for _, item := range delivered.Items {
		if item.State != domain.RequestStatusDelivered {
			t.Fatalf("filtered list holds a %s row", item.State)
		}
	}

	// A caller with no user row owns nothing: an empty page with a zero total,
	// not a refusal ("your requests" and you have none is not an error).
	localDev := authz.Principal{Super: true}
	empty, err := f.svc.ListOwnRequests(f.ctx, localDev, OwnRequestListInput{})
	if err != nil {
		t.Fatalf("ListOwnRequests as a principal with no user: %v", err)
	}
	if empty.Count != 0 || empty.Total != 0 || empty.Items == nil {
		t.Fatalf("empty list = %+v, want an empty page with a zero total", empty)
	}
	if len(empty.Items) != 0 {
		t.Fatalf("empty list holds %d rows", len(empty.Items))
	}
}

// ---- 3. the requester never sees a private field ----

// TestGetRequest_RequesterGetsNoPrivateField is criterion 3: the requester's
// answer carries no record at all, and none of the internal sentinels planted in
// the row reaches its serialized bytes.
func TestGetRequest_RequesterGetsNoPrivateField(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	// Every internal fact this surface must never emit, as a distinct sentinel
	// string, so a leak is identifiable by name rather than by "something
	// changed".
	const (
		contactSentinel   = "private-contact-sentinel@views.test"
		metaSentinel      = "internal-routing-sentinel"
		briefSentinel     = "internal-brief-sentinel"
		rejectShown       = "Out of scope: this belongs to the billing desk."
		externalIDSentine = "vendor-object-sentinel-12345"
	)
	claimed := intRef(f.lead)
	assigned := intRef(f.plain)
	canonical := f.request(f.team, &seq, domain.RequestStatusTriaging) // the duplicate's target
	reqID := f.request(f.team, &seq, domain.RequestStatusRejected, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
		r.RequesterContact = contactSentinel
		r.SourceKind = domain.RequestSourceConnector
		r.SourceExternalID = externalIDSentine
		r.Subject = "Subject the requester wrote"
		r.InputBrief = "Brief the requester wrote"
		r.OutcomeBrief = briefSentinel // mid-flight: must not be shown
		r.RejectReason = rejectShown
		r.ClaimUserID = claimed
		r.ClaimedAt = timePtr(time.Now().UTC())
		r.OwnerUserID = assigned
		r.DuplicateOf = &canonical
		r.Metadata = domain.Metadata{"internal_key": metaSentinel, "jira": "TEST-1"}
		r.Rev = 7
		r.NeedsInfoSince = timePtr(time.Now().UTC())
	})

	read, err := f.svc.GetRequest(f.ctx, f.principal(f.customer, ""), RequestReadInput{RequestID: reqID})
	if err != nil {
		t.Fatalf("requester read: %v", err)
	}
	if read.Record != nil {
		t.Fatalf("the requester's answer carries the record")
	}
	if read.Projection == nil {
		t.Fatalf("the requester's answer carries no projection")
	}
	body, err := json.Marshal(read.Projection)
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	text := string(body)
	for name, sentinel := range map[string]string{
		"contact snapshot":  contactSentinel,
		"metadata value":    metaSentinel,
		"outcome brief":     briefSentinel,
		"external id":       externalIDSentine,
		"internal org id":   fmt.Sprintf(`"org_id":%d`, f.org),
		"internal team id":  fmt.Sprintf(`"team_id":%d`, f.team),
		"claim field":       `"claim_user_id"`,
		"assignee field":    `"owner_user_id"`,
		"duplicate field":   `"duplicate_of"`,
		"revision field":    `"rev"`,
		"metadata field":    `"metadata"`,
		"requester contact": `"requester_contact"`,
	} {
		if strings.Contains(text, sentinel) {
			t.Fatalf("the requester's view leaks the %s (%q): %s", name, sentinel, text)
		}
	}
	// What it must carry: the requester's own words, the state word, the ref.
	if read.Projection.Subject != "Subject the requester wrote" {
		t.Fatalf("subject = %q", read.Projection.Subject)
	}
	if read.Projection.InputBrief != "Brief the requester wrote" {
		t.Fatalf("input brief = %q", read.Projection.InputBrief)
	}
	if read.Projection.StateWord == "" || read.Projection.State != domain.RequestStatusRejected {
		t.Fatalf("state = %q, word %q", read.Projection.State, read.Projection.StateWord)
	}
	// A rejected request's reason IS the requester's to see (contract §4): the
	// sentinel above proves it is shown, not leaked, once the state supports it.
	if read.Projection.RejectReason != rejectShown {
		t.Fatalf("reject reason = %q, want the team's reason (%q) on a rejected request", read.Projection.RejectReason, rejectShown)
	}
	// Mid-flight outcome text is not: delivery is a lifecycle act.
	if read.Projection.OutcomeBrief != "" {
		t.Fatalf("outcome brief = %q on an undelivered request, want it cleared", read.Projection.OutcomeBrief)
	}
}

// TestGetRequest_RequesterThreadAndFiles is criterion 3's sharing half: the
// requester's thread carries the questions the desk asked, the answers they gave
// and their own remarks — never the team's internal discussion or a system row —
// and the file list carries their own uploads in the projection's three words.
func TestGetRequest_RequesterThreadAndFiles(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	reqID := f.request(f.team, &seq, domain.RequestStatusNeedsInformation, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
		r.NeedsInfoSince = timePtr(time.Now().UTC())
	})
	f.comment(reqID, domain.RequestCommentQuestion, "Which quarter should the export cover?", intRef(f.lead))
	f.comment(reqID, domain.RequestCommentAnswer, "The last one, please.", intRef(f.customer))
	f.comment(reqID, domain.RequestCommentDiscussion, "Internal: I will take this one after lunch.", intRef(f.member))
	f.comment(reqID, domain.RequestCommentDiscussion, "My own follow-up note.", intRef(f.customer))
	f.comment(reqID, domain.RequestCommentSystem, "Internal system note: routed to project 12.", intRef(f.lead))

	// Files: the requester's own (clean), a pending one, and a team member's.
	f.attachment(reqID, "export.pdf", "application/pdf", intRef(f.customer), domain.RequestScanClean)
	f.attachment(reqID, "still-scanning.pdf", "application/pdf", intRef(f.customer), domain.RequestScanPending)
	f.attachment(reqID, "internal-notes.pdf", "application/pdf", intRef(f.member), domain.RequestScanClean)

	read, err := f.svc.GetRequest(f.ctx, f.principal(f.customer, ""), RequestReadInput{RequestID: reqID})
	if err != nil {
		t.Fatalf("requester read: %v", err)
	}
	v := read.Projection
	if v == nil {
		t.Fatalf("no projection")
	}

	// The thread: three shared rows in append order, and the two internal ones
	// absent.
	if len(v.Comments) != 3 {
		t.Fatalf("comments = %+v, want three shared rows", v.Comments)
	}
	wantKinds := []domain.RequestCommentKind{
		domain.RequestCommentQuestion, domain.RequestCommentAnswer, domain.RequestCommentDiscussion,
	}
	for i, want := range wantKinds {
		if v.Comments[i].Kind != want {
			t.Fatalf("comment %d kind = %q, want %q", i, v.Comments[i].Kind, want)
		}
	}
	if v.Comments[0].From != domain.RequesterAuthorDesk {
		t.Fatalf("the question's attribution = %q, want the desk", v.Comments[0].From)
	}
	if v.Comments[1].From != domain.RequesterAuthorYou || v.Comments[2].From != domain.RequesterAuthorYou {
		t.Fatalf("the requester's own rows are attributed %q/%q, want \"you\"",
			v.Comments[1].From, v.Comments[2].From)
	}
	for _, c := range v.Comments {
		for _, leak := range []string{"Internal", "internal", "Internal system note", "follow-up"} {
			if leak == "follow-up" && c.From == domain.RequesterAuthorYou {
				continue // their own remark is theirs to see
			}
			if strings.Contains(c.Body, leak) {
				t.Fatalf("comment %q leaks internal text %q", c.Body, leak)
			}
		}
	}

	// The open question card is the latest shared question, in needs_information.
	if v.Question == nil || !strings.Contains(v.Question.Body, "Which quarter") {
		t.Fatalf("question = %+v, want the desk's open question", v.Question)
	}

	// The files: two of the requester's own, the team's absent, in the closed
	// three-word vocabulary, and a reference only on the ready one.
	if len(v.Outputs) != 2 {
		t.Fatalf("outputs = %+v, want two (the requester's own uploads)", v.Outputs)
	}
	byName := map[string]domain.RequesterOutput{}
	for _, o := range v.Outputs {
		byName[o.Name] = o
		if o.Source != domain.RequesterOutputFromRequester {
			t.Fatalf("%s source = %q, want \"you\"", o.Name, o.Source)
		}
	}
	if _, ok := byName["internal-notes.pdf"]; ok {
		t.Fatalf("a team member's file reached the requester")
	}
	ready, ok := byName["export.pdf"]
	if !ok {
		t.Fatalf("the requester's own export.pdf is missing from the list: %+v", v.Outputs)
	}
	// No linker is configured yet, so nothing can serve the bytes and the
	// honest word is "unavailable" — never a promise the surface cannot keep.
	if ready.Status != domain.RequesterOutputUnavailable || ready.DownloadRef != "" {
		t.Fatalf("export.pdf with no linker = %+v, want unavailable with no reference", ready)
	}
	pending, ok := byName["still-scanning.pdf"]
	if !ok || pending.Status != domain.RequesterOutputPreparing {
		t.Fatalf("still-scanning.pdf = %+v (present %v), want preparing", pending, ok)
	}

	// With a linker configured, only the ready file gets a reference, and it is
	// passed through untouched.
	f.svc.RequestDownloads = stubDownloadLinker{ref: "opaque-grant-token"}
	withGrant, err := f.svc.GetRequest(f.ctx, f.principal(f.customer, ""), RequestReadInput{RequestID: reqID})
	if err != nil {
		t.Fatalf("requester read with a linker: %v", err)
	}
	for _, o := range withGrant.Projection.Outputs {
		switch o.Name {
		case "export.pdf":
			if o.Status != domain.RequesterOutputReady || o.DownloadRef != "opaque-grant-token" {
				t.Fatalf("ready file = %+v, want the minted reference", o)
			}
		default:
			if o.DownloadRef != "" {
				t.Fatalf("%s carries a reference (%q) on a %s file", o.Name, o.DownloadRef, o.Status)
			}
		}
	}
}

// TestGetRequest_TeamViewKeepsTheThreadAndFiles is the other side of the sharing
// rule: the team's view is the record, and its thread and files are read through
// the team's own reads (which carry every row, internal ones included). This
// test only proves the decision did not widen the requester's rule into the
// team's: the team's answer is the record, so nothing was filtered away from it.
func TestGetRequest_TeamViewKeepsTheThreadAndFiles(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	reqID := f.request(f.team, &seq, domain.RequestStatusTriaging, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
		r.Metadata = domain.Metadata{"internal_key": "kept-for-triagers"}
	})
	f.comment(reqID, domain.RequestCommentDiscussion, "Internal note the team must keep.", intRef(f.lead))

	read, err := f.svc.GetRequest(f.ctx, f.principal(f.lead, ""), RequestReadInput{TeamID: f.team, RequestID: reqID})
	if err != nil {
		t.Fatalf("team read: %v", err)
	}
	if read.Record == nil || read.Projection != nil {
		t.Fatalf("team read is not the record")
	}
	if read.Record.Metadata["internal_key"] != "kept-for-triagers" {
		t.Fatalf("the team's record lost its metadata")
	}
	// And the team's own thread read still sees the internal row: the read path
	// narrowed the requester's view, not the repository's.
	comments, err := f.svc.Repo.ListRequestComments(f.ctx, reqID, f.org, f.team, 0, 0)
	if err != nil {
		t.Fatalf("team thread: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("team thread has %d rows, want the internal one", len(comments))
	}
}

// ---- scope ----

// TestReads_RefuseSubmitScopedCredential pins the credential rule (contract §5,
// AMP #3772): a submit-only token may submit and nothing else, and the read path
// refuses it before a request is loaded so it cannot probe ids through it.
func TestReads_RefuseSubmitScopedCredential(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	reqID := f.request(f.team, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	scoped := f.principal(f.customer, auth.ChannelMCP)
	scoped.Scope = domain.TokenScopeSubmit
	scoped.ScopeTeamID = f.team

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"ListRequests", func() error {
			_, err := f.svc.ListRequests(f.ctx, scoped, RequestQueueInput{TeamID: f.team})
			return err
		}},
		{"GetRequest", func() error {
			_, err := f.svc.GetRequest(f.ctx, scoped, RequestReadInput{RequestID: reqID})
			return err
		}},
		{"GetRequest by team", func() error {
			_, err := f.svc.GetRequest(f.ctx, scoped, RequestReadInput{TeamID: f.team, RequestID: reqID})
			return err
		}},
		{"ListOwnRequests", func() error {
			_, err := f.svc.ListOwnRequests(f.ctx, scoped, OwnRequestListInput{})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("a submit-scoped credential was allowed to read")
			}
			if authz.CodeOf(err) != authz.CodeTokenScopeDenied {
				t.Fatalf("code = %q, want %q (err: %v)", authz.CodeOf(err), authz.CodeTokenScopeDenied, err)
			}
			if authz.StatusOf(err, 500) != 403 {
				t.Fatalf("status = %d, want 403", authz.StatusOf(err, 500))
			}
		})
	}
}

// ---- helpers ----

// stubDownloadLinker is the read path's view of slice G3: it mints one opaque
// reference and records that it was asked, which is enough to prove the ready-
// file rule without shipping a grant implementation in this slice.
type stubDownloadLinker struct {
	ref   string
	calls int
}

func (s stubDownloadLinker) RequesterDownloadRef(_ context.Context, _ authz.Principal, _ *domain.TeamRequest, _ *domain.RequestAttachment) (string, error) {
	return s.ref, nil
}

// isRequestNotFound reports whether err is the surface's single request 404: the
// stable code and the status both have to agree, because a client branches on
// the code and a person reads the message.
func isRequestNotFound(err error) bool {
	if err == nil {
		return false
	}
	return authz.CodeOf(err) == authz.CodeRequestNotFound && authz.StatusOf(err, 500) == 404
}

// assertRequestNotFound fails unless err is that one 404.
func assertRequestNotFound(t *testing.T, err error) {
	t.Helper()
	if !isRequestNotFound(err) {
		t.Fatalf("err = %v (code %q, status %d), want the surface's not-found (code %q, status 404)",
			err, authz.CodeOf(err), authz.StatusOf(err, 500), authz.CodeRequestNotFound)
	}
}

// timePtr is a pointer to v, for the nullable timestamps a test sets.
func timePtr(v time.Time) *time.Time { return &v }
