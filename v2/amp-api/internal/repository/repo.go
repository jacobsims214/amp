package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/simstech/amp-api/internal/domain"
)

// Repo is a thin wrapper around pgxpool.
// No ORM — just plain SQL. Fast to understand, easy to debug.
type Repo struct {
	db *pgxpool.Pool
}

func New(ctx context.Context, dsn string) (*Repo, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Repo{db: pool}, nil
}

// Migration is one versioned schema change, applied at most once.
type Migration struct {
	Version string // e.g. "004_orgs_teams" — ordering and identity
	SQL     string
}

// migrationLockKey is the pg_advisory_lock key serialising RunMigrations
// across processes (replicas starting together, parallel test packages).
const migrationLockKey = 0x616d70 // "amp"

// RunMigrations applies, in order, each migration not yet recorded in
// schema_migrations, each in its own transaction.
//
// Before this existed, every migration file was re-run on every startup,
// which only works for idempotent DDL. Migrations 001–003 are written that
// way, so on an existing database they simply run once more and get
// recorded; from then on everything (including one-time backfills) runs
// exactly once.
func (r *Repo) RunMigrations(ctx context.Context, migrations []Migration) error {
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.Version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", m.Version, err)
		}
		if applied {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return fmt.Errorf("migration %s: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.Version); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return fmt.Errorf("record migration %s: %w", m.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.Version, err)
		}
	}
	return nil
}

func (r *Repo) Close() { r.db.Close() }

// ---- Column lists and scanners ----
// Every read of a project/epic/story/task goes through these so new
// columns can't be forgotten in one of the many queries.

type rowScanner interface{ Scan(dest ...any) error }

const projectCols = `id, name, code, description, state, org_id, created_by, open_reassign, created_at, updated_at`

func scanProject(row rowScanner, p *domain.Project) error {
	return row.Scan(&p.ID, &p.Name, &p.Code, &p.Description, &p.State, &p.OrgID, &p.CreatedBy, &p.OpenReassign, &p.CreatedAt, &p.UpdatedAt)
}

const epicCols = `e.id, e.project_id, e.name, e.description, e.state, e.priority, e.created_by,
	COALESCE((SELECT array_agg(o.user_id ORDER BY o.user_id) FROM epic_owners o WHERE o.epic_id = e.id), '{}'),
	e.created_at, e.updated_at`

func scanEpic(row rowScanner, e *domain.Epic) error {
	return row.Scan(&e.ID, &e.ProjectID, &e.Name, &e.Description, &e.State, &e.Priority, &e.CreatedBy, &e.OwnerIDs, &e.CreatedAt, &e.UpdatedAt)
}

const storyCols = `s.id, s.project_id, s.epic_id, s.name, s.description, s.acceptance_criteria, s.state, s.priority, s.created_by,
	COALESCE((SELECT array_agg(o.user_id ORDER BY o.user_id) FROM story_owners o WHERE o.story_id = s.id), '{}'),
	s.created_at, s.updated_at`

func scanStory(row rowScanner, st *domain.Story) error {
	return row.Scan(&st.ID, &st.ProjectID, &st.EpicID, &st.Name, &st.Description, &st.AcceptanceCriteria, &st.State, &st.Priority, &st.CreatedBy, &st.OwnerIDs, &st.CreatedAt, &st.UpdatedAt)
}

const taskCols = `id, project_id, epic_id, story_id, name, description, acceptance_criteria, state, priority,
	assigned_to, agent_id, dispatched_at, completed_at, block_reason, start_at, created_by, owner_user_id, created_at, updated_at`

func scanTask(row rowScanner, t *domain.Task) error {
	return row.Scan(
		&t.ID, &t.ProjectID, &t.EpicID, &t.StoryID,
		&t.Name, &t.Description, &t.AcceptanceCriteria,
		&t.State, &t.Priority,
		&t.AssignedTo, &t.AgentID, &t.DispatchedAt, &t.CompletedAt, &t.BlockReason, &t.StartAt,
		&t.CreatedBy, &t.OwnerUserID,
		&t.CreatedAt, &t.UpdatedAt,
	)
}

