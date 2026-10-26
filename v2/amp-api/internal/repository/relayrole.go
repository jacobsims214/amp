package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// ---- amp-relay's least-privilege database role (#2392) ----
//
// amp-relay (cmd/relay) connects as its own Postgres role, not amp-api's, so
// a compromised relay can't read or change the rest of AMP (security review
// #2381, M3). The role itself is created outside AMP (the Helm chart declares
// it as a CloudNativePG managed role with its own Secret); amp-api, which owns
// the tables, grants it exactly what the relay's code paths use, after every
// migration run (GrantRelayRole).

// relayTableGrants is everything amp-relay does in the database, by table.
// Keep it in step with what cmd/relay reaches: the relay (internal/relay),
// the service's remote control and audit code, the lookup-only verifier, the
// cluster membership (NoProjects) and webhook matching. The relay test
// environment runs the relay as a role with exactly these grants, so a query
// that needs more fails the relay tests.
var relayTableGrants = []struct{ privs, table string }{
	// It waits for amp-api's migration (MigrationApplied).
	{"SELECT", "schema_migrations"},
	// Callers: the lookup-only verifier and authz (no provisioning, no API
	// token lookups: api_tokens is not readable).
	{"SELECT", "users"},
	{"SELECT", "user_roles"},
	{"SELECT", "orgs"},
	{"SELECT", "org_members"},
	{"SELECT", "projects"},
	{"SELECT", "project_teams"},
	{"SELECT", "team_members"},
	{"SELECT", "teams"},
	// A session's ticket name, when the harness sent only its id
	// (remoteSessionCols): those three columns, no ticket content.
	{"SELECT (id, project_id, name)", "tasks"},
	// Remote control's own state.
	{"SELECT, INSERT, UPDATE, DELETE", "remote_sessions"},
	{"SELECT, INSERT, UPDATE, DELETE", "remote_devices"},
	// Replica membership: its own heartbeat row, and finding the holder.
	// Row-level security (relayNodePolicies) limits the writes to rows the
	// relay's role registered.
	{"SELECT, INSERT, UPDATE, DELETE", "api_nodes"},
	// remote.* audit entries (INSERT ... RETURNING id, at) and their webhook
	// deliveries (RETURNING id); the relay reads neither table back.
	{"INSERT", "audit_log"},
	{"SELECT (id, at)", "audit_log"},
	{"INSERT", "webhook_deliveries"},
	{"SELECT (id)", "webhook_deliveries"},
	// Matching webhooks for those entries: every column but the signing
	// secret (webhookCols never selects it).
	{"SELECT (id, org_id, project_id, name, url, format, events, active, created_by, created_at, updated_at, " +
		"last_delivery_at, last_status, last_error, consecutive_failures)", "webhooks"},
}

// relaySequences are the serial sequences behind the relay's INSERTs.
var relaySequences = [][2]string{{"audit_log", "id"}, {"webhook_deliveries", "id"}, {"remote_devices", "id"}}

// relayNodePolicies keep the relay's role to its own api_nodes rows (#2398,
// re-review #2394 N2). amp-api sends its cluster token to the address in an
// amp-api node's row; a relay that could rewrite that address would receive
// the token. registered_by is current_user by default, and the relay's role
// may insert, update and delete only rows registered_by itself (and can't
// change who registered a row). It still reads every row. amp-api owns the
// table, so row-level security doesn't apply to it; every other role keeps
// full access through api_nodes_visible.
func relayNodePolicies(role string) []string {
	return []string{
		`ALTER TABLE api_nodes ENABLE ROW LEVEL SECURITY`,
		`DROP POLICY IF EXISTS api_nodes_visible ON api_nodes`,
		`DROP POLICY IF EXISTS api_nodes_relay_insert ON api_nodes`,
		`DROP POLICY IF EXISTS api_nodes_relay_update ON api_nodes`,
		`DROP POLICY IF EXISTS api_nodes_relay_delete ON api_nodes`,
		`CREATE POLICY api_nodes_visible ON api_nodes USING (true) WITH CHECK (true)`,
		`CREATE POLICY api_nodes_relay_insert ON api_nodes AS RESTRICTIVE FOR INSERT TO ` + role +
			` WITH CHECK (registered_by = current_user)`,
		`CREATE POLICY api_nodes_relay_update ON api_nodes AS RESTRICTIVE FOR UPDATE TO ` + role +
			` USING (registered_by = current_user) WITH CHECK (registered_by = current_user)`,
		`CREATE POLICY api_nodes_relay_delete ON api_nodes AS RESTRICTIVE FOR DELETE TO ` + role +
			` USING (registered_by = current_user)`,
	}
}

