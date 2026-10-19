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
		`INSERT INTO tasks SELECT * FROM jsonb_populate_record(NULL::tasks, '{"performer": "agent", "human_reason": ""}'::jsonb || ($1::jsonb->'task'))`,
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

// ---- Deleted epics and stories ----

// Group kinds in the trash.
const (
	GroupEpic  = "epic"
	GroupStory = "story"
)

// TrashGroup snapshots an epic (with its stories) or a story before it is
// deleted: the rows, their owners, and which tickets go with it. The tickets
// themselves are trashed with TrashTasks.
func (r *Repo) TrashGroup(ctx context.Context, kind string, id int, taskIDs []int, deletedBy *int) error {
	var q string
	switch kind {
	case GroupEpic:
		q = `INSERT INTO deleted_groups (kind, item_id, project_id, epic_id, name, snapshot, task_ids, deleted_by)
		SELECT 'epic', e.id, e.project_id, e.id, e.name,
		       jsonb_build_object(
		         'epic', to_jsonb(e.*),
		         'epic_owners', COALESCE((SELECT jsonb_agg(to_jsonb(o.*)) FROM epic_owners o WHERE o.epic_id = e.id), '[]'::jsonb),
		         'stories', COALESCE((SELECT jsonb_agg(to_jsonb(s.*) ORDER BY s.id) FROM stories s WHERE s.epic_id = e.id), '[]'::jsonb),
		         'story_owners', COALESCE((SELECT jsonb_agg(to_jsonb(o.*)) FROM story_owners o JOIN stories s ON s.id = o.story_id WHERE s.epic_id = e.id), '[]'::jsonb)),
		       $2, $3
		  FROM epics e WHERE e.id = $1`
	case GroupStory:
		q = `INSERT INTO deleted_groups (kind, item_id, project_id, epic_id, name, snapshot, task_ids, deleted_by)
		SELECT 'story', s.id, s.project_id, s.epic_id, s.name,
		       jsonb_build_object(
		         'stories', jsonb_build_array(to_jsonb(s.*)),
		         'story_owners', COALESCE((SELECT jsonb_agg(to_jsonb(o.*)) FROM story_owners o WHERE o.story_id = s.id), '[]'::jsonb)),
		       $2, $3
		  FROM stories s WHERE s.id = $1`
	default:
		return fmt.Errorf("trash: unknown kind %q", kind)
	}
	if taskIDs == nil {
		taskIDs = []int{}
	}
	_, err := r.db.Exec(ctx, q+`
		ON CONFLICT (kind, item_id) DO UPDATE SET snapshot = EXCLUDED.snapshot, task_ids = EXCLUDED.task_ids,
		  deleted_by = EXCLUDED.deleted_by, deleted_at = NOW()`, id, taskIDs, deletedBy)
	return err
}

// DeletedGroup is an epic or story in the trash.
type DeletedGroup struct {
	Kind      string    `json:"kind"`
	ID        int       `json:"id"`
	ProjectID int       `json:"project_id"`
	EpicID    int       `json:"epic_id"`
	Name      string    `json:"name"`
	TaskIDs   []int     `json:"task_ids"`
	Stories   int       `json:"stories"`
	DeletedBy *int      `json:"deleted_by,omitempty"`
	DeletedAt time.Time `json:"deleted_at"`
	// EpicExists says whether a story can be restored where it was (an
	// epic can always be restored).
	EpicExists bool `json:"epic_exists"`
}

// ListDeletedGroups lists a project's deleted epics and stories, newest
// first (30 days).
func (r *Repo) ListDeletedGroups(ctx context.Context, projectID int) ([]DeletedGroup, error) {
	rows, err := r.db.Query(ctx, `SELECT d.kind, d.item_id, d.project_id, d.epic_id, d.name, d.task_ids,
		jsonb_array_length(d.snapshot->'stories'), d.deleted_by, d.deleted_at,
		d.kind = 'epic' OR EXISTS (SELECT 1 FROM epics e WHERE e.id = d.epic_id)
		FROM deleted_groups d WHERE d.project_id = $1 AND d.deleted_at > NOW() - INTERVAL '30 days'
		ORDER BY d.deleted_at DESC, d.kind, d.item_id`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DeletedGroup, error) {
		var d DeletedGroup
		err := row.Scan(&d.Kind, &d.ID, &d.ProjectID, &d.EpicID, &d.Name, &d.TaskIDs, &d.Stories, &d.DeletedBy, &d.DeletedAt, &d.EpicExists)
		return d, err
	})
}

// RestoredGroup is what RestoreGroup put back.
type RestoredGroup struct {
	Kind     string
	ID       int
	EpicID   int
	StoryIDs []int
	// TaskIDs are the tickets deleted with it, for the caller to restore.
	TaskIDs []int
}

// RestoreGroup puts a trashed epic (with its stories) or story back, with
// the same IDs and owners. Its tickets are left in the trash for the caller
// to restore after the actors know about the epic or story.
func (r *Repo) RestoreGroup(ctx context.Context, projectID int, kind string, id int) (*RestoredGroup, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	g := &RestoredGroup{Kind: kind, ID: id}
	var snap []byte
	err = tx.QueryRow(ctx, `SELECT epic_id, task_ids, snapshot FROM deleted_groups WHERE kind = $1 AND item_id = $2 AND project_id = $3 FOR UPDATE`,
		kind, id, projectID).Scan(&g.EpicID, &g.TaskIDs, &snap)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("%s %d is not in this project's trash", kind, id)
	}
	if err != nil {
		return nil, err
	}
	if kind == GroupStory {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM epics WHERE id = $1)`, g.EpicID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("story %d's epic #%d was deleted too; restore the epic first", id, g.EpicID)
		}
	}
	qs := []string{}
	if kind == GroupEpic {
		qs = append(qs,
			`INSERT INTO epics SELECT * FROM jsonb_populate_record(NULL::epics, $1::jsonb->'epic')`,
			`INSERT INTO epic_owners SELECT * FROM jsonb_populate_recordset(NULL::epic_owners, $1::jsonb->'epic_owners')
			 WHERE user_id IN (SELECT id FROM users) ON CONFLICT DO NOTHING`)
	}
	qs = append(qs,
		`INSERT INTO stories SELECT * FROM jsonb_populate_recordset(NULL::stories, $1::jsonb->'stories') ON CONFLICT DO NOTHING`,
		`INSERT INTO story_owners SELECT * FROM jsonb_populate_recordset(NULL::story_owners, $1::jsonb->'story_owners')
		 WHERE user_id IN (SELECT id FROM users) ON CONFLICT DO NOTHING`)
	for _, q := range qs {
		if _, err := tx.Exec(ctx, q, snap); err != nil {
			return nil, fmt.Errorf("restore %s %d: %w", kind, id, err)
		}
	}
	var s struct {
		Stories []struct {
			ID int `json:"id"`
		} `json:"stories"`
	}
	if err := json.Unmarshal(snap, &s); err != nil {
		return nil, err
	}
	for _, st := range s.Stories {
		g.StoryIDs = append(g.StoryIDs, st.ID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM deleted_groups WHERE kind = $1 AND item_id = $2`, kind, id); err != nil {
		return nil, err
	}
	return g, tx.Commit(ctx)
}