func collect[T any](rows pgx.Rows, scan func(rowScanner, *T) error) ([]T, error) {
	defer rows.Close()
	out := make([]T, 0)
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- Projects ----

// CreateProject inserts the project and, when req.TeamID is set, links that
// team as its owner in the same transaction.
func (r *Repo) CreateProject(ctx context.Context, req domain.CreateProjectRequest) (*domain.Project, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	p := &domain.Project{}
	err = scanProject(tx.QueryRow(ctx,
		`INSERT INTO projects (name, code, description, org_id, created_by)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+projectCols,
		req.Name, req.Code, req.Description, req.OrgID, req.CreatedBy,
	), p)
	if isUniqueViolation(err) {
		return nil, conflictf("project code %q already exists", req.Code)
	}
	if err != nil {
		return nil, err
	}
	if req.TeamID != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'owner')`, p.ID, *req.TeamID); err != nil {
			return nil, err
		}
	}
	return p, tx.Commit(ctx)
}

func (r *Repo) GetProject(ctx context.Context, id int) (*domain.Project, error) {
	p := &domain.Project{}
	err := scanProject(r.db.QueryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE id = $1`, id), p)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("project %d not found", id)
	}
	return p, err
}

func (r *Repo) ListProjects(ctx context.Context) ([]domain.Project, error) {
	rows, err := r.db.Query(ctx, `SELECT `+projectCols+` FROM projects WHERE state = 'active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanProject)
}

func (r *Repo) ArchiveProject(ctx context.Context, id int) (*domain.Project, error) {
	return r.setProjectState(ctx, id, domain.ProjectStateArchived)
}

func (r *Repo) RestoreProject(ctx context.Context, id int) (*domain.Project, error) {
	return r.setProjectState(ctx, id, domain.ProjectStateActive)
}

// SetProjectOpenReassign turns the project's open-reassign setting on or off.
func (r *Repo) SetProjectOpenReassign(ctx context.Context, id int, on bool) (*domain.Project, error) {
	return r.UpdateProject(ctx, id, domain.ProjectSettings{OpenReassign: &on})
}

// UpdateProject changes the settings that are set (nil fields are kept).
func (r *Repo) UpdateProject(ctx context.Context, id int, u domain.ProjectSettings) (*domain.Project, error) {
	p := &domain.Project{}
	err := scanProject(r.db.QueryRow(ctx,
		`UPDATE projects SET
		   name = COALESCE($2, name),
		   description = COALESCE($3, description),
		   open_reassign = COALESCE($4, open_reassign),
		   updated_at = NOW()
		 WHERE id = $1 RETURNING `+projectCols,
		id, u.Name, u.Description, u.OpenReassign,
	), p)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("project %d not found", id)
	}
	return p, err
}

func (r *Repo) setProjectState(ctx context.Context, id int, state domain.ProjectState) (*domain.Project, error) {
	p := &domain.Project{}
	err := scanProject(r.db.QueryRow(ctx,
		`UPDATE projects SET state = $1, updated_at = NOW() WHERE id = $2 RETURNING `+projectCols,
		state, id,
	), p)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("project %d not found", id)
	}
	return p, err
}

func (r *Repo) ListArchivedProjects(ctx context.Context) ([]domain.Project, error) {
	rows, err := r.db.Query(ctx, `SELECT `+projectCols+` FROM projects WHERE state = 'archived' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanProject)
}

// ---- Epics ----

// CreateEpic inserts the epic and, when it has a creator, makes them its
// first owner.
func (r *Repo) CreateEpic(ctx context.Context, req domain.CreateEpicRequest) (*domain.Epic, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id int
	if err := tx.QueryRow(ctx,
		`INSERT INTO epics (project_id, name, description, priority, created_by)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		req.ProjectID, req.Name, req.Description, req.Priority, req.CreatedBy,
	).Scan(&id); err != nil {
		return nil, err
	}
	if req.CreatedBy != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO epic_owners (epic_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, *req.CreatedBy); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.GetEpic(ctx, id)
}

