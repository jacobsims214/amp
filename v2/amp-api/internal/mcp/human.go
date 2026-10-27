package mcp

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// HumanStaleAfter is how long a person's ready or started ticket can go
// without any activity before amp_list_tasks calls it stale. People work in
// days, not the minutes after which an agent's ticket is quiet.
const HumanStaleAfter = 48 * time.Hour

// PersonWait is one item in amp_list_tasks' waiting_on_people ("Waiting on
// you", KB decisions/human-work-ux.md §1 and §5): something a person has to
// do next, of one of four kinds (approve, answer, merge, do), or, in
// coming_up, a human ticket of theirs that isn't actionable yet.
//
// The fields before Kind are the original shape (when the list only held
// human tickets) and keep their meaning, so older managers still parse it:
// id is a ticket (for approve, the first of the epic's tickets awaiting
// approval), owner is who it waits on.
type PersonWait struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Reason       string `json:"reason"` // the human ticket's reason, else the kind
	Owner        string `json:"owner"`  // email, or "nobody" when unassigned
	State        string `json:"state"`  // do: ready | in_progress; answer: question; merge: completed; approve: awaiting_approval; coming_up: waiting | scheduled | awaiting_approval
	WaitingHours int    `json:"waiting_hours"`
	HoldingUp    []int  `json:"holding_up,omitempty"` // open tickets that can't start until it's done (transitively)
	Stale        bool   `json:"stale,omitempty"`      // do: no activity for HumanStaleAfter

	Kind        string    `json:"kind"`     // approve | answer | merge | do | coming_up
	TaskIDs     []int     `json:"task_ids"` // every ticket in the item (approve: all awaiting in the epic)
	OwnerUserID *int      `json:"owner_user_id,omitempty"`
	Since       time.Time `json:"since"`
	// approve
	EpicID   int    `json:"epic_id,omitempty"`
	EpicName string `json:"epic_name,omitempty"`
	Count    int    `json:"count,omitempty"`
	// answer
	Question string `json:"question,omitempty"`
	// merge: needed (Merge needed) or abandoned (PR closed without merging)
	MergeState string `json:"merge_state,omitempty"`
	PRURL      string `json:"pr_url,omitempty"`
	Branch     string `json:"branch,omitempty"`
	// coming_up: what it still waits on
	WaitsOn          []int      `json:"waits_on,omitempty"`
	AwaitingApproval bool       `json:"awaiting_approval,omitempty"`
	StartAt          *time.Time `json:"start_at,omitempty"`
}

