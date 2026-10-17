package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/simstech/amp-api/internal/domain"
)

// ---- Audit log (append-only) ----

func (r *Repo) InsertAudit(ctx context.Context, e domain.AuditEntry) (*domain.AuditEntry, error) {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	details, err := json.Marshal(e.Details)
	if err != nil {
		return nil, err
	}
	err = r.db.QueryRow(ctx, `
		INSERT INTO audit_log (actor_user_id, actor_email, via_token, org_id, project_id, action, target_type, target_id, summary, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id, at`,
		e.ActorUserID, e.ActorEmail, e.ViaToken, e.OrgID, e.ProjectID, e.Action, e.TargetType, e.TargetID, e.Summary, details,
	).Scan(&e.ID, &e.At)
	if err != nil {
		return nil, fmt.Errorf("insert audit entry: %w", err)
	}
	return &e, nil
}

// ListAudit returns entries newest first.
func (r *Repo) ListAudit(ctx context.Context, f domain.AuditFilter) ([]domain.AuditEntry, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.OrgID != 0 {
		o := arg(f.OrgID)
		where = append(where, "(org_id = "+o+" OR project_id IN (SELECT id FROM projects WHERE org_id = "+o+"))")
	}
	if f.ProjectID != 0 {
		where = append(where, "project_id = "+arg(f.ProjectID))
	}
	if f.ActorUserID != 0 {
		where = append(where, "actor_user_id = "+arg(f.ActorUserID))
	}
	if f.Action != "" {
		if strings.HasSuffix(f.Action, ".") {
			where = append(where, "starts_with(action, "+arg(f.Action)+")")
		} else {
			where = append(where, "action = "+arg(f.Action))
		}
	}
	if f.BeforeID != 0 {
		where = append(where, "id < "+arg(f.BeforeID))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, at, actor_user_id, actor_email, via_token, org_id, project_id, action, target_type, target_id, summary, details FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT " + arg(limit)
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	return collect(rows, func(row rowScanner, e *domain.AuditEntry) error {
		var details []byte
		if err := row.Scan(&e.ID, &e.At, &e.ActorUserID, &e.ActorEmail, &e.ViaToken, &e.OrgID, &e.ProjectID,
			&e.Action, &e.TargetType, &e.TargetID, &e.Summary, &details); err != nil {
			return err
		}
		return json.Unmarshal(details, &e.Details)
	})
}
