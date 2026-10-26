package repository

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Remote control (epic 165): org policy, session registry, devices ----
//
// The store never holds message content: sessions and devices are metadata,
// and the relay keeps chat payloads in memory only.

// GetOrgRemotePolicy is orgID's remote control setting.
func (r *Repo) GetOrgRemotePolicy(ctx context.Context, orgID int) (domain.RemotePolicy, error) {
	var p domain.RemotePolicy
	err := r.db.QueryRow(ctx, `SELECT remote_control_enabled, remote_idle_timeout_min, remote_max_lifetime_hours,
		remote_device_pin_days FROM orgs WHERE id = $1`, orgID).Scan(&p.Enabled, &p.IdleTimeoutMin, &p.MaxLifetimeHours,
		&p.DevicePinDays)
	if err == pgx.ErrNoRows {
		return p, fmt.Errorf("org %d: %w", orgID, ErrNotFound)
	}
	return p, err
}

// SetOrgRemotePolicy stores orgID's remote control setting. The values must
// already be in range (the table checks them too).
func (r *Repo) SetOrgRemotePolicy(ctx context.Context, orgID int, p domain.RemotePolicy) error {
	tag, err := r.db.Exec(ctx, `UPDATE orgs SET remote_control_enabled = $2, remote_idle_timeout_min = $3,
		remote_device_pin_days = $4, remote_max_lifetime_hours = $5 WHERE id = $1`,
		orgID, p.Enabled, p.IdleTimeoutMin, p.DevicePinDays, p.MaxLifetimeHours)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("org %d: %w", orgID, ErrNotFound)
	}
	return err
}

// RemoteEnabledOrgIDs lists the orgs userID is a member of that have remote
// control turned on, by id.
func (r *Repo) RemoteEnabledOrgIDs(ctx context.Context, userID int) ([]int, error) {
	rows, err := r.db.Query(ctx, `SELECT o.id FROM orgs o JOIN org_members om ON om.org_id = o.id
		WHERE om.user_id = $1 AND o.remote_control_enabled ORDER BY o.id`, userID)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row rowScanner, id *int) error { return row.Scan(id) })
}

// ProjectRemotePolicy is the remote control policy that applies to a
// project: its org's. A project with no org gets the default (off).
func (r *Repo) ProjectRemotePolicy(ctx context.Context, projectID int) (domain.RemotePolicy, *int, error) {
	p := domain.DefaultRemotePolicy()
	var orgID *int
	var enabled *bool
	var idle, life, pin *int
	err := r.db.QueryRow(ctx, `SELECT p.org_id, o.remote_control_enabled, o.remote_idle_timeout_min,
		o.remote_max_lifetime_hours, o.remote_device_pin_days
		FROM projects p LEFT JOIN orgs o ON o.id = p.org_id WHERE p.id = $1`, projectID).Scan(&orgID, &enabled, &idle, &life, &pin)
	if err == pgx.ErrNoRows {
		return p, nil, fmt.Errorf("project %d: %w", projectID, ErrNotFound)
	}
	if err != nil {
		return p, nil, err
	}
	if enabled != nil {
		p = domain.RemotePolicy{Enabled: *enabled, IdleTimeoutMin: *idle, MaxLifetimeHours: *life, DevicePinDays: *pin}
	}
	return p, orgID, nil
}

const remoteSessionCols = `id, owner_user_id, project_id, host, os, harness_version, harness_id, harness_pub,
	protocol_version, holder_node, state, end_reason, started_at, heartbeat_at, last_activity_at, expires_at, ended_at,
	connect_token_expires_at, model, current_ticket_id,
	COALESCE(current_ticket_name, (SELECT t.name FROM tasks t
		WHERE t.id = remote_sessions.current_ticket_id AND t.project_id = remote_sessions.project_id))`

