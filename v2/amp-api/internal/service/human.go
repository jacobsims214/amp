package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// Human work: a ticket whose performer is "human" is done by its owner, who
// starts and finishes it in AMP (or tells their agent it's done). It sits in
// the dependency graph like any other ticket, so the agent work after it
// waits for the person and starts as soon as they finish.

// HumanDone is what a person reports when they finish a human ticket.
type HumanDone struct {
	Note        string   `json:"note"`
	PRURL       string   `json:"pr_url"`
	HoursWorked *float64 `json:"hours_worked"`
}

func refuseAgentAccount(p authz.Principal) error {
	if p.Agent {
		return authz.Forbidden("agent accounts don't do human work; its owner starts and finishes it")
	}
	return nil
}

// StartHumanTask starts a human ticket for its owner: an unassigned one is
// claimed by them, someone else's is refused.
func (s *Service) StartHumanTask(ctx context.Context, p authz.Principal, taskID int) (*domain.Task, error) {
	if err := refuseAgentAccount(p); err != nil {
		return nil, err
	}
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskDispatch)
	if err != nil {
		return nil, err
	}
	if err := authz.RequireUI(p, fmt.Sprintf("Starting ticket %d (a person's ticket)", taskID), s.TaskURL(t.ProjectID, taskID)); err != nil {
		return nil, err
	}
	if !t.IsHuman() {
		return nil, authz.BadRequest("ticket %d is agent work; dispatch it to an agent, or switch it to a person first", taskID)
	}
	pid, err := s.Registry.Get(t.ProjectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgDispatchTask{
		TaskID: taskID, AgentID: domain.HumanAgentID, By: p.UserRef(), Human: true, ReplyCh: replyCh,
	})
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		return nil, authz.BadRequest("%v", reply.Err)
	}
	return s.Repo.GetTask(ctx, taskID)
}

// CompleteHumanTask marks a human ticket done and records the person's work
// as a run (agent_id "human"): their note, a link, and the hours they spent.
// A human ticket is the person's own gate (a merge, a deploy, a sign-off),
// so only the person marks it done, in the web UI; an agent recording it
// for them (viaAgent, over MCP) is refused with the ticket's link. A
// judgment ticket with a decision doc is refused too: RecordDecision
// finishes it.
func (s *Service) CompleteHumanTask(ctx context.Context, p authz.Principal, taskID int, done HumanDone, viaAgent bool) (*domain.Task, *domain.Run, error) {
	if err := s.refuseAgentDecision(ctx, p, taskID, viaAgent); err != nil {
		return nil, nil, err
	}
	if err := refuseAgentAccount(p); err != nil {
		return nil, nil, err
	}
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return nil, nil, err
	}
	if !t.IsHuman() {
		return nil, nil, authz.BadRequest("ticket %d is agent work, not a person's", taskID)
	}
	if viaAgent || !p.InUI() {
		err := authz.UIOnly(fmt.Sprintf("Marking ticket %d (a person's ticket) done", taskID), s.TaskURL(t.ProjectID, taskID))
		s.auditTaskDenied(ctx, p, "task.human.complete", t, err, map[string]any{"via_agent": viaAgent})
		return nil, nil, err
	}
	// A ticket with a decision doc is finished by Record decision, which
	// freezes the version its dependents receive; Mark done would skip that.
	doc, err := s.Repo.GetDecisionDoc(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	if doc != nil {
		return nil, nil, &authz.Error{Status: 409, Msg: fmt.Sprintf(decisionMarkDoneRefusal, s.TaskURL(t.ProjectID, taskID))}
	}
	return s.finishHumanTask(ctx, p, t, done, nil)
}

// decisionFinish is what Record decision passes to finishHumanTask: who
// decided (the stored decided_by), to whom the run and the comment are
// attributed even when someone else finishes a stalled record.
type decisionFinish struct {
	decider *int
}

