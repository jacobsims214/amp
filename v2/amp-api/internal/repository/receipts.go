package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// Verification receipts (#126). Rules live in internal/service/receipts.go.

const receiptCols = `id, task_id, project_id, dispatch_id, run_id, state, mode, held, agent_id, summary, files_changed,
	commit_sha, tree_sha, branch, base_branch, pr_url, kept, created_at, finalized_at`

func scanReceipt(row rowScanner, rc *domain.Receipt) error {
	var files []byte
	if err := row.Scan(&rc.ID, &rc.TaskID, &rc.ProjectID, &rc.DispatchID, &rc.RunID, &rc.State, &rc.Mode, &rc.Held,
		&rc.AgentID, &rc.Summary, &files, &rc.CommitSHA, &rc.TreeSHA, &rc.Branch, &rc.BaseBranch, &rc.PRURL, &rc.Kept,
		&rc.CreatedAt, &rc.FinalizedAt); err != nil {
		return err
	}
	rc.FilesChanged = []domain.FileChange{}
	if len(files) > 0 {
		_ = json.Unmarshal(files, &rc.FilesChanged)
	}
	return nil
}

const checkCols = `id, receipt_id, role, kind, verdict, criterion_ref, command, exit_code, duration_ms, note, evidence_key,
	checked_by, user_id, created_at`

func scanCheck(row rowScanner, c *domain.ReceiptCheck) error {
	return row.Scan(&c.ID, &c.ReceiptID, &c.Role, &c.Kind, &c.Verdict, &c.CriterionRef, &c.Command, &c.ExitCode,
		&c.DurationMS, &c.Note, &c.EvidenceKey, &c.CheckedBy, &c.UserID, &c.CreatedAt)
}

func filesJSON(files []domain.FileChange) []byte {
	if files == nil {
		files = []domain.FileChange{}
	}
	b, _ := json.Marshal(files)
	return b
}

// OpenReceiptDraft creates ticket taskID's draft for dispatchID, or updates
// the one it has: a second open for the same dispatch returns the same
// draft. summary and files replace the draft's only when given.
func (r *Repo) OpenReceiptDraft(ctx context.Context, taskID, projectID int, dispatchID, mode, agentID, summary string, files []domain.FileChange) (*domain.Receipt, error) {
	var fj any
	if files != nil {
		fj = filesJSON(files)
	}
	rc := &domain.Receipt{}
	err := scanReceipt(r.db.QueryRow(ctx, `INSERT INTO task_receipts (task_id, project_id, dispatch_id, mode, agent_id, summary, files_changed)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::jsonb, '[]'::jsonb))
		ON CONFLICT (task_id, dispatch_id) WHERE state = 'draft' AND dispatch_id <> '' DO UPDATE SET
		  summary = CASE WHEN EXCLUDED.summary <> '' THEN EXCLUDED.summary ELSE task_receipts.summary END,
		  files_changed = CASE WHEN $7::jsonb IS NOT NULL THEN EXCLUDED.files_changed ELSE task_receipts.files_changed END,
		  agent_id = CASE WHEN EXCLUDED.agent_id <> '' THEN EXCLUDED.agent_id ELSE task_receipts.agent_id END
		RETURNING `+receiptCols, taskID, projectID, dispatchID, mode, agentID, summary, fj), rc)
	return rc, err
}

// CreateReceipt inserts a draft receipt for a completion that opened none.
func (r *Repo) CreateReceipt(ctx context.Context, rc domain.Receipt) (*domain.Receipt, error) {
	out := &domain.Receipt{}
	err := scanReceipt(r.db.QueryRow(ctx, `INSERT INTO task_receipts (task_id, project_id, dispatch_id, mode, agent_id, summary, files_changed)
		VALUES ($1, $2, '', $3, $4, $5, $6::jsonb) RETURNING `+receiptCols,
		rc.TaskID, rc.ProjectID, rc.Mode, rc.AgentID, rc.Summary, filesJSON(rc.FilesChanged)), out)
	return out, err
}

// GetReceipt loads one receipt.
func (r *Repo) GetReceipt(ctx context.Context, id int64) (*domain.Receipt, error) {
	rc := &domain.Receipt{}
	err := scanReceipt(r.db.QueryRow(ctx, `SELECT `+receiptCols+` FROM task_receipts WHERE id = $1`, id), rc)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("receipt %d: %w", id, ErrNotFound)
	}
	return rc, err
}

// NewestReceiptDraft is ticket taskID's newest open draft, or nil.
func (r *Repo) NewestReceiptDraft(ctx context.Context, taskID int) (*domain.Receipt, error) {
	rc := &domain.Receipt{}
	err := scanReceipt(r.db.QueryRow(ctx, `SELECT `+receiptCols+` FROM task_receipts
		WHERE task_id = $1 AND state = 'draft' ORDER BY id DESC LIMIT 1`, taskID), rc)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return rc, err
}

