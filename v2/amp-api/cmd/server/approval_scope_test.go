package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
)

// Who does one person's plan approval cover? (AMP #2197, #2198)
//
// #2197 pinned the old behaviour: approval was one project-wide watermark,
// so one person's click approved everyone's tickets planned before it.
// #2198 (option D) made approval per ticket: each ticket records
// approved_at/approved_by and is approved iff approved_at >= planned_at. A
// click approves the clicker's OWN tickets (owned by them, or unassigned
// and planned by them); anyone else's needs an explicit on_behalf from a
// project lead, owner or admin. Assertions marked "SCOPE:" are the ones
// #2197 marked and #2198 flipped. See architecture/plan-approval.md.

// scopeWorld is the rbac world with plan approval on and one epic/story per
// person: alice (a Member through Devs) and bob (a Member through Devs), each
// running their own manager over MCP. lead leads Devs.
type scopeWorld struct {
	w                 *rbacWorld
	ra                *restAs
	alice, bob        *mcpStack
	aliceStory, bobSt int
	path              string
}

func newScopeWorld(t *testing.T) *scopeWorld {
	t.Helper()
	w := newRBACWorld(t)
	sw := &scopeWorld{w: w, ra: newRestAs(t, w), alice: w.as(w.alice), bob: w.as(w.bob)}
	approvalOn(t, w.as(w.root))
	ctx := context.Background()
	story := func(who string) int {
		e, err := w.s.repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: w.s.projectID, Name: who + "'s epic", Priority: "1"})
		if err != nil {
			t.Fatal(err)
		}
		s, err := w.s.repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: w.s.projectID, EpicID: e.ID, Name: who + "'s story", Priority: "1"})
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	sw.aliceStory, sw.bobSt = story("alice"), story("bob")
	sw.path = fmt.Sprintf("/api/projects/%d/approve-plan", w.s.projectID)
	return sw
}

func (sw *scopeWorld) task(t *testing.T, id int) *domain.Task {
	t.Helper()
	tk, err := sw.w.s.repo.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (sw *scopeWorld) planned(t *testing.T, id int) time.Time { return sw.task(t, id).PlannedAt }

func (sw *scopeWorld) project(t *testing.T) *domain.Project {
	t.Helper()
	p, err := sw.w.s.repo.GetProject(context.Background(), sw.w.s.projectID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// post clicks Approve in the web UI as who with body; it returns the
// response's approved and awaiting_approval ids.
func (sw *scopeWorld) post(t *testing.T, who string, body any) (approved, awaiting []int, out map[string]any) {
	t.Helper()
	out = sw.ra.want(t, http.StatusOK, who, http.MethodPost, sw.path, body)
	return idList(t, out["approved"]), idList(t, out["awaiting_approval"]), out
}

// approve clicks Approve plan as who with only as_of (nil: no body): the
// default, who's own tickets. It returns what still awaits, anyone's.
func (sw *scopeWorld) approve(t *testing.T, who string, asOf *time.Time) []int {
	t.Helper()
	var body any
	if asOf != nil {
		body = map[string]any{"as_of": asOf.Format(time.RFC3339Nano)}
	}
	_, awaiting, _ := sw.post(t, who, body)
	return awaiting
}

func idList(t *testing.T, v any) []int {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list of ids: %#v", v)
	}
	ids := []int{}
	for _, x := range list {
		ids = append(ids, int(x.(float64)))
	}
	return ids
}

func awaitingOf(t *testing.T, s *mcpStack) []int {
	t.Helper()
	ids := idList(t, s.call(t, "amp_check_plan", map[string]any{"project_id": float64(s.projectID)})["awaiting_approval"])
	slices.Sort(ids) // the plan check lists tickets in board order
	return ids
}

// yoursOf is amp_check_plan's awaiting_yours for s's person, sorted.
func yoursOf(t *testing.T, s *mcpStack) []int {
	t.Helper()
	ids := idList(t, s.call(t, "amp_check_plan", map[string]any{"project_id": float64(s.projectID)})["awaiting_yours"])
	slices.Sort(ids)
	return ids
}

func dispatch(s *mcpStack, t *testing.T, id int) string {
	t.Helper()
	return s.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "amp-worker"})
}

