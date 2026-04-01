// Hierarchy and ticket history tests.
//
// These tests cover:
//  1. The enforced hierarchy: project → epic → story → task (no orphans allowed)
//  2. Every way to create an orphan is rejected at the MCP layer
//  3. Cross-project and cross-epic mis-parenting is rejected
//  4. amp_get_ticket_history returns the full chronological log
//  5. The ticket-as-prompt flow: manager writes full context, worker reads it
package main

import (
	"strings"
	"testing"
)

// ---- Orphan prevention ----

// TestHierarchy_TaskRequiresStory verifies that creating a task without a
// story_id is rejected — the MCP layer enforces this before the DB even sees it.
func TestHierarchy_TaskRequiresStory(t *testing.T) {
	s := setupMCP(t)

	// Try to create a task with epic_id but no story_id.
	errMsg := s.callExpectError(t, "amp_create_task", map[string]interface{}{
		"project_id":  float64(s.projectID),
		"epic_id":     float64(s.epicID),
		"name":        "orphan task",
		"description": "no story",
	})
	if !strings.Contains(errMsg, "story_id") {
		t.Errorf("expected error mentioning story_id, got: %s", errMsg)
	}
	t.Logf("orphan task (no story_id) rejected: %s ✓", errMsg)
}

// TestHierarchy_TaskRequiresEpic verifies that creating a task without an
// epic_id is also rejected.
func TestHierarchy_TaskRequiresEpic(t *testing.T) {
	s := setupMCP(t)

	errMsg := s.callExpectError(t, "amp_create_task", map[string]interface{}{
		"project_id":  float64(s.projectID),
		"story_id":    float64(s.storyID),
		"name":        "orphan task",
		"description": "no epic",
	})
	if !strings.Contains(errMsg, "epic_id") {
		t.Errorf("expected error mentioning epic_id, got: %s", errMsg)
	}
	t.Logf("orphan task (no epic_id) rejected: %s ✓", errMsg)
}

// TestHierarchy_StoryRequiresEpic verifies that creating a story without an
// epic_id is rejected.
func TestHierarchy_StoryRequiresEpic(t *testing.T) {
	s := setupMCP(t)

	errMsg := s.callExpectError(t, "amp_create_story", map[string]interface{}{
		"project_id": float64(s.projectID),
		"name":       "orphan story",
	})
	if !strings.Contains(errMsg, "epic_id") {
		t.Errorf("expected error mentioning epic_id, got: %s", errMsg)
	}
	t.Logf("orphan story (no epic_id) rejected: %s ✓", errMsg)
}

// TestHierarchy_TaskEpicMustMatchStoryEpic verifies that passing an epic_id
// that doesn't match the story's epic_id is rejected — prevents mis-parenting.
func TestHierarchy_TaskEpicMustMatchStoryEpic(t *testing.T) {
	s := setupMCP(t)

	// Create a second epic and story.
	epic2 := s.call(t, "amp_create_epic", map[string]interface{}{
		"project_id": float64(s.projectID),
		"name":       "second-epic",
	})
	epic2ID := intField(t, epic2, "id")

	// Try to create a task under epic2 but story belongs to epic1.
	errMsg := s.callExpectError(t, "amp_create_task", map[string]interface{}{
		"project_id":  float64(s.projectID),
		"epic_id":     float64(epic2ID), // wrong epic for this story
		"story_id":    float64(s.storyID),
		"name":        "mis-parented task",
		"description": "epic doesn't match story's epic",
	})
	if !strings.Contains(errMsg, "epic") {
		t.Errorf("expected error about epic mismatch, got: %s", errMsg)
	}
	t.Logf("mis-parented task (epic/story mismatch) rejected: %s ✓", errMsg)
}

// TestHierarchy_CrossProjectStoryRejected verifies that passing a story_id from
// a different project is rejected.
func TestHierarchy_CrossProjectStoryRejected(t *testing.T) {
	shared := setupShared(t)
	projA := shared.project(t, "project-a")
	projB := shared.project(t, "project-b")

	// Try to create a task in project A using story from project B.
	errMsg := projA.callExpectError(t, "amp_create_task", map[string]interface{}{
		"project_id":  float64(projA.projectID),
		"epic_id":     float64(projA.epicID),
		"story_id":    float64(projB.storyID), // wrong project
		"name":        "cross-project task",
		"description": "story is from another project",
	})
	if !strings.Contains(errMsg, "project") {
		t.Errorf("expected error about project mismatch, got: %s", errMsg)
	}
	t.Logf("cross-project story rejected: %s ✓", errMsg)
}

// TestHierarchy_CrossProjectEpicInStoryRejected verifies that passing an epic_id
// from a different project on amp_create_story is rejected.
func TestHierarchy_CrossProjectEpicInStoryRejected(t *testing.T) {
	shared := setupShared(t)
	projA := shared.project(t, "project-a")
	projB := shared.project(t, "project-b")

	// Try to create a story in project A using an epic from project B.
	errMsg := projA.callExpectError(t, "amp_create_story", map[string]interface{}{
		"project_id": float64(projA.projectID),
		"epic_id":    float64(projB.epicID), // wrong project
		"name":       "cross-project story",
	})
	if !strings.Contains(errMsg, "project") {
		t.Errorf("expected error about project mismatch, got: %s", errMsg)
	}
	t.Logf("cross-project epic in story rejected: %s ✓", errMsg)
}

