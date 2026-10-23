package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
)

// Who does one person's plan approval cover? (AMP #2197)
//
// These tests pin the CURRENT behaviour of plan approval when two people (or
// one person with two agent sessions) plan work in the same project. Approval
// is one project-wide watermark (projects.plan_approved_at): every open ticket
// whose planned_at is at or before it is approved, whoever planned or owns it.
// Nothing is scoped to the approver, an owner, an epic or a session.
//
// Assertions marked "SCOPE:" encode behaviour the user may change (see
// architecture/plan-approval.md, "Who an approval covers"). If approval
// becomes scoped, flip those assertions; the rest should keep holding.

// scopeWorld is the rbac world with plan approval on and one epic/story per
// person: alice (a Member through Devs) and bob (a Member through Devs), each
// running their own manager over MCP.
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

func (sw *scopeWorld) planned(t *testing.T, id int) time.Time {
	t.Helper()
	tk, err := sw.w.s.repo.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk.PlannedAt
}

func (sw *scopeWorld) project(t *testing.T) *domain.Project {
	t.Helper()
	p, err := sw.w.s.repo.GetProject(context.Background(), sw.w.s.projectID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// approve clicks Approve plan in the web UI as who; asOf nil sends no body.
func (sw *scopeWorld) approve(t *testing.T, who string, asOf *time.Time) []int {
	t.Helper()
	var body any
	if asOf != nil {
		body = map[string]any{"as_of": asOf.Format(time.RFC3339Nano)}
	}
	out := sw.ra.want(t, http.StatusOK, who, http.MethodPost, sw.path, body)
	ids := []int{}
	for _, v := range out["awaiting_approval"].([]any) {
		ids = append(ids, int(v.(float64)))
	}
	return ids
}

func awaitingOf(t *testing.T, s *mcpStack) []int {
	t.Helper()
	r := s.call(t, "amp_check_plan", map[string]any{"project_id": float64(s.projectID)})
	ids := []int{}
	for _, v := range r["awaiting_approval"].([]any) {
		ids = append(ids, int(v.(float64)))
	}
	slices.Sort(ids) // the plan check lists tickets in board order
	return ids
}

func dispatch(s *mcpStack, t *testing.T, id int) string {
	t.Helper()
	return s.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "amp-worker"})
}

