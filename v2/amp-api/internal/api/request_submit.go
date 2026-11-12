package api

// Team request submission over REST (epic 202, story 543, AMP #3919; build
// slice F1 of discoveries/team-inbox-approved-build-slices.md, submit half;
// approved product contract artifact 36 v2 §2 and §5).
//
//	POST /api/teams/:id/requests    submit a request to the team's inbox
//
// This is the thin REST adapter over Service.SubmitRequest (slice S1) — the
// same method the MCP tool `amp_submit_request` (slice I1) calls. It decodes a
// bounded, strict body, maps the transport's own facts onto the service's input
// and encodes the service's answer. It decides nothing: authorization,
// attribution, validation, the limiter, idempotency and the `request.submit`
// audit row are all the service's, so the codes a client branches on are the
// same ones the MCP surface answers with.
//
// The route is registered through deniedMux (see RestHandler.register), which
// is what makes the two shared boundaries apply without this file knowing about
// them:
//
//   - submitScopeGate refuses a submit-only service credential on every route
//     but its own bound team's POST (token_scope.go, AMP #3772), so a
//     credential reaches this handler only for the team it is bound to.
//   - denied() records a 403 as access.denied, so a refused submission is in
//     the audit log like every other refused request on this surface.
//
// A 403 is therefore audited and a 404 is not. That is deliberate: the 404 is
// the team surface's answer for a team the caller may not submit to
// (authz.TeamSubmit's NotFound, indistinguishable from a team that does not
// exist), and "not found" is not an access event — recording it would put a
// probe's enumeration attempts in the log while telling the prober nothing.
// A 400 is a client's malformed submission, not an access decision, and is not
// audited either (KB decisions/audit-log-v2.md); the successful submission is
// audited once by the service.
//
// What this file deliberately does NOT do — each one another slice's concern,
// and none of them reachable from this route:
//
//   - no list or read. GET /api/teams/:id/requests is a read of the inbox (slice
//     F2), not a submission; it stays unregistered here and reaches teamSub,
//     which answers 404 — and the scope gate refuses it outright for a
//     submit-only credential.
//   - no lifecycle. A submission lands in `new` and nothing here moves it; the
//     one transition function is slice S5.
//   - no project. Intake routes nothing (D9): neither the path nor the body
//     names a project, and the service writes no link row.
//   - no attachments. Bytes are a separate upload path with their own capture
//     (slice G2); this route carries text only.
//   - no identity, tenant or source override. The body has no actor, org,
//     contact, requester, team, project, state or key field, and an unknown
//     field is refused rather than silently dropped (see decodeSubmission).
//     The source kind is the transport's own fact (see requestSourceOf).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// maxRequestSubmitBody bounds a submission body before the JSON decoder reads
// it. The domain's own bounds are the real validation (subject 200 characters,
// input_brief 20000, metadata 50 keys of 1000 characters); this is the memory
// guard in front of them. It is set well above the largest body those bounds
// allow — about 75 KB before JSON escaping — so a valid submission is never
// refused for its size, and a body that cannot be a submission is refused
// without being read into memory.
//
// An over-bound body is the coded 400 request.invalid naming the bound rather
// than a 413: the request.* vocabulary has no "too large" code (slice B4's four
// are fixed) and the client's branch on request.invalid is what "this payload
// cannot be a submission" means. The bound is a memory guard, not a second
// validation layer: within it, the domain's character bounds decide.
const maxRequestSubmitBody = 256 << 10

// submissionBody is what a REST client may send: the request's own content and
// nothing else. There is deliberately no actor, org, contact, requester, team,
// project, state or idempotency-key field — the first seven are the service's
// (the authenticated principal and the team row are the only sources), the
// state belongs to the lifecycle and the key goes in the Idempotency-Key
// header. An unknown field is refused, not ignored, so a client that sends
// `actor` or `org_id` is told rather than quietly having it dropped.
type submissionBody struct {
	Subject string `json:"subject"`
	// InputBrief is the detail the team triages from; it is stored as the
	// requester's words quoted as data, never as instructions (D13).
	InputBrief string `json:"input_brief"`
	// Metadata is connector labels and routing hints only (domain.Metadata's
	// bounds apply); it is never an authorization decision and never a typed
	// relation.
	Metadata domain.Metadata `json:"metadata"`
}

