package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// Human-only approval (AMP epic 162). Only a person in the web UI (REST,
// with a token issued to the UI's client) approves a plan; agents over MCP,
// API tokens and other tokens on REST are refused with the Flight plan link.

func planPath(projectID int) string { return fmt.Sprintf("/project/%d/plan", projectID) }

func approvalOn(t *testing.T, s *mcpStack) {
	t.Helper()
	// Turning approval on is allowed from anywhere, MCP included.
	s.call(t, "amp_update_project_settings", map[string]any{"project_id": float64(s.projectID), "require_plan_approval": true})
}

// New projects require plan approval.
func TestPlanApproval_OnByDefault(t *testing.T) {
	w := newRBACWorld(t)
	ts := time.Now().UnixNano()
	p, err := w.s.repo.CreateProject(context.Background(), domain.CreateProjectRequest{Name: fmt.Sprintf("default %d", ts), Code: fmt.Sprintf("def-%d", ts)})
	if err != nil {
		t.Fatal(err)
	}
	if !p.RequirePlanApproval {
		t.Fatal("a new project doesn't require plan approval")
	}
}

// Tickets planned since the last approval wait for a person in the web UI.
// No agent path can approve: amp_approve_plan stays in the catalog but always
// refuses with approve_url; REST refuses anything that isn't the UI.
func TestPlanApproval(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	approvalOn(t, root)
	a := mkIn(t, root, w.s.storyID, "first")
	pid := w.s.projectID

	check := root.call(t, "amp_check_plan", map[string]any{"project_id": float64(pid)})
	if !containsID(check["awaiting_approval"], a) || check["require_plan_approval"] != true {
		t.Fatalf("never-approved plan: %v / %v", check["awaiting_approval"], check["require_plan_approval"])
	}
	if u, _ := check["approve_url"].(string); !strings.HasSuffix(u, planPath(pid)) {
		t.Fatalf("approve_url = %q", check["approve_url"])
	}

	// Dispatch refuses, naming the page where a person approves.
	wantErr(t, root.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "w"}), "approved", planPath(pid))

	// amp_approve_plan: in the catalog, same input, never approves, even for a global admin.
	msg := root.callExpectError(t, "amp_approve_plan", map[string]any{"project_id": float64(pid)})
	wantErr(t, msg, "web UI", "approve_url", planPath(pid), "awaiting_approval", "require_plan_approval")
	// Nor does REST from anything but the UI: an API token, or an MCP-audience token.
	for _, ch := range []string{auth.ChannelToken, auth.ChannelRESTOther, auth.ChannelMCP} {
		code, out := ra.doVia(t, ch, "root", http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", pid), nil)
		if code != 403 || !strings.Contains(fmt.Sprint(out["error"]), planPath(pid)) {
			t.Fatalf("approve over REST via %s: %d %v", ch, code, out)
		}
	}
	// The service refuses an in-process caller with no channel, too.
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	if _, err := svc.ApprovePlan(context.Background(), authz.Principal{UserID: w.bob.User.ID, Email: w.bob.User.Email}, pid); authz.StatusOf(err, 0) != 403 {
		t.Fatalf("channel-less approve: %v", err)
	}
	if p, _ := w.s.repo.GetProject(context.Background(), pid); p.PlanApprovedAt != nil {
		t.Fatalf("plan approved by a refused call at %v", p.PlanApprovedAt)
	}

	// A person approves in the web UI; now it dispatches. The audit row says
	// who and how. Approval is per ticket (#2198): with no body a person
	// approves their own tickets, so root (who planned a) approves it.
	ra.want(t, 200, "root", http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", pid), nil)
	check = root.call(t, "amp_check_plan", map[string]any{"project_id": float64(pid)})
	if len(check["awaiting_approval"].([]any)) != 0 || check["plan_approved_at"] == nil || check["approve_url"] != nil {
		t.Fatalf("after approval: %v", check)
	}
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "w"})
	entries, err := w.s.repo.ListAudit(context.Background(), domain.AuditFilter{ProjectID: pid, Action: "plan.approve"})
	if err != nil || len(entries) != 1 {
		t.Fatalf("plan.approve audit: %v %v", entries, err)
	}
	if e := entries[0]; e.ActorEmail != w.root.User.Email || e.Details["channel"] != auth.ChannelUI ||
		fmt.Sprint(e.Details["approved"]) != fmt.Sprint([]any{float64(a)}) {
		t.Fatalf("audit = %+v", e)
	}

	// Any person on the project can approve (their own tickets) in the UI;
	// someone outside it can't.
	for _, who := range []string{"bob", "owner", "lead", "orgadmin"} {
		ra.want(t, 200, who, http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", pid), nil)
	}
	if code, _ := ra.do(t, "outsider", http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", pid), nil); code != 403 && code != 404 {
		t.Fatalf("outsider approved: %d", code)
	}
}

