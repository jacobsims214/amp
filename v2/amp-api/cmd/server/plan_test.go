package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/mcp"
)

// Planning: dependencies are validated, editable, kept right through
// deletes and moves, and called out when missing; every field of epics,
// stories, and tickets can be fixed in place.

// planTask reads a ticket as an agent sees it (from its actor).
func planTask(t *testing.T, s *mcpStack, id int) map[string]interface{} {
	t.Helper()
	return s.call(t, "amp_get_task", map[string]interface{}{"task_id": float64(id)})["task"].(map[string]interface{})
}

func stateOf(t *testing.T, s *mcpStack, id int) string {
	t.Helper()
	return planTask(t, s, id)["state"].(string)
}

func blockedBy(t *testing.T, s *mcpStack, id int) []int {
	t.Helper()
	var out []int
	if v, ok := planTask(t, s, id)["blocked_by_ids"].([]interface{}); ok {
		for _, x := range v {
			out = append(out, int(x.(float64)))
		}
	}
	return out
}

// newStory makes another epic and story in the stack's project.
func newStory(t *testing.T, s *mcpStack, name string) (epicID, storyID int) {
	t.Helper()
	e := s.call(t, "amp_create_epic", map[string]interface{}{"project_id": float64(s.projectID), "name": name + " epic"})
	epicID = intField(t, e, "id")
	st := s.call(t, "amp_create_story", map[string]interface{}{"project_id": float64(s.projectID), "epic_id": float64(epicID), "name": name})
	return epicID, intField(t, st, "id")
}

// listedTaskIDs is every ticket amp_list_tasks returns, across its buckets.
func listedTaskIDs(t *testing.T, s *mcpStack) []int {
	t.Helper()
	res := s.call(t, "amp_list_tasks", map[string]interface{}{"format": "full", "project_id": float64(s.projectID), "owner": "any"})
	var out []int
	for _, bucket := range []string{"ready_to_dispatch", "in_progress", "blocked", "scheduled", "completed"} {
		list, _ := res[bucket].([]interface{})
		for _, x := range list {
			out = append(out, int(x.(map[string]interface{})["id"].(float64)))
		}
	}
	return out
}

func mkIn(t *testing.T, s *mcpStack, storyID int, name string, deps ...int) int {
	t.Helper()
	ids := make([]interface{}, len(deps))
	for i, d := range deps {
		ids[i] = float64(d)
	}
	return taskID(t, s.call(t, "amp_create_task", map[string]interface{}{
		"project_id": float64(s.projectID), "story_id": float64(storyID), "name": name,
		"description": "d", "acceptance_criteria": "a", "dependency_ids": ids,
	}))
}

// Bug: a new ticket depending on an already-completed ticket in another
// story started blocked and never unblocked (the completion had already
// been announced). The same on reload.
func TestPlan_DependencyOnCompletedTicketInAnotherStory(t *testing.T) {
	s := setupMCP(t)
	_, otherStory := newStory(t, s, "API")
	api := mkIn(t, s, otherStory, "build API")
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(api)})

	ui := mkIn(t, s, s.storyID, "build UI", api)
	if st := stateOf(t, s, ui); st != "backlog" || len(blockedBy(t, s, ui)) != 0 {
		t.Fatalf("UI waiting on a finished API: state %s, blocked by %v", st, blockedBy(t, s, ui))
	}

	// After a restart, the reloaded ticket agrees.
	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)
	reg := actor.NewRegistry(system, s.repo, hub.New())
	pid, err := reg.Get(s.projectID)
	if err != nil {
		t.Fatal(err)
	}
	replyCh := make(chan actor.ReplyGetTask, 1)
	system.Root.Send(pid, &actor.MsgGetTask{TaskID: ui, ReplyCh: replyCh})
	if r := <-replyCh; r.Err != nil || len(r.Task.BlockedByIDs) != 0 {
		t.Fatalf("after reload: %+v %v", r.Task, r.Err)
	}
}