// requestSourceOf is the surface a REST submission arrived on. It is the
// adapter's fact, read from the authenticated principal's channel rather than
// from the payload: a person in the web UI is `web`, and every other REST
// caller (an API token, or a Dex token not issued to the UI) is `api`. Nothing
// else is possible on this listener — a connector arrives through the webhook
// envelope (slice H2) and an MCP client through the tool (slice I1) — so no
// request can claim to have arrived somewhere it did not.
//
// The channel matters beyond the stored label: 061's idempotency-key scope is
// (org, team, source, submitter), so a browser's key and an API client's key
// are different keys. That is correct — they are different transports with
// different retry shapes — and a replay still resolves within either one.
func requestSourceOf(p authz.Principal) domain.RequestSourceKind {
	if p.Channel == auth.ChannelUI {
		return domain.RequestSourceWeb
	}
	return domain.RequestSourceAPI
}

// teamRequestSubmit serves POST /api/teams/:id/requests.
func (h *RestHandler) teamRequestSubmit(w http.ResponseWriter, r *http.Request) {
	teamID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonErr(w, errorf("invalid team id"), http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	// The body is read and bounded before the service is called, so a payload
	// that cannot be a submission costs neither a limiter unit nor a write.
	body, err := decodeSubmission(w, r)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.svc.SubmitRequest(r.Context(), p, service.SubmitRequestInput{
		// TeamID is the path's team and the only tenant field a submission
		// names; the org comes from the team row, never from the payload.
		TeamID: teamID,
		// The adapter's two facts: the surface this arrived on and the client's
		// own retry key. Neither can be supplied by the body.
		Source:         requestSourceOf(p),
		Subject:        body.Subject,
		InputBrief:     body.InputBrief,
		Metadata:       body.Metadata,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		// A 429 is the one refusal whose wait the client needs, so it is written
		// here from the service's own RetryAfterS rather than by fail(): the
		// header and the body's retry_after_s then cannot disagree with each
		// other or with the message. Every other refusal is fail()'s, with the
		// status and code authz assigns it; a 403 then reaches denied(), which
		// records it as access.denied.
		var limited *service.SubmitRateLimited
		if errors.As(err, &limited) {
			submitRateLimited(w, err, limited)
			return
		}
		fail(w, err, http.StatusInternalServerError)
		return
	}
	// 201 when this call wrote the request, 200 when it answered a replay with
	// the request the key already produced — the same split the service's result
	// reports and the MCP tool's `created`, so a client can tell "created" from
	// "already had it" without reading the body.
	if res.Created {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}
	jsonOK(w, submissionResponse{Request: submittedView(res.Request), Created: res.Created})
}

// submitRateLimited writes the 429 envelope: Retry-After, which an HTTP client
// honours without reading the body, and the same wait as retry_after_s next to
// the stable code, which is the shape the relay envelope and the MCP tool's
// error data already use — one number, three surfaces.
func submitRateLimited(w http.ResponseWriter, err error, limited *service.SubmitRateLimited) {
	w.Header().Set("Retry-After", strconv.Itoa(limited.RetryAfterS))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": err.Error(),
		// CodeOf finds request.rate_limited through SubmitRateLimited's Unwrap,
		// so the code is read off the error rather than restated here.
		"code":          authz.CodeOf(err),
		"retry_after_s": limited.RetryAfterS,
	})
}

// submissionResponse is the submission's answer: the caller's own request (the
// one this call wrote, or — for a replay — the one its key already produced) and
// whether this call created it. A replay is only answered when the caller may
// see the request (the service's requireReplayVisible), so nothing here widens
// who can read a request.
type submissionResponse struct {
	Request submittedRequest `json:"request"`
	// Created distinguishes a create from a replay. A replay wrote no row, no
	// history event and no audit row, and its limiter unit was refunded.
	Created bool `json:"created"`
}