// require_plan_approval: agents may turn it on, only the web UI turns it off.
func TestPlanApproval_OnlyTheUITurnsItOff(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	pid := w.s.projectID
	approvalOn(t, root)
	wantErr(t, root.callExpectError(t, "amp_update_project_settings", map[string]any{"project_id": float64(pid), "require_plan_approval": false}),
		"web UI", fmt.Sprintf("/project/%d/settings", pid))
	path := fmt.Sprintf("/api/projects/%d", pid)
	if code, _ := ra.doVia(t, auth.ChannelToken, "root", http.MethodPatch, path, map[string]any{"require_plan_approval": false}); code != 403 {
		t.Fatalf("API token turned approval off: %d", code)
	}
	if p, _ := w.s.repo.GetProject(context.Background(), pid); !p.RequirePlanApproval {
		t.Fatal("approval turned off by a refused call")
	}
	// Other settings still change over MCP.
	root.call(t, "amp_update_project_settings", map[string]any{"project_id": float64(pid), "description": "still editable"})
	ra.want(t, 200, "root", http.MethodPatch, path, map[string]any{"require_plan_approval": false})
	if p, _ := w.s.repo.GetProject(context.Background(), pid); p.RequirePlanApproval {
		t.Fatal("the UI couldn't turn approval off")
	}
}

// Agents complete only work that was dispatched (and so approved): never a
// backlog ticket (#132), never by a state change. History never shows an
// empty actor. A person in the UI may still complete anything.
func TestAgentsCompleteOnlyDispatchedWork(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	pid := w.s.projectID
	a := mkIn(t, alice, w.s.storyID, "never dispatched")

	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(a)}), "never dispatched", fmt.Sprintf("task=%d", a))
	wantErr(t, alice.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": float64(a), "state": "completed"}), "never dispatched")
	wantErr(t, alice.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": float64(a), "state": "in_progress"}), "amp_dispatch_task")
	wantErr(t, alice.callExpectError(t, "amp_report_run", map[string]any{"task_id": float64(a), "outcome": "completed", "model": "m"}), "never dispatched")
	if code, _ := ra.doVia(t, auth.ChannelToken, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", a), nil); code != 409 {
		t.Fatalf("API token completed an undispatched ticket: %d", code)
	}
	if st := stateOf(t, alice, a); st != "backlog" {
		t.Fatalf("state = %s", st)
	}

	// Dispatched, it completes; moving work back stays allowed.
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "amp-worker"})
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(a), "state": "backlog", "reason": "agent crashed"})
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "amp-worker"})
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(a), "state": "completed"})
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(a), "state": "backlog", "reason": "reopen"})

	// A person in the web UI completes a ticket nobody dispatched; history
	// names them, not "".
	b := mkIn(t, alice, w.s.storyID, "done by hand")
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", b), nil)
	hist, err := w.s.repo.GetTicketHistory(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	var done *domain.ActivityLog
	for i := range hist {
		if hist[i].Action == "completed" {
			done = &hist[i]
		}
	}
	if done == nil || done.Actor != w.alice.User.Email || strings.Contains(done.Detail, `""`) {
		t.Fatalf("completion history: %+v", done)
	}
	for _, h := range hist {
		if h.Actor == "" {
			t.Fatalf("empty actor in history: %+v", h)
		}
	}
	_ = pid
}

// A question isn't cleared by a state change; the manager may relay the
// person's answer over MCP, recorded as relayed by agent.
func TestQuestions_RelayedAnswersAreMarked(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	q := mkIn(t, alice, w.s.storyID, "has a question")
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(q), "agent_id": "amp-worker"})
	alice.call(t, "amp_block_task", map[string]any{"task_id": float64(q), "reason": "Stripe or Avalara?"})
	wantErr(t, alice.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": float64(q), "state": "backlog"}), "question", "amp_answer_question")

	alice.call(t, "amp_answer_question", map[string]any{"task_id": float64(q), "answer": "Stripe"})
	comments, _ := w.s.repo.GetComments(context.Background(), q)
	if len(comments) == 0 || !strings.Contains(comments[len(comments)-1].Author, "relayed by agent") {
		t.Fatalf("relayed answer comment: %+v", comments)
	}
	hist, _ := w.s.repo.GetTicketHistory(context.Background(), q)
	if last := hist[len(hist)-1]; !strings.Contains(last.Detail, "relayed by agent") || !strings.Contains(last.Actor, "relayed by agent") {
		t.Fatalf("relayed answer history: %+v", last)
	}

	// Answered in the UI, it's the person's.
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(q), "agent_id": "amp-worker"})
	alice.call(t, "amp_block_task", map[string]any{"task_id": float64(q), "reason": "Which region?"})
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/unblock", q), map[string]any{"answer": "eu-west-1"})
	comments, _ = w.s.repo.GetComments(context.Background(), q)
	if c := comments[len(comments)-1]; c.Author != w.alice.User.Email {
		t.Fatalf("UI answer author = %q", c.Author)
	}
}

