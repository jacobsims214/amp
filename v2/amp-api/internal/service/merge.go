package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Merge signal (AMP #2475, v2/kb/working/human-work.md) ----
//
// A ticket that completes with a run carrying a PR or a pushed branch, in a
// project that requires merges, is done with merge_state "needed": the
// tickets that depend on it wait until the merge is recorded. The merge is
// recorded once, by whichever comes first: GitHub's webhook ("github"), CI
// calling amp_report_merged ("ci"), or Mark merged in the web UI ("ui").
// Later reports are no-ops that return the first one. An abandoned PR keeps
// the dependents waiting until the ticket is reopened or its merge is marked
// not needed.

// MergeRecord is a ticket's merge state after a merge report.
type MergeRecord struct {
	TaskID      int        `json:"task_id"`
	MergeState  string     `json:"merge_state"`
	MergeSource string     `json:"merge_source,omitempty"`
	MergedAt    *time.Time `json:"merged_at,omitempty"`
	// AlreadyRecorded: an earlier report recorded the merge; this one
	// changed nothing.
	AlreadyRecorded bool         `json:"already_recorded"`
	Message         string       `json:"message"`
	Run             *domain.Run  `json:"run,omitempty"`
	Task            *domain.Task `json:"task,omitempty"`
	// LinkedStep is the human merge step that tracks this merge (its
	// merge_for is this ticket): its id, state, and whether this report
	// completed it or found it already done (AMP #3009).
	LinkedStep *MergeStepRecord `json:"linked_step,omitempty"`
}

// MergeStepRecord is what a merge report did to the human step linked to
// the merge it recorded.
//
// The link is one-way by design: the step names the ticket whose PR it
// gates (merge_for), and a merge recorded from outside (GitHub's webhook,
// CI's amp_report_merged) completes the step as the same act, so the person
// owes nothing after merging on GitHub. It is not a sign-off invented for
// them: the step's acceptance was the merge, the report says the merge
// happened, and the ticket's own history names who recorded it.
type MergeStepRecord struct {
	TaskID int    `json:"task_id"`
	State  string `json:"state"`
	// Completed: this report completed the step. AlreadyDone: the step was
	// already done, and its merge record is the one on the PR's ticket.
	Completed   bool   `json:"completed"`
	AlreadyDone bool   `json:"already_done"`
	Message     string `json:"message"`
}

// mergeSourceOf is who a merge report comes from when the caller doesn't
// say: a person in the web UI, otherwise CI (an API token or an agent).
func mergeSourceOf(p authz.Principal) string {
	if p.InUI() {
		return domain.MergeSourceUI
	}
	return domain.MergeSourceCI
}

// ---- Planned merge steps (AMP #3009) ----
//
// A plan can put the merge itself in the graph as a person's step: a human
// ticket whose own metadata says which ticket's PR it tracks (merge_for).
// Before this, that step was a second card for the same real-world merge —
// My work's Merge item for the agent ticket, and the human ticket's own Do
// card (discoveries/merge-actions-user-journey.md) — with no code joining
// them. The link makes them one journey in both directions:
//
//   - the step's prerequisites are still its dependencies, so the merge
//     stays sequenced: the card's action is offered only once the step is
//     ready, and the step is the thing that releases whatever waits on it;
//   - a merge recorded from anywhere (Mark merged on the PR's card, GitHub's
//     webhook, CI's amp_report_merged) completes the step, because the
//     step's acceptance criteria were that merge. Nothing is left owing a
//     click for a merge that already happened.
//
// The auto-completion deliberately does not claim the person acted: the
// step's history names who recorded the merge, no run is recorded for them,
// and a step its owner has already started (in progress) is left to them.
// And it never overrides the plan's sequencing: while the step still waits
// on an unfinished prerequisite it stays open, and the merge report says so
// instead of quietly starting the work after it.
//
// A ticket without the key behaves exactly as before, so legacy merge
// tickets are unchanged; CheckPlan warns about a merge-like step that has no
// link, because that is the duplicate this key exists to remove.

// MergeFor is the ticket whose PR merge t tracks, from t's own metadata
// (never inherited: a story's link would silently point every ticket under
// it at one PR), or 0.
func MergeFor(t *domain.Task) int {
	id, ok := t.Metadata.MergeFor()
	if !ok {
		return 0
	}
	return id
}

// checkMergeFor validates a merge_for link before it is stored: a person's
// step only, naming agent work in the same project that no other step tracks
// (one step per merge is the duplicate this key removes). taskID is 0 for a
// ticket being created; a ticket with no merge_for passes untouched.
func (s *Service) checkMergeFor(ctx context.Context, projectID, taskID int, performer string, meta domain.Metadata) error {
	v, set := meta[domain.MetaMergeFor]
	if !set || v == nil {
		return nil // no link: nothing to check, and removing one is always allowed
	}
	forID, err := domain.ParseMergeFor(v)
	if err != nil {
		return authz.BadRequest("%v", err)
	}
	if forID == 0 {
		return authz.BadRequest("merge_for must name a ticket id; leave the key out to have no link")
	}
	who := "the new ticket"
	if taskID != 0 {
		who = fmt.Sprintf("ticket %d", taskID)
	}
	if performer != domain.PerformerHuman {
		return authz.BadRequest("%s: merge_for belongs on a person's merge step (performer \"human\"); "+
			"a merge of agent work is recorded on the ticket that opened the PR", who)
	}
	if taskID != 0 && forID == taskID {
		return authz.BadRequest("%s can't track its own merge", who)
	}
	target, err := s.Repo.GetTask(ctx, forID)
	if err != nil {
		return authz.BadRequest("%s: merge_for names ticket %d, which doesn't exist", who, forID)
	}
	if target.ProjectID != projectID {
		return authz.BadRequest("%s: merge_for must name a ticket in the same project (ticket %d is in project %d)",
			who, forID, target.ProjectID)
	}
	if target.IsHuman() {
		return authz.BadRequest("%s: merge_for must name the ticket whose work is merged (agent work); ticket %d is a person's ticket",
			who, forID)
	}
	other, err := s.Repo.TaskTrackingMerge(ctx, forID)
	if err != nil {
		return err
	}
	if other == taskID {
		return nil // already linked: nothing new to check, so other edits don't re-refuse it
	}
	if other != 0 {
		// A done step isn't tracking anything any more: its merge is recorded,
		// which the check below reports more usefully than a duplicate would.
		if done, err := s.Repo.GetTask(ctx, other); err == nil && done.State != domain.TaskStateCompleted {
			return authz.BadRequest("%s: ticket %d's merge is already tracked by ticket %d; one step per merge",
				who, forID, other)
		}
	}
	if target.MergeState == domain.MergeMerged {
		return authz.BadRequest("%s: ticket %d's merge is already recorded (by %s), so a step tracking it would have nothing to do; "+
			"drop the step, or point it at the ticket whose work still has to merge", who, forID, nameOrDash(target.MergeSource))
	}
	return nil
}

// checkTaskMergeFor validates a merge_for the edit req sets on t: against the
// ticket as the edit leaves it, since the same request may switch it between
// a person's step and agent work. A removal (null) passes: relinking is how a
// wrong link is fixed.
func (s *Service) checkTaskMergeFor(ctx context.Context, t *domain.Task, req domain.UpdateTaskRequest) error {
	v, set := req.Metadata[domain.MetaMergeFor]
	if !set || v == nil {
		return nil
	}
	performer := t.Performer
	if req.Performer != "" || req.HumanReason != "" {
		perf := req.Performer
		if perf == "" {
			perf = t.Performer
		}
		p, _, err := domain.NormalizePerformer(perf, req.HumanReason)
		if err != nil {
			return authz.BadRequest("%v", err)
		}
		performer = p
	}
	return s.checkMergeFor(ctx, t.ProjectID, t.ID, performer, domain.Metadata{domain.MetaMergeFor: v})
}

// MergeStepFor is the human step tracking ticketID's merge, or nil when the
// plan has none: the link that makes My work's Merge card and the plan's
// step one action instead of two.
func (s *Service) MergeStepFor(ctx context.Context, ticketID int) (*domain.Task, error) {
	stepID, err := s.Repo.TaskTrackingMerge(ctx, ticketID)
	if err != nil || stepID == 0 {
		return nil, err
	}
	return s.Repo.GetTask(ctx, stepID)
}

// recordLinkedMergeStep completes the human step tracking ticketID's merge as
// the same act as recording that merge, and says what it did (nil: no step is
// linked). A step already done reports itself done; one its owner started is
// left to them; one still waiting on an unfinished prerequisite stays open,
// and the record says what it waits on.
func (s *Service) recordLinkedMergeStep(ctx context.Context, p authz.Principal, ticketID int, source string) *MergeStepRecord {
	step, err := s.MergeStepFor(ctx, ticketID)
	if err != nil {
		slog.Warn("linked merge step", "task_id", ticketID, "err", err)
		return nil
	}
	if step == nil {
		return nil
	}
	switch step.State {
	case domain.TaskStateCompleted:
		return &MergeStepRecord{TaskID: step.ID, State: string(step.State), AlreadyDone: true,
			Message: fmt.Sprintf("its merge step #%d was already done", step.ID)}
	case domain.TaskStateInProgress:
		return &MergeStepRecord{TaskID: step.ID, State: string(step.State),
			Message: fmt.Sprintf("its merge step #%d is in progress: its owner finishes it", step.ID)}
	}
	if step.State == domain.TaskStateBlocked && step.BlockReason != "" && !strings.HasPrefix(step.BlockReason, "scheduled:") {
		// The step is blocked on a question for its owner: a merge report
		// must not answer it or close the step under it.
		return &MergeStepRecord{TaskID: step.ID, State: string(step.State),
			Message: fmt.Sprintf("its merge step #%d is waiting on a question, so it stays open", step.ID)}
	}
	if open, err := s.openPrerequisites(ctx, step); err != nil {
		slog.Warn("linked merge step prerequisites", "task_id", step.ID, "err", err)
	} else if len(open) > 0 {
		return &MergeStepRecord{TaskID: step.ID, State: string(step.State),
			Message: fmt.Sprintf("its merge step #%d still waits on %s, so it stays open", step.ID, joinIDs(open))}
	}
	pid, err := s.Registry.Get(step.ProjectID)
	if err != nil {
		return &MergeStepRecord{TaskID: step.ID, State: string(step.State), Message: "its merge step couldn't be completed: " + err.Error()}
	}
	replyCh := make(chan actor.ReplyComplete, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgCompleteTask{TaskID: step.ID, ByName: source,
		MergeStepFor: ticketID, Source: source, ReplyCh: replyCh})
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		slog.Warn("complete linked merge step", "task_id", step.ID, "err", reply.Err)
		return &MergeStepRecord{TaskID: step.ID, State: string(step.State),
			Message: "its merge step couldn't be completed: " + reply.Err.Error()}
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "task.merge.step_completed", ProjectID: intRef(step.ProjectID),
		TargetType: "task", TargetID: idStr(step.ID), TargetName: step.Name,
		Summary: fmt.Sprintf("%s recorded #%d's merge, which completes the merge step tracking it (#%d)", source, ticketID, step.ID),
		Details: map[string]any{"merge_for": ticketID, "source": source}})
	return &MergeStepRecord{TaskID: step.ID, State: string(domain.TaskStateCompleted), Completed: true,
		Message: fmt.Sprintf("its merge step #%d is complete", step.ID)}
}

