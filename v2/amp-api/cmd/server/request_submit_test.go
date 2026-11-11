package main

// Service-level regression suite for team-request submission: the tenant and
// principal boundaries, revocation, and what a retry actually costs (AMP #3900,
// epic 202, story 540; build slice S1 of
// discoveries/team-inbox-approved-build-slices.md).
//
// This file is the wave's *boundary* proof, not the feature's unit test. #3899
// shipped the happy path and the refusal list in
// internal/service/request_submit_test.go (ten tests against its own criteria);
// this suite asks the questions a reviewer asks about the same call from the
// outside — the ones that decide whether an inbox can be exposed to customers,
// connectors and submit-only credentials:
//
//  1. Tenancy and principals. TestRequestSubmit_TenantAndPrincipalMatrix is the
//     ticket's matrix: a team member, a customer holding an in-force submit
//     grant, a customer with no grant, a revoked grant, a team in another org, an
//     unknown team, a submit-only credential on its own team and on a foreign
//     team, and the other tenant's own owner. Every row asserts the answer
//     (created, or refused with a status and stable code) *and* its consequence
//     (the tenant-scoped request count after it), so a refusal that wrote a row
//     fails at the row that caused it, and a permitted submission that landed in
//     another tenant fails too.
//  2. Nothing gained and nothing leaked. TestRequestSubmit_GrantsNoProjectAccess
//     measures project visibility before and after for a member, a granted
//     customer, a submit-only credential and another tenant's owner: submitting
//     may never add a project, and a grant holder still reaches zero.
//     TestRequestSubmit_ForeignConnectorIDNeverLeaksAContact covers the boundary
//     a connector's external id opens — two permitted submitters, one vendor
//     object — and pins that the second gets the request surface's not-found
//     rather than the first requester's contact snapshot.
//  3. A retry costs one request. TestRequestSubmit_ConcurrentSameKeyCreatesOnce
//     races one key from many goroutines and asserts one request row, one
//     `submit` history row, one audit row and one unit of quota — measured
//     behaviourally, because a private counter is not the contract.
//     TestRequestSubmit_BurstLargerThanTheCeilingNeverDuplicates pins the other
//     side of the same rule: a retry burst larger than the window's budget is
//     partly refused with the coded 429 (the limiter is charged before the
//     repository can recognise a replay) and still never produces a second
//     request.
//  4. Fault injection. TestRequestSubmit_FaultedCreateThenRetryCreatesOnce fails
//     the create's own transaction with a plpgsql trigger (the injector idiom
//     internal/repository/request_create_test.go established), proves nothing
//     survived, and then retries the same submission, which creates.
//  5. Revocation takes effect immediately.
//     TestRequestSubmit_RevocationBetweenCreateAndReplay revokes the grant
//     between a create and its retry: the retry is refused at the gate with the
//     team surface's not-found, leaks no request reference, and writes nothing.
//  6. Quota. TestRequestSubmit_RateLimitRetryAfterAndReplayCost drives the limiter
//     to its exact ceiling: a replay is free, a conflict spends its unit, the
//     refusal is the coded 429 carrying the remaining seconds, it wrote nothing,
//     and the next window is a fresh budget.
//
// The database is a scratch database per test (freshDatabase, the CREATEDB-based
// helper the other cmd/server suites use), which is stronger than the
// unique-suffix convention: nothing this file writes is visible to any other
// test, so the audit and history counts below are exact and not "filtered by
// suffix". Every row lives in a database dropped when the test ends.
//
// Run against a disposable Postgres (never a live stack):
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRequestSubmit' -count=1 -race -v
//
// Deliberately NOT here: no REST or MCP surface (neither exists for submission
// yet; slices F1/I1 add them over this same service call), no implementation
// change, and no lifecycle assertions — intake lands a request in `new`, and the
// one transition function is slice S5.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// ---- fixture ----

// reqSubmitDB is one test's tenant, on a scratch database of its own with every
// migration applied.
type reqSubmitDB struct {
	t   *testing.T
	db  *pgx.Conn
	ctx context.Context
	svc *service.Service
	// now is the clock the service reads, so a window boundary is an instant a
	// test chooses rather than a sleep.
	now time.Time

	org     int // the inbox tenant
	team    int // the inbox: the only team a submission may be aimed at
	project int // linked to team; the project "access gained" is measured against
	// otherTeam is a second team in the same org that submissions must not reach.
	otherTeam int
	// otherOrg and foreignTeam are a second tenant.
	otherOrg    int
	foreignTeam int

	member   int // team member (no project role beyond the team link)
	admin    int // owner of org
	customer int // customer kind, holds an in-force submit grant on team
	other    int // customer kind, member of otherOrg only, no grant anywhere
	stranger int // owner of otherOrg
}

// newReqSubmitDB builds the fixture with a limiter generous enough that quota is
// never the reason a call fails; the quota tests build their own ceiling.
func newReqSubmitDB(t *testing.T) *reqSubmitDB {
	return newReqSubmitDBLimit(t, 30)
}

