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

// PlanURL is where a person approves the plan: the project's My work tab
// with the Approve view open on everything awaiting them (KB
// decisions/human-work-ux.md: approval happens only in My work). Callers keep
// returning it as approve_url / plan_url; the old /project/:id/plan link still
// works, because Flight plan links on to My work.
func (s *Service) PlanURL(projectID int) string {
	return s.link(fmt.Sprintf("/project/%d/my-work?approve=all", projectID))
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
// approval (the project requires approval and t isn't approved as planned
// now: Task.Approved). It fails closed when the project can't be loaded.
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
		return authz.Forbidden("ticket %d can't be %s: it isn't approved since it was planned or its scope last changed, and agents can't move "+
			"unapproved work forward. A person approves the plan in the AMP web UI: %s. Stop and ask your person to approve it there.",
			t.ID, doing, s.PlanURL(t.ProjectID))
	}
	return nil
}

// CompleteTask marks an agent ticket done. A person in the web UI may
// complete any ticket they can work. Anyone else (an agent over MCP, an API
// token) completes dispatched and approved work, as its worker (with the
// current dispatch_id) or as a closeout (AMP #2801): a completion by
// somebody other than the worker, or of a ticket no worker started, is
// allowed but recorded as a closeout with the caller's reason. Human
// tickets go through CompleteHumanTask.
func (s *Service) CompleteTask(ctx context.Context, p authz.Principal, taskID int) (*domain.Task, error) {
	t, _, err := s.CompleteTaskWithRun(ctx, p, taskID, nil, CompleteClaim{})
	return t, err
}

// Closeout is what a completion recorded beyond the ticket: whether it was
// a closeout (the caller wasn't the ticket's worker) and why.
type Closeout struct {
	Closeout bool   `json:"closeout"`
	Reason   string `json:"-"`
}

// checkAgentCompletion applies the off-UI completion rules to agent ticket
// t, returning whether the completion must go through as a closeout.
//
// A closeout is allowed where the worker's own completion would have been:
// it takes prior work on the ticket (a run), which is what makes it a
// closeout of somebody's finished work rather than a claim that work
// happened. That is also how a ready ticket with runs is closed out without
// dispatching it first (#2473/#2494).
func (s *Service) checkAgentCompletion(ctx context.Context, p authz.Principal, t *domain.Task, dispatchID string) error {
	if p.InUI() {
		return nil
	}
	if t.State == domain.TaskStateCompleted {
		return nil // idempotent
	}
	worker := t.CurrentDispatch(dispatchID)
	if t.State != domain.TaskStateInProgress {
		// Never dispatched (or dispatched and returned): a closeout only,
		// and only of work that has something to show for it.
		if t.State == domain.TaskStateBlocked && hasOpenQuestion(t) {
			return &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d is waiting on a question (%q): a closeout doesn't answer or replace it. "+
				"Relay the person's answer with amp_answer_question first, then close it out: %s", t.ID, t.BlockReason, s.TaskURL(t.ProjectID, t.ID))}
		}
		if t.State == domain.TaskStateBlocked {
			if err := s.openDepsGate(ctx, t, "closed out"); err != nil {
				return err
			}
		}
		if err := s.closeoutEvidence(ctx, t); err != nil {
			return err
		}
	} else if worker {
		// The worker's own: it must have been started (the claim), or its
		// completion is the fallback start of a plugin that doesn't claim.
		if err := s.openDepsGate(ctx, t, "completed"); err != nil {
			return err
		}
	} else if err := s.openDepsGate(ctx, t, "closed out"); err != nil {
		return err
	}
	return s.approvalGate(ctx, p, t, "completed")
}

// closeoutEvidence refuses a closeout of a ticket with nothing on it: no
// run, so no worker ever did anything there. Without it, an agent could mark
// any ready ticket done.
func (s *Service) closeoutEvidence(ctx context.Context, t *domain.Task) error {
	n, err := s.Repo.CountRuns(ctx, t.ID)
	if err != nil {
		return &authz.Error{Status: 503, Msg: fmt.Sprintf("ticket %d: couldn't read its runs (%v); nothing changed, try again", t.ID, err)}
	}
	if n > 0 {
		return nil
	}
	return &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d is %s: it was never dispatched and has no work recorded on it, so an agent "+
		"can't complete or close it out. Dispatch it and let its worker do it (amp_dispatch_task, once the plan is approved: %s), "+
		"or a person completes it in the AMP web UI: %s.",
		t.ID, t.State, s.PlanURL(t.ProjectID), s.TaskURL(t.ProjectID, t.ID))}
}

