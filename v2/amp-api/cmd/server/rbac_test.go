package main

// End-to-end coverage for orgs/teams/ownership through the real MCP tools
// (in-process: JSON-RPC → tool handler → authz → actors → postgres), run as
// distinct users. See internal/authz for the rules being exercised.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
)

// rbacWorld is one org with a project and a cast of users at every level.
//
//	org "Acme"
//	  team Core (owner team on the project) : owner
//	  team Devs (member team)               : alice, bob, lead (lead)
//	  team Ops  (member team)               : carol
//	  org owner, not on any team            : orgadmin
//	  org member, not on any team           : dave
//	org "Other"
//	  team X                                : outsider
//	global admin, in no org                 : root
type rbacWorld struct {
	s                                    *mcpStack // pointed at the Acme project
	org, other                           *domain.Org
	core, devs, ops, x                   *domain.Team
	owner, alice, bob, lead, carol, dave *auth.Identity
	orgadmin, outsider, root             *auth.Identity
}

func newRBACWorld(t *testing.T) *rbacWorld {
	t.Helper()
	base := setupMCP(t)
	ctx := context.Background()
	repo := base.repo
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())

	user := func(name string, admin bool) *auth.Identity {
		email := fmt.Sprintf("%s-%s@rbac.test", name, sfx)
		u, err := repo.UpsertUserFromClaims(ctx, "sub-"+name+"-"+sfx, email, name, nil)
		if err != nil {
			t.Fatalf("user %s: %v", name, err)
		}
		// The first user ever provisioned is auto-admin; pin roles explicitly.
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleAdmin, admin); err != nil {
			t.Fatal(err)
		}
		u.Roles = []string{domain.RoleMember}
		if admin {
			u.Roles = []string{domain.RoleAdmin}
		}
		return &auth.Identity{Subject: u.Subject, Email: u.Email, Name: name, User: u}
	}
	// root first: on an empty database the first user is auto-admin, and
	// the last global admin can't be demoted, so the admin must exist
	// before anyone else's role is pinned.
	root := user("root", true)
	w := &rbacWorld{
		root:  root,
		owner: user("owner", false), alice: user("alice", false), bob: user("bob", false),
		lead: user("lead", false), carol: user("carol", false), dave: user("dave", false),
		orgadmin: user("orgadmin", false), outsider: user("outsider", false),
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	w.org, err = repo.CreateOrg(ctx, "Acme "+sfx, "acme-"+sfx, nil, &w.orgadmin.User.ID)
	must(err)
	w.other, err = repo.CreateOrg(ctx, "Other "+sfx, "other-"+sfx, nil, &w.outsider.User.ID)
	must(err)
	team := func(org *domain.Org, name string) *domain.Team {
		tm, err := repo.CreateTeam(ctx, org.ID, name, nil)
		must(err)
		return tm
	}
	w.core, w.devs, w.ops, w.x = team(w.org, "Core"), team(w.org, "Devs"), team(w.org, "Ops"), team(w.other, "X")
	must(repo.SetTeamMember(ctx, w.core.ID, w.owner.User.ID, domain.TeamRoleMember))
	must(repo.SetTeamMember(ctx, w.devs.ID, w.alice.User.ID, domain.TeamRoleMember))
	must(repo.SetTeamMember(ctx, w.devs.ID, w.bob.User.ID, domain.TeamRoleMember))
	must(repo.SetTeamMember(ctx, w.devs.ID, w.lead.User.ID, domain.TeamRoleLead))
	must(repo.SetTeamMember(ctx, w.ops.ID, w.carol.User.ID, domain.TeamRoleMember))
	must(repo.SetTeamMember(ctx, w.x.ID, w.outsider.User.ID, domain.TeamRoleMember))
	must(repo.SetOrgMember(ctx, w.org.ID, w.dave.User.ID, domain.OrgRoleMember))

	proj, err := repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: "rbac " + sfx, Code: "rbac-" + sfx, OrgID: &w.org.ID, TeamID: &w.core.ID,
	})
	must(err)
	must(repo.SetProjectTeam(ctx, proj.ID, w.devs.ID, domain.ProjectTeamMember))
	must(repo.SetProjectTeam(ctx, proj.ID, w.ops.ID, domain.ProjectTeamMember))
	epic, err := repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: proj.ID, Name: "shared epic", Priority: "1"})
	must(err)
	story, err := repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: proj.ID, EpicID: epic.ID, Name: "shared story", Priority: "1"})
	must(err)

	s := *base
	s.projectID, s.epicID, s.storyID = proj.ID, epic.ID, story.ID
	w.s = &s
	return w
}

