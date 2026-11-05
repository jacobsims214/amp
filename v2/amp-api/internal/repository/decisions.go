package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/domain"
)

// ErrDecisionDocExists: the ticket already has a decision doc.
var ErrDecisionDocExists = errors.New("ticket already has a decision doc")

// CreateDecisionDoc writes a ticket's decision doc in one transaction: the
// artifact (role decision, kept from creation, no expiry), its v1 by the
// agent with the ask's options as metadata, and the decision_docs row. A
// ticket that already has one gets ErrDecisionDocExists and nothing changes.
func (r *Repo) CreateDecisionDoc(ctx context.Context, taskID int, a domain.Artifact, content []byte, contentType, note string, ask domain.DecisionAsk) (*domain.Artifact, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM decision_docs WHERE task_id = $1)`, taskID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrDecisionDocExists
	}
	var id int
	if err := tx.QueryRow(ctx, `INSERT INTO artifacts (project_id, task_id, title, kind, created_by, role, keep, kept_by, kept_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, TRUE, $5, NOW(), NULL) RETURNING id`,
		a.ProjectID, taskID, a.Title, domain.ArtifactMarkdown, a.CreatedBy, domain.ArtifactRoleDecision).Scan(&id); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	if _, err := tx.Exec(ctx, `INSERT INTO artifact_versions (artifact_id, version, content, content_type, size_bytes, sha256, note, created_by, author_kind, decision_ask)
		VALUES ($1, 1, $2, $3, $4, $5, $6, $7, $8, $9)`, id, content, contentType, len(content), hex.EncodeToString(sum[:]), note, a.CreatedBy,
		domain.AuthorAgent, ask); err != nil {
		return nil, err
	}
	// ON CONFLICT: a concurrent creation for the same ticket loses here, and
	// its artifact rolls back with it.
	tag, err := tx.Exec(ctx, `INSERT INTO decision_docs (task_id, artifact_id) VALUES ($1, $2) ON CONFLICT (task_id) DO NOTHING`, taskID, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrDecisionDocExists
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.GetArtifact(ctx, id)
}

const decisionDocCols = `d.task_id, d.artifact_id, a.current_version, d.decided_version, d.chosen_option, COALESCE(d.decision_text, ''),
	d.decided_by, COALESCE(NULLIF(u.display_name, ''), u.email, ''), d.recording_started_at, d.decided_at`

const decisionDocFrom = ` FROM decision_docs d JOIN artifacts a ON a.id = d.artifact_id LEFT JOIN users u ON u.id = d.decided_by`

func scanDecisionDoc(row pgx.Row, d *domain.DecisionDoc) error {
	return row.Scan(&d.TaskID, &d.ArtifactID, &d.CurrentVersion, &d.DecidedVersion, &d.ChosenOption, &d.DecisionText,
		&d.DecidedBy, &d.DecidedByName, &d.RecordingStartedAt, &d.DecidedAt)
}

// GetDecisionDoc loads a ticket's decision doc record (without content or
// versions), or nil when the ticket has none.
func (r *Repo) GetDecisionDoc(ctx context.Context, taskID int) (*domain.DecisionDoc, error) {
	d := &domain.DecisionDoc{}
	err := scanDecisionDoc(r.db.QueryRow(ctx, `SELECT `+decisionDocCols+decisionDocFrom+` WHERE d.task_id = $1`, taskID), d)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// DecisionDocForArtifact loads the decision doc record an artifact is, or
// nil when it is no ticket's decision doc.
func (r *Repo) DecisionDocForArtifact(ctx context.Context, artifactID int) (*domain.DecisionDoc, error) {
	d := &domain.DecisionDoc{}
	err := scanDecisionDoc(r.db.QueryRow(ctx, `SELECT `+decisionDocCols+decisionDocFrom+` WHERE d.artifact_id = $1`, artifactID), d)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// DecisionDocTaskIDs lists the project's tickets that have a decision doc.
func (r *Repo) DecisionDocTaskIDs(ctx context.Context, projectID int) (map[int]bool, error) {
	rows, err := r.db.Query(ctx, `SELECT d.task_id FROM decision_docs d JOIN tasks t ON t.id = d.task_id WHERE t.project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// DecisionDocVersions loads every version of a decision doc with its
// markdown and options, oldest first.
func (r *Repo) DecisionDocVersions(ctx context.Context, artifactID int) ([]domain.DecisionDocVersion, error) {
	rows, err := r.db.Query(ctx, `SELECT version, author_kind, content, decision_ask, created_by, created_at
		FROM artifact_versions WHERE artifact_id = $1 ORDER BY version`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.DecisionDocVersion{}
	for rows.Next() {
		var v domain.DecisionDocVersion
		var content []byte
		if err := rows.Scan(&v.Version, &v.AuthorKind, &content, &v.Ask, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Text = string(content)
		out = append(out, v)
	}
	return out, rows.Err()
}
