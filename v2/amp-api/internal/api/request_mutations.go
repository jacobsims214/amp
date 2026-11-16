package api

// Team request writes over REST (epic 202, story 549, AMP #3971; build slice
// F2's write half of discoveries/team-inbox-approved-build-slices.md; approved
// product contract artifact 36 v2 §1, §2 and §5).
//
//	PATCH /api/teams/:id/requests/:request              the submitter's versioned edit
//	POST  /api/teams/:id/requests/:request/comments     one row of discussion
//	POST  /api/teams/:id/requests/:request/claim        "I am looking at it now"
//	POST  /api/teams/:id/requests/:request/release      end my own claim
//	POST  /api/teams/:id/requests/:request/assign       a manager's explicit transfer
//
// Every one of them is a thin adapter over the one authorized write path
// (internal/service/request_mutations.go, slice S3/S4: EditRequest,
// CommentOnRequest, ClaimRequest, ReleaseRequestClaim, AssignRequest) — the same
// five methods the MCP tools call (internal/mcp/request_work.go, slice I2). It
// decodes a bounded, strict body, maps the transport's own facts onto the
// service's input and encodes the service's answer. It decides nothing: who may
// edit, when the edit window closes, who may claim or assign, whose voice a
// discussion row speaks in, what a stale revision or a lost claim race means,
// and every audit row are the service's, so the codes a client branches on are
// the ones the MCP tools answer with.
//
// # The path shape is the reads' and the service's
//
// A write names its team as well as its request, exactly like the reads beside
// it and the download mint below them. That is not decoration: the service's
// every mutation loads the team first, because the team row is where the org
// half of the tenant comes from (loadRequestForMutation), and an id-only
// mutation route would need a second service method that resolves a request to
// its tenant — a new decision surface this slice must not add. It also keeps
// "another team's request" indistinguishable from "no such request": the
// request is loaded inside the named team's tenant, so it is not found rather
// than found-and-refused.
//
// The :request segment is the serial, never the public reference. A reference is
// the requester's way of naming a *read* (GET /api/teams/{id}/requests/{request}
// accepts either), and the write path addresses rows by id; a reference in a
// write path is refused by name rather than searched for, so a caller is told
// which spelling this route takes instead of getting a 404 for a request that
// exists.
//
// # Known gap: a customer cannot address their own request yet (F4's, not this)
//
// The edit is the only act here that a requester may perform, and it is
// reachable today only by someone who can see the serial — a team member who
// submitted to their own inbox, or a requester handed the id out of band. The
// requester-facing answer (`domain.RequesterRequestView`, GET /api/requests/:ref)
// deliberately carries neither the serial nor `rev`: the projection is a read
// shape, and its whitelist is E3's decision, not this file's. So a customer with
// only their `public_ref` cannot build this path, and could learn a `rev` only
// from a 409. That is a real hole in the *requester's* addressing, and the slice
// that owns it is F4 (`POST /api/requests/:id/answer`, `…/accept`, `…/reopen` —
// "requester routes over the same service layer"), which must give the
// requester surface its own way of naming a request and a revision. It is NOT
// fixed here by accepting a reference on a write path: that would put a second
// tenant-scoped lookup and a second definition of what a reference may name in
// the adapter, which is the one thing this file exists not to do. The MCP tool
// has the same shape (`amp_update_request` takes team_id + request_id), so the
// gap is in the shared service surface's addressing, not in either adapter.
//
// # If-Match is the HTTP spelling of expected_rev
//
// A request write is a compare-and-set, and REST says so with If-Match
// (#3657's gotcha 8: the write precondition is new to this surface). The PATCH
// route requires a revision — in the If-Match header, or as expected_rev in the
// body — and refuses a payload that names neither (the service refuses a
// non-positive revision itself, with a message naming the field, so the adapter
// passes 0 through rather than repeating the rule). The three triage acts take a
// revision when the caller has one: zero is the service's "whatever the row is
// now", which is what a replay-tolerant caller passes, and a value the caller
// read turns the act into a compare-and-set.
//
//   - `If-Match: "7"` and `If-Match: 7` are both accepted: a client that models
//     the revision as an entity tag and one that sends the bare number mean the
//     same thing, and refusing the second would make the route awkward for
//     nothing.
//   - `If-Match: *` is REFUSED, not honoured. It means "any current
//     representation", which is precisely the lost update the revision exists to
//     prevent; the refusal names the header and tells the caller to send the
//     revision it read.
//   - A weak validator (`W/"7"`) is refused too: If-Match is compared strongly
//     by definition, so accepting a weak tag would claim a guarantee HTTP does
//     not give it.
//   - Header and body together are allowed only when they agree. Two spellings
//     of one precondition that disagree are a caller bug, and picking one of
//     them would silently drop the other's protection.
//
// # Conflicts carry the facts a client retries with
//
// A stale revision and a lost claim race are both 409 request.conflict, and both
// carry the thing the client needs next to the message: `rev` (the revision the
// row is actually at, so a retry needs no extra read) and `claim_user_id` (who
// holds the claim, so the loser can be told). The service's refusals already
// expose both through their typed errors — that is what its codedCause exists
// for — and this file reads them off the error rather than parsing its own
// message. Nothing else is added to a refusal, and no refusal names content.
//
// # The answer is the act, not the record
//
// A write answers with the request's identity and where it now stands
// (id, public_ref, state, rev) plus `changed` — never the stored record. The
// reason is the edit path's own caller: the submitter may be a customer who is
// not on the team, and a `domain.TeamRequest` also carries the org id, the triage
// claim, the assignee, the contact snapshot, connector metadata and a reject
// reason, none of which is theirs to see (contract §2; the requester projection
// is slice E3's). One envelope for every caller is therefore the safe one, and a
// triager's UI re-reads the record it is rendering — which it must do anyway,
// since the claim and assignee it shows are two fields this answer does not
// carry.
//
// The discussion answer is the same rule one step further: the stored comment's
// id, kind, body and time, with NO author. A thread's attribution is the
// service's decision and the requester's projection reduces every triager to
// "the desk"; a row's author label and user id must not reach a requester through
// a write acknowledgement. That matters on the retry path in particular: a
// `retry_of_comment_id` that names a row this caller did not write is answered
// with that row (the repository's replay rule), and echoing its author would name
// a team member to the person they are answering.
//
// # What this file deliberately does NOT expose
//
// No lifecycle, delivery, acceptance, answer, reopen, attach or attachment
// route. Each is another slice's (F3/F4/F5) and each has its own gate; this file
// registers five routes and all five are the mutations above. The submit-scope
// gate is what keeps a submit-only service credential out of every one of them
// (token_scope.go allows exactly POST /api/teams/{id}/requests), and because the
// gate sits inside denied(), its refusal is audited like every other 403 on this
// surface. A successful write is audited once, by the service, under
// request.update / request.claim / request.release / request.assign /
// request.comment — and a replay writes and audits nothing.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// maxRequestMutationBody bounds a write body before the JSON decoder reads it.
//
// It is the submission route's bound (request_submit.go's maxRequestSubmitBody)
// for the same reason: the domain's own limits are the real validation (a
// comment body is bounded like a brief at 20000 characters, a subject at 200),
// and this is only the memory guard in front of them — set well above the
// largest body those bounds allow, so a valid write is never refused for its
// size. An over-bound body is a coded 400 naming the bound rather than a 413:
// the request.* vocabulary has no "too large" code, and request.invalid is what
// "this payload cannot be a write" means.
const maxRequestMutationBody = 256 << 10

