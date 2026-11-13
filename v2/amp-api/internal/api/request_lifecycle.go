package api

// Team request lifecycle and requester acceptance over REST (epic 202, story
// 549, AMP #3973; build slices F3 and F4 of
// discoveries/team-inbox-approved-build-slices.md; approved product contract
// artifact 36 v2 §1, §2, §4 and §5).
//
//	POST /api/teams/:id/requests/:request/transition   the team's working step (to a named state)
//	POST /api/teams/:id/requests/:request/needs-info   ask the requester a question (I5)
//	POST /api/teams/:id/requests/:request/reject       decline, with a reason (I3)
//	POST /api/teams/:id/requests/:request/duplicate    fold into a canonical (I3, D7)
//	POST /api/teams/:id/requests/:request/reopen       the team reopens a closed request (D6)
//	POST /api/teams/:id/requests/:request/deliver      state the outcome exists (I2)
//	POST /api/requests/:request/answer                 the requester answers a question (I5)
//	POST /api/requests/:request/accept                 the requester accepts a delivery (I1, D5)
//	POST /api/requests/:request/reopen                 the requester reopens after delivery (D6)
//
// Every one of them is a thin adapter over the one reviewed write path
// (internal/service/request_lifecycle.go, slice S5/S6: TransitionRequest,
// DeliverRequest, AnswerRequest, AcceptRequestByRef, ReopenRequestByRef) and the
// one pure decision behind it (internal/service/request_transitions.go,
// PlanRequestTransition). It decodes a bounded, strict body and a path, maps the
// transport's own facts onto the service's input, and encodes the service's
// answer. It decides nothing: whether an edge exists, who may make it, what
// evidence the act must carry, what a stale revision means, whether the caller
// is the requester, and every audit row are the service's and the policy's. That
// is why no endpoint here has a capability check of its own — the surface rule
// is "no route bypasses the service", and the tests assert it by walking the
// refusal table rather than by reading each handler.
//
// # One reviewed write path, two surfaces
//
// The team surface and the requester surface are different *addressing*, not
// different rules. A triager works inside a team's inbox and addresses a request
// by its serial within the team it named; the requester holds only the opaque
// reference their submission answered with, and the requester projection
// deliberately carries neither the serial nor the team (E3's whitelist). So the
// two surfaces name requests differently and both end at the same decision:
//
//	team route       POST /api/teams/{id}/requests/{serial}/…   TransitionRequest / DeliverRequest
//	requester route  POST /api/requests/{ref}/…                AcceptRequestByRef / ReopenRequestByRef / AnswerRequest
//
// `AcceptRequestByRef` is the service's answer to the addressing gap #3971
// documented: it resolves the reference through the own-request read (scoped by
// the caller's requester column) and then calls the unchanged AcceptRequest.
// Nothing was added to the policy to make the requester surface possible.
//
// # Acceptance is a person's act, structurally
//
// `POST /api/requests/{ref}/accept` cannot be reached by a credential. The
// service refuses a caller that is not a person in the web UI **before** the
// reference is resolved, so an agent account, an API token or a submit-only
// credential gets byte-identical answers for a real reference, an unknown one
// and another person's, and learns nothing about which references exist. The
// policy then re-checks the actor's user id against the request's requester row,
// so a service that mis-resolved a role still cannot accept. There is no
// on-behalf path in v1 (D5) and this file adds none: the only way to `accept` is
// to be that request's requester, signed in, in the browser.
//
// # `:request` on a team write is the serial; on the requester surface it is
// # the reference (or the serial)
//
// The team routes reuse `requestMutationTarget`, so a public reference on a team
// write is refused by name rather than searched for (the reason #3971 gives: a
// second tenant-scoped lookup in an adapter would be a second definition of what
// a reference may name). The requester routes accept the reference *because that
// is the only thing a customer holds*, and the service normalizes its shape
// through the domain's own rule — exactly as the requester read does.
//
// # The answer is the move, not the record
//
// A successful act answers with the request's identity and where it now stands
// (`id`, `public_ref`, `state`, `rev`) plus `changed`, and for the two acts that
// write one, the discussion row the move appended (`question` / `answer`). It is
// the same four-fact envelope #3971's writes use, for the same reason: the
// requester routes are reachable by a customer, and one envelope for every
// caller is the safe one. The requester answer carries **no author**: the row
// the move wrote is the requester's own words, and a client that needs
// attribution reads the thread under the view rules that own it.
//
// # What is NOT here
//
// No links, no handoff, no attachments, no uploads (F5, another ticket). No
// claim, release, assign, edit or plain comment (#3971's file). No route records
// a delivery's links or approves anything: `deliver` states an outcome the
// request's own links already evidence, and a delivery whose evidence is missing
// is refused by the aggregate's gate before the lifecycle write runs.

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// requestLifecycleBody is what a team lifecycle route may send: the destination
// (for the one generic transition route), the evidence fields the policy's move
// kinds read, and the precondition.
//
// Every evidence field is present here so the *policy* refuses one the
// destination does not read (an outcome brief on a reject, a question on a plain
// step) rather than the decoder dropping it — the same "a field that vanished
// between the wire and the write tells its author something false" rule the edit
// body follows. There is deliberately no field for an actor, an org, a team, a
// state other than `to`, a capability, a trigger, a dry-run or an approval: the
// identity comes from the principal, the tenant from the path's team row, and
// the trigger is the service's constant (I8).
type requestLifecycleBody struct {
	// To is the destination of the generic transition route. It is ignored by
	// the named routes, which know their own destination, and refused there by
	// the decoder only if it is a field they do not have — so `needs-info` does
	// not accept a `to`.
	To domain.RequestStatus `json:"to"`

	// Question is what the team asks the requester (needs_information, I5).
	Question string `json:"question"`
	// RejectReason is why the request is declined (I3).
	RejectReason string `json:"reject_reason"`
	// CanonicalRequestID is the request a duplicate folds into (I3, D7).
	CanonicalRequestID int `json:"canonical_request_id"`
	// OutcomeBrief says what is and is not included (I2, deliver).
	OutcomeBrief string `json:"outcome_brief"`
	// NoArtifact is the explicit "there is nothing to link" record (I2).
	NoArtifact bool `json:"no_artifact"`

	// ExpectedRev is the revision the caller read. Zero means "the revision this
	// call reads" (a replay-tolerant act); a value the caller read turns the act
	// into a compare-and-set.
	ExpectedRev *int `json:"expected_rev"`
}

