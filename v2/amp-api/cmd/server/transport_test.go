// Transport tests — the public MCP listener over real HTTP.
//
// Unlike mcp_test.go (which calls MCPServer.HandleMessage directly), these
// go through newMCPHTTPHandler, the exact handler main() serves on :8000:
// origin guard → CORS → bearer auth → Streamable HTTP transport → tools →
// actors → postgres. They pin the upgrade-path guarantee for the MCP
// 2026-07-28 migration: legacy (initialize/session) clients see exactly the
// old behaviour while modern (stateless, per-request _meta) clients work on
// the same endpoint.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	jose "github.com/go-jose/go-jose/v4"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/mcp"
	"github.com/simstech/amp-api/internal/repository"
)

const (
	legacyVersion = "2025-11-25"
	modernVersion = "2026-07-28"
	testIssuer    = "https://amp.example.test/dex"
)

type transportStack struct {
	url         string
	projectCode string
	idp         *testIdP // nil when auth is disabled
}

type transportOpts struct {
	auth           bool
	allowedOrigins []string
}

func setupTransport(t *testing.T, o transportOpts) *transportStack {
	t.Helper()
	ctx := context.Background()
	repo, err := repository.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)
	sseHub := hub.New()
	reg := actor.NewRegistry(system, repo, sseHub)

	code := fmt.Sprintf("tr-%d", time.Now().UnixNano())
	if _, err := repo.CreateProject(ctx, domain.CreateProjectRequest{Name: "transport " + t.Name(), Code: code}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	st := &transportStack{projectCode: code}
	cfg := auth.Config{}
	if o.auth {
		st.idp = newTestIdP(t)
		cfg = auth.Config{IssuerURL: testIssuer, DiscoveryURL: st.idp.server.URL}
	}
	base, err := auth.NewVerifier(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	st.url = srv.URL + "/mcp"

	mcpCfg := mcpHTTPConfig{
		PublicURL:      st.url,
		OAuthURL:       "https://amp.example.test/mcp-oauth",
		Scopes:         []string{"openid", "email", "profile"},
		AllowedOrigins: o.allowedOrigins,
	}
	v := base.WithAudiences("mcp", "amp-mcp")
	v.ResourceMetadataURL = st.url + "/.well-known/oauth-protected-resource"
	v.Scope = strings.Join(mcpCfg.Scopes, " ")
	// Mirror Envoy's routing: /mcp and /mcp/* are forwarded to amp-api's
	// MCP listener with the /mcp prefix rewritten to "/".
	h := newMCPHTTPHandler(newMCPServer(mcp.NewServer(reg, repo, sseHub, nil)), v, mcpCfg)
	envoyRewrite := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/mcp"), "/")
		h.ServeHTTP(w, r2)
	})
	mux.Handle("/mcp", envoyRewrite)
	mux.Handle("/mcp/", envoyRewrite)
	return st
}

// ---- raw wire helpers ----

type wireResp struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func post(t *testing.T, url string, headers map[string]string, body any) wireResp {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := wireResp{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	text := string(raw)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// take the last data: line (the final JSON-RPC response)
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "data: ") {
				text = strings.TrimPrefix(line, "data: ")
			}
		}
	}
	_ = json.Unmarshal([]byte(text), &out.body)
	return out
}

func modernMeta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    modernVersion,
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "transport-test", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
}

func modernHeaders(method, name string) map[string]string {
	h := map[string]string{"MCP-Protocol-Version": modernVersion, "Mcp-Method": method}
	if name != "" {
		h["Mcp-Name"] = name
	}
	return h
}

func modernToolCall(id int, tool string, args map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args, "_meta": modernMeta()}}
}

func errCode(r wireResp) float64 {
	if e, ok := r.body["error"].(map[string]any); ok {
		c, _ := e["code"].(float64)
		return c
	}
	return 0
}

func result(t *testing.T, r wireResp) map[string]any {
	t.Helper()
	res, ok := r.body["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result (status %d): %s", r.status, r.raw)
	}
	return res
}

