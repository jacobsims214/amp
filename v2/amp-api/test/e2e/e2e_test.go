//go:build e2e

// Package e2e drives a deployed AMP stack end to end the way a real MCP
// client does: discovery → dynamic client registration → /authorize through
// the amp-mcpoauth broker → Dex login form → Dex /token → MCP calls through
// Envoy, in both protocol eras.
//
// It is phased so it can bracket an upgrade:
//
//	AMP_E2E_PHASE=pre   run against the OLD release: register a client,
//	                    log in, save its tokens to AMP_E2E_STATE
//	AMP_E2E_PHASE=post  run against the NEW release: old tokens are
//	                    rejected, re-login and new clients work
//	AMP_E2E_PHASE=rbac  multi-user: creates a second Dex user (via
//	                    authadmin), an org, a team, and checks access
//	                    and ticket ownership end to end. Needs no state.
//
// Required env: AMP_E2E_URL (e.g. https://amp.local), AMP_E2E_USER,
// AMP_E2E_PASSWORD (a Dex local user), AMP_E2E_STATE (a file path).
// Optional: AMP_E2E_DIAL=127.0.0.1:8443 to reach the URL's host through a
// port-forward without touching DNS, AMP_E2E_INSECURE=1 for a self-signed
// cert.
//
//	go test -tags e2e ./test/e2e -v -count=1
package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const redirectURI = "http://127.0.0.1:33418/callback"

type env struct {
	base     string
	user     string
	password string
	state    string
	phase    string
	http     *http.Client
}

func loadEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		base:     strings.TrimRight(os.Getenv("AMP_E2E_URL"), "/"),
		user:     os.Getenv("AMP_E2E_USER"),
		password: os.Getenv("AMP_E2E_PASSWORD"),
		state:    os.Getenv("AMP_E2E_STATE"),
		phase:    os.Getenv("AMP_E2E_PHASE"),
	}
	if e.base == "" || e.user == "" || e.password == "" || e.state == "" || e.phase == "" {
		t.Skip("AMP_E2E_URL, AMP_E2E_USER, AMP_E2E_PASSWORD, AMP_E2E_STATE and AMP_E2E_PHASE must be set")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: os.Getenv("AMP_E2E_INSECURE") == "1"}
	if dial := os.Getenv("AMP_E2E_DIAL"); dial != "" {
		d := &net.Dialer{Timeout: 10 * time.Second}
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, dial)
		}
	}
	e.http = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	return e
}

// ---- persisted state across phases ----

type state struct {
	ClientID     string `json:"client_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func (e *env) save(t *testing.T, name string, s state) {
	t.Helper()
	all := map[string]state{}
	if b, err := os.ReadFile(e.state); err == nil {
		_ = json.Unmarshal(b, &all)
	}
	all[name] = s
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.WriteFile(e.state, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *env) load(t *testing.T, name string) state {
	t.Helper()
	b, err := os.ReadFile(e.state)
	if err != nil {
		t.Fatalf("no state from an earlier phase: %v", err)
	}
	all := map[string]state{}
	_ = json.Unmarshal(b, &all)
	s, ok := all[name]
	if !ok {
		t.Fatalf("state %q missing (run the pre phase first)", name)
	}
	return s
}

// ---- OAuth ----

func (e *env) getJSON(t *testing.T, u string) map[string]any {
	t.Helper()
	resp, err := e.http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %v", u, resp.StatusCode, err)
	}
	return m
}

type authServer struct {
	authorize, token, register string
}

// discover follows the MCP authorization discovery chain from a 401.
func (e *env) discover(t *testing.T) authServer {
	t.Helper()
	resp, err := e.http.Post(e.base+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ch := resp.Header.Get("WWW-Authenticate")
	m := regexp.MustCompile(`resource_metadata="([^"]+)"`).FindStringSubmatch(ch)
	if resp.StatusCode != 401 || m == nil {
		t.Fatalf("unauthenticated /mcp: %d challenge=%q", resp.StatusCode, ch)
	}
	prm := e.getJSON(t, m[1])
	asURL := prm["authorization_servers"].([]any)[0].(string)
	u, _ := url.Parse(asURL)
	// RFC 8414 path-insertion form, as MCP clients compute it.
	meta := e.getJSON(t, fmt.Sprintf("%s://%s/.well-known/oauth-authorization-server%s", u.Scheme, u.Host, u.Path))
	return authServer{
		authorize: meta["authorization_endpoint"].(string),
		token:     meta["token_endpoint"].(string),
		register:  meta["registration_endpoint"].(string),
	}
}

