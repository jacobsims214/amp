package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// namedLockSpace is the first key of the two-key pg_try_advisory_lock form
// used by TryNamedLock. Two-key locks never collide with single-key ones
// such as migrationLockKey.
const namedLockSpace = 0x616d70 // "amp"

// TryNamedLock takes a session-level Postgres advisory lock named key,
// without waiting, so that only one replica at a time does the work it
// guards. ok is false when another session holds it. release unlocks it
// and must be called exactly once when ok is true.
//
// The lock lives on a connection of its own, opened for it outside the
// pool and closed on release. It must not hold a pooled connection: the
// work it guards runs through the pool, and the lock holders (audit
// checkpoints, audit retention, the KB backfill with a per-KB lock inside
// its run lock, the KB reconciler, KB reindexes) can outnumber a small
// pool. When they held pooled connections, four of them at pgxpool's
// default size on a 4-CPU host (4) left none for their own work or for
// the cluster heartbeat, and amp-api hung at startup (#2598).
func (r *Repo) TryNamedLock(ctx context.Context, key string) (release func(), ok bool, err error) {
	conn, err := pgx.ConnectConfig(ctx, r.db.Config().ConnConfig.Copy())
	if err != nil {
		return nil, false, fmt.Errorf("connect for advisory lock %q: %w", key, err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`, namedLockSpace, key).Scan(&ok); err != nil {
		_ = conn.Close(context.Background())
		return nil, false, fmt.Errorf("advisory lock %q: %w", key, err)
	}
	if !ok {
		_ = conn.Close(context.Background())
		return nil, false, nil
	}
	return func() {
		// Closing the session releases its advisory locks even if the
		// unlock fails or times out.
		uctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1, hashtext($2))`, namedLockSpace, key)
		_ = conn.Close(uctx)
	}, true, nil
}