func scanRemoteSession(row rowScanner, s *domain.RemoteSession) error {
	return row.Scan(&s.ID, &s.OwnerUserID, &s.ProjectID, &s.Host, &s.OS, &s.HarnessVersion, &s.HarnessID, &s.HarnessPub,
		&s.ProtocolVersion, &s.HolderNode, &s.State, &s.EndReason, &s.StartedAt, &s.HeartbeatAt, &s.LastActivityAt,
		&s.ExpiresAt, &s.EndedAt, &s.ConnectTokenExpiresAt, &s.Model, &s.CurrentTicketID, &s.CurrentTicketName)
}

// CreateRemoteSession inserts an active session. ID, OwnerUserID, ProjectID,
// HarnessID and HarnessPub are required; ExpiresAt is nil for a session with
// no lifetime (#2424); the times not given default to now.
func (r *Repo) CreateRemoteSession(ctx context.Context, s domain.RemoteSession) (*domain.RemoteSession, error) {
	if s.ProtocolVersion == 0 {
		s.ProtocolVersion = 1
	}
	var out domain.RemoteSession
	err := scanRemoteSession(r.db.QueryRow(ctx, `INSERT INTO remote_sessions
		(id, owner_user_id, project_id, host, os, harness_version, harness_id, harness_pub, protocol_version, holder_node, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+remoteSessionCols,
		s.ID, s.OwnerUserID, s.ProjectID, s.Host, s.OS, s.HarnessVersion, s.HarnessID, s.HarnessPub, s.ProtocolVersion,
		s.HolderNode, s.ExpiresAt), &out)
	if err != nil {
		return nil, fmt.Errorf("create remote session: %w", err)
	}
	return &out, nil
}

// DeleteRemoteSession removes a session row outright. Only for undoing a
// registration whose fail-closed audit write failed: ended sessions are kept.
func (r *Repo) DeleteRemoteSession(ctx context.Context, id string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM remote_sessions WHERE id = $1`, id)
	return err
}

// GetRemoteSession returns a session by id, active or ended.
func (r *Repo) GetRemoteSession(ctx context.Context, id string) (*domain.RemoteSession, error) {
	var s domain.RemoteSession
	err := scanRemoteSession(r.db.QueryRow(ctx, `SELECT `+remoteSessionCols+` FROM remote_sessions WHERE id = $1`, id), &s)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("remote session: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ListActiveRemoteSessions lists ownerUserID's live sessions, newest first.
func (r *Repo) ListActiveRemoteSessions(ctx context.Context, ownerUserID int) ([]domain.RemoteSession, error) {
	rows, err := r.db.Query(ctx, `SELECT `+remoteSessionCols+` FROM remote_sessions
		WHERE owner_user_id = $1 AND state = 'active' ORDER BY started_at DESC, id`, ownerUserID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRemoteSession)
}

// ListRecentRemoteSessions lists ownerUserID's live sessions and, after
// them, up to limit of the sessions that ended within the window (newest
// first). This is GET /api/sessions (#2389).
func (r *Repo) ListRecentRemoteSessions(ctx context.Context, ownerUserID int, window time.Duration, limit int) ([]domain.RemoteSession, error) {
	rows, err := r.db.Query(ctx, `(SELECT `+remoteSessionCols+` FROM remote_sessions
			WHERE owner_user_id = $1 AND state = 'active' ORDER BY started_at DESC, id)
		UNION ALL
		(SELECT `+remoteSessionCols+` FROM remote_sessions
			WHERE owner_user_id = $1 AND state = 'ended' AND ended_at > NOW() - $2::float8 * interval '1 second'
			ORDER BY ended_at DESC, id LIMIT $3)`, ownerUserID, window.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	list, err := collect(rows, scanRemoteSession)
	if err != nil {
		return nil, err
	}
	// UNION ALL doesn't promise to keep each branch's order.
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Active() != b.Active() {
			return a.Active()
		}
		ta, tb := a.StartedAt, b.StartedAt
		if !a.Active() && a.EndedAt != nil && b.EndedAt != nil {
			ta, tb = *a.EndedAt, *b.EndedAt
		}
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return a.ID < b.ID
	})
	return list, nil
}

// SetRemoteStatusMeta stores the model and current ticket from a live
// session's status. It changes nothing once the session has ended.
func (r *Repo) SetRemoteStatusMeta(ctx context.Context, id string, m domain.RemoteStatusMeta) error {
	_, err := r.db.Exec(ctx, `UPDATE remote_sessions SET model = $2, current_ticket_id = $3, current_ticket_name = $4
		WHERE id = $1 AND state = 'active'`, id, m.Model, m.TicketID, m.TicketName)
	return err
}

// CountActiveRemoteSessions counts ownerUserID's live sessions.
func (r *Repo) CountActiveRemoteSessions(ctx context.Context, ownerUserID int) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM remote_sessions WHERE owner_user_id = $1 AND state = 'active'`,
		ownerUserID).Scan(&n)
	return n, err
}

// SetRemoteConnectToken records the hash of a session's new connect token.
// Returns ErrNotFound unless the session is active.
func (r *Repo) SetRemoteConnectToken(ctx context.Context, id, tokenHash string, expiresAt time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE remote_sessions SET connect_token_hash = $2, connect_token_expires_at = $3
		WHERE id = $1 AND state = 'active'`, id, tokenHash, expiresAt)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("remote session: %w", ErrNotFound)
	}
	return err
}

