package main

// Agent accounts, API tokens, deactivation, sign-up limits, and the audit
// log.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// bearerServer serves the REST API behind the real auth middleware, which
// here accepts only AMP API tokens.
func bearerServer(t *testing.T, w *rbacWorld) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb).Register(mux)
	srv := httptest.NewServer(auth.NewAPITokenVerifier(w.s.repo).Middleware(mux))
	t.Cleanup(srv.Close)
	return srv
}

func bearerDo(t *testing.T, srv *httptest.Server, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
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

func num(t *testing.T, m map[string]any, keys ...string) int {
	t.Helper()
	var v any = m
	for _, k := range keys {
		v = v.(map[string]any)[k]
	}
	return int(v.(float64))
}

// mintToken creates a token as who for userID ("me" or an id).
func mintToken(t *testing.T, ra *restAs, who, user string) string {
	t.Helper()
	out := ra.want(t, 200, who, "POST", "/api/users/"+user+"/tokens", map[string]any{"name": "test harness"})
	tok := out["token"].(map[string]any)["token"].(string)
	if !strings.HasPrefix(tok, "amp_") {
		t.Fatalf("token %q", tok)
	}
	return tok
}

func auditActions(t *testing.T, ra *restAs, who, query string) []string {
	t.Helper()
	out := ra.want(t, 200, who, "GET", "/api/audit?"+query, nil)
	var got []string
	for _, e := range out["entries"].([]any) {
		got = append(got, e.(map[string]any)["action"].(string))
	}
	return got
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// An agent account is its own user: it signs in with a token its manager
// minted, sees what its teams see, and can't mint tokens or agents itself.
func TestAccounts_AgentAccountsAndTokens(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	srv := bearerServer(t, w)

	// alice may put her agent on her own team, not on someone else's.
	ra.want(t, 403, "alice", "POST", "/api/agents", map[string]any{"name": "Nope", "team_ids": []int{w.ops.ID}})
	out := ra.want(t, 200, "alice", "POST", "/api/agents", map[string]any{"name": "Alice's Builder", "team_ids": []int{w.devs.ID}})
	agent := out["agent"].(map[string]any)
	agentID := num(t, agent, "id")
	if agent["kind"] != "agent" || !strings.HasSuffix(agent["email"].(string), "@agents.amp") || num(t, agent, "managed_by") != w.alice.User.ID {
		t.Fatalf("agent = %v", agent)
	}
	if list := ra.want(t, 200, "alice", "GET", "/api/agents", nil)["agents"].([]any); len(list) != 1 {
		t.Fatalf("alice's agents = %v (the refused one must not exist)", list)
	}

	// Only alice (or a global admin) manages its tokens.
	ra.want(t, 403, "bob", "POST", fmt.Sprintf("/api/users/%d/tokens", agentID), map[string]any{"name": "x"})
	ra.want(t, 403, "bob", "GET", fmt.Sprintf("/api/users/%d/tokens", agentID), nil)
	tok := mintToken(t, ra, "alice", fmt.Sprint(agentID))

	code, me := bearerDo(t, srv, tok, "GET", "/api/me", nil)
	if code != 200 || num(t, me, "user_id") != agentID || me["kind"] != "agent" || me["via_token"] != true {
		t.Fatalf("/api/me with agent token: %d %v", code, me)
	}
	// It sees the project through the Devs team.
	if code, out := bearerDo(t, srv, tok, "GET", fmt.Sprintf("/api/projects/%d", w.s.projectID), nil); code != 200 {
		t.Fatalf("agent reading its team's project: %d %v", code, out)
	}
	// It can't mint tokens or agents.
	if code, _ := bearerDo(t, srv, tok, "POST", "/api/users/me/tokens", map[string]any{"name": "self"}); code != 403 {
		t.Errorf("agent minting its own token: %d, want 403", code)
	}
	if code, _ := bearerDo(t, srv, tok, "POST", "/api/agents", map[string]any{"name": "child"}); code != 403 {
		t.Errorf("agent creating an agent: %d, want 403", code)
	}
	// Nor be made a global admin.
	ra.want(t, 400, "root", "PATCH", fmt.Sprintf("/api/users/%d/role", agentID), map[string]any{"role": "admin", "grant": true})

	// The stored row holds a hash, never the token.
	tokens, _ := w.s.repo.ListAPITokens(context.Background(), agentID)
	if len(tokens) != 1 || !strings.HasPrefix(tok, tokens[0].Prefix) || tokens[0].LastUsedAt == nil {
		t.Fatalf("tokens = %+v", tokens)
	}

	// Revoked and expired tokens stop working.
	ra.want(t, 404, "bob", "DELETE", fmt.Sprintf("/api/tokens/%d", tokens[0].ID), nil)
	ra.want(t, 200, "alice", "DELETE", fmt.Sprintf("/api/tokens/%d", tokens[0].ID), nil)
	if code, _ := bearerDo(t, srv, tok, "GET", "/api/me", nil); code != 401 {
		t.Errorf("revoked token: %d, want 401", code)
	}
	raw, hash, prefix, _ := auth.NewAPIToken()
	past := time.Now().Add(-time.Hour)
	if _, err := w.s.repo.CreateAPIToken(context.Background(), agentID, "old", hash, prefix, nil, &past); err != nil {
		t.Fatal(err)
	}
	if code, _ := bearerDo(t, srv, raw, "GET", "/api/me", nil); code != 401 {
		t.Errorf("expired token: %d, want 401", code)
	}
	if code, _ := bearerDo(t, srv, "amp_not-a-real-token", "GET", "/api/me", nil); code != 401 {
		t.Errorf("unknown token: %d, want 401", code)
	}

	// People can mint tokens for themselves.
	bobTok := mintToken(t, ra, "bob", "me")
	if code, me := bearerDo(t, srv, bobTok, "GET", "/api/me", nil); code != 200 || num(t, me, "user_id") != w.bob.User.ID {
		t.Fatalf("bob's own token: %d %v", code, me)
	}
}

// A deactivated account is refused everywhere and can't be given work;
// reactivating restores it. Managers deactivate their agents, global
// admins anyone but themselves.
func TestAccounts_Deactivation(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	srv := bearerServer(t, w)

	agentID := num(t, ra.want(t, 200, "alice", "POST", "/api/agents", map[string]any{"name": "Runner", "team_ids": []int{w.devs.ID}}), "agent", "id")
	agentTok := mintToken(t, ra, "alice", fmt.Sprint(agentID))
	bobTok := mintToken(t, ra, "bob", "me")

	ra.want(t, 403, "bob", "POST", fmt.Sprintf("/api/users/%d/deactivate", agentID), nil)
	ra.want(t, 403, "alice", "POST", fmt.Sprintf("/api/users/%d/deactivate", w.bob.User.ID), nil)
	ra.want(t, 400, "root", "POST", fmt.Sprintf("/api/users/%d/deactivate", w.root.User.ID), nil)

	out := ra.want(t, 200, "alice", "POST", fmt.Sprintf("/api/users/%d/deactivate", agentID), nil)
	if out["user"].(map[string]any)["disabled_at"] == nil {
		t.Fatalf("deactivate: %v", out)
	}
	if code, body := bearerDo(t, srv, agentTok, "GET", "/api/me", nil); code != 403 || !strings.Contains(fmt.Sprint(body["error"]), "deactivated") {
		t.Errorf("deactivated agent: %d %v", code, body)
	}
	// No new work for it, and no new tokens.
	tid := taskID(t, w.as(w.lead).call(t, "amp_create_task", map[string]any{
		"project_id": float64(w.s.projectID), "story_id": float64(w.s.storyID), "name": "t", "description": "d", "acceptance_criteria": "a", "owner": "none",
	}))
	ra.want(t, 400, "lead", "POST", fmt.Sprintf("/api/tasks/%d/assign", tid), map[string]any{"user": fmt.Sprint(agentID)})
	ra.want(t, 400, "alice", "POST", fmt.Sprintf("/api/users/%d/tokens", agentID), map[string]any{"name": "x"})

	ra.want(t, 200, "alice", "POST", fmt.Sprintf("/api/users/%d/reactivate", agentID), nil)
	if code, _ := bearerDo(t, srv, agentTok, "GET", "/api/me", nil); code != 200 {
		t.Errorf("reactivated agent: %d", code)
	}
	ra.want(t, 200, "lead", "POST", fmt.Sprintf("/api/tasks/%d/assign", tid), map[string]any{"user": fmt.Sprint(agentID)})

	// A global admin deactivates a person.
	ra.want(t, 200, "root", "POST", fmt.Sprintf("/api/users/%d/deactivate", w.bob.User.ID), nil)
	if code, _ := bearerDo(t, srv, bobTok, "GET", "/api/me", nil); code != 403 {
		t.Errorf("deactivated person: %d, want 403", code)
	}
	// The member lists say so.
	team := ra.want(t, 200, "lead", "GET", fmt.Sprintf("/api/teams/%d", w.devs.ID), nil)
	for _, m := range team["members"].([]any) {
		m := m.(map[string]any)
		switch num(t, m, "user_id") {
		case w.bob.User.ID:
			if m["disabled"] != true {
				t.Errorf("bob in team list: %v", m)
			}
		case agentID:
			if m["kind"] != "agent" || m["disabled"] == true {
				t.Errorf("agent in team list: %v", m)
			}
		}
	}
}

// Who changed access, settings, and what was deleted, readable by the
// people responsible for each scope.
func TestAccounts_AuditLog(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)

	ra.want(t, 200, "orgadmin", "POST", fmt.Sprintf("/api/orgs/%d/members", w.org.ID), map[string]any{"user": w.dave.Email, "role": "admin"})
	ra.want(t, 201, "orgadmin", "POST", fmt.Sprintf("/api/orgs/%d/teams", w.org.ID), map[string]any{"name": "Audited"})
	agentID := num(t, ra.want(t, 200, "alice", "POST", "/api/agents", map[string]any{"name": "Logged", "team_ids": []int{w.devs.ID}}), "agent", "id")
	mintToken(t, ra, "alice", fmt.Sprint(agentID))
	tid := taskID(t, w.as(w.owner).call(t, "amp_create_task", map[string]any{
		"project_id": float64(w.s.projectID), "story_id": float64(w.s.storyID), "name": "doomed", "description": "d", "acceptance_criteria": "a",
	}))
	w.as(w.owner).call(t, "amp_delete_task", map[string]any{"task_id": float64(tid)})
	ra.want(t, 200, "owner", "PATCH", fmt.Sprintf("/api/projects/%d", w.s.projectID), map[string]any{"open_reassign": true})

	org := auditActions(t, ra, "orgadmin", fmt.Sprintf("org_id=%d", w.org.ID))
	for _, want := range []string{"org.member.set", "team.create", "team.member.set", "task.delete", "project.settings"} {
		if !contains(org, want) {
			t.Errorf("org audit lacks %s: %v", want, org)
		}
	}
	// Entries carry who did it.
	out := ra.want(t, 200, "orgadmin", "GET", fmt.Sprintf("/api/audit?org_id=%d&action=org.member.set", w.org.ID), nil)
	e := out["entries"].([]any)[0].(map[string]any)
	if num(t, e, "actor_user_id") != w.orgadmin.User.ID || !strings.Contains(e["summary"].(string), w.dave.Email) {
		t.Errorf("entry = %v", e)
	}
	// Project owners read their project's; prefixes filter.
	proj := auditActions(t, ra, "owner", fmt.Sprintf("project_id=%d&action=task.", w.s.projectID))
	if len(proj) != 1 || proj[0] != "task.delete" {
		t.Errorf("project audit = %v", proj)
	}
	// Global admins read everything, including account changes.
	all := auditActions(t, ra, "root", "limit=500")
	for _, want := range []string{"agent.create", "token.create"} {
		if !contains(all, want) {
			t.Errorf("full audit lacks %s", want)
		}
	}
	// Everyone else is refused.
	ra.want(t, 403, "alice", "GET", "/api/audit", nil)
	ra.want(t, 403, "alice", "GET", fmt.Sprintf("/api/audit?org_id=%d", w.org.ID), nil)
	ra.want(t, 403, "alice", "GET", fmt.Sprintf("/api/audit?project_id=%d", w.s.projectID), nil)
	ra.want(t, 404, "outsider", "GET", fmt.Sprintf("/api/audit?org_id=%d", w.org.ID), nil)
	// And over MCP.
	got := w.as(w.orgadmin).call(t, "amp_audit_log", map[string]any{"org_id": float64(w.org.ID), "action": "team."})
	if num(t, got, "count") < 2 {
		t.Errorf("amp_audit_log = %v", got)
	}
}

// With sign-up domains set, only those domains (and invited emails) become
// AMP users on first sign-in; existing users are unaffected. A sign-in
// never takes over an agent account by email.
func TestAccounts_SignupDomainsAndAgentEmails(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	repo, err := repository.New(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	repo.SetSignupDomains([]string{"@corp.test"})
	sfx := fmt.Sprint(time.Now().UnixNano())

	if _, err := repo.UpsertUserFromClaims(ctx, "s1-"+sfx, "eve-"+sfx+"@gmail.test", "Eve", nil); !errors.Is(err, repository.ErrSignupNotAllowed) {
		t.Errorf("outside domain: %v, want ErrSignupNotAllowed", err)
	}
	if _, err := repo.UpsertUserFromClaims(ctx, "s2-"+sfx, "ann-"+sfx+"@CORP.test", "Ann", nil); err != nil {
		t.Errorf("allowed domain: %v", err)
	}
	invited := "guest-" + sfx + "@gmail.test"
	if _, err := repo.CreateInvite(ctx, domain.Invite{Email: invited, OrgID: w.org.ID, OrgRole: "member", TeamRole: "member"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertUserFromClaims(ctx, "s3-"+sfx, invited, "Guest", nil); err != nil {
		t.Errorf("invited email: %v", err)
	}
	// alice exists already (rbac.test domain): still signs in.
	if _, err := repo.UpsertUserFromClaims(ctx, w.alice.Subject, w.alice.Email, "alice", nil); err != nil {
		t.Errorf("existing user: %v", err)
	}

	ra := newRestAs(t, w)
	agent := ra.want(t, 200, "alice", "POST", "/api/agents", map[string]any{"name": "Target"})["agent"].(map[string]any)
	if _, err := w.s.repo.UpsertUserFromClaims(ctx, "attacker-"+sfx, agent["email"].(string), "x", nil); err == nil {
		t.Error("a sign-in with an agent's email linked to the agent account")
	}
	if u, _ := w.s.repo.GetUserByID(ctx, num(t, agent, "id")); u.Subject != agent["subject"] {
		t.Errorf("agent subject changed to %q", u.Subject)
	}
}
