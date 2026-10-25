package main

// The remote control relay (epic 165, AMP #2374): both legs over HTTP, with a
// harness and a browser played by the test (real P-256 keys, signed
// envelopes), against the real database and service layer.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/cluster"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/relay"
	"github.com/simstech/amp-api/internal/relayserver"
	"github.com/simstech/amp-api/internal/service"
)

const relayOrigin = "https://amp.test"

// testClock is the relay's clock, which tests can move forward.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

type relayEnv struct {
	w     *rbacWorld
	rl    *relay.Relay
	svc   *service.Service // amp-relay's service
	srv   *httptest.Server // "Envoy": the relay paths to amp-relay, the rest to amp-api
	clock *testClock
	ids   map[string]*auth.Identity
	// node and internal are this amp-relay replica's cluster membership and
	// internal listener; stop ends its heartbeats and sweeps.
	node     *cluster.Node
	internal *httptest.Server
	stop     context.CancelFunc
}

// newRelayEnv runs one amp-relay replica (relayserver, exactly what
// cmd/relay serves) and amp-api's REST routes (apiMux, as cmd/server serves
// them) behind one test server that splits paths the way Envoy does: /relay/
// and /api/sessions to the relay, the rest to amp-api. Test identities stand
// in for the verifiers: X-Test-As names the user, X-Test-Channel overrides
// how the request came (default: the web UI on the browser leg and amp-api,
// an MCP-audience OAuth token on the harness leg).
func newRelayEnv(t *testing.T, tune func(*relay.Config)) *relayEnv {
	t.Helper()
	w := newRBACWorld(t)
	enableOrg(t, w.s.repo, w.org.ID, true)
	return newRelayEnvIn(t, w, &testClock{at: time.Now()}, tune)
}

var relayEnvSeq atomic.Int64

// newRelayEnvIn is newRelayEnv on an existing world and clock: several
// relay replicas can share them.
func newRelayEnvIn(t *testing.T, w *rbacWorld, clock *testClock, tune func(*relay.Config)) *relayEnv {
	t.Helper()
	agent := *w.alice
	agentUser := *w.alice.User
	agentUser.Kind = domain.UserKindAgent
	agent.User = &agentUser
	e := &relayEnv{w: w, clock: clock, ids: map[string]*auth.Identity{
		"alice": w.alice, "bob": w.bob, "owner": w.owner, "outsider": w.outsider, "root": w.root, "agent": &agent,
		"orgadmin": w.orgadmin,
	}}
	identify := func(listener, defChannel string, next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			who := r.Header.Get("X-Test-As")
			if who == "" {
				// The live Rust harness (relay_live_test.go) sends its
				// OAuth token as "Bearer test-oauth-<user>".
				who, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer test-oauth-")
			}
			ident, ok := e.ids[who]
			if !ok {
				rw.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(rw, `{"error":"missing bearer token"}`)
				return
			}
			c := *ident
			c.Listener, c.Channel = listener, defChannel
			if ch := r.Header.Get("X-Test-Channel"); ch != "" {
				c.Channel = ch
				c.APIToken = ch == auth.ChannelToken
			}
			next.ServeHTTP(rw, r.WithContext(auth.WithIdentity(r.Context(), &c)))
		})
	}

	// amp-relay always joins the replica group (cmd/relay): its replicas find
	// each other there, and amp-api finds the one holding a session.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts := fastCluster
	opts.ID = fmt.Sprintf("relay-%s-%d-%d", t.Name(), relayEnvSeq.Add(1), time.Now().UnixNano())
	opts.Addr = ln.Addr().String()
	node, err := cluster.Join(context.Background(), w.s.repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go node.Run(ctx)
	e.node, e.stop = node, cancel

	// amp-relay.
	e.svc = service.New(w.s.repo, nil, nil)
	e.svc.Remote, e.svc.AppURL = remoteOn, relayOrigin
	cfg := relay.Config{AppURL: relayOrigin, Now: e.clock.Now, Cluster: node}
	if tune != nil {
		tune(&cfg)
	}
	rs := relayserver.New(e.svc, relayserver.Options{
		Relay:       cfg,
		HarnessAuth: func(next http.Handler) http.Handler { return identify("mcp", auth.ChannelMCP, next) },
		BrowserAuth: func(next http.Handler) http.Handler { return identify("rest", auth.ChannelUI, next) },
	})
	if rs.Relay == nil {
		t.Fatal("relay not set up")
	}
	e.rl = rs.Relay
	e.internal = httptest.NewUnstartedServer(rs.InternalHandler())
	e.internal.Listener.Close()
	e.internal.Listener = ln
	e.internal.Start()

	// amp-api: its REST routes, offering remote control, telling the relay
	// about the sessions it ends.
	h := api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb)
	h.SetRemoteControl(remoteOn)
	hookRelayEnds(h.Service(), w.s.repo, node)
	ampAPI := identify("rest", auth.ChannelUI, api.RejectCrossSiteWrites(apiMux(h)))

	relaySrv := rs.Handler()
	e.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if relayserver.IsRelayPath(r.URL.Path) {
			relaySrv.ServeHTTP(rw, r)
			return
		}
		ampAPI.ServeHTTP(rw, r)
	}))
	t.Cleanup(func() {
		cancel()
		e.srv.CloseClientConnections()
		e.srv.Close()
		e.internal.CloseClientConnections()
		e.internal.Close()
		_ = node.Leave(context.Background())
	})
	return e
}

// ---- requests ----

type reqOpt func(*http.Request)

func as(who string) reqOpt { return func(r *http.Request) { r.Header.Set("X-Test-As", who) } }
func via(channel string) reqOpt {
	return func(r *http.Request) { r.Header.Set("X-Test-Channel", channel) }
}
func bearer(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}
func header(k, v string) reqOpt   { return func(r *http.Request) { r.Header.Set(k, v) } }
func noHeader(k string) reqOpt    { return func(r *http.Request) { r.Header.Del(k) } }
func fromOrigin(o string) reqOpt  { return header("Origin", o) }
func contentType(c string) reqOpt { return header("Content-Type", c) }

// do sends a request; a non-nil body is JSON ([]byte is sent as is). Browser
// POSTs carry the AMP origin unless an option changes it.
func (e *relayEnv) do(t *testing.T, method, path string, body any, opts ...reqOpt) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.HasPrefix(path, "/api/") && method != http.MethodGet {
		req.Header.Set("Origin", relayOrigin)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

func wantStatus(t *testing.T, what string, got int, body map[string]any, status int, code string) {
	t.Helper()
	if got != status || (code != "" && body["code"] != code) {
		t.Fatalf("%s: got %d %v, want %d %s", what, got, body, status, code)
	}
}

// ---- SSE ----

type sseFrame struct {
	ID, Event, Data string
	Comment         bool
}

type sseConn struct {
	frames chan sseFrame
	cancel context.CancelFunc
	status int
	body   map[string]any
}

// openSSE opens a stream; on a non-200 status it returns the error body.
func (e *relayEnv) openSSE(t *testing.T, path string, opts ...reqOpt) *sseConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+path, nil)
	for _, o := range opts {
		o(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &sseConn{frames: make(chan sseFrame, 1024), cancel: cancel, status: resp.StatusCode}
	t.Cleanup(c.close)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		_ = json.Unmarshal(raw, &c.body)
		close(c.frames)
		return c
	}
	go func() {
		defer close(c.frames)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f.Event != "" || f.Data != "" {
					c.frames <- f
				}
				f = sseFrame{}
			case strings.HasPrefix(line, ":"):
				c.frames <- sseFrame{Comment: true, Data: strings.TrimSpace(line[1:])}
			case strings.HasPrefix(line, "id: "):
				f.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				f.Event = line[7:]
			case strings.HasPrefix(line, "data: "):
				f.Data = line[6:]
			}
		}
	}()
	return c
}

func (c *sseConn) close() { c.cancel() }

// next returns the next frame that isn't a keepalive.
func (c *sseConn) next(t *testing.T) sseFrame {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				t.Fatal("stream closed")
			}
			if !f.Comment {
				return f
			}
		case <-timeout:
			t.Fatal("no frame within 5s")
		}
	}
}

// expect returns the next frame, which must be event.
func (c *sseConn) expect(t *testing.T, event string) sseFrame {
	t.Helper()
	f := c.next(t)
	if f.Event != event {
		t.Fatalf("got frame %s %s, want %s", f.Event, f.Data, event)
	}
	return f
}

