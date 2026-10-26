// Command relay is amp-relay, the remote control relay (epic 165, #2387): the
// one service that joins a person's amp-harness to the same person's browser.
// It serves only the relay (internal/relayserver): the harness leg
// (/relay/v1/...), the browser leg (/api/sessions/...), its probes, and its
// internal listener for forwarding between relay replicas. amp-api serves
// none of these, so with no amp-relay pod running, nobody can remote into a
// harness.
//
// It shares the database with amp-api but never migrates it: it waits until
// amp-api has applied the remote control migration. In Kubernetes it connects
// as its own least-privilege role (amp_relay, #2392), which amp-api grants
// only what the relay uses (repository.GrantRelayRole): it never provisions
// users (its verifier is lookup-only) and can't read amp-api's cluster token. It joins the replica
// group (api_nodes) so its replicas can find each other and amp-api can reach
// it (POST /internal/relay/ctl/end when an org turns remote control off); it
// never runs project actors.
//
// Environment (the same names amp-api uses):
//
//	DATABASE_URL                 Postgres (the amp_relay role in Kubernetes)
//	RELAY_CLUSTER_TOKEN          the relay's own cluster token (>= 32 chars), shared
//	                             with amp-api's calls to it; default: its own
//	                             cluster_settings row (needs amp-api's role)
//	RELAY_ADDR                   public listener (default :3002)
//	CLUSTER_ADDR                 internal listener (default :3003)
//	CLUSTER_ADVERTISE_ADDR       how other replicas reach it (default host:port)
//	OIDC_ISSUER_URL              required: the relay never runs without auth
//	OIDC_DISCOVERY_URL, BOOTSTRAP_ADMIN_EMAILS, SIGNUP_ALLOWED_DOMAINS
//	MCP_AUDIENCE, REST_AUDIENCES, UI_CLIENT_ID
//	MCP_PUBLIC_URL               for the OAuth challenge's resource metadata
//	APP_PUBLIC_URL               the web origin (default: MCP_PUBLIC_URL without /mcp)
//	SHUTDOWN_DELAY               e.g. 5s in Kubernetes
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/cluster"
	"github.com/simstech/amp-api/internal/relay"
	"github.com/simstech/amp-api/internal/relayctl"
	"github.com/simstech/amp-api/internal/relayserver"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
	"github.com/simstech/amp-api/internal/webhook"
)

