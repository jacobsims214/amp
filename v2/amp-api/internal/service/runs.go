package service

import (
	"context"
	"sort"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Runs: what each agent run used and where its work went ----

// RecordRun records an agent run on a ticket. Only someone who can work the
// ticket may report on it. The run is tied to the caller (whose token the
// agent ran under) and defaults to the ticket's agent.
func (s *Service) RecordRun(ctx context.Context, p authz.Principal, taskID int, run domain.Run) (domain.Run, error) {
	// Reporting a merge is a fact about the code, not work on the ticket: anyone
	// on the project may record it (a reviewer, or CI with its own token).
	action := authz.TaskWork
	if run.Merged != nil && *run.Merged && run.InputTokens == nil && run.OutputTokens == nil && run.CostUSD == nil {
		action = authz.TaskRead
	}
	t, _, err := s.LoadTask(ctx, p, taskID, action)
	if err != nil {
		return domain.Run{}, err
	}
	if err := s.checkRunOutcome(p, t, run.Outcome); err != nil {
		return domain.Run{}, err
	}
	return s.recordRun(ctx, p, t, run)
}

func (s *Service) recordRun(ctx context.Context, p authz.Principal, t *domain.Task, run domain.Run) (domain.Run, error) {
	run.TaskID, run.ProjectID = t.ID, t.ProjectID
	if p.HasUser() {
		uid := p.UserID
		run.UserID = &uid
	}
	if run.AgentID == "" {
		run.AgentID = t.AgentID
	}
	switch run.Outcome {
	case domain.RunCompleted, domain.RunBlocked, domain.RunProgress, domain.RunAbandoned:
	case "":
		run.Outcome = domain.RunProgress
	default:
		return domain.Run{}, authz.BadRequest("outcome must be completed, blocked, progress or abandoned")
	}
	if run.Commit != "" && len(run.Commit) < 7 {
		return domain.Run{}, authz.BadRequest("commit must be at least 7 characters of the SHA")
	}
	run.Commit = strings.ToLower(run.Commit)
	return s.Repo.AddRun(ctx, run)
}

// TaskRuns lists a ticket's runs for anyone who can see the ticket.
func (s *Service) TaskRuns(ctx context.Context, p authz.Principal, taskID int) ([]domain.Run, error) {
	if _, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead); err != nil {
		return nil, err
	}
	return s.Repo.TaskRuns(ctx, taskID)
}

// TraceHit is a ticket that a commit, branch or PR leads back to.
type TraceHit struct {
	Ref     string       `json:"ref"`
	Task    domain.Task  `json:"task"`
	Project string       `json:"project"`
	Runs    []domain.Run `json:"runs"` // the matching runs, newest first
}

// Trace finds the tickets whose recorded runs produced a commit (SHA or a
// prefix of 7+), a branch, or a PR, among the projects p can see: the way
// back from a code change to the ticket, its plan, and its history.
func (s *Service) Trace(ctx context.Context, p authz.Principal, commit, branch, pr string) ([]TraceHit, error) {
	commit, branch, pr = strings.ToLower(strings.TrimSpace(commit)), strings.TrimSpace(branch), strings.TrimSpace(pr)
	if commit == "" && branch == "" && pr == "" {
		return nil, authz.BadRequest("give a commit, a branch or a PR URL")
	}
	if commit != "" && len(commit) < 7 {
		return nil, authz.BadRequest("commit must be at least 7 characters of the SHA")
	}
	visible, err := s.Authz.VisibleProjects(ctx, p)
	if err != nil {
		return nil, err
	}
	var ids []int
	if visible != nil {
		ids = []int{}
		for id := range visible {
			ids = append(ids, id)
		}
	}
	runs, err := s.Repo.FindRuns(ctx, ids, commit, branch, pr)
	if err != nil {
		return nil, err
	}
	byTask := map[int]*TraceHit{}
	order := []int{}
	for _, r := range runs {
		h := byTask[r.TaskID]
		if h == nil {
			t, err := s.Repo.GetTask(ctx, r.TaskID)
			if err != nil {
				continue // the ticket was deleted; its runs remain but there's nothing to open
			}
			proj, err := s.Repo.GetProject(ctx, t.ProjectID)
			if err != nil {
				continue
			}
			h = &TraceHit{Ref: domain.TaskRef(proj.Code, t.ID), Task: *t, Project: proj.Name}
			byTask[r.TaskID] = h
			order = append(order, r.TaskID)
		}
		h.Runs = append(h.Runs, r)
	}
	out := make([]TraceHit, 0, len(order))
	for _, id := range order {
		out = append(out, *byTask[id])
	}
	return out, nil
}

// UsageRow is token spend for one model.
type UsageRow struct {
	Model string `json:"model"`
	domain.Usage
}

func usageByModel(runs []domain.Run) []UsageRow {
	m := map[string]*UsageRow{}
	for _, r := range runs {
		k := r.Model
		if k == "" {
			k = "(not reported)"
		}
		if m[k] == nil {
			m[k] = &UsageRow{Model: k}
		}
		m[k].Add(r)
	}
	out := make([]UsageRow, 0, len(m))
	for _, row := range m {
		row.Models = nil
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].InputTokens+out[i].OutputTokens > out[j].InputTokens+out[j].OutputTokens
	})
	return out
}

// UnmergedWork is a project's "waiting to merge" queue for its members.
func (s *Service) UnmergedWork(ctx context.Context, p authz.Principal, projectID int) ([]domain.Run, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	return s.Repo.UnmergedWork(ctx, projectID)
}

// MarkMerged records that the work on a branch, PR, or commit is merged, on
// every ticket whose runs produced it (in projects p can see). It's how CI
// reports merges: one call after merging, with the merge commit.
func (s *Service) MarkMerged(ctx context.Context, p authz.Principal, commit, branch, pr, mergeCommit string) ([]int, error) {
	hits, err := s.Trace(ctx, p, commit, branch, pr)
	if err != nil {
		return nil, err
	}
	merged := []int{}
	yes := true
	for _, h := range hits {
		last := h.Runs[0]
		run := domain.Run{Outcome: domain.RunCompleted, Merged: &yes, Branch: last.Branch, PRURL: last.PRURL,
			Commit: last.Commit, Note: "merged"}
		if mergeCommit != "" {
			run.Commit, run.Note = mergeCommit, "merged as "+mergeCommit
		}
		if _, err := s.RecordRun(ctx, p, h.Task.ID, run); err != nil {
			return merged, err
		}
		merged = append(merged, h.Task.ID)
	}
	return merged, nil
}
