package service

import (
	"context"
	"sort"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// Waiting on people (AMP #2496, KB decisions/human-work-ux.md §1 and §3):
// what the project waits on each person for, from the one definition
// (WaitingOn) run for everyone with work in it. Mission's "Waiting on
// people" panel (GET /api/projects/{id}/waiting-on-people) and MCP's
// waiting_on_people both read WaitingOnPeople, so neither keeps its own
// copy of the rules.

// WaitingPeople is everyone who can have something waiting on them in
// tasks: the owners of open or merge-gated tickets, and the approval owners
// of open tickets not approved as planned now. Sorted by user id. WaitingOn
// decides what, if anything, actually waits on each.
func WaitingPeople(tasks []domain.Task) []int {
	people := map[int]bool{}
	for i := range tasks {
		t := &tasks[i]
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
	out := make([]int, 0, len(people))
	for id := range people {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// WaitingOnPeople runs WaitingOn in projectID for each of WaitingPeople(tasks)
// (tasks: every ticket in the project), in user id order. People with
// nothing waiting and nothing coming up are left out. It checks nothing.
func (s *Service) WaitingOnPeople(ctx context.Context, projectID int, tasks []domain.Task) ([]*Waiting, error) {
	out := []*Waiting{}
	for _, uid := range WaitingPeople(tasks) {
		w, err := s.WaitingOn(ctx, uid, []int{projectID})
		if err != nil {
			return nil, err
		}
		if len(w.Items) == 0 && len(w.ComingUp) == 0 {
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

// PeopleWaiting is GET /api/projects/{id}/waiting-on-people: each person's
// counts by kind and ticket ids, nothing a member can't already see on the
// Board. People are in user id order; the UI orders its panel.
type PeopleWaiting struct {
	ProjectID int             `json:"project_id"`
	People    []PersonWaiting `json:"people"`
}

// PersonWaiting is one person in PeopleWaiting.
type PersonWaiting struct {
	UserID int        `json:"user_id"`
	Counts WaitCounts `json:"counts"`
	// Items are in WaitingOn's order: approve, answer, merge, do, oldest
	// first within each.
	Items []PersonWaitItem `json:"items"`
	// ComingUp is the ticket ids of their human tickets not actionable yet,
	// oldest first.
	ComingUp []int `json:"coming_up"`
}

// PersonWaitItem is one waiting item: a ticket, or for approve, an epic's
// tickets awaiting the person's approval.
type PersonWaitItem struct {
	Kind     string    `json:"kind"` // approve | answer | merge | do
	TaskIDs  []int     `json:"task_ids"`
	EpicID   int       `json:"epic_id,omitempty"`   // approve
	EpicName string    `json:"epic_name,omitempty"` // approve
	Since    time.Time `json:"since"`
}

// ProjectWaitingOnPeople is what projectID waits on each person for.
// Any project member may read it.
func (s *Service) ProjectWaitingOnPeople(ctx context.Context, p authz.Principal, projectID int) (*PeopleWaiting, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	// The tickets come from the project actor, as the Board's and MCP's do.
	pid, err := s.Registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	listCh := make(chan actor.ReplyListTasks, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgListTasks{ProjectID: projectID, ReplyCh: listCh})
	listed := actor.Await(ctx, listCh)
	if listed.Err != nil {
		return nil, listed.Err
	}
	all, err := s.WaitingOnPeople(ctx, projectID, listed.Tasks)
	if err != nil {
		return nil, err
	}
	out := &PeopleWaiting{ProjectID: projectID, People: make([]PersonWaiting, 0, len(all))}
	for _, w := range all {
		pw := PersonWaiting{UserID: w.UserID, Counts: w.Counts, Items: make([]PersonWaitItem, 0, len(w.Items)),
			ComingUp: make([]int, 0, len(w.ComingUp))}
		for _, it := range w.Items {
			ids := it.TaskIDs
			if ids == nil {
				ids = []int{}
			}
			item := PersonWaitItem{Kind: it.Kind, TaskIDs: ids, Since: it.Since}
			if it.Kind == WaitApprove {
				item.EpicID, item.EpicName = it.EpicID, it.EpicName
			}
			pw.Items = append(pw.Items, item)
		}
		for _, it := range w.ComingUp {
			pw.ComingUp = append(pw.ComingUp, it.TaskID)
		}
		out.People = append(out.People, pw)
	}
	return out, nil
}
