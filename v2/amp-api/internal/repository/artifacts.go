package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/domain"
)

const artifactCols = `a.id, a.project_id, a.task_id, COALESCE(t.name, ''), COALESCE(p.code, ''), a.title, a.kind, a.status,
	a.current_version, a.approved_version, a.reviewed_by, COALESCE(ru.email, ''), a.reviewed_at,
	a.created_by, COALESCE(cu.email, ''), a.created_at, a.updated_at,
	a.keep, a.kept_by, a.kept_at, a.expires_at, a.expiry_dismissed_at`

const artifactFrom = ` FROM artifacts a
	JOIN projects p ON p.id = a.project_id
	LEFT JOIN tasks t ON t.id = a.task_id
	LEFT JOIN users ru ON ru.id = a.reviewed_by
	LEFT JOIN users cu ON cu.id = a.created_by`

func scanArtifact(row pgx.Row, a *domain.Artifact) error {
	var code string
	if err := row.Scan(&a.ID, &a.ProjectID, &a.TaskID, &a.TaskName, &code, &a.Title, &a.Kind, &a.Status,
		&a.CurrentVersion, &a.ApprovedVersion, &a.ReviewedBy, &a.ReviewedByEmail, &a.ReviewedAt,
		&a.CreatedBy, &a.CreatedByEmail, &a.CreatedAt, &a.UpdatedAt,
		&a.Keep, &a.KeptBy, &a.KeptAt, &a.ExpiresAt, &a.ExpiryDismissedAt); err != nil {
		return err
	}
	if a.TaskID != nil {
		a.TaskRef = domain.TaskRef(code, *a.TaskID)
	}
	return nil
}

// CreateArtifact stores a new artifact with its first version.
func (r *Repo) CreateArtifact(ctx context.Context, a domain.Artifact, content []byte, contentType, note string, expires time.Time) (*domain.Artifact, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id int
	if err := tx.QueryRow(ctx, `INSERT INTO artifacts (project_id, task_id, title, kind, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, a.ProjectID, a.TaskID, a.Title, a.Kind, a.CreatedBy, expires).Scan(&id); err != nil {
		return nil, err
	}
	if err := insertArtifactVersion(ctx, tx, id, 1, content, contentType, note, a.CreatedBy); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.GetArtifact(ctx, id)
}

// AddArtifactVersion stores a new version and puts the artifact back in review.
// A new title replaces the old one when given.
func (r *Repo) AddArtifactVersion(ctx context.Context, id int, title string, content []byte, contentType, note string, by *int, expires time.Time) (*domain.Artifact, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var next int
	if err := tx.QueryRow(ctx, `UPDATE artifacts SET current_version = current_version + 1, status = 'in_review',
		title = COALESCE(NULLIF($2, ''), title), updated_at = NOW(), expires_at = $3, expiry_dismissed_at = NULL
		WHERE id = $1 RETURNING current_version`, id, title, expires).Scan(&next); err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("artifact %d not found", id)
		}
		return nil, err
	}
	if err := insertArtifactVersion(ctx, tx, id, next, content, contentType, note, by); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.GetArtifact(ctx, id)
}

func insertArtifactVersion(ctx context.Context, tx pgx.Tx, id, version int, content []byte, contentType, note string, by *int) error {
	sum := sha256.Sum256(content)
	_, err := tx.Exec(ctx, `INSERT INTO artifact_versions (artifact_id, version, content, content_type, size_bytes, sha256, note, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, version, content, contentType, len(content), hex.EncodeToString(sum[:]), note, by)
	return err
}

// GetArtifact loads one artifact (without content).
func (r *Repo) GetArtifact(ctx context.Context, id int) (*domain.Artifact, error) {
	a := &domain.Artifact{}
	err := scanArtifact(r.db.QueryRow(ctx, `SELECT `+artifactCols+artifactFrom+` WHERE a.id = $1`, id), a)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("artifact %d not found", id)
	}
	return a, err
}

