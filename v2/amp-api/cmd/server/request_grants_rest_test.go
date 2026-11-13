package main

// Team requester access administration over REST (epic 202, story 549, AMP
// #3975; build slice B2's REST half and B1's invite half; approved contract
// artifact 36 v2 §2 and §5, decision D14).
//
// internal/service's own suites (cmd/server/submit_grant_admin_test.go,
// cmd/server/requester_invite_test.go) already prove the administration
// *services* as boundaries, and cmd/server/token_scope_test.go proves the
// submit-only credential at the REST boundary. So this file asks only what is
// about *these six routes*, one test per acceptance criterion:
//
//	Criterion 1 — manager-only grant/invite operations tested:
//	  TestRESTRequestGrants_ManagerOnlyAdministration (a team lead and an org owner
//	  grant, list and revoke; a plain team member, a plain org member, a sibling
//	  team's lead, a customer and an agent with no role of its own are refused on
//	  every route and write nothing; a manager of another org's inbox gets one 404
//	  for a team it cannot see, byte for byte, so existence never leaks)
//	  TestRESTRequestInvites_RequesterInvitationAndRevocation (a lead invites an
//	  email as a requester: 201, kind=requester, no org or team role and no account
//	  provisioned; the listing shows it to a manager and to nobody else; a
//	  membership-shaped field is refused by name with the rule; an existing account
//	  is refused; revocation removes the pending intent, is team-scoped so a
//	  sibling inbox's lead cannot revoke it, and is a 404 the second time)
//	Criterion 2 — requester provisioning gains no project authority:
//	  TestRESTRequestGrants_RequesterProvisioningGrantsNoProjectAuthority (a granted
//	  requester, and an invited-then-signed-in requester, both reach zero projects,
//	  ProjectLevel is None, RequireProject and GET /api/projects/:id are 404, no
//	  org_members/team_members row appears, and the customer cannot be added to the
//	  team as a member — while the grant still lets them submit to the inbox)
//	Criterion 3 — secret/revocation behavior correct:
//	  TestRESTRequestGrants_RevocationIsImmediate (a revoked grant fails the very
//	  next submission with nothing to invalidate; the listing says revoked; a
//	  re-grant revives it with the new grantor and time)
//	  TestRESTRequestGrants_ScopedCredentialSecretAppearsOnce (the existing mint
//	  route returns the secret exactly once; no listing and no audit row carries
//	  it; a submit-only credential is refused on all six administration routes)
//
// Everything goes through the real mux (api.RestHandler.Register, so deniedMux,
// the submit-scope gate and the denied() audit sit in front of every handler), a
// real auth.NewAPITokenVerifier and a scratch database per test.
//
// # Why this suite has its own world
//
// `newReadsWorld` spends the package-shared submission limiter in its
// constructor, which makes unrelated `cmd/server` suites answer 429 in the same
// run (#3974's gotcha 4). This file builds its own tenant and its own Service
// with its own limiter, so it neither borrows from nor adds to that budget.
//
// Run against a disposable Postgres (never a live stack):
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRESTRequestGrants|TestRESTRequestInvites' -count=1 -race -v

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// grantsWorld is one tenant: an inbox team, a sibling team in the same org, a
// second org with its own inbox, a project the inbox owns, and the people every
// route can be called by.
type grantsWorld struct {
	t    *testing.T
	ctx  context.Context
	repo *repository.Repo
	db   *pgx.Conn
	svc  *service.Service
	srv  *httptest.Server

	org, team, otherTeam int
	otherOrg, otherInbox int

	// admin owns the inbox's org (a manager); lead leads the inbox; member is a
	// plain member of the inbox; member2 is a plain org member with no team role;
	// lead2 leads the sibling team; customer is the usual grant recipient; agent is
	// an agent kind account with no role of its own, managed by lead; stranger owns
	// the other org.
	admin, lead, member, member2, lead2, customer, agent, stranger int

	// project is linked to the inbox team: it is what "project authority" is
	// measured against, and a requester must reach none of it.
	project int
	sfx     string

	tokens map[int]string
}

