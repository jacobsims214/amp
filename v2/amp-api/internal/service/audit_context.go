package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/reqctx"
)

// Audit log v2 capture (KB decisions/audit-log-v2.md, ticket #2427): every
// row records who acted (actor type, auth method, token or session id),
// from where (trusted client IP, user agent, client, request and trace id)
// and with what result (outcome, reason).

// maxAuditReason bounds AuditEntry.Reason.
const maxAuditReason = 500

// Why a row has no source_ip (details.ip_reason).
const (
	ipReasonSystem    = "system job: no client request"
	ipReasonNoRequest = "in-process call: no client request reached a listener"
)

// fillAuditContext sets e's v2 fields from the principal and the request
// that reached the listener, leaving any field the caller already set.
func fillAuditContext(ctx context.Context, p authz.Principal, e *domain.AuditEntry) {
	e.SchemaVersion = domain.AuditSchemaVersion
	req := p.Req
	if req == nil {
		req, _ = reqctx.FromContext(ctx)
	}
	if e.ActorType == "" {
		e.ActorType = auditActorType(p, req)
	}
	if e.AuthMethod == "" {
		e.AuthMethod = p.AuthMethod
		if e.AuthMethod == "" {
			e.AuthMethod = "none"
		}
	}
	if e.TokenID == nil && p.TokenID != 0 {
		id := p.TokenID
		e.TokenID = &id
	}
	if e.SessionID == "" {
		e.SessionID = p.SessionID
	}
	if e.OnBehalfOfUserID == nil && p.Agent && p.ManagedBy != nil {
		id := *p.ManagedBy
		e.OnBehalfOfUserID = &id
	}
	if e.Outcome == "" {
		e.Outcome = domain.AuditOutcomeSuccess
	}
	e.Reason = reqctx.Clip(e.Reason, maxAuditReason)
	e.Before, e.After = redact(e.Before), redact(e.After)

	if req != nil {
		if e.SourceIP == "" && req.SourceIP.IsValid() {
			e.SourceIP = req.SourceIP.String()
		}
		if e.UserAgent == "" {
			e.UserAgent = req.UserAgent
		}
		if e.RequestID == "" {
			e.RequestID = req.RequestID
		}
		if e.TraceID == "" {
			e.TraceID = req.TraceID
		}
	}
	if e.Client == "" {
		e.Client = auditClient(e.ActorType, e.AuthMethod, req)
	}

	// Say why there's no address, and keep the raw forwarding chain so the
	// resolution can be checked later.
	setDetail := func(k string, v any) {
		if e.Details == nil {
			e.Details = map[string]any{}
		}
		if _, set := e.Details[k]; !set {
			e.Details[k] = v
		}
	}
	switch {
	case e.SourceIP != "":
	case req != nil && req.IPReason != "":
		setDetail("ip_reason", req.IPReason)
	case e.ActorType == domain.AuditActorSystem:
		setDetail("ip_reason", ipReasonSystem)
	default:
		setDetail("ip_reason", ipReasonNoRequest)
	}
	if req != nil && req.XFF != "" {
		setDetail("xff", req.XFF)
	}
}

// auditActorType classifies who acted: an agent account, a person (or the
// local-dev user), nobody authenticated on a request (anonymous), or AMP
// itself (a background job, no request).
func auditActorType(p authz.Principal, req *reqctx.Info) string {
	switch {
	case p.Agent:
		return domain.AuditActorAgent
	case p.UserID != 0, p.AuthMethod == auth.AuthLocalDev:
		return domain.AuditActorHuman
	case req != nil:
		return domain.AuditActorAnonymous
	}
	return domain.AuditActorSystem
}

// auditClient names the surface a change came through, from the listener,
// how the caller authenticated and (for the harness) its user agent, which
// is client-asserted.
func auditClient(actorType, authMethod string, req *reqctx.Info) string {
	if req == nil {
		if actorType == domain.AuditActorSystem {
			return domain.AuditClientSystem
		}
		return ""
	}
	switch req.Listener {
	case reqctx.ListenerMCP:
		if strings.Contains(strings.ToLower(req.UserAgent), "amp-harness") {
			return domain.AuditClientHarness
		}
		return domain.AuditClientMCP
	case reqctx.ListenerRelay:
		return domain.AuditClientRelay
	case reqctx.ListenerMCPOAuth:
		return domain.AuditClientMCP
	case reqctx.ListenerREST, reqctx.ListenerAuthAdmin:
		switch authMethod {
		case auth.AuthDexSession, auth.AuthLocalDev:
			return domain.AuditClientWeb
		}
		return domain.AuditClientAPI
	}
	return ""
}