// ListArtifacts lists a project's artifacts, newest activity first; taskID
// narrows it to one ticket's.
func (r *Repo) ListArtifacts(ctx context.Context, projectID int, taskID *int) ([]domain.Artifact, error) {
	q := `SELECT ` + artifactCols + artifactFrom + ` WHERE a.project_id = $1`
	args := []interface{}{projectID}
	if taskID != nil {
		q += ` AND a.task_id = $2`
		args = append(args, *taskID)
	}
	rows, err := r.db.Query(ctx, q+` ORDER BY a.updated_at DESC, a.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Artifact{}
	for rows.Next() {
		var a domain.Artifact
		if err := scanArtifact(rows, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListArtifactVersions lists an artifact's versions, newest first, without content.
func (r *Repo) ListArtifactVersions(ctx context.Context, id int) ([]domain.ArtifactVersion, error) {
	rows, err := r.db.Query(ctx, `SELECT v.artifact_id, v.version, v.content_type, v.size_bytes, v.sha256, v.note,
		v.created_by, COALESCE(u.email, ''), v.created_at
		FROM artifact_versions v LEFT JOIN users u ON u.id = v.created_by
		WHERE v.artifact_id = $1 ORDER BY v.version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ArtifactVersion{}
	for rows.Next() {
		var v domain.ArtifactVersion
		if err := rows.Scan(&v.ArtifactID, &v.Version, &v.ContentType, &v.SizeBytes, &v.SHA256, &v.Note,
			&v.CreatedBy, &v.CreatedByEmail, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetArtifactVersion loads one version with its content; version 0 means the current one.
func (r *Repo) GetArtifactVersion(ctx context.Context, id, version int) (*domain.ArtifactVersion, error) {
	v := &domain.ArtifactVersion{}
	err := r.db.QueryRow(ctx, `SELECT v.artifact_id, v.version, v.content_type, v.size_bytes, v.sha256, v.note,
		v.created_by, COALESCE(u.email, ''), v.created_at, v.content
		FROM artifact_versions v LEFT JOIN users u ON u.id = v.created_by
		WHERE v.artifact_id = $1 AND v.version = CASE WHEN $2 > 0 THEN $2 ELSE (SELECT current_version FROM artifacts WHERE id = $1) END`,
		id, version).Scan(&v.ArtifactID, &v.Version, &v.ContentType, &v.SizeBytes, &v.SHA256, &v.Note,
		&v.CreatedBy, &v.CreatedByEmail, &v.CreatedAt, &v.Content)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("artifact %d has no version %d", id, version)
	}
	return v, err
}

// ReviewArtifact records a person's verdict on a version.
func (r *Repo) ReviewArtifact(ctx context.Context, id, version int, status string, by *int) (*domain.Artifact, error) {
	var approved *int
	if status == domain.ArtifactApproved {
		approved = &version
	}
	tag, err := r.db.Exec(ctx, `UPDATE artifacts SET status = $2,
		approved_version = CASE WHEN $3::int IS NOT NULL THEN $3 ELSE approved_version END,
		reviewed_by = $4, reviewed_at = NOW(), updated_at = NOW() WHERE id = $1`, id, status, approved, by)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("artifact %d not found", id)
	}
	return r.GetArtifact(ctx, id)
}

// KeepArtifact keeps an artifact (it never expires) or lets it expire again at expires.
func (r *Repo) KeepArtifact(ctx context.Context, id int, keep bool, by *int, expires time.Time) (*domain.Artifact, error) {
	var err error
	if keep {
		_, err = r.db.Exec(ctx, `UPDATE artifacts SET keep = TRUE, kept_by = $2, kept_at = NOW() WHERE id = $1`, id, by)
	} else {
		_, err = r.db.Exec(ctx, `UPDATE artifacts SET keep = FALSE, kept_by = NULL, kept_at = NULL,
			expires_at = GREATEST(COALESCE(expires_at, $2), $2), expiry_dismissed_at = NULL WHERE id = $1`, id, expires)
	}
	if err != nil {
		return nil, err
	}
	return r.GetArtifact(ctx, id)
}

// DismissArtifactExpiry records that the warning was seen and the artifact may expire.
func (r *Repo) DismissArtifactExpiry(ctx context.Context, id int) error {
	_, err := r.db.Exec(ctx, `UPDATE artifacts SET expiry_dismissed_at = NOW() WHERE id = $1`, id)
	return err
}

// ExpiringArtifactsFor lists artifacts a user created, or that belong to a
// ticket they own, that expire before `before` and that nobody kept, and
// whose warning they haven't dismissed. Soonest first.
func (r *Repo) ExpiringArtifactsFor(ctx context.Context, userID int, before time.Time) ([]domain.Artifact, error) {
	rows, err := r.db.Query(ctx, `SELECT `+artifactCols+artifactFrom+`
		WHERE NOT a.keep AND a.expiry_dismissed_at IS NULL AND a.expires_at < $2
		AND (a.created_by = $1 OR t.owner_user_id = $1)
		ORDER BY a.expires_at, a.id`, userID, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Artifact{}
	for rows.Next() {
		var a domain.Artifact
		if err := scanArtifact(rows, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ExpiredArtifact is one artifact the purge deleted.
type ExpiredArtifact struct {
	ID        int
	ProjectID int
	TaskID    *int
	Title     string
}

// DeleteExpiredArtifacts deletes artifacts past their expiry that nobody
// kept, and returns them. Safe to run from several replicas at once: each row
// is deleted (and returned) once.
func (r *Repo) DeleteExpiredArtifacts(ctx context.Context, now time.Time) ([]ExpiredArtifact, error) {
	rows, err := r.db.Query(ctx, `DELETE FROM artifacts WHERE NOT keep AND expires_at < $1
		RETURNING id, project_id, task_id, title`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExpiredArtifact{}
	for rows.Next() {
		var e ExpiredArtifact
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.TaskID, &e.Title); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
