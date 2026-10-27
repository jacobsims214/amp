package service

import (
	"context"
	"fmt"
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
// for them (viaAgent, over MCP) is refused with the ticket's link.
func (s *Service) CompleteHumanTask(ctx context.Context, p authz.Principal, taskID int, done HumanDone, viaAgent bool) (*domain.Task, *domain.Run, error) {
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
	if h := done.HoursWorked; h != nil && (*h < 0 || *h > 1000) {
		return nil, nil, authz.BadRequest("hours_worked must be between 0 and 1000")
	}
	pid, err := s.Registry.Get(t.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgCompleteTask{TaskID: taskID, By: p.UserRef(), ByName: p.Label(), ReplyCh: replyCh})
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		return nil, nil, reply.Err
	}
	note := strings.TrimSpace(done.Note)
	run, err := s.recordRun(ctx, p, t, domain.Run{
		AgentID: domain.HumanAgentID, Outcome: domain.RunCompleted,
		PRURL: strings.TrimSpace(done.PRURL), Note: note, HoursWorked: done.HoursWorked,
	})
	if err != nil {
		return nil, nil, err
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
	s.audit(ctx, p, domain.AuditEntry{Action: "task.human.complete", ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(taskID),
		TargetName: t.Name, Summary: fmt.Sprintf("%s marked their ticket %d done", p.Label(), taskID), Details: details})
	updated, err := s.Repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	return updated, &run, nil
}