func (w *rbacWorld) as(id *auth.Identity) *mcpStack { return w.s.as(id) }

func taskID(t *testing.T, m map[string]interface{}) int {
	t.Helper()
	if task, ok := m["task"].(map[string]interface{}); ok {
		return intField(t, task, "id")
	}
	return intField(t, m, "id")
}

func taskOwner(t *testing.T, s *mcpStack, id int) *int {
	t.Helper()
	got, err := s.repo.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.OwnerUserID
}

func listedIDs(t *testing.T, s *mcpStack, owner string) []int {
	t.Helper()
	args := map[string]interface{}{"project_id": float64(s.projectID)}
	if owner != "" {
		args["owner"] = owner
	}
	res := s.call(t, "amp_list_tasks", args)
	var out []int
	for _, bucket := range []string{"ready_to_dispatch", "in_progress", "blocked", "scheduled", "completed"} {
		out = append(out, sliceIDs(t, res, bucket)...)
	}
	return out
}

func wantErr(t *testing.T, msg string, contains ...string) {
	t.Helper()
	for _, c := range contains {
		if !strings.Contains(msg, c) {
			t.Fatalf("error %q does not mention %q", msg, c)
		}
	}
}

// ---- Visibility ----

func TestRBAC_ProjectVisibility(t *testing.T) {
	w := newRBACWorld(t)
	projects := func(id *auth.Identity) []int {
		res := w.as(id).call(t, "amp_list_projects", map[string]interface{}{})
		return sliceIDs(t, res, "projects")
	}
	for _, id := range []*auth.Identity{w.owner, w.alice, w.lead, w.carol, w.orgadmin, w.root} {
		if !slices.Contains(projects(id), w.s.projectID) {
			t.Errorf("%s should see the project", id.Name)
		}
	}
	// Org membership alone isn't access: dave is in Acme but on no team.
	for _, id := range []*auth.Identity{w.dave, w.outsider} {
		if slices.Contains(projects(id), w.s.projectID) {
			t.Errorf("%s must not see the project", id.Name)
		}
		// ...and every project-scoped tool reports it as not found.
		msg := w.as(id).callExpectError(t, "amp_get_project", map[string]interface{}{"project_id": float64(w.s.projectID)})
		wantErr(t, msg, "not found")
		msg = w.as(id).callExpectError(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(w.s.projectID)})
		wantErr(t, msg, "not found")
		msg = w.as(id).callExpectError(t, "amp_get_story", map[string]interface{}{"story_id": float64(w.s.storyID)})
		wantErr(t, msg, "story", "not found")
		if strings.Contains(msg, "project") {
			t.Errorf("not-found error leaks the story's project: %q", msg)
		}
		msg = w.as(id).callExpectError(t, "amp_kb_list", map[string]interface{}{"project_id": float64(w.s.projectID)})
		wantErr(t, msg, "not found")
	}
	// A task reached by ID is just as hidden.
	tid := taskID(t, w.as(w.alice).mkTask(t, "secret", nil))
	msg := w.as(w.outsider).callExpectError(t, "amp_get_task", map[string]interface{}{"task_id": float64(tid)})
	wantErr(t, msg, "not found")
	msg = w.as(w.outsider).callExpectError(t, "amp_complete_task", map[string]interface{}{"task_id": float64(tid)})
	wantErr(t, msg, "not found")
}