// openPrerequisites is the merge step's dependencies that still hold it: not
// done, or done but waiting on their own merge (the same rule work starts by).
func (s *Service) openPrerequisites(ctx context.Context, step *domain.Task) ([]int, error) {
	if len(step.DependencyIDs) == 0 {
		return nil, nil
	}
	done, _, err := s.Repo.DepStates(ctx, step.DependencyIDs)
	if err != nil {
		return nil, err
	}
	var open []int
	for _, d := range step.DependencyIDs {
		if !done[d] {
			open = append(open, d)
		}
	}
	return open, nil
}

// retriggerMergeSteps finishes the merge steps that depIDs were holding.
//
// A merge recorded while a step still waits on a prerequisite leaves the step
// open on purpose: the plan says the merge comes after that prerequisite, and
// a merge report doesn't get to overturn the plan (recordLinkedMergeStep).
// But the merge is still a fact about the code, so the step must not go on
// asking its owner to acknowledge it once the prerequisite is satisfied. This
// is the other half: it runs wherever prerequisites become satisfied, finds
// the linked steps that are still open with their tracked merge already
// recorded, and finishes them the ordinary way.
//
// Called after depIDs change state (a completion, a recorded merge, a
// reopened ticket). It is bounded and best-effort: a failure here leaves the
// step open, which is where it already was, and the next report or completion
// retries it.
func (s *Service) retriggerMergeSteps(ctx context.Context, p authz.Principal, depIDs []int) {
	steps, err := s.Repo.OpenMergeStepsWaitingOn(ctx, depIDs)
	if err != nil {
		slog.Warn("merge steps waiting on dependencies", "deps", depIDs, "err", err)
		return
	}
	for _, w := range steps {
		// The name reported in the step's history is whoever recorded the
		// merge in the first place, not this reconciliation: the fact being
		// acted on is their report, and saying "github" (or "ci", or "ui")
		// keeps the step's record of why it is done truthful.
		source := ""
		if t, err := s.Repo.GetTask(ctx, w.Tracks); err == nil {
			source = t.MergeSource
		}
		if rec := s.recordLinkedMergeStep(ctx, p, w.Tracks, source); rec != nil && rec.Completed {
			slog.Info("merge step reconciled after its prerequisite finished", "step", w.StepID, "tracks", w.Tracks, "source", source)
		}
	}
}