// waitFor skips frames until one is event (with data containing sub).
func (c *sseConn) waitFor(t *testing.T, event, sub string) sseFrame {
	t.Helper()
	for {
		f := c.next(t)
		if f.Event == event && strings.Contains(f.Data, sub) {
			return f
		}
	}
}

// closed waits for the stream to end.
func (c *sseConn) closed(t *testing.T) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.frames:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("stream still open after 5s")
		}
	}
}

func dataOf(t *testing.T, f sseFrame) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(f.Data), &m); err != nil {
		t.Fatalf("frame data %q: %v", f.Data, err)
	}
	return m
}

// ---- the test harness and device ----

type rcKey struct {
	priv *ecdsa.PrivateKey
	pub  string // b64u raw
	id   string // fingerprint
}

func newRCKey(t *testing.T) rcKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := priv.PublicKey.Bytes()
	return rcKey{priv: priv, pub: base64.RawURLEncoding.EncodeToString(raw), id: relay.Fingerprint(raw)}
}

func rcNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func sign(t *testing.T, k rcKey, env map[string]any) []byte {
	t.Helper()
	b, err := relay.SignEnvelope(k.priv, env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rcSession is a registered session, from the harness's side.
type rcSession struct {
	e       *relayEnv
	who     string
	harness rcKey
	id      string
	token   string
	seq     int64
}

// register turns remote control on for who's harness in the world's project.
func (e *relayEnv) register(t *testing.T, who string) *rcSession {
	t.Helper()
	k := newRCKey(t)
	st, body, _ := e.do(t, "POST", "/relay/v1/sessions", e.registration(k), as(who))
	wantStatus(t, "register", st, body, 201, "")
	return &rcSession{e: e, who: who, harness: k, id: body["session_id"].(string), token: body["connect_token"].(string)}
}

// tokenProof is a refresh body signed by k for session id (protocol 8.2, #2390).
func (e *relayEnv) tokenProof(t *testing.T, k rcKey, id string) []byte {
	t.Helper()
	b, err := relay.SignTokenProof(k.priv, id, k.id, e.clock.Now().UnixMilli(), rcNonce())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *relayEnv) registration(k rcKey) map[string]any {
	return map[string]any{
		"protocol_versions": []int{1}, "harness_id": k.id, "harness_pub": k.pub,
		"host": "jacobs-mbp", "os": "macos", "harness_version": "0.4.0", "project_id": e.w.s.projectID,
	}
}

// event signs a harness event with the next seq.
func (s *rcSession) event(t *testing.T, op string, deviceID any, body map[string]any) json.RawMessage {
	t.Helper()
	s.seq++
	return sign(t, s.harness, map[string]any{
		"v": 1, "alg": "ES256", "session_id": s.id, "harness_id": s.harness.id, "device_id": deviceID,
		"seq": s.seq, "ts": s.e.clock.Now().UnixMilli(), "nonce": rcNonce(), "op": op, "body": body,
	})
}

func (s *rcSession) hello(t *testing.T) json.RawMessage {
	return s.event(t, "hello", nil, map[string]any{
		"harness": map[string]any{"harness_id": s.harness.id, "harness_pub": s.harness.pub, "host": "jacobs-mbp", "os": "macos", "version": "0.4.0"},
		"user_id": 1, "ops": []any{"prompt", "stop", "detach"}, "started_at_ms": 1, "expires_at_ms": 2, "idle_timeout_s": 1800,
		"status": map[string]any{"busy": false, "devices": []any{}},
	})
}

func (s *rcSession) publish(t *testing.T, events ...json.RawMessage) (int, map[string]any) {
	t.Helper()
	st, body, _ := s.e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/events", map[string]any{"events": events}, bearer(s.token))
	return st, body
}

func (s *rcSession) mustPublish(t *testing.T, events ...json.RawMessage) {
	t.Helper()
	if st, body := s.publish(t, events...); st != 200 {
		t.Fatalf("publish: %d %v", st, body)
	}
}

// connect opens the harness's command stream and reads the first viewers frame.
func (s *rcSession) connect(t *testing.T) *sseConn {
	t.Helper()
	c := s.e.openSSE(t, "/relay/v1/sessions/"+s.id+"/commands", bearer(s.token))
	if c.status != 200 {
		t.Fatalf("command stream: %d %v", c.status, c.body)
	}
	c.expect(t, "viewers")
	return c
}

// watch opens a browser event stream as the session's owner.
func (s *rcSession) watch(t *testing.T, query string, opts ...reqOpt) *sseConn {
	t.Helper()
	return s.e.openSSE(t, "/api/sessions/"+s.id+"/events"+query, append([]reqOpt{as(s.who)}, opts...)...)
}

// pin records device as paired with the session's harness (as a signed
// pair_result would), trusted until.
func (s *rcSession) pin(t *testing.T, dev rcKey, until time.Time) {
	t.Helper()
	sess, err := s.e.w.s.repo.GetRemoteSession(context.Background(), s.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.e.w.s.repo.UpsertRemoteDevice(context.Background(), domain.RemoteDevice{
		OwnerUserID: sess.OwnerUserID, HarnessID: s.harness.id, DeviceID: dev.id, DevicePub: dev.pub, Name: "Test browser", ExpiresAt: until,
	}); err != nil {
		t.Fatal(err)
	}
}

// command signs a device command for the session.
func (s *rcSession) command(t *testing.T, dev rcKey, seq int64, op string, body map[string]any) []byte {
	t.Helper()
	return sign(t, dev, map[string]any{
		"v": 1, "alg": "ES256", "session_id": s.id, "harness_id": s.harness.id, "device_id": dev.id,
		"seq": seq, "ts": s.e.clock.Now().UnixMilli(), "nonce": rcNonce(), "op": op, "body": body,
	})
}

func (s *rcSession) send(t *testing.T, cmd []byte, opts ...reqOpt) (int, map[string]any, http.Header) {
	t.Helper()
	return s.e.do(t, "POST", "/api/sessions/"+s.id+"/commands", cmd, append([]reqOpt{as(s.who)}, opts...)...)
}

func prompt(text string) map[string]any { return map[string]any{"text": text, "client_msg_id": "m-1"} }

// ---- tests ----

// The harness leg: who may register, the connect token, and the 404 when the
// relay isn't enabled.
func TestRelay_HarnessLegAuth(t *testing.T) {
	e := newRelayEnv(t, nil)
	k := newRCKey(t)
	reg := e.registration(k)

	st, body, _ := e.do(t, "POST", "/relay/v1/sessions", reg)
	if st != 401 {
		t.Fatalf("no credential: %d %v", st, body)
	}
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions", reg, as("alice"), via(auth.ChannelToken))
	wantStatus(t, "amp_ API token", st, body, 403, "token_not_allowed")
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions", reg, as("agent"))
	wantStatus(t, "agent account", st, body, 403, "agent_not_allowed")
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions", reg, as("outsider"))
	wantStatus(t, "project not visible", st, body, 404, "not_found")
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions", []byte("x=1"), as("alice"), contentType("text/plain"))
	wantStatus(t, "not JSON", st, body, 415, "unsupported_media_type")
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions", []byte(`{"pad":"`+strings.Repeat("x", 9000)+`"}`), as("alice"))
	wantStatus(t, "over 8 KiB", st, body, 413, "too_large")
	bad := e.registration(k)
	bad["protocol_versions"] = []int{2}
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions", bad, as("alice"))
	wantStatus(t, "no common version", st, body, 400, "unsupported_version")
	if v, _ := body["supported_versions"].([]any); len(v) != 1 || v[0] != float64(1) {
		t.Fatalf("supported_versions: %v", body)
	}
	bad = e.registration(k)
	bad["harness_id"] = newRCKey(t).id
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions", bad, as("alice"))
	wantStatus(t, "fp(harness_pub) != harness_id", st, body, 400, "malformed")

	// The org switch.
	enableOrg(t, e.w.s.repo, e.w.org.ID, false)
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions", reg, as("alice"))
	wantStatus(t, "org off", st, body, 403, "remote_disabled")
	enableOrg(t, e.w.s.repo, e.w.org.ID, true)

	st, body, _ = e.do(t, "POST", "/relay/v1/sessions", reg, as("alice"))
	wantStatus(t, "register", st, body, 201, "")
	for _, k := range []string{"session_id", "connect_token", "connect_token_expires_at_ms", "expires_at_ms", "session_url", "limits"} {
		if body[k] == nil {
			t.Fatalf("register response lacks %s: %v", k, body)
		}
	}
	id, tok := body["session_id"].(string), body["connect_token"].(string)
	if !strings.HasPrefix(tok, "rct_") || body["session_url"] != relayOrigin+"/sessions/"+id ||
		body["idle_timeout_s"] != float64(1800) || body["keepalive_interval_s"] != float64(20) {
		t.Fatalf("register response: %v", body)
	}

	// The connect token: required, bound to its session, expiring.
	hb := "/relay/v1/sessions/" + id + "/heartbeat"
	st, body, _ = e.do(t, "POST", hb, nil)
	wantStatus(t, "no connect token", st, body, 401, "unauthenticated")
	st, body, _ = e.do(t, "POST", hb, nil, bearer("rct_nope"))
	wantStatus(t, "unknown connect token", st, body, 401, "unauthenticated")
	other := e.register(t, "bob")
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+other.id+"/heartbeat", nil, bearer(tok))
	wantStatus(t, "another session's token", st, body, 401, "unauthenticated")
	st, body, _ = e.do(t, "POST", hb, nil, bearer(tok))
	wantStatus(t, "heartbeat", st, body, 200, "")
	if body["viewers"] != float64(0) || body["idle_deadline_ms"] == nil {
		t.Fatalf("heartbeat: %v", body)
	}
	// The OAuth leg of the token: only the owner refreshes, after the policy check.
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+id+"/token", nil, as("bob"))
	wantStatus(t, "refresh someone else's session", st, body, 404, "not_found")
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+id+"/token", nil, as("alice"), via(auth.ChannelToken))
	wantStatus(t, "refresh with an API token", st, body, 403, "token_not_allowed")
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+id+"/token", e.tokenProof(t, k, id), as("alice"))
	wantStatus(t, "refresh", st, body, 200, "")
	fresh := body["connect_token"].(string)
	st, body, _ = e.do(t, "POST", hb, nil, bearer(tok))
	wantStatus(t, "the replaced token", st, body, 401, "unauthenticated")
	e.clock.Add(16 * time.Minute)
	st, body, _ = e.do(t, "POST", hb, nil, bearer(fresh))
	wantStatus(t, "expired token", st, body, 401, "token_expired")
	e.clock.Add(-16 * time.Minute)

	// Ended: 410 with the reason.
	st, body, _ = e.do(t, "DELETE", "/relay/v1/sessions/"+id, map[string]any{"reason": "detached"}, bearer(fresh))
	if st != 204 {
		t.Fatalf("DELETE: %d %v", st, body)
	}
	st, body, _ = e.do(t, "POST", hb, nil, bearer(fresh))
	wantStatus(t, "after the end", st, body, 410, "session_ended")
	if body["reason"] != "detached" {
		t.Fatalf("410 reason: %v", body)
	}
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+id+"/token", nil, as("alice"))
	wantStatus(t, "refresh an ended session", st, body, 410, "session_ended")
	if body["reason"] != "detached" {
		t.Fatalf("refresh 410 reason: %v", body)
	}
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteDisable, id); len(a) != 1 || a[0].Details["reason"] != "detached" {
		t.Fatalf("remote.disable: %+v", a)
	}
}

