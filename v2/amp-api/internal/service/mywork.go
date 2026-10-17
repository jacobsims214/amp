package service

import (
	"context"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// MyAgent is one of the caller's agent accounts and what it is doing.
type MyAgent struct {
	User    domain.User           `json:"user"`
	Working []repository.WorkTask `json:"working"`
	Done24h int                   `json:"done_24h"`
}

// MyWork is the caller's home page: their open tickets, the flags in
// projects they can see, and their agents.
type MyWork struct {
	Owned   []repository.WorkTask `json:"owned"`
	Flags   []repository.WorkTask `json:"flags"`
	Agents  []MyAgent             `json:"agents"`
	Done24h int                   `json:"done_24h"` // tickets I own, finished in the last day
}

func (s *Service) MyWork(ctx context.Context, p authz.Principal) (*MyWork, error) {
	if !p.HasUser() {
		return &MyWork{Owned: []repository.WorkTask{}, Flags: []repository.WorkTask{}, Agents: []MyAgent{}}, nil
	}
	visible, err := s.Authz.VisibleProjects(ctx, p)
	if err != nil {
		return nil, err
	}
	keep := func(list []repository.WorkTask) []repository.WorkTask {
		if visible == nil {
			return list
		}
		out := list[:0]
		for _, t := range list {
			if visible[t.ProjectID] {
				out = append(out, t)
			}
		}
		return out
	}
	w := &MyWork{Agents: []MyAgent{}}
	if w.Owned, err = s.Repo.OpenTasksOwnedBy(ctx, []int{p.UserID}, 200); err != nil {
		return nil, err
	}
	w.Owned = keep(w.Owned)
	if w.Flags, err = s.Repo.FlaggedTasks(ctx, 100); err != nil {
		return nil, err
	}
	w.Flags = keep(w.Flags)

	since := time.Now().Add(-24 * time.Hour)
	mine, err := s.Repo.AgentWork(ctx, nil, []int{p.UserID}, since)
	if err != nil {
		return nil, err
	}
	for _, t := range keep(mine) {
		if t.State == domain.TaskStateCompleted {
			w.Done24h++
		}
	}

	agents, err := s.Repo.ListAgentUsers(ctx, p.UserRef())
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		names := []string{a.DisplayName}
		if a.DisplayName == "" {
			names = []string{a.Email}
		}
		work, err := s.Repo.AgentWork(ctx, names, []int{a.ID}, since)
		if err != nil {
			return nil, err
		}
		ma := MyAgent{User: a, Working: []repository.WorkTask{}}
		for _, t := range keep(work) {
			if t.State == domain.TaskStateCompleted {
				ma.Done24h++
			} else {
				ma.Working = append(ma.Working, t)
			}
		}
		w.Agents = append(w.Agents, ma)
	}
	return w, nil
}