// Re-planning: a material edit (name, description, acceptance criteria) or a
// restore from the trash puts a ticket back into awaiting approval, and
// agents can't move it forward until a person approves again. Comments and
// dependency changes don't.
func TestPlanApproval_ReplannedWorkWaitsAgain(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	pid := w.s.projectID
	approvalOn(t, w.s.as(w.root))
	a := mkIn(t, alice, w.s.storyID, "a")
	b := mkIn(t, alice, w.s.storyID, "b")
	gone := mkIn(t, alice, w.s.storyID, "deleted then restored")
	alice.call(t, "amp_delete_task", map[string]any{"task_id": float64(gone)})
	approve := func() {
		t.Helper()
		time.Sleep(5 * time.Millisecond) // approval strictly after what it covers
		ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", pid), nil)
		time.Sleep(5 * time.Millisecond)
	}
	approve()
	awaiting := func() []any {
		return alice.call(t, "amp_check_plan", map[string]any{"project_id": float64(pid)})["awaiting_approval"].([]any)
	}
	if n := len(awaiting()); n != 0 {
		t.Fatalf("awaiting after approval: %d", n)
	}
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "amp-worker"})

	// Not material: a comment, priority, a dependency.
	alice.call(t, "amp_add_task_comment", map[string]any{"task_id": float64(b), "body": "note"})
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(b), "priority": "1"})
	c := mkIn(t, alice, w.s.storyID, "c")
	alice.call(t, "amp_set_task_dependencies", map[string]any{"task_id": float64(c), "add": []any{float64(b)}})
	if got := awaiting(); containsID(got, b) || !containsID(got, c) {
		t.Fatalf("after non-material changes: %v", got)
	}

	// Material: the description of a ticket in progress. Its agent can't
	// complete it until a person approves the new scope.
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(a), "description": "now also do X"})
	if !containsID(awaiting(), a) {
		t.Fatalf("description change didn't re-plan ticket %d", a)
	}
	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(a)}), "isn't approved", planPath(pid))
	// Renaming a ready ticket: dispatch refuses.
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(b), "name": "b, renamed"})
	wantErr(t, alice.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(b), "agent_id": "amp-worker"}), "approved")

	// Restoring from the trash re-plans too.
	alice.call(t, "amp_restore_tasks", map[string]any{"project_id": float64(pid), "task_ids": []any{float64(gone)}})
	if !containsID(awaiting(), gone) {
		t.Fatalf("restored ticket %d doesn't await approval", gone)
	}

	approve()
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(a)})
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b), "agent_id": "amp-worker"})
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(gone), "agent_id": "amp-worker"})
}

// Approving an artifact is the person's, in the UI; asking for changes may
// be relayed over MCP.
func TestArtifactApprovalOnlyInTheUI(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	ticket := mkIn(t, alice, w.s.storyID, "mockup")
	out := alice.call(t, "amp_publish_artifact", map[string]any{"task_id": float64(ticket), "title": "Mockup", "kind": "text", "content": "v1"})
	id := intField(t, out["artifact"].(map[string]any), "id")
	wantErr(t, alice.callExpectError(t, "amp_review_artifact", map[string]any{"artifact_id": float64(id), "approve": true}),
		"web UI", fmt.Sprintf("/artifacts/%d", id))
	alice.call(t, "amp_review_artifact", map[string]any{"artifact_id": float64(id), "approve": false, "note": "bigger"})
	if code, _ := ra.doVia(t, auth.ChannelToken, "alice", http.MethodPost, fmt.Sprintf("/api/artifacts/%d/review", id), map[string]any{"approve": true}); code != 403 {
		t.Fatalf("API token approved an artifact: %d", code)
	}
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/artifacts/%d/review", id), map[string]any{"approve": true})
}

func containsID(v any, id int) bool {
	list, _ := v.([]any)
	for _, x := range list {
		if int(x.(float64)) == id {
			return true
		}
	}
	return false
}

