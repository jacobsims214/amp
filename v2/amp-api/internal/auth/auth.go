// Package auth validates OIDC access/ID tokens issued by Dex, for both the
// REST/SSE API and the MCP endpoint. Envoy also validates these tokens at
// the edge (jwt_authn filter) — this middleware is defense in depth per the
// deployment architecture (docs/deploy-architecture.md): amp-api never
// trusts the edge blindly.
//
// Auth is optional at the process level: if ISSUER_URL is unset, NewVerifier
// returns a verifier in "disabled" mode so that plain `make dev`/docker-compose
// workflows keep working without a Dex instance. Kubernetes deployments always
// set ISSUER_URL.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

type ctxKey string

const identityCtxKey ctxKey = "amp-identity"

// Identity is the authenticated principal extracted from a validated token.
type Identity struct {
	Subject string
	Email   string
	Name    string
	User    *domain.User // populated after JIT provisioning
	// LocalDev is set when auth is disabled (no ISSUER_URL): the request is
	// the anonymous local-dev user, who is treated as a global admin.
	LocalDev bool
}

// Verifier validates bearer tokens against Dex's JWKS. Safe for concurrent use.
type Verifier struct {
	inner            *oidc.IDTokenVerifier
	repo             *repository.Repo
	bootstrapAdmins  map[string]bool
	enabled          bool
	allowedAudiences []string
	label            string // which listener this verifier guards, for logs

	// ResourceMetadataURL, when set, is advertised in the WWW-Authenticate
	// header on 401s per RFC 9728 — lets MCP OAuth clients discover the
	// authorization server without out-of-band configuration.
	ResourceMetadataURL string
	// Scope, when set, is advertised as the scope= parameter of the
	// WWW-Authenticate challenge (MCP 2026-07-28 scope selection strategy).
	Scope string
}

// Config controls verifier construction.
type Config struct {
	IssuerURL string // e.g. https://amp.example.com/dex — empty disables auth
	// DiscoveryURL, when set, is used to actually fetch OIDC discovery/JWKS
	// (e.g. an in-cluster http://dex.amp.svc.cluster.local:5556/dex address),
	// while IssuerURL remains the externally-issued "iss" claim value tokens
	// carry. Avoids amp-api depending on its own public DNS name/TLS trust
	// just to talk to Dex over the loopback network. Defaults to IssuerURL.
	DiscoveryURL string
	// Audiences accepted on top of the issuer's own client IDs (e.g. the MCP
	// broker's client_id and oauth2-proxy's client_id both issue tokens users
	// will present to amp-api).
	Audiences      []string
	BootstrapAdmin string // comma-separated emails granted admin on first sight
	Repo           *repository.Repo
}

// NewVerifier builds a Verifier. When cfg.IssuerURL is empty, auth is disabled
// and every request is treated as an anonymous "local-dev" identity.
func NewVerifier(ctx context.Context, cfg Config) (*Verifier, error) {
	admins := map[string]bool{}
	for _, e := range strings.Split(cfg.BootstrapAdmin, ",") {
		e = strings.TrimSpace(strings.ToLower(e))
		if e != "" {
			admins[e] = true
		}
	}

	if cfg.IssuerURL == "" {
		slog.Warn("auth disabled: ISSUER_URL not set — all requests treated as anonymous local-dev user")
		return &Verifier{enabled: false, repo: cfg.Repo, bootstrapAdmins: admins}, nil
	}

	discoveryURL := cfg.DiscoveryURL
	if discoveryURL == "" {
		discoveryURL = cfg.IssuerURL
	}
	// Deliberately NOT using oidc.NewProvider/discovery here: Dex's discovery
	// document self-reports "jwks_uri" using its configured PUBLIC issuer
	// URL regardless of which URL you fetched discovery from, which would
	// send amp-api right back out through the public hostname just to fetch
	// signing keys — a hostname that doesn't resolve inside the cluster.
	// Instead we build the key set directly against the internal JWKS
	// endpoint and verify tokens against the public issuer string ourselves.
	keySet := oidc.NewRemoteKeySet(ctx, strings.TrimSuffix(discoveryURL, "/")+"/keys")
	verifier := oidc.NewVerifier(cfg.IssuerURL, keySet, &oidc.Config{SkipClientIDCheck: true})

	v := &Verifier{inner: verifier, enabled: true, repo: cfg.Repo, bootstrapAdmins: admins}
	if len(cfg.Audiences) > 0 {
		v.allowedAudiences = cfg.Audiences
	}
	return v, nil
}

// WithAudiences returns a copy of v that only accepts tokens whose aud
// claim names one of audiences. The copy shares the underlying key set, so
// one Verifier can back several listeners (e.g. MCP and REST) that each
// accept a different audience. Empty audiences are dropped; with none left
// any audience is accepted.
func (v *Verifier) WithAudiences(label string, audiences ...string) *Verifier {
	c := *v
	c.label = label
	c.allowedAudiences = nil
	for _, a := range audiences {
		if a = strings.TrimSpace(a); a != "" {
			c.allowedAudiences = append(c.allowedAudiences, a)
		}
	}
	return &c
}

// verifyAudience reports whether any of the token's audiences is accepted.
// An empty allow-list accepts everything.
func (v *Verifier) verifyAudience(audience []string) bool {
	if len(v.allowedAudiences) == 0 {
		return true
	}
	for _, a := range v.allowedAudiences {
		for _, aud := range audience {
			if a == aud {
				return true
			}
		}
	}
	return false
}