// requestEditBody is what a PATCH may send: the two fields a submitter can move,
// the two it cannot, and its precondition.
//
// There is deliberately no actor, org, team, state, claim, assignee, rev-to-write
// or requester field — the identity comes from the authenticated principal, the
// tenant from the path's team row, the state from the lifecycle, and the
// revision to write from the repository's compare-and-set. An unknown field is
// refused rather than ignored, so a client that sends `state` or `actor` is told.
//
// OutcomeBrief and Metadata are decoded precisely so a payload naming them is
// ANSWERED: the service refuses both by name (the outcome brief is the delivery
// transition's, metadata is the team's and its integrations'), and a field that
// vanished between the wire and the write would tell its author something false.
type requestEditBody struct {
	Subject      *string              `json:"subject"`
	InputBrief   *string              `json:"input_brief"`
	OutcomeBrief *string              `json:"outcome_brief"`
	Metadata     domain.MetadataPatch `json:"metadata"`
	ExpectedRev  *int                 `json:"expected_rev"`
}

// requestDiscussionBody is one row of discussion: what is said, in which voice,
// and the id a retried post is answering.
//
// Kind is passed through unnormalized: the service normalizes it through the
// domain's own rule and refuses an unknown kind with request.invalid, so there is
// one implementation of "which kinds exist" and this file cannot drift from it.
type requestDiscussionBody struct {
	Kind             string `json:"kind"`
	Body             string `json:"body"`
	RetryOfCommentID int64  `json:"retry_of_comment_id"`
}