// requestRequesterActBody is what a requester route may send: the answer's
// words and the question row they answer, plus the precondition.
//
// There is no field for a state, a destination or an actor. An answer's
// destination is the state the question interrupted, read from the request's own
// history — the requester does not choose where their request goes next (I5) —
// and acceptance's destination is fixed.
type requestRequesterActBody struct {
	// Answer is the requester's reply (answer only).
	Answer string `json:"answer"`
	// AnswerTo names the question comment being answered. Zero answers the
	// question currently open, which is the newest question in the thread.
	AnswerTo int64 `json:"answer_to"`
	// ExpectedRev is the revision the requester read; zero is "whatever it is
	// now", which is what a form that never saw a revision sends.
	ExpectedRev *int `json:"expected_rev"`
}

// requestLifecycleFacts is a lifecycle act's answer: the request's facts, the
// discussion row the move wrote (if any) and whether this call wrote anything.
//
// Rev is here for the same reason #3971's writes carry it: a compare-and-set
// client must be able to act again without a read, and a requester who accepted
// an outcome has no other way to learn the new revision.
type requestLifecycleFacts struct {
	Request requestMutationFacts `json:"request"`
	// Comment is the question or answer the move appended, absent when the act
	// carried none. It never carries an author.
	Comment *requestCommentFacts `json:"comment,omitempty"`
	// Changed is false only for a move that was already in force (a replayed
	// duplicate fold). Every other successful act wrote.
	Changed bool `json:"changed"`
}

// ---- The team surface ----

// teamRequestTransition serves POST /api/teams/:id/requests/:request/transition:
// the team's working step, to whichever of triaging / accepted / in_progress the
// caller names.
//
// A single generic route for the plain steps (new→triaging, triaging→accepted,
// accepted→in_progress) rather than three near-identical ones: the policy is what
// decides which edges exist and who may take them, and a route per edge would
// encode the graph a second time in this file. Everything that carries evidence
// has its own named route below, because its body shape is its own.
func (h *RestHandler) teamRequestTransition(w http.ResponseWriter, r *http.Request) {
	teamID, requestID, ok := requestLifecycleTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	body, ok := decodeRequestLifecycle(w, r, "request transition")
	if !ok {
		return
	}
	if strings.TrimSpace(string(body.To)) == "" {
		fail(w, authz.RequestInvalid("a transition names its destination in `to`: %s", requestTransitionDestinationNames()), http.StatusBadRequest)
		return
	}
	rev, err := requestWriteRev(r, body.ExpectedRev)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.svc.TransitionRequest(r.Context(), p, service.TransitionRequestInput{
		RequestID:   requestID,
		TeamID:      teamID,
		To:          body.To,
		ExpectedRev: rev,
	})
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	h.writeLifecycleResult(w, res)
}

