package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// Putting a project in an org, moving it, and who may change its team links.
// Projects from before orgs existed have no org; only a global admin can
// place one.
func TestProjectOrg_PlaceAndMove(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	ctx := context.Background()
	legacy, err := w.s.repo.CreateProject(ctx, domain.CreateProjectRequest{Name: "legacy", Code: fmt.Sprintf("leg-%d", w.org.ID)})
	if err != nil {
		t.Fatal(err)
	}
	pid := float64(legacy.ID)
	setOrg := func(org int, team int) map[string]any {
		return map[string]any{"project_id": pid, "action": "set_org", "org_id": float64(org), "team_id": float64(team)}
	}

	// An org admin can't claim a project with no org (they can't even see it).
	if msg := w.as(w.orgadmin).callExpectError(t, "amp_manage_project_teams", setOrg(w.org.ID, w.core.ID)); msg == "" {
		t.Fatal("org admin placed a project with no org")
	}

	// A global admin places it, over REST as the UI does, with an owner team.
	out := ra.want(t, 200, "root", http.MethodPost, fmt.Sprintf("/api/projects/%d/org", legacy.ID), map[string]any{"org_id": w.org.ID, "owner_team_id": w.core.ID})
	if p := out["project"].(map[string]any); int(p["org_id"].(float64)) != w.org.ID {
		t.Fatalf("org after placing: %v", p)
	}
	teams := w.as(w.root).call(t, "amp_manage_project_teams", map[string]any{"project_id": pid, "action": "list"})["teams"].([]any)
	if len(teams) != 1 || teams[0].(map[string]any)["role"] != "owner" || int(teams[0].(map[string]any)["team_id"].(float64)) != w.core.ID {
		t.Fatalf("teams after placing: %v", teams)
	}
	// Core's members can reach it now.
	w.as(w.owner).call(t, "amp_get_project", map[string]any{"project_id": pid})

	// The owner team must be in the new org.
	wantErr(t, w.as(w.root).callExpectError(t, "amp_manage_project_teams", setOrg(w.other.ID, w.core.ID)), "team in the new org")

	// Acme's admin can't move it into an org they don't administer...
	wantErr(t, w.as(w.orgadmin).callExpectError(t, "amp_manage_project_teams", setOrg(w.other.ID, w.x.ID)), "owner or admin of that org")
	// ...until they're an admin there too. Links to Acme teams go.
	if err := w.s.repo.SetOrgMember(ctx, w.other.ID, w.orgadmin.User.ID, domain.OrgRoleAdmin); err != nil {
		t.Fatal(err)
	}
	moved := w.as(w.orgadmin).call(t, "amp_manage_project_teams", setOrg(w.other.ID, w.x.ID))
	if un := moved["unlinked"].([]any); len(un) != 1 || int(un[0].(map[string]any)["team_id"].(float64)) != w.core.ID {
		t.Fatalf("unlinked: %v", moved["unlinked"])
	}
	if ts := moved["teams"].([]any); len(ts) != 1 || int(ts[0].(map[string]any)["team_id"].(float64)) != w.x.ID {
		t.Fatalf("teams after the move: %v", ts)
	}
	// Core's members lost access with the move.
	wantErr(t, w.as(w.owner).callExpectError(t, "amp_get_project", map[string]any{"project_id": pid}), "not found")
	// An ordinary member of the project can't move it.
	wantErr(t, w.as(w.outsider).callExpectError(t, "amp_manage_project_teams", setOrg(w.org.ID, 0)), "owner or admin")
}

// A team's lead may link and unlink their own team as a member team, but not
// make it an owner, and not touch other teams.
func TestProjectOrg_TeamLeadLinks(t *testing.T) {
	w := newRBACWorld(t)
	pid := float64(w.s.projectID)
	link := func(team int, role string) map[string]any {
		return map[string]any{"project_id": pid, "action": "link", "team_id": float64(team), "role": role}
	}
	unlink := func(team int) map[string]any {
		return map[string]any{"project_id": pid, "action": "unlink", "team_id": float64(team)}
	}
	lead := w.as(w.lead) // lead of Devs, a member team

	lead.call(t, "amp_manage_project_teams", unlink(w.devs.ID))
	// Devs is gone, so the lead (and Devs) lost access; an org admin links it back.
	w.as(w.orgadmin).call(t, "amp_manage_project_teams", link(w.devs.ID, "member"))
	lead.call(t, "amp_manage_project_teams", link(w.devs.ID, "member"))
	wantErr(t, lead.callExpectError(t, "amp_manage_project_teams", link(w.devs.ID, "owner")), "owner")
	wantErr(t, lead.callExpectError(t, "amp_manage_project_teams", unlink(w.ops.ID)), "lead of that team")
	wantErr(t, lead.callExpectError(t, "amp_manage_project_teams", unlink(w.core.ID)), "lead of that team")
	// A member who isn't a lead can't change links.
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_manage_project_teams", unlink(w.devs.ID)), "lead of that team")
	// Ops's members (not leads) can't link Ops back either.
	wantErr(t, w.as(w.carol).callExpectError(t, "amp_manage_project_teams", link(w.ops.ID, "owner")), "owner")
	// Once Devs is an owner team, its members are project owners, so the
	// lead may change links like any owner.
	w.as(w.orgadmin).call(t, "amp_manage_project_teams", link(w.devs.ID, "owner"))
	lead.call(t, "amp_manage_project_teams", link(w.ops.ID, "owner"))
}