// Middleware validates the Authorization: Bearer <token> header (when auth is
// enabled), JIT-provisions the user, and stashes the Identity on the request
// context. Unauthenticated/invalid requests get 401 unless auth is disabled.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !v.enabled {
			ident := &Identity{Subject: "local-dev", Email: "local-dev@amp.local", Name: "Local Dev", LocalDev: true}
			r = r.WithContext(context.WithValue(r.Context(), identityCtxKey, ident))
			next.ServeHTTP(w, r)
			return
		}

		authz := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authz, prefix) {
			// RFC 6750 §3.1: no error code when the request carried no credentials.
			v.writeChallenge(w, "", "missing bearer token")
			return
		}
		rawToken := strings.TrimSpace(strings.TrimPrefix(authz, prefix))

		idToken, err := v.inner.Verify(r.Context(), rawToken)
		if err != nil {
			slog.Warn("token verification failed", "err", err)
			v.writeUnauthorized(w, "invalid token")
			return
		}

		var claims struct {
			Subject string `json:"sub"`
			Email   string `json:"email"`
			Name    string `json:"name"`
		}
		if err := idToken.Claims(&claims); err != nil {
			http.Error(w, `{"error":"malformed claims"}`, http.StatusUnauthorized)
			return
		}
		if !v.verifyAudience(idToken.Audience) {
			// A 401 with the challenge makes MCP clients re-run OAuth and
			// come back with a token issued for this resource.
			slog.Warn("token rejected: audience not accepted",
				"listener", v.label, "aud", idToken.Audience, "accepted", v.allowedAudiences, "subject", claims.Subject)
			v.writeUnauthorized(w, "token audience not valid for this resource")
			return
		}

		ident := &Identity{Subject: claims.Subject, Email: claims.Email, Name: claims.Name}
		if v.repo != nil {
			u, err := v.repo.UpsertUserFromClaims(r.Context(), claims.Subject, claims.Email, claims.Name, v.bootstrapAdmins)
			if err != nil {
				if errors.Is(err, repository.EmptyEmailClaimError) {
					// Expected for machine clients (MCP OAuth client-credentials/
					// device-code flows) that don't go through a Dex local login
					// and therefore have no email claim. Not an error — the
					// request is still valid, just no identity to provision.
					slog.Warn("JIT user provisioning skipped (machine client, no email claim)",
						"subject", claims.Subject,
						"remote_addr", r.RemoteAddr,
						"user_agent", r.UserAgent(),
						"request_uri", r.RequestURI,
					)
				} else {
					slog.Error("JIT user provisioning failed",
						"err", err,
						"subject", claims.Subject,
						"email", claims.Email,
						"remote_addr", r.RemoteAddr,
						"user_agent", r.UserAgent(),
						"request_uri", r.RequestURI,
					)
				}
			} else {
				ident.User = u
			}
		}

		r = r.WithContext(context.WithValue(r.Context(), identityCtxKey, ident))
		next.ServeHTTP(w, r)
	})
}

// WithIdentity returns ctx carrying ident, as the middleware would. For
// in-process callers (tests, background jobs) that bypass HTTP.
func WithIdentity(ctx context.Context, ident *Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey, ident)
}

// FromContext retrieves the authenticated Identity, if any.
func FromContext(ctx context.Context) (*Identity, bool) {
	ident, ok := ctx.Value(identityCtxKey).(*Identity)
	return ident, ok
}

// writeUnauthorized writes a 401 for a token that was presented but rejected.
func (v *Verifier) writeUnauthorized(w http.ResponseWriter, reason string) {
	v.writeChallenge(w, "invalid_token", reason)
}

// writeChallenge writes a 401 with an RFC 6750 WWW-Authenticate challenge.
// errCode is omitted when the request carried no credentials at all
// (RFC 6750 §3.1). resource_metadata (RFC 9728) and scope (MCP scope
// selection strategy) let MCP OAuth clients discover the authorization
// server and what to ask it for directly from the failed request.
func (v *Verifier) writeChallenge(w http.ResponseWriter, errCode, reason string) {
	var params []string
	if errCode != "" {
		params = append(params, fmt.Sprintf(`error="%s"`, errCode), fmt.Sprintf(`error_description=%q`, reason))
	}
	if v.ResourceMetadataURL != "" {
		params = append(params, fmt.Sprintf(`resource_metadata="%s"`, v.ResourceMetadataURL))
	}
	if v.Scope != "" {
		params = append(params, fmt.Sprintf(`scope="%s"`, v.Scope))
	}
	challenge := "Bearer"
	if len(params) > 0 {
		challenge += " " + strings.Join(params, ", ")
	}
	w.Header().Set("WWW-Authenticate", challenge)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `{"error":%q}`, reason)
}

// HasRole reports whether the identity's provisioned user carries the given role.
func (i *Identity) HasRole(role string) bool {
	if i == nil || i.User == nil {
		return false
	}
	for _, r := range i.User.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// RequireRole wraps a handler, returning 403 unless the caller has the given role.
// Must run after Middleware.
func RequireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := FromContext(r.Context())
		if !ok || !ident.HasRole(role) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
