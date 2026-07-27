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
	mcpserver "github.com/mark3labs/mcp-go/server"
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
	// Streamable HTTP transport (2025-03-26+ spec) — a single endpoint
	// handling POST (requests), GET (optional server push), and DELETE
	// (session teardown), correlated via the Mcp-Session-Id header. This
	// replaced the older HTTP+SSE transport (separate /sse + /message
	// endpoints) after real-world testing showed modern clients (opencode,
	// Claude Code, etc.) default to Streamable HTTP and don't reliably keep
	// a legacy SSE GET connection open long enough for it to work.
	mcpSrv := mcpserver.NewMCPServer("amp-api", "2.0.0", mcpserver.WithToolCapabilities(false))
	mcpHandler := mcp.NewServer(registry, repo, sseHub, kbSvc)
	mcpHandler.Register(mcpSrv)

	mcpPublicBaseURL := envOrDefault("MCP_PUBLIC_URL", fmt.Sprintf("http://localhost%s", mcpAddr))
	streamableSrv := mcpserver.NewStreamableHTTPServer(mcpSrv)

	mcpCORS := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Content-Type", "Authorization", "Mcp-Session-Id"},
		ExposedHeaders: []string{"Mcp-Session-Id"},
	})

	mcpPublicMux := http.NewServeMux()
	mcpPublicMux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		authServerURL := envOrDefault("MCP_OAUTH_PUBLIC_URL", "http://localhost:8091")
		fmt.Fprintf(w, `{"resource":%q,"authorization_servers":[%q]}`, mcpPublicBaseURL, authServerURL)
	})
	mcpPublicMux.Handle("/", verifier.Middleware(streamableSrv))
	verifier.ResourceMetadataURL = mcpPublicBaseURL + "/.well-known/oauth-protected-resource"

	mcpPublicServer := &http.Server{
		Addr:    mcpAddr,
		Handler: mcpCORS.Handler(mcpPublicMux),
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
	apiHandler = authExceptHealth(verifier, apiHandler)
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
