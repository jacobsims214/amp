// MCP integration tests — simulates exactly what the agent does.
//
// Every test call goes through:
//
//	JSON-RPC request → MCPServer.HandleMessage → tool handler → actor → postgres
//
// and reads back JSON-RPC responses, parses the result text the same way
// an LLM agent would. If a test breaks, an agent using this MCP would break.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/mcp"
	"github.com/simstech/amp-api/internal/repository"
)

// mcpStack holds a fully wired MCP server backed by real postgres + actors.
type mcpStack struct {
	srv       *mcpserver.MCPServer
	repo      *repository.Repo
	projectID int
	epicID    int // default epic for this project (tests can create more)
	storyID   int // default story under the default epic
}

// setupMCP boots the full stack and returns an mcpStack scoped to a fresh project.
func setupMCP(t *testing.T) *mcpStack {
	t.Helper()
	ctx := context.Background()

	repo, err := repository.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(repo.Close)

	if err := repo.Migrate(ctx, migrationSQL); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sseHub := hub.New()
	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)

	reg := actor.NewRegistry(system, repo, sseHub)

	// Wire the MCP server — enable tool capabilities (same as production).
	srv := mcpserver.NewMCPServer("amp-api-test", "2.0.0", mcpserver.WithToolCapabilities(false))
	mcpHandler := mcp.NewServer(reg, repo, sseHub, nil)
	mcpHandler.Register(srv)

	// Create a project scoped to this test.
	ts := time.Now().UnixNano()
	proj, err := repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: fmt.Sprintf("mcp-test-%s-%d", t.Name(), ts),
		Code: fmt.Sprintf("mct-%d", ts),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Pre-create a default epic and story so tests don't all have to do it.
	epic, err := repo.CreateEpic(ctx, domain.CreateEpicRequest{
		ProjectID: proj.ID, Name: "default-epic", Priority: "1",
	})
	if err != nil {
		t.Fatalf("create default epic: %v", err)
	}
	story, err := repo.CreateStory(ctx, domain.CreateStoryRequest{
		ProjectID: proj.ID, EpicID: epic.ID,
		Name: "default-story", AcceptanceCriteria: "done", Priority: "1",
	})
	if err != nil {
		t.Fatalf("create default story: %v", err)
	}

	return &mcpStack{srv: srv, repo: repo, projectID: proj.ID, epicID: epic.ID, storyID: story.ID}
}

// call invokes an MCP tool by name with args, asserts no JSON-RPC error,
// and returns the parsed result map — exactly what an agent would read.
func (s *mcpStack) call(t *testing.T, toolName string, args map[string]interface{}) map[string]interface{} {
	t.Helper()

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      toolName,
			"arguments": args,
		},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	resp := s.srv.HandleMessage(context.Background(), raw)

	// Re-marshal and unmarshal to get a plain map we can inspect.
	respBytes, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	var rpcResp struct {
		Error  *struct{ Message string } `json:"error"`
		Result *mcpgo.CallToolResult     `json:"result"`
	}
	if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
		t.Fatalf("unmarshal rpc response: %v\nraw: %s", err, respBytes)
	}
	if rpcResp.Error != nil {
		t.Fatalf("tool %q returned RPC error: %s", toolName, rpcResp.Error.Message)
	}
	if rpcResp.Result == nil {
		t.Fatalf("tool %q: nil result", toolName)
	}

	// The content is a []interface{} where each item is a TextContent.
	// Parse the text of the first item — that's the JSON the agent reads.
	if len(rpcResp.Result.Content) == 0 {
		t.Fatalf("tool %q: empty content", toolName)
	}
	contentBytes, err := json.Marshal(rpcResp.Result.Content[0])
	if err != nil {
		t.Fatalf("marshal content[0]: %v", err)
	}
	var textContent struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(contentBytes, &textContent); err != nil {
		t.Fatalf("unmarshal text content: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(textContent.Text), &result); err != nil {
		t.Fatalf("unmarshal result payload from %q: %v\ntext: %s", toolName, err, textContent.Text)
	}
	return result
}

