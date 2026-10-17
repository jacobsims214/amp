package service

import (
	"context"
	"fmt"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- Trash: deleted tickets can be restored for 30 days ----

// DeletedTasks lists a project's trash for its members.
func (s *Service) DeletedTasks(ctx context.Context, p authz.Principal, projectID int) ([]repository.DeletedTask, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	return s.Repo.ListDeletedTasks(ctx, projectID)
}

// RestoreResult is one ticket's outcome in a restore.
type RestoreResult struct {
	TaskID   int    `json:"task_id"`
	Restored bool   `json:"restored"`
	Error    string `json:"error,omitempty"`
}

// RestoreTasks puts deleted tickets back: their text, owner, state, comments,
// history, and dependency links in both directions (to tickets that still
// exist). A project lead, or whoever deleted a ticket, may restore it.
func (s *Service) RestoreTasks(ctx context.Context, p authz.Principal, projectID int, ids []int) ([]RestoreResult, error) {
	lvl, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member)
	if err != nil {
		return nil, err
	}
	trash, err := s.Repo.ListDeletedTasks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	inTrash := map[int]repository.DeletedTask{}
	for _, d := range trash {
		inTrash[d.TaskID] = d
	}
	pid, err := s.Registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	out := []RestoreResult{}
	restored := []int{}
	pendingLinks := map[int][]int{} // dependent → restored tickets it depended on
	missing := map[int][]int{}      // restored ticket → its dependencies that weren't there yet
	for _, id := range ids {
		d, ok := inTrash[id]
		if !ok {
			out = append(out, RestoreResult{TaskID: id, Error: "not in this project's trash"})
			continue
		}
		if lvl < authz.Lead && !p.Super && (d.DeletedBy == nil || *d.DeletedBy != p.UserID) {
			out = append(out, RestoreResult{TaskID: id, Error: "only a project lead or whoever deleted it can restore it"})
			continue
		}
		r, err := s.Repo.RestoreTask(ctx, projectID, id)
		if err != nil {
			out = append(out, RestoreResult{TaskID: id, Error: err.Error()})
			continue
		}
		replyCh := make(chan actor.ReplyGetTask, 1)
		s.Registry.System().Root.Send(pid, &actor.MsgRestoreTask{TaskID: id, EpicID: r.EpicID, StoryID: r.StoryID, ReplyCh: replyCh})
		if reply := <-replyCh; reply.Err != nil {
			out = append(out, RestoreResult{TaskID: id, Restored: true, Error: "restored, but not loaded: " + reply.Err.Error()})
			continue
		}
		for _, dep := range r.Dependents {
			pendingLinks[dep] = append(pendingLinks[dep], id)
		}
		missing[id] = r.MissingDeps
		restored = append(restored, id)
		out = append(out, RestoreResult{TaskID: id, Restored: true})
	}
	// Dependencies on tickets restored later in this same batch.
	back := map[int]bool{}
	for _, id := range restored {
		back[id] = true
	}
	for id, deps := range missing {
		for _, d := range deps {
			if back[d] {
				pendingLinks[id] = append(pendingLinks[id], d)
			}
		}
	}
	// Re-link tickets that depended on what came back (including restored
	// tickets depending on each other), through the actors so their states follow.
	for dep, ups := range pendingLinks {
		current, err := s.Repo.TaskDependencies(ctx, dep)
		if err != nil {
			continue
		}
		seen := map[int]bool{}
		next := []int{}
		for _, d := range append(current, ups...) {
			if !seen[d] {
				seen[d] = true
				next = append(next, d)
			}
		}
		replyCh := make(chan actor.ReplyGetTask, 1)
		s.Registry.System().Root.Send(pid, &actor.MsgSetTaskDeps{TaskID: dep, DependencyIDs: next, ReplyCh: replyCh})
		<-replyCh
	}
	if len(restored) > 0 {
		s.audit(ctx, p, domain.AuditEntry{Action: "task.restore", ProjectID: intRef(projectID), TargetType: "task",
			Summary: fmt.Sprintf("restored %d ticket(s) from the trash: %s", len(restored), joinIDs(restored)), Details: map[string]any{"task_ids": restored}})
	}
	return out, nil
}
