// Command mcpoauth is a small OAuth "front door" that makes Dex speak the
// auth flavor MCP clients (opencode, Claude Code, etc.) expect:
//
//   - RFC 8414 authorization-server metadata discovery
//   - RFC 7591 Dynamic Client Registration
//   - PKCE (enforced by Dex itself — this service never sees or issues tokens)
//
// Dex already implements the real authorization_code+PKCE flow for public
// clients, so once a client is registered here (which just calls Dex's gRPC
// CreateClient), the client talks to Dex's /auth and /token endpoints
// directly. This service holds no secrets and mints no tokens — it is pure
// registration/discovery plumbing in front of Dex.
//
// Token audience (MCP 2026-07-28 requires MCP servers to accept only tokens
// issued for them): every registered client is added to the TrustedPeers of
// a dedicated "amp-mcp" Dex client, and /authorize adds Dex's cross-client
// scope for it, so tokens come back with aud=[amp-mcp, <client>] and
// amp-api can check for amp-mcp. See trust.go.
//
// See docs/deploy-architecture.md for the full flow.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	dexapi "github.com/dexidp/dex/api/v2"
	"github.com/google/uuid"
	"github.com/simstech/amp-api/internal/dexclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dynamicClientPrefix marks client IDs minted by /register.
const dynamicClientPrefix = "mcp-"

type server struct {
	dex   dexapi.DexClient
	trust *trustManager

	// Public URLs — all reachable through Envoy, used to build the
	// RFC 8414 metadata document.
	selfURL string // this service's own public base URL, e.g. https://amp.example.com/mcp-oauth
	dexURL  string // Dex's public base URL, e.g. https://amp.example.com/dex
	mcpURL  string // the MCP resource server's public URL, e.g. https://amp.example.com/mcp
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dexClient, conn, err := dexclient.New(dexclient.ConfigFromEnv())
	if err != nil {
		slog.Error("connect to dex grpc", "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := dexclient.Ping(ctx, dexClient); err != nil {
		slog.Warn("dex grpc ping failed at startup (will keep retrying per-request)", "err", err)
	}

	trust := newTrustManager(dexClient, envOrDefault("MCP_AUDIENCE_CLIENT_ID", "amp-mcp"))
	go trust.runEnsureAudienceClient(ctx)

	s := &server{
		dex:     dexClient,
		trust:   trust,
		selfURL: strings.TrimSuffix(envOrDefault("MCP_OAUTH_PUBLIC_URL", "http://localhost:8091"), "/"),
		dexURL:  strings.TrimSuffix(envOrDefault("DEX_PUBLIC_URL", "http://localhost:5556/dex"), "/"),
		mcpURL:  strings.TrimSuffix(envOrDefault("MCP_RESOURCE_URL", "http://localhost:8000"), "/"),
	}

	addr := envOrDefault("ADDR", ":8091")
	httpSrv := &http.Server{Addr: addr, Handler: s.handler()}

	go func() {
		slog.Info("amp-mcp-oauth listening", "addr", addr, "self", s.selfURL, "dex", s.dexURL, "mcp", s.mcpURL, "audience", trust.audienceID)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	// RFC 8414 — authorization server metadata.
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.authServerMetadata)
	// RFC 9728 — protected resource metadata (also mirrored here so clients
	// that probe the broker directly, not just the MCP resource, still
	// find their way; the canonical copy lives on amp-api's MCP listener).
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.protectedResourceMetadata)
	// RFC 7591 — dynamic client registration.
	mux.HandleFunc("/register", s.register)
	// Thin redirect in front of Dex's real /auth — see authorize() for why
	// this exists at all instead of pointing authorization_endpoint straight
	// at Dex.
	mux.HandleFunc("/authorize", s.authorize)
	return withCORS(mux)
}

