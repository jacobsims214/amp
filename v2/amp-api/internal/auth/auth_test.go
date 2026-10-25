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