func TestPlan_SetDependencies(t *testing.T) {
	s := setupMCP(t)
	a := mkIn(t, s, s.storyID, "schema")
	// A second ticket without dependencies gets a warning.
	b := s.mkTask(t, "endpoint", nil)
	if w, _ := b["warnings"].([]interface{}); len(w) != 1 || !strings.Contains(w[0].(string), "no dependencies") {
		t.Fatalf("expected a no-dependencies warning, got %v", b["warnings"])
	}
	bid := taskID(t, b)
	c := mkIn(t, s, s.storyID, "client", bid)
	if w, ok := s.call(t, "amp_get_task", map[string]interface{}{"task_id": float64(c)})["warnings"]; ok {
		t.Fatalf("a ticket with dependencies got warnings: %v", w)
	}

	// Add the missing dependency: the ready ticket becomes blocked.
	s.call(t, "amp_set_task_dependencies", map[string]interface{}{"task_id": float64(bid), "add": []interface{}{float64(a)}})
	if st := stateOf(t, s, bid); st != "blocked" || !slices.Equal(blockedBy(t, s, bid), []int{a}) {
		t.Fatalf("after adding a dependency: %s, blocked by %v", st, blockedBy(t, s, bid))
	}
	// Finishing it unblocks the ticket as usual.
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(a)})
	if st := stateOf(t, s, bid); st != "backlog" {
		t.Fatalf("after its dependency finished: %s", st)
	}

	// Replace the list; remove; refusals.
	other := mkIn(t, s, s.storyID, "other")
	s.call(t, "amp_set_task_dependencies", map[string]interface{}{"task_id": float64(bid), "dependency_ids": []interface{}{float64(a), float64(other)}})
	if st := stateOf(t, s, bid); st != "blocked" {
		t.Fatalf("waiting on an open ticket again: %s", st)
	}
	s.call(t, "amp_set_task_dependencies", map[string]interface{}{"task_id": float64(bid), "remove": []interface{}{float64(other)}})
	if st := stateOf(t, s, bid); st != "backlog" {
		t.Fatalf("after removing the open dependency: %s", st)
	}
	deps, _ := s.repo.TaskDependencies(context.Background(), bid)
	if !slices.Equal(deps, []int{a}) {
		t.Fatalf("stored dependencies = %v", deps)
	}
	set := func(task int, deps ...int) string {
		ids := make([]interface{}, len(deps))
		for i, d := range deps {
			ids[i] = float64(d)
		}
		return s.callExpectError(t, "amp_set_task_dependencies", map[string]interface{}{"task_id": float64(task), "dependency_ids": ids})
	}
	wantErr(t, set(a, c), "cycle", fmt.Sprintf("#%d", a)) // a ← b ← c, so a can't wait on c
	wantErr(t, set(bid, bid), "itself")
	wantErr(t, set(bid, 999999999), "don't exist")
	elsewhere := setupMCP(t)
	foreign := taskID(t, elsewhere.mkTask(t, "foreign", nil))
	wantErr(t, set(bid, foreign), "another project")
	wantErr(t, s.callExpectError(t, "amp_set_task_dependencies", map[string]interface{}{"task_id": float64(bid)}), "pass dependency_ids")
	wantErr(t, s.callExpectError(t, "amp_create_task", map[string]interface{}{"project_id": float64(s.projectID), "story_id": float64(s.storyID), "name": "x", "dependency_ids": []interface{}{float64(foreign)}}), "another project")

	// A ticket already running keeps running, with a warning.
	run := mkIn(t, s, s.storyID, "running")
	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(run), "agent_id": "a"})
	res := s.call(t, "amp_set_task_dependencies", map[string]interface{}{"task_id": float64(run), "add": []interface{}{float64(other)}})
	if res["state"] != "in_progress" || res["warnings"] == nil {
		t.Fatalf("running ticket after adding an open dependency: %v", res)
	}
}

