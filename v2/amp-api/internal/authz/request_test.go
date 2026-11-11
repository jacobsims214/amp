package authz

// Requester-scoped request visibility and the stable request.* codes (AMP
// #3771, build slice B4; approved contract artifact 36 v2 §2 and §5).
//
// Three claims, one test each:
//
//   - the visibility matrix decides correctly for a requester, a team member
//     and a stranger — including the cases the contract calls out: a grant
//     holder who is in no team reads their own request but not the inbox, a
//     project Level on the inbox's own project reached through another team
//     confers nothing, and a team member who is also the requester gets the
//     full team view rather than the projection
//     (TestRequestViewMatrix);
//   - a foreign request and a request that does not exist produce the same
//     refusal, byte for byte: same status, same code, same message, so nothing
//     about another tenant's existence, team, state or requester leaks
//     (TestRequestNotFoundIsIndistinguishable);
//   - every request.* code keeps its status through StatusOf, and no code
//     carries private detail (TestRequestCodes).
//
// The pure decision is tested without a database; the Checker-level tests
// (InboxRole / RequestAccess / ViewForLoad) run against a real Postgres and the
// real migrations when TEST_DATABASE_URL is set, like the TeamSubmit tests:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/authz -run 'Request|InboxRole' -count=1

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// requestAt builds a request owned by requester (0 for "no requester user", a
// connector or service submission) on team in org.
func requestAt(id, orgID, teamID int, requester int) *domain.TeamRequest {
	r := &domain.TeamRequest{
		ID: id, OrgID: orgID, TeamID: teamID, PublicRef: fmt.Sprintf("ref%014d", id),
		State: domain.RequestStatusTriaging, Rev: 1,
	}
	if requester != 0 {
		u := requester
		r.RequesterUserID = &u
	}
	return r
}

// TestRequestViewMatrix is the requester / team / stranger matrix. Rows are
// actors, columns are the request they ask about, and every cell states the
// view it must produce.
func TestRequestViewMatrix(t *testing.T) {
	const (
		org        = 1
		otherOrg   = 2
		inboxTeam  = 10
		peerTeam   = 11
		foreign    = 12
		requester  = 100 // the customer who submitted it
		otherCust  = 101 // another customer, also granted
		teamMember = 200
		teamLead   = 201
		orgAdmin   = 202
		peerMember = 203 // a member of a peer team on the same project
		plainOrg   = 204 // an org member with no team role
		stranger   = 205 // owns another org
		agent      = 206 // agent account, no role of its own
	)

	// The request under test belongs to inboxTeam in org, submitted by requester.
	own := requestAt(1, org, inboxTeam, requester)
	// A request with no requester user at all (a connector or service client
	// submission): it belongs to nobody's "own" view.
	serviceSubmitted := requestAt(2, org, inboxTeam, 0)
	// A request in another tenant, to prove a foreign id is not merely unowned.
	foreignReq := requestAt(3, otherOrg, foreign, 900)

	type row struct {
		name   string
		me     int
		role   InboxRole
		req    *domain.TeamRequest
		want   RequestView
		reason string
	}
	team := TeamRequestView()
	mine := OwnRequestView()
	none := NoRequestView()

	rows := []row{
		// The team: full record, whoever submitted it.
		{"team member on a requester's request", teamMember, InboxMember, own, team,
			"triage needs the whole record"},
		{"team lead on a requester's request", teamLead, InboxMember, own, team,
			"a lead reads the inbox"},
		{"org owner/admin on a requester's request", orgAdmin, InboxMember, own, team,
			"an org admin reads the inbox"},
		{"global admin on a requester's request", 1, InboxMember, own, team,
			"Super reads everything"},
		{"team member on a service-submitted request", teamMember, InboxMember, serviceSubmitted, team,
			"the team reads requests that have no requester user"},
		{"team member on a foreign request", teamMember, InboxMember, foreignReq, team,
			"the role is resolved for the request's own team; this row is a control for the pure function"},

		// The requester: own request only, and only as a projection.
		{"requester on their own request", requester, InboxNone, own, mine,
			"own request, own view — no project membership required"},
		{"requester on their own request while also a team member", requester, InboxMember, own, team,
			"the team view wins: a triager who submitted sees the record they triage"},
		{"requester on another requester's request", requester, InboxNone, requestAt(4, org, inboxTeam, otherCust), none,
			"customer-to-customer visibility is out of v1"},
		{"requester on a service-submitted request", requester, InboxNone, serviceSubmitted, none,
			"no requester user means no one's own view"},
		{"requester on a request in another tenant", requester, InboxNone, foreignReq, none,
			"tenancy comes from the request, never from the caller"},

		// The stranger: nothing, and no way to tell a real id from a made-up one.
		{"peer-team member on the inbox request", peerMember, InboxNone, own, none,
			"a peer team on the same project is not this inbox"},
		{"plain org member on the inbox request", plainOrg, InboxNone, own, none,
			"org membership alone grants nothing"},
		{"agent with no role of its own on the inbox request", agent, InboxNone, own, none,
			"an agent account holds its own access; it inherits nothing"},
		{"owner of another org on the inbox request", stranger, InboxNone, own, none,
			"tenancy: another org's owner reaches nothing"},
		{"no AMP user at all on the inbox request", 0, InboxNone, own, none,
			"no user, no requester column to match"},
		{"team member on a nil request", teamMember, InboxMember, nil, none,
			"a request that isn't there has no view"},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got := RequestViewFor(tc.req, tc.me, tc.role)
			if got != tc.want {
				t.Fatalf("RequestViewFor = %s, want %s (%s)", got, tc.want, tc.reason)
			}
			if can := CanViewRequest(tc.req, tc.me, tc.role); can != !tc.want.None() {
				t.Fatalf("CanViewRequest = %v, want %v", can, !tc.want.None())
			}
		})
	}
}

