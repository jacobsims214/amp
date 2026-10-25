// Package relay is the remote control relay inside amp-api (epic 165,
// protocol: v2/docs/remote-control/protocol.md). It joins a person's
// amp-harness on their laptop (the harness leg, /relay/v1/...) to the same
// person's browser (the browser leg, /api/sessions/...):
//
//   - The harness registers with its OAuth token, then holds a command stream
//     (SSE) and posts its signed events, authenticated by a session-bound
//     connect token.
//   - The browser, a person in the AMP web UI, watches the event stream (SSE,
//     with replay from an in-memory buffer) and posts signed commands.
//
// The relay checks envelope shape, ownership and limits; the harness and the
// browser verify signatures for themselves. Payloads live only in memory:
// never in Postgres, logs or audit rows (which hold metadata only, D3).
//
// The store, the org policy and the audit trail are in service/remote.go
// (#2371). With several replicas, a session is served on the one holding it
// (cluster.go, #2377).
package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/relayctl"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// Config tunes the relay. Zero values take the protocol's defaults; tests
// shorten the intervals.
type Config struct {
	// AppURL is the AMP web origin ("https://amp.example"): the base of
	// session_url and the only Origin the browser leg accepts.
	AppURL string
	// Node is this replica's api_nodes id (empty with CLUSTER_MODE=off).
	// Defaults to Cluster.ID().
	Node string
	// Cluster is this replica's membership; nil with CLUSTER_MODE=off (one
	// replica). With it, every request for a session is served on the
	// replica holding the session (cluster.go).
	Cluster Cluster
	// RouteTimeout bounds how long a request waits for a session's holder
	// to answer or be replaced (20 s: longer than the cluster TTL).
	RouteTimeout time.Duration
	// Keepalive is the SSE comment interval on both streams (20 s).
	Keepalive time.Duration
	// OfflineAfter marks the harness offline without a heartbeat (60 s).
	OfflineAfter time.Duration
	// Sweep is how often presence and the buffers are checked (5 s).
	Sweep time.Duration
	// Expire is how often the idle/lifetime sweep runs (30 s).
	Expire time.Duration
	// TokenTTL is the connect token's lifetime (15 min).
	TokenTTL time.Duration
	// Now is the clock (time.Now).
	Now func() time.Time
	// Registry holds live sessions (NewLocalRegistry()).
	Registry Registry
	// Forwarder sends browser calls to the replica that holds the session;
	// nil serves everything here (single replica).
	Forwarder Forwarder
}

func (c *Config) defaults() {
	if c.Keepalive <= 0 {
		c.Keepalive = 20 * time.Second
	}
	if c.OfflineAfter <= 0 {
		c.OfflineAfter = 60 * time.Second
	}
	if c.Sweep <= 0 {
		c.Sweep = 5 * time.Second
	}
	if c.Expire <= 0 {
		c.Expire = 30 * time.Second
	}
	if c.TokenTTL <= 0 {
		c.TokenTTL = 15 * time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Registry == nil {
		c.Registry = NewLocalRegistry()
	}
	if c.RouteTimeout <= 0 {
		c.RouteTimeout = 20 * time.Second
	}
	if c.Cluster != nil && c.Node == "" {
		c.Node = c.Cluster.ID()
	}
	c.AppURL = strings.TrimSuffix(c.AppURL, "/")
}

// Relay serves both legs.
type Relay struct {
	svc    *service.Service
	repo   *repository.Repo
	cfg    Config
	reg    Registry
	oauth  func(http.Handler) http.Handler
	client *http.Client // calls to other replicas

	mu        sync.Mutex
	registers map[int]*bucket // per user: 10 registration attempts a minute here
}

// New builds the relay on svc (whose Remote config must have the instance
// switch and auth on; main registers no relay routes otherwise). oauth is the
// MCP-audience token middleware (the one /mcp uses) for register and token
// refresh. New hooks svc.OnRemoteEnded so sessions the service ends (policy
// off, revoke-all, the expiry sweep) close their streams.
func New(svc *service.Service, oauth func(http.Handler) http.Handler, cfg Config) *Relay {
	cfg.defaults()
	rl := &Relay{
		svc: svc, repo: svc.Repo, cfg: cfg, reg: cfg.Registry, oauth: oauth,
		client: newInternalClient(), registers: map[int]*bucket{},
	}
	if cfg.Cluster != nil && cfg.Forwarder == nil {
		rl.cfg.Forwarder = &clusterForwarder{rl: rl, client: rl.client}
	}
	prev := svc.OnRemoteEnded
	svc.OnRemoteEnded = func(ended []domain.RemoteSession) {
		if prev != nil {
			prev(ended)
		}
		rl.sessionsEnded(ended)
	}
	return rl
}

// Registry is the relay's session registry.
func (rl *Relay) Registry() Registry { return rl.reg }

// sessionsEnded closes the streams of sessions that just ended: here, and on
// the replica holding each one when that's another.
func (rl *Relay) sessionsEnded(ended []domain.RemoteSession) {
	for i := range ended {
		reason := domain.RemoteEndHarnessEnded
		if ended[i].EndReason != nil {
			reason = *ended[i].EndReason
		}
		rl.endHere(ended[i].ID, reason)
	}
	if rl.cfg.Cluster == nil {
		return
	}
	for node, notes := range relayctl.Notes(ended, rl.cfg.Node) {
		rl.tellEnded(node, notes)
	}
}

// endHere closes the streams of a session held here, if it is.
func (rl *Relay) endHere(id, reason string) {
	live := rl.reg.Remove(id)
	if live == nil {
		return
	}
	live.end(reason)
	slog.Info("remote session ended", "session", id, "reason", reason)
}

// Run sweeps presence and buffers, and ends sessions past their idle limit
// or 12 h lifetime (the harness is authoritative; this is the backstop).
func (rl *Relay) Run(ctx context.Context) {
	sweep := time.NewTicker(rl.cfg.Sweep)
	defer sweep.Stop()
	expire := time.NewTicker(rl.cfg.Expire)
	defer expire.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			rl.sweep()
		case <-expire.C:
			rl.ExpireNow(ctx)
		}
	}
}