// Bug: deleting a ticket left the tickets waiting on it blocked forever.
func TestPlan_DeletingADependencyUnblocksDependents(t *testing.T) {
	s := setupMCP(t)
	a := mkIn(t, s, s.storyID, "a")
	b := mkIn(t, s, s.storyID, "b", a)
	if stateOf(t, s, b) != "blocked" {
		t.Fatal("b should wait on a")
	}
	res := s.call(t, "amp_delete_task", map[string]interface{}{"task_id": float64(a)})
	if st := stateOf(t, s, b); st != "backlog" || len(blockedBy(t, s, b)) != 0 {
		t.Fatalf("after deleting its only dependency: %s, blocked by %v (%v)", st, blockedBy(t, s, b), res)
	}

	// The same when the dependency goes with its story or epic.
	epicID, storyID := newStory(t, s, "doomed")
	d1 := mkIn(t, s, storyID, "in doomed story")
	w1 := mkIn(t, s, s.storyID, "waits on story", d1)
	s.call(t, "amp_delete_story", map[string]interface{}{"story_id": float64(storyID)})
	if st := stateOf(t, s, w1); st != "backlog" {
		t.Fatalf("after deleting the story it waited on: %s", st)
	}
	if _, err := s.repo.GetStory(context.Background(), storyID); err == nil {
		t.Fatal("story still exists")
	}
	_, story2 := newStory(t, s, "doomed epic")
	e2, _ := s.repo.GetStory(context.Background(), story2)
	d2 := mkIn(t, s, story2, "in doomed epic")
	w2 := mkIn(t, s, s.storyID, "waits on epic", d2)
	s.call(t, "amp_delete_epic", map[string]interface{}{"epic_id": float64(e2.EpicID)})
	if st := stateOf(t, s, w2); st != "backlog" {
		t.Fatalf("after deleting the epic it waited on: %s", st)
	}
	_ = epicID
	// And the deleted story's tickets are really gone from the actors.
	for _, id := range listedTaskIDs(t, s) {
		if id == d1 || id == d2 {
			t.Fatalf("deleted ticket %d still listed", id)
		}
	}
}

// Bug: finishing its dependencies unblocked a ticket blocked by hand for
// another reason.
func TestPlan_ManualBlockOutlivesDependencies(t *testing.T) {
	s := setupMCP(t)
	a := mkIn(t, s, s.storyID, "a")
	b := mkIn(t, s, s.storyID, "b", a)
	s.call(t, "amp_block_task", map[string]interface{}{"task_id": float64(b), "reason": "waiting on API keys"})
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(a)})
	got := planTask(t, s, b)
	if got["state"] != "blocked" || got["block_reason"] != "waiting on API keys" {
		t.Fatalf("hand-blocked ticket after its dependency finished: %v %v", got["state"], got["block_reason"])
	}
}

func TestPlan_MoveTicketsAndStories(t *testing.T) {
	s := setupMCP(t)
	a := mkIn(t, s, s.storyID, "a")
	b := mkIn(t, s, s.storyID, "b", a)
	epic2, story2 := newStory(t, s, "elsewhere")

	// Across epics: the ticket keeps its dependency and state, and every
	// operation still reaches it.
	moved := s.call(t, "amp_move_task", map[string]interface{}{"task_id": float64(b), "story_id": float64(story2)})["task"].(map[string]interface{})
	if int(moved["story_id"].(float64)) != story2 || int(moved["epic_id"].(float64)) != epic2 || moved["state"] != "blocked" {
		t.Fatalf("moved ticket = %v", moved)
	}
	s.call(t, "amp_update_task", map[string]interface{}{"task_id": float64(b), "name": "b, renamed after the move"})
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(a)})
	if st := stateOf(t, s, b); st != "backlog" {
		t.Fatalf("moved ticket didn't unblock when its dependency finished: %s", st)
	}
	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(b), "agent_id": "a"})
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(b)})

	// Within an epic, and back.
	c := mkIn(t, s, s.storyID, "c")
	st3 := intField(t, s.call(t, "amp_create_story", map[string]interface{}{"project_id": float64(s.projectID), "epic_id": float64(s.epicID), "name": "sibling"}), "id")
	s.call(t, "amp_move_task", map[string]interface{}{"task_id": float64(c), "story_id": float64(st3)})
	// The move must leave the ticket routed to its new story, not to a
	// fallback that asks every story (whose "not mine" replies can win).
	for i := 0; i < 25; i++ {
		s.call(t, "amp_update_task", map[string]interface{}{"task_id": float64(c), "priority": "3"})
		planTask(t, s, c)
	}
	if p := planTask(t, s, c); int(p["story_id"].(float64)) != st3 || p["priority"] != "3" {
		t.Fatalf("ticket moved within the epic = %v", p)
	}

	// A story with its tickets, to another epic.
	d := mkIn(t, s, st3, "d", c)
	s.call(t, "amp_move_story", map[string]interface{}{"story_id": float64(st3), "epic_id": float64(epic2)})
	for _, id := range []int{c, d} {
		p := planTask(t, s, id)
		if int(p["epic_id"].(float64)) != epic2 {
			t.Fatalf("ticket %d's epic after moving its story = %v", id, p["epic_id"])
		}
	}
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(c)})
	if st := stateOf(t, s, d); st != "backlog" {
		t.Fatalf("ticket in a moved story didn't unblock: %s", st)
	}
	// Each ticket is listed exactly once.
	seen := map[int]int{}
	for _, id := range listedTaskIDs(t, s) {
		seen[id]++
	}
	for _, id := range []int{a, b, c, d} {
		if seen[id] != 1 {
			t.Fatalf("ticket %d listed %d times", id, seen[id])
		}
	}
	elsewhere := setupMCP(t)
	wantErr(t, s.callExpectError(t, "amp_move_task", map[string]interface{}{"task_id": float64(a), "story_id": float64(elsewhere.storyID)}), "not in task")
}

