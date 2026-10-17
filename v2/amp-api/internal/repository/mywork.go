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
}

const workCols = taskCols + `,
	(SELECT code FROM projects p WHERE p.id = tasks.project_id),
	(SELECT name FROM projects p WHERE p.id = tasks.project_id)`

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
			&t.CreatedBy, &t.OwnerUserID, &t.Metadata, &t.CreatedAt, &t.UpdatedAt, &w.ProjectCode, &w.ProjectName,
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
// longest-waiting first.
func (r *Repo) FlaggedTasks(ctx context.Context, limit int) ([]WorkTask, error) {
	return r.workTasks(ctx, `state = 'blocked' AND block_reason <> '' AND block_reason NOT LIKE 'scheduled:%'
		ORDER BY updated_at ASC LIMIT $1`, limit)
}

// AgentWork lists what the named agents (by agent_id, or by the user that
// owns the ticket) are working on now and finished since `since`.
func (r *Repo) AgentWork(ctx context.Context, agentNames []string, agentUserIDs []int, since time.Time) ([]WorkTask, error) {
	return r.workTasks(ctx, `(agent_id = ANY($1) OR owner_user_id = ANY($2))
		AND (state = 'in_progress' OR (state = 'completed' AND completed_at >= $3))
		ORDER BY COALESCE(completed_at, dispatched_at) DESC LIMIT 500`, agentNames, agentUserIDs, since)
}
