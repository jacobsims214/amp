package main

// #124: with auth off, the local-dev identity is a real AMP user (an admin of
// the Default org), so My work, approvals, People & agents and tokens work
// for it. With auth on it is never created.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// localDevMembership is the local-dev user's Default org role and Everyone
// team role ("" when not a member).
func localDevMembership(t *testing.T, s *mcpStack, userID int) (orgRole, teamRole string) {
	t.Helper()
	ctx := context.Background()
	orgs, err := s.repo.ListOrgs(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orgs {
		if o.Slug != "default" {
			continue
		}
		if orgRole, err = s.repo.GetOrgRole(ctx, o.ID, userID); err != nil {
			t.Fatal(err)
		}
		teams, err := s.repo.ListUserTeams(ctx, o.ID, userID)
		if err != nil {
			t.Fatal(err)
		}
		for _, tm := range teams { // team name in Email, as ListUserTeams packs it
			if tm.Email == "Everyone" {
				teamRole = tm.Role
			}
		}
	}
	return orgRole, teamRole
}

func TestLocalDev_ProvisionedAsAdminUser(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	v, err := auth.NewVerifier(ctx, auth.Config{Repo: s.repo}) // no OIDC_ISSUER_URL: auth off
	if err != nil {
		t.Fatal(err)
	}
	if v.Enabled() {
		t.Fatal("verifier with no issuer has auth on")
	}
	u, err := v.ProvisionLocalDev(ctx)
	if err != nil || u == nil {
		t.Fatalf("provision: %v %v", u, err)
	}
	// Idempotent: a second boot is the same user, still an admin.
	again, err := v.ProvisionLocalDev(ctx)
	if err != nil || again.ID != u.ID {
		t.Fatalf("second provision: %v %v (first id %d)", again, err, u.ID)
	}
	if u.Subject != auth.LocalDevSubject || u.Email != auth.LocalDevEmail || u.Kind != domain.UserKindHuman || !slices.Contains(again.Roles, domain.RoleAdmin) {
		t.Fatalf("local dev user = %+v", again)
	}
	if org, team := localDevMembership(t, s, u.ID); org != domain.OrgRoleAdmin && org != domain.OrgRoleOwner || team != "lead" {
		t.Fatalf("Default org role %q, Everyone team role %q", org, team)
	}

	// The REST listener as main() wires it: every request is the local-dev
	// identity, now with its AMP user.
	mux := http.NewServeMux()
	api.NewRestHandler(s.reg, s.repo, s.hub, s.kb).Register(mux)
	var seen authz.Principal
	srv := httptest.NewServer(v.WithAudiences("rest").Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, err := authz.FromContext(r.Context()); err == nil {
			seen = p
		}
		mux.ServeHTTP(w, r)
	})))
	t.Cleanup(srv.Close)
	do := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	want := func(code int, method, path string, body any) map[string]any {
		t.Helper()
		got, out := do(method, path, body)
		if got != code {
			t.Fatalf("%s %s: %d %v, want %d", method, path, got, out, code)
		}
		return out
	}

	me := want(200, "GET", "/api/me", nil)
	if me["user_id"] != float64(u.ID) || me["local_dev"] != true || fmt.Sprint(me["roles"]) != "[admin]" {
		t.Fatalf("/api/me = %v", me)
	}
	if !seen.HasUser() || seen.UserID != u.ID || !seen.Super || !seen.LocalDev() || !seen.InUI() {
		t.Fatalf("principal = %+v", seen)
	}

	// People & agents: the admin-only user list, with the local-dev user in it.
	users := want(200, "GET", "/api/users", nil)
	if !strings.Contains(fmt.Sprint(users["users"]), auth.LocalDevEmail) {
		t.Fatalf("/api/users without the local dev user: %v", users)
	}
	// Agents & tokens: its own tokens, and an agent account it manages.
	want(200, "GET", "/api/users/me/tokens", nil)
	tok := want(200, "POST", "/api/users/me/tokens", map[string]any{"name": "local"})
	secret, _ := tok["token"].(map[string]any)["token"].(string)
	if !strings.HasPrefix(secret, "amp_") {
		t.Fatalf("new token: %v", tok)
	}
	// Turned on later, auth refuses that token: it would act as an admin
	// nobody signs in as.
	on, err := auth.NewVerifier(ctx, auth.Config{IssuerURL: "https://issuer.invalid/dex", Repo: s.repo})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	on.WithAudiences("rest").Middleware(mux).ServeHTTP(rec, req)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "belongs to the Local Dev user") {
		t.Fatalf("local dev token with auth on: %d %s", rec.Code, rec.Body)
	}
	if code, out := do("POST", "/api/agents", map[string]any{"name": fmt.Sprintf("local-agent-%d", s.projectID)}); code != 200 && code != 201 {
		t.Fatalf("create agent account: %d %v", code, out)
	}

	// The core loop: what an agent (over MCP, the same identity) plans waits
	// on the person in My work, and Approve works.
	ident := &auth.Identity{Subject: auth.LocalDevSubject, Email: auth.LocalDevEmail, Name: auth.LocalDevName,
		LocalDev: true, User: u, AuthMethod: auth.AuthLocalDev}
	agent := s.as(ident)
	approvalOn(t, agent)
	task := mkIn(t, agent, s.storyID, "needs approval")

	mw := want(200, "GET", fmt.Sprintf("/api/projects/%d/my-work", s.projectID), nil)
	if j := mustJSON(mw); !strings.Contains(j, `"kind":"approve"`) || !strings.Contains(j, fmt.Sprintf(`"task_ids":[%d]`, task)) {
		t.Fatalf("project My work has no approval for ticket %d: %s", task, mustJSON(mw))
	}
	want(200, "GET", "/api/my-work", nil)
	want(200, "POST", fmt.Sprintf("/api/projects/%d/approve-plan", s.projectID), nil)
	got, err := s.repo.GetTask(ctx, task)
	if err != nil || !got.Approved() {
		t.Fatalf("ticket %d not approved: %+v %v", task, got, err)
	}
}

