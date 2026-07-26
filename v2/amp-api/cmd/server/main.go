package main

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/cors"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/mcp"
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

	// ---- 5. MCP server (port 8000) ----
	// The SSE server binds to an internal-only loopback address; the public
	// listener in front of it applies the auth middleware, then reverse
	// proxies through. This lets us add auth without forking mcp-go's
	// internal mux. baseURL is the externally reachable address the SSE
	// "endpoint" event advertises to clients — it must stay public-facing
	// even though the actual bind is internal.
	mcpSrv := mcpserver.NewMCPServer("amp-api", "2.0.0", mcpserver.WithToolCapabilities(false))
	mcpHandler := mcp.NewServer(registry, repo, sseHub, kbSvc)
	mcpHandler.Register(mcpSrv)

	mcpPublicBaseURL := envOrDefault("MCP_PUBLIC_URL", fmt.Sprintf("http://localhost%s", mcpAddr))
	mcpInternalAddr := "127.0.0.1:18789"
	mcpHTTP := mcpserver.NewSSEServer(mcpSrv, mcpPublicBaseURL)
	go func() {
		slog.Info("MCP internal server listening", "addr", mcpInternalAddr)
		if err := mcpHTTP.Start(mcpInternalAddr); err != nil && err != http.ErrServerClosed {
			slog.Error("MCP internal server error", "err", err)
		}
	}()

	mcpProxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: mcpInternalAddr})

	mcpPublicMux := http.NewServeMux()
	mcpPublicMux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		authServerURL := envOrDefault("MCP_OAUTH_PUBLIC_URL", "http://localhost:8091")
		fmt.Fprintf(w, `{"resource":%q,"authorization_servers":[%q]}`, mcpPublicBaseURL, authServerURL)
	})
	mcpPublicMux.Handle("/", verifier.Middleware(mcpProxy))
	verifier.ResourceMetadataURL = mcpPublicBaseURL + "/.well-known/oauth-protected-resource"

	mcpPublicServer := &http.Server{
		Addr:    mcpAddr,
		Handler: mcpPublicMux,
	}
	go func() {
		slog.Info("MCP public server listening (auth-enforced)", "addr", mcpAddr)
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
