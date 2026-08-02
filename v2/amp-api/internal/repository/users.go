package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Users & roles ----
// Identity/credentials are owned by Dex. This is JIT-provisioned identity +
// role assignment used inside amp-api for attribution/authorization.

// UpsertUserFromClaims JIT-provisions a user row on first (or every) validated
// request. If this is the very first user ever created, or their email is in
// bootstrapAdmins, they are granted the admin role automatically.
//
// The upsert is deliberately tolerant of two real-world edge cases:
//
//  1. The same OIDC subject arriving with a changed email (normal upsert via
//     ON CONFLICT (subject) DO UPDATE).
//  2. A *new* subject arriving with an email that already belongs to an
//     existing row — this happens when Dex tokens arrive without an email
//     claim (empty string), or when a user's Dex identity is recreated with a
//     different subject but the same email. In that case the ON CONFLICT
//     (subject) path fails with a unique violation on users_email_key. We
//     fall back to updating the existing row's subject and other fields where
//     the email matches, so the provisioning succeeds instead of failing
//     forever on every subsequent request (this was a real prod incident —
//     every request from that client would error with SQLSTATE 23505).
//
//  Empty emails are short-circuited entirely: provisioning with an empty
//  email would either collide with an existing empty-email row (id 9356 was
//  created this way in prod) or create a meaningless row. We skip provisioning
//  and return a sentinel error so callers can handle it (e.g., log and
//  continue without an identity rather than failing the request).
func (r *Repo) UpsertUserFromClaims(ctx context.Context, subject, email, displayName string, bootstrapAdmins map[string]bool) (*domain.User, error) {
	if email == "" {
		return nil, errors.New("upsert user: empty email claim — skipping provisioning")
	}

	u := &domain.User{}
	err := r.db.QueryRow(ctx,
		`INSERT INTO users (subject, email, display_name)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (subject) DO UPDATE SET
		   email = EXCLUDED.email,
		   display_name = CASE WHEN EXCLUDED.display_name != '' THEN EXCLUDED.display_name ELSE users.display_name END,
		   last_seen_at = NOW()
		 RETURNING id, subject, email, display_name, created_at, last_seen_at`,
		subject, email, displayName,
	).Scan(&u.ID, &u.Subject, &u.Email, &u.DisplayName, &u.CreatedAt, &u.LastSeenAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_key" {
			// A new subject arrived with an email that already exists — fall
			// back to updating the existing row's subject and other fields
			// instead of failing forever.
			return r.updateExistingUserByEmail(ctx, subject, email, displayName, bootstrapAdmins)
		}
		return nil, fmt.Errorf("upsert user: %w", err)
	}

	return r.finishUpsert(ctx, u, email, bootstrapAdmins)
}

// updateExistingUserByEmail handles the fallback path when a new OIDC subject
// presents an email that already belongs to an existing user row. We update
// that row's subject (and display name if provided) so the provisioning
// succeeds, then continue with the normal role-granting flow.
func (r *Repo) updateExistingUserByEmail(ctx context.Context, subject, email, displayName string, bootstrapAdmins map[string]bool) (*domain.User, error) {
	u := &domain.User{}
	err := r.db.QueryRow(ctx,
		`UPDATE users
		 SET subject = $1,
		     display_name = CASE WHEN $3 != '' THEN $3 ELSE display_name END,
		     last_seen_at = NOW()
		 WHERE email = $2
		 RETURNING id, subject, email, display_name, created_at, last_seen_at`,
		subject, email, displayName,
	).Scan(&u.ID, &u.Subject, &u.Email, &u.DisplayName, &u.CreatedAt, &u.LastSeenAt)
	if err != nil {
		return nil, fmt.Errorf("update existing user by email: %w", err)
	}
	return r.finishUpsert(ctx, u, email, bootstrapAdmins)
}

// finishUpsert completes the provisioning flow after a successful user row
// upsert: fetches roles, grants the initial role if needed, and returns the
// populated user object.
func (r *Repo) finishUpsert(ctx context.Context, u *domain.User, email string, bootstrapAdmins map[string]bool) (*domain.User, error) {
	roles, err := r.getUserRoles(ctx, u.ID)
	if err != nil {
		return nil, err
	}

	if len(roles) == 0 {
		grantAdmin := bootstrapAdmins[email]
		if !grantAdmin {
			var totalUsers int
			if err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&totalUsers); err != nil {
				return nil, fmt.Errorf("count users: %w", err)
			}
			grantAdmin = totalUsers == 1 // this upsert just created the very first user
		}
		role := domain.RoleMember
		if grantAdmin {
			role = domain.RoleAdmin
		}
		if _, err := r.db.Exec(ctx,
			`INSERT INTO user_roles (user_id, role) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			u.ID, role,
		); err != nil {
			return nil, fmt.Errorf("grant initial role: %w", err)
		}
		roles = []string{role}
	}

	u.Roles = roles
	return u, nil
}

func (r *Repo) getUserRoles(ctx context.Context, userID int) ([]string, error) {
	rows, err := r.db.Query(ctx, `SELECT role FROM user_roles WHERE user_id = $1 ORDER BY role`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user roles: %w", err)
	}
	defer rows.Close()

	roles := make([]string, 0)
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

// GetUserBySubject fetches a user (with roles) by their Dex "sub" claim.
func (r *Repo) GetUserBySubject(ctx context.Context, subject string) (*domain.User, error) {
	u := &domain.User{}
	err := r.db.QueryRow(ctx,
		`SELECT id, subject, email, display_name, created_at, last_seen_at
		 FROM users WHERE subject = $1`, subject,
	).Scan(&u.ID, &u.Subject, &u.Email, &u.DisplayName, &u.CreatedAt, &u.LastSeenAt)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, err
	}
	roles, err := r.getUserRoles(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	u.Roles = roles
	return u, nil
}

// GetUserByEmail fetches a user (with roles) by email.
func (r *Repo) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	u := &domain.User{}
	err := r.db.QueryRow(ctx,
		`SELECT id, subject, email, display_name, created_at, last_seen_at
		 FROM users WHERE email = $1`, email,
	).Scan(&u.ID, &u.Subject, &u.Email, &u.DisplayName, &u.CreatedAt, &u.LastSeenAt)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, err
	}
	roles, err := r.getUserRoles(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	u.Roles = roles
	return u, nil
}

// ListUsers returns every provisioned user with their roles, newest first.
func (r *Repo) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, subject, email, display_name, created_at, last_seen_at
		 FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	out := make([]domain.User, 0)
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.Subject, &u.Email, &u.DisplayName, &u.CreatedAt, &u.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		roles, err := r.getUserRoles(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Roles = roles
	}
	return out, nil
}

// SetUserRole grants or revokes a role for a user.
func (r *Repo) SetUserRole(ctx context.Context, userID int, role string, grant bool) error {
	if role != domain.RoleAdmin && role != domain.RoleMember {
		return fmt.Errorf("invalid role %q", role)
	}
	var err error
	if grant {
		_, err = r.db.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, role)
	} else {
		_, err = r.db.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1 AND role = $2`, userID, role)
	}
	return err
}

// DeleteUser removes a JIT-provisioned user row (does not touch Dex credentials).
func (r *Repo) DeleteUser(ctx context.Context, userID int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	return err
}
