package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- Replicas and project ownership (see internal/cluster) ----

// NodeHeartbeat records that node id, reachable at addr, is alive. The row
// is registered by this connection's role (registered_by, #2398): amp-api
// trusts only its own role's rows, and amp-relay's role can write only its.
func (r *Repo) NodeHeartbeat(ctx context.Context, id, addr string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO api_nodes (id, addr) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET addr = EXCLUDED.addr, heartbeat_at = NOW(), registered_by = current_user`, id, addr)
	return err
}

// RemoveNode forgets a node and releases every project it owned, so other
// replicas take them over at once instead of waiting for it to time out.
func (r *Repo) RemoveNode(ctx context.Context, id string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM project_owners WHERE node_id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM api_nodes WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PruneNodes forgets nodes that stopped heartbeating more than olderThan
// ago, and the ownership rows still pointing at them.
func (r *Repo) PruneNodes(ctx context.Context, olderThan time.Duration) error {
	_, err := r.db.Exec(ctx, `
		WITH gone AS (DELETE FROM api_nodes WHERE heartbeat_at < NOW() - $1::float8 * interval '1 second' RETURNING id)
		DELETE FROM project_owners WHERE node_id IN (SELECT id FROM gone)`, olderThan.Seconds())
	return err
}

// ProjectOwner is the replica that runs a project's actors.
type ProjectOwner struct {
	NodeID string
	Addr   string
	Epoch  int64
}

// ClaimProject makes node the owner of a project unless another node that
// heartbeated within ttl already owns it. It returns the owner either way:
// node itself when the claim succeeded (or it already owned the project).
//
// Only nodes registered by this connection's role count (#2398): the owner's
// address gets amp-api's cluster token, so a row another role wrote (the
// relay's) is never alive for a claim and never gives an address.
func (r *Repo) ClaimProject(ctx context.Context, projectID int, node string, ttl time.Duration) (ProjectOwner, error) {
	var o ProjectOwner
	err := r.db.QueryRow(ctx, `
		INSERT INTO project_owners (project_id, node_id) VALUES ($1, $2)
		ON CONFLICT (project_id) DO UPDATE
		   SET node_id = EXCLUDED.node_id,
		       epoch = CASE WHEN project_owners.node_id = EXCLUDED.node_id THEN project_owners.epoch ELSE project_owners.epoch + 1 END,
		       acquired_at = CASE WHEN project_owners.node_id = EXCLUDED.node_id THEN project_owners.acquired_at ELSE NOW() END
		 WHERE project_owners.node_id = EXCLUDED.node_id
		    OR NOT EXISTS (SELECT 1 FROM api_nodes n
		                    WHERE n.id = project_owners.node_id AND n.registered_by = current_user
		                      AND n.heartbeat_at > NOW() - $3::float8 * interval '1 second')
		RETURNING node_id, epoch`, projectID, node, ttl.Seconds()).Scan(&o.NodeID, &o.Epoch)
	if err == nil {
		return o, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return o, err
	}
	// Someone alive owns it.
	err = r.db.QueryRow(ctx, `
		SELECT o.node_id, COALESCE(n.addr, ''), o.epoch
		  FROM project_owners o LEFT JOIN api_nodes n ON n.id = o.node_id AND n.registered_by = current_user
		 WHERE o.project_id = $1`, projectID).Scan(&o.NodeID, &o.Addr, &o.Epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		// Released between the two statements; the caller tries again.
		return o, fmt.Errorf("project %d: ownership changed while claiming", projectID)
	}
	return o, err
}

// ClusterToken returns the token replicas use to call each other, creating
// it the first time. Anything that can read the database is already
// trusted with everything the token protects.
func (r *Repo) ClusterToken(ctx context.Context) (string, error) {
	return r.settingToken(ctx, "internal_token")
}

// ProjectsWithDueSchedules lists projects with a ticket waiting on a start
// time that has passed or will within the window, so their actors can be
// started to unblock it.
func (r *Repo) ProjectsWithDueSchedules(ctx context.Context, within time.Duration) ([]int, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT project_id FROM tasks
		 WHERE start_at IS NOT NULL AND start_at <= NOW() + $1::float8 * interval '1 second'
		   AND state = 'blocked' AND block_reason LIKE 'scheduled:%'`, within.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// ---- Live events between replicas ----

// Notify sends payload on a Postgres NOTIFY channel.
func (r *Repo) Notify(ctx context.Context, channel, payload string) error {
	_, err := r.db.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, payload)
	return err
}

// PutLiveEvent stores an event too big for a NOTIFY payload.
func (r *Repo) PutLiveEvent(ctx context.Context, body string) (int64, error) {
	var id int64
	err := r.db.QueryRow(ctx, `INSERT INTO live_events (body) VALUES ($1) RETURNING id`, body).Scan(&id)
	return id, err
}

// LiveEvent reads an event stored by PutLiveEvent.
func (r *Repo) LiveEvent(ctx context.Context, id int64) (string, error) {
	var body string
	err := r.db.QueryRow(ctx, `SELECT body FROM live_events WHERE id = $1`, id).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return body, err
}

// PruneLiveEvents deletes stored events older than olderThan.
func (r *Repo) PruneLiveEvents(ctx context.Context, olderThan time.Duration) error {
	_, err := r.db.Exec(ctx, `DELETE FROM live_events WHERE at < NOW() - $1::float8 * interval '1 second'`, olderThan.Seconds())
	return err
}

// Listen calls fn with every payload sent on channel until ctx ends or the
// connection fails (it returns the error; the caller reconnects).
// ready, if not nil, is called once the LISTEN is in place.
func (r *Repo) Listen(ctx context.Context, channel string, ready func(), fn func(payload string)) error {
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return err
	}
	// The connection may be left mid-wait; don't hand it back to the pool.
	c := conn.Hijack()
	defer c.Close(context.Background()) //nolint:errcheck
	if _, err := c.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	for {
		n, err := c.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Payload)
	}
}