// callExpectError calls a tool and asserts it returns a JSON-RPC level error OR
// an application-level error — either way the tool should not succeed silently.
func (s *mcpStack) callExpectError(t *testing.T, toolName string, args map[string]interface{}) string {
	t.Helper()

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      toolName,
			"arguments": args,
		},
	}
	raw, _ := json.Marshal(req)
	resp := s.srv.HandleMessage(context.Background(), raw)
	respBytes, _ := json.Marshal(resp)

	var rpcResp struct {
		Error *struct{ Message string } `json:"error"`
	}
	json.Unmarshal(respBytes, &rpcResp)
	if rpcResp.Error != nil {
		return rpcResp.Error.Message
	}
	t.Fatalf("tool %q: expected an error but got success. response: %s", toolName, respBytes)
	return ""
}

// helpers to pull typed fields out of result maps
func intField(t *testing.T, m map[string]interface{}, key string) int {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("field %q missing from %v", key, m)
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	t.Fatalf("field %q: expected number, got %T", key, v)
	return 0
}

func strField(t *testing.T, m map[string]interface{}, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("field %q missing from %v", key, m)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("field %q: expected string, got %T", key, v)
	}
	return s
}

func sliceLen(t *testing.T, m map[string]interface{}, key string) int {
	t.Helper()
	v, ok := m[key]
	if !ok {
		return 0
	}
	s, ok := v.([]interface{})
	if !ok {
		t.Fatalf("field %q: expected array, got %T", key, v)
	}
	return len(s)
}

func sliceIDs(t *testing.T, m map[string]interface{}, key string) []int {
	t.Helper()
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]int, len(arr))
	for i, item := range arr {
		switch n := item.(type) {
		case float64:
			out[i] = int(n)
		case map[string]interface{}:
			out[i] = intField(t, n, "id")
		}
	}
	return out
}

// mkTask is a convenience wrapper that always includes the default epic_id and
// story_id. Tests that need a specific hierarchy create their own; tests that
// just need a task use this.
func (s *mcpStack) mkTask(t *testing.T, name string, extras map[string]interface{}) map[string]interface{} {
	t.Helper()
	args := map[string]interface{}{
		"project_id":          float64(s.projectID),
		"epic_id":             float64(s.epicID),
		"story_id":            float64(s.storyID),
		"name":                name,
		"description":         name + " description",
		"acceptance_criteria": name + " done",
	}
	for k, v := range extras {
		args[k] = v
	}
	return s.call(t, "amp_create_task", args)
}

// ---- Tests ----

// TestMCP_CreateTaskNoDepIsBacklog mirrors: agent creates a standalone task,
// expects it to be immediately dispatchable.
func TestMCP_CreateTaskNoDepIsBacklog(t *testing.T) {
	s := setupMCP(t)

	result := s.mkTask(t, "write the readme", map[string]interface{}{
		"acceptance_criteria": "readme exists",
	})

	if strField(t, result, "state") != "backlog" {
		t.Errorf("want state=backlog, got %q", strField(t, result, "state"))
	}
}

// TestMCP_CreateTaskWithDepIsBlocked mirrors: agent creates a task that depends
// on another, expects it to start blocked.
func TestMCP_CreateTaskWithDepIsBlocked(t *testing.T) {
	s := setupMCP(t)

	// Create the prerequisite.
	pre := s.mkTask(t, "prerequisite", nil)
	preID := intField(t, pre, "id")

	// Create the dependent.
	dep := s.mkTask(t, "depends on prereq", map[string]interface{}{
		"dependency_ids": []interface{}{float64(preID)},
	})

	if strField(t, dep, "state") != "blocked" {
		t.Errorf("want state=blocked, got %q", strField(t, dep, "state"))
	}
	blockedBy := sliceIDs(t, dep, "blocked_by_ids")
	if len(blockedBy) != 1 || blockedBy[0] != preID {
		t.Errorf("want blocked_by_ids=[%d], got %v", preID, blockedBy)
	}
}

// TestMCP_DispatchBlockedTaskFails mirrors: agent tries to dispatch a blocked task,
// expects the MCP call to fail with a clear error.
func TestMCP_DispatchBlockedTaskFails(t *testing.T) {
	s := setupMCP(t)

	blocker := s.mkTask(t, "blocker", nil)
	blocked := s.mkTask(t, "blocked", map[string]interface{}{
		"dependency_ids": []interface{}{float64(intField(t, blocker, "id"))},
	})

	errMsg := s.callExpectError(t, "amp_dispatch_task", map[string]interface{}{
		"task_id":  float64(intField(t, blocked, "id")),
		"agent_id": "amp-worker",
	})
	t.Logf("got expected error: %s", errMsg)
}