func toolText(t *testing.T, r wireResp) string {
	t.Helper()
	content, _ := result(t, r)["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content: %s", r.raw)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

// ---- tests ----

// A legacy client must see byte-for-byte the pre-upgrade protocol: an
// initialize handshake, an Mcp-Session-Id, and results without any of the
// 2026-07-28 fields.
func TestTransport_LegacyClientUnchanged(t *testing.T) {
	st := setupTransport(t, transportOpts{})

	init := post(t, st.url, nil, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": legacyVersion, "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "legacy", "version": "1"}}})
	if init.status != 200 {
		t.Fatalf("initialize: %d %s", init.status, init.raw)
	}
	if got := result(t, init)["protocolVersion"]; got != legacyVersion {
		t.Fatalf("negotiated %v, want %s", got, legacyVersion)
	}
	sid := init.header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("legacy initialize did not mint a session")
	}
	h := map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": legacyVersion}
	if r := post(t, st.url, h, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); r.status != http.StatusAccepted {
		t.Fatalf("initialized notification: %d", r.status)
	}

	call := post(t, st.url, h, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "amp_list_projects", "arguments": map[string]any{}}})
	if !strings.Contains(toolText(t, call), st.projectCode) {
		t.Fatalf("legacy tools/call missing project: %s", call.raw)
	}
	for _, field := range []string{"resultType", "ttlMs", "cacheScope", "_meta"} {
		if _, ok := result(t, call)[field]; ok {
			t.Errorf("legacy response leaked 2026-07-28 field %q: %s", field, call.raw)
		}
	}

	// Unknown session is still the legacy "re-initialize" signal.
	bad := post(t, st.url, map[string]string{"Mcp-Session-Id": "nope", "MCP-Protocol-Version": legacyVersion},
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"})
	if bad.status != http.StatusNotFound {
		t.Fatalf("unknown session: %d, want 404", bad.status)
	}
}

// A modern client needs no handshake and no session: every request stands
// alone, which is what lets amp-api sit behind plain round-robin LB.
func TestTransport_ModernStateless(t *testing.T) {
	st := setupTransport(t, transportOpts{})

	disc := post(t, st.url, modernHeaders("server/discover", ""),
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover", "params": map[string]any{"_meta": modernMeta()}})
	versions, _ := result(t, disc)["supportedVersions"].([]any)
	joined := fmt.Sprint(versions)
	if !strings.Contains(joined, modernVersion) || !strings.Contains(joined, legacyVersion) {
		t.Fatalf("server/discover supportedVersions=%v, want both eras", versions)
	}
	if caps, _ := result(t, disc)["capabilities"].(map[string]any); caps["tools"] == nil {
		t.Fatalf("discover did not advertise tools: %s", disc.raw)
	}

	for i := 0; i < 3; i++ {
		call := post(t, st.url, modernHeaders("tools/call", "amp_list_projects"), modernToolCall(10+i, "amp_list_projects", map[string]any{}))
		if call.status != 200 {
			t.Fatalf("modern tools/call #%d: %d %s", i, call.status, call.raw)
		}
		if call.header.Get("Mcp-Session-Id") != "" {
			t.Fatal("modern request was given a session id")
		}
		if result(t, call)["resultType"] != "complete" {
			t.Fatalf("missing resultType: %s", call.raw)
		}
		if !strings.Contains(toolText(t, call), st.projectCode) {
			t.Fatalf("modern tools/call missing project: %s", call.raw)
		}
	}

	// A write followed by a read on a fresh, unrelated request.
	created := post(t, st.url, modernHeaders("tools/call", "amp_create_project"),
		modernToolCall(20, "amp_create_project", map[string]any{"name": "modern-created", "code": st.projectCode + "-m"}))
	if r := result(t, created); r["isError"] == true {
		t.Fatalf("create via modern request failed: %s", created.raw)
	}
	list := post(t, st.url, modernHeaders("tools/call", "amp_list_projects"), modernToolCall(21, "amp_list_projects", map[string]any{}))
	if !strings.Contains(toolText(t, list), st.projectCode+"-m") {
		t.Fatalf("stateless write not visible to next request: %s", list.raw)
	}
}

func TestTransport_ModernValidation(t *testing.T) {
	st := setupTransport(t, transportOpts{})

	t.Run("Mcp-Name must match body", func(t *testing.T) {
		r := post(t, st.url, modernHeaders("tools/call", "amp_delete_task"), modernToolCall(1, "amp_list_projects", map[string]any{}))
		if r.status != 400 || errCode(r) != -32020 {
			t.Fatalf("got %d code %v, want 400/-32020 HeaderMismatch: %s", r.status, errCode(r), r.raw)
		}
	})
	t.Run("Mcp-Method required", func(t *testing.T) {
		r := post(t, st.url, map[string]string{"MCP-Protocol-Version": modernVersion}, modernToolCall(2, "amp_list_projects", map[string]any{}))
		if r.status != 400 || errCode(r) != -32020 {
			t.Fatalf("got %d code %v, want 400/-32020: %s", r.status, errCode(r), r.raw)
		}
	})
	t.Run("unsupported version lists supported ones", func(t *testing.T) {
		body := modernToolCall(3, "amp_list_projects", map[string]any{})
		body["params"].(map[string]any)["_meta"].(map[string]any)["io.modelcontextprotocol/protocolVersion"] = "2099-01-01"
		h := modernHeaders("tools/call", "amp_list_projects")
		h["MCP-Protocol-Version"] = "2099-01-01"
		r := post(t, st.url, h, body)
		if r.status != 400 || errCode(r) != -32022 || !strings.Contains(r.raw, modernVersion) {
			t.Fatalf("got %d code %v, want 400/-32022 listing %s: %s", r.status, errCode(r), modernVersion, r.raw)
		}
	})
}

func TestTransport_ToolsListCacheHints(t *testing.T) {
	st := setupTransport(t, transportOpts{})
	r := post(t, st.url, modernHeaders("tools/list", ""),
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{"_meta": modernMeta()}})
	res := result(t, r)
	// Private: the list depends on the caller (admin and person-only tools).
	if res["cacheScope"] != "private" || res["ttlMs"] != float64(toolsListCacheTTLMs) {
		t.Fatalf("cache hints = %v/%v, want private/%d", res["cacheScope"], res["ttlMs"], toolsListCacheTTLMs)
	}
	tools, _ := res["tools"].([]any)
	if len(tools) < 40 {
		t.Fatalf("only %d tools listed", len(tools))
	}
}