// Registrations are limited to 10 a minute per user, and 5 live sessions.
func TestRelay_RegistrationLimits(t *testing.T) {
	e := newRelayEnv(t, nil)
	var live []*rcSession
	for i := 0; i < 5; i++ {
		live = append(live, e.register(t, "alice"))
	}
	st, body, _ := e.do(t, "POST", "/relay/v1/sessions", e.registration(newRCKey(t)), as("alice"))
	wantStatus(t, "6th live session", st, body, 429, "too_many_sessions")
	for _, s := range live {
		if st, _, _ := e.do(t, "DELETE", "/relay/v1/sessions/"+s.id, nil, bearer(s.token)); st != 204 {
			t.Fatalf("DELETE: %d", st)
		}
	}
	for i := 0; i < 3; i++ {
		s := e.register(t, "alice")
		e.do(t, "DELETE", "/relay/v1/sessions/"+s.id, nil, bearer(s.token))
	}
	// 6 + 3 = 9 attempts so far (the refused one counted); the 10th passes, the 11th doesn't.
	e.register(t, "alice")
	st, body, hdr := e.do(t, "POST", "/relay/v1/sessions", e.registration(newRCKey(t)), as("alice"))
	wantStatus(t, "11th registration in a minute", st, body, 429, "rate_limited")
	if hdr.Get("Retry-After") == "" || body["retry_after_s"] == nil {
		t.Fatalf("429 without Retry-After: %v %v", hdr, body)
	}
	// Someone else isn't affected.
	e.register(t, "bob")
}

// With the instance switch or auth off, amp-relay serves no relay route:
// plain 404s. (cmd/relay refuses to start without auth anyway.)
func TestRelay_InstanceSwitchOff(t *testing.T) {
	w := newRBACWorld(t)
	for _, cfg := range []service.RemoteConfig{{}, {InstanceEnabled: true}, {AuthEnabled: true}} {
		svc := service.New(w.s.repo, nil, nil)
		svc.Remote = cfg
		rs := relayserver.New(svc, relayserver.Options{})
		if rs.Relay != nil {
			t.Fatalf("%+v: relay set up", cfg)
		}
		// Never nil (#2390): an http.Server with a nil Handler serves
		// http.DefaultServeMux.
		ih := rs.InternalHandler()
		if ih == nil {
			t.Fatalf("%+v: nil internal handler", cfg)
		}
		rec := httptest.NewRecorder()
		ih.ServeHTTP(rec, httptest.NewRequest("GET", "/debug/pprof/", nil))
		if rec.Code != 404 {
			t.Fatalf("%+v: internal handler answered %d, want 404", cfg, rec.Code)
		}
		srv := httptest.NewServer(rs.Handler())
		for _, path := range []string{"/relay/v1/sessions", "/relay/v1/sessions/rs_x/commands", "/api/sessions", "/api/sessions/rs_x/events"} {
			resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 404 {
				t.Fatalf("%+v %s: %d, want 404", cfg, path, resp.StatusCode)
			}
		}
		srv.Close()
	}
}

// The browser leg: a person in the web UI, the owner only, same-origin only.
func TestRelay_BrowserLegAuthz(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	mine := "/api/sessions/" + s.id

	for _, ch := range []string{auth.ChannelToken, auth.ChannelMCP, auth.ChannelRESTOther} {
		st, body, _ := e.do(t, "GET", "/api/sessions", nil, as("alice"), via(ch))
		wantStatus(t, "list via "+ch, st, body, 403, "not_ui_channel")
		st, body, _ = e.do(t, "POST", mine+"/commands", []byte(`{}`), as("alice"), via(ch))
		wantStatus(t, "command via "+ch, st, body, 403, "not_ui_channel")
	}
	st, body, _ := e.do(t, "GET", mine, nil, as("agent"))
	wantStatus(t, "agent account", st, body, 403, "not_ui_channel")

	// Someone else gets the same 404 as a missing session: a teammate (bob),
	// the org owner, the project-owner team, a global admin and another
	// org's user (#2379). None of them sees it in their list either.
	for _, who := range []string{"bob", "orgadmin", "owner", "root", "outsider"} {
		st, body, _ := e.do(t, "GET", "/api/sessions", nil, as(who))
		if st != 200 || len(body["sessions"].([]any)) != 0 {
			t.Fatalf("%s's list: %d %v", who, st, body)
		}
		st, body, _ = e.do(t, "GET", mine, nil, as(who))
		wantStatus(t, who+" GET", st, body, 404, "not_found")
		c := e.openSSE(t, mine+"/events", as(who))
		if c.status != 404 || c.body["code"] != "not_found" {
			t.Fatalf("%s watch: %d %v", who, c.status, c.body)
		}
		for _, sub := range []string{"/commands", "/pair", "/disconnect"} {
			st, body, _ = e.do(t, "POST", mine+sub, []byte(`{}`), as(who))
			wantStatus(t, who+" POST "+sub, st, body, 404, "not_found")
		}
	}
	st, body, _ = e.do(t, "GET", "/api/sessions/rs_AAAAAAAAAAAAAAAAAAAAAA", nil, as("alice"))
	wantStatus(t, "missing session", st, body, 404, "not_found")
	st, body, _ = e.do(t, "GET", "/api/sessions", nil, as("bob"))
	if st != 200 || len(body["sessions"].([]any)) != 0 {
		t.Fatalf("bob's list: %d %v", st, body)
	}
	st, body, _ = e.do(t, "GET", "/api/sessions", nil, as("alice"))
	list := body["sessions"].([]any)
	if st != 200 || len(list) != 1 {
		t.Fatalf("alice's list: %d %v", st, body)
	}
	row := list[0].(map[string]any)
	if row["session_id"] != s.id || row["host"] != "jacobs-mbp" || row["online"] != false || row["harness_pub"] != nil ||
		row["harness_fp_display"] != relay.FingerprintDisplay(s.harness.pub) || row["project"].(map[string]any)["id"] != float64(e.w.s.projectID) {
		t.Fatalf("list row: %v", row)
	}
	st, body, _ = e.do(t, "GET", mine, nil, as("alice"))
	if st != 200 || body["harness_pub"] != s.harness.pub {
		t.Fatalf("GET own session: %d %v", st, body)
	}

	// Cross-site.
	c := s.watch(t, "", header("Sec-Fetch-Site", "cross-site"))
	if c.status != 403 || c.body["code"] != "cross_site" {
		t.Fatalf("cross-site watch: %d %v", c.status, c.body)
	}
	c = s.watch(t, "", fromOrigin("https://evil.test"))
	if c.status != 403 || c.body["code"] != "cross_site" {
		t.Fatalf("foreign-origin watch: %d %v", c.status, c.body)
	}
	dev := newRCKey(t)
	cmd := s.command(t, dev, 1, "stop", map[string]any{})
	st, body, _ = s.send(t, cmd, fromOrigin("https://evil.test"))
	wantStatus(t, "foreign Origin POST", st, body, 403, "cross_site")
	st, body, _ = s.send(t, cmd, noHeader("Origin"))
	wantStatus(t, "POST without Origin", st, body, 403, "cross_site")
	st, body, _ = s.send(t, cmd, contentType("text/plain"))
	wantStatus(t, "command not JSON", st, body, 415, "unsupported_media_type")

	// An ended session of your own is 410.
	if st, _, _ := e.do(t, "POST", mine+"/disconnect", nil, as("alice")); st != 204 {
		t.Fatalf("disconnect: %d", st)
	}
	st, body, _ = e.do(t, "GET", mine, nil, as("alice"))
	wantStatus(t, "ended session", st, body, 410, "session_ended")
	if body["reason"] != "disconnected" {
		t.Fatalf("410 reason: %v", body)
	}
	st, body, _ = e.do(t, "GET", mine, nil, as("bob"))
	wantStatus(t, "someone else's ended session", st, body, 404, "not_found")
}