// ---- Full hierarchy creation ----

// TestHierarchy_FullHierarchyViaM CP walks the complete creation flow that the
// manager skill prescribes: project → epic → story → task.
// Verifies IDs propagate correctly and all reads work.
func TestHierarchy_FullHierarchyViaMCP(t *testing.T) {
	s := setupMCP(t)

	// Verify the default hierarchy created by setupMCP is readable.
	epic := s.call(t, "amp_get_epic", map[string]interface{}{
		"epic_id": float64(s.epicID),
	})
	if intField(t, epic["epic"].(map[string]interface{}), "project_id") != s.projectID {
		t.Errorf("epic.project_id mismatch")
	}
	t.Logf("epic %d belongs to project %d ✓", s.epicID, s.projectID)

	story := s.call(t, "amp_get_story", map[string]interface{}{
		"story_id": float64(s.storyID),
	})
	storyMap := story["story"].(map[string]interface{})
	if intField(t, storyMap, "project_id") != s.projectID {
		t.Errorf("story.project_id mismatch")
	}
	if intField(t, storyMap, "epic_id") != s.epicID {
		t.Errorf("story.epic_id mismatch")
	}
	t.Logf("story %d belongs to epic %d in project %d ✓", s.storyID, s.epicID, s.projectID)

	// Create a task under the hierarchy.
	task := s.mkTask(t, "implement auth", map[string]interface{}{
		"description":         "## What to do\nAdd JWT auth.\n\n## Acceptance criteria\nPOST /login returns 200 with token.",
		"acceptance_criteria": "POST /login returns 200 with token. Protected routes return 401 without token.",
	})

	taskMap := task
	if intField(t, taskMap, "project_id") != s.projectID {
		t.Errorf("task.project_id mismatch")
	}
	if intField(t, taskMap, "epic_id") != s.epicID {
		t.Errorf("task.epic_id mismatch")
	}
	if intField(t, taskMap, "story_id") != s.storyID {
		t.Errorf("task.story_id mismatch")
	}
	taskID := intField(t, taskMap, "id")
	t.Logf("task %d: project=%d epic=%d story=%d ✓", taskID, s.projectID, s.epicID, s.storyID)

	// List tasks under the project — should include our task.
	list := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID)})
	if sliceLen(t, list, "ready_to_dispatch") != 1 {
		t.Errorf("want 1 task in ready_to_dispatch, got %d", sliceLen(t, list, "ready_to_dispatch"))
	}

	// List epics and stories.
	epics := s.call(t, "amp_list_epics", map[string]interface{}{"project_id": float64(s.projectID)})
	if sliceLen(t, epics, "epics") != 1 {
		t.Errorf("want 1 epic, got %d", sliceLen(t, epics, "epics"))
	}
	stories := s.call(t, "amp_list_stories", map[string]interface{}{"epic_id": float64(s.epicID)})
	if sliceLen(t, stories, "stories") != 1 {
		t.Errorf("want 1 story, got %d", sliceLen(t, stories, "stories"))
	}
	t.Log("full hierarchy created and readable via MCP ✓")
}

// ---- Ticket history / activity log ----