// requestPreconditionBody is the whole body the three triage acts accept: a
// revision, and nothing else. A claim is always for the caller (claiming for
// someone else is an assignment), so there is no field for a claimant, a holder,
// an actor or a state; a payload carrying one is refused by the decoder rather
// than quietly dropped.
type requestPreconditionBody struct {
	ExpectedRev *int `json:"expected_rev"`
}

// requestAssignBody is a transfer: who owns the request, and the precondition.
// Assignee is a user id, an email, "me" or "none"/"unassigned" — resolved and
// authorized by the service, never here.
type requestAssignBody struct {
	Assignee    string `json:"assignee"`
	ExpectedRev *int   `json:"expected_rev"`
}

// requestMutationFacts is what a write answers with: which request, where it now
// stands, at what revision. It is a whitelist, not the record (see the file
// header): no org, no claim, no assignee, no contact, no metadata, no reject
// reason.
//
// Rev is here for both views, and it is the one internal-looking number a
// requester gets. It has to be: an edit is a compare-and-set, so a form saved
// twice needs the revision the first save produced, and the requester projection
// deliberately carries no revision (it is a read shape, not a concurrency
// token). A revision says only "this row has been written N times", which the
// 409 already tells them.
type requestMutationFacts struct {
	ID        int                  `json:"id"`
	PublicRef string               `json:"public_ref"`
	State     domain.RequestStatus `json:"state"`
	Rev       int                  `json:"rev"`
}

// requestMutationAnswer is the answer to a write: the request's facts and whether
// this call actually changed anything.
//
// Changed is false for a replay — an edit whose fields already had those values,
// a claim the caller already holds, an assignee already set, a release with no
// claim in force. The service wrote, bumped and audited nothing in that case, and
// the flag is how a client tells "done" from "already was".
type requestMutationAnswer struct {
	Request requestMutationFacts `json:"request"`
	Changed bool                 `json:"changed"`
}

// requestCommentAnswer is the answer to a discussion row: the stored row's own
// facts and whether this call wrote it.
//
// There is no author here, and that is a decision (file header): the retry path
// can answer with a row this caller did not write, and a requester must never be
// handed a triager's name. A triager posting knows who they are; the thread read
// is where attribution is shown, under the view rules that own it.
type requestCommentAnswer struct {
	Comment requestCommentFacts `json:"comment"`
	// Wrote distinguishes a new row (201) from a retry answered with the row the
	// earlier attempt wrote (200).
	Wrote bool `json:"wrote"`
}

// requestCommentFacts is one discussion row as a write answers it.
type requestCommentFacts struct {
	ID        int64                     `json:"id"`
	Kind      domain.RequestCommentKind `json:"kind"`
	Body      string                    `json:"body"`
	CreatedAt time.Time                 `json:"created_at"`
}

// ---- The edit ----

// teamRequestEdit serves PATCH /api/teams/:id/requests/:request: the submitter's
// versioned edit of their own request, while it is still untriaged.
//
// The adapter's whole job is the transport: the body is bounded and strict, the
// revision is read from If-Match or expected_rev, and the service is handed the
// two fields a submitter may write plus the two it may not (so their presence is
// answered instead of dropped).
func (h *RestHandler) teamRequestEdit(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	requestID, err := requestMutationTarget(r)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var body requestEditBody
	if err := decodeRequestMutation(w, r, &body, "request edit"); err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	rev, err := requestWriteRev(r, body.ExpectedRev)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	updated, changed, err := h.svc.EditRequest(r.Context(), p, teamID, requestID, rev, service.RequestEditInput{
		Subject:      body.Subject,
		InputBrief:   body.InputBrief,
		OutcomeBrief: body.OutcomeBrief,
		Metadata:     body.Metadata,
	})
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	writeRequestMutation(w, updated, changed)
}

// ---- The discussion ----

// teamRequestComment serves POST /api/teams/:id/requests/:request/comments: one
// row in the caller's own voice.
//
// 201 when this call wrote the row, 200 when it answered a retry with the row the
// earlier attempt wrote — the same split the submission route uses for a create
// versus a replay, so a client can tell the two apart without reading the body.
func (h *RestHandler) teamRequestComment(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	requestID, err := requestMutationTarget(r)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var body requestDiscussionBody
	if err := decodeRequestMutation(w, r, &body, "request comment"); err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	stored, wrote, err := h.svc.CommentOnRequest(r.Context(), p, teamID, requestID, service.RequestDiscussionInput{
		// The kind is the caller's; the service normalizes it and refuses an
		// unknown one, and its voice table refuses a known one posted out of
		// turn (403).
		Kind:             domain.RequestCommentKind(strings.TrimSpace(body.Kind)),
		Body:             body.Body,
		RetryOfCommentID: body.RetryOfCommentID,
	})
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	if stored == nil {
		fail(w, errorf("the comment write answered no row"), http.StatusInternalServerError)
		return
	}
	if wrote {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}
	jsonOK(w, requestCommentAnswer{
		Comment: requestCommentFacts{
			ID:        stored.ID,
			Kind:      stored.Kind,
			Body:      stored.Body,
			CreatedAt: stored.CreatedAt,
		},
		Wrote: wrote,
	})
}