func TestRBAC_TokenWithoutUserIsRefused(t *testing.T) {
	w := newRBACWorld(t)
	noUser := &auth.Identity{Subject: "machine-client"} // validated token, no email claim
	msg := w.as(noUser).callExpectError(t, "amp_list_projects", map[string]interface{}{})
	wantErr(t, msg, "no AMP user")
}

// ---- Ticket ownership ----

func TestRBAC_CreatedTicketsAreOwnedByCreator(t *testing.T) {
	w := newRBACWorld(t)
	res := w.as(w.alice).mkTask(t, "alice's work", nil)
	tid := taskID(t, res)
	got, _ := w.s.repo.GetTask(context.Background(), tid)
	if got.CreatedBy == nil || *got.CreatedBy != w.alice.User.ID || got.OwnerUserID == nil || *got.OwnerUserID != w.alice.User.ID {
		t.Fatalf("created_by=%v owner=%v, want alice (%d) for both", got.CreatedBy, got.OwnerUserID, w.alice.User.ID)
	}
	// The activity log records the user too.
	hist := w.as(w.alice).call(t, "amp_get_ticket_history", map[string]interface{}{"task_id": float64(tid)})
	first := hist["history"].([]interface{})[0].(map[string]interface{})
	if intField(t, first, "actor_user_id") != w.alice.User.ID {
		t.Fatalf("created activity actor_user_id = %v", first["actor_user_id"])
	}
}

func TestRBAC_DefaultListIsYourOwnWork(t *testing.T) {
	w := newRBACWorld(t)
	mine := taskID(t, w.as(w.alice).mkTask(t, "alice owns", nil))
	pool := taskID(t, w.as(w.alice).mkTask(t, "alice's pool ticket", map[string]interface{}{"owner": "none"}))
	bobs := taskID(t, w.as(w.bob).mkTask(t, "bob owns", nil))
	bobPool := taskID(t, w.as(w.bob).mkTask(t, "bob's pool ticket", map[string]interface{}{"owner": "none"}))

	alice := w.as(w.alice)
	if got := listedIDs(t, alice, ""); !slices.Contains(got, mine) || !slices.Contains(got, pool) ||
		slices.Contains(got, bobs) || slices.Contains(got, bobPool) {
		t.Fatalf("alice default view %v: want her own (%d) and her unassigned (%d) only", got, mine, pool)
	}
	if got := listedIDs(t, alice, "unassigned"); !slices.Contains(got, pool) || !slices.Contains(got, bobPool) || slices.Contains(got, mine) {
		t.Fatalf("unassigned view %v", got)
	}
	if got := listedIDs(t, alice, "any"); len(got) < 4 {
		t.Fatalf("any view %v: want all four", got)
	}
	if got := listedIDs(t, alice, w.bob.Email); !slices.Equal(got, []int{bobs}) {
		t.Fatalf("bob's view from alice %v, want [%d]", got, bobs)
	}
}

