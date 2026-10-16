package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dexapi "github.com/dexidp/dex/api/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fakeDex models the parts of Dex's gRPC API the broker uses, with Dex's
// real semantics: GetClient on a missing client is storage.ErrNotFound
// passed straight through gRPC (codes.Unknown, "not found"),
// CreateClient reports AlreadyExists, and UpdateClient *replaces*
// TrustedPeers wholesale — the property that makes concurrent updates
// from several broker replicas lossy.
type fakeDex struct {
	dexapi.DexClient // unimplemented methods panic

	mu      sync.Mutex
	clients map[string]*dexapi.Client

	// updateDelay widens the read-modify-write window to provoke lost updates.
	updateDelay time.Duration
	updates     int
	// clobberNext simulates another replica's concurrent write landing
	// right after ours: the next N UpdateClient calls are acknowledged
	// but their TrustedPeers change is discarded.
	clobberNext int
}

func newFakeDex() *fakeDex { return &fakeDex{clients: map[string]*dexapi.Client{}} }

func (f *fakeDex) GetClient(_ context.Context, in *dexapi.GetClientReq, _ ...grpc.CallOption) (*dexapi.GetClientResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.clients[in.Id]
	if !ok {
		return nil, status.Error(codes.Unknown, "not found")
	}
	return &dexapi.GetClientResp{Client: proto.Clone(c).(*dexapi.Client)}, nil
}

func (f *fakeDex) CreateClient(_ context.Context, in *dexapi.CreateClientReq, _ ...grpc.CallOption) (*dexapi.CreateClientResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.clients[in.Client.Id]; ok {
		return &dexapi.CreateClientResp{AlreadyExists: true}, nil
	}
	c := proto.Clone(in.Client).(*dexapi.Client)
	f.clients[c.Id] = c
	return &dexapi.CreateClientResp{Client: c}, nil
}

