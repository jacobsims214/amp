package service

// Tests for the authorized request-submission path (AMP #3899, epic 202 story
// 540; build slice S1 of discoveries/team-inbox-approved-build-slices.md).
//
// The ticket's three criteria are what these assert, in order:
//
//  1. authorized create, authorized replay and a payload mismatch are each
//     answered correctly: a member and a grant-holding requester create; the
//     same key with the same normalized payload answers with the SAME request
//     (created=false, same public_ref, no second history row); the same key with
//     a different payload is request.conflict and writes nothing
//     (TestSubmitRequest_CreateReplayAndMismatch).
//  2. attribution cannot be spoofed: the stored requester user, contact snapshot
//     and org are the principal's and the team's, never the payload's — the
//     input type has no such field, and a payload that talks about another
//     requester changes nothing. A connector external id that already belongs to
//     ANOTHER requester's request is answered with the surface's single
//     not-found rather than with that request, so one requester's contact can
//     never leak through a replay (TestSubmitRequest_AttributionCannotBeSpoofed,
//     TestSubmitRequest_ForeignExternalIDLeaksNothing).
//  3. a fresh submit is audited exactly once and the limiter behaves: one
//     `request.submit` audit row per created request, none for a replay, one
//     unit charged per attempt and refunded on replay, and a burst over the limit
//     is the coded 429 with a retry-after
//     (TestSubmitRequest_AuditsOnceAndKeepsQuota).
//
// Around the criteria the tests pin what else the path promises: who is refused
// (ungranted customer, revoked grant, a submit-only token on a foreign team, a
// cross-org team, an unknown team, a caller with no AMP user) and that a refusal
// writes nothing and audits nothing (TestSubmitRequest_Refusals); a created
// request is `new` at rev 1 with no claim, owner or link, and submitting grants
// the requester no project access (TestSubmitRequest_WritesNoLifecycleState);
// and the shapers name the field they refuse (TestShapeSubmission_*).
//
// The database-backed tests run against a real Postgres with every migration
// applied, when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'SubmitRequest|ShapeSubmission' -count=1 -race
//
// Every fixture row this file creates carries a run-unique suffix (orgs, teams,
// users and the minted public refs), so repeated runs and other tests in the
// same database never see each other's rows.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
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

// submitTestRepo returns a Repo whose database has every migration applied.
func submitTestRepo(t *testing.T) *repository.Repo {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the submission tests against Postgres")
	}
	ctx := context.Background()
	r, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(r.Close)
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "server", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	var ms []repository.Migration
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ms = append(ms, repository.Migration{Version: strings.TrimSuffix(filepath.Base(f), ".sql"), SQL: string(b)})
	}
	if err := r.RunMigrations(ctx, ms); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return r
}

// submitFixture is one test's tenant: an inbox team linked to a project, a
// second team in the same org, a second org with its own team, and the people
// around them.
type submitFixture struct {
	t   *testing.T
	svc *Service
	db  *pgx.Conn
	ctx context.Context
	// limits is the service's limiter, so a test can read a key's count.
	limits *RequestLimiter
	// now is the clock the service reads for the limiter.
	now time.Time

	org       int
	team      int
	project   int
	otherTeam int // another team in the same org, NOT linked to project
	otherOrg  int
	foreign   int // a team in otherOrg

	member    int // team member, no project role beyond the team link
	admin     int // owner of org
	customer  int // customer kind: submit grant on team, no membership
	stranger  int // in otherOrg only
	otherCust int // customer kind, no grant anywhere
}

func newSubmitFixture(t *testing.T) *submitFixture {
	t.Helper()
	repo := submitTestRepo(t)
	ctx := context.Background()
	db, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect (fixture): %v", err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })

	suffix := rand.IntN(1_000_000_000)
	f := &submitFixture{
		t: t, db: db, ctx: ctx,
		now: time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC),
	}
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
			fmt.Sprintf("submit-%s-%d", label, suffix),
			fmt.Sprintf("submit-%s-%d@submit.test", label, suffix), kind)
	}

	f.org = q(`INSERT INTO orgs (name, slug) VALUES ('Submit org', $1) RETURNING id`, fmt.Sprintf("submit-%d", suffix))
	f.team = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, f.org)
	f.otherTeam = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other desk') RETURNING id`, f.org)
	f.project = q(`INSERT INTO projects (name, code, org_id) VALUES ('Delivered work', $1, $2) RETURNING id`,
		fmt.Sprintf("SUB%d", suffix%100000), f.org)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, f.project, f.team)
	f.otherOrg = q(`INSERT INTO orgs (name, slug) VALUES ('Submit other org', $1) RETURNING id`, fmt.Sprintf("submit-other-%d", suffix))
	f.foreign = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign inbox') RETURNING id`, f.otherOrg)

	f.member = user("member", domain.UserKindHuman)
	f.admin = user("admin", domain.UserKindHuman)
	f.customer = user("customer", domain.UserKindCustomer)
	f.otherCust = user("othercustomer", domain.UserKindCustomer)
	f.stranger = user("stranger", domain.UserKindHuman)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.org, f.admin)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, f.org, f.member)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, f.team, f.member)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.otherOrg, f.stranger)
	exec(`INSERT INTO team_submit_grants (team_id, org_id, user_id) VALUES ($1, $2, $3)`, f.team, f.org, f.customer)

	f.svc = New(repo, nil, nil)
	f.limits = NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 30})
	f.svc.RequestLimits = f.limits
	f.svc.Now = func() time.Time { return f.now }
	return f
}