func (r *Repo) GetEpic(ctx context.Context, id int) (*domain.Epic, error) {
	e := &domain.Epic{}
	err := scanEpic(r.db.QueryRow(ctx, `SELECT `+epicCols+` FROM epics e WHERE e.id = $1`, id), e)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("epic %d not found", id)
	}
	return e, err
}

func (r *Repo) ListEpics(ctx context.Context, projectID int) ([]domain.Epic, error) {
	rows, err := r.db.Query(ctx, `SELECT `+epicCols+` FROM epics e WHERE e.project_id = $1 ORDER BY e.id`, projectID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanEpic)
}

// ---- Stories ----

// CreateStory inserts the story and makes its creator the first owner.
func (r *Repo) CreateStory(ctx context.Context, req domain.CreateStoryRequest) (*domain.Story, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id int
	if err := tx.QueryRow(ctx,
		`INSERT INTO stories (project_id, epic_id, name, description, acceptance_criteria, priority, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		req.ProjectID, req.EpicID, req.Name, req.Description, req.AcceptanceCriteria, req.Priority, req.CreatedBy,
	).Scan(&id); err != nil {
		return nil, err
	}
	if req.CreatedBy != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO story_owners (story_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, *req.CreatedBy); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.GetStory(ctx, id)
}

func (r *Repo) GetStory(ctx context.Context, id int) (*domain.Story, error) {
	st := &domain.Story{}
	err := scanStory(r.db.QueryRow(ctx, `SELECT `+storyCols+` FROM stories s WHERE s.id = $1`, id), st)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("story %d not found", id)
	}
	return st, err
}

func (r *Repo) ListStories(ctx context.Context, epicID int) ([]domain.Story, error) {
	rows, err := r.db.Query(ctx, `SELECT `+storyCols+` FROM stories s WHERE s.epic_id = $1 ORDER BY s.id`, epicID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanStory)
}

// ---- Tasks ----

func (r *Repo) CreateTask(ctx context.Context, req domain.CreateTaskRequest) (*domain.Task, error) {
	t := &domain.Task{}
	err := scanTask(r.db.QueryRow(ctx,
		`INSERT INTO tasks (project_id, epic_id, story_id, name, description, acceptance_criteria, priority, assigned_to, start_at, created_by, owner_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 RETURNING `+taskCols,
		req.ProjectID, req.EpicID, req.StoryID, req.Name, req.Description, req.AcceptanceCriteria, req.Priority, req.AssignedTo, req.StartAt,
		req.CreatedBy, req.OwnerUserID,
	), t)
	if err != nil {
		return nil, err
	}

	// Store dependencies.
	for _, depID := range req.DependencyIDs {
		if _, err := r.db.Exec(ctx,
			`INSERT INTO task_dependencies (task_id, depends_on) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			t.ID, depID,
		); err != nil {
			return nil, fmt.Errorf("insert dependency %d→%d: %w", t.ID, depID, err)
		}
	}
	t.DependencyIDs = req.DependencyIDs

	return t, nil
}