// lastApproveBy is the newest plan.approve audit row by user id.
func (sw *scopeWorld) lastApproveBy(t *testing.T, userID int) *domain.AuditEntry {
	t.Helper()
	entries, err := sw.w.s.repo.ListAudit(context.Background(), domain.AuditFilter{ProjectID: sw.w.s.projectID, Action: "plan.approve"})
	if err != nil {
		t.Fatal(err)
	}
	var row *domain.AuditEntry
	for i := range entries {
		if entries[i].ActorUserID != nil && *entries[i].ActorUserID == userID && (row == nil || entries[i].ID > row.ID) {
			row = &entries[i]
		}
	}
	if row == nil {
		t.Fatalf("no plan.approve audit row by user %d in %v", userID, entries)
	}
	return row
}

// approvalHistory is ticket id's plan_approved history entries.
func (sw *scopeWorld) approvalHistory(t *testing.T, id int) []domain.ActivityLog {
	t.Helper()
	hist, err := sw.w.s.repo.GetTicketHistory(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.ActivityLog
	for _, h := range hist {
		if h.Action == "plan_approved" {
			out = append(out, h)
		}
	}
	return out
}

// Two people, each with their own manager, plan in different epics of one
// project. Each person's click approves only their own tickets; a lead
// approves someone else's only on their behalf, and both names are recorded.
func TestApprovalScope_EachPersonApprovesTheirOwn(t *testing.T) {
	sw := newScopeWorld(t)
	w := sw.w
	pid := float64(w.s.projectID)

	// Planned in this order: bob's b0, alice's a1, bob's b1.
	b0 := mkIn(t, sw.bob, sw.bobSt, "bob's first")
	a1 := mkIn(t, sw.alice, sw.aliceStory, "alice's")
	b1 := mkIn(t, sw.bob, sw.bobSt, "bob's second")
	for id, owner := range map[int]int{b0: w.bob.User.ID, a1: w.alice.User.ID, b1: w.bob.User.ID} {
		if got := taskOwner(t, w.s, id); got == nil || *got != owner {
			t.Fatalf("ticket %d owner %v, want %d (the planner owns what their manager creates)", id, got, owner)
		}
	}

	// Before anyone approves, both managers see every awaiting ticket (the
	// list stays project-wide, for compatibility) and the same approve_url;
	// awaiting_yours and awaiting_approval_tickets say whose each one is.
	all := []int{b0, a1, b1}
	for name, s := range map[string]*mcpStack{"alice": sw.alice, "bob": sw.bob} {
		if got := awaitingOf(t, s); !slices.Equal(got, all) {
			t.Fatalf("%s's amp_check_plan awaiting = %v, want every ticket %v", name, got, all)
		}
		r := s.call(t, "amp_check_plan", map[string]any{"project_id": pid})
		if r["approve_url"] != fmt.Sprintf("/project/%d/plan", s.projectID) {
			t.Fatalf("%s's approve_url = %v", name, r["approve_url"])
		}
	}
	if got := yoursOf(t, sw.alice); !slices.Equal(got, []int{a1}) {
		t.Fatalf("alice's awaiting_yours = %v, want [%d]", got, a1)
	}
	if got := yoursOf(t, sw.bob); !slices.Equal(got, []int{b0, b1}) {
		t.Fatalf("bob's awaiting_yours = %v, want [%d %d]", got, b0, b1)
	}
	r := sw.alice.call(t, "amp_check_plan", map[string]any{"project_id": pid})
	for _, v := range r["awaiting_approval_tickets"].([]any) {
		it := v.(map[string]any)
		id, mine := int(it["task_id"].(float64)), it["yours"] == true
		owner := int(it["owner_user_id"].(float64))
		if mine != (id == a1) || (id == a1) != (owner == w.alice.User.ID) {
			t.Fatalf("alice's awaiting_approval_tickets item %v", it)
		}
	}

	// The web UI's awaiting list is built from GET tasks?owner=any, so each
	// person's page still shows the other person's tickets; each ticket now
	// carries its own approved_at/approved_by for the UI to show.
	tasksPath := fmt.Sprintf("/api/projects/%d/tasks", w.s.projectID)
	ids := func(out map[string]any) []int {
		got := []int{}
		for _, v := range out["tasks"].([]any) {
			got = append(got, int(v.(map[string]any)["id"].(float64)))
		}
		slices.Sort(got)
		return got
	}
	for _, who := range []string{"alice", "bob"} {
		if got := ids(sw.ra.want(t, 200, who, "GET", tasksPath+"?owner=any", nil)); !slices.Equal(got, all) {
			t.Fatalf("%s's UI task list (owner=any) = %v, want %v", who, got, all)
		}
	}

	// SCOPE: alice approves the whole page as she sees it (as_of = the newest
	// ticket, b1). Only her own a1 is approved; bob's b0 and b1 still await.
	asOf := sw.planned(t, b1)
	approved, awaiting, out := sw.post(t, "alice", map[string]any{"as_of": asOf.Format(time.RFC3339Nano)})
	if !slices.Equal(approved, []int{a1}) || !slices.Equal(awaiting, []int{b0, b1}) {
		t.Fatalf("alice's click approved %v, awaiting %v; want [%d] and [%d %d]", approved, awaiting, a1, b0, b1)
	}
	if out["on_behalf"] != false || len(idList(t, out["awaiting_yours"])) != 0 {
		t.Fatalf("alice's response: on_behalf %v, awaiting_yours %v", out["on_behalf"], out["awaiting_yours"])
	}
	for _, v := range out["still_awaiting"].([]any) {
		it := v.(map[string]any)
		if it["yours"] != false || int(it["owner_user_id"].(float64)) != w.bob.User.ID {
			t.Fatalf("still_awaiting item %v, want bob's and not alice's", it)
		}
	}
	if p := sw.project(t); p.PlanApprovedBy == nil || *p.PlanApprovedBy != w.alice.User.ID {
		t.Fatalf("plan_approved_by = %v, want alice", p.PlanApprovedBy)
	}
	if tk := sw.task(t, a1); !tk.Approved() || tk.ApprovedBy == nil || *tk.ApprovedBy != w.alice.User.ID {
		t.Fatalf("a1 approved_at %v by %v", tk.ApprovedAt, tk.ApprovedBy)
	}
	for name, s := range map[string]*mcpStack{"alice": sw.alice, "bob": sw.bob} {
		if got := awaitingOf(t, s); !slices.Equal(got, []int{b0, b1}) {
			t.Fatalf("%s's awaiting after alice's approval = %v, want [%d %d]", name, got, b0, b1)
		}
	}

	// SCOPE: bob's dispatch of b0 doesn't pass on alice's click; alice's a1 does.
	wantErr(t, dispatch(sw.bob, t, b0), "approved")
	sw.alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a1), "agent_id": "amp-worker"})
	// Working stays owner-scoped too: alice's manager can't dispatch bob's ticket.
	wantErr(t, dispatch(sw.alice, t, b1), "owned by user")

	// SCOPE: an old UI or script (no body) approves only the caller's own.
	if got := sw.approve(t, "alice", nil); !slices.Equal(got, []int{b0, b1}) {
		t.Fatalf("awaiting after alice's body-less approve = %v", got)
	}
	// Naming bob's ticket without on_behalf is refused, and so is on_behalf
	// from a plain member. Nothing is approved either way.
	code, refused := sw.ra.do(t, "alice", http.MethodPost, sw.path, map[string]any{"task_ids": []int{b0}})
	if code != 403 || !strings.Contains(fmt.Sprint(refused["error"]), "on_behalf") {
		t.Fatalf("alice naming bob's ticket: %d %v", code, refused)
	}
	code, refused = sw.ra.do(t, "alice", http.MethodPost, sw.path, map[string]any{"task_ids": []int{a1, b0}, "on_behalf": true})
	if code != 403 || !strings.Contains(fmt.Sprint(refused["error"]), "lead") {
		t.Fatalf("member approving on behalf: %d %v", code, refused)
	}
	if sw.task(t, b0).ApprovedAt != nil {
		t.Fatal("a refused approval approved b0")
	}

	// The audit row of alice's first click names the ticket she approved and
	// its owner; a1's history says she approved it.
	entries, _ := w.s.repo.ListAudit(context.Background(), domain.AuditFilter{ProjectID: w.s.projectID, Action: "plan.approve"})
	var first *domain.AuditEntry
	for i := range entries {
		if entries[i].ActorUserID != nil && *entries[i].ActorUserID == w.alice.User.ID && len(idList(t, entries[i].Details["approved"])) > 0 {
			first = &entries[i]
		}
	}
	if first == nil || fmt.Sprint(idList(t, first.Details["approved"])) != fmt.Sprint([]int{a1}) || first.Details["on_behalf"] != false ||
		first.Details["channel"] != auth.ChannelUI || first.Details["as_of"] != asOf.UTC().Format(time.RFC3339Nano) ||
		fmt.Sprint(first.Details["owners"]) != fmt.Sprint(map[string]any{fmt.Sprint(a1): float64(w.alice.User.ID)}) {
		t.Fatalf("alice's audit row: %+v", first)
	}
	if h := sw.approvalHistory(t, a1); len(h) != 1 || h[0].Detail != "Plan approved by "+w.alice.User.Email ||
		h[0].ActorUserID == nil || *h[0].ActorUserID != w.alice.User.ID {
		t.Fatalf("a1's approval history: %+v", h)
	}
	if h := sw.approvalHistory(t, b0); len(h) != 0 {
		t.Fatalf("b0 has approval history before anyone approved it: %+v", h)
	}

	// The lead approves bob's b0 on his behalf: recorded with both names.
	approved, _, out = sw.post(t, "lead", map[string]any{"task_ids": []int{b0}, "on_behalf": true})
	if !slices.Equal(approved, []int{b0}) || out["on_behalf"] != true {
		t.Fatalf("lead on behalf: approved %v, on_behalf %v", approved, out["on_behalf"])
	}
	if it := out["approved_tickets"].([]any)[0].(map[string]any); it["on_behalf"] != true || int(it["owner_user_id"].(float64)) != w.bob.User.ID {
		t.Fatalf("approved_tickets[0] = %v", it)
	}
	if tk := sw.task(t, b0); !tk.Approved() || *tk.ApprovedBy != w.lead.User.ID {
		t.Fatalf("b0 approved_by %v, want the lead", tk.ApprovedBy)
	}
	want := "Plan approved on behalf of " + w.bob.User.Email + " by " + w.lead.User.Email
	if h := sw.approvalHistory(t, b0); len(h) != 1 || h[0].Detail != want || *h[0].ActorUserID != w.lead.User.ID {
		t.Fatalf("b0's approval history: %+v, want %q", h, want)
	}
	row := sw.lastApproveBy(t, w.lead.User.ID)
	if row.Details["on_behalf"] != true || fmt.Sprint(row.Details["on_behalf_of"]) != fmt.Sprint([]any{w.bob.User.Email}) ||
		fmt.Sprint(idList(t, row.Details["approved"])) != fmt.Sprint([]int{b0}) || row.Details["approved_by"] != float64(w.lead.User.ID) {
		t.Fatalf("lead's audit row: %v", row.Details)
	}
	sw.bob.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b0), "agent_id": "amp-worker"})
	wantErr(t, dispatch(sw.bob, t, b1), "approved")

	// bob approves his own b1.
	if got := sw.approve(t, "bob", nil); len(got) != 0 {
		t.Fatalf("awaiting after bob approved his own = %v", got)
	}

	// SCOPE: a re-scoped ticket awaits again; alice's click doesn't approve
	// it, bob's does.
	sw.bob.call(t, "amp_update_task", map[string]any{"task_id": float64(b1), "description": "bigger"})
	if got := awaitingOf(t, sw.alice); !slices.Equal(got, []int{b1}) {
		t.Fatalf("alice's awaiting after bob re-scoped b1 = %v", got)
	}
	if got := sw.approve(t, "alice", nil); !slices.Equal(got, []int{b1}) {
		t.Fatalf("alice's click approved bob's re-scoped b1: awaiting %v", got)
	}
	sw.approve(t, "bob", nil)
	sw.bob.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b1), "agent_id": "amp-worker"})

	// A lead's one-click team approval: on_behalf with no task_ids approves
	// every awaiting ticket, whoever's.
	a2 := mkIn(t, sw.alice, sw.aliceStory, "alice's second")
	b2 := mkIn(t, sw.bob, sw.bobSt, "bob's third")
	approved, awaiting, _ = sw.post(t, "lead", map[string]any{"on_behalf": true})
	if !slices.Equal(approved, []int{a2, b2}) || len(awaiting) != 0 {
		t.Fatalf("lead's team approval: approved %v, awaiting %v", approved, awaiting)
	}
}

