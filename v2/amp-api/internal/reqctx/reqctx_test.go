package reqctx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The production trust list after #2430: the pod CIDR (Traefik, Envoy) and
// a Cloudflare range of each family.
const prodTrusted = "10.244.0.0/16, 173.245.48.0/20 2400:cb00::/32"

func mustResolver(t *testing.T, list string) *Resolver {
	t.Helper()
	p, err := ParseTrusted(list)
	if err != nil {
		t.Fatal(err)
	}
	return NewResolver(p)
}

// The spoof-proof rule (KB decisions/audit-log-v2.md, section 2).
func TestClientIP(t *testing.T) {
	cases := []struct {
		name    string
		trusted string
		remote  string
		xff     []string
		want    string
	}{
		// Nothing trusted (AMP_TRUSTED_PROXIES unset): always the peer.
		{"untrusted peer, no header", "", "203.0.113.7:5555", nil, "203.0.113.7"},
		{"untrusted peer ignores a spoofed XFF", "", "203.0.113.7:5555", []string{"6.6.6.6"}, "203.0.113.7"},
		{"peer outside the list ignores XFF", prodTrusted, "198.51.100.9:443", []string{"6.6.6.6, 10.244.1.5"}, "198.51.100.9"},

		// Through Cloudflare, Traefik and Envoy: [spoofed, client, cf-edge, traefik].
		{"real chain through the edge", prodTrusted, "10.244.0.12:40000",
			[]string{"6.6.6.6, 203.0.113.7, 173.245.48.10, 10.244.1.5"}, "203.0.113.7"},
		{"spoof left of the client is never reached", prodTrusted, "10.244.0.12:40000",
			[]string{"1.1.1.1, 2.2.2.2, 203.0.113.7, 10.244.1.5"}, "203.0.113.7"},
		{"several XFF headers are joined in order", prodTrusted, "10.244.0.12:40000",
			[]string{"6.6.6.6", "203.0.113.7, 173.245.48.10", "10.244.1.5"}, "203.0.113.7"},
		// Direct to origin: Traefik strips XFF, so [attacker, traefik].
		{"direct to origin records the attacker", prodTrusted, "10.244.0.12:40000",
			[]string{"198.51.100.66, 10.244.1.5"}, "198.51.100.66"},
		// A pod forging a header still sits on the right, as the peer.
		{"client address only from trusted hops", prodTrusted, "10.244.0.12:40000",
			[]string{"10.244.3.3, 10.244.1.5"}, "10.244.3.3"},
		{"trusted peer, no header: the peer", prodTrusted, "10.244.0.12:40000", nil, "10.244.0.12"},
		{"garbage stops the walk at the last trusted hop", prodTrusted, "10.244.0.12:40000",
			[]string{"203.0.113.7, not-an-ip, 10.244.1.5"}, "10.244.1.5"},
		{"empty entries are skipped", prodTrusted, "10.244.0.12:40000",
			[]string{" , 203.0.113.7,, 10.244.1.5 "}, "203.0.113.7"},
		{"ip:port in XFF is garbage, not a client", prodTrusted, "10.244.0.12:40000",
			[]string{"203.0.113.7:1234, 10.244.1.5"}, "10.244.1.5"},

		// IPv6.
		{"v6 client through a v6 edge", prodTrusted, "10.244.0.12:40000",
			[]string{"2001:db8::1, 2400:cb00::7, 10.244.1.5"}, "2001:db8::1"},
		{"v6 peer untrusted", prodTrusted, "[2001:db8::2]:443", []string{"6.6.6.6"}, "2001:db8::2"},
		{"v4-mapped peer is unmapped and trusted", prodTrusted, "[::ffff:10.244.0.12]:40000",
			[]string{"203.0.113.7"}, "203.0.113.7"},
		{"v4-mapped client is unmapped", prodTrusted, "10.244.0.12:40000",
			[]string{"::ffff:203.0.113.7"}, "203.0.113.7"},

		// docker-desktop: kubectl port-forward makes Envoy's peer 127.0.0.1.
		{"local: spoof through port-forward", "10.244.0.0/16", "10.244.0.30:5000",
			[]string{"6.6.6.6, 127.0.0.1"}, "127.0.0.1"},
		{"bare address in the trust list", "127.0.0.1", "127.0.0.1:9", []string{"203.0.113.7"}, "203.0.113.7"},
		{"RemoteAddr without a port", "", "203.0.113.7", nil, "203.0.113.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := mustResolver(t, c.trusted).ClientIP(c.remote, c.xff)
			if got.String() != c.want || reason != "" {
				t.Fatalf("ClientIP(%q, %q) = %v (%q), want %s", c.remote, c.xff, got, reason, c.want)
			}
		})
	}
}

func TestClientIP_UnparseablePeer(t *testing.T) {
	got, reason := mustResolver(t, prodTrusted).ClientIP("@unix", []string{"203.0.113.7"})
	if got.IsValid() || reason == "" {
		t.Fatalf("got %v %q, want no address and a reason", got, reason)
	}
}

