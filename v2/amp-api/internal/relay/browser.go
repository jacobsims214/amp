package relay

// The browser leg, /api/sessions/... (protocol.md 8.3). It sits behind the
// REST listener's verifier (the oauth2-proxy cookie through Envoy), and every
// call must come from a person in the web UI (Channel == ui), which rules out
// agents, amp_ API tokens and MCP tokens. Only the session's owner can see,
// watch or drive it: someone else's session, global admins included, is the
// same 404 as a missing one.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// RegisterBrowser registers the browser leg on the REST mux.
func (rl *Relay) RegisterBrowser(mux *http.ServeMux) {
	m := rl.browserMux()
	mux.Handle("/api/sessions", m)
	mux.Handle("/api/sessions/", m)
}

// browserMux routes the browser leg.
func (rl *Relay) browserMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/sessions", rl.ui(rl.listSessions))
	m.HandleFunc("POST /api/sessions/revoke-all", rl.ui(rl.revokeAll))
	m.HandleFunc("GET /api/sessions/{id}", rl.ui(rl.getSession))
	m.HandleFunc("GET /api/sessions/{id}/events", rl.ui(rl.eventStream))
	m.HandleFunc("POST /api/sessions/{id}/commands", rl.ui(rl.postCommand))
	m.HandleFunc("POST /api/sessions/{id}/pair", rl.ui(rl.pair))
	m.HandleFunc("POST /api/sessions/{id}/disconnect", rl.ui(rl.disconnect))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, errf(404, domain.RemoteCodeNotFound, "not found"))
	})
	return m
}

// ui requires a person in the web UI (protocol 8.1).
func (rl *Relay) ui(h func(http.ResponseWriter, *http.Request, authz.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := principalOf(r)
		switch {
		case err != nil:
			writeErr(w, err)
		case !p.InUI():
			writeErr(w, errf(403, domain.RemoteCodeNotUIChannel,
				"remote sessions are used by a person in the AMP web UI, not by an agent, an API token or over MCP"))
		case !p.HasUser():
			writeErr(w, errf(404, domain.RemoteCodeNotFound, "remote control isn't available on this AMP server"))
		default:
			h(w, r, p)
		}
	}
}

// checkOrigin refuses cross-site requests: Sec-Fetch-Site cross-site, or an
// Origin other than the AMP web origin. POSTs must carry the Origin (every
// browser sends it on a POST).
func (rl *Relay) checkOrigin(r *http.Request, requireOrigin bool) error {
	bad := errf(403, "cross_site", "cross-site requests can't use remote sessions")
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return bad
	}
	o := r.Header.Get("Origin")
	if o == "" {
		if requireOrigin {
			return bad
		}
		return nil
	}
	want := rl.cfg.AppURL
	if want == "" { // no public URL configured: the request's own host
		if o == "https://"+r.Host || o == "http://"+r.Host {
			return nil
		}
		return bad
	}
	if o != want {
		return bad
	}
	return nil
}

// ownSession loads the caller's live session from the path (404 for missing
// or not theirs, 410 for their ended one), or forwards the call to the
// replica holding it. false means the response has been written.
func (rl *Relay) ownSession(w http.ResponseWriter, r *http.Request, p authz.Principal) (*domain.RemoteSession, bool) {
	sess, err := rl.svc.OwnRemoteSession(r.Context(), p, r.PathValue("id"))
	if err != nil {
		writeErr(w, serviceErr(sess, err))
		return nil, false
	}
	if rl.cfg.Forwarder != nil && rl.cfg.Forwarder.Forward(w, r, sess) {
		return nil, false
	}
	return sess, true
}

