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

// Errors from writing and recording a decision doc (§3 Editing, Record
// decision). The service turns each into the spec's refusal.
var (
	ErrNoDecisionDoc         = errors.New("ticket has no decision doc")
	ErrDecisionRecorded      = errors.New("decision is recorded")
	ErrDecisionStale         = errors.New("decision doc changed since the base version")
	ErrDecisionVersionCap    = errors.New("decision doc has the most versions one artifact keeps")
	ErrDecisionNotCurrent    = errors.New("version is not the decision doc's current version")
	ErrDecisionBeingRecorded = errors.New("decision is being recorded")
)

// DecisionClaimTTL is how long Record decision's claim holds: after it a
// retry finishes the record with the stored values.
const DecisionClaimTTL = "1 minute"

// lockDecisionDoc locks a ticket's decision_docs row for the transaction,
// the lock edits and Record decision share, and then loads the doc's state.
// The state is read in a second statement: under READ COMMITTED, a statement
// that waited for the lock still sees its own snapshot of the other tables,
// so reading the current version in the locking statement would miss an
// edit that committed while it waited.
func lockDecisionDoc(ctx context.Context, tx pgx.Tx, taskID int) (artifactID, current int, claimed, decided, fresh bool, err error) {
	if err = tx.QueryRow(ctx, `SELECT artifact_id FROM decision_docs WHERE task_id = $1 FOR UPDATE`, taskID).Scan(&artifactID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNoDecisionDoc
		}
		return
	}
	err = tx.QueryRow(ctx, `SELECT a.current_version,
			d.decided_version IS NOT NULL AND (d.recording_started_at IS NOT NULL OR d.decided_at IS NOT NULL),
			d.decided_at IS NOT NULL,
			d.recording_started_at IS NOT NULL AND d.recording_started_at > NOW() - INTERVAL '`+DecisionClaimTTL+`'
		FROM decision_docs d JOIN artifacts a ON a.id = d.artifact_id WHERE d.task_id = $1`, taskID).
		Scan(&current, &claimed, &decided, &fresh)
	return
}

// SaveDecisionVersion writes a person's edit of a ticket's decision doc as
// one new version (markdown and options together, author person), under the
// decision_docs row lock. It refuses a claimed or decided doc
// (ErrDecisionRecorded), a base that isn't the current version
// (ErrDecisionStale) and a doc at the version cap (ErrDecisionVersionCap).
// It returns the new version.
func (r *Repo) SaveDecisionVersion(ctx context.Context, taskID, baseVersion int, content []byte, contentType, note string, ask domain.DecisionAsk, by *int) (int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	artifactID, current, claimed, _, _, err := lockDecisionDoc(ctx, tx, taskID)
	if err != nil {
		return 0, err
	}
	if claimed {
		return 0, ErrDecisionRecorded
	}
	if baseVersion != current {
		return 0, ErrDecisionStale
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM artifact_versions WHERE artifact_id = $1`, artifactID).Scan(&n); err != nil {
		return 0, err
	}
	if n >= domain.MaxArtifactVersions {
		return 0, ErrDecisionVersionCap
	}
	// The doc stays kept with no expiry (unlike a published version, which
	// pushes the expiry back): a decision doc never expires.
	var next int
	if err := tx.QueryRow(ctx, `UPDATE artifacts SET current_version = current_version + 1, status = 'in_review', updated_at = NOW()
		WHERE id = $1 RETURNING current_version`, artifactID).Scan(&next); err != nil {
		return 0, err
	}
	sum := sha256.Sum256(content)
	if _, err := tx.Exec(ctx, `INSERT INTO artifact_versions (artifact_id, version, content, content_type, size_bytes, sha256, note, created_by, author_kind, decision_ask)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, artifactID, next, content, contentType, len(content), hex.EncodeToString(sum[:]), note, by,
		domain.AuthorPerson, ask); err != nil {
		return 0, err
	}
	return next, tx.Commit(ctx)
}