// newReqSubmitDBLimit is newReqSubmitDB with an explicit per-window ceiling, so a
// quota assertion is one probe away: spend to the ceiling, then observe the
// refusal.
func newReqSubmitDBLimit(t *testing.T, maxPerWindow int) *reqSubmitDB {
	t.Helper()
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })

	d := &reqSubmitDB{
		t: t, db: db, ctx: ctx,
		now: time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC),
	}
	insert := func(sql string, args ...any) int {
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
	suffix := uniq("reqsubmit")
	user := func(label, kind string) int {
		return insert(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
			fmt.Sprintf("%s-%s", label, suffix),
			fmt.Sprintf("%s-%s@reqsubmit.test", label, suffix), kind)
	}

	d.org = insert(`INSERT INTO orgs (name, slug) VALUES ('Submit matrix org', $1) RETURNING id`, suffix)
	d.team = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, d.org)
	d.otherTeam = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other desk') RETURNING id`, d.org)
	d.project = insert(`INSERT INTO projects (name, code, org_id) VALUES ('Delivered work', $1, $2) RETURNING id`,
		strings.ToUpper(suffix[:6]), d.org)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, d.project, d.team)
	d.otherOrg = insert(`INSERT INTO orgs (name, slug) VALUES ('Submit matrix other org', $1) RETURNING id`, suffix+"-o")
	d.foreignTeam = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign inbox') RETURNING id`, d.otherOrg)

	d.member = user("member", domain.UserKindHuman)
	d.admin = user("admin", domain.UserKindHuman)
	d.customer = user("customer", domain.UserKindCustomer)
	d.other = user("othercustomer", domain.UserKindCustomer)
	d.stranger = user("stranger", domain.UserKindHuman)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, d.org, d.member)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, d.org, d.admin)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, d.team, d.member)
	// other is a member of the *other* org and nothing here: the authenticated
	// but not permitted row. stranger owns that other org.
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, d.otherOrg, d.other)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, d.otherOrg, d.stranger)
	if _, err := repo.GrantTeamSubmit(ctx, d.team, d.org, d.customer, nil); err != nil {
		t.Fatalf("grant the customer: %v", err)
	}

	d.svc = service.New(repo, nil, nil)
	d.svc.RequestLimits = service.NewRequestLimiter(service.RequestLimitConfig{Window: time.Minute, Max: maxPerWindow})
	d.svc.Now = func() time.Time { return d.now }
	return d
}