// sessionView is a session as GET /api/sessions shows it (metadata only).
// devices maps harness_id to the devices paired with it; a harness missing
// from it is looked up.
func (rl *Relay) sessionView(ctx context.Context, sess *domain.RemoteSession, names map[int]string, devices map[string][]domain.RemoteDevice, withPub bool) map[string]any {
	var project any
	if name, ok := names[sess.ProjectID]; ok {
		project = map[string]any{"id": sess.ProjectID, "name": name}
	} else if proj, err := rl.repo.GetProject(ctx, sess.ProjectID); err == nil {
		names[sess.ProjectID] = proj.Name
		project = map[string]any{"id": sess.ProjectID, "name": proj.Name}
	}
	online, viewers := false, 0
	if sess.Active() {
		online, viewers = rl.presence(ctx, sess)
	}
	devs, ok := devices[sess.HarnessID]
	if !ok {
		list, err := rl.repo.ListRemoteDevices(ctx, sess.OwnerUserID, sess.HarnessID)
		if err != nil {
			slog.Warn("listing remote devices failed", "session", sess.ID, "err", err)
		}
		devs = list
	}
	devOut := make([]map[string]any, 0, len(devs))
	for _, d := range devs {
		devOut = append(devOut, map[string]any{
			"device_id": d.DeviceID, "name": d.Name, "paired_at_ms": ms(d.PairedAt), "expires_at_ms": ms(d.ExpiresAt),
		})
	}
	var ticket any
	if sess.CurrentTicketID != nil {
		ticket = map[string]any{"id": *sess.CurrentTicketID, "name": sess.CurrentTicketName}
	}
	var endedAt any
	if sess.EndedAt != nil {
		endedAt = ms(*sess.EndedAt)
	}
	v := map[string]any{
		"session_id": sess.ID, "harness_id": sess.HarnessID, "harness_fp_display": FingerprintDisplay(sess.HarnessPub),
		"host": sess.Host, "os": sess.OS, "harness_version": sess.HarnessVersion, "project": project,
		"online": online, "viewers": viewers,
		"started_at_ms": ms(sess.StartedAt), "expires_at_ms": ms(sess.ExpiresAt),
		"last_activity_at_ms": ms(sess.LastActivityAt), "protocol_version": sess.ProtocolVersion,
		// #2389, additive: state, end reason, model, devices and current ticket.
		"state": sess.State, "end_reason": sess.EndReason, "ended_at_ms": endedAt,
		"model": sess.Model, "ticket": ticket, "devices": devOut,
	}
	if withPub {
		v["harness_pub"] = sess.HarnessPub
	}
	return v
}

