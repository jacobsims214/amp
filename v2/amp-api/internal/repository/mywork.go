package repository

import (
	"context"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// WorkTask is a ticket with its project's code and name, for views that
// span projects.
type WorkTask struct {
	domain.Task
	ProjectCode string `json:"project_code"`
	ProjectName string `json:"project_name"`
	// LastActivityAt is the ticket's latest activity (state change, comment,
	// dispatch), so a ticket in progress that has gone silent can be spotted.
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
}

const workCols = taskCols + `,
	(SELECT code FROM projects p WHERE p.id = tasks.project_id),
	(SELECT name FROM projects p WHERE p.id = tasks.project_id),
	(SELECT MAX(created_at) FROM activity_log a WHERE a.task_id = tasks.id)`

const activeProject = `project_id IN (SELECT id FROM projects WHERE state <> 'archived')`

func (r *Repo) workTasks(ctx context.Context, where string, args ...any) ([]WorkTask, error) {
	rows, err := r.db.Query(ctx, `SELECT `+workCols+` FROM tasks WHERE `+activeProject+` AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkTask{}
	for rows.Next() {
		var w WorkTask
		t := &w.Task
		if err := rows.Scan(
			&t.ID, &t.ProjectID, &t.EpicID, &t.StoryID, &t.Name, &t.Description, &t.AcceptanceCriteria,
			&t.State, &t.Priority, &t.AssignedTo, &t.AgentID, &t.DispatchedAt, &t.CompletedAt, &t.BlockReason, &t.StartAt,
			&t.CreatedBy, &t.OwnerUserID, &t.Metadata, &t.CreatedAt, &t.UpdatedAt,
			&t.Performer, &t.HumanReason, &t.ReadyAt, &t.PlannedAt, &t.ApprovedAt, &t.ApprovedBy, &w.ProjectCode, &w.ProjectName, &w.LastActivityAt,
		); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// OpenTasksOwnedBy lists unfinished tickets owned by any of the users, most
// important first.
func (r *Repo) OpenTasksOwnedBy(ctx context.Context, userIDs []int, limit int) ([]WorkTask, error) {
	return r.workTasks(ctx, `owner_user_id = ANY($1) AND state <> 'completed'
		ORDER BY (state = 'in_progress') DESC, priority DESC, updated_at DESC LIMIT $2`, userIDs, limit)
}

// FlaggedTasks lists tickets an agent blocked with a reason (not a schedule),
// longest-waiting first, in the given projects (nil: every project). The
// projects filter in SQL so the limit applies to what the caller can see.
func (r *Repo) FlaggedTasks(ctx context.Context, projectIDs []int, limit int) ([]WorkTask, error) {
	return r.workTasks(ctx, `state = 'blocked' AND block_reason <> '' AND block_reason NOT LIKE 'scheduled:%'
		AND ($2::int[] IS NULL OR project_id = ANY($2))
		ORDER BY updated_at ASC LIMIT $1`, limit, projectIDs)
}

// AgentWork lists what the named agents (by agent_id, or by the user that
// owns the ticket) are working on now and finished since `since`.
func (r *Repo) AgentWork(ctx context.Context, agentNames []string, agentUserIDs []int, since time.Time) ([]WorkTask, error) {
	return r.workTasks(ctx, `(agent_id = ANY($1) OR owner_user_id = ANY($2))
		AND (state = 'in_progress' OR (state = 'completed' AND completed_at >= $3))
		ORDER BY COALESCE(completed_at, dispatched_at) DESC LIMIT 500`, agentNames, agentUserIDs, since)
}

// TaskWithMeta is a ticket and its effective metadata (project, then epic,
// then story, then the ticket's own; later wins).
type TaskWithMeta struct {
	domain.Task
	Effective domain.Metadata
}

// ProjectTasksWithMeta lists every ticket in a project with its effective
// metadata, for reports.
func (r *Repo) ProjectTasksWithMeta(ctx context.Context, projectID int) ([]TaskWithMeta, error) {
	rows, err := r.db.Query(ctx, `
		SELECT t.id, t.project_id, t.epic_id, t.story_id, t.name, t.state, t.priority, t.agent_id,
		       t.dispatched_at, t.completed_at, t.block_reason, t.created_at, t.updated_at, t.owner_user_id,
		       p.metadata || e.metadata || s.metadata || t.metadata,
		       t.performer, t.human_reason, t.ready_at,
		       COALESCE((SELECT array_agg(d.depends_on ORDER BY d.depends_on) FROM task_dependencies d WHERE d.task_id = t.id), '{}')
		  FROM tasks t
		  JOIN projects p ON p.id = t.project_id
		  JOIN epics e ON e.id = t.epic_id
		  JOIN stories s ON s.id = t.story_id
		 WHERE t.project_id = $1
		 ORDER BY t.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskWithMeta{}
	for rows.Next() {
		var x TaskWithMeta
		t := &x.Task
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.EpicID, &t.StoryID, &t.Name, &t.State, &t.Priority, &t.AgentID,
			&t.DispatchedAt, &t.CompletedAt, &t.BlockReason, &t.CreatedAt, &t.UpdatedAt, &t.OwnerUserID, &x.Effective,
			&t.Performer, &t.HumanReason, &t.ReadyAt, &t.DependencyIDs); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// LastActivity returns each ticket's latest sign of life: its newest
// activity-log entry (state changes, dispatches, comments) or its dispatch
// time, whichever is later.
func (r *Repo) LastActivity(ctx context.Context, taskIDs []int) (map[int]time.Time, error) {
	out := map[int]time.Time{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT t.id, GREATEST(t.dispatched_at, (SELECT MAX(a.created_at) FROM activity_log a WHERE a.task_id = t.id))
		FROM tasks t WHERE t.id = ANY($1)`, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var at *time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		if at != nil {
			out[id] = *at
		}
	}
	return out, rows.Err()
}
