package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Webhooks and their delivery outbox ----

// webhookCols never reads auth_header_value: only the delivery loop
// (ClaimDueDeliveries) needs it.
const webhookCols = `id, org_id, project_id, name, url, format, events, active, created_by, created_at, updated_at,
	last_delivery_at, last_status, last_error, consecutive_failures, auth_header_name, auth_header_value <> ''`

func scanWebhook(row rowScanner, w *domain.Webhook) error {
	err := row.Scan(&w.ID, &w.OrgID, &w.ProjectID, &w.Name, &w.URL, &w.Format, &w.Events, &w.Active, &w.CreatedBy,
		&w.CreatedAt, &w.UpdatedAt, &w.LastDeliveryAt, &w.LastStatus, &w.LastError, &w.ConsecutiveFailures,
		&w.AuthHeaderName, &w.AuthHeaderSet)
	w.Instance = w.OrgID == nil && w.ProjectID == nil
	return err
}

// CreateWebhook stores a webhook (w.Secret included).
func (r *Repo) CreateWebhook(ctx context.Context, w domain.Webhook) (*domain.Webhook, error) {
	out := &domain.Webhook{}
	err := scanWebhook(r.db.QueryRow(ctx, `
		INSERT INTO webhooks (org_id, project_id, name, url, format, secret, events, active, created_by, auth_header_name, auth_header_value)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+webhookCols,
		w.OrgID, w.ProjectID, w.Name, w.URL, w.Format, w.Secret, w.Events, w.Active, w.CreatedBy,
		w.AuthHeaderName, w.AuthHeaderValue), out)
	if err != nil {
		return nil, fmt.Errorf("create webhook: %w", err)
	}
	out.Secret = w.Secret
	return out, nil
}

func (r *Repo) GetWebhook(ctx context.Context, id int) (*domain.Webhook, error) {
	w := &domain.Webhook{}
	err := scanWebhook(r.db.QueryRow(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = $1`, id), w)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("webhook %d: %w", id, ErrNotFound)
	}
	return w, err
}

// ListWebhooks returns an org's or a project's webhooks, or with neither
// the instance webhooks (secrets omitted).
func (r *Repo) ListWebhooks(ctx context.Context, orgID, projectID *int) ([]domain.Webhook, error) {
	rows, err := r.db.Query(ctx, `SELECT `+webhookCols+` FROM webhooks
		WHERE ($1::int IS NOT NULL AND org_id = $1) OR ($2::int IS NOT NULL AND project_id = $2)
		   OR ($1::int IS NULL AND $2::int IS NULL AND org_id IS NULL AND project_id IS NULL) ORDER BY id`, orgID, projectID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanWebhook)
}

// UpdateWebhook saves a webhook's editable fields. A change of URL, or
// turning it back on, clears its failure count. setAuthHeader replaces the
// auth header with w's (an empty name removes it); otherwise it is kept.
func (r *Repo) UpdateWebhook(ctx context.Context, w domain.Webhook, setAuthHeader bool) (*domain.Webhook, error) {
	out := &domain.Webhook{}
	err := scanWebhook(r.db.QueryRow(ctx, `
		UPDATE webhooks SET name = $2, url = $3, format = $4, events = $5, active = $6, updated_at = NOW(),
		       consecutive_failures = CASE WHEN url <> $3 OR (NOT active AND $6) THEN 0 ELSE consecutive_failures END,
		       auth_header_name = CASE WHEN $7 THEN $8 ELSE auth_header_name END,
		       auth_header_value = CASE WHEN $7 THEN $9 ELSE auth_header_value END
		WHERE id = $1 RETURNING `+webhookCols, w.ID, w.Name, w.URL, w.Format, w.Events, w.Active,
		setAuthHeader, w.AuthHeaderName, w.AuthHeaderValue), out)
	return out, err
}

func (r *Repo) SetWebhookSecret(ctx context.Context, id int, secret string) error {
	_, err := r.db.Exec(ctx, `UPDATE webhooks SET secret = $2, updated_at = NOW() WHERE id = $1`, id, secret)
	return err
}

func (r *Repo) DeleteWebhook(ctx context.Context, id int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM webhooks WHERE id = $1`, id)
	return err
}

// WebhooksForEvent returns the active webhooks that may receive an event:
// the project's own and its org's (projectID 0: only orgID's), and for an
// audit entry also the instance webhooks, which receive every audit entry
// (projectID and orgID 0: only those).
func (r *Repo) WebhooksForEvent(ctx context.Context, projectID, orgID int, audit bool) ([]domain.Webhook, error) {
	rows, err := r.db.Query(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE active AND (
		($1 <> 0 AND project_id = $1) OR
		org_id = CASE WHEN $2 <> 0 THEN $2 ELSE (SELECT org_id FROM projects WHERE id = $1) END OR
		($3 AND org_id IS NULL AND project_id IS NULL))`, projectID, orgID, audit)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanWebhook)
}