func TestPlan_CreateTasksBatch(t *testing.T) {
	s := setupMCP(t)
	existing := mkIn(t, s, s.storyID, "existing design")
	_, apiStory := newStory(t, s, "API")
	spec := func(ref string, story int, deps ...interface{}) map[string]interface{} {
		return map[string]interface{}{"ref": ref, "story_id": float64(story), "name": ref, "description": "d", "acceptance_criteria": "a", "depends_on": deps}
	}
	res := s.call(t, "amp_create_tasks", map[string]interface{}{"project_id": float64(s.projectID), "tasks": []interface{}{
		spec("ui", s.storyID, "api"),
		spec("api", apiStory, "schema", float64(existing)),
		spec("schema", apiStory),
		spec("docs", s.storyID),
	}})
	ids := map[string]int{}
	for _, c := range res["created"].([]interface{}) {
		m := c.(map[string]interface{})
		ids[m["ref"].(string)] = int(m["id"].(float64))
	}
	if len(ids) != 4 {
		t.Fatalf("created = %v", res)
	}
	if deps := planTask(t, s, ids["api"])["dependency_ids"].([]interface{}); len(deps) != 2 {
		t.Fatalf("api's dependencies = %v", deps)
	}
	if st := stateOf(t, s, ids["schema"]); st != "backlog" {
		t.Fatalf("schema (no dependencies) = %s", st)
	}
	if st := stateOf(t, s, ids["ui"]); st != "blocked" {
		t.Fatalf("ui (waits on api) = %s", st)
	}
	if w := fmt.Sprint(res["warnings"]); !strings.Contains(w, "docs") || strings.Contains(w, "schema") {
		t.Fatalf("warnings should name only the unconnected ticket: %v", w)
	}

	// Problems are found before anything is created.
	before, _ := s.repo.ListTasks(context.Background(), s.projectID, "")
	for _, bad := range []struct {
		tasks []interface{}
		want  string
	}{
		{[]interface{}{spec("a", s.storyID, "b"), spec("b", s.storyID, "a")}, "cycle"},
		{[]interface{}{spec("a", s.storyID, "nope")}, "neither a ref"},
		{[]interface{}{spec("a", s.storyID), spec("a", s.storyID)}, "used twice"},
		{[]interface{}{spec("a", s.storyID, "a")}, "itself"},
	} {
		wantErr(t, s.callExpectError(t, "amp_create_tasks", map[string]interface{}{"project_id": float64(s.projectID), "tasks": bad.tasks}), bad.want)
	}
	after, _ := s.repo.ListTasks(context.Background(), s.projectID, "")
	if len(after) != len(before) {
		t.Fatalf("a refused batch created %d tickets", len(after)-len(before))
	}
}

