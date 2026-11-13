package main

// Team request reads over REST (epic 202, story 549, AMP #3968; build slice F1's
// list half and F2's read half; approved contract artifact 36 v2 §2, §4, §5).
//
// internal/service/request_views_test.go already proves the one authorized read
// path (S2): who may see what, the cursor-free counts, the requester projection
// and the 404s. So this file asks only the questions that are about *the routes*:
//
//	Criterion 1 — every read enforces the view matrix:
//	  TestRESTRequestQueue_ViewMatrix (a member reads the queue; a requester, a
//	  stranger and an unknown team all answer the one 404; a submit-only credential
//	  is refused on all five routes with token.scope_denied)
//	  TestRESTRequestDetail_ViewMatrix (the team's view is the record; a requester
//	  who is not a member gets the projection; another requester, a sibling team's
//	  request and an id that never existed are the same bytes)
//	Criterion 2 — pagination, counts and the coded error contract are tested:
//	  TestRESTRequestQueue_PagingAndCounts (keyset pages hold no duplicate row and
//	  the totals equal the filter total on every page; the summary route answers the
//	  same numbers; filters narrow page and counts together)
//	  TestRESTRequestReads_CodedRefusals (each malformed parameter is 400
//	  request.invalid and names itself; a request read never writes an audit row)
//	Criterion 3 — no internal DTO reaches a requester:
//	  TestRESTOwnRequests_ProjectionLeaksNothing (the own list and the own detail
//	  carry the whitelist's keys and none of the record's internal ones, checked in
//	  the raw response body)
//
// Everything goes through the real mux (api.RestHandler.Register, so deniedMux,
// the submit-scope gate and the denied() audit sit in front of the handlers) and
// the real auth.NewAPITokenVerifier, on a scratch database per test, so the
// statuses, codes and rows are the product's and not a stub's.
//
// Run against a disposable Postgres (never a live stack), per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRESTRequest|TestRESTOwnRequests' -count=1 -race -v

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// readsWorld is one tenant with an inbox team, a sibling team in the same org, a
// second org's team, and the people a request read's caller can be. Its requests
// are created through the real submission service, so every row is the product's.
type readsWorld struct {
	t   *testing.T
	srv *httptest.Server
	ctx context.Context
	db  *pgx.Conn
	svc *service.Service

	org, team, otherTeam, otherOrg, foreign int

	member    *auth.Identity // member of the inbox team
	admin     *auth.Identity // owner of the inbox's org
	requester *auth.Identity // customer with a submit grant on the inbox
	other     *auth.Identity // another customer, no relationship to the requests
	stranger  *auth.Identity // owner of another org

	memberTok, requesterTok, otherTok, strangerTok string
	// adminTok is minted on first use (an org owner's token).
	adminTok string
	// scopedTok is a submit-only credential bound to the inbox: it may POST and
	// nothing else, every read on this surface included.
	scopedTok string

	// mined are the inbox's four requests, in creation order:
	//   [0] requester's, assigned to member
	//   [1] requester's, claimed by member, state triaging
	//   [2] other customer's
	//   [3] member's own (a team member submitting to their own inbox)
	mined []*domain.TeamRequest
	// sibling lives in otherTeam (same org) and foreign in another org; neither
	// may ever appear in the inbox's queue.
	sibling, foreignReq *domain.TeamRequest
}

