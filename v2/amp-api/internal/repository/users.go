package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Users & roles ----
// Identity/credentials are owned by Dex. This is JIT-provisioned identity +
// role assignment used inside amp-api for attribution/authorization.

// UpsertUserFromClaims JIT-provisions a user row on first (or every) validated
// request. If this is the very first user ever created, or their email is in
// bootstrapAdmins, they are granted the admin role automatically.
func (r *Repo) UpsertUserFromClaims(ctx context.Context, subject, email, displayName string, bootstrapAdmins map[string]bool) (*domain.User, error) {
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
		return nil, fmt.Errorf("upsert user: %w", err)
	}

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
