package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
)

// Planning: creating tickets with their dependencies, changing the
// dependency graph and the epic/story structure afterwards, and checking a
// plan. AMP's scheduling is only as good as the dependency graph — a ticket
// with no dependencies is dispatchable immediately, in parallel with every
// other ready ticket — so dependencies are validated on every change and
// missing ones are called out.

// NoDependenciesWarning is returned with a new ticket created without
// dependencies in a project that already has other tickets.
const NoDependenciesWarning = "This ticket has no dependencies, so it is ready to dispatch right now, in parallel with " +
	"every other ready ticket. If it needs another ticket's output (an API, a schema, a design, a fix), add " +
	"dependencies with amp_set_task_dependencies before anything is dispatched. Run amp_check_plan when you " +
	"finish planning."

// ValidateDependencies checks a ticket's proposed dependencies: they exist,
// are in the same project, aren't the ticket itself, and (for an existing
// ticket) don't make a cycle. It returns them de-duplicated and sorted.
func (s *Service) ValidateDependencies(ctx context.Context, projectID, taskID int, deps []int) ([]int, error) {
	seen := map[int]bool{}
	clean := make([]int, 0, len(deps))
	for _, d := range deps {
		if d == taskID && taskID != 0 {
			return nil, authz.BadRequest("task %d can't depend on itself", taskID)
		}
		if !seen[d] {
			seen[d] = true
			clean = append(clean, d)
		}
	}
	sort.Ints(clean)
	infos, err := s.Repo.DepInfos(ctx, clean)
	if err != nil {
		return nil, err
	}
	var missing, elsewhere []string
	for _, d := range clean {
		info, ok := infos[d]
		switch {
		case !ok:
			missing = append(missing, fmt.Sprint(d))
		case info.ProjectID != projectID:
			elsewhere = append(elsewhere, fmt.Sprint(d))
		}
	}
	if len(missing) > 0 {
		return nil, authz.BadRequest("dependency task(s) %s don't exist", strings.Join(missing, ", "))
	}
	if len(elsewhere) > 0 {
		return nil, authz.BadRequest("dependency task(s) %s are in another project; dependencies must be in project %d", strings.Join(elsewhere, ", "), projectID)
	}
	if taskID != 0 && len(clean) > 0 {
		edges, err := s.Repo.ProjectDependencies(ctx, projectID)
		if err != nil {
			return nil, err
		}
		edges[taskID] = clean
		if path := findCycle(edges, taskID); path != nil {
			return nil, authz.BadRequest("that would make a cycle: %s; a ticket can't (indirectly) depend on itself", formatPath(path))
		}
	}
	return clean, nil
}

