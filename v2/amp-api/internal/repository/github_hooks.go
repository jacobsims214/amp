package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/secretbox"
)

// ---- Inbound GitHub webhook (AMP #2479) ----
//
// Each project has at most one secret, sealed by secretbox like webhook
// signing secrets (plaintext without a key; ResealGitHubHookSecrets seals
// it once one is configured). Deliveries are claimed by X-GitHub-Delivery,
// so a replay is a no-op, and only the last GitHubHookLogSize per project
// are kept.

// GitHubHookLogSize is how many deliveries each project's log keeps.
const GitHubHookLogSize = 50

// ErrNoGitHubHook: the project has no GitHub webhook secret.
var ErrNoGitHubHook = errors.New("no GitHub webhook is set up for this project")

const githubHookCols = `project_id, created_by, created_at, rotated_at, rejected_count, last_rejected_at`

func scanGitHubHook(row rowScanner, h *domain.GitHubHook) error {
	return row.Scan(&h.ProjectID, &h.CreatedBy, &h.CreatedAt, &h.RotatedAt, &h.RejectedCount, &h.LastRejectedAt)
}

// GetGitHubHook returns projectID's hook (without its secret), or
// ErrNoGitHubHook.
func (r *Repo) GetGitHubHook(ctx context.Context, projectID int) (*domain.GitHubHook, error) {
	h := &domain.GitHubHook{}
	err := scanGitHubHook(r.db.QueryRow(ctx, `SELECT `+githubHookCols+` FROM project_github_hooks WHERE project_id = $1`, projectID), h)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoGitHubHook
	}
	return h, err
}

// GitHubHookSecret returns projectID's secret in plaintext, or
// ErrNoGitHubHook. Only the webhook handler calls it.
func (r *Repo) GitHubHookSecret(ctx context.Context, projectID int) (string, error) {
	var stored string
	err := r.db.QueryRow(ctx, `SELECT secret FROM project_github_hooks WHERE project_id = $1`, projectID).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoGitHubHook
	}
	if err != nil {
		return "", err
	}
	return r.secrets().Open(secretbox.PurposeGitHubHookSecret, stored)
}

// SetGitHubHookSecret creates projectID's hook or replaces its secret.
func (r *Repo) SetGitHubHookSecret(ctx context.Context, projectID int, secret string, by *int) (*domain.GitHubHook, error) {
	stored := secret
	if r.secrets().CanSeal() {
		var err error
		if stored, err = r.secrets().Seal(secretbox.PurposeGitHubHookSecret, secret); err != nil {
			return nil, err
		}
	}
	h := &domain.GitHubHook{}
	err := scanGitHubHook(r.db.QueryRow(ctx, `INSERT INTO project_github_hooks (project_id, secret, created_by) VALUES ($1, $2, $3)
		ON CONFLICT (project_id) DO UPDATE SET secret = EXCLUDED.secret, rotated_at = NOW()
		RETURNING `+githubHookCols, projectID, stored, by), h)
	return h, err
}