// retriggerMergeStepsAfter is retriggerMergeSteps for the one ticket whose
// state just changed (it completed, or its merge was recorded): the merge
// steps waiting on it may now be free.
func (s *Service) retriggerMergeStepsAfter(ctx context.Context, p authz.Principal, taskID int) {
	s.retriggerMergeSteps(ctx, p, []int{taskID})
}

// mergedNow reports whether ticketID's merge is recorded: what the reverse
// bridge asks before recording one from a person's step. A read that fails
// answers false, so a completion never records a merge it couldn't confirm was
// still owed.
func (s *Service) mergedNow(ctx context.Context, ticketID int) bool {
	t, err := s.Repo.GetTask(ctx, ticketID)
	return err == nil && t.MergeState == domain.MergeMerged
}

// mergeWhere names a run's PR (or pushed branch) for waiting details.
func mergeWhere(run domain.Run) string {
	if run.PRURL != "" {
		return "PR " + run.PRURL
	}
	if run.Branch != "" {
		return "branch " + run.Branch
	}
	return ""
}

// mergeGateFor reports whether run, finishing ticket t (completed at
// completedAt, nil: now), leaves t's work waiting on a merge that holds its
// dependents: the run opens a PR or pushed a branch, t is agent work not
// already merged, and its project requires merges and did when t completed.
func (s *Service) mergeGateFor(ctx context.Context, t *domain.Task, run *domain.Run, completedAt *time.Time) bool {
	if run == nil || !run.OpensMerge() || t.IsHuman() || run.AgentID == domain.HumanAgentID || t.MergeState == domain.MergeMerged {
		return false
	}
	proj, err := s.Repo.GetProject(ctx, t.ProjectID)
	if err != nil || !proj.RequireMerge {
		return false
	}
	if completedAt != nil && proj.RequireMergeSince != nil && completedAt.Before(*proj.RequireMergeSince) {
		return false // completed before merges were required: never gated
	}
	if run.LandsOn(s.integrationBranch(ctx, t)) {
		// The work is merged into (or its PR targets) the ticket's
		// integration branch, where it belongs: nothing after it waits. It
		// stays unmerged (none) until that branch's merge is reported.
		return false
	}
	return true
}