// principal builds a signed-in caller for userID: a person in the web UI unless
// the test says otherwise.
func (d *reqSubmitDB) principal(userID int) authz.Principal {
	d.t.Helper()
	var email string
	if err := d.db.QueryRow(d.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		d.t.Fatalf("email of %d: %v", userID, err)
	}
	return authz.Principal{UserID: userID, Email: email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
}

// scoped builds the submit-only credential bound to teamID, presenting it the way
// the token path does — through the principal's scope fields, never a payload.
func (d *reqSubmitDB) scoped(userID, teamID int) authz.Principal {
	p := d.principal(userID)
	p.Scope, p.ScopeTeamID, p.ViaToken = domain.TokenScopeSubmit, teamID, true
	p.Channel, p.AuthMethod = auth.ChannelToken, auth.AuthAPIToken
	return p
}

// submission is one well-formed submission to the inbox team, as a web adapter
// would build it: no identity, no org, no project, an optional key.
func (d *reqSubmitDB) submission(key string) service.SubmitRequestInput {
	return service.SubmitRequestInput{
		TeamID:         d.team,
		Source:         domain.RequestSourceWeb,
		Subject:        "Please add a CSV export of the queue",
		InputBrief:     "The triage view has no export; we need the queue as CSV weekly.",
		Metadata:       domain.Metadata{"connector_label": "support-desk"},
		IdempotencyKey: key,
	}
}

// count is the number of rows a query matches; args are bound in order.
func (d *reqSubmitDB) count(query string, args ...any) int {
	d.t.Helper()
	var n int
	if err := d.db.QueryRow(d.ctx, query, args...).Scan(&n); err != nil {
		d.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// requestsIn is how many request rows exist in orgID. Every matrix row uses the
// tenant-scoped count, so a write into the wrong tenant cannot hide in a total.
func (d *reqSubmitDB) requestsIn(orgID int) int {
	return d.count(`SELECT count(*) FROM team_requests WHERE org_id = $1`, orgID)
}

// submitEvents is how many `submit` history rows requestID has.
func (d *reqSubmitDB) submitEvents(requestID int) int {
	return d.count(`SELECT count(*) FROM request_events WHERE request_id = $1 AND action = 'submit'`, requestID)
}

// auditsFor is every `request.submit` audit row targeting requestID.
func (d *reqSubmitDB) auditsFor(requestID int) []domain.AuditEntry {
	d.t.Helper()
	entries, err := d.svc.Repo.ListAudit(d.ctx, domain.AuditFilter{
		Action: "request.submit", TargetID: fmt.Sprint(requestID),
	})
	if err != nil {
		d.t.Fatalf("list audit for request %d: %v", requestID, err)
	}
	return entries
}

// tableCount is how many rows a table holds in total, for the "no membership was
// written" assertions. table is a literal from this file, never input.
func (d *reqSubmitDB) tableCount(table string) int {
	d.t.Helper()
	return d.count(`SELECT count(*) FROM ` + table)
}

// visibleProjects is the set of projects p may reach, through the same query the
// authorization layer uses.
func (d *reqSubmitDB) visibleProjects(p authz.Principal) map[int]bool {
	d.t.Helper()
	ids, err := d.svc.Authz.VisibleProjects(d.ctx, p)
	if err != nil {
		d.t.Fatalf("visible projects of %d: %v", p.UserID, err)
	}
	return ids
}

// deny asserts a submission was refused with status and, when non-empty, the
// stable code. It returns the error for a caller that wants to read it.
func deny(t *testing.T, err error, status int, code string) error {
	t.Helper()
	if err == nil {
		t.Fatalf("the submission was accepted, want a %d refusal (%q)", status, code)
	}
	if got := authz.StatusOf(err, 500); got != status {
		t.Fatalf("refusal = %v (status %d), want status %d", err, got, status)
	}
	if code != "" {
		if got := authz.CodeOf(err); got != code {
			t.Fatalf("refusal code = %q (%v), want %q", got, err, code)
		}
	}
	return err
}

// ---- criterion 1: the principal and tenant matrix ----

// TestRequestSubmit_TenantAndPrincipalMatrix is the ticket's matrix: who may
// submit to which team, and what a refusal leaves behind.
func TestRequestSubmit_TenantAndPrincipalMatrix(t *testing.T) {
	d := newReqSubmitDB(t)

	membershipsBefore := d.tableCount("org_members") + d.tableCount("team_members") +
		d.tableCount("project_teams") + d.tableCount("team_submit_grants")

	type row struct {
		name string
		// principal is built per row so a row can carry a scoped credential (and
		// so a revocation row can revoke immediately before the call).
		principal func() authz.Principal
		in        func() service.SubmitRequestInput
		// wantOK means the submission must be created.
		wantOK bool
		// status and code describe the refusal; ignored when wantOK.
		status int
		code   string
		// wantRequestsIn is the tenant-scoped request count expected afterwards.
		wantRequestsIn int
	}
	rows := []row{
		{
			// The inbox's own member: team membership alone, no grant.
			name:      "team member submits to their own inbox",
			principal: func() authz.Principal { return d.principal(d.member) },
			in:        func() service.SubmitRequestInput { return d.submission("") },
			wantOK:    true, wantRequestsIn: 1,
		},
		{
			// A customer holding an in-force submit grant: authenticated, not a
			// team member, permitted by the grant alone.
			name:      "customer with an in-force grant submits",
			principal: func() authz.Principal { return d.principal(d.customer) },
			in: func() service.SubmitRequestInput {
				in := d.submission("")
				in.Source = domain.RequestSourceMCP
				return in
			},
			wantOK: true, wantRequestsIn: 2,
		},
		{
			// Authenticated in AMP, in no tenant that owns this team: the answer
			// is "not found", not "forbidden", so the refusal says nothing about
			// whether this team exists.
			name:      "a customer from another org is not found",
			principal: func() authz.Principal { return d.principal(d.other) },
			in:        func() service.SubmitRequestInput { return d.submission("") },
			status:    404, wantRequestsIn: 2,
		},
		{
			// A revoked grant is gone on the very next call: there is no cache to
			// invalidate (the per-call re-check), so this row is unambiguously
			// "revoked, then attempted".
			name: "a revoked grant is refused on the next call",
			principal: func() authz.Principal {
				if _, err := d.svc.Repo.RevokeTeamSubmit(d.ctx, d.team, d.customer, nil); err != nil {
					d.t.Fatalf("revoke: %v", err)
				}
				return d.principal(d.customer)
			},
			in:     func() service.SubmitRequestInput { return d.submission("") },
			status: 404, wantRequestsIn: 2,
		},
		{
			// A submit-only credential on the team it is bound to, with the
			// account's grant restored, so the scope's own-account rule passes: the
			// scope narrows, it never substitutes for the account's standing.
			name: "submit-only credential on its own team submits",
			principal: func() authz.Principal {
				if _, err := d.svc.Repo.GrantTeamSubmit(d.ctx, d.team, d.org, d.customer, nil); err != nil {
					d.t.Fatalf("re-grant: %v", err)
				}
				return d.scoped(d.customer, d.team)
			},
			in: func() service.SubmitRequestInput {
				in := d.submission("")
				in.Source = domain.RequestSourceAPI
				return in
			},
			wantOK: true, wantRequestsIn: 3,
		},
		{
			// The same credential aimed at a team it is not bound to.
			name:      "submit-only credential on a foreign team is not found",
			principal: func() authz.Principal { return d.scoped(d.customer, d.team) },
			in: func() service.SubmitRequestInput {
				in := d.submission("")
				in.TeamID = d.otherTeam
				return in
			},
			status: 404, wantRequestsIn: 3,
		},
		{
			// A team in another org, aimed at by *this* org's owner (who may submit
			// anywhere in their own org).
			name:      "cross-org team is not found for this org's owner",
			principal: func() authz.Principal { return d.principal(d.admin) },
			in: func() service.SubmitRequestInput {
				in := d.submission("")
				in.TeamID = d.foreignTeam
				return in
			},
			status: 404, wantRequestsIn: 3,
		},
		{
			// A grant is scoped to its team, so it cannot be pointed at another
			// tenant's inbox.
			name:      "a granted customer cannot reach another org's team",
			principal: func() authz.Principal { return d.principal(d.customer) },
			in: func() service.SubmitRequestInput {
				in := d.submission("")
				in.TeamID = d.foreignTeam
				return in
			},
			status: 404, wantRequestsIn: 3,
		},
		{
			// An unknown team is the same answer as an unauthorized one.
			name:      "an unknown team is not found",
			principal: func() authz.Principal { return d.principal(d.member) },
			in: func() service.SubmitRequestInput {
				in := d.submission("")
				in.TeamID = 0
				return in
			},
			status: 404, wantRequestsIn: 3,
		},
		{
			// The other tenant's own owner submits to their own team: the row that
			// must succeed, and its request lands in *their* org.
			name:      "the foreign org's own owner submits to their team",
			principal: func() authz.Principal { return d.principal(d.stranger) },
			in: func() service.SubmitRequestInput {
				in := d.submission("")
				in.TeamID = d.foreignTeam
				return in
			},
			wantOK: true, wantRequestsIn: 3,
		},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			res, err := d.svc.SubmitRequest(d.ctx, tc.principal(), tc.in())
			if tc.wantOK {
				if err != nil {
					t.Fatalf("submission refused: %v", err)
				}
				if !res.Created || res.Request == nil {
					t.Fatalf("a permitted submission did not create: %+v", res)
				}
				if res.Request.OrgID != d.org && res.Request.OrgID != d.otherOrg {
					t.Fatalf("the request landed in org %d, which is neither tenant", res.Request.OrgID)
				}
			} else {
				deny(t, err, tc.status, tc.code)
				if res != nil {
					t.Fatalf("a refused submission returned a result: %+v", res)
				}
			}
		})
		// Checked inside the loop, so a row that wrote is the row that fails.
		if got := d.requestsIn(d.org); got != tc.wantRequestsIn {
			t.Fatalf("after %q: %d requests in this org, want %d", tc.name, got, tc.wantRequestsIn)
		}
	}

	// The other tenant holds exactly its own owner's request and nothing from
	// this org.
	if got := d.requestsIn(d.otherOrg); got != 1 {
		t.Fatalf("the foreign org has %d requests, want its owner's 1", got)
	}

	// Submitting changed no membership, grant or project link.
	membershipsAfter := d.tableCount("org_members") + d.tableCount("team_members") +
		d.tableCount("project_teams") + d.tableCount("team_submit_grants")
	if membershipsAfter != membershipsBefore {
		t.Fatalf("submitting wrote membership rows: %d before, %d after", membershipsBefore, membershipsAfter)
	}

	// The three requests this org created came from the three permitted callers,
	// attributed to that caller, on that surface, in this org's inbox team.
	want := []struct {
		requester int
		source    domain.RequestSourceKind
	}{
		{d.member, domain.RequestSourceWeb},
		{d.customer, domain.RequestSourceMCP},
		{d.customer, domain.RequestSourceAPI},
	}
	rows2, err := d.db.Query(d.ctx, `SELECT org_id, team_id, requester_user_id, source_kind
		FROM team_requests WHERE org_id = $1 ORDER BY id`, d.org)
	if err != nil {
		t.Fatalf("read requests: %v", err)
	}
	defer rows2.Close()
	var got []struct {
		requester int
		source    domain.RequestSourceKind
	}
	for rows2.Next() {
		var orgID, teamID int
		var requester *int
		var source string
		if err := rows2.Scan(&orgID, &teamID, &requester, &source); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if orgID != d.org || teamID != d.team {
			t.Fatalf("stored tenant = org %d team %d, want org %d team %d", orgID, teamID, d.org, d.team)
		}
		if requester == nil {
			t.Fatal("a request was stored against nobody")
		}
		got = append(got, struct {
			requester int
			source    domain.RequestSourceKind
		}{*requester, domain.RequestSourceKind(source)})
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("read requests: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d requests in this org, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].requester != want[i].requester || got[i].source != want[i].source {
			t.Fatalf("request %d = requester %d source %q, want requester %d source %q",
				i, got[i].requester, got[i].source, want[i].requester, want[i].source)
		}
	}
}

// TestRequestSubmit_ForeignConnectorIDNeverLeaksAContact is the tenant boundary
// that has nothing to do with orgs: a connector's external id is unique per
// (team, source, external id) and deliberately carries no submitter, because a
// vendor's object id identifies the *object*. So two permitted submitters can
// present the same id, and the second must not be handed the first's record —
// the record snapshots that requester's contact address.
//
// The answer is the request surface's not-found, byte-for-byte what a request
// that does not exist produces, so the second submitter does not even learn that
// the id belongs to someone else. The same submitter replaying their own id is
// still answered normally, and the team can still read the request: the rule
// narrows replays, it does not break connector retries or the inbox.
func TestRequestSubmit_ForeignConnectorIDNeverLeaksAContact(t *testing.T) {
	d := newReqSubmitDB(t)
	// A second grant holder who is *not* a team member: submitting is not seeing,
	// so they pass the gate and fail the visibility check — exactly the case the
	// guard exists for.
	if _, err := d.svc.Repo.GrantTeamSubmit(d.ctx, d.team, d.org, d.other, nil); err != nil {
		t.Fatalf("grant the second customer: %v", err)
	}
	owner := d.principal(d.customer)
	foreign := d.principal(d.other)

	connector := service.SubmitRequestInput{
		TeamID: d.team, Source: domain.RequestSourceConnector, SourceExternalID: "issue-4242",
		Subject: "Vendor issue 4242", InputBrief: "reported by the connector",
	}
	first, err := d.svc.SubmitRequest(d.ctx, owner, connector)
	if err != nil {
		t.Fatalf("the connector's first submission: %v", err)
	}
	if !first.Created {
		t.Fatal("the connector's first submission was answered as a replay")
	}
	ownerContact := first.Request.RequesterContact
	if ownerContact == "" {
		t.Fatal("the request stored no contact snapshot, so this test proves nothing")
	}

	// The other permitted submitter presents the same external id.
	leak, err := d.svc.SubmitRequest(d.ctx, foreign, connector)
	deny(t, err, 404, authz.CodeRequestNotFound)
	if leak != nil {
		t.Fatalf("a foreign external id returned a result: %+v", leak)
	}
	for _, banned := range []string{ownerContact, owner.Label(), first.Request.PublicRef} {
		if banned != "" && strings.Contains(err.Error(), banned) {
			t.Fatalf("the refusal leaked %q: %v", banned, err)
		}
	}

	// The same submitter presenting their own id again is the object seen again
	// (D12), answered with their own request — the replay half of the rule.
	replay, err := d.svc.SubmitRequest(d.ctx, owner, connector)
	if err != nil {
		t.Fatalf("the owner's own connector replay: %v", err)
	}
	if replay.Created || replay.Request.ID != first.Request.ID {
		t.Fatalf("connector replay = request %d created=%v, want request %d created=false",
			replay.Request.ID, replay.Created, first.Request.ID)
	}

	// The team still sees its inbox: the refusal was about the caller, not the
	// request.
	view, err := d.svc.Authz.RequestAccess(d.ctx, d.principal(d.member), first.Request)
	if err != nil {
		t.Fatalf("the member's view of the request: %v", err)
	}
	if !view.Full() {
		t.Fatalf("a team member's view = %s, want the team view", view)
	}

	// Nor was anything written by the refused call: one request, one history and
	// one audit row, and the refused foreign submitter gained no access.
	if got := d.requestsIn(d.org); got != 1 {
		t.Fatalf("%d request rows after a refused foreign external id, want 1", got)
	}
	if n := d.submitEvents(first.Request.ID); n != 1 {
		t.Fatalf("the request has %d submit history rows, want 1", n)
	}
	if n := len(d.auditsFor(first.Request.ID)); n != 1 {
		t.Fatalf("the request has %d audit rows, want 1", n)
	}
	if n := len(d.visibleProjects(foreign)); n != 0 {
		t.Fatalf("the refused foreign submitter can see %d projects, want 0", n)
	}
}

// TestRequestSubmit_GrantsNoProjectAccess: a submission is not a route. Four
// kinds of caller submit, and none gains a project — in particular the granted
// customer, whose grant exists so they can submit *without* project membership,
// still reaches zero projects afterwards.
func TestRequestSubmit_GrantsNoProjectAccess(t *testing.T) {
	d := newReqSubmitDB(t)

	callers := []struct {
		name string
		p    func() authz.Principal
		in   func() service.SubmitRequestInput
	}{
		{"team member", func() authz.Principal { return d.principal(d.member) },
			func() service.SubmitRequestInput { return d.submission("") }},
		{"granted customer", func() authz.Principal { return d.principal(d.customer) },
			func() service.SubmitRequestInput { return d.submission("") }},
		{"submit-only credential", func() authz.Principal { return d.scoped(d.customer, d.team) },
			func() service.SubmitRequestInput { return d.submission("") }},
		{ // each tenant's owner submits to their own team: the one row per tenant
			// that is always permitted, so "no access gained" is measured where a
			// route *could* have granted something.
			"another tenant's owner", func() authz.Principal { return d.principal(d.stranger) },
			func() service.SubmitRequestInput {
				in := d.submission("")
				in.TeamID = d.foreignTeam
				return in
			},
		},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			p := c.p()
			before := d.visibleProjects(p)
			res, err := d.svc.SubmitRequest(d.ctx, p, c.in())
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			after := d.visibleProjects(p)
			// The only legitimate difference is access the caller already had (a
			// team member sees the team's linked project); what must never happen
			// is that submitting *added* one.
			for id := range after {
				if !before[id] {
					t.Fatalf("submitting granted project %d to the %s", id, c.name)
				}
			}
			if len(after) > len(before) {
				t.Fatalf("submitting grew project access for the %s: %d -> %d", c.name, len(before), len(after))
			}
			// And the created request carries no link: intake routes nothing.
			if n := d.count(`SELECT count(*) FROM request_links WHERE request_id = $1`, res.Request.ID); n != 0 {
				t.Fatalf("a submission wrote %d link rows; intake plans and routes nothing", n)
			}
		})
	}

	// The crisp version of the rule: the grant holder can submit and still
	// reaches zero projects, before and after.
	customer := d.principal(d.customer)
	if n := len(d.visibleProjects(customer)); n != 0 {
		t.Fatalf("the granted customer can see %d projects; a submit grant must not imply project membership", n)
	}
}