func newReadsWorld(t *testing.T) *readsWorld {
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

	w := &readsWorld{t: t, ctx: ctx, db: db, svc: nil}
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
	sfx := uniq("readsrest")
	// The first sign-in in a scratch database is a global admin, and a global
	// admin reaches every inbox by design (authz.InboxRole) — so a throwaway
	// bootstrap account takes that slot and every person tested with is pinned.
	if _, err := repo.UpsertUserFromClaims(ctx, sfx+"-bootstrap",
		"bootstrap-"+sfx+"@reads-rest.test", "bootstrap", nil); err != nil {
		t.Fatalf("bootstrap user: %v", err)
	}
	user := func(label, kind string) *auth.Identity {
		u, err := repo.UpsertUserFromClaims(ctx, sfx+"-"+label,
			fmt.Sprintf("%s-%s@reads-rest.test", label, sfx), label, nil)
		if err != nil {
			t.Fatalf("user %s: %v", label, err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleMember, true); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleAdmin, false); err != nil {
			t.Fatalf("pin %s as a non-admin: %v", label, err)
		}
		if kind == domain.UserKindCustomer {
			exec(`UPDATE users SET kind = $2 WHERE id = $1`, u.ID, kind)
			u.Kind = kind
		}
		return &auth.Identity{Subject: u.Subject, Email: u.Email, Name: label, User: u}
	}

	w.org = insert(`INSERT INTO orgs (name, slug) VALUES ('Reads org', $1) RETURNING id`, sfx)
	w.team = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Reads desk') RETURNING id`, w.org)
	w.otherTeam = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Reads sibling desk') RETURNING id`, w.org)
	w.otherOrg = insert(`INSERT INTO orgs (name, slug) VALUES ('Reads other org', $1) RETURNING id`, sfx+"-o")
	w.foreign = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign desk') RETURNING id`, w.otherOrg)

	w.stranger = user("stranger", domain.UserKindHuman)
	w.member = user("member", domain.UserKindHuman)
	w.admin = user("admin", domain.UserKindHuman)
	w.requester = user("requester", domain.UserKindCustomer)
	w.other = user("othercustomer", domain.UserKindCustomer)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.org, w.member.User.ID)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, w.team, w.member.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.org, w.admin.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.otherOrg, w.other.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.otherOrg, w.stranger.User.ID)
	if _, err := repo.GrantTeamSubmit(ctx, w.team, w.org, w.requester.User.ID, nil); err != nil {
		t.Fatalf("grant the requester: %v", err)
	}
	// The second requester sends to the same desk: their request lives in the
	// inbox and belongs to nobody else's "own" list.
	if _, err := repo.GrantTeamSubmit(ctx, w.team, w.org, w.other.User.ID, nil); err != nil {
		t.Fatalf("grant the second requester: %v", err)
	}

	// The handler's own service behind the real REST mux and token verifier.
	mux := http.NewServeMux()
	h := api.NewRestHandler(nil, repo, nil, nil)
	w.svc = h.Service()
	h.Register(mux)
	w.srv = httptest.NewServer(auth.NewAPITokenVerifier(repo).Middleware(mux))
	t.Cleanup(w.srv.Close)

	w.memberTok = w.mintToken(w.member)
	w.requesterTok = w.mintToken(w.requester)
	w.otherTok = w.mintToken(w.other)
	w.strangerTok = w.mintToken(w.stranger)
	ui := authz.Principal{UserID: w.member.User.ID, Email: w.member.Email,
		Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	scoped, err := w.svc.CreateScopedToken(ctx, ui, w.member.User.ID, "reads-scoped", 0,
		service.TokenScopeRequest{Scope: "submit", TeamID: w.team})
	if err != nil {
		t.Fatalf("mint submit-only token: %v", err)
	}
	w.scopedTok = scoped.Token

	// The four inbox requests, through the real submission path.
	submit := func(who *auth.Identity, teamID int, subject string) *domain.TeamRequest {
		t.Helper()
		res, err := w.svc.SubmitRequest(ctx, w.principal(who.User.ID), service.SubmitRequestInput{
			TeamID: teamID, Source: domain.RequestSourceWeb,
			Subject: subject, InputBrief: "The details are in the ticket body.",
		})
		if err != nil {
			t.Fatalf("submit %q: %v", subject, err)
		}
		return res.Request
	}
	w.mined = []*domain.TeamRequest{
		submit(w.requester, w.team, "Export the queue as CSV"),
		submit(w.requester, w.team, "The dashboard total looks wrong"),
		submit(w.other, w.team, "Please add a dark theme"),
		submit(w.member, w.team, "A team member's own request"),
	}
	// r0 is assigned to the member, r1 is claimed by them (D4's two distinct
	// fields, which is exactly what "mine" reads).
	exec(`UPDATE team_requests SET owner_user_id = $2 WHERE id = $1`, w.mined[0].ID, w.member.User.ID)
	exec(`UPDATE team_requests SET claim_user_id = $2, claimed_at = NOW(), state = 'triaging' WHERE id = $1`,
		w.mined[1].ID, w.member.User.ID)
	w.sibling = submit(w.admin, w.otherTeam, "A sibling desk's request")
	w.foreignReq = submit(w.stranger, w.foreign, "Another org's request")
	return w
}