// submittedRequest is what a submission answers with — a whitelist, not the
// stored record, for the same reason the MCP tool's answer is one (AMP #3920):
// a request row also carries its org, the triage claim, the assignee, its
// revision and a reject reason, which are the team's internal state a requester
// must never see (contract §2; the requester projection is slice E3's), and a
// replay can resolve to a row triagers have touched since it was written. So a
// client gets the opaque reference it will quote (D1), the state word, its own
// subject and team, how it arrived, and when — nothing derivable and nothing
// internal.
type submittedRequest struct {
	PublicRef  string                   `json:"public_ref"`
	State      domain.RequestStatus     `json:"state"`
	Subject    string                   `json:"subject"`
	TeamID     int                      `json:"team_id"`
	SourceKind domain.RequestSourceKind `json:"source_kind"`
	CreatedAt  time.Time                `json:"created_at"`
}

// submittedView projects the stored record onto that whitelist.
func submittedView(r *domain.TeamRequest) submittedRequest {
	return submittedRequest{
		PublicRef:  r.PublicRef,
		State:      r.State,
		Subject:    r.Subject,
		TeamID:     r.TeamID,
		SourceKind: r.SourceKind,
		CreatedAt:  r.CreatedAt,
	}
}

// decodeSubmission reads one submission from the request body. It is strict on
// purpose: the decoder rejects unknown fields (so a payload cannot smuggle an
// actor, an org or a contact in and have it ignored), the body is bounded by
// maxRequestSubmitBody before decoding (so an oversized body is refused without
// being read), and a second JSON value in the same body is refused rather than
// ignored.
//
// Every refusal is authz.RequestInvalid, so the client gets the stable
// request.invalid code next to the message and the message names what was
// wrong. An empty body is not a decode error: it is an empty submission, and
// the domain's own "subject is required" is the answer a caller can act on.
//
// A connector's `source_external_id` is refused by name rather than by the
// decoder's generic unknown-field message, because a caller that sent one is
// acting on a real rule (D12's vendor object id) and deserves to be told where
// it belongs: connectors arrive through the envelope (slice H2), and on this
// route — as on the MCP tool — a client's retries are the Idempotency-Key,
// which is scoped to the submitter. Accepting it here would also make the two
// submission surfaces disagree about their shape for no product gain.
func decodeSubmission(w http.ResponseWriter, r *http.Request) (submissionBody, error) {
	var body submissionBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestSubmitBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return body, nil
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return body, authz.RequestInvalid("the body is over %d bytes: a submission carries a subject, an input brief and its metadata", maxRequestSubmitBody)
		}
		if field := unknownField(err); connectorSubmitFields[field] {
			return body, authz.RequestInvalid("%q belongs to a connector's source identity, not to a REST submission: "+
				"a connector arrives through the webhook envelope, and a REST client's retries use the Idempotency-Key header", field)
		}
		return body, authz.RequestInvalid("the body is not a submission: %s", err)
	}
	// One JSON value only. Trailing garbage is not a submission either, and
	// ignoring it would leave the client unsure what it actually sent.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return body, authz.RequestInvalid("the body holds more than one JSON value: send one submission")
	}
	return body, nil
}

// connectorSubmitFields are the body fields this route refuses while naming
// them, rather than letting the decoder answer "unknown field". Each is a
// connector's source identity (D12), which the envelope adapter sets from the
// integration it served.
var connectorSubmitFields = map[string]bool{"source_external_id": true, "external_id": true}

// unknownField extracts the field name from encoding/json's
// `json: unknown field "x"` message, the only shape DisallowUnknownFields
// produces. An error it does not recognise yields "", which the caller treats
// as a plain malformed body.
func unknownField(err error) string {
	const marker = `unknown field "`
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j]
	}
	return ""
}