func newGrantsWorld(t *testing.T) *grantsWorld {
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

	w := &grantsWorld{t: t, ctx: ctx, repo: repo, db: db, tokens: map[int]string{}, sfx: uniq("grantsrest")}
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
	// The first account provisioned in a scratch database is a global admin, and a
	// global admin reaches every inbox by design (RequireTeamManager), so a
	// throwaway bootstrap account takes that slot and every person tested with is
	// pinned to a non-admin member role.
	if _, err := repo.UpsertUserFromClaims(ctx, w.sfx+"-bootstrap",
		"bootstrap-"+w.sfx+"@grants-rest.test", "bootstrap", nil); err != nil {
		t.Fatalf("bootstrap user: %v", err)
	}
	user := func(label, kind string) int {
		u, err := repo.UpsertUserFromClaims(ctx, w.sfx+"-"+label,
			fmt.Sprintf("%s-%s@grants-rest.test", label, w.sfx), label, nil)
		if err != nil {
			t.Fatalf("user %s: %v", label, err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleMember, true); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleAdmin, false); err != nil {
			t.Fatalf("pin %s as a non-admin: %v", label, err)
		}
		if kind != domain.UserKindHuman {
			exec(`UPDATE users SET kind = $2 WHERE id = $1`, u.ID, kind)
		}
		return u.ID
	}

	w.org = q(`INSERT INTO orgs (name, slug) VALUES ('Grants org', $1) RETURNING id`, w.sfx)
	w.team = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Grants inbox') RETURNING id`, w.org)
	w.otherTeam = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Grants sibling inbox') RETURNING id`, w.org)
	w.otherOrg = q(`INSERT INTO orgs (name, slug) VALUES ('Grants other org', $1) RETURNING id`, w.sfx+"-o")
	w.otherInbox = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign inbox') RETURNING id`, w.otherOrg)

	w.admin = user("admin", domain.UserKindHuman)
	w.lead = user("lead", domain.UserKindHuman)
	w.member = user("member", domain.UserKindHuman)
	w.member2 = user("member2", domain.UserKindHuman)
	w.lead2 = user("lead2", domain.UserKindHuman)
	w.customer = user("customer", domain.UserKindCustomer)
	w.stranger = user("stranger", domain.UserKindHuman)
	w.agent = user("agent", domain.UserKindAgent)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.org, w.admin)
	for _, u := range []int{w.lead, w.member, w.member2, w.lead2} {
		exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.org, u)
	}
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.otherOrg, w.customer)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.otherOrg, w.stranger)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, w.team, w.lead)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, w.team, w.member)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, w.otherTeam, w.lead2)
	exec(`UPDATE users SET managed_by = $2 WHERE id = $1`, w.agent, w.lead)

	proj, err := repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: "Grants delivered work", Code: projectCode("GW", w.sfx), OrgID: &w.org, TeamID: &w.team,
	})
	if err != nil {
		t.Fatalf("create the project: %v", err)
	}
	w.project = proj.ID

	// The handler's own service behind the real REST mux and token verifier, with
	// its own submission budget so this suite cannot spend another's.
	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)
	reg := actor.NewRegistry(system, repo, hub.New())
	mux := http.NewServeMux()
	h := api.NewRestHandler(reg, repo, hub.New(), nil)
	w.svc = h.Service()
	w.svc.RequestLimits = service.NewRequestLimiter(service.RequestLimitConfig{Window: time.Minute, Max: 10000})
	h.Register(mux)
	w.srv = httptest.NewServer(auth.NewAPITokenVerifier(repo).Middleware(mux))
	t.Cleanup(w.srv.Close)
	return w
}

// ---- people, tokens and requests ----

// principalOf builds the principal a signed-in person's request carries.
func (w *grantsWorld) principalOf(userID int) authz.Principal {
	w.t.Helper()
	return authz.Principal{UserID: userID, Email: w.emailOf(userID), Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
}

// emailOf returns a user's email.
func (w *grantsWorld) emailOf(userID int) string {
	w.t.Helper()
	var email string
	if err := w.db.QueryRow(w.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		w.t.Fatalf("email of %d: %v", userID, err)
	}
	return email
}

// token mints (once) a full-scope API token for one person and returns it.
func (w *grantsWorld) token(userID int) string {
	w.t.Helper()
	if tok, ok := w.tokens[userID]; ok {
		return tok
	}
	tok, err := w.svc.CreateToken(w.ctx, w.principalOf(userID), userID, "grants-"+uniq("t"), 0)
	if err != nil {
		w.t.Fatalf("mint a token for %d: %v", userID, err)
	}
	w.tokens[userID] = tok.Token
	return tok.Token
}

// send issues one request as a person's own token.
func (w *grantsWorld) send(userID int, method, path string, body any) writeResult {
	w.t.Helper()
	return w.sendAs(w.token(userID), method, path, body)
}

// sendAs issues one request as an explicit bearer token ("" for none).
func (w *grantsWorld) sendAs(token, method, path string, body any) writeResult {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, w.srv.URL+path, reader)
	if err != nil {
		w.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := writeResult{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// submitAs submits one request through the real submission service, as userID,
// and returns the error (nil on success) — the "does this person still have
// submit access" question, asked the way a submission asks it.
func (w *grantsWorld) submitAs(userID, teamID int, subject string) error {
	w.t.Helper()
	_, err := w.svc.SubmitRequest(w.ctx, w.principalOf(userID), service.SubmitRequestInput{
		TeamID: teamID, Source: domain.RequestSourceWeb, Subject: subject, InputBrief: "The detail.",
	})
	return err
}

// ---- stored facts ----

// grantOf returns the stored (team, user) grant row, or nil.
func (w *grantsWorld) grantOf(teamID, userID int) *domain.TeamSubmitGrant {
	w.t.Helper()
	g, err := w.repo.GetTeamSubmitGrant(w.ctx, teamID, userID)
	if err != nil {
		if strings.Contains(err.Error(), repository.ErrNotFound.Error()) {
			return nil
		}
		w.t.Fatalf("grant (%d,%d): %v", teamID, userID, err)
	}
	return g
}

// maySubmit reports whether the stored grant on (team, user) is in force.
func (w *grantsWorld) maySubmit(teamID, userID int) bool {
	w.t.Helper()
	ok, err := w.repo.HasTeamSubmitGrant(w.ctx, teamID, userID)
	if err != nil {
		w.t.Fatalf("has grant (%d,%d): %v", teamID, userID, err)
	}
	return ok
}

// projectCount counts the projects userID can reach, through the query the
// authorization layer itself uses.
func (w *grantsWorld) projectCount(userID int) int {
	w.t.Helper()
	ids, err := w.repo.AccessibleProjectIDs(w.ctx, userID)
	if err != nil {
		w.t.Fatalf("accessible projects of %d: %v", userID, err)
	}
	return len(ids)
}

// memberships returns userID's org roles and team roles.
func (w *grantsWorld) memberships(userID int) (orgRoles, teamRoles []string) {
	w.t.Helper()
	rows, err := w.db.Query(w.ctx, `SELECT role FROM org_members WHERE user_id = $1 ORDER BY role`, userID)
	if err != nil {
		w.t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			w.t.Fatal(err)
		}
		orgRoles = append(orgRoles, r)
	}
	rows.Close()
	rows, err = w.db.Query(w.ctx, `SELECT role FROM team_members WHERE user_id = $1 ORDER BY role`, userID)
	if err != nil {
		w.t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			w.t.Fatal(err)
		}
		teamRoles = append(teamRoles, r)
	}
	rows.Close()
	return orgRoles, teamRoles
}

// userCount returns how many accounts the users table holds — how "no signup, no
// provisioning" is measured.
func (w *grantsWorld) userCount() int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		w.t.Fatalf("count users: %v", err)
	}
	return n
}

