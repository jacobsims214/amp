package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
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
//
// Only the worker's activity counts (AMP #2949). The manager that dispatched
// often comments on the ticket, or reports a run, right after dispatching;
// counting that as the start switched the watchdog off for a ticket nobody
// was working, which is the exact case it exists to catch. The manager and
// an old plugin's worker usually call as the same user (one person's token,
// one MCP connection), so the user alone can't tell them apart. The rule:
//
//   - an abandoned run report never starts a ticket: it records a run that
//     ended, the opposite of a worker starting one (a manager recording a
//     dead worker, AMP's own advice);
//   - activity from a user other than the dispatcher always counts, as
//     before;
//   - the dispatcher's own activity counts once DispatcherGrace has passed
//     since the dispatch, or straight away when a comment is signed as the
//     dispatched worker (author "amp-worker" for agent
//     "amp:amp-worker-backend": every old worker skill signs that way, a
//     manager doesn't).
//
// So a manager's note in the same turn as its dispatch leaves the ticket not
// started, and an old worker is started by its first signed comment, or by
// any later activity.

// DispatcherGrace is how long after a dispatch the dispatcher's own unsigned
// activity is taken as the manager's rather than its worker's.
var DispatcherGrace = 2 * time.Minute

// InferStart starts a dispatched ticket for a caller that doesn't claim: p
// is the caller, author a comment's author ("" for a run report). Any state
// other than in-progress, an already-started ticket, or activity that is the
// dispatcher's own (see above) is a no-op: it never refuses, because it is
// the path an older worker takes.
func (s *Service) InferStart(ctx context.Context, p authz.Principal, taskID int, author string) {
	t, err := s.Repo.GetTask(ctx, taskID)
	if err != nil || t.State != domain.TaskStateInProgress || t.StartedAt != nil {
		return
	}
	if !s.workerActivity(ctx, p, t, author) {
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

// workerActivity reports whether activity by p (a comment signed author, or
// a run report when author is "") on the dispatched, unstarted t counts as
// its worker starting it, by the rule above. When the dispatcher can't be
// read it counts: the old behaviour, never a stuck ticket.
func (s *Service) workerActivity(ctx context.Context, p authz.Principal, t *domain.Task, author string) bool {
	if SignedAsWorker(author, t.AgentID) {
		return true
	}
	if t.DispatchedAt == nil || time.Since(*t.DispatchedAt) >= DispatcherGrace {
		return true
	}
	by, ok, err := s.Repo.LastDispatcher(ctx, t.ID)
	if err != nil || !ok {
		return true
	}
	return !sameUser(by, p.UserRef())
}

// SignedAsWorker reports whether a comment's author names the agent a
// ticket was dispatched to: the agent id itself, its name without the plugin
// prefix ("amp-worker-backend" for "amp:amp-worker-backend"), or the family
// that name belongs to ("amp-worker"). The default author ("agent") and an
// empty one name nobody.
func SignedAsWorker(author, agentID string) bool {
	a := strings.ToLower(strings.TrimSpace(author))
	agent := strings.ToLower(strings.TrimSpace(agentID))
	if a == "" || a == "agent" || agent == "" {
		return false
	}
	name := agent
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[i+1:]
	}
	// The family match needs a hyphenated name, so a bare "amp" names no one.
	return a == agent || a == name || (strings.Contains(a, "-") && strings.HasPrefix(name, a+"-"))
}

func sameUser(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
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
