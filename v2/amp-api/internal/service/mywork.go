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
// projects they can see, and their agents. Since AMP #2478 it also carries
// the "waiting on you" summary grouped by project (MyWorkSummary, inlined:
// counts, projects, clear_projects, ...); the older fields stay for UI
// builds that read them.
type MyWork struct {
	Owned   []repository.WorkTask `json:"owned"`
	Flags   []repository.WorkTask `json:"flags"`
	Agents  []MyAgent             `json:"agents"`
	Done24h int                   `json:"done_24h"` // tickets I own, finished in the last day
	// OwnedTruncated / FlagsTruncated: Owned and Flags hit their caps
	// (200 and 100) and there are more. The summary has no such cap.
	OwnedTruncated bool `json:"owned_truncated"`
	FlagsTruncated bool `json:"flags_truncated"`
	*MyWorkSummary
}

// Caps on MyWork's older lists.
const (
	myWorkOwnedCap = 200
	myWorkFlagsCap = 100
)

// MyWork is MyWorkPage with the summary's default paging.
func (s *Service) MyWork(ctx context.Context, p authz.Principal) (*MyWork, error) {
	return s.MyWorkPage(ctx, p, MyWorkPageOptions{})
}

// MyWorkPage is GET /api/my-work: the older home-page lists plus the
// "waiting on you" summary, paged by opt.
func (s *Service) MyWorkPage(ctx context.Context, p authz.Principal, opt MyWorkPageOptions) (*MyWork, error) {
	if !p.HasUser() {
		sum, err := s.MyWorkSummaryFor(ctx, p, opt)
		if err != nil {
			return nil, err
		}
		return &MyWork{Owned: []repository.WorkTask{}, Flags: []repository.WorkTask{}, Agents: []MyAgent{}, MyWorkSummary: sum}, nil
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
	if w.Owned, err = s.Repo.OpenTasksOwnedBy(ctx, []int{p.UserID}, myWorkOwnedCap+1); err != nil {
		return nil, err
	}
	if len(w.Owned) > myWorkOwnedCap {
		w.Owned, w.OwnedTruncated = w.Owned[:myWorkOwnedCap], true
	}
	w.Owned = keep(w.Owned)
	var visibleIDs []int
	if visible != nil {
		visibleIDs = []int{}
		for id := range visible {
			visibleIDs = append(visibleIDs, id)
		}
	}
	if w.Flags, err = s.Repo.FlaggedTasks(ctx, visibleIDs, myWorkFlagsCap+1); err != nil {
		return nil, err
	}
	if len(w.Flags) > myWorkFlagsCap {
		w.Flags, w.FlagsTruncated = w.Flags[:myWorkFlagsCap], true
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
	if w.MyWorkSummary, err = s.MyWorkSummaryFor(ctx, p, opt); err != nil {
		return nil, err
	}
	return w, nil
}
