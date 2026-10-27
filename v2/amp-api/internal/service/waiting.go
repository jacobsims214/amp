package service

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Waiting on you (AMP #2478, KB decisions/human-work-ux.md §1 and §3).
//
// WaitingOn is the one definition of what waits on a person. Every view
// reads it: the project's My work tab (ProjectMyWork), the top-level
// summary (MyWorkPage), the sidebar badge (MyWorkCounts), and later MCP's
// waiting_on_people. Four kinds, in this order, oldest first within each:
//
//   - approve: tickets whose plan approval targets the person (owner, else
//     the person who planned an unassigned one: Task.ApprovalOwner), in a
//     project that requires approval, not approved as planned now. One item
//     per epic, with the tickets and their count.
//   - answer: a ticket they own, blocked with a question (a block reason
//     that isn't "scheduled:…").
//   - merge: a ticket they own, completed, merge_state needed or abandoned.
//   - do: a human ticket they own that is ready (backlog) or in progress.
//
// coming_up is their human tickets still waiting on dependencies, a
// schedule or plan approval: shown, never counted. "Mine" is
// owner_user_id = the person, nothing else; approval follows approval's own
// rule (an unassigned ticket is its planner's to approve), as it always has.

// Kinds of item that wait on a person, in display order.
const (
	WaitApprove = "approve"
	WaitAnswer  = "answer"
	WaitMerge   = "merge"
	WaitDo      = "do"
)

// WaitKinds is the display order of the kinds.
var WaitKinds = []string{WaitApprove, WaitAnswer, WaitMerge, WaitDo}

// WaitCounts counts what waits on a person. Waiting is the sum of the four
// kinds; ComingUp is shown but never part of Waiting.
type WaitCounts struct {
	Waiting  int `json:"waiting"`
	Approve  int `json:"approve"`
	Answer   int `json:"answer"`
	Merge    int `json:"merge"`
	Do       int `json:"do"`
	ComingUp int `json:"coming_up"`
}

func (c *WaitCounts) add(kind string) {
	switch kind {
	case WaitApprove:
		c.Approve++
	case WaitAnswer:
		c.Answer++
	case WaitMerge:
		c.Merge++
	case WaitDo:
		c.Do++
	default:
		c.ComingUp++
		return
	}
	c.Waiting++
}

// WaitItem is one thing waiting on a person (or coming up for them).
type WaitItem struct {
	Kind string `json:"kind"` // approve | answer | merge | do | coming_up
	// Key identifies the item: "approve:<epic id>" or "task:<ticket id>".
	Key         string `json:"key"`
	ProjectID   int    `json:"project_id"`
	ProjectCode string `json:"project_code"`
	ProjectName string `json:"project_name"`
	// TaskID is the ticket (every kind but approve); TaskIDs every ticket
	// the item contains, so ?focus=<any of them> finds it.
	TaskID  int    `json:"task_id,omitempty"`
	TaskIDs []int  `json:"task_ids"`
	Name    string `json:"name"`
	State   string `json:"state,omitempty"`
	// Since is how long it has waited: approve, the oldest planned_at among
	// its tickets; answer, when the ticket was last updated (blocked);
	// merge, completion; do, when it became ready or started; coming_up,
	// creation.
	Since       time.Time `json:"since"`
	Performer   string    `json:"performer,omitempty"`
	HumanReason string    `json:"human_reason,omitempty"`
	OwnerUserID *int      `json:"owner_user_id,omitempty"`

	// approve
	EpicID   int    `json:"epic_id,omitempty"`
	EpicName string `json:"epic_name,omitempty"`
	Count    int    `json:"count,omitempty"`
	// answer
	Question string `json:"question,omitempty"`
	// merge
	MergeState  string     `json:"merge_state,omitempty"`
	PRURL       string     `json:"pr_url,omitempty"`
	Branch      string     `json:"branch,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	HoldingUp   []int      `json:"holding_up,omitempty"` // open tickets that depend on it directly
	// coming_up: what it still waits on.
	WaitsOn          []repository.DepSummary `json:"waits_on,omitempty"`
	AwaitingApproval bool                    `json:"awaiting_approval,omitempty"`
	StartAt          *time.Time              `json:"start_at,omitempty"` // a schedule holds it

	// Detail (the project tab only): the ticket in full, and for approve
	// the plan in place.
	Task *domain.Task  `json:"task,omitempty"`
	Plan *ApprovalPlan `json:"plan,omitempty"`
}

// Waiting is what WaitingOn found for one person.
type Waiting struct {
	UserID    int                `json:"user_id"`
	Items     []WaitItem         `json:"items"`     // the four kinds, in order, oldest first within each
	ComingUp  []WaitItem         `json:"coming_up"` // oldest first
	Counts    WaitCounts         `json:"counts"`
	ByProject map[int]WaitCounts `json:"-"`
}

// waitOptions say how much WaitingOn fills in.
type waitOptions struct {
	// detail: dependency summaries for coming_up and the ticket on each item.
	detail bool
}

// WaitingOn returns what waits on userID in the given projects (nil: every
// active project). It checks nothing: callers decide who may read whose
// list and which projects.
func (s *Service) WaitingOn(ctx context.Context, userID int, projectIDs []int) (*Waiting, error) {
	return s.waitingOn(ctx, userID, projectIDs, waitOptions{})
}

func (s *Service) waitingOn(ctx context.Context, userID int, projectIDs []int, opt waitOptions) (*Waiting, error) {
	w := &Waiting{UserID: userID, Items: []WaitItem{}, ComingUp: []WaitItem{}, ByProject: map[int]WaitCounts{}}
	cands, err := s.Repo.WaitingCandidates(ctx, userID, projectIDs)
	if err != nil {
		return nil, err
	}
	type epicKey struct{ project, epic int }
	approve := map[epicKey]*WaitItem{}
	var approveOrder []epicKey
	var merges, comingDeps []int
	add := func(it WaitItem) {
		if it.Kind == "coming_up" {
			w.ComingUp = append(w.ComingUp, it)
		} else {
			w.Items = append(w.Items, it)
		}
	}
	single := func(c *repository.WaitTask, kind string, since time.Time) WaitItem {
		t := &c.Task
		it := WaitItem{Kind: kind, Key: fmt.Sprintf("task:%d", t.ID), ProjectID: t.ProjectID, ProjectCode: c.ProjectCode,
			ProjectName: c.ProjectName, TaskID: t.ID, TaskIDs: []int{t.ID}, Name: t.Name, State: string(t.State), Since: since,
			Performer: t.Performer, HumanReason: t.HumanReason, OwnerUserID: t.OwnerUserID, EpicID: t.EpicID}
		if opt.detail {
			cp := *t
			it.Task = &cp
		}
		return it
	}
	for i := range cands {
		c := &cands[i]
		t := &c.Task
		awaits := t.State != domain.TaskStateCompleted && !t.Approved()
		if c.RequirePlanApproval && awaits {
			if who := t.ApprovalOwner(); who != nil && *who == userID {
				k := epicKey{t.ProjectID, t.EpicID}
				it := approve[k]
				if it == nil {
					it = &WaitItem{Kind: WaitApprove, Key: fmt.Sprintf("approve:%d", t.EpicID), ProjectID: t.ProjectID,
						ProjectCode: c.ProjectCode, ProjectName: c.ProjectName, EpicID: t.EpicID, TaskIDs: []int{}, Since: t.Planned()}
					approve[k] = it
					approveOrder = append(approveOrder, k)
				}
				it.TaskIDs = append(it.TaskIDs, t.ID)
				it.Count++
				if t.Planned().Before(it.Since) {
					it.Since = t.Planned()
				}
			}
		}
		if t.OwnerUserID == nil || *t.OwnerUserID != userID {
			continue
		}
		question := t.State == domain.TaskStateBlocked && t.BlockReason != "" && !strings.HasPrefix(t.BlockReason, "scheduled:")
		switch {
		case question:
			it := single(c, WaitAnswer, t.UpdatedAt)
			it.Question = t.BlockReason
			add(it)
			continue
		case t.State == domain.TaskStateCompleted && domain.MergeGates(t.MergeState):
			since := t.UpdatedAt
			if t.CompletedAt != nil {
				since = *t.CompletedAt
			}
			it := single(c, WaitMerge, since)
			it.MergeState, it.CompletedAt = t.MergeState, t.CompletedAt
			add(it)
			merges = append(merges, t.ID)
			continue
		}
		if !t.IsHuman() || t.State == domain.TaskStateCompleted {
			continue
		}
		switch {
		case t.State == domain.TaskStateInProgress:
			since := t.CreatedAt
			if t.DispatchedAt != nil {
				since = *t.DispatchedAt
			} else if t.ReadyAt != nil {
				since = *t.ReadyAt
			}
			add(single(c, WaitDo, since))
		case t.State == domain.TaskStateBacklog && !(c.RequirePlanApproval && awaits):
			since := t.CreatedAt
			if t.ReadyAt != nil {
				since = *t.ReadyAt
			}
			add(single(c, WaitDo, since))
		default:
			// Waiting on dependencies, a schedule, or plan approval.
			it := single(c, "coming_up", t.CreatedAt)
			it.AwaitingApproval = c.RequirePlanApproval && awaits
			if t.StartAt != nil && strings.HasPrefix(t.BlockReason, "scheduled:") {
				it.StartAt = t.StartAt
			}
			add(it)
			comingDeps = append(comingDeps, t.DependencyIDs...)
		}
	}

	// Epic names for approve items.
	if len(approveOrder) > 0 {
		names := map[int]string{}
		for _, k := range approveOrder {
			if _, ok := names[k.epic]; ok {
				continue
			}
			if e, err := s.Repo.GetEpic(ctx, k.epic); err == nil {
				names[k.epic] = e.Name
			}
		}
		for _, k := range approveOrder {
			it := approve[k]
			it.EpicName = names[k.epic]
			it.Name = it.EpicName
			if it.Name == "" {
				it.Name = fmt.Sprintf("Epic %d", k.epic)
			}
			w.Items = append(w.Items, *it)
		}
	}

	// Merge items: where the work is, and what waits on it.
	if len(merges) > 0 {
		refs, err := s.Repo.LatestMergeRefs(ctx, merges)
		if err != nil {
			return nil, err
		}
		deps, err := s.Repo.OpenDependents(ctx, merges)
		if err != nil {
			return nil, err
		}
		for i := range w.Items {
			it := &w.Items[i]
			if it.Kind != WaitMerge {
				continue
			}
			it.PRURL, it.Branch = refs[it.TaskID].PRURL, refs[it.TaskID].Branch
			it.HoldingUp = deps[it.TaskID]
		}
	}

	// Coming up: which dependencies still hold each one.
	if opt.detail && len(comingDeps) > 0 {
		sums, err := s.Repo.DepSummaries(ctx, comingDeps)
		if err != nil {
			return nil, err
		}
		comingIDs := make([]int, 0, len(w.ComingUp))
		for _, it := range w.ComingUp {
			comingIDs = append(comingIDs, it.Task.ID)
		}
		noWait, err := s.Repo.NoMergeWait(ctx, comingIDs)
		if err != nil {
			return nil, err
		}
		for i := range w.ComingUp {
			it := &w.ComingUp[i]
			for _, d := range it.Task.DependencyIDs {
				if ds, ok := sums[d]; ok && !depSatisfiedFor(ds, noWait[it.Task.ID]) {
					it.WaitsOn = append(it.WaitsOn, ds)
				}
			}
		}
	}

	rank := map[string]int{WaitApprove: 0, WaitAnswer: 1, WaitMerge: 2, WaitDo: 3}
	sort.SliceStable(w.Items, func(i, j int) bool {
		a, b := w.Items[i], w.Items[j]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		return a.Key < b.Key
	})
	sort.SliceStable(w.ComingUp, func(i, j int) bool {
		a, b := w.ComingUp[i], w.ComingUp[j]
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		return a.TaskID < b.TaskID
	})
	for _, it := range append(append([]WaitItem{}, w.Items...), w.ComingUp...) {
		w.Counts.add(it.Kind)
		pc := w.ByProject[it.ProjectID]
		pc.add(it.Kind)
		w.ByProject[it.ProjectID] = pc
	}
	return w, nil
}

// ---- The plan in place (approve items) ----

// ApprovalPlan is everything the My work Approve view shows about one or
// more epics' tickets awaiting approval (design #2494, artifact #24 v2):
// every open ticket in the epics in full (awaiting and already approved:
// the outline needs both), their dependencies outside the set, and what
// approving does. With several epics (?approve=172,2 or all) everything is
// computed over the whole set, so a dependency between two of the epics
// counts as inside it, and there is one as_of.
type ApprovalPlan struct {
	// Epic is the first epic of the set (kept for single-epic readers);
	// Epics lists every epic in the set, in the order asked for.
	Epic  PlanEpic   `json:"epic"`
	Epics []PlanEpic `json:"epics"`
	// Stories: every story with an open ticket in the set, including ones
	// whose tickets are all approved already (the view collapses those).
	Stories []PlanStory `json:"stories"`
	// DoneStories: stories in the set whose tickets are all completed.
	DoneStories []PlanStory `json:"done_stories"`
	// AsOf is the newest planned_at among the tickets returned: send it
	// back verbatim as approve-plan's as_of, so anything re-planned while
	// the person read is skipped instead of approved unseen.
	AsOf *time.Time `json:"as_of,omitempty"`
	// CallerLastApprovedAt: when the caller last approved a ticket in this
	// epic ("since your last approval at 12:41").
	CallerLastApprovedAt *time.Time `json:"caller_last_approved_at,omitempty"`
	RequirePlanApproval  bool       `json:"require_plan_approval"`
	// CanApproveOnBehalf: the caller may approve others' tickets here
	// (lead or above, in the web UI), sending on_behalf: true.
	CanApproveOnBehalf bool `json:"can_approve_on_behalf"`

	Tickets []PlanTicket `json:"tickets"`
	// Paging over Tickets (everything else covers the whole epic):
	// TicketsTotal open tickets in the epic, Offset/Limit what this page
	// holds, More whether there are more after it.
	TicketsTotal int  `json:"tickets_total"`
	Offset       int  `json:"offset"`
	Limit        int  `json:"limit"`
	More         bool `json:"more"`

	// AwaitingIDs: every open ticket in the epic awaiting approval, anyone's;
	// YoursIDs the caller's own among them; ForUserIDs the ones whose
	// approval targets the person whose list this is.
	AwaitingIDs []int `json:"awaiting_ids"`
	YoursIDs    []int `json:"yours_ids"`
	ForUserIDs  []int `json:"for_user_ids"`
	// NewIDs never approved; ChangedIDs approved before, re-planned since.
	NewIDs     []int `json:"new_ids"`
	ChangedIDs []int `json:"changed_ids"`
	// StartsOnApproval: awaiting agent tickets dispatch would accept right
	// after approval (ready, every dependency satisfied by the merge rule).
	StartsOnApproval []int `json:"starts_on_approval"`
	// HumanGates: the epic's open human tickets and how much open work
	// waits on each (as amp_check_plan).
	HumanGates []HumanGate `json:"human_gates"`
	// Issues: amp_check_plan's issues narrowed to this epic's tickets.
	Issues []PlanIssue `json:"issues"`
	// Outside: tickets outside the epic that its tickets depend on, or that
	// depend on them.
	Outside []OutsideTicket `json:"outside"`
}

// PlanEpic is an epic an approval covers.
type PlanEpic struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// CallerLastApprovedAt: when the caller last approved a ticket in this epic.
	CallerLastApprovedAt *time.Time `json:"caller_last_approved_at,omitempty"`
	// TicketCount: open tickets in this epic; AwaitingIDs those awaiting approval.
	TicketCount int   `json:"ticket_count"`
	AwaitingIDs []int `json:"awaiting_ids"`
}

// PlanStory is a story with open tickets in the set (or, in DoneStories,
// one with only completed tickets).
type PlanStory struct {
	ID                 int    `json:"id"`
	EpicID             int    `json:"epic_id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	TaskIDs            []int  `json:"task_ids"`
	AwaitingIDs        []int  `json:"awaiting_ids"`
}

// PlanTicket is one open ticket in an approval plan, in full.
type PlanTicket struct {
	ID                 int        `json:"id"`
	EpicID             int        `json:"epic_id"`
	StoryID            int        `json:"story_id"`
	Name               string     `json:"name"`
	Description        string     `json:"description"`
	AcceptanceCriteria string     `json:"acceptance_criteria"`
	Priority           string     `json:"priority"`
	State              string     `json:"state"`
	BlockReason        string     `json:"block_reason,omitempty"`
	Performer          string     `json:"performer"`
	HumanReason        string     `json:"human_reason,omitempty"`
	AssignedTo         string     `json:"assigned_to,omitempty"`
	OwnerUserID        *int       `json:"owner_user_id"`
	OwnerName          string     `json:"owner_name"`
	CreatedBy          *int       `json:"created_by"`
	PlannedAt          time.Time  `json:"planned_at"`
	ApprovedAt         *time.Time `json:"approved_at,omitempty"`
	ApprovedBy         *int       `json:"approved_by,omitempty"`
	ApprovedFor        *int       `json:"approved_for,omitempty"`
	MergeState         string     `json:"merge_state"`
	// Awaiting: not approved as planned now. Yours: the caller approves it
	// as their own (Task.ApprovesAsOwn).
	Awaiting bool `json:"awaiting"`
	Yours    bool `json:"yours"`
	// Change: "new" (never approved), "changed" (approved, re-planned
	// since) or null.
	Change       *string       `json:"change"`
	ChangedSince *ChangedSince `json:"changed_since,omitempty"`
	// DependencyIDs: every dependency, wherever it is; DependentIDs the
	// open tickets that depend on it; WaitsOn the dependencies not
	// satisfied now.
	DependencyIDs    []int `json:"dependency_ids"`
	DependentIDs     []int `json:"dependent_ids"`
	WaitsOn          []int `json:"waits_on"`
	StartsOnApproval bool  `json:"starts_on_approval"`
}

// ChangedSince says what changed on a ticket since it was last approved:
// when it was re-planned (and by whom when that's known), and Previous,
// the ticket as it was approved (name, description, acceptance criteria,
// dependency ids) from the snapshot ApproveTask stores (migration 040).
// Previous is absent for tickets approved before that snapshot existed.
type ChangedSince struct {
	At             time.Time                    `json:"at"`
	By             string                       `json:"by,omitempty"`
	LastApprovedAt *time.Time                   `json:"last_approved_at,omitempty"`
	Previous       *repository.ApprovedSnapshot `json:"previous,omitempty"`
}

// OutsideTicket is a ticket outside the approval's epic that its tickets
// depend on ("dependency"), that depends on them ("dependent"), or both.
type OutsideTicket struct {
	repository.DepSummary
	EpicName    string `json:"epic_name"`
	ProjectCode string `json:"project_code,omitempty"` // set when in another project
	ProjectName string `json:"project_name,omitempty"`
	Relation    string `json:"relation"`
}

// MaxPlanTickets is the most tickets an approval plan returns at once.
const MaxPlanTickets = 200

// planCtx is what building approval plans for one project needs, loaded once.
type planCtx struct {
	proj    *domain.Project
	all     []domain.Task
	byID    map[int]*domain.Task
	stories []domain.Story
	epics   map[int]domain.Epic
	graph   *PlanReport
	down    map[int][]int // open dependents, same project
	names   map[int]string
}

func (s *Service) loadPlanCtx(ctx context.Context, projectID int) (*planCtx, error) {
	pc := &planCtx{byID: map[int]*domain.Task{}, epics: map[int]domain.Epic{}, down: map[int][]int{}, names: map[int]string{}}
	var err error
	if pc.proj, err = s.Repo.GetProject(ctx, projectID); err != nil {
		return nil, err
	}
	if pc.all, err = s.Repo.ListTasks(ctx, projectID, ""); err != nil {
		return nil, err
	}
	if pc.stories, err = s.Repo.ListStoriesByProject(ctx, projectID); err != nil {
		return nil, err
	}
	epics, err := s.Repo.ListEpics(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, e := range epics {
		pc.epics[e.ID] = e
	}
	for i := range pc.all {
		pc.byID[pc.all[i].ID] = &pc.all[i]
	}
	for _, t := range pc.all {
		if t.State == domain.TaskStateCompleted {
			continue
		}
		for _, d := range t.DependencyIDs {
			pc.down[d] = append(pc.down[d], t.ID)
		}
	}
	pc.graph = &PlanReport{ReadyNow: []int{}, Issues: []PlanIssue{}, HumanGates: []HumanGate{}}
	planGraph(pc.graph, pc.all, pc.stories)
	return pc, nil
}

func (s *Service) userName(ctx context.Context, cache map[int]string, id *int) string {
	if id == nil {
		return ""
	}
	if n, ok := cache[*id]; ok {
		return n
	}
	n := fmt.Sprintf("User #%d", *id)
	if u, err := s.Repo.GetUserByID(ctx, *id); err == nil {
		switch {
		case u.DisplayName != "":
			n = u.DisplayName
		case u.Email != "":
			n = u.Email
		}
	}
	cache[*id] = n
	return n
}

// approvalPlan builds the plan for the epics in epicIDs (one or more, in
// the order given): caller is who reads it (yours, on-behalf), forUser
// whose list it is. offset/limit page the tickets.
func (s *Service) approvalPlan(ctx context.Context, pc *planCtx, caller authz.Principal, lvl authz.Level, forUser int, epicIDs []int, offset, limit int) (*ApprovalPlan, error) {
	if limit <= 0 || limit > MaxPlanTickets {
		limit = MaxPlanTickets
	}
	if offset < 0 {
		offset = 0
	}
	ap := &ApprovalPlan{Epics: []PlanEpic{}, Stories: []PlanStory{}, DoneStories: []PlanStory{},
		RequirePlanApproval: pc.proj.RequirePlanApproval, CanApproveOnBehalf: lvl >= authz.Lead && caller.InUI(),
		Tickets: []PlanTicket{}, Offset: offset, Limit: limit, AwaitingIDs: []int{}, YoursIDs: []int{}, ForUserIDs: []int{},
		NewIDs: []int{}, ChangedIDs: []int{}, StartsOnApproval: []int{}, HumanGates: []HumanGate{}, Issues: []PlanIssue{},
		Outside: []OutsideTicket{}}

	epicIdx := map[int]int{} // epic id -> index in ap.Epics
	for _, id := range epicIDs {
		if _, dup := epicIdx[id]; dup {
			continue
		}
		e, ok := pc.epics[id]
		if !ok {
			return nil, authz.NotFound("epic %d not found in project %d", id, pc.proj.ID)
		}
		epicIdx[id] = len(ap.Epics)
		ap.Epics = append(ap.Epics, PlanEpic{ID: e.ID, Name: e.Name, Description: e.Description, AwaitingIDs: []int{}})
	}
	// lastApproved records a caller approval at t in epic ei.
	lastApproved := func(ei int, t *domain.Task) {
		if t.ApprovedBy == nil || *t.ApprovedBy != caller.UserID || t.ApprovedAt == nil {
			return
		}
		at := *t.ApprovedAt
		if e := &ap.Epics[ei]; e.CallerLastApprovedAt == nil || at.After(*e.CallerLastApprovedAt) {
			e.CallerLastApprovedAt = &at
		}
		if ap.CallerLastApprovedAt == nil || at.After(*ap.CallerLastApprovedAt) {
			ap.CallerLastApprovedAt = &at
		}
	}

	var open []*domain.Task
	inEpic := map[int]bool{} // open tickets in the set
	for i := range pc.all {
		t := &pc.all[i]
		if _, ok := epicIdx[t.EpicID]; ok && t.State != domain.TaskStateCompleted {
			open = append(open, t)
			inEpic[t.ID] = true
		}
	}
	// Dependency states (merge-aware), for every dependency of the epic's tickets.
	var depIDs []int
	for _, t := range open {
		depIDs = append(depIDs, t.DependencyIDs...)
	}
	sums, err := s.Repo.DepSummaries(ctx, depIDs)
	if err != nil {
		return nil, err
	}
	openIDs := make([]int, 0, len(open))
	for _, t := range open {
		openIDs = append(openIDs, t.ID)
	}
	noWait, err := s.Repo.NoMergeWait(ctx, openIDs)
	if err != nil {
		return nil, err
	}
	storyIdx := map[int]int{}
	for _, st := range pc.stories {
		if _, ok := epicIdx[st.EpicID]; ok {
			storyIdx[st.ID] = len(ap.Stories)
			ap.Stories = append(ap.Stories, PlanStory{ID: st.ID, EpicID: st.EpicID, Name: st.Name, Description: st.Description,
				AcceptanceCriteria: st.AcceptanceCriteria, TaskIDs: []int{}, AwaitingIDs: []int{}})
		}
	}
	// The approval snapshots of re-planned tickets, for the diff.
	var changedIDs []int
	for _, t := range open {
		if !t.Approved() && t.ApprovedAt != nil {
			changedIDs = append(changedIDs, t.ID)
		}
	}
	snaps, err := s.Repo.ApprovedSnapshots(ctx, changedIDs)
	if err != nil {
		return nil, err
	}
	tickets := make([]PlanTicket, 0, len(open))
	for _, t := range open {
		ei := epicIdx[t.EpicID]
		ap.Epics[ei].TicketCount++
		pt := PlanTicket{ID: t.ID, EpicID: t.EpicID, StoryID: t.StoryID, Name: t.Name, Description: t.Description, AcceptanceCriteria: t.AcceptanceCriteria,
			Priority: t.Priority, State: string(t.State), BlockReason: t.BlockReason, Performer: t.Performer, HumanReason: t.HumanReason,
			AssignedTo: t.AssignedTo, OwnerUserID: t.OwnerUserID, OwnerName: s.userName(ctx, pc.names, t.ApprovalOwner()), CreatedBy: t.CreatedBy,
			PlannedAt: t.Planned(), ApprovedAt: t.ApprovedAt, ApprovedBy: t.ApprovedBy, ApprovedFor: t.ApprovedFor, MergeState: t.MergeState,
			Awaiting: !t.Approved(), Yours: t.ApprovesAsOwn(caller.UserID),
			DependencyIDs: append([]int{}, t.DependencyIDs...), DependentIDs: append([]int{}, pc.down[t.ID]...), WaitsOn: []int{}}
		if t.IsHuman() {
			pt.AssignedTo = ""
		}
		for _, d := range t.DependencyIDs {
			if !depSatisfiedFor(sums[d], noWait[t.ID]) {
				pt.WaitsOn = append(pt.WaitsOn, d)
			}
		}
		if i, ok := storyIdx[t.StoryID]; ok {
			ap.Stories[i].TaskIDs = append(ap.Stories[i].TaskIDs, t.ID)
		}
		if pt.Awaiting {
			ap.AwaitingIDs = append(ap.AwaitingIDs, t.ID)
			ap.Epics[ei].AwaitingIDs = append(ap.Epics[ei].AwaitingIDs, t.ID)
			if i, ok := storyIdx[t.StoryID]; ok {
				ap.Stories[i].AwaitingIDs = append(ap.Stories[i].AwaitingIDs, t.ID)
			}
			if pt.Yours {
				ap.YoursIDs = append(ap.YoursIDs, t.ID)
			}
			if who := t.ApprovalOwner(); who != nil && *who == forUser {
				ap.ForUserIDs = append(ap.ForUserIDs, t.ID)
			}
			change := "new"
			if t.ApprovedAt != nil {
				change = "changed"
				pt.ChangedSince = &ChangedSince{At: t.Planned(), LastApprovedAt: t.ApprovedAt}
				if snap, ok := snaps[t.ID]; ok {
					pt.ChangedSince.Previous = &snap
				}
				ap.ChangedIDs = append(ap.ChangedIDs, t.ID)
			} else {
				ap.NewIDs = append(ap.NewIDs, t.ID)
			}
			pt.Change = &change
			if !t.IsHuman() && t.State == domain.TaskStateBacklog && len(pt.WaitsOn) == 0 {
				pt.StartsOnApproval = true
				ap.StartsOnApproval = append(ap.StartsOnApproval, t.ID)
			}
		}
		if ap.AsOf == nil || pt.PlannedAt.After(*ap.AsOf) {
			at := pt.PlannedAt
			ap.AsOf = &at
		}
		tickets = append(tickets, pt)
	}
	// Every ticket the caller approved counts for "since your last
	// approval", completed ones too; completed tickets mark done stories.
	storyDone := map[int]int{} // story id -> completed tickets
	for i := range pc.all {
		t := &pc.all[i]
		ei, ok := epicIdx[t.EpicID]
		if !ok {
			continue
		}
		lastApproved(ei, t)
		if t.State == domain.TaskStateCompleted {
			storyDone[t.StoryID]++
		}
	}
	// Stories with open tickets are part of the approval, even when all of
	// them are approved already; stories with only completed tickets are
	// listed apart, as done.
	stories := make([]PlanStory, 0, len(ap.Stories))
	for _, st := range ap.Stories {
		switch {
		case len(st.TaskIDs) > 0:
			stories = append(stories, st)
		case storyDone[st.ID] > 0:
			st.TaskIDs = []int{}
			for i := range pc.all {
				if pc.all[i].StoryID == st.ID {
					st.TaskIDs = append(st.TaskIDs, pc.all[i].ID)
				}
			}
			ap.DoneStories = append(ap.DoneStories, st)
		}
	}
	ap.Stories = stories
	if len(ap.Epics) > 0 {
		ap.Epic = ap.Epics[0]
	}

	// The plan check, narrowed to this epic's tickets.
	for _, iss := range pc.graph.Issues {
		var ids []int
		for _, id := range iss.TaskIDs {
			if inEpic[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		iss.TaskIDs = ids
		ap.Issues = append(ap.Issues, iss)
	}
	// Too much starting at once, for this approval.
	if n := len(ap.StartsOnApproval); n > readyFanoutThreshold && n*2 > len(ap.AwaitingIDs) &&
		!slices.ContainsFunc(ap.Issues, func(i PlanIssue) bool { return i.Kind == "wide_start" }) {
		ap.Issues = append(ap.Issues, PlanIssue{Kind: "wide_start", TaskIDs: ap.StartsOnApproval,
			Message: fmt.Sprintf("%d of the %d tickets awaiting approval would start at once. That much parallel work usually means dependencies are missing.", n, len(ap.AwaitingIDs)),
			Fix:     "Check each one: does it need anything built first? Add those dependencies."})
	}
	for _, g := range pc.graph.HumanGates {
		if inEpic[g.ID] {
			ap.HumanGates = append(ap.HumanGates, g)
		}
	}

	// Outside the epic: dependencies and dependents.
	rel := map[int]string{}
	mark := func(id int, r string) {
		if inEpic[id] {
			return
		}
		switch rel[id] {
		case "":
			rel[id] = r
		case r:
		default:
			rel[id] = "both"
		}
	}
	for _, t := range open {
		for _, d := range t.DependencyIDs {
			mark(d, "dependency")
		}
		for _, d := range pc.down[t.ID] {
			mark(d, "dependent")
		}
	}
	if len(rel) > 0 {
		ids := make([]int, 0, len(rel))
		for id := range rel {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		out, err := s.Repo.DepSummaries(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			d, ok := out[id]
			if !ok {
				continue
			}
			ot := OutsideTicket{DepSummary: d, Relation: rel[id]}
			if e, ok := pc.epics[d.EpicID]; ok {
				ot.EpicName = e.Name
			} else if e, err := s.Repo.GetEpic(ctx, d.EpicID); err == nil {
				ot.EpicName = e.Name
			}
			if d.ProjectID != pc.proj.ID {
				if p, err := s.Repo.GetProject(ctx, d.ProjectID); err == nil {
					ot.ProjectCode, ot.ProjectName = p.Code, p.Name
				}
			}
			ap.Outside = append(ap.Outside, ot)
		}
	}

	ap.TicketsTotal = len(tickets)
	end := min(offset+limit, len(tickets))
	if offset < len(tickets) {
		ap.Tickets = tickets[offset:end]
	}
	ap.More = end < len(tickets)
	return ap, nil
}

// ---- The project's My work tab ----

// ProjectMyWork is GET /api/projects/{id}/my-work: what waits on a person
// in one project, in full, with what they cleared today and their agents.
type ProjectMyWork struct {
	ProjectID int `json:"project_id"`
	UserID    int `json:"user_id"`
	// ReadOnly: the caller is reading someone else's list (a lead acting
	// for them): the actions are theirs.
	ReadOnly bool                  `json:"read_only"`
	Kinds    []string              `json:"kinds"` // display order
	Counts   WaitCounts            `json:"counts"`
	Items    map[string][]WaitItem `json:"items"` // by kind
	ComingUp []WaitItem            `json:"coming_up"`
	// DoneToday: what was cleared today (in tz), oldest first. Approvals
	// are one entry per epic, merges say who recorded them.
	DoneToday []DoneItem             `json:"done_today"`
	Agents    repository.AgentCounts `json:"agents"`
	Day       string                 `json:"day"`
}

// DoneItem is something cleared from a person's list.
type DoneItem struct {
	Kind        string    `json:"kind"`
	TaskID      int       `json:"task_id,omitempty"`
	TaskIDs     []int     `json:"task_ids"`
	EpicID      int       `json:"epic_id,omitempty"`
	Name        string    `json:"name"`
	At          time.Time `json:"at"`
	Count       int       `json:"count"`
	MergeSource string    `json:"merge_source,omitempty"`
	MergeState  string    `json:"merge_state,omitempty"`
}

// ProjectMyWorkOptions are ProjectMyWork's query parameters.
type ProjectMyWorkOptions struct {
	User string // "", "me", an id or an email: whose list (others' need lead access)
	TZ   string // for "today"
	// Approve, when set, asks for one epic's approval plan only
	// (ApprovalPlanFor); PlanOffset/PlanLimit page its tickets.
	PlanOffset, PlanLimit int
}

// myWorkSubject resolves whose list p asks for in projectID and checks p
// may read it: their own (any member), or anyone's as a lead or above.
func (s *Service) myWorkSubject(ctx context.Context, p authz.Principal, projectID int, ref string) (int, authz.Level, error) {
	lvl, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member)
	if err != nil {
		return 0, lvl, err
	}
	if !p.HasUser() && (ref == "" || strings.EqualFold(ref, "me")) {
		return 0, lvl, authz.BadRequest("My work needs a signed-in AMP user")
	}
	uid := p.UserID
	if ref = strings.TrimSpace(ref); ref != "" && !strings.EqualFold(ref, "me") {
		if uid, err = s.Authz.ResolveUser(ctx, p, ref); err != nil {
			return 0, lvl, err
		}
	}
	if uid != p.UserID && lvl < authz.Lead {
		return 0, lvl, authz.Forbidden("only project leads, owners and admins can read another person's My work in project %d; you can read your own", projectID)
	}
	return uid, lvl, nil
}

// ProjectMyWork returns what waits on a person in projectID, with each
// approve item's plan in place.
func (s *Service) ProjectMyWork(ctx context.Context, p authz.Principal, projectID int, opt ProjectMyWorkOptions) (*ProjectMyWork, error) {
	uid, lvl, err := s.myWorkSubject(ctx, p, projectID, opt.User)
	if err != nil {
		return nil, err
	}
	day, start, end, err := DayWindow("", opt.TZ)
	if err != nil {
		return nil, err
	}
	w, err := s.waitingOn(ctx, uid, []int{projectID}, waitOptions{detail: true})
	if err != nil {
		return nil, err
	}
	out := &ProjectMyWork{ProjectID: projectID, UserID: uid, ReadOnly: uid != p.UserID, Kinds: WaitKinds, Counts: w.Counts,
		Items: map[string][]WaitItem{}, ComingUp: w.ComingUp, DoneToday: []DoneItem{}, Day: day,
		Agents: repository.AgentCounts{ProjectID: projectID}}
	for _, k := range WaitKinds {
		out.Items[k] = []WaitItem{}
	}
	var pc *planCtx
	for _, it := range w.Items {
		if it.Kind == WaitApprove {
			if pc == nil {
				if pc, err = s.loadPlanCtx(ctx, projectID); err != nil {
					return nil, err
				}
			}
			if it.Plan, err = s.approvalPlan(ctx, pc, p, lvl, uid, []int{it.EpicID}, opt.PlanOffset, opt.PlanLimit); err != nil {
				return nil, err
			}
		}
		out.Items[it.Kind] = append(out.Items[it.Kind], it)
	}

	cleared, err := s.Repo.ClearedBetween(ctx, uid, projectID, start, end)
	if err != nil {
		return nil, err
	}
	epicDone := map[int]int{} // epic id -> index in DoneToday
	var epicNames map[int]string
	for _, c := range cleared {
		if c.Kind == WaitApprove {
			if i, ok := epicDone[c.EpicID]; ok {
				d := &out.DoneToday[i]
				d.TaskIDs = append(d.TaskIDs, c.TaskID)
				d.Count++
				if c.At.After(d.At) {
					d.At = c.At
				}
				continue
			}
			if epicNames == nil {
				epicNames = map[int]string{}
				if epics, err := s.Repo.ListEpics(ctx, projectID); err == nil {
					for _, e := range epics {
						epicNames[e.ID] = e.Name
					}
				}
			}
			epicDone[c.EpicID] = len(out.DoneToday)
			out.DoneToday = append(out.DoneToday, DoneItem{Kind: WaitApprove, TaskIDs: []int{c.TaskID}, EpicID: c.EpicID,
				Name: epicNames[c.EpicID], At: c.At, Count: 1})
			continue
		}
		out.DoneToday = append(out.DoneToday, DoneItem{Kind: c.Kind, TaskID: c.TaskID, TaskIDs: []int{c.TaskID}, EpicID: c.EpicID,
			Name: c.Name, At: c.At, Count: 1, MergeSource: c.MergeSource, MergeState: c.MergeState})
	}
	sort.SliceStable(out.DoneToday, func(i, j int) bool { return out.DoneToday[i].At.Before(out.DoneToday[j].At) })

	agents, err := s.Repo.AgentCountsFor(ctx, uid, []int{projectID}, start)
	if err != nil {
		return nil, err
	}
	if a, ok := agents[projectID]; ok {
		out.Agents = a
	}
	return out, nil
}

// ApprovalPlanFor returns the approval plan of one or more epics in
// projectID, for the full Approve view (?approve=<epic id>[,<epic id>…]),
// paged by offset/limit. It is returned whether or not anything in it
// awaits the person. With no epic ids (?approve=all) the set is every epic
// with tickets awaiting the person's approval, oldest first; when nothing
// awaits them the plan has no epics and no tickets.
func (s *Service) ApprovalPlanFor(ctx context.Context, p authz.Principal, projectID int, epicIDs []int, opt ProjectMyWorkOptions) (*ApprovalPlan, error) {
	uid, lvl, err := s.myWorkSubject(ctx, p, projectID, opt.User)
	if err != nil {
		return nil, err
	}
	if len(epicIDs) == 0 {
		w, err := s.waitingOn(ctx, uid, []int{projectID}, waitOptions{})
		if err != nil {
			return nil, err
		}
		for _, it := range w.Items {
			if it.Kind == WaitApprove {
				epicIDs = append(epicIDs, it.EpicID)
			}
		}
	}
	pc, err := s.loadPlanCtx(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return s.approvalPlan(ctx, pc, p, lvl, uid, epicIDs, opt.PlanOffset, opt.PlanLimit)
}

// ---- Across projects: the summary and the badge ----

// ProjectWait is one project's card in the top-level My work summary.
type ProjectWait struct {
	ProjectID   int        `json:"project_id"`
	ProjectCode string     `json:"project_code"`
	ProjectName string     `json:"project_name"`
	Counts      WaitCounts `json:"counts"`
	// Rows: the first items (the four kinds in order, oldest first), at most
	// the rows asked for; More how many more there are in the project.
	Rows []WaitItem `json:"rows"`
	More int        `json:"more"`
	// AgentsInProgress: the person's agent tickets in progress here.
	AgentsInProgress int `json:"agents_in_progress"`
}

// MyWorkSummary is the new part of GET /api/my-work: counts across
// projects and one card per project with something for the person.
type MyWorkSummary struct {
	Counts WaitCounts `json:"counts"`
	// Projects: projects with anything waiting or coming up, most waiting
	// first, paged by ProjectsOffset/ProjectsLimit over ProjectsTotal.
	Projects       []ProjectWait `json:"projects"`
	ProjectsTotal  int           `json:"projects_total"`
	ProjectsOffset int           `json:"projects_offset"`
	ProjectsLimit  int           `json:"projects_limit"`
	ProjectsMore   bool          `json:"projects_more"`
	RowsPerProject int           `json:"rows_per_project"`
	// ClearProjects: the person's projects with nothing for them.
	ClearProjects []repository.ProjectRef `json:"clear_projects"`
	// AgentsInProgress: the person's agent tickets in progress, everywhere.
	AgentsInProgress int `json:"agents_in_progress"`
}

// MyWorkPageOptions page the summary.
type MyWorkPageOptions struct {
	Rows           int // rows per project (default 6, at most 50; 0 means the default)
	ProjectsOffset int
	ProjectsLimit  int // default 50, at most 200
	TZ             string
}

// visibleIDs is the projects p may see as a list (nil: all, for a global admin).
func (s *Service) visibleIDs(ctx context.Context, p authz.Principal) ([]int, error) {
	visible, err := s.Authz.VisibleProjects(ctx, p)
	if err != nil || visible == nil {
		return nil, err
	}
	ids := make([]int, 0, len(visible))
	for id := range visible {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids, nil
}

// MyWorkSummaryFor builds the summary for p across the projects they can see.
func (s *Service) MyWorkSummaryFor(ctx context.Context, p authz.Principal, opt MyWorkPageOptions) (*MyWorkSummary, error) {
	rows := opt.Rows
	if rows <= 0 {
		rows = 6
	}
	rows = min(rows, 50)
	limit := opt.ProjectsLimit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	offset := max(opt.ProjectsOffset, 0)
	out := &MyWorkSummary{Projects: []ProjectWait{}, ClearProjects: []repository.ProjectRef{}, ProjectsOffset: offset,
		ProjectsLimit: limit, RowsPerProject: rows}
	if !p.HasUser() {
		return out, nil
	}
	ids, err := s.visibleIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	if ids != nil && len(ids) == 0 {
		return out, nil
	}
	w, err := s.waitingOn(ctx, p.UserID, ids, waitOptions{})
	if err != nil {
		return nil, err
	}
	out.Counts = w.Counts
	_, start, _, err := DayWindow("", opt.TZ)
	if err != nil {
		return nil, err
	}
	agents, err := s.Repo.AgentCountsFor(ctx, p.UserID, ids, start)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		out.AgentsInProgress += a.InProgress
	}
	byProject := map[int]*ProjectWait{}
	var cards []*ProjectWait
	for _, it := range w.Items {
		pw := byProject[it.ProjectID]
		if pw == nil {
			pw = &ProjectWait{ProjectID: it.ProjectID, ProjectCode: it.ProjectCode, ProjectName: it.ProjectName,
				Counts: w.ByProject[it.ProjectID], Rows: []WaitItem{}, AgentsInProgress: agents[it.ProjectID].InProgress}
			byProject[it.ProjectID] = pw
			cards = append(cards, pw)
		}
		if len(pw.Rows) < rows {
			pw.Rows = append(pw.Rows, it)
		} else {
			pw.More++
		}
	}
	for _, it := range w.ComingUp { // coming up only: a card with no rows
		if byProject[it.ProjectID] == nil {
			pw := &ProjectWait{ProjectID: it.ProjectID, ProjectCode: it.ProjectCode, ProjectName: it.ProjectName,
				Counts: w.ByProject[it.ProjectID], Rows: []WaitItem{}, AgentsInProgress: agents[it.ProjectID].InProgress}
			byProject[it.ProjectID] = pw
			cards = append(cards, pw)
		}
	}
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := cards[i], cards[j]
		if a.Counts.Waiting != b.Counts.Waiting {
			return a.Counts.Waiting > b.Counts.Waiting
		}
		if a.Counts.ComingUp != b.Counts.ComingUp {
			return a.Counts.ComingUp > b.Counts.ComingUp
		}
		if !strings.EqualFold(a.ProjectName, b.ProjectName) {
			return strings.ToLower(a.ProjectName) < strings.ToLower(b.ProjectName)
		}
		return a.ProjectID < b.ProjectID
	})
	out.ProjectsTotal = len(cards)
	end := min(offset+limit, len(cards))
	for i := offset; i < end; i++ {
		out.Projects = append(out.Projects, *cards[i])
	}
	out.ProjectsMore = end < len(cards)

	refs, err := s.Repo.ActiveProjectRefs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if byProject[r.ID] == nil {
			out.ClearProjects = append(out.ClearProjects, r)
		}
	}
	return out, nil
}

// MyWorkCountsResult is the sidebar badge: totals and each project's counts.
type MyWorkCountsResult struct {
	Counts   WaitCounts         `json:"counts"`
	Projects map[int]WaitCounts `json:"projects"` // only projects with something waiting or coming up
}

// MyWorkCounts counts what waits on p, in total and per project they can see.
func (s *Service) MyWorkCounts(ctx context.Context, p authz.Principal) (*MyWorkCountsResult, error) {
	out := &MyWorkCountsResult{Projects: map[int]WaitCounts{}}
	if !p.HasUser() {
		return out, nil
	}
	ids, err := s.visibleIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	if ids != nil && len(ids) == 0 {
		return out, nil
	}
	w, err := s.waitingOn(ctx, p.UserID, ids, waitOptions{})
	if err != nil {
		return nil, err
	}
	out.Counts, out.Projects = w.Counts, w.ByProject
	return out, nil
}

// depSatisfiedFor reports whether dependency ds no longer holds a dependent
// that does (noWait false) or doesn't (waits_for_merge: false, AMP #2596)
// wait for its dependencies' merges.
func depSatisfiedFor(ds repository.DepSummary, noWait bool) bool {
	return ds.Satisfied || (noWait && ds.State == string(domain.TaskStateCompleted))
}
