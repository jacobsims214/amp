package main

// amp-relay's least privilege (AMP #2392, security review #2381 M3): its own
// Postgres role with only the grants repository.GrantRelayRole gives, its own
// cluster token, and a lookup-only verifier. Every relay test runs the relay
// side as that role (relayRoleRepo), so the grant list is proven by the whole
// relay suite; the tests here check what the role must NOT reach, and that
// the two cluster tokens are not interchangeable.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/cluster"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/relayctl"
	"github.com/simstech/amp-api/internal/repository"
)

// testRelayRole is the stand-in for the chart's amp_relay role.
const testRelayRole = "amp_relay_test"

var (
	relayRoleOnce sync.Once
	relayRoleDSN  string
	relayRoleRepo *repository.Repo
	relayRoleErr  error
)

// relayRole returns a repo connected as testRelayRole, created (once per test
// process) with a random password and granted by owner exactly what amp-api
// grants amp-relay. The test database user must be able to create roles (CI's
// and the documented local one are superusers).
func relayRole(t *testing.T, owner *repository.Repo) (*repository.Repo, string) {
	t.Helper()
	relayRoleOnce.Do(func() {
		ctx := context.Background()
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		pw := hex.EncodeToString(b)
		conn, err := pgx.Connect(ctx, testDSN(t))
		if err != nil {
			relayRoleErr = err
			return
		}
		defer conn.Close(ctx)
		if _, err = conn.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[1]s') THEN CREATE ROLE %[1]s LOGIN; END IF;
		END $$`, testRelayRole)); err != nil {
			relayRoleErr = fmt.Errorf("create role (the test database user needs CREATEROLE): %w", err)
			return
		}
		if _, err = conn.Exec(ctx, fmt.Sprintf(`ALTER ROLE %s LOGIN NOSUPERUSER PASSWORD '%s'`, testRelayRole, pw)); err != nil {
			relayRoleErr = err
			return
		}
		if relayRoleErr = owner.GrantRelayRole(ctx, testRelayRole); relayRoleErr != nil {
			return
		}
		u, err := url.Parse(testDSN(t))
		if err != nil {
			relayRoleErr = err
			return
		}
		u.User = url.UserPassword(testRelayRole, pw)
		relayRoleDSN = u.String()
		relayRoleRepo, relayRoleErr = repository.New(ctx, relayRoleDSN)
	})
	if relayRoleErr != nil {
		t.Fatalf("amp-relay's test role: %v", relayRoleErr)
	}
	return relayRoleRepo, relayRoleDSN
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// The role reaches what the relay needs and nothing else: no API token hashes,
// no cluster tokens, no tickets or KB, no webhook secrets, no writes to users.
func TestRelayRole_LeastPrivilege(t *testing.T) {
	w := newRBACWorld(t)
	_, dsn := relayRole(t, w.s.repo)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	var super bool
	if err := conn.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil || super {
		t.Fatalf("the relay role is a superuser (%v)", err)
	}
	denied := []string{
		`SELECT token_hash FROM api_tokens LIMIT 1`,
		`SELECT value FROM cluster_settings LIMIT 1`,
		`SELECT secret FROM webhooks LIMIT 1`,
		`SELECT * FROM webhooks LIMIT 1`,
		`SELECT description FROM tasks LIMIT 1`,
		`SELECT node_id FROM project_owners LIMIT 1`,
		`SELECT details FROM audit_log LIMIT 1`,
		`SELECT body FROM webhook_deliveries LIMIT 1`,
		`SELECT id FROM live_events LIMIT 1`,
		`UPDATE users SET display_name = display_name WHERE id = 0`,
		`INSERT INTO users (subject, email) VALUES ('x', 'x@relay.test')`,
		`INSERT INTO user_roles (user_id, role) VALUES (0, 'admin')`,
		`UPDATE orgs SET remote_control_enabled = TRUE WHERE id = 0`,
		`DELETE FROM project_owners`,
		`DELETE FROM audit_log WHERE id = 0`,
		`INSERT INTO cluster_settings (key, value) VALUES ('relay_token', 'x')`,
	}
	for _, q := range denied {
		_, err := conn.Exec(ctx, q)
		if pgCode(err) != "42501" { // insufficient_privilege
			t.Errorf("%s: want permission denied, got %v", q, err)
		}
	}
	allowed := []string{
		`SELECT id, subject, email FROM users LIMIT 1`,
		`SELECT id FROM remote_sessions LIMIT 1`,
		`SELECT id, url, events FROM webhooks LIMIT 1`,
		`SELECT version FROM schema_migrations LIMIT 1`,
		`SELECT id, addr FROM api_nodes LIMIT 1`,
	}
	for _, q := range allowed {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}

	// What the relay writes besides its own tables: the audit entry and the
	// webhook delivery for it, through the repository as the relay does.
	rr, _ := relayRole(t, w.s.repo)
	org := w.org.ID
	if _, err := rr.InsertAudit(ctx, domain.AuditEntry{ActorUserID: &w.alice.User.ID, ActorEmail: w.alice.Email, OrgID: &org,
		Action: "remote.test", TargetType: "remote_session", TargetID: "rs_role", Summary: "role test"}); err != nil {
		t.Fatalf("audit insert as the relay: %v", err)
	}
	hook, err := w.s.repo.CreateWebhook(ctx, domain.Webhook{OrgID: &org, Name: "relay role", URL: "https://hooks.example.test/x",
		Format: "json", Secret: "s3cret-not-for-the-relay", Events: []string{"audit.remote.*"}, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := rr.WebhooksForEvent(ctx, 0, org)
	if err != nil || len(hooks) == 0 {
		t.Fatalf("webhooks as the relay: %v %v", hooks, err)
	}
	if _, err := rr.EnqueueDelivery(ctx, hook.ID, "audit.remote.test", "{}"); err != nil {
		t.Fatalf("delivery as the relay: %v", err)
	}

	// Granting again is idempotent, and a missing role is reported as such.
	if err := w.s.repo.GrantRelayRole(ctx, testRelayRole); err != nil {
		t.Fatal(err)
	}
	if err := w.s.repo.GrantRelayRole(ctx, "amp_relay_missing_role"); !errors.Is(err, repository.ErrNoSuchRole) {
		t.Fatalf("missing role: %v", err)
	}
	if err := w.s.repo.GrantRelayRole(ctx, `x"; DROP TABLE users; --`); err == nil {
		t.Fatal("an odd role name was accepted")
	}
}

// The relay's role can't touch amp-api's replica rows (#2398, re-review
// #2394 N2). amp-api sends its own cluster token to a project owner's
// address; a relay that could rewrite an amp-api node's addr (or register a
// row amp-api would trust) would receive that token. Row-level security keeps
// the relay to the rows its role registered, and amp-api uses only rows its
// own role registered.
func TestRelayRole_CantRedirectAmpAPINodes(t *testing.T) {
	w := newRBACWorld(t)
	rr, dsn := relayRole(t, w.s.repo)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	suffix := fmt.Sprint(time.Now().UnixNano())
	apiNode, relayNode := "api-n2-"+suffix, "relay-n2-"+suffix
	const apiAddr, evilAddr = "10.0.0.7:3003", "10.0.0.66:3003"
	if err := w.s.repo.NodeHeartbeat(ctx, apiNode, apiAddr); err != nil {
		t.Fatal(err)
	}
	if err := rr.NodeHeartbeat(ctx, relayNode, evilAddr); err != nil {
		t.Fatalf("the relay's own heartbeat: %v", err)
	}
	t.Cleanup(func() {
		_ = w.s.repo.RemoveNode(context.Background(), apiNode)
		_ = w.s.repo.RemoveNode(context.Background(), relayNode)
	})

	// Writes to amp-api's row: refused (an error) or filtered out (no row).
	for _, q := range []string{
		`UPDATE api_nodes SET addr = '` + evilAddr + `' WHERE id = '` + apiNode + `'`,
		`DELETE FROM api_nodes WHERE id = '` + apiNode + `'`,
		`INSERT INTO api_nodes (id, addr) VALUES ('` + apiNode + `', '` + evilAddr + `')
		   ON CONFLICT (id) DO UPDATE SET addr = EXCLUDED.addr`,
		`INSERT INTO api_nodes (id, addr, registered_by) VALUES ('forged-` + suffix + `', '` + evilAddr + `', current_user || 'x')`,
		`UPDATE api_nodes SET registered_by = 'amp' WHERE id = '` + relayNode + `'`,
	} {
		tag, err := conn.Exec(ctx, q)
		switch {
		case err != nil && pgCode(err) != "42501":
			t.Errorf("%s: want a row-level security refusal, got %v", q, err)
		case err == nil && tag.RowsAffected() > 0:
			t.Errorf("%s: allowed (%d rows)", q, tag.RowsAffected())
		}
	}
	owner, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	var addr, by string
	if err := owner.QueryRow(ctx, `SELECT addr, registered_by FROM api_nodes WHERE id = $1`, apiNode).Scan(&addr, &by); err != nil {
		t.Fatalf("amp-api's row: %v", err)
	}
	if addr != apiAddr {
		t.Fatalf("the relay's role rewrote amp-api's node address to %s", addr)
	}
	if err := owner.QueryRow(ctx, `SELECT registered_by FROM api_nodes WHERE id = $1`, relayNode).Scan(&by); err != nil || by != testRelayRole {
		t.Fatalf("the relay's row is registered by %q (%v), want %s", by, err, testRelayRole)
	}
	// What the relay does with its own row still works.
	if err := rr.NodeHeartbeat(ctx, relayNode, evilAddr); err != nil {
		t.Fatalf("the relay's second heartbeat: %v", err)
	}
	if got, _, err := rr.NodeAddr(ctx, apiNode, time.Minute); err != nil || got != apiAddr {
		t.Fatalf("the relay can't read replica addresses: %v", err)
	}

	// amp-api never gives its token to an address the relay registered: a
	// project pointed at the relay's row (which the relay itself can't do; a
	// stale or forged ownership row stands in for it) is taken over, not
	// forwarded to the relay.
	project := 900000000 + int(time.Now().UnixNano()%1000000)
	if _, err := owner.Exec(ctx, `INSERT INTO project_owners (project_id, node_id) VALUES ($1, $2)`, project, relayNode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DELETE FROM project_owners WHERE project_id = $1`, project)
	})
	o, err := w.s.repo.ClaimProject(ctx, project, apiNode, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if o.NodeID != apiNode || o.Addr == evilAddr {
		t.Fatalf("amp-api's claim on a project pointed at the relay's row: owner %s at %q, want itself", o.NodeID, o.Addr)
	}
	// And a project owned by a live amp-api replica is forwarded to that
	// replica's own address.
	other := "api-n2b-" + suffix
	if err := w.s.repo.NodeHeartbeat(ctx, other, "10.0.0.8:3003"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.s.repo.RemoveNode(context.Background(), other) })
	o, err = w.s.repo.ClaimProject(ctx, project, other, time.Minute)
	if err != nil || o.NodeID != apiNode || o.Addr != apiAddr {
		t.Fatalf("another replica's claim: %+v %v, want %s at %s", o, err, apiNode, apiAddr)
	}
}

// The relay's cluster token is its own: never amp-api's, refused by amp-api's
// actor listener, and amp-api's token is refused by the relay's internal
// listener. amp-api's end notifier uses the relay's token.
func TestRelayRole_ClusterTokensAreSeparate(t *testing.T) {
	a, _ := twoRelays(t)
	ctx := context.Background()
	apiTok, err := a.w.s.repo.ClusterToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	relayTok, err := relayctl.Token(ctx, a.w.s.repo)
	if err != nil {
		t.Fatal(err)
	}
	if relayTok == "" || relayTok == apiTok || a.node.Token() != relayTok {
		t.Fatalf("relay token: empty, amp-api's, or not the relay node's (%v)", a.node.Token() == apiTok)
	}

	// The relay's internal listener: amp-api's token is refused; the relay
	// token reaches a control call.
	s := a.register(t, "alice")
	call := func(base, path, tok string) int {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("X-AMP-Cluster-Token", tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := call(a.internal.URL, "/internal/relay/ctl/presence?id="+s.id, apiTok); st != 401 {
		t.Fatalf("relay listener with amp-api's token: %d", st)
	}
	if st := call(a.internal.URL, "/internal/relay/ctl/presence?id="+s.id, relayTok); st != 200 {
		t.Fatalf("relay listener with the relay token: %d", st)
	}

	// amp-api's actor listener: the relay token is refused.
	sys := protoactor.NewActorSystem()
	t.Cleanup(sys.Shutdown)
	opts := fastCluster
	opts.ID, opts.Addr = fmt.Sprintf("api-%d", time.Now().UnixNano()), "127.0.0.1:1"
	apiNode, err := cluster.Join(ctx, a.w.s.repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = apiNode.Leave(context.Background()) })
	if apiNode.Token() != apiTok {
		t.Fatal("amp-api's node doesn't use amp-api's token")
	}
	reg := actor.NewRegistry(sys, a.w.s.repo, hub.New())
	reg.EnableCluster(apiNode)
	actorSrv := httptest.NewServer(reg.InternalHandler())
	defer actorSrv.Close()
	post := func(tok string) int {
		req, _ := http.NewRequest("POST", actorSrv.URL+"/internal/actor?project=1", strings.NewReader("{}"))
		req.Header.Set("X-AMP-Cluster-Token", tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := post(relayTok); st != 401 {
		t.Fatalf("amp-api's actor listener with the relay token: %d", st)
	}
	if st := post(apiTok); st == 401 {
		t.Fatal("amp-api's actor listener refused its own token")
	}
}

// RELAY_CLUSTER_TOKEN, when set, is the relay token (and must be long).
func TestRelayRole_ConfiguredToken(t *testing.T) {
	t.Setenv(relayctl.TokenEnv, "short")
	if _, err := relayctl.ConfiguredToken(); err == nil {
		t.Fatal("a short token was accepted")
	}
	tok := strings.Repeat("k", relayctl.MinTokenLen)
	t.Setenv(relayctl.TokenEnv, tok)
	got, err := relayctl.Token(context.Background(), nil)
	if err != nil || got != tok {
		t.Fatalf("configured token: %q %v", got, err)
	}
	t.Setenv(relayctl.TokenEnv, "")
	if got, err := relayctl.ConfiguredToken(); got != "" || err != nil {
		t.Fatalf("unset: %q %v", got, err)
	}
}

// The relay's verifier is lookup-only: a known person is found, an unknown
// one is not provisioned (no user on the identity, no row written), and an
// AMP API token is refused with token_not_allowed without a lookup. All of it
// as the relay's role.
func TestRelayRole_LookupOnlyVerifier(t *testing.T) {
	w := newRBACWorld(t)
	rr, _ := relayRole(t, w.s.repo)
	idp := newTestIdP(t)
	v, err := auth.NewVerifier(context.Background(), auth.Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, Repo: rr, LookupOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	h := v.WithAudiences("mcp", "amp-mcp").Middleware(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ident, _ := auth.FromContext(r.Context())
		if ident.User == nil {
			fmt.Fprint(rw, "no-user")
			return
		}
		fmt.Fprintf(rw, "user-%d", ident.User.ID)
	}))
	do := func(bearer string) (int, string) {
		req := httptest.NewRequest("POST", "/relay/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if st, body := do(idp.token(t, map[string]any{"sub": w.alice.Subject, "email": w.alice.Email, "aud": "amp-mcp"})); st != 200 || body != fmt.Sprintf("user-%d", w.alice.User.ID) {
		t.Fatalf("known person: %d %s", st, body)
	}
	stranger := fmt.Sprintf("stranger-%d@relay.test", time.Now().UnixNano())
	if st, body := do(idp.token(t, map[string]any{"sub": "sub-" + stranger, "email": stranger, "aud": "amp-mcp"})); st != 200 || body != "no-user" {
		t.Fatalf("unknown person: %d %s", st, body)
	}
	if _, err := w.s.repo.GetUserByEmail(context.Background(), stranger); err == nil {
		t.Fatal("the lookup-only verifier provisioned a user")
	}
	if st, body := do("amp_" + strings.Repeat("a", 40)); st != 403 || !strings.Contains(body, "token_not_allowed") {
		t.Fatalf("API token: %d %s", st, body)
	}
}