// grantRowCount counts this team's grant rows, revoked ones included.
func (w *grantsWorld) grantRowCount(teamID int) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx, `SELECT count(*) FROM team_submit_grants WHERE team_id = $1`, teamID).Scan(&n); err != nil {
		w.t.Fatalf("count grants: %v", err)
	}
	return n
}

// inviteRowCount counts a team's invites, accepted ones included.
func (w *grantsWorld) inviteRowCount(teamID int) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx, `SELECT count(*) FROM invites WHERE team_id = $1`, teamID).Scan(&n); err != nil {
		w.t.Fatalf("count invites: %v", err)
	}
	return n
}

// pendingRequesterInvites returns a team's pending requester invitations.
func (w *grantsWorld) pendingRequesterInvites(teamID int) []domain.Invite {
	w.t.Helper()
	invites, err := w.repo.ListPendingRequesterInvites(w.ctx, teamID)
	if err != nil {
		w.t.Fatalf("list requester invites of %d: %v", teamID, err)
	}
	return invites
}

// auditText returns every audit row these routes write, flattened to one string
// per row (details + before/after + target + summary), so a leaked secret or an
// unexpected row is visible.
func (w *grantsWorld) auditText() []string {
	w.t.Helper()
	rows, err := w.db.Query(w.ctx, `
		SELECT action || '|' || coalesce(details::text, '') || coalesce("before"::text, '') ||
		       coalesce("after"::text, '') || coalesce(target_name, '') || coalesce(summary, '')
		FROM audit_log ORDER BY id`)
	if err != nil {
		w.t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			w.t.Fatalf("scan audit: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		w.t.Fatalf("audit rows: %v", err)
	}
	return out
}

// auditActions returns the actions of the audit rows that name an action.
func (w *grantsWorld) auditActions() []string {
	w.t.Helper()
	var out []string
	for _, row := range w.auditText() {
		if action, _, ok := strings.Cut(row, "|"); ok {
			out = append(out, action)
		}
	}
	return out
}

// ---- assertions ----

// grantRoutes are the six administration routes, as a caller who may not
// administer should meet them.
func (w *grantsWorld) adminRoutes() []struct{ method, path string } {
	user := url.PathEscape(w.emailOf(w.customer))
	invite := strconv.Itoa(4242)
	return []struct{ method, path string }{
		{"GET", fmt.Sprintf("/api/teams/%d/request-grants", w.team)},
		{"POST", fmt.Sprintf("/api/teams/%d/request-grants", w.team)},
		{"DELETE", fmt.Sprintf("/api/teams/%d/request-grants/%s", w.team, user)},
		{"GET", fmt.Sprintf("/api/teams/%d/request-invites", w.team)},
		{"POST", fmt.Sprintf("/api/teams/%d/request-invites", w.team)},
		{"DELETE", fmt.Sprintf("/api/teams/%d/request-invites/%s", w.team, invite)},
	}
}

// grantBody is the body a POST to the grant route carries.
func (w *grantsWorld) grantBody(userID int) map[string]any {
	return map[string]any{"user": w.emailOf(userID)}
}

// inviteBody is the body a POST to the invitation route carries.
func (w *grantsWorld) inviteBody(email string) map[string]any {
	return map[string]any{"email": email}
}

// codeOf is the stable error code in a response body ("" when there is none).
func codeOf(res writeResult) string {
	s, _ := res.body["code"].(string)
	return s
}

// errorOf is the message in a response body.
func errorOf(res writeResult) string {
	s, _ := res.body["error"].(string)
	return s
}

// blankIDs replaces every run of digits in s with "#", so two refusals that
// differ only in the id the caller asked for compare equal. The refusal text
// is allowed to name that id — it is the caller's own input — but nothing else
// about the target may vary.
func blankIDs(s string) string {
	var b strings.Builder
	inDigits := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			if !inDigits {
				b.WriteByte('#')
				inDigits = true
			}
			continue
		}
		inDigits = false
		b.WriteRune(r)
	}
	return b.String()
}

// grantAnswer reads a grant response's grant object.
func grantAnswer(t *testing.T, res writeResult) map[string]any {
	t.Helper()
	g, ok := res.body["grant"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no grant object: %d %s", res.status, res.raw)
	}
	return g
}

// ---- Criterion 1: manager-only administration ----