func TestRBAC_LegacyTicketsStayInEveryonesDefaultView(t *testing.T) {
	w := newRBACWorld(t)
	// A row from before attribution: no creator, no owner. Created before
	// any tool call, so the project's actors load it on first use.
	legacy, err := w.s.repo.CreateTask(context.Background(), domain.CreateTaskRequest{
		ProjectID: w.s.projectID, EpicID: w.s.epicID, StoryID: w.s.storyID, Name: "legacy", Priority: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []*auth.Identity{w.alice, w.carol} {
		if !slices.Contains(listedIDs(t, w.as(id), ""), legacy.ID) {
			t.Errorf("legacy ticket missing from %s's default view", id.Name)
		}
	}
}

func TestRBAC_ClaimAssignRelease(t *testing.T) {
	w := newRBACWorld(t)
	pool := taskID(t, w.as(w.alice).mkTask(t, "pool", map[string]interface{}{"owner": "none"}))

	// bob claims it; it's now his, and alice can't take it.
	w.as(w.bob).call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(pool)})
	if o := taskOwner(t, w.s, pool); o == nil || *o != w.bob.User.ID {
		t.Fatalf("owner after bob's claim = %v", o)
	}
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_claim_task", map[string]interface{}{"task_id": float64(pool)}), "owned by")
	// Claiming never takes a ticket from someone, even for a global admin.
	wantErr(t, w.as(w.root).callExpectError(t, "amp_claim_task", map[string]interface{}{"task_id": float64(pool)}), "owned by")
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(pool), "agent_id": "a"}), "owned by")
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_complete_task", map[string]interface{}{"task_id": float64(pool)}), "owned by")
	// Others can still read and comment.
	w.as(w.alice).call(t, "amp_get_task", map[string]interface{}{"task_id": float64(pool)})
	w.as(w.carol).call(t, "amp_add_task_comment", map[string]interface{}{"task_id": float64(pool), "body": "fyi"})

	// bob hands it to carol (Ops team, same project): it moves to her view.
	w.as(w.bob).call(t, "amp_assign_task", map[string]interface{}{"task_id": float64(pool), "user": w.carol.Email})
	if o := taskOwner(t, w.s, pool); o == nil || *o != w.carol.User.ID {
		t.Fatalf("owner after assign = %v", o)
	}
	if !slices.Contains(listedIDs(t, w.as(w.carol), ""), pool) || slices.Contains(listedIDs(t, w.as(w.bob), ""), pool) {
		t.Fatal("assigned ticket should move from bob's view to carol's")
	}
	// bob no longer owns it and isn't a lead: he can't take it back.
	wantErr(t, w.as(w.bob).callExpectError(t, "amp_assign_task", map[string]interface{}{"task_id": float64(pool), "user": "me"}), "owned by")

	// A team lead can reassign anyone's ticket; so can a project owner.
	w.as(w.lead).call(t, "amp_assign_task", map[string]interface{}{"task_id": float64(pool), "user": w.alice.Email})
	w.as(w.owner).call(t, "amp_assign_task", map[string]interface{}{"task_id": float64(pool), "user": "me"})
	if o := taskOwner(t, w.s, pool); o == nil || *o != w.owner.User.ID {
		t.Fatalf("owner after project owner took it = %v", o)
	}

	// Release: back to the pool.
	w.as(w.owner).call(t, "amp_assign_task", map[string]interface{}{"task_id": float64(pool), "user": "none"})
	if o := taskOwner(t, w.s, pool); o != nil {
		t.Fatalf("owner after release = %d", *o)
	}
}

func TestRBAC_AssigneeMustHaveProjectAccess(t *testing.T) {
	w := newRBACWorld(t)
	tid := taskID(t, w.as(w.alice).mkTask(t, "mine", nil))
	for _, who := range []*auth.Identity{w.outsider, w.dave} {
		msg := w.as(w.alice).callExpectError(t, "amp_assign_task", map[string]interface{}{"task_id": float64(tid), "user": who.Email})
		wantErr(t, msg, "no access to project")
		msg = w.as(w.alice).callExpectError(t, "amp_create_task", map[string]interface{}{
			"project_id": float64(w.s.projectID), "epic_id": float64(w.s.epicID), "story_id": float64(w.s.storyID),
			"name": "x", "owner": who.Email,
		})
		wantErr(t, msg, "no access to project")
	}
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_assign_task", map[string]interface{}{"task_id": float64(tid), "user": "nobody@nowhere.test"}), "no AMP user")
}