// TestHistory_FullActivityLog verifies that amp_get_ticket_history returns a
// complete chronological log of everything that happened on a ticket.
// This is the exact sequence an agent or manager would see.
func TestHistory_FullActivityLog(t *testing.T) {
	s := setupMCP(t)

	// Manager creates a task with rich context (the ticket IS the prompt).
	task := s.mkTask(t, "implement user registration", map[string]interface{}{
		"description": `## What to do
Add a POST /register endpoint.

## Context
We are building an auth system. The DB schema is already set up (task 1 done).

## Where to look
- internal/handler/auth.go — add the handler here
- internal/repo/user.go — User.Create() is already implemented

## Acceptance criteria
- POST /register with {email, password} creates a user and returns 201
- Duplicate email returns 409
- Missing fields return 400

## Gotchas
- Password must be hashed with bcrypt before storage
- Email must be lowercased before uniqueness check`,
		"acceptance_criteria": "POST /register returns 201. Duplicate returns 409. Missing fields return 400.",
		"priority":            "2",
	})
	taskID := intField(t, task, "id")
	t.Logf("task %d created by manager", taskID)

	// Manager dispatches to a worker agent.
	s.call(t, "amp_dispatch_task", map[string]interface{}{
		"task_id": float64(taskID), "agent_id": "amp-worker-1",
	})

	// Worker reads the ticket (simulated — we just verify the content is there).
	readTask := s.call(t, "amp_get_task", map[string]interface{}{"task_id": float64(taskID)})
	taskDetail := readTask["task"].(map[string]interface{})
	desc := taskDetail["description"].(string)
	if !strings.Contains(desc, "POST /register") {
		t.Errorf("task description should contain instructions, got: %s", desc[:50])
	}
	if !strings.Contains(taskDetail["acceptance_criteria"].(string), "201") {
		t.Errorf("acceptance_criteria should be set")
	}
	t.Log("worker read ticket — description and acceptance_criteria present ✓")

	// Worker posts starting comment.
	s.call(t, "amp_add_task_comment", map[string]interface{}{
		"task_id": float64(taskID),
		"body": `Starting work.
Plan:
1. Read internal/handler/auth.go
2. Add POST /register handler
3. Hash password with bcrypt
4. Write tests`,
		"author": "amp-worker-1",
	})

	// Worker posts a finding.
	s.call(t, "amp_add_task_comment", map[string]interface{}{
		"task_id": float64(taskID),
		"body":    "Finding: internal/handler/auth.go already has a Login handler. Adding Register handler next to it.",
		"author":  "amp-worker-1",
	})

	// Worker posts completion summary.
	s.call(t, "amp_add_task_comment", map[string]interface{}{
		"task_id": float64(taskID),
		"body": `Work complete.
Files changed:
- internal/handler/auth.go: added RegisterHandler
- internal/handler/auth_test.go: added 3 test cases

Acceptance criteria:
- POST /register returns 201: VERIFIED via test
- Duplicate returns 409: VERIFIED via test
- Missing fields return 400: VERIFIED via test`,
		"author": "amp-worker-1",
	})

	// Worker completes the task.
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(taskID)})

	// ---- Read the full ticket history ----
	history := s.call(t, "amp_get_ticket_history", map[string]interface{}{"task_id": float64(taskID)})

	entries, _ := history["history"].([]interface{})
	count := intField(t, history, "count")
	t.Logf("ticket history has %d entries:", count)

	actions := make([]string, 0, len(entries))
	actors := make([]string, 0, len(entries))
	for _, e := range entries {
		entry := e.(map[string]interface{})
		action := entry["action"].(string)
		actor := entry["actor"].(string)
		actions = append(actions, action)
		actors = append(actors, actor)
		t.Logf("  [%s] %s → %s | %s",
			actor, entry["from_state"], entry["to_state"], action)
	}

	// Verify the expected sequence of events.
	if count < 5 {
		t.Errorf("want at least 5 history entries (created, dispatched, 3 comments, completed), got %d", count)
	}

	// First entry must be "created" by system.
	if len(actions) > 0 && actions[0] != "created" {
		t.Errorf("first entry should be 'created', got %q", actions[0])
	}

	// Must contain a "dispatched" entry.
	hasDispatched := false
	for _, a := range actions {
		if a == "dispatched" {
			hasDispatched = true
		}
	}
	if !hasDispatched {
		t.Error("history should contain a 'dispatched' entry")
	}

	// Must contain comment entries.
	commentCount := 0
	for _, a := range actions {
		if a == "comment" {
			commentCount++
		}
	}
	if commentCount < 3 {
		t.Errorf("want at least 3 comment entries, got %d", commentCount)
	}

	// Must contain a "completed" entry.
	hasCompleted := false
	for _, a := range actions {
		if a == "completed" {
			hasCompleted = true
		}
	}
	if !hasCompleted {
		t.Error("history should contain a 'completed' entry")
	}

	// Worker must appear as actor in comments.
	hasWorker := false
	for _, a := range actors {
		if a == "amp-worker-1" {
			hasWorker = true
		}
	}
	if !hasWorker {
		t.Error("amp-worker-1 should appear in history actors")
	}

	t.Logf("ticket history: %d entries, actions=%v ✓", count, actions)

	// The task data is also returned with history — worker can read context + log in one call.
	historyTask := history["task"].(map[string]interface{})
	if intField(t, historyTask, "id") != taskID {
		t.Errorf("history task id mismatch")
	}
	t.Log("ticket history includes full task context ✓")
}

// TestHistory_UnblockAppearsInLog verifies that auto-unblock events are recorded
// in the activity log — so the manager can see when and why a task became ready.
func TestHistory_UnblockAppearsInLog(t *testing.T) {
	s := setupMCP(t)

	dep := s.mkTask(t, "dep-task", nil)
	depID := intField(t, dep, "id")

	blocked := s.mkTask(t, "blocked-task", map[string]interface{}{
		"dependency_ids": []interface{}{float64(depID)},
	})
	blockedID := intField(t, blocked, "id")

	// Dispatch and complete the dep.
	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(depID), "agent_id": "worker"})
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(depID)})

	// Read history of the blocked task — should show "created" (blocked) then "unblocked".
	history := s.call(t, "amp_get_ticket_history", map[string]interface{}{"task_id": float64(blockedID)})
	entries, _ := history["history"].([]interface{})

	actions := make([]string, 0)
	for _, e := range entries {
		entry := e.(map[string]interface{})
		actions = append(actions, entry["action"].(string))
	}

	hasUnblocked := false
	for _, a := range actions {
		if a == "unblocked" {
			hasUnblocked = true
		}
	}
	if !hasUnblocked {
		t.Errorf("blocked task history should contain 'unblocked' entry, got: %v", actions)
	}
	t.Logf("unblock event appears in ticket history: %v ✓", actions)
}