// teamRequestNeedsInfo serves POST /api/teams/:id/requests/:request/needs-info:
// asking the requester a question, which is what entering needs_information
// means (I5 — asking obliges an answer, so the question is not optional).
func (h *RestHandler) teamRequestNeedsInfo(w http.ResponseWriter, r *http.Request) {
	h.teamRequestLifecycleAct(w, r, domain.RequestStatusNeedsInformation, "request question",
		func(body requestLifecycleBody, rev int) service.TransitionRequestInput {
			return service.TransitionRequestInput{To: domain.RequestStatusNeedsInformation, ExpectedRev: rev, Question: body.Question}
		})
}

// teamRequestReject serves POST /api/teams/:id/requests/:request/reject: the
// team's close, with the reason the requester is shown (I3).
func (h *RestHandler) teamRequestReject(w http.ResponseWriter, r *http.Request) {
	h.teamRequestLifecycleAct(w, r, domain.RequestStatusRejected, "request rejection",
		func(body requestLifecycleBody, rev int) service.TransitionRequestInput {
			return service.TransitionRequestInput{To: domain.RequestStatusRejected, ExpectedRev: rev, RejectReason: body.RejectReason}
		})
}

// teamRequestDuplicate serves POST /api/teams/:id/requests/:request/duplicate:
// folding a request into the canonical it repeats (I3, D7). The canonical is read
// by the service — this adapter passes the id the caller named and nothing that
// asserts anything about it.
func (h *RestHandler) teamRequestDuplicate(w http.ResponseWriter, r *http.Request) {
	h.teamRequestLifecycleAct(w, r, domain.RequestStatusDuplicate, "request duplicate",
		func(body requestLifecycleBody, rev int) service.TransitionRequestInput {
			return service.TransitionRequestInput{To: domain.RequestStatusDuplicate, ExpectedRev: rev, CanonicalRequestID: body.CanonicalRequestID}
		})
}

// teamRequestReopen serves POST /api/teams/:id/requests/:request/reopen: the
// team reconsidering its own close (D6 — a rejected or duplicate request returns
// to triaging, and a duplicate's detach happens in the same write).
//
// The requester's reopen is a different route on their own surface: which of the
// two applies is not a transport fact but the caller's standing, and the policy
// decides it from the capability the service resolved.
func (h *RestHandler) teamRequestReopen(w http.ResponseWriter, r *http.Request) {
	teamID, requestID, ok := requestLifecycleTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if _, ok := decodeRequestLifecycle(w, r, "request reopen"); !ok {
		return
	}
	rev, err := requestMutationPreconditionRev(w, r)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.svc.ReopenRequest(r.Context(), p, requestID, teamID, rev)
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	h.writeLifecycleResult(w, res)
}

// teamRequestDeliver serves POST /api/teams/:id/requests/:request/deliver: the
// team stating the outcome exists, with what is and is not included (I2).
//
// It calls DeliverRequest rather than TransitionRequest because the delivery gate
// lives there: the aggregate is rebuilt from the request's own links and a
// delivery whose brief or evidence does not make it eligible is refused with
// what is missing — one definition, checked by the rail's gate and by this
// route alike.
func (h *RestHandler) teamRequestDeliver(w http.ResponseWriter, r *http.Request) {
	teamID, requestID, ok := requestLifecycleTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	body, ok := decodeRequestLifecycle(w, r, "request delivery")
	if !ok {
		return
	}
	rev, err := requestWriteRev(r, body.ExpectedRev)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.svc.DeliverRequest(r.Context(), p, service.DeliverRequestInput{
		RequestID:    requestID,
		TeamID:       teamID,
		ExpectedRev:  rev,
		OutcomeBrief: body.OutcomeBrief,
		NoArtifact:   body.NoArtifact,
	})
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	h.writeLifecycleResult(w, res)
}

