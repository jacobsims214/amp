package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TrashTasks snapshots tickets into deleted_tasks before they are deleted:
// the row, comments, activity, and dependency links in both directions.
func (r *Repo) TrashTasks(ctx context.Context, ids []int, deletedBy *int) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO deleted_tasks (task_id, project_id, epic_id, story_id, name, snapshot, deleted_by)
		SELECT t.id, t.project_id, t.epic_id, t.story_id, t.name,
		       jsonb_build_object(
		         'task', to_jsonb(t.*),
		         'comments', COALESCE((SELECT jsonb_agg(to_jsonb(c.*)) FROM comments c WHERE c.task_id = t.id), '[]'::jsonb),
		         'activity', COALESCE((SELECT jsonb_agg(to_jsonb(a.*)) FROM activity_log a WHERE a.task_id = t.id), '[]'::jsonb),
		         'depends_on', COALESCE((SELECT jsonb_agg(d.depends_on) FROM task_dependencies d WHERE d.task_id = t.id), '[]'::jsonb),
		         'dependents', COALESCE((SELECT jsonb_agg(d.task_id) FROM task_dependencies d WHERE d.depends_on = t.id), '[]'::jsonb)),
		       $2
		  FROM tasks t WHERE t.id = ANY($1)
		ON CONFLICT (task_id) DO UPDATE SET snapshot = EXCLUDED.snapshot, deleted_by = EXCLUDED.deleted_by, deleted_at = NOW()`,
		ids, deletedBy)
	return err
}

// DeletedTask is a ticket in the trash.
type DeletedTask struct {
	TaskID    int       `json:"task_id"`
	ProjectID int       `json:"project_id"`
	EpicID    int       `json:"epic_id"`
	StoryID   int       `json:"story_id"`
	Name      string    `json:"name"`
	DeletedBy *int      `json:"deleted_by,omitempty"`
	DeletedAt time.Time `json:"deleted_at"`
	// StoryExists says whether it can be restored where it was.
	StoryExists bool `json:"story_exists"`
}

// ListDeletedTasks lists a project's trash, newest first (30 days).
func (r *Repo) ListDeletedTasks(ctx context.Context, projectID int) ([]DeletedTask, error) {
	rows, err := r.db.Query(ctx, `SELECT d.task_id, d.project_id, d.epic_id, d.story_id, d.name, d.deleted_by, d.deleted_at,
		EXISTS (SELECT 1 FROM stories s WHERE s.id = d.story_id)
		FROM deleted_tasks d WHERE d.project_id = $1 AND d.deleted_at > NOW() - INTERVAL '30 days'
		ORDER BY d.deleted_at DESC, d.task_id`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DeletedTask, error) {
		var d DeletedTask
		err := row.Scan(&d.TaskID, &d.ProjectID, &d.EpicID, &d.StoryID, &d.Name, &d.DeletedBy, &d.DeletedAt, &d.StoryExists)
		return d, err
	})
}

// RestoredTask is what RestoreTask put back.
type RestoredTask struct {
	TaskID     int
	ProjectID  int
	EpicID     int
	StoryID    int
	Dependents []int // tickets that depended on it and still exist
	// MissingDeps are tickets it depended on that don't exist right now
	// (perhaps in the trash too, coming back in the same restore).
	MissingDeps []int
}

// RestoreTask puts a trashed ticket back with its comments, activity, and
// the dependencies it had on tickets that still exist. Links from tickets
// that depended on it are returned for the caller to re-add (their actors
// must learn about them).
func (r *Repo) RestoreTask(ctx context.Context, projectID, taskID int) (*RestoredTask, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var epicID, storyID int
	var snap []byte
	err = tx.QueryRow(ctx, `SELECT epic_id, story_id, snapshot FROM deleted_tasks WHERE task_id = $1 AND project_id = $2 FOR UPDATE`,
		taskID, projectID).Scan(&epicID, &storyID, &snap)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("ticket %d is not in this project's trash", taskID)
	}
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stories WHERE id = $1)`, storyID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("ticket %d's story #%d was deleted too; recreate the story first", taskID, storyID)
	}
	for _, q := range []string{
		`INSERT INTO tasks SELECT * FROM jsonb_populate_record(NULL::tasks, $1::jsonb->'task')`,
		`INSERT INTO comments SELECT * FROM jsonb_populate_recordset(NULL::comments, $1::jsonb->'comments') ON CONFLICT DO NOTHING`,
		`INSERT INTO activity_log SELECT * FROM jsonb_populate_recordset(NULL::activity_log, $1::jsonb->'activity') ON CONFLICT DO NOTHING`,
	} {
		if _, err := tx.Exec(ctx, q, snap); err != nil {
			return nil, fmt.Errorf("restore ticket %d: %w", taskID, err)
		}
	}
	var s struct {
		DependsOn  []int `json:"depends_on"`
		Dependents []int `json:"dependents"`
	}
	if err := json.Unmarshal(snap, &s); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_dependencies (task_id, depends_on)
		SELECT $1, d FROM unnest($2::int[]) d WHERE EXISTS (SELECT 1 FROM tasks WHERE id = d) ON CONFLICT DO NOTHING`, taskID, s.DependsOn); err != nil {
		return nil, err
	}
	var missing []int
	if len(s.DependsOn) > 0 {
		rows, err := tx.Query(ctx, `SELECT d FROM unnest($1::int[]) d WHERE NOT EXISTS (SELECT 1 FROM tasks WHERE id = d)`, s.DependsOn)
		if err != nil {
			return nil, err
		}
		if missing, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
			return nil, err
		}
	}
	var dependents []int
	if len(s.Dependents) > 0 {
		rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE id = ANY($1)`, s.Dependents)
		if err != nil {
			return nil, err
		}
		if dependents, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM deleted_tasks WHERE task_id = $1`, taskID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &RestoredTask{TaskID: taskID, ProjectID: projectID, EpicID: epicID, StoryID: storyID, Dependents: dependents, MissingDeps: missing}, nil
}