// openDepsGate refuses (409) moving t forward off the web UI while any of
// its dependencies is incomplete, naming each one; a person's ticket comes
// with the link where they finish it. The task actor re-checks atomically
// (MsgCompleteTask/MsgSetTaskState RequireDeps; dispatch always).
func (s *Service) openDepsGate(ctx context.Context, t *domain.Task, doing string) error {
	if len(t.DependencyIDs) == 0 {
		return nil
	}
	done, _, err := s.Repo.DepStatesFor(ctx, t.ID, t.DependencyIDs)
	if err != nil {
		return &authz.Error{Status: 503, Msg: fmt.Sprintf("ticket %d: couldn't check its dependencies (%v); nothing changed, try again", t.ID, err)}
	}
	var waits []string
	for _, d := range t.DependencyIDs {
		if done[d] {
			continue
		}
		w := fmt.Sprintf("ticket %d", d)
		if dep, err := s.Repo.GetTask(ctx, d); err == nil {
			w = fmt.Sprintf("ticket %d %q (%s)", d, dep.Name, dep.State)
			if dep.IsHuman() {
				w += fmt.Sprintf(", a person's ticket: they finish it in the AMP web UI (%s)", s.TaskURL(dep.ProjectID, d))
			}
		}
		waits = append(waits, w)
	}
	if len(waits) == 0 {
		return nil
	}
	return &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d can't be %s: it waits on %s. It becomes ready by itself once "+
		"its dependencies are complete; no state change gets it there sooner.", t.ID, doing, strings.Join(waits, "; "))}
}

// sendComplete completes t in its actor. offUI makes the actor re-check,
// atomically, that the ticket is still in progress and that its
// dependencies are complete.
func (s *Service) sendComplete(ctx context.Context, p authz.Principal, t *domain.Task, offUI bool) error {
	_, err := s.sendCompleteMerge(ctx, p, t, offUI, false, "", "", "")
	return err
}

// sendCompleteMerge is sendComplete for work that may wait on a merge
// (MsgCompleteTask.MergeRequired). dispatchID is the caller's claim to be
// the ticket's worker; closeoutReason is why it is closing a ticket that
// isn't its own work. The reply says whether the completion was recorded as
// a closeout.
func (s *Service) sendCompleteMerge(ctx context.Context, p authz.Principal, t *domain.Task, offUI, mergeRequired bool, mergeWhere, dispatchID, closeoutReason string) (actor.ReplyComplete, error) {
	replyCh := make(chan actor.ReplyComplete, 1)
	// A closeout of a ticket that isn't in progress is the one completion the
	// actor would otherwise refuse off the UI: its prior work is what makes it
	// one, and checkAgentCompletion has read it (AMP #2801).
	closeoutOK := offUI && t.State != domain.TaskStateCompleted && !t.IsHuman() &&
		t.State != domain.TaskStateInProgress && !(t.State == domain.TaskStateBlocked && hasOpenQuestion(t))
	if err := s.Registry.Send(t.ProjectID, &actor.MsgCompleteTask{
		TaskID: t.ID, By: p.UserRef(), ByName: p.Label(), RequireStarted: offUI, RequireDeps: offUI,
		MergeRequired: mergeRequired, MergeWhere: mergeWhere, DispatchID: dispatchID, CloseoutReason: closeoutReason,
		CloseoutOK: closeoutOK, InUI: p.InUI(), ReplyCh: replyCh,
	}); err != nil {
		return actor.ReplyComplete{}, err
	}
	reply := actor.Await(ctx, replyCh)
	if reply.Err != nil {
		return reply, &authz.Error{Status: 409, Msg: reply.Err.Error()}
	}
	return reply, nil
}

// SetTaskState overrides a ticket's state (amp_set_task_state). A person in
// the web UI may set any state. Off the UI, work only moves forward the
// normal way: in_progress is refused (dispatch it), completed follows
// CompleteTask's rules, a question isn't cleared by a state change (answer
// it), a person's ticket isn't marked done, and a ticket waiting on an
// incomplete dependency isn't made ready (backlog) or done. Moving work back
// (to backlog from in progress or completed, or to blocked) is always
// allowed; with a dependency open it lands blocked on it.
func (s *Service) SetTaskState(ctx context.Context, p authz.Principal, taskID int, state domain.TaskState, reason string) (*domain.Task, error) {
	return s.SetTaskStateAs(ctx, p, taskID, state, reason, "")
}