// TestRESTRequestGrants_ManagerOnlyAdministration: the D14 gate holds on every
// administration route, and a refused call leaves no grant row, no invitation
// and no audit row of its own behind.
func TestRESTRequestGrants_ManagerOnlyAdministration(t *testing.T) {
	w := newGrantsWorld(t)
	grantPath := fmt.Sprintf("/api/teams/%d/request-grants", w.team)

	t.Run("a team lead grants, lists and revokes", func(t *testing.T) {
		res := w.send(w.lead, http.MethodPost, grantPath, w.grantBody(w.customer))
		if res.status != http.StatusOK {
			t.Fatalf("team lead grant = %d %s", res.status, res.raw)
		}
		g := grantAnswer(t, res)
		if intField(t, g, "team_id") != w.team || intField(t, g, "org_id") != w.org || intField(t, g, "user_id") != w.customer {
			t.Fatalf("grant = %v, want team %d, org %d, user %d", g, w.team, w.org, w.customer)
		}
		if got := intField(t, g, "granted_by"); got != w.lead {
			t.Fatalf("granted_by = %d, want the team lead %d", got, w.lead)
		}
		if res.body["active"] != true {
			t.Fatalf("active = %v, want true", res.body["active"])
		}
		if !w.maySubmit(w.team, w.customer) {
			t.Fatal("the granted customer has no submit access")
		}

		// The listing shows it to a manager, in force first.
		res = w.send(w.lead, http.MethodGet, grantPath, nil)
		if res.status != http.StatusOK {
			t.Fatalf("list = %d %s", res.status, res.raw)
		}
		grants, _ := res.body["grants"].([]any)
		if len(grants) != 1 {
			t.Fatalf("%d grants listed, want 1: %s", len(grants), res.raw)
		}
		listed, _ := grants[0].(map[string]any)
		if listed["email"] != w.emailOf(w.customer) || listed["kind"] != domain.UserKindCustomer {
			t.Fatalf("listed grant = %v, want the customer's email and kind", listed)
		}
		// The listing carries no credential and no role: a grant is not a secret.
		for _, k := range []string{"token", "secret", "role", "scope"} {
			if _, found := listed[k]; found {
				t.Fatalf("the grant listing carries %q: %v", k, listed)
			}
		}

		// The org owner is a manager too, and can revoke what the lead granted.
		res = w.send(w.admin, http.MethodDelete, grantPath+"/"+url.PathEscape(w.emailOf(w.customer)), nil)
		if res.status != http.StatusOK {
			t.Fatalf("org owner revoke = %d %s", res.status, res.raw)
		}
		if res.body["active"] != false {
			t.Fatalf("active after revoke = %v, want false", res.body["active"])
		}
		if w.maySubmit(w.team, w.customer) {
			t.Fatal("the revoked customer still has submit access")
		}
	})

	t.Run("non-managers are refused on every route and write nothing", func(t *testing.T) {
		w := newGrantsWorld(t)
		refused := []struct {
			name string
			user int
			want int
		}{
			// A plain team member and a plain org member can see the team but
			// may not administer it: 403 with the reason.
			{"a plain team member", w.member, http.StatusForbidden},
			{"a plain org member", w.member2, http.StatusForbidden},
			{"a sibling team's lead", w.lead2, http.StatusForbidden},
			// A customer is in another org, and an agent holds no role of its
			// own: neither has a relationship with this inbox, so it is not
			// found — a cross-tenant caller learns nothing.
			{"a customer", w.customer, http.StatusNotFound},
			{"an agent with no role of its own", w.agent, http.StatusNotFound},
			{"another org's owner", w.stranger, http.StatusNotFound},
		}
		// Mint every caller's token first: a mint writes its own token.create
		// row, which is not a refusal and would otherwise land in the window.
		for _, c := range refused {
			w.token(c.user)
		}
		auditBefore := len(w.auditText())
		for _, c := range refused {
			for _, rt := range w.adminRoutes() {
				var body any
				switch rt.method {
				case http.MethodPost:
					if strings.HasSuffix(rt.path, "request-grants") {
						body = w.grantBody(w.customer)
					} else {
						body = w.inviteBody("refused-" + uniq("r") + "@grants-rest.test")
					}
				}
				res := w.send(c.user, rt.method, rt.path, body)
				if res.status != c.want {
					t.Fatalf("%s %s %s = %d %s, want %d", c.name, rt.method, rt.path, res.status, res.raw, c.want)
				}
				if res.status == http.StatusForbidden && codeOf(res) != "" {
					// A 403 from these routes is the service's, not a coded
					// request.* refusal: granting a team is team administration.
					t.Fatalf("%s %s carried code %q, want none", c.name, rt.path, codeOf(res))
				}
			}
		}
		if n := w.grantRowCount(w.team); n != 0 {
			t.Fatalf("%d grant rows after every refusal, want 0", n)
		}
		if n := w.inviteRowCount(w.team); n != 0 {
			t.Fatalf("%d invitation rows after every refusal, want 0", n)
		}
		// The only rows a refusal writes are access.denied (deniedMux), one per
		// 403 — never a team.submit_grant.* or invite.* row, which would claim
		// something happened.
		for _, action := range w.auditActions()[auditBefore:] {
			if action != "access.denied" {
				t.Fatalf("a refused administration call wrote %q", action)
			}
		}
	})

	t.Run("a wrong method is not found, and unauthenticated is refused", func(t *testing.T) {
		grantPath := fmt.Sprintf("/api/teams/%d/request-grants", w.team)
		invitePath := fmt.Sprintf("/api/teams/%d/request-invites", w.team)
		// Method+path pairs are registered, so a third method falls through to
		// teamSub and answers 404 — the same answer every other request route
		// gives, rather than a 405 that would enumerate the surface.
		for _, rt := range []struct{ method, path string }{
			{"PUT", grantPath},
			{"PATCH", grantPath},
			{"GET", grantPath + "/7"},
			{"PUT", invitePath},
			{"POST", invitePath + "/7"},
		} {
			if res := w.send(w.lead, rt.method, rt.path, nil); res.status != http.StatusNotFound {
				t.Fatalf("%s %s = %d %s, want 404", rt.method, rt.path, res.status, res.raw)
			}
		}
		// No route here is reachable without a credential: the auth middleware
		// answers first, so administration is never anonymous.
		for _, rt := range w.adminRoutes() {
			if res := w.sendAs("", rt.method, rt.path, nil); res.status != http.StatusUnauthorized {
				t.Fatalf("%s %s unauthenticated = %d, want 401", rt.method, rt.path, res.status)
			}
		}
	})

	t.Run("another org's inbox is not found, whatever the caller", func(t *testing.T) {
		w := newGrantsWorld(t)
		// The stranger owns the other org, so it can administer its own inbox...
		if res := w.send(w.stranger, http.MethodGet, fmt.Sprintf("/api/teams/%d/request-grants", w.otherInbox), nil); res.status != http.StatusOK {
			t.Fatalf("the foreign inbox's own owner = %d %s, want 200", res.status, res.raw)
		}
		// ...and the inbox team's own org owner gets the same answer for that
		// inbox as for a team that never existed: existence never leaks.
		real := w.send(w.admin, http.MethodGet, fmt.Sprintf("/api/teams/%d/request-grants", w.otherInbox), nil)
		missing := w.send(w.admin, http.MethodGet, "/api/teams/999999/request-grants", nil)
		if real.status != http.StatusNotFound {
			t.Fatalf("a cross-org inbox = %d %s, want 404", real.status, real.raw)
		}
		// The message names the id the caller asked for and nothing else: with
		// the id blanked out, a team that exists in another org and a team that
		// never existed are the same answer.
		if blankIDs(errorOf(real)) != blankIDs(errorOf(missing)) {
			t.Fatalf("cross-org %q and never-issued %q differ: existence leaks", errorOf(real), errorOf(missing))
		}
	})
}

