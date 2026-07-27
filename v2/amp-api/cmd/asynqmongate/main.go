// Command asynqmongate is a tiny admin-only auth proxy in front of
// asynqmon (the asynq queue monitoring UI), which has no auth of its own.
// It validates the caller's JWT and requires the "admin" role, then reverse
// proxies to asynqmon running as a sidecar in the same pod on localhost.
// Mirrors the same pattern as amp-authadmin/amp-mcpoauth: a small,
// single-purpose Go binary sharing internal/auth with amp-api.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dsn := envOrDefault("DATABASE_URL", "postgres://amp:amp@localhost:5432/amp?sslmode=disable")
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		slog.Error("connect to postgres", "err", err)
		os.Exit(1)
	}
	defer repo.Close()

	verifier, err := auth.NewVerifier(ctx, auth.Config{
		IssuerURL:      os.Getenv("OIDC_ISSUER_URL"),
		DiscoveryURL:   os.Getenv("OIDC_DISCOVERY_URL"),
		BootstrapAdmin: os.Getenv("BOOTSTRAP_ADMIN_EMAILS"),
		Repo:           repo,
	})
	if err != nil {
		slog.Error("init auth verifier", "err", err)
		os.Exit(1)
	}

	asynqmonAddr := envOrDefault("ASYNQMON_ADDR", "127.0.0.1:8080")
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: asynqmonAddr})

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/", auth.RequireRole(domain.RoleAdmin, proxy.ServeHTTP))

	var handler http.Handler = mux
	handler = requireAuthExceptHealth(verifier, handler)

	addr := envOrDefault("ADDR", ":8092")
	httpSrv := &http.Server{Addr: addr, Handler: handler}

	go func() {
		slog.Info("amp-asynqmon-gate listening", "addr", addr, "asynqmon", asynqmonAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func requireAuthExceptHealth(v *auth.Verifier, next http.Handler) http.Handler {
	authed := v.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		authed.ServeHTTP(w, r)
	})
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
