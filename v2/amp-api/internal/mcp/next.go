package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// The wording of every state-changing result's "next" (AMP #2801, epic 180).
//
// A tool result is read by a model on every call, which makes it the one
// place that steers the flow reliably: the dispatch result tells the manager
// to launch the worker now (not to stop after marking the ticket), the claim
// result tells the worker it now owns the ticket and must finish it itself,
// and a closeout says plainly that it wasn't the worker's completion. They
// are short, imperative, and worded for the caller the tool implies.

// dispatchNext is the dispatch result's next: it names the ticket, the
// agent, the token, and what happens if nobody claims it.
func dispatchNext(t *domain.Task, agentID string, returnAt *time.Time, returnMinutes int) string {
	next := fmt.Sprintf("Ticket #%d is dispatched to %s but no worker has started it yet. Start the worker now, in this same turn, "+
		"with worker_prompt as its prompt (the launch tool of your harness: Agent/Task in Claude Code, the task tool in opencode, "+
		"amp_run_ticket in the AMP harness). It stays \"not started\" until the worker calls amp_claim_task(task_id=%d, dispatch_id=%q).",
		t.ID, nameOr(agentID, "an agent"), t.ID, t.DispatchID)
	if returnAt != nil && returnMinutes > 0 {
		next += fmt.Sprintf(" If nobody claims it by %s (%d minutes), AMP returns it to ready by itself.",
			returnAt.UTC().Format(time.RFC3339), returnMinutes)
	}
	return next + fmt.Sprintf(" If you aren't starting the worker now, undo this with amp_set_task_state(task_id=%d, state=\"backlog\", reason=\"not started\").",
		t.ID)
}

// workerPrompt is a ready-to-use prompt for the worker subagent: everything
// it needs to claim the ticket and work it, without the manager retyping it.
func workerPrompt(t *domain.Task, agentID string) string {
	return fmt.Sprintf("Execute approved ticket #%d in project %d (dispatched to %s).\n\n"+
		"First call amp_claim_task(task_id=%d, dispatch_id=%q) to start it, then follow the amp-execution "+
		"protocol: read the ticket with amp_get_task(task_id=%d, include=[\"comments\",\"history\",\"runs\"]), "+
		"post your starting comment, do the work, and finish the ticket yourself with "+
		"amp_complete_task(task_id=%d, dispatch_id=%q, model=…, tokens…, branch=…, commit=…, pr_url=…) when every "+
		"acceptance criterion is met, or amp_block_task(task_id=%d, dispatch_id=%q, reason=<your question>) if you need a person. "+
		"Don't end your run without one of those two calls.",
		t.ID, t.ProjectID, agentID, t.ID, t.DispatchID, t.ID, t.ID, t.DispatchID, t.ID, t.DispatchID)
}

// claimNext is the claim result's next: the worker is now the ticket's, and
// must close it itself.
func claimNext(t *domain.Task) string {
	if t.StartedAt == nil {
		// Started before the claim existed (a plugin that doesn't claim):
		// the claim still took ownership, so the rest is the same.
		return fmt.Sprintf("You are the worker on #%d. Read it with amp_get_task(task_id=%d, include=[\"comments\"]), "+
			"post your \"Starting work\" comment, and do the work. Finish this ticket yourself: amp_complete_task(task_id=%d, ...) "+
			"when every acceptance criterion is met, or amp_block_task(task_id=%d, reason=<your question>) if you need a person. "+
			"Don't end your run without one of those two calls.", t.ID, t.ID, t.ID, t.ID)
	}
	return fmt.Sprintf("You are now the worker on #%d (started; dispatch %s). Read it with amp_get_task(task_id=%d, include=[\"comments\"]), "+
		"post your \"Starting work\" comment, and do the work. Finish this ticket yourself: "+
		"amp_complete_task(task_id=%d, dispatch_id=%q, with your model, tokens, branch, commit and pr_url) when every "+
		"acceptance criterion is met, or amp_block_task(task_id=%d, dispatch_id=%q, reason=<your question>) if you need a person. "+
		"Don't end your run without one of those two calls.",
		t.ID, t.DispatchID, t.ID, t.ID, t.DispatchID, t.ID, t.DispatchID)
}

