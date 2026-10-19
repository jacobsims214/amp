package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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

// RecentAudit reports whether actor (by user id, or email when there's no
// user) recorded action on target since `since`: for de-duplicating views.
func (r *Repo) RecentAudit(ctx context.Context, action, targetType, targetID string, actorUserID *int, actorEmail string, since time.Time) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_log WHERE action = $1 AND target_type = $2 AND target_id = $3
		AND ((actor_user_id IS NOT NULL AND actor_user_id = $4) OR ($4::int IS NULL AND actor_email = $5)) AND at > $6)`,
		action, targetType, targetID, actorUserID, actorEmail, since).Scan(&ok)
	return ok, err
}

// TargetAudit lists the audit entries about one target, newest first.
func (r *Repo) TargetAudit(ctx context.Context, targetType, targetID string, limit int) ([]domain.AuditEntry, error) {
	rows, err := r.db.Query(ctx, `SELECT id, at, actor_user_id, actor_email, via_token, org_id, project_id, action, target_type, target_id, summary, details
		FROM audit_log WHERE target_type = $1 AND target_id = $2 ORDER BY at DESC, id DESC LIMIT $3`, targetType, targetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AuditEntry{}
	for rows.Next() {
		var e domain.AuditEntry
		var details []byte
		if err := rows.Scan(&e.ID, &e.At, &e.ActorUserID, &e.ActorEmail, &e.ViaToken, &e.OrgID, &e.ProjectID,
			&e.Action, &e.TargetType, &e.TargetID, &e.Summary, &details); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(details, &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountArtifactVersions is how many versions an artifact has.
func (r *Repo) CountArtifactVersions(ctx context.Context, id int) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM artifact_versions WHERE artifact_id = $1`, id).Scan(&n)
	return n, err
}