// The real mcp-go client, both eras, end to end over HTTP.
func TestTransport_RealClients(t *testing.T) {
	st := setupTransport(t, transportOpts{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name    string
		opts    []mcpclient.ClientOption
		wantVer string
	}{
		{"dual-era client negotiates 2026-07-28", nil, modernVersion},
		{"legacy-pinned client keeps 2025-11-25", []mcpclient.ClientOption{mcpclient.WithProtocolVersion(legacyVersion)}, legacyVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := transport.NewStreamableHTTP(st.url)
			if err != nil {
				t.Fatal(err)
			}
			c := mcpclient.NewClient(tr, tc.opts...)
			defer c.Close()
			if err := c.Start(ctx); err != nil {
				t.Fatal(err)
			}
			init := mcpgo.InitializeRequest{}
			init.Params.ClientInfo = mcpgo.Implementation{Name: "amp-transport-test", Version: "1"}
			if _, err := c.Initialize(ctx, init); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			if got := c.ProtocolVersion(); got != tc.wantVer {
				t.Fatalf("protocol version %q, want %q", got, tc.wantVer)
			}
			req := mcpgo.CallToolRequest{}
			req.Params.Name = "amp_list_projects"
			res, err := c.CallTool(ctx, req)
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			text := res.Content[0].(mcpgo.TextContent).Text
			if !strings.Contains(text, st.projectCode) {
				t.Fatalf("tool result missing project: %s", text)
			}
		})
	}
}