// The whole path: enable, connect, events, watch with replay, pair, signed
// commands to the harness, and what the audit log keeps.
func TestRelay_EndToEnd(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	hc := s.connect(t)
	s.mustPublish(t, s.hello(t), s.event(t, "status", nil, map[string]any{"busy": false, "devices": []any{}}))

	// A viewer gets the attach frame, the snapshots, then the buffered events.
	v := s.watch(t, "")
	if v.status != 200 {
		t.Fatalf("watch: %d %v", v.status, v.body)
	}
	if a := dataOf(t, v.expect(t, "relay")); a["type"] != "attached" || a["online"] != true || a["oldest_seq"] != float64(1) || a["latest_seq"] != float64(2) {
		t.Fatalf("attached: %v", a)
	}
	if h := dataOf(t, v.expect(t, "snapshot")); h["op"] != "hello" {
		t.Fatalf("first snapshot: %v", h)
	}
	if h := dataOf(t, v.expect(t, "snapshot")); h["op"] != "status" {
		t.Fatalf("second snapshot: %v", h)
	}
	if f := v.expect(t, "envelope"); f.ID != "1" || dataOf(t, f)["op"] != "hello" {
		t.Fatalf("envelope 1: %+v", f)
	}
	v.expect(t, "envelope")
	if n := dataOf(t, hc.expect(t, "viewers")); n["count"] != float64(1) {
		t.Fatalf("viewers: %v", n)
	}
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteAttach, s.id); len(a) != 1 {
		t.Fatalf("remote.attach: %+v", a)
	}

	// Pairing: the harness opens a window; the browser's request reaches it
	// with the verified caller; the signed accept pins the device.
	dev := newRCKey(t)
	pid := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))
	st, body, _ := e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/pairings",
		map[string]any{"pairing_id": pid, "expires_at_ms": e.clock.Now().Add(5 * time.Minute).UnixMilli()}, bearer(s.token))
	wantStatus(t, "open pairing", st, body, 201, "")
	pairReq := map[string]any{
		"request": map[string]any{
			"type": "pair_request", "v": 1, "session_id": s.id, "harness_id": s.harness.id, "pairing_id": pid,
			"device_id": dev.id, "device_pub": dev.pub, "device_name": "Safari on iPhone", "ts": e.clock.Now().UnixMilli(), "nonce": rcNonce(),
		},
		"mac": "bWFj", "sig": "c2ln",
	}
	st, body, _ = e.do(t, "POST", "/api/sessions/"+s.id+"/pair", pairReq, as("alice"), header("User-Agent", "TestBrowser/1"))
	wantStatus(t, "pair", st, body, 202, "")
	if body["status"] != "pending" || body["expires_at_ms"] == nil {
		t.Fatalf("pair: %v", body)
	}
	fwd := dataOf(t, hc.expect(t, "pair_request"))
	rel := fwd["relay"].(map[string]any)
	if rel["user_id"] != float64(e.w.alice.User.ID) || rel["email"] != e.w.alice.Email || rel["user_agent"] != "TestBrowser/1" ||
		fwd["request"].(map[string]any)["device_id"] != dev.id || fwd["mac"] != "bWFj" {
		t.Fatalf("forwarded pair request: %v", fwd)
	}
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemotePair, s.id); len(a) != 1 || a[0].Details["device_id"] != dev.id {
		t.Fatalf("remote.pair: %+v", a)
	}
	s.mustPublish(t, s.event(t, "pair_result", dev.id, map[string]any{
		"pairing_id": pid, "result": "accepted", "code": nil, "tries_left": nil, "pinned_until_ms": e.clock.Now().Add(24 * time.Hour).UnixMilli(),
	}))
	if f := v.expect(t, "envelope"); dataOf(t, f)["op"] != "pair_result" {
		t.Fatalf("pair_result envelope: %v", f)
	}
	d, err := e.w.s.repo.GetRemoteDevice(context.Background(), e.w.alice.User.ID, s.harness.id, dev.id)
	if err != nil || d.DevicePub != dev.pub || d.Name != "Safari on iPhone" {
		t.Fatalf("device not recorded: %+v %v", d, err)
	}
	// The window is consumed.
	st, body, _ = e.do(t, "POST", "/api/sessions/"+s.id+"/pair", pairReq, as("alice"))
	wantStatus(t, "pair after the window was consumed", st, body, 409, "pairing_unavailable")

	// A signed prompt goes through byte-identical (compacted).
	const marker = "MARKER-7f3a-do-not-store"
	cmd := s.command(t, dev, 10, "prompt", prompt("run the tests "+marker))
	spaced := bytes.ReplaceAll(cmd, []byte(`,"`), []byte(`, "`))
	st, body, _ = s.send(t, spaced)
	wantStatus(t, "prompt", st, body, 202, "")
	if got := hc.expect(t, "command"); got.Data != string(cmd) {
		t.Fatalf("forwarded command:\n got %s\nwant %s", got.Data, cmd)
	}
	st, body, _ = s.send(t, s.command(t, dev, 11, "stop", map[string]any{}))
	wantStatus(t, "stop", st, body, 202, "")
	hc.expect(t, "command")
	// Harness-only refusals (unknown op) are forwarded for the harness to refuse.
	st, body, _ = s.send(t, s.command(t, dev, 12, "set_model", map[string]any{"model": "x"}))
	wantStatus(t, "unknown op", st, body, 202, "")
	hc.expect(t, "command")

	p := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemotePrompt, s.id)
	if len(p) != 1 {
		t.Fatalf("remote.prompt: %+v", p)
	}
	wantLen, wantSHA := service.PromptDigest("run the tests " + marker)
	if p[0].Details["len"] != float64(wantLen) || p[0].Details["sha256"] != wantSHA || p[0].Details["device_id"] != dev.id {
		t.Fatalf("remote.prompt details: %v", p[0].Details)
	}
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteStop, s.id); len(a) != 1 {
		t.Fatalf("remote.stop: %+v", a)
	}

	// Replay: Last-Event-ID wins over from_seq; from_seq alone.
	v2 := s.watch(t, "?from_seq=1", header("Last-Event-ID", "2"))
	v2.expect(t, "relay")
	v2.expect(t, "snapshot")
	v2.expect(t, "snapshot")
	if f := v2.expect(t, "envelope"); f.ID != "3" {
		t.Fatalf("Last-Event-ID 2 replay starts at %s", f.ID)
	}
	v3 := s.watch(t, "?from_seq=3")
	v3.expect(t, "relay")
	v3.expect(t, "snapshot")
	v3.expect(t, "snapshot")
	if f := v3.expect(t, "envelope"); f.ID != "3" {
		t.Fatalf("from_seq=3 replay starts at %s", f.ID)
	}
	// A 4th viewer is refused.
	if c := s.watch(t, ""); c.status != 429 || c.body["code"] != "too_many_viewers" {
		t.Fatalf("4th viewer: %d %v", c.status, c.body)
	}
	// Live events reach every viewer.
	s.mustPublish(t, s.event(t, "chat_event", nil, map[string]any{"type": "text_delta", "message_id": "m", "delta": "hi"}))
	for _, c := range []*sseConn{v, v2, v3} {
		if f := c.waitFor(t, "envelope", "text_delta"); f.ID != "4" {
			t.Fatalf("live event id %s", f.ID)
		}
	}
}