// A stale tab no longer takes the credit: a click that approves nothing
// leaves the project's last approver alone, and an already-approved ticket
// keeps the person who approved it.
func TestApprovalScope_StaleClickKeepsTheApprover(t *testing.T) {
	sw := newScopeWorld(t)
	a1 := mkIn(t, sw.alice, sw.aliceStory, "alice's")
	b1 := mkIn(t, sw.bob, sw.bobSt, "bob's")
	bobSaw, aliceSaw := sw.planned(t, a1), sw.planned(t, b1)

	sw.approve(t, "alice", &aliceSaw)
	before := sw.project(t)

	// bob's tab loaded before b1 existed: his as_of is older, so his own b1
	// isn't covered and he approves nothing.
	if got := sw.approve(t, "bob", &bobSaw); !slices.Equal(got, []int{b1}) {
		t.Fatalf("awaiting after bob's stale click = %v, want [%d]", got, b1)
	}
	after := sw.project(t)
	if !after.PlanApprovedAt.Equal(*before.PlanApprovedAt) {
		t.Fatalf("stale click moved plan_approved_at %v -> %v", before.PlanApprovedAt, after.PlanApprovedAt)
	}
	// SCOPE (was the attribution bug): plan_approved_by stays alice.
	if after.PlanApprovedBy == nil || *after.PlanApprovedBy != sw.w.alice.User.ID {
		t.Fatalf("plan_approved_by after a stale click = %v, want alice", after.PlanApprovedBy)
	}
	// A lead re-approving alice's ticket on her behalf skips it: it's
	// already approved, and stays alice's approval.
	approved, _, out := sw.post(t, "lead", map[string]any{"task_ids": []int{a1}, "on_behalf": true})
	if len(approved) != 0 || !strings.Contains(fmt.Sprint(out["skipped"]), "already approved") {
		t.Fatalf("lead re-approving a1: approved %v, skipped %v", approved, out["skipped"])
	}
	if tk := sw.task(t, a1); *tk.ApprovedBy != sw.w.alice.User.ID {
		t.Fatalf("a1 approved_by %v, want alice", tk.ApprovedBy)
	}
}