// DeleteGitHubHook removes projectID's hook and its delivery log. It
// reports whether there was one.
func (r *Repo) DeleteGitHubHook(ctx context.Context, projectID int) (bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `DELETE FROM project_github_hooks WHERE project_id = $1`, projectID)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM github_hook_deliveries WHERE project_id = $1`, projectID); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, tx.Commit(ctx)
}

// NoteGitHubHookRejected counts a request to projectID's hook that failed
// its signature check.
func (r *Repo) NoteGitHubHookRejected(ctx context.Context, projectID int) error {
	_, err := r.db.Exec(ctx, `UPDATE project_github_hooks SET rejected_count = rejected_count + 1, last_rejected_at = NOW()
		WHERE project_id = $1`, projectID)
	return err
}

// ClaimGitHubDelivery claims delivery deliveryID for projectID. It returns
// true when this request should handle it: it is new, or an earlier attempt
// ended in a 5xx (GitHub's redeliver). Otherwise it returns false and the
// earlier record: a replay, which changes nothing.
func (r *Repo) ClaimGitHubDelivery(ctx context.Context, projectID int, deliveryID, event, action string) (bool, *domain.GitHubHookDelivery, error) {
	var id int64
	err := r.db.QueryRow(ctx, `INSERT INTO github_hook_deliveries (project_id, delivery_id, event, action) VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_id, delivery_id) DO UPDATE SET status = 0, result = '', task_ids = '{}', received_at = NOW()
		WHERE github_hook_deliveries.status >= 500
		RETURNING id`, projectID, deliveryID, event, action).Scan(&id)
	if err == nil {
		// Keep only the newest GitHubHookLogSize deliveries.
		_, err = r.db.Exec(ctx, `DELETE FROM github_hook_deliveries WHERE project_id = $1 AND id NOT IN (
			SELECT id FROM github_hook_deliveries WHERE project_id = $1 ORDER BY received_at DESC, id DESC LIMIT $2)`,
			projectID, GitHubHookLogSize)
		return true, nil, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, nil, fmt.Errorf("claim delivery: %w", err)
	}
	d := &domain.GitHubHookDelivery{}
	err = r.db.QueryRow(ctx, `SELECT delivery_id, event, action, status, result, task_ids, received_at
		FROM github_hook_deliveries WHERE project_id = $1 AND delivery_id = $2`, projectID, deliveryID).
		Scan(&d.DeliveryID, &d.Event, &d.Action, &d.Status, &d.Result, &d.TaskIDs, &d.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Pruned between the two statements: treat it as handled.
		return false, &domain.GitHubHookDelivery{DeliveryID: deliveryID, Event: event, Action: action}, nil
	}
	return false, d, err
}

// FinishGitHubDelivery records how a claimed delivery went.
func (r *Repo) FinishGitHubDelivery(ctx context.Context, projectID int, deliveryID string, status int, result string, taskIDs []int) error {
	if taskIDs == nil {
		taskIDs = []int{}
	}
	_, err := r.db.Exec(ctx, `UPDATE github_hook_deliveries SET status = $3, result = $4, task_ids = $5
		WHERE project_id = $1 AND delivery_id = $2`, projectID, deliveryID, status, result, taskIDs)
	return err
}

// ListGitHubDeliveries returns projectID's logged deliveries, newest first.
func (r *Repo) ListGitHubDeliveries(ctx context.Context, projectID int) ([]domain.GitHubHookDelivery, error) {
	rows, err := r.db.Query(ctx, `SELECT delivery_id, event, action, status, result, task_ids, received_at
		FROM github_hook_deliveries WHERE project_id = $1 ORDER BY received_at DESC, id DESC LIMIT $2`, projectID, GitHubHookLogSize)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.GitHubHookDelivery, error) {
		var d domain.GitHubHookDelivery
		return d, row.Scan(&d.DeliveryID, &d.Event, &d.Action, &d.Status, &d.Result, &d.TaskIDs, &d.ReceivedAt)
	})
}

// FindRunsForPR returns projectID's runs that produced a PR: its URL, its
// head branch, or any of commits (full SHAs; a run's commit matches when
// either is a prefix of the other, 7 characters at least). Newest first.
func (r *Repo) FindRunsForPR(ctx context.Context, projectID int, commits []string, branch, pr string) ([]domain.Run, error) {
	if commits == nil {
		commits = []string{}
	}
	return r.queryRuns(ctx, `WHERE project_id = $1 AND (
		($3 <> '' AND pr_url = $3) OR ($2 <> '' AND branch = $2)
		OR (length(commit_sha) >= 7 AND EXISTS (SELECT 1 FROM unnest($4::text[]) c
			WHERE length(c) >= 7 AND (c LIKE commit_sha || '%' OR commit_sha LIKE c || '%'))))
		ORDER BY created_at DESC, id DESC LIMIT 200`, projectID, branch, pr, commits)
}

// ResealGitHubHookSecrets brings every stored GitHub webhook secret under
// the current key, as ResealWebhookSecrets does for webhooks. It returns
// how many it sealed and how many it couldn't open.
func (r *Repo) ResealGitHubHookSecrets(ctx context.Context) (resealed, unopenable int, err error) {
	rows, err := r.db.Query(ctx, `SELECT project_id, secret FROM project_github_hooks ORDER BY project_id`)
	if err != nil {
		return 0, 0, fmt.Errorf("read GitHub webhook secrets: %w", err)
	}
	type stored struct {
		id     int
		secret string
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stored, error) {
		var s stored
		return s, row.Scan(&s.id, &s.secret)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("read GitHub webhook secrets: %w", err)
	}
	for _, s := range all {
		plain, err := r.secrets().Open(secretbox.PurposeGitHubHookSecret, s.secret)
		if err != nil {
			unopenable++
			continue
		}
		if !r.secrets().NeedsReseal(s.secret) {
			continue
		}
		sealed, err := r.secrets().Seal(secretbox.PurposeGitHubHookSecret, plain)
		if err != nil {
			return resealed, unopenable, err
		}
		tag, err := r.db.Exec(ctx, `UPDATE project_github_hooks SET secret = $3 WHERE project_id = $1 AND secret = $2`, s.id, s.secret, sealed)
		if err != nil {
			return resealed, unopenable, fmt.Errorf("seal GitHub webhook secret of project %d: %w", s.id, err)
		}
		if tag.RowsAffected() == 1 {
			resealed++
		}
	}
	return resealed, unopenable, nil
}
