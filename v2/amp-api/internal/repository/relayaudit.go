package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// ---- amp-relay's audit entries and refresh-proof nonces (#2399) ----

// InsertRelayAudit records a remote.* audit entry the way amp-relay must:
// through relay_record_audit (migration 030), which checks the action, the
// actor and the session, keeps only known detail members, fills the email,
// org, project, target and summary itself, and queues the entry for amp-api's
// webhook dispatcher (ClaimRelayAudits). The relay's role has no INSERT on
// audit_log or webhook_deliveries (re-review #2394 N3). sessionID "" means
// the entry is about no one session.
func (r *Repo) InsertRelayAudit(ctx context.Context, actorUserID *int, sessionID, action string, details map[string]any) (*domain.AuditEntry, error) {
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	var sid *string
	if sessionID != "" {
		sid = &sessionID
	}
	e := domain.AuditEntry{ActorUserID: actorUserID, Action: action, Details: details}
	if err := r.db.QueryRow(ctx, `SELECT audit_id, audit_at FROM relay_record_audit($1, $2, $3, $4)`,
		actorUserID, sid, action, b).Scan(&e.ID, &e.At); err != nil {
		return nil, fmt.Errorf("relay audit entry: %w", err)
	}
	return &e, nil
}

// ClaimRelayAudits takes up to limit entries amp-relay recorded off the queue
// and returns them, oldest first, for amp-api's webhook dispatcher. Each entry
// is taken by exactly one amp-api replica (SKIP LOCKED); like an event still
// in a dispatcher's memory, an entry taken by a replica that dies before
// matching it is not delivered.
func (r *Repo) ClaimRelayAudits(ctx context.Context, limit int) ([]domain.AuditEntry, error) {
	rows, err := r.db.Query(ctx, `
		WITH taken AS (
			DELETE FROM relay_audit_outbox WHERE audit_id IN (
				SELECT audit_id FROM relay_audit_outbox ORDER BY audit_id LIMIT $1 FOR UPDATE SKIP LOCKED)
			RETURNING audit_id)
		SELECT id, at, actor_user_id, actor_email, via_token, org_id, project_id, action, target_type, target_id, summary, details
		FROM audit_log WHERE id IN (SELECT audit_id FROM taken) ORDER BY id`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim relay audit entries: %w", err)
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

// UseProofNonce records a verified refresh proof's nonce for sessionID until
// expires, and reports whether it was already recorded and not yet expired:
// a replay, on this replica or any other (re-review #2394 N6). now is the
// relay's clock.
func (r *Repo) UseProofNonce(ctx context.Context, sessionID, nonce string, now, expires time.Time) (replayed bool, err error) {
	tag, err := r.db.Exec(ctx, `
		INSERT INTO remote_proof_nonces (session_id, nonce, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (session_id, nonce) DO UPDATE SET expires_at = EXCLUDED.expires_at
		WHERE remote_proof_nonces.expires_at <= $4`, sessionID, nonce, expires, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 0, nil
}

// PruneProofNonces forgets nonces that expired before now.
func (r *Repo) PruneProofNonces(ctx context.Context, now time.Time) error {
	_, err := r.db.Exec(ctx, `DELETE FROM remote_proof_nonces WHERE expires_at <= $1`, now)
	return err
}
