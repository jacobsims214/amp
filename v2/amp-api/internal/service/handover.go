package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// HandoverResult is one ticket's outcome in a handover.
type HandoverResult struct {
	TaskID int    `json:"task_id"`
	Name   string `json:"name"`
	Moved  bool   `json:"moved"`
	Error  string `json:"error,omitempty"`
}

// Handover moves someone's open tickets in a project to someone else — when
// a person is out, leaves the team, or scope moves between people. Each goes
// through the normal assignment (so the same rules and history apply) and
// gets a comment saying why. Tickets in progress stay put unless asked:
// their agent is working under the old owner. A project lead or org
// owner/admin may hand over someone else's work; anyone may hand over their own.
func (s *Service) Handover(ctx context.Context, p authz.Principal, projectID int, fromRef, toRef string, taskIDs []int, includeInProgress bool, reason string) ([]HandoverResult, error) {
	lvl, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member)
	if err != nil {
		return nil, err
	}
	from, err := s.Authz.ResolveUser(ctx, p, fromRef)
	if err != nil {
		return nil, err
	}
	to, err := s.Authz.ResolveUser(ctx, p, toRef)
	if err != nil {
		return nil, err
	}
	if from == to {
		return nil, authz.BadRequest("from and to are the same person")
	}
	if from != p.UserID && lvl < authz.Lead && !p.Super {
		return nil, authz.Forbidden("handing over someone else's tickets takes a project lead; ask yours, or hand over your own")
	}
	toUser, err := s.Repo.GetUserByID(ctx, to)
	if err != nil {
		return nil, err
	}
	open, err := s.Repo.OpenTasksOwnedBy(ctx, []int{from}, 1000)
	if err != nil {
		return nil, err
	}
	want := map[int]bool{}
	for _, id := range taskIDs {
		want[id] = true
	}
	reason = strings.TrimSpace(reason)
	out := []HandoverResult{}
	moved := []int{}
	for _, t := range open {
		if t.ProjectID != projectID || (len(want) > 0 && !want[t.ID]) {
			continue
		}
		res := HandoverResult{TaskID: t.ID, Name: t.Name}
		if t.State == domain.TaskStateInProgress && !includeInProgress {
			res.Error = "in progress: its agent is working; finish or stop it first, or include in-progress tickets"
			out = append(out, res)
			continue
		}
		if _, err := s.AssignTask(ctx, p, t.ID, fmt.Sprint(to)); err != nil {
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		body := fmt.Sprintf("Handed over to %s by %s.", userLabel(toUser), p.Email)
		if reason != "" {
			body += " Why: " + reason
		}
		added := make(chan actor.ReplyAddComment, 1)
		if err := s.Registry.Send(projectID, &actor.MsgAddComment{Req: domain.AddCommentRequest{
			TaskID: t.ID, Author: p.Email, Body: body, AuthorUserID: p.UserRef(),
		}, ReplyCh: added}); err == nil {
			<-added
		}
		res.Moved = true
		moved = append(moved, t.ID)
		out = append(out, res)
	}
	if len(moved) > 0 {
		s.audit(ctx, p, domain.AuditEntry{Action: "task.handover", ProjectID: intRef(projectID), TargetType: "task",
			Summary: fmt.Sprintf("handed %d ticket(s) to %s: %s", len(moved), userLabel(toUser), joinIDs(moved)),
			Details: map[string]any{"task_ids": moved, "from_user_id": from, "to_user_id": to, "reason": reason}})
	}
	return out, nil
}

func userLabel(u *domain.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Email
}
