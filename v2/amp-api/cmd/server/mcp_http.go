package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/cors"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/mcp"
)

// toolsListCacheTTLMs is the cache hint attached to tools/list results under
// MCP 2026-07-28 (SEP-2549). The tool set is fixed per build and identical
// for every caller, so it is safe to cache publicly; a deploy that changes
// tools is picked up within this window.
const toolsListCacheTTLMs = 5 * 60 * 1000

// mcpHTTPConfig is everything the public MCP listener needs beyond the
// MCP server itself.
type mcpHTTPConfig struct {
	// PublicURL is the canonical MCP resource URL (RFC 8707/9728), e.g.
	// https://amp.example.com/mcp.
	PublicURL string
	// OAuthURL is the authorization server advertised in protected
	// resource metadata (the amp-mcpoauth broker).
	OAuthURL string
	// Scopes advertised in protected resource metadata and in the
	// WWW-Authenticate challenge.
	Scopes []string
	// AllowedOrigins is the set of browser origins allowed to call the
	// endpoint. Requests without an Origin header (every non-browser MCP
	// client) are always allowed; requests with any other Origin get 403.
	// Empty means "localhost origins only".
	AllowedOrigins []string
}

// newMCPServer builds the MCP protocol server with every AMP tool
// registered. It speaks both the legacy initialize/session protocol
// (2025-11-25 and earlier) and the stateless 2026-07-28 protocol; mcp-go
// picks the era per request.
func newMCPServer(h *mcp.Server) *mcpserver.MCPServer {
	srv := mcpserver.NewMCPServer("amp-api", "2.0.0",
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithMethodCacheHints(mcpgo.MethodToolsList, toolsListCacheTTLMs, mcpgo.CacheScopePublic),
	)
	h.Register(srv)
	return srv
}

// newMCPHTTPHandler builds the full public MCP listener: protected resource
// metadata, origin validation, CORS, bearer auth, and the Streamable HTTP
// transport. main() and the transport tests share this so the tests
// exercise exactly what ships.
func newMCPHTTPHandler(srv *mcpserver.MCPServer, verifier *auth.Verifier, cfg mcpHTTPConfig) http.Handler {
	streamable := mcpserver.NewStreamableHTTPServer(srv)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":                 cfg.PublicURL,
			"authorization_servers":    []string{cfg.OAuthURL},
			"scopes_supported":         cfg.Scopes,
			"bearer_methods_supported": []string{"header"},
			"resource_name":            "AMP",
		})
	})
	mux.Handle("/", verifier.Middleware(streamable))

	c := cors.New(cors.Options{
		AllowOriginFunc: newOriginMatcher(cfg.AllowedOrigins),
		AllowedMethods:  []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodOptions},
		AllowedHeaders: []string{
			"Content-Type", "Accept", "Authorization", "Last-Event-ID",
			// legacy (2025-11-25 and earlier) session header
			"Mcp-Session-Id",
			// 2026-07-28 request metadata headers (SEP-2243)
			"MCP-Protocol-Version", "Mcp-Method", "Mcp-Name",
		},
		ExposedHeaders: []string{"Mcp-Session-Id", "WWW-Authenticate"},
	})
	return originGuard(newOriginMatcher(cfg.AllowedOrigins), c.Handler(mux))
}

// originGuard enforces the Streamable HTTP rule that servers MUST validate
// Origin and answer 403 when it is present and not allowed (DNS-rebinding
// protection). CORS alone only stops the browser from reading the
// response; it does not stop the request from executing.
func originGuard(allowed func(string) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !allowed(origin) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"origin not allowed"}}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newOriginMatcher returns a predicate for allowed browser origins. With no
// configured origins only loopback origins (local dev tools such as the MCP
// Inspector) are allowed.
func newOriginMatcher(allowed []string) func(string) bool {
	set := map[string]bool{}
	for _, o := range allowed {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			set[strings.ToLower(o)] = true
		}
	}
	return func(origin string) bool {
		if set[strings.ToLower(origin)] {
			return true
		}
		if len(set) > 0 {
			return false
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return true
		}
		return false
	}
}

// splitList splits a comma-separated env value, dropping empty entries.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