// One person, two agent sessions (two MCP clients under the same login). The
// server can't tell the sessions apart: same user, same channel, no session
// id. They share their person's approval: tickets from both sessions are
// the person's own, and one click approves them together.
func TestApprovalScope_OnePersonTwoSessions(t *testing.T) {
	sw := newScopeWorld(t)
	s1, s2 := sw.w.as(sw.w.alice), sw.w.as(sw.w.alice)

	x1 := mkIn(t, s1, sw.aliceStory, "session 1's")
	y1 := mkIn(t, s2, sw.bobSt, "session 2's") // another epic
	for i, s := range []*mcpStack{s1, s2} {
		if got := yoursOf(t, s); !slices.Equal(got, []int{x1, y1}) {
			t.Fatalf("session %d awaiting_yours = %v, want both sessions' [%d %d]", i+1, got, x1, y1)
		}
	}
	seen := sw.planned(t, x1)
	if got := sw.approve(t, "alice", &seen); !slices.Equal(got, []int{y1}) {
		t.Fatalf("awaiting after approving as of x1 = %v, want [%d]", got, y1)
	}
	// The gate is per ticket, not per session: neither session can dispatch
	// y1, and session 2 can dispatch session 1's x1 (same owner).
	wantErr(t, dispatch(s1, t, y1), "approved")
	wantErr(t, dispatch(s2, t, y1), "approved")
	s2.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(x1), "agent_id": "amp-worker"})

	if got := sw.approve(t, "alice", nil); len(got) != 0 {
		t.Fatalf("awaiting after alice approved her own = %v", got)
	}
	s1.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(y1), "agent_id": "amp-worker"})
}

