package relay

// The harness leg, /relay/v1/... (protocol.md 8.2). It is not behind the REST
// listener's oauth2-proxy verifier: register and token refresh take the
// harness's OAuth access token through the MCP-audience verifier (the one
// /mcp uses), refusing amp_ API tokens and agent accounts; every other call
// takes the session's connect token.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// HarnessHandler serves /relay/v1/.
func (rl *Relay) HarnessHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /relay/v1/sessions", rl.oauth(http.HandlerFunc(rl.register)))
	mux.Handle("POST /relay/v1/sessions/{id}/token", rl.oauth(http.HandlerFunc(rl.refresh)))
	mux.HandleFunc("GET /relay/v1/sessions/{id}/commands", rl.withToken(rl.commandStream))
	mux.HandleFunc("POST /relay/v1/sessions/{id}/events", rl.withToken(rl.publish))
	mux.HandleFunc("POST /relay/v1/sessions/{id}/heartbeat", rl.withToken(rl.heartbeat))
	mux.HandleFunc("POST /relay/v1/sessions/{id}/pairings", rl.withToken(rl.openPairing))
	mux.HandleFunc("DELETE /relay/v1/sessions/{id}", rl.withToken(rl.endFromHarness))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, errf(404, domain.RemoteCodeNotFound, "not found"))
	})
	return mux
}

// harnessPerson refuses OAuth callers that can't use remote control: agent
// accounts and amp_ API tokens (protocol 8.1).
func harnessPerson(r *http.Request) (authz.Principal, error) {
	p, err := authz.FromContext(r.Context())
	switch {
	case err != nil:
		return p, err
	case !p.HasUser():
		return p, errf(404, domain.RemoteCodeNotFound, "remote control isn't available on this AMP server")
	case p.Agent:
		return p, errf(403, domain.RemoteCodeAgentNotAllowed, "agent accounts can't use remote control")
	case p.ViaToken:
		return p, errf(403, domain.RemoteCodeTokenNotAllowed,
			"API tokens can't use remote control; sign in with `amp-harness login`")
	}
	return p, nil
}

// sessionEnded is the 410 for an ended session, with its reason.
func sessionEnded(sess *domain.RemoteSession) *apiError {
	reason := domain.RemoteEndHarnessEnded
	if sess != nil && sess.EndReason != nil {
		reason = *sess.EndReason
	}
	return errf(410, domain.RemoteCodeSessionEnded, "remote session has ended").with("reason", reason)
}

// serviceErr turns the service's 410 into one that carries the reason.
func serviceErr(sess *domain.RemoteSession, err error) error {
	if authz.CodeOf(err) == domain.RemoteCodeSessionEnded {
		return sessionEnded(sess)
	}
	return err
}

// ---- register and refresh (OAuth) ----

type registerRequest struct {
	ProtocolVersions []int  `json:"protocol_versions"`
	HarnessID        string `json:"harness_id"`
	HarnessPub       string `json:"harness_pub"`
	Host             string `json:"host"`
	OS               string `json:"os"`
	HarnessVersion   string `json:"harness_version"`
	ProjectID        int    `json:"project_id"`
}