// RemoteSessionByConnectToken finds the session whose current connect token
// hashes to tokenHash (active or ended; the caller checks state and expiry).
// A deactivated owner's token finds nothing (#2390): deactivation ends the
// owner's sessions, and this holds even if that end didn't happen.
func (r *Repo) RemoteSessionByConnectToken(ctx context.Context, tokenHash string) (*domain.RemoteSession, error) {
	var s domain.RemoteSession
	err := scanRemoteSession(r.db.QueryRow(ctx, `SELECT `+remoteSessionCols+` FROM remote_sessions
		WHERE connect_token_hash = $1
		  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = owner_user_id AND u.disabled_at IS NOT NULL)`, tokenHash), &s)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("remote session: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ClaimRemoteSession records that node (an api_nodes id) now holds the
// session's command stream, and counts it as a heartbeat. Returns false when
// the session isn't active.
func (r *Repo) ClaimRemoteSession(ctx context.Context, id, node string) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE remote_sessions SET holder_node = $2, heartbeat_at = NOW()
		WHERE id = $1 AND state = 'active'`, id, node)
	return tag.RowsAffected() > 0, err
}

// TouchRemoteHeartbeat records a harness heartbeat. Returns false when the
// session isn't active.
func (r *Repo) TouchRemoteHeartbeat(ctx context.Context, id string) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE remote_sessions SET heartbeat_at = NOW() WHERE id = $1 AND state = 'active'`, id)
	return tag.RowsAffected() > 0, err
}

