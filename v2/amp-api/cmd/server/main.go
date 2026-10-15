package main

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	"github.com/hibiken/asynq"
	"github.com/rs/cors"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/mcp"
	"github.com/simstech/amp-api/internal/queue"
	"github.com/simstech/amp-api/internal/repository"
)

//go:embed migrations/001_init.sql
var migration001 string

//go:embed migrations/002_task_start_at.sql
var migration002 string

//go:embed migrations/003_users.sql
var migration003 string

// migrationSQL is the full, concatenated migration set — also used directly
// by the test suite to spin up a schema.
var migrationSQL = migration001 + "\n" + migration002 + "\n" + migration003

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	dsn := envOrDefault("DATABASE_URL", "postgres://amp:amp@localhost:5432/amp?sslmode=disable")
	mcpAddr := envOrDefault("MCP_ADDR", ":8000")
	apiAddr := envOrDefault("API_ADDR", ":3001")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ---- 1. Database ----
	slog.Info("connecting to postgres", "dsn", dsn)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		slog.Error("failed to connect to postgres", "err", err)
		os.Exit(1)
	}
	defer repo.Close()

	slog.Info("running migrations")
	if err := repo.Migrate(ctx, migrationSQL); err != nil {
		slog.Error("migration failed", "err", err)
		os.Exit(1)
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

	// ---- 3. Actor system ----
	system := protoactor.NewActorSystem()
	registry := actor.NewRegistry(system, repo, sseHub)

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

	// ---- 5. MCP server (port 8000) ----
	// Streamable HTTP, dual-era: requests carrying 2026-07-28 per-request
	// _meta are served statelessly; legacy clients that open with
	// initialize (2025-11-25 and earlier) keep their Mcp-Session-Id
	// session exactly as before. See mcp_http.go.
	mcpSrv := newMCPServer(mcp.NewServer(registry, repo, sseHub, kbSvc))

	// Token audience (RFC 8707 / MCP 2026-07-28 "token audience binding").
	// MCP tokens must carry the MCP_AUDIENCE client ID, which the
	// amp-mcpoauth broker obtains via Dex cross-client trust; the REST API
	// additionally accepts the UI's oauth2-proxy client. AUDIENCE_MODE
	// (off|warn|enforce) staggers rollout: warn logs tokens that enforce
	// would reject so pre-upgrade clients can be watched before enforcing.
	audienceMode, err := auth.ParseAudienceMode(os.Getenv("AUDIENCE_MODE"))
	if err != nil {
		slog.Error("invalid AUDIENCE_MODE", "err", err)
		os.Exit(1)
	}
	mcpAudience := envOrDefault("MCP_AUDIENCE", "amp-mcp")
	restAudiences := append(splitList(os.Getenv("REST_AUDIENCES")), mcpAudience)

	mcpCfg := mcpHTTPConfig{
		PublicURL:      envOrDefault("MCP_PUBLIC_URL", fmt.Sprintf("http://localhost%s", mcpAddr)),
		OAuthURL:       envOrDefault("MCP_OAUTH_PUBLIC_URL", "http://localhost:8091"),
		Scopes:         []string{"openid", "email", "profile"},
		AllowedOrigins: splitList(os.Getenv("MCP_ALLOWED_ORIGINS")),
	}
	mcpVerifier := verifier.WithAudiences("mcp", audienceMode, mcpAudience)
	mcpVerifier.ResourceMetadataURL = mcpCfg.PublicURL + "/.well-known/oauth-protected-resource"
	mcpVerifier.Scope = strings.Join(mcpCfg.Scopes, " ")
	restVerifier := verifier.WithAudiences("rest", audienceMode, restAudiences...)
	slog.Info("token audience policy", "mode", audienceMode, "mcp", mcpAudience, "rest", restAudiences)

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
	restHandler.Register(mux)

	// Health check (unauthenticated, used by k8s probes)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"status":"ok"}`)
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

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = apiServer.Shutdown(shutdownCtx)
	if asynqWorker != nil {
		asynqWorker.Shutdown()
	}
	system.Shutdown()
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// authExceptHealth applies the auth verifier to every path except /health,
// which k8s liveness/readiness probes hit unauthenticated.
func authExceptHealth(v *auth.Verifier, next http.Handler) http.Handler {
	authed := v.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		authed.ServeHTTP(w, r)
	})
}