func TestTransport_OriginValidation(t *testing.T) {
	call := func(st *transportStack, origin string) wireResp {
		h := modernHeaders("tools/call", "amp_list_projects")
		if origin != "" {
			h["Origin"] = origin
		}
		return post(t, st.url, h, modernToolCall(1, "amp_list_projects", map[string]any{}))
	}

	configured := setupTransport(t, transportOpts{allowedOrigins: []string{"https://amp.example.test"}})
	if r := call(configured, ""); r.status != 200 {
		t.Fatalf("no Origin (CLI/agent client) rejected: %d", r.status)
	}
	if r := call(configured, "https://evil.example"); r.status != http.StatusForbidden {
		t.Fatalf("foreign Origin: %d, want 403", r.status)
	}
	r := call(configured, "https://amp.example.test")
	if r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "https://amp.example.test" {
		t.Fatalf("allowed Origin: %d ACAO=%q", r.status, r.header.Get("Access-Control-Allow-Origin"))
	}
	if r := call(configured, "http://localhost:6274"); r.status != http.StatusForbidden {
		t.Fatalf("localhost allowed despite explicit origin list: %d", r.status)
	}

	dflt := setupTransport(t, transportOpts{})
	if r := call(dflt, "http://localhost:6274"); r.status != 200 {
		t.Fatalf("default config should allow local MCP Inspector origin: %d", r.status)
	}
	if r := call(dflt, "https://evil.example"); r.status != http.StatusForbidden {
		t.Fatalf("default config allowed foreign origin: %d", r.status)
	}

	// CORS preflight for the new 2026-07-28 headers.
	req, _ := http.NewRequest(http.MethodOptions, configured.url, nil)
	req.Header.Set("Origin", "https://amp.example.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	// Browsers send this lowercased and sorted, which rs/cors relies on.
	req.Header.Set("Access-Control-Request-Headers", "authorization,mcp-method,mcp-name,mcp-protocol-version")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	allowed := strings.ToLower(resp.Header.Get("Access-Control-Allow-Headers"))
	for _, h := range []string{"mcp-protocol-version", "mcp-method", "mcp-name"} {
		if !strings.Contains(allowed, h) {
			t.Fatalf("preflight Allow-Headers %q missing %s", allowed, h)
		}
	}
}

func TestTransport_Auth(t *testing.T) {
	st := setupTransport(t, transportOpts{auth: true})
	good := st.idp.token(t, map[string]any{"aud": []string{"amp-mcp", "mcp-abc"}, "azp": "mcp-abc"})
	preUpgrade := st.idp.token(t, map[string]any{"aud": "mcp-abc"})

	t.Run("protected resource metadata is public", func(t *testing.T) {
		resp, err := http.Get(st.url + "/.well-known/oauth-protected-resource")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var prm map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&prm)
		if resp.StatusCode != 200 || prm["resource"] != st.url || fmt.Sprint(prm["authorization_servers"]) != "[https://amp.example.test/mcp-oauth]" {
			t.Fatalf("PRM %d %v", resp.StatusCode, prm)
		}
		if fmt.Sprint(prm["scopes_supported"]) != "[openid email profile]" {
			t.Fatalf("scopes_supported %v", prm["scopes_supported"])
		}
	})
	t.Run("no token: 401 with discovery challenge", func(t *testing.T) {
		r := post(t, st.url, modernHeaders("tools/call", "amp_list_projects"), modernToolCall(1, "amp_list_projects", map[string]any{}))
		ch := r.header.Get("WWW-Authenticate")
		if r.status != 401 || !strings.Contains(ch, "resource_metadata=") || !strings.Contains(ch, `scope="openid email profile"`) {
			t.Fatalf("%d %q", r.status, ch)
		}
	})
	t.Run("audience-bound token works in both eras", func(t *testing.T) {
		h := modernHeaders("tools/call", "amp_list_projects")
		h["Authorization"] = "Bearer " + good
		if r := post(t, st.url, h, modernToolCall(1, "amp_list_projects", map[string]any{})); r.status != 200 {
			t.Fatalf("modern: %d %s", r.status, r.raw)
		}
		init := post(t, st.url, map[string]string{"Authorization": "Bearer " + good}, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{"protocolVersion": legacyVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "l", "version": "1"}}})
		if init.status != 200 {
			t.Fatalf("legacy initialize: %d %s", init.status, init.raw)
		}
	})
	t.Run("pre-upgrade token rejected with re-auth challenge", func(t *testing.T) {
		h := modernHeaders("tools/call", "amp_list_projects")
		h["Authorization"] = "Bearer " + preUpgrade
		r := post(t, st.url, h, modernToolCall(1, "amp_list_projects", map[string]any{}))
		if r.status != 401 || !strings.Contains(r.header.Get("WWW-Authenticate"), `error="invalid_token"`) {
			t.Fatalf("%d %q", r.status, r.header.Get("WWW-Authenticate"))
		}
	})
	t.Run("token for another client rejected", func(t *testing.T) {
		h := modernHeaders("tools/call", "amp_list_projects")
		h["Authorization"] = "Bearer " + st.idp.token(t, map[string]any{"aud": "amp-ui"})
		if r := post(t, st.url, h, modernToolCall(1, "amp_list_projects", map[string]any{})); r.status != 401 {
			t.Fatalf("%d %s", r.status, r.raw)
		}
	})
}

// ---- minimal Dex stand-in: RSA key + JWKS + token minter ----

type testIdP struct {
	key    *rsa.PrivateKey
	server *httptest.Server
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(s.Close)
	return &testIdP{key: key, server: s}
}

func (idp *testIdP) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: idp.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	c := map[string]any{"iss": testIssuer, "sub": "u1", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range claims {
		c[k] = v
	}
	payload, _ := json.Marshal(c)
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := jws.CompactSerialize()
	return out
}