// listSessions is GET /api/sessions: the caller's own live sessions, then
// the ones that ended in the last 7 days (#2389).
func (rl *Relay) listSessions(w http.ResponseWriter, r *http.Request, p authz.Principal) {
	list, err := rl.repo.ListRecentRemoteSessions(r.Context(), p.UserID, domain.RemoteListEndedFor, domain.RemoteListEndedMax)
	if err != nil {
		writeErr(w, err)
		return
	}
	devices := map[string][]domain.RemoteDevice{}
	if len(list) > 0 {
		all, err := rl.repo.ListRemoteDevices(r.Context(), p.UserID, "")
		if err != nil {
			writeErr(w, err)
			return
		}
		for _, s := range list {
			devices[s.HarnessID] = nil // listed, even with no devices
		}
		for _, d := range all {
			if _, ok := devices[d.HarnessID]; ok {
				devices[d.HarnessID] = append(devices[d.HarnessID], d)
			}
		}
	}
	names := map[int]string{}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, rl.sessionView(r.Context(), &list[i], names, devices, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// getSession is GET /api/sessions/:id, with the harness's public key.
func (rl *Relay) getSession(w http.ResponseWriter, r *http.Request, p authz.Principal) {
	sess, ok := rl.ownSession(w, r, p)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rl.sessionView(r.Context(), sess, map[int]string{}, nil, true))
}

// eventStream is GET /api/sessions/:id/events: replay from the buffer
// (Last-Event-ID, else from_seq, else the oldest buffered event), then live
// events, presence and keepalives.
func (rl *Relay) eventStream(w http.ResponseWriter, r *http.Request, p authz.Principal) {
	ctx := r.Context()
	if err := rl.checkOrigin(r, false); err != nil {
		writeErr(w, err)
		return
	}
	sess, ok := rl.ownSession(w, r, p)
	if !ok {
		return
	}
	var start int64
	hasStart := false
	if s := r.Header.Get("Last-Event-ID"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
			start, hasStart = n+1, true
		}
	}
	if s := r.URL.Query().Get("from_seq"); !hasStart && s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
			start, hasStart = n, true
		}
	}

	live := rl.reg.Ensure(sess)
	now := rl.cfg.Now()
	st := newViewerStream(rl.userBytes.acquire(sess.OwnerUserID), rl.viewerBytes)
	defer rl.userBytes.release(sess.OwnerUserID) // after st.drain below
	live.mu.Lock()
	if live.ended {
		live.mu.Unlock()
		writeErr(w, sessionEnded(nil))
		return
	}
	if len(live.viewers) >= MaxViewers {
		live.mu.Unlock()
		writeErr(w, errf(429, "too_many_viewers", "this session already has %d viewers", MaxViewers).with("retry_after_s", 10))
		return
	}
	if ok, wait := live.attaches.take(now); !ok {
		live.mu.Unlock()
		writeErr(w, errf(429, "rate_limited", "too many attaches; wait a moment").with("retry_after_s", retryAfter(wait)))
		return
	}
	live.updatePresence(now, rl.cfg.OfflineAfter)
	live.evict(now)
	oldest := live.oldest()
	nullable := func(n int64) any {
		if n == 0 {
			return nil
		}
		return n
	}
	initial := []frame{relayFrame(map[string]any{
		"type": "attached", "online": live.online, "oldest_seq": nullable(oldest), "latest_seq": nullable(live.acceptedThrough),
	})}
	for _, snap := range [][]byte{live.hello, live.status} {
		if snap != nil {
			initial = append(initial, frame{event: "snapshot", data: snap})
		}
	}
	if hasStart {
		switch {
		case oldest > 0 && start < oldest:
			initial = append(initial, relayFrame(map[string]any{"type": "gap", "requested_seq": start, "oldest_seq": oldest}))
		case oldest == 0 && live.acceptedThrough > 0 && start <= live.acceptedThrough:
			initial = append(initial, relayFrame(map[string]any{"type": "gap", "requested_seq": start, "oldest_seq": live.acceptedThrough + 1}))
		}
	}
	for _, e := range live.ring {
		if !hasStart || e.seq >= start {
			initial = append(initial, frame{id: e.seq, event: "envelope", data: e.data})
		}
	}
	if hasStart && start > live.acceptedThrough {
		st.want = start // still ahead of the buffer: a gap can only show on the next event
	}
	live.viewers[st] = struct{}{}
	live.tellHarnessViewers()
	live.mu.Unlock()

	rl.svc.RecordRemote(ctx, p, sess, service.RemoteAudit{Action: domain.AuditRemoteAttach, Op: "attach"})
	if _, err := rl.repo.TouchRemoteActivity(ctx, sess.ID); err != nil {
		slog.Warn("recording remote activity failed", "session", sess.ID, "err", err)
	}

	out := rl.startSSE(w)
	ok = out.raw("retry: 3000\n\n") == nil
	for _, f := range initial {
		if !ok {
			break
		}
		ok = out.write(f) == nil
	}
	if ok {
		rl.pump(ctx, out, st)
	}

	live.mu.Lock()
	if _, held := live.viewers[st]; held {
		delete(live.viewers, st)
		live.tellHarnessViewers()
	}
	live.mu.Unlock()
	st.close()
	st.drain()
}

// pinLookup reads the relay's record of a device (remote_devices) for the
// early command checks; revoked devices are unknown.
func (rl *Relay) pinLookup(ctx context.Context, sess *domain.RemoteSession) func(string) *DevicePin {
	return func(deviceID string) *DevicePin {
		d, err := rl.repo.GetRemoteDevice(ctx, sess.OwnerUserID, sess.HarnessID, deviceID)
		if err != nil || d.RevokedAt != nil {
			return nil
		}
		return &DevicePin{DevicePub: d.DevicePub, UserID: d.OwnerUserID, PinnedUntilMS: ms(d.ExpiresAt)}
	}
}

// commandStatus is the HTTP status of a relay refusal (protocol 8.3).
func commandStatus(code string) int {
	switch code {
	case CodeTooLarge:
		return 413
	case CodeUnknownDevice, CodeDeviceExpired, CodeBadSignature:
		return 403
	}
	return 400
}