// Unassigned tickets: approving one is approving your own when you planned
// it, and on behalf otherwise.
func TestApprovalScope_UnassignedTickets(t *testing.T) {
	sw := newScopeWorld(t)
	unassigned := func(s *mcpStack, story int, name string) int {
		return taskID(t, s.call(t, "amp_create_task", map[string]any{
			"project_id": float64(s.projectID), "story_id": float64(story), "name": name,
			"description": "d", "acceptance_criteria": "a", "owner": "none",
		}))
	}
	mine := unassigned(sw.alice, sw.aliceStory, "alice planned it, nobody owns it")
	theirs := unassigned(sw.bob, sw.bobSt, "bob planned it, nobody owns it")
	if o := taskOwner(t, sw.w.s, mine); o != nil {
		t.Fatalf("ticket %d owned by %v, want unassigned", mine, o)
	}
	if got := yoursOf(t, sw.alice); !slices.Equal(got, []int{mine}) {
		t.Fatalf("alice's awaiting_yours = %v, want [%d]", got, mine)
	}
	approved, awaiting, _ := sw.post(t, "alice", nil)
	if !slices.Equal(approved, []int{mine}) || !slices.Equal(awaiting, []int{theirs}) {
		t.Fatalf("alice approved %v, awaiting %v", approved, awaiting)
	}
	if code, _ := sw.ra.do(t, "alice", http.MethodPost, sw.path, map[string]any{"task_ids": []int{theirs}}); code != 403 {
		t.Fatalf("alice approving bob's unassigned ticket without on_behalf: %d", code)
	}
	approved, _, _ = sw.post(t, "lead", map[string]any{"task_ids": []int{theirs}, "on_behalf": true})
	if !slices.Equal(approved, []int{theirs}) {
		t.Fatalf("lead on behalf of bob's unassigned ticket: %v", approved)
	}
	if h := sw.approvalHistory(t, theirs); len(h) != 1 || !strings.Contains(h[0].Detail, "on behalf of "+sw.w.bob.User.Email+" (unassigned") {
		t.Fatalf("history: %+v", h)
	}
}

