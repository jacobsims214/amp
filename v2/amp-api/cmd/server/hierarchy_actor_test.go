// Hierarchy actor tests — validate the full Project→Epic→Story→Task actor tree.
//
// These tests verify behaviours that only exist in the new hierarchy:
//   - Story state auto-rolls up when all its tasks complete
//   - Epic state auto-rolls up when all its stories complete
//   - Cross-story deps unblock correctly through the hierarchy
//   - MsgDepCompleted fans correctly across epic boundaries
package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// TestHierarchyActor_StoryAutoCompletesWhenAllTasksDone verifies that when every
// task in a story completes, the story state automatically moves to completed.
// This is pure actor rollup — no MCP call involved.
func TestHierarchyActor_StoryAutoCompletesWhenAllTasksDone(t *testing.T) {
	s := setupMCP(t)

	// Create two tasks in the default story.
	t1 := s.mkTask(t, "story-task-1", nil)
	t2 := s.mkTask(t, "story-task-2", nil)
	id1 := intField(t, t1, "id")
	id2 := intField(t, t2, "id")

	// Dispatch and complete both.
	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(id1), "agent_id": "agent"})
	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(id2), "agent_id": "agent"})
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(id1)})
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(id2)})

	// The rollup is async (TaskActor → StoryActor is fire-and-forget): wait
	// for it rather than sleeping a fixed time, which flakes under -race.
	waitRollupState(t, s, "story", s.storyID, "completed")
	t.Logf("story auto-completed ✓")
}

// TestHierarchyActor_EpicAutoCompletesWhenAllStoriesDone verifies the full rollup
// chain: all tasks done → story completed → epic completed.
func TestHierarchyActor_EpicAutoCompletesWhenAllStoriesDone(t *testing.T) {
	s := setupMCP(t)

	// Create one task, complete it.
	task := s.mkTask(t, "epic-only-task", nil)
	taskID := intField(t, task, "id")

	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(taskID), "agent_id": "agent"})
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(taskID)})

	// Wait for the async rollup to propagate: task → story → epic.
	waitRollupState(t, s, "epic", s.epicID, "completed")
	t.Logf("epic auto-completed ✓")
}

// TestHierarchyActor_StoryMovesToInProgressOnFirstDispatch verifies that
// a story transitions from backlog to in_progress when its first task is dispatched.
func TestHierarchyActor_StoryMovesToInProgressOnFirstDispatch(t *testing.T) {
	s := setupMCP(t)

	task := s.mkTask(t, "in-progress-task", nil)
	taskID := intField(t, task, "id")

	// Before dispatch — story should be backlog.
	story := s.call(t, "amp_get_story", map[string]interface{}{"story_id": float64(s.storyID)})
	initialState := story["story"].(map[string]interface{})["state"].(string)
	t.Logf("story state before dispatch: %s", initialState)

	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(taskID), "agent_id": "agent"})

	// Wait for the async rollup.
	waitRollupState(t, s, "story", s.storyID, "in_progress")
	t.Logf("story moved to in_progress on first dispatch ✓")
}

// TestHierarchyActor_CrossStoryDepUnblocksCorrectly verifies that a task in
// Story B that depends on a task in Story A unblocks when Story A's task completes.
// This tests the MsgDepCompleted fan-out through EpicActor → both StoryActors.
func TestHierarchyActor_CrossStoryDepUnblocksCorrectly(t *testing.T) {
	s := setupMCP(t)

	// Create a second story in the same epic.
	story2 := s.call(t, "amp_create_story", map[string]interface{}{
		"project_id":          float64(s.projectID),
		"epic_id":             float64(s.epicID),
		"name":                "story-b",
		"acceptance_criteria": "done",
	})
	storyBID := intField(t, story2, "id")
	t.Logf("story A=%d, story B=%d", s.storyID, storyBID)

	// Task 1 in Story A.
	t1 := s.mkTask(t, "story-a-task", nil)
	id1 := intField(t, t1, "id")

	// Task 2 in Story B, depends on Task 1 in Story A.
	t2 := s.call(t, "amp_create_task", map[string]interface{}{
		"project_id":          float64(s.projectID),
		"epic_id":             float64(s.epicID),
		"story_id":            float64(storyBID),
		"name":                "story-b-task-depends-on-story-a",
		"description":         "cross-story dep",
		"acceptance_criteria": "done",
		"assigned_to":         "amp-worker",
		"dependency_ids":      []interface{}{float64(id1)},
	})
	id2 := intField(t, t2, "id")

	if strField(t, t2, "state") != "blocked" {
		t.Fatalf("task2 should start blocked, got %q", strField(t, t2, "state"))
	}
	t.Logf("task2 starts blocked (cross-story dep on task1) ✓")

	// Complete task 1 — should unblock task 2 via EpicActor fan-out.
	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(id1), "agent_id": "agent"})
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(id1)})

	// Task 2 should now be backlog.
	list := s.call(t, "amp_list_tasks", map[string]interface{}{"format": "full", "project_id": float64(s.projectID)})
	readyIDs := sliceIDs(t, list, "ready_to_dispatch")

	found := false
	for _, rid := range readyIDs {
		if rid == id2 {
			found = true
		}
	}
	if !found {
		t.Errorf("task2 should be in ready_to_dispatch after task1 completes, ready=%v", readyIDs)
	}
	t.Logf("cross-story dep unblocked correctly via EpicActor fan-out ✓")
}

