package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
	before, err := jsonOrNil(e.Before)
	if err != nil {
		return nil, err
	}
	after, err := jsonOrNil(e.After)
	if err != nil {
		return nil, err
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = domain.AuditSchemaVersion
	}
	if e.Outcome == "" {
		e.Outcome = domain.AuditOutcomeSuccess
	}
	err = r.db.QueryRow(ctx, `
		INSERT INTO audit_log (actor_user_id, actor_email, via_token, org_id, project_id, action, target_type, target_id, summary, details,
			schema_version, actor_type, auth_method, token_id, session_id, on_behalf_of_user_id, agent_profile, target_name,
			outcome, reason, source_ip, user_agent, client, request_id, trace_id, "before", "after")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14, NULLIF($15, ''), $16, NULLIF($17, ''), NULLIF($18, ''),
			$19, $20, NULLIF($21, '')::inet, NULLIF($22, ''), $23, NULLIF($24, ''), NULLIF($25, ''), $26, $27)
		RETURNING id, at`,
		e.ActorUserID, e.ActorEmail, e.ViaToken, e.OrgID, e.ProjectID, e.Action, e.TargetType, e.TargetID, e.Summary, details,
		e.SchemaVersion, e.ActorType, e.AuthMethod, e.TokenID, e.SessionID, e.OnBehalfOfUserID, e.AgentProfile, e.TargetName,
		e.Outcome, e.Reason, e.SourceIP, e.UserAgent, e.Client, e.RequestID, e.TraceID, before, after,
	).Scan(&e.ID, &e.At)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42703" {
		// undefined_column: this process is newer than the database
		// (authadmin doesn't migrate, so it can start before amp-api has
		// applied 031). Keep the row in the v1 shape rather than lose it.
		e.SchemaVersion = 1
		err = r.db.QueryRow(ctx, `
			INSERT INTO audit_log (actor_user_id, actor_email, via_token, org_id, project_id, action, target_type, target_id, summary, details)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id, at`,
			e.ActorUserID, e.ActorEmail, e.ViaToken, e.OrgID, e.ProjectID, e.Action, e.TargetType, e.TargetID, e.Summary, details,
		).Scan(&e.ID, &e.At)
	}
	if err != nil {
		return nil, fmt.Errorf("insert audit entry: %w", err)
	}
	return &e, nil
}

func jsonOrNil(m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// auditCols is every audit_log column, in scanAudit's order. Nullable
// text columns read as "".
const auditCols = `id, at, actor_user_id, actor_email, via_token, org_id, project_id, action, target_type, target_id, summary, details,
	schema_version, actor_type, auth_method, token_id, COALESCE(session_id, ''), on_behalf_of_user_id, COALESCE(agent_profile, ''),
	COALESCE(target_name, ''), outcome, reason, COALESCE(host(source_ip), ''), COALESCE(user_agent, ''), client,
	COALESCE(request_id, ''), COALESCE(trace_id, ''), "before", "after"`

func scanAudit(row rowScanner, e *domain.AuditEntry) error {
	var details, before, after []byte
	if err := row.Scan(&e.ID, &e.At, &e.ActorUserID, &e.ActorEmail, &e.ViaToken, &e.OrgID, &e.ProjectID,
		&e.Action, &e.TargetType, &e.TargetID, &e.Summary, &details,
		&e.SchemaVersion, &e.ActorType, &e.AuthMethod, &e.TokenID, &e.SessionID, &e.OnBehalfOfUserID, &e.AgentProfile,
		&e.TargetName, &e.Outcome, &e.Reason, &e.SourceIP, &e.UserAgent, &e.Client,
		&e.RequestID, &e.TraceID, &before, &after); err != nil {
		return err
	}
	if err := json.Unmarshal(details, &e.Details); err != nil {
		return err
	}
	if before != nil {
		if err := json.Unmarshal(before, &e.Before); err != nil {
			return err
		}
	}
	if after != nil {
		if err := json.Unmarshal(after, &e.After); err != nil {
			return err
		}
	}
	return nil
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
	q := `SELECT ` + auditCols + ` FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT " + arg(limit)
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	return collect(rows, scanAudit)
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
	rows, err := r.db.Query(ctx, `SELECT `+auditCols+`
		FROM audit_log WHERE target_type = $1 AND target_id = $2 ORDER BY at DESC, id DESC LIMIT $3`, targetType, targetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AuditEntry{}
	for rows.Next() {
		var e domain.AuditEntry
		if err := scanAudit(rows, &e); err != nil {
			return nil, err
		}
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

// AuditLoginSeen reports whether an auth.login row already exists for the
// Dex session sessionID (any replica wrote it), so a sign-in is recorded
// once per session (index idx_audit_login_session, migration 032).
func (r *Repo) AuditLoginSeen(ctx context.Context, sessionID string) (bool, error) {
	var seen bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM audit_log WHERE action = 'auth.login' AND session_id = $1)`, sessionID).Scan(&seen)
	return seen, err
}

// AuditTokenIPSeen reports whether any audit row since since was made with
// API token tokenID from address ip (index idx_audit_token_ip, migration
// 032). token.use.new_ip is written when this is false.
func (r *Repo) AuditTokenIPSeen(ctx context.Context, tokenID int64, ip string, since time.Time) (bool, error) {
	var seen bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM audit_log WHERE token_id = $1 AND source_ip = $2::inet AND at >= $3)`, tokenID, ip, since).Scan(&seen)
	return seen, err
}
