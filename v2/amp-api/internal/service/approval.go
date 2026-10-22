package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// Human-only approval (epic 162). A plan, an artifact, turning plan approval
// off, and a person's own ticket are approved only by a person in the web UI
// (authz.Principal.InUI: a token issued to the UI's own client, on the REST
// listener). Agents, over MCP or with API tokens, get a refusal naming the
// page where the person does it. Agents also can't move work forward that a
// person hasn't approved: completing or starting a ticket needs it
// dispatched, and dispatch needs the plan approved.
// See KB architecture/plan-approval.md.

// link turns an app path into a URL a person can open: absolute when AMP
// knows its public address, the bare path otherwise.
func (s *Service) link(path string) string {
	if s.AppURL == "" {
		return path
	}
	return strings.TrimSuffix(s.AppURL, "/") + path
}

// PlanURL is the project's Flight plan page, where a person approves the plan.
func (s *Service) PlanURL(projectID int) string {
	return s.link(fmt.Sprintf("/project/%d/plan", projectID))
}

// TaskURL opens the ticket on its project's board.
func (s *Service) TaskURL(projectID, taskID int) string {
	return s.link(fmt.Sprintf("/project/%d/board?task=%d", projectID, taskID))
}

// SettingsURL is the project's settings page.
func (s *Service) SettingsURL(projectID int) string {
	return s.link(fmt.Sprintf("/project/%d/settings", projectID))
}

// approvalGate refuses a non-UI caller moving t forward while it waits for
// the plan to be approved (the project requires approval and t was planned
// after the last one). It fails closed when the project can't be loaded.
func (s *Service) approvalGate(ctx context.Context, p authz.Principal, t *domain.Task, doing string) error {
	if p.InUI() {
		return nil
	}
	proj, err := s.Repo.GetProject(ctx, t.ProjectID)
	if err != nil {
		return &authz.Error{Status: 503, Msg: fmt.Sprintf("ticket %d: couldn't load project %d to check plan approval (%v); nothing changed, try again",
			t.ID, t.ProjectID, err)}
	}
	if proj.RequirePlanApproval && proj.AwaitsApproval(t) {
		return authz.Forbidden("ticket %d can't be %s: it was planned after the plan was last approved, and agents can't move "+
			"unapproved work forward. A person approves the plan in the AMP web UI: %s. Stop and ask your person to approve it there.",
			t.ID, doing, s.PlanURL(t.ProjectID))
	}
	return nil
}

// CompleteTask marks an agent ticket done. A person in the web UI may
// complete any ticket they can work. Anyone else (an agent over MCP, an API
// token) only completes work that was dispatched and approved: the ticket
// must be in progress, so a ticket never dispatched can't be closed by the
// agent that skipped the dispatch. Human tickets go through
// CompleteHumanTask.
func (s *Service) CompleteTask(ctx context.Context, p authz.Principal, taskID int) (*domain.Task, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return nil, err
	}
	if t.IsHuman() {
		return nil, authz.BadRequest("ticket %d is a person's ticket; they mark it done in AMP: %s", taskID, s.TaskURL(t.ProjectID, taskID))
	}
	if err := s.checkAgentCompletion(ctx, p, t); err != nil {
		return nil, err
	}
	if err := s.sendComplete(ctx, p, t, !p.InUI()); err != nil {
		return nil, err
	}
	return s.Repo.GetTask(ctx, taskID)
}

// checkAgentCompletion applies the off-UI completion rules to agent ticket t.
func (s *Service) checkAgentCompletion(ctx context.Context, p authz.Principal, t *domain.Task) error {
	if p.InUI() {
		return nil
	}
	switch t.State {
	case domain.TaskStateCompleted:
		return nil // idempotent
	case domain.TaskStateInProgress:
	default:
		return &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d is %s: it was never dispatched, so an agent can't complete it. "+
			"Dispatch it first (amp_dispatch_task, once the plan is approved: %s), or a person completes it in the AMP web UI: %s.",
			t.ID, t.State, s.PlanURL(t.ProjectID), s.TaskURL(t.ProjectID, t.ID))}
	}
	return s.approvalGate(ctx, p, t, "completed")
}

// sendComplete completes t in its actor. requireStarted makes the actor
// re-check, atomically, that the ticket is still in progress.
func (s *Service) sendComplete(ctx context.Context, p authz.Principal, t *domain.Task, requireStarted bool) error {
	replyCh := make(chan actor.ReplySimple, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgCompleteTask{
		TaskID: t.ID, By: p.UserRef(), ByName: p.Label(), RequireStarted: requireStarted, ReplyCh: replyCh,
	}); err != nil {
		return err
	}
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		return &authz.Error{Status: 409, Msg: reply.Err.Error()}
	}
	return nil
}

// SetTaskState overrides a ticket's state (amp_set_task_state). A person in
// the web UI may set any state. Off the UI, work only moves forward the
// normal way: in_progress is refused (dispatch it), completed follows
// CompleteTask's rules, a question isn't cleared by a state change (answer
// it), and a person's ticket isn't marked done. Moving work back (to
// backlog from in progress or completed, or to blocked) is always allowed.
func (s *Service) SetTaskState(ctx context.Context, p authz.Principal, taskID int, state domain.TaskState, reason string) (*domain.Task, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return nil, err
	}
	requireStarted := false
	if !p.InUI() {
		switch state {
		case domain.TaskStateInProgress:
			if t.State != domain.TaskStateInProgress {
				return nil, authz.Forbidden("ticket %d: agents can't set a ticket in progress by hand; dispatch it with amp_dispatch_task "+
					"(it needs the plan approved by a person in the AMP web UI: %s)", taskID, s.PlanURL(t.ProjectID))
			}
		case domain.TaskStateCompleted:
			if t.IsHuman() {
				return nil, authz.RequireUI(p, fmt.Sprintf("Marking ticket %d (a person's ticket) done", taskID), s.TaskURL(t.ProjectID, taskID))
			}
			if err := s.checkAgentCompletion(ctx, p, t); err != nil {
				return nil, err
			}
			requireStarted = t.State != domain.TaskStateCompleted
		case domain.TaskStateBacklog:
			if t.State == domain.TaskStateBlocked && t.BlockReason != "" && !strings.HasPrefix(t.BlockReason, "scheduled:") {
				return nil, authz.Forbidden("ticket %d is waiting on a question (%q); a state change doesn't answer it. A person answers it "+
					"in the AMP web UI (%s), or relay their answer with amp_answer_question.", taskID, t.BlockReason, s.TaskURL(t.ProjectID, taskID))
			}
		}
	}
	replyCh := make(chan actor.ReplySimple, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgSetTaskState{
		TaskID: taskID, State: state, Reason: reason, By: p.UserRef(), ByName: p.Label(), RequireStarted: requireStarted, ReplyCh: replyCh,
	}); err != nil {
		return nil, err
	}
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		return nil, &authz.Error{Status: 409, Msg: reply.Err.Error()}
	}
	t.State = state
	return t, nil
}

// checkRunOutcome refuses a non-UI "completed" run on a ticket that was
// never dispatched: the run record would claim work no one approved.
func (s *Service) checkRunOutcome(p authz.Principal, t *domain.Task, outcome string) error {
	if p.InUI() || outcome != domain.RunCompleted || t.IsHuman() {
		return nil
	}
	if t.State == domain.TaskStateBacklog || (t.State == domain.TaskStateBlocked && t.DispatchedAt == nil) {
		return &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d was never dispatched, so a completed run can't be recorded on it; "+
			"dispatch it first (once the plan is approved: %s)", t.ID, s.PlanURL(t.ProjectID))}
	}
	return nil
}