func TestPlan_CheckPlan(t *testing.T) {
	s := setupMCP(t)
	a := mkIn(t, s, s.storyID, "a")
	mkIn(t, s, s.storyID, "b", a)
	lonely := mkIn(t, s, s.storyID, "lonely")
	for i := 0; i < 5; i++ {
		mkIn(t, s, s.storyID, fmt.Sprintf("parallel %d", i))
	}
	_, empty := newStory(t, s, "empty")

	// Simulate a ticket stuck blocked on finished work (as the old bugs left them).
	stuck := mkIn(t, s, s.storyID, "stuck", a)
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(a)})
	if err := s.repo.SetTaskState(context.Background(), stuck, domain.TaskStateBlocked, ""); err != nil {
		t.Fatal(err)
	}
	pid, _ := s.reg.Get(s.projectID)
	rc := make(chan actor.ReplySimple, 1)
	s.reg.System().Root.Send(pid, &actor.MsgSetTaskState{TaskID: stuck, State: domain.TaskStateBlocked, ReplyCh: rc})
	<-rc

	report := s.call(t, "amp_check_plan", map[string]interface{}{"project_id": float64(s.projectID)})
	kinds := map[string][]int{}
	for _, iss := range report["issues"].([]interface{}) {
		m := iss.(map[string]interface{})
		var ids []int
		if v, ok := m["task_ids"].([]interface{}); ok {
			for _, x := range v {
				ids = append(ids, int(x.(float64)))
			}
		}
		kinds[m["kind"].(string)] = ids
	}
	if !slices.Contains(kinds["isolated"], lonely) {
		t.Errorf("isolated = %v, want to include %d", kinds["isolated"], lonely)
	}
	if _, ok := kinds["wide_start"]; !ok {
		t.Errorf("expected a wide_start issue: %v", report)
	}
	if _, ok := kinds["empty_stories"]; !ok || !strings.Contains(fmt.Sprint(report["issues"]), fmt.Sprintf("#%d", empty)) {
		t.Errorf("expected the empty story: %v", report["issues"])
	}
	if report["ok"] != false {
		t.Error("a plan with issues isn't ok")
	}
	// Only the stuck ticket: b, which also waited on a, unblocked normally.
	if rep, _ := report["repaired_task_ids"].([]interface{}); len(rep) != 1 || int(rep[0].(float64)) != stuck {
		t.Fatalf("repaired = %v, want only %d", report["repaired_task_ids"], stuck)
	}
	if st := stateOf(t, s, stuck); st != "backlog" {
		t.Fatalf("stuck ticket after the check: %s", st)
	}
}

// Every field can be fixed in place, and agents read back what they wrote.
func TestPlan_EditEveryField(t *testing.T) {
	s := setupMCP(t)
	id := mkIn(t, s, s.storyID, "draft")
	s.call(t, "amp_update_task", map[string]interface{}{"task_id": float64(id),
		"name": "final", "description": "new d", "acceptance_criteria": "new ac", "priority": "2", "assigned_to": "amp-worker-2"})
	got := planTask(t, s, id)
	for k, want := range map[string]string{"name": "final", "description": "new d", "acceptance_criteria": "new ac", "priority": "2", "assigned_to": "amp-worker-2"} {
		if got[k] != want {
			t.Errorf("task %s = %v, want %q", k, got[k], want)
		}
	}
	s.call(t, "amp_update_epic", map[string]interface{}{"epic_id": float64(s.epicID), "name": "E2", "description": "ed", "priority": "3"})
	e := s.call(t, "amp_get_epic", map[string]interface{}{"epic_id": float64(s.epicID)})["epic"].(map[string]interface{})
	if e["name"] != "E2" || e["description"] != "ed" || e["priority"] != "3" {
		t.Errorf("epic = %v", e)
	}
	s.call(t, "amp_update_story", map[string]interface{}{"story_id": float64(s.storyID), "name": "S2", "description": "sd", "acceptance_criteria": "sac", "priority": "0"})
	st := s.call(t, "amp_get_story", map[string]interface{}{"story_id": float64(s.storyID)})["story"].(map[string]interface{})
	if st["name"] != "S2" || st["description"] != "sd" || st["acceptance_criteria"] != "sac" || st["priority"] != "0" {
		t.Errorf("story = %v", st)
	}
}

