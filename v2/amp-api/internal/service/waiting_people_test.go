package service

import (
	"slices"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// WaitingPeople names the owners of open or merge-gated tickets and the
// approval owners (owner, else creator) of open tickets not approved as
// planned now. Done and merged (or merge-free) work names nobody.
func TestWaitingPeople(t *testing.T) {
	id := func(n int) *int { return &n }
	now := time.Now()
	after := now.Add(time.Minute)
	tasks := []domain.Task{
		{ID: 1, State: domain.TaskStateInProgress, OwnerUserID: id(7), PlannedAt: now, ApprovedAt: &after},
		{ID: 2, State: domain.TaskStateCompleted, OwnerUserID: id(8), MergeState: domain.MergeNeeded},
		{ID: 3, State: domain.TaskStateCompleted, OwnerUserID: id(9), MergeState: domain.MergeMerged},
		{ID: 4, State: domain.TaskStateCompleted, OwnerUserID: id(10)},
		{ID: 5, State: domain.TaskStateBacklog, CreatedBy: id(11), PlannedAt: now},                     // unowned, unapproved: its creator
		{ID: 6, State: domain.TaskStateBacklog, CreatedBy: id(12), PlannedAt: now, ApprovedAt: &after}, // unowned, approved: nobody
		{ID: 7, State: domain.TaskStateCompleted, OwnerUserID: id(13), MergeState: domain.MergeAbandoned},
		{ID: 8, State: domain.TaskStateBacklog, OwnerUserID: id(7), CreatedBy: id(14), PlannedAt: now},
	}
	got := WaitingPeople(tasks)
	if want := []int{7, 8, 11, 13}; !slices.Equal(got, want) {
		t.Fatalf("WaitingPeople = %v, want %v", got, want)
	}
	if got := WaitingPeople(nil); len(got) != 0 {
		t.Fatalf("no tickets: %v", got)
	}
}
