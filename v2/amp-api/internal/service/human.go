package service

import (
	"context"
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
// viaAgent is set when their agent records it for them (over MCP).
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
	if h := done.HoursWorked; h != nil && (*h < 0 || *h > 1000) {
		return nil, nil, authz.BadRequest("hours_worked must be between 0 and 1000")
	}
	pid, err := s.Registry.Get(t.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgCompleteTask{TaskID: taskID, By: p.UserRef(), ReplyCh: replyCh})
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		return nil, nil, reply.Err
	}
	note := strings.TrimSpace(done.Note)
	if viaAgent {
		who := p.Email
		if who == "" {
			who = "its owner"
		}
		note = strings.TrimSpace("Recorded by " + who + "'s agent. " + note)
	}
	run, err := s.recordRun(ctx, p, t, domain.Run{
		AgentID: domain.HumanAgentID, Outcome: domain.RunCompleted,
		PRURL: strings.TrimSpace(done.PRURL), Note: note, HoursWorked: done.HoursWorked,
	})
	if err != nil {
		return nil, nil, err
	}
	updated, err := s.Repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	return updated, &run, nil
}