// Event batches: retries, gaps, bad envelopes, sizes, and the buffer's limits.
func TestRelay_EventBatchesAndBuffer(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	e1, e2 := s.hello(t), s.event(t, "status", nil, map[string]any{"busy": true})
	s.mustPublish(t, e1, e2)
	// A retry of accepted events is skipped.
	e3 := s.event(t, "chat_event", nil, map[string]any{"type": "notice"})
	st, body := s.publish(t, e2, e3)
	if st != 200 || body["accepted_through"] != float64(3) {
		t.Fatalf("retry batch: %d %v", st, body)
	}
	// A gap.
	s.seq++
	st, body = s.publish(t, s.event(t, "chat_event", nil, map[string]any{}))
	wantStatus(t, "gap", st, body, 409, "seq_gap")
	if body["expected_seq"] != float64(4) || body["accepted_through"] != float64(3) {
		t.Fatalf("seq_gap: %v", body)
	}
	s.seq = 3
	// A bad envelope after a good one: the good one is kept.
	good := s.event(t, "chat_event", nil, map[string]any{})
	forged := s.event(t, "chat_event", nil, map[string]any{"type": "text_delta", "delta": "x"})
	forged = bytes.Replace(forged, []byte(`"x"`), []byte(`"y"`), 1)
	st, body = s.publish(t, good, forged)
	wantStatus(t, "bad signature", st, body, 400, "bad_signature")
	if body["accepted_through"] != float64(4) || body["index"] != float64(1) {
		t.Fatalf("bad_signature: %v", body)
	}
	s.seq = 4
	// device_id is only for ack/refused/pair_result.
	st, body = s.publish(t, s.event(t, "chat_event", newRCKey(t).id, map[string]any{}))
	wantStatus(t, "device_id on a chat_event", st, body, 400, "malformed")
	s.seq = 4
	// Signed by another key, or for another session.
	other := e.register(t, "alice")
	other.seq = 4
	st, body = s.publish(t, other.event(t, "chat_event", nil, map[string]any{}))
	if st != 400 || (body["code"] != "wrong_session") {
		t.Fatalf("another session's event: %d %v", st, body)
	}
	// A hello must carry the registered key.
	s.seq = 4
	bad := s.event(t, "hello", nil, map[string]any{"harness": map[string]any{"harness_id": other.harness.id, "harness_pub": other.harness.pub}})
	st, body = s.publish(t, bad)
	wantStatus(t, "hello with another key", st, body, 400, "malformed")
	s.seq = 4
	// Sizes: over 64 events, over 1 MiB for one event.
	var many []json.RawMessage
	for i := 0; i < 65; i++ {
		many = append(many, s.event(t, "chat_event", nil, map[string]any{}))
	}
	st, body = s.publish(t, many...)
	wantStatus(t, "65 events", st, body, 413, "too_large")
	s.seq = 4
	big := s.event(t, "chat_event", nil, map[string]any{"type": "text_delta", "delta": strings.Repeat("x", 1<<20)})
	st, body = s.publish(t, big)
	wantStatus(t, "event over 1 MiB", st, body, 413, "too_large")
	s.seq = 4
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/events", []byte(`{"events":[]}`), bearer(s.token))
	wantStatus(t, "empty batch", st, body, 400, "malformed")

	// The buffer keeps the newest 200 events: a viewer asking for older ones
	// gets a gap frame.
	for n := 0; n < 4; n++ {
		var batch []json.RawMessage
		for i := 0; i < 50; i++ {
			batch = append(batch, s.event(t, "chat_event", nil, map[string]any{"type": "text_delta", "delta": "d"}))
		}
		s.mustPublish(t, batch...) // seq 5..204
	}
	v := s.watch(t, "?from_seq=1")
	if a := dataOf(t, v.expect(t, "relay")); a["oldest_seq"] != float64(5) || a["latest_seq"] != float64(204) {
		t.Fatalf("attached: %v", a)
	}
	v.expect(t, "snapshot")
	v.expect(t, "snapshot")
	if g := dataOf(t, v.expect(t, "relay")); g["type"] != "gap" || g["requested_seq"] != float64(1) || g["oldest_seq"] != float64(5) {
		t.Fatalf("gap: %v", g)
	}
	if f := v.expect(t, "envelope"); f.ID != "5" {
		t.Fatalf("first buffered: %s", f.ID)
	}
	// And the 10-minute TTL.
	e.clock.Add(11 * time.Minute)
	s.mustPublish(t, s.event(t, "chat_event", nil, map[string]any{}))
	v2 := s.watch(t, "?from_seq=100")
	if a := dataOf(t, v2.expect(t, "relay")); a["oldest_seq"] != float64(205) {
		t.Fatalf("after the TTL: %v", a)
	}
}

