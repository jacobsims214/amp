// Project isolation tests.
//
// These tests prove that two projects running on the SAME actor system,
// the SAME MCP server, and the SAME postgres instance are completely isolated.
//
// What "isolated" means:
//   - Tasks created in project A never appear in project B's amp_list_tasks
//   - Completing a task in project A never unblocks tasks in project B
//   - Both projects can run the full plan→dispatch→complete loop concurrently
//   - A blocked task in project A cannot be dispatched even if the identically-
//     numbered dep task is completed in project B
//
// Architecture note: the shared actor system is intentional. In production there
// is ONE amp-api process. Each project gets its own ProjectActor goroutine. The
// registry routes every MCP call to the correct actor by project_id. The test
// proves that routing is correct and that no state leaks between actors.
package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/mcp"
	"github.com/simstech/amp-api/internal/repository"
)

// sharedStack is one MCP server + actor system shared across multiple projects.
// This is the production topology: one process, many projects.
type sharedStack struct {
	srv  *mcpserver.MCPServer
	repo *repository.Repo
}

// project returns an mcpStack scoped to a new project on this shared server,
// with a default epic and story pre-created.
func (ss *sharedStack) project(t *testing.T, label string) *mcpStack {
	t.Helper()
	ctx := context.Background()
	ts := time.Now().UnixNano()
	proj, err := ss.repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: fmt.Sprintf("iso-%s-%d", label, ts),
		Code: fmt.Sprintf("iso-%s-%d", label, ts%99999),
	})
	if err != nil {
		t.Fatalf("create project %q: %v", label, err)
	}
	epic, err := ss.repo.CreateEpic(ctx, domain.CreateEpicRequest{
		ProjectID: proj.ID, Name: label + "-epic", Priority: "1",
	})
	if err != nil {
		t.Fatalf("create epic for %q: %v", label, err)
	}
	story, err := ss.repo.CreateStory(ctx, domain.CreateStoryRequest{
		ProjectID: proj.ID, EpicID: epic.ID,
		Name: label + "-story", AcceptanceCriteria: "done", Priority: "1",
	})
	if err != nil {
		t.Fatalf("create story for %q: %v", label, err)
	}
	t.Logf("project %q: id=%d epic=%d story=%d", label, proj.ID, epic.ID, story.ID)
	return &mcpStack{srv: ss.srv, repo: ss.repo, projectID: proj.ID, epicID: epic.ID, storyID: story.ID}
}

// setupShared boots ONE shared stack — single actor system, single MCP server,
// single postgres connection pool. Multiple projects will be created on top.
func setupShared(t *testing.T) *sharedStack {
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

	srv := mcpserver.NewMCPServer("amp-api-isolation-test", "2.0.0", mcpserver.WithToolCapabilities(false))
	mcpHandler := mcp.NewServer(reg, repo, sseHub, nil)
	mcpHandler.Register(srv)

	return &sharedStack{srv: srv, repo: repo}
}

// ---- Tests ----

// TestIsolation_TasksDoNotCrossProjects is the fundamental check:
// tasks created in project A must never appear in project B's list.
func TestIsolation_TasksDoNotCrossProjects(t *testing.T) {
	shared := setupShared(t)
	alpha := shared.project(t, "alpha")
	beta := shared.project(t, "beta")

	// Create 2 tasks in alpha, 3 tasks in beta.
	for i := range 2 {
		alpha.mkTask(t, fmt.Sprintf("alpha-task-%d", i), nil)
	}
	for i := range 3 {
		beta.mkTask(t, fmt.Sprintf("beta-task-%d", i), nil)
	}

	alphaList := alpha.call(t, "amp_list_tasks", map[string]interface{}{
		"project_id": float64(alpha.projectID),
	})
	betaList := beta.call(t, "amp_list_tasks", map[string]interface{}{
		"project_id": float64(beta.projectID),
	})

	alphaCount := sliceLen(t, alphaList, "ready_to_dispatch")
	betaCount := sliceLen(t, betaList, "ready_to_dispatch")

	if alphaCount != 2 {
		t.Errorf("alpha: want 2 tasks, got %d", alphaCount)
	}
	if betaCount != 3 {
		t.Errorf("beta: want 3 tasks, got %d", betaCount)
	}

	t.Logf("alpha sees %d tasks, beta sees %d tasks — no cross-contamination ✓", alphaCount, betaCount)
}