// TestFilterRequests is the list half: a team view keeps the rows it was given,
// an own view keeps only the caller's own, and no view keeps nothing.
func TestFilterRequests(t *testing.T) {
	reqs := []domain.TeamRequest{
		*requestAt(1, 1, 10, 100), // mine
		*requestAt(2, 1, 10, 101), // someone else's
		*requestAt(3, 1, 10, 0),   // no requester user
		*requestAt(4, 1, 10, 100), // mine
	}
	ids := func(rs []domain.TeamRequest) []int {
		out := make([]int, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids(FilterRequests(reqs, TeamRequestView(), 100)); len(got) != 4 {
		t.Fatalf("team view = %v, want all four", got)
	}
	if got := ids(FilterRequests(reqs, OwnRequestView(), 100)); fmt.Sprint(got) != "[1 4]" {
		t.Fatalf("own view = %v, want [1 4]", got)
	}
	if got := FilterRequests(reqs, NoRequestView(), 100); len(got) != 0 {
		t.Fatalf("no view = %v, want nothing", ids(got))
	}
	// me == 0 matches nobody's own view, not even the rows with no requester.
	if got := FilterRequests(reqs, OwnRequestView(), 0); len(got) != 0 {
		t.Fatalf("own view with no user = %v, want nothing", ids(got))
	}
}

// TestRequestNotFoundIsIndistinguishable is the contract's "existence never
// leaks" rule, asserted on the refusal itself rather than on a handler: a
// request that does not exist and a request that is not yours must be the same
// bytes, because a difference is exactly what lets someone walk ids.
func TestRequestNotFoundIsIndistinguishable(t *testing.T) {
	unknown := RequestNotFound()
	foreign := RequestNotFound()

	for _, err := range []error{unknown, foreign} {
		if got := StatusOf(err, 500); got != 404 {
			t.Fatalf("RequestNotFound status = %d, want 404", got)
		}
		if got := CodeOf(err); got != CodeRequestNotFound {
			t.Fatalf("RequestNotFound code = %q, want %q", got, CodeRequestNotFound)
		}
	}
	if unknown.Error() != foreign.Error() {
		t.Fatalf("messages differ: %q vs %q", unknown.Error(), foreign.Error())
	}

	// The message is pinned exactly, not merely scanned for suspicious words:
	// the only way this refusal can stay indistinguishable from the one for a
	// request that does not exist is to be a constant. Any id, ref, team, state
	// or requester interpolated into it would be something a probe could read,
	// and pinning the whole string is what makes that a test failure rather
	// than a judgement call about which words count as a leak.
	const want = "request not found"
	if got := unknown.Error(); got != want {
		t.Fatalf("RequestNotFound message = %q, want exactly %q — a refusal that varies tells a probe something", got, want)
	}
	// And the refusal has no formatting verbs at all, so no caller can hand it
	// an id to interpolate.
	for _, verb := range []string{"%d", "%s", "%v", "%q"} {
		if strings.Contains(want, verb) {
			t.Fatalf("the not-found message carries the verb %q", verb)
		}
	}
}

// TestRequestCodes is the code contract: each request.* code carries its status
// through StatusOf, is an *authz.Error a listener can map, and says nothing
// private.
func TestRequestCodes(t *testing.T) {
	rows := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		// wantNot is text the message must not carry: a rejected value echoed
		// back, or anything naming the tenant.
		wantNot []string
	}{
		{"not found", RequestNotFound(), 404, CodeRequestNotFound, []string{"org", "team"}},
		{"conflict on a stale rev", RequestConflict("this request changed since you loaded it (now rev %d); reload and reapply", 7), 409, CodeRequestConflict, nil},
		{"invalid submission", RequestInvalid("subject is required"), 400, CodeRequestInvalid, nil},
		{"rate limited", RequestRateLimited(30, "too many submissions from this address"), 429, CodeRequestRateLimited, nil},
		{"rate limited with no reason", RequestRateLimited(0, ""), 429, CodeRequestRateLimited, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := StatusOf(row.err, 500); got != row.wantStatus {
				t.Fatalf("StatusOf = %d, want %d", got, row.wantStatus)
			}
			if got := CodeOf(row.err); got != row.wantCode {
				t.Fatalf("CodeOf = %q, want %q", got, row.wantCode)
			}
			var ae *Error
			if !errors.As(row.err, &ae) {
				t.Fatalf("%v is not an authz error, so a listener could not map it", row.err)
			}
			if ae.Code != row.wantCode || ae.Status != row.wantStatus {
				t.Fatalf("carried status/code = %d/%q, want %d/%q", ae.Status, ae.Code, row.wantStatus, row.wantCode)
			}
			if ae.Msg == "" {
				t.Fatal("refusal has no message for a person to read")
			}
			for _, not := range row.wantNot {
				if strings.Contains(ae.Msg, not) {
					t.Fatalf("message %q contains %q", ae.Msg, not)
				}
			}
		})
	}

	// The rate limit's wait is stated and never less than a second: a "retry
	// now" that cannot succeed is worse than no hint at all.
	for _, row := range []struct {
		given int
		want  string
	}{{30, "30s"}, {0, "1s"}, {-5, "1s"}} {
		msg := RequestRateLimited(row.given, "").Error()
		if !strings.Contains(msg, row.want) {
			t.Fatalf("RequestRateLimited(%d) = %q, want the wait %q in it", row.given, msg, row.want)
		}
	}
}