// ---- The three triage acts ----

// teamRequestClaim serves POST /api/teams/:id/requests/:request/claim.
func (h *RestHandler) teamRequestClaim(w http.ResponseWriter, r *http.Request) {
	h.requestClaimAct(w, r, false)
}

// teamRequestRelease serves POST /api/teams/:id/requests/:request/release.
func (h *RestHandler) teamRequestRelease(w http.ResponseWriter, r *http.Request) {
	h.requestClaimAct(w, r, true)
}

// requestClaimAct is the claim and the release: one act each, one shape. They
// share everything but the service call, and keeping them one function is what
// keeps their preconditions, bodies and answers from drifting.
func (h *RestHandler) requestClaimAct(w http.ResponseWriter, r *http.Request, release bool) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	requestID, err := requestMutationTarget(r)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	rev, err := requestMutationPrecondition(w, r)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	var updated *domain.TeamRequest
	var changed bool
	if release {
		updated, changed, err = h.svc.ReleaseRequestClaim(r.Context(), p, teamID, requestID, rev)
	} else {
		updated, changed, err = h.svc.ClaimRequest(r.Context(), p, teamID, requestID, rev)
	}
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	writeRequestMutation(w, updated, changed)
}

// teamRequestAssign serves POST /api/teams/:id/requests/:request/assign: a team
// manager's explicit transfer, which also clears any active claim.
func (h *RestHandler) teamRequestAssign(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	requestID, err := requestMutationTarget(r)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var body requestAssignBody
	if err := decodeRequestMutation(w, r, &body, "request assignment"); err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	rev, err := requestWriteRev(r, body.ExpectedRev)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	updated, changed, err := h.svc.AssignRequest(r.Context(), p, teamID, requestID, body.Assignee, rev)
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	writeRequestMutation(w, updated, changed)
}

// ---- Decoding, preconditions and answers ----

// requestMutationTarget reads the :request path value of a write route: the
// request's serial, and nothing else.
//
// The reads beside this one accept the serial or the public reference, because a
// requester holds only the reference. A write does not: the mutation service
// addresses rows by id, and resolving a reference to an id here would mean a
// tenant-scoped lookup the service already does again — and a second place that
// decides what a reference may name. So a value that is not a positive whole
// number is refused with request.invalid naming the rule, never searched for and
// never answered with a 404 for a request that exists.
func requestMutationTarget(r *http.Request) (int, error) {
	segment := strings.TrimSpace(r.PathValue("request"))
	if segment == "" {
		return 0, authz.RequestInvalid("name the request by its serial")
	}
	id, err := strconv.Atoi(segment)
	if err != nil || id <= 0 {
		return 0, authz.RequestInvalid("a request write addresses the request by its serial, a whole number: " +
			"GET /api/teams/{id}/requests/{request} accepts the public reference too and answers the serial")
	}
	return id, nil
}

// decodeRequestMutation reads one write body: bounded, strict about unknown
// fields, and one JSON value only.
//
// The three refusals are the submission route's, for the same reasons: an
// unknown field would otherwise be silently dropped (so a payload could carry
// `actor`, `state` or `claim_user_id` and believe it took effect), an oversized
// body is refused without being read, and a second JSON value leaves the client
// unsure what it sent. An empty body is not an error here: the triage acts take
// no fields, and the edit's own "expected_rev is required" (the service's
// message) is the answer a caller can act on.
func decodeRequestMutation(w http.ResponseWriter, r *http.Request, into any, what string) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestMutationBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return requestBodyRefusal(err, what)
	}
	// One JSON value only. Trailing garbage is not a write either, and ignoring it
	// would leave the client unsure what it actually sent. The bound is checked
	// here too: a body that is one value followed by padding only reaches its
	// limit on this second read, and reporting that as "two JSON values" would
	// name the wrong mistake.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return requestBodyTooBig()
		}
		return authz.RequestInvalid("the body holds more than one JSON value: send one")
	}
	return nil
}

// requestBodyRefusal is the coded 400 for a body the decoder could not read: an
// over-bound body names the bound, and anything else says what the decoder
// objected to (an unknown field, a wrong type, malformed JSON).
func requestBodyRefusal(err error, what string) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return requestBodyTooBig()
	}
	return authz.RequestInvalid("the body is not a %s: %s", what, err)
}

