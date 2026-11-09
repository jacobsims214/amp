package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
)

// humanChain plans build (agent) → review (human, bob's) → deploy (agent) as
// alice, the way a manager puts a person in the loop.
func humanChain(t *testing.T, w *rbacWorld) (build, review, deploy int) {
	t.Helper()
	res := w.s.as(w.alice).call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "build", "story_id": float64(w.s.storyID), "name": "Build the quotes API"},
		map[string]any{"ref": "review", "story_id": float64(w.s.storyID), "name": "Review and merge the quotes PR",
			"performer": "human", "human_reason": "review", "owner": w.bob.Email, "depends_on": []any{"build"}, "description": "What the person does", "acceptance_criteria": "Done when it is done"},
		map[string]any{"ref": "deploy", "story_id": float64(w.s.storyID), "name": "Deploy quotes", "depends_on": []any{"review"}},
	}})
	ids := map[string]int{}
	for _, c := range res["created"].([]any) {
		m := c.(map[string]any)
		ids[m["ref"].(string)] = int(m["id"].(float64))
	}
	return ids["build"], ids["review"], ids["deploy"]
}

func humanTask(t *testing.T, s *mcpStack, id int) map[string]any {
	t.Helper()
	out := s.call(t, "amp_get_task", map[string]any{"task_id": float64(id)})
	return out["task"].(map[string]any)
}

// A human ticket is a node in the plan like any other: the agent work after
// it waits for the person and is ready the moment they finish. Agents never
// dispatch it; its owner starts and finishes it in AMP.
func TestHuman_ChainWaitsForThePerson(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice, bob := w.s.as(w.alice), w.s.as(w.bob)
	build, review, deploy := humanChain(t, w)

	rv := humanTask(t, alice, review)
	if rv["performer"] != "human" || rv["human_reason"] != "review" || int(rv["owner_user_id"].(float64)) != w.bob.User.ID || rv["state"] != "blocked" {
		t.Fatalf("review ticket = %v", rv)
	}
	if b := humanTask(t, alice, build); b["performer"] != "agent" {
		t.Fatalf("an agent ticket's performer = %v", b["performer"])
	}

	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(build), "agent_id": "amp-worker"})
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(build)})
	if st := humanTask(t, alice, review)["state"]; st != "backlog" {
		t.Fatalf("review after build: %v, want ready", st)
	}

	// Alice's board: the review isn't hers to dispatch, but her deploy waits on it.
	board := alice.call(t, "amp_list_tasks", map[string]any{"project_id": float64(w.s.projectID)})
	if strings.Contains(fmt.Sprint(board["ready_to_dispatch"]), fmt.Sprint(review)) {
		t.Fatalf("human ticket offered for dispatch: %v", board["ready_to_dispatch"])
	}
	people := board["waiting_on_people"].([]any)
	if len(people) != 1 {
		t.Fatalf("waiting_on_people = %v", people)
	}
	p := people[0].(map[string]any)
	if int(p["id"].(float64)) != review || p["owner"] != w.bob.Email || p["reason"] != "review" || p["state"] != "ready" ||
		fmt.Sprint(p["holding_up"]) != fmt.Sprintf("[%d]", deploy) {
		t.Fatalf("waiting on = %v", p)
	}
	full := alice.call(t, "amp_list_tasks", map[string]any{"project_id": float64(w.s.projectID), "owner": "any", "format": "full"})
	for _, x := range full["ready_to_dispatch"].([]any) {
		if int(x.(map[string]any)["id"].(float64)) == review {
			t.Fatal("full board offers the human ticket for dispatch")
		}
	}
	if len(full["waiting_on_people"].([]any)) != 1 {
		t.Fatalf("full board waiting_on_people = %v", full["waiting_on_people"])
	}

	// Nobody's agent dispatches it, not even its owner's.
	alice.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(review), "agent_id": "amp-worker"})
	if msg := bob.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(review), "agent_id": "amp-worker"}); !strings.Contains(msg, "human work") {
		t.Fatalf("dispatching a human ticket: %q", msg)
	}
	// Only its owner starts it; an agent ticket isn't started this way.
	if code, _ := ra.do(t, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", review), nil); code < 400 {
		t.Fatalf("alice started bob's human ticket: %d", code)
	}
	ra.want(t, 400, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", deploy), nil)
	started := ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", review), nil)["task"].(map[string]any)
	if started["state"] != "in_progress" || started["agent_id"] != domain.HumanAgentID || started["dispatched_at"] == nil {
		t.Fatalf("started = %v", started)
	}
	// Never quiet: people work in days, not minutes.
	if q := alice.call(t, "amp_list_tasks", map[string]any{"project_id": float64(w.s.projectID), "owner": "any"})["in_progress"]; strings.Contains(fmt.Sprint(q), fmt.Sprint(review)) {
		t.Fatalf("human ticket listed as agent work in progress: %v", q)
	}

	// Bob finishes it: the deploy is ready, and his work is on the record.
	hours := 1.5
	done := ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", review),
		map[string]any{"note": "Merged after two rounds", "pr_url": "https://git.example.com/acme/pull/42", "hours_worked": hours})
	if done["state"] != "completed" {
		t.Fatalf("complete = %v", done)
	}
	if st := humanTask(t, alice, deploy)["state"]; st != "backlog" {
		t.Fatalf("deploy after the review: %v, want ready", st)
	}
	runs := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(review), "include": []any{"runs", "history"}})
	rs := runs["runs"].([]any)
	if len(rs) != 1 {
		t.Fatalf("runs = %v", runs["runs"])
	}
	run := rs[0].(map[string]any)
	if run["agent_id"] != domain.HumanAgentID || run["hours_worked"] != hours || run["pr_url"] != "https://git.example.com/acme/pull/42" || run["outcome"] != "completed" {
		t.Fatalf("human run = %v", run)
	}
	if u := runs["usage"].(map[string]any); num(t, u, "runs") != 0 || num(t, u, "human_runs") != 1 || num(t, u, "unreported") != 0 {
		t.Fatalf("a person's work counted as agent usage: %v", u)
	}
	h := fmt.Sprint(runs["history"])
	if !strings.Contains(h, "Started by its owner") || !strings.Contains(h, "Done by its owner") {
		t.Fatalf("history = %v", h)
	}

	// The plan's human impact.
	imp := alice.call(t, "amp_human_impact", map[string]any{"project_id": float64(w.s.projectID)})["work"].(map[string]any)
	hs := imp["human"].(map[string]any)
	if num(t, hs, "tickets") != 1 || num(t, hs, "completed_in_range") != 1 || hs["hours_worked"] != hours {
		t.Fatalf("human stats = %v", hs)
	}
	if as := imp["agent"].(map[string]any); num(t, as, "tickets") != 2 {
		t.Fatalf("agent stats = %v", as)
	}
	slow := imp["slowest_human_tickets"].([]any)
	if len(slow) != 1 || num(t, slow[0].(map[string]any), "id") != review || num(t, slow[0].(map[string]any), "downstream") != 1 {
		t.Fatalf("slowest = %v", slow)
	}
	if r := imp["human_by_reason"].([]any); len(r) != 1 || r[0].(map[string]any)["reason"] != "review" {
		t.Fatalf("by reason = %v", r)
	}
	// The project report's agents are agents only.
	rep := ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/report", w.s.projectID), nil)
	if strings.Contains(fmt.Sprint(rep["agents"]), "key:human") {
		t.Fatalf("people listed among agents: %v", rep["agents"])
	}
}

