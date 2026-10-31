package repository

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Dispatch integrity (AMP #2801, epic 180, story 444).
//
// A dispatched ticket isn't started until its worker claims it. The four
// task states are unchanged; "dispatched, not started" is in_progress with
// started_at NULL, so old plugins and clients see what they always saw.
// These are the durable pieces: the dispatch token, the start stamp, the
// return stamp, and the two lists amp_list_tasks reports (not started,
// returned).

// dispatchIDPrefix marks a dispatch token, so one is recognisable in a log
// or a prompt: d_3kz7…. Unguessable (16 random bytes): it is the only thing
// that distinguishes the worker that was dispatched from anyone else
// completing the ticket.
const dispatchIDPrefix = "d_"

var dispatchIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewDispatchID returns a fresh dispatch token.
func NewDispatchID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return dispatchIDPrefix + strings.ToLower(dispatchIDEncoding.EncodeToString(b[:])), nil
}

// SetTaskDispatch records a new dispatch: the token, who it went to, and
// when. It clears any earlier start and return, so the ticket reads as
// "dispatched, not started" until its worker claims it.
func (r *Repo) SetTaskDispatch(ctx context.Context, taskID int, agentID, dispatchID string, at time.Time) error {
	_, err := r.db.Exec(ctx,
		`UPDATE tasks SET agent_id = $1, dispatched_at = $2, dispatch_id = $3, updated_at = NOW(),
		        started_at = NULL, started_by = '', returned_at = NULL, returned_reason = ''
		 WHERE id = $4`,
		agentID, at, dispatchID, taskID,
	)
	return err
}

// StartTask stamps a ticket as started: when, and by which agent
// ("inferred" for a plugin whose first activity counts as the start).
// Re-starting returns the moment it was first started, so a repeat claim
// reports the same thing.
func (r *Repo) StartTask(ctx context.Context, taskID int, by string) (time.Time, error) {
	var at time.Time
	err := r.db.QueryRow(ctx,
		`UPDATE tasks SET started_at = NOW(), started_by = $1, updated_at = NOW()
		 WHERE id = $2 AND started_at IS NULL
		 RETURNING started_at`,
		by, taskID,
	).Scan(&at)
	if err == pgx.ErrNoRows {
		err = r.db.QueryRow(ctx, `SELECT started_at FROM tasks WHERE id = $1`, taskID).Scan(&at)
	}
	return at, err
}

// ReturnTask records that a dispatched ticket went back to ready without a
// worker finishing it: the reason, when, and no dispatch or start any more
// (a run holding the current dispatch may return its own started ticket). agent_id
// is kept as the record of who it was dispatched to (reported in the
// returned list; the next dispatch overwrites it). The state itself is set
// by the ticket's actor (backlog, or blocked on open deps).
func (r *Repo) ReturnTask(ctx context.Context, taskID int, reason string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE tasks SET returned_at = NOW(), returned_reason = $1, dispatch_id = '',
		        dispatched_at = NULL, started_at = NULL, started_by = '', updated_at = NOW()
		 WHERE id = $2`,
		reason, taskID,
	)
	return err
}

// ClearTaskDispatch ends a ticket's current dispatch in the database: the
// token is no longer current, so a reloaded actor can't revive a revoked one
// (AMP #2944). With release it also clears the start, for an owner releasing
// a ticket mid-flight. The actor makes the same change in memory in the same
// step.
func (r *Repo) ClearTaskDispatch(ctx context.Context, taskID int, release bool) error {
	q := `UPDATE tasks SET dispatch_id = '' WHERE id = $1 AND dispatch_id <> ''`
	if release {
		q = `UPDATE tasks SET dispatch_id = '', started_at = NULL, started_by = '', updated_at = NOW() WHERE id = $1`
	}
	_, err := r.db.Exec(ctx, q, taskID)
	return err
}

// ClearTaskReturn drops a ticket's return stamp once it is completed: done
// work isn't "returned" any more, whether it was closed out directly or
// completed by a person in the web UI without a re-dispatch (AMP #2947).
// The actor clears the same fields in memory in the same step.
func (r *Repo) ClearTaskReturn(ctx context.Context, taskID int) error {
	_, err := r.db.Exec(ctx,
		`UPDATE tasks SET returned_at = NULL, returned_reason = '' WHERE id = $1 AND returned_at IS NOT NULL`,
		taskID,
	)
	return err
}

// CountRuns is the number of runs recorded on a ticket: the closeout rule
// lets a never-started ticket be closed out only when it has prior work.
func (r *Repo) CountRuns(ctx context.Context, taskID int) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM task_runs WHERE task_id = $1`, taskID).Scan(&n)
	return n, err
}