func TestRBAC_DispatchClaimsUnassignedTicket(t *testing.T) {
	w := newRBACWorld(t)
	pool := taskID(t, w.as(w.alice).mkTask(t, "pool", map[string]interface{}{"owner": "none"}))
	w.as(w.carol).call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(pool), "agent_id": "carol-agent"})
	if o := taskOwner(t, w.s, pool); o == nil || *o != w.carol.User.ID {
		t.Fatalf("dispatching an unassigned ticket should claim it for carol, owner=%v", o)
	}
	// Releasing an in-progress ticket puts it back in backlog with no agent.
	w.as(w.carol).call(t, "amp_assign_task", map[string]interface{}{"task_id": float64(pool), "user": "none"})
	got, _ := w.s.repo.GetTask(context.Background(), pool)
	if got.State != domain.TaskStateBacklog || got.AgentID != "" || got.OwnerUserID != nil {
		t.Fatalf("after release: state=%s agent=%q owner=%v", got.State, got.AgentID, got.OwnerUserID)
	}
	// Org admins keep an escape hatch: they can dispatch someone else's ticket.
	bobs := taskID(t, w.as(w.bob).mkTask(t, "bob's", nil))
	w.as(w.orgadmin).call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(bobs), "agent_id": "ops"})
	if o := taskOwner(t, w.s, bobs); o == nil || *o != w.bob.User.ID {
		t.Fatal("an org admin dispatching must not take ownership")
	}
}

