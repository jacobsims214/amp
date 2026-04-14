package main

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/cors"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/mcp"
	"github.com/simstech/amp-api/internal/repository"
)

//go:embed migrations/001_init.sql
var migration001 string

//go:embed migrations/002_task_start_at.sql
var migration002 string

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
	allMigrations := migration001 + "\n" + migration002
	if err := repo.Migrate(ctx, allMigrations); err != nil {
		slog.Error("migration failed", "err", err)
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
	mcpSrv := mcpserver.NewMCPServer("amp-api", "2.0.0", mcpserver.WithToolCapabilities(false))
	mcpHandler := mcp.NewServer(registry, repo, sseHub, kbSvc)
	mcpHandler.Register(mcpSrv)

	mcpHTTP := mcpserver.NewSSEServer(mcpSrv, fmt.Sprintf("http://localhost%s", mcpAddr))
	go func() {
		slog.Info("MCP server listening", "addr", mcpAddr)
		if err := mcpHTTP.Start(mcpAddr); err != nil && err != http.ErrServerClosed {
			slog.Error("MCP server error", "err", err)
		}
	}()

	// ---- 6. REST + SSE API (port 3001) ----
	mux := http.NewServeMux()
	restHandler := api.NewRestHandler(registry, repo, sseHub, kbSvc)
	restHandler.Register(mux)

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	corsMiddleware := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Content-Type", "Authorization"},
	})

	apiServer := &http.Server{
		Addr:    apiAddr,
		Handler: corsMiddleware.Handler(mux),
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
