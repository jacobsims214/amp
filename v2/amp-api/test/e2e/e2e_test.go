//go:build e2e

// Package e2e drives a deployed AMP stack end to end the way a real MCP
// client does: discovery → dynamic client registration → /authorize through
// the amp-mcpoauth broker → Dex login form → Dex /token → MCP calls through
// Envoy, in both protocol eras.
//
// It is phased so it can bracket an upgrade:
//
//	AMP_E2E_PHASE=pre      run against the OLD release: register a client,
//	                       log in, save its tokens to AMP_E2E_STATE
//	AMP_E2E_PHASE=warn     run against the NEW release with
//	                       auth.audienceMode=warn
//	AMP_E2E_PHASE=enforce  run against the NEW release with
//	                       auth.audienceMode=enforce
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
				form.Set("login", e.user)
				form.Set("password", e.password)
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
	cb := e.browse(t, e.browser(), as.authorize+"?"+q.Encode(), func(u *url.URL) bool {
		return strings.HasPrefix(u.String(), redirectURI)
	})
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

	case "warn":
		pre := e.load(t, "pre")

		t.Run("pre-upgrade token keeps working in both eras", func(t *testing.T) {
			for _, opts := range [][]mcpclient.ClientOption{nil, {mcpclient.WithProtocolVersion("2025-11-25")}} {
				ver, err := e.mcpCall(t, pre.AccessToken, opts...)
				if err != nil {
					t.Fatalf("pre-upgrade token rejected under warn: %v", err)
				}
				t.Logf("pre-upgrade token accepted, protocol %s ✓", ver)
			}
		})
		t.Run("pre-upgrade refresh token still refreshes", func(t *testing.T) {
			tok := e.refresh(t, as, pre.ClientID, pre.RefreshToken)
			c := decode(t, tok["access_token"].(string))
			t.Logf("refreshed pre-upgrade token aud=%v (original scopes, so no amp-mcp yet)", c.Aud)
			if _, err := e.mcpCall(t, tok["access_token"].(string)); err != nil {
				t.Fatalf("refreshed pre-upgrade token rejected under warn: %v", err)
			}
			pre.RefreshToken = str(tok["refresh_token"])
			e.save(t, "pre", pre)
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
			t.Log("refresh keeps the amp-mcp audience ✓")
			e.save(t, "post", state{ClientID: id, AccessToken: tok["access_token"].(string), RefreshToken: str(r["refresh_token"])})
		})
		t.Run("pre-upgrade client is healed on its next login", func(t *testing.T) {
			tok := e.login(t, as, pre.ClientID)
			c := decode(t, tok["access_token"].(string))
			if !slices.Contains(c.Aud, "amp-mcp") {
				t.Fatalf("re-login of pre-upgrade client %s still has aud=%v", pre.ClientID, c.Aud)
			}
			t.Logf("pre-upgrade client re-login: aud=%v ✓", c.Aud)
			e.save(t, "healed", state{ClientID: pre.ClientID, AccessToken: tok["access_token"].(string)})
		})
		t.Run("UI session (aud=amp-ui) still reaches the REST API", func(t *testing.T) {
			me := e.uiAPIMe(t)
			t.Logf("/api/me: %v ✓", me["email"])
		})

	case "enforce":
		pre, post, healed := e.load(t, "pre"), e.load(t, "post"), e.load(t, "healed")

		t.Run("pre-upgrade token is rejected with a re-auth challenge", func(t *testing.T) {
			code, h := e.rawModern(t, pre.AccessToken, "")
			ch := h.Get("WWW-Authenticate")
			if code != 401 || !strings.Contains(ch, `error="invalid_token"`) || !strings.Contains(ch, "resource_metadata=") {
				t.Fatalf("got %d %q, want 401 with invalid_token + resource_metadata", code, ch)
			}
			t.Logf("401 %s ✓", ch)
		})
		t.Run("audience-bound tokens work in both eras", func(t *testing.T) {
			for name, tok := range map[string]string{"post": post.AccessToken, "healed": healed.AccessToken} {
				for _, opts := range [][]mcpclient.ClientOption{nil, {mcpclient.WithProtocolVersion("2025-11-25")}} {
					ver, err := e.mcpCall(t, tok, opts...)
					if err != nil {
						t.Fatalf("%s token rejected under enforce: %v", name, err)
					}
					t.Logf("%s token, protocol %s ✓", name, ver)
				}
			}
		})
		t.Run("pre-upgrade client recovers by logging in again", func(t *testing.T) {
			tok := e.login(t, as, pre.ClientID)
			if _, err := e.mcpCall(t, tok["access_token"].(string)); err != nil {
				t.Fatalf("re-authorized pre-upgrade client still rejected: %v", err)
			}
			t.Log("re-login recovers under enforce ✓")
		})
		t.Run("UI session still reaches the REST API", func(t *testing.T) {
			me := e.uiAPIMe(t)
			t.Logf("/api/me: %v ✓", me["email"])
		})
		t.Run("foreign browser Origin is refused", func(t *testing.T) {
			if code, _ := e.rawModern(t, post.AccessToken, "https://evil.example"); code != http.StatusForbidden {
				t.Fatalf("foreign origin: %d, want 403", code)
			}
			if code, _ := e.rawModern(t, post.AccessToken, e.base); code != 200 {
				t.Fatalf("own origin: %d, want 200", code)
			}
			t.Log("Origin validation ✓")
		})

	default:
		t.Fatalf("unknown AMP_E2E_PHASE %q", e.phase)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