// A human ticket is the person's own gate: only they mark it done, in the
// web UI. Their own agent (over MCP) is refused with the ticket's link, and
// agent accounts never do human work.
func TestHuman_DoneOnlyByThePersonInTheUI(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice, bob := w.s.as(w.alice), w.s.as(w.bob)
	build, review, _ := humanChain(t, w)
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(build), "agent_id": "amp-worker"})
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(build)})

	// Someone else's agent can't.
	alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(review)})
	// An agent account can't, even its owner's.
	agentUser := *w.bob.User
	agentUser.Kind = domain.UserKindAgent
	bot := w.s.as(&auth.Identity{Subject: w.bob.Subject, Email: w.bob.Email, User: &agentUser})
	if msg := bot.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(review)}); !strings.Contains(msg, "agent accounts") {
		t.Fatalf("agent account completing human work: %q", msg)
	}
	// The person's own agent, over MCP, is refused with the link to the ticket.
	wantErr(t, bob.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(review), "note": "merged it"}),
		"web UI", fmt.Sprintf("/t/%d", review))
	wantErr(t, bob.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": float64(review), "state": "completed"}), "web UI")
	// So is the person's API token on REST: it isn't the web UI either.
	if code, _ := ra.doVia(t, auth.ChannelToken, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", review), map[string]any{"note": "x"}); code != 403 {
		t.Fatalf("API token completing a human ticket: %d, want 403", code)
	}
	if st := stateOf(t, bob, review); st == "completed" {
		t.Fatal("human ticket completed off the UI")
	}
	// The person, in the web UI, marks it done.
	out := ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", review), map[string]any{"note": "merged it", "hours_worked": 0.5})
	run := out["run"].(map[string]any)
	if !strings.Contains(run["note"].(string), "merged it") || strings.Contains(run["note"].(string), "agent") {
		t.Fatalf("note = %v", run["note"])
	}
}