// TestMCP_FullManagerLoop is the end-to-end scenario:
//
//	Plan:
//	  Task 1: "Set up DB schema"       — no deps, backlog immediately
//	  Task 2: "Write user model"       — no deps, backlog immediately
//	  Task 3: "Wire auth middleware"   — depends on 1 AND 2, starts blocked
//
//	Loop:
//	  1. amp_list_tasks → ready_to_dispatch=[1,2], blocked=[3]
//	  2. Dispatch 1 and 2 (as amp-worker would)
//	  3. Worker completes task 1 → check: task 3 still blocked (needs 2)
//	  4. Worker completes task 2 → check: task 3 now in ready_to_dispatch
//	  5. Dispatch task 3
//	  6. Worker completes task 3 → check: everything completed
func TestMCP_FullManagerLoop(t *testing.T) {
	s := setupMCP(t)

	// ---- Phase 1: Manager plans and creates tasks ----

	t1 := s.mkTask(t, "Set up DB schema", map[string]interface{}{
		"description":         "Create the initial database schema",
		"acceptance_criteria": "migrations run cleanly",
	})
	t2 := s.mkTask(t, "Write user model", map[string]interface{}{
		"description":         "Implement the User struct and repo",
		"acceptance_criteria": "user can be created and fetched",
	})
	t3 := s.mkTask(t, "Wire auth middleware", map[string]interface{}{
		"description":         "JWT middleware using the user model and schema",
		"acceptance_criteria": "protected routes reject unauthenticated requests",
		"dependency_ids":      []interface{}{float64(intField(t, t1, "id")), float64(intField(t, t2, "id"))},
	})

	id1 := intField(t, t1, "id")
	id2 := intField(t, t2, "id")
	id3 := intField(t, t3, "id")

	t.Logf("created tasks: t1=%d t2=%d t3=%d", id1, id2, id3)

	// Verify initial states from create responses.
	if strField(t, t1, "state") != "backlog" {
		t.Errorf("t1: want backlog, got %q", strField(t, t1, "state"))
	}
	if strField(t, t2, "state") != "backlog" {
		t.Errorf("t2: want backlog, got %q", strField(t, t2, "state"))
	}
	if strField(t, t3, "state") != "blocked" {
		t.Errorf("t3: want blocked, got %q", strField(t, t3, "state"))
	}

	// ---- Phase 2: Manager calls amp_list_tasks, sees what to dispatch ----

	list := s.call(t, "amp_list_tasks", map[string]interface{}{
		"project_id": float64(s.projectID),
	})

	readyIDs := sliceIDs(t, list, "ready_to_dispatch")
	if len(readyIDs) != 2 {
		t.Fatalf("want 2 ready_to_dispatch, got %d: %v", len(readyIDs), readyIDs)
	}
	t.Logf("ready_to_dispatch: %v", readyIDs)

	blockedList, _ := list["blocked"].([]interface{})
	if len(blockedList) != 1 {
		t.Fatalf("want 1 blocked, got %d", len(blockedList))
	}
	// Verify blocked_by_ids is surfaced correctly.
	blockedTask := blockedList[0].(map[string]interface{})
	blockedByIDs := sliceIDs(t, blockedTask, "blocked_by_ids")
	if len(blockedByIDs) != 2 {
		t.Errorf("t3 blocked_by_ids: want 2, got %v", blockedByIDs)
	}
	t.Logf("t3 blocked_by_ids: %v", blockedByIDs)

	// ---- Phase 3: Manager dispatches tasks 1 and 2 ----
	// (in production these spawn parallel sub-agents; here we call sequentially)

	s.call(t, "amp_dispatch_task", map[string]interface{}{
		"task_id":  float64(id1),
		"agent_id": "amp-worker",
	})
	s.call(t, "amp_dispatch_task", map[string]interface{}{
		"task_id":  float64(id2),
		"agent_id": "amp-worker",
	})

	// Verify dispatched state via amp_get_task.
	gt1 := s.call(t, "amp_get_task", map[string]interface{}{"task_id": float64(id1)})
	if strField(t, gt1["task"].(map[string]interface{}), "state") != "in_progress" {
		t.Errorf("after dispatch: t1 want in_progress, got %q", strField(t, gt1["task"].(map[string]interface{}), "state"))
	}

	// ---- Phase 4: Worker-A completes task 1 ----
	// Worker posts a comment then marks complete — exactly as amp-worker would.

	s.call(t, "amp_add_task_comment", map[string]interface{}{
		"task_id": float64(id1),
		"body":    "Schema migration complete. Ran 001_init.sql successfully.",
		"author":  "amp-worker",
	})
	s.call(t, "amp_complete_task", map[string]interface{}{
		"task_id": float64(id1),
	})

	// Task 3 should still be blocked — task 2 not done yet.
	list2 := s.call(t, "amp_list_tasks", map[string]interface{}{
		"project_id": float64(s.projectID),
	})
	if sliceLen(t, list2, "ready_to_dispatch") != 0 {
		t.Errorf("after t1 done: want 0 ready_to_dispatch, got %d", sliceLen(t, list2, "ready_to_dispatch"))
	}
	if sliceLen(t, list2, "blocked") != 1 {
		t.Errorf("after t1 done: want t3 still blocked, got blocked count %d", sliceLen(t, list2, "blocked"))
	}
	// blocked_by_ids for t3 should now be just [id2].
	bl2 := list2["blocked"].([]interface{})[0].(map[string]interface{})
	bby2 := sliceIDs(t, bl2, "blocked_by_ids")
	if len(bby2) != 1 || bby2[0] != id2 {
		t.Errorf("after t1 done: t3 blocked_by_ids want [%d], got %v", id2, bby2)
	}
	t.Logf("after t1 complete: t3 blocked_by_ids=%v (correct — still needs t2)", bby2)

	// ---- Phase 5: Worker-B completes task 2 ----

	s.call(t, "amp_add_task_comment", map[string]interface{}{
		"task_id": float64(id2),
		"body":    "User model implemented. CRUD tests pass.",
		"author":  "amp-worker",
	})
	s.call(t, "amp_complete_task", map[string]interface{}{
		"task_id": float64(id2),
	})

	// Task 3 must now be in ready_to_dispatch — actor auto-unblocked it.
	list3 := s.call(t, "amp_list_tasks", map[string]interface{}{
		"project_id": float64(s.projectID),
	})
	ready3 := sliceIDs(t, list3, "ready_to_dispatch")
	if len(ready3) != 1 || ready3[0] != id3 {
		t.Fatalf("after t2 done: want ready_to_dispatch=[%d], got %v", id3, ready3)
	}
	t.Logf("after t2 complete: t3 auto-unblocked and in ready_to_dispatch ✓")

	// ---- Phase 6: Manager dispatches task 3 ----

	s.call(t, "amp_dispatch_task", map[string]interface{}{
		"task_id":  float64(id3),
		"agent_id": "amp-worker",
	})

	// ---- Phase 7: Worker-C completes task 3 ----

	s.call(t, "amp_add_task_comment", map[string]interface{}{
		"task_id": float64(id3),
		"body":    "Auth middleware wired. Protected routes return 401 without token.",
		"author":  "amp-worker",
	})
	s.call(t, "amp_complete_task", map[string]interface{}{
		"task_id": float64(id3),
	})

	// ---- Phase 8: Final state check — everything done ----

	finalList := s.call(t, "amp_list_tasks", map[string]interface{}{
		"project_id": float64(s.projectID),
	})

	if sliceLen(t, finalList, "ready_to_dispatch") != 0 {
		t.Errorf("final: want 0 ready_to_dispatch, got %d", sliceLen(t, finalList, "ready_to_dispatch"))
	}
	if sliceLen(t, finalList, "in_progress") != 0 {
		t.Errorf("final: want 0 in_progress, got %d", sliceLen(t, finalList, "in_progress"))
	}
	if sliceLen(t, finalList, "blocked") != 0 {
		t.Errorf("final: want 0 blocked, got %d", sliceLen(t, finalList, "blocked"))
	}
	completed := sliceLen(t, finalList, "completed")
	if completed != 3 {
		t.Errorf("final: want 3 completed, got %d", completed)
	}
	t.Logf("all %d tasks completed ✓", completed)

	// Verify comments persisted on t1.
	comments := s.call(t, "amp_get_task_comments", map[string]interface{}{"task_id": float64(id1)})
	if sliceLen(t, comments, "comments") == 0 {
		t.Errorf("want comments on t1, got none")
	}
	t.Logf("t1 comment count: %d ✓", sliceLen(t, comments, "comments"))
}