// TestRESTRequestInvites_RequesterInvitationAndRevocation: a requester
// invitation is a row that provisions a customer identity at first sign-in, it
// is never a membership, and its revocation is team-scoped.
func TestRESTRequestInvites_RequesterInvitationAndRevocation(t *testing.T) {
	w := newGrantsWorld(t)
	invitePath := fmt.Sprintf("/api/teams/%d/request-invites", w.team)
	fresh := "invited-" + w.sfx + "@grants-rest.test"
	usersBefore := w.userCount()

	t.Run("a team lead invites an email as a requester", func(t *testing.T) {
		res := w.send(w.lead, http.MethodPost, invitePath, w.inviteBody(fresh))
		if res.status != http.StatusCreated {
			t.Fatalf("invite = %d %s, want 201", res.status, res.raw)
		}
		inv, ok := res.body["invite"].(map[string]any)
		if !ok {
			t.Fatalf("the answer carries no invite: %s", res.raw)
		}
		if inv["kind"] != domain.InviteKindRequester {
			t.Fatalf("kind = %v, want %q", inv["kind"], domain.InviteKindRequester)
		}
		if inv["org_role"] != domain.OrgRoleMember || inv["team_role"] != domain.TeamRoleMember {
			t.Fatalf("roles = %v/%v, want member/member: an invitation grants no role", inv["org_role"], inv["team_role"])
		}
		if got := intField(t, inv, "team_id"); got != w.team {
			t.Fatalf("invite team = %d, want %d", got, w.team)
		}
		if got := intField(t, inv, "org_id"); got != w.org {
			t.Fatalf("invite org = %d, want %d", got, w.org)
		}
		// No signup: inviting writes a row and provisions nothing.
		if n := w.userCount(); n != usersBefore {
			t.Fatalf("%d accounts after inviting, want %d (an invitation creates no identity)", n, usersBefore)
		}
	})

	t.Run("the listing is manager-only and shows the pending invitation", func(t *testing.T) {
		res := w.send(w.lead, http.MethodGet, invitePath, nil)
		if res.status != http.StatusOK {
			t.Fatalf("list = %d %s", res.status, res.raw)
		}
		invites, _ := res.body["invites"].([]any)
		if len(invites) != 1 {
			t.Fatalf("%d invitations listed, want 1: %s", len(invites), res.raw)
		}
		listed, _ := invites[0].(map[string]any)
		if listed["email"] != fresh || listed["kind"] != domain.InviteKindRequester {
			t.Fatalf("listed invitation = %v", listed)
		}
		// A plain team member may read the inbox but not administer it.
		if res := w.send(w.member, http.MethodGet, invitePath, nil); res.status != http.StatusForbidden {
			t.Fatalf("a plain team member listing invitations = %d, want 403", res.status)
		}
		// The sibling inbox's lead administers a different inbox.
		if res := w.send(w.lead2, http.MethodGet, invitePath, nil); res.status != http.StatusForbidden {
			t.Fatalf("a sibling team's lead listing this inbox's invitations = %d, want 403", res.status)
		}
		if got := w.pendingRequesterInvites(w.otherTeam); len(got) != 0 {
			t.Fatalf("the sibling inbox lists %d of this inbox's invitations", len(got))
		}
	})

	t.Run("a membership-shaped field is refused by name with the rule", func(t *testing.T) {
		for _, field := range []string{"org_role", "team_role", "role", "kind", "team_id", "org_id"} {
			body := map[string]any{"email": "shaped-" + uniq("s") + "@grants-rest.test", field: "lead"}
			res := w.send(w.lead, http.MethodPost, invitePath, body)
			if res.status != http.StatusBadRequest {
				t.Fatalf("%s = %d %s, want 400", field, res.status, res.raw)
			}
			if codeOf(res) != authz.CodeRequestInvalid {
				t.Fatalf("%s code = %q, want %q", field, codeOf(res), authz.CodeRequestInvalid)
			}
			msg := errorOf(res)
			if !strings.Contains(msg, field) || !strings.Contains(msg, "no org or team role") && !strings.Contains(msg, "path's") &&
				!strings.Contains(msg, "kind \"member\"") {
				t.Fatalf("%s refusal = %q, want it to name the field and the rule", field, msg)
			}
			// The decoder's generic "unknown field" is not the answer.
			if strings.Contains(msg, "unknown field") {
				t.Fatalf("%s refusal fell through to the decoder: %q", field, msg)
			}
		}
		// A grant body has the same rule, and a role there is never granted.
		for _, field := range []string{"role", "org_role", "team_role", "scope", "project_id"} {
			res := w.send(w.lead, http.MethodPost, fmt.Sprintf("/api/teams/%d/request-grants", w.team),
				map[string]any{"user": w.emailOf(w.customer), field: "lead"})
			if res.status != http.StatusBadRequest {
				t.Fatalf("grant %s = %d %s, want 400", field, res.status, res.raw)
			}
			if codeOf(res) != authz.CodeRequestInvalid || strings.Contains(errorOf(res), "unknown field") {
				t.Fatalf("grant %s refusal = %d %q", field, res.status, errorOf(res))
			}
		}
		if n := w.grantRowCount(w.team); n != 0 {
			t.Fatalf("%d grant rows after every malformed body, want 0", n)
		}
		if n := w.inviteRowCount(w.team); n != 1 {
			t.Fatalf("%d invitation rows, want 1 (the one real invitation)", n)
		}
	})

	t.Run("an existing account cannot be invited as a requester", func(t *testing.T) {
		// The membership path already behaves this way (#3767); the route must
		// not be a way around it: an existing human keeps its kind.
		res := w.send(w.lead, http.MethodPost, invitePath, w.inviteBody(w.emailOf(w.member)))
		if res.status != http.StatusBadRequest {
			t.Fatalf("inviting an existing account = %d %s, want 400", res.status, res.raw)
		}
		if !strings.Contains(errorOf(res), "already has an AMP account") {
			t.Fatalf("refusal = %q, want it to say the account exists", errorOf(res))
		}
		var kind string
		if err := w.db.QueryRow(w.ctx, `SELECT kind FROM users WHERE id = $1`, w.member).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != domain.UserKindHuman {
			t.Fatalf("the existing account's kind became %q", kind)
		}
	})

	t.Run("revocation is team-scoped and removes the pending intent", func(t *testing.T) {
		invites := w.pendingRequesterInvites(w.team)
		if len(invites) != 1 {
			t.Fatalf("%d pending invitations, want 1", len(invites))
		}
		id := strconv.Itoa(invites[0].ID)

		// A sibling inbox's lead cannot revoke this inbox's invitation by
		// putting this inbox in the path: the row is not that team's.
		if res := w.send(w.lead2, http.MethodDelete, invitePath+"/"+id, nil); res.status != http.StatusForbidden {
			t.Fatalf("a sibling lead revoking this inbox's invitation = %d, want 403", res.status)
		}
		// ...and the sibling's own manager, naming its own team in the path, is
		// not found: the invitation is not that team's. (A manager of team A
		// cannot revoke team B's row by putting A in the path.)
		if res := w.send(w.lead2, http.MethodDelete, fmt.Sprintf("/api/teams/%d/request-invites/%s", w.otherTeam, id), nil); res.status != http.StatusNotFound {
			t.Fatalf("revoking this inbox's invitation through the sibling's path = %d %s, want 404", res.status, res.raw)
		}
		if got := len(w.pendingRequesterInvites(w.team)); got != 1 {
			t.Fatalf("%d invitations left after two refused revocations, want 1", got)
		}

		res := w.send(w.lead, http.MethodDelete, invitePath+"/"+id, nil)
		if res.status != http.StatusOK || res.body["revoked"] != true {
			t.Fatalf("revoke = %d %s", res.status, res.raw)
		}
		if got := len(w.pendingRequesterInvites(w.team)); got != 0 {
			t.Fatalf("%d invitations left after the revocation, want 0", got)
		}
		// A second revocation of the same row is not found: there is nothing
		// pending left to revoke.
		if res := w.send(w.lead, http.MethodDelete, invitePath+"/"+id, nil); res.status != http.StatusNotFound {
			t.Fatalf("second revoke = %d %s, want 404", res.status, res.raw)
		}
	})
}

