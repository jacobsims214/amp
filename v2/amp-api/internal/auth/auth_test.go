package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/simstech/amp-api/internal/domain"
)

const testIssuer = "https://amp.example.test/dex"

// testIdP is a minimal stand-in for Dex: an RSA key, a JWKS endpoint, and a
// token minter. Tokens are real RS256 JWTs verified by the production code
// path (go-oidc remote key set → IDTokenVerifier).
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
	idp := &testIdP{key: key}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}
	idp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *testIdP) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: idp.key},
		(&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{
		"iss":   testIssuer,
		"sub":   "user-1",
		"email": "dev@example.test",
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range claims {
		base[k] = v
	}
	payload, _ := json.Marshal(base)
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (idp *testIdP) verifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := NewVerifier(context.Background(), Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// do runs one request through v.Middleware and reports what the protected
// handler saw.
func do(t *testing.T, v *Verifier, authz string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if _, ok := FromContext(r.Context()); !ok {
			t.Error("identity missing from context")
		}
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, reached
}

func TestAudiences(t *testing.T) {
	idp := newTestIdP(t)
	base := idp.verifier(t)

	mcpToken := idp.token(t, map[string]any{"aud": []string{"amp-mcp", "mcp-1234"}, "azp": "mcp-1234"})
	legacyToken := idp.token(t, map[string]any{"aud": "mcp-1234"}) // pre-upgrade: no amp-mcp audience
	uiToken := idp.token(t, map[string]any{"aud": "amp-ui"})

	cases := []struct {
		name      string
		audiences []string
		token     string
		wantCode  int
	}{
		{"accepts cross-client audience", []string{"amp-mcp"}, mcpToken, 200},
		{"rejects pre-upgrade token", []string{"amp-mcp"}, legacyToken, 401},
		{"rejects other client's token", []string{"amp-mcp"}, uiToken, 401},
		{"rest accepts ui audience", []string{"amp-ui", "amp-mcp"}, uiToken, 200},
		{"rest accepts mcp audience", []string{"amp-ui", "amp-mcp"}, mcpToken, 200},
		{"rest rejects pre-upgrade token", []string{"amp-ui", "amp-mcp"}, legacyToken, 401},
		{"no audiences configured accepts any", nil, uiToken, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := base.WithAudiences("test", tc.audiences...)
			rec, reached := do(t, v, "Bearer "+tc.token)
			if reached != (tc.wantCode == 200) || rec.Code != tc.wantCode {
				t.Fatalf("reached=%v code=%d, want %d (body %s)", reached, rec.Code, tc.wantCode, rec.Body)
			}
			if tc.wantCode == 401 {
				if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, `error="invalid_token"`) {
					t.Fatalf("WWW-Authenticate=%q, want invalid_token challenge so clients re-authorize", got)
				}
			}
		})
	}
}

func TestWithAudiencesDoesNotMutateBase(t *testing.T) {
	idp := newTestIdP(t)
	base := idp.verifier(t)
	_ = base.WithAudiences("mcp", "amp-mcp")
	rec, reached := do(t, base, "Bearer "+idp.token(t, map[string]any{"aud": "anything"}))
	if !reached {
		t.Fatalf("base verifier started enforcing after WithAudiences: %d %s", rec.Code, rec.Body)
	}
}

func TestChallenges(t *testing.T) {
	idp := newTestIdP(t)
	v := idp.verifier(t).WithAudiences("mcp", "amp-mcp")
	v.ResourceMetadataURL = "https://amp.example.test/mcp/.well-known/oauth-protected-resource"
	v.Scope = "openid email profile"

	t.Run("no credentials: no error code, still discoverable", func(t *testing.T) {
		rec, _ := do(t, v, "")
		got := rec.Header().Get("WWW-Authenticate")
		if rec.Code != 401 || strings.Contains(got, "error=") {
			t.Fatalf("code=%d challenge=%q", rec.Code, got)
		}
		for _, want := range []string{`resource_metadata="` + v.ResourceMetadataURL + `"`, `scope="openid email profile"`} {
			if !strings.Contains(got, want) {
				t.Fatalf("challenge %q missing %s", got, want)
			}
		}
	})
	t.Run("bad signature", func(t *testing.T) {
		other := newTestIdP(t)
		rec, reached := do(t, v, "Bearer "+other.token(t, map[string]any{"aud": "amp-mcp"}))
		if reached || rec.Code != 401 {
			t.Fatalf("forged token accepted: %d", rec.Code)
		}
	})
	t.Run("expired", func(t *testing.T) {
		tok := idp.token(t, map[string]any{"aud": "amp-mcp", "exp": time.Now().Add(-time.Minute).Unix()})
		if _, reached := do(t, v, "Bearer "+tok); reached {
			t.Fatal("expired token accepted")
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		tok := idp.token(t, map[string]any{"aud": "amp-mcp", "iss": "https://evil.example/dex"})
		if _, reached := do(t, v, "Bearer "+tok); reached {
			t.Fatal("token from another issuer accepted")
		}
	})
}

// channelSeen runs one request through v and returns the identity's channel.
func channelSeen(t *testing.T, v *Verifier, authz string) string {
	t.Helper()
	var got string
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ident, _ := FromContext(r.Context())
		got = ident.Channel
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("request refused: %d %s", rec.Code, rec.Body)
	}
	return got
}

// Only a token issued to the UI's own client, on the REST listener, is the
// web UI. The same person's MCP token is not, on either listener.
func TestChannel(t *testing.T) {
	idp := newTestIdP(t)
	base, err := NewVerifier(context.Background(), Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, UIClientID: "amp-ui"})
	if err != nil {
		t.Fatal(err)
	}
	rest := base.WithAudiences("rest", "amp-ui", "amp-mcp")
	mcp := base.WithAudiences("mcp", "amp-mcp")
	ui := idp.token(t, map[string]any{"aud": "amp-ui"})
	uiAZP := idp.token(t, map[string]any{"aud": []string{"amp-ui"}, "azp": "amp-ui"})
	mcpTok := idp.token(t, map[string]any{"aud": []string{"amp-mcp", "mcp-1234"}, "azp": "mcp-1234"})
	// A token naming the UI among its audiences but issued to another client.
	borrowed := idp.token(t, map[string]any{"aud": []string{"amp-ui", "amp-mcp"}, "azp": "mcp-1234"})
	for _, tc := range []struct {
		name string
		v    *Verifier
		tok  string
		want string
	}{
		{"UI token on REST", rest, ui, ChannelUI},
		{"UI token with azp on REST", rest, uiAZP, ChannelUI},
		{"MCP token on REST", rest, mcpTok, ChannelRESTOther},
		{"UI audience, other azp", rest, borrowed, ChannelRESTOther},
		{"MCP token on MCP", mcp, mcpTok, ChannelMCP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := channelSeen(t, tc.v, "Bearer "+tc.tok); got != tc.want {
				t.Fatalf("channel = %q, want %q", got, tc.want)
			}
		})
	}
	// Without UI_CLIENT_ID, no token is the UI.
	noUI := idp.verifier(t).WithAudiences("rest", "amp-ui")
	if got := channelSeen(t, noUI, "Bearer "+ui); got != ChannelRESTOther {
		t.Fatalf("no UI client configured: channel = %q", got)
	}
	// Auth off (compose): REST is the UI, MCP never is.
	off, _ := NewVerifier(context.Background(), Config{})
	if got := channelSeen(t, off.WithAudiences("rest"), ""); got != ChannelUI {
		t.Fatalf("auth off, REST: %q", got)
	}
	if got := channelSeen(t, off.WithAudiences("mcp"), ""); got != ChannelMCP {
		t.Fatalf("auth off, MCP: %q", got)
	}
}

