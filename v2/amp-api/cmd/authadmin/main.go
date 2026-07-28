// Command authadmin is a tiny admin-only HTTP service for managing AMP user
// credentials in Dex (the "add and manage users" surface backing the UI's
// Users page). It wraps Dex's gRPC password API — credentials always live in
// Dex, never here. Only reachable via Envoy on a route gated on the `admin`
// JWT role claim; it also re-validates the token itself for defense in
// depth, matching amp-api's posture.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	dexapi "github.com/dexidp/dex/api/v2"
	"github.com/google/uuid"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/dexclient"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

type server struct {
	dex  dexapi.DexClient
	repo *repository.Repo
}

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

	dexClient, conn, err := dexclient.New(dexclient.ConfigFromEnv())
	if err != nil {
		slog.Error("connect to dex grpc", "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := dexclient.Ping(ctx, dexClient); err != nil {
		slog.Warn("dex grpc ping failed at startup (will keep retrying per-request)", "err", err)
	}

	// One-shot bootstrap mode: the Helm post-install Job runs this same image
	// with RUN_MODE=seed-admin to create the very first login (otherwise
	// there's a chicken-and-egg problem — you need to already be an admin to
	// use the Users page that creates admins). Runs on every install/upgrade,
	// but is intentionally a NO-OP once the admin already exists — see below.
	//
	// Real incident + correction: the amp-bootstrap-admin Secret's password
	// used to be generated with Helm's lookup-based "reuse existing value"
	// trick, which is unreliable through ArgoCD's rendering and was silently
	// regenerating the Secret's value on every sync (see docs/deploy-architecture.md).
	// That's now fixed by pinning it via global.secretOverrides.bootstrapAdminPassword
	// in values-production.yaml, so the Secret itself no longer drifts.
	//
	// A first attempt at fixing the resulting lockout made this job force-sync
	// Dex's password hash to the Secret's value on every run, even when the
	// admin already existed. That was wrong: it meant any password reset done
	// later via the Users admin page would get silently overwritten back to
	// the bootstrap value on the next sync, defeating the entire point of a
	// password-reset feature. Bootstrapping must stay a genuine one-time
	// operation — once the admin exists in Dex, this job does nothing further
	// and whatever password the admin currently has (bootstrap value or a
	// later reset) is left alone. AlreadyExists really does mean "nothing to
	// do" now that the Secret itself is stable.
	if os.Getenv("RUN_MODE") == "seed-admin" {
		email := strings.ToLower(strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL")))
		password := os.Getenv("SEED_ADMIN_PASSWORD")
		if email == "" || password == "" {
			slog.Error("seed-admin mode requires SEED_ADMIN_EMAIL and SEED_ADMIN_PASSWORD")
			os.Exit(1)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			slog.Error("hash seed admin password", "err", err)
			os.Exit(1)
		}
		resp, err := dexClient.CreatePassword(ctx, &dexapi.CreatePasswordReq{
			Password: &dexapi.Password{Email: email, Hash: hash, Username: email, UserId: uuid.NewString()},
		})
		if err != nil {
			slog.Error("seed admin CreatePassword failed", "err", err)
			os.Exit(1)
		}
		if resp.AlreadyExists {
			slog.Info("seed admin already exists, nothing to do (bootstrapping is one-time only — a later password reset via the Users page is intentionally preserved)", "email", email)
		} else {
			slog.Info("seed admin created", "email", email)
		}
		return
	}

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

	s := &server{dex: dexClient, repo: repo}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/authadmin/users", auth.RequireRole(domain.RoleAdmin, s.listOrCreateUser))
	mux.HandleFunc("/authadmin/users/", auth.RequireRole(domain.RoleAdmin, s.userSub))

	var handler http.Handler = mux
	handler = requireAuthExceptHealth(verifier, handler)

	addr := envOrDefault("ADDR", ":8090")
	httpSrv := &http.Server{Addr: addr, Handler: handler}

	go func() {
		slog.Info("amp-authadmin listening", "addr", addr)
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

// GET /authadmin/users — list all Dex local-connector credentials.
// POST /authadmin/users {email, password, username, display_name} — create.
func (s *server) listOrCreateUser(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, err := s.dex.ListPasswords(r.Context(), &dexapi.ListPasswordReq{})
		if err != nil {
			jsonErr(w, err, 502)
			return
		}
		out := make([]map[string]string, 0, len(resp.Passwords))
		for _, p := range resp.Passwords {
			out = append(out, map[string]string{"email": p.Email, "username": p.Username})
		}
		jsonOK(w, map[string]interface{}{"users": out})

	case http.MethodPost:
		var body struct {
			Email       string `json:"email"`
			Password    string `json:"password"`
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		body.Email = strings.ToLower(strings.TrimSpace(body.Email))
		if body.Email == "" || body.Password == "" {
			jsonErr(w, fmt.Errorf("email and password are required"), 400)
			return
		}
		if len(body.Password) < 8 {
			jsonErr(w, fmt.Errorf("password must be at least 8 characters"), 400)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		username := body.Username
		if username == "" {
			username = body.Email
		}
		resp, err := s.dex.CreatePassword(r.Context(), &dexapi.CreatePasswordReq{
			Password: &dexapi.Password{
				Email:    body.Email,
				Hash:     hash,
				Username: username,
				UserId:   uuid.NewString(),
			},
		})
		if err != nil {
			jsonErr(w, err, 502)
			return
		}
		if resp.AlreadyExists {
			jsonErr(w, fmt.Errorf("user already exists"), 409)
			return
		}
		jsonOK(w, map[string]interface{}{"email": body.Email, "username": username})

	default:
		http.NotFound(w, r)
	}
}

// PATCH /authadmin/users/:email {password} — reset password.
// DELETE /authadmin/users/:email — remove Dex credential.
func (s *server) userSub(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimPrefix(r.URL.Path, "/authadmin/users/")
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		if len(body.Password) < 8 {
			jsonErr(w, fmt.Errorf("password must be at least 8 characters"), 400)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		resp, err := s.dex.UpdatePassword(r.Context(), &dexapi.UpdatePasswordReq{
			Email:   email,
			NewHash: hash,
		})
		if err != nil {
			jsonErr(w, err, 502)
			return
		}
		if resp.NotFound {
			jsonErr(w, fmt.Errorf("user not found"), 404)
			return
		}
		jsonOK(w, map[string]interface{}{"email": email, "password_reset": true})

	case http.MethodDelete:
		resp, err := s.dex.DeletePassword(r.Context(), &dexapi.DeletePasswordReq{Email: email})
		if err != nil {
			jsonErr(w, err, 502)
			return
		}
		if resp.NotFound {
			jsonErr(w, fmt.Errorf("user not found"), 404)
			return
		}
		if u, err := s.repo.GetUserByEmail(r.Context(), email); err == nil {
			_ = s.repo.DeleteUser(r.Context(), u.ID)
		}
		jsonOK(w, map[string]interface{}{"email": email, "deleted": true})

	default:
		http.NotFound(w, r)
	}
}

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