// Agents learn the dependency rules when they connect.
func TestPlan_ServerInstructions(t *testing.T) {
	srv := newMCPServer(mcp.NewServer(nil, nil, hub.New(), nil))
	raw, _ := json.Marshal(srv.HandleMessage(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)))
	for _, want := range []string{"DEPENDENCIES ARE CRITICAL", "amp_create_tasks", "amp_check_plan", "never delete and re-create"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("initialize response lacks %q", want)
		}
	}
}

// Text fields can be blanked on purpose with clear; an empty value still
// means "keep", so agents sending only what changes don't wipe anything.
func TestPlan_ClearFields(t *testing.T) {
	s := setupMCP(t)
	id := mkIn(t, s, s.storyID, "draft")
	s.call(t, "amp_update_task", map[string]interface{}{"task_id": float64(id), "assigned_to": "amp-worker", "description": ""})
	if got := planTask(t, s, id); got["description"] != "d" || got["assigned_to"] != "amp-worker" {
		t.Fatalf("empty description should keep it: %v", got)
	}
	s.call(t, "amp_update_task", map[string]interface{}{"task_id": float64(id),
		"clear": []interface{}{"description", "acceptance_criteria", "assigned_to"}})
	got := planTask(t, s, id)
	for _, f := range []string{"description", "acceptance_criteria"} {
		if got[f] != "" {
			t.Errorf("%s = %q after clear, want blank", f, got[f])
		}
	}
	if v, ok := got["assigned_to"]; ok && v != "" {
		t.Errorf("assigned_to = %q after clear", v)
	}
	if row, _ := s.repo.GetTask(context.Background(), id); row.Description != "" || row.AssignedTo != "" || row.Name != "draft" {
		t.Errorf("stored task = %+v", row)
	}
	if msg := s.callExpectError(t, "amp_update_task", map[string]interface{}{"task_id": float64(id), "clear": []interface{}{"name"}}); !strings.Contains(msg, "can't be cleared") {
		t.Errorf("clearing name: %q", msg)
	}

	s.call(t, "amp_update_epic", map[string]interface{}{"epic_id": float64(s.epicID), "description": "ed"})
	s.call(t, "amp_update_epic", map[string]interface{}{"epic_id": float64(s.epicID), "clear": []interface{}{"description"}})
	if e, _ := s.repo.GetEpic(context.Background(), s.epicID); e.Description != "" {
		t.Errorf("epic description = %q", e.Description)
	}
	s.call(t, "amp_update_story", map[string]interface{}{"story_id": float64(s.storyID), "clear": []interface{}{"acceptance_criteria"}})
	if st, _ := s.repo.GetStory(context.Background(), s.storyID); st.AcceptanceCriteria != "" || st.Name != "default-story" {
		t.Errorf("story = %+v", st)
	}
}

// Bug: amp_set_task_start_at never answered — no actor above the ticket
// passed the message on — so scheduling an existing ticket hung forever.
func TestPlan_ScheduleExistingTicket(t *testing.T) {
	s := setupMCP(t)
	id := mkIn(t, s, s.storyID, "later")
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.call(t, "amp_set_task_start_at", map[string]interface{}{"task_id": float64(id), "start_at": at})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("amp_set_task_start_at never answered")
	}
	if st := stateOf(t, s, id); st != "blocked" {
		t.Fatalf("scheduled ticket: %s", st)
	}
	s.call(t, "amp_set_task_start_at", map[string]interface{}{"task_id": float64(id)})
	if st := stateOf(t, s, id); st != "backlog" {
		t.Fatalf("after clearing the schedule: %s", st)
	}
}