func (r *Repo) GetTask(ctx context.Context, id int) (*domain.Task, error) {
	t := &domain.Task{}
	err := scanTask(r.db.QueryRow(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = $1`, id), t)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("task %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	t.DependencyIDs, err = r.getTaskDeps(ctx, id)
	return t, err
}

func (r *Repo) ListTasks(ctx context.Context, projectID int, state string) ([]domain.Task, error) {
	query := `SELECT ` + taskCols + ` FROM tasks WHERE project_id = $1`
	args := []interface{}{projectID}
	if state != "" {
		query += ` AND state = $2`
		args = append(args, state)
	}
	query += ` ORDER BY id`

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out, err := collect(rows, scanTask)
	if err != nil {
		return nil, err
	}

	// Load deps for all tasks in one query.
	if len(out) > 0 {
		depMap, err := r.getTaskDepsForProject(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].DependencyIDs = depMap[out[i].ID]
		}
	}
	return out, nil
}

// ListTasksByStory returns all tasks for a story. Used by StoryActor on startup.
func (r *Repo) ListTasksByStory(ctx context.Context, storyID int) ([]domain.Task, error) {
	rows, err := r.db.Query(ctx, `SELECT `+taskCols+` FROM tasks WHERE story_id = $1 ORDER BY id`, storyID)
	if err != nil {
		return nil, err
	}
	out, err := collect(rows, scanTask)
	if err != nil {
		return nil, err
	}
	for i := range out {
		deps, err := r.getTaskDeps(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].DependencyIDs = deps
	}
	return out, nil
}

// ListTaskIDsByEpic returns a map[task_id]story_id for all tasks in an epic.
// Used by EpicActor on startup to build its task routing index.
func (r *Repo) ListTaskIDsByEpic(ctx context.Context, epicID int) (map[int]int, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, story_id FROM tasks WHERE epic_id = $1`, epicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]int)
	for rows.Next() {
		var taskID, storyID int
		if err := rows.Scan(&taskID, &storyID); err != nil {
			return nil, err
		}
		out[taskID] = storyID
	}
	return out, rows.Err()
}

// ListTaskIDsByProject returns a map[task_id]epic_id for all tasks in a project.
// Used by ProjectActor on startup to build its routing index cheaply.
func (r *Repo) ListTaskIDsByProject(ctx context.Context, projectID int) (map[int]int, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, epic_id FROM tasks WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]int)
	for rows.Next() {
		var taskID, epicID int
		if err := rows.Scan(&taskID, &epicID); err != nil {
			return nil, err
		}
		out[taskID] = epicID
	}
	return out, rows.Err()
}

// SetEpicState updates the state of an epic. Called by EpicActor on rollup.
func (r *Repo) SetEpicState(ctx context.Context, epicID int, state domain.EpicState) error {
	_, err := r.db.Exec(ctx,
		`UPDATE epics SET state = $1, updated_at = NOW() WHERE id = $2`,
		state, epicID,
	)
	return err
}

// SetStoryState updates the state of a story. Called by StoryActor on rollup.
func (r *Repo) SetStoryState(ctx context.Context, storyID int, state domain.StoryState) error {
	_, err := r.db.Exec(ctx,
		`UPDATE stories SET state = $1, updated_at = NOW() WHERE id = $2`,
		state, storyID,
	)
	return err
}

// ResetProject deletes all epics, stories, and tasks for a project but keeps
// the project record itself. Useful for replanning from scratch.
// Cascades: epics → stories → tasks (via FK ON DELETE CASCADE).
func (r *Repo) ResetProject(ctx context.Context, projectID int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM epics WHERE project_id = $1`, projectID)
	return err
}

// DeleteTask removes a single task and its dependencies/comments/activity log.
func (r *Repo) DeleteTask(ctx context.Context, taskID int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM tasks WHERE id = $1`, taskID)
	return err
}

// DeleteEpic removes an epic and cascades to all its stories and tasks.
func (r *Repo) DeleteEpic(ctx context.Context, epicID int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM epics WHERE id = $1`, epicID)
	return err
}

// ListStoriesByProject returns all stories for a project across all epics.
func (r *Repo) ListStoriesByProject(ctx context.Context, projectID int) ([]domain.Story, error) {
	rows, err := r.db.Query(ctx, `SELECT `+storyCols+` FROM stories s WHERE s.project_id = $1 ORDER BY s.epic_id, s.id`, projectID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanStory)
}

// SetTaskState records a task's state. completed_at is stamped when the
// task completes (kept if it already was) and cleared when it leaves
// completed, so reports read it from the database, not actor memory.
func (r *Repo) SetTaskState(ctx context.Context, taskID int, state domain.TaskState, blockReason string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE tasks SET state = $1, block_reason = $2, updated_at = NOW(),
		   completed_at = CASE WHEN $1 = 'completed' THEN COALESCE(completed_at, NOW()) END
		 WHERE id = $3`,
		state, blockReason, taskID,
	)
	return err
}

