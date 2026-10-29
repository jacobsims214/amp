package repository

import (
	"context"
	"strings"
	"time"
)

// Waiting on you (AMP #2478, KB decisions/human-work-ux.md §1). These
// queries fetch the candidate tickets; service.WaitingOn classifies them, so
// every view (the project's My work tab, the top-level summary, the badge
// counts) reads one definition.

// WaitTask is a candidate ticket for a person's "waiting on you" list, with
// the project settings its classification depends on.
type WaitTask struct {
	WorkTask
	RequirePlanApproval bool `json:"-"`
	RequireMerge        bool `json:"-"`
	// NotStartedFlagMinutes: a dispatched ticket no worker started is the
	// owner's to act on once it has waited this long (0: never, AMP #2801).
	NotStartedFlagMinutes int `json:"-"`
}

// qualifiedTaskCols is taskCols with each column read from alias w.
var qualifiedTaskCols = func() string {
	cols := strings.Split(taskCols, ",")
	for i, c := range cols {
		cols[i] = "w." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}()

// WaitingCandidates returns the open or merge-pending tickets that may wait
// on userID, in active projects (projectIDs nil: every project): tickets
// whose plan approval targets them (owner, else creator: Task.ApprovalOwner),
// their own tickets blocked with a question, their own completed tickets
// whose merge is needed or abandoned, and their own human tickets not yet
// done. Each comes with its dependency ids.
func (r *Repo) WaitingCandidates(ctx context.Context, userID int, projectIDs []int) ([]WaitTask, error) {
	rows, err := r.db.Query(ctx, `SELECT `+qualifiedTaskCols+`, p.code, p.name, p.require_plan_approval, p.require_merge,
		p.not_started_flag_minutes,
		COALESCE((SELECT array_agg(d.depends_on ORDER BY d.depends_on) FROM task_dependencies d WHERE d.task_id = w.id), '{}')
		FROM (SELECT * FROM tasks
		       WHERE ($2::int[] IS NULL OR project_id = ANY($2))
		         AND (
		           (COALESCE(owner_user_id, created_by) = $1 AND state <> 'completed'
		              AND (approved_at IS NULL OR approved_at < planned_at))
		           OR (owner_user_id = $1 AND state = 'blocked' AND block_reason <> '' AND block_reason NOT LIKE 'scheduled:%')
		           OR (owner_user_id = $1 AND state = 'completed' AND merge_state IN ('needed', 'abandoned'))
		           OR (owner_user_id = $1 AND state = 'in_progress' AND started_at IS NULL AND performer <> 'human')
		           OR (owner_user_id = $1 AND performer = 'human' AND state <> 'completed')
		         )) w
		JOIN projects p ON p.id = w.project_id
		WHERE p.state <> 'archived'
		ORDER BY w.id`, userID, projectIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WaitTask{}
	for rows.Next() {
		var w WaitTask
		t := &w.Task
		if err := rows.Scan(append(taskFields(t), &w.ProjectCode, &w.ProjectName, &w.RequirePlanApproval, &w.RequireMerge, &w.NotStartedFlagMinutes, &t.DependencyIDs)...); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DepSummary is a ticket named as a dependency or dependent of another.
type DepSummary struct {
	ID          int    `json:"id"`
	ProjectID   int    `json:"project_id"`
	EpicID      int    `json:"epic_id"`
	StoryID     int    `json:"story_id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	BlockReason string `json:"block_reason,omitempty"`
	Performer   string `json:"performer"`
	OwnerUserID *int   `json:"owner_user_id"`
	MergeState  string `json:"merge_state"`
	// Satisfied: the tickets after it may start (completed and, while its
	// project requires merges, not waiting on a merge: satisfiedSQL).
	Satisfied bool `json:"satisfied"`
	// WaitingDetail says why a completed ticket still holds its dependents.
	WaitingDetail string `json:"waiting_detail,omitempty"`
}

// DepSummaries describes the tickets ids, wherever they are.
func (r *Repo) DepSummaries(ctx context.Context, ids []int) (map[int]DepSummary, error) {
	out := map[int]DepSummary{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT t.id, t.project_id, t.epic_id, t.story_id, t.name, t.state, t.block_reason, t.performer,
		t.owner_user_id, t.merge_state, (`+satisfiedSQL+`)
		FROM tasks t WHERE t.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d DepSummary
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.EpicID, &d.StoryID, &d.Name, &d.State, &d.BlockReason, &d.Performer,
			&d.OwnerUserID, &d.MergeState, &d.Satisfied); err != nil {
			return nil, err
		}
		out[d.ID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Why a done ticket still holds: the same text the actor shows.
	var held []int
	for id, d := range out {
		if !d.Satisfied && d.State == "completed" {
			held = append(held, id)
		}
	}
	if len(held) > 0 {
		_, waits, err := r.DepStates(ctx, held)
		if err != nil {
			return nil, err
		}
		for id, w := range waits {
			d := out[id]
			d.WaitingDetail = w
			out[id] = d
		}
	}
	return out, nil
}

// OpenDependents maps each of ids to the open (not completed) tickets that
// depend on it directly, by id.
func (r *Repo) OpenDependents(ctx context.Context, ids []int) (map[int][]int, error) {
	out := map[int][]int{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT d.depends_on, d.task_id FROM task_dependencies d JOIN tasks t ON t.id = d.task_id
		WHERE d.depends_on = ANY($1) AND t.state <> 'completed' ORDER BY d.depends_on, d.task_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var dep, task int
		if err := rows.Scan(&dep, &task); err != nil {
			return nil, err
		}
		out[dep] = append(out[dep], task)
	}
	return out, rows.Err()
}

// MergeRef is where a ticket's unmerged work is: the latest run that names
// a pull request or a branch.
type MergeRef struct {
	PRURL  string    `json:"pr_url,omitempty"`
	Branch string    `json:"branch,omitempty"`
	At     time.Time `json:"at"`
}

// LatestMergeRefs returns, for each of ids that has one, its latest run
// carrying a PR URL or a branch.
func (r *Repo) LatestMergeRefs(ctx context.Context, ids []int) (map[int]MergeRef, error) {
	out := map[int]MergeRef{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT DISTINCT ON (task_id) task_id, pr_url, branch, created_at FROM task_runs
		WHERE task_id = ANY($1) AND (pr_url <> '' OR branch <> '')
		ORDER BY task_id, created_at DESC, id DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var m MergeRef
		if err := rows.Scan(&id, &m.PRURL, &m.Branch, &m.At); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

// ClearedTask is something a person cleared from their list in a window:
// kind (approve, answer, merge, do), the ticket, and when.
type ClearedTask struct {
	Kind        string    `json:"kind"`
	TaskID      int       `json:"task_id"`
	ProjectID   int       `json:"project_id"`
	EpicID      int       `json:"epic_id"`
	Name        string    `json:"name"`
	At          time.Time `json:"at"`
	MergeSource string    `json:"merge_source,omitempty"` // merge: who recorded it (ui, ci, github)
	MergeState  string    `json:"merge_state,omitempty"`
}

// ClearedBetween lists what waited on userID and was cleared in [start, end)
// in projectID: tickets they approved, questions on their tickets answered,
// merges recorded on their tickets, and their human tickets finished.
// Oldest first.
func (r *Repo) ClearedBetween(ctx context.Context, userID, projectID int, start, end time.Time) ([]ClearedTask, error) {
	rows, err := r.db.Query(ctx, `
		SELECT 'approve', id, project_id, epic_id, name, approved_at, '', '' FROM tasks
		 WHERE project_id = $2 AND approved_by = $1 AND approved_at >= $3 AND approved_at < $4
		UNION ALL
		SELECT 'answer', t.id, t.project_id, t.epic_id, t.name, a.at, '', '' FROM tasks t
		  JOIN LATERAL (SELECT MAX(created_at) AS at FROM activity_log l WHERE l.task_id = t.id
		                   AND l.detail LIKE 'Question answered%' AND l.created_at >= $3 AND l.created_at < $4) a ON a.at IS NOT NULL
		 WHERE t.project_id = $2 AND t.owner_user_id = $1
		UNION ALL
		SELECT 'merge', id, project_id, epic_id, name, merged_at, merge_source, merge_state FROM tasks
		 WHERE project_id = $2 AND owner_user_id = $1 AND merged_at >= $3 AND merged_at < $4
		UNION ALL
		SELECT 'do', id, project_id, epic_id, name, completed_at, '', '' FROM tasks
		 WHERE project_id = $2 AND owner_user_id = $1 AND performer = 'human' AND state = 'completed'
		   AND completed_at >= $3 AND completed_at < $4
		ORDER BY 6, 2`, userID, projectID, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClearedTask{}
	for rows.Next() {
		var c ClearedTask
		if err := rows.Scan(&c.Kind, &c.TaskID, &c.ProjectID, &c.EpicID, &c.Name, &c.At, &c.MergeSource, &c.MergeState); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AgentCounts is how much of a person's agent work (agent tickets they
// own) is in each state in a project.
type AgentCounts struct {
	ProjectID  int `json:"project_id"`
	InProgress int `json:"in_progress"`
	Ready      int `json:"ready"`
	Waiting    int `json:"waiting"`
	DoneToday  int `json:"done_today"`
}

// AgentCountsFor counts the agent tickets userID owns, per active project
// (projectIDs nil: every project). done_today counts completions since
// since.
func (r *Repo) AgentCountsFor(ctx context.Context, userID int, projectIDs []int, since time.Time) (map[int]AgentCounts, error) {
	rows, err := r.db.Query(ctx, `SELECT t.project_id,
		COUNT(*) FILTER (WHERE t.state = 'in_progress'),
		COUNT(*) FILTER (WHERE t.state = 'backlog'),
		COUNT(*) FILTER (WHERE t.state = 'blocked'),
		COUNT(*) FILTER (WHERE t.state = 'completed' AND t.completed_at >= $3)
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.state <> 'archived' AND t.owner_user_id = $1 AND t.performer <> 'human'
		  AND ($2::int[] IS NULL OR t.project_id = ANY($2))
		  AND (t.state <> 'completed' OR t.completed_at >= $3)
		GROUP BY t.project_id`, userID, projectIDs, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]AgentCounts{}
	for rows.Next() {
		var c AgentCounts
		if err := rows.Scan(&c.ProjectID, &c.InProgress, &c.Ready, &c.Waiting, &c.DoneToday); err != nil {
			return nil, err
		}
		out[c.ProjectID] = c
	}
	return out, rows.Err()
}

// ProjectRef is a project's id, code and name.
type ProjectRef struct {
	ID   int    `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// ActiveProjectRefs lists active projects (ids nil: every one), by name.
func (r *Repo) ActiveProjectRefs(ctx context.Context, ids []int) ([]ProjectRef, error) {
	rows, err := r.db.Query(ctx, `SELECT id, code, name FROM projects WHERE state <> 'archived'
		AND ($1::int[] IS NULL OR id = ANY($1)) ORDER BY lower(name), id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectRef{}
	for rows.Next() {
		var p ProjectRef
		if err := rows.Scan(&p.ID, &p.Code, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