// FinalizeReceipt makes draft rc final with its run details and adds the
// checks, in one transaction. It is a no-op returning false when rc is no
// longer a draft (another completion finalized it first).
func (r *Repo) FinalizeReceipt(ctx context.Context, rc domain.Receipt, checks []domain.ReceiptCheck) (bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE task_receipts SET state = 'final', finalized_at = NOW(), run_id = $2, held = $3, kept = kept OR $4,
		agent_id = $5, summary = $6, files_changed = $7::jsonb, commit_sha = $8, tree_sha = $9, branch = $10, base_branch = $11,
		pr_url = $12, mode = $13
		WHERE id = $1 AND state = 'draft'`,
		rc.ID, rc.RunID, rc.Held, rc.Kept, rc.AgentID, rc.Summary, filesJSON(rc.FilesChanged), rc.CommitSHA, rc.TreeSHA,
		rc.Branch, rc.BaseBranch, rc.PRURL, rc.Mode)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	for _, c := range checks {
		if _, err := tx.Exec(ctx, `INSERT INTO receipt_checks (receipt_id, role, kind, verdict, criterion_ref, command, exit_code,
			duration_ms, note, evidence_key, checked_by, user_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			rc.ID, c.Role, c.Kind, c.Verdict, c.CriterionRef, c.Command, c.ExitCode, c.DurationMS, c.Note, c.EvidenceKey,
			c.CheckedBy, c.UserID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// AddReceiptCheck adds one check (a person's verdict) to a receipt.
func (r *Repo) AddReceiptCheck(ctx context.Context, c domain.ReceiptCheck) (domain.ReceiptCheck, error) {
	var out domain.ReceiptCheck
	err := scanCheck(r.db.QueryRow(ctx, `INSERT INTO receipt_checks (receipt_id, role, kind, verdict, criterion_ref, command, exit_code,
		duration_ms, note, evidence_key, checked_by, user_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+checkCols,
		c.ReceiptID, c.Role, c.Kind, c.Verdict, c.CriterionRef, c.Command, c.ExitCode, c.DurationMS, c.Note, c.EvidenceKey,
		c.CheckedBy, c.UserID), &out)
	return out, err
}

// DeleteReceiptCheck deletes one check (a verdict whose move failed).
func (r *Repo) DeleteReceiptCheck(ctx context.Context, id int64) error {
	_, err := r.db.Exec(ctx, `DELETE FROM receipt_checks WHERE id = $1`, id)
	return err
}

// ReceiptChecks lists the checks of the given receipts, oldest first, by
// receipt.
func (r *Repo) ReceiptChecks(ctx context.Context, ids []int64) (map[int64][]domain.ReceiptCheck, error) {
	out := map[int64][]domain.ReceiptCheck{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT `+checkCols+` FROM receipt_checks WHERE receipt_id = ANY($1) ORDER BY created_at, id`, ids)
	if err != nil {
		return nil, err
	}
	checks, err := collect(rows, scanCheck)
	if err != nil {
		return nil, err
	}
	for _, c := range checks {
		out[c.ReceiptID] = append(out[c.ReceiptID], c)
	}
	return out, nil
}

// TaskReceipts lists ticket taskID's final receipts, newest first.
func (r *Repo) TaskReceipts(ctx context.Context, taskID int) ([]domain.Receipt, error) {
	rows, err := r.db.Query(ctx, `SELECT `+receiptCols+` FROM task_receipts WHERE task_id = $1 AND state = 'final' ORDER BY id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanReceipt)
}

// LatestReceipt is a ticket's latest final receipt with what the summary
// needs of the ticket: its state, current dispatch and owner (a receipt is
// held only while the ticket is in progress with no dispatch).
type LatestReceipt struct {
	domain.Receipt
	TaskState    domain.TaskState
	TaskDispatch string
	OwnerUserID  *int
}

// ProjectLatestReceipts is each live ticket's latest final receipt in a
// project, with whether it has stored media evidence.
func (r *Repo) ProjectLatestReceipts(ctx context.Context, projectID int) ([]LatestReceipt, map[int64]bool, error) {
	cols := prefixed("rc", receiptCols)
	rows, err := r.db.Query(ctx, `SELECT `+cols+`, t.state, t.dispatch_id, t.owner_user_id FROM (
		  SELECT DISTINCT ON (task_id) * FROM task_receipts WHERE project_id = $1 AND state = 'final' ORDER BY task_id, id DESC) rc
		JOIN tasks t ON t.id = rc.task_id ORDER BY rc.task_id`, projectID)
	if err != nil {
		return nil, nil, err
	}
	list, err := collect(rows, func(row rowScanner, lr *LatestReceipt) error {
		var files []byte
		rc := &lr.Receipt
		if err := row.Scan(&rc.ID, &rc.TaskID, &rc.ProjectID, &rc.DispatchID, &rc.RunID, &rc.State, &rc.Mode, &rc.Held,
			&rc.AgentID, &rc.Summary, &files, &rc.CommitSHA, &rc.TreeSHA, &rc.Branch, &rc.BaseBranch, &rc.PRURL, &rc.Kept,
			&rc.CreatedAt, &rc.FinalizedAt, &lr.TaskState, &lr.TaskDispatch, &lr.OwnerUserID); err != nil {
			return err
		}
		rc.FilesChanged = []domain.FileChange{}
		if len(files) > 0 {
			_ = json.Unmarshal(files, &rc.FilesChanged)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	media := map[int64]bool{}
	ids := make([]int64, len(list))
	for i, rc := range list {
		ids[i] = rc.ID
	}
	if len(ids) > 0 {
		mrows, err := r.db.Query(ctx, `SELECT DISTINCT receipt_id FROM receipt_evidence WHERE receipt_id = ANY($1) AND status = 'stored'
			AND (content_type LIKE 'video/%' OR content_type LIKE 'image/%')`, ids)
		if err != nil {
			return nil, nil, err
		}
		defer mrows.Close()
		for mrows.Next() {
			var id int64
			if err := mrows.Scan(&id); err != nil {
				return nil, nil, err
			}
			media[id] = true
		}
		if err := mrows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return list, media, nil
}

// ReceiptEvidence lists a receipt's evidence rows, oldest first.
func (r *Repo) ReceiptEvidence(ctx context.Context, receiptID int64) ([]domain.ReceiptEvidence, error) {
	rows, err := r.db.Query(ctx, `SELECT id, receipt_id, key, name, tree_sha, content_type, bytes, sha256, duration_ms, frames,
		status, created_at, stored_at FROM receipt_evidence WHERE receipt_id = $1 ORDER BY id`, receiptID)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row rowScanner, e *domain.ReceiptEvidence) error {
		return row.Scan(&e.ID, &e.ReceiptID, &e.Key, &e.Name, &e.TreeSHA, &e.ContentType, &e.Bytes, &e.SHA256, &e.DurationMS,
			&e.Frames, &e.Status, &e.CreatedAt, &e.StoredAt)
	})
}

// KeepTaskReceipts keeps every receipt of a ticket (its merge was recorded,
// D13): their evidence then never expires.
func (r *Repo) KeepTaskReceipts(ctx context.Context, taskID int) error {
	_, err := r.db.Exec(ctx, `UPDATE task_receipts SET kept = TRUE WHERE task_id = $1 AND NOT kept`, taskID)
	return err
}

// SetReceiptKept keeps one receipt (a person accepted it).
func (r *Repo) SetReceiptKept(ctx context.Context, id int64) error {
	_, err := r.db.Exec(ctx, `UPDATE task_receipts SET kept = TRUE WHERE id = $1`, id)
	return err
}

// ClearReceiptHeld ends a receipt's hold (a person accepted or rejected
// it). It reports whether the receipt was held.
func (r *Repo) ClearReceiptHeld(ctx context.Context, id int64) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE task_receipts SET held = FALSE WHERE id = $1 AND held`, id)
	return tag.RowsAffected() > 0, err
}

// ClearTaskHeld ends every hold on ticket taskID's receipts: it left the
// held state another way (moved, dispatched, completed, deleted).
func (r *Repo) ClearTaskHeld(ctx context.Context, taskID int) error {
	_, err := r.db.Exec(ctx, `UPDATE task_receipts SET held = FALSE WHERE task_id = $1 AND held`, taskID)
	return err
}

// ClearTasksHeld is ClearTaskHeld for several tickets.
func (r *Repo) ClearTasksHeld(ctx context.Context, taskIDs []int) error {
	if len(taskIDs) == 0 {
		return nil
	}
	_, err := r.db.Exec(ctx, `UPDATE task_receipts SET held = FALSE WHERE task_id = ANY($1) AND held`, taskIDs)
	return err
}

// RecentAgentCompletions lists the agent tickets in a project completed
// since since (for the Require warning count).
func (r *Repo) RecentAgentCompletions(ctx context.Context, projectID int, since time.Time) ([]int, error) {
	rows, err := r.db.Query(ctx, `SELECT id FROM tasks WHERE project_id = $1 AND state = 'completed' AND completed_at >= $2
		AND performer <> $3 ORDER BY id`, projectID, since, domain.PerformerHuman)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