func (r *Repo) SetTaskAgent(ctx context.Context, taskID int, agentID string, dispatchedAt *time.Time) error {
	_, err := r.db.Exec(ctx,
		`UPDATE tasks SET agent_id = $1, dispatched_at = $2, updated_at = NOW() WHERE id = $3`,
		agentID, dispatchedAt, taskID,
	)
	return err
}

// SetTaskOwner sets (or, with nil, clears) a ticket's single owner.
func (r *Repo) SetTaskOwner(ctx context.Context, taskID int, ownerUserID *int) error {
	_, err := r.db.Exec(ctx,
		`UPDATE tasks SET owner_user_id = $1, updated_at = NOW() WHERE id = $2`,
		ownerUserID, taskID,
	)
	return err
}

func (r *Repo) SetTaskStartAt(ctx context.Context, taskID int, startAt *time.Time) error {
	_, err := r.db.Exec(ctx,
		`UPDATE tasks SET start_at = $1, updated_at = NOW() WHERE id = $2`,
		startAt, taskID,
	)
	return err
}

func (r *Repo) UpdateTask(ctx context.Context, req domain.UpdateTaskRequest) error {
	query := `UPDATE tasks SET
	   name                = CASE WHEN $1 != '' THEN $1 ELSE name END,
	   description         = CASE WHEN $2 != '' THEN $2 ELSE description END,
	   acceptance_criteria = CASE WHEN $3 != '' THEN $3 ELSE acceptance_criteria END,
	   assigned_to         = CASE WHEN $4 != '' THEN $4 ELSE assigned_to END,
	   agent_id            = CASE WHEN $5 != '' THEN $5 ELSE agent_id END,
	   priority            = CASE WHEN $6 != '' THEN $6 ELSE priority END,
	   updated_at          = NOW()`

	args := []interface{}{req.Name, req.Description, req.AcceptanceCriteria, req.AssignedTo, req.AgentID, req.Priority}

	if req.StartAt != nil {
		query += `, start_at = $7`
		args = append(args, req.StartAt)
	}

	query += ` WHERE id = $` + fmt.Sprint(len(args)+1)
	args = append(args, req.TaskID)

	_, err := r.db.Exec(ctx, query, args...)
	return err
}

// UpdateEpic updates an epic's mutable fields. Empty string = keep existing value.
func (r *Repo) UpdateEpic(ctx context.Context, epicID int, name, description, priority string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE epics SET
		   name        = CASE WHEN $1 != '' THEN $1 ELSE name END,
		   description = CASE WHEN $2 != '' THEN $2 ELSE description END,
		   priority    = CASE WHEN $3 != '' THEN $3 ELSE priority END,
		   updated_at  = NOW()
		 WHERE id = $4`,
		name, description, priority, epicID,
	)
	return err
}

// UpdateStory updates a story's mutable fields. Empty string = keep existing value.
func (r *Repo) UpdateStory(ctx context.Context, storyID int, name, description, acceptanceCriteria, priority string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE stories SET
		   name                = CASE WHEN $1 != '' THEN $1 ELSE name END,
		   description         = CASE WHEN $2 != '' THEN $2 ELSE description END,
		   acceptance_criteria = CASE WHEN $3 != '' THEN $3 ELSE acceptance_criteria END,
		   priority            = CASE WHEN $4 != '' THEN $4 ELSE priority END,
		   updated_at          = NOW()
		 WHERE id = $5`,
		name, description, acceptanceCriteria, priority, storyID,
	)
	return err
}