// TestHierarchyActor_CrossEpicDepUnblocksCorrectly verifies that a task in
// Epic B that depends on a task in Epic A unblocks when Epic A's task completes.
// This is the deepest fan-out: ProjectActor → all EpicActors → StoryActors → TaskActors.
func TestHierarchyActor_CrossEpicDepUnblocksCorrectly(t *testing.T) {
	shared := setupShared(t)

	// Use a single project with two epics.
	ts := time.Now().UnixNano()
	proj, err := shared.repo.CreateProject(context.Background(), domain.CreateProjectRequest{
		Name: fmt.Sprintf("cross-epic-test-%d", ts),
		Code: fmt.Sprintf("cet-%d", ts),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	planApprovalOff(t, shared.repo, proj.ID)

	s := &mcpStack{srv: shared.srv, repo: shared.repo, projectID: proj.ID}

	// Create Epic A with a story and task.
	epicAResult := s.call(t, "amp_create_epic", map[string]interface{}{
		"project_id": float64(proj.ID), "name": "epic-a",
	})
	epicAID := intField(t, epicAResult, "id")

	storyAResult := s.call(t, "amp_create_story", map[string]interface{}{
		"project_id": float64(proj.ID), "epic_id": float64(epicAID),
		"name": "epic-a-story", "acceptance_criteria": "done",
	})
	storyAID := intField(t, storyAResult, "id")

	taskA := s.call(t, "amp_create_task", map[string]interface{}{
		"project_id": float64(proj.ID), "epic_id": float64(epicAID), "story_id": float64(storyAID),
		"name": "epic-a-task", "description": ".", "acceptance_criteria": ".", "assigned_to": "worker",
	})
	idA := intField(t, taskA, "id")

	// Create Epic B with a story and task that depends on Epic A's task.
	epicBResult := s.call(t, "amp_create_epic", map[string]interface{}{
		"project_id": float64(proj.ID), "name": "epic-b",
	})
	epicBID := intField(t, epicBResult, "id")

	storyBResult := s.call(t, "amp_create_story", map[string]interface{}{
		"project_id": float64(proj.ID), "epic_id": float64(epicBID),
		"name": "epic-b-story", "acceptance_criteria": "done",
	})
	storyBID := intField(t, storyBResult, "id")

	taskB := s.call(t, "amp_create_task", map[string]interface{}{
		"project_id": float64(proj.ID), "epic_id": float64(epicBID), "story_id": float64(storyBID),
		"name":        "epic-b-task-depends-on-epic-a",
		"description": ".", "acceptance_criteria": ".", "assigned_to": "worker",
		"dependency_ids": []interface{}{float64(idA)},
	})
	idB := intField(t, taskB, "id")

	if strField(t, taskB, "state") != "blocked" {
		t.Fatalf("epic-b task should start blocked, got %q", strField(t, taskB, "state"))
	}
	t.Logf("epic-b task blocked on epic-a task ✓ (ids: A=%d B=%d)", idA, idB)

	// Complete epic A's task.
	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(idA), "agent_id": "agent"})
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(idA)})

	// Epic B's task should now be in ready_to_dispatch.
	list := s.call(t, "amp_list_tasks", map[string]interface{}{"format": "full", "project_id": float64(proj.ID)})
	readyIDs := sliceIDs(t, list, "ready_to_dispatch")

	found := false
	for _, rid := range readyIDs {
		if rid == idB {
			found = true
		}
	}
	if !found {
		t.Errorf("epic-b task should be ready after epic-a task completes. ready=%v", readyIDs)
	}
	t.Logf("cross-epic dep unblocked via ProjectActor fan-out ✓")
}

// waitRollupState polls amp_get_story / amp_get_epic until the story or epic
// reaches want, failing after 5s. Story and epic state roll up asynchronously
// (TaskActor → StoryActor → EpicActor are fire-and-forget sends, each hop
// persisting to Postgres), so the state lands some time after the MCP call
// that triggered it returns. The ordering is deterministic; only the latency
// varies, and under -race with the whole package running it can exceed any
// fixed sleep.
func waitRollupState(t *testing.T, s *mcpStack, kind string, id int, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := s.call(t, "amp_get_"+kind, map[string]interface{}{kind + "_id": float64(id)})
		got, _ := res[kind].(map[string]interface{})["state"].(string)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %d: state %q after 5s, want %q", kind, id, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
