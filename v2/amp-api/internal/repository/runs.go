package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

const runCols = `id, task_id, project_id, user_id, agent_id, outcome, model, input_tokens, output_tokens,
	cache_read_tokens, cache_write_tokens, cost_usd::float8, branch, commit_sha, pr_url, pushed, merged, note, created_at`

func scanRun(row pgx.Row) (domain.Run, error) {
	var r domain.Run
	err := row.Scan(&r.ID, &r.TaskID, &r.ProjectID, &r.UserID, &r.AgentID, &r.Outcome, &r.Model, &r.InputTokens,
		&r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.CostUSD, &r.Branch, &r.Commit, &r.PRURL,
		&r.Pushed, &r.Merged, &r.Note, &r.CreatedAt)
	return r, err
}

// AddRun records one agent run on a ticket.
func (r *Repo) AddRun(ctx context.Context, run domain.Run) (domain.Run, error) {
	return scanRun(r.db.QueryRow(ctx, `INSERT INTO task_runs (task_id, project_id, user_id, agent_id, outcome, model,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, branch, commit_sha, pr_url, pushed, merged, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING `+runCols,
		run.TaskID, run.ProjectID, run.UserID, run.AgentID, run.Outcome, run.Model, run.InputTokens, run.OutputTokens,
		run.CacheReadTokens, run.CacheWriteTokens, run.CostUSD, run.Branch, run.Commit, run.PRURL, run.Pushed, run.Merged, run.Note))
}

func (r *Repo) queryRuns(ctx context.Context, q string, args ...any) ([]domain.Run, error) {
	rows, err := r.db.Query(ctx, `SELECT `+runCols+` FROM task_runs `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// TaskRuns lists a ticket's runs, oldest first.
func (r *Repo) TaskRuns(ctx context.Context, taskID int) ([]domain.Run, error) {
	return r.queryRuns(ctx, `WHERE task_id = $1 ORDER BY created_at, id`, taskID)
}

// ProjectRuns lists a project's runs recorded in [since, until).
func (r *Repo) ProjectRuns(ctx context.Context, projectID int, since, until any) ([]domain.Run, error) {
	return r.queryRuns(ctx, `WHERE project_id = $1 AND created_at >= $2 AND created_at < $3 ORDER BY created_at, id`, projectID, since, until)
}

// FindRuns finds runs by commit (a prefix of at least 7 characters), branch,
// or PR URL, in the given projects (nil: every project).
func (r *Repo) FindRuns(ctx context.Context, projectIDs []int, commit, branch, pr string) ([]domain.Run, error) {
	return r.queryRuns(ctx, `WHERE ($1::int[] IS NULL OR project_id = ANY($1))
		AND (($2 <> '' AND commit_sha LIKE $2 || '%') OR ($3 <> '' AND branch = $3) OR ($4 <> '' AND pr_url = $4))
		ORDER BY created_at DESC, id DESC LIMIT 100`, projectIDs, commit, branch, pr)
}

// UnmergedWork lists, for a project's completed tickets, the latest run that
// put work on a branch or PR, where no run on the ticket has reported it
// merged: work the agents finished that waits on a person to merge.
func (r *Repo) UnmergedWork(ctx context.Context, projectID int) ([]domain.Run, error) {
	return r.queryRuns(ctx, `WHERE id IN (
		SELECT DISTINCT ON (tr.task_id) tr.id
		  FROM task_runs tr JOIN tasks t ON t.id = tr.task_id
		 WHERE tr.project_id = $1 AND t.state = 'completed' AND (tr.branch <> '' OR tr.pr_url <> '')
		   AND NOT EXISTS (SELECT 1 FROM task_runs m WHERE m.task_id = tr.task_id AND m.merged)
		 ORDER BY tr.task_id, tr.created_at DESC, tr.id DESC)
		ORDER BY created_at`, projectID)
}