// Who does a ticket can change before it starts, never mid-work, and every
// change is on its history. A human ticket always needs an owner.
func TestHuman_SwitchingAndRules(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	id := mkIn(t, alice, w.s.storyID, "Rotate the payment provider's API key")

	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(id), "performer": "human", "human_reason": "access"})
	if tk := humanTask(t, alice, id); tk["performer"] != "human" || tk["human_reason"] != "access" {
		t.Fatalf("after switch: %v", tk)
	}
	// Changing just the reason keeps it human.
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(id), "human_reason": "approval"})
	if tk := humanTask(t, alice, id); tk["performer"] != "human" || tk["human_reason"] != "approval" {
		t.Fatalf("after reason change: %v", tk)
	}
	if msg := alice.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(id), "human_reason": "vibes"}); !strings.Contains(msg, "human_reason must be one of") {
		t.Fatalf("bad reason: %q", msg)
	}
	// Back to agents (a person, in the web UI: see TestHumanTicketsBecomeAgentWorkOnlyInTheUI),
	// then started by one: no switching mid-work.
	newRestAs(t, w).want(t, 200, "alice", http.MethodPatch, fmt.Sprintf("/api/tasks/%d", id), map[string]any{"performer": "agent"})
	if tk := humanTask(t, alice, id); tk["performer"] != "agent" || tk["human_reason"] != nil {
		t.Fatalf("back to agent: %v", tk)
	}
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "amp-worker"})
	if msg := alice.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(id), "performer": "human"}); !strings.Contains(msg, "in progress") {
		t.Fatalf("switch mid-work: %q", msg)
	}
	hist := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(id), "include": []any{"history"}})
	if h := fmt.Sprint(hist["history"]); !strings.Contains(h, "Now done by a person (was an agent): access") || !strings.Contains(h, "Now done by an agent (was a person)") {
		t.Fatalf("history = %v", h)
	}

	// A human ticket nobody owns is refused at creation, and flagged by the plan check if it loses its owner.
	msg := alice.callExpectError(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "x", "story_id": float64(w.s.storyID), "name": "Call the bank", "performer": "human", "owner": "none"}}})
	if !strings.Contains(msg, "needs an owner") {
		t.Fatalf("ownerless human ticket: %q", msg)
	}
	res := alice.call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "call", "story_id": float64(w.s.storyID), "name": "Call the bank", "performer": "human", "human_reason": "external", "description": "What the person does", "acceptance_criteria": "Done when it is done"},
		map[string]any{"ref": "after", "story_id": float64(w.s.storyID), "name": "Wire the bank's settings", "depends_on": []any{"call"}}}})
	call := int(res["created"].([]any)[0].(map[string]any)["id"].(float64))
	if tk := humanTask(t, alice, call); int(tk["owner_user_id"].(float64)) != w.alice.User.ID {
		t.Fatalf("a human ticket without an owner given is its creator's: %v", tk["owner_user_id"])
	}
	plan := alice.call(t, "amp_check_plan", map[string]any{"project_id": float64(w.s.projectID)})
	gates := plan["human_gates"].([]any)
	if len(gates) != 1 || num(t, gates[0].(map[string]any), "id") != call || num(t, gates[0].(map[string]any), "holding_up") != 1 {
		t.Fatalf("human gates = %v", gates)
	}
	alice.call(t, "amp_assign_task", map[string]any{"task_id": float64(call), "user": "none"})
	plan = alice.call(t, "amp_check_plan", map[string]any{"project_id": float64(w.s.projectID)})
	if !strings.Contains(fmt.Sprint(plan["issues"]), "human_without_owner") || plan["ok"] != false {
		t.Fatalf("ownerless human ticket not flagged: %v", plan["issues"])
	}

	// Trash keeps who does it. An open human ticket is deleted in the web UI
	// (#2190: off the UI that would remove the person's gate).
	alice.call(t, "amp_assign_task", map[string]any{"task_id": float64(call), "user": "me"})
	newRestAs(t, w).want(t, 200, "lead", http.MethodDelete, fmt.Sprintf("/api/tasks/%d", call), nil)
	w.s.as(w.lead).call(t, "amp_restore_tasks", map[string]any{"project_id": float64(w.s.projectID), "task_ids": []any{float64(call)}})
	if tk := humanTask(t, alice, call); tk["performer"] != "human" || tk["human_reason"] != "external" {
		t.Fatalf("restored: %v", tk)
	}
}

