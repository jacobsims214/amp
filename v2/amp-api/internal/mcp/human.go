package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// HumanStaleAfter is how long a person's ready or started ticket can go
// without any activity before amp_list_tasks calls it stale. People work in
// days, not the minutes after which an agent's ticket is quiet.
const HumanStaleAfter = 48 * time.Hour

// PersonWait is a human ticket a person can do now, and what waits on it.
type PersonWait struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Reason       string `json:"reason"`
	Owner        string `json:"owner"` // email, or "nobody" when unassigned
	State        string `json:"state"` // ready | in_progress
	WaitingHours int    `json:"waiting_hours"`
	HoldingUp    []int  `json:"holding_up,omitempty"` // open tickets that can't start until it's done
	Stale        bool   `json:"stale,omitempty"`      // no activity for HumanStaleAfter
}

// waitingOnPeople lists the project's human tickets a person can act on now
// (ready or started) that matter to the caller: in their view, or holding up
// a ticket that is. all is every ticket in the project; view is the caller's.
func (s *Server) waitingOnPeople(ctx context.Context, projectID int, all, view []domain.Task) ([]PersonWait, error) {
	out := []PersonWait{}
	dependents := map[int][]int{}
	open := map[int]bool{}
	for _, t := range all {
		if t.State != domain.TaskStateCompleted {
			open[t.ID] = true
		}
		for _, d := range t.DependencyIDs {
			dependents[d] = append(dependents[d], t.ID)
		}
	}
	inView := map[int]bool{}
	for _, t := range view {
		inView[t.ID] = true
	}
	var cands []domain.Task
	for _, t := range all {
		if t.IsHuman() && (t.State == domain.TaskStateBacklog || t.State == domain.TaskStateInProgress) {
			cands = append(cands, t)
		}
	}
	if len(cands) == 0 {
		return out, nil
	}
	emails := map[int]string{}
	if members, err := s.svc.ProjectMembers(ctx, projectID); err == nil {
		for _, m := range members {
			emails[m.UserID] = m.Email
		}
	}
	ids := make([]int, len(cands))
	for i, t := range cands {
		ids[i] = t.ID
	}
	last, err := s.repo.LastActivity(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, t := range cands {
		// Everything open downstream of it, transitively.
		var holding []int
		relevant := inView[t.ID]
		seen := map[int]bool{}
		queue := append([]int(nil), dependents[t.ID]...)
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			if open[id] {
				holding = append(holding, id)
				relevant = relevant || inView[id]
			}
			queue = append(queue, dependents[id]...)
		}
		if !relevant {
			continue
		}
		w := PersonWait{ID: t.ID, Name: t.Name, Reason: t.HumanReason, Owner: "nobody", HoldingUp: holding, State: "ready"}
		if t.OwnerUserID != nil {
			w.Owner = emails[*t.OwnerUserID]
			if w.Owner == "" {
				w.Owner = fmt.Sprintf("user %d", *t.OwnerUserID)
			}
		}
		since := t.ReadyAt
		if t.State == domain.TaskStateInProgress {
			w.State = "in_progress"
			if t.DispatchedAt != nil {
				since = t.DispatchedAt
			}
		}
		if since == nil {
			since = &t.CreatedAt
		}
		w.WaitingHours = int(time.Since(*since).Hours())
		seenAt := last[t.ID]
		if seenAt.IsZero() {
			seenAt = *since
		}
		w.Stale = time.Since(seenAt) > HumanStaleAfter
		out = append(out, w)
	}
	return out, nil
}
