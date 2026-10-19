package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Agent accounts, deactivation, API tokens ----

// CreateAgentUser adds an agent account managed by managedBy, with the
// member role.
func (r *Repo) CreateAgentUser(ctx context.Context, subject, email, displayName string, managedBy *int) (*domain.User, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	u := &domain.User{}
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (subject, email, display_name, kind, managed_by) VALUES ($1, $2, $3, 'agent', $4)
		 RETURNING `+userCols, subject, email, displayName, managedBy,
	).Scan(userDest(u)...); err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'member')`, u.ID); err != nil {
		return nil, err
	}
	u.Roles = []string{domain.RoleMember}
	return u, tx.Commit(ctx)
}

// ListAgentUsers returns agent accounts, newest first: those managedBy
// manages, or every one when managedBy is nil.
func (r *Repo) ListAgentUsers(ctx context.Context, managedBy *int) ([]domain.User, error) {
	rows, err := r.db.Query(ctx, `SELECT `+userCols+` FROM users
		WHERE kind = 'agent' AND ($1::int IS NULL OR managed_by = $1) ORDER BY created_at DESC`, managedBy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.User{}
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(userDest(&u)...); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserDisabled deactivates or reactivates an account. Deactivating the
// last active global admin is refused.
//
// Either way, the account's live API tokens are revoked in the same
// transaction and returned, so the caller can audit each one. Deactivating
// revokes them so that reactivating doesn't bring them back. Reactivating
// revokes any that are still live: those belong to accounts deactivated
// before deactivation revoked tokens (tokens can't be minted for a
// deactivated account), and this is the moment they would start working
// again. No migration is needed for those accounts.
func (r *Repo) SetUserDisabled(ctx context.Context, userID int, disabled bool) ([]domain.APIToken, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if disabled {
		var others int
		var admin bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id = $1 AND role = 'admin'),
			       (SELECT COUNT(*) FROM user_roles ur JOIN users u ON u.id = ur.user_id
			         WHERE ur.role = 'admin' AND u.disabled_at IS NULL AND u.id <> $1)`, userID,
		).Scan(&admin, &others); err != nil {
			return nil, err
		}
		if admin && others == 0 {
			return nil, conflictf("this is the only active global admin; make someone else a global admin first")
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET disabled_at = COALESCE(disabled_at, NOW()) WHERE id = $1`, userID); err != nil {
			return nil, err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE users SET disabled_at = NULL WHERE id = $1`, userID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `UPDATE api_tokens SET revoked_at = NOW()
		WHERE user_id = $1 AND revoked_at IS NULL RETURNING `+tokenCols, userID)
	if err != nil {
		return nil, err
	}
	revoked, err := collect(rows, scanToken)
	if err != nil {
		return nil, err
	}
	return revoked, tx.Commit(ctx)
}

const tokenCols = `id, user_id, name, prefix, created_by, created_at, expires_at, last_used_at, revoked_at`

func scanToken(row rowScanner, t *domain.APIToken) error {
	return row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt)
}

// CreateAPIToken stores a token's hash.
func (r *Repo) CreateAPIToken(ctx context.Context, userID int, name, hash, prefix string, createdBy *int, expiresAt *time.Time) (*domain.APIToken, error) {
	t := &domain.APIToken{}
	err := scanToken(r.db.QueryRow(ctx,
		`INSERT INTO api_tokens (user_id, name, token_hash, prefix, created_by, expires_at) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+tokenCols, userID, name, hash, prefix, createdBy, expiresAt), t)
	return t, err
}

// ListAPITokens returns a user's tokens, revoked ones included, newest first.
func (r *Repo) ListAPITokens(ctx context.Context, userID int) ([]domain.APIToken, error) {
	rows, err := r.db.Query(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanToken)
}

func (r *Repo) GetAPIToken(ctx context.Context, id int) (*domain.APIToken, error) {
	t := &domain.APIToken{}
	err := scanToken(r.db.QueryRow(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE id = $1`, id), t)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("token %d: %w", id, ErrNotFound)
	}
	return t, err
}

func (r *Repo) RevokeAPIToken(ctx context.Context, id int) error {
	_, err := r.db.Exec(ctx, `UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, NOW()) WHERE id = $1`, id)
	return err
}

// UserForAPIToken returns the account a live token belongs to, and notes
// the token's use. A revoked, expired, or unknown token is ErrNotFound.
//
// A deactivated account is returned for any of its tokens, live or not, and
// its use isn't noted: the caller refuses it with "account deactivated",
// which says more than "revoked token" would (deactivation revokes the
// account's tokens, so otherwise that's all its agents would ever see).
func (r *Repo) UserForAPIToken(ctx context.Context, hash string) (*domain.User, error) {
	var tokenID int
	u := &domain.User{}
	err := r.db.QueryRow(ctx, `
		SELECT t.id, `+prefixed("u", userCols)+`
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1 AND (u.disabled_at IS NOT NULL
		      OR (t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > NOW())))`, hash,
	).Scan(append([]any{&tokenID}, userDest(u)...)...)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if u.DisabledAt != nil {
		return u, nil
	}
	// At most one write a minute per token, however busy the agent is.
	if _, err := r.db.Exec(ctx, `UPDATE api_tokens SET last_used_at = NOW()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < NOW() - interval '1 minute')`, tokenID); err != nil {
		return nil, err
	}
	if _, err := r.db.Exec(ctx, `UPDATE users SET last_seen_at = NOW()
		WHERE id = $1 AND last_seen_at < NOW() - interval '1 minute'`, u.ID); err != nil {
		return nil, err
	}
	if u.Roles, err = r.getUserRoles(ctx, u.ID); err != nil {
		return nil, err
	}
	return u, nil
}

// prefixed qualifies a comma-separated column list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