// finishHumanTask completes a person's human ticket and records their run
// and the note's comment. Its callers have checked who may act. For Record
// decision (rec), the run is written and decision_docs.run_id set in one
// transaction, only while run_id is unset (Repo.AddDecisionRun), and the
// comment is posted only by the call that wrote the run: a retry of a record
// that stopped part way completes the ticket again (a no-op) without a second
// run or comment. Its actor refusals come back as 409s (the ticket can't be
// completed), which RecordDecision tells apart from transient failures.
func (s *Service) finishHumanTask(ctx context.Context, p authz.Principal, t *domain.Task, done HumanDone, rec *decisionFinish) (*domain.Task, *domain.Run, error) {
	taskID := t.ID
	if h := done.HoursWorked; h != nil && (*h < 0 || *h > 1000) {
		return nil, nil, authz.BadRequest("hours_worked must be between 0 and 1000")
	}
	note := strings.TrimSpace(done.Note)
	decision := isDecisionReason(t.HumanReason)
	if decision && note == "" {
		return nil, nil, authz.BadRequest("a decision ticket needs your decision: say what you decided")
	}
	if rec != nil && s.DecisionCompleteFault != nil {
		if err := s.DecisionCompleteFault(taskID); err != nil {
			return nil, nil, err
		}
	}
	pid, err := s.Registry.Get(t.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	replyCh := make(chan actor.ReplyComplete, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgCompleteTask{TaskID: taskID, By: p.UserRef(), ByName: p.Label(), InUI: p.InUI(), ReplyCh: replyCh})
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		if rec != nil && !errors.Is(reply.Err, actor.ErrNoReply) {
			return nil, nil, &authz.Error{Status: 409, Msg: reply.Err.Error()}
		}
		return nil, nil, reply.Err
	}
	runIn := domain.Run{
		AgentID: domain.HumanAgentID, Outcome: domain.RunCompleted,
		PRURL: strings.TrimSpace(done.PRURL), Note: note, HoursWorked: done.HoursWorked,
	}
	var run domain.Run
	name := ""
	if rec != nil {
		if s.DecisionRunFault != nil {
			if err := s.DecisionRunFault(taskID); err != nil {
				return nil, nil, err
			}
		}
		runIn.TaskID, runIn.ProjectID, runIn.UserID = t.ID, t.ProjectID, rec.decider
		var wrote bool
		if run, wrote, err = s.Repo.AddDecisionRun(ctx, taskID, runIn); err != nil {
			return nil, nil, err
		}
		if !wrote {
			// Another attempt wrote the run and posts the comment.
			updated, err := s.Repo.GetTask(ctx, taskID)
			return updated, nil, err
		}
		name = s.userNameOr(ctx, rec.decider, p)
	} else {
		if run, err = s.recordRun(ctx, p, t, runIn); err != nil {
			return nil, nil, err
		}
		name = s.personName(ctx, p)
	}
	// A person's ticket is their gate (a merge, a deploy, a sign-off): who
	// marked it done, and from where, belongs in the audit log.
	details := map[string]any{"run_id": run.ID, "note_length": len(note)}
	if run.PRURL != "" {
		details["pr_url"] = run.PRURL
	}
	if done.HoursWorked != nil {
		details["hours_worked"] = *done.HoursWorked
	}
	// Bridge (AMP #2475): a person's ticket done with a PR link (a legacy
	// merge ticket) records that PR merged on the agent tickets that
	// produced it, so one acknowledgement clears both.
	if ids := s.bridgeHumanMerge(ctx, p, t, run.PRURL); len(ids) > 0 {
		details["merged_task_ids"] = ids
	}
	// The reverse bridge (AMP #3009): a step linked to its PR with merge_for
	// records that ticket's merge as the same act, so finishing the step is
	// one click for the whole merge and nothing is left half-recorded.
	//
	// It only ever records a merge that was still waiting to be recorded, and
	// only when the person said they merged (a PR link on their run): merge_for
	// says which ticket's merge the step tracks, not that every completion of
	// the step is a merge. Without both, linking an approval or a sign-off to
	// a ticket would claim a merge nobody performed, which is the opposite of
	// what this key is for.
	if forID := MergeFor(t); forID != 0 {
		details["merge_for"] = forID
		switch {
		case run.PRURL == "":
			// The person said the step is done, not that they merged
			// anything: no merge is claimed on their behalf.
			details["merge_inferred"] = false
		case !s.mergedNow(ctx, forID):
			// The merge this step tracks is still owed, and the person says
			// they merged it: recording it is the same act as finishing the
			// step, so the ticket stops waiting and its dependents start.
			rec, err := s.MarkTaskMerged(ctx, p, forID, domain.MergeSourceUI, "")
			if err != nil {
				slog.Warn("record the merge a linked step finished", "task_id", taskID, "merge_for", forID, "err", err)
				details["merge_error"] = err.Error()
			} else {
				details["merge_recorded"] = !rec.AlreadyRecorded
			}
		default:
			// Already recorded — by this run's textual bridge above, by
			// GitHub, by CI, or by someone's Mark merged. Nothing is left to
			// record; keep the detail key so the audit row still says so.
			details["merge_recorded"] = false
		}
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "task.human.complete", ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(taskID),
		TargetName: t.Name, Summary: fmt.Sprintf("%s marked their ticket %d done", p.Label(), taskID), Details: details})
	// The note reaches the people and agents waiting on this ticket: it is a
	// comment on it, and upstream_decisions carries it to its dependents.
	if note != "" {
		verb := "Done"
		if decision {
			verb = "Decision"
		}
		s.comment(t.ProjectID, taskID, p, fmt.Sprintf("%s by %s: %s", verb, name, note))
	}
	updated, err := s.Repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	return updated, &run, nil
}

// isDecisionReason: a human ticket whose note is a decision other tickets
// depend on, so the note is required.
func isDecisionReason(reason string) bool { return reason == "judgment" || reason == "approval" }

// personName is how a person is named in a comment: their display name, else
// their email.
func (s *Service) personName(ctx context.Context, p authz.Principal) string {
	return s.userNameOr(ctx, p.UserRef(), p)
}

// userNameOr is how user id is named in a comment: their display name, else
// their email; p's label when id is nil or unknown.
func (s *Service) userNameOr(ctx context.Context, id *int, p authz.Principal) string {
	if id != nil {
		if u, err := s.Repo.GetUserByID(ctx, *id); err == nil && u != nil {
			if u.DisplayName != "" {
				return u.DisplayName
			}
			if u.Email != "" {
				return u.Email
			}
		}
	}
	return p.Label()
}
