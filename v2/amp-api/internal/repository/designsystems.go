package repository

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/domain"
)

const dsCols = `id, scope, org_id, project_id, source_project_id, version, current_epic_id, published_at, published_by, created_at`

func scanDS(row pgx.Row) (*domain.DesignSystem, error) {
	d := &domain.DesignSystem{}
	err := row.Scan(&d.ID, &d.Scope, &d.OrgID, &d.ProjectID, &d.SourceProjectID, &d.Version, &d.CurrentEpicID,
		&d.PublishedAt, &d.PublishedBy, &d.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return d, err
}

// OrgDesignSystem is the org's design system, or nil.
func (r *Repo) OrgDesignSystem(ctx context.Context, orgID int) (*domain.DesignSystem, error) {
	return scanDS(r.db.QueryRow(ctx, `SELECT `+dsCols+` FROM design_systems WHERE scope = 'org' AND org_id = $1`, orgID))
}

// ProjectDesignSystem is a product project's design system layer, or nil.
func (r *Repo) ProjectDesignSystem(ctx context.Context, projectID int) (*domain.DesignSystem, error) {
	return scanDS(r.db.QueryRow(ctx, `SELECT `+dsCols+` FROM design_systems WHERE scope = 'project' AND project_id = $1`, projectID))
}

// DesignSystemBySource is the design system whose work happens in a project
// (an org's Design System project, or a product project), or nil.
func (r *Repo) DesignSystemBySource(ctx context.Context, projectID int) (*domain.DesignSystem, error) {
	return scanDS(r.db.QueryRow(ctx, `SELECT `+dsCols+` FROM design_systems WHERE source_project_id = $1
		ORDER BY (scope = 'org') DESC LIMIT 1`, projectID))
}

// CreateDesignSystem registers a design system.
func (r *Repo) CreateDesignSystem(ctx context.Context, d domain.DesignSystem, by *int) (*domain.DesignSystem, error) {
	return scanDS(r.db.QueryRow(ctx, `INSERT INTO design_systems (scope, org_id, project_id, source_project_id, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+dsCols, d.Scope, d.OrgID, d.ProjectID, d.SourceProjectID, by))
}

// SetDesignSystemEpic records the epic the next version is being built in.
func (r *Repo) SetDesignSystemEpic(ctx context.Context, id int, epicID *int) error {
	_, err := r.db.Exec(ctx, `UPDATE design_systems SET current_epic_id = $2 WHERE id = $1`, id, epicID)
	return err
}

// PublishDesignSystem records a new version and closes the epic it was built in.
func (r *Repo) PublishDesignSystem(ctx context.Context, id, version int, changelog string, arts []domain.DesignSystemArtifact, by *int) error {
	b, err := json.Marshal(arts)
	if err != nil {
		return err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO design_system_versions (design_system_id, version, changelog, artifacts, published_by)
		VALUES ($1, $2, $3, $4, $5)`, id, version, changelog, b, by); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE design_systems SET version = $2, current_epic_id = NULL, published_at = NOW(), published_by = $3
		WHERE id = $1`, id, version, by); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DesignSystemVersions lists a design system's publishes, newest first.
func (r *Repo) DesignSystemVersions(ctx context.Context, id int) ([]domain.DesignSystemVersion, error) {
	rows, err := r.db.Query(ctx, `SELECT version, changelog, artifacts, published_by, published_at
		FROM design_system_versions WHERE design_system_id = $1 ORDER BY version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.DesignSystemVersion{}
	for rows.Next() {
		var v domain.DesignSystemVersion
		var raw []byte
		if err := rows.Scan(&v.Version, &v.Changelog, &raw, &v.PublishedBy, &v.PublishedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &v.Artifacts)
		out = append(out, v)
	}
	return out, rows.Err()
}
