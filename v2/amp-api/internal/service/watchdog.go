package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
)

// The dispatch watchdog (AMP #2801, epic 180).
//
// A dispatched ticket whose worker never starts it is invisible today: it
// sits in_progress exactly like one being worked. The watchdog makes it
// visible and self-healing:
//
//   - flag (derived, nothing to write): a ticket in_progress with no
//     started_at shows in amp_list_tasks' not_started list once it has been
//     dispatched longer than the project's not_started_flag_minutes (10),
//     and in its owner's waiting_on_people as kind "start".
//   - return: past not_started_return_minutes (30) the ticket goes back to
//     ready, its dispatch cleared, and it is marked returned with the reason
//     ("not started within 30 min of dispatch to amp-worker-x"), so the
//     manager sees it under amp_list_tasks' returned list and re-dispatches
//     knowingly.
//
// Both are per project and 0 turns them off. The sweep runs on a timer in
// amp-api, not in the project actors: a project whose actors aren't loaded
// here still has its orphans, and the scan is one indexed query. Only one
// replica sweeps at a time (a named advisory lock, #2598), and the state
// change itself goes through the project's actor, so in-memory state, SSE
// and history stay right wherever the project runs.

// Defaults for the watchdog's two timeouts, when a project says nothing.
const (
	DefaultNotStartedFlagMinutes   = 10
	DefaultNotStartedReturnMinutes = 30
)

// WatchdogInterval is how often the sweep runs.
const WatchdogInterval = time.Minute

// NotStartedWatchdog returns dispatched tickets to ready once they have
// waited longer than their project's return timeout. It runs until ctx is
// cancelled. locked reports false when another replica holds the sweep lock.
func (s *Service) RunDispatchWatchdog(ctx context.Context, interval time.Duration, lock func(context.Context, string) (func(), bool, error)) {
	if interval <= 0 {
		interval = WatchdogInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if n, err := s.SweepNotStarted(ctx, lock); err != nil && ctx.Err() == nil {
			slog.Warn("dispatch watchdog sweep failed", "err", err)
		} else if n > 0 {
			slog.Info("dispatch watchdog returned tickets nobody started", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// SweepNotStarted is one pass of the watchdog: it returns every dispatched
// ticket past its project's return timeout, and reports how many it did.
// Tickets inside the timeout are only flagged, which is a read and needs no
// sweep. lock serialises the pass across replicas; when it can't be taken
// this replica does nothing (another one is sweeping) and returns 0, nil.
func (s *Service) SweepNotStarted(ctx context.Context, lock func(context.Context, string) (func(), bool, error)) (int, error) {
	release, ok, err := lock(ctx, "dispatch-watchdog")
	if err != nil {
		return 0, fmt.Errorf("watchdog lock: %w", err)
	}
	if !ok {
		return 0, nil // another replica is sweeping
	}
	defer release()

	projects, err := s.Repo.ProjectsWithNotStartedTickets(ctx)
	if err != nil {
		return 0, fmt.Errorf("projects with unstarted tickets: %w", err)
	}
	returned := 0
	for _, projectID := range projects {
		if ctx.Err() != nil {
			return returned, ctx.Err()
		}
		proj, err := s.Repo.GetProject(ctx, projectID)
		if err != nil {
			return returned, fmt.Errorf("project %d: %w", projectID, err)
		}
		minutes := proj.NotStartedReturnMinutes
		if minutes <= 0 {
			continue // returning is off for this project
		}
		timeout := time.Duration(minutes) * time.Minute
		// Read the dues from the database: a ticket started between the
		// scan and this point is no longer in the list, and one that started
		// after the read is caught by the actor's own started check.
		due, err := s.Repo.NotStarted(ctx, projectID, timeout)
		if err != nil {
			return returned, fmt.Errorf("project %d: unstarted tickets: %w", projectID, err)
		}
		for _, n := range due {
			reason := fmt.Sprintf("not started within %d min of dispatch to %s", minutes, n.AgentID)
			if err := s.returnUnstarted(ctx, projectID, n.ID, n.DispatchID, reason); err != nil {
				if errors.Is(err, errDispatchMovedOn) {
					continue // claimed, or returned and dispatched again, since the read: not ours to return
				}
				slog.Warn("dispatch watchdog: return failed", "project_id", projectID, "task_id", n.ID, "err", err)
				continue
			}
			returned++
		}
	}
	return returned, nil
}

// errDispatchMovedOn is a watchdog return that didn't happen because the
// ticket's dispatch is no longer the one the sweep saw.
var errDispatchMovedOn = errors.New("dispatch moved on since the sweep read it")

// returnUnstarted returns one ticket through its project's actor, as the
// watchdog (a system action: no user asked for it). It returns only the
// dispatch the sweep observed (dispatchID, AMP #2949): a ticket returned and
// dispatched again in between, or claimed, is left alone.
func (s *Service) returnUnstarted(ctx context.Context, projectID, taskID int, dispatchID, reason string) error {
	replyCh := make(chan actor.ReplyGetTask, 1)
	if err := s.Registry.Send(projectID, &actor.MsgReturnTask{TaskID: taskID, Reason: reason, ByName: "system",
		IfDispatch: true, ExpectDispatchID: dispatchID, ReplyCh: replyCh}); err != nil {
		return err
	}
	reply := actor.Await(ctx, replyCh)
	if reply.Err != nil {
		if cur := reply.Task; cur != nil && (cur.DispatchID != dispatchID || cur.StartedAt != nil || cur.State != domain.TaskStateInProgress) {
			return fmt.Errorf("%w: %v", errDispatchMovedOn, reply.Err)
		}
		return reply.Err
	}
	s.RecordSystem(ctx, domain.AuditEntry{
		ProjectID: intRef(projectID), TargetType: "task", TargetID: idStr(taskID), TargetName: reply.Task.Name,
		Action:  "task.return",
		Summary: fmt.Sprintf("returned ticket %d to ready: %s", taskID, reason),
		Details: map[string]any{"reason": reason, "automatic": true, "agent_id": reply.Task.AgentID},
	})
	return nil
}