func (w *readsWorld) principal(userID int) authz.Principal {
	w.t.Helper()
	var email string
	if err := w.db.QueryRow(w.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		w.t.Fatalf("email of %d: %v", userID, err)
	}
	return authz.Principal{UserID: userID, Email: email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
}

func (w *readsWorld) mintToken(ident *auth.Identity) string {
	w.t.Helper()
	ui := authz.Principal{UserID: ident.User.ID, Email: ident.Email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	tok, err := w.svc.CreateToken(w.ctx, ui, ident.User.ID, "reads-"+uniq("t"), 0)
	if err != nil {
		w.t.Fatalf("mint a token: %v", err)
	}
	return tok.Token
}

// readsResult is one response: status, headers, the decoded body and the raw
// bytes (which is what a leak test has to read — a field can hide in a nested
// value).
type readsResult struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (w *readsWorld) do(token, path string) readsResult {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.srv.URL+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := readsResult{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// queuePath / summaryPath / detailPath name the team surface's reads.
func (w *readsWorld) queuePath(teamID int, query string) string {
	p := fmt.Sprintf("/api/teams/%d/requests", teamID)
	if query != "" {
		p += "?" + query
	}
	return p
}

func (w *readsWorld) summaryPath(teamID int, query string) string {
	p := fmt.Sprintf("/api/teams/%d/requests/summary", teamID)
	if query != "" {
		p += "?" + query
	}
	return p
}

func (w *readsWorld) detailPath(teamID int, request string) string {
	return fmt.Sprintf("/api/teams/%d/requests/%s", teamID, request)
}

// wantCode asserts a refusal's status and stable code.
func (w *readsWorld) wantCode(r readsResult, status int, code string) {
	w.t.Helper()
	if r.status != status {
		w.t.Fatalf("%d %v, want %d %s", r.status, r.body, status, code)
	}
	if got, _ := r.body["code"].(string); got != code {
		w.t.Fatalf("code = %v, want %q (body %v)", r.body["code"], code, r.body)
	}
}

// auditRows counts the audit rows this caller produced for one action.
func (w *readsWorld) auditRows(userID int, action string) int {
	w.t.Helper()
	rows, err := w.svc.Repo.ListAudit(w.ctx, domain.AuditFilter{ActorUserID: userID, Action: action, Limit: 200})
	if err != nil {
		w.t.Fatalf("read audit: %v", err)
	}
	return len(rows)
}

// ---- criterion 1: the view matrix, on the queue ----

// TestRESTRequestQueue_ViewMatrix: a member of the inbox reads the queue; a
// requester (who is not a member), a stranger and an unknown team id all answer
// the surface's single 404 with its stable code; and a submit-only credential is
// refused on every read route with token.scope_denied — the gate, not a handler,
// is what stops it.
func TestRESTRequestQueue_ViewMatrix(t *testing.T) {
	w := newReadsWorld(t)

	ok := w.do(w.memberTok, w.queuePath(w.team, ""))
	if ok.status != 200 {
		t.Fatalf("member queue = %d %v, want 200", ok.status, ok.body)
	}
	items, _ := ok.body["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("queue holds %d rows, want the inbox's 4 (a sibling team's and another org's must not appear)", len(items))
	}
	seen := map[int]bool{}
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		id, _ := row["id"].(float64)
		seen[int(id)] = true
	}
	for _, r := range w.mined {
		if !seen[r.ID] {
			t.Fatalf("request %d is missing from its own team's queue", r.ID)
		}
	}
	if seen[w.sibling.ID] || seen[w.foreignReq.ID] {
		t.Fatalf("the queue leaked a row from another team (sibling=%v foreign=%v)", seen[w.sibling.ID], seen[w.foreignReq.ID])
	}

	// A requester has no inbox: their own requests are the /api/requests
	// surface, never the team's queue (contract §2: "cannot read other
	// requests"), and the refusal does not confirm the team exists.
	notFound := w.do(w.requesterTok, w.queuePath(w.team, ""))
	w.wantCode(notFound, 404, authz.CodeRequestNotFound)
	msg := fmt.Sprint(notFound.body["error"])

	// A stranger owning another org, and a team id that was never issued, are
	// the same bytes as the requester's refusal.
	stranger := w.do(w.strangerTok, w.queuePath(w.team, ""))
	w.wantCode(stranger, 404, authz.CodeRequestNotFound)
	unknown := w.do(w.memberTok, w.queuePath(999999, ""))
	w.wantCode(unknown, 404, authz.CodeRequestNotFound)
	for name, r := range map[string]readsResult{"requester": notFound, "stranger": stranger, "unknown team": unknown} {
		if fmt.Sprint(r.body["error"]) != msg {
			t.Fatalf("%s refusal = %v, want the identical message %q", name, r.body["error"], msg)
		}
	}

	// The submit-only credential reaches exactly one call, and no read is it —
	// on every read route, and without depending on a handler existing.
	for _, path := range []string{
		w.queuePath(w.team, ""),
		w.summaryPath(w.team, ""),
		w.detailPath(w.team, strconv.Itoa(w.mined[0].ID)),
		"/api/requests",
		"/api/requests/" + strconv.Itoa(w.mined[0].ID),
	} {
		got := w.do(w.scopedTok, path)
		if got.status != 403 {
			t.Fatalf("submit-only %s = %d %v, want 403", path, got.status, got.body)
		}
		if code, _ := got.body["code"].(string); code != authz.CodeTokenScopeDenied {
			t.Fatalf("submit-only %s code = %v, want %q", path, got.body["code"], authz.CodeTokenScopeDenied)
		}
	}

	// Signed out is 401, not a 404: the read never ran.
	if anon := w.do("", w.queuePath(w.team, "")); anon.status != 401 {
		t.Fatalf("no token = %d, want 401", anon.status)
	}
}

// TestRESTRequestDetail_ViewMatrix: one request, seen through the two views. The
// team gets the record and its delivery aggregate; a requester who is not a
// member gets only the projection; another requester, a request in a sibling
// team and an id that never existed are one indistinguishable 404.
func TestRESTRequestDetail_ViewMatrix(t *testing.T) {
	w := newReadsWorld(t)
	r0 := w.mined[0]

	team := w.do(w.memberTok, w.detailPath(w.team, strconv.Itoa(r0.ID)))
	if team.status != 200 {
		t.Fatalf("member detail = %d %v, want 200", team.status, team.body)
	}
	if v, _ := team.body["view"].(string); v != "team" {
		t.Fatalf("view = %v, want \"team\" for an inbox member", team.body["view"])
	}
	rec, ok := team.body["request"].(map[string]any)
	if !ok {
		t.Fatalf("team answer has no request record: %v", team.body)
	}
	// The record is the record: internal triage state reaches the member, which
	// is the whole point of the team view.
	for _, key := range []string{"org_id", "team_id", "claim_user_id", "owner_user_id", "rev", "requester_contact", "metadata"} {
		if _, present := rec[key]; !present {
			t.Fatalf("team record is missing %q: %v", key, rec)
		}
	}
	if _, ok := team.body["outcome"].(map[string]any); !ok {
		t.Fatalf("team answer carries no delivery aggregate: %v", team.body)
	}
	if _, leaked := team.body["projection"]; leaked {
		t.Fatalf("the team answer also carries a requester projection: %v", team.body)
	}

	// The requester sees their own request through the same route, and gets the
	// projection — never the record.
	own := w.do(w.requesterTok, w.detailPath(w.team, strconv.Itoa(r0.ID)))
	if own.status != 200 {
		t.Fatalf("requester detail = %d %v, want 200", own.status, own.body)
	}
	if v, _ := own.body["view"].(string); v != "requester" {
		t.Fatalf("view = %v, want \"requester\"", own.body["view"])
	}
	if _, hasRecord := own.body["request"]; hasRecord {
		t.Fatalf("a requester's answer carries the stored record: %v", own.body)
	}
	if _, hasOutcome := own.body["outcome"]; hasOutcome {
		t.Fatalf("a requester's answer carries the team's delivery aggregate: %v", own.body)
	}
	proj, ok := own.body["projection"].(map[string]any)
	if !ok {
		t.Fatalf("requester answer has no projection: %v", own.body)
	}
	if got, _ := proj["public_ref"].(string); got != r0.PublicRef {
		t.Fatalf("projection public_ref = %v, want %q", proj["public_ref"], r0.PublicRef)
	}

	// Another requester's request, a sibling team's request read through the
	// inbox's own path, and an id that never existed are the same bytes.
	other := w.do(w.otherTok, w.detailPath(w.team, strconv.Itoa(r0.ID)))
	w.wantCode(other, 404, authz.CodeRequestNotFound)
	sibling := w.do(w.memberTok, w.detailPath(w.team, strconv.Itoa(w.sibling.ID)))
	w.wantCode(sibling, 404, authz.CodeRequestNotFound)
	unknown := w.do(w.memberTok, w.detailPath(w.team, "999999"))
	w.wantCode(unknown, 404, authz.CodeRequestNotFound)
	if other.raw != unknown.raw || sibling.raw != unknown.raw {
		t.Fatalf("404 bodies differ:\n other=%s sibling=%s unknown=%s", other.raw, sibling.raw, unknown.raw)
	}

	// A request is also addressable by the opaque reference the submission
	// answered with, including the display form (D1).
	byRef := w.do(w.memberTok, w.detailPath(w.team, "REQ-"+r0.PublicRef))
	if byRef.status != 200 {
		t.Fatalf("detail by reference = %d %v, want 200", byRef.status, byRef.body)
	}
	// A reference from another tenant is the same 404.
	if foreignRef := w.do(w.memberTok, w.detailPath(w.team, w.foreignReq.PublicRef)); foreignRef.status != 404 {
		t.Fatalf("a foreign reference = %d, want 404", foreignRef.status)
	}
	// A value that could never be a reference is the caller's mistake: 400, not
	// a 404 that would report "no such request" for it.
	if bogus := w.do(w.memberTok, w.detailPath(w.team, "not-a-reference!")); bogus.status != 400 {
		t.Fatalf("bogus reference = %d %v, want 400", bogus.status, bogus.body)
	}
}

// ---- criterion 2: paging, counts and coded refusals ----

// TestRESTRequestQueue_PagingAndCounts walks the queue page by page and compares
// it with the counts beside it. The pill numbers are the filter's total, not the
// page's remainder, so they must not move as someone scrolls; the summary route
// must answer the same numbers; and a filter must narrow both the page and the
// counts together, never one of them.
func TestRESTRequestQueue_PagingAndCounts(t *testing.T) {
	w := newReadsWorld(t)
	// Four more rows, so paging is not one page of four.
	for i := 0; i < 4; i++ {
		if _, err := w.svc.SubmitRequest(w.ctx, w.principal(w.requester.User.ID), service.SubmitRequestInput{
			TeamID: w.team, Source: domain.RequestSourceWeb,
			Subject: fmt.Sprintf("Extra request %d %s", i, uniq("x")),
		}); err != nil {
			t.Fatalf("submit extra %d: %v", i, err)
		}
	}

	first := w.do(w.memberTok, w.queuePath(w.team, "limit=3"))
	if first.status != 200 {
		t.Fatalf("first page = %d %v", first.status, first.body)
	}
	if got, _ := first.body["count"].(float64); got != 3 {
		t.Fatalf("count = %v, want the page's 3 rows", first.body["count"])
	}
	totals, ok := first.body["totals"].(map[string]any)
	if !ok {
		t.Fatalf("queue has no totals: %v", first.body)
	}
	if got, _ := totals["total"].(float64); got != 8 {
		t.Fatalf("totals.total = %v, want 8", totals["total"])
	}
	states, _ := totals["states"].(map[string]any)
	if len(states) != len(domain.RequestStatuses) {
		t.Fatalf("totals.states holds %d keys, want all %d (zeros included)", len(states), len(domain.RequestStatuses))
	}
	var sum float64
	for _, v := range states {
		n, _ := v.(float64)
		sum += n
	}
	if sum != 8 {
		t.Fatalf("the per-state counts sum to %v, want 8", sum)
	}

	// Page to the end and assert no duplicate, no skip, and the same totals.
	var ids []int
	cursor := 0
	for page := 0; page < 10; page++ {
		q := "limit=3"
		if cursor != 0 {
			q += "&before_id=" + strconv.Itoa(cursor)
		}
		res := w.do(w.memberTok, w.queuePath(w.team, q))
		if res.status != 200 {
			t.Fatalf("page %d = %d %v", page, res.status, res.body)
		}
		items, _ := res.body["items"].([]any)
		for _, raw := range items {
			row, _ := raw.(map[string]any)
			id, _ := row["id"].(float64)
			ids = append(ids, int(id))
		}
		paged, _ := res.body["totals"].(map[string]any)
		if got, _ := paged["total"].(float64); got != 8 {
			t.Fatalf("page %d totals.total = %v, want the filter total 8 (a pill must not shrink as someone pages)", page, paged["total"])
		}
		next, _ := res.body["next_before_id"].(float64)
		if next == 0 {
			break
		}
		cursor = int(next)
	}
	if len(ids) != 8 {
		t.Fatalf("paging yielded %d rows, want 8: %v", len(ids), ids)
	}
	if unique := uniqueInts(ids); len(unique) != 8 {
		t.Fatalf("paging repeated a row: %v", ids)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] >= ids[i-1] {
			t.Fatalf("paging is not strictly id-DESC: %v", ids)
		}
	}

	// The summary route answers the same aggregates without a page.
	summary := w.do(w.memberTok, w.summaryPath(w.team, ""))
	if summary.status != 200 {
		t.Fatalf("summary = %d %v", summary.status, summary.body)
	}
	if _, hasItems := summary.body["items"]; hasItems {
		t.Fatalf("summary carries a page: %v", summary.body)
	}
	summaryTotals, _ := summary.body["totals"].(map[string]any)
	if got, _ := summaryTotals["total"].(float64); got != 8 {
		t.Fatalf("summary totals.total = %v, want 8", summaryTotals["total"])
	}

	// A state filter narrows the page AND the counts: the page holds only new
	// requests and every other state reads 0. A filter given is a filter applied.
	filtered := w.do(w.memberTok, w.queuePath(w.team, "state=new"))
	if filtered.status != 200 {
		t.Fatalf("state filter = %d %v", filtered.status, filtered.body)
	}
	fitems, _ := filtered.body["items"].([]any)
	for _, raw := range fitems {
		row, _ := raw.(map[string]any)
		if row["state"] != string(domain.RequestStatusNew) {
			t.Fatalf("a filtered page carries a %v request", row["state"])
		}
	}
	ftotals, _ := filtered.body["totals"].(map[string]any)
	if got, _ := ftotals["total"].(float64); got != float64(len(fitems)) {
		t.Fatalf("filtered totals.total = %v but its page holds %d rows", ftotals["total"], len(fitems))
	}
	fstates, _ := ftotals["states"].(map[string]any)
	if triaging, _ := fstates[string(domain.RequestStatusTriaging)].(float64); triaging != 0 {
		t.Fatalf("a new-only filter counts %v triaging requests", triaging)
	}

	// "Mine" is the caller's own claimed/assigned slice of the same queue.
	mine := w.do(w.memberTok, w.queuePath(w.team, "mine=true"))
	if mine.status != 200 {
		t.Fatalf("mine = %d %v", mine.status, mine.body)
	}
	owned, ok := mine.body["mine"].(map[string]any)
	if !ok {
		t.Fatalf("mine=true answered no slice: %v", mine.body)
	}
	claimed := intSlice(owned["claimed_ids"])
	assigned := intSlice(owned["assigned_ids"])
	all := intSlice(owned["ids"])
	if len(claimed) != 1 || claimed[0] != w.mined[1].ID {
		t.Fatalf("claimed_ids = %v, want [%d]", claimed, w.mined[1].ID)
	}
	if len(assigned) != 1 || assigned[0] != w.mined[0].ID {
		t.Fatalf("assigned_ids = %v, want [%d]", assigned, w.mined[0].ID)
	}
	if len(all) != 2 {
		t.Fatalf("mine.ids = %v, want the two rows (each once)", all)
	}
	if got, _ := owned["total"].(float64); got != 2 {
		t.Fatalf("mine.total = %v, want 2", owned["total"])
	}
	// A caller with nothing of their own gets no slice rather than an error, and
	// the queue itself is unchanged.
	adminMine := w.do(w.adminTokOrFail(t), w.queuePath(w.team, "mine=true"))
	if adminMine.status != 200 {
		t.Fatalf("admin queue = %d %v", adminMine.status, adminMine.body)
	}
}

// adminTokOrFail mints (once) and returns the org owner's token.
func (w *readsWorld) adminTokOrFail(t *testing.T) string {
	t.Helper()
	if w.adminTok == "" {
		w.adminTok = w.mintToken(w.admin)
	}
	return w.adminTok
}

// TestRESTRequestReads_CodedRefusals pins the error contract: each malformed
// parameter is a 400 carrying request.invalid and the parameter's name, and a
// read is an act of looking, not an act on a request — so it writes no audit row
// and no access.denied.
func TestRESTRequestReads_CodedRefusals(t *testing.T) {
	w := newReadsWorld(t)
	before := w.auditRows(w.member.User.ID, "request.read")

	cases := []struct {
		name string
		path string
		code string
	}{
		{"unknown state", w.queuePath(w.team, "state=almost_done"), authz.CodeRequestInvalid},
		{"empty state", w.queuePath(w.team, "state="), authz.CodeRequestInvalid},
		{"unknown source kind", w.queuePath(w.team, "source_kind=carrier_pigeon"), authz.CodeRequestInvalid},
		{"non-numeric before_id", w.queuePath(w.team, "before_id=abc"), authz.CodeRequestInvalid},
		{"negative before_id", w.queuePath(w.team, "before_id=-1"), authz.CodeRequestInvalid},
		{"non-numeric limit", w.queuePath(w.team, "limit=lots"), authz.CodeRequestInvalid},
		{"bad created_after", w.queuePath(w.team, "created_after=yesterday"), authz.CodeRequestInvalid},
		{"bad attached", w.queuePath(w.team, "attached=maybe"), authz.CodeRequestInvalid},
		{"bad mine", w.queuePath(w.team, "mine=maybe"), authz.CodeRequestInvalid},
		{"bad submitter", w.queuePath(w.team, "submitter=somebody"), authz.CodeRequestInvalid},
		{"summary shares the filters", w.summaryPath(w.team, "state=almost_done"), authz.CodeRequestInvalid},
		{"own list bad before_id", "/api/requests?before_id=abc", authz.CodeRequestInvalid},
		{"own list bad state", "/api/requests?state=almost_done", authz.CodeRequestInvalid},
		{"own detail bogus reference", "/api/requests/not-a-reference!", authz.CodeRequestInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := w.do(w.memberTok, c.path)
			w.wantCode(res, 400, c.code)
			if msg, _ := res.body["error"].(string); strings.TrimSpace(msg) == "" {
				t.Fatalf("%s refusal has no message", c.name)
			}
		})
	}

	// A bad team id in the path is a malformed URL, not a request-surface
	// refusal, and it is not a request.* code.
	if res := w.do(w.memberTok, "/api/teams/not-a-number/requests"); res.status != 400 {
		t.Fatalf("bad team id = %d %v, want 400", res.status, res.body)
	}
	if res := w.do(w.memberTok, "/api/teams/0/requests"); res.status != 404 {
		t.Fatalf("team 0 = %d %v, want 404 (no such team)", res.status, res.body)
	}

	// A 404 is not an access event and a read writes nothing at all.
	if got := w.auditRows(w.member.User.ID, "request.read"); got != before {
		t.Fatalf("reads wrote %d request.read audit rows, want none", got-before)
	}
	if got := w.auditRows(w.member.User.ID, "access.denied"); got != 0 {
		t.Fatalf("reads wrote %d access.denied rows, want none", got)
	}
}

// ---- criterion 3: the requester's answer never carries the record ----

// TestRESTOwnRequests_ProjectionLeaksNothing: /api/requests and /api/requests/:ref
// answer the whitelist projection. The stored record's internal fields must not
// appear anywhere in the raw response — a leak through a nested value is the
// same leak — and one requester can never see another's request, or tell a
// foreign reference from an unknown one.
func TestRESTOwnRequests_ProjectionLeaksNothing(t *testing.T) {
	w := newReadsWorld(t)

	list := w.do(w.requesterTok, "/api/requests")
	if list.status != 200 {
		t.Fatalf("own list = %d %v", list.status, list.body)
	}
	items, _ := list.body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("own list holds %d requests, want the requester's 2", len(items))
	}
	if got, _ := list.body["total"].(float64); got != 2 {
		t.Fatalf("total = %v, want 2", list.body["total"])
	}
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		for _, key := range []string{"public_ref", "state", "state_word", "subject"} {
			if _, present := row[key]; !present {
				t.Fatalf("a projection row is missing %q: %v", key, row)
			}
		}
		// The list row is projected WITHOUT the thread and the files (those belong
		// to the detail read, one request at a time), so both are empty here — the
		// projection's type keeps the keys, and the read path does not open a
		// thread per row to fill them.
		for _, key := range []string{"comments", "outputs"} {
			list, _ := row[key].([]any)
			if len(list) != 0 {
				t.Fatalf("a list row carries %d %s: a queue row does not open a thread or a file list", len(list), key)
			}
		}
	}

	// The detail carries the projection's own fields and none of the record's.
	detail := w.do(w.requesterTok, "/api/requests/"+w.mined[0].PublicRef)
	if detail.status != 200 {
		t.Fatalf("own detail = %d %v", detail.status, detail.body)
	}
	proj, ok := detail.body["projection"].(map[string]any)
	if !ok {
		t.Fatalf("own detail has no projection: %v", detail.body)
	}
	for _, key := range []string{"milestones", "comments", "outputs"} {
		if _, present := proj[key]; !present {
			t.Fatalf("the detail projection is missing %q", key)
		}
	}
	for _, key := range internalRequestKeys {
		if strings.Contains(detail.raw, `"`+key+`"`) {
			t.Fatalf("the requester's answer carries the internal field %q: %s", key, detail.raw)
		}
		if strings.Contains(list.raw, `"`+key+`"`) {
			t.Fatalf("the requester's list carries the internal field %q: %s", key, list.raw)
		}
	}
	// The contact snapshot is a stored value, not a key: check the bytes too.
	var contact string
	if err := w.db.QueryRow(w.ctx, `SELECT requester_contact FROM team_requests WHERE id = $1`, w.mined[0].ID).
		Scan(&contact); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail.raw, contact) || strings.Contains(list.raw, contact) {
		t.Fatalf("the requester's own contact snapshot %q leaked into an answer", contact)
	}
	// And the other requester's request, whose subject is unique, is nowhere.
	if strings.Contains(list.raw, w.mined[2].Subject) {
		t.Fatalf("another requester's request appears in the caller's own list")
	}

	// Another requester's request and a reference that was never minted are the
	// same bytes, so a guess confirms nothing.
	foreignToList := w.do(w.requesterTok, "/api/requests/"+w.mined[2].PublicRef)
	w.wantCode(foreignToList, 404, authz.CodeRequestNotFound)
	unknown := w.do(w.requesterTok, "/api/requests/"+"a"+uniq("ghost"))
	w.wantCode(unknown, 404, authz.CodeRequestNotFound)
	if foreignToList.raw != unknown.raw {
		t.Fatalf("a foreign reference and an unknown one answer differently:\n foreign=%s unknown=%s", foreignToList.raw, unknown.raw)
	}
	// By serial too: another requester's id, and an id nobody holds.
	byID := w.do(w.requesterTok, "/api/requests/"+strconv.Itoa(w.mined[2].ID))
	w.wantCode(byID, 404, authz.CodeRequestNotFound)

	// A team member's own list is their own submissions — not the inbox they
	// triage (the queue is the other route), and not somebody else's requests.
	memberOwn := w.do(w.memberTok, "/api/requests")
	if memberOwn.status != 200 {
		t.Fatalf("member own list = %d %v", memberOwn.status, memberOwn.body)
	}
	if got, _ := memberOwn.body["total"].(float64); got != 1 {
		t.Fatalf("a member's own list total = %v, want their 1 submission", memberOwn.body["total"])
	}

	// A submit-only credential reads nothing here either (it is refused before
	// the handler, exactly as on the team routes).
	if scoped := w.do(w.scopedTok, "/api/requests"); scoped.status != 403 {
		t.Fatalf("submit-only own list = %d, want 403", scoped.status)
	}
}

// internalRequestKeys are the stored record's fields a requester must never
// receive. They are the exact complement of domain.RequesterRequestView: every
// one of them is a column of team_requests that the projection has no field for.
var internalRequestKeys = []string{
	"org_id",
	"team_id",
	"requester_user_id",
	"requester_contact",
	"source_external_id",
	"claim_user_id",
	"claimed_at",
	"owner_user_id",
	"duplicate_of",
	"needs_info_since",
	"planned_at",
	"accepted_by",
	"reopened_at",
	"reopened_count",
	"rev",
	"metadata",
	"updated_at",
}

// uniqueInts returns the distinct values of ids, order preserved.
func uniqueInts(ids []int) []int {
	seen := make(map[int]bool, len(ids))
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// intSlice reads a JSON array of numbers.
func intSlice(v any) []int {
	raw, _ := v.([]any)
	out := make([]int, 0, len(raw))
	for _, item := range raw {
		if n, ok := item.(float64); ok {
			out = append(out, int(n))
		}
	}
	return out
}