// waitingOnPeople is what the project waits on people for, from the one
// definition (service.WaitingOn) run for everyone with work in it, and the
// human tickets coming up for them. Only items that matter to the caller are
// kept: one of the item's tickets is in their view, or holds up one that is.
// all is every ticket in the project; view is the caller's.
//
// Unowned human tickets that are ready or started are kept as kind "do" with
// owner "nobody", as the list always showed them: WaitingOn is owner-only.
func (s *Server) waitingOnPeople(ctx context.Context, projectID int, all, view []domain.Task) (items, comingUp []PersonWait, err error) {
	items, comingUp = []PersonWait{}, []PersonWait{}
	byID := map[int]*domain.Task{}
	dependents := map[int][]int{}
	people := map[int]bool{}
	for i := range all {
		t := &all[i]
		byID[t.ID] = t
		for _, d := range t.DependencyIDs {
			dependents[d] = append(dependents[d], t.ID)
		}
		if t.State == domain.TaskStateCompleted && !domain.MergeGates(t.MergeState) {
			continue
		}
		if t.OwnerUserID != nil {
			people[*t.OwnerUserID] = true
		}
		if who := t.ApprovalOwner(); who != nil && t.State != domain.TaskStateCompleted && !t.Approved() {
			people[*who] = true
		}
	}
	inView := map[int]bool{}
	for _, t := range view {
		inView[t.ID] = true
	}
	// Everything open downstream of id, transitively.
	held := map[int][]int{}
	holding := func(id int) []int {
		if out, ok := held[id]; ok {
			return out
		}
		var out []int
		seen := map[int]bool{}
		queue := append([]int(nil), dependents[id]...)
		for len(queue) > 0 {
			d := queue[0]
			queue = queue[1:]
			if seen[d] {
				continue
			}
			seen[d] = true
			if t := byID[d]; t != nil && t.State != domain.TaskStateCompleted {
				out = append(out, d)
			}
			queue = append(queue, dependents[d]...)
		}
		held[id] = out
		return out
	}
	relevant := func(ids []int) bool {
		for _, id := range ids {
			if inView[id] || slices.ContainsFunc(holding(id), func(d int) bool { return inView[d] }) {
				return true
			}
		}
		return false
	}

	emails := map[int]string{}
	if members, err := s.svc.ProjectMembers(ctx, projectID); err == nil {
		for _, m := range members {
			emails[m.UserID] = m.Email
		}
	}
	email := func(id *int) string {
		if id == nil {
			return "nobody"
		}
		if e := emails[*id]; e != "" {
			return e
		}
		return fmt.Sprintf("user %d", *id)
	}

	uids := make([]int, 0, len(people))
	for id := range people {
		uids = append(uids, id)
	}
	sort.Ints(uids)
	for _, uid := range uids {
		w, err := s.svc.WaitingOn(ctx, uid, []int{projectID})
		if err != nil {
			return nil, nil, err
		}
		who := uid
		for _, it := range w.Items {
			if !relevant(it.TaskIDs) {
				continue
			}
			items = append(items, s.personWait(it, &who, email(&who), holding))
		}
		for _, it := range w.ComingUp {
			if !relevant(it.TaskIDs) {
				continue
			}
			p := s.personWait(it, &who, email(&who), holding)
			p.AwaitingApproval, p.StartAt = it.AwaitingApproval, it.StartAt
			if t := byID[it.TaskID]; t != nil {
				p.WaitsOn = t.BlockedByIDs
			}
			switch {
			case p.AwaitingApproval:
				p.State = "awaiting_approval"
			case p.StartAt != nil:
				p.State = "scheduled"
			default:
				p.State = "waiting"
			}
			comingUp = append(comingUp, p)
		}
	}

	// Unowned human tickets a person could pick up now.
	requireApproval := false
	if p, err := s.repo.GetProject(ctx, projectID); err == nil {
		requireApproval = p.RequirePlanApproval
	}
	for i := range all {
		t := &all[i]
		if !t.IsHuman() || t.OwnerUserID != nil || !relevant([]int{t.ID}) {
			continue
		}
		var since time.Time
		switch {
		case t.State == domain.TaskStateInProgress:
			since = t.CreatedAt
			if t.DispatchedAt != nil {
				since = *t.DispatchedAt
			} else if t.ReadyAt != nil {
				since = *t.ReadyAt
			}
		case t.State == domain.TaskStateBacklog && !(requireApproval && !t.Approved()):
			since = t.CreatedAt
			if t.ReadyAt != nil {
				since = *t.ReadyAt
			}
		default:
			continue
		}
		items = append(items, s.personWait(service.WaitItem{Kind: service.WaitDo, Key: fmt.Sprintf("task:%d", t.ID), TaskID: t.ID,
			TaskIDs: []int{t.ID}, Name: t.Name, State: string(t.State), Since: since, HumanReason: t.HumanReason}, nil, "nobody", holding))
	}

	rank := map[string]int{service.WaitApprove: 0, service.WaitAnswer: 1, service.WaitMerge: 2, service.WaitDo: 3}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		return a.ID < b.ID
	})
	sort.SliceStable(comingUp, func(i, j int) bool {
		if !comingUp[i].Since.Equal(comingUp[j].Since) {
			return comingUp[i].Since.Before(comingUp[j].Since)
		}
		return comingUp[i].ID < comingUp[j].ID
	})

	// Stale: a person's to do with no activity for HumanStaleAfter.
	var dos []int
	for _, p := range items {
		if p.Kind == service.WaitDo {
			dos = append(dos, p.ID)
		}
	}
	if len(dos) > 0 {
		last, err := s.repo.LastActivity(ctx, dos)
		if err != nil {
			return nil, nil, err
		}
		for i := range items {
			p := &items[i]
			if p.Kind != service.WaitDo {
				continue
			}
			seenAt := last[p.ID]
			if seenAt.IsZero() {
				seenAt = p.Since
			}
			p.Stale = time.Since(seenAt) > HumanStaleAfter
		}
	}
	return items, comingUp, nil
}

// personWait turns one WaitingOn item into its waiting_on_people entry.
func (s *Server) personWait(it service.WaitItem, owner *int, ownerEmail string, holding func(int) []int) PersonWait {
	p := PersonWait{ID: it.TaskID, Name: it.Name, Reason: it.HumanReason, Owner: ownerEmail, Kind: it.Kind,
		TaskIDs: it.TaskIDs, OwnerUserID: owner, Since: it.Since, WaitingHours: int(time.Since(it.Since).Hours())}
	if p.Reason == "" {
		p.Reason = it.Kind
	}
	if p.TaskIDs == nil {
		p.TaskIDs = []int{}
	}
	switch it.Kind {
	case service.WaitApprove:
		if len(it.TaskIDs) > 0 {
			p.ID = it.TaskIDs[0]
		}
		p.State = "awaiting_approval"
		p.EpicID, p.EpicName, p.Count = it.EpicID, it.EpicName, it.Count
		p.Name = fmt.Sprintf("Approve plan: %s (%d %s)", it.Name, it.Count, plural(it.Count, "ticket", "tickets"))
	case service.WaitAnswer:
		p.State = "question"
		p.Question = it.Question
	case service.WaitMerge:
		p.State = "completed"
		p.MergeState, p.PRURL, p.Branch = it.MergeState, it.PRURL, it.Branch
	case service.WaitDo:
		p.State = "ready"
		if it.State == string(domain.TaskStateInProgress) {
			p.State = "in_progress"
		}
	}
	if it.Kind != service.WaitApprove {
		p.HoldingUp = holding(p.ID)
	}
	return p
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