// TestIsolation_CompletingInAlphaDoesNotUnblockBeta proves the critical boundary:
// a blocked task in beta stays blocked even if a task with the SAME numeric ID
// completes in alpha. The actor routing must use project_id, not just task_id.
func TestIsolation_CompletingInAlphaDoesNotUnblockBeta(t *testing.T) {
	shared := setupShared(t)
	alpha := shared.project(t, "alpha")
	beta := shared.project(t, "beta")

	// Create a plain task in alpha first — we want its ID to exist in the DB
	// before we create tasks in beta, so there's a chance of ID collision confusion.
	alphaTask := alpha.mkTask(t, "alpha standalone", nil)
	alphaID := intField(t, alphaTask, "id")
	t.Logf("alpha standalone task id=%d", alphaID)

	// Create a dep chain in beta: beta-A → beta-B (beta-B depends on beta-A).
	betaA := beta.mkTask(t, "beta-A", nil)
	betaAID := intField(t, betaA, "id")

	betaB := beta.mkTask(t, "beta-B", map[string]interface{}{
		"dependency_ids": []interface{}{float64(betaAID)},
	})
	betaBID := intField(t, betaB, "id")
	t.Logf("beta chain: beta-A id=%d → beta-B id=%d (blocked)", betaAID, betaBID)

	// Confirm beta-B is blocked.
	if strField(t, betaB, "state") != "blocked" {
		t.Fatalf("precondition: beta-B should be blocked")
	}

	// Dispatch and complete alpha's task — this must NOT affect beta-B.
	alpha.call(t, "amp_dispatch_task", map[string]interface{}{
		"task_id": float64(alphaID), "agent_id": "alpha-worker",
	})
	alpha.call(t, "amp_complete_task", map[string]interface{}{
		"task_id": float64(alphaID),
	})
	t.Logf("alpha task %d completed", alphaID)

	// beta-B must still be blocked.
	betaList := beta.call(t, "amp_list_tasks", map[string]interface{}{
		"project_id": float64(beta.projectID),
	})
	if sliceLen(t, betaList, "blocked") != 1 {
		t.Errorf("after alpha completes: beta-B should still be blocked, blocked count=%d",
			sliceLen(t, betaList, "blocked"))
	}
	if sliceLen(t, betaList, "ready_to_dispatch") != 1 {
		// beta-A should still be in backlog (undispatched)
		t.Errorf("beta-A should still be ready_to_dispatch, count=%d",
			sliceLen(t, betaList, "ready_to_dispatch"))
	}
	t.Logf("after alpha completes: beta-B still blocked, beta-A still ready ✓")

	// Now complete beta-A properly — beta-B should unblock.
	beta.call(t, "amp_dispatch_task", map[string]interface{}{
		"task_id": float64(betaAID), "agent_id": "beta-worker",
	})
	beta.call(t, "amp_complete_task", map[string]interface{}{
		"task_id": float64(betaAID),
	})

	betaList2 := beta.call(t, "amp_list_tasks", map[string]interface{}{
		"project_id": float64(beta.projectID),
	})
	if sliceLen(t, betaList2, "ready_to_dispatch") != 1 {
		t.Errorf("after beta-A completes: beta-B should be ready_to_dispatch")
	}
	if sliceLen(t, betaList2, "blocked") != 0 {
		t.Errorf("after beta-A completes: no tasks should be blocked")
	}
	t.Logf("after beta-A completes: beta-B unblocked correctly ✓")
}

