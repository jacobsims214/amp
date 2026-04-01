// Integration tests for amp-api.
//
// These tests spin up the full stack against a real postgres database:
//
//	real actor system → real repo → real postgres
//
// They do NOT use mocks. If a test fails it means something is actually broken.
//
// Run with:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./... -v -run Integration -count=1
package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/repository"
)

// testDSN returns the test database DSN, skipping if not set.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable"
	}
	return dsn
}

// testStack boots the full stack against the test DB, runs fn, then tears down.
// Each test gets a fresh project so tests don't interfere.
func testStack(t *testing.T, fn func(reg *actor.Registry, repo *repository.Repo, projectID int)) {
	t.Helper()
	ctx := context.Background()

	repo, err := repository.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connect to postgres: %v\nIs postgres running? Set TEST_DATABASE_URL env var.", err)
	}
	t.Cleanup(repo.Close)

	if err := repo.Migrate(ctx, migrationSQL); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sseHub := hub.New()
	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)

	reg := actor.NewRegistry(system, repo, sseHub)

	// Create a unique project for this test run so tests are isolated.
	// Use t.Name() + nanos so parallel tests never collide on the code unique constraint.
	ts := time.Now().UnixNano()
	proj, err := repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: fmt.Sprintf("test-project-%s-%d", t.Name(), ts),
		Code: fmt.Sprintf("tp-%d", ts),
	})
	if err != nil {
		t.Fatalf("create test project: %v", err)
	}

	fn(reg, repo, proj.ID)
}

// send is a helper that sends a message to the project actor and waits for the reply.
func sendMsg[R any](t *testing.T, reg *actor.Registry, projectID int, msg interface{}, replyCh chan R) R {
	t.Helper()
	pid, err := reg.Get(projectID)
	if err != nil {
		t.Fatalf("get actor for project %d: %v", projectID, err)
	}
	reg.System().Root.Send(pid, msg)
	select {
	case r := <-replyCh:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("actor did not reply within 5s")
		var zero R
		return zero
	}
}

// testHierarchy creates an epic and story for a project and returns their IDs.
// All integration tests that create tasks must use this — epic_id and story_id
// are now required at the DB level, not optional.
func testHierarchy(t *testing.T, repo *repository.Repo, projectID int) (epicID, storyID int) {
	t.Helper()
	ctx := context.Background()
	epic, err := repo.CreateEpic(ctx, domain.CreateEpicRequest{
		ProjectID: projectID, Name: "test-epic", Priority: "1",
	})
	if err != nil {
		t.Fatalf("create test epic: %v", err)
	}
	story, err := repo.CreateStory(ctx, domain.CreateStoryRequest{
		ProjectID: projectID, EpicID: epic.ID,
		Name: "test-story", AcceptanceCriteria: "done", Priority: "1",
	})
	if err != nil {
		t.Fatalf("create test story: %v", err)
	}
	return epic.ID, story.ID
}

// ---- Tests ----

// TestIntegration_CreateTask_NoDepStartsBacklog verifies the most basic rule:
// a task with no dependencies is immediately ready to dispatch.
func TestIntegration_CreateTask_NoDepStartsBacklog(t *testing.T) {
	testStack(t, func(reg *actor.Registry, repo *repository.Repo, projectID int) {
		epicID, storyID := testHierarchy(t, repo, projectID)
		replyCh := make(chan actor.ReplyCreateTask, 1)
		reply := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
			Req: domain.CreateTaskRequest{
				ProjectID: projectID, EpicID: epicID, StoryID: storyID,
				Name: "standalone task", Description: "no deps",
			},
			ReplyCh: replyCh,
		}, replyCh)

		if reply.Err != nil {
			t.Fatalf("create task: %v", reply.Err)
		}
		if reply.Task.State != domain.TaskStateBacklog {
			t.Errorf("want state=backlog, got %q", reply.Task.State)
		}
		if len(reply.Task.BlockedByIDs) != 0 {
			t.Errorf("want no blocked_by_ids, got %v", reply.Task.BlockedByIDs)
		}
	})
}