// A plan with nothing in the project KB says so: the context behind it
// would otherwise live only in the session that planned it.
func TestPlan_CheckPlanWantsContext(t *testing.T) {
	s := setupMCP(t)
	if s.kb == nil {
		t.Skip("TEST_TYPESENSE_URL not set")
	}
	a := mkIn(t, s, s.storyID, "a")
	mkIn(t, s, s.storyID, "b", a)
	pid := float64(s.projectID)

	hasNoContext := func() bool {
		report := s.call(t, "amp_check_plan", map[string]interface{}{"project_id": pid})
		for _, iss := range report["issues"].([]interface{}) {
			if iss.(map[string]interface{})["kind"] == "no_context" {
				return true
			}
		}
		return false
	}
	if !hasNoContext() {
		t.Fatal("empty project KB: expected a no_context issue")
	}
	s.call(t, "amp_kb_write", map[string]interface{}{"project_id": pid, "path": "architecture/project-overview.md",
		"title": "Overview", "content": "What this project is and how it's built."})
	if hasNoContext() {
		t.Fatal("no_context still reported after writing the overview")
	}
}

// A gate that joins different people's work is flagged as a warning: it
// doesn't make the plan NO-GO, but it says whose work waits on whose.
func TestPlan_CheckPlanWarnsOnCoupledGate(t *testing.T) {
	w := newRBACWorld(t)
	alice, bob := w.s.as(w.alice), w.s.as(w.bob)
	a := mkIn(t, alice, w.s.storyID, "alice's libs")
	b := mkIn(t, bob, w.s.storyID, "bob's libs")
	gate := mkIn(t, alice, w.s.storyID, "foundation check", a, b)
	mkIn(t, alice, w.s.storyID, "alice's service", gate)
	mkIn(t, bob, w.s.storyID, "bob's app", gate)

	report := alice.call(t, "amp_check_plan", map[string]interface{}{"project_id": float64(w.s.projectID)})
	var found map[string]interface{}
	for _, iss := range report["issues"].([]interface{}) {
		if m := iss.(map[string]interface{}); m["kind"] == "coupled_gate" {
			found = m
		}
	}
	if found == nil || found["severity"] != "warn" || !strings.Contains(fmt.Sprint(found["task_ids"]), fmt.Sprint(gate)) {
		t.Fatalf("no coupled_gate warning for #%d: %v", gate, report["issues"])
	}
	for _, iss := range report["issues"].([]interface{}) {
		if m := iss.(map[string]interface{}); m["severity"] != "warn" {
			return // another (blocking) issue in this shared world; ok is false for that reason
		}
	}
	if report["ok"] != true {
		t.Fatalf("a warning alone shouldn't make the plan NO-GO: %v", report)
	}
}

// Epics missing metadata their siblings carry (or carrying it under another
// name) are flagged, since reports group by those keys.
func TestPlan_CheckPlanWarnsOnMetadataDrift(t *testing.T) {
	s := setupMCP(t)
	pid := float64(s.projectID)
	epic := func(name string, md map[string]interface{}) {
		s.call(t, "amp_create_epic", map[string]interface{}{"project_id": pid, "name": name, "description": "d", "metadata": md})
	}
	s.call(t, "amp_update_epic", map[string]interface{}{"epic_id": float64(s.epicID), "metadata": map[string]interface{}{"capitalizable": true, "jira_epic": "NWC-1"}})
	epic("catalog", map[string]interface{}{"capitalizable": true, "jira_epic": "NWC-2"})
	epic("orders", map[string]interface{}{"capitalizable": true, "jira_epic": "NWC-3"})
	epic("quotes", map[string]interface{}{"capitalizable": true, "jira_key": "NWC-12"})

	report := s.call(t, "amp_check_plan", map[string]interface{}{"project_id": pid})
	for _, iss := range report["issues"].([]interface{}) {
		m := iss.(map[string]interface{})
		if m["kind"] != "metadata_drift" {
			continue
		}
		msg := m["message"].(string)
		if m["severity"] != "warn" || !strings.Contains(msg, `"quotes"`) || !strings.Contains(msg, "jira_epic") || !strings.Contains(msg, "jira_key") {
			t.Fatalf("drift issue: %v", m)
		}
		if strings.Contains(msg, `"catalog"`) {
			t.Fatalf("a complete epic is flagged: %v", msg)
		}
		return
	}
	t.Fatalf("no metadata_drift issue: %v", report["issues"])
}