// postCommand is POST /api/sessions/:id/commands: one signed envelope,
// checked (5.6 steps 1-10) and forwarded unchanged (compacted) to the
// harness's command stream. Never queued: an offline harness is a 409.
func (rl *Relay) postCommand(w http.ResponseWriter, r *http.Request, p authz.Principal) {
	ctx := r.Context()
	if err := rl.checkOrigin(r, true); err != nil {
		writeErr(w, err)
		return
	}
	if err := requireJSON(r); err != nil {
		writeErr(w, err)
		return
	}
	sess, ok := rl.ownSession(w, r, p)
	if !ok {
		return
	}
	live := rl.reg.Ensure(sess)
	body, err := readBody(w, r, MaxCommandBytes)
	if err != nil {
		rl.refused(ctx, p, sess, live, nil, CodeTooLarge)
		writeErr(w, err)
		return
	}
	now := rl.cfg.Now()
	code, env := CheckCommand(body, CommandReceiver{
		NowMS: ms(now), UserID: sess.OwnerUserID, SessionID: sess.ID, HarnessID: sess.HarnessID,
		Pin: rl.pinLookup(ctx, sess),
	})
	if code != CodeOK {
		rl.refused(ctx, p, sess, live, env, code)
		writeErr(w, errf(commandStatus(code), code, "command refused: %s", code))
		return
	}
	var compact bytes.Buffer
	if json.Compact(&compact, body) != nil {
		writeErr(w, errf(400, CodeMalformed, "command refused: malformed"))
		return
	}

	live.mu.Lock()
	limiter := live.ops
	if env.Op == "prompt" {
		limiter = live.prompts
	}
	if ok, wait := limiter.take(now); !ok {
		live.mu.Unlock()
		rl.refused(ctx, p, sess, live, env, "rate_limited")
		writeErr(w, errf(429, "rate_limited", "too many %s commands; wait a moment", env.Op).with("retry_after_s", retryAfter(wait)))
		return
	}
	sent := live.isOnline(now, rl.cfg.OfflineAfter) && live.harness.send(frame{event: "command", data: compact.Bytes()})
	live.mu.Unlock()
	if !sent {
		writeErr(w, errf(409, "harness_offline", "the laptop isn't connected; nothing was sent"))
		return
	}

	a := service.RemoteAudit{Op: env.Op, DeviceID: *env.DeviceID}
	switch env.Op {
	case "prompt":
		text, _ := env.Body["text"].(string)
		a.Action = domain.AuditRemotePrompt
		a.PromptLen, a.PromptSHA256 = service.PromptDigest(text)
	case "stop":
		a.Action = domain.AuditRemoteStop
	case "detach":
		a.Action = domain.AuditRemoteDetach
	}
	if a.Action != "" {
		rl.svc.RecordRemote(ctx, p, sess, a)
	}
	if _, err := rl.repo.TouchRemoteActivity(ctx, sess.ID); err != nil {
		slog.Warn("recording remote activity failed", "session", sess.ID, "err", err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "forwarded"})
}

// refused audits a relay refusal, sampled to one per minute per session and
// code. env may be nil.
func (rl *Relay) refused(ctx context.Context, p authz.Principal, sess *domain.RemoteSession, live *Live, env *Envelope, code string) {
	now := rl.cfg.Now()
	live.mu.Lock()
	last, seen := live.refused[code]
	if seen && now.Sub(last) < refusedAuditGap {
		live.mu.Unlock()
		return
	}
	live.refused[code] = now
	live.mu.Unlock()
	a := service.RemoteAudit{Action: domain.AuditRemoteRefused, Op: "command", Code: code}
	if env != nil {
		a.Op = env.Op
		if env.DeviceID != nil && len(*env.DeviceID) <= 64 {
			a.DeviceID = *env.DeviceID
		}
	}
	if len(a.Op) > 32 {
		a.Op = a.Op[:32]
	}
	slog.Info("remote command refused", "session", sess.ID, "code", code)
	rl.svc.RecordRemote(ctx, p, sess, a)
}