// ---- Criterion 2: no project authority ----

// TestRESTRequestGrants_RequesterProvisioningGrantsNoProjectAuthority: a person
// who may submit to an inbox reaches none of its projects — the whole reason
// submit access is a grant instead of a team role.
func TestRESTRequestGrants_RequesterProvisioningGrantsNoProjectAuthority(t *testing.T) {
	w := newGrantsWorld(t)

	// A requester granted through the route, and one who accepted a requester
	// invitation and signed in: the two ways a customer arrives.
	if res := w.send(w.lead, http.MethodPost, fmt.Sprintf("/api/teams/%d/request-grants", w.team), w.grantBody(w.customer)); res.status != http.StatusOK {
		t.Fatalf("grant = %d %s", res.status, res.raw)
	}
	invited := "joined-" + w.sfx + "@grants-rest.test"
	if res := w.send(w.lead, http.MethodPost, fmt.Sprintf("/api/teams/%d/request-invites", w.team), w.inviteBody(invited)); res.status != http.StatusCreated {
		t.Fatalf("invite = %d %s", res.status, res.raw)
	}
	// Their first sign-in: the provisioning path, not a REST call. This is what
	// turns the invitation into a customer identity.
	u, pv, err := w.repo.ProvisionUserFromClaims(w.ctx, w.sfx+"-joined", invited, "joined", nil)
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if u.Kind != domain.UserKindCustomer {
		t.Fatalf("an invited requester signed in as %q, want %q", u.Kind, domain.UserKindCustomer)
	}
	if len(pv.Invites) != 1 || pv.Invites[0].Kind != domain.InviteKindRequester {
		t.Fatalf("accepted invites = %+v, want the one requester invitation", pv.Invites)
	}
	// Acceptance consumes the pending intent: the administration listing is
	// pending invitations, not a history, so the accepted row is gone from it.
	if left := w.pendingRequesterInvites(w.team); len(left) != 0 {
		t.Fatalf("%d pending invitations after the person signed in, want 0: %+v", len(left), left)
	}

	for _, c := range []struct {
		name string
		user int
	}{{"a granted requester", w.customer}, {"an invited requester after sign-in", u.ID}} {
		orgRoles, teamRoles := w.memberships(c.user)
		if len(orgRoles) != 0 && c.name == "an invited requester after sign-in" {
			t.Fatalf("%s holds org roles %v, want none from the invitation", c.name, orgRoles)
		}
		if len(teamRoles) != 0 {
			t.Fatalf("%s holds team roles %v, want none (submit access is not membership)", c.name, teamRoles)
		}
		if got := w.projectCount(c.user); got != 0 {
			t.Fatalf("%s reaches %d projects, want 0", c.name, got)
		}
		lvl, err := w.svc.Authz.ProjectLevel(w.ctx, w.principalOf(c.user), w.project)
		if err != nil {
			t.Fatalf("%s ProjectLevel: %v", c.name, err)
		}
		if lvl != authz.None {
			t.Fatalf("%s ProjectLevel = %v, want None", c.name, lvl)
		}
		_, err = w.svc.Authz.RequireProject(w.ctx, w.principalOf(c.user), w.project, authz.Member)
		if err == nil || authz.StatusOf(err, 500) != http.StatusNotFound {
			t.Fatalf("%s RequireProject = %v, want a 404", c.name, err)
		}
		// The same answer through the route the UI reads.
		if res := w.send(c.user, http.MethodGet, fmt.Sprintf("/api/projects/%d", w.project), nil); res.status != http.StatusNotFound {
			t.Fatalf("%s GET /api/projects/%d = %d, want 404", c.name, w.project, res.status)
		}
	}

	// The grant is what lets the granted requester submit; the invitation alone
	// grants nothing.
	if err := w.submitAs(w.customer, w.team, "A granted requester's request"); err != nil {
		t.Fatalf("the granted requester cannot submit: %v", err)
	}
	if err := w.submitAs(u.ID, w.team, "An invited requester's request"); err == nil {
		t.Fatal("an invited requester could submit with no grant: the invitation granted submit access")
	}

	// A customer cannot be made a team member either, so the grant is not a
	// side door into membership: the membership route refuses it by name.
	res := w.send(w.lead, http.MethodPost, fmt.Sprintf("/api/teams/%d/members", w.team),
		map[string]any{"user": w.emailOf(w.customer), "role": "member"})
	if res.status != http.StatusConflict {
		t.Fatalf("adding a customer to the team = %d %s, want 409", res.status, res.raw)
	}
	if !strings.Contains(errorOf(res), "submit access") {
		t.Fatalf("the refusal = %q, want it to point at submit access", errorOf(res))
	}

	// Revoking the grant takes the submit access and nothing else away: the
	// account keeps its kind and still reaches no project.
	if res := w.send(w.lead, http.MethodDelete, fmt.Sprintf("/api/teams/%d/request-grants/%s", w.team, url.PathEscape(w.emailOf(w.customer))), nil); res.status != http.StatusOK {
		t.Fatalf("revoke = %d %s", res.status, res.raw)
	}
	if err := w.submitAs(w.customer, w.team, "After the revocation"); err == nil {
		t.Fatal("the revoked requester can still submit")
	}
	var kind string
	if err := w.db.QueryRow(w.ctx, `SELECT kind FROM users WHERE id = $1`, w.customer).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != domain.UserKindCustomer || w.projectCount(w.customer) != 0 {
		t.Fatalf("revocation changed the account: kind %q, %d projects", kind, w.projectCount(w.customer))
	}
}

