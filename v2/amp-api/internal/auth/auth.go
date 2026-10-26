// Package auth validates OIDC access/ID tokens issued by Dex, for both the
// REST/SSE API and the MCP endpoint. Envoy does not validate JWTs (there is no
// jwt_authn filter): browser routes are gated by ext_authz/oauth2-proxy, and
// this middleware is what actually verifies bearer tokens — amp-api never
// trusts the edge blindly (see v2/docs/deploy-architecture.md).
//
// Auth is optional at the process level: if OIDC_ISSUER_URL is unset,
// NewVerifier returns a verifier in "disabled" mode so that plain `make
// dev`/docker-compose workflows keep working without a Dex instance.
// Kubernetes deployments always set OIDC_ISSUER_URL.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

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
	// LocalDev is set when auth is disabled (no OIDC_ISSUER_URL): the request is
	// the anonymous local-dev user, who is treated as a global admin.
	LocalDev bool
	// APIToken is set when the request authenticated with an AMP API token
	// rather than a Dex-issued token.
	APIToken bool
	// Listener is the label of the verifier that accepted the request
	// ("mcp" or "rest"); empty for in-process callers.
	Listener string
	// Audience and AZP are the verified token's aud and azp claims (empty
	// for API tokens and local dev).
	Audience []string
	AZP      string
	// Channel is how the request reached AMP (see the Channel constants):
	// only ChannelUI is a person in the web UI. Set by Middleware; empty
	// (treated as not the UI) for in-process callers.
	Channel string
	// AuthMethod is how the request authenticated, for the audit log (the
	// AuthMethod constants). Set by Middleware.
	AuthMethod string
	// TokenID is the api_tokens.id of the API token used (never the token).
	TokenID int64
	// SessionID identifies the Dex session a JWT belongs to: a truncated
	// SHA-256 of its sub and iat (Dex issues no sid). Never the token.
	SessionID string
}

// How a request authenticated, recorded as the audit log's auth_method.
const (
	// AuthDexSession: a Dex token from the web UI's oauth2-proxy session.
	AuthDexSession = "dex_session"
	// AuthOAuthMCP: a Dex token obtained by an MCP client's OAuth flow.
	AuthOAuthMCP = "oauth_mcp"
	// AuthAPIToken: an AMP API token (amp_...).
	AuthAPIToken = "api_token"
	// AuthLocalDev: auth is disabled (no OIDC_ISSUER_URL).
	AuthLocalDev = "local_dev"
)

// Channels a request can arrive on. Approvals (a plan, an artifact, a
// person's own ticket) need ChannelUI.
const (
	// ChannelUI: the web UI's session, i.e. a Dex token issued to the UI's
	// oauth2-proxy client (aud contains UIClientID, azp empty or the same)
	// on the REST listener. With auth disabled, anything on REST.
	ChannelUI = "ui"
	// ChannelMCP: anything on the MCP listener.
	ChannelMCP = "mcp"
	// ChannelToken: an AMP API token (amp_...), on either listener.
	ChannelToken = "token"
	// ChannelRESTOther: a Dex token on REST that wasn't issued to the UI
	// (e.g. an MCP-audience token).
	ChannelRESTOther = "rest-other"
)

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

	// uiClientID is the web UI's (oauth2-proxy's) Dex client ID: a REST
	// token whose audience names it is a person in the web UI.
	uiClientID string

	// lookupOnly: see Config.LookupOnly.
	lookupOnly bool
}