// Trash snapshots from before human work existed restore as agent tickets.
func TestHuman_OldTrashSnapshotsRestore(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	id := mkIn(t, alice, w.s.storyID, "legacy ticket")
	w.s.as(w.lead).call(t, "amp_delete_task", map[string]any{"task_id": float64(id)})
	db, err := pgx.Connect(t.Context(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	if _, err := db.Exec(t.Context(),
		`UPDATE deleted_tasks SET snapshot = jsonb_set(snapshot, '{task}', (snapshot->'task') - 'performer' - 'human_reason' - 'ready_at') WHERE task_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	w.s.as(w.lead).call(t, "amp_restore_tasks", map[string]any{"project_id": float64(w.s.projectID), "task_ids": []any{float64(id)}})
	if tk := humanTask(t, alice, id); tk["performer"] != "agent" {
		t.Fatalf("restored legacy ticket: %v", tk)
	}
}

func commentBodies(t *testing.T, s *mcpStack, id int) string {
	t.Helper()
	out := s.call(t, "amp_get_task", map[string]any{"task_id": float64(id), "include": []any{"comments"}})
	return fmt.Sprint(out["comments"])
}

func upstreamOf(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	var ups []map[string]any
	if raw, ok := out["upstream_decisions"].([]any); ok {
		for _, x := range raw {
			ups = append(ups, x.(map[string]any))
		}
	}
	return ups
}

// A person's note is the decision other tickets wait on: a judgment or
// approval ticket needs one, it is posted as a comment, and it reaches the
// tickets that depend on it (amp_get_task, amp_claim_task, and the REST
// endpoint) without an extra call. Other reasons keep the note optional.
func TestHuman_DecisionReachesDependents(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	story, pid := float64(w.s.storyID), float64(w.s.projectID)
	res := alice.call(t, "amp_create_tasks", map[string]any{"project_id": pid, "tasks": []any{
		map[string]any{"ref": "j", "story_id": story, "name": "Confirm: north-star targets", "performer": "human", "human_reason": "judgment", "owner": w.bob.Email, "description": "What the person does", "acceptance_criteria": "Done when it is done", "ask": northStarAsk()},
		map[string]any{"ref": "a", "story_id": story, "name": "Sign off the budget", "performer": "human", "human_reason": "approval", "owner": w.bob.Email, "description": "What the person does", "acceptance_criteria": "Done when it is done"},
		map[string]any{"ref": "k", "story_id": story, "name": "Rack the server", "performer": "human", "human_reason": "physical", "owner": w.bob.Email, "description": "What the person does", "acceptance_criteria": "Done when it is done"},
		map[string]any{"ref": "m", "story_id": story, "name": "Pick a vendor", "performer": "human", "human_reason": "judgment", "owner": w.bob.Email, "description": "What the person does", "acceptance_criteria": "Done when it is done", "ask": northStarAsk()},
		map[string]any{"ref": "d1", "story_id": story, "name": "Build scorecard", "depends_on": []any{"j", "a", "k"}},
		map[string]any{"ref": "d2", "story_id": story, "name": "Wire vendor", "depends_on": []any{"j", "m"}},
	}})
	ids := map[string]int{}
	for _, c := range res["created"].([]any) {
		m := c.(map[string]any)
		ids[m["ref"].(string)] = int(m["id"].(float64))
	}
	// j stands for a judgment ticket without a doc (J3: made before decision
	// docs, or by a person in the UI), whose owner writes the decision when
	// marking it done. A ticket with a doc is recorded on it (decisions_test.go).
	execSQL(t, `DELETE FROM artifacts WHERE id IN (SELECT artifact_id FROM decision_docs WHERE task_id = $1)`, ids["j"])
	done := func(who string, id int, body map[string]any) (int, map[string]any) {
		return ra.do(t, who, http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", id), body)
	}

	// A decision ticket needs the decision, on the server too.
	for _, note := range []any{nil, "", "   "} {
		body := map[string]any{}
		if note != nil {
			body["note"] = note
		}
		code, out := done("bob", ids["j"], body)
		if code != 400 || !strings.Contains(fmt.Sprint(out), "say what you decided") {
			t.Fatalf("judgment ticket with note %v: %d %v", note, code, out)
		}
	}
	if code, _ := done("bob", ids["a"], map[string]any{}); code != 400 {
		t.Fatalf("approval ticket with no note: %d, want 400", code)
	}
	if st := stateOf(t, alice, ids["j"]); st == "completed" {
		t.Fatal("a refused completion still completed the ticket")
	}

	// The note is a comment: Decision by <name> for judgment and approval,
	// Done by <name> for other reasons, nothing without a note.
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", ids["j"]), map[string]any{"note": "Go with B; keep trial at 14 days"})
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", ids["a"]), map[string]any{"note": "Approved at 40k"})
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", ids["k"]), map[string]any{})
	if c := commentBodies(t, alice, ids["j"]); !strings.Contains(c, "Decision by bob: Go with B; keep trial at 14 days") {
		t.Fatalf("judgment comments = %s", c)
	}
	if c := commentBodies(t, alice, ids["a"]); !strings.Contains(c, "Decision by bob: Approved at 40k") {
		t.Fatalf("approval comments = %s", c)
	}
	if c := commentBodies(t, alice, ids["k"]); strings.Contains(c, "Done by") || strings.Contains(c, "Decision by") {
		t.Fatalf("a physical ticket with no note posted a comment: %s", c)
	}
	// Another reason, with a note, says Done by.
	other := alice.call(t, "amp_create_tasks", map[string]any{"project_id": pid, "tasks": []any{
		map[string]any{"ref": "x", "story_id": story, "name": "Call the bank", "performer": "human", "human_reason": "external", "owner": w.bob.Email, "description": "What the person does", "acceptance_criteria": "Done when it is done"}}})
	xid := int(other["created"].([]any)[0].(map[string]any)["id"].(float64))
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", xid), map[string]any{"note": "Called; they will wire Friday"})
	if c := commentBodies(t, alice, xid); !strings.Contains(c, "Done by bob: Called; they will wire Friday") {
		t.Fatalf("external comments = %s", c)
	}

	// amp_get_task on a dependent: the decided tickets, not the empty-note
	// one (k) and not the one still open (m).
	got := upstreamOf(t, alice.call(t, "amp_get_task", map[string]any{"task_id": float64(ids["d1"])}))
	if len(got) != 2 || int(got[0]["task_id"].(float64)) != ids["j"] || int(got[1]["task_id"].(float64)) != ids["a"] {
		t.Fatalf("d1 upstream_decisions = %v", got)
	}
	j := got[0]
	if j["decision"] != "Go with B; keep trial at 14 days" || j["human_reason"] != "judgment" || j["name"] != "Confirm: north-star targets" ||
		j["decided_by_name"] != "bob" || int(j["decided_by"].(float64)) != w.bob.User.ID || j["decided_at"] == nil {
		t.Fatalf("decision = %v", j)
	}
	if ups := upstreamOf(t, alice.call(t, "amp_get_task", map[string]any{"task_id": float64(ids["d2"])})); len(ups) != 1 || int(ups[0]["task_id"].(float64)) != ids["j"] {
		t.Fatalf("d2 upstream_decisions (m is still open) = %v", ups)
	}
	// A ticket with nothing decided upstream has no key at all.
	if _, has := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(ids["j"])})["upstream_decisions"]; has {
		t.Fatal("upstream_decisions on a ticket with no dependencies")
	}

	// amp_claim_task returns it, and says to follow it.
	d := alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(ids["d1"]), "agent_id": "amp-worker"})
	claim := alice.call(t, "amp_claim_task", map[string]any{"task_id": float64(ids["d1"]), "dispatch_id": d["dispatch_id"]})
	if ups := upstreamOf(t, claim); len(ups) != 2 || ups[0]["decision"] != "Go with B; keep trial at 14 days" {
		t.Fatalf("claim upstream_decisions = %v", ups)
	}
	if next, _ := claim["next"].(string); !strings.Contains(next, "Decisions people made that this ticket depends on are in upstream_decisions; follow them.") {
		t.Fatalf("claim next = %q", next)
	}

	// The drawer's endpoint.
	rest := ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decisions", ids["d1"]), nil)
	if rest["own"] != nil {
		t.Fatalf("own = %v, want null in Phase 1", rest["own"])
	}
	if up, _ := rest["upstream"].([]any); len(up) != 2 || up[0].(map[string]any)["decided_by_name"] != "bob" {
		t.Fatalf("GET decisions = %v", rest)
	}
	// Nothing upstream is an empty list, not null.
	if up, ok := ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decisions", ids["j"]), nil)["upstream"].([]any); !ok || len(up) != 0 {
		t.Fatalf("empty upstream = %v", up)
	}
	// Someone outside the project can't read it.
	if code, _ := ra.do(t, "outsider", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decisions", ids["d1"]), nil); code < 400 {
		t.Fatalf("outsider read decisions: %d", code)
	}
}