// TestIntegration_CreateTask_WithDepStartsBlocked verifies the core DAG rule:
// a task whose dependency is not yet complete starts blocked.
func TestIntegration_CreateTask_WithDepStartsBlocked(t *testing.T) {
	testStack(t, func(reg *actor.Registry, repo *repository.Repo, projectID int) {
		epicID, storyID := testHierarchy(t, repo, projectID)
		// Create dep task first.
		ch1 := make(chan actor.ReplyCreateTask, 1)
		r1 := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
			Req:     domain.CreateTaskRequest{ProjectID: projectID, EpicID: epicID, StoryID: storyID, Name: "dep task"},
			ReplyCh: ch1,
		}, ch1)
		if r1.Err != nil {
			t.Fatalf("create dep task: %v", r1.Err)
		}
		depID := r1.Task.ID

		// Create dependent task.
		ch2 := make(chan actor.ReplyCreateTask, 1)
		r2 := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
			Req: domain.CreateTaskRequest{
				ProjectID: projectID, EpicID: epicID, StoryID: storyID,
				Name: "dependent task", DependencyIDs: []int{depID},
			},
			ReplyCh: ch2,
		}, ch2)
		if r2.Err != nil {
			t.Fatalf("create dependent task: %v", r2.Err)
		}

		if r2.Task.State != domain.TaskStateBlocked {
			t.Errorf("want state=blocked, got %q", r2.Task.State)
		}
		if len(r2.Task.BlockedByIDs) != 1 || r2.Task.BlockedByIDs[0] != depID {
			t.Errorf("want blocked_by_ids=[%d], got %v", depID, r2.Task.BlockedByIDs)
		}
	})
}

// TestIntegration_CompleteDepUnblocksDependent is THE critical test:
// completing a dependency must automatically move the dependent task to backlog.
func TestIntegration_CompleteDepUnblocksDependent(t *testing.T) {
	testStack(t, func(reg *actor.Registry, repo *repository.Repo, projectID int) {
		epicID, storyID := testHierarchy(t, repo, projectID)
		// Task A — no deps, goes to backlog.
		chA := make(chan actor.ReplyCreateTask, 1)
		rA := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
			Req:     domain.CreateTaskRequest{ProjectID: projectID, EpicID: epicID, StoryID: storyID, Name: "task A"},
			ReplyCh: chA,
		}, chA)
		if rA.Err != nil {
			t.Fatalf("create A: %v", rA.Err)
		}
		taskA := rA.Task.ID

		// Task B — depends on A, starts blocked.
		chB := make(chan actor.ReplyCreateTask, 1)
		rB := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
			Req: domain.CreateTaskRequest{
				ProjectID: projectID, EpicID: epicID, StoryID: storyID,
				Name: "task B", DependencyIDs: []int{taskA},
			},
			ReplyCh: chB,
		}, chB)
		if rB.Err != nil {
			t.Fatalf("create B: %v", rB.Err)
		}
		taskB := rB.Task.ID

		if rB.Task.State != domain.TaskStateBlocked {
			t.Fatalf("precondition: task B should be blocked, got %q", rB.Task.State)
		}

		// Dispatch A.
		chDispatch := make(chan actor.ReplySimple, 1)
		rDispatch := sendMsg(t, reg, projectID, &actor.MsgDispatchTask{
			TaskID: taskA, AgentID: "test-agent", ReplyCh: chDispatch,
		}, chDispatch)
		if rDispatch.Err != nil {
			t.Fatalf("dispatch A: %v", rDispatch.Err)
		}

		// Complete A — this should auto-unblock B.
		chComplete := make(chan actor.ReplySimple, 1)
		rComplete := sendMsg(t, reg, projectID, &actor.MsgCompleteTask{
			TaskID: taskA, ReplyCh: chComplete,
		}, chComplete)
		if rComplete.Err != nil {
			t.Fatalf("complete A: %v", rComplete.Err)
		}

		// Read B back — must be backlog now, blocked_by_ids must be empty.
		chGet := make(chan actor.ReplyGetTask, 1)
		rGet := sendMsg(t, reg, projectID, &actor.MsgGetTask{
			TaskID: taskB, ReplyCh: chGet,
		}, chGet)
		if rGet.Err != nil {
			t.Fatalf("get B: %v", rGet.Err)
		}

		if rGet.Task.State != domain.TaskStateBacklog {
			t.Errorf("want task B state=backlog after A completes, got %q", rGet.Task.State)
		}
		if len(rGet.Task.BlockedByIDs) != 0 {
			t.Errorf("want task B blocked_by_ids=[], got %v", rGet.Task.BlockedByIDs)
		}
	})
}

