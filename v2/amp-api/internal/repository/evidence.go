package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// Receipt evidence metadata (#126, Phase 3). The bytes are in the receipt
// store (internal/receipts); these rows say what each file is, whether it
// is stored, and the one-time upload token's hash while it is pending.

// EvidenceRow is an evidence row with what serving, uploading and the sweep
// need of its receipt.
type EvidenceRow struct {
	domain.ReceiptEvidence
	ProjectID       int
	TaskID          int
	ReceiptState    string
	ReceiptDispatch string
	Kept            bool
	TokenHash       string
	UploadExpiresAt *time.Time
}

const evidenceRowCols = `e.id, e.receipt_id, e.key, e.name, e.tree_sha, e.content_type, e.bytes, e.sha256, e.duration_ms, e.frames,
	e.status, e.created_at, e.stored_at, e.project_id, r.task_id, r.state, r.dispatch_id, r.kept, e.upload_token_hash, e.upload_expires_at`

func scanEvidenceRow(row rowScanner, e *EvidenceRow) error {
	return row.Scan(&e.ID, &e.ReceiptID, &e.Key, &e.Name, &e.TreeSHA, &e.ContentType, &e.Bytes, &e.SHA256, &e.DurationMS,
		&e.Frames, &e.Status, &e.CreatedAt, &e.StoredAt, &e.ProjectID, &e.TaskID, &e.ReceiptState, &e.ReceiptDispatch, &e.Kept,
		&e.TokenHash, &e.UploadExpiresAt)
}