// schemaVersion is the amp-api migration the relay needs.
const schemaVersion = "029_api_nodes_registered_by"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).With("service", "amp-relay"))
	if err := run(); err != nil {
		slog.Error("amp-relay stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	issuer := os.Getenv("OIDC_ISSUER_URL")
	if issuer == "" {
		return errors.New("OIDC_ISSUER_URL is required: the relay never runs without auth")
	}
	dsn := env("DATABASE_URL", "postgres://amp:amp@localhost:5432/amp?sslmode=disable")
	publicAddr := env("RELAY_ADDR", ":3002")
	internalAddr := env("CLUSTER_ADDR", ":3003")

	slog.Info("connecting to postgres", "dsn", redactDSN(dsn))
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer repo.Close()
	if err := waitForSchema(ctx, repo); err != nil {
		return err
	}
	if d := splitList(os.Getenv("SIGNUP_ALLOWED_DOMAINS")); len(d) > 0 {
		repo.SetSignupDomains(d)
	}

	// The same verifiers amp-api builds: the harness's OAuth token is checked
	// for the MCP audience (as /mcp does), the browser's for the REST ones.
	// Lookup-only: the person is already an AMP user (amp-api provisions on
	// sign-in); the relay never writes users or reads API tokens.
	uiClientID := os.Getenv("UI_CLIENT_ID")
	if uiClientID == "" {
		if auds := splitList(os.Getenv("REST_AUDIENCES")); len(auds) > 0 {
			uiClientID = auds[0]
		}
	}
	verifier, err := auth.NewVerifier(ctx, auth.Config{
		IssuerURL:      issuer,
		DiscoveryURL:   os.Getenv("OIDC_DISCOVERY_URL"),
		BootstrapAdmin: os.Getenv("BOOTSTRAP_ADMIN_EMAILS"),
		Repo:           repo,
		UIClientID:     uiClientID,
		LookupOnly:     true,
	})
	if err != nil {
		return fmt.Errorf("auth verifier: %w", err)
	}
	mcpAudience := env("MCP_AUDIENCE", "amp-mcp")
	mcpPublicURL := env("MCP_PUBLIC_URL", "http://localhost:8000")
	harnessVerifier := verifier.WithAudiences("mcp", mcpAudience)
	harnessVerifier.ResourceMetadataURL = mcpPublicURL + "/.well-known/oauth-protected-resource"
	harnessVerifier.Scope = "openid email profile"
	browserVerifier := verifier.WithAudiences("rest", append(splitList(os.Getenv("REST_AUDIENCES")), mcpAudience)...)

	// Join the replica group: relay replicas forward a session's requests to
	// the one holding it, and amp-api finds the holder to end its sessions.
	advertise := os.Getenv("CLUSTER_ADVERTISE_ADDR")
	if advertise == "" {
		host, _ := os.Hostname()
		advertise = host + internalAddr[strings.LastIndex(internalAddr, ":"):]
	}
	// The relay's own cluster token, never amp-api's: relay replicas use it
	// with each other and amp-api uses it to reach them (relayctl).
	token, err := relayctl.Token(ctx, repo)
	if err != nil {
		return fmt.Errorf("relay cluster token (set %s): %w", relayctl.TokenEnv, err)
	}
	node, err := cluster.Join(ctx, repo, cluster.Options{Addr: advertise, Token: token, NoProjects: true})
	if err != nil {
		return fmt.Errorf("joining the replica group: %w", err)
	}
	clusterCtx, stopCluster := context.WithCancel(context.Background())
	defer stopCluster()
	go node.Run(clusterCtx)

	// remote.* audit entries still reach webhooks: the relay writes the
	// outbox rows, amp-api delivers them (the relay makes no outbound calls).
	webhooks := webhook.New(repo, webhook.Options{})
	service.OnAudit(webhooks.Audit)
	go webhooks.RunMatching(ctx)

	appURL := appPublicURL(mcpPublicURL)
	svc := service.New(repo, nil, nil)
	svc.AppURL = appURL
	svc.Remote = service.RemoteConfig{InstanceEnabled: true, AuthEnabled: true}

	var draining atomic.Bool
	srv := relayserver.New(svc, relayserver.Options{
		Relay:       relay.Config{AppURL: appURL, Cluster: node},
		HarnessAuth: harnessVerifier.Middleware,
		BrowserAuth: browserVerifier.Middleware,
		Ready: func() error {
			switch {
			case draining.Load():
				return errors.New("shutting down")
			case !node.Alive():
				return errors.New("no database heartbeat")
			}
			return nil
		},
	})
	go srv.Run(ctx)

	internal := &http.Server{Addr: internalAddr, Handler: srv.InternalHandler()}
	public := &http.Server{Addr: publicAddr, Handler: srv.Handler()}
	errc := make(chan error, 2)
	go func() {
		slog.Info("relay internal listener up", "addr", internalAddr, "advertised", advertise, "node", node.ID())
		if err := internal.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- fmt.Errorf("internal listener: %w", err)
		}
	}()
	go func() {
		slog.Info("remote control relay listening", "addr", publicAddr, "app_url", appURL)
		if err := public.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- fmt.Errorf("public listener: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	slog.Info("shutting down")
	draining.Store(true)
	if d, err := time.ParseDuration(os.Getenv("SHUTDOWN_DELAY")); err == nil && d > 0 {
		time.Sleep(d)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	// Long-lived streams don't finish on their own: Shutdown waits for the
	// deadline, then Close drops them; clients reconnect to another replica,
	// which takes the sessions over once this one has left.
	if err := public.Shutdown(sctx); err != nil {
		_ = public.Close()
	}
	_ = internal.Shutdown(sctx)
	if err := node.Leave(context.Background()); err != nil {
		slog.Warn("leaving the replica group failed; others take this replica's sessions over when its heartbeat expires", "err", err)
	}
	return nil
}

// waitForSchema waits until amp-api has applied the remote control migration.
func waitForSchema(ctx context.Context, repo *repository.Repo) error {
	for i := 0; ; i++ {
		ok, err := repo.MigrationApplied(ctx, schemaVersion)
		if ok {
			return nil
		}
		if i%10 == 0 {
			slog.Info("waiting for amp-api to apply the database migrations", "need", schemaVersion, "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// appPublicURL is the web origin: APP_PUBLIC_URL, or MCP_PUBLIC_URL without
// its /mcp path (as amp-api derives it).
func appPublicURL(mcpPublicURL string) string {
	if u := os.Getenv("APP_PUBLIC_URL"); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	if os.Getenv("MCP_PUBLIC_URL") == "" {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(mcpPublicURL, "/"), "/mcp")
}

func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable DATABASE_URL)"
	}
	return u.Redacted()
}