func (e *env) register(t *testing.T, as authServer, name string) string {
	t.Helper()
	body := fmt.Sprintf(`{"redirect_uris":[%q],"client_name":%q,"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`, redirectURI, name)
	resp, err := e.http.Post(as.register, "application/json", strings.NewReader(body))
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

var (
	formRE     = regexp.MustCompile(`(?is)<form([^>]*)>(.*?)</form>`)
	actionRE   = regexp.MustCompile(`(?i)action="([^"]*)"`)
	hiddenRE   = regexp.MustCompile(`(?is)<input[^>]*type="hidden"[^>]*>`)
	attrRE     = regexp.MustCompile(`(?i)(name|value)="([^"]*)"`)
	passwordRE = regexp.MustCompile(`(?i)name="password"`)
	approvalRE = regexp.MustCompile(`(?i)name="approval"`)
)

// browse drives a cookie-carrying "browser" from start, following
// redirects and submitting Dex's login and approval forms the way a user
// would, until stop returns true for a redirect target (returned) or a
// non-form page is reached (nil returned).
func (e *env) browse(t *testing.T, browser *http.Client, start string, stop func(*url.URL) bool) *url.URL {
	t.Helper()
	return e.browseAs(t, browser, start, stop, e.user, e.password)
}

// browseAs is browse, signing in to Dex as user.
func (e *env) browseAs(t *testing.T, browser *http.Client, start string, stop func(*url.URL) bool, user, password string) *url.URL {
	t.Helper()
	next, method, form := start, http.MethodGet, url.Values(nil)
	for hop := 0; hop < 25; hop++ {
		var req *http.Request
		if method == http.MethodPost {
			req, _ = http.NewRequest(method, next, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req, _ = http.NewRequest(method, next, nil)
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatalf("hop %d %s: %v", hop, next, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && loc != "" {
			u, _ := resp.Request.URL.Parse(loc)
			if stop != nil && stop(u) {
				return u
			}
			next, method, form = u.String(), http.MethodGet, nil
			continue
		}
		if resp.StatusCode == 200 && (passwordRE.Match(body) || approvalRE.Match(body)) {
			m := formRE.FindSubmatch(body) // first form: login, or "approve" on the approval page
			if m == nil {
				t.Fatalf("no form at %s", resp.Request.URL)
			}
			action := resp.Request.URL // a form without action posts back to itself
			if a := actionRE.FindSubmatch(m[1]); a != nil && len(a[1]) > 0 {
				action, _ = resp.Request.URL.Parse(html.UnescapeString(string(a[1])))
			}
			form = url.Values{}
			for _, in := range hiddenRE.FindAll(m[2], -1) {
				var name, value string
				for _, a := range attrRE.FindAllSubmatch(in, -1) {
					if strings.EqualFold(string(a[1]), "name") {
						name = html.UnescapeString(string(a[2]))
					} else {
						value = html.UnescapeString(string(a[2]))
					}
				}
				if name != "" {
					form.Set(name, value)
				}
			}
			if passwordRE.Match(body) {
				form.Set("login", user)
				form.Set("password", password)
			} else {
				form.Set("approval", "approve")
			}
			next, method = action.String(), http.MethodPost
			continue
		}
		if stop != nil {
			t.Fatalf("flow stuck at %s: %d %s", resp.Request.URL, resp.StatusCode, truncate(string(body), 300))
		}
		return nil
	}
	t.Fatal("too many hops")
	return nil
}

func (e *env) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: e.http.Transport, Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// login runs authorization_code + PKCE through the broker and Dex's local
// login form, exactly as a browser would, and returns the token response.
// Like real MCP clients, it asks for offline_access to get a refresh token.
func (e *env) login(t *testing.T, as authServer, clientID string) map[string]any {
	t.Helper()
	return e.loginAs(t, as, clientID, e.user, e.password)
}

// loginAs is login, signing in as user.
func (e *env) loginAs(t *testing.T, as authServer, clientID, user, password string) map[string]any {
	t.Helper()
	verifier := randString(t)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {randString(t)},
		"resource":              {e.base + "/mcp"},
		"scope":                 {"offline_access"},
	}
	cb := e.browseAs(t, e.browser(), as.authorize+"?"+q.Encode(), func(u *url.URL) bool {
		return strings.HasPrefix(u.String(), redirectURI)
	}, user, password)
	if errMsg := cb.Query().Get("error"); errMsg != "" {
		t.Fatalf("authorization error: %s %s", errMsg, cb.Query().Get("error_description"))
	}
	if cb.Query().Get("state") != q.Get("state") {
		t.Fatal("state mismatch")
	}
	return e.token(t, as, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {cb.Query().Get("code")},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {e.base + "/mcp"},
	})
}

func (e *env) token(t *testing.T, as authServer, form url.Values) map[string]any {
	t.Helper()
	resp, err := e.http.PostForm(as.token, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("token (%s): %d %v", form.Get("grant_type"), resp.StatusCode, out)
	}
	return out
}

func (e *env) refresh(t *testing.T, as authServer, clientID, refreshToken string) map[string]any {
	return e.token(t, as, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID}})
}