// Who may click Approve plan: any person with at least Member access to the
// project, in the web UI, for their own tickets. Approving on someone's
// behalf takes lead access or above (team lead, project owner, org admin,
// global admin). Someone who can't see the project gets 404, and nobody
// approves over MCP or with an API token.
func TestApprovalScope_WhoCanApprove(t *testing.T) {
	sw := newScopeWorld(t)
	ids := sw.ra.ids
	root := sw.w.as(sw.w.root)
	for _, who := range []string{"alice", "bob", "carol", "lead", "owner", "orgadmin", "root"} {
		own := mkIn(t, sw.w.as(ids[who]), sw.w.s.storyID, "for "+who)
		if approved, _, _ := sw.post(t, who, nil); !slices.Equal(approved, []int{own}) {
			t.Fatalf("%s approved %v, want their own [%d]", who, approved, own)
		}
	}
	for _, who := range []string{"alice", "bob", "carol"} {
		id := mkIn(t, root, sw.w.s.storyID, "root's, for "+who+" to try")
		code, out := sw.ra.do(t, who, http.MethodPost, sw.path, map[string]any{"task_ids": []int{id}, "on_behalf": true})
		if code != 403 || !strings.Contains(fmt.Sprint(out["error"]), "lead") {
			t.Fatalf("member %s approving on behalf: %d %v", who, code, out)
		}
		sw.post(t, "root", nil) // root approves their own, to keep the plan clean
	}
	for _, who := range []string{"lead", "owner", "orgadmin"} {
		id := mkIn(t, root, sw.w.s.storyID, "root's, for "+who)
		if approved, _, _ := sw.post(t, who, map[string]any{"task_ids": []int{id}, "on_behalf": true}); !slices.Equal(approved, []int{id}) {
			t.Fatalf("%s on behalf approved %v, want [%d]", who, approved, id)
		}
	}
	// Unknown tickets are a 400, and nothing is approved.
	id := mkIn(t, root, sw.w.s.storyID, "nobody below may approve this")
	if code, out := sw.ra.do(t, "root", http.MethodPost, sw.path, map[string]any{"task_ids": []int{id, 999999}}); code != 400 ||
		!strings.Contains(fmt.Sprint(out["error"]), "999999") {
		t.Fatalf("unknown ticket: %d %v", code, out)
	}
	for _, who := range []string{"dave", "outsider"} {
		sw.ra.want(t, http.StatusNotFound, who, http.MethodPost, sw.path, nil)
	}
	for _, who := range []string{"alice", "owner", "root"} {
		if code, _ := sw.ra.doVia(t, auth.ChannelToken, who, http.MethodPost, sw.path, nil); code != http.StatusForbidden {
			t.Fatalf("%s approving with an API token: %d, want 403", who, code)
		}
	}
	for _, s := range []*mcpStack{sw.alice, root} {
		wantErr(t, s.callExpectError(t, "amp_approve_plan", map[string]any{"project_id": float64(s.projectID)}), "web UI")
	}
	if got := awaitingOf(t, sw.alice); !slices.Equal(got, []int{id}) {
		t.Fatalf("refused approvals changed the plan: awaiting %v, want [%d]", got, id)
	}
}