// ClaimDecisionRecord is Record decision's claim, under the same row lock
// as edits. An unclaimed doc stores the version, option, text and decider
// with recording_started_at, but only when version is current
// (ErrDecisionNotCurrent). A decided doc is ErrDecisionRecorded, and a claim
// younger than DecisionClaimTTL is ErrDecisionBeingRecorded. An older claim
// is renewed with its stored values, which the caller then finishes with
// (retried reports this). It returns the record as stored.
func (r *Repo) ClaimDecisionRecord(ctx context.Context, taskID, version int, option *int, text string, by *int) (doc *domain.DecisionDoc, retried bool, err error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	_, current, claimed, decided, fresh, err := lockDecisionDoc(ctx, tx, taskID)
	if err != nil {
		return nil, false, err
	}
	switch {
	case decided:
		return nil, false, ErrDecisionRecorded
	case claimed && fresh:
		return nil, false, ErrDecisionBeingRecorded
	case claimed:
		retried = true
		_, err = tx.Exec(ctx, `UPDATE decision_docs SET recording_started_at = NOW() WHERE task_id = $1`, taskID)
	case version != current:
		return nil, false, ErrDecisionNotCurrent
	default:
		_, err = tx.Exec(ctx, `UPDATE decision_docs SET decided_version = $2, chosen_option = $3, decision_text = $4, decided_by = $5,
			recording_started_at = NOW() WHERE task_id = $1`, taskID, version, option, text, by)
	}
	if err != nil {
		return nil, false, err
	}
	doc = &domain.DecisionDoc{}
	if err := scanDecisionDoc(tx.QueryRow(ctx, `SELECT `+decisionDocCols+decisionDocFrom+` WHERE d.task_id = $1`, taskID), doc); err != nil {
		return nil, false, err
	}
	return doc, retried, tx.Commit(ctx)
}

// AddDecisionRun writes Record decision's run and sets decision_docs.run_id
// in one transaction, under the row lock, only while run_id is unset. It
// reports false (and writes nothing) when a run was already recorded, so
// the run and its comment happen once however many attempts finish.
func (r *Repo) AddDecisionRun(ctx context.Context, taskID int, run domain.Run) (domain.Run, bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return domain.Run{}, false, err
	}
	defer tx.Rollback(ctx)
	var existing *int64
	if err := tx.QueryRow(ctx, `SELECT run_id FROM decision_docs WHERE task_id = $1 FOR UPDATE`, taskID).Scan(&existing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Run{}, false, ErrNoDecisionDoc
		}
		return domain.Run{}, false, err
	}
	if existing != nil {
		return domain.Run{}, false, nil
	}
	added, err := insertRun(ctx, tx, run)
	if err != nil {
		return domain.Run{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE decision_docs SET run_id = $2 WHERE task_id = $1`, taskID, added.ID); err != nil {
		return domain.Run{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Run{}, false, err
	}
	return added, true, nil
}

// ReleaseDecisionClaim drops a claim whose record can't finish (the ticket
// can't be completed), so the doc is editable again and dependents stop
// reading it. It never touches a record that wrote its run or finished.
func (r *Repo) ReleaseDecisionClaim(ctx context.Context, taskID int) error {
	_, err := r.db.Exec(ctx, `UPDATE decision_docs SET decided_version = NULL, chosen_option = NULL, decision_text = NULL,
		decided_by = NULL, recording_started_at = NULL WHERE task_id = $1 AND decided_at IS NULL AND run_id IS NULL`, taskID)
	return err
}

// FinishDecisionRecord is Record decision's last step, in one transaction:
// the doc is approved at its decided version and kept with no expiry, and
// the record gets decided_at. It is a no-op on a doc already decided.
func (r *Repo) FinishDecisionRecord(ctx context.Context, taskID int, by *int) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var artifactID int
	var version *int
	var decided bool
	if err := tx.QueryRow(ctx, `SELECT artifact_id, decided_version, decided_at IS NOT NULL FROM decision_docs WHERE task_id = $1 FOR UPDATE`, taskID).
		Scan(&artifactID, &version, &decided); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoDecisionDoc
		}
		return err
	}
	if decided {
		return nil
	}
	if version == nil {
		return errors.New("decision record has no version: claim it first")
	}
	if _, err := tx.Exec(ctx, `UPDATE artifacts SET status = $2, approved_version = $3, reviewed_by = $4, reviewed_at = NOW(), updated_at = NOW(),
		keep = TRUE, kept_by = COALESCE(kept_by, $4), kept_at = COALESCE(kept_at, NOW()), expires_at = NULL WHERE id = $1`,
		artifactID, domain.ArtifactApproved, *version, by); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE decision_docs SET decided_at = NOW() WHERE task_id = $1`, taskID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
