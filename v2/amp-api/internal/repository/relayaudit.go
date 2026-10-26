package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/simstech/amp-api/internal/domain"
)

// ---- amp-relay's audit entries and refresh-proof nonces (#2399) ----

// RelayAuditContext is the request context amp-relay records with a
// remote.* entry (#2433): what its public listener resolved (internal/reqctx)
// and how the caller signed in. relay_record_audit validates every member
// (migration 036); empty members are left out.
type RelayAuditContext struct {
	IP         string // one address, no prefix
	UserAgent  string // at most 512 characters
	RequestID  string // 1-128 of [A-Za-z0-9-]
	AuthMethod string // dex_session or oauth_mcp, with an actor only
	SessionID  string // the Dex session id (hex), with an actor only
}

func (c *RelayAuditContext) json() ([]byte, error) {
	if c == nil {
		return nil, nil
	}
	m := map[string]string{"client": "relay"}
	for k, v := range map[string]string{"ip": c.IP, "user_agent": c.UserAgent, "request_id": c.RequestID,
		"auth_method": c.AuthMethod, "session_id": c.SessionID} {
		if v != "" {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

// relayAuditCtxFallback is logged once when the database has no
// five-argument relay_record_audit (or the relay's role may not call it yet):
// amp-api hasn't applied migration 036 or re-granted the role.
var relayAuditCtxFallback sync.Once

// InsertRelayAudit records a remote.* audit entry the way amp-relay must:
// through relay_record_audit (migrations 030 and 036), which checks the
// action, the actor and the session, keeps only known detail members,
// validates the request context, fills the email, org, project, target and
// summary itself, and queues the entry for amp-api's webhook dispatcher. The
// relay's role has no INSERT on audit_log or webhook_deliveries (re-review
// #2394 N3). sessionID "" means the entry is about no one session; rc nil
// records no request context (the four-argument version). During a rolling
// deploy, before amp-api has applied 036 and re-granted the role, the entry
// is recorded without its context rather than not at all.
func (r *Repo) InsertRelayAudit(ctx context.Context, actorUserID *int, sessionID, action string, details map[string]any, rc *RelayAuditContext) (*domain.AuditEntry, error) {
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
	if rc != nil {
		cb, err := rc.json()
		if err != nil {
			return nil, err
		}
		err = r.db.QueryRow(ctx, `SELECT audit_id, audit_at FROM relay_record_audit($1, $2, $3, $4, $5)`,
			actorUserID, sid, action, b, cb).Scan(&e.ID, &e.At)
		var pgErr *pgconn.PgError
		if err == nil {
			return &e, nil
		}
		if !errors.As(err, &pgErr) || (pgErr.Code != "42883" && pgErr.Code != "42501") {
			return nil, fmt.Errorf("relay audit entry: %w", err)
		}
		relayAuditCtxFallback.Do(func() {
			slog.Warn("relay audit entries are recorded without request context until amp-api applies migration 036 and re-grants the relay role", "err", err)
		})
	}
	if err := r.db.QueryRow(ctx, `SELECT audit_id, audit_at FROM relay_record_audit($1, $2, $3, $4)`,
		actorUserID, sid, action, b).Scan(&e.ID, &e.At); err != nil {
		return nil, fmt.Errorf("relay audit entry: %w", err)
	}
	return &e, nil
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