// integrationBranch is ticket t's effective integration_branch metadata
// (project → epic → story → ticket), or "" (AMP #2596).
func (s *Service) integrationBranch(ctx context.Context, t *domain.Task) string {
	inherited, err := s.Repo.InheritedMetadata(ctx, "task", t.ID)
	if err != nil {
		inherited = domain.Metadata{}
	}
	return inherited.Merge(t.Metadata).IntegrationBranch()
}

// ReconcileMergeMetadata follows a metadata change on an item in projectID
// that sets or removes integration_branch or waits_for_merge (AMP #2596):
// done tickets waiting on a merge whose work is already on their integration
// branch stop holding their dependents (merge_state back to none, still
// unmerged), and the dependents of the tickets that still wait on a merge
// re-read their dependencies, so a waits_for_merge change takes effect now.
// It never touches planned_at: metadata isn't part of the plan's approval.
func (s *Service) ReconcileMergeMetadata(ctx context.Context, p authz.Principal, projectID int, patch domain.MetadataPatch) {
	if !patch.TouchesMergeGate() {
		return
	}
	ids, err := s.Repo.IntegrationMerged(ctx, projectID)
	if err != nil {
		slog.Warn("integration merges", "project_id", projectID, "err", err)
	}
	no := false
	for _, id := range ids {
		t, err := s.Repo.GetTask(ctx, id)
		if err != nil {
			continue
		}
		if _, err := s.setMergeState(ctx, p, t, actor.MsgSetMergeState{State: domain.MergeNone, Required: &no, From: []string{domain.MergeNeeded},
			Detail: "Its work is on its integration branch (" + s.integrationBranch(ctx, t) + "): the tickets that depend on it no longer wait; " +
				"it stays unmerged until that branch is merged"}); err != nil {
			slog.Warn("release integration merge", "task_id", id, "err", err)
		}
	}
	s.reconcileMergeGated(ctx, projectID)
}

