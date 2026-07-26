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
	"strings"
	"syscall"
	"time"

	dexapi "github.com/dexidp/dex/api/v2"
	"github.com/google/uuid"
	"github.com/simstech/amp-api/internal/dexclient"
)

type server struct {
	dex dexapi.DexClient

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

	s := &server{
		dex:     dexClient,
		selfURL: strings.TrimSuffix(envOrDefault("MCP_OAUTH_PUBLIC_URL", "http://localhost:8091"), "/"),
		dexURL:  strings.TrimSuffix(envOrDefault("DEX_PUBLIC_URL", "http://localhost:5556/dex"), "/"),
		mcpURL:  strings.TrimSuffix(envOrDefault("MCP_RESOURCE_URL", "http://localhost:8000"), "/"),
	}

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

	addr := envOrDefault("ADDR", ":8091")
	httpSrv := &http.Server{Addr: addr, Handler: withCORS(mux)}

	go func() {
		slog.Info("amp-mcp-oauth listening", "addr", addr, "self", s.selfURL, "dex", s.dexURL, "mcp", s.mcpURL)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func (s *server) authServerMetadata(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]interface{}{
		"issuer":                                s.dexURL,
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
	})
}

// authorize is a pure redirect in front of Dex's real /auth endpoint. It
// exists for exactly one reason: Dex is an OIDC-only provider and rejects
// any authorization request that doesn't include the "openid" scope — but
// the MCP OAuth spec (RFC 8707 resource indicators, not OIDC) doesn't
// require clients to request "openid" at all, since they only need a
// resource-scoped access token, not an ID token. Standards-compliant MCP
// clients (opencode, Claude Code, etc.) therefore omit it, and Dex's /auth
// would 400 with "Missing required scope(s) [\"openid\"]" if we pointed
// authorization_endpoint straight at Dex. This handler force-injects
// "openid" into whatever scope the client requested and 302s to Dex's real
// endpoint — Dex still does all the actual PKCE/consent/token work, this
// never touches a code, token, or secret.
func (s *server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	scopes := strings.Fields(q.Get("scope"))
	hasOpenID := false
	for _, sc := range scopes {
		if sc == "openid" {
			hasOpenID = true
			break
		}
	}
	if !hasOpenID {
		scopes = append([]string{"openid"}, scopes...)
	}
	q.Set("scope", strings.Join(scopes, " "))

	target := s.dexURL + "/auth?" + q.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *server) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]interface{}{
		"resource":              s.mcpURL,
		"authorization_servers": []string{s.selfURL},
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

	clientID := "mcp-" + uuid.NewString()
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
		clientID = "mcp-" + uuid.NewString()
		resp, err = s.dex.CreateClient(r.Context(), &dexapi.CreateClientReq{
			Client: &dexapi.Client{Id: clientID, Public: true, RedirectUris: req.RedirectURIs, Name: clientName},
		})
		if err != nil {
			jsonErr(w, "server_error", err.Error(), 502)
			return
		}
	}

	slog.Info("registered MCP OAuth client", "client_id", clientID, "name", clientName, "redirect_uris", req.RedirectURIs)

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
