package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/domain"
)

// ---- Dependencies and moves ----

// DepInfo is what dependency checks need to know about a task.
type DepInfo struct {
	ID        int
	ProjectID int
	State     domain.TaskState
}

// DepInfos returns the tasks among ids that exist.
func (r *Repo) DepInfos(ctx context.Context, ids []int) (map[int]DepInfo, error) {
	out := map[int]DepInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT id, project_id, state FROM tasks WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d DepInfo
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.State); err != nil {
			return nil, err
		}
		out[d.ID] = d
	}
	return out, rows.Err()
}

// CompletedAmong returns which of ids are completed tasks, wherever they are
// in the project.
func (r *Repo) CompletedAmong(ctx context.Context, ids []int) (map[int]bool, error) {
	out := map[int]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT id FROM tasks WHERE id = ANY($1) AND state = 'completed'`, ids)
	if err != nil {
		return nil, err
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[int])
	for _, id := range done {
		out[id] = true
	}
	return out, err
}

// TaskDependencies returns the tasks a task depends on.
func (r *Repo) TaskDependencies(ctx context.Context, taskID int) ([]int, error) {
	deps, err := r.getTaskDeps(ctx, taskID)
	if deps == nil {
		deps = []int{}
	}
	return deps, err
}

// ProjectDependencies returns every dependency edge in a project:
// task → the tasks it depends on.
func (r *Repo) ProjectDependencies(ctx context.Context, projectID int) (map[int][]int, error) {
	return r.getTaskDepsForProject(ctx, projectID)
}

// Dependents returns the tasks that depend on taskID.
func (r *Repo) Dependents(ctx context.Context, taskIDs []int) ([]int, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, `SELECT DISTINCT task_id FROM task_dependencies WHERE depends_on = ANY($1)`, taskIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// ReplaceTaskDependencies sets exactly which tasks taskID depends on.
func (r *Repo) ReplaceTaskDependencies(ctx context.Context, taskID int, deps []int) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM task_dependencies WHERE task_id = $1`, taskID); err != nil {
		return err
	}
	for _, d := range deps {
		if _, err := tx.Exec(ctx, `INSERT INTO task_dependencies (task_id, depends_on) VALUES ($1, $2) ON CONFLICT DO NOTHING`, taskID, d); err != nil {
			return fmt.Errorf("add dependency %d→%d: %w", taskID, d, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET updated_at = NOW() WHERE id = $1`, taskID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MoveTask puts a task under another story (and that story's epic).
func (r *Repo) MoveTask(ctx context.Context, taskID, epicID, storyID int) error {
	_, err := r.db.Exec(ctx, `UPDATE tasks SET epic_id = $1, story_id = $2, updated_at = NOW() WHERE id = $3`, epicID, storyID, taskID)
	return err
}

// MoveStory puts a story, and its tasks, under another epic.
func (r *Repo) MoveStory(ctx context.Context, storyID, epicID int) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE stories SET epic_id = $1, updated_at = NOW() WHERE id = $2`, epicID, storyID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET epic_id = $1, updated_at = NOW() WHERE story_id = $2`, epicID, storyID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TaskIDsInStories returns the ids of the tasks in the given stories.
func (r *Repo) TaskIDsInStories(ctx context.Context, storyIDs []int) ([]int, error) {
	rows, err := r.db.Query(ctx, `SELECT id FROM tasks WHERE story_id = ANY($1)`, storyIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// TaskIDsInEpic returns the ids of the tasks in an epic.
func (r *Repo) TaskIDsInEpic(ctx context.Context, epicID int) ([]int, error) {
	rows, err := r.db.Query(ctx, `SELECT id FROM tasks WHERE epic_id = $1`, epicID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}