// ---- criterion 2: concurrent retry, audit and quota ----

// TestRequestSubmit_ConcurrentSameKeyCreatesOnce is the retry contract under real
// concurrency: many goroutines submitting the same key and payload at once
// produce exactly one request, one history row, one audit row, and no two units
// of quota for one submission.
//
// The ceiling is exactly the burst size, which is what makes the quota assertion
// exact rather than hand-wavy. Each caller charges its unit *before* the
// repository can tell it this is a replay (request_submit.go: authorize →
// validate → Allow → create → refund), so the ceiling must cover the whole burst
// or callers would be refused for racing rather than for retrying. The create
// then spends one unit and each of the eleven replays refunds its own, leaving
// exactly one spent — so exactly `callers-1` further *new* submissions fit in the
// window and the next one is refused. That count is the measurable statement of
// "a retry costs one request, not one unit each"; if a replay had spent a unit it
// would be 0 or negative, and the loop below would fail at its first iteration.
func TestRequestSubmit_ConcurrentSameKeyCreatesOnce(t *testing.T) {
	const callers = 12
	d := newReqSubmitDBLimit(t, callers) // exactly the burst, so the arithmetic is exact
	p := d.principal(d.member)

	base := d.submission("concurrent-key-1")

	type result struct {
		res     *service.SubmitRequestResult
		err     error
		created bool
	}
	results := make([]result, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // released together, so they genuinely race
			res, err := d.svc.SubmitRequest(d.ctx, p, base)
			results[i] = result{res: res, err: err, created: res != nil && res.Created}
		}(i)
	}
	close(start)
	wg.Wait()

	var created int
	var winnerID int
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("caller %d: %v", i, r.err)
		}
		if r.res == nil || r.res.Request == nil {
			t.Fatalf("caller %d returned no request", i)
		}
		if r.created {
			created++
			winnerID = r.res.Request.ID
		}
	}
	if created != 1 {
		t.Fatalf("%d callers reported creating the request, want exactly 1", created)
	}
	if got := d.requestsIn(d.org); got != 1 {
		t.Fatalf("%d request rows after a contested same-key race, want 1", got)
	}
	// Every caller was answered with the winner's request: no caller saw a
	// different public reference or a conflict.
	for i, r := range results {
		if r.res.Request.ID != winnerID {
			t.Fatalf("caller %d was answered request %d, want the winner's %d", i, r.res.Request.ID, winnerID)
		}
	}
	if n := d.submitEvents(winnerID); n != 1 {
		t.Fatalf("a request created once by %d racing callers has %d submit history rows, want 1", callers, n)
	}
	if n := len(d.auditsFor(winnerID)); n != 1 {
		t.Fatalf("a request created once by %d racing callers has %d audit rows, want 1", callers, n)
	}

	// Quota, measured: after the race exactly one unit is spent, so exactly
	// callers-1 further new submissions fit and the next is refused. A window that
	// charged each retry would run out earlier, and this loop would fail on its
	// first iteration instead of its twelfth.
	for i := range callers - 1 {
		if _, err := d.svc.SubmitRequest(d.ctx, p, d.submission("")); err != nil {
			t.Fatalf("new submission %d of %d after the race failed: %v (a replay spent quota it must refund)",
				i+1, callers-1, err)
		}
	}
	var limited *service.SubmitRateLimited
	if err := mustSubmit(d.svc, d.ctx, p, d.submission("")); !errors.As(err, &limited) {
		t.Fatalf("the submission after %d new ones = %v, want *SubmitRateLimited: exactly one unit was spent by the race",
			callers-1, err)
	}
	// The refusal is the coded 429, not a server fault, and it wrote nothing.
	if got := authz.CodeOf(limited); got != authz.CodeRequestRateLimited {
		t.Fatalf("the refusal code = %q, want %q", got, authz.CodeRequestRateLimited)
	}
	if got := d.requestsIn(d.org); got != callers {
		t.Fatalf("%d request rows (the raced one plus %d new probes), want %d", got, callers-1, callers)
	}
}

