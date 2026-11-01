package repository

import (
	"context"
	"fmt"
	"time"

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

// satisfiedSQL is when a dependency no longer holds the tickets after it:
// it is completed and, while its project requires merges, its work isn't
// waiting on a merge (merge_state needed, or abandoned: a PR closed without
// merging). AMP #2475. It is the rule for a dependent that waits for
// merges, the default; one with waits_for_merge: false sees DepStatesFor.
const satisfiedSQL = `t.state = 'completed' AND (t.merge_state NOT IN ('needed', 'abandoned')
	OR NOT (SELECT p.require_merge FROM projects p WHERE p.id = t.project_id))`

// CompletedAmong returns which of ids are done as far as the tickets that
// depend on them are concerned (satisfiedSQL), wherever they are in the
// project.
func (r *Repo) CompletedAmong(ctx context.Context, ids []int) (map[int]bool, error) {
	done, _, err := r.DepStates(ctx, ids)
	return done, err
}

// DepStates returns which of ids are satisfied dependencies (as
// CompletedAmong), and for each one that is completed but still waits on
// its merge, why: "Waiting on merge of #N (PR …)".
func (r *Repo) DepStates(ctx context.Context, ids []int) (map[int]bool, map[int]string, error) {
	done, waits := map[int]bool{}, map[int]string{}
	if len(ids) == 0 {
		return done, waits, nil
	}
	rows, err := r.db.Query(ctx, `SELECT t.id, (`+satisfiedSQL+`), t.state = 'completed', t.merge_state,
		COALESCE((SELECT CASE WHEN tr.pr_url <> '' THEN 'PR ' || tr.pr_url ELSE 'branch ' || tr.branch END
		            FROM task_runs tr WHERE tr.task_id = t.id AND (tr.pr_url <> '' OR tr.branch <> '')
		           ORDER BY tr.created_at DESC, tr.id DESC LIMIT 1), '')
		FROM tasks t WHERE t.id = ANY($1)`, ids)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var ok, completed bool
		var ms, where string
		if err := rows.Scan(&id, &ok, &completed, &ms, &where); err != nil {
			return nil, nil, err
		}
		switch {
		case ok:
			done[id] = true
		case completed:
			waits[id] = MergeWaitDetail(id, ms, where)
		}
	}
	return done, waits, rows.Err()
}

// effectiveTaskMetaSQL is ticket t's effective metadata: its project's,
// epic's and story's, then its own (later wins), as InheritedMetadata.
const effectiveTaskMetaSQL = `(SELECT p.metadata || e.metadata || s.metadata || t.metadata
	FROM stories s, epics e, projects p WHERE s.id = t.story_id AND e.id = t.epic_id AND p.id = t.project_id)`

// noMergeWaitSQL: ticket t's effective metadata says waits_for_merge false
// (domain.Metadata.WaitsForMerge): it waits for its dependencies to
// complete, not for their merges.
const noMergeWaitSQL = `(lower(btrim(COALESCE(` + effectiveTaskMetaSQL + ` ->> '` + domain.MetaWaitsForMerge + `', ''))) = 'false')`

// NoMergeWait returns which of ids opted out of waiting on their
// dependencies' merges (waits_for_merge: false, AMP #2596).
func (r *Repo) NoMergeWait(ctx context.Context, ids []int) (map[int]bool, error) {
	out := map[int]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT t.id FROM tasks t WHERE t.id = ANY($1) AND `+noMergeWaitSQL, ids)
	if err != nil {
		return nil, err
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[int])
	for _, id := range got {
		out[id] = true
	}
	return out, err
}

// DepStatesFor is DepStates as dependent sees its dependencies ids: when
// dependent doesn't wait for merges (waits_for_merge: false), a completed
// dependency is satisfied even while its merge is pending.
func (r *Repo) DepStatesFor(ctx context.Context, dependent int, ids []int) (map[int]bool, map[int]string, error) {
	done, waits, err := r.DepStates(ctx, ids)
	if err != nil || len(waits) == 0 || dependent == 0 {
		return done, waits, err
	}
	nowait, err := r.NoMergeWait(ctx, []int{dependent})
	if err != nil {
		return nil, nil, err
	}
	if nowait[dependent] {
		RelaxMergeWaits(done, waits)
	}
	return done, waits, nil
}

// RelaxMergeWaits counts the dependencies in waits (done, waiting only on a
// merge) as satisfied, for a dependent that doesn't wait for merges.
func RelaxMergeWaits(done map[int]bool, waits map[int]string) {
	for id := range waits {
		done[id] = true
		delete(waits, id)
	}
}

// IntegrationMerged lists projectID's done tickets waiting on a merge
// (needed) whose work is already on their integration branch: the latest
// run that put work up landed there (domain.Run.LandsOn: base_branch = the
// ticket's effective integration_branch, or no PR and branch = it) (#2596).
func (r *Repo) IntegrationMerged(ctx context.Context, projectID int) ([]int, error) {
	rows, err := r.db.Query(ctx, `SELECT t.id FROM tasks t
		WHERE t.project_id = $1 AND t.state = 'completed' AND t.merge_state = 'needed'
		  AND COALESCE(`+effectiveTaskMetaSQL+` ->> '`+domain.MetaIntegrationBranch+`', '') <> ''
		  AND btrim(`+effectiveTaskMetaSQL+` ->> '`+domain.MetaIntegrationBranch+`') =
		      (SELECT CASE WHEN tr.base_branch <> '' THEN tr.base_branch WHEN tr.pr_url = '' THEN tr.branch END
		         FROM task_runs tr WHERE tr.task_id = t.id AND (tr.pr_url <> '' OR tr.branch <> '')
		        ORDER BY tr.created_at DESC, tr.id DESC LIMIT 1)
		ORDER BY t.id`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// MergeWaitDetail says why a done ticket still holds its dependents.
func MergeWaitDetail(id int, mergeState, where string) string {
	if mergeState == domain.MergeAbandoned {
		s := fmt.Sprintf("Waiting on #%d: its PR was closed without merging", id)
		if where != "" {
			s += " (" + where + ")"
		}
		return s + "; reopen it or mark the merge not needed"
	}
	s := fmt.Sprintf("Waiting on merge of #%d", id)
	if where != "" {
		s += " (" + where + ")"
	}
	return s
}

// MergeGatedTasks lists a project's completed tickets whose merge state
// would hold their dependents (needed or abandoned).
func (r *Repo) MergeGatedTasks(ctx context.Context, projectID int) ([]int, error) {
	rows, err := r.db.Query(ctx, `SELECT id FROM tasks WHERE project_id = $1 AND state = 'completed'
		AND merge_state IN ('needed', 'abandoned') ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// SetTaskMerge writes a ticket's merge fields. required nil keeps
// merge_required; at and source are stored as given (nil/"" clear them).
func (r *Repo) SetTaskMerge(ctx context.Context, taskID int, required *bool, state string, at *time.Time, source string) error {
	_, err := r.db.Exec(ctx, `UPDATE tasks SET merge_required = COALESCE($2, merge_required), merge_state = $3,
		merged_at = $4, merge_source = $5, updated_at = NOW() WHERE id = $1`, taskID, required, state, at, source)
	return err
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