// CompleteTaskWithRun completes agent ticket taskID as CompleteTask does,
// knowing the run that finished it: when that run leaves work waiting on a
// merge (mergeGateFor), the ticket completes with merge_state "needed" and
// its dependents keep waiting. The caller records the run afterwards.
// complete carries the caller's claim to be the ticket's worker (the
// dispatch_id it was given, if any) and, for a closeout, why.
func (s *Service) CompleteTaskWithRun(ctx context.Context, p authz.Principal, taskID int, run *domain.Run, complete CompleteClaim) (*domain.Task, Closeout, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return nil, Closeout{}, err
	}
	if t.IsHuman() {
		return nil, Closeout{}, authz.BadRequest("ticket %d is a person's ticket; they mark it done in AMP: %s", taskID, s.TaskURL(t.ProjectID, taskID))
	}
	if err := s.checkAgentCompletion(ctx, p, t, complete.DispatchID); err != nil {
		s.auditTaskDenied(ctx, p, "task.complete", t, err, nil)
		return nil, Closeout{}, err
	}
	gate := t.State != domain.TaskStateCompleted && s.mergeGateFor(ctx, t, run, nil)
	where := ""
	if gate {
		where = mergeWhere(*run)
	}
	// An explicit closeout_reason makes it a closeout even on a dispatched
	// ticket nobody started; a bare completion there stays an old plugin's
	// own (AMP #2908).
	closeout := t.State != domain.TaskStateCompleted && !p.InUI() && t.CompletionIsCloseout(complete.DispatchID, complete.Reason)
	reason := ""
	if closeout {
		reason = strings.TrimSpace(complete.Reason)
		if reason == "" && run != nil {
			reason = strings.TrimSpace(run.Note)
		}
	}
	reply, err := s.sendCompleteMerge(ctx, p, t, !p.InUI(), gate, where, complete.DispatchID, reason)
	if err != nil {
		return nil, Closeout{}, err
	}
	// This completion may have satisfied the last prerequisite of a linked
	// merge step (AMP #3009): a merge recorded earlier left it open on
	// purpose, and it must not keep asking its owner to acknowledge a merge
	// that already happened. Only when the ticket holds nothing now: gated,
	// it completes waiting on its own merge, so its dependents keep waiting
	// and nothing was freed.
	if !gate {
		s.retriggerMergeStepsAfter(ctx, p, taskID)
	}
	task, err := s.Repo.GetTask(ctx, taskID)
	return task, Closeout{Closeout: reply.Closeout, Reason: reason}, err
}

// CompleteClaim is how a caller presents itself to a completion: the
// dispatch_id it was dispatched with (the worker's own completion), and for
// a closeout, why it is closing a ticket that isn't its own work. Reason is
// only the caller's explicit closeout_reason, never a run note: a non-empty
// one makes the completion a closeout without the current dispatch_id
// (AMP #2908).
type CompleteClaim struct {
	DispatchID string
	Reason     string
}

// setMergeState sends MsgSetMergeState to t's actor.
func (s *Service) setMergeState(ctx context.Context, p authz.Principal, t *domain.Task, msg actor.MsgSetMergeState) (actor.ReplyMergeState, error) {
	replyCh := make(chan actor.ReplyMergeState, 1)
	msg.TaskID, msg.By, msg.ByName, msg.ReplyCh = t.ID, p.UserRef(), p.Label(), replyCh
	if err := s.Registry.Send(t.ProjectID, &msg); err != nil {
		return actor.ReplyMergeState{}, err
	}
	reply := actor.Await(ctx, replyCh)
	if reply.Err != nil {
		return reply, &authz.Error{Status: 409, Msg: reply.Err.Error()}
	}
	return reply, nil
}

// afterRun keeps t's merge state in step with a run just recorded on it:
// a run that reports the work merged records the merge (if nothing had); a
// done ticket's gate then follows the latest run that put work up — a PR or
// pushed branch reported elsewhere makes it wait on that merge, and one that
// landed the work on the ticket's integration branch releases the wait
// (still unmerged: only the integration branch's own merge to main is
// "merged"). Merged-first-wins: once a merge is recorded, no later run
// changes it.
//
// The release is the same decision ReconcileMergeMetadata makes on a
// metadata edit (Repo.IntegrationMerged), and both go through the same
// domain rule (Run.LandsOn), so a #2966-shaped correction works whether it
// arrives as a run or as metadata (#3002). It is also what keeps the two
// consistent across several runs: only the LATEST run that put work up
// decides, so a later PR to main reported after an integration landing
// gates again, and a correction after that releases again.
func (s *Service) afterRun(ctx context.Context, p authz.Principal, t *domain.Task, run domain.Run) {
	switch {
	case run.Merged != nil && *run.Merged:
		if t.MergeState == domain.MergeMerged {
			return
		}
		at := run.CreatedAt
		if _, err := s.setMergeState(ctx, p, t, actor.MsgSetMergeState{State: domain.MergeMerged, Source: mergeSourceOf(p), At: &at,
			Detail: fmt.Sprintf("Merge recorded by %s (%s)", mergeSourceOf(p), p.Label())}); err != nil {
			slog.Warn("record merge state", "task_id", t.ID, "err", err)
		}
	case t.State == domain.TaskStateCompleted && t.MergeState == domain.MergeMerged:
		// First report wins: a later run never undoes a recorded merge.
	case t.State == domain.TaskStateCompleted && t.MergeState == domain.MergeNone && !t.MergeRequired &&
		s.mergeGateFor(ctx, t, &run, t.CompletedAt):
		yes := true
		if _, err := s.setMergeState(ctx, p, t, actor.MsgSetMergeState{State: domain.MergeNeeded, Required: &yes, Where: mergeWhere(run),
			From:   []string{domain.MergeNone},
			Detail: "Its work was reported on " + nameOrDash(mergeWhere(run)) + " after it was done: the tickets that depend on it wait for the merge"}); err != nil {
			slog.Warn("record merge needed", "task_id", t.ID, "err", err)
		}
	case t.State == domain.TaskStateCompleted && t.MergeState == domain.MergeNeeded:
		// A later run may correct where the work went: if the ticket's
		// latest run that put work up landed on its integration branch,
		// the wait is for a merge that will never happen.
		s.releaseIntegrationLanding(ctx, p, t)
	}
}

