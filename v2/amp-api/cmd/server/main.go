package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // IANA time zones for daily reports, even in minimal images

	protoactor "github.com/asynkron/protoactor-go/actor"
	"github.com/hibiken/asynq"
	"github.com/rs/cors"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/cluster"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/mcp"
	"github.com/simstech/amp-api/internal/queue"
	"github.com/simstech/amp-api/internal/relayctl"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/reqctx"
	"github.com/simstech/amp-api/internal/service"
	"github.com/simstech/amp-api/internal/webhook"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrations is the ordered, versioned migration set (file name without
// .sql is the version) — also used directly by the test suite.
var migrations = mustLoadMigrations()

func mustLoadMigrations() []repository.Migration {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		panic(err)
	}
	var out []repository.Migration
	for _, e := range entries { // ReadDir returns entries sorted by name
		b, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			panic(err)
		}
		out = append(out, repository.Migration{Version: strings.TrimSuffix(e.Name(), ".sql"), SQL: string(b)})
	}
	return out
}

func main() {
	// `amp-api audit ...`: verify the audit chain, write checkpoints now,
	// or make a signing key (audit_cli.go).
	if len(os.Args) > 1 && os.Args[1] == "audit" {
		os.Exit(runAuditCLI(os.Args[2:], os.Stdout, os.Stderr))
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	dsn := envOrDefault("DATABASE_URL", "postgres://amp:amp@localhost:5432/amp?sslmode=disable")
	mcpAddr := envOrDefault("MCP_ADDR", ":8000")
	apiAddr := envOrDefault("API_ADDR", ":3001")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ---- 1. Database ----
	slog.Info("connecting to postgres", "dsn", redactDSN(dsn))
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		slog.Error("failed to connect to postgres", "err", err)
		os.Exit(1)
	}
	defer repo.Close()

	slog.Info("running migrations")
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		slog.Error("migration failed", "err", err)
		os.Exit(1)
	}
	// amp-relay's own least-privilege database role (#2392): when the chart
	// names it (RELAY_DB_ROLE), grant it exactly what the relay uses, now
	// that the tables are current. The role may not exist yet (the operator
	// creates it), so keep trying in the background.
	if role := strings.TrimSpace(os.Getenv("RELAY_DB_ROLE")); role != "" {
		go grantRelayRole(ctx, repo, role)
	}

	// Webhook secrets at rest (#2441): auth headers and signing secrets are
	// encrypted with AMP_SECRETS_KEY (Helm ampApi.secretsEncryption). Every
	// startup seals legacy plaintext and re-seals values under a retired key.
	configureSecretsKeys(ctx, repo)

	// Who may become an AMP user by signing in (e.g. through an SSO
	// connector): emails in these domains, invited emails, and bootstrap
	// admins. Unset allows anyone who can sign in to Dex.
	if d := splitList(os.Getenv("SIGNUP_ALLOWED_DOMAINS")); len(d) > 0 {
		repo.SetSignupDomains(d)
		slog.Info("sign-ups limited to domains (plus invites and bootstrap admins)", "domains", d)
	}

	// ---- 1b. Auth (OIDC verifier against Dex) ----
	// OIDC_AUDIENCES used to set an allow-list on this base verifier, but
	// every listener derives its own via WithAudiences (MCP_AUDIENCE,
	// REST_AUDIENCES below), so it never had an effect. It's no longer read.
	if os.Getenv("OIDC_AUDIENCES") != "" {
		slog.Warn("OIDC_AUDIENCES is deprecated and ignored; token audiences come from MCP_AUDIENCE and REST_AUDIENCES")
	}
	// UI_CLIENT_ID is the web UI's oauth2-proxy Dex client: only REST
	// requests with a token issued to it count as a person in the web UI,
	// the one channel that can approve plans and artifacts, turn plan
	// approval off, or mark a person's ticket done. Defaults to the first
	// REST_AUDIENCES entry (which Helm sets to that client).
	uiClientID := os.Getenv("UI_CLIENT_ID")
	if uiClientID == "" {
		if auds := splitList(os.Getenv("REST_AUDIENCES")); len(auds) > 0 {
			uiClientID = auds[0]
		}
	}
	verifier, err := auth.NewVerifier(ctx, auth.Config{
		IssuerURL:      os.Getenv("OIDC_ISSUER_URL"),
		DiscoveryURL:   os.Getenv("OIDC_DISCOVERY_URL"),
		BootstrapAdmin: os.Getenv("BOOTSTRAP_ADMIN_EMAILS"),
		Repo:           repo,
		UIClientID:     uiClientID,
	})
	if err != nil {
		slog.Error("failed to initialize auth verifier", "err", err)
		os.Exit(1)
	}

	// ---- 2. SSE hub ----
	sseHub := hub.New()

	// ---- 2b. Webhooks ----
	// Hub events and audit entries are matched to webhooks and delivered
	// from a Postgres outbox (internal/webhook). Webhooks can't reach
	// private networks unless WEBHOOK_ALLOW_PRIVATE_NETWORKS=true.
	allowPrivate := os.Getenv("WEBHOOK_ALLOW_PRIVATE_NETWORKS") == "true"
	service.AllowPrivateWebhooks = allowPrivate
	webhooks := webhook.New(repo, webhook.Options{AllowPrivateNetworks: allowPrivate})
	sseHub.AddSink(webhooks.Event)
	service.OnAudit(webhooks.Audit)
	go webhooks.Run(ctx)
	slog.Info("webhook dispatcher started", "allow_private_networks", allowPrivate)

	// ---- 3. Actor system ----
	system := protoactor.NewActorSystem()
	registry := actor.NewRegistry(system, repo, sseHub)

	// ---- 3b. Replicas ----
	// Any number of amp-api replicas can run against one database. Each
	// project's actors run on the replica that owns it; the others pass its
	// requests there over the internal listener, and live events reach
	// every replica (internal/cluster). CLUSTER_MODE=off runs every project
	// here, as before, and is only safe with a single replica.
	clusterCtx, stopCluster := context.WithCancel(context.Background())
	defer stopCluster()
	var node *cluster.Node
	var internalServer *http.Server
	if os.Getenv("CLUSTER_MODE") != "off" {
		internalAddr := envOrDefault("CLUSTER_ADDR", ":3003")
		advertise := os.Getenv("CLUSTER_ADVERTISE_ADDR")
		if advertise == "" {
			host, _ := os.Hostname()
			advertise = host + internalAddr[strings.LastIndex(internalAddr, ":"):]
		}
		node, err = cluster.Join(ctx, repo, cluster.Options{Addr: advertise})
		if err != nil {
			slog.Error("failed to join the replica group", "err", err)
			os.Exit(1)
		}
		registry.EnableCluster(node)
		go node.Run(clusterCtx)
		go cluster.NewRelay(repo, sseHub, node.ID()).Run(clusterCtx)
		internalServer = &http.Server{Addr: internalAddr, Handler: registry.InternalHandler()}
		go func() {
			slog.Info("replica listener up", "addr", internalAddr, "advertised", advertise, "node", node.ID())
			if err := internalServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("replica listener error", "err", err)
				os.Exit(1)
			}
		}()
	} else {
		slog.Warn("CLUSTER_MODE=off: this replica runs every project; never run more than one like this")
	}

	// Scheduled tickets unblock from their project's actor, so start the
	// actors of projects with a ticket coming due (after a restart, or when
	// a project's replica went away).
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			ids, err := repo.ProjectsWithDueSchedules(clusterCtx, time.Minute)
			if err != nil && clusterCtx.Err() == nil {
				slog.Warn("checking for scheduled tickets failed", "err", err)
			}
			for _, id := range ids {
				if err := registry.Touch(id); err != nil {
					slog.Warn("starting a project with scheduled tickets failed", "project_id", id, "err", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	// ---- 4. KB service (Typesense + Ollama) ----
	kbSvc := kb.New(
		envOrDefault("TYPESENSE_URL", "http://localhost:8108"),
		envOrDefault("TYPESENSE_API_KEY", "amp-local-dev"),
		envOrDefault("OLLAMA_URL", "http://localhost:11434"),
		envOrDefault("OLLAMA_MODEL", "nomic-embed-text"),
	)
	// Postgres is the source of truth for project, team and org KB docs;
	// Typesense is their search index (decisions/kb-postgres-source-of-truth.md).
	kbSvc.SetStore(repo)

	// ---- 4b. KB indexing queue (Valkey/Redis via asynq) ----
	// Optional: if REDIS_ADDR is unset (e.g. local `make dev` without
	// Valkey running), kbSvc falls back to writing/deleting synchronously —
	// see internal/kb/service.go. When configured, a burst of KB writes
	// gets smoothed out at a bounded worker concurrency instead of hitting
	// Typesense inline with the request.
	var asynqWorker *asynq.Server
	if redisAddr := os.Getenv("REDIS_ADDR"); redisAddr != "" {
		connOpt := queue.RedisConnOpt(redisAddr, os.Getenv("REDIS_PASSWORD"))

		queueClient := asynq.NewClient(connOpt)
		defer queueClient.Close()
		kbSvc.SetQueueClient(queueClient)

		concurrency := 4
		if c := os.Getenv("KB_INDEX_CONCURRENCY"); c != "" {
			if n, err := strconv.Atoi(c); err == nil && n > 0 {
				concurrency = n
			}
		}
		asynqWorker = asynq.NewServer(connOpt, asynq.Config{
			Concurrency: concurrency,
			Queues:      map[string]int{queue.QueueName: 1},
		})
		mux := asynq.NewServeMux()
		mux.HandleFunc(queue.TypeKBWriteDoc, kbSvc.HandleWriteDocTask)
		mux.HandleFunc(queue.TypeKBDeleteDoc, kbSvc.HandleDeleteDocTask)
		mux.HandleFunc(queue.TypeKBIndexDoc, kbSvc.HandleIndexDocTask)
		if err := asynqWorker.Start(mux); err != nil {
			slog.Error("failed to start asynq worker", "err", err)
			os.Exit(1)
		}
		slog.Info("KB indexing queue worker started", "redis_addr", redisAddr, "concurrency", concurrency, "queue", queue.QueueName)
	} else {
		slog.Warn("REDIS_ADDR not set — KB writes/deletes will run synchronously (fine for local dev, not recommended under real load)")
	}

	// ---- 4b'. KB backfill, then the reconciler ----
	// The per-KB reindex lock must be cross-replica before the backfill and
	// the first reconcile pass (mcp.NewServer sets the same lock later;
	// setting it twice is harmless).
	kbSvc.SetReindexLock(repo.TryNamedLock)
	//
	// The backfill copies every doc the Typesense index holds for a
	// project, team or org KB (archived projects and KBs whose owner is gone
	// included) into Postgres, insert-only, and marks each KB complete once
	// the copy is verified; reads, reindex and the reconciler wait for that.
	// A failed KB stays incomplete and the next startup retries it. One
	// replica runs it (advisory lock); it runs in the background and never
	// blocks startup. KB_BACKFILL=off skips it.
	//
	// The reconciler re-indexes docs that are in Postgres but missing or
	// stale in the search index (e.g. written while Typesense was down), and
	// removes index entries of deleted docs. It never writes Postgres. It
	// starts once the backfill run ends, then runs every
	// KB_RECONCILE_INTERVAL (default 15m; "off" or 0 disables); the advisory
	// lock lets only one replica run a pass.
	reconcileEvery := kbReconcileInterval()
	if reconcileEvery <= 0 {
		slog.Info("KB reconciler disabled (KB_RECONCILE_INTERVAL)")
	}
	go func() {
		if os.Getenv("KB_BACKFILL") != "off" {
			kbSvc.RunBackfill(ctx, repo.ListKBOwnerScopes, repo.TryNamedLock)
		} else {
			slog.Warn("KB backfill disabled (KB_BACKFILL=off): KBs not yet backfilled stay incomplete")
		}
		if reconcileEvery > 0 {
			kbSvc.RunReconciler(ctx, reconcileEvery, repo.TryNamedLock)
		}
	}()

	// ---- 4c. AMP guide (the global KB) ----
	// The image ships the repo's kb/ folder; every replica syncs it into the
	// global KB in the background (idempotent upserts), so a new image
	// brings its guide with it. See internal/kb/global.go.
	if os.Getenv("AMP_GLOBAL_KB_SYNC") != "off" {
		go kbSvc.SyncGlobalWithRetry(ctx, envOrDefault("AMP_GLOBAL_KB_DIR", "/app/kb"))
	}

	// ---- 5. MCP server (port 8000) ----
	// Streamable HTTP, dual-era: requests carrying 2026-07-28 per-request
	// _meta are served statelessly; legacy clients that open with
	// initialize (2025-11-25 and earlier) keep their Mcp-Session-Id
	// session exactly as before. See mcp_http.go.
	mcpTools := mcp.NewServer(registry, repo, sseHub, kbSvc)
	mcpTools.SetAppURL(appPublicURL())
	mcpTools.SetArtifactRetention(artifactRetention())
	mcpSrv := newMCPServer(mcpTools)

	// Token audience (RFC 8707 / MCP 2026-07-28 "token audience binding").
	// MCP tokens must carry the MCP_AUDIENCE client ID, which the
	// amp-mcpoauth broker obtains via Dex cross-client trust; the REST API
	// additionally accepts the UI's oauth2-proxy client. Tokens issued
	// before this check existed lack the audience and get a 401, which
	// sends MCP clients back through a one-time browser login.
	mcpAudience := envOrDefault("MCP_AUDIENCE", "amp-mcp")
	restAudiences := append(splitList(os.Getenv("REST_AUDIENCES")), mcpAudience)

	mcpCfg := mcpHTTPConfig{
		PublicURL:      envOrDefault("MCP_PUBLIC_URL", fmt.Sprintf("http://localhost%s", mcpAddr)),
		OAuthURL:       envOrDefault("MCP_OAUTH_PUBLIC_URL", "http://localhost:8091"),
		Scopes:         []string{"openid", "email", "profile"},
		AllowedOrigins: splitList(os.Getenv("MCP_ALLOWED_ORIGINS")),
	}
	mcpVerifier := verifier.WithAudiences("mcp", mcpAudience)
	mcpVerifier.ResourceMetadataURL = mcpCfg.PublicURL + "/.well-known/oauth-protected-resource"
	mcpVerifier.Scope = strings.Join(mcpCfg.Scopes, " ")
	restVerifier := verifier.WithAudiences("rest", restAudiences...)
	// Sign-ins, accounts created at first sign-in, accepted invites, API
	// token use from a new address, and rejected tokens go to the audit log.
	authEvents := service.New(repo, registry, kbSvc).RecordAuthEvent
	mcpVerifier.OnEvent, restVerifier.OnEvent = authEvents, authEvents
	slog.Info("token audience policy", "mcp", mcpAudience, "rest", restAudiences, "ui_client", uiClientID)
	if os.Getenv("OIDC_ISSUER_URL") != "" && uiClientID == "" {
		slog.Warn("UI_CLIENT_ID and REST_AUDIENCES unset: no request counts as the web UI, so nobody can approve a plan")
	}

	// Every listener resolves the trusted client IP, user agent and request
	// id for the audit log (AMP_TRUSTED_PROXIES; see internal/reqctx).
	ipResolver := reqctx.FromEnv()

	mcpPublicServer := &http.Server{
		Addr:    mcpAddr,
		Handler: reqctx.Middleware(reqctx.ListenerMCP, ipResolver)(newMCPHTTPHandler(mcpSrv, mcpVerifier, mcpCfg)),
	}
	go func() {
		slog.Info("MCP public server listening (Streamable HTTP, auth-enforced)", "addr", mcpAddr)
		if err := mcpPublicServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("MCP public server error", "err", err)
		}
	}()

	// ---- 6. REST + SSE API (port 3001) ----
	restHandler := api.NewRestHandler(registry, repo, sseHub, kbSvc)
	restHandler.SetAppURL(appPublicURL())
	restHandler.SetArtifactRetention(artifactRetention())
	// ---- 6a. Remote control (epic 165) ----
	// The relay itself is its own service, amp-relay (cmd/relay): amp-api
	// serves none of its routes (/relay/, /api/sessions). It keeps the org
	// setting (/api/orgs/:id/remote-control) and the capability on /api/me,
	// where RELAY_ENABLED (Helm relay.enabled, which also deploys amp-relay)
	// is only the signal that a relay is deployed. Sessions an org admin ends
	// here are closed on the relay replica holding them, over its internal
	// listener (relayctl); with no relay running there is nothing to close.
	// amp-relay's internal listener takes the relay cluster token
	// (RELAY_CLUSTER_TOKEN, else its own database row), never amp-api's.
	if _, err := relayctl.ConfiguredToken(); err != nil {
		slog.Error("relay cluster token", "err", err)
		os.Exit(1)
	}
	remoteCfg := remoteConfig()
	restHandler.SetRemoteControl(remoteCfg)
	hookRelayEnds(restHandler.Service(), repo, node)
	mux := apiMux(restHandler)
	// Artifacts nobody kept are deleted after the retention period.
	go restHandler.RunArtifactPurge(context.Background(), time.Hour)
	// Trash rows (deleted tickets, epics, stories) past the 30-day retention
	// are deleted. Idempotent, so every replica can run it.
	go service.New(repo, registry, kbSvc).RunTrashPurge(context.Background(), time.Hour)

	// Audit log integrity (#2429): sign each chain's head hourly, and
	// delete entries past AUDIT_RETENTION_DAYS daily through the anchored
	// path. One replica at a time runs each (advisory locks).
	service.AuditKeys = loadAuditKeys()
	// The dispatcher records turning a failing webhook off and dropping
	// deliveries (#2439 M4).
	webhooks.OnSystemAudit(service.New(repo, registry, kbSvc).RecordSystem)
	auditRetention := auditRetentionFromEnv()
	service.AuditRetention = auditRetention
	if auditRetention > 0 {
		if ready, err := repo.AuditPurgeRoleReady(ctx); err != nil {
			slog.Warn("can't tell whether audit retention can run", "err", err)
		} else if !ready {
			slog.Warn("audit retention is off until a superuser sets up the amp_audit_purge role: run `amp-api audit purge-role-sql` and feed it to psql as a superuser (see KB admin/trash-audit-and-projects)")
		}
	}
	go service.New(repo, registry, kbSvc).RunAuditCheckpoints(ctx, time.Hour, repo.TryNamedLock)
	go service.New(repo, registry, kbSvc).RunAuditRetention(ctx, auditRetention, 24*time.Hour, repo.TryNamedLock)

	// Health check (unauthenticated, used by k8s probes)
	var draining atomic.Bool
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	// Readiness: not while shutting down, nor while this replica can't
	// heartbeat (it isn't serving its projects then).
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case draining.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"shutting down"}`)
		case node != nil && !node.Alive():
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"no database heartbeat"}`)
		default:
			fmt.Fprint(w, `{"status":"ok"}`)
		}
	})

	corsMiddleware := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Content-Type", "Authorization"},
	})

	// Auth wraps everything except /health.
	var apiHandler http.Handler = mux
	apiHandler = api.RejectCrossSiteWrites(apiHandler)
	apiHandler = authExceptHealth(restVerifier, apiHandler)
	apiHandler = corsMiddleware.Handler(apiHandler)
	apiHandler = reqctx.Middleware(reqctx.ListenerREST, ipResolver)(apiHandler)

	apiServer := &http.Server{
		Addr:    apiAddr,
		Handler: apiHandler,
	}

	go func() {
		slog.Info("REST API server listening", "addr", apiAddr)
		if err := apiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("API server error", "err", err)
		}
	}()

	// ---- 6. Wait for shutdown ----
	<-ctx.Done()
	slog.Info("shutting down")

	// Stop being ready, and give the load balancer time to notice before
	// refusing connections (SHUTDOWN_DELAY, e.g. "5s" in Kubernetes).
	draining.Store(true)
	if d, err := time.ParseDuration(os.Getenv("SHUTDOWN_DELAY")); err == nil && d > 0 {
		time.Sleep(d)
	}

	// Finish requests in flight, including ones other replicas passed here;
	// then stop this replica's actors and hand its projects over at once.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = apiServer.Shutdown(shutdownCtx)
	_ = mcpPublicServer.Shutdown(shutdownCtx)
	if internalServer != nil {
		_ = internalServer.Shutdown(shutdownCtx)
	}
	if asynqWorker != nil {
		asynqWorker.Shutdown()
	}
	registry.StopAll()
	system.Shutdown()
	if node != nil {
		if err := node.Leave(context.Background()); err != nil {
			slog.Warn("could not hand this replica's projects over; others take them over when its heartbeat expires", "err", err)
		}
	}
}