// DeleteStory removes a story. Tasks cascade via FK ON DELETE CASCADE.
func (r *Repo) DeleteStory(ctx context.Context, storyID int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM stories WHERE id = $1`, storyID)
	return err
}

// ---- Comments ----

func (r *Repo) AddComment(ctx context.Context, req domain.AddCommentRequest) (*domain.Comment, error) {
	c := &domain.Comment{}
	author := req.Author
	if author == "" {
		author = "agent"
	}
	err := r.db.QueryRow(ctx,
		`INSERT INTO comments (task_id, body, author, author_user_id) VALUES ($1, $2, $3, $4)
		 RETURNING id, task_id, body, author, author_user_id, created_at`,
		req.TaskID, req.Body, author, req.AuthorUserID,
	).Scan(&c.ID, &c.TaskID, &c.Body, &c.Author, &c.AuthorUserID, &c.CreatedAt)
	return c, err
}

func (r *Repo) GetComments(ctx context.Context, taskID int) ([]domain.Comment, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, task_id, body, author, author_user_id, created_at
		 FROM comments WHERE task_id = $1 ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Comment, 0)
	for rows.Next() {
		var c domain.Comment
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Body, &c.Author, &c.AuthorUserID, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- Activity log ----

// LogActivity appends one immutable entry to the activity log.
// Called by the actor after every state change and comment.
func (r *Repo) LogActivity(ctx context.Context, entry domain.ActivityLog) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO activity_log (task_id, project_id, actor, action, from_state, to_state, detail, actor_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		entry.TaskID, entry.ProjectID, entry.Actor,
		entry.Action, entry.FromState, entry.ToState, entry.Detail, entry.ActorUserID,
	)
	return err
}

// GetTicketHistory returns the full activity log for a task ordered oldest first.
// This is the complete story of a ticket: who did what and when.
func (r *Repo) GetTicketHistory(ctx context.Context, taskID int) ([]domain.ActivityLog, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, task_id, project_id, actor, action, from_state, to_state, detail, actor_user_id, created_at
		 FROM activity_log WHERE task_id = $1 ORDER BY created_at ASC, id ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.ActivityLog, 0)
	for rows.Next() {
		var e domain.ActivityLog
		if err := rows.Scan(
			&e.ID, &e.TaskID, &e.ProjectID,
			&e.Actor, &e.Action, &e.FromState, &e.ToState, &e.Detail,
			&e.ActorUserID, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetProjectActivity returns the full activity log for a project, newest first.
// Supports optional date range filtering for the report dashboard.
func (r *Repo) GetProjectActivity(ctx context.Context, projectID int, since, until string) ([]domain.ActivityLog, error) {
	q := `SELECT id, task_id, project_id, actor, action, from_state, to_state, detail, actor_user_id, created_at
	      FROM activity_log WHERE project_id = $1`
	args := []interface{}{projectID}
	if since != "" {
		args = append(args, since)
		q += fmt.Sprintf(` AND created_at >= $%d`, len(args))
	}
	if until != "" {
		args = append(args, until)
		q += fmt.Sprintf(` AND created_at <= $%d`, len(args))
	}
	q += ` ORDER BY created_at DESC, id DESC`

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.ActivityLog, 0)
	for rows.Next() {
		var e domain.ActivityLog
		if err := rows.Scan(
			&e.ID, &e.TaskID, &e.ProjectID,
			&e.Actor, &e.Action, &e.FromState, &e.ToState, &e.Detail,
			&e.ActorUserID, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Helpers ----

func (r *Repo) getTaskDeps(ctx context.Context, taskID int) ([]int, error) {
	rows, err := r.db.Query(ctx,
		`SELECT depends_on FROM task_dependencies WHERE task_id = $1`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deps []int
	for rows.Next() {
		var d int
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

func (r *Repo) getTaskDepsForProject(ctx context.Context, projectID int) (map[int][]int, error) {
	rows, err := r.db.Query(ctx,
		`SELECT td.task_id, td.depends_on
		 FROM task_dependencies td
		 JOIN tasks t ON t.id = td.task_id
		 WHERE t.project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int][]int)
	for rows.Next() {
		var taskID, depID int
		if err := rows.Scan(&taskID, &depID); err != nil {
			return nil, err
		}
		out[taskID] = append(out[taskID], depID)
	}
	return out, rows.Err()
}