// releaseIntegrationLanding clears t's merge gate when the latest of t's
// runs that put work up landed on t's integration branch: the same rule the
// project-wide sweep applies (Repo.IntegrationMerged), on the same run
// (Run.LandsOn — base_branch is it, or, with no PR, branch is it). No
// integration branch, or work elsewhere, changes nothing. It never records
// the ticket merged: the work is on the integration branch, not on the main
// line, so it stays unmerged until that branch is merged (AMP #3002).
// Only "needed" is released: an abandoned gate (a PR closed without
// merging) is a person's decision to reopen or wave through, exactly as
// IntegrationMerged leaves it.
func (s *Service) releaseIntegrationLanding(ctx context.Context, p authz.Principal, t *domain.Task) {
	ib := s.integrationBranch(ctx, t)
	if ib == "" {
		return
	}
	runs, err := s.Repo.TaskRuns(ctx, t.ID)
	if err != nil {
		slog.Warn("integration landing", "task_id", t.ID, "err", err)
		return
	}
	if !latestOpeningRun(runs).LandsOn(ib) {
		return
	}
	no := false
	if _, err := s.setMergeState(ctx, p, t, actor.MsgSetMergeState{State: domain.MergeNone, Required: &no,
		From: []string{domain.MergeNeeded},
		Detail: "Its work is on its integration branch (" + ib + "): the tickets that depend on it no longer wait; " +
			"it stays unmerged until that branch is merged"}); err != nil {
		slog.Warn("release integration merge", "task_id", t.ID, "err", err)
	}
}

// latestOpeningRun is the last of runs (oldest first) that put work up: it
// reports a PR or a branch. Zero when none does, which LandsOn nothing. It
// is the run the merge gate follows — the same one Repo.IntegrationMerged
// picks (ORDER BY created_at DESC LIMIT 1), so a correction reported as a
// run and one applied as metadata reach the same answer.
func latestOpeningRun(runs []domain.Run) domain.Run {
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].PRURL != "" || runs[i].Branch != "" {
			return runs[i]
		}
	}
	return domain.Run{}
}

func nameOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// recordMergeReport records a run that only reports t's work merged, once:
// the first report records the merge (its source and time) and releases the
// tickets waiting on it; a later one returns the first and records nothing.
func (s *Service) recordMergeReport(ctx context.Context, p authz.Principal, t *domain.Task, run domain.Run, source string) (MergeRecord, error) {
	if source == "" {
		source = mergeSourceOf(p)
	}
	if t.MergeState == domain.MergeMerged {
		// Already recorded: nothing to change on the ticket, but a step linked
		// to it may still be open (it was linked after the merge, or its
		// prerequisites finished since). Reconcile it, so a repeat report is
		// how a step that was held by a prerequisite gets closed.
		rec, err := s.alreadyMerged(ctx, t)
		if err == nil {
			rec.LinkedStep = s.recordLinkedMergeStep(ctx, p, t.ID, source)
		}
		return rec, err
	}
	now := time.Now()
	reply, err := s.setMergeState(ctx, p, t, actor.MsgSetMergeState{State: domain.MergeMerged, Source: source, At: &now,
		Detail: fmt.Sprintf("Merge recorded by %s (%s)", source, p.Label())})
	if err != nil {
		return MergeRecord{}, err
	}
	if !reply.Changed && reply.Task != nil && reply.Task.MergeState == domain.MergeMerged {
		return s.alreadyMerged(ctx, reply.Task) // another report won the race
	}
	yes := true
	run.Merged = &yes
	if run.Outcome == "" {
		run.Outcome = domain.RunCompleted
	}
	if run.Note == "" {
		run.Note = "merged (recorded by " + source + ")"
	}
	saved, err := s.recordRun(ctx, p, t, run)
	if err != nil {
		return MergeRecord{}, err
	}
	cur := reply.Task
	rec := MergeRecord{TaskID: t.ID, MergeState: domain.MergeMerged, MergeSource: source, MergedAt: &now, Run: &saved, Task: cur,
		Message: fmt.Sprintf("merged, recorded by %s", source)}
	if cur != nil {
		rec.MergedAt = cur.MergedAt
	}
	// A planned merge step tracking this merge is the same act, not a second
	// card for it (AMP #3009): it completes here, whichever of the three
	// sources recorded the merge.
	rec.LinkedStep = s.recordLinkedMergeStep(ctx, p, t.ID, source)
	// Recording a merge satisfies the merge waits of whoever depended on this
	// ticket, and a merge step of theirs may have been held by exactly that
	// wait. Re-check the ones this ticket was holding.
	s.retriggerMergeStepsAfter(ctx, p, t.ID)
	return rec, nil
}

