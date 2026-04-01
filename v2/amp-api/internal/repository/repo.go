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

// Migrate runs the provided SQL. Call this from main with the embedded migration content.
func (r *Repo) Migrate(ctx context.Context, sql string) error {
	_, err := r.db.Exec(ctx, sql)
	return err
}

func (r *Repo) Close() { r.db.Close() }

// ---- Projects ----

func (r *Repo) CreateProject(ctx context.Context, req domain.CreateProjectRequest) (*domain.Project, error) {
	p := &domain.Project{}
	err := r.db.QueryRow(ctx,
		`INSERT INTO projects (name, code, description)
		 VALUES ($1, $2, $3)
		 RETURNING id, name, code, description, state, created_at, updated_at`,
		req.Name, req.Code, req.Description,
	).Scan(&p.ID, &p.Name, &p.Code, &p.Description, &p.State, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (r *Repo) GetProject(ctx context.Context, id int) (*domain.Project, error) {
	p := &domain.Project{}
	err := r.db.QueryRow(ctx,
		`SELECT id, name, code, description, state, created_at, updated_at
		 FROM projects WHERE id = $1`, id,
	).Scan(&p.ID, &p.Name, &p.Code, &p.Description, &p.State, &p.CreatedAt, &p.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("project %d not found", id)
	}
	return p, err
}

func (r *Repo) ListProjects(ctx context.Context) ([]domain.Project, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, name, code, description, state, created_at, updated_at
		 FROM projects WHERE state = 'active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]domain.Project, 0)
	for rows.Next() {
		var p domain.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Code, &p.Description, &p.State, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- Epics ----

func (r *Repo) CreateEpic(ctx context.Context, req domain.CreateEpicRequest) (*domain.Epic, error) {
	e := &domain.Epic{}
	err := r.db.QueryRow(ctx,
		`INSERT INTO epics (project_id, name, description, priority)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, project_id, name, description, state, priority, created_at, updated_at`,
		req.ProjectID, req.Name, req.Description, req.Priority,
	).Scan(&e.ID, &e.ProjectID, &e.Name, &e.Description, &e.State, &e.Priority, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

func (r *Repo) GetEpic(ctx context.Context, id int) (*domain.Epic, error) {
	e := &domain.Epic{}
	err := r.db.QueryRow(ctx,
		`SELECT id, project_id, name, description, state, priority, created_at, updated_at
		 FROM epics WHERE id = $1`, id,
	).Scan(&e.ID, &e.ProjectID, &e.Name, &e.Description, &e.State, &e.Priority, &e.CreatedAt, &e.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("epic %d not found", id)
	}
	return e, err
}

func (r *Repo) ListEpics(ctx context.Context, projectID int) ([]domain.Epic, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, project_id, name, description, state, priority, created_at, updated_at
		 FROM epics WHERE project_id = $1 ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Epic, 0)
	for rows.Next() {
		var e domain.Epic
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Name, &e.Description, &e.State, &e.Priority, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Stories ----

func (r *Repo) CreateStory(ctx context.Context, req domain.CreateStoryRequest) (*domain.Story, error) {
	s := &domain.Story{}
	err := r.db.QueryRow(ctx,
		`INSERT INTO stories (project_id, epic_id, name, description, acceptance_criteria, priority)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, project_id, epic_id, name, description, acceptance_criteria, state, priority, created_at, updated_at`,
		req.ProjectID, req.EpicID, req.Name, req.Description, req.AcceptanceCriteria, req.Priority,
	).Scan(&s.ID, &s.ProjectID, &s.EpicID, &s.Name, &s.Description, &s.AcceptanceCriteria, &s.State, &s.Priority, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

func (r *Repo) GetStory(ctx context.Context, id int) (*domain.Story, error) {
	s := &domain.Story{}
	err := r.db.QueryRow(ctx,
		`SELECT id, project_id, epic_id, name, description, acceptance_criteria, state, priority, created_at, updated_at
		 FROM stories WHERE id = $1`, id,
	).Scan(&s.ID, &s.ProjectID, &s.EpicID, &s.Name, &s.Description, &s.AcceptanceCriteria, &s.State, &s.Priority, &s.CreatedAt, &s.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("story %d not found", id)
	}
	return s, err
}

func (r *Repo) ListStories(ctx context.Context, epicID int) ([]domain.Story, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, project_id, epic_id, name, description, acceptance_criteria, state, priority, created_at, updated_at
		 FROM stories WHERE epic_id = $1 ORDER BY id`, epicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Story, 0)
	for rows.Next() {
		var s domain.Story
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.EpicID, &s.Name, &s.Description, &s.AcceptanceCriteria, &s.State, &s.Priority, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- Tasks ----

func (r *Repo) CreateTask(ctx context.Context, req domain.CreateTaskRequest) (*domain.Task, error) {
	t := &domain.Task{}
	err := r.db.QueryRow(ctx,
		`INSERT INTO tasks (project_id, epic_id, story_id, name, description, acceptance_criteria, priority, assigned_to)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, project_id, epic_id, story_id, name, description, acceptance_criteria, state, priority,
		           assigned_to, agent_id, dispatched_at, completed_at, block_reason, created_at, updated_at`,
		req.ProjectID, req.EpicID, req.StoryID, req.Name, req.Description, req.AcceptanceCriteria, req.Priority, req.AssignedTo,
	).Scan(
		&t.ID, &t.ProjectID, &t.EpicID, &t.StoryID,
		&t.Name, &t.Description, &t.AcceptanceCriteria,
		&t.State, &t.Priority,
		&t.AssignedTo, &t.AgentID, &t.DispatchedAt, &t.CompletedAt, &t.BlockReason,
		&t.CreatedAt, &t.UpdatedAt,
	)
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
	err := r.db.QueryRow(ctx,
		`SELECT id, project_id, epic_id, story_id, name, description, acceptance_criteria, state, priority,
		        assigned_to, agent_id, dispatched_at, completed_at, block_reason, created_at, updated_at
		 FROM tasks WHERE id = $1`, id,
	).Scan(
		&t.ID, &t.ProjectID, &t.EpicID, &t.StoryID,
		&t.Name, &t.Description, &t.AcceptanceCriteria,
		&t.State, &t.Priority,
		&t.AssignedTo, &t.AgentID, &t.DispatchedAt, &t.CompletedAt, &t.BlockReason,
		&t.CreatedAt, &t.UpdatedAt,
	)
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
	query := `SELECT id, project_id, epic_id, story_id, name, description, acceptance_criteria, state, priority,
	                 assigned_to, agent_id, dispatched_at, completed_at, block_reason, created_at, updated_at
	          FROM tasks WHERE project_id = $1`
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
	defer rows.Close()

	out := make([]domain.Task, 0)
	for rows.Next() {
		var t domain.Task
		if err := rows.Scan(
			&t.ID, &t.ProjectID, &t.EpicID, &t.StoryID,
			&t.Name, &t.Description, &t.AcceptanceCriteria,
			&t.State, &t.Priority,
			&t.AssignedTo, &t.AgentID, &t.DispatchedAt, &t.CompletedAt, &t.BlockReason,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
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
	rows, err := r.db.Query(ctx,
		`SELECT id, project_id, epic_id, story_id, name, description, acceptance_criteria, state, priority,
		        assigned_to, agent_id, dispatched_at, completed_at, block_reason, created_at, updated_at
		 FROM tasks WHERE story_id = $1 ORDER BY id`, storyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Task, 0)
	for rows.Next() {
		var t domain.Task
		if err := rows.Scan(
			&t.ID, &t.ProjectID, &t.EpicID, &t.StoryID,
			&t.Name, &t.Description, &t.AcceptanceCriteria,
			&t.State, &t.Priority,
			&t.AssignedTo, &t.AgentID, &t.DispatchedAt, &t.CompletedAt, &t.BlockReason,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Load deps for all tasks
	if len(out) > 0 {
		for i := range out {
			deps, err := r.getTaskDeps(ctx, out[i].ID)
			if err != nil {
				return nil, err
			}
			out[i].DependencyIDs = deps
		}
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
	rows, err := r.db.Query(ctx,
		`SELECT id, project_id, epic_id, name, description, acceptance_criteria, state, priority, created_at, updated_at
		 FROM stories WHERE project_id = $1 ORDER BY epic_id, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Story, 0)
	for rows.Next() {
		var s domain.Story
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.EpicID, &s.Name, &s.Description,
			&s.AcceptanceCriteria, &s.State, &s.Priority, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repo) SetTaskState(ctx context.Context, taskID int, state domain.TaskState, blockReason string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE tasks SET state = $1, block_reason = $2, updated_at = NOW() WHERE id = $3`,
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

func (r *Repo) UpdateTask(ctx context.Context, req domain.UpdateTaskRequest) error {
	_, err := r.db.Exec(ctx,
		`UPDATE tasks SET
		   name        = CASE WHEN $1 != '' THEN $1 ELSE name END,
		   description = CASE WHEN $2 != '' THEN $2 ELSE description END,
		   assigned_to = CASE WHEN $3 != '' THEN $3 ELSE assigned_to END,
		   agent_id    = CASE WHEN $4 != '' THEN $4 ELSE agent_id END,
		   updated_at  = NOW()
		 WHERE id = $5`,
		req.Name, req.Description, req.AssignedTo, req.AgentID, req.TaskID,
	)
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
		`INSERT INTO comments (task_id, body, author) VALUES ($1, $2, $3)
		 RETURNING id, task_id, body, author, created_at`,
		req.TaskID, req.Body, author,
	).Scan(&c.ID, &c.TaskID, &c.Body, &c.Author, &c.CreatedAt)
	return c, err
}

func (r *Repo) GetComments(ctx context.Context, taskID int) ([]domain.Comment, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, task_id, body, author, created_at
		 FROM comments WHERE task_id = $1 ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Comment, 0)
	for rows.Next() {
		var c domain.Comment
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Body, &c.Author, &c.CreatedAt); err != nil {
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
		`INSERT INTO activity_log (task_id, project_id, actor, action, from_state, to_state, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		entry.TaskID, entry.ProjectID, entry.Actor,
		entry.Action, entry.FromState, entry.ToState, entry.Detail,
	)
	return err
}

// GetTicketHistory returns the full activity log for a task ordered oldest first.
// This is the complete story of a ticket: who did what and when.
func (r *Repo) GetTicketHistory(ctx context.Context, taskID int) ([]domain.ActivityLog, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, task_id, project_id, actor, action, from_state, to_state, detail, created_at
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
			&e.CreatedAt,
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
	q := `SELECT id, task_id, project_id, actor, action, from_state, to_state, detail, created_at
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
			&e.CreatedAt,
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
