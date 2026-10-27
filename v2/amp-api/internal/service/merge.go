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

// ---- Merge signal (AMP #2475, KB decisions/human-work-ux.md §4) ----
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
}

// mergeSourceOf is who a merge report comes from when the caller doesn't
// say: a person in the web UI, otherwise CI (an API token or an agent).
func mergeSourceOf(p authz.Principal) string {
	if p.InUI() {
		return domain.MergeSourceUI
	}
	return domain.MergeSourceCI
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
	return true
}

// CompleteTaskWithRun completes agent ticket taskID as CompleteTask does,
// knowing the run that finished it: when that run leaves work waiting on a
// merge (mergeGateFor), the ticket completes with merge_state "needed" and
// its dependents keep waiting. The caller records the run afterwards.
func (s *Service) CompleteTaskWithRun(ctx context.Context, p authz.Principal, taskID int, run *domain.Run) (*domain.Task, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return nil, err
	}
	if t.IsHuman() {
		return nil, authz.BadRequest("ticket %d is a person's ticket; they mark it done in AMP: %s", taskID, s.TaskURL(t.ProjectID, taskID))
	}
	if err := s.checkAgentCompletion(ctx, p, t); err != nil {
		s.auditTaskDenied(ctx, p, "task.complete", t, err, nil)
		return nil, err
	}
	gate := t.State != domain.TaskStateCompleted && s.mergeGateFor(ctx, t, run, nil)
	where := ""
	if gate {
		where = mergeWhere(*run)
	}
	if err := s.sendCompleteMerge(ctx, p, t, !p.InUI(), gate, where); err != nil {
		return nil, err
	}
	return s.Repo.GetTask(ctx, taskID)
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
// a run that reports the work merged records the merge (if nothing had),
// and a PR or pushed branch reported after t completed, while merges are
// required, makes it wait on that merge.
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
	case t.State == domain.TaskStateCompleted && t.MergeState == domain.MergeNone && !t.MergeRequired &&
		s.mergeGateFor(ctx, t, &run, t.CompletedAt):
		yes := true
		if _, err := s.setMergeState(ctx, p, t, actor.MsgSetMergeState{State: domain.MergeNeeded, Required: &yes, Where: mergeWhere(run),
			From:   []string{domain.MergeNone},
			Detail: "Its work was reported on " + nameOrDash(mergeWhere(run)) + " after it was done: the tickets that depend on it wait for the merge"}); err != nil {
			slog.Warn("record merge needed", "task_id", t.ID, "err", err)
		}
	}
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
		return s.alreadyMerged(ctx, t)
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