// TestRequestSubmit_BurstLargerThanTheCeilingNeverDuplicates pins the one
// consequence of charging before the create, so a future reader does not mistake
// it for a bug and "fix" it into a duplicate.
//
// A same-key retry burst bigger than the window's budget is partly refused: each
// caller spends its unit before the repository can recognise the replay
// (request_limits.go's Refund documents exactly this — it does not admit a
// replay, and request_submit.go records that the pre-Allow key probe is the
// slice that would close the gap). What must still hold, and is the safety
// property that matters, is that the refusals are refusals: the burst never
// creates a second request, never writes a second history or audit row, and the
// refused callers get the coded 429 rather than an error they cannot act on.
func TestRequestSubmit_BurstLargerThanTheCeilingNeverDuplicates(t *testing.T) {
	const callers = 8
	d := newReqSubmitDBLimit(t, 3) // a budget smaller than the burst, on purpose
	p := d.principal(d.member)

	base := d.submission("burst-over-ceiling-key")
	type result struct {
		res *service.SubmitRequestResult
		err error
	}
	results := make([]result, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := d.svc.SubmitRequest(d.ctx, p, base)
			results[i] = result{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var admitted, refused int
	var requestID int
	for i, r := range results {
		if r.err != nil {
			refused++
			var limited *service.SubmitRateLimited
			if !errors.As(r.err, &limited) {
				t.Fatalf("caller %d was refused with %v (%T); an over-budget retry must be the coded 429", i, r.err, r.err)
			}
			if r.res != nil {
				t.Fatalf("caller %d was refused and still returned a result: %+v", i, r.res)
			}
			continue
		}
		admitted++
		if r.res == nil || r.res.Request == nil {
			t.Fatalf("caller %d was admitted with no request", i)
		}
		if requestID == 0 {
			requestID = r.res.Request.ID
		} else if r.res.Request.ID != requestID {
			t.Fatalf("caller %d was answered request %d, want the one request %d", i, r.res.Request.ID, requestID)
		}
	}
	if admitted < 1 {
		t.Fatal("no caller was admitted, so the ceiling refused the whole burst")
	}
	if admitted > callers {
		t.Fatalf("%d callers admitted out of %d", admitted, callers)
	}

	// The invariant regardless of how many were admitted: one request, one
	// history row, one audit row. Refusals are refusals, not extra writes.
	if got := d.requestsIn(d.org); got != 1 {
		t.Fatalf("%d request rows after an over-budget retry burst, want exactly 1 (no duplicate)", got)
	}
	if n := d.submitEvents(requestID); n != 1 {
		t.Fatalf("the request has %d submit history rows, want 1", n)
	}
	if n := len(d.auditsFor(requestID)); n != 1 {
		t.Fatalf("the request has %d audit rows, want 1", n)
	}
}

// TestRequestSubmit_FaultedCreateThenRetryCreatesOnce is the fault-injected retry:
// the create's own transaction dies after it has inserted its request (a plpgsql
// trigger on the idempotency key row), nothing survives, and the client's retry
// of the same submission creates the request exactly once.
//
// The injector is the idiom internal/repository/request_create_test.go
// established: a trigger that raises, so the create's transaction rolls back
// rather than a fake error being threaded through the service.
func TestRequestSubmit_FaultedCreateThenRetryCreatesOnce(t *testing.T) {
	d := newReqSubmitDB(t)
	p := d.principal(d.member)
	in := d.submission("faulted-create-key")

	if _, err := d.db.Exec(d.ctx, `CREATE OR REPLACE FUNCTION reqsubmit_test_fail_key() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'injected failure writing idempotency key %', NEW.idempotency_key;
		END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create the failure injector: %v", err)
	}
	if _, err := d.db.Exec(d.ctx, `CREATE TRIGGER reqsubmit_test_fail_key
		BEFORE INSERT ON request_idempotency_keys FOR EACH ROW
		EXECUTE FUNCTION reqsubmit_test_fail_key()`); err != nil {
		t.Fatalf("install the failure injector: %v", err)
	}
	dropInjector := func() {
		t.Helper()
		if _, err := d.db.Exec(d.ctx, `DROP TRIGGER IF EXISTS reqsubmit_test_fail_key ON request_idempotency_keys`); err != nil {
			t.Fatalf("drop the injector trigger: %v", err)
		}
		if _, err := d.db.Exec(d.ctx, `DROP FUNCTION IF EXISTS reqsubmit_test_fail_key()`); err != nil {
			t.Fatalf("drop the injector function: %v", err)
		}
	}
	t.Cleanup(dropInjector)

	// The faulted attempt: an error, no result, and nothing left behind. "No
	// stuck claim" is structural here — the key row exists only in a transaction
	// that commits, so a dead attempt cannot hold the key.
	_, err := d.svc.SubmitRequest(d.ctx, p, in)
	if err == nil {
		t.Fatal("the injected failure produced no error")
	}
	if got := d.requestsIn(d.org); got != 0 {
		t.Fatalf("%d request rows survived a failed create, want 0", got)
	}
	if got := d.tableCount("request_events"); got != 0 {
		t.Fatalf("%d history rows survived a failed create, want 0", got)
	}
	if got := d.tableCount("request_idempotency_keys"); got != 0 {
		t.Fatalf("%d key rows survived a failed create, want 0", got)
	}

	// The retry after the transient fault: a first attempt from the database's
	// point of view, so it creates.
	dropInjector()
	created, err := d.svc.SubmitRequest(d.ctx, p, in)
	if err != nil {
		t.Fatalf("the retry after a failed create: %v", err)
	}
	if !created.Created {
		t.Fatal("the retry was answered as a replay of a request the failed attempt never wrote")
	}
	if got := d.requestsIn(d.org); got != 1 {
		t.Fatalf("%d request rows after the retry, want 1", got)
	}
	if n := d.submitEvents(created.Request.ID); n != 1 {
		t.Fatalf("the retried request has %d submit history rows, want 1", n)
	}
	if n := len(d.auditsFor(created.Request.ID)); n != 1 {
		t.Fatalf("the retried request has %d audit rows, want 1", n)
	}

	// A third attempt now *is* a replay, because the retry committed the key.
	replay, err := d.svc.SubmitRequest(d.ctx, p, in)
	if err != nil {
		t.Fatalf("replay after the successful retry: %v", err)
	}
	if replay.Created || replay.Request.ID != created.Request.ID {
		t.Fatalf("replay after the retry = request %d created=%v, want request %d created=false",
			replay.Request.ID, replay.Created, created.Request.ID)
	}
	if n := d.submitEvents(created.Request.ID); n != 1 {
		t.Fatalf("the replay appended a history row: %d, want 1", n)
	}
	if n := len(d.auditsFor(created.Request.ID)); n != 1 {
		t.Fatalf("the replay wrote an audit row: %d, want 1", n)
	}
}

// TestRequestSubmit_RevocationBetweenCreateAndReplay: revoking the grant between
// a create and its retry makes the retry fail at the gate, and the answer is the
// team surface's not-found — the same bytes a team that does not exist produces —
// so revocation leaks nothing about the request that is still there.
func TestRequestSubmit_RevocationBetweenCreateAndReplay(t *testing.T) {
	d := newReqSubmitDB(t)
	p := d.principal(d.customer)
	in := d.submission("revocation-between-key")

	first, err := d.svc.SubmitRequest(d.ctx, p, in)
	if err != nil {
		t.Fatalf("the granted customer's submit: %v", err)
	}
	if !first.Created {
		t.Fatal("the first submission was answered as a replay")
	}

	// Revoke, then retry the *identical* submission. The key would still resolve
	// to a request; the gate never lets the call get that far.
	if _, err := d.svc.Repo.RevokeTeamSubmit(d.ctx, d.team, d.customer, nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	retry, err := d.svc.SubmitRequest(d.ctx, p, in)
	deny(t, err, 404, "")
	if retry != nil {
		t.Fatalf("a refused retry returned a result: %+v", retry)
	}
	// The refusal is the team surface's, not the request surface's: a revoked
	// requester cannot probe for their old request's existence or reference.
	if got := authz.CodeOf(err); got == authz.CodeRequestNotFound {
		t.Fatalf("the revoked retry used the request-surface code %q; it must be the team's not-found", got)
	}
	if strings.Contains(err.Error(), first.Request.PublicRef) {
		t.Fatalf("the refusal leaked the request reference: %v", err)
	}

	// Nothing changed because of the refused retry.
	if got := d.requestsIn(d.org); got != 1 {
		t.Fatalf("%d request rows, want the original 1", got)
	}
	if n := d.submitEvents(first.Request.ID); n != 1 {
		t.Fatalf("the refused retry left %d submit history rows, want 1", n)
	}
	if n := len(d.auditsFor(first.Request.ID)); n != 1 {
		t.Fatalf("the refused retry left %d audit rows, want the original 1", n)
	}

	// Re-granting does not turn the retry into a *create*: the original key is
	// committed, so it is the original request's replay — proving the refusal was
	// authorization and not the key having been consumed.
	if _, err := d.svc.Repo.GrantTeamSubmit(d.ctx, d.team, d.org, d.customer, nil); err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	back, err := d.svc.SubmitRequest(d.ctx, p, in)
	if err != nil {
		t.Fatalf("the retry after re-granting: %v", err)
	}
	if back.Created || back.Request.ID != first.Request.ID {
		t.Fatalf("retry after re-grant = request %d created=%v, want the original %d created=false",
			back.Request.ID, back.Created, first.Request.ID)
	}
	if got := d.requestsIn(d.org); got != 1 {
		t.Fatalf("%d request rows after the re-granted retry, want 1", got)
	}
}

// TestRequestSubmit_RateLimitRetryAfterAndReplayCost pins the quota arithmetic of
// the whole path in one two-unit window:
//
//   - the create spends one unit;
//   - a replay spends none (it must refund, or a retrying client would burn its
//     own budget — and the next assertion would fail);
//   - the next *new* submission spends the second unit, so the refusal that
//     follows is exact, coded 429 with the seconds left in the window;
//   - a refused conflict spends its unit, because the limiter is charged before
//     the repository decides (a request that failed validation is refused before
//     the limiter, and that is a separate row below);
//   - the next window is a fresh budget.
func TestRequestSubmit_RateLimitRetryAfterAndReplayCost(t *testing.T) {
	d := newReqSubmitDBLimit(t, 2)
	p := d.principal(d.member)
	// 30 s into a 60 s window, so the wait a refusal reports is exact.
	d.now = time.Date(2026, 11, 11, 20, 0, 30, 0, time.UTC)

	keyed := d.submission("quota-arithmetic-key")
	first, err := d.svc.SubmitRequest(d.ctx, p, keyed)
	if err != nil {
		t.Fatalf("the first submission: %v", err)
	}
	if !first.Created {
		t.Fatal("the first submission was answered as a replay")
	}

	// The replay: allowed (one unit of headroom left), and it gives its own unit
	// back, so the headroom is still one.
	replay, err := d.svc.SubmitRequest(d.ctx, p, keyed)
	if err != nil {
		t.Fatalf("the replay: %v", err)
	}
	if replay.Created || replay.Request.ID != first.Request.ID {
		t.Fatalf("the replay = request %d created=%v, want request %d created=false",
			replay.Request.ID, replay.Created, first.Request.ID)
	}

	// The second unit: a genuinely new submission still fits, which is the
	// behavioural proof that the replay above cost nothing.
	second, err := d.svc.SubmitRequest(d.ctx, p, d.submission(""))
	if err != nil {
		t.Fatalf("the first new submission after a replay: %v (a replay spent quota it must refund)", err)
	}
	if !second.Created {
		t.Fatal("the second new submission was answered as a replay")
	}

	// The ceiling: a third new submission is refused, with the exact remaining
	// wait and the request surface's stable code.
	err = mustSubmit(d.svc, d.ctx, p, d.submission(""))
	var limited *service.SubmitRateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("the submission past the ceiling = %v (%T), want *SubmitRateLimited", err, err)
	}
	if limited.RetryAfterS != 30 {
		t.Fatalf("Retry-After = %ds, want the 30 s left in the window", limited.RetryAfterS)
	}
	if limited.RetryAfterS < 1 {
		t.Fatalf("Retry-After = %ds; a header must never say zero", limited.RetryAfterS)
	}
	if got := authz.CodeOf(err); got != authz.CodeRequestRateLimited {
		t.Fatalf("the rate-limit code = %q, want %q", got, authz.CodeRequestRateLimited)
	}
	if got := authz.StatusOf(err, 500); got != 429 {
		t.Fatalf("the rate-limit status = %d, want 429", got)
	}
	// The refusal wrote nothing: two requests, each with one history and one
	// audit row.
	if got := d.requestsIn(d.org); got != 2 {
		t.Fatalf("%d request rows after a refused burst, want 2", got)
	}
	for _, id := range []int{first.Request.ID, second.Request.ID} {
		if n := d.submitEvents(id); n != 1 {
			t.Fatalf("request %d has %d submit history rows, want 1", id, n)
		}
		if n := len(d.auditsFor(id)); n != 1 {
			t.Fatalf("request %d has %d audit rows, want 1", id, n)
		}
	}

	// Validation runs before the limiter, so a malformed submission is a 400 that
	// names the field even with the budget exhausted — a client fumbling a form is
	// never told to wait.
	oversize := d.submission("")
	oversize.Subject = strings.Repeat("s", domain.MaxRequestSubjectLen+1)
	invalid := mustSubmit(d.svc, d.ctx, p, oversize)
	if got := authz.CodeOf(invalid); got != authz.CodeRequestInvalid {
		t.Fatalf("a malformed submission at the ceiling = %v (%q), want %q", invalid, got, authz.CodeRequestInvalid)
	}
	if got := authz.StatusOf(invalid, 500); got != 400 {
		t.Fatalf("a malformed submission at the ceiling = %d, want 400", got)
	}

	// The next window is a fresh budget on the same key.
	d.now = d.now.Add(time.Minute)
	next, err := d.svc.SubmitRequest(d.ctx, p, d.submission(""))
	if err != nil {
		t.Fatalf("the next window's submission: %v", err)
	}
	if !next.Created {
		t.Fatal("the next window's submission was answered as a replay")
	}
	if got := d.requestsIn(d.org); got != 3 {
		t.Fatalf("%d request rows after the next window, want 3", got)
	}
}

// ---- the fresh-only accounting rule, measured exactly ----

// TestRequestSubmit_AuditAndQuotaArePerCreate turns "no new quota/audit double
// counting" into arithmetic: four creates, three replays, one refused conflict —
// and the audit rows, history rows and units spent are all derived from that
// count, not from the attempt count.
//
// The ceiling is five on purpose: four creates spend four units, and the refused
// conflict spends the fifth (it reaches the limiter before the repository, and a
// conflict is not a replay, so nothing refunds it). The submission after that is
// refused, which is the observable that pins the total.
func TestRequestSubmit_AuditAndQuotaArePerCreate(t *testing.T) {
	d := newReqSubmitDBLimit(t, 5)
	p := d.principal(d.member)

	var createdIDs []int
	// Four creates: three distinct unkeyed submissions and one keyed one.
	for i := range 3 {
		in := d.submission("")
		in.Subject = fmt.Sprintf("Request %d for the export", i+1)
		res, err := d.svc.SubmitRequest(d.ctx, p, in)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if !res.Created {
			t.Fatalf("create %d reported a replay", i)
		}
		createdIDs = append(createdIDs, res.Request.ID)
	}
	keyed := d.submission("audit-arithmetic-key")
	keyedRes, err := d.svc.SubmitRequest(d.ctx, p, keyed)
	if err != nil {
		t.Fatalf("keyed create: %v", err)
	}
	if !keyedRes.Created {
		t.Fatal("the keyed create reported a replay")
	}
	createdIDs = append(createdIDs, keyedRes.Request.ID)

	// Two replays and one refused conflict on that key.
	for i := range 2 {
		replay, err := d.svc.SubmitRequest(d.ctx, p, keyed)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if replay.Created || replay.Request.ID != keyedRes.Request.ID {
			t.Fatalf("replay %d = request %d created=%v, want the keyed request %d",
				i, replay.Request.ID, replay.Created, keyedRes.Request.ID)
		}
	}
	conflicting := keyed
	conflicting.InputBrief = "A different brief for the same key."
	conflictErr := mustSubmit(d.svc, d.ctx, p, conflicting)
	deny(t, conflictErr, 409, authz.CodeRequestConflict)
	// The conflict keeps the repository's conflict identity, so a caller that only
	// distinguishes "conflict" from success needs no special case.
	if !errors.Is(conflictErr, repository.ErrConflict) {
		t.Fatalf("the conflict lost errors.Is(ErrConflict): %v", conflictErr)
	}

	if got := d.requestsIn(d.org); got != 4 {
		t.Fatalf("%d request rows after 4 creates, their replays and a conflict, want 4", got)
	}
	for _, id := range createdIDs {
		if n := d.submitEvents(id); n != 1 {
			t.Fatalf("request %d has %d submit history rows, want exactly 1", id, n)
		}
		if n := len(d.auditsFor(id)); n != 1 {
			t.Fatalf("request %d has %d audit rows, want exactly 1", id, n)
		}
	}

	// Five units are gone (four creates plus the refused conflict), so the sixth
	// new submission is refused. A create or a replay that had double-counted
	// would hit the ceiling earlier, and the creates above would already have
	// failed.
	var limited *service.SubmitRateLimited
	if err := mustSubmit(d.svc, d.ctx, p, d.submission("")); !errors.As(err, &limited) {
		t.Fatalf("the submission past the five-unit ceiling = %v, want *SubmitRateLimited", err)
	}
	if got := d.requestsIn(d.org); got != 4 {
		t.Fatalf("the refused submission past the ceiling wrote a row: %d requests, want 4", got)
	}
}

// mustSubmit runs one submission and returns only its error, for the assertions
// above that care about the refusal rather than the result.
func mustSubmit(svc *service.Service, ctx context.Context, p authz.Principal, in service.SubmitRequestInput) error {
	_, err := svc.SubmitRequest(ctx, p, in)
	return err
}