// ---- Export ----

// ImportBundle creates a new project with all epics, stories, and tasks from a bundle.
// All inserts happen in a single transaction. If any step fails, the entire transaction rolls back.
// ID remapping is handled: old IDs from the bundle are mapped to new auto-increment IDs.
// Task dependencies are remapped after all tasks are created.
// All imported tasks start in 'backlog' state (fresh start, ignoring original state).
//
// home places the new project (org, owner team, importer). Ownership and
// attribution in the bundle refer to another database's users, so they are
// not carried over: the importer becomes creator of everything and owner of
// the epics and stories, and every ticket starts unassigned.
func (r *Repo) ImportBundle(ctx context.Context, bundle domain.ExportBundle, newCode, newName string, home domain.CreateProjectRequest) (*domain.Project, error) {
	// Start a transaction
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Determine project code and name
	projectCode := newCode
	projectName := newName
	if projectCode == "" {
		projectCode = bundle.Project.Code
	}
	if projectName == "" {
		projectName = bundle.Project.Name
	}

	// 1. Create the new project
	newProject := &domain.Project{}
	err = scanProject(tx.QueryRow(ctx,
		`INSERT INTO projects (name, code, description, org_id, created_by)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+projectCols,
		projectName, projectCode, bundle.Project.Description, home.OrgID, home.CreatedBy,
	), newProject)
	if isUniqueViolation(err) {
		return nil, conflictf("project code %q already exists", projectCode)
	}
	if err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	if home.TeamID != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'owner')`, newProject.ID, *home.TeamID); err != nil {
			return nil, fmt.Errorf("link owner team: %w", err)
		}
	}

	// 2. Create all epics and build oldEpicID → newEpicID map
	epicIDMap := make(map[int]int)
	for _, oldEpic := range bundle.Epics {
		newEpic := &domain.Epic{}
		err = tx.QueryRow(ctx,
			`INSERT INTO epics (project_id, name, description, priority, created_by)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING id, project_id, name, description, state, priority, created_at, updated_at`,
			newProject.ID, oldEpic.Name, oldEpic.Description, oldEpic.Priority, home.CreatedBy,
		).Scan(&newEpic.ID, &newEpic.ProjectID, &newEpic.Name, &newEpic.Description, &newEpic.State, &newEpic.Priority, &newEpic.CreatedAt, &newEpic.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("create epic: %w", err)
		}
		if home.CreatedBy != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO epic_owners (epic_id, user_id) VALUES ($1, $2)`, newEpic.ID, *home.CreatedBy); err != nil {
				return nil, fmt.Errorf("epic owner: %w", err)
			}
		}
		epicIDMap[oldEpic.ID] = newEpic.ID
	}

	// 3. Create all stories and build oldStoryID → newStoryID map
	storyIDMap := make(map[int]int)
	for _, oldStory := range bundle.Stories {
		// Remap epic_id via the epic map
		newEpicID := epicIDMap[oldStory.EpicID]
		newStory := &domain.Story{}
		err = tx.QueryRow(ctx,
			`INSERT INTO stories (project_id, epic_id, name, description, acceptance_criteria, priority, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 RETURNING id, project_id, epic_id, name, description, acceptance_criteria, state, priority, created_at, updated_at`,
			newProject.ID, newEpicID, oldStory.Name, oldStory.Description, oldStory.AcceptanceCriteria, oldStory.Priority, home.CreatedBy,
		).Scan(&newStory.ID, &newStory.ProjectID, &newStory.EpicID, &newStory.Name, &newStory.Description, &newStory.AcceptanceCriteria, &newStory.State, &newStory.Priority, &newStory.CreatedAt, &newStory.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("create story: %w", err)
		}
		if home.CreatedBy != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO story_owners (story_id, user_id) VALUES ($1, $2)`, newStory.ID, *home.CreatedBy); err != nil {
				return nil, fmt.Errorf("story owner: %w", err)
			}
		}
		storyIDMap[oldStory.ID] = newStory.ID
	}

	// 4. Create all tasks and build oldTaskID → newTaskID map
	taskIDMap := make(map[int]int)
	for _, oldExportTask := range bundle.Tasks {
		oldTask := oldExportTask.Task
		// Remap epic_id and story_id via the maps
		newEpicID := epicIDMap[oldTask.EpicID]
		newStoryID := storyIDMap[oldTask.StoryID]
		// All imported tasks start in 'backlog' state (fresh start)
		newTask := &domain.Task{}
		err = tx.QueryRow(ctx,
			`INSERT INTO tasks (project_id, epic_id, story_id, name, description, acceptance_criteria, priority, assigned_to, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 RETURNING id, project_id, epic_id, story_id, name, description, acceptance_criteria, state, priority,
			           assigned_to, agent_id, dispatched_at, completed_at, block_reason, created_at, updated_at`,
			newProject.ID, newEpicID, newStoryID, oldTask.Name, oldTask.Description, oldTask.AcceptanceCriteria, oldTask.Priority, oldTask.AssignedTo, home.CreatedBy,
		).Scan(
			&newTask.ID, &newTask.ProjectID, &newTask.EpicID, &newTask.StoryID,
			&newTask.Name, &newTask.Description, &newTask.AcceptanceCriteria,
			&newTask.State, &newTask.Priority,
			&newTask.AssignedTo, &newTask.AgentID, &newTask.DispatchedAt, &newTask.CompletedAt, &newTask.BlockReason,
			&newTask.CreatedAt, &newTask.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("create task: %w", err)
		}
		taskIDMap[oldTask.ID] = newTask.ID
	}

	// 5. Insert task dependencies with remapped IDs
	for _, oldExportTask := range bundle.Tasks {
		oldTaskID := oldExportTask.Task.ID
		newTaskID := taskIDMap[oldTaskID]
		// Remap all dependency IDs
		for _, oldDepID := range oldExportTask.DependencyIDs {
			newDepID := taskIDMap[oldDepID]
			_, err := tx.Exec(ctx,
				`INSERT INTO task_dependencies (task_id, depends_on) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
				newTaskID, newDepID,
			)
			if err != nil {
				return nil, fmt.Errorf("insert dependency %d→%d: %w", newTaskID, newDepID, err)
			}
		}
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	return newProject, nil
}

// BuildExportBundle loads all project data needed for export/import.
// It returns a self-contained bundle with the project, all epics, stories, and tasks.
// KB docs are fetched separately by the handler.
func (r *Repo) BuildExportBundle(ctx context.Context, projectID int) (*domain.ExportBundle, error) {
	// Load the project record
	project, err := r.GetProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}

	// Load all epics for the project
	epics, err := r.ListEpics(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list epics: %w", err)
	}

	// Load all stories for the project
	stories, err := r.ListStoriesByProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list stories: %w", err)
	}

	// Load all tasks for the project (no state filter)
	tasks, err := r.ListTasks(ctx, projectID, "")
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	// Convert tasks to ExportTask format (DependencyIDs already populated by ListTasks)
	exportTasks := make([]domain.ExportTask, len(tasks))
	for i, t := range tasks {
		exportTasks[i] = domain.ExportTask{
			Task:          t,
			DependencyIDs: t.DependencyIDs,
		}
	}

	bundle := &domain.ExportBundle{
		Version:    "1",
		ExportedAt: time.Now(),
		Project:    *project,
		Epics:      epics,
		Stories:    stories,
		Tasks:      exportTasks,
		KBDocs:     []domain.ExportKBDoc{}, // KB docs are fetched separately by the handler
	}

	return bundle, nil
}