// Config controls verifier construction.
type Config struct {
	IssuerURL string // e.g. https://amp.example.com/dex — empty disables auth
	// DiscoveryURL, when set, is used to actually fetch OIDC discovery/JWKS
	// (e.g. an in-cluster http://dex.amp.svc.cluster.local:5556/dex address),
	// while IssuerURL remains the externally-issued "iss" claim value tokens
	// carry. Avoids amp-api depending on its own public DNS name/TLS trust
	// just to talk to Dex over the loopback network. Defaults to IssuerURL.
	DiscoveryURL   string
	BootstrapAdmin string // comma-separated emails granted admin on first sight
	Repo           *repository.Repo
	// UIClientID is the web UI's oauth2-proxy Dex client ID (UI_CLIENT_ID).
	// Only REST requests carrying a token issued to it count as the web UI
	// (ChannelUI). Empty: no Dex token counts as the UI.
	UIClientID string
	// LookupOnly makes the verifier read-only (amp-relay, #2392): a Dex
	// token's user is looked up by subject, never provisioned or updated
	// (amp-api does that when the person signs in to AMP), and AMP API
	// tokens are refused without being looked up, so the caller's database
	// role needs no write on users and no read of api_tokens.
	LookupOnly bool
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
		slog.Warn("auth disabled: OIDC_ISSUER_URL not set — all requests treated as anonymous local-dev user")
		return &Verifier{enabled: false, repo: cfg.Repo, bootstrapAdmins: admins, uiClientID: strings.TrimSpace(cfg.UIClientID)}, nil
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

	v := &Verifier{inner: verifier, enabled: true, repo: cfg.Repo, bootstrapAdmins: admins, uiClientID: strings.TrimSpace(cfg.UIClientID),
		lookupOnly: cfg.LookupOnly}
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
			ident := &Identity{Subject: "local-dev", Email: "local-dev@amp.local", Name: "Local Dev", LocalDev: true, Listener: v.label}
			ident.Channel = v.channelOf(ident)
			ident.AuthMethod = AuthLocalDev
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

		var ident *Identity
		if IsAPIToken(rawToken) {
			ident = v.identityFromAPIToken(w, r, rawToken)
		} else {
			ident = v.identityFromJWT(w, r, rawToken)
		}
		if ident == nil {
			return // the response has been written
		}
		ident.Listener = v.label
		ident.Channel = v.channelOf(ident)
		ident.AuthMethod = v.authMethodOf(ident)
		if ident.User != nil && ident.User.DisabledAt != nil {
			slog.Warn("request refused: account deactivated", "listener", v.label, "user_id", ident.User.ID, "email", ident.User.Email)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"this AMP account is deactivated; ask an AMP admin to reactivate it"}`)
			return
		}

		r = r.WithContext(context.WithValue(r.Context(), identityCtxKey, ident))
		next.ServeHTTP(w, r)
	})
}

// identityFromJWT verifies a Dex-issued token and JIT-provisions its user.
// On failure it writes the response and returns nil.
func (v *Verifier) identityFromJWT(w http.ResponseWriter, r *http.Request, rawToken string) *Identity {
	if v.inner == nil {
		v.writeUnauthorized(w, "this endpoint only accepts AMP API tokens")
		return nil
	}
	idToken, err := v.inner.Verify(r.Context(), rawToken)
	if err != nil {
		slog.Warn("token verification failed", "err", err)
		v.writeUnauthorized(w, "invalid token")
		return nil
	}

	var claims struct {
		AZP     string `json:"azp"`
		Subject string `json:"sub"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		// EmailVerified is a pointer so an absent claim (Dex's password
		// DB omits it on some versions) isn't read as false.
		EmailVerified *bool `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, `{"error":"malformed claims"}`, http.StatusUnauthorized)
		return nil
	}
	if !v.verifyAudience(idToken.Audience) {
		// A 401 with the challenge makes MCP clients re-run OAuth and
		// come back with a token issued for this resource.
		slog.Warn("token rejected: audience not accepted",
			"listener", v.label, "aud", idToken.Audience, "accepted", v.allowedAudiences, "subject", claims.Subject)
		v.writeUnauthorized(w, "token audience not valid for this resource")
		return nil
	}

	ident := &Identity{Subject: claims.Subject, Email: claims.Email, Name: claims.Name,
		Audience: append([]string(nil), idToken.Audience...), AZP: claims.AZP,
		SessionID: SessionID(claims.Subject, idToken.IssuedAt)}
	if v.repo == nil {
		return ident
	}
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		// An unverified email could be anyone's: never provision or link
		// an AMP user from it (an SSO connector can pass these through).
		slog.Warn("JIT user provisioning refused: email not verified", "subject", claims.Subject, "email", claims.Email)
		return ident
	}
	if v.lookupOnly {
		u, err := v.repo.GetUserBySubject(r.Context(), claims.Subject)
		if err != nil {
			// Not yet an AMP user here (or the lookup failed): no user on the
			// identity, which the handlers refuse (authz.FromContext).
			slog.Warn("token's user not found (lookup only: sign in to AMP first)", "listener", v.label, "subject", claims.Subject, "err", err)
			return ident
		}
		ident.User = u
		return ident
	}
	u, err := v.repo.UpsertUserFromClaims(r.Context(), claims.Subject, claims.Email, claims.Name, v.bootstrapAdmins)
	switch {
	case errors.Is(err, repository.EmptyEmailClaimError):
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
	case errors.Is(err, repository.ErrSignupNotAllowed):
		slog.Warn("JIT user provisioning refused: email domain not allowed", "subject", claims.Subject, "email", claims.Email)
	case err != nil:
		slog.Error("JIT user provisioning failed",
			"err", err,
			"subject", claims.Subject,
			"email", claims.Email,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
			"request_uri", r.RequestURI,
		)
	default:
		ident.User = u
	}
	return ident
}

// identityFromAPIToken looks up an AMP API token. On failure it writes the
// response and returns nil.
func (v *Verifier) identityFromAPIToken(w http.ResponseWriter, r *http.Request, rawToken string) *Identity {
	if v.repo == nil {
		v.writeUnauthorized(w, "API tokens are not available")
		return nil
	}
	if v.lookupOnly {
		// The relay refuses API tokens anyway (remote control is for a
		// person signed in with amp-harness login); refuse them here, with
		// the relay's code, without reading api_tokens.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"API tokens can't use remote control; sign in with `+"`amp-harness login`"+`","code":"token_not_allowed"}`)
		return nil
	}
	u, tokenID, err := v.repo.UserAndTokenForAPIToken(r.Context(), HashAPIToken(rawToken))
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			slog.Error("API token lookup failed", "err", err)
		}
		v.writeUnauthorized(w, "invalid, expired, or revoked API token")
		return nil
	}
	return &Identity{Subject: u.Subject, Email: u.Email, Name: u.DisplayName, User: u, APIToken: true, TokenID: tokenID}
}

