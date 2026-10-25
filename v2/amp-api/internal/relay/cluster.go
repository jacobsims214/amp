package relay

// Multi-replica relay (#2377). A session's live state (the harness's command
// stream, the viewers, the resync buffer, the pairing window and the rate
// buckets) lives on exactly one replica: the holder, remote_sessions.holder_node.
// Every request for a session, on either leg, is served there:
//
//   - A replica that gets a request for a session it doesn't hold proxies the
//     whole HTTP exchange (SSE streams included) to the holder's internal
//     listener (CLUSTER_ADDR, :3003) under /internal/relay/..., with the
//     cluster token. The browser leg's verified caller travels in a header the
//     holder trusts only behind that token; the harness leg's connect token is
//     simply re-checked on the holder.
//   - Side effects that start elsewhere reach the holder by control calls on
//     the same listener: a session ending (any replica can end one: the org
//     switch, revoke-all, the expiry sweep), revoke-all's device_revoked, and
//     the presence shown in GET /api/sessions.
//
// The holder is chosen the way project owners are (internal/cluster): the
// replica that registers a session holds it; a replica takes a session over
// only when the holder's api_nodes heartbeat is older than the cluster TTL
// (it died, or left on shutdown). After a takeover the session starts empty
// and offline on the new holder until the harness reconnects there (it
// reconnects to whichever replica the load balancer picks, which forwards).
//
// No payload ever goes through Postgres (no NOTIFY): the database only says
// who holds what.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/relayctl"
)

// Cluster is this replica's membership (a *cluster.Node).
type Cluster interface {
	// ID is this replica's api_nodes id.
	ID() string
	// Token authenticates calls between replicas.
	Token() string
	// TTL is how long after its last heartbeat a replica counts as gone.
	TTL() time.Duration
}

const (
	internalPrefix  = relayctl.InternalPrefix
	tokenHeader     = relayctl.TokenHeader
	principalHeader = "X-AMP-Relay-Principal"
	notHolderHeader = "X-AMP-Relay-Not-Holder"
	ctlTimeout      = relayctl.CallTimeout
	dialTimeout     = 3 * time.Second
	// maxForwardBody is read before forwarding (so a refused hop can be
	// retried); one byte over the largest limit, so the holder still answers
	// 413 for anything bigger.
	maxForwardBody = MaxBatchBytes + 1
)

type ctxKey int

const (
	forwardedKey ctxKey = iota // the request came from another replica
	principalKey               // the browser caller verified by that replica
)

func forwarded(ctx context.Context) bool { v, _ := ctx.Value(forwardedKey).(bool); return v }

// principalOf is the browser caller: the one the forwarding replica verified,
// else this request's own.
func principalOf(r *http.Request) (authz.Principal, error) {
	if p, ok := r.Context().Value(principalKey).(authz.Principal); ok {
		return p, nil
	}
	return authz.FromContext(r.Context())
}

// dialError marks a failure to connect: the request certainly wasn't
// delivered, so it may be sent again (to the same or a new holder).
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

func newInternalClient() *http.Client {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, &dialError{err}
			}
			return c, nil
		},
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}}
}

// holder returns "" when this replica serves session id (it holds it, takes
// it over from a replica that's gone, or the session has ended and the
// handlers here answer 410), else the holder's internal address.
func (rl *Relay) holder(ctx context.Context, id string) (string, error) {
	if rl.cfg.Cluster == nil {
		return "", nil
	}
	ttl := rl.cfg.Cluster.TTL()
	for i := 0; i < 3; i++ {
		h, err := rl.repo.RemoteSessionHolder(ctx, id, ttl)
		if err != nil {
			return "", err
		}
		if !h.Active || h.NodeID == rl.cfg.Node {
			return "", nil
		}
		if h.NodeID != "" && h.Alive && h.Addr != "" {
			return h.Addr, nil
		}
		took, err := rl.repo.TakeRemoteSession(ctx, id, rl.cfg.Node, ttl)
		if err != nil {
			return "", err
		}
		if took {
			slog.Info("remote session taken over", "session", id, "from", h.NodeID)
			return "", nil
		}
	}
	return "", errf(503, "relay_unavailable", "couldn't find the replica holding this session; try again")
}

// clusterForwarder is the Forwarder in cluster mode: it serves a session's
// requests on its holder.
type clusterForwarder struct {
	rl     *Relay
	client *http.Client
}