func (s *server) authServerMetadata(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]interface{}{
		// RFC 8414 §3.3: the issuer must be the URL the metadata was
		// discovered from (this broker), or strict clients reject it
		// ("issuer mismatch"). Tokens are still Dex's, validated against
		// Dex's JWKS; MCP clients treat them as opaque.
		"issuer":                                s.selfURL,
		"authorization_endpoint":                s.selfURL + "/authorize",
		"token_endpoint":                        s.dexURL + "/token",
		"jwks_uri":                              s.dexURL + "/keys",
		"registration_endpoint":                 s.selfURL + "/register",
		"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
		"revocation_endpoint":                   s.dexURL + "/token/revoke",
		// Dex redirects straight back to the client and does not emit the
		// RFC 9207 "iss" parameter, so this must stay false: advertising
		// true would make 2026-07-28 clients reject every authorization
		// response that lacks it.
		"authorization_response_iss_parameter_supported": false,
	})
}

// authorize is a redirect in front of Dex's real /auth endpoint. It never
// touches a code, token, or secret; it only adjusts the request's scope:
//
//   - "openid": Dex is OIDC-only and rejects any authorization request
//     without it, but MCP clients (OAuth 2.1 + RFC 8707, not OIDC) don't
//     request it.
//   - "email" and "profile": without these Dex leaves email/name out of the
//     token, and amp-api can't attribute MCP calls to a user (JIT
//     provisioning is skipped for tokens with no email claim).
//   - the cross-client audience scope for amp-mcp: makes Dex put amp-mcp in
//     the token's aud, which amp-api checks. Before adding it, the client is
//     (re-)added to amp-mcp's TrustedPeers — this heals clients registered
//     before this release, and any TrustedPeers update lost to a concurrent
//     write on another replica. If trust can't be established the request
//     fails with 503: a token without the audience would only be rejected
//     by amp-api, sending the client round the login loop again.
func (s *server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	scopes := strings.Fields(q.Get("scope"))
	for _, want := range []string{"openid", "email", "profile"} {
		if !slices.Contains(scopes, want) {
			scopes = append(scopes, want)
		}
	}

	clientID := q.Get("client_id")
	audScope := s.trust.audienceScope()
	if clientID != "" && !slices.Contains(scopes, audScope) {
		isMCP, err := s.trustRegisteredClient(r.Context(), clientID)
		switch {
		case err != nil:
			slog.Error("could not establish MCP audience trust for client", "client_id", clientID, "err", err)
			http.Error(w, "AMP sign-in is temporarily unavailable, please retry", http.StatusServiceUnavailable)
			return
		case isMCP:
			scopes = append(scopes, audScope)
		}
	}
	q.Set("scope", strings.Join(scopes, " "))

	target := s.dexURL + "/auth?" + q.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}

// trustRegisteredClient adds clientID to the audience client's TrustedPeers,
// but only for a public client that actually exists in Dex: /authorize is
// unauthenticated, so an arbitrary client_id must not grow the list, and
// confidential clients (the UI's oauth2-proxy client, the audience client
// itself) are not MCP clients. Every client /register has ever created is
// public — including ones minted by earlier broker versions with Dex-style
// IDs instead of the "mcp-" prefix — so all of them get upgraded here.
//
// isMCP is false for clients that aren't MCP clients (unknown, confidential,
// or the audience client); those are passed to Dex unchanged, which rejects
// unknown ones itself. err is set only when an MCP client couldn't be trusted.
func (s *server) trustRegisteredClient(ctx context.Context, clientID string) (isMCP bool, err error) {
	got, err := s.dex.GetClient(ctx, &dexapi.GetClientReq{Id: clientID})
	if isDexNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up client %q in dex: %w", clientID, err)
	}
	if !got.GetClient().GetPublic() || clientID == s.trust.audienceID {
		return false, nil
	}
	return true, s.trust.ensureTrusted(ctx, clientID)
}