// TouchRemoteActivity records remote activity (an accepted command or a new
// viewer), which resets the idle timer. Returns false when the session isn't
// active.
func (r *Repo) TouchRemoteActivity(ctx context.Context, id string) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE remote_sessions SET last_activity_at = NOW() WHERE id = $1 AND state = 'active'`, id)
	return tag.RowsAffected() > 0, err
}

// EndRemoteSession ends an active session with reason and returns it; nil
// (and no error) when it had already ended. The connect token stops working
// (calls with it get 410).
func (r *Repo) EndRemoteSession(ctx context.Context, id, reason string) (*domain.RemoteSession, error) {
	ended, err := r.endRemoteSessions(ctx, `id = $2`, reason, id)
	if err != nil || len(ended) == 0 {
		return nil, err
	}
	return &ended[0], nil
}

// EndOwnerRemoteSessions ends every live session of ownerUserID.
func (r *Repo) EndOwnerRemoteSessions(ctx context.Context, ownerUserID int, reason string) ([]domain.RemoteSession, error) {
	return r.endRemoteSessions(ctx, `owner_user_id = $2`, reason, ownerUserID)
}

// EndOrgRemoteSessions ends every live session on orgID's projects (the org
// turned remote control off).
func (r *Repo) EndOrgRemoteSessions(ctx context.Context, orgID int, reason string) ([]domain.RemoteSession, error) {
	return r.endRemoteSessions(ctx, `project_id IN (SELECT id FROM projects WHERE org_id = $2)`, reason, orgID)
}

// remoteOwnerHasAccess is true when a session's owner can still see its
// project: the same rule as authz.ProjectLevel >= Member (a global admin, an
// owner or admin of the project's org, or a member of a team linked to the
// project).
const remoteOwnerHasAccess = `(
	EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = remote_sessions.owner_user_id AND ur.role = 'admin')
	OR EXISTS (SELECT 1 FROM org_members om JOIN projects p ON p.org_id = om.org_id
		WHERE p.id = remote_sessions.project_id AND om.user_id = remote_sessions.owner_user_id AND om.role IN ('owner', 'admin'))
	OR EXISTS (SELECT 1 FROM project_teams pt JOIN team_members tm ON tm.team_id = pt.team_id
		WHERE pt.project_id = remote_sessions.project_id AND tm.user_id = remote_sessions.owner_user_id))`

// EndRemoteSessionsWithoutAccess ends (with reason) the live sessions whose
// owner can no longer see the session's project: removed from its team, the
// team unlinked, removed from the org (re-review #2394 N7). With owners, only
// their sessions are checked; with none, every live session.
func (r *Repo) EndRemoteSessionsWithoutAccess(ctx context.Context, reason string, owners ...int) ([]domain.RemoteSession, error) {
	if len(owners) > 0 {
		return r.endRemoteSessions(ctx, `owner_user_id = ANY($2) AND NOT `+remoteOwnerHasAccess, reason, owners)
	}
	return r.endRemoteSessions(ctx, `NOT `+remoteOwnerHasAccess, reason)
}

// EndExpiredRemoteSessions ends live sessions past their lifetime (reason
// expired) and those idle longer than their org's idle timeout plus grace
// (reason idle; the harness is authoritative, this is the backstop). Both are
// opt-in (#2424): a session with no expires_at has no lifetime, and an org
// whose idle timeout is 0 (the default) has no idle limit.
func (r *Repo) EndExpiredRemoteSessions(ctx context.Context, now time.Time, grace time.Duration) ([]domain.RemoteSession, error) {
	expired, err := r.endRemoteSessions(ctx, `expires_at IS NOT NULL AND expires_at <= $2`, domain.RemoteEndExpired, now)
	if err != nil {
		return nil, err
	}
	idle, err := r.endRemoteSessions(ctx, `EXISTS (SELECT 1 FROM projects p JOIN orgs o ON o.id = p.org_id
			WHERE p.id = project_id AND o.remote_idle_timeout_min > 0
			AND last_activity_at + make_interval(mins => o.remote_idle_timeout_min) + make_interval(secs => $3) <= $2)`,
		domain.RemoteEndIdle, now, grace.Seconds())
	if err != nil {
		return nil, err
	}
	return append(expired, idle...), nil
}

// endRemoteSessions ends the active sessions matching where ($2.. are args)
// and returns them as they are now. The connect token's hash is kept (the
// token itself is useless once the session ended), so a harness call with it
// gets 410 session_ended with the reason on every replica, not a 401.
func (r *Repo) endRemoteSessions(ctx context.Context, where, reason string, args ...any) ([]domain.RemoteSession, error) {
	rows, err := r.db.Query(ctx, `UPDATE remote_sessions SET state = 'ended', end_reason = $1, ended_at = NOW()
		WHERE state = 'active' AND `+where+` RETURNING `+remoteSessionCols, append([]any{reason}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("end remote sessions: %w", err)
	}
	return collect(rows, scanRemoteSession)
}

// ---- Which replica holds a session (multi-replica relay, #2377) ----

// RemoteHolder is the replica that holds a session's live state (its
// harness stream, viewers, buffer and limits).
type RemoteHolder struct {
	NodeID string // "" when the session has no holder
	Addr   string // the holder's internal listener
	Alive  bool   // the holder heartbeated within the TTL
	Active bool   // the session hasn't ended
}

// RemoteSessionHolder reports who holds session id, judging the holder alive
// when its api_nodes heartbeat is within ttl (by the database clock, as
// ClaimProject does).
func (r *Repo) RemoteSessionHolder(ctx context.Context, id string, ttl time.Duration) (RemoteHolder, error) {
	var h RemoteHolder
	var node, addr *string
	var alive *bool
	var state string
	err := r.db.QueryRow(ctx, `SELECT s.state, s.holder_node, n.addr, n.heartbeat_at > NOW() - $2::float8 * interval '1 second'
		FROM remote_sessions s LEFT JOIN api_nodes n ON n.id = s.holder_node WHERE s.id = $1`, id, ttl.Seconds()).
		Scan(&state, &node, &addr, &alive)
	if err == pgx.ErrNoRows {
		return h, fmt.Errorf("remote session: %w", ErrNotFound)
	}
	if err != nil {
		return h, err
	}
	h.Active = state == domain.RemoteSessionActive
	if node != nil {
		h.NodeID = *node
	}
	if addr != nil {
		h.Addr = *addr
	}
	h.Alive = alive != nil && *alive
	return h, nil
}

// TakeRemoteSession makes node the holder of active session id unless a
// replica that heartbeated within ttl already holds it. It reports whether
// node holds it now.
func (r *Repo) TakeRemoteSession(ctx context.Context, id, node string, ttl time.Duration) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE remote_sessions s SET holder_node = $2
		WHERE s.id = $1 AND s.state = 'active' AND (s.holder_node IS NULL OR s.holder_node = $2 OR NOT EXISTS (
			SELECT 1 FROM api_nodes n WHERE n.id = s.holder_node AND n.heartbeat_at > NOW() - $3::float8 * interval '1 second'))`,
		id, node, ttl.Seconds())
	return tag.RowsAffected() > 0, err
}

