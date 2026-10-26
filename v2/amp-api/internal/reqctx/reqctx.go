// Package reqctx resolves who is on the other end of an HTTP request, for
// the audit log: the trusted client IP, the user agent, the request id and
// the trace id. Every AMP listener (amp-api REST and MCP, authadmin,
// mcpoauth, and the relay on remote-m1) wraps its handler in Middleware, and
// the audit writer reads the result back with FromContext.
//
// # The client IP (KB decisions/audit-log-v2.md, section 2)
//
// Client-sent headers are spoofable, so the resolver trusts only what a
// trusted proxy added:
//
//   - CF-Connecting-IP, X-Real-IP and Forwarded are never read.
//   - If the TCP peer (RemoteAddr) is not in AMP_TRUSTED_PROXIES, the peer
//     is the client and every header is ignored.
//   - Otherwise every X-Forwarded-For header is joined and walked right to
//     left, skipping trusted addresses; the first untrusted address is the
//     client. A value the client made up sits to the left of the address
//     the first trusted proxy appended, so the walk never reaches it.
//   - If every address is trusted, the leftmost one examined is the client.
//   - An entry that isn't an IP address stops the walk, and the last trusted
//     hop is the client.
//
// With AMP_TRUSTED_PROXIES unset nothing is trusted, so the peer is always
// the client: never spoofable, but behind Envoy it records Envoy's address.
package reqctx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"unicode/utf8"
)

// EnvTrustedProxies names the environment variable holding the trusted
// proxy list: comma- or space-separated CIDRs or bare addresses.
const EnvTrustedProxies = "AMP_TRUSTED_PROXIES"

// Limits on what a request may put in an audit row.
const (
	MaxUserAgent = 512
	MaxRequestID = 128
	MaxXFF       = 512
)

// Listener names, recorded so the audit writer can tell which surface a
// request arrived on.
const (
	ListenerREST      = "rest"
	ListenerMCP       = "mcp"
	ListenerAuthAdmin = "authadmin"
	ListenerMCPOAuth  = "mcpoauth"
	ListenerRelay     = "relay"
)

// Info is what Middleware learned about one request.
type Info struct {
	// SourceIP is the resolved client address; invalid (zero) when it
	// couldn't be resolved, with IPReason saying why.
	SourceIP netip.Addr
	IPReason string
	// XFF is the raw X-Forwarded-For chain as received, kept only when it
	// was shorter than MaxXFF bytes (for checking the resolution later).
	XFF string
	// UserAgent is the client-asserted User-Agent, clipped to MaxUserAgent.
	UserAgent string
	// RequestID is the edge proxy's x-request-id when it is well formed,
	// otherwise one generated here.
	RequestID string
	// TraceID is the W3C traceparent trace id, if the request carried one.
	TraceID string
	// Listener is the surface that served the request (the Listener
	// constants).
	Listener string
}

type ctxKey struct{}

// WithInfo returns ctx carrying info.
func WithInfo(ctx context.Context, info *Info) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

// FromContext returns the request's Info, if Middleware ran.
func FromContext(ctx context.Context) (*Info, bool) {
	info, ok := ctx.Value(ctxKey{}).(*Info)
	return info, ok && info != nil
}

// Resolver resolves client addresses. The zero value trusts nothing.
type Resolver struct {
	trusted []netip.Prefix
}

// NewResolver returns a resolver that trusts the given proxy prefixes.
func NewResolver(trusted []netip.Prefix) *Resolver {
	return &Resolver{trusted: append([]netip.Prefix(nil), trusted...)}
}

// ParseTrusted parses a comma- or whitespace-separated list of CIDRs and
// bare addresses ("10.244.0.0/16, 173.245.48.0/20 2400:cb00::/32 127.0.0.1").
// A bare address is a single-host prefix.
func ParseTrusted(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not a CIDR: %w", EnvTrustedProxies, f, err)
			}
			if p.Addr().Is4In6() && p.Bits() >= 96 {
				p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not an IP address or CIDR: %w", EnvTrustedProxies, f, err)
		}
		a = a.Unmap().WithZone("")
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// ResolverFromEnv builds the resolver from AMP_TRUSTED_PROXIES. A malformed
// list is an error: guessing which part was meant could trust too much.
func ResolverFromEnv() (*Resolver, error) {
	trusted, err := ParseTrusted(os.Getenv(EnvTrustedProxies))
	if err != nil {
		return nil, err
	}
	if len(trusted) == 0 {
		slog.Info(EnvTrustedProxies + " is empty: audit rows record each request's TCP peer as its client IP")
	} else {
		slog.Info("trusted proxies for client IP resolution", "prefixes", len(trusted))
	}
	return NewResolver(trusted), nil
}

