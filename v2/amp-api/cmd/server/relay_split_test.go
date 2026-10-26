package main

// The relay is its own service, amp-relay (AMP #2387): amp-api serves no
// relay route and doesn't even link the relay, so with no amp-relay running
// nobody can remote into a harness.

import (
	"go/build"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/relayserver"
)

// relayRoutes is every route of both relay legs (protocol.md).
var relayRoutes = [][2]string{
	{"POST", "/relay/v1/sessions"},
	{"POST", "/relay/v1/sessions/rs_x/token"},
	{"GET", "/relay/v1/sessions/rs_x/commands"},
	{"POST", "/relay/v1/sessions/rs_x/events"},
	{"POST", "/relay/v1/sessions/rs_x/heartbeat"},
	{"POST", "/relay/v1/sessions/rs_x/pairings"},
	{"DELETE", "/relay/v1/sessions/rs_x"},
	{"GET", "/relay"},
	{"GET", "/api/sessions"},
	{"POST", "/api/sessions/revoke-all"},
	{"GET", "/api/sessions/rs_x"},
	{"GET", "/api/sessions/rs_x/events"},
	{"POST", "/api/sessions/rs_x/commands"},
	{"POST", "/api/sessions/rs_x/pair"},
	{"POST", "/api/sessions/rs_x/disconnect"},
}

// amp-api's routes, with remote control offered (RELAY_ENABLED) and a global
// admin in the web UI calling, answer 404 on every relay route.
func TestAPI_ServesNoRelayRoutes(t *testing.T) {
	w := newRBACWorld(t)
	enableOrg(t, w.s.repo, w.org.ID, true)
	h := api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb)
	h.SetRemoteControl(remoteOn)
	mux := apiMux(h)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c := *w.root
		c.Listener, c.Channel = "rest", auth.ChannelUI
		api.RejectCrossSiteWrites(mux).ServeHTTP(rw, r.WithContext(auth.WithIdentity(r.Context(), &c)))
	}))
	defer srv.Close()
	for _, rt := range relayRoutes {
		if !relayserver.IsRelayPath(rt[1]) {
			t.Fatalf("%s isn't a relay path to Envoy's split", rt[1])
		}
		req, _ := http.NewRequest(rt[0], srv.URL+rt[1], strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", relayOrigin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("amp-api %s %s: %d, want 404", rt[0], rt[1], resp.StatusCode)
		}
	}
	// What amp-api keeps: the org setting and the capability.
	req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/api/me: %d", resp.StatusCode)
	}
}

// The amp-api binary (cmd/server without its tests) doesn't import the relay
// or its server, directly or through any package of this module. It talks to
// amp-relay only through relayctl's control calls.
func TestAPI_BinaryDoesNotLinkTheRelay(t *testing.T) {
	const module = "github.com/simstech/amp-api/"
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{module + "internal/relay": true, module + "internal/relayserver": true}
	seen := map[string]bool{}
	var walk func(dir, path string, from []string)
	walk = func(dir, path string, from []string) {
		if seen[path] {
			return
		}
		seen[path] = true
		pkg, err := build.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, imp := range pkg.Imports { // non-test files only
			if !strings.HasPrefix(imp, module) {
				continue
			}
			chain := append(append([]string{}, from...), imp)
			if forbidden[imp] {
				t.Fatalf("amp-api links the relay: %s", strings.Join(chain, " -> "))
			}
			walk(filepath.Join(root, strings.TrimPrefix(imp, module)), imp, chain)
		}
	}
	walk(".", module+"cmd/server", []string{module + "cmd/server"})
	if !seen[module+"internal/relayctl"] {
		t.Fatal("expected amp-api to use relayctl (the walk found nothing?)")
	}
}