// withClock pins the service's clock at t0.
func (f *submitFixture) withClock(now time.Time) {
	f.now = now
	f.svc.Now = func() time.Time { return f.now }
}

// principal builds a signed-in caller for userID: a person in the web UI unless
// the test says otherwise.
func (f *submitFixture) principal(userID int) authz.Principal {
	f.t.Helper()
	var email string
	if err := f.db.QueryRow(f.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		f.t.Fatalf("email of %d: %v", userID, err)
	}
	return authz.Principal{UserID: userID, Email: email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
}

// email is userID's stored address, which is what a submission snapshots.
func (f *submitFixture) email(userID int) string {
	f.t.Helper()
	var email string
	if err := f.db.QueryRow(f.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		f.t.Fatalf("email of %d: %v", userID, err)
	}
	return email
}

// submission is one well-formed submission to the inbox team, as a web adapter
// would build it: no identity, no org, no project, an optional key.
func (f *submitFixture) submission(key string) SubmitRequestInput {
	return SubmitRequestInput{
		TeamID:         f.team,
		Source:         domain.RequestSourceWeb,
		Subject:        "Please add a CSV export of the request queue",
		InputBrief:     "The triage view has no export. We need the queue as CSV for our weekly review.",
		Metadata:       domain.Metadata{"connector_label": "support-desk"},
		IdempotencyKey: key,
	}
}

// requestCount is how many request rows this fixture's org has.
func (f *submitFixture) requestCount() int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM team_requests WHERE org_id = $1`, f.org).Scan(&n); err != nil {
		f.t.Fatalf("count requests: %v", err)
	}
	return n
}

// submitEvents is how many `submit` history rows requestID has.
func (f *submitFixture) submitEvents(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx,
		`SELECT count(*) FROM request_events WHERE request_id = $1 AND action = 'submit'`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count submit events for %d: %v", requestID, err)
	}
	return n
}

// audits is every audit row this fixture can attribute to requestID, scoped by
// the action and the request it targets (other tests share the database).
func (f *submitFixture) audits(requestID int) []domain.AuditEntry {
	f.t.Helper()
	entries, err := f.svc.Repo.ListAudit(f.ctx, domain.AuditFilter{Action: "request.submit", TargetID: idStr(requestID)})
	if err != nil {
		f.t.Fatalf("list audit for %d: %v", requestID, err)
	}
	return entries
}

// limitCount is how many units key has spent in its current window. Tests read
// it instead of calling Allow, which would spend one.
func (f *submitFixture) limitCount(k RequestLimitKey) int {
	f.t.Helper()
	sh := f.limits.shardFor(k.String())
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if !sh.window.Equal(f.now.Truncate(f.limits.window)) {
		return 0
	}
	return sh.counts[k.String()]
}

func (f *submitFixture) webKey(userID int) RequestLimitKey {
	return reqKey(f.org, f.team, userID, domain.RequestSourceWeb)
}

// linkCount is how many request_links rows requestID has.
func (f *submitFixture) linkCount(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_links WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count links for %d: %v", requestID, err)
	}
	return n
}

// ---- criterion 1: create, replay, mismatch ----

// A team member and a grant-holding customer both create; the same key and
// payload replays the SAME request; the same key with a different payload is a
// conflict that writes nothing.
func TestSubmitRequest_CreateReplayAndMismatch(t *testing.T) {
	f := newSubmitFixture(t)
	f.withClock(f.now)
	member := f.principal(f.member)
	customer := f.principal(f.customer)

	// A team member submits to their own inbox.
	first, err := f.svc.SubmitRequest(f.ctx, member, f.submission(""))
	if err != nil {
		t.Fatalf("member submit: %v", err)
	}
	if !first.Created || first.Request == nil {
		t.Fatalf("a fresh submission did not report a create: %+v", first)
	}
	if first.Request.State != domain.RequestStatusNew || first.Request.Rev != 1 {
		t.Fatalf("a submission landed in state %q rev %d, want new rev 1", first.Request.State, first.Request.Rev)
	}
	if first.Request.TeamID != f.team || first.Request.OrgID != f.org {
		t.Fatalf("stored tenant = team %d org %d, want team %d org %d", first.Request.TeamID, first.Request.OrgID, f.team, f.org)
	}
	if ref, err := domain.NormalizeRequestPublicRef(first.Request.PublicRef); err != nil || ref != first.Request.PublicRef {
		t.Fatalf("stored public_ref %q is not the shape the requester holds (normalized %q, err %v)", first.Request.PublicRef, ref, err)
	}

	// A grant-holding customer (no membership) submits to the same inbox.
	granted, err := f.svc.SubmitRequest(f.ctx, customer, SubmitRequestInput{
		TeamID: f.team, Source: domain.RequestSourceMCP,
		Subject: "Add a filter for requests I submitted", InputBrief: "I can only see mine one by one.",
	})
	if err != nil {
		t.Fatalf("customer with a grant submit: %v", err)
	}
	if !granted.Created {
		t.Fatalf("a customer's first submission was answered as a replay")
	}
	if got := f.requestCount(); got != 2 {
		t.Fatalf("%d request rows after two distinct submissions, want 2", got)
	}

	// The same key and the same payload replays: the same request, created=false.
	in := f.submission("member-key-1")
	created1, err := f.svc.SubmitRequest(f.ctx, member, in)
	if err != nil {
		t.Fatalf("keyed submit: %v", err)
	}
	replay, err := f.svc.SubmitRequest(f.ctx, member, in)
	if err != nil {
		t.Fatalf("keyed replay: %v", err)
	}
	if replay.Created {
		t.Fatalf("the replay reported a create")
	}
	if replay.Request.ID != created1.Request.ID {
		t.Fatalf("the replay answered with request %d, want the key's own request %d", replay.Request.ID, created1.Request.ID)
	}
	if replay.Request.PublicRef != created1.Request.PublicRef {
		t.Fatalf("the replay answered with public_ref %q, want the original %q", replay.Request.PublicRef, created1.Request.PublicRef)
	}
	if n := f.submitEvents(created1.Request.ID); n != 1 {
		t.Fatalf("a replayed request has %d submit history rows, want exactly 1", n)
	}
	if got := f.requestCount(); got != 3 {
		t.Fatalf("%d request rows after a create and its replay, want 3", got)
	}

	// The same key with a different payload is a conflict, not a replay.
	mismatch := in
	mismatch.Subject = "Please add a PDF export instead"
	_, err = f.svc.SubmitRequest(f.ctx, member, mismatch)
	if err == nil {
		t.Fatal("a reused key with a different payload was accepted")
	}
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("mismatched payload error = %v, want a conflict", err)
	}
	if code := authz.CodeOf(err); code != authz.CodeRequestConflict {
		t.Fatalf("mismatched payload code = %q, want %q", code, authz.CodeRequestConflict)
	}
	if status := authz.StatusOf(err, 500); status != 409 {
		t.Fatalf("mismatched payload status = %d, want 409", status)
	}
	if got := f.requestCount(); got != 3 {
		t.Fatalf("the conflict wrote a request: %d rows, want 3", got)
	}
	if n := len(f.audits(created1.Request.ID)); n != 1 {
		t.Fatalf("the conflict audited the request again: %d rows, want 1", n)
	}

	// The original payload still replays after the refused attempt: the conflict
	// wrote nothing, so the key still names its one request.
	again, err := f.svc.SubmitRequest(f.ctx, member, in)
	if err != nil {
		t.Fatalf("replay after a refused mismatch: %v", err)
	}
	if again.Created || again.Request.ID != created1.Request.ID {
		t.Fatalf("replay after a conflict = request %d created=%v, want request %d created=false",
			again.Request.ID, again.Created, created1.Request.ID)
	}
}

// ---- criterion 2: attribution cannot be spoofed ----

// The stored requester, contact and org are the principal's and the team's; a
// payload cannot name them, and content that talks about another requester is
// stored as the requester's text and nothing else.
func TestSubmitRequest_AttributionCannotBeSpoofed(t *testing.T) {
	f := newSubmitFixture(t)
	f.withClock(f.now)
	customer := f.principal(f.customer)
	member := f.principal(f.member)

	// The input type has no field for identity at all, which is what makes
	// spoofing impossible rather than merely checked: this submission names the
	// other account only inside the text a requester wrote.
	in := SubmitRequestInput{
		TeamID:     f.team,
		Source:     domain.RequestSourceAPI,
		Subject:    "Act on behalf of the org owner",
		InputBrief: "requester_user_id: " + idStr(f.admin) + "\nrequester_contact: evil@example.test\norg_id: 1",
		Metadata:   domain.Metadata{"route_to_project": "1"},
	}
	res, err := f.svc.SubmitRequest(f.ctx, customer, in)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	got := res.Request
	if got.RequesterUserID == nil || *got.RequesterUserID != f.customer {
		t.Fatalf("stored requester_user_id = %v, want the authenticated %d", got.RequesterUserID, f.customer)
	}
	if want := f.email(f.customer); got.RequesterContact != want {
		t.Fatalf("stored requester_contact = %q, want the account's own %q", got.RequesterContact, want)
	}
	if got.OrgID != f.org || got.TeamID != f.team {
		t.Fatalf("stored tenant = org %d team %d, want org %d team %d", got.OrgID, got.TeamID, f.org, f.team)
	}
	if got.SourceKind != domain.RequestSourceAPI {
		t.Fatalf("stored source_kind = %q, want the adapter's api", got.SourceKind)
	}
	// The metadata a connector may send is labels, never a routing decision.
	if _, ok := got.Metadata["route_to_project"]; !ok {
		t.Fatalf("metadata was not stored: %v", got.Metadata)
	}
	if n := f.linkCount(got.ID); n != 0 {
		t.Fatalf("a submission wrote %d link rows; intake routes nothing", n)
	}
	// The brief is stored as text, so a would-be instruction is evidence, not
	// an instruction.
	if !strings.Contains(got.InputBrief, "requester_user_id") {
		t.Fatalf("the brief was altered: %q", got.InputBrief)
	}

	// A team member's own submission is attributed to the member, not to some
	// earlier requester: attribution is per submission, never inherited.
	memberRes, err := f.svc.SubmitRequest(f.ctx, member, f.submission(""))
	if err != nil {
		t.Fatalf("member submit: %v", err)
	}
	if memberRes.Request.RequesterUserID == nil || *memberRes.Request.RequesterUserID != f.member {
		t.Fatalf("member's request requester = %v, want %d", memberRes.Request.RequesterUserID, f.member)
	}
	if memberRes.Request.RequesterContact != f.email(f.member) {
		t.Fatalf("member's request contact = %q, want their own address", memberRes.Request.RequesterContact)
	}

	// The requester's own replay returns their own record, contact included —
	// the one case where a contact snapshot is the caller's business.
	keyed := SubmitRequestInput{TeamID: f.team, Source: domain.RequestSourceAPI,
		Subject: "Replay me", InputBrief: "with my own contact", IdempotencyKey: "cust-key-1"}
	own, err := f.svc.SubmitRequest(f.ctx, customer, keyed)
	if err != nil {
		t.Fatalf("keyed customer submit: %v", err)
	}
	replay, err := f.svc.SubmitRequest(f.ctx, customer, keyed)
	if err != nil {
		t.Fatalf("customer's own replay: %v", err)
	}
	if replay.Request.ID != own.Request.ID || replay.Request.RequesterContact != f.email(f.customer) {
		t.Fatalf("the requester's own replay = request %d contact %q, want request %d and their own address",
			replay.Request.ID, replay.Request.RequesterContact, own.Request.ID)
	}
}

// A connector's external id is scoped to (team, source, external id) and
// deliberately not to a submitter, so two people can present one id. The second
// gets the not-found the surface gives for a request that is not theirs — never
// the first requester's record, contact snapshot included.
func TestSubmitRequest_ForeignExternalIDLeaksNothing(t *testing.T) {
	f := newSubmitFixture(t)
	f.withClock(f.now)
	customer := f.principal(f.customer)

	connector := SubmitRequestInput{
		TeamID: f.team, Source: domain.RequestSourceConnector, SourceExternalID: "issue-4242",
		Subject: "Vendor issue 4242", InputBrief: "reported by the connector",
	}
	owner, err := f.svc.SubmitRequest(f.ctx, customer, connector)
	if err != nil {
		t.Fatalf("connector submit: %v", err)
	}
	if !owner.Created {
		t.Fatal("the first connector submission was answered as a replay")
	}

	// A different customer holds their own submit grant (so they pass the
	// authorization gate) but no view of the team's inbox: submitting is not
	// seeing. They present the same external id.
	if _, err := f.svc.Repo.GrantTeamSubmit(f.ctx, f.team, f.org, f.otherCust, nil); err != nil {
		t.Fatalf("grant the second customer: %v", err)
	}
	other := f.principal(f.otherCust)
	_, err = f.svc.SubmitRequest(f.ctx, other, connector)
	if err == nil {
		t.Fatal("another requester was answered with a request it may not see")
	}
	if authz.CodeOf(err) != authz.CodeRequestNotFound || authz.StatusOf(err, 500) != 404 {
		t.Fatalf("foreign external id = %v (code %q, status %d), want request.not_found / 404",
			err, authz.CodeOf(err), authz.StatusOf(err, 500))
	}
	if strings.Contains(err.Error(), f.email(f.customer)) {
		t.Fatalf("the refusal leaked the owning requester's contact: %v", err)
	}

	// The same submitter presenting their own external id again is the object
	// seen again (D12) and is answered with their own request — the replay half
	// of the rule, so the visibility check above is a narrowing and not a
	// blanket refusal of connector retries.
	repeat, err := f.svc.SubmitRequest(f.ctx, customer, connector)
	if err != nil {
		t.Fatalf("the same connector replaying its own external id: %v", err)
	}
	if repeat.Created || repeat.Request.ID != owner.Request.ID {
		t.Fatalf("connector replay = request %d created=%v, want request %d created=false",
			repeat.Request.ID, repeat.Created, owner.Request.ID)
	}

	// The team still sees its inbox — the post-condition is about the caller,
	// not about the request.
	inboxView, err := f.svc.Authz.RequestAccess(f.ctx, f.principal(f.member), owner.Request)
	if err != nil {
		t.Fatalf("member access: %v", err)
	}
	if !inboxView.Full() {
		t.Fatalf("a team member's view of the request = %s, want the team view", inboxView)
	}
}

// ---- refusals write nothing ----

// Every principal that may not submit is refused, and a refusal writes no
// request, no history, no grant and no audit row of its own.
func TestSubmitRequest_Refusals(t *testing.T) {
	f := newSubmitFixture(t)
	f.withClock(f.now)

	// A customer of the org with no grant: no team role, no org role, no grant.
	if _, err := f.svc.SubmitRequest(f.ctx, f.principal(f.otherCust), f.submission("")); err == nil {
		t.Fatal("an ungranted customer submitted to the inbox")
	}
	// A revoked grant takes effect on the very next call (per-request re-check).
	granted, err := f.svc.SubmitRequest(f.ctx, f.principal(f.customer), f.submission(""))
	if err != nil {
		t.Fatalf("granted customer submit: %v", err)
	}
	if _, err := f.svc.Repo.RevokeTeamSubmit(f.ctx, f.team, f.customer, nil); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}
	if _, err := f.svc.SubmitRequest(f.ctx, f.principal(f.customer), f.submission("")); err == nil {
		t.Fatal("a revoked grant still submitted")
	}

	// A submit-only token is bound to one team: its own team works, any other
	// team is not found. Both halves are asserted so the narrowing is visible.
	scoped := f.principal(f.customer)
	scoped.Scope, scoped.ScopeTeamID, scoped.ViaToken = domain.TokenScopeSubmit, f.team, true
	// The customer's grant is revoked, so the account itself may no longer
	// submit either: the scope never widens the account's own right.
	if _, err := f.svc.SubmitRequest(f.ctx, scoped, f.submission("")); err == nil {
		t.Fatal("a scoped token submitted where its account's grant was revoked")
	}
	if _, err := f.svc.Repo.GrantTeamSubmit(f.ctx, f.team, f.org, f.customer, nil); err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	if _, err := f.svc.SubmitRequest(f.ctx, scoped, f.submission("")); err != nil {
		t.Fatalf("scoped token on its own team: %v", err)
	}
	foreign := f.submission("")
	foreign.TeamID = f.otherTeam
	if _, err := f.svc.SubmitRequest(f.ctx, scoped, foreign); authz.StatusOf(err, 500) != 404 {
		t.Fatalf("scoped token on another team = %v (status %d), want 404", err, authz.StatusOf(err, 500))
	}

	// A cross-org team: member, customer and admin of THIS org may not submit
	// there, and they learn nothing about the other tenant's team.
	crossOrg := f.submission("")
	crossOrg.TeamID = f.foreign
	for _, who := range []int{f.member, f.customer, f.admin} {
		if _, err := f.svc.SubmitRequest(f.ctx, f.principal(who), crossOrg); authz.StatusOf(err, 500) != 404 {
			t.Fatalf("user %d submitted to a foreign org's team: %v (status %d)", who, err, authz.StatusOf(err, 500))
		}
	}
	// The other tenant's own owner may submit there and gets the other team's
	// org, never this one's.
	elsewhere, err := f.svc.SubmitRequest(f.ctx, f.principal(f.stranger), crossOrg)
	if err != nil {
		t.Fatalf("foreign org owner submit: %v", err)
	}
	if elsewhere.Request.OrgID != f.otherOrg {
		t.Fatalf("cross-org request stored org %d, want the team's own %d", elsewhere.Request.OrgID, f.otherOrg)
	}

	// An unknown team is the same not-found.
	unknown := f.submission("")
	unknown.TeamID = 0
	if _, err := f.svc.SubmitRequest(f.ctx, f.principal(f.admin), unknown); authz.StatusOf(err, 500) != 404 {
		t.Fatalf("submission to team 0 = %v, want 404", err)
	}

	// A global admin with no user row (local dev before provisioning) has no
	// requester to snapshot, so the submission is refused rather than stored
	// against nobody — as a refusal the caller can read, never a server fault.
	rowsBefore := f.requestCount()
	anon := authz.Principal{Super: true, Channel: auth.ChannelUI, AuthMethod: auth.AuthLocalDev}
	_, err = f.svc.SubmitRequest(f.ctx, anon, f.submission(""))
	if err == nil {
		t.Fatal("a principal with no AMP user submitted")
	}
	if status := authz.StatusOf(err, 500); status != 403 {
		t.Fatalf("the no-account refusal = %v (status %d), want a 403 about the missing account", err, status)
	}
	if got := f.requestCount(); got != rowsBefore {
		t.Fatalf("a principal with no account wrote %d request rows", got-rowsBefore)
	}

	// Only the requests that were authorized exist in this org: the granted
	// customer's first submit and its re-granted scoped submit. The foreign org
	// owner's request landed in their own tenant, which is the point of the
	// cross-tenant half above.
	if got, want := f.requestCount(), 2; got != want {
		t.Fatalf("%d request rows in this org after the refused attempts, want %d", got, want)
	}
	var elsewhereRows int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM team_requests WHERE org_id = $1`, f.otherOrg).Scan(&elsewhereRows); err != nil {
		t.Fatalf("count the other org's requests: %v", err)
	}
	if elsewhereRows != 1 {
		t.Fatalf("the foreign org has %d requests, want its owner's 1", elsewhereRows)
	}
	for _, target := range []int{granted.Request.ID} {
		if n := f.submitEvents(target); n != 1 {
			t.Fatalf("the refused attempts appended %d extra history rows to request %d", n-1, target)
		}
	}
}

