package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- Trash: deleted tickets can be restored for 30 days ----

// RunTrashPurge deletes trash rows past retention every interval (and once
// at start) until ctx is done. Safe on every replica at once.
func (s *Service) RunTrashPurge(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := s.Repo.PurgeTrash(ctx); err != nil {
			slog.Warn("trash purge failed", "err", err)
		} else if n > 0 {
			slog.Info("trash purge", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

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
		if reply := actor.Await(ctx, replyCh); reply.Err != nil {
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
		actor.Await(ctx, replyCh)
	}
	if len(restored) > 0 {
		s.audit(ctx, p, domain.AuditEntry{Action: "task.restore", ProjectID: intRef(projectID), TargetType: "task",
			Summary: fmt.Sprintf("restored %d ticket(s) from the trash: %s", len(restored), joinIDs(restored)), Details: map[string]any{"task_ids": restored}})
	}
	return out, nil
}

// DeletedGroups lists a project's deleted epics and stories for its members.
func (s *Service) DeletedGroups(ctx context.Context, p authz.Principal, projectID int) ([]repository.DeletedGroup, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	return s.Repo.ListDeletedGroups(ctx, projectID)
}

// GroupRestoreResult is one epic's or story's outcome in a restore.
type GroupRestoreResult struct {
	Kind     string `json:"kind"`
	ID       int    `json:"id"`
	Restored bool   `json:"restored"`
	Error    string `json:"error,omitempty"`
	// Tickets are the outcomes for the tickets deleted with it.
	Tickets []RestoreResult `json:"tickets,omitempty"`
}

// RestoreGroups puts deleted epics (with their stories) and stories back,
// then the tickets that were deleted with them. Epics go first, so a story
// and its epic can come back in one call. A project lead, or whoever
// deleted it, may restore it.
func (s *Service) RestoreGroups(ctx context.Context, p authz.Principal, projectID int, epicIDs, storyIDs []int) ([]GroupRestoreResult, error) {
	lvl, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member)
	if err != nil {
		return nil, err
	}
	trash, err := s.Repo.ListDeletedGroups(ctx, projectID)
	if err != nil {
		return nil, err
	}
	type key struct {
		kind string
		id   int
	}
	inTrash := map[key]repository.DeletedGroup{}
	for _, g := range trash {
		inTrash[key{g.Kind, g.ID}] = g
	}
	pid, err := s.Registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	out := []GroupRestoreResult{}
	var names []string
	restore := func(kind string, id int) {
		res := GroupRestoreResult{Kind: kind, ID: id}
		defer func() { out = append(out, res) }()
		d, ok := inTrash[key{kind, id}]
		if !ok {
			res.Error = "not in this project's trash"
			return
		}
		if lvl < authz.Lead && !p.Super && (d.DeletedBy == nil || *d.DeletedBy != p.UserID) {
			res.Error = "only a project lead or whoever deleted it can restore it"
			return
		}
		g, err := s.Repo.RestoreGroup(ctx, projectID, kind, id)
		if err != nil {
			res.Error = err.Error()
			return
		}
		replyCh := make(chan actor.ReplySimple, 1)
		if kind == repository.GroupEpic {
			s.Registry.System().Root.Send(pid, &actor.MsgRestoreEpic{EpicID: id, ReplyCh: replyCh})
		} else {
			s.Registry.System().Root.Send(pid, &actor.MsgRestoreStory{EpicID: g.EpicID, StoryID: id, ReplyCh: replyCh})
		}
		if reply := actor.Await(ctx, replyCh); reply.Err != nil {
			res.Restored, res.Error = true, "restored, but not loaded: "+reply.Err.Error()
			return
		}
		res.Restored = true
		names = append(names, fmt.Sprintf("%s %q", kind, d.Name))
		// Its tickets, as far as they're still in the trash.
		deleted, err := s.Repo.ListDeletedTasks(ctx, projectID)
		if err != nil {
			res.Error = "restored, but its tickets weren't: " + err.Error()
			return
		}
		want := map[int]bool{}
		for _, t := range g.TaskIDs {
			want[t] = true
		}
		var ids []int
		for _, t := range deleted {
			if want[t.TaskID] {
				ids = append(ids, t.TaskID)
			}
		}
		if len(ids) > 0 {
			if res.Tickets, err = s.RestoreTasks(ctx, p, projectID, ids); err != nil {
				res.Error = "restored, but its tickets weren't: " + err.Error()
			}
		}
	}
	for _, id := range epicIDs {
		restore(repository.GroupEpic, id)
	}
	for _, id := range storyIDs {
		restore(repository.GroupStory, id)
	}
	if len(names) > 0 {
		s.audit(ctx, p, domain.AuditEntry{Action: "plan.restore", ProjectID: intRef(projectID), TargetType: "project", TargetID: idStr(projectID),
			Summary: "restored from the trash: " + strings.Join(names, ", "), Details: map[string]any{"epic_ids": epicIDs, "story_ids": storyIDs}})
	}
	return out, nil
}