type claims struct {
	Aud   audience `json:"aud"`
	Azp   string   `json:"azp"`
	Email string   `json:"email"`
	Sub   string   `json:"sub"`
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	err := json.Unmarshal(b, &many)
	*a = many
	return err
}

func decode(t *testing.T, jwt string) claims {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("access token is not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c claims
	_ = json.Unmarshal(b, &c)
	return c
}

// ---- MCP ----

// mcpCall connects with the real mcp-go client and calls amp_list_projects.
// It returns the negotiated protocol version, or the error.
func (e *env) mcpCall(t *testing.T, token string, opts ...mcpclient.ClientOption) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tr, err := transport.NewStreamableHTTP(e.base+"/mcp",
		transport.WithHTTPBasicClient(e.http),
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		return "", err
	}
	c := mcpclient.NewClient(tr, opts...)
	defer c.Close()
	if err := c.Start(ctx); err != nil {
		return "", err
	}
	init := mcpgo.InitializeRequest{}
	init.Params.ClientInfo = mcpgo.Implementation{Name: "amp-e2e", Version: "1"}
	if _, err := c.Initialize(ctx, init); err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "amp_list_projects"
	res, err := c.CallTool(ctx, req)
	if err != nil {
		return "", fmt.Errorf("tools/call: %w", err)
	}
	if res.IsError || !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "projects") {
		return "", fmt.Errorf("unexpected tool result: %+v", res.Content)
	}
	return c.ProtocolVersion(), nil
}

