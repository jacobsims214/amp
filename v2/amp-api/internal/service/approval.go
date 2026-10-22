package service

import (
	"context"
	"fmt"
	"net/mail"
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
	// "scheduled:" is the reason the scheduler writes; a ticket is scheduled
	// with amp_set_task_start_at, never by setting the reason by hand (that
	// would also hide a question as a schedule).
	if state == domain.TaskStateBlocked && strings.HasPrefix(strings.TrimSpace(reason), "scheduled:") {
		return nil, authz.BadRequest("ticket %d: a block reason can't start with \"scheduled:\"; schedule a ticket with amp_set_task_start_at", taskID)
	}
	requireStarted := false
	if !p.InUI() {
		// A ticket waiting on a person's answer keeps its question until it's
		// answered: off the UI, no state change may replace or clear it, so
		// the answer always leaves a "relayed by agent" record.
		if hasOpenQuestion(t) && (state != domain.TaskStateBlocked || reason != t.BlockReason) {
			switch state {
			case domain.TaskStateBacklog, domain.TaskStateBlocked:
				return nil, authz.Forbidden("ticket %d is waiting on a question (%q); a state change doesn't answer or replace it. A person answers it "+
					"in the AMP web UI (%s), or relay their answer with amp_answer_question.", taskID, t.BlockReason, s.TaskURL(t.ProjectID, taskID))
			}
		}
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

// hasOpenQuestion reports whether t is blocked on a question for a person
// (a reason that isn't the scheduler's "scheduled:").
func hasOpenQuestion(t *domain.Task) bool {
	return t.State == domain.TaskStateBlocked && t.BlockReason != "" && !strings.HasPrefix(t.BlockReason, "scheduled:")
}

// relayedSuffix marks a comment or history actor an agent posted for a
// person; the web UI badges it ("relayed by agent").
const relayedSuffix = " (relayed by agent)"

// CommentAuthor is the author recorded for a comment p posts as author
// (amp_add_task_comment). In the web UI it's kept as given. Off the UI,
// author stays free text for agent names, but an author that is a person's
// email (the caller's own, or any person's account) is recorded as
// "<email> (relayed by agent)": workers treat answers on a ticket as
// binding, so an agent can't post words as if the person typed them.
func (s *Service) CommentAuthor(ctx context.Context, p authz.Principal, author string) string {
	a := strings.TrimSpace(author)
	if p.InUI() || !strings.Contains(a, "@") || strings.HasSuffix(a, strings.TrimSpace(relayedSuffix)) {
		return author
	}
	addr, err := mail.ParseAddress(a)
	if err != nil {
		return author
	}
	email := addr.Address
	if p.Email != "" && strings.EqualFold(email, p.Email) {
		return email + relayedSuffix
	}
	if s.Repo != nil {
		if u, err := s.Repo.GetUserByEmail(ctx, email); err == nil && !u.IsAgent() {
			return u.Email + relayedSuffix
		}
	}
	return author
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

// UpdateTask applies an edit to t (amp_update_task, REST PATCH
// /api/tasks/{id}); the caller has loaded t with the access the edit needs.
// Off the web UI, a person's ticket can't be switched to agent work: an agent
// could otherwise flip it, dispatch it and complete it, which is marking the
// person's ticket done without them. Agent work may become a person's ticket
// from anywhere. The task actor re-checks atomically, and a refusal leaves
// the ticket unchanged. Other actor errors come back as 400.
func (s *Service) UpdateTask(ctx context.Context, p authz.Principal, t *domain.Task, req domain.UpdateTaskRequest) error {
	req.TaskID = t.ID
	keepHuman := ""
	if !p.InUI() {
		refusal := authz.UIOnly(fmt.Sprintf("Switching ticket %d (a person's ticket) to agent work", t.ID), s.TaskURL(t.ProjectID, t.ID))
		if t.IsHuman() && strings.EqualFold(strings.TrimSpace(req.Performer), domain.PerformerAgent) {
			return refusal
		}
		keepHuman = refusal.Error()
	}
	replyCh := make(chan actor.ReplySimple, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgUpdateTask{Req: req, KeepHuman: keepHuman, ReplyCh: replyCh}); err != nil {
		return err
	}
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		if keepHuman != "" && reply.Err.Error() == keepHuman {
			return &authz.Error{Status: 403, Msg: keepHuman}
		}
		return &authz.Error{Status: 400, Msg: reply.Err.Error()}
	}
	return nil
}
