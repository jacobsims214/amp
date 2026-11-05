package service

import (
	"context"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// UpstreamDecisions returns what people decided on the ticket's direct
// dependencies: each completed human ticket's latest non-empty human run
// note. A dependency with no note, or one that isn't a completed human
// ticket, is left out. The caller needs to be able to read the ticket.
func (s *Service) UpstreamDecisions(ctx context.Context, p authz.Principal, taskID int) ([]domain.UpstreamDecision, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead)
	if err != nil {
		return nil, err
	}
	return s.upstreamDecisionsFor(ctx, t)
}

func (s *Service) upstreamDecisionsFor(ctx context.Context, t *domain.Task) ([]domain.UpstreamDecision, error) {
	out := []domain.UpstreamDecision{}
	names := map[int]string{}
	for _, depID := range t.DependencyIDs {
		dep, err := s.Repo.GetTask(ctx, depID)
		if err != nil || dep.ProjectID != t.ProjectID || !dep.IsHuman() || dep.State != domain.TaskStateCompleted {
			continue
		}
		runs, err := s.Repo.TaskRuns(ctx, depID)
		if err != nil {
			return nil, err
		}
		var latest *domain.Run
		for i := range runs {
			r := &runs[i]
			if r.AgentID == domain.HumanAgentID && r.Outcome == domain.RunCompleted && r.Note != "" {
				latest = r // runs are oldest first
			}
		}
		if latest == nil {
			continue
		}
		d := domain.UpstreamDecision{TaskID: dep.ID, Name: dep.Name, Reason: dep.HumanReason,
			Decision: latest.Note, By: latest.UserID, At: latest.CreatedAt}
		if latest.UserID != nil {
			name, ok := names[*latest.UserID]
			if !ok {
				if u, err := s.Repo.GetUserByID(ctx, *latest.UserID); err == nil && u != nil {
					name = userLabel(u)
				}
				names[*latest.UserID] = name
			}
			d.ByName = name
		}
		out = append(out, d)
	}
	return out, nil
}