// rawModern sends one stateless 2026-07-28 tools/call and returns the status.
func (e *env) rawModern(t *testing.T, token, origin string) (int, http.Header) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"amp_list_projects","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"amp-e2e","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, _ := http.NewRequest(http.MethodPost, e.base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "amp_list_projects")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// uiAPIMe logs into the browser UI through oauth2-proxy and calls /api/me,
// proving the REST listener still accepts the UI's (aud=amp-ui) tokens.
func (e *env) uiAPIMe(t *testing.T) map[string]any {
	t.Helper()
	b := e.browser()
	e.browse(t, b, e.base+"/oauth2/start?rd=/", nil)
	resp, err := b.Get(e.base + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var me map[string]any
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || json.Unmarshal(body, &me) != nil {
		t.Fatalf("/api/me via UI session: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), truncate(string(body), 300))
	}
	if me["email"] != e.user {
		t.Fatalf("/api/me identity %v, want %s", me, e.user)
	}
	return me
}

// uiSession is a browser signed in to the UI through oauth2-proxy.
type uiSession struct {
	e *env
	b *http.Client
}

func (e *env) uiLogin(t *testing.T, user, password string) *uiSession {
	t.Helper()
	b := e.browser()
	e.browseAs(t, b, e.base+"/oauth2/start?rd=/", nil, user, password)
	return &uiSession{e: e, b: b}
}

// do calls the REST API (or authadmin) as the signed-in user.
func (u *uiSession) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, u.e.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"_raw": truncate(string(raw), 200)}
	}
	return resp.StatusCode, out
}

func (u *uiSession) must(t *testing.T, code int, method, path string, body any) map[string]any {
	t.Helper()
	got, out := u.do(t, method, path, body)
	if got != code {
		t.Fatalf("%s %s: %d %v, want %d", method, path, got, out, code)
	}
	return out
}