// TestIntegration_DiamondDAG tests a diamond dependency graph:
//
//	  A
//	 / \
//	B   C
//	 \ /
//	  D
//
// D must not unblock until both B and C complete.
func TestIntegration_DiamondDAG(t *testing.T) {
	testStack(t, func(reg *actor.Registry, repo *repository.Repo, projectID int) {
		epicID, storyID := testHierarchy(t, repo, projectID)
		create := func(name string, deps []int) int {
			ch := make(chan actor.ReplyCreateTask, 1)
			r := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
				Req: domain.CreateTaskRequest{
					ProjectID: projectID, EpicID: epicID, StoryID: storyID,
					Name: name, DependencyIDs: deps,
				},
				ReplyCh: ch,
			}, ch)
			if r.Err != nil {
				t.Fatalf("create %q: %v", name, r.Err)
			}
			return r.Task.ID
		}
		dispatch := func(id int) {
			ch := make(chan actor.ReplySimple, 1)
			r := sendMsg(t, reg, projectID, &actor.MsgDispatchTask{
				TaskID: id, AgentID: "agent", ReplyCh: ch,
			}, ch)
			if r.Err != nil {
				t.Fatalf("dispatch %d: %v", id, r.Err)
			}
		}
		complete := func(id int) {
			ch := make(chan actor.ReplySimple, 1)
			r := sendMsg(t, reg, projectID, &actor.MsgCompleteTask{
				TaskID: id, ReplyCh: ch,
			}, ch)
			if r.Err != nil {
				t.Fatalf("complete %d: %v", id, r.Err)
			}
		}
		getState := func(id int) (domain.TaskState, []int) {
			ch := make(chan actor.ReplyGetTask, 1)
			r := sendMsg(t, reg, projectID, &actor.MsgGetTask{TaskID: id, ReplyCh: ch}, ch)
			if r.Err != nil {
				t.Fatalf("get %d: %v", id, r.Err)
			}
			return r.Task.State, r.Task.BlockedByIDs
		}

		a := create("A", nil)
		b := create("B", []int{a})
		c := create("C", []int{a})
		d := create("D", []int{b, c})

		// Verify initial states.
		if s, _ := getState(a); s != domain.TaskStateBacklog {
			t.Errorf("A: want backlog, got %q", s)
		}
		if s, _ := getState(b); s != domain.TaskStateBlocked {
			t.Errorf("B: want blocked, got %q", s)
		}
		if s, _ := getState(c); s != domain.TaskStateBlocked {
			t.Errorf("C: want blocked, got %q", s)
		}
		if s, _ := getState(d); s != domain.TaskStateBlocked {
			t.Errorf("D: want blocked, got %q", s)
		}

		// Complete A → B and C should unblock.
		dispatch(a)
		complete(a)

		if s, _ := getState(b); s != domain.TaskStateBacklog {
			t.Errorf("after A done: B want backlog, got %q", s)
		}
		if s, _ := getState(c); s != domain.TaskStateBacklog {
			t.Errorf("after A done: C want backlog, got %q", s)
		}
		// D still blocked on both B and C.
		if s, bby := getState(d); s != domain.TaskStateBlocked {
			t.Errorf("after A done: D want blocked, got %q", s)
		} else if len(bby) != 2 {
			t.Errorf("after A done: D blocked_by_ids want 2, got %v", bby)
		}

		// Complete B → D still blocked on C.
		dispatch(b)
		complete(b)
		if s, bby := getState(d); s != domain.TaskStateBlocked {
			t.Errorf("after B done: D want blocked, got %q", s)
		} else if len(bby) != 1 || bby[0] != c {
			t.Errorf("after B done: D blocked_by_ids want [%d], got %v", c, bby)
		}

		// Complete C → D finally unblocks.
		dispatch(c)
		complete(c)
		if s, bby := getState(d); s != domain.TaskStateBacklog {
			t.Errorf("after C done: D want backlog, got %q", s)
		} else if len(bby) != 0 {
			t.Errorf("after C done: D blocked_by_ids want [], got %v", bby)
		}
	})
}

// TestIntegration_DispatchBlockedTaskFails verifies an agent cannot accidentally
// dispatch a blocked task — the actor must return an error.
func TestIntegration_DispatchBlockedTaskFails(t *testing.T) {
	testStack(t, func(reg *actor.Registry, repo *repository.Repo, projectID int) {
		epicID, storyID := testHierarchy(t, repo, projectID)
		// Create blocker.
		ch1 := make(chan actor.ReplyCreateTask, 1)
		r1 := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
			Req:     domain.CreateTaskRequest{ProjectID: projectID, EpicID: epicID, StoryID: storyID, Name: "blocker"},
			ReplyCh: ch1,
		}, ch1)
		blockerID := r1.Task.ID

		// Create blocked task.
		ch2 := make(chan actor.ReplyCreateTask, 1)
		r2 := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
			Req: domain.CreateTaskRequest{
				ProjectID: projectID, EpicID: epicID, StoryID: storyID,
				Name: "blocked", DependencyIDs: []int{blockerID},
			},
			ReplyCh: ch2,
		}, ch2)
		blockedID := r2.Task.ID

		// Try to dispatch the blocked task — must fail.
		chDispatch := make(chan actor.ReplySimple, 1)
		rDispatch := sendMsg(t, reg, projectID, &actor.MsgDispatchTask{
			TaskID: blockedID, AgentID: "test-agent", ReplyCh: chDispatch,
		}, chDispatch)

		if rDispatch.Err == nil {
			t.Error("expected error dispatching a blocked task, got nil")
		} else {
			t.Logf("got expected error: %v", rDispatch.Err)
		}
	})
}