// channelOf decides how a request reached AMP (see the Channel constants).
// Only the REST listener can be the web UI: with auth on, the token must
// have been issued to the UI's own client (its audience names UIClientID
// and azp, if any, is that client too), which only the UI's oauth2-proxy
// can obtain, with a secret no agent holds. With auth off (local dev and
// compose), REST counts as the UI and MCP never does.
func (v *Verifier) channelOf(ident *Identity) string {
	switch {
	case ident.APIToken:
		return ChannelToken
	case v.label != "rest":
		return ChannelMCP
	case ident.LocalDev:
		return ChannelUI
	case v.uiClientID != "" && slices.Contains(ident.Audience, v.uiClientID) && (ident.AZP == "" || ident.AZP == v.uiClientID):
		return ChannelUI
	}
	return ChannelRESTOther
}

// authMethodOf names how ident authenticated, for the audit log. A Dex
// token is the web UI's session when it was issued to the UI (ChannelUI),
// and on a verifier with no listener label (authadmin, whose only route
// sits behind oauth2-proxy); on the MCP listener, or an MCP-audience token
// on REST, it came from an MCP client's OAuth flow.
func (v *Verifier) authMethodOf(ident *Identity) string {
	switch {
	case ident.LocalDev:
		return AuthLocalDev
	case ident.APIToken:
		return AuthAPIToken
	case ident.Channel == ChannelUI, v.label == "":
		return AuthDexSession
	}
	return AuthOAuthMCP
}

// SessionID derives a stable, non-reversible id for the Dex session a JWT
// belongs to from its sub and iat: the first 16 bytes of SHA-256, in hex.
// Empty when either is missing.
func SessionID(subject string, issuedAt time.Time) string {
	if subject == "" || issuedAt.IsZero() {
		return ""
	}
	sum := sha256.Sum256([]byte(subject + "\x00" + strconv.FormatInt(issuedAt.Unix(), 10)))
	return hex.EncodeToString(sum[:16])
}

// IsUI reports whether the request came from a person in the web UI.
func (i *Identity) IsUI() bool { return i != nil && i.Channel == ChannelUI }

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