// An approval is for one person (AMP #2218): whose ticket it was when it was
// approved (approved_for). When a Member moves the ticket to someone else,
// the approval is dropped, so nobody can approve another person's ticket by
// claiming or reassigning it, approving it as their own and handing it back,
// nor hand their approved ticket to someone who never approved it. A project
// lead's reassignment keeps it (a lead may approve on anyone's behalf), and
// so does dispatch's automatic claim of an unassigned ticket.
func TestApprovalScope_ApprovalDoesNotSurviveAMembersReassignment(t *testing.T) {
	sw := newScopeWorld(t)
	w := sw.w
	alice, bob := w.alice.User.ID, w.bob.User.ID
	assign := func(s *mcpStack, id int, user string) {
		t.Helper()
		s.call(t, "amp_assign_task", map[string]any{"task_id": float64(id), "user": user})
	}
	approveOwn := func(who string, id int) {
		t.Helper()
		if approved, _, _ := sw.post(t, who, map[string]any{"task_ids": []int{id}}); !slices.Equal(approved, []int{id}) {
			t.Fatalf("%s approving ticket %d as their own approved %v", who, id, approved)
		}
	}
	unassigned := func(s *mcpStack, story int, name string) int {
		return taskID(t, s.call(t, "amp_create_task", map[string]any{
			"project_id": float64(s.projectID), "story_id": float64(story), "name": name,
			"description": "d", "acceptance_criteria": "a", "owner": "none",
		}))
	}
	approvedFor := func(id int, want *int) {
		t.Helper()
		tk := sw.task(t, id)
		if !tk.Approved() || !sameUserPtr(tk.ApprovedFor, want) {
			t.Fatalf("ticket %d: approved=%v approved_for=%v, want approved for %v", id, tk.Approved(), tk.ApprovedFor, want)
		}
	}
	awaits := func(id int) {
		t.Helper()
		tk := sw.task(t, id)
		if tk.Approved() || tk.ApprovedAt != nil || tk.ApprovedBy != nil || tk.ApprovedFor != nil {
			t.Fatalf("ticket %d should await approval again: approved_at=%v by=%v for=%v", id, tk.ApprovedAt, tk.ApprovedBy, tk.ApprovedFor)
		}
		if !slices.Contains(awaitingOf(t, sw.alice), id) {
			t.Fatalf("ticket %d missing from amp_check_plan awaiting_approval", id)
		}
		dropped := false
		hist, _ := w.s.repo.GetTicketHistory(context.Background(), id)
		for _, h := range hist {
			dropped = dropped || (h.Action == "approval_dropped" && strings.Contains(h.Detail, "Plan approval dropped"))
		}
		if !dropped {
			t.Fatalf("ticket %d: no approval_dropped history entry in %+v", id, hist)
		}
	}
	restDispatch := func(who string, id int) (int, map[string]any) {
		return sw.ra.do(t, who, http.MethodPost, fmt.Sprintf("/api/tasks/%d/dispatch", id), map[string]any{"agent_id": "amp-worker"})
	}

	// (a) bob planned an unassigned ticket. alice claims it, approves it as
	// her own, and releases it: it's bob's again (he planned it), and it awaits.
	a := unassigned(sw.bob, sw.bobSt, "bob planned it, unassigned")
	sw.alice.call(t, "amp_claim_task", map[string]any{"task_id": float64(a)})
	approveOwn("alice", a)
	approvedFor(a, &alice)
	assign(sw.alice, a, "none")
	awaits(a)
	if !slices.Contains(yoursOf(t, sw.bob), a) {
		t.Fatalf("ticket %d should await bob (he planned it)", a)
	}
	if code, out := restDispatch("bob", a); code != http.StatusForbidden || !strings.Contains(fmt.Sprint(out["error"]), "isn't approved") {
		t.Fatalf("dispatching the released ticket: %d %v, want 403 isn't approved", code, out)
	}
	wantErr(t, dispatch(sw.bob, t, a), "isn't approved")

	// (b) open_reassign (the default): alice takes bob's idle ticket, approves
	// it, and gives it back. It awaits bob.
	b := mkIn(t, sw.bob, sw.bobSt, "bob's own")
	assign(sw.alice, b, "me")
	approveOwn("alice", b)
	assign(sw.alice, b, w.bob.Email)
	if o := taskOwner(t, w.s, b); o == nil || *o != bob {
		t.Fatalf("ticket %d owner %v, want bob", b, o)
	}
	awaits(b)
	wantErr(t, dispatch(sw.bob, t, b), "isn't approved")

	// (c) alice approves her own ticket, then hands it to bob, who never
	// approved it. It awaits bob; once he approves it, it's approved for him.
	c := mkIn(t, sw.alice, sw.aliceStory, "alice's, handed to bob")
	approveOwn("alice", c)
	assign(sw.alice, c, w.bob.Email)
	awaits(c)
	if !slices.Contains(yoursOf(t, sw.bob), c) {
		t.Fatalf("ticket %d should await bob", c)
	}
	approveOwn("bob", c)
	approvedFor(c, &bob)

	// Handing a ticket back to the person it was approved for keeps it, and so
	// does releasing your own ticket (it stays yours: you planned it).
	own := mkIn(t, sw.alice, sw.aliceStory, "alice's, released")
	approveOwn("alice", own)
	assign(sw.alice, own, "none")
	approvedFor(own, &alice)
	assign(sw.alice, own, "me")
	approvedFor(own, &alice)

	// (d) A lead's reassignment keeps the approval (approved for whom it was).
	d := mkIn(t, sw.alice, sw.aliceStory, "alice's, moved by the lead")
	approveOwn("alice", d)
	assign(w.as(w.lead), d, w.bob.Email)
	if o := taskOwner(t, w.s, d); o == nil || *o != bob {
		t.Fatalf("ticket %d owner %v, want bob", d, o)
	}
	approvedFor(d, &alice)
	if slices.Contains(yoursOf(t, sw.bob), d) {
		t.Fatalf("ticket %d moved by a lead should not await bob", d)
	}
	// A lead's handover keeps it too.
	h := mkIn(t, sw.alice, sw.aliceStory, "alice's, handed over by the lead")
	approveOwn("alice", h)
	w.as(w.lead).call(t, "amp_handover_tasks", map[string]any{"project_id": float64(w.s.projectID),
		"from": w.alice.Email, "to": w.bob.Email, "task_ids": []any{float64(h)}})
	if o := taskOwner(t, w.s, h); o == nil || *o != bob {
		t.Fatalf("ticket %d owner %v after the lead's handover, want bob", h, o)
	}
	approvedFor(h, &alice)
	sw.bob.finish(t, d)

	// (e) Dispatch's automatic claim of an approved unassigned ticket keeps
	// it approved, and the ticket can be completed.
	e := unassigned(sw.bob, sw.bobSt, "bob planned it, alice's agent runs it")
	approveOwn("bob", e)
	sw.alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(e), "agent_id": "amp-worker"})
	if o := taskOwner(t, w.s, e); o == nil || *o != alice {
		t.Fatalf("ticket %d owner %v after dispatch, want alice (claimed on dispatch)", e, o)
	}
	approvedFor(e, &bob)
	sw.alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(e)})
	if tk := sw.task(t, e); tk.State != domain.TaskStateCompleted {
		t.Fatalf("ticket %d state %s, want completed", e, tk.State)
	}
}

func sameUserPtr(x, y *int) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	return *x == *y
}
