package main

import (
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// TestIntegration_CompleteWhileListingDoesNotDeadlock completes tickets while
// the project's tickets are being listed at the same time — what an agent
// finishing work does while someone has the board, amp_list_tasks or
// waiting-on-people open.
//
// Completing a ticket fans its dependency out through the ProjectActor;
// listing has the ProjectActor collect every ticket through its epics and
// stories. A TaskActor that waited on the ProjectActor inside its own mailbox
// while the ProjectActor waited on that same TaskActor deadlocked the whole
// project: every later request for it hung until the API was restarted
// (prod, 2026-11-06).
func TestIntegration_CompleteWhileListingDoesNotDeadlock(t *testing.T) {
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

		const n = 40
		tasks := make([]int, n)
		for i := range tasks {
			tasks[i] = create("ticket", nil)
		}
		final := create("after all of them", tasks)

		pid, err := reg.Get(projectID)
		if err != nil {
			t.Fatalf("get project actor: %v", err)
		}
		root := reg.System().Root
		// Any reply slower than this means the project is wedged.
		const stuck = 5 * time.Second

		done := make(chan struct{})
		var wg sync.WaitGroup
		var listErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				ch := make(chan actor.ReplyListTasks, 1)
				root.Send(pid, &actor.MsgListTasks{ProjectID: projectID, ReplyCh: ch})
				select {
				case r := <-ch:
					if r.Err != nil {
						listErr = r.Err
						return
					}
				case <-time.After(stuck):
					listErr = errDeadlocked
					return
				}
			}
		}()

		for _, id := range tasks {
			ch := make(chan actor.ReplyComplete, 1)
			root.Send(pid, &actor.MsgCompleteTask{TaskID: id, ReplyCh: ch})
			select {
			case r := <-ch:
				if r.Err != nil {
					t.Fatalf("complete %d: %v", id, r.Err)
				}
			case <-time.After(stuck):
				close(done)
				t.Fatalf("completing ticket %d got no reply in %s: the project actor is deadlocked", id, stuck)
			}
		}
		close(done)
		wg.Wait()
		if listErr != nil {
			t.Fatalf("listing while completing: %v", listErr)
		}

		// The fan-out still lands before complete replies: the ticket that
		// waited on all of them is unblocked as soon as the last one is done.
		ch := make(chan actor.ReplyGetTask, 1)
		r := sendMsg(t, reg, projectID, &actor.MsgGetTask{TaskID: final, ReplyCh: ch}, ch)
		if r.Err != nil {
			t.Fatalf("get final: %v", r.Err)
		}
		if r.Task.State != domain.TaskStateBacklog || len(r.Task.BlockedByIDs) != 0 {
			t.Fatalf("want the dependent ticket unblocked, got state=%q blocked_by=%v", r.Task.State, r.Task.BlockedByIDs)
		}
	})
}

type deadlockErr struct{}

func (deadlockErr) Error() string { return "list got no reply: the project actor is deadlocked" }

var errDeadlocked error = deadlockErr{}