// ---- Checker-level: the role is read from the rows, every call ----

// requestFixture is one tenant for the Checker tests: an inbox team with a lead
// and a member, an org admin, a peer team on the same project, a customer with a
// grant, a plain org member, a stranger from another org, and an agent account.
type requestFixture struct {
	t     *testing.T
	repo  *repository.Repo
	db    *pgx.Conn
	check *Checker
	ctx   context.Context

	org, otherOrg                 int
	team, peerTeam, foreignTeam   int
	project                       int
	lead, member, admin           int
	peer, plainOrg, stranger      int
	cust, otherCust, agent        int
	super                         int
	requester, otherRequesterUser int
}

func newRequestFixture(t *testing.T) *requestFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the request-visibility tests against Postgres")
	}
	ctx := context.Background()

	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(ctx, teamSubmitMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (fixture): %v", err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })

	suffix := rand.IntN(1_000_000_000)
	f := &requestFixture{t: t, repo: repo, db: db, check: NewChecker(repo), ctx: ctx}

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

	f.org = q(`INSERT INTO orgs (name, slug) VALUES ('view org', $1) RETURNING id`, fmt.Sprintf("view-%d", suffix))
	f.otherOrg = q(`INSERT INTO orgs (name, slug) VALUES ('view other', $1) RETURNING id`, fmt.Sprintf("view-other-%d", suffix))
	f.team = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, f.org)
	f.peerTeam = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Peer team') RETURNING id`, f.org)
	f.foreignTeam = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign desk') RETURNING id`, f.otherOrg)
	f.project = q(`INSERT INTO projects (name, code, org_id) VALUES ('Inbox work', $1, $2) RETURNING id`,
		fmt.Sprintf("VIW%d", suffix), f.org)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, f.project, f.team)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, f.project, f.peerTeam)

	user := func(handle, kind string) int {
		t.Helper()
		return q(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
			fmt.Sprintf("%s-%d", handle, suffix), fmt.Sprintf("%s-%d@view.test", handle, suffix), kind)
	}
	f.lead = user("view-lead", domain.UserKindHuman)
	f.member = user("view-member", domain.UserKindHuman)
	f.admin = user("view-admin", domain.UserKindHuman)
	f.peer = user("view-peer", domain.UserKindHuman)
	f.plainOrg = user("view-plainorg", domain.UserKindHuman)
	f.stranger = user("view-stranger", domain.UserKindHuman)
	f.cust = user("view-cust", domain.UserKindCustomer)
	f.otherCust = user("view-othercust", domain.UserKindCustomer)
	f.super = user("view-super", domain.UserKindHuman)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.org, f.admin)
	for _, u := range []int{f.lead, f.member, f.peer, f.plainOrg} {
		exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, f.org, u)
	}
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.otherOrg, f.stranger)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, f.team, f.lead)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, f.team, f.member)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, f.peerTeam, f.peer)
	exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, f.super)
	f.agent = q(`INSERT INTO users (subject, email, kind, managed_by) VALUES ($1, $2, 'agent', $3) RETURNING id`,
		fmt.Sprintf("view-agent-%d", suffix), fmt.Sprintf("view-agent-%d@view.test", suffix), f.lead)

	// Both customers hold a grant: a grant is submit access, never read access,
	// and neither is on the team or in the org.
	for _, u := range []int{f.cust, f.otherCust} {
		if _, err := f.repo.GrantTeamSubmit(ctx, f.team, f.org, u, &f.lead); err != nil {
			t.Fatalf("GrantTeamSubmit(%d): %v", u, err)
		}
	}

	f.requester, f.otherRequesterUser = f.cust, f.otherCust
	return f
}

