package service

import (
	"context"
	"fmt"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
)

// The fallback start (AMP #2801, epic 180).
//
// A plugin that doesn't know about claims still has to work: its first
// activity on a dispatched ticket — a progress comment, a run report, a
// block, a completion — starts the ticket, recorded as started_by
// "inferred". Without it, a worker doing exactly what it always did would be
// flagged at 10 minutes and returned at 30 by the watchdog.
//
// It is deliberately silent: nothing is posted on the ticket (the worker's
// own comment already is the record) and nothing refuses. It only stamps
// started_at, which is what the watchdog reads.

// InferStart starts a dispatched ticket for a caller that doesn't claim.
// Any state other than in-progress, or an already-started ticket or project,
// is a no-op: it never refuses, because it is the path an older worker takes.
func (s *Service) InferStart(ctx context.Context, taskID int) {
	t, err := s.Repo.GetTask(ctx, taskID)
	if err != nil || t.State != domain.TaskStateInProgress || t.StartedAt != nil {
		return
	}
	// Bounded: the start is bookkeeping, and the caller's real work (its
	// comment, its run) must not wait long on it.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	replyCh := make(chan actor.ReplyStart, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgClaimStart{TaskID: taskID, Inferred: true, ReplyCh: replyCh}); err != nil {
		return
	}
	_ = actor.Await(cctx, replyCh)
}

// ReturnReasonFor names an automatic return, for the watchdog and for
// amp_dispatch_task's result.
func ReturnReasonFor(minutes int, agentID string) string {
	if agentID == "" {
		agentID = "an agent"
	}
	return fmt.Sprintf("not started within %d min of dispatch to %s", minutes, agentID)
}

// CloseoutNote is the run note a closeout records, so the run itself says it
// was a closing rather than the worker's completion.
func CloseoutNote(reason string) string {
	if reason == "" {
		reason = "closed out by its manager"
	}
	return "closeout: " + reason
}