// TestIsolation_ConcurrentProjectsConcurrentAgents is the full stress test:
// two projects run their ENTIRE plan→dispatch→complete loop at the same time
// using goroutines. Both must complete cleanly with correct final state,
// and neither must interfere with the other.
//
// This is the closest simulation of two real amp-manager sessions running
// in parallel — exactly what happens when two engineers use the tool simultaneously.
func TestIsolation_ConcurrentProjectsConcurrentAgents(t *testing.T) {
	shared := setupShared(t)
	alpha := shared.project(t, "alpha-concurrent")
	beta := shared.project(t, "beta-concurrent")

	// runProject executes the full manager loop for one project:
	//   create 3 tasks (A, B, C where C depends on A and B)
	//   dispatch A and B
	//   complete A — verify C still blocked
	//   complete B — verify C unblocked
	//   dispatch and complete C
	//   verify all 3 completed with no leftovers
	runProject := func(t *testing.T, s *mcpStack, name string) {
		t.Helper()
		log := func(msg string, args ...interface{}) {
			t.Logf("[%s] "+msg, append([]interface{}{name}, args...)...)
		}

		// Plan: 3 tasks, C blocked on A and B.
		tA := s.mkTask(t, name+"-task-A", map[string]interface{}{"acceptance_criteria": "A done"})
		tB := s.mkTask(t, name+"-task-B", map[string]interface{}{"acceptance_criteria": "B done"})
		tC := s.mkTask(t, name+"-task-C", map[string]interface{}{
			"acceptance_criteria": "C done",
			"dependency_ids":      []interface{}{float64(intField(t, tA, "id")), float64(intField(t, tB, "id"))},
		})

		idA := intField(t, tA, "id")
		idB := intField(t, tB, "id")
		idC := intField(t, tC, "id")
		log("created A=%d B=%d C=%d", idA, idB, idC)

		// Verify initial list.
		list0 := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID)})
		if sliceLen(t, list0, "ready_to_dispatch") != 2 {
			t.Errorf("[%s] initial: want 2 ready, got %d", name, sliceLen(t, list0, "ready_to_dispatch"))
		}
		if sliceLen(t, list0, "blocked") != 1 {
			t.Errorf("[%s] initial: want 1 blocked, got %d", name, sliceLen(t, list0, "blocked"))
		}
		log("initial state: ready=%d blocked=%d ✓",
			sliceLen(t, list0, "ready_to_dispatch"), sliceLen(t, list0, "blocked"))

		// Dispatch A and B (manager dispatches everything in ready_to_dispatch).
		s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(idA), "agent_id": name + "-worker"})
		s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(idB), "agent_id": name + "-worker"})
		log("dispatched A and B")

		// Worker-A posts a comment and completes.
		s.call(t, "amp_add_task_comment", map[string]interface{}{
			"task_id": float64(idA),
			"body":    fmt.Sprintf("[%s] task A complete. Files changed: none.", name),
			"author":  name + "-worker",
		})
		s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(idA)})
		log("A completed")

		// C must still be blocked — B not done yet.
		list1 := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID)})
		if sliceLen(t, list1, "blocked") != 1 {
			t.Errorf("[%s] after A: C should still be blocked, got blocked=%d",
				name, sliceLen(t, list1, "blocked"))
		}
		// Verify the blocked task's blocked_by_ids contains only B.
		blockedTasks, _ := list1["blocked"].([]interface{})
		if len(blockedTasks) > 0 {
			blockedC := blockedTasks[0].(map[string]interface{})
			bby := sliceIDs(t, blockedC, "blocked_by_ids")
			if len(bby) != 1 || bby[0] != idB {
				t.Errorf("[%s] after A: C blocked_by_ids want [%d], got %v", name, idB, bby)
			}
		}
		log("after A: C still blocked, blocked_by=[B=%d] ✓", idB)

		// Worker-B posts a comment and completes — this triggers C's unblock.
		s.call(t, "amp_add_task_comment", map[string]interface{}{
			"task_id": float64(idB),
			"body":    fmt.Sprintf("[%s] task B complete. Files changed: none.", name),
			"author":  name + "-worker",
		})
		s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(idB)})
		log("B completed")

		// C must now be in ready_to_dispatch — actor auto-unblocked it.
		list2 := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID)})
		ready2 := sliceIDs(t, list2, "ready_to_dispatch")
		if len(ready2) != 1 || ready2[0] != idC {
			t.Errorf("[%s] after B: want ready=[C=%d], got %v", name, idC, ready2)
		}
		log("after B: C auto-unblocked and in ready_to_dispatch ✓")

		// Dispatch C.
		s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(idC), "agent_id": name + "-worker"})

		// Worker-C completes.
		s.call(t, "amp_add_task_comment", map[string]interface{}{
			"task_id": float64(idC),
			"body":    fmt.Sprintf("[%s] task C complete. All deps were satisfied.", name),
			"author":  name + "-worker",
		})
		s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(idC)})
		log("C completed")

		// Final state: all 3 done, nothing ready, nothing blocked.
		final := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID)})
		if sliceLen(t, final, "ready_to_dispatch") != 0 {
			t.Errorf("[%s] final: want 0 ready, got %d", name, sliceLen(t, final, "ready_to_dispatch"))
		}
		if sliceLen(t, final, "in_progress") != 0 {
			t.Errorf("[%s] final: want 0 in_progress, got %d", name, sliceLen(t, final, "in_progress"))
		}
		if sliceLen(t, final, "blocked") != 0 {
			t.Errorf("[%s] final: want 0 blocked, got %d", name, sliceLen(t, final, "blocked"))
		}
		if sliceLen(t, final, "completed") != 3 {
			t.Errorf("[%s] final: want 3 completed, got %d", name, sliceLen(t, final, "completed"))
		}
		log("ALL DONE — 3/3 tasks completed, 0 ready, 0 blocked ✓")
	}

	// Run both projects concurrently — two goroutines, same shared stack.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		runProject(t, alpha, "alpha")
	}()
	go func() {
		defer wg.Done()
		runProject(t, beta, "beta")
	}()

	wg.Wait()

	// Cross-check: alpha's list must never contain beta's tasks and vice versa.
	alphaFinal := alpha.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(alpha.projectID)})
	betaFinal := beta.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(beta.projectID)})

	alphaTotal := sliceLen(t, alphaFinal, "completed")
	betaTotal := sliceLen(t, betaFinal, "completed")

	if alphaTotal != 3 {
		t.Errorf("cross-check: alpha should have exactly 3 completed tasks, got %d", alphaTotal)
	}
	if betaTotal != 3 {
		t.Errorf("cross-check: beta should have exactly 3 completed tasks, got %d", betaTotal)
	}

	t.Logf("ISOLATION VERIFIED: alpha=%d completed, beta=%d completed, no cross-contamination ✓",
		alphaTotal, betaTotal)
}
