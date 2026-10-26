package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/auditchain"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Audit log integrity (migration 033, ticket #2429) ----

// AuditChainKeys lists every chain, global first.
func (r *Repo) AuditChainKeys(ctx context.Context) ([]string, error) {
	rows, err := r.db.Query(ctx, `SELECT chain_key FROM audit_chain_heads ORDER BY chain_key <> 'global', chain_key`)
	if err != nil {
		return nil, fmt.Errorf("list audit chains: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// AuditChainHead is a chain's head, or nil if the chain doesn't exist.
func (r *Repo) AuditChainHead(ctx context.Context, key string) (*auditchain.Head, error) {
	var h auditchain.Head
	err := r.db.QueryRow(ctx, `SELECT seq, hash FROM audit_chain_heads WHERE chain_key = $1`, key).Scan(&h.Seq, &h.Hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit chain head: %w", err)
	}
	return &h, nil
}

const checkpointCols = `id, chain_key, upto_seq, hash, at, kind, key_id, signature`

func scanCheckpoint(row rowScanner, c *auditchain.Checkpoint) error {
	return row.Scan(&c.ID, &c.ChainKey, &c.UptoSeq, &c.Hash, &c.At, &c.Kind, &c.KeyID, &c.Signature)
}

// AuditCheckpoints lists a chain's checkpoints, oldest first.
func (r *Repo) AuditCheckpoints(ctx context.Context, key string) ([]auditchain.Checkpoint, error) {
	rows, err := r.db.Query(ctx, `SELECT `+checkpointCols+` FROM audit_checkpoints WHERE chain_key = $1 ORDER BY id`, key)
	if err != nil {
		return nil, fmt.Errorf("audit checkpoints: %w", err)
	}
	return collect(rows, scanCheckpoint)
}

// LastAuditCheckpoint is a chain's newest checkpoint, or nil.
func (r *Repo) LastAuditCheckpoint(ctx context.Context, key string) (*auditchain.Checkpoint, error) {
	var c auditchain.Checkpoint
	err := scanCheckpoint(r.db.QueryRow(ctx, `SELECT `+checkpointCols+` FROM audit_checkpoints
		WHERE chain_key = $1 ORDER BY upto_seq DESC, id DESC LIMIT 1`, key), &c)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("last audit checkpoint: %w", err)
	}
	return &c, nil
}

// InsertAuditCheckpoint stores a (signed) checkpoint and sets its id.
func (r *Repo) InsertAuditCheckpoint(ctx context.Context, c *auditchain.Checkpoint) error {
	return insertCheckpoint(ctx, r.db, c)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func insertCheckpoint(ctx context.Context, q queryRower, c *auditchain.Checkpoint) error {
	err := q.QueryRow(ctx, `INSERT INTO audit_checkpoints (chain_key, upto_seq, hash, at, kind, key_id, signature)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		c.ChainKey, c.UptoSeq, c.Hash, c.At, c.Kind, c.KeyID, c.Signature).Scan(&c.ID)
	if err != nil {
		return fmt.Errorf("insert audit checkpoint: %w", err)
	}
	return nil
}

// AuditActionAt is the action of a chain's row at seq ("" if there's none).
func (r *Repo) AuditActionAt(ctx context.Context, key string, seq int64) (string, error) {
	var a string
	err := r.db.QueryRow(ctx, `SELECT action FROM audit_log WHERE chain_key = $1 AND chain_seq = $2`, key, seq).Scan(&a)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return a, err
}

// chainRowCols reads a row the way auditchain.Canonical needs it: jsonb and
// inet as Postgres's own text of them.
const chainRowCols = `id, at, actor_email, via_token, org_id, project_id, action, target_type, target_id, summary, details::text,
	schema_version, actor_type, auth_method, token_id, session_id, on_behalf_of_user_id, agent_profile, target_name,
	outcome, reason, source_ip::text, user_agent, client, request_id, trace_id, "before"::text, "after"::text,
	chain_key, chain_seq, prev_hash, hash`

// ReadAuditChain reads one chain from a single snapshot, so a verification
// running while entries are written or purged sees a consistent chain: it
// calls start with the chain's head (nil if none) and checkpoints, then row
// with each row in seq order, then (for "global" only, since they can't be
// placed in an org's chain) any rows that have no chain at all.
func (r *Repo) ReadAuditChain(ctx context.Context, key string, start func(*auditchain.Head, []auditchain.Checkpoint), row func(auditchain.Row)) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var head *auditchain.Head
	var h auditchain.Head
	err = tx.QueryRow(ctx, `SELECT seq, hash FROM audit_chain_heads WHERE chain_key = $1`, key).Scan(&h.Seq, &h.Hash)
	switch {
	case err == nil:
		head = &h
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("audit chain head: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT `+checkpointCols+` FROM audit_checkpoints WHERE chain_key = $1 ORDER BY id`, key)
	if err != nil {
		return fmt.Errorf("audit checkpoints: %w", err)
	}
	cps, err := collect(rows, scanCheckpoint)
	if err != nil {
		return fmt.Errorf("audit checkpoints: %w", err)
	}
	start(head, cps)
	q := `SELECT ` + chainRowCols + ` FROM audit_log WHERE chain_key = $1 ORDER BY chain_seq`
	if key == "global" {
		q = `SELECT ` + chainRowCols + ` FROM audit_log WHERE chain_key = $1 OR chain_key IS NULL
			ORDER BY chain_key IS NULL, chain_seq, id`
	}
	rs, err := tx.Query(ctx, q, key)
	if err != nil {
		return fmt.Errorf("read audit chain: %w", err)
	}
	defer rs.Close()
	for rs.Next() {
		var c auditchain.Row
		var orgID, projectID, onBehalf *int32
		var schema int16
		if err := rs.Scan(&c.ID, &c.At, &c.ActorEmail, &c.ViaToken, &orgID, &projectID, &c.Action, &c.TargetType, &c.TargetID,
			&c.Summary, &c.Details, &schema, &c.ActorType, &c.AuthMethod, &c.TokenID, &c.SessionID, &onBehalf, &c.AgentProfile,
			&c.TargetName, &c.Outcome, &c.Reason, &c.SourceIP, &c.UserAgent, &c.Client, &c.RequestID, &c.TraceID, &c.Before, &c.After,
			&c.ChainKey, &c.ChainSeq, &c.PrevHash, &c.Hash); err != nil {
			return fmt.Errorf("read audit chain row: %w", err)
		}
		c.OrgID, c.ProjectID, c.OnBehalfOfUserID = widen(orgID), widen(projectID), widen(onBehalf)
		c.SchemaVersion = int64(schema)
		row(c)
	}
	return rs.Err()
}

func widen(n *int32) *int64 {
	if n == nil {
		return nil
	}
	v := int64(*n)
	return &v
}

// PurgeAuditChain deletes the rows of chain key written before cutoff,
// through audit_purge(): it signs (sign) and stores an anchor checkpoint at
// the last row it deletes, then deletes up to it, in one transaction. Only a
// prefix of the chain is deleted, so a row older than cutoff that follows a
// newer one (clock skew between writers) waits for the next run. It returns
// the anchor and how many rows went (nil, 0 when nothing was due).
func (r *Repo) PurgeAuditChain(ctx context.Context, key string, cutoff time.Time, sign func(*auditchain.Checkpoint)) (*auditchain.Checkpoint, int64, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Hold the head so nothing joins the chain meanwhile.
	var head auditchain.Head
	if err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit_chain_heads WHERE chain_key = $1 FOR UPDATE`, key).Scan(&head.Seq, &head.Hash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("lock audit chain head: %w", err)
	}
	// The last seq before the first row that is still in retention.
	var upto int64
	var hash []byte
	err = tx.QueryRow(ctx, `
		WITH keep AS (SELECT MIN(chain_seq) AS seq FROM audit_log WHERE chain_key = $1 AND at >= $2)
		SELECT l.chain_seq, l.hash FROM audit_log l, keep
		WHERE l.chain_key = $1 AND l.chain_seq < COALESCE(keep.seq, $3 + 1)
		ORDER BY l.chain_seq DESC LIMIT 1`, key, cutoff, head.Seq).Scan(&upto, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil // nothing old enough
	}
	if err != nil {
		return nil, 0, fmt.Errorf("find audit purge boundary: %w", err)
	}
	cp := &auditchain.Checkpoint{ChainKey: key, UptoSeq: upto, Hash: hash, At: time.Now(), Kind: auditchain.KindAnchor}
	sign(cp)
	if err := insertCheckpoint(ctx, tx, cp); err != nil {
		return nil, 0, err
	}
	var n int64
	if err := tx.QueryRow(ctx, `SELECT audit_purge($1, $2)`, key, upto).Scan(&n); err != nil {
		return nil, 0, fmt.Errorf("audit_purge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	return cp, n, nil
}

// ---- Durable audit outbox (audit_outbox, filled by a trigger) ----

// ClaimAuditOutbox leases up to limit queued audit entries for lease and
// returns them, oldest first. Another dispatcher skips them meanwhile; if
// this one dies before AckAuditOutbox, they come due again when the lease
// runs out, so every entry is matched at least once.
func (r *Repo) ClaimAuditOutbox(ctx context.Context, limit int, lease time.Duration) ([]domain.AuditEntry, error) {
	rows, err := r.db.Query(ctx, `
		WITH due AS (
			SELECT audit_id FROM audit_outbox WHERE next_attempt_at <= NOW()
			ORDER BY audit_id LIMIT $1 FOR UPDATE SKIP LOCKED
		), leased AS (
			UPDATE audit_outbox o SET next_attempt_at = NOW() + $2::interval, attempts = o.attempts + 1
			FROM due WHERE o.audit_id = due.audit_id
			RETURNING o.audit_id
		)
		SELECT `+auditCols+` FROM audit_log WHERE id IN (SELECT audit_id FROM leased) ORDER BY id`,
		limit, fmt.Sprintf("%d milliseconds", lease.Milliseconds()))
	if err != nil {
		return nil, fmt.Errorf("claim audit outbox: %w", err)
	}
	return collect(rows, scanAudit)
}

// AckAuditOutbox removes matched entries from the outbox.
func (r *Repo) AckAuditOutbox(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.db.Exec(ctx, `DELETE FROM audit_outbox WHERE audit_id = ANY($1)`, ids)
	return err
}