// tool calls one MCP tool with the real mcp-go client and decodes its JSON
// result. A tool-level error is returned as err.
func (e *env) tool(t *testing.T, token, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tr, err := transport.NewStreamableHTTP(e.base+"/mcp",
		transport.WithHTTPBasicClient(e.http),
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		t.Fatal(err)
	}
	c := mcpclient.NewClient(tr)
	defer c.Close()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	init := mcpgo.InitializeRequest{}
	init.Params.ClientInfo = mcpgo.Implementation{Name: "amp-e2e", Version: "1"}
	if _, err := c.Initialize(ctx, init); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(ctx, req)
	if err != nil {
		return nil, err
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	if res.IsError {
		return nil, fmt.Errorf("%s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s result is not JSON: %s", name, truncate(text, 200))
	}
	return out, nil
}

func (e *env) mustTool(t *testing.T, token, name string, args map[string]any) map[string]any {
	t.Helper()
	out, err := e.tool(t, token, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

func num(m map[string]any, key string) int {
	v, _ := m[key].(float64)
	return int(v)
}

// listed returns the IDs of every task in an amp_list_tasks result.
func listed(res map[string]any) []int {
	var ids []int
	for _, bucket := range []string{"ready_to_dispatch", "in_progress", "blocked", "scheduled", "completed"} {
		items, _ := res[bucket].([]any)
		for _, it := range items {
			ids = append(ids, num(it.(map[string]any), "id"))
		}
	}
	return ids
}

func randString(t *testing.T) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- phases ----

func TestE2E(t *testing.T) {
	e := loadEnv(t)
	as := e.discover(t)
	t.Logf("phase=%s authorize=%s token=%s register=%s", e.phase, as.authorize, as.token, as.register)

	switch e.phase {
	case "pre":
		// A client exactly as it exists in a deployment before this release.
		id := e.register(t, as, "e2e pre-upgrade client")
		tok := e.login(t, as, id)
		c := decode(t, tok["access_token"].(string))
		t.Logf("pre-upgrade token: aud=%v azp=%q email=%q", c.Aud, c.Azp, c.Email)
		if slices.Contains(c.Aud, "amp-mcp") {
			t.Fatal("old release unexpectedly issued an amp-mcp audience")
		}
		ver, err := e.mcpCall(t, tok["access_token"].(string), mcpclient.WithProtocolVersion("2025-11-25"))
		if err != nil {
			t.Fatalf("legacy MCP call on old release: %v", err)
		}
		t.Logf("old release serves legacy MCP (%s) ✓", ver)
		if code, _ := e.rawModern(t, tok["access_token"].(string), ""); code == 200 {
			t.Fatal("old release unexpectedly served a stateless 2026-07-28 request")
		} else {
			t.Logf("old release rejects stateless 2026-07-28 request (%d), as expected ✓", code)
		}
		if str(tok["refresh_token"]) == "" {
			t.Fatal("no refresh token issued for offline_access")
		}
		e.save(t, "pre", state{ClientID: id, AccessToken: tok["access_token"].(string), RefreshToken: str(tok["refresh_token"])})

	case "post":
		pre := e.load(t, "pre")
		var post state

		t.Run("pre-upgrade token is rejected with a re-auth challenge", func(t *testing.T) {
			code, h := e.rawModern(t, pre.AccessToken, "")
			ch := h.Get("WWW-Authenticate")
			if code != 401 || !strings.Contains(ch, `error="invalid_token"`) || !strings.Contains(ch, "resource_metadata=") {
				t.Fatalf("got %d %q, want 401 with invalid_token + resource_metadata", code, ch)
			}
			if _, err := e.mcpCall(t, pre.AccessToken, mcpclient.WithProtocolVersion("2025-11-25")); err == nil {
				t.Fatal("pre-upgrade token accepted by a legacy-era client")
			}
			t.Logf("401 %s ✓", ch)
		})
		t.Run("refreshing a pre-upgrade token doesn't get past the check", func(t *testing.T) {
			tok := e.refresh(t, as, pre.ClientID, pre.RefreshToken)
			if c := decode(t, tok["access_token"].(string)); slices.Contains(c.Aud, "amp-mcp") {
				t.Fatalf("refresh unexpectedly added amp-mcp: %v", c.Aud)
			}
			if _, err := e.mcpCall(t, tok["access_token"].(string)); err == nil {
				t.Fatal("refreshed pre-upgrade token accepted; clients would never be sent to re-login")
			}
			t.Log("refreshed pre-upgrade token rejected, client must log in ✓")
		})
		t.Run("new client gets an audience-bound, attributable token", func(t *testing.T) {
			id := e.register(t, as, "e2e post-upgrade client")
			tok := e.login(t, as, id)
			c := decode(t, tok["access_token"].(string))
			t.Logf("new token: aud=%v azp=%q email=%q", c.Aud, c.Azp, c.Email)
			if !slices.Contains(c.Aud, "amp-mcp") || c.Azp != id {
				t.Fatalf("aud=%v azp=%q, want amp-mcp audience authorized by %s", c.Aud, c.Azp, id)
			}
			if c.Email == "" {
				t.Fatal("no email claim — amp-api can't attribute MCP calls to a user")
			}
			ver, err := e.mcpCall(t, tok["access_token"].(string))
			if err != nil || ver != "2026-07-28" {
				t.Fatalf("dual-era client: version %q err %v, want 2026-07-28", ver, err)
			}
			ver, err = e.mcpCall(t, tok["access_token"].(string), mcpclient.WithProtocolVersion("2025-11-25"))
			if err != nil || ver != "2025-11-25" {
				t.Fatalf("legacy-pinned client: version %q err %v", ver, err)
			}
			t.Log("modern (2026-07-28) and legacy (2025-11-25) clients both served ✓")

			r := e.refresh(t, as, id, str(tok["refresh_token"]))
			if rc := decode(t, r["access_token"].(string)); !slices.Contains(rc.Aud, "amp-mcp") {
				t.Fatalf("refreshed token lost the amp-mcp audience: %v", rc.Aud)
			}
			if _, err := e.mcpCall(t, r["access_token"].(string)); err != nil {
				t.Fatalf("refreshed token rejected: %v", err)
			}
			t.Log("refresh keeps the amp-mcp audience ✓")
			post = state{ClientID: id, AccessToken: tok["access_token"].(string)}
		})
		t.Run("pre-upgrade client recovers by logging in again", func(t *testing.T) {
			tok := e.login(t, as, pre.ClientID)
			c := decode(t, tok["access_token"].(string))
			if !slices.Contains(c.Aud, "amp-mcp") {
				t.Fatalf("re-login of pre-upgrade client %s still has aud=%v", pre.ClientID, c.Aud)
			}
			for _, opts := range [][]mcpclient.ClientOption{nil, {mcpclient.WithProtocolVersion("2025-11-25")}} {
				if _, err := e.mcpCall(t, tok["access_token"].(string), opts...); err != nil {
					t.Fatalf("re-authorized pre-upgrade client still rejected: %v", err)
				}
			}
			t.Logf("pre-upgrade client re-login: aud=%v ✓", c.Aud)
		})
		t.Run("UI session (aud=amp-ui) still reaches the REST API", func(t *testing.T) {
			me := e.uiAPIMe(t)
			t.Logf("/api/me: %v ✓", me["email"])
		})
		t.Run("foreign browser Origin is refused", func(t *testing.T) {
			if post.AccessToken == "" {
				t.Skip("no post-upgrade token")
			}
			if code, _ := e.rawModern(t, post.AccessToken, "https://evil.example"); code != http.StatusForbidden {
				t.Fatalf("foreign origin: %d, want 403", code)
			}
			if code, _ := e.rawModern(t, post.AccessToken, e.base); code != 200 {
				t.Fatalf("own origin: %d, want 200", code)
			}
			t.Log("Origin validation ✓")
		})

	case "rbac":
		// Multi-user: two real Dex users, an org and team made through the
		// API, an invite applied on first sign-in, and ticket ownership
		// enforced across MCP and the UI's REST API.
		sfx := fmt.Sprint(time.Now().UnixNano())
		admin := e.uiLogin(t, e.user, e.password)
		email2, pw2 := "e2e-rbac-"+sfx+"@amp.local", "pw-"+randString(t)[:16]
		admin.must(t, 200, "POST", "/authadmin/users", map[string]any{"email": email2, "password": pw2, "display_name": "E2E Teammate"})
		defer admin.do(t, "DELETE", "/authadmin/users/"+url.PathEscape(email2), nil)

		org := admin.must(t, 201, "POST", "/api/orgs", map[string]any{"name": "E2E " + sfx, "slug": "e2e-" + sfx})["org"].(map[string]any)
		orgID := num(org, "id")
		team := admin.must(t, 201, "POST", fmt.Sprintf("/api/orgs/%d/teams", orgID), map[string]any{"name": "Builders"})["team"].(map[string]any)
		teamID := num(team, "id")
		// The teammate has never signed in: adding them creates an invite.
		inv := admin.must(t, 201, "POST", fmt.Sprintf("/api/teams/%d/members", teamID), map[string]any{"user": email2})
		if inv["invited"] != true {
			t.Fatalf("expected a pending invite, got %v", inv)
		}
		t.Log("org, team, and pending invite created via REST ✓")

		adminTok := e.login(t, as, e.register(t, as, "e2e rbac admin"))["access_token"].(string)
		mateTok := e.loginAs(t, as, e.register(t, as, "e2e rbac teammate"), email2, pw2)["access_token"].(string)

		who := e.mustTool(t, mateTok, "amp_whoami", nil)
		orgs, _ := who["orgs"].([]any)
		if len(orgs) != 1 || num(orgs[0].(map[string]any), "id") != orgID {
			t.Fatalf("teammate's orgs after first sign-in = %v, want the invited org", orgs)
		}
		t.Log("invite applied on the teammate's first sign-in ✓")

		// Org/team placement: the admin creates a project owned by the team.
		proj := e.mustTool(t, adminTok, "amp_create_project", map[string]any{"name": "E2E RBAC " + sfx, "code": "e2e-rbac-" + sfx, "org_id": orgID, "team_id": teamID})
		pid := num(proj, "id")
		epic := e.mustTool(t, adminTok, "amp_create_epic", map[string]any{"project_id": pid, "name": "shared epic"})
		story := e.mustTool(t, adminTok, "amp_create_story", map[string]any{"project_id": pid, "epic_id": num(epic, "id"), "name": "shared story"})
		mk := func(tok, name, owner string) int {
			args := map[string]any{"project_id": pid, "epic_id": num(epic, "id"), "story_id": num(story, "id"), "name": name, "description": name, "acceptance_criteria": "done", "assigned_to": "amp-worker"}
			if owner != "" {
				args["owner"] = owner
			}
			return num(e.mustTool(t, tok, "amp_create_task", args), "id")
		}
		pool := mk(adminTok, "pool ticket", "none")
		adminOwn := mk(adminTok, "admin's ticket", "")
		handoff := mk(adminTok, "to hand off", "")

		// The teammate only sees projects they're on, and only their own work by default.
		projects, _ := e.mustTool(t, mateTok, "amp_list_projects", nil)["projects"].([]any)
		if len(projects) != 1 || num(projects[0].(map[string]any), "id") != pid {
			t.Fatalf("teammate sees projects %v, want only %d", projects, pid)
		}
		if got := listed(e.mustTool(t, mateTok, "amp_list_tasks", map[string]any{"project_id": pid})); len(got) != 0 {
			t.Fatalf("teammate's default view %v, want empty", got)
		}
		if got := listed(e.mustTool(t, mateTok, "amp_list_tasks", map[string]any{"project_id": pid, "owner": "unassigned"})); !slices.Equal(got, []int{pool}) {
			t.Fatalf("unassigned pool %v, want [%d]", got, pool)
		}
		t.Log("project visibility and default 'mine' view ✓")

		// Claim from the pool; nobody can take it after that, not even the admin by claiming.
		e.mustTool(t, mateTok, "amp_claim_task", map[string]any{"task_id": pool})
		if _, err := e.tool(t, adminTok, "amp_claim_task", map[string]any{"task_id": pool}); err == nil || !strings.Contains(err.Error(), "owned by") {
			t.Fatalf("admin claim of a taken ticket: %v", err)
		}
		// The teammate can't work the admin's ticket...
		if _, err := e.tool(t, mateTok, "amp_dispatch_task", map[string]any{"task_id": adminOwn, "agent_id": "x"}); err == nil {
			t.Fatal("teammate dispatched a ticket owned by someone else")
		}
		// ...until it's assigned to them.
		e.mustTool(t, adminTok, "amp_assign_task", map[string]any{"task_id": handoff, "user": email2})
		mine := listed(e.mustTool(t, mateTok, "amp_list_tasks", map[string]any{"project_id": pid}))
		if !slices.Contains(mine, pool) || !slices.Contains(mine, handoff) || slices.Contains(mine, adminOwn) {
			t.Fatalf("teammate's view after claim + assignment = %v", mine)
		}
		e.mustTool(t, mateTok, "amp_dispatch_task", map[string]any{"task_id": handoff, "agent_id": "teammate-agent"})
		e.mustTool(t, mateTok, "amp_complete_task", map[string]any{"task_id": handoff})
		t.Log("claim, assign, and owner-only work enforced over MCP ✓")

		// The same rules through the UI's REST API, signed in as the teammate.
		mate := e.uiLogin(t, email2, pw2)
		tasks := mate.must(t, 200, "GET", fmt.Sprintf("/api/projects/%d/tasks", pid), nil)["tasks"].([]any)
		if len(tasks) != 2 {
			t.Fatalf("teammate's REST default view has %d tasks, want 2", len(tasks))
		}
		mate.must(t, 403, "POST", fmt.Sprintf("/api/tasks/%d/complete", adminOwn), nil)
		mate.must(t, 403, "POST", "/api/orgs", map[string]any{"name": "nope"})
		mate.must(t, 403, "GET", fmt.Sprintf("/api/kb/config?project_id=%d", pid), nil)
		t.Log("REST enforcement for the teammate's UI session ✓")

		// Platform admin over MCP: the admin's agent adds a second team and
		// puts the teammate on it; the teammate's agent can't do admin work.
		team2 := e.mustTool(t, adminTok, "amp_manage_teams", map[string]any{"action": "create", "org_id": orgID, "name": "Reviewers"})["team"].(map[string]any)
		e.mustTool(t, adminTok, "amp_manage_team_members", map[string]any{"action": "add", "team_id": num(team2, "id"), "user": email2, "role": "lead"})
		e.mustTool(t, adminTok, "amp_manage_project_teams", map[string]any{"action": "link", "project_id": pid, "team_id": num(team2, "id"), "role": "member"})
		if _, err := e.tool(t, mateTok, "amp_create_org", map[string]any{"name": "nope"}); err == nil || !strings.Contains(err.Error(), "only global admins") {
			t.Fatalf("teammate created an org over MCP: %v", err)
		}
		if _, err := e.tool(t, mateTok, "amp_manage_org_members", map[string]any{"action": "add", "org_id": orgID, "user": "x@amp.local"}); err == nil {
			t.Fatal("teammate managed org members over MCP")
		}
		members := e.mustTool(t, mateTok, "amp_manage_team_members", map[string]any{"action": "list", "team_id": num(team2, "id")})["members"].([]any)
		if len(members) != 1 {
			t.Fatalf("Reviewers members %v", members)
		}
		t.Log("platform administration over MCP ✓")

		// Promote the teammate to global admin from the Users page's API. Their
		// existing MCP token and UI session gain admin powers on the next
		// request (roles are read per request — no re-login), and lose them
		// again on demotion.
		var mateID int
		for _, u := range admin.must(t, 200, "GET", "/api/users", nil)["users"].([]any) {
			if um := u.(map[string]any); um["email"] == email2 {
				mateID = num(um, "id")
			}
		}
		if mateID == 0 {
			t.Fatal("teammate not in /api/users")
		}
		mate.must(t, 403, "GET", "/api/users", nil)
		admin.must(t, 200, "PATCH", fmt.Sprintf("/api/users/%d/role", mateID), map[string]any{"role": "admin", "grant": true})
		org2 := e.mustTool(t, mateTok, "amp_create_org", map[string]any{"name": "Made by promoted " + sfx, "slug": "promoted-" + sfx})
		if org2["org"] == nil {
			t.Fatalf("promoted teammate couldn't create an org: %v", org2)
		}
		allOrgs := e.mustTool(t, mateTok, "amp_list_orgs", nil)["orgs"].([]any)
		if len(allOrgs) < 3 { // Default, E2E, and the one just made — global admins see every org
			t.Fatalf("promoted teammate sees %d orgs, want all", len(allOrgs))
		}
		mate.must(t, 200, "GET", "/api/users", nil)
		mate.must(t, 200, "GET", "/authadmin/users", nil)
		t.Log("promotion to global admin takes effect immediately (MCP + UI) ✓")

		admin.must(t, 200, "PATCH", fmt.Sprintf("/api/users/%d/role", mateID), map[string]any{"role": "admin", "grant": false})
		if _, err := e.tool(t, mateTok, "amp_create_org", map[string]any{"name": "after demotion"}); err == nil || !strings.Contains(err.Error(), "only global admins") {
			t.Fatalf("demoted teammate still has admin tools: %v", err)
		}
		mate.must(t, 403, "GET", "/api/users", nil)
		t.Log("demotion takes effect immediately ✓")

		// The last global admin can't remove themselves.
		var adminID int
		for _, u := range admin.must(t, 200, "GET", "/api/users", nil)["users"].([]any) {
			if um := u.(map[string]any); um["email"] == e.user {
				adminID = num(um, "id")
			}
		}
		if code, out := admin.do(t, "PATCH", fmt.Sprintf("/api/users/%d/role", adminID), map[string]any{"role": "admin", "grant": false}); code == 200 {
			t.Fatalf("the only global admin revoked their own role: %v", out)
		} else {
			t.Logf("revoking the last global admin refused (%d: %v) ✓", code, out["error"])
		}

	default:
		t.Fatalf("unknown AMP_E2E_PHASE %q", e.phase)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