// isDexNotFound reports whether err is Dex's "no such client". Dex's gRPC
// GetClient returns storage.ErrNotFound unwrapped, which reaches us as
// codes.Unknown with message "not found"; codes.NotFound is accepted too.
func isDexNotFound(err error) bool {
	if err == nil {
		return false
	}
	st := status.Convert(err)
	return st.Code() == codes.NotFound || (st.Code() == codes.Unknown && st.Message() == "not found")
}

func (s *server) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]interface{}{
		"resource":                 s.mcpURL,
		"authorization_servers":    []string{s.selfURL},
		"scopes_supported":         []string{"openid", "email", "profile"},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "AMP",
	})
}

// register implements RFC 7591 Dynamic Client Registration by delegating to
// Dex's gRPC CreateClient. Every registered client is public (no secret) and
// PKCE-only, matching how installed/native MCP clients authenticate.
func (s *server) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid_client_metadata", err.Error(), 400)
		return
	}
	if len(req.RedirectURIs) == 0 {
		jsonErr(w, "invalid_redirect_uri", "redirect_uris is required", 400)
		return
	}
	for _, u := range req.RedirectURIs {
		if !isAllowedRedirect(u) {
			jsonErr(w, "invalid_redirect_uri", fmt.Sprintf("redirect_uri %q not permitted (loopback or custom-scheme only)", u), 400)
			return
		}
	}

	clientID := dynamicClientPrefix + uuid.NewString()
	clientName := req.ClientName
	if clientName == "" {
		clientName = "MCP client"
	}

	resp, err := s.dex.CreateClient(r.Context(), &dexapi.CreateClientReq{
		Client: &dexapi.Client{
			Id:           clientID,
			Public:       true,
			RedirectUris: req.RedirectURIs,
			Name:         clientName,
		},
	})
	if err != nil {
		jsonErr(w, "server_error", err.Error(), 502)
		return
	}
	if resp.AlreadyExists {
		// Extremely unlikely with a uuid, but retry once with a fresh id.
		clientID = dynamicClientPrefix + uuid.NewString()
		resp, err = s.dex.CreateClient(r.Context(), &dexapi.CreateClientReq{
			Client: &dexapi.Client{Id: clientID, Public: true, RedirectUris: req.RedirectURIs, Name: clientName},
		})
		if err != nil {
			jsonErr(w, "server_error", err.Error(), 502)
			return
		}
	}

	slog.Info("registered MCP OAuth client", "client_id", clientID, "name", clientName, "redirect_uris", req.RedirectURIs)

	// Trust the new client for the amp-mcp audience now so its first login
	// already gets an audience-bound token. Not fatal: /authorize retries.
	if err := s.trust.ensureTrusted(r.Context(), clientID); err != nil {
		slog.Error("could not add new MCP client to audience trusted peers (will retry at /authorize)",
			"client_id", clientID, "err", err)
	}

	jsonOK(w, map[string]interface{}{
		"client_id":                  clientID,
		"client_id_issued_at":        time.Now().Unix(),
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"client_name":                clientName,
	})
}

// isAllowedRedirect permits loopback HTTP redirects (RFC 8252 native apps,
// any port) and non-http custom URI schemes (e.g. "opencode://callback",
// "com.anthropic.claude://callback"). Anything else is rejected to prevent
// open-redirect abuse of the registration endpoint.
func isAllowedRedirect(raw string) bool {
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "http://127.0.0.1") || strings.HasPrefix(lower, "http://localhost") || strings.HasPrefix(lower, "http://[::1]") {
		return true
	}
	if strings.HasPrefix(lower, "https://") {
		return false // MCP clients doing loopback redirects use http, not https
	}
	if strings.HasPrefix(lower, "http://") {
		return false
	}
	// Custom scheme, e.g. myapp://callback — allowed.
	return strings.Contains(raw, "://")
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// jsonErr follows RFC 7591's error shape ({"error": "...", "error_description": "..."}).
func jsonErr(w http.ResponseWriter, code, description string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