// ExpireNow runs the idle/lifetime sweep once.
func (rl *Relay) ExpireNow(ctx context.Context) {
	if _, err := rl.svc.ExpireRemoteSessions(ctx, rl.cfg.Now()); err != nil && ctx.Err() == nil {
		slog.Warn("remote session expiry sweep failed", "err", err)
	}
}

func (rl *Relay) sweep() {
	rl.reconcile()
	now := rl.cfg.Now()
	for _, l := range rl.reg.All() {
		l.mu.Lock()
		l.updatePresence(now, rl.cfg.OfflineAfter)
		l.evict(now)
		l.mu.Unlock()
	}
}

// ---- connect tokens ----

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// mintToken issues a new connect token for sess (protocol 8.2) and returns it
// with its expiry. Only its hash is stored.
func (rl *Relay) mintToken(ctx context.Context, sess *domain.RemoteSession) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	tok := "rct_" + b64(b)
	exp := rl.cfg.Now().Add(rl.cfg.TokenTTL)
	if err := rl.repo.SetRemoteConnectToken(ctx, sess.ID, hashToken(tok), exp); err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// ---- HTTP helpers ----

// apiError is an error response: {"error", "code"} plus extras (protocol 8.1).
type apiError struct {
	status int
	code   string
	msg    string
	extra  map[string]any
}

func (e *apiError) Error() string { return e.msg }

func errf(status int, code, format string, a ...any) *apiError {
	return &apiError{status: status, code: code, msg: fmt.Sprintf(format, a...)}
}

func (e *apiError) with(k string, v any) *apiError {
	if e.extra == nil {
		e.extra = map[string]any{}
	}
	e.extra[k] = v
	return e
}

// writeErr writes err: an *apiError, an authz error (status and code), or
// anything else as a 500 without its text.
func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	switch e := err.(type) {
	case *apiError:
		ae = e
	default:
		status := authz.StatusOf(err, 500)
		code := authz.CodeOf(err)
		msg := err.Error()
		if status == 500 {
			slog.Error("remote control request failed", "err", err)
			msg, code = "internal error", "internal_error"
		}
		ae = &apiError{status: status, code: code, msg: msg}
	}
	body := map[string]any{"error": ae.msg}
	if ae.code != "" {
		body["code"] = ae.code
	}
	for k, v := range ae.extra {
		body[k] = v
	}
	if ae.status == http.StatusTooManyRequests {
		s, _ := body["retry_after_s"].(int)
		if s < 1 {
			s = 1
			body["retry_after_s"] = s
		}
		w.Header().Set("Retry-After", strconv.Itoa(s))
	}
	writeJSON(w, ae.status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func retryAfter(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// requireJSON refuses a POST body that isn't JSON (415).
func requireJSON(r *http.Request) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errf(415, "unsupported_media_type", "send application/json")
	}
	return nil
}

// readBody reads at most max bytes; a larger body is 413 too_large.
func readBody(w http.ResponseWriter, r *http.Request, max int) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(max)))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errf(413, CodeTooLarge, "the body is over %d bytes", max)
		}
		return nil, errf(400, CodeMalformed, "couldn't read the body")
	}
	return b, nil
}

// sse is an open text/event-stream response.
type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter) *sse {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &sse{w: w, rc: http.NewResponseController(w)}
	_ = s.rc.Flush()
	return s
}

// write sends one frame. data must be a single line (compact JSON).
func (s *sse) write(f frame) error {
	var b strings.Builder
	if f.id > 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatInt(f.id, 10))
		b.WriteByte('\n')
	}
	if f.event != "" {
		b.WriteString("event: ")
		b.WriteString(f.event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(f.data)
	b.WriteString("\n\n")
	if _, err := s.w.Write([]byte(b.String())); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sse) raw(text string) error {
	if _, err := s.w.Write([]byte(text)); err != nil {
		return err
	}
	return s.rc.Flush()
}

// pump writes the stream's frames and a keepalive comment every interval
// until the stream closes or the request goes away.
func (rl *Relay) pump(ctx context.Context, out *sse, st *stream) {
	ka := time.NewTicker(rl.cfg.Keepalive)
	defer ka.Stop()
	for {
		select {
		case f := <-st.ch:
			if out.write(f) != nil {
				return
			}
		case <-ka.C:
			if out.raw(": ka\n\n") != nil {
				return
			}
		case <-st.done:
			// Flush what was queued before the close (an end frame).
			for {
				select {
				case f := <-st.ch:
					if out.write(f) != nil {
						return
					}
				default:
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func ms(t time.Time) int64 { return t.UnixMilli() }