var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ErrNoSuchRole is GrantRelayRole's answer when the role doesn't exist yet
// (the operator hasn't created it); the caller tries again later.
var ErrNoSuchRole = fmt.Errorf("role does not exist")

// GrantRelayRole gives role exactly the privileges amp-relay needs, and no
// others: it revokes everything the role has on this database's tables and
// sequences first, in the same transaction, so the grants follow the code as
// tables and queries change. Idempotent. The caller must own the tables
// (amp-api's role does).
func (r *Repo) GrantRelayRole(ctx context.Context, role string) error {
	if !roleName.MatchString(role) {
		return fmt.Errorf("relay role %q: not a plain lower-case identifier", role)
	}
	var exists bool
	if err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("relay role %s: %w", role, ErrNoSuchRole)
	}
	id := pgx.Identifier{role}.Sanitize()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	stmts := []string{
		`REVOKE ALL ON ALL TABLES IN SCHEMA public FROM ` + id,
		`REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM ` + id,
		`GRANT USAGE ON SCHEMA public TO ` + id,
	}
	for _, g := range relayTableGrants {
		stmts = append(stmts, fmt.Sprintf(`GRANT %s ON TABLE %s TO %s`, g.privs, pgx.Identifier{g.table}.Sanitize(), id))
	}
	stmts = append(stmts, relayNodePolicies(id)...)
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("granting the relay role: %s: %w", s, err)
		}
	}
	for _, ts := range relaySequences {
		var seq *string
		if err := tx.QueryRow(ctx, `SELECT pg_get_serial_sequence($1, $2)`, ts[0], ts[1]).Scan(&seq); err != nil {
			return err
		}
		if seq == nil {
			continue
		}
		// pg_get_serial_sequence returns an already quoted, qualified name.
		if _, err := tx.Exec(ctx, `GRANT USAGE ON SEQUENCE `+*seq+` TO `+id); err != nil {
			return fmt.Errorf("granting the relay role %s: %w", *seq, err)
		}
	}
	return tx.Commit(ctx)
}

// RelayClusterToken is the token amp-relay replicas use with each other and
// amp-api uses to call them (POST /internal/relay/ctl/end), when it isn't
// configured (RELAY_CLUSTER_TOKEN; the Helm chart always sets it). It is
// never amp-api's own cluster token (ClusterToken): holding it reaches only
// the relay's internal listener, never amp-api's actors. Created the first
// time; needs access to cluster_settings, which the least-privilege relay
// role doesn't have.
func (r *Repo) RelayClusterToken(ctx context.Context) (string, error) {
	return r.settingToken(ctx, "relay_token")
}

func (r *Repo) settingToken(ctx context.Context, key string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if _, err := r.db.Exec(ctx, `
		INSERT INTO cluster_settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO NOTHING`, key, hex.EncodeToString(b)); err != nil {
		return "", err
	}
	var tok string
	err := r.db.QueryRow(ctx, `SELECT value FROM cluster_settings WHERE key = $1`, key).Scan(&tok)
	return tok, err
}

// RemoveRelayNode forgets a relay replica (which never owns projects): only
// its own api_nodes row (row-level security allows no other).
func (r *Repo) RemoveRelayNode(ctx context.Context, id string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM api_nodes WHERE id = $1`, id)
	return err
}