// completeNext: what the caller does now that the ticket is done. A
// closeout says so plainly, so a manager doesn't read it as its worker's.
func completeNext(t *domain.Task, closeout bool) string {
	if closeout {
		return fmt.Sprintf("Closed out by you (not the worker), recorded as a closeout with your reason. "+
			"Don't start other tickets; report to your user that #%d is done.", t.ID)
	}
	return fmt.Sprintf("Done. Tell your manager the outcome of #%d; don't start other tickets.", t.ID)
}

// blockNext: a blocked ticket waits on a person.
func blockNext(t *domain.Task) string {
	return fmt.Sprintf("Stop now and end your final message with: QUESTION on task %d: <your question>. The ticket shows as a question "+
		"in AMP; when it's answered it goes back to ready and is dispatched again.", t.ID)
}

// returnNext: a returned ticket is ready again, for someone to dispatch.
func returnNext(t *domain.Task) string {
	return fmt.Sprintf("Ticket #%d is back in %s with its dispatch cleared: %s. Re-dispatch it when a worker can start it now "+
		"(amp_dispatch_task), or leave it for the user to decide.", t.ID, t.State, t.ReturnedReason)
}

// commentNext: a progress comment doesn't end a run, so it says what is left.
func commentNext(t *domain.Task) string {
	if t.State == domain.TaskStateInProgress {
		return fmt.Sprintf("Comment recorded. Keep working #%d: finish it with amp_complete_task or amp_block_task, "+
			"don't end your run without one of them.", t.ID)
	}
	return fmt.Sprintf("Comment recorded on #%d (now %s).", t.ID, t.State)
}

// reportRunNext: reporting a run doesn't change the ticket either.
func reportRunNext(t *domain.Task) string {
	return fmt.Sprintf("Run recorded on #%d (now %s). If you are its worker, the ticket still needs amp_complete_task or "+
		"amp_block_task when the work is done.", t.ID, t.State)
}

// stateNext describes the ticket after amp_set_task_state.
func stateNext(t *domain.Task) string {
	switch t.State {
	case domain.TaskStateCompleted:
		return fmt.Sprintf("Ticket #%d is completed. Reopen it with amp_set_task_state(state=\"backlog\") if that was a mistake.", t.ID)
	case domain.TaskStateBacklog:
		return fmt.Sprintf("Ticket #%d is ready. Dispatch it with amp_dispatch_task when a worker can start it now.", t.ID)
	case domain.TaskStateBlocked:
		return fmt.Sprintf("Ticket #%d is blocked: %s.", t.ID, nameOr(t.BlockReason, "waiting on its dependencies"))
	default:
		return fmt.Sprintf("Ticket #%d is %s.", t.ID, t.State)
	}
}

// answerNext is the answer result's next: the ticket is ready to dispatch.
func answerNext(t *domain.Task) string {
	if t.State == domain.TaskStateBlocked {
		return "It still waits on unfinished dependencies; it becomes ready when they're done."
	}
	return "It's ready: dispatch it again with amp_dispatch_task and launch the worker in the same turn; the worker will find the answer in the ticket's comments."
}

func nameOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// returnAt is when the watchdog returns a dispatch nobody claimed, or nil
// when returning is off for the project.
func returnAt(now time.Time, returnMinutes int) *time.Time {
	if returnMinutes <= 0 {
		return nil
	}
	at := now.Add(time.Duration(returnMinutes) * time.Minute)
	return &at
}

// watchdogMinutes is a project's two dispatch-watchdog timeouts (AMP #2801),
// with the defaults when the row can't be read: the settings are read on
// every dispatch and every board, and a lookup failure must not turn the
// watchdog off silently.
func (s *Server) watchdogMinutes(ctx context.Context, projectID int) (flag, ret int) {
	proj, err := s.repo.GetProject(ctx, projectID)
	if err != nil {
		return service.DefaultNotStartedFlagMinutes, service.DefaultNotStartedReturnMinutes
	}
	return proj.NotStartedFlagMinutes, proj.NotStartedReturnMinutes
}
