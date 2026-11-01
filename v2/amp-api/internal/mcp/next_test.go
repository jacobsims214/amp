package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// TestClaimNext: a plain ownership claim steers to dispatching, never to
// working the ticket; only a started claim makes the caller its worker (AMP #2945).
func TestClaimNext(t *testing.T) {
	started := time.Now()
	tests := []struct {
		name    string
		task    domain.Task
		want    []string
		notWant []string
	}{
		{
			name:    "ownership claim of a backlog ticket",
			task:    domain.Task{ID: 7, State: domain.TaskStateBacklog},
			want:    []string{"#7 is now yours", "amp_dispatch_task(task_id=7", "worker_prompt", "same turn", "Don't work it yourself"},
			notWant: []string{"do the work", "Finish this ticket yourself", "amp_complete_task", "You are the worker", "You are now the worker"},
		},
		{
			name:    "ownership claim of a blocked ticket",
			task:    domain.Task{ID: 8, State: domain.TaskStateBlocked},
			want:    []string{"#8 is now yours", "amp_dispatch_task(task_id=8"},
			notWant: []string{"do the work", "Finish this ticket yourself", "amp_complete_task"},
		},
		{
			name:    "ownership claim of a dispatched, unstarted ticket",
			task:    domain.Task{ID: 9, State: domain.TaskStateInProgress, DispatchID: "tok"},
			want:    []string{"#9 is now yours", "worker_prompt", "launch the worker"},
			notWant: []string{"do the work", "Finish this ticket yourself", "amp_complete_task"},
		},
		{
			name: "worker claim that started the ticket",
			task: domain.Task{ID: 10, State: domain.TaskStateInProgress, DispatchID: "tok", StartedAt: &started},
			want: []string{"You are now the worker on #10", "started; dispatch tok", "do the work",
				"Finish this ticket yourself", `amp_complete_task(task_id=10, dispatch_id="tok"`, `amp_block_task(task_id=10, dispatch_id="tok"`},
			notWant: []string{"amp_dispatch_task"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := claimNext(&tc.task)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("next missing %q:\n%s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("next should not contain %q:\n%s", nw, got)
				}
			}
		})
	}
}