func TestClientIP_NilResolverTrustsNothing(t *testing.T) {
	var r *Resolver
	if got, _ := r.ClientIP("203.0.113.7:1", []string{"6.6.6.6"}); got.String() != "203.0.113.7" {
		t.Fatalf("got %v", got)
	}
}

func TestParseTrusted(t *testing.T) {
	p, err := ParseTrusted(" 10.244.0.0/16,\n173.245.48.0/20  2400:cb00::/32,127.0.0.1 ::1 ::ffff:10.0.0.0/104 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 6 {
		t.Fatalf("got %v", p)
	}
	if p[5].String() != "10.0.0.0/8" {
		t.Fatalf("v4-mapped prefix = %v, want 10.0.0.0/8", p[5])
	}
	for _, bad := range []string{"10.244.0.0/33", "not-an-ip", "10.0.0.0/8, nope"} {
		if _, err := ParseTrusted(bad); err == nil {
			t.Errorf("ParseTrusted(%q) accepted", bad)
		}
	}
	if p, err := ParseTrusted(""); err != nil || len(p) != 0 {
		t.Fatalf("empty list = %v, %v", p, err)
	}
}

func TestFromEnv_BadListTrustsNothing(t *testing.T) {
	t.Setenv(EnvTrustedProxies, "10.0.0.0/8, garbage")
	r := FromEnv()
	if got, _ := r.ClientIP("10.1.2.3:1", []string{"6.6.6.6"}); got.String() != "10.1.2.3" {
		t.Fatalf("a malformed list trusted something: got %v", got)
	}
}

// The middleware ignores every header but X-Forwarded-For, and only via
// the walk.
func TestMiddleware(t *testing.T) {
	var got *Info
	h := Middleware(ListenerREST, mustResolver(t, prodTrusted))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
	}))

	req := httptest.NewRequest("POST", "/api/x", nil)
	req.RemoteAddr = "10.244.0.12:40000"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.7, 10.244.1.5")
	req.Header.Set("CF-Connecting-IP", "6.6.6.6")
	req.Header.Set("X-Real-IP", "6.6.6.6")
	req.Header.Set("Forwarded", "for=6.6.6.6")
	req.Header.Set("User-Agent", "amp-test/1 "+strings.Repeat("x", 600))
	req.Header.Set("X-Request-Id", "0f1e2d3c-aaaa-4bbb-8ccc-123456789abc")
	req.Header.Set("Traceparent", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got == nil {
		t.Fatal("no Info on the request context")
	}
	if got.SourceIP.String() != "203.0.113.7" || got.Listener != ListenerREST {
		t.Errorf("Info = %+v", got)
	}
	if len(got.UserAgent) != MaxUserAgent || !strings.HasPrefix(got.UserAgent, "amp-test/1 ") {
		t.Errorf("user agent not clipped to %d: %d", MaxUserAgent, len(got.UserAgent))
	}
	if got.RequestID != "0f1e2d3c-aaaa-4bbb-8ccc-123456789abc" || rec.Header().Get("X-Request-Id") != got.RequestID {
		t.Errorf("request id = %q, response header %q", got.RequestID, rec.Header().Get("X-Request-Id"))
	}
	if got.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace id = %q", got.TraceID)
	}
	if got.XFF != "6.6.6.6, 203.0.113.7, 10.244.1.5" {
		t.Errorf("xff = %q", got.XFF)
	}

	// Only CF-Connecting-IP / X-Real-IP / Forwarded set: they are ignored.
	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.244.0.12:40000"
	req.Header.Set("CF-Connecting-IP", "6.6.6.6")
	req.Header.Set("X-Real-IP", "6.6.6.6")
	req.Header.Set("Forwarded", "for=6.6.6.6")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.SourceIP.String() != "10.244.0.12" {
		t.Errorf("a non-XFF header was trusted: %v", got.SourceIP)
	}
}

func TestRequestID(t *testing.T) {
	for _, bad := range []string{"", "has space", "semi;colon", "ü", strings.Repeat("a", MaxRequestID+1)} {
		got := requestID(bad)
		if got == bad || !ValidRequestID(got) || len(got) != 36 {
			t.Errorf("requestID(%q) = %q, want a generated id", bad, got)
		}
	}
	if a, b := NewRequestID(), NewRequestID(); a == b {
		t.Errorf("ids repeat: %s", a)
	}
	ok := strings.Repeat("a", MaxRequestID)
	if requestID(ok) != ok {
		t.Errorf("a valid id was replaced")
	}
}

func TestTraceID(t *testing.T) {
	for tp, want := range map[string]string{
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01": "4bf92f3577b34da6a3ce929d0e0e4736",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01": "",
		"00-xyz-00f067aa0ba902b7-01":                              "",
		"00-4bf92f3577b34da6a3ce929d0e0e47zz-00f067aa0ba902b7-01": "",
		"": "",
	} {
		if got := traceID(tp); got != want {
			t.Errorf("traceID(%q) = %q, want %q", tp, got, want)
		}
	}
}

func TestClip(t *testing.T) {
	if got := Clip("a\x00b\xffc", 10); got != "abc" {
		t.Errorf("Clip = %q", got)
	}
	if got := Clip("ééé", 5); got != "éé" {
		t.Errorf("Clip cut inside a rune: %q", got)
	}
}