// Forward proxies r to the session's holder and reports true, or reports
// false when this replica serves it. On a request that already came from
// another replica it never forwards again: if this replica no longer holds
// the session it answers 409 not_holder, and the sender looks again.
func (f *clusterForwarder) Forward(w http.ResponseWriter, r *http.Request, sess *domain.RemoteSession) bool {
	rl := f.rl
	ctx := r.Context()
	if forwarded(ctx) {
		addr, err := rl.holder(ctx, sess.ID)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if addr != "" {
			w.Header().Set(notHolderHeader, "1")
			writeErr(w, errf(409, "not_holder", "this replica doesn't hold the session"))
			return true
		}
		return false
	}
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxForwardBody))
		if err != nil {
			writeErr(w, errf(400, CodeMalformed, "couldn't read the body"))
			return true
		}
		body = b
	}
	start := time.Now()
	backoff := 25 * time.Millisecond
	for {
		addr, err := rl.holder(ctx, sess.ID)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if addr == "" {
			r.Body = io.NopCloser(bytes.NewReader(body))
			return false
		}
		resp, err := f.send(r, addr, body)
		if err == nil && !(resp.StatusCode == http.StatusConflict && resp.Header.Get(notHolderHeader) != "") {
			copyResponse(w, resp)
			return true
		}
		if resp != nil {
			resp.Body.Close()
		}
		var de *dialError
		if err != nil && !errors.As(err, &de) {
			// It may have been delivered: never send it twice.
			if ctx.Err() == nil {
				slog.Warn("forwarding a remote session request failed", "session", sess.ID, "holder", addr, "err", err)
				writeErr(w, errf(502, "relay_unavailable", "the replica holding this session didn't answer; try again"))
			}
			return true
		}
		if time.Since(start) > rl.cfg.RouteTimeout {
			writeErr(w, errf(503, "relay_unavailable", "the replica holding this session is unreachable; try again").with("retry_after_s", 3))
			return true
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > time.Second {
			backoff = time.Second
		}
	}
}

// hopHeaders aren't passed through a proxy.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer",
	"Transfer-Encoding", "Upgrade"}

// send passes r (with body) to the holder at addr.
func (f *clusterForwarder) send(r *http.Request, addr string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://"+addr+internalPrefix+r.URL.RequestURI(), rd)
	if err != nil {
		return nil, err
	}
	req.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	req.Header.Del(principalHeader)
	req.Header.Set(tokenHeader, f.rl.cfg.Cluster.Token())
	if p, err := authz.FromContext(r.Context()); err == nil {
		raw, _ := json.Marshal(p)
		req.Header.Set(principalHeader, base64.RawURLEncoding.EncodeToString(raw))
	}
	req.Host = r.Host // the Origin check on the holder compares with it
	return f.client.Do(req)
}

// copyResponse writes the holder's response, flushing as it arrives (SSE).
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// ---- the internal listener ----

// InternalHandler serves /internal/relay/ on the internal listener: requests
// forwarded from other replicas, and their control calls. Every call needs
// the cluster token.
func (rl *Relay) InternalHandler() http.Handler {
	ctl := http.NewServeMux()
	ctl.HandleFunc("POST "+internalPrefix+"/ctl/end", rl.ctlEnd)
	ctl.HandleFunc("POST "+internalPrefix+"/ctl/device-revoked", rl.ctlDeviceRevoked)
	ctl.HandleFunc("GET "+internalPrefix+"/ctl/presence", rl.ctlPresence)
	legs := http.NewServeMux()
	legs.Handle("/relay/v1/", rl.HarnessHandler())
	b := rl.browserMux()
	legs.Handle("/api/sessions", b)
	legs.Handle("/api/sessions/", b)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rl.cfg.Cluster == nil ||
			subtle.ConstantTimeCompare([]byte(r.Header.Get(tokenHeader)), []byte(rl.cfg.Cluster.Token())) != 1 {
			writeErr(w, errf(401, "unauthenticated", "replica token required"))
			return
		}
		if strings.HasPrefix(r.URL.Path, internalPrefix+"/ctl/") {
			ctl.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), forwardedKey, true)
		if h := r.Header.Get(principalHeader); h != "" {
			var p authz.Principal
			raw, err := base64.RawURLEncoding.DecodeString(h)
			if err != nil || json.Unmarshal(raw, &p) != nil {
				writeErr(w, errf(400, CodeMalformed, "bad forwarded caller"))
				return
			}
			ctx = context.WithValue(ctx, principalKey, p)
		}
		r2 := r.WithContext(ctx)
		u := *r.URL
		u.Path, u.RawPath = strings.TrimPrefix(u.Path, internalPrefix), ""
		r2.URL = &u
		r2.RequestURI = u.RequestURI()
		legs.ServeHTTP(w, r2)
	})
}

type endNote = relayctl.EndNote