// alreadyMerged is the no-op answer to a merge reported again.
func (s *Service) alreadyMerged(ctx context.Context, t *domain.Task) (MergeRecord, error) {
	rec := MergeRecord{TaskID: t.ID, MergeState: domain.MergeMerged, MergeSource: t.MergeSource, MergedAt: t.MergedAt,
		AlreadyRecorded: true, Task: t}
	rec.Run, _ = s.Repo.FirstMergedRun(ctx, t.ID)
	rec.Message = "already merged"
	if t.MergeSource != "" {
		rec.Message += ", recorded by " + t.MergeSource
	}
	if t.MergedAt != nil {
		rec.Message += " at " + t.MergedAt.UTC().Format(time.RFC3339)
	}
	return rec, nil
}

// MarkTaskMerged records that ticket taskID's work is merged (Mark merged in
// the UI; the webhook passes source "github"). Anyone on the project may
// report it. Idempotent: the first report wins.
func (s *Service) MarkTaskMerged(ctx context.Context, p authz.Principal, taskID int, source, mergeCommit string) (MergeRecord, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead)
	if err != nil {
		return MergeRecord{}, err
	}
	return s.markTaskMerged(ctx, p, t, source, mergeCommit)
}

func (s *Service) markTaskMerged(ctx context.Context, p authz.Principal, t *domain.Task, source, mergeCommit string) (MergeRecord, error) {
	run := domain.Run{Outcome: domain.RunCompleted}
	if runs, err := s.Repo.TaskRuns(ctx, t.ID); err == nil {
		for i := len(runs) - 1; i >= 0; i-- {
			if r := runs[i]; r.PRURL != "" || r.Branch != "" || r.Commit != "" {
				run.Branch, run.PRURL, run.Commit = r.Branch, r.PRURL, r.Commit
				break
			}
		}
	}
	if mergeCommit = strings.ToLower(strings.TrimSpace(mergeCommit)); mergeCommit != "" {
		run.Commit, run.Note = mergeCommit, "merged as "+mergeCommit
	}
	return s.recordMergeReport(ctx, p, t, run, source)
}

// MarkMergeAbandoned records that ticket taskID's PR was closed without
// merging (the webhook, AMP #2475 B3). Only a ticket waiting on its merge
// changes; its dependents keep waiting until it is reopened or its merge is
// marked not needed.
func (s *Service) MarkMergeAbandoned(ctx context.Context, p authz.Principal, taskID int, source string) (MergeRecord, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead)
	if err != nil {
		return MergeRecord{}, err
	}
	if source == "" {
		source = mergeSourceOf(p)
	}
	reply, err := s.setMergeState(ctx, p, t, actor.MsgSetMergeState{State: domain.MergeAbandoned, From: []string{domain.MergeNeeded},
		Detail: fmt.Sprintf("PR closed without merging (reported by %s)", source)})
	if err != nil {
		return MergeRecord{}, err
	}
	cur := reply.Task
	if cur == nil {
		cur = t
	}
	rec := MergeRecord{TaskID: t.ID, MergeState: cur.MergeState, MergeSource: cur.MergeSource, MergedAt: cur.MergedAt, Task: cur,
		AlreadyRecorded: !reply.Changed}
	rec.Message = "PR closed without merging: the tickets that depend on it wait until it's reopened or its merge is marked not needed"
	if !reply.Changed {
		rec.Message = "nothing changed: its merge state is " + cur.MergeState
	}
	return rec, nil
}