func (f *requestFixture) principal(userID int) Principal {
	return Principal{UserID: userID, Email: fmt.Sprintf("user-%d@view.test", userID)}
}

// insertRequest writes a team_requests row and returns its id. The read path
// this ticket builds has no repository method of its own yet (slice C5 lands
// GetRequest), so the fixture writes and reads the row directly: the decision
// under test takes a loaded *domain.TeamRequest and never touches the database
// for the record itself.
func (f *requestFixture) insertRequest(teamID, orgID int, requester int) int {
	f.t.Helper()
	ref := fmt.Sprintf("ref%08d%06d", teamID, rand.IntN(1_000_000))
	var id int
	err := f.db.QueryRow(f.ctx,
		`INSERT INTO team_requests (public_ref, org_id, team_id, requester_user_id, requester_contact, subject, state)
		 VALUES ($1, $2, $3, $4, 'someone@view.test', 'please build a thing', 'triaging') RETURNING id`,
		ref, orgID, teamID, requester).Scan(&id)
	if err != nil {
		f.t.Fatalf("insert request: %v", err)
	}
	return id
}

// getRequest loads one request the way a handler will: the row as the database
// holds it, into the domain type the decision reads.
func (f *requestFixture) getRequest(id int) *domain.TeamRequest {
	f.t.Helper()
	r := &domain.TeamRequest{}
	err := f.db.QueryRow(f.ctx, `
		SELECT id, public_ref, org_id, team_id, requester_user_id, requester_contact,
		       source_kind, source_external_id, subject, input_brief, outcome_brief,
		       state, claim_user_id, claimed_at, owner_user_id, duplicate_of, reject_reason,
		       needs_info_since, planned_at, delivered_at, accepted_at, accepted_by,
		       closed_at, reopened_at, reopened_count, rev, metadata, created_at, updated_at
		FROM team_requests WHERE id = $1`, id).Scan(
		&r.ID, &r.PublicRef, &r.OrgID, &r.TeamID, &r.RequesterUserID, &r.RequesterContact,
		&r.SourceKind, &r.SourceExternalID, &r.Subject, &r.InputBrief, &r.OutcomeBrief,
		&r.State, &r.ClaimUserID, &r.ClaimedAt, &r.OwnerUserID, &r.DuplicateOf, &r.RejectReason,
		&r.NeedsInfoSince, &r.PlannedAt, &r.DeliveredAt, &r.AcceptedAt, &r.AcceptedBy,
		&r.ClosedAt, &r.ReopenedAt, &r.ReopenedCount, &r.Rev, &r.Metadata, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		f.t.Fatalf("get request %d: %v", id, err)
	}
	return r
}