// Approving publishes project.updated (what the UI's live stream refreshes
// the Flight plan page on): the service itself publishes it, so every way
// of approving refreshes the page the same way (AMP #1897). Refused
// approvals publish nothing.
func TestApprovePlanPublishesProjectUpdated(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	var mu sync.Mutex
	var got []domain.Event
	w.s.hub.AddSink(func(e domain.Event) {
		if e.Type == domain.EventProjectUpdated && e.ProjectID == w.s.projectID {
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
		}
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := len(got)
		got = nil
		return n
	}

	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	person := authz.Principal{UserID: w.bob.User.ID, Email: w.bob.User.Email, Channel: auth.ChannelUI}
	if _, err := svc.ApprovePlan(context.Background(), person, w.s.projectID); err != nil {
		t.Fatalf("service approve: %v", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("service approve: %d events", n)
	}
	// root approves a ticket of their own, so the project's last approval moves.
	mkIn(t, w.s.as(w.root), w.s.storyID, "root's")
	count()
	ra.want(t, 200, "root", http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", w.s.projectID), nil)
	mu.Lock()
	if len(got) != 1 {
		mu.Unlock()
		t.Fatalf("REST approve: %d events", len(got))
	}
	if p, ok := got[0].Payload.(*domain.Project); !ok || p.ID != w.s.projectID || p.PlanApprovedAt == nil {
		mu.Unlock()
		t.Fatalf("payload %#v", got[0].Payload)
	}
	got = nil
	mu.Unlock()

	w.s.as(w.root).callExpectError(t, "amp_approve_plan", map[string]any{"project_id": float64(w.s.projectID)})
	agent := authz.Principal{UserID: w.alice.User.ID, Email: "bot@agents.amp", Agent: true, Channel: auth.ChannelUI}
	if _, err := svc.ApprovePlan(context.Background(), agent, w.s.projectID); authz.StatusOf(err, 0) != 403 {
		t.Fatalf("agent approve: %v", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("refused approvals published %d events", n)
	}
}

// A person's ticket becomes agent work only in the web UI. Otherwise an agent
// could flip it to agent work, dispatch it and complete it: the person's
// merge or sign-off marked done without them (PR #77 review, ticket #2189).
// Agent work may still become a person's ticket from anywhere.
func TestHumanTicketsBecomeAgentWorkOnlyInTheUI(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	bob := w.s.as(w.bob)
	_, review, _ := humanChain(t, w)
	link := fmt.Sprintf("/project/%d/board?task=%d", w.s.projectID, review)
	path := fmt.Sprintf("/api/tasks/%d", review)
	stillHuman := func(when string) {
		t.Helper()
		tk := humanTask(t, bob, review)
		if tk["performer"] != "human" || tk["human_reason"] != "review" || tk["name"] != "Review and merge the quotes PR" {
			t.Fatalf("%s: ticket changed: %v", when, tk)
		}
	}

	// The person's own agent, over MCP: refused with the ticket link, nothing
	// changes (not even the other fields in the same call).
	wantErr(t, bob.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(review), "performer": "agent", "name": "renamed"}),
		"web UI", link)
	stillHuman("MCP")
	// Case and spacing don't get around it.
	wantErr(t, bob.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(review), "performer": " Agent "}), "web UI")
	stillHuman("MCP, mixed case")
	// REST off the UI (API token, a non-UI Dex token): 403 with the link.
	for _, ch := range []string{auth.ChannelToken, auth.ChannelRESTOther} {
		code, out := ra.doVia(t, ch, "bob", http.MethodPatch, path, map[string]any{"performer": "agent"})
		if code != 403 || !strings.Contains(fmt.Sprint(out), link) {
			t.Fatalf("REST %s switching a human ticket to agent: %d %v, want 403 with %s", ch, code, out, link)
		}
		stillHuman("REST " + ch)
	}
	// Other edits to a person's ticket still work over MCP.
	bob.call(t, "amp_update_task", map[string]any{"task_id": float64(review), "description": "Merge once CI is green", "human_reason": "review"})
	stillHuman("MCP edit")

	// The person, in the web UI, may hand it to agents.
	out := ra.want(t, 200, "bob", http.MethodPatch, path, map[string]any{"performer": "agent"})
	if tk := out["task"].(map[string]any); tk["performer"] != "agent" {
		t.Fatalf("UI switch to agent: %v", tk)
	}
	// And agent work may become a person's ticket over MCP.
	bob.call(t, "amp_update_task", map[string]any{"task_id": float64(review), "performer": "human", "human_reason": "review"})
	stillHuman("back to human over MCP")
}