// Commands: the relay's early checks, rate limits, and never queueing for an
// offline harness.
func TestRelay_CommandChecks(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	dev := newRCKey(t)
	s.pin(t, dev, e.clock.Now().Add(24*time.Hour))

	// Offline: refused, not queued.
	st, body, _ := s.send(t, s.command(t, dev, 1, "stop", map[string]any{}))
	wantStatus(t, "offline harness", st, body, 409, "harness_offline")
	hc := s.connect(t)
	select {
	case f := <-hc.frames:
		if !f.Comment {
			t.Fatalf("a command refused while offline was delivered later: %+v", f)
		}
	case <-time.After(200 * time.Millisecond):
	}

	stranger := newRCKey(t)
	cases := []struct {
		name   string
		cmd    []byte
		status int
		code   string
	}{
		{"unknown device", s.command(t, stranger, 2, "stop", map[string]any{}), 403, "unknown_device"},
		{"malformed", []byte(`{"v":1}`), 400, "malformed"},
		{"float", bytes.Replace(s.command(t, dev, 2, "stop", map[string]any{}), []byte(`"v":1`), []byte(`"v":1.0`), 1), 400, "malformed"},
		{"too large", []byte(`{"pad":"` + strings.Repeat("x", 70000) + `"}`), 413, "too_large"},
	}
	wrongSess := s.command(t, dev, 2, "stop", map[string]any{})
	wrongSess = bytes.Replace(wrongSess, []byte(s.id), []byte("rs_AAAAAAAAAAAAAAAAAAAAAA"), 1)
	cases = append(cases, struct {
		name   string
		cmd    []byte
		status int
		code   string
	}{"wrong session", wrongSess, 400, "wrong_session"})
	forged := s.command(t, dev, 2, "prompt", prompt("a"))
	forged = bytes.Replace(forged, []byte(`"text":"a"`), []byte(`"text":"b"`), 1)
	cases = append(cases, struct {
		name   string
		cmd    []byte
		status int
		code   string
	}{"bad signature", forged, 403, "bad_signature"})
	for _, c := range cases {
		st, body, _ := s.send(t, c.cmd)
		wantStatus(t, c.name, st, body, c.status, c.code)
	}
	e.clock.Add(-2 * time.Minute)
	old := s.command(t, dev, 3, "stop", map[string]any{})
	e.clock.Add(2 * time.Minute)
	st, body, _ = s.send(t, old)
	wantStatus(t, "stale ts", st, body, 400, "stale_ts")
	// Refusals are audited, sampled once a minute per code.
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteRefused, s.id); len(a) != 6 {
		t.Fatalf("remote.refused: %d entries, want one per code (6)", len(a))
	}
	s.send(t, s.command(t, stranger, 4, "stop", map[string]any{}))
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteRefused, s.id); len(a) != 6 {
		t.Fatalf("remote.refused not sampled: %d entries", len(a))
	}

	// Prompts: burst 5, then 429 with Retry-After; stop is on its own budget.
	for i := 0; i < 5; i++ {
		st, body, _ := s.send(t, s.command(t, dev, int64(10+i), "prompt", prompt("p")))
		wantStatus(t, "prompt within the burst", st, body, 202, "")
	}
	st, body, hdr := s.send(t, s.command(t, dev, 20, "prompt", prompt("p")))
	wantStatus(t, "6th prompt", st, body, 429, "rate_limited")
	if hdr.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	st, body, _ = s.send(t, s.command(t, dev, 21, "stop", map[string]any{}))
	wantStatus(t, "stop while prompts are limited", st, body, 202, "")
	e.clock.Add(3 * time.Second) // 20/min: one prompt every 3 s
	st, body, _ = s.send(t, s.command(t, dev, 22, "prompt", prompt("p")))
	wantStatus(t, "prompt after 3 s", st, body, 202, "")

	// An expired pin, then a revoked one.
	s.pin(t, dev, e.clock.Now().Add(-time.Second))
	st, body, _ = s.send(t, s.command(t, dev, 30, "stop", map[string]any{}))
	wantStatus(t, "expired pin", st, body, 403, "device_expired")
	s.pin(t, dev, e.clock.Now().Add(time.Hour))
	if _, err := e.w.s.repo.RevokeRemoteDevices(context.Background(), e.w.alice.User.ID, dev.id); err != nil {
		t.Fatal(err)
	}
	st, body, _ = s.send(t, s.command(t, dev, 31, "stop", map[string]any{}))
	wantStatus(t, "revoked device", st, body, 403, "unknown_device")

	// A pin recorded for another user (alice) of the same device and harness
	// doesn't count for bob's session.
	b := e.register(t, "bob")
	if _, err := e.w.s.repo.UpsertRemoteDevice(context.Background(), domain.RemoteDevice{
		OwnerUserID: e.w.alice.User.ID, HarnessID: b.harness.id, DeviceID: dev.id, DevicePub: dev.pub, ExpiresAt: e.clock.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	b.connect(t)
	st, body, _ = b.send(t, b.command(t, dev, 1, "stop", map[string]any{}))
	wantStatus(t, "bob with a device paired by alice", st, body, 403, "unknown_device")
}

// Pairing limits: no window, the wrong window, expiry, 5 tries, offline, and
// the fail-closed audit.
func TestRelay_PairingLimits(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	dev := newRCKey(t)
	pid := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16))
	req := func(pairingID string) map[string]any {
		return map[string]any{"request": map[string]any{
			"type": "pair_request", "v": 1, "session_id": s.id, "harness_id": s.harness.id, "pairing_id": pairingID,
			"device_id": dev.id, "device_pub": dev.pub, "device_name": "Chrome on macOS", "ts": e.clock.Now().UnixMilli(), "nonce": rcNonce(),
		}, "mac": "bWFj", "sig": "c2ln"}
	}
	pair := func(body any) (int, map[string]any) {
		st, b, _ := e.do(t, "POST", "/api/sessions/"+s.id+"/pair", body, as("alice"))
		return st, b
	}
	open := func(exp time.Time) {
		t.Helper()
		st, body, _ := e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/pairings",
			map[string]any{"pairing_id": pid, "expires_at_ms": exp.UnixMilli()}, bearer(s.token))
		wantStatus(t, "open pairing", st, body, 201, "")
	}
	st, body := pair(req(pid))
	wantStatus(t, "no window", st, body, 409, "pairing_unavailable")
	// The harness's window must be short and well-formed.
	for _, bad := range []map[string]any{
		{"pairing_id": "short", "expires_at_ms": e.clock.Now().Add(time.Minute).UnixMilli()},
		{"pairing_id": pid, "expires_at_ms": e.clock.Now().Add(time.Hour).UnixMilli()},
		{"pairing_id": pid, "expires_at_ms": e.clock.Now().Add(-time.Second).UnixMilli()},
	} {
		st, body, _ := e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/pairings", bad, bearer(s.token))
		wantStatus(t, "bad window", st, body, 400, "malformed")
	}
	open(e.clock.Now().Add(5 * time.Minute))
	st, body = pair(req(pid))
	wantStatus(t, "harness offline", st, body, 409, "harness_offline")
	hc := s.connect(t)
	st, body = pair(req(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16))))
	wantStatus(t, "another pairing id", st, body, 409, "pairing_unavailable")
	wrong := req(pid)
	wrong["request"].(map[string]any)["session_id"] = "rs_AAAAAAAAAAAAAAAAAAAAAA"
	st, body = pair(wrong)
	wantStatus(t, "request for another session", st, body, 400, "wrong_session")
	st, body = pair([]byte(`{"request":{"a":1.5},"mac":"x","sig":"y"}`))
	wantStatus(t, "malformed request", st, body, 400, "malformed")

	// The fail-closed audit: nothing is forwarded without its remote.pair entry.
	undo := breakRemoteAudit(t, e.w.alice.Email, domain.AuditRemotePair)
	st, body = pair(req(pid))
	wantStatus(t, "audit down", st, body, 503, "audit_unavailable")
	undo()
	for i := 0; i < 4; i++ { // 1 counted above + 4 = 5 tries
		st, body = pair(req(pid))
		wantStatus(t, "pair try", st, body, 202, "")
		hc.expect(t, "pair_request")
	}
	select {
	case f := <-hc.frames:
		if !f.Comment {
			t.Fatalf("an extra frame reached the harness: %+v", f)
		}
	default:
	}
	st, body = pair(req(pid))
	wantStatus(t, "6th try", st, body, 429, "pairing_locked")
	// A failed pair result is audited as remote.pair.reject.
	s.mustPublish(t, s.event(t, "pair_result", dev.id, map[string]any{
		"pairing_id": pid, "result": "locked", "code": "pairing_locked", "tries_left": 0, "pinned_until_ms": nil,
	}))
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemotePairReject, s.id); len(a) != 1 || a[0].Details["code"] != "pairing_locked" {
		t.Fatalf("remote.pair.reject: %+v", a)
	}
	// A new window resets the count; it expires.
	open(e.clock.Now().Add(time.Minute))
	st, body = pair(req(pid))
	wantStatus(t, "after a new window", st, body, 202, "")
	e.clock.Add(61 * time.Second)
	st, body = pair(req(pid))
	wantStatus(t, "expired window", st, body, 409, "pairing_expired")
}

