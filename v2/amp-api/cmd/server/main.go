package main

import (
	"context"
	"embed"
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
	"github.com/simstech/amp-api/internal/repository"
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

	// Who may become an AMP user by signing in (e.g. through an SSO
	// connector): emails in these domains, invited emails, and bootstrap
	// admins. Unset allows anyone who can sign in to Dex.
	if d := splitList(os.Getenv("SIGNUP_ALLOWED_DOMAINS")); len(d) > 0 {
		repo.SetSignupDomains(d)
		slog.Info("sign-ups limited to domains (plus invites and bootstrap admins)", "domains", d)
	}

	// ---- 1b. Auth (OIDC verifier against Dex) ----
	var audiences []string
	if a := os.Getenv("OIDC_AUDIENCES"); a != "" {
		audiences = strings.Split(a, ",")
	}
	verifier, err := auth.NewVerifier(ctx, auth.Config{
		IssuerURL:      os.Getenv("OIDC_ISSUER_URL"),
		DiscoveryURL:   os.Getenv("OIDC_DISCOVERY_URL"),
		Audiences:      audiences,
		BootstrapAdmin: os.Getenv("BOOTSTRAP_ADMIN_EMAILS"),
		Repo:           repo,
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
		if err := asynqWorker.Start(mux); err != nil {
			slog.Error("failed to start asynq worker", "err", err)
			os.Exit(1)
		}
		slog.Info("KB indexing queue worker started", "redis_addr", redisAddr, "concurrency", concurrency, "queue", queue.QueueName)
	} else {
		slog.Warn("REDIS_ADDR not set — KB writes/deletes will run synchronously (fine for local dev, not recommended under real load)")
	}

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
	slog.Info("token audience policy", "mcp", mcpAudience, "rest", restAudiences)

	mcpPublicServer := &http.Server{
		Addr:    mcpAddr,
		Handler: newMCPHTTPHandler(mcpSrv, mcpVerifier, mcpCfg),
	}
	go func() {
		slog.Info("MCP public server listening (Streamable HTTP, auth-enforced)", "addr", mcpAddr)
		if err := mcpPublicServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("MCP public server error", "err", err)
		}
	}()

	// ---- 6. REST + SSE API (port 3001) ----
	mux := http.NewServeMux()
	restHandler := api.NewRestHandler(registry, repo, sseHub, kbSvc)
	restHandler.SetAppURL(appPublicURL())
	restHandler.SetArtifactRetention(artifactRetention())
	restHandler.Register(mux)
	// Artifacts nobody kept are deleted after the retention period.
	go restHandler.RunArtifactPurge(context.Background(), time.Hour)

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
	apiHandler = authExceptHealth(restVerifier, apiHandler)
	apiHandler = corsMiddleware.Handler(apiHandler)

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