// teamRequestLifecycleAct is the shared shape of the three evidence-carrying
// closes and the question. They differ only in the destination they fix and the
// field they hand the service, so keeping them one function is what stops their
// preconditions and answers from drifting apart.
func (h *RestHandler) teamRequestLifecycleAct(w http.ResponseWriter, r *http.Request,
	to domain.RequestStatus, what string, build func(requestLifecycleBody, int) service.TransitionRequestInput) {
	teamID, requestID, ok := requestLifecycleTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	body, ok := decodeRequestLifecycle(w, r, what)
	if !ok {
		return
	}
	if strings.TrimSpace(string(body.To)) != "" {
		// The destination is the route's, and a body naming another one would
		// either be silently ignored (a caller believing it did something else)
		// or honoured (a second way to reach every edge from one URL). Answered
		// rather than dropped, like every other unknown payload field.
		fail(w, authz.RequestInvalid("this route's destination is fixed (%s): send its evidence fields only, without `to`", to), http.StatusBadRequest)
		return
	}
	rev, err := requestWriteRev(r, body.ExpectedRev)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	in := build(body, rev)
	in.RequestID, in.TeamID = requestID, teamID
	res, err := h.svc.TransitionRequest(r.Context(), p, in)
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	h.writeLifecycleResult(w, res)
}

// ---- The requester surface ----

// ownRequestAnswer serves POST /api/requests/:request/answer: the requester's
// reply to the desk's question (I5). The destination is the state the question
// interrupted, read from the request's own history — never the caller's choice.
func (h *RestHandler) ownRequestAnswer(w http.ResponseWriter, r *http.Request) {
	ref, ok := requesterActTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	body, ok := decodeRequesterAct(w, r, "answer")
	if !ok {
		return
	}
	rev, err := requestWriteRev(r, body.ExpectedRev)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	// The service loads the request from the reference through the own-request
	// read, so the team and the serial are the row's, not the caller's — and the
	// answer path's own person-in-the-UI refusal runs before either is known.
	res, err := h.svc.AnswerRequestByRef(r.Context(), p, ref, service.TransitionRequestInput{
		Answer:      body.Answer,
		AnswerTo:    body.AnswerTo,
		ExpectedRev: rev,
	})
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	h.writeLifecycleResult(w, res)
}

// ownRequestAccept serves POST /api/requests/:request/accept: the requester
// accepting a delivery — the one act no team member, lead, agent, token or
// connector may perform (I1, D5).
func (h *RestHandler) ownRequestAccept(w http.ResponseWriter, r *http.Request) {
	ref, ok := requesterActTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	body, ok := decodeRequesterAct(w, r, "acceptance")
	if !ok {
		return
	}
	rev, err := requestWriteRev(r, body.ExpectedRev)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.svc.AcceptRequestByRef(r.Context(), p, ref, rev)
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	h.writeLifecycleResult(w, res)
}

// ownRequestReopen serves POST /api/requests/:request/reopen: the requester
// reopening after delivery (D6). Whether it is the requester's reopen or the
// team's is the policy's answer from the caller's capability, so this route is
// simply the requester surface's way of reaching the one decision.
func (h *RestHandler) ownRequestReopen(w http.ResponseWriter, r *http.Request) {
	ref, ok := requesterActTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	body, ok := decodeRequesterAct(w, r, "reopen")
	if !ok {
		return
	}
	rev, err := requestWriteRev(r, body.ExpectedRev)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.svc.ReopenRequestByRef(r.Context(), p, ref, rev)
	if err != nil {
		failRequestMutation(w, err)
		return
	}
	h.writeLifecycleResult(w, res)
}

// requesterActTarget reads the requester surface's :request segment: the opaque
// reference the customer holds, or the serial when a team member is acting on
// their own submission.
//
// An empty segment is the caller's mistake and says so. Everything else is passed
// through to the service, which normalizes the reference through the domain's own
// rule — so a value the domain could never have minted is a coded 400 naming the
// field rather than a 404 that would claim a request is missing. It never
// searches for anything here.
func requesterActTarget(w http.ResponseWriter, r *http.Request) (service.RequesterRequestRef, bool) {
	segment := strings.TrimSpace(r.PathValue("request"))
	if segment == "" {
		fail(w, authz.RequestInvalid("name your request by the reference you were given"), http.StatusBadRequest)
		return service.RequesterRequestRef{}, false
	}
	if n, err := strconv.Atoi(segment); err == nil && n > 0 {
		return service.RequesterRequestRef{RequestID: n}, true
	}
	return service.RequesterRequestRef{PublicRef: segment}, true
}

// ---- Shared decoding and answers ----