// ctlEnd closes the streams of sessions another replica ended.
func (rl *Relay) ctlEnd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sessions []endNote `json:"sessions"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, errf(400, CodeMalformed, "bad body"))
		return
	}
	for _, n := range req.Sessions {
		rl.endHere(n.ID, n.Reason)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ctlDeviceRevoked tells the session's harness that every device is revoked.
func (rl *Relay) ctlDeviceRevoked(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, errf(400, CodeMalformed, "bad body"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"delivered": rl.deviceRevokedHere(req.ID)})
}

// ctlPresence reports a held session's presence and viewer count.
func (rl *Relay) ctlPresence(w http.ResponseWriter, r *http.Request) {
	online, viewers := rl.presenceHere(r.URL.Query().Get("id"))
	writeJSON(w, http.StatusOK, map[string]any{"online": online, "viewers": viewers})
}

// ctl makes a control call to the replica at addr; out, if set, receives the
// JSON reply.
func (rl *Relay) ctl(ctx context.Context, addr, method, path string, in, out any) error {
	return relayctl.Call(ctx, rl.client, rl.cfg.Cluster.Token(), addr, method, path, in, out)
}

// ---- side effects that reach the holder ----

// tellEnded tells the replica node (holding these sessions) that they ended.
// A replica that's gone holds nothing; a failed call is caught by the
// holder's own sweep, which checks its sessions against the database.
func (rl *Relay) tellEnded(node string, notes []endNote) {
	relayctl.TellEnded(context.Background(), rl.repo, rl.client, rl.cfg.Cluster.Token(), rl.cfg.Cluster.TTL(), node, notes)
}

// deviceRevoked sends device_revoked {all} to the session's harness, on
// whichever replica holds it.
func (rl *Relay) deviceRevoked(ctx context.Context, sess *domain.RemoteSession) {
	addr, err := rl.holder(ctx, sess.ID)
	if err != nil {
		slog.Warn("finding a remote session's holder failed", "session", sess.ID, "err", err)
		return
	}
	if addr == "" {
		rl.deviceRevokedHere(sess.ID)
		return
	}
	if err := rl.ctl(ctx, addr, http.MethodPost, "device-revoked", map[string]string{"id": sess.ID}, nil); err != nil {
		slog.Warn("sending device_revoked to the holder failed", "session", sess.ID, "err", err)
	}
}

func (rl *Relay) deviceRevokedHere(id string) bool {
	live := rl.reg.Get(id)
	if live == nil {
		return false
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	return live.harness != nil && live.harness.send(frame{event: "device_revoked", data: []byte(`{"all":true}`)})
}

// presence is the session's harness presence and viewer count, from its holder.
func (rl *Relay) presence(ctx context.Context, sess *domain.RemoteSession) (bool, int) {
	if rl.cfg.Cluster != nil && !forwarded(ctx) {
		addr, err := rl.holder(ctx, sess.ID)
		if err != nil {
			return false, 0
		}
		if addr != "" {
			var out struct {
				Online  bool `json:"online"`
				Viewers int  `json:"viewers"`
			}
			if err := rl.ctl(ctx, addr, http.MethodGet, "presence?id="+url.QueryEscape(sess.ID), nil, &out); err != nil {
				return false, 0
			}
			return out.Online, out.Viewers
		}
	}
	return rl.presenceHere(sess.ID)
}

func (rl *Relay) presenceHere(id string) (bool, int) {
	live := rl.reg.Get(id)
	if live == nil {
		return false, 0
	}
	return live.Online(rl.cfg.Now(), rl.cfg.OfflineAfter), live.Viewers()
}

// reconcile checks the sessions held here against the database: one that
// ended elsewhere closes (should the holder have missed the control call),
// and one another replica took over (this one was judged gone) is released
// without an end, so its harness and viewers reconnect to the new holder.
func (rl *Relay) reconcile() {
	lives := rl.reg.All()
	if len(lives) == 0 {
		return
	}
	ids := make([]string, len(lives))
	for i, l := range lives {
		ids[i] = l.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := rl.repo.RemoteSessionsByID(ctx, ids)
	if err != nil {
		return
	}
	byID := make(map[string]*domain.RemoteSession, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	for _, l := range lives {
		s := byID[l.ID]
		switch {
		case s == nil:
			rl.endHere(l.ID, domain.RemoteEndHarnessEnded)
		case !s.Active():
			reason := domain.RemoteEndHarnessEnded
			if s.EndReason != nil {
				reason = *s.EndReason
			}
			rl.endHere(l.ID, reason)
		case rl.cfg.Cluster != nil && s.HolderNode != nil && *s.HolderNode != rl.cfg.Node:
			if live := rl.reg.Remove(l.ID); live != nil {
				live.release()
				slog.Info("remote session released to another replica", "session", l.ID, "holder", *s.HolderNode)
			}
		}
	}
}