// identitySeen runs one request through v and returns the identity.
func identitySeen(t *testing.T, v *Verifier, authz string) *Identity {
	t.Helper()
	var got *Identity
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("request refused: %d %s", rec.Code, rec.Body)
	}
	return got
}

// The audit log's auth_method and session_id: the UI's session, an MCP
// client's OAuth token, authadmin (no listener label, behind oauth2-proxy)
// and auth off. The session id is derived from sub+iat, never the token.
func TestAuthMethodAndSession(t *testing.T) {
	idp := newTestIdP(t)
	base, err := NewVerifier(context.Background(), Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, UIClientID: "amp-ui"})
	if err != nil {
		t.Fatal(err)
	}
	iat := time.Now().Add(-time.Minute).Truncate(time.Second)
	ui := idp.token(t, map[string]any{"aud": "amp-ui", "iat": iat.Unix()})
	mcpTok := idp.token(t, map[string]any{"aud": []string{"amp-mcp"}, "azp": "mcp-1234", "iat": iat.Unix()})
	for _, tc := range []struct {
		name string
		v    *Verifier
		tok  string
		want string
	}{
		{"UI token on REST", base.WithAudiences("rest", "amp-ui", "amp-mcp"), ui, AuthDexSession},
		{"MCP token on REST", base.WithAudiences("rest", "amp-ui", "amp-mcp"), mcpTok, AuthOAuthMCP},
		{"MCP token on MCP", base.WithAudiences("mcp", "amp-mcp"), mcpTok, AuthOAuthMCP},
		{"authadmin (no label)", base, ui, AuthDexSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ident := identitySeen(t, tc.v, "Bearer "+tc.tok)
			if ident.AuthMethod != tc.want {
				t.Fatalf("auth method = %q, want %q", ident.AuthMethod, tc.want)
			}
			if want := SessionID("user-1", iat); ident.SessionID != want || len(want) != 32 {
				t.Fatalf("session id = %q, want %q", ident.SessionID, want)
			}
			if strings.Contains(tc.tok, ident.SessionID) {
				t.Fatal("session id is part of the token")
			}
		})
	}
	off, _ := NewVerifier(context.Background(), Config{})
	if got := identitySeen(t, off.WithAudiences("rest"), ""); got.AuthMethod != AuthLocalDev {
		t.Fatalf("auth off: %q", got.AuthMethod)
	}
	if SessionID("user-1", iat) == SessionID("user-1", iat.Add(time.Second)) {
		t.Fatal("a new sign-in (new iat) kept the session id")
	}
	if SessionID("", iat) != "" || SessionID("user-1", time.Time{}) != "" {
		t.Fatal("session id without sub or iat")
	}
}