// Ten users race to claim one ticket: exactly one wins, everyone else gets
// a clear error, and the stored owner is the winner.
func TestRBAC_ConcurrentClaimsHaveOneWinner(t *testing.T) {
	w := newRBACWorld(t)
	pool := taskID(t, w.as(w.alice).mkTask(t, "contested", map[string]interface{}{"owner": "none"}))
	racers := []*auth.Identity{w.alice, w.bob, w.carol, w.lead, w.owner}

	for round := 0; round < 5; round++ {
		w.as(w.owner).call(t, "amp_assign_task", map[string]interface{}{"task_id": float64(pool), "user": "none"})
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			winners []int
		)
		for _, r := range racers {
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(id *auth.Identity) {
					defer wg.Done()
					st := w.as(id)
					raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"amp_claim_task","arguments":{"task_id":%d}}}`, pool)
					resp, _ := json.Marshal(st.srv.HandleMessage(st.callCtx(), []byte(raw)))
					if !strings.Contains(string(resp), `"error"`) {
						mu.Lock()
						winners = append(winners, id.User.ID)
						mu.Unlock()
					}
				}(r)
			}
		}
		wg.Wait()
		owner := taskOwner(t, w.s, pool)
		if owner == nil {
			t.Fatalf("round %d: nobody owns it", round)
		}
		for _, wnr := range winners {
			if wnr != *owner {
				t.Fatalf("round %d: user %d was told they claimed it but the owner is %d (winners %v)", round, wnr, *owner, winners)
			}
		}
	}
}

// ---- Shared groupings ----

func TestRBAC_EpicAndStoryOwners(t *testing.T) {
	w := newRBACWorld(t)
	res := w.as(w.alice).call(t, "amp_create_epic", map[string]interface{}{"project_id": float64(w.s.projectID), "name": "alice's epic"})
	epicID := intField(t, res, "id")
	e, _ := w.s.repo.GetEpic(context.Background(), epicID)
	if e.CreatedBy == nil || *e.CreatedBy != w.alice.User.ID || !slices.Equal(e.OwnerIDs, []int{w.alice.User.ID}) {
		t.Fatalf("new epic created_by=%v owners=%v, want alice", e.CreatedBy, e.OwnerIDs)
	}

	// Epics are shared: bob can put stories and tickets under it...
	st := w.as(w.bob).call(t, "amp_create_story", map[string]interface{}{"project_id": float64(w.s.projectID), "epic_id": float64(epicID), "name": "bob's story"})
	storyID := intField(t, st, "id")
	// ...but can't change it, and neither can a lead (Lead < Owner).
	for _, id := range []*auth.Identity{w.bob, w.lead} {
		msg := w.as(id).callExpectError(t, "amp_update_epic", map[string]interface{}{"epic_id": float64(epicID), "name": "renamed"})
		wantErr(t, msg, "only its owners")
	}
	wantErr(t, w.as(w.bob).callExpectError(t, "amp_delete_epic", map[string]interface{}{"epic_id": float64(epicID)}), "only its owners")

	// alice makes bob a co-owner; now he can.
	w.as(w.alice).call(t, "amp_update_epic", map[string]interface{}{"epic_id": float64(epicID), "add_owners": []interface{}{w.bob.Email}})
	w.as(w.bob).call(t, "amp_update_epic", map[string]interface{}{"epic_id": float64(epicID), "name": "renamed by bob"})
	e, _ = w.s.repo.GetEpic(context.Background(), epicID)
	if e.Name != "renamed by bob" || !slices.Contains(e.OwnerIDs, w.bob.User.ID) {
		t.Fatalf("epic %+v", e)
	}
	// Owners must be project members.
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_update_epic", map[string]interface{}{"epic_id": float64(epicID), "add_owners": []interface{}{w.outsider.Email}}), "no access to project")

	// Stories work the same way; project owners can always edit.
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_update_story", map[string]interface{}{"story_id": float64(storyID), "name": "x"}), "only its owners")
	w.as(w.owner).call(t, "amp_update_story", map[string]interface{}{"story_id": float64(storyID), "name": "edited by project owner"})
}

// ---- Project-level permissions ----

func TestRBAC_ProjectManagementNeedsOwner(t *testing.T) {
	w := newRBACWorld(t)
	for _, tool := range []string{"amp_archive_project", "amp_reset_project"} {
		for _, id := range []*auth.Identity{w.alice, w.lead, w.carol} {
			msg := w.as(id).callExpectError(t, tool, map[string]interface{}{"project_id": float64(w.s.projectID)})
			wantErr(t, msg, "requires owner access")
		}
	}
	w.as(w.owner).call(t, "amp_archive_project", map[string]interface{}{"project_id": float64(w.s.projectID)})
	w.as(w.orgadmin).call(t, "amp_restore_project", map[string]interface{}{"project_id": float64(w.s.projectID)})
}

// Leads and project owners reassign; they don't work someone else's ticket
// under that person's name. Org admins can, as an escape hatch.
func TestRBAC_OnlyTheOwnerWorksATicket(t *testing.T) {
	w := newRBACWorld(t)
	tid := taskID(t, w.as(w.alice).mkTask(t, "alice's", nil))
	for _, id := range []*auth.Identity{w.bob, w.lead, w.owner} {
		for _, tool := range []string{"amp_complete_task", "amp_block_task", "amp_update_task"} {
			msg := w.as(id).callExpectError(t, tool, map[string]interface{}{"task_id": float64(tid), "reason": "x", "name": "x"})
			wantErr(t, msg, "owned by")
		}
	}
	w.as(w.orgadmin).call(t, "amp_block_task", map[string]interface{}{"task_id": float64(tid), "reason": "org admin override"})
	// The owner-team member can still take it over explicitly, then work it.
	w.as(w.owner).call(t, "amp_assign_task", map[string]interface{}{"task_id": float64(tid), "user": "me"})
	w.as(w.owner).call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(tid)})
}

func TestRBAC_DeleteTicket(t *testing.T) {
	w := newRBACWorld(t)
	tid := taskID(t, w.as(w.alice).mkTask(t, "alice's", nil))
	wantErr(t, w.as(w.bob).callExpectError(t, "amp_delete_task", map[string]interface{}{"task_id": float64(tid)}), "owned by")
	w.as(w.alice).call(t, "amp_delete_task", map[string]interface{}{"task_id": float64(tid)})
}

func TestRBAC_CreateProjectPlacement(t *testing.T) {
	w := newRBACWorld(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	// alice is in one org and one team: both are inferred.
	res := w.as(w.alice).call(t, "amp_create_project", map[string]interface{}{"name": "alice proj", "code": "ap-" + sfx})
	pid := intField(t, res, "id")
	teams, _ := w.s.repo.ListProjectTeams(context.Background(), pid)
	if len(teams) != 1 || teams[0].TeamID != w.devs.ID || teams[0].Role != domain.ProjectTeamOwner {
		t.Fatalf("alice's project teams = %+v, want Devs as owner", teams)
	}
	// dave is in the org but on no team: refused.
	wantErr(t, w.as(w.dave).callExpectError(t, "amp_create_project", map[string]interface{}{"name": "d", "code": "dp-" + sfx}), "not on any team")
	// alice can't create in an org she isn't in.
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_create_project", map[string]interface{}{"name": "x", "code": "xp-" + sfx, "org_id": float64(w.other.ID)}), "not found")
	// ...or name a team she isn't on.
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_create_project", map[string]interface{}{"name": "y", "code": "yp-" + sfx, "team_id": float64(w.ops.ID)}), "not on team")
	// The outsider's project is invisible to alice.
	other := w.as(w.outsider).call(t, "amp_create_project", map[string]interface{}{"name": "o", "code": "op-" + sfx})
	if slices.Contains(sliceIDs(t, w.as(w.alice).call(t, "amp_list_projects", map[string]interface{}{}), "projects"), intField(t, other, "id")) {
		t.Fatal("alice can see another org's project")
	}
}

func TestRBAC_WhoAmI(t *testing.T) {
	w := newRBACWorld(t)
	res := w.as(w.lead).call(t, "amp_whoami", map[string]interface{}{})
	if strField(t, res, "email") != w.lead.Email {
		t.Fatalf("whoami %v", res)
	}
	orgs := res["orgs"].([]interface{})
	if len(orgs) != 1 {
		t.Fatalf("lead's orgs %v", orgs)
	}
	members := w.as(w.carol).call(t, "amp_list_project_members", map[string]interface{}{"project_id": float64(w.s.projectID)})
	got := map[string]string{}
	for _, m := range members["members"].([]interface{}) {
		mm := m.(map[string]interface{})
		got[mm["email"].(string)] = mm["access"].(string)
	}
	want := map[string]string{
		w.owner.Email: "owner", w.alice.Email: "member", w.lead.Email: "lead", w.carol.Email: "member", w.orgadmin.Email: "org_admin",
	}
	for email, access := range want {
		if got[email] != access {
			t.Errorf("%s access = %q, want %q", email, got[email], access)
		}
	}
	if _, ok := got[w.dave.Email]; ok {
		t.Error("dave (org member, no team) listed as a project member")
	}
}

// ---- Platform administration over MCP ----

// An admin's agent can run the platform (orgs, teams, members, invites,
// project access) through MCP, held to the same rules as the UI.
func TestRBAC_AdminToolsOverMCP(t *testing.T) {
	w := newRBACWorld(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	newEmail := "newhire-" + sfx + "@rbac.test"

	// Only global admins create orgs.
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_create_org", map[string]interface{}{"name": "Nope " + sfx}), "only global admins")
	res := w.as(w.root).call(t, "amp_create_org", map[string]interface{}{"name": "Platform " + sfx, "owner": w.alice.Email})
	orgID := intField(t, res["org"].(map[string]interface{}), "id")
	orgs := w.as(w.alice).call(t, "amp_list_orgs", map[string]interface{}{})["orgs"].([]interface{})
	if !slices.ContainsFunc(orgs, func(o interface{}) bool {
		m := o.(map[string]interface{})
		return int(m["id"].(float64)) == orgID && m["my_role"] == "owner"
	}) {
		t.Fatalf("alice should own the new org: %v", orgs)
	}

	// alice, as org owner, builds a team and staffs it.
	alice := w.as(w.alice)
	team := alice.call(t, "amp_manage_teams", map[string]interface{}{"action": "create", "org_id": float64(orgID), "name": "Core"})["team"].(map[string]interface{})
	teamID := float64(intField(t, team, "id"))
	alice.call(t, "amp_manage_team_members", map[string]interface{}{"action": "add", "team_id": teamID, "user": w.bob.Email, "role": "lead"})
	inv := alice.call(t, "amp_manage_team_members", map[string]interface{}{"action": "add", "team_id": teamID, "user": newEmail})
	if inv["invited"] != true {
		t.Fatalf("unknown email should be invited: %v", inv)
	}
	invites := alice.call(t, "amp_manage_invites", map[string]interface{}{"action": "list", "org_id": float64(orgID)})["invites"].([]interface{})
	if len(invites) != 1 {
		t.Fatalf("pending invites %v", invites)
	}
	alice.call(t, "amp_manage_invites", map[string]interface{}{"action": "revoke", "org_id": float64(orgID), "invite_id": invites[0].(map[string]interface{})["id"]})

	// bob, a team lead, manages his team but not the org.
	bob := w.as(w.bob)
	bob.call(t, "amp_manage_team_members", map[string]interface{}{"action": "add", "team_id": teamID, "user": w.dave.Email})
	wantErr(t, bob.callExpectError(t, "amp_manage_org_members", map[string]interface{}{"action": "add", "org_id": float64(orgID), "user": w.carol.Email}), "requires org owner or admin")
	wantErr(t, bob.callExpectError(t, "amp_manage_teams", map[string]interface{}{"action": "create", "org_id": float64(orgID), "name": "Rogue"}), "requires org owner or admin")
	members := alice.call(t, "amp_manage_team_members", map[string]interface{}{"action": "list", "team_id": teamID})["members"].([]interface{})
	if len(members) != 2 {
		t.Fatalf("team members %v, want bob and dave", members)
	}

	// Org roles: alice promotes carol to admin; an admin can't grant owner.
	alice.call(t, "amp_manage_org_members", map[string]interface{}{"action": "add", "org_id": float64(orgID), "user": w.carol.Email, "role": "admin"})
	wantErr(t, w.as(w.carol).callExpectError(t, "amp_manage_org_members", map[string]interface{}{"action": "set_role", "org_id": float64(orgID), "user": w.dave.Email, "role": "owner"}), "only org owners")
	// The last owner can't be removed.
	wantErr(t, w.as(w.root).callExpectError(t, "amp_manage_org_members", map[string]interface{}{"action": "remove", "org_id": float64(orgID), "user": w.alice.Email}), "at least one owner")

	// Outsiders can't see the org at all.
	wantErr(t, w.as(w.outsider).callExpectError(t, "amp_manage_org_members", map[string]interface{}{"action": "list", "org_id": float64(orgID)}), "not found")
	wantErr(t, alice.callExpectError(t, "amp_manage_teams", map[string]interface{}{"action": "explode", "org_id": float64(orgID)}), "unknown action")
}

func TestRBAC_ProjectTeamsOverMCP(t *testing.T) {
	w := newRBACWorld(t)
	pid := float64(w.s.projectID)
	teams := w.as(w.carol).call(t, "amp_manage_project_teams", map[string]interface{}{"action": "list", "project_id": pid})["teams"].([]interface{})
	if len(teams) != 3 {
		t.Fatalf("teams %v", teams)
	}
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_manage_project_teams", map[string]interface{}{"action": "unlink", "project_id": pid, "team_id": float64(w.ops.ID)}), "requires owner access")
	w.as(w.owner).call(t, "amp_manage_project_teams", map[string]interface{}{"action": "unlink", "project_id": pid, "team_id": float64(w.ops.ID)})
	wantErr(t, w.as(w.carol).callExpectError(t, "amp_list_tasks", map[string]interface{}{"project_id": pid}), "not found")
	wantErr(t, w.as(w.owner).callExpectError(t, "amp_manage_project_teams", map[string]interface{}{"action": "link", "project_id": pid, "team_id": float64(w.x.ID)}), "different org")
	wantErr(t, w.as(w.owner).callExpectError(t, "amp_manage_project_teams", map[string]interface{}{"action": "unlink", "project_id": pid, "team_id": float64(w.core.ID)}), "at least one owner team")
}