// register is POST /relay/v1/sessions: remote control on (`/remote`).
func (rl *Relay) register(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := harnessPerson(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := requireJSON(r); err != nil {
		writeErr(w, err)
		return
	}
	body, err := readBody(w, r, MaxSmallBody)
	if err != nil {
		writeErr(w, err)
		return
	}
	var req registerRequest
	if json.Unmarshal(body, &req) != nil || req.HarnessID == "" || req.HarnessPub == "" || req.ProjectID <= 0 {
		writeErr(w, errf(400, CodeMalformed, "send protocol_versions, harness_id, harness_pub, host, os, harness_version and project_id"))
		return
	}
	if !slices.Contains(req.ProtocolVersions, ProtocolVersion) {
		writeErr(w, errf(400, CodeUnsupportedVersion, "this AMP server speaks remote control protocol v%d", ProtocolVersion).
			with("supported_versions", []int{ProtocolVersion}))
		return
	}
	now := rl.cfg.Now()
	rl.mu.Lock()
	b := rl.registers[p.UserID]
	if b == nil {
		b = newBucket(registrationsPerMin, registrationsPerMin)
		rl.registers[p.UserID] = b
	}
	ok, wait := b.take(now)
	rl.mu.Unlock()
	if !ok {
		writeErr(w, errf(429, "rate_limited", "too many registrations; wait a minute").with("retry_after_s", retryAfter(wait)))
		return
	}
	// The same limit across replicas: sessions started in the last minute.
	if n, wait, err := rl.repo.RecentRemoteRegistrations(ctx, p.UserID, time.Minute); err != nil {
		writeErr(w, err)
		return
	} else if n >= registrationsPerMin {
		writeErr(w, errf(429, "rate_limited", "too many registrations; wait a minute").with("retry_after_s", retryAfter(wait)))
		return
	}
	sess, err := rl.svc.EnableRemoteSession(ctx, p, service.RemoteRegistration{
		ProjectID: req.ProjectID, HarnessID: req.HarnessID, HarnessPub: req.HarnessPub,
		Host: req.Host, OS: req.OS, HarnessVersion: req.HarnessVersion,
		ProtocolVersion: ProtocolVersion, Node: rl.cfg.Node,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	rl.reg.Ensure(sess)
	tok, exp, err := rl.mintToken(ctx, sess)
	if err != nil {
		writeErr(w, err)
		return
	}
	pol, _, err := rl.repo.ProjectRemotePolicy(ctx, sess.ProjectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	slog.Info("remote session registered", "session", sess.ID, "user_id", p.UserID, "project_id", sess.ProjectID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"session_id":                  sess.ID,
		"protocol_version":            ProtocolVersion,
		"connect_token":               tok,
		"connect_token_expires_at_ms": ms(exp),
		"started_at_ms":               ms(sess.StartedAt),
		// null: no lifetime / no idle limit, the default (#2424).
		"expires_at_ms":               msOrNull(sess.ExpiresAt),
		"idle_timeout_s":              idleSeconds(pol),
		"device_pin_max_days":         pol.DevicePinDays,
		"heartbeat_interval_s":        20,
		"keepalive_interval_s":        int(rl.cfg.Keepalive / time.Second),
		"session_url":                 rl.cfg.AppURL + "/sessions/" + sess.ID,
		"limits": map[string]int{
			"command_bytes": MaxCommandBytes, "event_bytes": MaxEventBytes, "batch_bytes": MaxBatchBytes,
			"batch_events": MaxBatchEvents, "prompts_per_min": 20, "ops_per_min": 120, "viewers": MaxViewers,
			"pairing_tries": PairingTries,
		},
	})
}

// refresh is POST /relay/v1/sessions/:id/token: a new connect token, after
// re-checking the person, the harness key's proof (token.go) and the org
// policy.
func (rl *Relay) refresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := harnessPerson(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	sess, err := rl.svc.OwnRemoteSession(ctx, p, r.PathValue("id"))
	if err != nil {
		writeErr(w, serviceErr(sess, err))
		return
	}
	// The OAuth login alone isn't enough (#2390): the harness proves it holds
	// the session's key, so another program with the same login can't take
	// the session over.
	if err := requireJSON(r); err != nil {
		writeErr(w, err)
		return
	}
	body, err := readBody(w, r, MaxSmallBody)
	if err != nil {
		writeErr(w, err)
		return
	}
	key, _ := parsePub(sess.HarnessPub)
	now := rl.cfg.Now()
	var nonceErr error
	code := CheckTokenProof(body, TokenProofReceiver{
		NowMS: now.UnixMilli(), SessionID: sess.ID, HarnessID: sess.HarnessID, Harness: key,
		// In the database, so a proof used on one replica can't be used
		// again on another (re-review #2394 N6).
		Seen: func(nonce string) bool {
			replayed, err := rl.repo.UseProofNonce(ctx, sess.ID, nonce, now, now.Add(2*TSWindow))
			if err != nil {
				nonceErr = err
				return true
			}
			return replayed
		},
	})
	if nonceErr != nil {
		writeErr(w, nonceErr)
		return
	}
	if code != CodeOK {
		status := 400
		switch code {
		case CodeBadSignature:
			status = 403
		case CodeTooLarge:
			status = 413
		}
		slog.Warn("remote token refresh refused", "session", sess.ID, "user_id", p.UserID, "code", code)
		e := errf(status, code, "the token refresh needs a proof signed by the session's harness key: %s", code)
		if code == CodeStaleTS {
			// The harness tells its person how far off the laptop's clock is
			// (re-review #2394 N5).
			e = e.with("now_ms", now.UnixMilli())
		}
		writeErr(w, e)
		return
	}
	// The person may have lost access to the project (removed from its team
	// or org) since the session started: that ends the session, as
	// deactivation does (re-review #2394 N7).
	if ended, err := rl.svc.EndRemoteWithoutAccess(ctx, authz.Principal{}, sess.OwnerUserID); err != nil {
		writeErr(w, err)
		return
	} else if slices.ContainsFunc(ended, func(x domain.RemoteSession) bool { return x.ID == sess.ID }) {
		writeErr(w, errf(410, domain.RemoteCodeSessionEnded, "remote session has ended").with("reason", domain.RemoteEndRevoked))
		return
	}
	pol, _, err := rl.repo.ProjectRemotePolicy(ctx, sess.ProjectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !pol.Enabled {
		writeErr(w, errf(403, domain.RemoteCodeDisabled, "remote control is off for this project's org"))
		return
	}
	tok, exp, err := rl.mintToken(ctx, sess)
	if err != nil {
		writeErr(w, serviceErr(sess, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connect_token": tok, "connect_token_expires_at_ms": ms(exp)})
}

// ---- connect-token calls ----

// registrationsPerMin limits a person's registrations (protocol 10).
const registrationsPerMin = 10

// withToken authenticates a harness call by the session's connect token. The
// token is bound to one session; an ended session is 410 with its reason (the
// database keeps the ended session's token hash, so every replica knows), an
// expired token 401 token_expired. The call is then served on the replica
// holding the session.
func (rl *Relay) withToken(h func(http.ResponseWriter, *http.Request, *domain.RemoteSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		tok := strings.TrimSpace(strings.TrimPrefix(hdr, "Bearer "))
		if !strings.HasPrefix(hdr, "Bearer ") || !strings.HasPrefix(tok, "rct_") {
			writeErr(w, errf(401, "unauthenticated", "send the session's connect token"))
			return
		}
		hash := hashToken(tok)
		sess, err := rl.repo.RemoteSessionByConnectToken(r.Context(), hash)
		if errors.Is(err, repository.ErrNotFound) {
			writeErr(w, errf(401, "unauthenticated", "unknown connect token"))
			return
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		switch {
		case sess.ID != r.PathValue("id"):
			writeErr(w, errf(401, "unauthenticated", "this connect token is for another session"))
		case !sess.Active():
			writeErr(w, sessionEnded(sess))
		case sess.ConnectTokenExpiresAt != nil && !rl.cfg.Now().Before(*sess.ConnectTokenExpiresAt):
			writeErr(w, errf(401, "token_expired", "the connect token has expired; refresh it"))
		case rl.cfg.Forwarder != nil && rl.cfg.Forwarder.Forward(w, r, sess):
		default:
			h(w, r, sess)
		}
	}
}

// commandStream is GET /relay/v1/sessions/:id/commands: the harness's one
// stream. A new stream replaces the old one, which gets end {replaced}.
func (rl *Relay) commandStream(w http.ResponseWriter, r *http.Request, sess *domain.RemoteSession) {
	ctx := r.Context()
	var held bool
	var err error
	if rl.cfg.Node != "" {
		held, err = rl.repo.ClaimRemoteSession(ctx, sess.ID, rl.cfg.Node)
	} else {
		held, err = rl.repo.TouchRemoteHeartbeat(ctx, sess.ID)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	if !held {
		writeErr(w, sessionEnded(nil))
		return
	}
	live := rl.reg.Ensure(sess)
	st := newStream(harnessQueue)
	now := rl.cfg.Now()
	live.mu.Lock()
	if live.ended {
		live.mu.Unlock()
		writeErr(w, sessionEnded(nil))
		return
	}
	if old := live.harness; old != nil {
		old.send(frame{event: "end", data: []byte(`{"reason":"replaced"}`)})
		old.close()
	}
	live.harness, live.heartbeat = st, now
	live.tellHarnessViewers()
	live.updatePresence(now, rl.cfg.OfflineAfter)
	live.mu.Unlock()
	slog.Info("remote harness connected", "session", sess.ID)

	rl.pump(ctx, rl.startSSE(w), st)

	live.mu.Lock()
	if live.harness == st {
		live.harness = nil
		live.updatePresence(rl.cfg.Now(), rl.cfg.OfflineAfter)
	}
	live.mu.Unlock()
	st.close()
	st.drain()
	slog.Info("remote harness disconnected", "session", sess.ID)
}

// eventOpsWithDevice are the event ops whose device_id may be set.
var eventOpsWithDevice = []string{"ack", "refused", "pair_result"}

// publish is POST /relay/v1/sessions/:id/events: a batch of signed events,
// checked (5.7 steps 1-7 with the registered key), sequenced, buffered and
// fanned out to viewers. Payloads are never logged or stored.
func (rl *Relay) publish(w http.ResponseWriter, r *http.Request, sess *domain.RemoteSession) {
	ctx := r.Context()
	if err := requireJSON(r); err != nil {
		writeErr(w, err)
		return
	}
	body, err := readBody(w, r, MaxBatchBytes)
	if err != nil {
		writeErr(w, err)
		return
	}
	var batch struct {
		Events []json.RawMessage `json:"events"`
	}
	if json.Unmarshal(body, &batch) != nil || len(batch.Events) == 0 {
		writeErr(w, errf(400, CodeMalformed, `send {"events": [envelope, ...]}`))
		return
	}
	if len(batch.Events) > MaxBatchEvents {
		writeErr(w, errf(413, CodeTooLarge, "at most %d events per batch", MaxBatchEvents))
		return
	}
	live := rl.reg.Ensure(sess)
	// Event bytes per session (#2390): a harness can't push more than the
	// relay and its viewers can carry.
	live.mu.Lock()
	ok, wait := live.events.takeN(rl.cfg.Now(), float64(len(body)))
	live.mu.Unlock()
	if !ok {
		writeErr(w, errf(429, "rate_limited", "too many event bytes; slow down").with("retry_after_s", retryAfter(wait)))
		return
	}
	rx := EventReceiver{SessionID: sess.ID, HarnessID: sess.HarnessID, Harness: live.harnessKey}
	acceptedThrough := func() int64 {
		live.mu.Lock()
		defer live.mu.Unlock()
		return live.acceptedThrough
	}
	refuse := func(i int, code string) {
		status := 400
		if code == CodeTooLarge {
			status = 413
		}
		slog.Warn("remote event refused", "session", sess.ID, "code", code, "index", i)
		writeErr(w, errf(status, code, "event %d refused: %s", i, code).
			with("accepted_through", acceptedThrough()).with("index", i))
	}
	for i, raw := range batch.Events {
		code, env := CheckEvent(raw, rx)
		if code == CodeOK && env.DeviceID != nil && !slices.Contains(eventOpsWithDevice, env.Op) {
			code = CodeMalformed
		}
		if code == CodeOK && env.Op == "hello" && !rl.helloMatches(env, sess) {
			code = CodeMalformed
		}
		if code != CodeOK {
			refuse(i, code)
			return
		}
		var compact bytes.Buffer
		if json.Compact(&compact, raw) != nil {
			refuse(i, CodeMalformed)
			return
		}
		data := compact.Bytes()
		now := rl.cfg.Now()

		live.mu.Lock()
		if live.ended {
			live.mu.Unlock()
			writeErr(w, sessionEnded(nil))
			return
		}
		acc := live.acceptedThrough
		if acc > 0 && env.Seq <= acc {
			live.mu.Unlock()
			continue // a retry: already accepted
		}
		if acc > 0 && env.Seq != acc+1 {
			live.mu.Unlock()
			writeErr(w, errf(409, "seq_gap", "events must be contiguous").
				with("expected_seq", acc+1).with("accepted_through", acc))
			return
		}
		// A replica with no state for the session (a fresh session, or one
		// taken over after a failover) accepts the first seq it sees; a
		// viewer waiting to resume from an earlier seq is told of the gap.
		if acc == 0 {
			for v := range live.viewers {
				if v.want > 0 && v.want < env.Seq {
					v.send(relayFrame(map[string]any{"type": "gap", "requested_seq": v.want, "oldest_seq": env.Seq}))
				}
			}
		}
		live.acceptedThrough = env.Seq
		live.ring = append(live.ring, bufferedEvent{seq: env.Seq, data: data, at: now})
		live.ringBytes += len(data)
		live.evict(now)
		switch env.Op {
		case "hello":
			live.hello = data
		case "status":
			live.status = data
		}
		live.broadcast(frame{id: env.Seq, event: "envelope", data: data})
		live.mu.Unlock()

		rl.afterEvent(ctx, sess, live, env)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted_through": acceptedThrough()})
}

// helloMatches checks hello.body.harness: its key must be the registered one.
func (rl *Relay) helloMatches(env *Envelope, sess *domain.RemoteSession) bool {
	h, _ := env.Body["harness"].(map[string]any)
	id, _ := h["harness_id"].(string)
	pub, _ := h["harness_pub"].(string)
	return id == sess.HarnessID && pub == sess.HarnessPub
}

// afterEvent applies an accepted event's metadata: devices the harness
// accepted or no longer pins, and pairing outcomes for the audit trail.
func (rl *Relay) afterEvent(ctx context.Context, sess *domain.RemoteSession, live *Live, env *Envelope) {
	switch env.Op {
	case "hello":
		st, _ := env.Body["status"].(map[string]any)
		rl.reconcileDevices(ctx, sess, live, st)
		rl.storeStatusMeta(ctx, sess, live, st)
	case "status":
		rl.reconcileDevices(ctx, sess, live, env.Body)
		rl.storeStatusMeta(ctx, sess, live, env.Body)
	case "pair_result":
		if env.DeviceID == nil {
			return
		}
		dev := *env.DeviceID
		result, _ := env.Body["result"].(string)
		pid, _ := env.Body["pairing_id"].(string)
		code, _ := env.Body["code"].(string)
		live.mu.Lock()
		pend, havePending := live.pending[dev]
		if result != "failed" {
			delete(live.pending, dev)
			if live.pairing != nil && live.pairing.id == pid {
				live.pairing = nil // consumed (accepted, rejected, expired) or burned (locked)
			}
		}
		live.mu.Unlock()
		switch result {
		case "accepted":
			if havePending && pend.pairingID == pid {
				if _, err := rl.svc.PinRemoteDevice(ctx, sess, dev, pend.devicePub, pend.name); err != nil {
					slog.Warn("recording a paired remote device failed", "session", sess.ID, "err", err)
				}
			}
		case "rejected", "failed", "locked":
			if code == "" {
				code = result
			}
			rl.svc.RecordRemote(ctx, rl.ownerPrincipal(ctx, sess), sess, service.RemoteAudit{
				Action: domain.AuditRemotePairReject, Op: "pair", DeviceID: dev, Code: code,
			})
		}
	}
}

// reconcileDevices deletes the relay's device rows that the harness no longer
// pins (status.devices, protocol 7.6), when the set changed.
func (rl *Relay) reconcileDevices(ctx context.Context, sess *domain.RemoteSession, live *Live, status map[string]any) {
	list, ok := status["devices"].([]any)
	if !ok {
		return
	}
	keep := []string{}
	for _, d := range list {
		m, _ := d.(map[string]any)
		if id, ok := m["device_id"].(string); ok {
			keep = append(keep, id)
		}
	}
	slices.Sort(keep)
	key := strings.Join(keep, ",")
	live.mu.Lock()
	same := live.devices == key && key != ""
	live.devices = key
	live.mu.Unlock()
	if same {
		return
	}
	if _, err := rl.repo.PruneRemoteDevices(ctx, sess.OwnerUserID, sess.HarnessID, keep); err != nil {
		slog.Warn("reconciling remote devices failed", "session", sess.ID, "err", err)
	}
}

// Longest model and ticket name the relay stores, in bytes (cut on a rune boundary).
const (
	maxModelBytes      = 200
	maxTicketNameBytes = 200
)

// statusMeta picks the members of a status the relay stores (#2389): model,
// ticket_id and ticket_name. Nothing else in the status is ever stored. A
// member missing or of the wrong type counts as null; an older harness has no
// ticket_name.
func statusMeta(status map[string]any) domain.RemoteStatusMeta {
	var m domain.RemoteStatusMeta
	if v, ok := status["model"].(string); ok && strings.TrimSpace(v) != "" {
		v = clipRunes(strings.TrimSpace(v), maxModelBytes)
		m.Model = &v
	}
	if v, ok := status["ticket_id"].(int64); ok && v > 0 {
		m.TicketID = &v
		if n, ok := status["ticket_name"].(string); ok && strings.TrimSpace(n) != "" {
			n = clipRunes(strings.TrimSpace(n), maxTicketNameBytes)
			m.TicketName = &n
		}
	}
	return m
}

// storeStatusMeta writes the status's model and current ticket to the
// session row when they changed since the last status this replica stored.
func (rl *Relay) storeStatusMeta(ctx context.Context, sess *domain.RemoteSession, live *Live, status map[string]any) {
	if status == nil {
		return
	}
	m := statusMeta(status)
	key := fmt.Sprintf("%v|%v|%v", deref(m.Model), deref(m.TicketID), deref(m.TicketName))
	live.mu.Lock()
	same := live.meta == key
	live.meta = key
	live.mu.Unlock()
	if same {
		return
	}
	if err := rl.repo.SetRemoteStatusMeta(ctx, sess.ID, m); err != nil {
		slog.Warn("storing remote session status failed", "session", sess.ID, "err", err)
		live.mu.Lock()
		live.meta = "" // try again on the next status
		live.mu.Unlock()
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// ownerPrincipal is the session owner, for audit entries the harness causes.
func (rl *Relay) ownerPrincipal(ctx context.Context, sess *domain.RemoteSession) authz.Principal {
	p := authz.Principal{UserID: sess.OwnerUserID}
	if u, err := rl.repo.GetUserByID(ctx, sess.OwnerUserID); err == nil {
		p.Email = u.Email
	}
	return p
}

// heartbeat is POST /relay/v1/sessions/:id/heartbeat (every 20 s).
func (rl *Relay) heartbeat(w http.ResponseWriter, r *http.Request, sess *domain.RemoteSession) {
	ctx := r.Context()
	ok, err := rl.repo.TouchRemoteHeartbeat(ctx, sess.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, sessionEnded(nil))
		return
	}
	pol, _, err := rl.repo.ProjectRemotePolicy(ctx, sess.ProjectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	live := rl.reg.Ensure(sess)
	now := rl.cfg.Now()
	live.mu.Lock()
	live.heartbeat = now
	live.updatePresence(now, rl.cfg.OfflineAfter)
	viewers := len(live.viewers)
	live.mu.Unlock()
	var idle *time.Time
	if pol.IdleTimeoutMin > 0 {
		t := sess.LastActivityAt.Add(pol.IdleTimeout())
		idle = &t
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"viewers": viewers, "idle_deadline_ms": msOrNull(idle), "expires_at_ms": msOrNull(sess.ExpiresAt),
	})
}

// openPairing is POST /relay/v1/sessions/:id/pairings: the harness opens a
// pairing window (it replaces any earlier one and resets the count).
func (rl *Relay) openPairing(w http.ResponseWriter, r *http.Request, sess *domain.RemoteSession) {
	if err := requireJSON(r); err != nil {
		writeErr(w, err)
		return
	}
	body, err := readBody(w, r, MaxSmallBody)
	if err != nil {
		writeErr(w, err)
		return
	}
	var req struct {
		PairingID   string `json:"pairing_id"`
		ExpiresAtMS int64  `json:"expires_at_ms"`
	}
	now := rl.cfg.Now()
	if json.Unmarshal(body, &req) != nil {
		writeErr(w, errf(400, CodeMalformed, `send {"pairing_id", "expires_at_ms"}`))
		return
	}
	if raw, err := b64uDecode(req.PairingID); err != nil || len(raw) != 16 {
		writeErr(w, errf(400, CodeMalformed, "pairing_id must be 16 bytes of b64u"))
		return
	}
	exp := time.UnixMilli(req.ExpiresAtMS)
	if !exp.After(now) || exp.After(now.Add(PairingWindow+5*time.Second)) {
		writeErr(w, errf(400, CodeMalformed, "expires_at_ms must be within the next 5 minutes"))
		return
	}
	live := rl.reg.Ensure(sess)
	live.mu.Lock()
	live.pairing = &pairingWindow{id: req.PairingID, expires: exp}
	live.pending = map[string]pendingPair{}
	live.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{})
}

// harnessEndReasons are the reasons a harness may give when it ends a session.
var harnessEndReasons = []string{
	domain.RemoteEndHarnessEnded, domain.RemoteEndDetached, domain.RemoteEndIdle, domain.RemoteEndExpired,
}

// endFromHarness is DELETE /relay/v1/sessions/:id: remote off, detach or
// exit. The session ends (audited remote.disable, or remote.expire for idle
// and expired) and every stream closes.
func (rl *Relay) endFromHarness(w http.ResponseWriter, r *http.Request, sess *domain.RemoteSession) {
	ctx := r.Context()
	body, err := readBody(w, r, MaxSmallBody)
	if err != nil {
		writeErr(w, err)
		return
	}
	reason := domain.RemoteEndHarnessEnded
	if len(bytes.TrimSpace(body)) > 0 {
		var req struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(body, &req) != nil || (req.Reason != "" && !slices.Contains(harnessEndReasons, req.Reason)) {
			writeErr(w, errf(400, CodeMalformed, "reason must be one of %s", strings.Join(harnessEndReasons, ", ")))
			return
		}
		if req.Reason != "" {
			reason = req.Reason
		}
	}
	if _, err := rl.svc.EndRemoteSession(ctx, rl.ownerPrincipal(ctx, sess), sess.ID, reason); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