// clipRunes cuts s to at most n bytes on a rune boundary.
func clipRunes(s string, n int) string {
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// pair is POST /api/sessions/:id/pair (protocol 7.4): checked against the
// harness's pairing window, counted, audited (fail closed) and forwarded with
// the verified caller's identity. The result arrives as a pair_result event.
func (rl *Relay) pair(w http.ResponseWriter, r *http.Request, p authz.Principal) {
	ctx := r.Context()
	if err := rl.checkOrigin(r, true); err != nil {
		writeErr(w, err)
		return
	}
	if err := requireJSON(r); err != nil {
		writeErr(w, err)
		return
	}
	sess, ok := rl.ownSession(w, r, p)
	if !ok {
		return
	}
	body, err := readBody(w, r, MaxSmallBody)
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := parseStrict(body)
	m, _ := v.(map[string]any)
	req, _ := m["request"].(map[string]any)
	mac, macOK := m["mac"].(string)
	sig, sigOK := m["sig"].(string)
	if err != nil || req == nil || !macOK || !sigOK {
		writeErr(w, errf(400, CodeMalformed, `send {"request", "mac", "sig"}`))
		return
	}
	if sid, _ := req["session_id"].(string); sid != sess.ID {
		writeErr(w, errf(400, CodeWrongSession, "the request is for another session"))
		return
	}
	pid, _ := req["pairing_id"].(string)
	devID, _ := req["device_id"].(string)
	devPub, _ := req["device_pub"].(string)
	name, _ := req["device_name"].(string)

	live := rl.reg.Ensure(sess)
	now := rl.cfg.Now()
	live.mu.Lock()
	win := live.pairing
	var perr *apiError
	switch {
	case win == nil || win.id != pid:
		perr = errf(409, "pairing_unavailable", "this laptop isn't waiting for that pairing; run /remote pair there")
	case !now.Before(win.expires):
		perr = errf(409, "pairing_expired", "the pairing link has expired; run /remote pair on the laptop")
	case win.attempts >= PairingTries:
		perr = errf(429, "pairing_locked", "too many pairing attempts; run /remote pair on the laptop").
			with("retry_after_s", retryAfter(win.expires.Sub(now)))
	case !live.isOnline(now, rl.cfg.OfflineAfter):
		perr = errf(409, "harness_offline", "the laptop isn't connected")
	}
	if perr != nil {
		live.mu.Unlock()
		writeErr(w, perr)
		return
	}
	win.attempts++
	expires := win.expires
	live.mu.Unlock()

	if err := rl.svc.RecordRemotePair(ctx, p, sess, clipRunes(devID, 64)); err != nil {
		writeErr(w, err) // 503 audit_unavailable: fail closed, nothing forwarded
		return
	}
	data := canonical(nil, map[string]any{
		"request": req, "mac": mac, "sig": sig,
		"relay": map[string]any{"user_id": p.UserID, "email": p.Email, "user_agent": clipRunes(r.UserAgent(), 200)},
	})
	live.mu.Lock()
	sent := live.harness != nil && live.harness.send(frame{event: "pair_request", data: data})
	if sent {
		if raw, err := b64uDecode(devPub); err == nil && Fingerprint(raw) == devID {
			live.pending[devID] = pendingPair{pairingID: pid, devicePub: devPub, name: clipRunes(name, 256)}
		}
	}
	live.mu.Unlock()
	if !sent {
		writeErr(w, errf(409, "harness_offline", "the laptop isn't connected"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending", "expires_at_ms": ms(expires)})
}

// disconnect is POST /api/sessions/:id/disconnect: ends the session whether
// or not the harness is online (reason disconnected); the harness gets `end`.
func (rl *Relay) disconnect(w http.ResponseWriter, r *http.Request, p authz.Principal) {
	ctx := r.Context()
	if err := rl.checkOrigin(r, true); err != nil {
		writeErr(w, err)
		return
	}
	sess, ok := rl.ownSession(w, r, p)
	if !ok {
		return
	}
	if _, err := rl.svc.EndRemoteSession(ctx, p, sess.ID, domain.RemoteEndDisconnected); err != nil {
		writeErr(w, err)
		return
	}
	rl.svc.RecordRemote(ctx, p, sess, service.RemoteAudit{Action: domain.AuditRemoteDetach, Op: "disconnect"})
	w.WriteHeader(http.StatusNoContent)
}

// revokeAll is POST /api/sessions/revoke-all: every harness of the caller
// unpins all devices (device_revoked {all}), then every live session ends
// (reason revoked) and the caller's device rows are revoked.
func (rl *Relay) revokeAll(w http.ResponseWriter, r *http.Request, p authz.Principal) {
	ctx := r.Context()
	if err := rl.checkOrigin(r, true); err != nil {
		writeErr(w, err)
		return
	}
	list, err := rl.repo.ListActiveRemoteSessions(ctx, p.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	for i := range list {
		rl.deviceRevoked(ctx, &list[i]) // on the replica holding it
	}
	ended, devices, err := rl.svc.RevokeAllRemote(ctx, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ended": len(ended), "devices_revoked": devices})
}