// TestInboxRoleMatrix is the Checker's half of the matrix: the standing is read
// from the rows, and it is exactly the read allow-set (no grant path).
func TestInboxRoleMatrix(t *testing.T) {
	f := newRequestFixture(t)

	rows := []struct {
		name string
		p    Principal
		team int
		want InboxRole
	}{
		{"team lead", f.principal(f.lead), f.team, InboxMember},
		{"team member", f.principal(f.member), f.team, InboxMember},
		{"org owner/admin", f.principal(f.admin), f.team, InboxMember},
		{"global admin", Principal{UserID: f.super, Super: true}, f.team, InboxMember},

		{"peer-team member on the same project", f.principal(f.peer), f.team, InboxNone},
		{"plain org member", f.principal(f.plainOrg), f.team, InboxNone},
		{"grant holder in no team and no org", f.principal(f.cust), f.team, InboxNone},
		{"owner of another org", f.principal(f.stranger), f.team, InboxNone},
		{"agent account with no role of its own", f.principal(f.agent), f.team, InboxNone},
		{"no AMP user and not Super", Principal{}, f.team, InboxNone},
		{"a team that does not exist", f.principal(f.admin), 1 << 30, InboxNone},
		// A global admin asking about a team that isn't there gets "no
		// standing", not "I'm a global admin, so yes": the team is loaded
		// before the Super short-circuit, the same order TeamSubmit uses.
		{"global admin on a team that does not exist", Principal{UserID: f.super, Super: true}, 1 << 30, InboxNone},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got, err := f.check.InboxRole(f.ctx, row.p, row.team)
			if err != nil {
				t.Fatalf("InboxRole: %v", err)
			}
			if got != row.want {
				t.Fatalf("InboxRole = %s, want %s", got, row.want)
			}
		})
	}

	// The grant is per team: holding one on the inbox does not open the peer
	// team's inbox.
	if got, err := f.check.InboxRole(f.ctx, f.principal(f.cust), f.peerTeam); err != nil || got != InboxNone {
		t.Fatalf("grant holder on the peer team = %s (%v), want none", got, err)
	}
	// And nothing opens a team in another org.
	if got, err := f.check.InboxRole(f.ctx, f.principal(f.admin), f.foreignTeam); err != nil || got != InboxNone {
		t.Fatalf("org admin on another org's team = %s (%v), want none", got, err)
	}
}