// breakRemoteAudit makes audit inserts of action by email fail until undone.
func breakRemoteAudit(t *testing.T, email, action string) (undo func()) {
	t.Helper()
	ctx := context.Background()
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	sfx := fmt.Sprint(time.Now().UnixNano())
	fn, trig := "rl_audit_fail_"+sfx, "rl_audit_fail_trg_"+sfx
	if _, err := db.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.actor_email = %s AND NEW.action = %s THEN
				RAISE EXCEPTION 'audit log unavailable (test)';
			END IF;
			RETURN NEW;
		END $$`, fn, pgQuote(email), pgQuote(action))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION %s()`, trig, fn)); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	undo = func() {
		once.Do(func() {
			db.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON audit_log`, trig)) //nolint:errcheck
			db.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))             //nolint:errcheck
			db.Close(context.Background())
		})
	}
	t.Cleanup(undo)
	return undo
}

// Keepalives, presence, the one-stream rule, and the idle and lifetime ends.
func TestRelay_KeepalivePresenceAndTimeouts(t *testing.T) {
	e := newRelayEnv(t, func(c *relay.Config) {
		c.Keepalive = 50 * time.Millisecond
		c.OfflineAfter = time.Minute
		c.Sweep = 20 * time.Millisecond
		c.Expire = time.Hour // the test runs the sweep itself
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.rl.Run(ctx)

	s := e.register(t, "alice")
	hc := s.connect(t)
	v := s.watch(t, "")
	if a := dataOf(t, v.expect(t, "relay")); a["online"] != true {
		t.Fatalf("attached: %v", a)
	}
	// Keepalive comments on both streams.
	for _, c := range []*sseConn{hc, v} {
		deadline := time.After(2 * time.Second)
		for got := false; !got; {
			select {
			case f := <-c.frames:
				got = f.Comment && f.Data == "ka"
			case <-deadline:
				t.Fatal("no keepalive")
			}
		}
	}
	// No heartbeat for over 60 s: offline, and commands are refused.
	e.clock.Add(61 * time.Second)
	if f := v.waitFor(t, "relay", "offline"); dataOf(t, f)["type"] != "offline" {
		t.Fatalf("offline frame: %v", f)
	}
	dev := newRCKey(t)
	s.pin(t, dev, e.clock.Now().Add(time.Hour))
	st, body, _ := s.send(t, s.command(t, dev, 1, "stop", map[string]any{}))
	wantStatus(t, "stale heartbeat", st, body, 409, "harness_offline")
	if st, _, _ := e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/heartbeat", nil, bearer(s.token)); st != 200 {
		t.Fatalf("heartbeat: %d", st)
	}
	v.waitFor(t, "relay", `"online"`)

	// A second command stream replaces the first.
	hc2 := s.connect(t)
	if f := hc.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "replaced" {
		t.Fatalf("replaced: %v", f)
	}
	hc.closed(t)
	// The harness stream closing takes the session offline.
	hc2.close()
	v.waitFor(t, "relay", "offline")
	hc3 := s.connect(t)
	v.waitFor(t, "relay", `"online"`)

	// Idle: no remote activity for the org's 30 min (+60 s grace).
	db, err := pgx.Connect(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	if _, err := db.Exec(context.Background(), `UPDATE remote_sessions SET last_activity_at = NOW() - INTERVAL '32 minutes' WHERE id = $1`, s.id); err != nil {
		t.Fatal(err)
	}
	e.rl.ExpireNow(context.Background())
	if f := hc3.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "idle" {
		t.Fatalf("idle end: %v", f)
	}
	if f := v.waitFor(t, "relay", "ended"); dataOf(t, f)["reason"] != "idle" {
		t.Fatalf("viewer end: %v", f)
	}
	v.closed(t)
	hc3.closed(t)
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/heartbeat", nil, bearer(s.token))
	wantStatus(t, "heartbeat after the idle end", st, body, 410, "session_ended")
	if body["reason"] != "idle" {
		t.Fatalf("410 reason: %v", body)
	}
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteExpire, s.id); len(a) != 1 {
		t.Fatalf("remote.expire: %+v", a)
	}

	// Max lifetime: 12 h from registration.
	s2 := e.register(t, "alice")
	hc4 := s2.connect(t)
	if _, err := db.Exec(context.Background(), `UPDATE remote_sessions SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, s2.id); err != nil {
		t.Fatal(err)
	}
	e.rl.ExpireNow(context.Background())
	if f := hc4.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "expired" {
		t.Fatalf("lifetime end: %v", f)
	}
}

// Ending a session from either side, the org switch, and revoke-all close
// every stream at once.
func TestRelay_EndsCloseStreams(t *testing.T) {
	e := newRelayEnv(t, nil)

	// Disconnect from the browser.
	s := e.register(t, "alice")
	hc := s.connect(t)
	v := s.watch(t, "")
	st, _, _ := e.do(t, "POST", "/api/sessions/"+s.id+"/disconnect", nil, as("alice"))
	if st != 204 {
		t.Fatalf("disconnect: %d", st)
	}
	if f := hc.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "disconnected" {
		t.Fatalf("harness end: %v", f)
	}
	if f := v.waitFor(t, "relay", "ended"); dataOf(t, f)["reason"] != "disconnected" {
		t.Fatalf("viewer end: %v", f)
	}
	hc.closed(t)
	v.closed(t)
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteDetach, s.id); len(a) != 1 {
		t.Fatalf("remote.detach: %+v", a)
	}

	// DELETE from the harness.
	s = e.register(t, "alice")
	s.connect(t)
	v = s.watch(t, "")
	if st, _, _ := e.do(t, "DELETE", "/relay/v1/sessions/"+s.id, nil, bearer(s.token)); st != 204 {
		t.Fatalf("DELETE: %d", st)
	}
	if f := v.waitFor(t, "relay", "ended"); dataOf(t, f)["reason"] != "harness_ended" {
		t.Fatalf("viewer end: %v", f)
	}
	st, body, _ := e.do(t, "DELETE", "/relay/v1/sessions/"+s.id, map[string]any{"reason": "nope"}, bearer(s.token))
	if st != 410 {
		t.Fatalf("DELETE again: %d %v", st, body)
	}

	// The org switch off ends the org's sessions (policy_off).
	s = e.register(t, "alice")
	hc = s.connect(t)
	st, body, _ = e.do(t, "PUT", fmt.Sprintf("/api/orgs/%d/remote-control", e.w.org.ID), map[string]any{"enabled": false}, as("orgadmin"))
	if st != 200 || body["enabled"] != false {
		t.Fatalf("org switch off: %d %v", st, body)
	}
	if f := hc.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "policy_off" {
		t.Fatalf("policy_off end: %v", f)
	}
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/token", nil, as("alice"))
	wantStatus(t, "refresh after policy off", st, body, 410, "session_ended")
	enableOrg(t, e.w.s.repo, e.w.org.ID, true)

	// Revoke all: every harness hears device_revoked {all}, then end {revoked}.
	a, b := e.register(t, "alice"), e.register(t, "alice")
	ha, hb := a.connect(t), b.connect(t)
	dev := newRCKey(t)
	a.pin(t, dev, e.clock.Now().Add(time.Hour))
	keep := e.register(t, "bob")
	hk := keep.connect(t)
	st, body, _ = e.do(t, "POST", "/api/sessions/revoke-all", nil, as("alice"))
	if st != 200 || body["ended"] != float64(2) || body["devices_revoked"].(float64) < 1 {
		t.Fatalf("revoke-all: %d %v", st, body)
	}
	for _, c := range []*sseConn{ha, hb} {
		if f := c.expect(t, "device_revoked"); dataOf(t, f)["all"] != true {
			t.Fatalf("device_revoked: %v", f)
		}
		if f := c.expect(t, "end"); dataOf(t, f)["reason"] != "revoked" {
			t.Fatalf("revoked end: %v", f)
		}
	}
	if d, err := e.w.s.repo.GetRemoteDevice(context.Background(), e.w.alice.User.ID, a.harness.id, dev.id); err != nil || d.RevokedAt == nil {
		t.Fatalf("device not revoked: %+v %v", d, err)
	}
	// Bob's session is untouched.
	select {
	case f := <-hk.frames:
		if !f.Comment {
			t.Fatalf("bob's harness got %+v", f)
		}
	case <-time.After(100 * time.Millisecond):
	}
	if st, _, _ := e.do(t, "POST", "/relay/v1/sessions/"+keep.id+"/heartbeat", nil, bearer(keep.token)); st != 200 {
		t.Fatalf("bob's session: %d", st)
	}
}