// FromEnv is ResolverFromEnv for a listener's main(): a malformed list is
// logged and nothing is trusted, which records the TCP peer (never a
// spoofable header) rather than refusing to start.
func FromEnv() *Resolver {
	r, err := ResolverFromEnv()
	if err != nil {
		slog.Error("ignoring "+EnvTrustedProxies+": nothing is trusted, so audit rows record each request's TCP peer", "err", err)
		return NewResolver(nil)
	}
	return r
}

// Trusted reports whether a is one of the trusted proxies.
func (r *Resolver) Trusted(a netip.Addr) bool {
	if r == nil || !a.IsValid() {
		return false
	}
	a = a.Unmap().WithZone("")
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP resolves the client address from the TCP peer (an http.Request's
// RemoteAddr, "host:port" or a bare address) and the request's
// X-Forwarded-For header values, by the rule in the package comment. reason
// is set when the result is invalid.
func (r *Resolver) ClientIP(remoteAddr string, xff []string) (ip netip.Addr, reason string) {
	peer, ok := parsePeer(remoteAddr)
	if !ok {
		return netip.Addr{}, "unparseable remote address"
	}
	if !r.Trusted(peer) {
		return peer, ""
	}
	hops := splitXFF(xff)
	last := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			return last, "" // garbage: stop at the last trusted hop
		}
		a = a.Unmap().WithZone("")
		if !r.Trusted(a) {
			return a, ""
		}
		last = a
	}
	return last, ""
}

func parsePeer(remoteAddr string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}

func splitXFF(values []string) []string {
	var hops []string
	for _, v := range values {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	return hops
}

// Middleware resolves the request's Info and stores it on the request
// context for the audit writer. It echoes the request id back in the
// X-Request-Id response header so a caller can quote it.
func Middleware(listener string, res *Resolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := Capture(r, listener, res)
			w.Header().Set("X-Request-Id", info.RequestID)
			next.ServeHTTP(w, r.WithContext(WithInfo(r.Context(), info)))
		})
	}
}

// Capture builds a request's Info (Middleware without the wrapping).
func Capture(r *http.Request, listener string, res *Resolver) *Info {
	xff := r.Header.Values("X-Forwarded-For")
	ip, reason := res.ClientIP(r.RemoteAddr, xff)
	info := &Info{
		SourceIP:  ip,
		IPReason:  reason,
		UserAgent: Clip(r.UserAgent(), MaxUserAgent),
		RequestID: requestID(r.Header.Get("X-Request-Id")),
		TraceID:   traceID(r.Header.Get("Traceparent")),
		Listener:  listener,
	}
	if raw := strings.Join(xff, ", "); raw != "" && len(raw) < MaxXFF {
		info.XFF = Clip(raw, MaxXFF)
	}
	return info
}

// ValidRequestID reports whether id is a request id AMP keeps as given: 1 to
// MaxRequestID characters of letters, digits and dashes (Envoy's UUIDs).
func ValidRequestID(id string) bool {
	if id == "" || len(id) > MaxRequestID {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func requestID(given string) string {
	if ValidRequestID(given) {
		return given
	}
	return NewRequestID()
}

// NewRequestID returns a random UUIDv4-shaped id.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// traceID returns the trace id of a W3C traceparent header
// ("00-<32 hex>-<16 hex>-<2 hex>"), or "" when it is absent or malformed.
func traceID(tp string) string {
	parts := strings.Split(strings.TrimSpace(tp), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || len(parts[1]) != 32 {
		return ""
	}
	id := strings.ToLower(parts[1])
	if _, err := hex.DecodeString(id); err != nil || id == strings.Repeat("0", 32) {
		return ""
	}
	return id
}

// Clip makes s safe to store in a Postgres TEXT column and at most max
// bytes long: invalid UTF-8 and NUL bytes are dropped, and it is cut on a
// rune boundary.
func Clip(s string, max int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