// NotStartedTicket is an in-progress ticket no worker has started.
type NotStartedTicket struct {
	ID           int        `json:"id"`
	ProjectID    int        `json:"project_id"`
	Name         string     `json:"name"`
	AgentID      string     `json:"agent_id"`
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	// Minutes is how long it has been dispatched without a start.
	Minutes int `json:"minutes"`
}

// NotStarted lists a project's in-progress tickets with no started_at,
// dispatched at least minAge ago, oldest dispatch first. minAge 0 lists all
// of them (the watchdog's own scan).
func (r *Repo) NotStarted(ctx context.Context, projectID int, minAge time.Duration) ([]NotStartedTicket, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, project_id, name, COALESCE(agent_id, ''), dispatched_at,
		       GREATEST(0, FLOOR(EXTRACT(EPOCH FROM (NOW() - COALESCE(dispatched_at, updated_at))) / 60))::int
		  FROM tasks
		 WHERE project_id = $1 AND state = 'in_progress' AND started_at IS NULL
		   AND COALESCE(dispatched_at, updated_at) <= NOW() - $2::float8 * interval '1 second'
		 ORDER BY COALESCE(dispatched_at, updated_at), id`, projectID, minAge.Seconds())
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (NotStartedTicket, error) {
		var n NotStartedTicket
		err := row.Scan(&n.ID, &n.ProjectID, &n.Name, &n.AgentID, &n.DispatchedAt, &n.Minutes)
		return n, err
	})
	if out == nil {
		out = []NotStartedTicket{}
	}
	return out, err
}

// ProjectsWithNotStartedTickets lists the projects that have in-progress
// tickets with no start, for the watchdog's scan: a ticket nobody started is
// returned whether or not its project's actors happen to be loaded on this
// replica.
func (r *Repo) ProjectsWithNotStartedTickets(ctx context.Context) ([]int, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT project_id FROM tasks
		 WHERE state = 'in_progress' AND started_at IS NULL AND dispatched_at IS NOT NULL
		 ORDER BY project_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// ReturnedTicket is a ticket sent back to ready before any worker started
// it, and not dispatched again since.
type ReturnedTicket struct {
	ID             int        `json:"id"`
	ProjectID      int        `json:"project_id"`
	Name           string     `json:"name"`
	AgentID        string     `json:"agent_id"`
	ReturnedAt     *time.Time `json:"returned_at,omitempty"`
	ReturnedReason string     `json:"returned_reason"`
	// Minutes is how long ago it was returned.
	Minutes int `json:"minutes"`
}

// Returned lists a project's tickets that were returned and not dispatched
// again, most recent first. A completed ticket is never listed, even if its
// return stamp somehow survived (completion clears it, AMP #2947).
func (r *Repo) Returned(ctx context.Context, projectID int) ([]ReturnedTicket, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, project_id, name, COALESCE(agent_id, ''), returned_at, returned_reason,
		       GREATEST(0, FLOOR(EXTRACT(EPOCH FROM (NOW() - returned_at)) / 60))::int
		  FROM tasks
		 WHERE project_id = $1 AND returned_at IS NOT NULL AND state <> 'completed'
		 ORDER BY returned_at DESC, id`, projectID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReturnedTicket, error) {
		var t ReturnedTicket
		err := row.Scan(&t.ID, &t.ProjectID, &t.Name, &t.AgentID, &t.ReturnedAt, &t.ReturnedReason, &t.Minutes)
		return t, err
	})
	if out == nil {
		out = []ReturnedTicket{}
	}
	return out, err
}
