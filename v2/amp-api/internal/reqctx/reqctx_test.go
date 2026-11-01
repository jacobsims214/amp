package reqctx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Envoy's pods (a pod CIDR) and, for a mesh sidecar in front of each
// service, the sidecar's inbound passthrough address.
const envoyHops = "10.244.0.0/16, fd00:10:244::/56, 127.0.0.6"

func mustResolver(t *testing.T, list string) *Resolver {
	t.Helper()
	p, err := ParseTrusted(list)
	if err != nil {
		t.Fatal(err)
	}
	return NewResolver(p)
}

// The rule (KB decisions/audit-log-v2.md, as built by #2469): Envoy decides
// the client and says so in X-Amp-Client-Ip; the services believe that one
// header from a trusted peer and nothing else.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name    string
		trusted string
		remote  string
		hdr     []string
		want    string
	}{
		// Nothing trusted (AMP_TRUSTED_PROXIES unset): always the peer.
		{"untrusted peer, no header", "", "203.0.113.7:5555", nil, "203.0.113.7"},
		{"nothing trusted ignores the header", "", "10.244.0.12:5555", []string{"6.6.6.6"}, "10.244.0.12"},
		// A peer outside the list (anything that bypassed Envoy) can't
		// choose its address.
		{"untrusted peer's header is ignored", envoyHops, "198.51.100.9:443", []string{"6.6.6.6"}, "198.51.100.9"},
		{"untrusted v6 peer", envoyHops, "[2001:db8::2]:443", []string{"6.6.6.6"}, "2001:db8::2"},

		// From Envoy: its decision.
		{"Envoy's decision", envoyHops, "10.244.0.12:40000", []string{"203.0.113.7"}, "203.0.113.7"},
		{"Envoy's decision, v6", envoyHops, "10.244.0.12:40000", []string{"2001:db8::1"}, "2001:db8::1"},
		{"Envoy over v6", envoyHops, "[fd00:10:244::12]:40000", []string{"203.0.113.7"}, "203.0.113.7"},
		{"surrounding space is trimmed", envoyHops, "10.244.0.12:40000", []string{" 203.0.113.7 "}, "203.0.113.7"},
		{"v4-mapped peer is unmapped and trusted", envoyHops, "[::ffff:10.244.0.12]:40000", []string{"203.0.113.7"}, "203.0.113.7"},
		{"v4-mapped client is unmapped", envoyHops, "10.244.0.12:40000", []string{"::ffff:203.0.113.7"}, "203.0.113.7"},
		// Behind a mesh sidecar the services' peer is the sidecar.
		{"through a sidecar (127.0.0.6)", envoyHops, "127.0.0.6:51000", []string{"203.0.113.7"}, "203.0.113.7"},
		{"sidecar address not listed", "10.244.0.0/16", "127.0.0.6:51000", []string{"203.0.113.7"}, "127.0.0.6"},
		{"bare address in the trust list", "127.0.0.1", "127.0.0.1:9", []string{"203.0.113.7"}, "203.0.113.7"},

		// What Envoy never sends is not believed: the peer is recorded.
		{"trusted peer, no header: the peer", envoyHops, "10.244.0.12:40000", nil, "10.244.0.12"},
		{"two headers: the peer", envoyHops, "10.244.0.12:40000", []string{"203.0.113.7", "6.6.6.6"}, "10.244.0.12"},
		{"a list: the peer", envoyHops, "10.244.0.12:40000", []string{"6.6.6.6, 203.0.113.7"}, "10.244.0.12"},
		{"ip:port: the peer", envoyHops, "10.244.0.12:40000", []string{"203.0.113.7:1234"}, "10.244.0.12"},
		{"zone: the peer", envoyHops, "10.244.0.12:40000", []string{"fe80::1%eth0"}, "10.244.0.12"},
		{"garbage: the peer", envoyHops, "10.244.0.12:40000", []string{"not-an-ip"}, "10.244.0.12"},
		{"empty: the peer", envoyHops, "10.244.0.12:40000", []string{""}, "10.244.0.12"},
		{"RemoteAddr without a port", "", "203.0.113.7", nil, "203.0.113.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := mustResolver(t, c.trusted).ClientIP(c.remote, c.hdr)
			if got.String() != c.want || reason != "" {
				t.Fatalf("ClientIP(%q, %q) = %v (%q), want %s", c.remote, c.hdr, got, reason, c.want)
			}
		})
	}
}