// NodeAddr is a replica's internal address, and whether it heartbeated
// within ttl. An unknown node is not alive.
func (r *Repo) NodeAddr(ctx context.Context, node string, ttl time.Duration) (string, bool, error) {
	var addr string
	var alive bool
	err := r.db.QueryRow(ctx, `SELECT addr, heartbeat_at > NOW() - $2::float8 * interval '1 second' FROM api_nodes WHERE id = $1`,
		node, ttl.Seconds()).Scan(&addr, &alive)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	return addr, alive, err
}

// RemoteSessionsByID returns the sessions with these ids, active or ended.
func (r *Repo) RemoteSessionsByID(ctx context.Context, ids []string) ([]domain.RemoteSession, error) {
	rows, err := r.db.Query(ctx, `SELECT `+remoteSessionCols+` FROM remote_sessions WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRemoteSession)
}

// RecentRemoteRegistrations counts ownerUserID's sessions started within
// window (on any replica) and how long until the oldest of them leaves it.
func (r *Repo) RecentRemoteRegistrations(ctx context.Context, ownerUserID int, window time.Duration) (int, time.Duration, error) {
	var n int
	var wait float64
	err := r.db.QueryRow(ctx, `SELECT COUNT(*), COALESCE(EXTRACT(EPOCH FROM MIN(started_at) + $2::float8 * interval '1 second' - NOW()), 0)
		FROM remote_sessions WHERE owner_user_id = $1 AND started_at > NOW() - $2::float8 * interval '1 second'`,
		ownerUserID, window.Seconds()).Scan(&n, &wait)
	return n, time.Duration(wait * float64(time.Second)), err
}

// ---- Devices ----

const remoteDeviceCols = `id, owner_user_id, harness_id, device_id, device_pub, name, paired_at, expires_at, revoked_at`

func scanRemoteDevice(row rowScanner, d *domain.RemoteDevice) error {
	return row.Scan(&d.ID, &d.OwnerUserID, &d.HarnessID, &d.DeviceID, &d.DevicePub, &d.Name, &d.PairedAt, &d.ExpiresAt, &d.RevokedAt)
}

// UpsertRemoteDevice records a device pairing (a re-pair refreshes the key,
// name and expiry and clears a revoke). PairedAt defaults to now.
func (r *Repo) UpsertRemoteDevice(ctx context.Context, d domain.RemoteDevice) (*domain.RemoteDevice, error) {
	if d.PairedAt.IsZero() {
		d.PairedAt = time.Now()
	}
	var out domain.RemoteDevice
	err := scanRemoteDevice(r.db.QueryRow(ctx, `INSERT INTO remote_devices
		(owner_user_id, harness_id, device_id, device_pub, name, paired_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owner_user_id, harness_id, device_id) DO UPDATE SET device_pub = EXCLUDED.device_pub,
			name = EXCLUDED.name, paired_at = EXCLUDED.paired_at, expires_at = EXCLUDED.expires_at, revoked_at = NULL
		RETURNING `+remoteDeviceCols,
		d.OwnerUserID, d.HarnessID, d.DeviceID, d.DevicePub, d.Name, d.PairedAt, d.ExpiresAt), &out)
	if err != nil {
		return nil, fmt.Errorf("upsert remote device: %w", err)
	}
	return &out, nil
}

// GetRemoteDevice returns ownerUserID's pairing of deviceID with harnessID,
// revoked or expired included (the caller checks).
func (r *Repo) GetRemoteDevice(ctx context.Context, ownerUserID int, harnessID, deviceID string) (*domain.RemoteDevice, error) {
	var d domain.RemoteDevice
	err := scanRemoteDevice(r.db.QueryRow(ctx, `SELECT `+remoteDeviceCols+` FROM remote_devices
		WHERE owner_user_id = $1 AND harness_id = $2 AND device_id = $3`, ownerUserID, harnessID, deviceID), &d)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("remote device: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListRemoteDevices lists ownerUserID's devices that are neither revoked nor
// expired; harnessID "" means every harness.
func (r *Repo) ListRemoteDevices(ctx context.Context, ownerUserID int, harnessID string) ([]domain.RemoteDevice, error) {
	rows, err := r.db.Query(ctx, `SELECT `+remoteDeviceCols+` FROM remote_devices
		WHERE owner_user_id = $1 AND ($2 = '' OR harness_id = $2) AND revoked_at IS NULL AND expires_at > NOW()
		ORDER BY paired_at DESC, id`, ownerUserID, harnessID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRemoteDevice)
}

// RevokeRemoteDevices revokes ownerUserID's devices: one device when
// deviceID is set, else all of them. Returns how many it revoked.
func (r *Repo) RevokeRemoteDevices(ctx context.Context, ownerUserID int, deviceID string) (int, error) {
	tag, err := r.db.Exec(ctx, `UPDATE remote_devices SET revoked_at = NOW()
		WHERE owner_user_id = $1 AND ($2 = '' OR device_id = $2) AND revoked_at IS NULL`, ownerUserID, deviceID)
	return int(tag.RowsAffected()), err
}

// PruneRemoteDevices deletes ownerUserID's devices for harnessID that the
// harness no longer lists (protocol 7.6: the relay reconciles from status).
func (r *Repo) PruneRemoteDevices(ctx context.Context, ownerUserID int, harnessID string, keep []string) (int, error) {
	if keep == nil {
		keep = []string{}
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM remote_devices WHERE owner_user_id = $1 AND harness_id = $2
		AND NOT (device_id = ANY($3))`, ownerUserID, harnessID, keep)
	return int(tag.RowsAffected()), err
}

// MigrationApplied reports whether amp-api has applied the migration version
// (e.g. "027_remote_control"). amp-relay never migrates; it waits for this.
func (r *Repo) MigrationApplied(ctx context.Context, version string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&ok)
	if err != nil || !ok {
		return false, err
	}
	err = r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&ok)
	return ok, err
}