// TestIntegration_CommentsPersistedAndReadBack verifies comments survive
// round-trip through the actor and postgres.
func TestIntegration_CommentsPersistedAndReadBack(t *testing.T) {
	testStack(t, func(reg *actor.Registry, repo *repository.Repo, projectID int) {
		epicID, storyID := testHierarchy(t, repo, projectID)
		// Create a task.
		chCreate := make(chan actor.ReplyCreateTask, 1)
		rCreate := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
			Req:     domain.CreateTaskRequest{ProjectID: projectID, EpicID: epicID, StoryID: storyID, Name: "commented task"},
			ReplyCh: chCreate,
		}, chCreate)
		taskID := rCreate.Task.ID

		// Add two comments.
		for i, body := range []string{"starting work", "found a bug"} {
			chComment := make(chan actor.ReplyAddComment, 1)
			rComment := sendMsg(t, reg, projectID, &actor.MsgAddComment{
				Req:     domain.AddCommentRequest{TaskID: taskID, Body: body, Author: "test-agent"},
				ReplyCh: chComment,
			}, chComment)
			if rComment.Err != nil {
				t.Fatalf("add comment %d: %v", i, rComment.Err)
			}
		}

		// Read them back.
		chGet := make(chan actor.ReplyGetComments, 1)
		rGet := sendMsg(t, reg, projectID, &actor.MsgGetComments{
			TaskID: taskID, ReplyCh: chGet,
		}, chGet)
		if rGet.Err != nil {
			t.Fatalf("get comments: %v", rGet.Err)
		}
		if len(rGet.Comments) != 2 {
			t.Errorf("want 2 comments, got %d", len(rGet.Comments))
		}
		if rGet.Comments[0].Body != "starting work" {
			t.Errorf("comment 0 body: want %q, got %q", "starting work", rGet.Comments[0].Body)
		}
	})
}

// TestIntegration_ListTasksBucketing verifies that amp_list_tasks returns the
// correct buckets — the thing the manager reads to decide what to dispatch.
func TestIntegration_ListTasksBucketing(t *testing.T) {
	testStack(t, func(reg *actor.Registry, repo *repository.Repo, projectID int) {
		epicID, storyID := testHierarchy(t, repo, projectID)
		create := func(name string, deps []int) int {
			ch := make(chan actor.ReplyCreateTask, 1)
			r := sendMsg(t, reg, projectID, &actor.MsgCreateTask{
				Req: domain.CreateTaskRequest{
					ProjectID: projectID, EpicID: epicID, StoryID: storyID,
					Name: name, DependencyIDs: deps,
				},
				ReplyCh: ch,
			}, ch)
			if r.Err != nil {
				t.Fatalf("create %q: %v", name, r.Err)
			}
			return r.Task.ID
		}

		t1 := create("free task 1", nil)
		t2 := create("free task 2", nil)
		t3 := create("blocked task", []int{t1})

		// Dispatch t1 so it goes in_progress.
		chD := make(chan actor.ReplySimple, 1)
		sendMsg(t, reg, projectID, &actor.MsgDispatchTask{
			TaskID: t1, AgentID: "agent", ReplyCh: chD,
		}, chD)

		// List all tasks.
		chList := make(chan actor.ReplyListTasks, 1)
		rList := sendMsg(t, reg, projectID, &actor.MsgListTasks{
			ProjectID: projectID, ReplyCh: chList,
		}, chList)
		if rList.Err != nil {
			t.Fatalf("list tasks: %v", rList.Err)
		}

		// Bucket manually the same way amp_list_tasks does.
		var backlog, inProgress, blocked []domain.Task
		for _, task := range rList.Tasks {
			switch task.State {
			case domain.TaskStateBacklog:
				backlog = append(backlog, task)
			case domain.TaskStateInProgress:
				inProgress = append(inProgress, task)
			case domain.TaskStateBlocked:
				blocked = append(blocked, task)
			}
		}

		if len(backlog) != 1 || backlog[0].ID != t2 {
			t.Errorf("want backlog=[t2], got IDs %v", ids(backlog))
		}
		if len(inProgress) != 1 || inProgress[0].ID != t1 {
			t.Errorf("want in_progress=[t1], got IDs %v", ids(inProgress))
		}
		if len(blocked) != 1 || blocked[0].ID != t3 {
			t.Errorf("want blocked=[t3], got IDs %v", ids(blocked))
		}
	})
}

func ids(tasks []domain.Task) []int {
	out := make([]int, len(tasks))
	for i, t := range tasks {
		out[i] = t.ID
	}
	return out
}