// No message content is ever written to the database or the logs: prompts,
// deltas, tool output and pairing secrets travel only in memory.
func TestRelay_NoContentPersistedOrLogged(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	e := newRelayEnv(t, nil)
	const marker = "CONTENT-MARKER-91c2e"
	s := e.register(t, "alice")
	hc := s.connect(t)
	v := s.watch(t, "")
	dev := newRCKey(t)
	s.mustPublish(t, s.hello(t),
		s.event(t, "chat_event", nil, map[string]any{"type": "user_message", "text": "prompt " + marker}),
		s.event(t, "chat_event", nil, map[string]any{"type": "text_delta", "message_id": "m", "delta": "answer " + marker}),
		s.event(t, "chat_event", nil, map[string]any{"type": "tool_end", "text": "output " + marker}),
		// #2389: the relay stores the status's model and ticket, and nothing
		// else from it (branch, project, members it doesn't know).
		s.event(t, "status", nil, map[string]any{
			"model": "model-safe", "busy": true, "ticket_id": 7, "ticket_name": "ticket-safe", "branch": "b " + marker,
			"project": map[string]any{"id": 1, "name": "p " + marker}, "notes": "n " + marker,
		}),
	)
	v.waitFor(t, "envelope", "tool_end")
	s.pin(t, dev, e.clock.Now().Add(time.Hour)) // after the hello, whose device list would prune it
	st, body, _ := s.send(t, s.command(t, dev, 1, "prompt", prompt("please "+marker)))
	wantStatus(t, "prompt", st, body, 202, "")
	if f := hc.waitFor(t, "command", ""); !strings.Contains(f.Data, marker) {
		t.Fatal("the command didn't reach the harness")
	}
	// Refused ones too.
	forged := bytes.Replace(s.command(t, dev, 2, "prompt", prompt("x "+marker)), []byte(`"x `), []byte(`"y `), 1)
	if st, _, _ := s.send(t, forged); st != 403 {
		t.Fatalf("forged: %d", st)
	}
	bad := bytes.Replace(s.event(t, "chat_event", nil, map[string]any{"type": "text_delta", "delta": "x " + marker}), []byte(`"x `), []byte(`"z `), 1)
	if st, _ := s.publish(t, bad); st != 400 {
		t.Fatalf("forged event: %d", st)
	}
	if st, _, _ := e.do(t, "DELETE", "/relay/v1/sessions/"+s.id, nil, bearer(s.token)); st != 204 {
		t.Fatal("DELETE")
	}

	if strings.Contains(logs.String(), marker) {
		t.Fatalf("message content reached the logs:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), s.token) {
		t.Fatal("a connect token reached the logs")
	}
	db, err := pgx.Connect(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	for _, q := range []string{
		`SELECT COUNT(*) FROM audit_log WHERE details::text LIKE $1 OR summary LIKE $1`,
		`SELECT COUNT(*) FROM remote_sessions WHERE row_to_json(remote_sessions)::text LIKE $1`,
		`SELECT COUNT(*) FROM remote_devices WHERE row_to_json(remote_devices)::text LIKE $1`,
		`SELECT COUNT(*) FROM live_events WHERE row_to_json(live_events)::text LIKE $1`,
	} {
		var n int
		if err := db.QueryRow(context.Background(), q, "%"+marker+"%").Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Fatalf("message content stored: %s matched %d rows", q, n)
		}
	}
	var model, ticketName *string
	if err := db.QueryRow(context.Background(), `SELECT model, current_ticket_name FROM remote_sessions WHERE id = $1`, s.id).
		Scan(&model, &ticketName); err != nil || model == nil || *model != "model-safe" || ticketName == nil || *ticketName != "ticket-safe" {
		t.Fatalf("status metadata not stored: %v %v %v", model, ticketName, err)
	}
	var n int
	if err := db.QueryRow(context.Background(), `SELECT COUNT(*) FROM remote_sessions WHERE row_to_json(remote_sessions)::text LIKE $1`,
		"%"+s.token+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("connect token stored in clear: %d %v", n, err)
	}
	// The audit trail did record the prompt, as metadata.
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemotePrompt, s.id); len(a) != 1 || a[0].Details["sha256"] == nil {
		t.Fatalf("remote.prompt: %+v", a)
	}
}

// GET /api/sessions (#2389): live sessions first, then the caller's sessions
// that ended in the last 7 days, each with its state, end reason, model,
// devices and current ticket. Owner only.
func TestRelay_SessionListHistory(t *testing.T) {
	e := newRelayEnv(t, nil)
	ctx := context.Background()
	list := func(who string) []map[string]any {
		t.Helper()
		st, body, _ := e.do(t, "GET", "/api/sessions", nil, as(who))
		if st != 200 {
			t.Fatalf("%s's list: %d %v", who, st, body)
		}
		var out []map[string]any
		for _, r := range body["sessions"].([]any) {
			out = append(out, r.(map[string]any))
		}
		return out
	}
	status := func(s *rcSession, body map[string]any) {
		t.Helper()
		body["busy"] = false
		s.mustPublish(t, s.event(t, "status", nil, body))
	}

	s1 := e.register(t, "alice")
	s1.connect(t)
	s1.mustPublish(t, s1.event(t, "hello", nil, map[string]any{
		"harness": map[string]any{"harness_id": s1.harness.id, "harness_pub": s1.harness.pub, "host": "jacobs-mbp", "os": "macos", "version": "0.4.0"},
		"user_id": 1, "ops": []any{"prompt", "stop", "detach"}, "started_at_ms": 1, "expires_at_ms": 2, "idle_timeout_s": 1800,
		"status": map[string]any{"busy": false, "devices": []any{}, "model": "anthropic/claude-opus-5-5", "ticket_id": 4242, "ticket_name": "Ship the list"},
	}))
	rows := list("alice")
	if len(rows) != 1 {
		t.Fatalf("alice's list: %v", rows)
	}
	r := rows[0]
	if r["session_id"] != s1.id || r["state"] != "active" || r["end_reason"] != nil || r["ended_at_ms"] != nil ||
		r["model"] != "anthropic/claude-opus-5-5" || len(r["devices"].([]any)) != 0 {
		t.Fatalf("live row: %v", r)
	}
	if tk := r["ticket"].(map[string]any); tk["id"] != float64(4242) || tk["name"] != "Ship the list" {
		t.Fatalf("ticket from hello: %v", r["ticket"])
	}

	// A paired device shows with its pairing times.
	dev := newRCKey(t)
	until := e.clock.Now().Add(24 * time.Hour)
	s1.pin(t, dev, until)
	devs := list("alice")[0]["devices"].([]any)
	if len(devs) != 1 {
		t.Fatalf("devices: %v", devs)
	}
	d := devs[0].(map[string]any)
	if d["device_id"] != dev.id || d["name"] != "Test browser" || d["expires_at_ms"] != float64(until.UnixMilli()) || d["paired_at_ms"] == nil {
		t.Fatalf("device: %v", d)
	}

	// A status naming a ticket without its name gets the name from AMP; a
	// status without a ticket clears it.
	task, err := e.w.s.repo.CreateTask(ctx, domain.CreateTaskRequest{
		ProjectID: e.w.s.projectID, EpicID: e.w.s.epicID, StoryID: e.w.s.storyID, Name: "Real ticket", Priority: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	status(s1, map[string]any{"model": "openai/gpt-x", "ticket_id": task.ID})
	r = list("alice")[0]
	if tk, _ := r["ticket"].(map[string]any); r["model"] != "openai/gpt-x" || tk == nil || tk["id"] != float64(task.ID) || tk["name"] != "Real ticket" {
		t.Fatalf("after status: %v", r)
	}
	status(s1, map[string]any{"model": nil, "ticket_id": nil})
	if r = list("alice")[0]; r["model"] != nil || r["ticket"] != nil {
		t.Fatalf("after a status without model or ticket: %v", r)
	}

	// An ended session stays listed, after the live ones, with its reason.
	s2 := e.register(t, "alice")
	if st, _, _ := e.do(t, "POST", "/api/sessions/"+s2.id+"/disconnect", nil, as("alice")); st != 204 {
		t.Fatalf("disconnect: %d", st)
	}
	rows = list("alice")
	if len(rows) != 2 || rows[0]["session_id"] != s1.id || rows[1]["session_id"] != s2.id {
		t.Fatalf("alice's list after an end: %v", rows)
	}
	if r = rows[1]; r["state"] != "ended" || r["end_reason"] != "disconnected" || r["ended_at_ms"] == nil || r["online"] != false {
		t.Fatalf("ended row: %v", r)
	}
	// It stays 410 on its own URL.
	st, body, _ := e.do(t, "GET", "/api/sessions/"+s2.id, nil, as("alice"))
	wantStatus(t, "ended session", st, body, 410, "session_ended")

	// Owner only: bob's sessions, live or ended, are his alone.
	b := e.register(t, "bob")
	if st, _, _ := e.do(t, "POST", "/api/sessions/"+b.id+"/disconnect", nil, as("bob")); st != 204 {
		t.Fatalf("bob's disconnect: %d", st)
	}
	if rows = list("bob"); len(rows) != 1 || rows[0]["session_id"] != b.id {
		t.Fatalf("bob's list: %v", rows)
	}
	for _, who := range []string{"orgadmin", "owner", "root", "outsider"} {
		if rows := list(who); len(rows) != 0 {
			t.Fatalf("%s sees %v", who, rows)
		}
	}
	if rows = list("alice"); len(rows) != 2 {
		t.Fatalf("alice sees bob's session: %v", rows)
	}

	// Sessions that ended more than 7 days ago drop out.
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	if _, err := db.Exec(ctx, `UPDATE remote_sessions SET ended_at = NOW() - interval '8 days' WHERE id = $1`, s2.id); err != nil {
		t.Fatal(err)
	}
	if rows = list("alice"); len(rows) != 1 || rows[0]["session_id"] != s1.id {
		t.Fatalf("an 8-day-old session is still listed: %v", rows)
	}
	// An ended session's metadata no longer changes.
	late := "late-model"
	if err := e.w.s.repo.SetRemoteStatusMeta(ctx, s2.id, domain.RemoteStatusMeta{Model: &late}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.w.s.repo.GetRemoteSession(ctx, s2.id); err != nil || got.Model != nil {
		t.Fatalf("an ended session's model changed: %v %v", got, err)
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