// findCycle returns a dependency path from start back to start, if any.
func findCycle(edges map[int][]int, start int) []int {
	var path []int
	seen := map[int]bool{}
	var walk func(n int) bool
	walk = func(n int) bool {
		path = append(path, n)
		for _, d := range edges[n] {
			if d == start {
				path = append(path, d)
				return true
			}
			if !seen[d] {
				seen[d] = true
				if walk(d) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		return false
	}
	if walk(start) {
		return path
	}
	return nil
}

func formatPath(path []int) string {
	parts := make([]string, len(path))
	for i, id := range path {
		parts[i] = fmt.Sprintf("#%d", id)
	}
	return strings.Join(parts, " depends on ")
}

// CreateTask creates a ticket in a story (its epic is the story's), after
// checking its dependencies. The ticket's owner is resolved from ownerRef
// as for ResolveNewTaskOwner. Warnings point out likely planning mistakes.
func (s *Service) CreateTask(ctx context.Context, p authz.Principal, req domain.CreateTaskRequest, ownerRef string) (*domain.Task, []string, error) {
	story, err := s.Repo.GetStory(ctx, req.StoryID)
	if err != nil || story.ProjectID != req.ProjectID {
		return nil, nil, authz.BadRequest("story %d is not in project %d", req.StoryID, req.ProjectID)
	}
	if req.EpicID != 0 && req.EpicID != story.EpicID {
		return nil, nil, authz.BadRequest("story %d belongs to epic %d, not epic %d (epic_id can be left out: it's the story's)", story.ID, story.EpicID, req.EpicID)
	}
	req.EpicID = story.EpicID
	if strings.TrimSpace(req.Name) == "" {
		return nil, nil, authz.BadRequest("name is required")
	}
	if req.DependencyIDs, err = s.ValidateDependencies(ctx, req.ProjectID, 0, req.DependencyIDs); err != nil {
		return nil, nil, err
	}
	if req.OwnerUserID, err = s.ResolveNewTaskOwner(ctx, p, req.ProjectID, ownerRef); err != nil {
		return nil, nil, err
	}
	req.CreatedBy = p.UserRef()

	pid, err := s.Registry.Get(req.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	replyCh := make(chan actor.ReplyCreateTask, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgCreateTask{Req: req, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, nil, reply.Err
	}

	var warnings []string
	if len(req.DependencyIDs) == 0 {
		others, err := s.Repo.ListTasks(ctx, req.ProjectID, "")
		if err == nil && len(others) > 1 {
			warnings = append(warnings, NoDependenciesWarning)
		}
	}
	return reply.Task, warnings, nil
}

// loadForPlanning loads a ticket and checks p may change how it fits the plan.
func (s *Service) loadForPlanning(ctx context.Context, p authz.Principal, taskID int) (*domain.Task, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskPlan)
	return t, err
}

// SetTaskDependencies replaces a ticket's dependencies. A ready ticket that
// now waits on something becomes blocked; a ticket blocked only by its
// dependencies becomes ready once none are left.
func (s *Service) SetTaskDependencies(ctx context.Context, p authz.Principal, taskID int, deps []int) (*domain.Task, []string, error) {
	t, err := s.loadForPlanning(ctx, p, taskID)
	if err != nil {
		return nil, nil, err
	}
	if deps, err = s.ValidateDependencies(ctx, t.ProjectID, taskID, deps); err != nil {
		return nil, nil, err
	}
	pid, err := s.Registry.Get(t.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	replyCh := make(chan actor.ReplyGetTask, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgSetTaskDeps{TaskID: taskID, DependencyIDs: deps, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, nil, reply.Err
	}
	var warnings []string
	if (reply.Task.State == domain.TaskStateInProgress || reply.Task.State == domain.TaskStateCompleted) && len(reply.Task.BlockedByIDs) > 0 {
		warnings = append(warnings, fmt.Sprintf("Task %d is already %s, so it keeps that state even though it now depends on unfinished work (%s).",
			taskID, strings.ReplaceAll(string(reply.Task.State), "_", " "), joinIDs(reply.Task.BlockedByIDs)))
	}
	return reply.Task, warnings, nil
}

// EditDependencies adds and removes dependencies of a ticket.
func (s *Service) EditDependencies(ctx context.Context, p authz.Principal, taskID int, add, remove []int) (*domain.Task, []string, error) {
	current, err := s.Repo.TaskDependencies(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	drop := map[int]bool{}
	for _, r := range remove {
		drop[r] = true
	}
	next := []int{}
	for _, d := range current {
		if !drop[d] {
			next = append(next, d)
		}
	}
	return s.SetTaskDependencies(ctx, p, taskID, append(next, add...))
}

func joinIDs(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("#%d", id)
	}
	return strings.Join(parts, ", ")
}

// MoveTask puts a ticket under another story (in any epic of its project).
// Its dependencies, owner, and state are unchanged.
func (s *Service) MoveTask(ctx context.Context, p authz.Principal, taskID, storyID int) (*domain.Task, error) {
	t, err := s.loadForPlanning(ctx, p, taskID)
	if err != nil {
		return nil, err
	}
	story, err := s.Repo.GetStory(ctx, storyID)
	if err != nil || story.ProjectID != t.ProjectID {
		return nil, authz.BadRequest("story %d is not in task %d's project", storyID, taskID)
	}
	if story.ID == t.StoryID {
		return t, nil
	}
	pid, err := s.Registry.Get(t.ProjectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplyGetTask, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgMoveTask{TaskID: taskID, FromEpicID: t.EpicID, ToEpicID: story.EpicID, ToStoryID: story.ID, ReplyCh: replyCh})
	reply := <-replyCh
	return reply.Task, reply.Err
}

// MoveStory puts a story, with its tickets, under another epic of its project.
func (s *Service) MoveStory(ctx context.Context, p authz.Principal, storyID, epicID int) (*domain.Story, error) {
	story, err := s.RequireEditStory(ctx, p, storyID)
	if err != nil {
		return nil, err
	}
	epic, err := s.Repo.GetEpic(ctx, epicID)
	if err != nil || epic.ProjectID != story.ProjectID {
		return nil, authz.BadRequest("epic %d is not in story %d's project", epicID, storyID)
	}
	if epic.ID == story.EpicID {
		return story, nil
	}
	pid, err := s.Registry.Get(story.ProjectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgMoveStory{StoryID: storyID, FromEpicID: story.EpicID, ToEpicID: epicID, ReplyCh: replyCh})
	if reply := <-replyCh; reply.Err != nil {
		return nil, reply.Err
	}
	return s.Repo.GetStory(ctx, storyID)
}

// ReconcileDependents makes tickets re-read their dependencies after some
// of their dependencies were deleted, so none stays blocked on work that no
// longer exists. dependents must be found before the delete (it removes the
// dependency rows).
func (s *Service) ReconcileDependents(ctx context.Context, projectID int, dependents, deleted []int) {
	gone := map[int]bool{}
	for _, id := range deleted {
		gone[id] = true
	}
	pid, err := s.Registry.Get(projectID)
	if err != nil {
		return
	}
	for _, id := range dependents {
		if gone[id] {
			continue
		}
		replyCh := make(chan actor.ReplySimple, 1)
		s.Registry.System().Root.Send(pid, &actor.MsgReconcileDeps{TaskID: id, ReplyCh: replyCh})
		<-replyCh
	}
}

// ---- Plan check ----

// PlanIssue is a likely problem in a project's plan.
type PlanIssue struct {
	Kind string `json:"kind"`
	// Severity is "warn" for advice that doesn't make the plan NO-GO.
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message"`
	TaskIDs  []int  `json:"task_ids,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

// PlanReport summarises a project's dependency graph and what looks wrong.
type PlanReport struct {
	OK bool `json:"ok"`
	// Plan approval: open tickets planned since the last approval, when
	// that was, and whether the project requires it before dispatch.
	AwaitingApproval    []int       `json:"awaiting_approval"`
	ApprovedAt          *time.Time  `json:"plan_approved_at,omitempty"`
	ApprovedBy          *int        `json:"plan_approved_by,omitempty"`
	RequirePlanApproval bool        `json:"require_plan_approval"`
	Tasks               int         `json:"tasks"`
	OpenTasks           int         `json:"open_tasks"`
	Dependencies        int         `json:"dependencies"`
	ReadyNow            []int       `json:"ready_now"`
	LongestChain        int         `json:"longest_chain"` // tickets on the longest dependency path
	Issues              []PlanIssue `json:"issues"`
	RepairedTaskIDs     []int       `json:"repaired_task_ids,omitempty"`
	Summary             string      `json:"summary"`
}

// readyFanoutThreshold: more ready tickets than this, when they're also more
// than half the open work, usually means dependencies are missing.
const readyFanoutThreshold = 4

// CheckPlan reviews a project's tickets and dependencies: isolated tickets,
// too much that can start at once, empty stories, and tickets stuck blocked
// on dependencies that are all done (which it repairs).
func (s *Service) CheckPlan(ctx context.Context, p authz.Principal, projectID int) (*PlanReport, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	// Read tickets from the actors, not the database: an actor answers after
	// any unblock already on its way to it, so in-flight changes aren't
	// mistaken for problems.
	pid, err := s.Registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	listCh := make(chan actor.ReplyListTasks, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgListTasks{ProjectID: projectID, ReplyCh: listCh})
	listed := <-listCh
	if listed.Err != nil {
		return nil, listed.Err
	}
	tasks := listed.Tasks
	stories, err := s.Repo.ListStoriesByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	r := &PlanReport{Tasks: len(tasks), ReadyNow: []int{}, Issues: []PlanIssue{}, AwaitingApproval: []int{}}
	proj, err := s.Repo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	r.ApprovedAt, r.ApprovedBy, r.RequirePlanApproval = proj.PlanApprovedAt, proj.PlanApprovedBy, proj.RequirePlanApproval
	for i := range tasks {
		if tasks[i].State != domain.TaskStateCompleted && proj.AwaitsApproval(&tasks[i]) {
			r.AwaitingApproval = append(r.AwaitingApproval, tasks[i].ID)
		}
	}
	byID := map[int]domain.Task{}
	dependents := map[int]int{}
	for _, t := range tasks {
		byID[t.ID] = t
		r.Dependencies += len(t.DependencyIDs)
		for _, d := range t.DependencyIDs {
			dependents[d]++
		}
	}

	var isolated, stuck []int
	for _, t := range tasks {
		if t.State == domain.TaskStateCompleted {
			continue
		}
		r.OpenTasks++
		waiting := false
		for _, d := range t.DependencyIDs {
			if byID[d].State != domain.TaskStateCompleted {
				waiting = true
			}
		}
		if t.State == domain.TaskStateBacklog && !waiting {
			r.ReadyNow = append(r.ReadyNow, t.ID)
		}
		if t.State == domain.TaskStateBlocked && t.BlockReason == "" && !waiting {
			stuck = append(stuck, t.ID)
		}
		if len(t.DependencyIDs) == 0 && dependents[t.ID] == 0 {
			isolated = append(isolated, t.ID)
		}
	}

	if len(tasks) > 1 && len(isolated) > 0 {
		r.Issues = append(r.Issues, PlanIssue{
			Kind: "isolated", TaskIDs: isolated,
			Message: fmt.Sprintf("%d open ticket(s) neither depend on anything nor have anything depending on them: %s. Each can run at any time, in parallel with everything else.", len(isolated), joinIDs(isolated)),
			Fix:     "If one needs another's output, or something needs its output, wire it with amp_set_task_dependencies. Leave it only if it truly is independent work.",
		})
	}
	if len(r.ReadyNow) > readyFanoutThreshold && len(r.ReadyNow)*2 > r.OpenTasks {
		r.Issues = append(r.Issues, PlanIssue{
			Kind: "wide_start", TaskIDs: r.ReadyNow,
			Message: fmt.Sprintf("%d of %d open tickets can be dispatched right now. That much parallel work usually means dependencies are missing.", len(r.ReadyNow), r.OpenTasks),
			Fix:     "Check each ready ticket: does it need anything built first? Add those dependencies.",
		})
	}
	storyTasks := map[int]int{}
	for _, t := range tasks {
		storyTasks[t.StoryID]++
	}
	var empty []string
	for _, st := range stories {
		if storyTasks[st.ID] == 0 {
			empty = append(empty, fmt.Sprintf("%q (#%d)", st.Name, st.ID))
		}
	}
	if len(empty) > 0 {
		r.Issues = append(r.Issues, PlanIssue{
			Kind:    "empty_stories",
			Message: "Stories with no tickets: " + strings.Join(empty, ", "),
			Fix:     "Add their tickets, move tickets into them (amp_move_task), or delete them (amp_delete_story).",
		})
	}

	// A gate that couples different people's work: its dependencies belong to
	// two or more people, and so do the tickets waiting on it, so each person's
	// next work waits on everyone's. Often a review that should be split.
	ownerOf := func(id int) string {
		if t, ok := byID[id]; ok && t.OwnerUserID != nil {
			return fmt.Sprint(*t.OwnerUserID)
		}
		return ""
	}
	waiters := map[int][]int{}
	for _, t := range tasks {
		for _, d := range t.DependencyIDs {
			waiters[d] = append(waiters[d], t.ID)
		}
	}
	var coupled []int
	var coupledDetail []string
	for _, t := range tasks {
		if t.State == domain.TaskStateCompleted || len(t.DependencyIDs) < 2 || len(waiters[t.ID]) < 2 {
			continue
		}
		up, down := map[string]bool{}, map[string]bool{}
		for _, d := range t.DependencyIDs {
			if o := ownerOf(d); o != "" {
				up[o] = true
			}
		}
		for _, w := range waiters[t.ID] {
			if o := ownerOf(w); o != "" {
				down[o] = true
			}
		}
		if len(up) >= 2 && len(down) >= 2 {
			coupled = append(coupled, t.ID)
			coupledDetail = append(coupledDetail, fmt.Sprintf("#%d (waits on %d people's tickets; %d tickets of %d people wait on it)", t.ID, len(up), len(waiters[t.ID]), len(down)))
		}
	}
	if len(coupled) > 0 {
		r.Issues = append(r.Issues, PlanIssue{
			Kind: "coupled_gate", Severity: "warn", TaskIDs: coupled,
			Message: "These tickets join different people's work: " + strings.Join(coupledDetail, "; ") +
				". Everyone's next tickets wait until all of it is done, so one person's delay stalls the others.",
			Fix: "If the waiting tickets only need part of it, split the gate (e.g. one check per stack or per person) and re-point dependencies with amp_set_task_dependencies.",
		})
	}

	// Metadata drift: epics missing keys most of their sibling epics carry
	// (e.g. capitalizable, jira_epic), often under a near-miss name instead.
	// Reports group by these keys, so a missing or misspelled one silently
	// drops the epic's work from a finance or Jira rollup.
	if epics, err := s.Repo.ListEpics(ctx, projectID); err == nil && len(epics) >= 3 {
		freq := map[string]int{}
		for _, e := range epics {
			for k := range e.Metadata {
				freq[k]++
			}
		}
		expected := map[string]bool{}
		for k, n := range freq {
			if n >= 2 && n*10 >= len(epics)*6 { // on at least 60% of epics
				expected[k] = true
			}
		}
		var drift []string
		for _, e := range epics {
			var missing, odd []string
			for k := range expected {
				if _, ok := e.Metadata[k]; !ok {
					missing = append(missing, k)
				}
			}
			if len(missing) == 0 {
				continue
			}
			for k := range e.Metadata {
				if !expected[k] && freq[k] == 1 {
					odd = append(odd, k)
				}
			}
			sort.Strings(missing)
			sort.Strings(odd)
			line := fmt.Sprintf("epic %q (#%d) lacks %s", e.Name, e.ID, strings.Join(missing, ", "))
			if len(odd) > 0 {
				line += " but has " + strings.Join(odd, ", ") + " (only this epic uses that)"
			}
			drift = append(drift, line)
		}
		if len(drift) > 0 {
			sort.Strings(drift)
			r.Issues = append(r.Issues, PlanIssue{
				Kind: "metadata_drift", Severity: "warn",
				Message: "Epics don't carry the same metadata: " + strings.Join(drift, "; ") + ". Reports group by these keys, so this work drops out of those rollups.",
				Fix:     "Set the missing keys with amp_update_epic metadata (use the same key names the other epics use), and remove a misspelled key by setting it to null.",
			})
		}
	}

	// A plan with an empty project KB: the decisions behind it live only in
	// the planner's session, and the next session or engineer starts blind.
	if r.OpenTasks > 0 && s.KB != nil {
		if n, err := s.KB.CountDocs(ctx, kb.Project(projectID)); err == nil && n == 0 {
			r.Issues = append(r.Issues, PlanIssue{
				Kind:    "no_context",
				Message: "The project knowledge base is empty. The decisions behind this plan exist only in the session that made it.",
				Fix:     "Write architecture/project-overview.md (what this is, the stack, how it's laid out) and a doc per decision the user made under decisions/ (what, why, when) with amp_kb_write.",
			})
		}
	}

	// Blocked by dependencies that are all done: left stuck by older versions
	// of AMP. The ticket should be ready; repair it.
	if len(stuck) > 0 {
		s.ReconcileDependents(ctx, projectID, stuck, nil)
		r.RepairedTaskIDs = stuck
	}

	r.LongestChain = longestChain(tasks)
	r.OK = true
	for _, iss := range r.Issues {
		if iss.Severity != "warn" {
			r.OK = false
		}
	}
	r.Summary = fmt.Sprintf("%d tickets (%d open), %d dependencies, %d ready to dispatch now, longest chain %d.",
		r.Tasks, r.OpenTasks, r.Dependencies, len(r.ReadyNow), r.LongestChain)
	warns := 0
	for _, iss := range r.Issues {
		if iss.Severity == "warn" {
			warns++
		}
	}
	switch {
	case r.OK && warns == 0:
		r.Summary += " No problems found."
	case r.OK:
		r.Summary += fmt.Sprintf(" No blocking problems; %d warning(s) worth a look.", warns)
	}
	if len(stuck) > 0 {
		r.Summary += fmt.Sprintf(" Repaired %d ticket(s) stuck blocked although all their dependencies are done; they are ready now: %s.", len(stuck), joinIDs(stuck))
	}
	return r, nil
}

func longestChain(tasks []domain.Task) int {
	deps := map[int][]int{}
	for _, t := range tasks {
		deps[t.ID] = t.DependencyIDs
	}
	memo := map[int]int{}
	var depth func(id int, seen map[int]bool) int
	depth = func(id int, seen map[int]bool) int {
		if v, ok := memo[id]; ok {
			return v
		}
		if seen[id] { // cycle guard (shouldn't happen)
			return 0
		}
		seen[id] = true
		best := 0
		for _, d := range deps[id] {
			if v := depth(d, seen); v > best {
				best = v
			}
		}
		delete(seen, id)
		memo[id] = best + 1
		return best + 1
	}
	longest := 0
	for _, t := range tasks {
		if v := depth(t.ID, map[int]bool{}); v > longest {
			longest = v
		}
	}
	return longest
}

// ---- Batch creation ----

// TaskSpec is one ticket in a batch: Ref names it within the batch, and
// DependsOn lists the refs (or "#id" / id of existing tickets) it waits on.
type TaskSpec struct {
	Ref                string
	StoryID            int
	Name               string
	Description        string
	AcceptanceCriteria string
	Priority           string
	AssignedTo         string
	Owner              string
	DependsOn          []string
	Metadata           domain.Metadata
}

// CreatedTask maps a batch ref to the ticket created for it.
type CreatedTask struct {
	Ref          string `json:"ref"`
	ID           int    `json:"id"`
	State        string `json:"state"`
	Dependencies []int  `json:"dependency_ids"`
}

// CreateTasks creates a batch of tickets with their dependency graph in one
// go: dependencies may point at other tickets in the batch (by ref) or at
// existing tickets. Everything is checked — unknown refs, other projects,
// cycles — before anything is created, then tickets are created in
// dependency order.
func (s *Service) CreateTasks(ctx context.Context, p authz.Principal, projectID int, specs []TaskSpec) ([]CreatedTask, []string, error) {
	if len(specs) == 0 {
		return nil, nil, authz.BadRequest("tasks is empty")
	}
	if len(specs) > 100 {
		return nil, nil, authz.BadRequest("at most 100 tasks per batch")
	}
	byRef := map[string]int{}
	for i, sp := range specs {
		ref := strings.TrimSpace(sp.Ref)
		if ref == "" {
			return nil, nil, authz.BadRequest("task %d in the batch needs a ref (a short name other tasks can depend on)", i+1)
		}
		if _, dup := byRef[ref]; dup {
			return nil, nil, authz.BadRequest("ref %q is used twice", ref)
		}
		byRef[ref] = i
		specs[i].Ref = ref
	}

	// Resolve dependencies: batch refs, or existing ticket ids.
	refDeps := make([][]int, len(specs)) // indexes into specs
	idDeps := make([][]int, len(specs))  // existing ticket ids
	var existing []int
	dependedOn := map[int]bool{}
	for i, sp := range specs {
		for _, d := range sp.DependsOn {
			d = strings.TrimSpace(d)
			if j, ok := byRef[d]; ok {
				if j == i {
					return nil, nil, authz.BadRequest("%q can't depend on itself", sp.Ref)
				}
				refDeps[i] = append(refDeps[i], j)
				dependedOn[j] = true
				continue
			}
			var id int
			if _, err := fmt.Sscanf(strings.TrimPrefix(d, "#"), "%d", &id); err != nil || id <= 0 {
				return nil, nil, authz.BadRequest("%q depends on %q, which is neither a ref in this batch nor a ticket id", sp.Ref, d)
			}
			idDeps[i] = append(idDeps[i], id)
			existing = append(existing, id)
		}
	}
	if _, err := s.ValidateDependencies(ctx, projectID, 0, existing); err != nil {
		return nil, nil, err
	}

	// Dependency order (Kahn); anything left over is in a cycle.
	waiting := make([]int, len(specs))
	dependents := make([][]int, len(specs))
	for i := range specs {
		waiting[i] = len(refDeps[i])
		for _, j := range refDeps[i] {
			dependents[j] = append(dependents[j], i)
		}
	}
	var order, queue []int
	for i := range specs {
		if waiting[i] == 0 {
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		order = append(order, i)
		for _, k := range dependents[i] {
			if waiting[k]--; waiting[k] == 0 {
				queue = append(queue, k)
			}
		}
	}
	if len(order) != len(specs) {
		var cyc []string
		for i := range specs {
			if waiting[i] > 0 {
				cyc = append(cyc, specs[i].Ref)
			}
		}
		return nil, nil, authz.BadRequest("these tasks depend on each other in a cycle: %s", strings.Join(cyc, ", "))
	}

	ids := make([]int, len(specs))
	out := make([]CreatedTask, len(specs))
	for _, i := range order {
		sp := specs[i]
		deps := append([]int(nil), idDeps[i]...)
		for _, j := range refDeps[i] {
			deps = append(deps, ids[j])
		}
		t, _, err := s.CreateTask(ctx, p, domain.CreateTaskRequest{
			ProjectID: projectID, StoryID: sp.StoryID, Name: sp.Name, Description: sp.Description,
			AcceptanceCriteria: sp.AcceptanceCriteria, Priority: sp.Priority, AssignedTo: sp.AssignedTo,
			DependencyIDs: deps, Metadata: sp.Metadata,
		}, sp.Owner)
		if err != nil {
			var made []string
			for _, c := range out {
				if c.ID != 0 {
					made = append(made, fmt.Sprintf("%s=#%d", c.Ref, c.ID))
				}
			}
			return out, nil, fmt.Errorf("creating %q failed (%v); already created: %s", sp.Ref, err, strings.Join(made, ", "))
		}
		ids[i] = t.ID
		out[i] = CreatedTask{Ref: sp.Ref, ID: t.ID, State: string(t.State), Dependencies: t.DependencyIDs}
	}

	var warnings []string
	if len(specs) > 1 {
		var loose []string
		for i, sp := range specs {
			if len(refDeps[i]) == 0 && len(idDeps[i]) == 0 && !dependedOn[i] {
				loose = append(loose, sp.Ref)
			}
		}
		if len(loose) > 0 {
			warnings = append(warnings, fmt.Sprintf("These tasks are not connected to the rest of the batch (no dependencies either way), so they "+
				"can all be dispatched at once: %s. If that's not intended, wire them with amp_set_task_dependencies.", strings.Join(loose, ", ")))
		}
	}
	return out, warnings, nil
}

// ---- Deleting ----
//
// Deleting tickets removes their dependency rows, but tickets elsewhere that
// waited on them hold the dependency in memory and may be blocked on it. So
// every delete finds those dependents first and has them re-read their
// dependencies afterwards.

// DeleteTask deletes a ticket (permission already checked).
func (s *Service) DeleteTask(ctx context.Context, p authz.Principal, t *domain.Task) ([]int, error) {
	dependents, err := s.deleteTickets(ctx, p, t.ProjectID, []int{t.ID}, func() error {
		return s.Repo.DeleteTask(ctx, t.ID)
	}, &actor.MsgDeleteTask{TaskID: t.ID})
	if err == nil {
		s.audit(ctx, p, domain.AuditEntry{Action: "task.delete", ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(t.ID),
			Summary: fmt.Sprintf("deleted ticket #%d %q", t.ID, t.Name), Details: map[string]any{"story_id": t.StoryID, "state": t.State}})
	}
	return dependents, err
}

// DeleteStory deletes a story and its tickets (permission already checked).
func (s *Service) DeleteStory(ctx context.Context, p authz.Principal, st *domain.Story) error {
	ids, err := s.Repo.TaskIDsInStories(ctx, []int{st.ID})
	if err != nil {
		return err
	}
	_, err = s.deleteTickets(ctx, p, st.ProjectID, ids, func() error {
		if err := s.Repo.TrashGroup(ctx, repository.GroupStory, st.ID, ids, deleterOf(p)); err != nil {
			return err
		}
		return s.Repo.DeleteStory(ctx, st.ID)
	}, &actor.MsgDeleteStory{StoryID: st.ID})
	if err == nil {
		s.audit(ctx, p, domain.AuditEntry{Action: "story.delete", ProjectID: intRef(st.ProjectID), TargetType: "story", TargetID: idStr(st.ID),
			Summary: fmt.Sprintf("deleted story %q and its %d tickets", st.Name, len(ids)), Details: map[string]any{"task_ids": ids}})
	}
	return err
}

// DeleteEpic deletes an epic, its stories, and their tickets (permission
// already checked).
func (s *Service) DeleteEpic(ctx context.Context, p authz.Principal, e *domain.Epic) error {
	ids, err := s.Repo.TaskIDsInEpic(ctx, e.ID)
	if err != nil {
		return err
	}
	_, err = s.deleteTickets(ctx, p, e.ProjectID, ids, func() error {
		if err := s.Repo.TrashGroup(ctx, repository.GroupEpic, e.ID, ids, deleterOf(p)); err != nil {
			return err
		}
		return s.Repo.DeleteEpic(ctx, e.ID)
	}, &actor.MsgDeleteEpic{EpicID: e.ID})
	if err == nil {
		s.audit(ctx, p, domain.AuditEntry{Action: "epic.delete", ProjectID: intRef(e.ProjectID), TargetType: "epic", TargetID: idStr(e.ID),
			Summary: fmt.Sprintf("deleted epic %q with its stories and %d tickets", e.Name, len(ids)), Details: map[string]any{"task_ids": ids}})
	}
	return err
}

// deleterOf is who the trash records as deleting something.
func deleterOf(p authz.Principal) *int {
	if !p.HasUser() {
		return nil
	}
	uid := p.UserID
	return &uid
}

// deleteTickets runs a delete, evicts the deleted work from the project's
// actors (msg is a delete message without its reply channel), and updates
// the tickets that depended on what was deleted. It returns those tickets.
func (s *Service) deleteTickets(ctx context.Context, p authz.Principal, projectID int, ids []int, del func() error, msg interface{}) ([]int, error) {
	dependents, err := s.Repo.Dependents(ctx, ids)
	if err != nil {
		return nil, err
	}
	// Keep a copy in the trash so the deletion can be undone.
	if err := s.Repo.TrashTasks(ctx, ids, deleterOf(p)); err != nil {
		return nil, err
	}
	if err := del(); err != nil {
		return nil, err
	}
	pid, err := s.Registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	switch m := msg.(type) {
	case *actor.MsgDeleteTask:
		m.ReplyCh = replyCh
	case *actor.MsgDeleteStory:
		m.ReplyCh = replyCh
	case *actor.MsgDeleteEpic:
		m.ReplyCh = replyCh
	}
	s.Registry.System().Root.Send(pid, msg)
	<-replyCh
	s.ReconcileDependents(ctx, projectID, dependents, ids)
	var kept []int
	gone := map[int]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	for _, id := range dependents {
		if !gone[id] {
			kept = append(kept, id)
		}
	}
	return kept, nil
}

// ---- Answering an agent's question ----

// AnswerQuestion answers a question: a ticket an agent blocked with a reason
// because it needs a person. The answer, if any, is posted as a comment first; then
// the ticket goes back to ready, or to waiting if it still depends on
// unfinished work.
func (s *Service) AnswerQuestion(ctx context.Context, p authz.Principal, taskID int, answer string) (*domain.Task, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return nil, err
	}
	if t.State != domain.TaskStateBlocked || t.BlockReason == "" || strings.HasPrefix(t.BlockReason, "scheduled:") {
		return nil, authz.BadRequest("ticket %d has no open question: only a ticket blocked with a reason (amp_block_task) can be answered", taskID)
	}
	if answer = strings.TrimSpace(answer); answer != "" {
		added := make(chan actor.ReplyAddComment, 1)
		if err := s.Registry.Send(t.ProjectID, &actor.MsgAddComment{Req: domain.AddCommentRequest{
			TaskID: taskID, Author: p.Email, Body: answer, AuthorUserID: p.UserRef(),
		}, ReplyCh: added}); err != nil {
			return nil, err
		}
		if r := <-added; r.Err != nil {
			return nil, r.Err
		}
	}
	set := make(chan actor.ReplySimple, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgSetTaskState{TaskID: taskID, State: domain.TaskStateBacklog, Reason: "", By: p.UserRef(), ReplyCh: set}); err != nil {
		return nil, err
	}
	if r := <-set; r.Err != nil {
		return nil, r.Err
	}
	// Back to waiting if a dependency is unfinished.
	rec := make(chan actor.ReplySimple, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgReconcileDeps{TaskID: taskID, ReplyCh: rec}); err != nil {
		return nil, err
	}
	if r := <-rec; r.Err != nil {
		return nil, r.Err
	}
	got := make(chan actor.ReplyGetTask, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgGetTask{TaskID: taskID, ReplyCh: got}); err != nil {
		return nil, err
	}
	r := <-got
	return r.Task, r.Err
}

// ---- Plan approval ----

// ApprovePlan records that a person approved the project's plan as it is
// now: every ticket planned so far may be dispatched. Team leads and
// project owners can approve; agents can't (a person approves agents' plans).
func (s *Service) ApprovePlan(ctx context.Context, p authz.Principal, projectID int) (*domain.Project, error) {
	if p.Agent {
		return nil, authz.Forbidden("agent accounts can't approve plans; a person approves (in AMP, or by telling the orchestrator)")
	}
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	// Approval is the person saying "go" to the plan their agent drafted: any
	// person on the project may give it (agent accounts can't, above).
	proj, err := s.Repo.ApprovePlan(ctx, projectID, p.UserRef())
	if err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "plan.approve", ProjectID: intRef(projectID), TargetType: "project", TargetID: idStr(projectID),
		Summary: fmt.Sprintf("approved the plan of %s", proj.Name)})
	return proj, nil
}