// EnqueueDelivery adds a delivery to the outbox, due now.
func (r *Repo) EnqueueDelivery(ctx context.Context, webhookID int, eventType, body string) (int64, error) {
	var id int64
	err := r.db.QueryRow(ctx, `INSERT INTO webhook_deliveries (webhook_id, event_type, body) VALUES ($1, $2, $3) RETURNING id`,
		webhookID, eventType, body).Scan(&id)
	return id, err
}

// DueDelivery is a claimed delivery with what's needed to send it.
type DueDelivery struct {
	ID        int64
	WebhookID int
	EventType string
	Body      string
	Attempts  int
	URL       string
	Secret    string
	// AuthHeaderName/Value: the webhook's extra header ("" for none). The
	// value is a secret: never log it.
	AuthHeaderName  string
	AuthHeaderValue string
}

// ClaimDueDeliveries leases up to limit due deliveries of active webhooks
// for lease: other dispatchers (other replicas) skip them meanwhile, and
// if this one dies they come due again when the lease runs out.
func (r *Repo) ClaimDueDeliveries(ctx context.Context, limit int, lease time.Duration) ([]DueDelivery, error) {
	rows, err := r.db.Query(ctx, `
		WITH due AS (
		  SELECT d.id FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id
		  WHERE d.status = 'pending' AND d.next_attempt_at <= NOW() AND w.active
		  ORDER BY d.id LIMIT $1 FOR UPDATE OF d SKIP LOCKED
		)
		UPDATE webhook_deliveries d SET next_attempt_at = NOW() + $2::interval
		FROM due, webhooks w
		WHERE d.id = due.id AND w.id = d.webhook_id
		RETURNING d.id, d.webhook_id, d.event_type, d.body, d.attempts, w.url, w.secret, w.auth_header_name, w.auth_header_value`,
		limit, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row rowScanner, d *DueDelivery) error {
		return row.Scan(&d.ID, &d.WebhookID, &d.EventType, &d.Body, &d.Attempts, &d.URL, &d.Secret, &d.AuthHeaderName, &d.AuthHeaderValue)
	})
}

// FinishDelivery records an attempt. retryAt nil with ok false means the
// delivery has failed for good. disableAfter turns the webhook off after
// that many deliveries in a row have failed for good.
func (r *Repo) FinishDelivery(ctx context.Context, d DueDelivery, ok bool, status int, errMsg string, retryAt *time.Time, disableAfter int) error {
	var statusPtr *int
	if status != 0 {
		statusPtr = &status
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	state := "pending"
	switch {
	case ok:
		state = "delivered"
	case retryAt == nil:
		state = "failed"
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = $2, attempts = attempts + 1, last_status = $3, last_error = $4,
		next_attempt_at = COALESCE($5, next_attempt_at), delivered_at = CASE WHEN $2 = 'delivered' THEN NOW() END WHERE id = $1`,
		d.ID, state, statusPtr, errMsg, retryAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE webhooks SET last_delivery_at = NOW(), last_status = $2, last_error = $3,
		consecutive_failures = CASE WHEN $4 THEN 0 WHEN $5 THEN consecutive_failures + 1 ELSE consecutive_failures END,
		active = active AND NOT ($5 AND consecutive_failures + 1 >= $6)
		WHERE id = $1`, d.WebhookID, statusPtr, errMsg, ok, state == "failed", disableAfter); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListDeliveries returns a webhook's most recent deliveries.
func (r *Repo) ListDeliveries(ctx context.Context, webhookID, limit int) ([]domain.WebhookDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `SELECT id, webhook_id, event_type, body, status, attempts, next_attempt_at, last_status, last_error, created_at, delivered_at
		FROM webhook_deliveries WHERE webhook_id = $1 ORDER BY id DESC LIMIT $2`, webhookID, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row rowScanner, d *domain.WebhookDelivery) error {
		return row.Scan(&d.ID, &d.WebhookID, &d.EventType, &d.Body, &d.Status, &d.Attempts, &d.NextAttemptAt, &d.LastStatus, &d.LastError, &d.CreatedAt, &d.DeliveredAt)
	})
}

// PruneDeliveries deletes finished deliveries older than keep.
func (r *Repo) PruneDeliveries(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM webhook_deliveries WHERE status <> 'pending' AND created_at < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int(keep.Seconds())))
	return tag.RowsAffected(), err
}