// requestLifecycleTarget reads a team lifecycle route's path: the team id and the
// request serial.
//
// The serial rule is the mutation routes' (requestMutationTarget): a public
// reference is refused by name rather than searched for, because resolving one
// here would put a second tenant-scoped lookup and a second definition of what a
// reference may name in an adapter. The requester surface is where a reference
// is the natural spelling, and it resolves it in the service.
func requestLifecycleTarget(w http.ResponseWriter, r *http.Request) (teamID, requestID int, ok bool) {
	teamID, ok = requestPathTeam(w, r)
	if !ok {
		return 0, 0, false
	}
	requestID, err := requestMutationTarget(r)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return 0, 0, false
	}
	return teamID, requestID, true
}

// decodeRequestLifecycle reads one team lifecycle body: bounded, strict about
// unknown fields, one JSON value, and an empty body is "no fields" (the plain
// steps and the reopen take none).
func decodeRequestLifecycle(w http.ResponseWriter, r *http.Request, what string) (requestLifecycleBody, bool) {
	var body requestLifecycleBody
	if err := decodeRequestMutation(w, r, &body, what); err != nil {
		fail(w, err, http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// decodeRequesterAct reads one requester body: the same bounded, strict decoding
// as the team routes, since the caller is a customer and the bound is the
// memory guard in front of the domain's own limits.
func decodeRequesterAct(w http.ResponseWriter, r *http.Request, what string) (requestRequesterActBody, bool) {
	var body requestRequesterActBody
	if err := decodeRequestMutation(w, r, &body, what); err != nil {
		fail(w, err, http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// requestMutationPreconditionRev reads the optional precondition of an act whose
// body carries no other field (the reopen), by decoding the (possibly empty)
// body and merging If-Match with it.
func requestMutationPreconditionRev(w http.ResponseWriter, r *http.Request) (int, error) {
	var body requestLifecycleBody
	if err := decodeRequestMutation(w, r, &body, "request reopen"); err != nil {
		return 0, err
	}
	if strings.TrimSpace(string(body.To)) != "" {
		return 0, authz.RequestInvalid("this route's destination is fixed: send the precondition only, without `to`")
	}
	return requestWriteRev(r, body.ExpectedRev)
}

// writeLifecycleResult encodes one lifecycle act's outcome, or writes its coded
// refusal.
//
// The refusal path is `failRequestMutation`, which is #3971's: it reads the
// service's typed conflicts off the error chain and adds the one fact a client
// retries with (`rev`). A lifecycle conflict is the same shape as a lost claim —
// "the row moved since you read it, reload and decide again" — so reusing the
// helper is what keeps one 409 contract on this surface instead of two.
func (h *RestHandler) writeLifecycleResult(w http.ResponseWriter, res *service.TransitionRequestResult) {
	if res == nil || res.Request == nil {
		// Unreachable through the service, which returns the stored row on every
		// success. Failing closed keeps a future caller from answering 200 for an
		// act with nothing to show for itself.
		fail(w, errorf("the request lifecycle act answered no request"), http.StatusInternalServerError)
		return
	}
	out := requestLifecycleFacts{
		Request: requestMutationFacts{
			ID:        res.Request.ID,
			PublicRef: res.Request.PublicRef,
			State:     res.Request.State,
			Rev:       res.Request.Rev,
		},
		Changed: res.Changed,
	}
	if res.Comment != nil {
		// No author, deliberately: the question is the desk's and the answer is
		// the requester's own words, and attribution belongs to the thread read
		// under the view rules that reduce a triager to "the desk".
		out.Comment = &requestCommentFacts{
			ID:        res.Comment.ID,
			Kind:      res.Comment.Kind,
			Body:      res.Comment.Body,
			CreatedAt: res.Comment.CreatedAt,
		}
	}
	jsonOK(w, out)
}

// requestTransitionDestinationNames is the hint a caller gets for omitting `to`
// on the generic transition route: the plain working steps this route exists for.
// It is deliberately narrower than the whole graph — the evidence-carrying moves
// have their own routes, and naming them here would advertise a destination this
// route cannot reach without evidence it does not accept.
func requestTransitionDestinationNames() string {
	return "the plain working steps are " + string(domain.RequestStatusTriaging) + ", " +
		string(domain.RequestStatusAccepted) + " and " + string(domain.RequestStatusInProgress) +
		"; asking a question, declining, folding a duplicate and delivering have their own routes"
}