// Two people, each with their own manager, plan in different epics of one
// project. One person's click approves the other's tickets whenever they were
// planned before the approval's as_of.
func TestApprovalScope_TwoPeopleShareOneApproval(t *testing.T) {
	sw := newScopeWorld(t)
	w, ctx := sw.w, context.Background()

	// Planned in this order: bob's b0, alice's a1, bob's b1.
	b0 := mkIn(t, sw.bob, sw.bobSt, "bob's first")
	a1 := mkIn(t, sw.alice, sw.aliceStory, "alice's")
	b1 := mkIn(t, sw.bob, sw.bobSt, "bob's second")
	for id, owner := range map[int]int{b0: w.bob.User.ID, a1: w.alice.User.ID, b1: w.bob.User.ID} {
		if got := taskOwner(t, w.s, id); got == nil || *got != owner {
			t.Fatalf("ticket %d owner %v, want %d (the planner owns what their manager creates)", id, got, owner)
		}
	}

	// Before anyone approves, both managers see the same project-wide list,
	// every ticket in it (not just their own), and the same approve_url.
	all := []int{b0, a1, b1}
	for name, s := range map[string]*mcpStack{"alice": sw.alice, "bob": sw.bob} {
		if got := awaitingOf(t, s); !slices.Equal(got, all) {
			t.Fatalf("%s's amp_check_plan awaiting = %v, want every ticket %v", name, got, all)
		}
		r := s.call(t, "amp_check_plan", map[string]any{"project_id": float64(s.projectID)})
		if r["approve_url"] != fmt.Sprintf("/project/%d/plan", s.projectID) {
			t.Fatalf("%s's approve_url = %v", name, r["approve_url"])
		}
	}

	// The web UI's awaiting list is built from GET tasks?owner=any (the board
	// and Flight plan ask for every ticket), so each person's banner lists the
	// other person's tickets too. Only the server's default view ("mine")
	// narrows to the caller's own.
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
	if got := ids(sw.ra.want(t, 200, "alice", "GET", tasksPath, nil)); !slices.Equal(got, []int{a1}) {
		t.Fatalf("alice's default task view = %v, want only [%d]", got, a1)
	}

	// SCOPE: alice approves as of her own ticket. bob's b0, planned earlier,
	// is approved by alice's click; bob's b1, planned later, still awaits.
	asOf := sw.planned(t, a1)
	if got := sw.approve(t, "alice", &asOf); !slices.Equal(got, []int{b1}) {
		t.Fatalf("awaiting after alice approved as of a1 = %v, want [%d]", got, b1)
	}
	if p := sw.project(t); p.PlanApprovedBy == nil || *p.PlanApprovedBy != w.alice.User.ID {
		t.Fatalf("plan_approved_by = %v, want alice", p.PlanApprovedBy)
	}
	// Both managers now see only b1 awaiting.
	for name, s := range map[string]*mcpStack{"alice": sw.alice, "bob": sw.bob} {
		if got := awaitingOf(t, s); !slices.Equal(got, []int{b1}) {
			t.Fatalf("%s's awaiting after alice's approval = %v, want [%d]", name, got, b1)
		}
	}

	// SCOPE: bob's dispatch of b0 now passes on alice's approval; b1 is held.
	sw.bob.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b0), "agent_id": "amp-worker"})
	wantErr(t, dispatch(sw.bob, t, b1), "approved")

	// Approval is shared, but working stays owner-scoped: alice's manager
	// can't dispatch bob's ticket (the plan gate passes, ownership refuses).
	wantErr(t, dispatch(sw.alice, t, b1), "owned by user")

	// The audit row names alice, her channel and as_of, and what still awaits;
	// it doesn't list which tickets her click approved (b0 and a1), and b0's
	// own history says nothing about the approval.
	entries, err := w.s.repo.ListAudit(ctx, domain.AuditFilter{ProjectID: w.s.projectID, Action: "plan.approve"})
	if err != nil {
		t.Fatal(err)
	}
	var row *domain.AuditEntry
	for i := range entries {
		if entries[i].ActorUserID != nil && *entries[i].ActorUserID == w.alice.User.ID {
			row = &entries[i]
		}
	}
	if row == nil {
		t.Fatalf("no plan.approve audit row by alice in %v", entries)
	}
	if row.Details["channel"] != auth.ChannelUI || row.Details["as_of"] != asOf.UTC().Format(time.RFC3339Nano) ||
		fmt.Sprint(row.Details["still_awaiting"]) != fmt.Sprint([]any{float64(b1)}) {
		t.Fatalf("audit details = %v", row.Details)
	}
	if _, listed := row.Details["approved"]; listed {
		t.Fatalf("audit now lists approved tickets (update this test and the KB): %v", row.Details)
	}
	hist, err := w.s.repo.GetTicketHistory(ctx, b0)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hist {
		if h.ActorUserID != nil && *h.ActorUserID == w.alice.User.ID {
			t.Fatalf("b0's history has an entry by alice (%v): update this test and the KB", h)
		}
	}

	// SCOPE: a click on the Flight plan as it stands (as_of = the newest
	// ticket on the page, which shows everyone's tickets) approves bob's b1
	// too, and so does an approve with no body (old UI, scripts).
	b2 := mkIn(t, sw.bob, sw.bobSt, "bob's third")
	newest := sw.planned(t, b2)
	if got := sw.approve(t, "alice", &newest); len(got) != 0 {
		t.Fatalf("awaiting after alice approved the whole page = %v", got)
	}
	sw.bob.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b2), "agent_id": "amp-worker"})
	b3 := mkIn(t, sw.bob, sw.bobSt, "bob's fourth")
	if got := sw.approve(t, "alice", nil); len(got) != 0 {
		t.Fatalf("awaiting after a body-less approve = %v (b3 %d)", got, b3)
	}

	// SCOPE: a re-scoped ticket goes back to awaiting for everyone, and the
	// next click by either person approves it.
	sw.bob.call(t, "amp_update_task", map[string]any{"task_id": float64(b3), "description": "bigger"})
	if got := awaitingOf(t, sw.alice); !slices.Equal(got, []int{b3}) {
		t.Fatalf("alice's awaiting after bob re-scoped b3 = %v", got)
	}
	again := sw.planned(t, b3)
	sw.approve(t, "alice", &again)
	sw.bob.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b3), "agent_id": "amp-worker"})
}