// ---- Refusals ----

// deniedWindow is how long auditDenied writes one row per (actor, action,
// target); repeats inside it are counted, not written.
const deniedWindow = 10 * time.Minute

var (
	deniedMu   sync.Mutex
	deniedSeen = map[string]*deniedState{}
)

type deniedState struct {
	until      time.Time
	suppressed int
}

// auditDenied records a refused action. action is the action that was
// attempted ("plan.approve"); the row's action is it with ".denied"
// appended, so a consumer that ignores outcome never reads a refusal as the
// change itself. e carries the target and any details; reason is the
// refusal (err's message, clipped, never secrets). One row per actor,
// action and target is written per 10 minutes: the next one written carries
// how many were skipped in details.suppressed.
func (s *Service) auditDenied(ctx context.Context, p authz.Principal, action string, e domain.AuditEntry, err error) {
	// The listener's generic access.denied hook skips a request whose
	// refusal is recorded here (even when the row below is rate-limited).
	req := p.Req
	if req == nil {
		req, _ = reqctx.FromContext(ctx)
	}
	req.MarkDeniedAudited()
	e.Action = deniedAction(action)
	e.Outcome = domain.AuditOutcomeDenied
	if e.Reason == "" && err != nil {
		var ae *authz.Error
		if errors.As(err, &ae) {
			e.Reason = ae.Msg
		} else {
			e.Reason = err.Error()
		}
	}
	key := fmt.Sprintf("%d|%s|%s|%s|%s:%s", p.UserID, p.Email, func() string {
		if p.Req != nil && p.Req.SourceIP.IsValid() && p.UserID == 0 {
			return p.Req.SourceIP.String() // anonymous: per address
		}
		return ""
	}(), e.Action, e.TargetType, e.TargetID)
	write, suppressed := deniedAllow(key, time.Now())
	if !write {
		return
	}
	if suppressed > 0 {
		if e.Details == nil {
			e.Details = map[string]any{}
		}
		e.Details["suppressed"] = suppressed
	}
	s.audit(ctx, p, e)
}

// deniedAllow reports whether a refusal under key is written at now, and
// how many were skipped since the last one written.
func deniedAllow(key string, now time.Time) (write bool, suppressed int) {
	deniedMu.Lock()
	defer deniedMu.Unlock()
	st, seen := deniedSeen[key]
	if seen && now.Before(st.until) {
		st.suppressed++
		return false, 0
	}
	if seen {
		suppressed = st.suppressed
	}
	deniedSeen[key] = &deniedState{until: now.Add(deniedWindow)}
	// Forget windows long past so the map stays small.
	if len(deniedSeen) > 10000 {
		for k, v := range deniedSeen {
			if now.After(v.until) {
				delete(deniedSeen, k)
			}
		}
	}
	return true, suppressed
}

// deniedAction is the action name a refusal is recorded under.
func deniedAction(action string) string {
	if strings.HasSuffix(action, ".denied") || strings.HasSuffix(action, ".fail") || strings.HasSuffix(action, ".refused") {
		return action
	}
	return action + ".denied"
}

// ---- Redaction ----

var secretKey = regexp.MustCompile(`(?i)secret|token|password|passwd|key|authorization|auth_?header|cookie|credential`)

// redactedValue replaces anything redact hides.
const redactedValue = "[redacted]"

// redact returns a copy of m safe to store in before/after: values under
// keys that look secret are replaced, and http(s) URLs keep only scheme and
// host (redactURL). Nested maps and lists
// are walked. nil stays nil.
func redact(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if secretKey.MatchString(k) {
			out[k] = redactedValue
			continue
		}
		out[k] = redactValue(v)
	}
	return out
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return redact(t)
	case []any:
		cp := make([]any, len(t))
		for i, x := range t {
			cp[i] = redactValue(x)
		}
		return cp
	case string:
		// The same rule as webhook descriptions: keep scheme and host only,
		// since a webhook URL's path or query is often itself a credential.
		if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
			return redactURL(t)
		}
		return t
	}
	return v
}
