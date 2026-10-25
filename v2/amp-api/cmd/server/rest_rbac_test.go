package main

// REST coverage for org/team administration, invites, project-team links,
// the default task view, SSE filtering, and the restricted KB config.

import (
	"bufio"
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
	"github.com/simstech/amp-api/internal/domain"
)

// restAs serves the real REST mux; each request runs as the identity named
// in the X-Test-As header (what the auth middleware would have resolved).
type restAs struct {
	w      *rbacWorld
	server *httptest.Server
	ids    map[string]*auth.Identity
}

func newRestAs(t *testing.T, w *rbacWorld) *restAs {
	t.Helper()
	mux := http.NewServeMux()
	api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb).Register(mux)
	ra := &restAs{w: w, ids: map[string]*auth.Identity{
		"owner": w.owner, "alice": w.alice, "bob": w.bob, "lead": w.lead, "carol": w.carol,
		"dave": w.dave, "orgadmin": w.orgadmin, "outsider": w.outsider, "root": w.root,
	}}
	ra.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if ident, ok := ra.ids[r.Header.Get("X-Test-As")]; ok {
			// The web UI's session on the REST listener, unless the test
			// says the request came another way (X-Test-Channel: an MCP-
			// audience token, an API token).
			c := *ident
			c.Listener, c.Channel = "rest", auth.ChannelUI
			if ch := r.Header.Get("X-Test-Channel"); ch != "" {
				c.Channel = ch
				c.APIToken = ch == auth.ChannelToken
			}
			r = r.WithContext(auth.WithIdentity(r.Context(), &c))
		}
		mux.ServeHTTP(rw, r)
	}))
	t.Cleanup(ra.server.Close)
	return ra
}

func (ra *restAs) do(t *testing.T, who, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return ra.doVia(t, "", who, method, path, body)
}