// requestBodyTooBig is the one refusal for a body over the transport's bound.
func requestBodyTooBig() error {
	return authz.RequestInvalid("the body is over %d bytes: a request write carries its own fields only", maxRequestMutationBody)
}

// requestWriteRev merges the two spellings of a write precondition — the
// If-Match header and the body's expected_rev — into the one revision the service
// takes. Zero means "no revision given", which the edit refuses (in the service,
// naming the field) and the triage acts read as "the revision this call reads".
func requestWriteRev(r *http.Request, bodyRev *int) (int, error) {
	headerRev, hasHeader, err := requestIfMatch(r)
	if err != nil {
		return 0, err
	}
	if bodyRev == nil {
		if hasHeader {
			return headerRev, nil
		}
		return 0, nil
	}
	if *bodyRev < 1 {
		return 0, authz.RequestInvalid("expected_rev must be a positive revision: send the rev you read, or omit it and send If-Match")
	}
	if hasHeader && headerRev != *bodyRev {
		return 0, authz.RequestInvalid("If-Match and expected_rev disagree (%d and %d): send the revision you read once, in one of the two", headerRev, *bodyRev)
	}
	return *bodyRev, nil
}

// requestMutationPrecondition reads the optional precondition of a triage act:
// If-Match, or expected_rev in the body, or neither.
func requestMutationPrecondition(w http.ResponseWriter, r *http.Request) (int, error) {
	var body requestPreconditionBody
	if err := decodeRequestMutation(w, r, &body, "request write"); err != nil {
		return 0, err
	}
	return requestWriteRev(r, body.ExpectedRev)
}

// requestIfMatch reads the If-Match header as a revision.
//
// The accepted forms and the refused ones are the file header's: a quoted or bare
// positive integer; not `*` (which would drop the compare-and-set the revision
// exists for), not a weak validator (If-Match is a strong comparison), and not
// anything else — a precondition that cannot be read must not be treated as
// absent, because the caller believes it protected the write.
func requestIfMatch(r *http.Request) (rev int, present bool, err error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, false, nil
	}
	if raw == "*" {
		return 0, true, authz.RequestInvalid(`If-Match: * cannot guard a request write: send the revision you read, as If-Match: "7" or expected_rev`)
	}
	value := raw
	if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && len(value) >= 2 {
		value = value[1 : len(value)-1]
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(value))
	if convErr != nil || n < 1 {
		return 0, true, authz.RequestInvalid(`If-Match must be the revision you read, as "7" or 7: it is a strong precondition, so * and weak validators are refused`)
	}
	return n, true, nil
}

// writeRequestMutation encodes one successful write's answer.
func writeRequestMutation(w http.ResponseWriter, updated *domain.TeamRequest, changed bool) {
	if updated == nil {
		// Unreachable through the service, which returns the stored row on every
		// success. Failing closed keeps a future caller from answering 200 for a
		// write that has nothing to show for itself.
		fail(w, errorf("the request write answered no request"), http.StatusInternalServerError)
		return
	}
	jsonOK(w, requestMutationAnswer{
		Request: requestMutationFacts{
			ID:        updated.ID,
			PublicRef: updated.PublicRef,
			State:     updated.State,
			Rev:       updated.Rev,
		},
		Changed: changed,
	})
}

// failRequestMutation writes a refusal with the status and code authz assigns it,
// and adds the one fact a conflict leaves the client needing: the revision the
// row is actually at, or the user holding the claim.
//
// The facts come off the service's typed error, not out of its message — the
// service keeps the repository's *RequestRevConflict / *RequestClaimHeld in the
// error chain for exactly this (requestMutationRefusal's codedCause), so a client
// can retry a stale edit without re-reading and a claim loser can be told who
// won. Both are only reachable by a caller already entitled to see the request,
// and neither names content.
func failRequestMutation(w http.ResponseWriter, err error) {
	var rev *repository.RequestRevConflict
	var held *repository.RequestClaimHeld
	hasRev, hasHeld := errors.As(err, &rev), errors.As(err, &held)
	if !hasRev && !hasHeld {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	body := map[string]any{"error": err.Error()}
	if c := authz.CodeOf(err); c != "" {
		body["code"] = c
	}
	if hasRev {
		body["rev"] = rev.Current
	}
	if hasHeld {
		body["claim_user_id"] = held.Holder
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(authz.StatusOf(err, http.StatusConflict))
	_ = json.NewEncoder(w).Encode(body)
}