// redactDSN hides the password in a database URL for logging.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable DATABASE_URL)"
	}
	return u.Redacted()
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// authExceptHealth applies the auth verifier to every path except /health
// and /ready, which k8s liveness/readiness probes hit unauthenticated.
func authExceptHealth(v *auth.Verifier, next http.Handler) http.Handler {
	authed := v.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}
		authed.ServeHTTP(w, r)
	})
}

// apiMux is amp-api's REST routes (the /health and /ready probes are added
// by main). It has no remote control relay route: the relay is amp-relay.
func apiMux(h *api.RestHandler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// hookRelayEnds makes sessions svc ends (an org turning remote control off)
// close on the amp-relay replica holding them.
// grantRelayRole applies amp-relay's grants (repository.GrantRelayRole),
// retrying until the role exists and the grants succeed, or ctx ends.
func grantRelayRole(ctx context.Context, repo *repository.Repo, role string) {
	wait := 5 * time.Second
	for {
		err := repo.GrantRelayRole(ctx, role)
		if err == nil {
			slog.Info("amp-relay's database role has its grants", "role", role)
			return
		}
		if errors.Is(err, repository.ErrNoSuchRole) {
			slog.Info("amp-relay's database role doesn't exist yet; trying again", "role", role, "in", wait)
		} else {
			slog.Warn("granting amp-relay's database role failed; trying again", "role", role, "err", err, "in", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < 2*time.Minute {
			wait *= 2
		}
	}
}

func hookRelayEnds(svc *service.Service, repo *repository.Repo, node *cluster.Node) {
	ttl := time.Duration(0)
	if node != nil {
		ttl = node.TTL()
	}
	svc.OnRemoteEnded = relayctl.NewEndNotifier(repo, ttl).Notify
}

// appPublicURL is AMP's public address, for links to pages in the UI:
// APP_PUBLIC_URL, or MCP_PUBLIC_URL without its /mcp path. Empty when
// neither is set (local dev), and links are left out.
func appPublicURL() string {
	if u := os.Getenv("APP_PUBLIC_URL"); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	if u := os.Getenv("MCP_PUBLIC_URL"); u != "" {
		return strings.TrimSuffix(strings.TrimSuffix(u, "/"), "/mcp")
	}
	return ""
}

// artifactRetention is how long an artifact lives after its last version
// unless someone keeps it: ARTIFACT_RETENTION_DAYS, default 45.
func artifactRetention() time.Duration {
	if d, err := strconv.Atoi(os.Getenv("ARTIFACT_RETENTION_DAYS")); err == nil && d > 0 {
		return time.Duration(d) * 24 * time.Hour
	}
	return 0 // the service default
}

// kbReconcileInterval is how often the KB reconciler runs, from
// KB_RECONCILE_INTERVAL (a Go duration, e.g. "10m"). Unset means 15
// minutes; "off", "0" or a non-positive duration disables it. An
// unparseable value falls back to the default.
func kbReconcileInterval() time.Duration {
	const def = 15 * time.Minute
	v := strings.TrimSpace(os.Getenv("KB_RECONCILE_INTERVAL"))
	switch v {
	case "":
		return def
	case "off", "0":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("bad KB_RECONCILE_INTERVAL; using the default", "value", v, "default", def.String())
		return def
	}
	return d
}

// remoteConfig is the instance's remote control configuration (epic 165).
// RELAY_ENABLED (Helm relay.enabled, which also deploys amp-relay) says a
// relay is deployed, so /api/me can offer remote control; off unless set to
// true, and never on with auth off (OIDC_ISSUER_URL empty: compose and local
// dev, where every caller is a global admin). It turns no route on here.
func remoteConfig() service.RemoteConfig {
	on, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("RELAY_ENABLED")))
	cfg := service.RemoteConfig{InstanceEnabled: on, AuthEnabled: os.Getenv("OIDC_ISSUER_URL") != ""}
	if cfg.InstanceEnabled && !cfg.AuthEnabled {
		slog.Warn("RELAY_ENABLED is set but auth is off (no OIDC_ISSUER_URL): remote control stays unavailable")
	}
	return cfg
}