// doVia is do over a channel other than the web UI (auth.ChannelToken,
// auth.ChannelRESTOther); "" is the UI.
func (ra *restAs) doVia(t *testing.T, channel, who, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, ra.server.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.Header.Set("X-Test-As", who)
	}
	if channel != "" {
		req.Header.Set("X-Test-Channel", channel)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (ra *restAs) want(t *testing.T, code int, who, method, path string, body any) map[string]any {
	t.Helper()
	got, out := ra.do(t, who, method, path, body)
	if got != code {
		t.Fatalf("%s %s %s as %s: %d %v, want %d", method, path, mustJSON(body), who, got, out, code)
	}
	return out
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestREST_Unauthenticated(t *testing.T) {
	ra := newRestAs(t, newRBACWorld(t))
	for _, path := range []string{"/api/projects", "/api/orgs", fmt.Sprintf("/api/projects/%d/tasks", ra.w.s.projectID)} {
		ra.want(t, 401, "", "GET", path, nil)
	}
}

func TestREST_OnlyGlobalAdminsCreateOrgs(t *testing.T) {
	ra := newRestAs(t, newRBACWorld(t))
	slug := fmt.Sprintf("new-org-%d", time.Now().UnixNano())
	for _, who := range []string{"orgadmin", "alice"} {
		ra.want(t, 403, who, "POST", "/api/orgs", map[string]any{"name": "Nope", "slug": slug})
	}
	out := ra.want(t, 201, "root", "POST", "/api/orgs", map[string]any{"name": "New Org", "slug": slug, "owner": ra.w.alice.Email})
	orgID := int(out["org"].(map[string]any)["id"].(float64))
	// alice is now its owner and sees it; bob doesn't.
	orgs := ra.want(t, 200, "alice", "GET", "/api/orgs", nil)["orgs"].([]any)
	if !slices.ContainsFunc(orgs, func(o any) bool {
		m := o.(map[string]any)
		return int(m["id"].(float64)) == orgID && m["my_role"] == "owner"
	}) {
		t.Fatalf("alice's orgs %v missing the new org as owner", orgs)
	}
	ra.want(t, 404, "bob", "GET", fmt.Sprintf("/api/orgs/%d", orgID), nil)
	// Duplicate slug.
	ra.want(t, 409, "root", "POST", "/api/orgs", map[string]any{"name": "Dup", "slug": slug})
}

// An org admin invites someone who has never signed in; their first sign-in
// (JIT provisioning) applies the membership.
func TestREST_InviteAppliedOnFirstSignIn(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	email := fmt.Sprintf("Newbie-%d@Invite.test", time.Now().UnixNano())
	path := fmt.Sprintf("/api/teams/%d/members", w.devs.ID)

	// The team lead adds an unknown email to their team → pending invite.
	out := ra.want(t, 201, "lead", "POST", path, map[string]any{"user": email, "role": "member"})
	if out["invited"] != true {
		t.Fatalf("expected an invite, got %v", out)
	}
	invites := ra.want(t, 200, "orgadmin", "GET", fmt.Sprintf("/api/orgs/%d/invites", w.org.ID), nil)["invites"].([]any)
	if len(invites) != 1 || invites[0].(map[string]any)["email"] != strings.ToLower(email) {
		t.Fatalf("pending invites %v", invites)
	}
	// Non-admins can't list invites.
	ra.want(t, 403, "lead", "GET", fmt.Sprintf("/api/orgs/%d/invites", w.org.ID), nil)

	// First sign-in, with different email casing.
	u, err := w.s.repo.UpsertUserFromClaims(context.Background(), fmt.Sprintf("sub-newbie-%d", time.Now().UnixNano()), strings.ToUpper(email), "Newbie", nil)
	if err != nil {
		t.Fatal(err)
	}
	if role, _ := w.s.repo.GetTeamRole(context.Background(), w.devs.ID, u.ID); role != domain.TeamRoleMember {
		t.Fatalf("team role after first sign-in = %q", role)
	}
	if role, _ := w.s.repo.GetOrgRole(context.Background(), w.org.ID, u.ID); role != domain.OrgRoleMember {
		t.Fatalf("org role after first sign-in = %q", role)
	}
	if left := ra.want(t, 200, "orgadmin", "GET", fmt.Sprintf("/api/orgs/%d/invites", w.org.ID), nil)["invites"].([]any); len(left) != 0 {
		t.Fatalf("invite still pending: %v", left)
	}
	// They now reach the project through Devs.
	if lvl, _ := w.s.repo.GetProjectAccess(context.Background(), u.ID, w.s.projectID); !lvl.Member {
		t.Fatal("invitee has no project access")
	}

	// Inviting an existing user applies immediately.
	out = ra.want(t, 200, "orgadmin", "POST", fmt.Sprintf("/api/teams/%d/members", w.ops.ID), map[string]any{"user": w.dave.Email})
	if out["user_id"] == nil {
		t.Fatalf("existing user should be added directly: %v", out)
	}
}

func TestREST_TeamLeadsManageOnlyTheirTeam(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	ra.want(t, 200, "lead", "POST", fmt.Sprintf("/api/teams/%d/members", w.devs.ID), map[string]any{"user": w.dave.Email})
	ra.want(t, 403, "lead", "POST", fmt.Sprintf("/api/teams/%d/members", w.ops.ID), map[string]any{"user": w.dave.Email})
	ra.want(t, 403, "alice", "POST", fmt.Sprintf("/api/teams/%d/members", w.devs.ID), map[string]any{"user": w.carol.Email})
	// Leads can't create teams or add org admins.
	ra.want(t, 403, "lead", "POST", fmt.Sprintf("/api/orgs/%d/teams", w.org.ID), map[string]any{"name": "Rogue"})
	ra.want(t, 403, "lead", "POST", fmt.Sprintf("/api/orgs/%d/members", w.org.ID), map[string]any{"user": w.dave.Email, "role": "admin"})
	// Outsiders can't even see the team.
	ra.want(t, 404, "outsider", "GET", fmt.Sprintf("/api/teams/%d", w.devs.ID), nil)
}

func TestREST_OrgOwnerRules(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	base := fmt.Sprintf("/api/orgs/%d/members", w.org.ID)
	// orgadmin is the only owner: can't be removed or demoted.
	ra.want(t, 409, "root", "DELETE", fmt.Sprintf("%s/%d", base, w.orgadmin.User.ID), nil)
	ra.want(t, 409, "root", "PUT", fmt.Sprintf("%s/%d", base, w.orgadmin.User.ID), map[string]any{"role": "member"})
	// An org admin (not owner) can't grant ownership.
	ra.want(t, 200, "orgadmin", "PUT", fmt.Sprintf("%s/%d", base, w.lead.User.ID), map[string]any{"role": "admin"})
	ra.want(t, 403, "lead", "PUT", fmt.Sprintf("%s/%d", base, w.bob.User.ID), map[string]any{"role": "owner"})
	// Removing someone from the org also removes them from its teams.
	ra.want(t, 200, "orgadmin", "DELETE", fmt.Sprintf("%s/%d", base, w.bob.User.ID), nil)
	if role, _ := w.s.repo.GetTeamRole(context.Background(), w.devs.ID, w.bob.User.ID); role != "" {
		t.Fatal("bob still on Devs after leaving the org")
	}
}

func TestREST_ProjectTeams(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	base := fmt.Sprintf("/api/projects/%d/teams", w.s.projectID)
	if teams := ra.want(t, 200, "carol", "GET", base, nil)["teams"].([]any); len(teams) != 3 {
		t.Fatalf("teams %v", teams)
	}
	// Members can't change links; owners can.
	ra.want(t, 403, "alice", "PUT", fmt.Sprintf("%s/%d", base, w.devs.ID), map[string]any{"role": "owner"})
	ra.want(t, 200, "owner", "PUT", fmt.Sprintf("%s/%d", base, w.devs.ID), map[string]any{"role": "owner"})
	// Another org's team can't be linked.
	ra.want(t, 409, "owner", "PUT", fmt.Sprintf("%s/%d", base, w.x.ID), map[string]any{"role": "member"})
	// The last owner team can't be removed or demoted.
	ra.want(t, 200, "owner", "PUT", fmt.Sprintf("%s/%d", base, w.devs.ID), map[string]any{"role": "member"})
	ra.want(t, 409, "owner", "DELETE", fmt.Sprintf("%s/%d", base, w.core.ID), nil)
	ra.want(t, 409, "orgadmin", "DELETE", fmt.Sprintf("/api/teams/%d", w.core.ID), nil)
	// Unlinking Ops removes carol's access.
	ra.want(t, 200, "owner", "DELETE", fmt.Sprintf("%s/%d", base, w.ops.ID), nil)
	ra.want(t, 404, "carol", "GET", fmt.Sprintf("/api/projects/%d", w.s.projectID), nil)
}

func TestREST_TaskListDefaultsToMine(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	base := fmt.Sprintf("/api/projects/%d/tasks", w.s.projectID)
	body := func(name string, extra map[string]any) map[string]any {
		m := map[string]any{"epic_id": w.s.epicID, "story_id": w.s.storyID, "name": name, "priority": "1"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	mine := ra.want(t, 200, "alice", "POST", base, body("alice's", nil))
	ra.want(t, 200, "bob", "POST", base, body("bob's", nil))
	// Clients can't forge attribution.
	forged := ra.want(t, 200, "alice", "POST", base, body("forged", map[string]any{"created_by": w.bob.User.ID, "owner_user_id": w.bob.User.ID}))
	if int(forged["created_by"].(float64)) != w.alice.User.ID || int(forged["owner_user_id"].(float64)) != w.alice.User.ID {
		t.Fatalf("attribution taken from the request body: %v", forged)
	}

	names := func(who, q string) []string {
		var out []string
		for _, tk := range ra.want(t, 200, who, "GET", base+q, nil)["tasks"].([]any) {
			out = append(out, tk.(map[string]any)["name"].(string))
		}
		return out
	}
	if got := names("alice", ""); !slices.Contains(got, "alice's") || slices.Contains(got, "bob's") {
		t.Fatalf("alice default %v", got)
	}
	if got := names("alice", "?owner=any"); !slices.Contains(got, "bob's") {
		t.Fatalf("alice any %v", got)
	}
	// Assign through REST, then it's bob's.
	tid := int(mine["id"].(float64))
	ra.want(t, 200, "alice", "POST", fmt.Sprintf("/api/tasks/%d/assign", tid), map[string]any{"user": w.bob.Email})
	if got := names("bob", ""); !slices.Contains(got, "alice's") {
		t.Fatalf("bob default after assignment %v", got)
	}
	ra.want(t, 403, "alice", "POST", fmt.Sprintf("/api/tasks/%d/complete", tid), nil)
	ra.want(t, 400, "alice", "POST", fmt.Sprintf("/api/tasks/%d/assign", tid), map[string]any{"user": ""})

	// Project settings: owners only.
	proj := fmt.Sprintf("/api/projects/%d", w.s.projectID)
	ra.want(t, 403, "alice", "PATCH", proj, map[string]any{"open_reassign": false})
	ra.want(t, 400, "owner", "PATCH", proj, map[string]any{})
	if p := ra.want(t, 200, "owner", "PATCH", proj, map[string]any{"open_reassign": false})["project"].(map[string]any); p["open_reassign"] != false {
		t.Fatalf("PATCH project = %v", p)
	}
}

func TestREST_KBConfigIsAdminOnly(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	ra.want(t, 403, "owner", "GET", fmt.Sprintf("/api/kb/config?project_id=%d", w.s.projectID), nil)
	ra.want(t, 404, "outsider", "GET", fmt.Sprintf("/api/kb/list?project_id=%d", w.s.projectID), nil)
}

// The SSE stream only carries events for projects the viewer can see.
func TestREST_EventsAreFilteredPerUser(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	open := func(who string) *bufio.Scanner {
		req, _ := http.NewRequest("GET", ra.server.URL+"/api/events", nil)
		req.Header.Set("X-Test-As", who)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Scan() // "connected"
		return sc
	}
	aliceStream, outsiderStream := open("alice"), open("outsider")

	otherProj, err := w.s.repo.CreateProject(context.Background(), domain.CreateProjectRequest{
		Name: "o", Code: fmt.Sprintf("o-%d", time.Now().UnixNano()), OrgID: &w.other.ID, TeamID: &w.x.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.s.hub.Publish(domain.Event{Type: "test.other", ProjectID: otherProj.ID})
	w.s.hub.Publish(domain.Event{Type: "test.acme", ProjectID: w.s.projectID})

	next := func(sc *bufio.Scanner) string {
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				return line
			}
		}
		return ""
	}
	if got := next(aliceStream); !strings.Contains(got, "test.acme") {
		t.Fatalf("alice's first event %q, want only her project's", got)
	}
	if got := next(outsiderStream); !strings.Contains(got, "test.other") {
		t.Fatalf("outsider's first event %q, want only their project's", got)
	}
}