// #2440 (review #2432 L3): oauth2-proxy refreshes the UI token hourly. Dex
// keeps the sign-in's nonce in every refreshed token but issues a new iat,
// so the session id follows the nonce (or auth_time), and auth.login is
// written once per sign-in, not once an hour.
func TestSessionSurvivesRefresh(t *testing.T) {
	idp := newTestIdP(t)
	v, err := NewVerifier(context.Background(), Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, UIClientID: "amp-ui"})
	if err != nil {
		t.Fatal(err)
	}
	v = v.WithAudiences("rest", "amp-ui")
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	sid := func(claims map[string]any) string {
		t.Helper()
		claims["aud"] = "amp-ui"
		return identitySeen(t, v, "Bearer "+idp.token(t, claims)).SessionID
	}
	signIn := sid(map[string]any{"iat": t0.Unix(), "nonce": "n-1"})
	refreshed := sid(map[string]any{"iat": t0.Add(time.Hour).Unix(), "nonce": "n-1"})
	again := sid(map[string]any{"iat": t0.Add(2 * time.Hour).Unix(), "nonce": "n-1"})
	if signIn == "" || refreshed != signIn || again != signIn {
		t.Fatalf("a refreshed token (same nonce, new iat) changed the session: %q %q %q", signIn, refreshed, again)
	}
	if other := sid(map[string]any{"iat": t0.Add(time.Hour).Unix(), "nonce": "n-2"}); other == signIn {
		t.Fatal("a new sign-in (new nonce) kept the session id")
	}
	// auth_time wins over the nonce when Dex sets it.
	at := sid(map[string]any{"iat": t0.Unix(), "auth_time": t0.Unix(), "nonce": "n-1"})
	if at2 := sid(map[string]any{"iat": t0.Add(time.Hour).Unix(), "auth_time": t0.Unix(), "nonce": "n-9"}); at2 != at || at == signIn {
		t.Fatalf("auth_time session ids: %q %q (nonce one %q)", at, at2, signIn)
	}
	// Neither: iat, as before (so existing rows keep their meaning).
	if got := sid(map[string]any{"iat": t0.Unix()}); got != SessionID("user-1", t0) {
		t.Fatalf("no nonce: %q, want the sub+iat id", got)
	}
	if DexSessionID("", 1, "n", t0) != "" {
		t.Fatal("session id without a subject")
	}
}