func (r *Repo) oneEvidence(ctx context.Context, where string, arg any) (*EvidenceRow, error) {
	e := &EvidenceRow{}
	err := scanEvidenceRow(r.db.QueryRow(ctx, `SELECT `+evidenceRowCols+` FROM receipt_evidence e
		JOIN task_receipts r ON r.id = e.receipt_id WHERE `+where, arg), e)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// EvidenceByKey is the evidence row at key, or nil.
func (r *Repo) EvidenceByKey(ctx context.Context, key string) (*EvidenceRow, error) {
	return r.oneEvidence(ctx, `e.key = $1`, key)
}

// PendingEvidenceByToken is the pending row whose upload token hashes to
// hash, or nil (unknown, already used, or no longer pending). A row an
// upload has claimed (ClaimPendingEvidence) is found too: its TokenHash is
// then hash plus ":" and the claim.
func (r *Repo) PendingEvidenceByToken(ctx context.Context, hash string) (*EvidenceRow, error) {
	if hash == "" {
		return nil, nil
	}
	return r.oneEvidence(ctx, `(e.upload_token_hash = $1 OR starts_with(e.upload_token_hash, $1 || ':')) AND e.status = 'pending'`, hash)
}

// CountReceiptEvidence is how many evidence rows a receipt has.
func (r *Repo) CountReceiptEvidence(ctx context.Context, receiptID int64) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM receipt_evidence WHERE receipt_id = $1`, receiptID).Scan(&n)
	return n, err
}

// PutPendingEvidence creates key's pending row, or re-mints it (a new
// token, size and hash) when it exists and isn't stored. It returns false
// when key is stored (the caller compares the hashes first; this only
// guards a race).
func (r *Repo) PutPendingEvidence(ctx context.Context, e domain.ReceiptEvidence, projectID int, tokenHash string, expires time.Time) (bool, error) {
	tag, err := r.db.Exec(ctx, `INSERT INTO receipt_evidence (receipt_id, project_id, key, name, tree_sha, content_type, bytes, sha256,
		duration_ms, frames, status, upload_token_hash, upload_expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending',$11,$12)
		ON CONFLICT (key) DO UPDATE SET tree_sha = EXCLUDED.tree_sha, content_type = EXCLUDED.content_type, bytes = EXCLUDED.bytes,
		  sha256 = EXCLUDED.sha256, duration_ms = EXCLUDED.duration_ms, frames = EXCLUDED.frames, status = 'pending',
		  upload_token_hash = EXCLUDED.upload_token_hash, upload_expires_at = EXCLUDED.upload_expires_at, stored_at = NULL
		WHERE receipt_evidence.status <> 'stored' AND receipt_evidence.receipt_id = EXCLUDED.receipt_id`,
		e.ReceiptID, projectID, e.Key, e.Name, e.TreeSHA, e.ContentType, e.Bytes, e.SHA256, e.DurationMS, e.Frames, tokenHash, expires)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MarkEvidenceStored makes pending row id stored and clears its token (the
// token is single use). tokenHash is the upload's claim. It reports false
// when the row was no longer pending with it (re-minted meanwhile).
func (r *Repo) MarkEvidenceStored(ctx context.Context, id int64, tokenHash string) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE receipt_evidence SET status = 'stored', stored_at = NOW(), upload_token_hash = '',
		upload_expires_at = NULL WHERE id = $1 AND status = 'pending' AND upload_token_hash = $2`, id, tokenHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetReceiptRevision records the commit and tree a draft's evidence was
// recorded on (amp_add_evidence). commit "" keeps the receipt's.
func (r *Repo) SetReceiptRevision(ctx context.Context, receiptID int64, commit, tree string) error {
	_, err := r.db.Exec(ctx, `UPDATE task_receipts SET tree_sha = $2,
		commit_sha = CASE WHEN $3 <> '' THEN $3 ELSE commit_sha END WHERE id = $1 AND state = 'draft'`, receiptID, tree, commit)
	return err
}

// evidenceRows lists rows matching where, oldest first; limit 0 is all.
func (r *Repo) evidenceRows(ctx context.Context, where string, limit int, args ...any) ([]EvidenceRow, error) {
	tail := ""
	if limit > 0 {
		tail = fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := r.db.Query(ctx, `SELECT `+evidenceRowCols+` FROM receipt_evidence e
		JOIN task_receipts r ON r.id = e.receipt_id WHERE `+where+` ORDER BY e.id`+tail, args...)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanEvidenceRow)
}

// ExpirableEvidence is stored evidence of unkept receipts created before
// before (the retention sweep), at most limit rows.
func (r *Repo) ExpirableEvidence(ctx context.Context, before time.Time, limit int) ([]EvidenceRow, error) {
	return r.evidenceRows(ctx, `e.status = 'stored' AND NOT r.kept AND r.created_at < $1`, limit, before)
}

// ExpireEvidenceLocked expires stored row id: it locks the row and its
// receipt (only while the receipt is unkept), runs del with the row's key,
// and only then marks the row expired. A Keep waits for the lock, so kept
// evidence is never deleted; a failed del leaves the row stored, for the
// next pass. false: nothing to do (kept, gone, or no longer stored).
func (r *Repo) ExpireEvidenceLocked(ctx context.Context, id int64, del func(key string) error) (bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var key string
	err = tx.QueryRow(ctx, `SELECT e.key FROM receipt_evidence e JOIN task_receipts r ON r.id = e.receipt_id
		WHERE e.id = $1 AND e.status = 'stored' AND NOT r.kept FOR UPDATE OF e, r`, id).Scan(&key)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := del(key); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE receipt_evidence SET status = 'expired' WHERE id = $1`, id); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// StalePendingEvidence is pending rows whose upload expired before before.
func (r *Repo) StalePendingEvidence(ctx context.Context, before time.Time, limit int) ([]EvidenceRow, error) {
	return r.evidenceRows(ctx, `e.status = 'pending' AND e.upload_expires_at < $1`, limit, before)
}

// DeleteStalePendingLocked deletes pending row id whose upload expired
// before before, if it still has token hash tokenHash: it locks the row,
// runs del with its key, then deletes the row. A re-mint or an upload of the
// row waits for the lock (and then finds it gone), so a fresh upload's
// object is never deleted; a failed del keeps the row for the next pass.
func (r *Repo) DeleteStalePendingLocked(ctx context.Context, id int64, tokenHash string, before time.Time, del func(key string) error) (bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var key string
	err = tx.QueryRow(ctx, `SELECT key FROM receipt_evidence WHERE id = $1 AND status = 'pending' AND upload_token_hash = $2
		AND upload_expires_at < $3 FOR UPDATE`, id, tokenHash, before).Scan(&key)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := del(key); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM receipt_evidence WHERE id = $1`, id); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ClaimPendingEvidence swaps pending row id's token hash for claim, if it
// still has tokenHash: the one request that wins uploads, any other with the
// same token is refused.
func (r *Repo) ClaimPendingEvidence(ctx context.Context, id int64, tokenHash, claim string) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE receipt_evidence SET upload_token_hash = $3
		WHERE id = $1 AND status = 'pending' AND upload_token_hash = $2`, id, tokenHash, claim)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReleasePendingEvidence gives a claimed row its token hash back (the
// upload failed; the URL can be retried), unless it was re-minted or stored.
func (r *Repo) ReleasePendingEvidence(ctx context.Context, id int64, claim, tokenHash string) error {
	_, err := r.db.Exec(ctx, `UPDATE receipt_evidence SET upload_token_hash = $3
		WHERE id = $1 AND status = 'pending' AND upload_token_hash = $2`, id, claim, tokenHash)
	return err
}

// TasksEvidence is every evidence row of the given tickets' receipts.
func (r *Repo) TasksEvidence(ctx context.Context, taskIDs []int) ([]EvidenceRow, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	return r.evidenceRows(ctx, `r.task_id = ANY($1)`, 0, taskIDs)
}

// DeleteTasksReceipts deletes the given tickets' receipts (their checks and
// evidence rows cascade). Receipts have no FK to tasks, so a purge of the
// tickets calls this.
func (r *Repo) DeleteTasksReceipts(ctx context.Context, taskIDs []int) (int64, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM task_receipts WHERE task_id = ANY($1)`, taskIDs)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ProjectEvidence is every evidence row of a project's receipts.
func (r *Repo) ProjectEvidence(ctx context.Context, projectID int) ([]EvidenceRow, error) {
	return r.evidenceRows(ctx, `r.project_id = $1`, 0, projectID)
}

// DeleteProjectReceipts deletes all of a project's receipts (a reset).
func (r *Repo) DeleteProjectReceipts(ctx context.Context, projectID int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM task_receipts WHERE project_id = $1`, projectID)
	return err
}

// PurgeableTrashTaskIDs is the tickets in the trash past retention: the
// ones PurgeTrash deletes next.
func (r *Repo) PurgeableTrashTaskIDs(ctx context.Context) ([]int, error) {
	rows, err := r.db.Query(ctx, `SELECT task_id FROM deleted_tasks WHERE deleted_at < NOW() - INTERVAL '`+TrashRetention+`' ORDER BY task_id`)
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