// ---- Criterion 3: secret and revocation behaviour ----

// TestRESTRequestGrants_RevocationIsImmediate: revocation is a state on the row,
// re-read on every check, so it takes effect on the very next request with no
// cache to invalidate; a re-grant revives it with the new grantor.
func TestRESTRequestGrants_RevocationIsImmediate(t *testing.T) {
	w := newGrantsWorld(t)
	grantPath := fmt.Sprintf("/api/teams/%d/request-grants", w.team)
	revokePath := grantPath + "/" + url.PathEscape(w.emailOf(w.customer))

	if res := w.send(w.lead, http.MethodPost, grantPath, w.grantBody(w.customer)); res.status != http.StatusOK {
		t.Fatalf("grant = %d %s", res.status, res.raw)
	}
	if err := w.submitAs(w.customer, w.team, "Before the revocation"); err != nil {
		t.Fatalf("the granted customer cannot submit: %v", err)
	}

	first := w.grantOf(w.team, w.customer)
	if first == nil || !first.Active() {
		t.Fatalf("stored grant = %+v, want an active row", first)
	}
	if res := w.send(w.lead, http.MethodDelete, revokePath, nil); res.status != http.StatusOK {
		t.Fatalf("revoke = %d %s", res.status, res.raw)
	}
	// The very next submission, with no reload or cache reset in between.
	if err := w.submitAs(w.customer, w.team, "The request after the revocation"); err == nil {
		t.Fatal("a revoked grant still submitted: revocation is not effective")
	}
	// The row survives, revoked — that is what makes a re-grant a revival.
	revoked := w.grantOf(w.team, w.customer)
	if revoked == nil || revoked.Active() || revoked.RevokedAt == nil || revoked.RevokedBy == nil || *revoked.RevokedBy != w.lead {
		t.Fatalf("stored grant after revoke = %+v, want a revoked row attributed to the leader", revoked)
	}
	// The listing says so, beside the in-force ones.
	res := w.send(w.lead, http.MethodGet, grantPath, nil)
	grants, _ := res.body["grants"].([]any)
	if len(grants) != 1 {
		t.Fatalf("%d grants listed, want 1 (the revoked one is listed on purpose): %s", len(grants), res.raw)
	}
	listed, _ := grants[0].(map[string]any)
	if _, found := listed["revoked_at"]; !found {
		t.Fatalf("the listing does not say the grant is revoked: %v", listed)
	}

	// A repeated revocation is the no-op it is: 200, still revoked, still the
	// first revoker.
	if res := w.send(w.admin, http.MethodDelete, revokePath, nil); res.status != http.StatusOK {
		t.Fatalf("second revoke = %d %s, want 200", res.status, res.raw)
	}
	twice := w.grantOf(w.team, w.customer)
	if twice.RevokedBy == nil || *twice.RevokedBy != w.lead {
		t.Fatalf("the repeated revocation replaced the first revoker: %+v", twice)
	}

	// A re-grant revives the row with the new grantor and a new time: the access
	// in force is the one this call created.
	if res := w.send(w.admin, http.MethodPost, grantPath, w.grantBody(w.customer)); res.status != http.StatusOK {
		t.Fatalf("re-grant = %d %s", res.status, res.raw)
	}
	revived := w.grantOf(w.team, w.customer)
	if !revived.Active() {
		t.Fatalf("the re-grant did not revive the row: %+v", revived)
	}
	if revived.GrantedBy == nil || *revived.GrantedBy != w.admin {
		t.Fatalf("revived grantor = %v, want the org owner %d", revived.GrantedBy, w.admin)
	}
	if !revived.GrantedAt.After(first.GrantedAt) {
		t.Fatalf("revived granted_at %v is not after the original %v", revived.GrantedAt, first.GrantedAt)
	}
	if err := w.submitAs(w.customer, w.team, "After the re-grant"); err != nil {
		t.Fatalf("the re-granted customer cannot submit: %v", err)
	}

	// Revoking a pair with no grant row is not found, and writes no row.
	if res := w.send(w.lead, http.MethodDelete, grantPath+"/"+url.PathEscape(w.emailOf(w.member2)), nil); res.status != http.StatusNotFound {
		t.Fatalf("revoking a non-existent grant = %d %s, want 404", res.status, res.raw)
	}
}

