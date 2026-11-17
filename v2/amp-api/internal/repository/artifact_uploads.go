package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ArtifactUploadRow is a pending (or finished) publish by upload URL.
type ArtifactUploadRow struct {
	ID         int64
	UserID     *int
	Caller     []byte
	Request    []byte
	Bytes      int64
	SHA256     string
	ExpiresAt  time.Time
	ClaimedAt  *time.Time
	ArtifactID *int
	Version    *int
	DoneAt     *time.Time
}

const artifactUploadCols = `id, user_id, caller, request, bytes, sha256, expires_at, claimed_at, artifact_id, version, done_at`

func scanArtifactUpload(row rowScanner) (*ArtifactUploadRow, error) {
	u := &ArtifactUploadRow{}
	err := row.Scan(&u.ID, &u.UserID, &u.Caller, &u.Request, &u.Bytes, &u.SHA256, &u.ExpiresAt, &u.ClaimedAt,
		&u.ArtifactID, &u.Version, &u.DoneAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// PutArtifactUpload records a minted upload. Rows a day past expiry are
// swept here, so the table needs no job of its own.
func (r *Repo) PutArtifactUpload(ctx context.Context, tokenHash string, userID *int, caller, request []byte, bytes int64, sha256 string, expires time.Time) error {
	if _, err := r.db.Exec(ctx, `DELETE FROM artifact_uploads WHERE expires_at < NOW() - INTERVAL '1 day'`); err != nil {
		return err
	}
	_, err := r.db.Exec(ctx, `INSERT INTO artifact_uploads (token_hash, user_id, caller, request, bytes, sha256, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, tokenHash, userID, caller, request, bytes, sha256, expires)
	return err
}

// ArtifactUploadByToken is the row whose token hashes to hash, or nil.
func (r *Repo) ArtifactUploadByToken(ctx context.Context, hash string) (*ArtifactUploadRow, error) {
	return scanArtifactUpload(r.db.QueryRow(ctx, `SELECT `+artifactUploadCols+` FROM artifact_uploads WHERE token_hash = $1`, hash))
}

// ClaimArtifactUpload claims the unfinished, unexpired row for hash so one
// request uploads it; nil when there is none to claim. A claim older than
// staleAfter is from an upload that died and can be taken over.
func (r *Repo) ClaimArtifactUpload(ctx context.Context, hash string, staleAfter time.Duration) (*ArtifactUploadRow, error) {
	return scanArtifactUpload(r.db.QueryRow(ctx, `UPDATE artifact_uploads SET claimed_at = NOW()
		WHERE token_hash = $1 AND done_at IS NULL AND expires_at > NOW()
		  AND (claimed_at IS NULL OR claimed_at < NOW() - make_interval(secs => $2))
		RETURNING `+artifactUploadCols, hash, staleAfter.Seconds()))
}

// ReleaseArtifactUpload un-claims row id after a failed upload, so the URL
// can be retried until it expires. claimedAt is the caller's own claim: one
// taken over since is left alone.
func (r *Repo) ReleaseArtifactUpload(ctx context.Context, id int64, claimedAt time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE artifact_uploads SET claimed_at = NULL
		WHERE id = $1 AND claimed_at = $2 AND done_at IS NULL`, id, claimedAt)
	return err
}

// FinishArtifactUpload marks row id used by the version it published, if
// the claim made at claimedAt still holds; false when it was taken over or
// the row is already used.
func (r *Repo) FinishArtifactUpload(ctx context.Context, id int64, claimedAt time.Time, artifactID, version int) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE artifact_uploads SET done_at = NOW(), artifact_id = $3, version = $4
		WHERE id = $1 AND claimed_at = $2 AND done_at IS NULL`, id, claimedAt, artifactID, version)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