// SetTaskStateAs is SetTaskState by a caller that presents a dispatch_id:
// the worker setting its own ticket's state, so a move to completed is the
// worker's rather than a closeout (AMP #2801). Every other caller passes "".
func (s *Service) SetTaskStateAs(ctx context.Context, p authz.Principal, taskID int, state domain.TaskState, reason, dispatchID string) (*domain.Task, error) {
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
	requireStarted, err := s.offUIStateGate(ctx, p, t, state, reason, dispatchID)
	if err != nil {
		s.auditTaskDenied(ctx, p, "task.state", t, err, map[string]any{"to": string(state)})
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgSetTaskState{
		TaskID: taskID, State: state, Reason: reason, By: p.UserRef(), ByName: p.Label(),
		RequireStarted: requireStarted, RequireDeps: !p.InUI(), DispatchID: dispatchID, InUI: p.InUI(), ReplyCh: replyCh,
	}); err != nil {
		return nil, err
	}
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		return nil, &authz.Error{Status: 409, Msg: reply.Err.Error()}
	}
	if t.State == domain.TaskStateCompleted && state != domain.TaskStateCompleted {
		// Reopened: the tickets after it re-read their dependencies, so none
		// keeps saying why it waited on the done ticket (its PR closed
		// without merging, "reopen it") once the ticket is open again.
		if deps, err := s.Repo.OpenDependents(ctx, []int{taskID}); err == nil {
			s.ReconcileDependents(ctx, t.ProjectID, deps[taskID], nil)
		}
	}
	// The actor may have kept it blocked on its dependencies.
	if cur, err := s.Repo.GetTask(ctx, taskID); err == nil {
		return cur, nil
	}
	t.State = state
	return t, nil
}

// offUIStateGate applies SetTaskState's rules for callers off the web UI
// (see SetTaskState), returning whether the actor must re-check that the
// ticket is still in progress.
func (s *Service) offUIStateGate(ctx context.Context, p authz.Principal, t *domain.Task, state domain.TaskState, reason, dispatchID string) (bool, error) {
	requireStarted := false
	if !p.InUI() {
		// A ticket waiting on a person's answer keeps its question until it's
		// answered: off the UI, no state change may replace or clear it, so
		// the answer always leaves a "relayed by agent" record.
		if hasOpenQuestion(t) && (state != domain.TaskStateBlocked || reason != t.BlockReason) {
			switch state {
			case domain.TaskStateBacklog, domain.TaskStateBlocked:
				return false, authz.Forbidden("ticket %d is waiting on a question (%q); a state change doesn't answer or replace it. A person answers it "+
					"in the AMP web UI (%s), or relay their answer with amp_answer_question.", t.ID, t.BlockReason, s.TaskURL(t.ProjectID, t.ID))
			}
		}
		// A ticket waiting on its dependencies isn't made ready by hand:
		// it becomes ready when the last one completes (dispatch checks
		// again regardless). Moving work back from in progress or
		// completed stays allowed; the actor leaves it blocked on its
		// dependencies rather than ready.
		if state == domain.TaskStateBacklog && t.State != domain.TaskStateInProgress && t.State != domain.TaskStateCompleted {
			if err := s.openDepsGate(ctx, t, "made ready (backlog)"); err != nil {
				return false, err
			}
		}
		switch state {
		case domain.TaskStateInProgress:
			if t.State != domain.TaskStateInProgress {
				return false, authz.Forbidden("ticket %d: agents can't set a ticket in progress by hand; dispatch it with amp_dispatch_task "+
					"(it needs the plan approved by a person in the AMP web UI: %s)", t.ID, s.PlanURL(t.ProjectID))
			}
		case domain.TaskStateCompleted:
			if t.IsHuman() {
				return false, authz.RequireUI(p, fmt.Sprintf("Marking ticket %d (a person's ticket) done", t.ID), s.TaskURL(t.ProjectID, t.ID))
			}
			if err := s.checkAgentCompletion(ctx, p, t, dispatchID); err != nil {
				return false, err
			}
			requireStarted = t.State != domain.TaskStateCompleted
		}
	}
	return requireStarted, nil
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
	s.ReconcileMergeMetadata(ctx, p, t.ProjectID, req.Metadata)
	return nil
}
