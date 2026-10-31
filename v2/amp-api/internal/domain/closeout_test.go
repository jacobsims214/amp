package domain

import (
	"testing"
	"time"
)

// A completion's closeout reading (AMP #2801, #2908): an explicit closeout
// reason without the current dispatch token is a closeout even on a
// dispatched ticket nobody started; a bare completion there stays an old
// plugin's own.
func TestCompletionIsCloseout(t *testing.T) {
	started := time.Now()
	const tok = "d_current"
	inProgress := func(startedAt *time.Time, startedBy string) Task {
		return Task{State: TaskStateInProgress, DispatchID: tok, StartedAt: startedAt, StartedBy: startedBy}
	}
	for _, c := range []struct {
		name       string
		task       Task
		dispatchID string
		reason     string
		want       bool
	}{
		{"never started, reason, no dispatch_id: closeout", inProgress(nil, ""), "", "worker connection lost", true},
		{"never started, no reason, no dispatch_id: legacy worker", inProgress(nil, ""), "", "", false},
		{"never started, blank reason: legacy worker", inProgress(nil, ""), "", "   ", false},
		{"never started, current dispatch_id, reason: worker", inProgress(nil, ""), tok, "worker connection lost", false},
		{"never started, current dispatch_id: worker", inProgress(nil, ""), tok, "", false},
		{"never started, old dispatch_id, reason: closeout", inProgress(nil, ""), "d_old", "re-dispatched", true},
		{"inferred start, reason: closeout", inProgress(&started, StartedByInferred), "", "worker died", true},
		{"inferred start, no reason: legacy worker", inProgress(&started, StartedByInferred), "", "", false},
		{"claimed, no dispatch_id, no reason: closeout", inProgress(&started, "amp-worker"), "", "", true},
		{"claimed, current dispatch_id: worker", inProgress(&started, "amp-worker"), tok, "", false},
		{"ready with runs, reason: closeout", Task{State: TaskStateBacklog}, "", "session died", true},
		{"human ticket, reason: never a closeout", Task{State: TaskStateInProgress, Performer: PerformerHuman}, "", "x", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			task := c.task
			if got := task.CompletionIsCloseout(c.dispatchID, c.reason); got != c.want {
				t.Errorf("CompletionIsCloseout(%q, %q) = %v, want %v", c.dispatchID, c.reason, got, c.want)
			}
		})
	}
}