// A stale tab can't move the approval back, but it does take the credit:
// plan_approved_by becomes the stale clicker while plan_approved_at keeps
// the other person's later approval.
func TestApprovalScope_StaleClickTakesTheCredit(t *testing.T) {
	sw := newScopeWorld(t)
	a1 := mkIn(t, sw.alice, sw.aliceStory, "alice's")
	b1 := mkIn(t, sw.bob, sw.bobSt, "bob's")
	bobSaw, aliceSaw := sw.planned(t, a1), sw.planned(t, b1)

	sw.approve(t, "alice", &aliceSaw)
	before := sw.project(t)

	// bob's tab loaded before b1 existed: his as_of is older.
	if got := sw.approve(t, "bob", &bobSaw); len(got) != 0 {
		t.Fatalf("awaiting after bob's stale click = %v", got)
	}
	after := sw.project(t)
	if !after.PlanApprovedAt.Equal(*before.PlanApprovedAt) {
		t.Fatalf("stale click moved plan_approved_at %v -> %v", before.PlanApprovedAt, after.PlanApprovedAt)
	}
	// SCOPE (attribution bug): the approval that covers b1 is alice's, yet
	// plan_approved_by now says bob ("Last approved ... by bob" in the UI).
	if after.PlanApprovedBy == nil || *after.PlanApprovedBy != sw.w.bob.User.ID {
		t.Fatalf("plan_approved_by after a stale click = %v, want bob (current behaviour)", after.PlanApprovedBy)
	}
}

// One person, two agent sessions (two MCP clients under the same login). The
// server can't tell the sessions apart: same user, same channel, no session
// id. Approval and dispatch treat them as one person.
func TestApprovalScope_OnePersonTwoSessions(t *testing.T) {
	sw := newScopeWorld(t)
	s1, s2 := sw.w.as(sw.w.alice), sw.w.as(sw.w.alice)

	x1 := mkIn(t, s1, sw.aliceStory, "session 1's")
	y1 := mkIn(t, s2, sw.bobSt, "session 2's") // another epic
	seen := sw.planned(t, x1)
	if got := sw.approve(t, "alice", &seen); !slices.Equal(got, []int{y1}) {
		t.Fatalf("awaiting after approving as of x1 = %v, want [%d]", got, y1)
	}
	for i, s := range []*mcpStack{s1, s2} {
		if got := awaitingOf(t, s); !slices.Equal(got, []int{y1}) {
			t.Fatalf("session %d awaiting = %v, want [%d]", i+1, got, y1)
		}
	}
	// The gate is per ticket (planned_at), not per session: neither session
	// can dispatch y1, and session 2 can dispatch session 1's x1 (same owner).
	wantErr(t, dispatch(s1, t, y1), "approved")
	wantErr(t, dispatch(s2, t, y1), "approved")
	s2.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(x1), "agent_id": "amp-worker"})

	newest := sw.planned(t, y1)
	sw.approve(t, "alice", &newest)
	s1.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(y1), "agent_id": "amp-worker"})
}

// Who may click Approve plan: any person with at least Member access to the
// project, in the web UI. AMP has no read-only (viewer) project role: the
// lowest level that can see the project (Member) can approve. Someone who
// can't see the project (an org member on no linked team, an outsider) gets
// 404, and nobody approves over MCP or with an API token.
func TestApprovalScope_WhoCanApprove(t *testing.T) {
	sw := newScopeWorld(t)
	for _, who := range []string{"alice", "bob", "carol", "lead", "owner", "orgadmin", "root"} {
		mkIn(t, sw.w.as(sw.w.root), sw.w.s.storyID, "for "+who)
		if got := sw.approve(t, who, nil); len(got) != 0 {
			t.Fatalf("%s approved but %v still await", who, got)
		}
	}
	id := mkIn(t, sw.w.as(sw.w.root), sw.w.s.storyID, "nobody below may approve this")
	for _, who := range []string{"dave", "outsider"} {
		sw.ra.want(t, http.StatusNotFound, who, http.MethodPost, sw.path, nil)
	}
	for _, who := range []string{"alice", "owner", "root"} {
		if code, _ := sw.ra.doVia(t, auth.ChannelToken, who, http.MethodPost, sw.path, nil); code != http.StatusForbidden {
			t.Fatalf("%s approving with an API token: %d, want 403", who, code)
		}
	}
	for _, s := range []*mcpStack{sw.alice, sw.w.as(sw.w.root)} {
		wantErr(t, s.callExpectError(t, "amp_approve_plan", map[string]any{"project_id": float64(s.projectID)}), "web UI")
	}
	if got := awaitingOf(t, sw.alice); !slices.Equal(got, []int{id}) {
		t.Fatalf("refused approvals changed the plan: awaiting %v, want [%d]", got, id)
	}
}
