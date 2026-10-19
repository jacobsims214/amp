package repository

import (
	"context"
	"fmt"
)

// namedLockSpace is the first key of the two-key pg_try_advisory_lock form
// used by TryNamedLock. Two-key locks never collide with single-key ones
// such as migrationLockKey.
const namedLockSpace = 0x616d70 // "amp"

// TryNamedLock takes a session-level Postgres advisory lock named key,
// without waiting, so that only one replica at a time does the work it
// guards. ok is false when another session holds it. While held, the lock
// keeps one pool connection; release unlocks and returns it, and must be
// called exactly once when ok is true.
func (r *Repo) TryNamedLock(ctx context.Context, key string) (release func(), ok bool, err error) {
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire connection: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`, namedLockSpace, key).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("advisory lock %q: %w", key, err)
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, hashtext($2))`, namedLockSpace, key); err != nil {
			// Unlocking failed, so the session may still hold the lock:
			// close the connection rather than hand it back to the pool.
			_ = conn.Conn().Close(context.Background())
		}
		conn.Release()
	}, true, nil
}