// TestRESTRequestGrants_ScopedCredentialSecretAppearsOnce: the submit-only
// credential is minted by the existing reviewed route, its secret appears in
// that answer and nowhere else, and it can never administer the inbox it may
// submit to.
func TestRESTRequestGrants_ScopedCredentialSecretAppearsOnce(t *testing.T) {
	w := newGrantsWorld(t)
	tokensPath := fmt.Sprintf("/api/users/%d/tokens", w.lead)

	res := w.send(w.lead, http.MethodPost, tokensPath, map[string]any{
		"name": "inbox connector", "scope": "submit", "scope_team_id": w.team,
	})
	if res.status != http.StatusOK {
		t.Fatalf("mint a submit-only token = %d %s", res.status, res.raw)
	}
	tok, ok := res.body["token"].(map[string]any)
	if !ok {
		t.Fatalf("the mint answered no token object: %s", res.raw)
	}
	secret, _ := tok["token"].(string)
	if !strings.HasPrefix(secret, "amp_") {
		t.Fatalf("the mint's secret = %q, want an amp_ token", secret)
	}
	if tok["scope"] != domain.TokenScopeSubmit {
		t.Fatalf("scope = %v, want %q", tok["scope"], domain.TokenScopeSubmit)
	}
	if got := intField(t, tok, "scope_team_id"); got != w.team {
		t.Fatalf("scope_team_id = %d, want %d", got, w.team)
	}

	// The listing never carries it: a token is shown once, and a management
	// screen reads the prefix only.
	res = w.send(w.lead, http.MethodGet, tokensPath, nil)
	tokens, _ := res.body["tokens"].([]any)
	if len(tokens) == 0 {
		t.Fatalf("the listing is empty: %s", res.raw)
	}
	var scopedListed map[string]any
	for _, entry := range tokens {
		tk, _ := entry.(map[string]any)
		if _, found := tk["token"]; found {
			t.Fatalf("the token listing carries a secret: %v", tk)
		}
		if tk["scope"] == domain.TokenScopeSubmit {
			scopedListed = tk
		}
	}
	if scopedListed == nil {
		t.Fatalf("the submit-only token is not listed: %s", res.raw)
	}
	if rawListing, err := json.Marshal(res.body); err != nil || strings.Contains(string(rawListing), secret) {
		t.Fatal("the token listing carries the minted secret")
	}

	// No audit row carries it either — not the mint's, not an administration
	// row's. The mint names the token by id and prefix, never by value.
	for _, row := range w.auditText() {
		if strings.Contains(row, secret) {
			t.Fatalf("an audit row carries the secret: %s", row)
		}
		if strings.Contains(row, auth.HashAPIToken(secret)) {
			t.Fatalf("an audit row carries the token's hash: %s", row)
		}
	}

	// A submit-only credential may submit to its bound inbox and nothing else:
	// every administration route on this surface refuses it with the one scope
	// code, before any handler runs.
	for _, rt := range w.adminRoutes() {
		var body any
		if rt.method == http.MethodPost {
			body = map[string]any{}
		}
		res := w.sendAs(secret, rt.method, rt.path, body)
		if res.status != http.StatusForbidden {
			t.Fatalf("%s %s as a submit-only credential = %d %s, want 403", rt.method, rt.path, res.status, res.raw)
		}
		if codeOf(res) != authz.CodeTokenScopeDenied {
			t.Fatalf("%s %s code = %q, want %q", rt.method, rt.path, codeOf(res), authz.CodeTokenScopeDenied)
		}
	}

	// The grant it was minted from is untouched by any of that: no row appeared.
	if n := w.grantRowCount(w.team); n != 0 {
		t.Fatalf("%d grant rows after the scoped refusals, want 0", n)
	}
	if n := w.inviteRowCount(w.team); n != 0 {
		t.Fatalf("%d invitation rows after the scoped refusals, want 0", n)
	}
}
