package repository

// Sealed project GitHub credentials (AMP #3154).
//
// A project can hold one GitHub credential per API host (migration 044's
// UNIQUE (project_id, host)); the facts cache references it by id. This file
// is the only place the token behind that row is written or read:
//
//   - the token is stored ONLY sealed (internal/secretbox, purpose
//     project_github_credentials.sealed_secret), and migration 045's CHECK
//     refuses anything that isn't a sealed value, so a plaintext token can't
//     reach a row even if a caller forgets to seal;
//   - sealing needs a key, and without one an attach or rotate is refused
//     (ErrSecretsKeyMissing) rather than downgraded to plaintext. That is
//     deliberately stricter than signing secrets, which keep working
//     unencrypted on an install that never configured a key: a token is a
//     credential to somebody else's system, and AMP never stores one it
//     can't encrypt;
//   - reads never hand the token to a request path: GetGitHubCredential
//     returns the row without it, and only GitHubCredentialSecret opens it,
//     which is meant for the facts reader (#3151) and the refresh
//     coordinator (#3156) and for nothing a user can call.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/secretbox"
)

// GitHubCredentialHost is the API host a credential is valid for when the
// project didn't name another one. Enterprise hosts stay explicitly
// unsupported until there is a host/credential decision (044), but the host
// is recorded so facts from two hosts never collide.
const GitHubCredentialHost = "github.com"

// ErrNoGitHubCredential: the project has no GitHub credential for that host.
var ErrNoGitHubCredential = errors.New("no GitHub credential is attached to this project")

// ErrGitHubCredentialNotSealed: a credential row holds a value that isn't a
// sealed secret. Migration 045's CHECK makes this unreachable through SQL;
// the error exists so a corrupted row is reported as such, never returned as
// if it were a token.
var ErrGitHubCredentialNotSealed = errors.New("the stored GitHub credential isn't a sealed value")

const githubCredentialCols = `id, project_id, host, label, created_by, created_at, rotated_at`

func scanGitHubCredential(row rowScanner, c *domain.GitHubCredential) error {
	return row.Scan(&c.ID, &c.ProjectID, &c.Host, &c.Label, &c.CreatedBy, &c.CreatedAt, &c.RotatedAt)
}

// sealGitHubCredential seals a token for storage. Unlike signing secrets it
// never falls back to plaintext: no key means the token can't be stored.
func (r *Repo) sealGitHubCredential(token string) (string, error) {
	if token == "" {
		return "", errors.New("a GitHub credential needs a token")
	}
	if !r.secrets().CanSeal() {
		return "", ErrSecretsKeyMissing
	}
	return r.secrets().Seal(secretbox.PurposeGitHubCredential, token)
}

// GetGitHubCredential returns projectID's credential for host WITHOUT its
// token: id, host, label and lifecycle only. It returns
// ErrNoGitHubCredential when there is none.
func (r *Repo) GetGitHubCredential(ctx context.Context, projectID int, host string) (*domain.GitHubCredential, error) {
	c := &domain.GitHubCredential{}
	err := scanGitHubCredential(r.db.QueryRow(ctx, `SELECT `+githubCredentialCols+`
		FROM project_github_credentials WHERE project_id = $1 AND host = $2`, projectID, credentialHost(host)), c)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoGitHubCredential
	}
	return c, err
}

// GitHubCredentialSecret returns projectID's token for host in plaintext.
// Only the facts reader and the refresh coordinator call it; nothing that
// answers a user request may. The error never contains the stored value.
func (r *Repo) GitHubCredentialSecret(ctx context.Context, projectID int, host string) (string, error) {
	var stored string
	err := r.db.QueryRow(ctx, `SELECT sealed_secret FROM project_github_credentials
		WHERE project_id = $1 AND host = $2`, projectID, credentialHost(host)).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoGitHubCredential
	}
	if err != nil {
		return "", err
	}
	// '' is a credential context with no token (044's reference metadata):
	// there is nothing to open, and claiming otherwise would make a tokenless
	// credential read as a verified one.
	if stored == "" {
		return "", ErrNoGitHubCredential
	}
	if !secretbox.IsSealed(stored) {
		return "", ErrGitHubCredentialNotSealed
	}
	return r.secrets().Open(secretbox.PurposeGitHubCredential, stored)
}

// SetGitHubCredential attaches projectID's token for host, or replaces it in
// place (a rotation), and returns the stored row without the token. by is
// who attached or rotated it. rotated_at is set only on a replacement, so
// "attached once" and "rotated since" stay distinguishable.
func (r *Repo) SetGitHubCredential(ctx context.Context, projectID int, host, label, token string, by *int) (*domain.GitHubCredential, error) {
	sealed, err := r.sealGitHubCredential(token)
	if err != nil {
		return nil, err
	}
	c := &domain.GitHubCredential{}
	err = scanGitHubCredential(r.db.QueryRow(ctx, `INSERT INTO project_github_credentials
			(project_id, host, label, sealed_secret, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (project_id, host) DO UPDATE SET
			label = EXCLUDED.label,
			sealed_secret = EXCLUDED.sealed_secret,
			rotated_at = NOW()
		RETURNING `+githubCredentialCols,
		projectID, credentialHost(host), label, sealed, by), c)
	return c, err
}

// DeleteGitHubCredential removes projectID's credential for host and, with
// it, every fact it produced (044's composite foreign key cascades). It
// reports whether there was one.
func (r *Repo) DeleteGitHubCredential(ctx context.Context, projectID int, host string) (bool, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM project_github_credentials WHERE project_id = $1 AND host = $2`,
		projectID, credentialHost(host))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ResealGitHubCredentialSecrets brings every stored project GitHub credential
// under the current key, as ResealGitHubHookSecrets does for webhook secrets.
// It returns how many it sealed and how many it couldn't open. Rows with no
// sealed secret (044's reference metadata) are left alone: there is nothing
// to open.
func (r *Repo) ResealGitHubCredentialSecrets(ctx context.Context) (resealed, unopenable int, err error) {
	rows, err := r.db.Query(ctx, `SELECT id, sealed_secret FROM project_github_credentials
		WHERE sealed_secret <> '' ORDER BY id`)
	if err != nil {
		return 0, 0, fmt.Errorf("read project GitHub credentials: %w", err)
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
		return 0, 0, fmt.Errorf("read project GitHub credentials: %w", err)
	}
	for _, s := range all {
		plain, err := r.secrets().Open(secretbox.PurposeGitHubCredential, s.secret)
		if err != nil {
			unopenable++
			continue
		}
		if !r.secrets().NeedsReseal(s.secret) {
			continue
		}
		sealed, err := r.secrets().Seal(secretbox.PurposeGitHubCredential, plain)
		if err != nil {
			return resealed, unopenable, err
		}
		tag, err := r.db.Exec(ctx, `UPDATE project_github_credentials SET sealed_secret = $3
			WHERE id = $1 AND sealed_secret = $2`, s.id, s.secret, sealed)
		if err != nil {
			return resealed, unopenable, fmt.Errorf("seal GitHub credential %d: %w", s.id, err)
		}
		if tag.RowsAffected() == 1 {
			resealed++
		}
	}
	return resealed, unopenable, nil
}

// credentialHost defaults an empty host to GitHubCredentialHost, so callers
// never have to spell it out and "" can't become a second row next to
// 'github.com'.
func credentialHost(host string) string {
	if host == "" {
		return GitHubCredentialHost
	}
	return host
}
