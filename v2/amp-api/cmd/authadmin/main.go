// Command authadmin is a tiny admin-only HTTP service for managing AMP user
// credentials in Dex (the "add and manage users" surface backing the UI's
// Users page). It wraps Dex's gRPC password API — credentials always live in
// Dex, never here. Envoy routes /authadmin here without any role check, so
// the admin gate lives in this process: requireAuthExceptHealth verifies the
// bearer token (every path but /health), and each /authadmin/users route is
// wrapped in auth.RequireRole(domain.RoleAdmin, ...), which returns 403 to
// anyone who isn't a global admin.
//
// Every successful password create, reset and delete writes one entry to
// AMP's audit log (user.password.create, user.password.reset,
// user.password.delete, so the UI's "Accounts" filter shows them) through
// the same writer amp-api uses (service.Service.Record). Entries name the
// actor and the target account; they never carry the password or its hash.
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
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/dexclient"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/reqctx"
	"github.com/simstech/amp-api/internal/service"
	"golang.org/x/crypto/bcrypt"
)

// userStore is the slice of *repository.Repo authadmin uses.
type userStore interface {
	GetUserByEmail(ctx context.Context, email string) (*domain.User, error)
	ListUsers(ctx context.Context) ([]domain.User, error)
	DeleteUser(ctx context.Context, userID int) error
}

// auditFunc records an audit entry made by p; in production it is
// (*service.Service).Record, the writer amp-api uses.
type auditFunc func(ctx context.Context, p authz.Principal, e domain.AuditEntry)

type server struct {
	dex   dexapi.DexClient
	repo  userStore
	audit auditFunc
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

	// Only Service.Repo is needed to write audit entries. No OnAudit sinks
	// (webhooks) are registered here: account-level entries have no org or
	// project, so amp-api would never deliver them to a webhook either.
	auditSvc := &service.Service{Repo: repo}
	s := &server{dex: dexClient, repo: repo, audit: auditSvc.Record}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/authadmin/users", auth.RequireRole(domain.RoleAdmin, s.listOrCreateUser))
	mux.HandleFunc("/authadmin/users/", auth.RequireRole(domain.RoleAdmin, s.userSub))

	var handler http.Handler = mux
	handler = requireAuthExceptHealth(verifier, handler)
	// The audit rows this writes carry the trusted client IP, user agent
	// and request id (AMP_TRUSTED_PROXIES; see internal/reqctx).
	handler = reqctx.Middleware(reqctx.ListenerAuthAdmin, reqctx.FromEnv())(handler)

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
		s.recordPassword(r, "user.password.create", body.Email, nil, "created a password login for ",
			map[string]any{"username": username})
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
		s.recordPassword(r, "user.password.reset", email, nil, "reset the password of ", nil)
		jsonOK(w, map[string]interface{}{"email": email, "password_reset": true})

	case http.MethodDelete:
		// Refuse before touching Dex if this is the only global admin:
		// deleting their login would lock everyone out of administration.
		if u, err := s.repo.GetUserByEmail(r.Context(), email); err == nil && u.HasRole(domain.RoleAdmin) {
			users, err := s.repo.ListUsers(r.Context())
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			admins := 0
			for _, other := range users {
				if other.HasRole(domain.RoleAdmin) {
					admins++
				}
			}
			if admins == 1 {
				jsonErr(w, fmt.Errorf("this is the only global admin; make someone else a global admin first"), 409)
				return
			}
		}
		resp, err := s.dex.DeletePassword(r.Context(), &dexapi.DeletePasswordReq{Email: email})
		if err != nil {
			jsonErr(w, err, 502)
			return
		}
		if resp.NotFound {
			jsonErr(w, fmt.Errorf("user not found"), 404)
			return
		}
		// Look the AMP account up before deleting it, so the audit entry
		// still names its id.
		var target *domain.User
		deleted := false
		if u, err := s.repo.GetUserByEmail(r.Context(), email); err == nil {
			target = u
			deleted = s.repo.DeleteUser(r.Context(), u.ID) == nil
		}
		s.recordPassword(r, "user.password.delete", email, target, "deleted the password login of ",
			map[string]any{"amp_user_deleted": deleted})
		jsonOK(w, map[string]interface{}{"email": email, "deleted": true})

	default:
		http.NotFound(w, r)
	}
}

// recordPassword writes one audit entry for a password change to email's
// login, made by the request's caller. target is the AMP account when the
// caller already has it; otherwise it is looked up by email, and an account
// that has never signed in (so has no AMP user yet) is recorded with an
// empty target_id. details must never hold the password or its hash.
func (s *server) recordPassword(r *http.Request, action, email string, target *domain.User, verb string, details map[string]any) {
	if s.audit == nil {
		return
	}
	ctx := r.Context()
	p, err := authz.FromContext(ctx)
	if err != nil {
		// RequireRole already admitted the caller, so this shouldn't happen;
		// still record the change rather than lose it.
		slog.Warn("audit: no principal for authadmin request", "action", action, "err", err)
	}
	if target == nil {
		if u, err := s.repo.GetUserByEmail(ctx, email); err == nil {
			target = u
		}
	}
	d := map[string]any{"email": email}
	for k, v := range details {
		d[k] = v
	}
	e := domain.AuditEntry{Action: action, TargetType: "user", Summary: verb + email, Details: d}
	if target != nil {
		e.TargetID = fmt.Sprint(target.ID)
	}
	s.audit(ctx, p, e)
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