// MarkMergeNotNeeded is "Not needed": ticket taskID's work won't be merged
// and nothing should wait for it. Its merge state goes back to none and the
// tickets that depend on it are released. Someone who can work the ticket
// decides it.
func (s *Service) MarkMergeNotNeeded(ctx context.Context, p authz.Principal, taskID int) (MergeRecord, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return MergeRecord{}, err
	}
	if !domain.MergeGates(t.MergeState) {
		return MergeRecord{}, &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d isn't waiting on a merge (merge state %s); nothing to release",
			taskID, nameOrDash(t.MergeState))}
	}
	reply, err := s.setMergeState(ctx, p, t, actor.MsgSetMergeState{State: domain.MergeNone, From: []string{domain.MergeNeeded, domain.MergeAbandoned},
		Detail: "Merge marked not needed by " + p.Label() + ": the tickets that depend on it no longer wait"})
	if err != nil {
		return MergeRecord{}, err
	}
	cur := reply.Task
	if cur == nil {
		cur = t
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "task.merge.not_needed", ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(t.ID),
		TargetName: t.Name, Summary: fmt.Sprintf("%s marked ticket %d's merge not needed", p.Label(), t.ID),
		Details: map[string]any{"from": t.MergeState}})
	return MergeRecord{TaskID: t.ID, MergeState: cur.MergeState, Task: cur, AlreadyRecorded: !reply.Changed,
		Message: "merge not needed: the tickets that depend on it are released"}, nil
}

// MarkMergedFrom is MarkMerged with the reporter named (the webhook passes
// "github"). It returns every ticket that matched, whether this report
// recorded its merge or an earlier one had.
func (s *Service) MarkMergedFrom(ctx context.Context, p authz.Principal, commit, branch, pr, mergeCommit, source string) ([]MergeRecord, error) {
	hits, err := s.Trace(ctx, p, commit, branch, pr)
	if err != nil {
		return nil, err
	}
	return s.markHitsMerged(ctx, p, hits, mergeCommit, source)
}

// markHitsMerged records the merge on every agent ticket among hits.
func (s *Service) markHitsMerged(ctx context.Context, p authz.Principal, hits []TraceHit, mergeCommit, source string) ([]MergeRecord, error) {
	out := []MergeRecord{}
	for _, h := range hits {
		if h.Task.IsHuman() {
			continue // a person's link on their own ticket isn't work waiting to merge
		}
		t := h.Task
		last := h.Runs[0]
		run := domain.Run{Outcome: domain.RunCompleted, Branch: last.Branch, PRURL: last.PRURL, Commit: last.Commit, Note: "merged"}
		if mergeCommit != "" {
			run.Commit, run.Note = strings.ToLower(mergeCommit), "merged as "+mergeCommit
		}
		if _, _, err := s.LoadTask(ctx, p, t.ID, authz.TaskRead); err != nil {
			return out, err
		}
		rec, err := s.recordMergeReport(ctx, p, &t, run, source)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// MarkAbandonedFrom marks abandoned every ticket whose runs produced the
// branch, PR or commit and that waits on its merge (the webhook's
// pull_request closed without merging).
func (s *Service) MarkAbandonedFrom(ctx context.Context, p authz.Principal, commit, branch, pr, source string) ([]MergeRecord, error) {
	hits, err := s.Trace(ctx, p, commit, branch, pr)
	if err != nil {
		return nil, err
	}
	return s.markHitsAbandoned(ctx, p, hits, source)
}

// markHitsAbandoned marks abandoned the tickets among hits that wait on
// their merge.
func (s *Service) markHitsAbandoned(ctx context.Context, p authz.Principal, hits []TraceHit, source string) ([]MergeRecord, error) {
	out := []MergeRecord{}
	for _, h := range hits {
		if h.Task.IsHuman() || h.Task.MergeState != domain.MergeNeeded {
			continue
		}
		rec, err := s.MarkMergeAbandoned(ctx, p, h.Task.ID, source)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// bridgeHumanMerge: a person marking their ticket done with a PR link
// (a legacy merge ticket) records that PR merged on the agent tickets whose
// runs carry the same link, so one acknowledgement clears both. Returns the
// tickets it recorded.
func (s *Service) bridgeHumanMerge(ctx context.Context, p authz.Principal, human *domain.Task, prURL string) []int {
	if prURL == "" {
		return nil
	}
	recs, err := s.MarkMergedFrom(ctx, p, "", "", prURL, "", domain.MergeSourceUI)
	if err != nil {
		slog.Warn("bridge human merge", "task_id", human.ID, "pr_url", prURL, "err", err)
	}
	var ids []int
	for _, r := range recs {
		if r.TaskID != human.ID && !r.AlreadyRecorded {
			ids = append(ids, r.TaskID)
		}
	}
	return ids
}

// reconcileMergeGated makes the tickets that depend on projectID's done,
// merge-waiting tickets re-read their dependencies: after require_merge is
// switched, they wait or are released to match.
func (s *Service) reconcileMergeGated(ctx context.Context, projectID int) {
	gated, err := s.Repo.MergeGatedTasks(ctx, projectID)
	if err != nil || len(gated) == 0 {
		return
	}
	deps, err := s.Repo.Dependents(ctx, gated)
	if err != nil {
		return
	}
	s.ReconcileDependents(ctx, projectID, deps, nil)
}