// ---- criterion 3: audited once, quota preserved ----

// A fresh submission is audited exactly once, a replay not at all; the limiter
// charges one unit per attempt and gives it back on a replay; and a burst over
// the limit is the coded 429 carrying its wait.
func TestSubmitRequest_AuditsOnceAndKeepsQuota(t *testing.T) {
	f := newSubmitFixture(t)
	f.withClock(f.now)
	member := f.principal(f.member)
	key := f.webKey(f.member)

	in := f.submission("audit-key-1")
	first, err := f.svc.SubmitRequest(f.ctx, member, in)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if n := len(f.audits(first.Request.ID)); n != 1 {
		t.Fatalf("a fresh submit wrote %d audit rows, want exactly 1", n)
	}
	entry := f.audits(first.Request.ID)[0]
	if entry.OrgID == nil || *entry.OrgID != f.org {
		t.Fatalf("audit org = %v, want the team's org %d", entry.OrgID, f.org)
	}
	if entry.TargetType != "request" || entry.TargetName != first.Request.PublicRef {
		t.Fatalf("audit target = %s/%q, want request/%q", entry.TargetType, entry.TargetName, first.Request.PublicRef)
	}
	if entry.ActorUserID == nil || *entry.ActorUserID != f.member {
		t.Fatalf("audit actor = %v, want %d", entry.ActorUserID, f.member)
	}
	if created, _ := entry.Details["created"].(bool); !created {
		t.Fatalf("audit details do not say the submit created a request: %v", entry.Details)
	}
	// The row is written through the service's normal audit path, so it carries
	// the actor type, channel and address context every other audited change has.
	if entry.ActorType != domain.AuditActorHuman {
		t.Fatalf("audit actor_type = %q, want %q for a person in the web UI", entry.ActorType, domain.AuditActorHuman)
	}
	if got := entry.Details["channel"]; got != auth.ChannelUI {
		t.Fatalf("audit channel = %v, want %q", got, auth.ChannelUI)
	}
	if entry.AuthMethod != auth.AuthDexSession {
		t.Fatalf("audit auth_method = %q, want %q", entry.AuthMethod, auth.AuthDexSession)
	}
	// The row names ids and the source, never the request's content or the
	// requester's contact address.
	for _, banned := range []string{first.Request.InputBrief, first.Request.RequesterContact} {
		if banned != "" && strings.Contains(entry.Summary+fmt.Sprint(entry.Details), banned) {
			t.Fatalf("the audit row carries %q", banned)
		}
	}
	if n := f.limitCount(key); n != 1 {
		t.Fatalf("a fresh submit charged %d units, want 1", n)
	}
	// Nothing downstream happened: submitting plans, approves and dispatches
	// nothing (§3), so no ticket exists in this project and no task was touched.
	var tasks int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM tasks t JOIN stories s ON s.id = t.story_id
		JOIN epics e ON e.id = s.epic_id WHERE e.project_id = $1`, f.project).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("submitting created %d tickets in the inbox's project, want 0", tasks)
	}

	// The replay: no audit row, no quota spent.
	replay, err := f.svc.SubmitRequest(f.ctx, member, in)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Created {
		t.Fatal("the replay reported a create")
	}
	if n := len(f.audits(first.Request.ID)); n != 1 {
		t.Fatalf("a replay wrote an audit row: %d rows, want the original 1", n)
	}
	// One unit for the fresh submit, none for the replay: the replay's charge is
	// refunded in the same window, so the key is back to exactly one.
	if n := f.limitCount(key); n != 1 {
		t.Fatalf("a fresh submit plus its replay left %d units spent, want 1", n)
	}

	// A burst over the limit is refused with the coded 429 and its wait.
	f.limits = NewRequestLimiter(RequestLimitConfig{Window: time.Minute, Max: 1})
	f.svc.RequestLimits = f.limits
	if _, err := f.svc.SubmitRequest(f.ctx, member, f.submission("")); err != nil {
		t.Fatalf("first fresh submit under the limit: %v", err)
	}
	_, err = f.svc.SubmitRequest(f.ctx, member, f.submission(""))
	if err == nil {
		t.Fatal("the limiter let a burst through")
	}
	var limited *SubmitRateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("rate-limit refusal = %T %v, want *SubmitRateLimited", err, err)
	}
	if limited.RetryAfterS < 1 {
		t.Fatalf("rate-limit refusal retry-after = %ds, want at least 1", limited.RetryAfterS)
	}
	if code := authz.CodeOf(err); code != authz.CodeRequestRateLimited {
		t.Fatalf("rate-limit code = %q, want %q", code, authz.CodeRequestRateLimited)
	}
	if status := authz.StatusOf(err, 500); status != 429 {
		t.Fatalf("rate-limit status = %d, want 429", status)
	}
	// The refusal wrote nothing: the first submit (row 1) and the one under the
	// new single-unit limit (row 2) exist, and the refused burst is a third
	// request only if the limiter let it through.
	if got, want := f.requestCount(), 2; got != want {
		t.Fatalf("%d request rows after a refused burst, want %d", got, want)
	}

	// The next window is a fresh budget on the same key — the fixed-window
	// contract the limiter promises.
	f.now = f.now.Add(time.Minute)
	next, err := f.svc.SubmitRequest(f.ctx, member, f.submission(""))
	if err != nil {
		t.Fatalf("submit in the next window: %v", err)
	}
	if !next.Created {
		t.Fatal("the next window's submission was answered as a replay")
	}
}

// ---- no lifecycle, no project access ----

// A created request is `new` at rev 1 with no claim, owner, duplicate, reason or
// timestamp, no link, and no state this path could move; and submitting grants
// the requester no project access at all.
func TestSubmitRequest_WritesNoLifecycleState(t *testing.T) {
	f := newSubmitFixture(t)
	f.withClock(f.now)
	customer := f.principal(f.customer)

	before, err := f.svc.Authz.VisibleProjects(f.ctx, customer)
	if err != nil {
		t.Fatalf("visible projects before: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("the customer could see %d projects before submitting, want 0", len(before))
	}
	res, err := f.svc.SubmitRequest(f.ctx, customer, f.submission(""))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	got := res.Request
	if got.State != domain.RequestStatusNew {
		t.Fatalf("state = %q, want new", got.State)
	}
	if got.Rev != 1 {
		t.Fatalf("rev = %d, want 1", got.Rev)
	}
	if got.ClaimUserID != nil || got.ClaimedAt != nil {
		t.Fatalf("a submission took a triage claim: user %v at %v", got.ClaimUserID, got.ClaimedAt)
	}
	if got.OwnerUserID != nil {
		t.Fatalf("a submission assigned an owner: %v", got.OwnerUserID)
	}
	if got.DuplicateOf != nil || got.RejectReason != "" || got.OutcomeBrief != "" {
		t.Fatalf("a submission carried triage fields: duplicate %v reason %q outcome %q",
			got.DuplicateOf, got.RejectReason, got.OutcomeBrief)
	}
	if got.NeedsInfoSince != nil || got.PlannedAt != nil || got.DeliveredAt != nil ||
		got.AcceptedAt != nil || got.AcceptedBy != nil || got.ClosedAt != nil || got.ReopenedAt != nil {
		t.Fatal("a submission set a lifecycle timestamp")
	}
	if n := f.linkCount(got.ID); n != 0 {
		t.Fatalf("a submission wrote %d links; it plans, routes and approves nothing", n)
	}
	after, err := f.svc.Authz.VisibleProjects(f.ctx, customer)
	if err != nil {
		t.Fatalf("visible projects after: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("submitting granted access to %d projects, want 0", len(after))
	}
	// The project the inbox team is linked to is not one of them either: a
	// submission is not a route (D9).
	if after[f.project] {
		t.Fatalf("submitting granted the inbox's project %d", f.project)
	}
}

// ---- pure: the shapers name the field ----

// Every malformed submission is refused as request.invalid naming the field it
// got wrong, before anything is written and without echoing the content back.
func TestShapeSubmission_NamesTheField(t *testing.T) {
	team := &domain.Team{ID: 7, OrgID: 3, Name: "Inbox"}
	requester := 42
	cases := []struct {
		name string
		in   SubmitRequestInput
		want string // a substring the refusal must contain (the field)
	}{
		{"no subject", SubmitRequestInput{Subject: "  "}, "subject"},
		{"subject over the bound", SubmitRequestInput{Subject: strings.Repeat("s", domain.MaxRequestSubjectLen+1)}, "subject"},
		{"brief over the bound", SubmitRequestInput{Subject: "ok", InputBrief: strings.Repeat("b", domain.MaxRequestBriefLen+1)}, "input_brief"},
		{"key over the bound", SubmitRequestInput{Subject: "ok", IdempotencyKey: strings.Repeat("k", maxRequestIdempotencyKeyLen+1)}, "idempotency_key"},
		{"connector without an external id", SubmitRequestInput{Subject: "ok", Source: domain.RequestSourceConnector}, "source_external_id"},
		{"web with an external id", SubmitRequestInput{Subject: "ok", Source: domain.RequestSourceWeb, SourceExternalID: "issue-1"}, "source_external_id"},
		{"unknown source", SubmitRequestInput{Subject: "ok", Source: "carrier-pigeon"}, "source_kind"},
		{"metadata value over the bound", SubmitRequestInput{Subject: "ok",
			Metadata: domain.Metadata{"x": strings.Repeat("v", domain.MaxMetadataValueLen+1)}}, "metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := shapeSubmission(team, &requester, "someone@example.test", tc.in)
			if err == nil {
				t.Fatalf("shapeSubmission accepted %+v", tc.in)
			}
			if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
				t.Fatalf("code = %q, want %q", code, authz.CodeRequestInvalid)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %q does not name %q", err.Error(), tc.want)
			}
			// The rejected content never comes back: a shared inbox's field may be
			// another party's text.
			if tc.in.InputBrief != "" && strings.Contains(err.Error(), tc.in.InputBrief) {
				t.Fatalf("refusal echoed the brief: %q", err.Error())
			}
		})
	}

	// A well-formed submission is shaped into the stored record: tenant from the
	// team, requester and contact from the caller, normalized text, rev 1, new.
	shaped, key, err := shapeSubmission(team, &requester, "  someone@example.test  ", SubmitRequestInput{
		Subject: "  Fix the export  ", InputBrief: "  it is broken  ", IdempotencyKey: "  key-1  ",
		Metadata: domain.Metadata{"connector_label": "desk", "dropped": nil},
	})
	if err != nil {
		t.Fatalf("shapeSubmission refused a good submission: %v", err)
	}
	if key != "key-1" {
		t.Fatalf("key = %q, want the trimmed key", key)
	}
	if shaped.Subject != "Fix the export" || shaped.InputBrief != "it is broken" {
		t.Fatalf("text was not normalized: subject %q brief %q", shaped.Subject, shaped.InputBrief)
	}
	if shaped.OrgID != team.OrgID || shaped.TeamID != team.ID {
		t.Fatalf("tenant = org %d team %d, want the team's own org %d team %d", shaped.OrgID, shaped.TeamID, team.OrgID, team.ID)
	}
	if shaped.RequesterUserID == nil || *shaped.RequesterUserID != requester || shaped.RequesterContact != "someone@example.test" {
		t.Fatalf("requester = %v %q, want the caller's", shaped.RequesterUserID, shaped.RequesterContact)
	}
	if shaped.State != domain.RequestStatusNew || shaped.Rev != 1 {
		t.Fatalf("shaped state/rev = %s/%d, want new/1", shaped.State, shaped.Rev)
	}
	if _, ok := shaped.Metadata["dropped"]; ok {
		t.Fatalf("a null metadata value was stored: %v", shaped.Metadata)
	}
	if _, ok := shaped.Metadata["connector_label"]; !ok {
		t.Fatalf("a real metadata value was dropped: %v", shaped.Metadata)
	}
}

// A minted reference is the domain's opaque shape — long, lettered, and
// different every time — so nothing about one request can be guessed from
// another's reference.
func TestNewRequestPublicRef_IsOpaqueAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := range 200 {
		ref, err := newRequestPublicRef()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		norm, err := domain.NormalizeRequestPublicRef(ref)
		if err != nil {
			t.Fatalf("minted %q is not the domain's shape: %v", ref, err)
		}
		if norm != ref {
			t.Fatalf("minted %q normalizes to %q: not stored as minted", ref, norm)
		}
		if len(ref) < domain.MinRequestPublicRefLen {
			t.Fatalf("minted %q is shorter than the %d-character floor", ref, domain.MinRequestPublicRefLen)
		}
		if seen[ref] {
			t.Fatalf("minted %q twice in %d attempts", ref, i+1)
		}
		seen[ref] = true
	}
}

// The repository's answers become the surface's codes.
func TestSubmitRequestError_MapsCodes(t *testing.T) {
	conflict := submitRequestError(9, fmt.Errorf("wrap: %w", &repository.RequestIdempotencyConflict{RequestID: 12, Submitter: 3, Source: domain.RequestSourceWeb}))
	if authz.CodeOf(conflict) != authz.CodeRequestConflict || authz.StatusOf(conflict, 500) != 409 {
		t.Fatalf("conflict mapped to %v (%q/%d)", conflict, authz.CodeOf(conflict), authz.StatusOf(conflict, 500))
	}
	submitter := submitRequestError(9, fmt.Errorf("wrap: %w", repository.ErrRequestSubmitter))
	if authz.CodeOf(submitter) != authz.CodeRequestInvalid || authz.StatusOf(submitter, 500) != 400 {
		t.Fatalf("missing submitter mapped to %v (%q/%d)", submitter, authz.CodeOf(submitter), authz.StatusOf(submitter, 500))
	}
	gone := submitRequestError(9, fmt.Errorf("wrap: %w", repository.ErrNotFound))
	if authz.StatusOf(gone, 500) != 404 {
		t.Fatalf("a vanished record mapped to %d, want 404", authz.StatusOf(gone, 500))
	}
	// Anything else keeps its own identity, so a dead connection reads as one.
	boom := errors.New("connection reset")
	if got := submitRequestError(9, boom); !errors.Is(got, boom) {
		t.Fatalf("an internal failure lost its identity: %v", got)
	}
}