// RequireRole's 403 says what's missing, never a bare "forbidden" (#124).
func TestRequireRole_SaysWhatIsMissing(t *testing.T) {
	h := RequireRole(domain.RoleAdmin, func(w http.ResponseWriter, r *http.Request) { t.Fatal("handler ran") })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/users", nil)
	req = req.WithContext(WithIdentity(req.Context(), &Identity{Subject: "s", User: &domain.User{ID: 1, Roles: []string{domain.RoleMember}}}))
	h(rec, req)
	var body struct{ Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 403 {
		t.Fatalf("%d %s (%v)", rec.Code, rec.Body, err)
	}
	if strings.EqualFold(strings.TrimSpace(body.Error), "forbidden") || !strings.Contains(body.Error, "admin") {
		t.Fatalf("403 body = %q", body.Error)
	}
}

// RequireRole's 403 says which role is needed and who the caller is signed
// in as, in the {"error": string} shape the UI already reads (#69).
func TestRequireRole_Message(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ident *Identity
		code  int
		want  string
	}{
		{"no identity", nil, 403, "this needs the AMP admin role, and this request isn't signed in as anyone AMP knows; sign in, then ask an AMP admin to grant it"},
		{"user without the role", &Identity{Email: "tok@example.test", User: &domain.User{ID: 1, Email: "ann@example.test"}}, 403,
			"this needs the AMP admin role, which your account (ann@example.test) doesn't have; ask an AMP admin to grant it"},
		{"identity without a user", &Identity{Email: "bob@example.test"}, 403,
			"this needs the AMP admin role, and you're signed in as bob@example.test, who has no AMP account; ask an AMP admin to invite you, then sign in again"},
		{"sign-in that provisioning refused", &Identity{Email: "eve@example.test", ProvisionRefusal: RefusalUnverifiedEmail}, 403,
			"this needs the AMP admin role. Your identity provider reports your email as unverified, so AMP did not create your account; ask your IdP admin to verify it."},
		{"local dev without its user", &Identity{Subject: LocalDevSubject, Email: LocalDevEmail, LocalDev: true}, 403,
			"this needs the AMP admin role, and the Local Dev user couldn't be set up at startup (see amp-api's log); restart amp-api once that's fixed"},
		{"identity with no user and no email", &Identity{Subject: "machine"}, 403,
			"this needs the AMP admin role, and this request isn't signed in as anyone AMP knows; sign in, then ask an AMP admin to grant it"},
		{"admin", &Identity{User: &domain.User{ID: 2, Email: "root@example.test", Roles: []string{domain.RoleAdmin}}}, 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := RequireRole(domain.RoleAdmin, func(w http.ResponseWriter, r *http.Request) {})
			req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
			if tc.ident != nil {
				req = req.WithContext(WithIdentity(req.Context(), tc.ident))
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d", rec.Code, tc.code)
			}
			if tc.code == 200 {
				return
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type %q", ct)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body, err)
			}
			if body["error"] != tc.want || len(body) != 1 {
				t.Errorf("body %v, want {error: %q}", body, tc.want)
			}
		})
	}
}