func TestClientIP_UnparseablePeer(t *testing.T) {
	got, reason := mustResolver(t, envoyHops).ClientIP("@unix", []string{"203.0.113.7"})
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
	p, err := ParseTrusted(" 10.244.0.0/16,\n192.0.2.0/24  2001:db8::/32,127.0.0.6 ::1 ::ffff:10.0.0.0/104 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 6 {
		t.Fatalf("got %v", p)
	}
	if p[3].String() != "127.0.0.6/32" || p[5].String() != "10.0.0.0/8" {
		t.Fatalf("got %v, want 127.0.0.6/32 and v4-mapped 10.0.0.0/8", p)
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

// Spoofing: X-Forwarded-For never chooses the client, and no forwarding or
// vendor client-IP header is read, whoever the peer is.
func TestMiddleware(t *testing.T) {
	var got *Info
	h := Middleware(ListenerREST, mustResolver(t, envoyHops))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
	}))

	req := httptest.NewRequest("POST", "/api/x", nil)
	req.RemoteAddr = "10.244.0.12:40000"
	req.Header.Set(HeaderClientIP, "203.0.113.7")
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.7, 10.244.1.5")
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

	// From Envoy, without its header: nothing else is believed.
	for _, hdr := range [][2]string{
		{"X-Forwarded-For", "6.6.6.6"},
		{"Forwarded", "for=6.6.6.6"},
		{"X-Real-IP", "6.6.6.6"},
		{"True-Client-IP", "6.6.6.6"},
		{"CF-Connecting-IP", "6.6.6.6"},
		{"X-Envoy-External-Address", "6.6.6.6"},
	} {
		req = httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.244.0.12:40000"
		req.Header.Set(hdr[0], hdr[1])
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got.SourceIP.String() != "10.244.0.12" {
			t.Errorf("%s was believed: %v", hdr[0], got.SourceIP)
		}
	}

	// Bypassing Envoy: the caller's own X-Amp-Client-Ip is ignored.
	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.9:443"
	req.Header.Set(HeaderClientIP, "6.6.6.6")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.SourceIP.String() != "198.51.100.9" {
		t.Errorf("an untrusted peer chose its address: %v", got.SourceIP)
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

// Kubelet probes reach the services from the node, whose address is often
// inside the trusted pod CIDR (a CNI's host interface), without Envoy's
// header. They must not trip the "trusted proxy sent no header" warning,
// which is one-shot and would then be spent on probes instead of on a real
// misconfigured Envoy, and they get no audit Info.
func TestMiddleware_ProbesAreNotResolved(t *testing.T) {
	res := mustResolver(t, envoyHops)
	var got *Info
	var called bool
	h := Middleware(ListenerREST, res)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		got, _ = FromContext(r.Context())
	}))
	for _, path := range []string{"/health", "/ready"} {
		called, got = false, nil
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "10.244.0.145:51234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !called {
			t.Fatalf("%s: handler not called", path)
		}
		if got != nil {
			t.Errorf("%s: probe got audit Info %+v", path, got)
		}
		if rec.Header().Get("X-Request-Id") != "" {
			t.Errorf("%s: probe got a request id", path)
		}
	}
	if res.warned.Load() {
		t.Error("a probe spent the missing-header warning")
	}

	// A real request from a trusted peer without the header still warns.
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.RemoteAddr = "10.244.0.12:40000"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !res.warned.Load() {
		t.Error("a trusted peer without the header on /api/me did not warn")
	}
	if got == nil || got.SourceIP.String() != "10.244.0.12" {
		t.Errorf("Info = %+v", got)
	}

	// Only the exact probe paths are skipped.
	for _, p := range []string{"/healthz", "/health/x", "/api/health", "/readyz"} {
		if ProbePath(p) {
			t.Errorf("ProbePath(%q) = true", p)
		}
	}
}
