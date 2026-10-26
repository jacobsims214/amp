// Package relayserver is the amp-relay service's HTTP surface (epic 165,
// #2387): the router cmd/relay serves, built here so the relay's handler,
// cluster and security tests run against exactly what the binary serves.
//
// amp-relay serves ONLY the remote control relay:
//
//   - /relay/v1/...   the harness leg (its own bearer auth: the OAuth token
//     through the MCP-audience verifier for register and refresh, then the
//     session's rct_ connect token);
//   - /api/sessions   the browser leg, behind the REST verifier (a person in
//     the web UI) and the cross-site write check;
//   - /health, /ready  for the probes;
//
// and on its internal listener (CLUSTER_ADDR) /internal/relay/: requests
// forwarded between relay replicas and the control calls (ctl/end, which
// amp-api also calls when an org turns remote control off).
//
// Every other path is a 404. amp-api serves none of these routes, so with no
// amp-relay pod running nobody can reach a harness: Envoy has no upstream for
// /relay/ and /api/sessions.
package relayserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/relay"
	"github.com/simstech/amp-api/internal/service"
)

// Options configure the relay service.
type Options struct {
	// Relay tunes the relay (AppURL, Cluster, timings).
	Relay relay.Config
	// HarnessAuth verifies the harness's OAuth token (the MCP audience, as
	// /mcp does) on register and token refresh.
	HarnessAuth func(http.Handler) http.Handler
	// BrowserAuth verifies the browser leg's caller (the REST audiences: the
	// web UI's oauth2-proxy token).
	BrowserAuth func(http.Handler) http.Handler
	// Ready reports whether this replica can serve (nil: always). Called by
	// /ready; a non-nil error answers 503 with its text.
	Ready func() error
}

// Server is one amp-relay replica's HTTP surface.
type Server struct {
	// Relay is nil when remote control is off (svc.Remote): then every
	// relay path answers 404.
	Relay  *relay.Relay
	public http.Handler
}

// New builds the relay on svc. With the instance switch or auth off in
// svc.Remote, no relay route exists and every relay path is a plain 404
// (protocol 8.1).
func New(svc *service.Service, o Options) *Server {
	s := &Server{}
	harness := http.Handler(http.HandlerFunc(http.NotFound))
	browser := http.Handler(http.HandlerFunc(http.NotFound))
	if svc.Remote.InstanceEnabled && svc.Remote.AuthEnabled {
		if o.HarnessAuth == nil || o.BrowserAuth == nil {
			panic("relayserver: HarnessAuth and BrowserAuth are required")
		}
		s.Relay = relay.New(svc, o.HarnessAuth, o.Relay)
		harness = s.Relay.HarnessHandler()
		bm := http.NewServeMux()
		s.Relay.RegisterBrowser(bm)
		browser = o.BrowserAuth(api.RejectCrossSiteWrites(bm))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if o.Ready != nil {
			if err := o.Ready(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, `{"status":%q}`, err.Error())
				return
			}
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.Handle("/relay", harness)
	mux.Handle("/relay/", harness)
	mux.Handle("/api/sessions", browser)
	mux.Handle("/api/sessions/", browser)
	mux.HandleFunc("/", http.NotFound)
	s.public = mux
	return s
}

// Handler is the public listener's handler (Envoy routes /relay/ and
// /api/sessions here).
func (s *Server) Handler() http.Handler { return s.public }

// InternalHandler is the internal listener's handler: /internal/relay/ (with
// the cluster token), nothing else. With the relay off it answers 404 to
// everything (never nil: an http.Server with a nil Handler would serve
// http.DefaultServeMux).
func (s *Server) InternalHandler() http.Handler {
	if s.Relay == nil {
		return http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	mux.Handle("/internal/relay/", s.Relay.InternalHandler())
	mux.HandleFunc("/", http.NotFound)
	return mux
}

// Run runs the relay's sweeps until ctx ends (no-op when the relay is off).
func (s *Server) Run(ctx context.Context) {
	if s.Relay != nil {
		s.Relay.Run(ctx)
	}
}

// IsRelayPath reports whether path belongs to the relay service: the paths
// Envoy routes to amp-relay. amp-api serves none of them.
func IsRelayPath(path string) bool {
	return path == "/relay" || strings.HasPrefix(path, "/relay/") ||
		path == "/api/sessions" || strings.HasPrefix(path, "/api/sessions/")
}