func (f *fakeDex) UpdateClient(_ context.Context, in *dexapi.UpdateClientReq, _ ...grpc.CallOption) (*dexapi.UpdateClientResp, error) {
	time.Sleep(f.updateDelay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	c, ok := f.clients[in.Id]
	if !ok {
		return &dexapi.UpdateClientResp{NotFound: true}, nil
	}
	if f.clobberNext > 0 {
		f.clobberNext--
		return &dexapi.UpdateClientResp{}, nil
	}
	if in.TrustedPeers != nil {
		c.TrustedPeers = slices.Clone(in.TrustedPeers)
	}
	return &dexapi.UpdateClientResp{}, nil
}

func (f *fakeDex) peers(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.clients[id]; ok {
		return slices.Clone(c.TrustedPeers)
	}
	return nil
}

func testServer(dex *fakeDex) (*server, *httptest.Server) {
	tm := newTrustManager(dex, "amp-mcp")
	tm.recheckDelay = 0
	s := &server{
		dex:     dex,
		trust:   tm,
		selfURL: "https://amp.example.test/mcp-oauth",
		dexURL:  "https://amp.example.test/dex",
		mcpURL:  "https://amp.example.test/mcp",
	}
	return s, httptest.NewServer(s.handler())
}

func register(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(ts.URL+"/register", "application/json",
		strings.NewReader(`{"redirect_uris":["http://127.0.0.1:33418/callback"],"client_name":"test client"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("register: %d %v", resp.StatusCode, out)
	}
	return out["client_id"].(string)
}

// authorize follows nothing; it returns the scope the broker sent to Dex.
func authorize(t *testing.T, ts *httptest.Server, clientID, scope string) []string {
	t.Helper()
	q := url.Values{"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {"http://127.0.0.1:33418/callback"},
		"code_challenge": {"abc"}, "code_challenge_method": {"S256"}, "resource": {"https://amp.example.test/mcp"}}
	if scope != "" {
		q.Set("scope", scope)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(ts.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), "https://amp.example.test/dex/auth?") {
		t.Fatalf("redirected to %s, want dex /auth", loc)
	}
	// Everything else the client sent (PKCE, resource, redirect_uri) passes through untouched.
	for _, k := range []string{"client_id", "code_challenge", "code_challenge_method", "redirect_uri", "resource"} {
		if loc.Query().Get(k) != q.Get(k) {
			t.Fatalf("%s altered: %q -> %q", k, q.Get(k), loc.Query().Get(k))
		}
	}
	return strings.Fields(loc.Query().Get("scope"))
}

func TestEnsureAudienceClient(t *testing.T) {
	dex := newFakeDex()
	tm := newTrustManager(dex, "amp-mcp")
	for i := 0; i < 3; i++ { // idempotent
		if err := tm.ensureAudienceClient(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	c := dex.clients["amp-mcp"]
	if c == nil || c.Public || c.Secret == "" || len(c.RedirectUris) != 0 {
		t.Fatalf("audience client must be confidential with an unknown secret and no redirects (unusable for login): %+v", c)
	}
	secret := c.Secret
	_ = tm.ensureAudienceClient(context.Background())
	if dex.clients["amp-mcp"].Secret != secret {
		t.Fatal("re-running ensureAudienceClient replaced the existing client")
	}
}

func TestRegisterTrustsNewClient(t *testing.T) {
	dex := newFakeDex()
	_, ts := testServer(dex)
	defer ts.Close()

	// No audience client yet (fresh install): registration creates it.
	id := register(t, ts)
	if !strings.HasPrefix(id, dynamicClientPrefix) {
		t.Fatalf("client id %q", id)
	}
	if !slices.Contains(dex.peers("amp-mcp"), id) {
		t.Fatalf("new client %s not in amp-mcp trusted peers %v", id, dex.peers("amp-mcp"))
	}
	if c := dex.clients[id]; !c.Public || fmt.Sprint(c.RedirectUris) != "[http://127.0.0.1:33418/callback]" {
		t.Fatalf("registered client %+v", c)
	}
}

func TestAuthorizeScopes(t *testing.T) {
	dex := newFakeDex()
	_, ts := testServer(dex)
	defer ts.Close()
	id := register(t, ts)

	t.Run("adds openid, email, profile and the audience scope", func(t *testing.T) {
		got := authorize(t, ts, id, "")
		for _, want := range []string{"openid", "email", "profile", "audience:server:client_id:amp-mcp"} {
			if !slices.Contains(got, want) {
				t.Fatalf("scope %v missing %q", got, want)
			}
		}
	})
	t.Run("keeps client scopes and doesn't duplicate", func(t *testing.T) {
		got := authorize(t, ts, id, "offline_access openid email")
		if !slices.Contains(got, "offline_access") {
			t.Fatalf("dropped client scope: %v", got)
		}
		seen := map[string]bool{}
		for _, s := range got {
			if seen[s] {
				t.Fatalf("duplicate scope %q in %v", s, got)
			}
			seen[s] = true
		}
	})
	t.Run("confidential clients get no audience scope", func(t *testing.T) {
		dex.mu.Lock()
		dex.clients["amp-ui"] = &dexapi.Client{Id: "amp-ui", Secret: "s", Public: false}
		dex.mu.Unlock()
		got := authorize(t, ts, "amp-ui", "openid")
		if slices.Contains(got, "audience:server:client_id:amp-mcp") || slices.Contains(dex.peers("amp-mcp"), "amp-ui") {
			t.Fatalf("confidential client treated as MCP client: scope=%v peers=%v", got, dex.peers("amp-mcp"))
		}
	})
}

// A client registered by the pre-upgrade broker was never added to
// TrustedPeers. Its next login must repair that, otherwise Dex would reject
// the audience scope.
func TestAuthorizeHealsPreUpgradeClient(t *testing.T) {
	// Both ID shapes exist in real deployments: "mcp-<uuid>" from the
	// current /register, and Dex-style IDs from an earlier broker version.
	for _, id := range []string{"mcp-legacy", "fu4n4gs2tz276ak67hrpv3aqm"} {
		t.Run(id, func(t *testing.T) {
			dex := newFakeDex()
			dex.clients[id] = &dexapi.Client{Id: id, Public: true, Name: "OpenCode"}
			dex.clients["amp-mcp"] = &dexapi.Client{Id: "amp-mcp", Secret: "x", TrustedPeers: []string{"mcp-other"}}
			_, ts := testServer(dex)
			defer ts.Close()

			got := authorize(t, ts, id, "")
			if !slices.Contains(got, "audience:server:client_id:amp-mcp") {
				t.Fatalf("no audience scope for healed client: %v", got)
			}
			if p := dex.peers("amp-mcp"); !slices.Contains(p, id) || !slices.Contains(p, "mcp-other") {
				t.Fatalf("trusted peers after heal %v (must add, never drop)", p)
			}
		})
	}
}

// If an MCP client can't be added to TrustedPeers, /authorize must fail
// rather than send it to Dex without the audience scope: the resulting token
// would be rejected by amp-api and the client would loop through login.
func TestAuthorizeFailsWithoutTrust(t *testing.T) {
	dex := newFakeDex()
	dex.clients["mcp-1"] = &dexapi.Client{Id: "mcp-1", Public: true}
	dex.clients["amp-mcp"] = &dexapi.Client{Id: "amp-mcp", Secret: "x"}
	dex.clobberNext = 1000 // every TrustedPeers write is lost
	_, ts := testServer(dex)
	defer ts.Close()

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(ts.URL + "/authorize?" + url.Values{"client_id": {"mcp-1"}, "response_type": {"code"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Location") != "" {
		t.Fatalf("got %d Location=%q, want 503 with no redirect to Dex", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// /authorize is unauthenticated: a made-up client_id must not be added to
// TrustedPeers, and it gets no audience scope (Dex rejects the unknown
// client itself).
func TestAuthorizeUnknownClient(t *testing.T) {
	dex := newFakeDex()
	dex.clients["amp-mcp"] = &dexapi.Client{Id: "amp-mcp", Secret: "x"}
	_, ts := testServer(dex)
	defer ts.Close()

	got := authorize(t, ts, "mcp-not-registered", "")
	if slices.Contains(authorize(t, ts, "amp-mcp", ""), "audience:server:client_id:amp-mcp") {
		t.Fatal("audience client must never be made a peer of itself")
	}
	if slices.Contains(got, "audience:server:client_id:amp-mcp") {
		t.Fatalf("audience scope added for unknown client: %v", got)
	}
	if len(dex.peers("amp-mcp")) != 0 {
		t.Fatalf("unknown client added to trusted peers: %v", dex.peers("amp-mcp"))
	}
	if !slices.Contains(got, "openid") {
		t.Fatal("openid must still be injected")
	}
}

// Deterministic version of the race below: our write is acknowledged but
// overwritten. ensureTrusted must notice on re-read and write again.
func TestEnsureTrustedRetriesLostUpdate(t *testing.T) {
	dex := newFakeDex()
	dex.clients["amp-mcp"] = &dexapi.Client{Id: "amp-mcp", Secret: "x"}
	dex.clobberNext = 2
	tm := newTrustManager(dex, "amp-mcp")
	tm.recheckDelay = 0
	if err := tm.ensureTrusted(context.Background(), "mcp-a"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dex.peers("amp-mcp"), "mcp-a") || dex.updates != 3 {
		t.Fatalf("peers=%v updates=%d, want mcp-a present after 3 writes", dex.peers("amp-mcp"), dex.updates)
	}

	dex.clobberNext = 100
	tm.maxAttempts = 4
	if err := tm.ensureTrusted(context.Background(), "mcp-b"); err == nil {
		t.Fatal("expected an error once retries are exhausted, not silent success")
	}
}

// Two broker replicas (separate trustManagers, separate mutexes) register
// clients concurrently against one Dex whose UpdateClient replaces the
// list. The verify-and-retry loop must end with every client trusted.
func TestConcurrentRegistrationAcrossReplicas(t *testing.T) {
	dex := newFakeDex()
	dex.updateDelay = 2 * time.Millisecond
	dex.clients["amp-mcp"] = &dexapi.Client{Id: "amp-mcp", Secret: "x"}
	replicas := []*trustManager{newTrustManager(dex, "amp-mcp"), newTrustManager(dex, "amp-mcp")}
	for _, r := range replicas {
		r.recheckDelay = 0
		r.maxAttempts = 50
	}

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- replicas[i%2].ensureTrusted(context.Background(), fmt.Sprintf("mcp-%02d", i))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	missing := 0
	for i := 0; i < n; i++ {
		if !slices.Contains(dex.peers("amp-mcp"), fmt.Sprintf("mcp-%02d", i)) {
			missing++
		}
	}
	t.Logf("after concurrent pass: %d/%d missing (late overwrites)", missing, n)
	// A late overwrite can still drop an entry after its owner verified;
	// that's what the delayed re-check and /authorize healing cover. Run
	// the re-check pass and require convergence.
	for i := 0; i < n; i++ {
		if err := replicas[i%2].ensureTrustedOnce(context.Background(), fmt.Sprintf("mcp-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	peers := dex.peers("amp-mcp")
	for i := 0; i < n; i++ {
		if id := fmt.Sprintf("mcp-%02d", i); !slices.Contains(peers, id) {
			t.Fatalf("%s lost from trusted peers (%d/%d present)", id, len(peers), n)
		}
	}
	t.Logf("%d clients trusted after %d UpdateClient calls", n, dex.updates)
}

func TestMetadata(t *testing.T) {
	_, ts := testServer(newFakeDex())
	defer ts.Close()

	get := func(path string) map[string]any {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	as := get("/.well-known/oauth-authorization-server")
	if as["authorization_response_iss_parameter_supported"] != false {
		t.Fatal("must not advertise RFC 9207 iss: Dex doesn't emit it and 2026-07-28 clients would reject every login")
	}
	if as["authorization_endpoint"] != "https://amp.example.test/mcp-oauth/authorize" || as["registration_endpoint"] != "https://amp.example.test/mcp-oauth/register" {
		t.Fatalf("endpoints: %v", as)
	}
	prm := get("/.well-known/oauth-protected-resource")
	if prm["resource"] != "https://amp.example.test/mcp" || fmt.Sprint(prm["scopes_supported"]) != "[openid email profile]" {
		t.Fatalf("PRM: %v", prm)
	}
}