// TestRequestAccessReadsRowsEveryCall proves the decision is re-derived on every
// call: the same Checker, nothing rebuilt, and removing the membership (or
// adding it back) changes the answer on the very next request.
func TestRequestAccessReadsRowsEveryCall(t *testing.T) {
	f := newRequestFixture(t)
	id := f.insertRequest(f.team, f.org, f.requester)
	r := f.getRequest(id)
	p := f.principal(f.member)

	view, err := f.check.RequestAccess(f.ctx, p, r)
	if err != nil || !view.Full() {
		t.Fatalf("team member view = %s (%v), want the team view", view, err)
	}
	if _, err := f.db.Exec(f.ctx, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, f.team, f.member); err != nil {
		t.Fatalf("remove membership: %v", err)
	}
	view, err = f.check.RequestAccess(f.ctx, p, r)
	if err != nil {
		t.Fatalf("after removal: %v", err)
	}
	if !view.None() {
		t.Fatalf("after removal the view = %s, want none — nothing may be cached", view)
	}
}

// TestViewForLoadIsTheOnlyNotFound is the structural half of the
// indistinguishability rule: the read path gets the same error whether the
// request is missing, foreign, or invisible, and a real failure stays a real
// failure rather than being turned into a 404.
func TestViewForLoadIsTheOnlyNotFound(t *testing.T) {
	f := newRequestFixture(t)
	own := f.insertRequest(f.team, f.org, f.requester)
	foreign := f.insertRequest(f.foreignTeam, f.otherOrg, f.stranger)

	loadedOwn := f.getRequest(own)
	loadedForeign := f.getRequest(foreign)

	requester := f.principal(f.requester)
	teamMember := f.principal(f.member)

	// The requester reads their own request.
	view, err := f.check.ViewForLoad(f.ctx, requester, loadedOwn, nil)
	if err != nil || !view.Own() {
		t.Fatalf("requester on their own = %s (%v), want the own view", view, err)
	}
	// The team reads it in full.
	view, err = f.check.ViewForLoad(f.ctx, teamMember, loadedOwn, nil)
	if err != nil || !view.Full() {
		t.Fatalf("team member = %s (%v), want the team view", view, err)
	}

	// Another requester's request, an unknown id and a missing row are one
	// refusal. Compare the three errors field by field, not just by status.
	other := f.insertRequest(f.team, f.org, f.otherRequesterUser)
	loadedOther := f.getRequest(other)
	cases := map[string]error{}
	_, cases["another requester's request"] = f.check.ViewForLoad(f.ctx, requester, loadedOther, nil)
	_, cases["an id that does not exist"] = f.check.ViewForLoad(f.ctx, requester, nil, fmt.Errorf("request %d: %w", 1<<30, repository.ErrNotFound))
	_, cases["a request in another org"] = f.check.ViewForLoad(f.ctx, requester, loadedForeign, nil)
	_, cases["a stranger's read of the inbox request"] = f.check.ViewForLoad(f.ctx, f.principal(f.plainOrg), loadedOwn, nil)

	want := RequestNotFound()
	var wantAE *Error
	if !errors.As(want, &wantAE) {
		t.Fatalf("RequestNotFound is not an authz error")
	}
	for name, err := range cases {
		var gotAE *Error
		if !errors.As(err, &gotAE) {
			t.Fatalf("%s: %v is not an authz error", name, err)
		}
		if gotAE.Status != wantAE.Status || gotAE.Code != wantAE.Code || gotAE.Msg != wantAE.Msg {
			t.Fatalf("%s: refusal = {%d %q %q}, want {%d %q %q} — existence must never leak",
				name, gotAE.Status, gotAE.Code, gotAE.Msg, wantAE.Status, wantAE.Code, wantAE.Msg)
		}
	}

	// A genuine failure is not disguised as "not found": a listener must be
	// able to tell "I can't see it" from "the database broke".
	boom := errors.New("connection reset")
	if _, err := f.check.ViewForLoad(f.ctx, requester, nil, boom); !errors.Is(err, boom) {
		t.Fatalf("a load failure = %v, want the underlying error", err)
	}
}