// TestMCP_DiamondDAGThroughMCP runs the diamond dependency graph purely through
// MCP calls — A→{B,C}→D — verifying partial unblocking works correctly.
func TestMCP_DiamondDAGThroughMCP(t *testing.T) {
	s := setupMCP(t)

	mk := func(name string, deps []int) int {
		extras := map[string]interface{}{}
		if len(deps) > 0 {
			depArgs := make([]interface{}, len(deps))
			for i, d := range deps {
				depArgs[i] = float64(d)
			}
			extras["dependency_ids"] = depArgs
		}
		r := s.mkTask(t, name, extras)
		return intField(t, r, "id")
	}
	dispatch := func(id int) {
		s.call(t, "amp_dispatch_task", map[string]interface{}{
			"task_id": float64(id), "agent_id": "amp-worker",
		})
	}
	complete := func(id int) {
		s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(id)})
	}
	listReady := func() []int {
		r := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID)})
		return sliceIDs(t, r, "ready_to_dispatch")
	}
	listBlocked := func() []map[string]interface{} {
		r := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID)})
		raw, _ := r["blocked"].([]interface{})
		out := make([]map[string]interface{}, len(raw))
		for i, item := range raw {
			out[i] = item.(map[string]interface{})
		}
		return out
	}

	a := mk("A", nil)
	b := mk("B", []int{a})
	c := mk("C", []int{a})
	d := mk("D", []int{b, c})
	t.Logf("created A=%d B=%d C=%d D=%d", a, b, c, d)

	// Initial: only A is ready.
	ready := listReady()
	if len(ready) != 1 || ready[0] != a {
		t.Fatalf("initial: want ready=[A=%d], got %v", a, ready)
	}
	if bl := listBlocked(); len(bl) != 3 {
		t.Fatalf("initial: want 3 blocked (B,C,D), got %d", len(bl))
	}

	// Complete A → B and C unblock.
	dispatch(a)
	complete(a)

	ready2 := listReady()
	if len(ready2) != 2 {
		t.Fatalf("after A: want ready=[B,C], got %v", ready2)
	}
	t.Logf("after A complete: ready=%v ✓", ready2)

	// D is still blocked — check blocked_by_ids has both B and C.
	bl2 := listBlocked()
	if len(bl2) != 1 {
		t.Fatalf("after A: want 1 blocked (D), got %d", len(bl2))
	}
	dBlockedBy := sliceIDs(t, bl2[0], "blocked_by_ids")
	if len(dBlockedBy) != 2 {
		t.Errorf("D blocked_by_ids: want 2 (B and C), got %v", dBlockedBy)
	}
	t.Logf("D blocked_by_ids=%v ✓", dBlockedBy)

	// Complete B → D still blocked on C only.
	dispatch(b)
	complete(b)

	bl3 := listBlocked()
	if len(bl3) != 1 {
		t.Fatalf("after B: want D still blocked, got %d blocked", len(bl3))
	}
	dBlockedBy2 := sliceIDs(t, bl3[0], "blocked_by_ids")
	if len(dBlockedBy2) != 1 || dBlockedBy2[0] != c {
		t.Errorf("after B: D blocked_by_ids want [C=%d], got %v", c, dBlockedBy2)
	}
	t.Logf("after B: D blocked_by_ids=%v (only C left) ✓", dBlockedBy2)

	// Complete C → D finally unblocks.
	dispatch(c)
	complete(c)

	ready3 := listReady()
	if len(ready3) != 1 || ready3[0] != d {
		t.Fatalf("after C: want ready=[D=%d], got %v", d, ready3)
	}
	t.Logf("after C: D in ready_to_dispatch ✓")

	// Complete D — all done.
	dispatch(d)
	complete(d)

	finalReady := listReady()
	if len(finalReady) != 0 {
		t.Errorf("final: want 0 ready, got %v", finalReady)
	}
	t.Logf("diamond DAG complete — all tasks done ✓")
}