// On a fresh database: with auth on, startup never creates the Local Dev
// user; and on one that ran with auth off first, the first person to sign in
// is still the first user, a global admin (Local Dev and its agents don't
// count, #124).
func TestLocalDev_FreshDatabase(t *testing.T) {
	ctx := context.Background()
	repo, err := repository.New(ctx, freshDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	on, err := auth.NewVerifier(ctx, auth.Config{IssuerURL: "https://issuer.invalid/dex", Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if u, err := on.ProvisionLocalDev(ctx); err != nil || u != nil {
		t.Fatalf("provision with auth on: %v %v", u, err)
	}
	if _, err := repo.GetUserBySubject(ctx, auth.LocalDevSubject); err == nil {
		t.Fatal("auth on created the Local Dev user")
	}

	// Now an auth-off boot, an agent it makes, then auth on and a first sign-in.
	off, err := auth.NewVerifier(ctx, auth.Config{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	ld, err := off.ProvisionLocalDev(ctx)
	if err != nil || ld == nil {
		t.Fatalf("auth-off provision: %v %v", ld, err)
	}
	if _, err := repo.CreateAgentUser(ctx, "agent:ld-bot", "ld-bot@agents.amp", "ld bot", &ld.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := on.ProvisionLocalDev(ctx); err != nil {
		t.Fatal(err)
	}
	first, _, err := repo.ProvisionUserFromClaims(ctx, "dex|first", "first@example.test", "First", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(first.Roles, domain.RoleAdmin) {
		t.Fatalf("first person after an auth-off boot got %v, want admin", first.Roles)
	}
	second, _, err := repo.ProvisionUserFromClaims(ctx, "dex|second", "second@example.test", "Second", nil)
	if err != nil || slices.Contains(second.Roles, domain.RoleAdmin) {
		t.Fatalf("second person: %v %v, want member", second.Roles, err)
	}
}

// A database that ran with auth off keeps its Local Dev user, a global admin.
// An auth-on boot deactivates it (idempotently), so none of these can act as
// it: a token it minted, an OIDC sign-in with its email (and no
// email_verified claim, as Dex's password DB sends), or one with its subject.
// Agent accounts it made stay as they are (the operator made them).
func TestLocalDev_AuthOnDeactivatesIt(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	off, err := auth.NewVerifier(ctx, auth.Config{Repo: s.repo})
	if err != nil {
		t.Fatal(err)
	}
	u, err := off.ProvisionLocalDev(ctx)
	if err != nil || u == nil || u.DisabledAt != nil {
		t.Fatalf("auth-off provision: %+v %v", u, err)
	}
	p := authz.Principal{UserID: u.ID, Email: u.Email, Super: true, AuthMethod: auth.AuthLocalDev, Channel: auth.ChannelUI}
	svc := service.New(s.repo, s.reg, s.kb)
	tok, err := svc.CreateToken(ctx, p, u.ID, "made with auth off", 0)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := svc.CreateAgent(ctx, p, fmt.Sprintf("ld-agent-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}

	// The auth-on boot.
	idp := newTestIdP(t)
	on, err := auth.NewVerifier(ctx, auth.Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, Repo: s.repo})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if got, err := on.ProvisionLocalDev(ctx); err != nil || got != nil {
			t.Fatalf("auth-on boot %d: %v %v", i, got, err)
		}
	}
	row, err := s.repo.GetUserBySubject(ctx, auth.LocalDevSubject)
	if err != nil || row.ID != u.ID || row.DisabledAt == nil {
		t.Fatalf("after an auth-on boot the Local Dev user is %+v (%v), want it deactivated", row, err)
	}
	if a, err := s.repo.GetUserByID(ctx, agent.ID); err != nil || a.DisabledAt != nil {
		t.Fatalf("agent account made by Local Dev: %+v %v, want it left active", a, err)
	}

	mux := http.NewServeMux()
	api.NewRestHandler(s.reg, s.repo, s.hub, s.kb).Register(mux)
	srv := on.WithAudiences("rest").Middleware(mux)
	get := func(bearer, path string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		srv.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// Its token: refused, and says why.
	if code, body := get(tok.Token, "/api/me"); code != 401 || !strings.Contains(body, "Local Dev") {
		t.Fatalf("Local Dev token with auth on: %d %s", code, body)
	}
	// An OIDC sign-in with its email and no email_verified claim, or with
	// its subject: never the admin row.
	for _, claims := range []map[string]any{
		{"sub": fmt.Sprintf("someone-%d", time.Now().UnixNano()), "email": auth.LocalDevEmail, "name": "Someone"},
		{"sub": fmt.Sprintf("someone-%d", time.Now().UnixNano()), "email": "LOCAL-DEV@amp.local"},
		{"sub": auth.LocalDevSubject, "email": fmt.Sprintf("other-%d@example.test", time.Now().UnixNano())},
	} {
		jwt := idp.token(t, claims)
		code, body := get(jwt, "/api/me")
		var me map[string]any
		_ = json.Unmarshal([]byte(body), &me)
		if code == 200 && (me["user_id"] != nil || strings.Contains(fmt.Sprint(me["roles"]), domain.RoleAdmin)) {
			t.Fatalf("sign-in %v resolved to an AMP user: %s", claims, body)
		}
		if code, body := get(jwt, "/api/users"); code != 403 {
			t.Fatalf("sign-in %v listed users: %d %s", claims, code, body)
		}
	}
	row, err = s.repo.GetUserBySubject(ctx, auth.LocalDevSubject)
	if err != nil || row.ID != u.ID || row.Email != auth.LocalDevEmail || row.DisabledAt == nil || !slices.Contains(row.Roles, domain.RoleAdmin) {
		t.Fatalf("the Local Dev row changed: %+v %v", row, err)
	}

	// Back to auth off: it is the Local Dev admin again.
	if again, err := off.ProvisionLocalDev(ctx); err != nil || again.ID != u.ID || again.DisabledAt != nil {
		t.Fatalf("auth-off provision after auth on: %+v %v", again, err)
	}
}

// Nobody deletes their own account: for Local Dev it would leave every
// request on a deleted user until restart.
func TestLocalDev_CannotDeleteItself(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	v, err := auth.NewVerifier(ctx, auth.Config{Repo: s.repo})
	if err != nil {
		t.Fatal(err)
	}
	u, err := v.ProvisionLocalDev(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := authz.Principal{UserID: u.ID, Email: u.Email, Super: true, AuthMethod: auth.AuthLocalDev, Channel: auth.ChannelUI}
	if err := service.New(s.repo, s.reg, s.kb).DeleteUser(ctx, p, u.ID); authz.StatusOf(err, 0) != 400 {
		t.Fatalf("self-delete: %v", err)
	}
	if _, err := s.repo.GetUserBySubject(ctx, auth.LocalDevSubject); err != nil {
		t.Fatalf("Local Dev deleted: %v", err)
	}
}

// The relay's lookup-only verifier never resolves the reserved Local Dev
// identity, even while an auth-off boot has left the row active (#124).
func TestLocalDev_LookupOnlyVerifierRefusesIt(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	off, err := auth.NewVerifier(ctx, auth.Config{Repo: s.repo})
	if err != nil {
		t.Fatal(err)
	}
	u, err := off.ProvisionLocalDev(ctx)
	if err != nil || u == nil || u.DisabledAt != nil {
		t.Fatalf("auth-off provision: %+v %v", u, err)
	}
	idp := newTestIdP(t)
	v, err := auth.NewVerifier(ctx, auth.Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, Repo: s.repo, LookupOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	h := v.WithAudiences("mcp", "amp-mcp").Middleware(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if ident, _ := auth.FromContext(r.Context()); ident.User != nil {
			fmt.Fprintf(rw, "user-%d", ident.User.ID)
			return
		}
		fmt.Fprint(rw, "no-user")
	}))
	for _, claims := range []map[string]any{
		{"sub": auth.LocalDevSubject, "email": auth.LocalDevEmail, "aud": "amp-mcp"},
		{"sub": auth.LocalDevSubject, "aud": "amp-mcp"},
		{"sub": fmt.Sprintf("someone-%d", time.Now().UnixNano()), "email": "Local-Dev@amp.local", "aud": "amp-mcp"},
	} {
		req := httptest.NewRequest("POST", "/relay/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+idp.token(t, claims))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.String() != "no-user" {
			t.Fatalf("lookup-only %v: %d %s, want no user", claims, rec.Code, rec.Body)
		}
	}
}
