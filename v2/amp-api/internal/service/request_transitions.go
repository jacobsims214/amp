package service

// Team request lifecycle: the one pure decision that answers whether a move is
// legal, who may make it, and what the write must carry (epic 202, story 548,
// AMP #3953; approved product contract artifact 36 v2 §1, §2 and §4; approved
// mockups artifact 37 v1 and 38 v1; build slices S5 and S6 of
// discoveries/team-inbox-approved-build-slices.md).
//
// This file is a decision, not a write. It reads no database, takes no clock,
// writes no row and appends no comment. Given the request's current state, the
// caller's capability on it, and the structured evidence the act carries, it
// answers one of two things:
//
//   - a RequestTransitionPlan: the destination, the columns the same write must
//     set (the state-entry timestamps 053 keeps first-write-wins, the reopen
//     counters, the duplicate pointer or its detach), the question or answer
//     comment the service must append, and the actor to record for an
//     acceptance;
//   - a coded refusal: request.invalid (400) naming the states the caller may
//     reach instead (I7), request.not_found for a caller with no standing at
//     all, or a 403 for a call the caller may see but not make.
//
// Why the decision is separate from the write. The state machine is the one
// thing every surface — REST, MCP, the web UI, a future connector — must agree
// on, and the one thing a reviewer must be able to read in one page. Persisting
// it (repository/request_transitions.go, service/request_lifecycle.go, #3960)
// then has one job: perform exactly the plan in one transaction and append the
// one history row (I6). If the rules and the SQL lived together, every
// concurrency fix would be a rules change and every rules change a migration
// risk. So timestamps are named here and written there.
//
// The approved graph, which is the whole of the legality table below (contract
// §1's line, `new → triaging → (needs_information ↔) → accepted → in_progress →
// delivered → requester_accepted`, plus terminal `rejected` and `duplicate`):
//
//	new ──────────▶ triaging ──────▶ accepted ──────▶ in_progress ──────▶ delivered ──────▶ requester_accepted
//	 │  ╲            │   ╲            │   ╲              │   ╲                │                    │
//	 │   ╲           │    ╲           │    ╲             │    ╲               │ reopen              │ reopen
//	 │    ╲          ▼     ╲          ▼     ╲           ▼     ╲              ▼                    ▼
//	 │     ╲     needs_information ◀───┴──────┴───────────┴─── (ask)        triaging ◀────────────┘
//	 │      ╲          │ (the requester's answer restores the state it interrupted)
//	 ▼       ▼         ▼
//	rejected  duplicate ◀── team fold (D7); reopen detaches it
//
// Six things the table encodes, each one an invariant a test pins:
//
//  1. Acceptance is the requester's alone (I1, D5). `requester_accepted` has
//     exactly one entry: from `delivered`, for RequestCapabilityRequester, and
//     that capability is only ever granted to the request's own requester
//     acting as a person in the web UI (RequestCapabilityFor). A team member, a
//     team lead, an agent account, an API token and a connector can neither
//     reach the state nor declare themselves its requester: the capability is
//     re-checked against the actor's user id here, so a caller cannot pass
//     RequestCapabilityRequester on someone else's behalf.
//  2. Asking obliges an answer (I5). Entering needs_information requires a
//     question in the evidence, and the requester's way out is an answer naming
//     that question, which restores the state the question interrupted — never
//     straight to delivered.
//  3. Terminal states carry their reasons (I3). `rejected` needs a reason,
//     `duplicate` needs a canonical, and a fold whose target is itself a
//     duplicate is refused here so the one-hop rule (D7) holds even if the
//     repository's chain flattening were bypassed.
//  4. Delivered is evidenced (I2). `delivered` needs an outcome brief and
//     either at least one delivered_by link or an explicit no-artifact record —
//     one of the two, never neither and never both.
//  5. Reopen is a transition, not a state (D6). The requester reopens from
//     delivered/requester_accepted; the team reopens from rejected/duplicate;
//     every reopen raises reopened_at/reopened_count, and a duplicate's reopen
//     detaches it in the same write (053 refuses a duplicate with no canonical,
//     so the two cannot disagree even for an instant).
//  6. Nothing auto-advances from linked work (I8). The evidence names what
//     asked for the move, and only an explicit act qualifies: a linked ticket's
//     completion, a recomputed outcome aggregate and a background sweep are all
//     refused for every state, every destination and every capability.
//
// What this file deliberately does not decide, because each belongs to another
// slice and none of it is reachable from a pure function:
//
//   - who a caller is, in terms of rows (authz.InboxRole and
//     RequireTeamManager resolve that; RequestCapabilityFor only maps their
//     answers onto this file's vocabulary);
//   - whether the request exists in the caller's tenant, and whether a caller
//     with no standing learns that it does (repository/request_rows.go's
//     tenant-scoped lookup and authz.ViewForLoad's one 404);
//   - the optimistic-concurrency check (rev/If-Match, request_claims.go's
//     RequestRevConflict) — a stale write is a 409 before the graph is
//     consulted;
//   - audit rows, history rows, comments' text, notifications and the clock:
//     the plan says what to write, and service/request_lifecycle.go writes it
//     in one transaction.

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Who is asking ----

// RequestCapability is what a caller may do with one request. It is resolved
// once per call from the rows (authz) and passed in; nothing in a request's
// text, subject, brief, metadata or comment ever names a capability.
//
// The values are ordered so the team ladder reads naturally (team < manager),
// but the ordering is NOT one ladder for everyone: RequestCapabilityRequester
// sits beside it, not below it, because a requester is not a junior team member
// and a team member is not the requester of a request they did not submit. The
// two branches are deliberately not comparable, which is why canMake spells the
// pairs out rather than comparing numbers alone.
type RequestCapability int

const (
	// RequestCapabilityNone: no standing on this request at all — a stranger
	// from another org, a submit grant holder who is on no team and in no org
	// (a grant confers submitting, never reading: contract §2), an agent
	// account acting as nobody in particular, or a request whose requester row
	// was deleted and whose caller holds no team role. Every act is refused as
	// not-found, the same bytes a request that does not exist gets.
	RequestCapabilityNone RequestCapability = iota
	// RequestCapabilityRequester: the person who submitted this request, and
	// nobody else. Acceptance, answering a question and reopening after
	// delivery are theirs.
	RequestCapabilityRequester
	// RequestCapabilityTeam: a member of the inbox's team (or an org
	// owner/admin, or a global admin — authz.InboxRole's InboxMember). Triages,
	// plans work, asks questions and delivers.
	RequestCapabilityTeam
	// RequestCapabilityManager: a lead of the inbox's team, an owner/admin of
	// its org, or a global admin — exactly authz.Checker.RequireTeamManager.
	// Adds the consequential closes the contract gives that row alone (reject,
	// duplicate) and their reopen.
	RequestCapabilityManager
)

func (c RequestCapability) String() string {
	switch c {
	case RequestCapabilityRequester:
		return "requester"
	case RequestCapabilityTeam:
		return "team"
	case RequestCapabilityManager:
		return "manager"
	default:
		return "none"
	}
}

// canMake reports whether a caller at capability c may make a move whose
// minimum is min. Spelled out rather than `c >= min` because the requester
// branch is not a rung on the team ladder: a team member is not "more than" the
// requester, and a manager is not allowed to accept.
func (c RequestCapability) canMake(min RequestCapability) bool {
	switch min {
	case RequestCapabilityRequester:
		return c == RequestCapabilityRequester
	case RequestCapabilityTeam:
		return c == RequestCapabilityTeam || c == RequestCapabilityManager
	case RequestCapabilityManager:
		return c == RequestCapabilityManager
	default:
		return false
	}
}

// RequestCapabilityFor maps a caller's standing on a request onto the policy's
// vocabulary. role is authz's InboxMember answer for the request's team and
// manager is RequireTeamManager's answer for that team; both are read from the
// rows on this call by the caller, never cached (contract §2's per-request
// re-check).
//
// Order matters, and it is the same order authz.RequestViewFor uses: the team's
// standing wins. A team member who submitted to their own inbox is a team
// member here, not a requester, so they triage and deliver but do not accept —
// the contract's manager row lists "act as requester" among the things that row
// cannot do, and a team member seeing their own request sees the team's view,
// not the requester's projection.
//
// The requester branch additionally requires p.InUI(): a person in the web UI,
// not an agent account and not an API token (authz.InUI's own doc says exactly
// that acceptance "can never be a token's act"). Two reasons, and the second is
// the one a reviewer should weigh:
//
//   - Acceptance is the requester's alone (D5, structural): a credential is not
//     a person, whatever channel label it was handed.
//   - The contract's own operation list (§5) has no accept, answer or reopen on
//     the API/MCP surface at all — it lists submit, get, list, update, comment,
//     claim, attach, attachments, all team-side — and both approved requester
//     mockups put answering, accepting and reopening on the signed-in
//     requester's browser surface (artifact 38 v1, J6). So a non-UI principal is
//     not a requester here.
//
// If the user ever wants a programmatic requester to answer or reopen (not
// accept), the narrow change is to split this branch: return
// RequestCapabilityRequester for an authenticated requester on any channel, and
// keep the InUI test where the acceptance row is checked. That is a product
// decision, not a tidy-up, and until it is made the stricter reading stands.
func RequestCapabilityFor(r *domain.TeamRequest, p authz.Principal, role authz.InboxRole, manager bool) RequestCapability {
	if r == nil {
		// Nothing to have standing on. Answering "manager" for a nil request would
		// let a caller that forgot to load one plan a move against nothing.
		return RequestCapabilityNone
	}
	if role >= authz.InboxMember {
		if manager {
			return RequestCapabilityManager
		}
		return RequestCapabilityTeam
	}
	if p.InUI() && p.HasUser() && r.RequesterUserID != nil && *r.RequesterUserID == p.UserID {
		return RequestCapabilityRequester
	}
	return RequestCapabilityNone
}

// ---- What the policy reads ----

// RequestSnapshot is the part of a request's record the transition policy
// reads. It is deliberately not the whole record: a decision that could see the
// subject or the brief is a decision that could start reading authority out of
// text, and nothing here is text-derived.
type RequestSnapshot struct {
	// ID is the request's serial, used only to refuse a self-fold and to name
	// the request in a refusal.
	ID int
	// State is where the request is now (domain.RequestStatuses).
	State domain.RequestStatus
	// DuplicateOf is the canonical this request is folded into, nil when it is
	// not a duplicate. The policy reads it to know whether a reopen detaches.
	DuplicateOf *int
	// RequesterUserID is the person acceptance belongs to; nil for a service or
	// connector submission, which means nobody can accept it.
	RequesterUserID *int
	// PriorState is where the request was before it entered needs_information,
	// supplied by the caller from the request's transition history — the schema
	// keeps no prior-state column on purpose (053's state graph is a service
	// function, not a stored graph, and a second column could disagree with the
	// history).
	PriorState domain.RequestStatus
}

// RequestSnapshotOf takes the request-side facts a decision needs from a loaded
// record, so the mapping exists once and a caller never hand-builds a snapshot
// from a record it read a moment differently. PriorState is not on the record
// and is not set here; a caller answering a question sets it from the history
// (the last transition into needs_information names the state it left).
func RequestSnapshotOf(r *domain.TeamRequest) RequestSnapshot {
	if r == nil {
		return RequestSnapshot{}
	}
	return RequestSnapshot{
		ID:              r.ID,
		State:           r.State,
		DuplicateOf:     r.DuplicateOf,
		RequesterUserID: r.RequesterUserID,
	}
}

// RequestTrigger is what asked for the move. It exists for one invariant:
// nothing auto-advances from linked work (I8). A request's state changes when a
// person or an agent decides it does, and a linked ticket's completion, a
// recomputed outcome aggregate or a background sweep may recommend a move in a
// view but never perform it.
//
// The trigger is declared by the caller and cannot be verified from here; what
// this file guarantees is its own half of the rule, absolutely: no trigger but
// RequestTriggerExplicit ever produces a plan, for any state, destination,
// capability or evidence. A caller that lies about its trigger is a caller
// written to lie, which is a review finding rather than a policy one — and the
// service (#3960) derives no transition from a ticket's state in the first
// place.
type RequestTrigger string

const (
	// RequestTriggerExplicit: a person or agent asked for this move, now.
	RequestTriggerExplicit RequestTrigger = "explicit"
	// RequestTriggerLinkedWork: a linked ticket was completed, started or
	// merged. Refused — a closing ticket never flips a request (I8).
	RequestTriggerLinkedWork RequestTrigger = "linked_work"
	// RequestTriggerAggregate: the outcome aggregate recomputed and suggests
	// the move. Refused — the aggregate reports, it does not decide (E2).
	RequestTriggerAggregate RequestTrigger = "aggregate"
	// RequestTriggerSweep: a clock or background job wants to move it. Refused
	// — v1 has no SLA clock and no automatic transition (D8).
	RequestTriggerSweep RequestTrigger = "sweep"
)

// RequestTransitionEvidence is what the act carries, structurally. Every field
// is a typed value the caller had to obtain deliberately; none is parsed out of
// a subject, a brief or a comment body. A move reads exactly the fields its
// kind owns, and passing a field a move does not read is refused rather than
// ignored — an ignored input reads to the caller as one that was used.
type RequestTransitionEvidence struct {
	// Trigger is what asked for the move (see RequestTrigger).
	Trigger RequestTrigger

	// Question is the question being put to the requester. Required when the
	// move enters needs_information (I5: asking obliges an answer) and refused
	// otherwise. The service appends it as a `question` comment in the same
	// transaction as the state move.
	Question string

	// AnswerTo is the id of the question comment this answer replies to.
	// Required for the requester's answer that leaves needs_information: an
	// answer with no question is not an answer, and the pair is what makes the
	// thread readable in order.
	AnswerTo int64

	// RejectReason is why the request is declined. Required for rejected (I3)
	// and shown to the requester verbatim.
	RejectReason string

	// CanonicalRequestID is the request this one is folded into. Required for
	// duplicate (I3, D7) and must be the root of its chain.
	CanonicalRequestID int

	// CanonicalIsDuplicate says the named canonical is itself a duplicate,
	// which D7 refuses: a canonical is never a duplicate, so a fold lands on the
	// root. The caller resolves the root (repository.MarkDuplicate flattens and
	// is the arbiter of chains) and answers this flag honestly; answering true
	// is refused so the two layers cannot disagree.
	CanonicalIsDuplicate bool

	// OutcomeBrief is the delivery note: what is and is not included. Required
	// for delivered (I2), bounded like the input brief.
	OutcomeBrief string

	// DeliveredByCount is how many delivered_by links the delivery has (tasks,
	// artifacts or receipts, D10). Required to be at least one unless
	// NoArtifact says the outcome has no artifact at all.
	DeliveredByCount int

	// NoArtifact is the explicit "the outcome exists and there is no artifact to
	// link" record I2 allows instead of a link. It is an explicit flag, not the
	// absence of one: "nobody linked anything yet" and "there is nothing to link"
	// must not be the same input.
	NoArtifact bool
}

// RequestTransitionColumn names a column the transition's write must set (or
// advance) in the same statement as the state. The names are the schema's
// column names, so a reviewer can check the plan against 053 without a mapping
// table, and every one of them is first-write-wins except reopened_count, which
// counts (053: delivered_at, planned_at, accepted_at and closed_at never move;
// reopened_at and reopened_count record the reopen).
type RequestTransitionColumn string

const (
	// RequestSetNeedsInfoSince: entering needs_information stamps since when
	// the requester has been the one being waited on (D8). It is not cleared on
	// the answer: the wait happened, and a later clock excludes it by reading
	// this column.
	RequestSetNeedsInfoSince RequestTransitionColumn = "needs_info_since"
	// RequestSetDeliveredAt: first entry into delivered.
	RequestSetDeliveredAt RequestTransitionColumn = "delivered_at"
	// RequestSetAcceptedAt: the requester's acceptance moment. The acting user
	// goes to accepted_by (RequestTransitionPlan.AcceptedBy).
	RequestSetAcceptedAt RequestTransitionColumn = "accepted_at"
	// RequestSetClosedAt: first entry into a terminal state (rejected,
	// duplicate). A reopen does not clear it: 053 makes it first-write-wins, so
	// it records that the request was once closed and when.
	RequestSetClosedAt RequestTransitionColumn = "closed_at"
	// RequestSetReopenedAt: first reopen. Like closed_at it is written once, so
	// reopened_count is what says how many times.
	RequestSetReopenedAt RequestTransitionColumn = "reopened_at"
	// RequestSetReopenedCount: incremented by one on every reopen.
	RequestSetReopenedCount RequestTransitionColumn = "reopened_count"
)

// RequestTransitionPlan is one validated move: everything the write layer needs
// and nothing it has to decide. Applying it is one transaction that sets state,
// these columns, rev + 1 and updated_at, appends exactly one request_events row
// (I6), appends the question or answer comment when the plan names one, and
// audits the act.
type RequestTransitionPlan struct {
	// From and To are the move's ends. From is what the caller's snapshot said;
	// a write that finds a different state must refuse rather than apply this.
	From domain.RequestStatus
	To   domain.RequestStatus

	// Set names the further columns to set, in the fixed order of the constants
	// above, so a test can compare plans exactly.
	Set []RequestTransitionColumn

	// Reopened is true for D6's reopen moves. Its columns are also in Set
	// (reopened_at, reopened_count); the flag exists because a caller almost
	// always wants to know the move was a reopen without scanning the list.
	Reopened bool

	// ClearDuplicateOf detaches the request from its canonical in the same
	// write. 053 refuses a duplicate with no canonical, so the state move and
	// the detach cannot be split (D7's reopen rule).
	ClearDuplicateOf bool

	// DuplicateOf is the flattened canonical to point duplicate_of at, for a
	// fold. Nil otherwise.
	DuplicateOf *int

	// RejectReason is the normalized reason, for rejected.
	RejectReason string

	// OutcomeBrief is the normalized delivery note, for delivered.
	OutcomeBrief string

	// Question is the normalized question comment to append before moving into
	// needs_information (I5).
	Question string

	// AnswerTo is the question comment the requester's answer replies to; the
	// answer comment itself is the service's to append.
	AnswerTo int64

	// AcceptedBy is the user to record in accepted_by, for a requester's
	// acceptance. It is the actor, re-checked against the request's requester in
	// PlanRequestTransition, never a value from the evidence.
	AcceptedBy *int
}

// RequestTransitionInput is one question to the policy: may this caller move
// this request to this state, on this evidence?
type RequestTransitionInput struct {
	// Request is the state the caller read. The policy does not re-read it.
	Request RequestSnapshot
	// To is the destination the caller wants.
	To domain.RequestStatus
	// Capability is the caller's standing, from RequestCapabilityFor.
	Capability RequestCapability
	// ActorUserID is the person or account making the move. It is used to
	// re-check a declared requester capability and to fill accepted_by; it is
	// never a display name and never comes from the request's text.
	ActorUserID int
	// Evidence is what the act carries, structurally.
	Evidence RequestTransitionEvidence
}

// ---- The decision ----

// PlanRequestTransition decides one move. It is pure: same input, same answer,
// no clock, no rows. The error is always a coded authz error the surfaces
// already know how to render (request.invalid, request.not_found, or a 403 with
// a sentence naming what the caller would need).
func PlanRequestTransition(in RequestTransitionInput) (*RequestTransitionPlan, error) {
	to, err := domain.NormalizeRequestStatus(string(in.To))
	if err != nil {
		return nil, authz.RequestInvalid("%s", err)
	}
	from, err := domain.NormalizeRequestStatus(string(in.Request.State))
	if err != nil {
		return nil, authz.RequestInvalid("the request's state %q is not a lifecycle state", string(in.Request.State))
	}
	switch in.Capability {
	case RequestCapabilityNone:
		// Not the caller's to see and not there are the same answer (contract
		// §2), and it is answered before anything about the move is evaluated.
		return nil, authz.RequestNotFound()
	case RequestCapabilityRequester:
		if err := checkRequesterActor(in.Request, in.ActorUserID); err != nil {
			return nil, err
		}
	case RequestCapabilityTeam, RequestCapabilityManager:
	default:
		return nil, authz.RequestInvalid("unknown request capability %d", in.Capability)
	}
	if err := checkRequestTrigger(in.Evidence.Trigger); err != nil {
		return nil, err
	}
	if from == to {
		return nil, authz.RequestInvalid("request %d is already %s: a transition moves it to a different state", in.Request.ID, from)
	}
	// Leaving needs_information by the requester is the answer (I5), and its
	// destination is not chosen by the caller: it is the state the question
	// interrupted. It is handled before the table because the table's other
	// needs_information rows are the team's closes.
	if from == domain.RequestStatusNeedsInformation && in.Capability == RequestCapabilityRequester {
		return planRequestAnswer(in, from, to)
	}
	move, ok := requestMoveFor(from, to)
	if !ok {
		return nil, refuseRequestMove(from, to, in.Request, in.Capability)
	}
	if !in.Capability.canMake(move.min) {
		return nil, refuseRequestMoveCapability(move, in.Capability)
	}
	if err := checkRequestMoveEvidence(move, in.Request, in.Evidence); err != nil {
		return nil, err
	}
	return buildRequestTransitionPlan(move, in), nil
}

// AllowedRequestTransitions answers the second half of a refusal, and the UI's
// "what can I do with this?" question: the states this caller may move the
// request to, in lifecycle order (domain.RequestStatuses' order), so a rendered
// action list is stable across requests and a refusal's list matches a UI's. It reads no evidence — this is the graph and
// the capability, not the particular act — so a caller uses it to render
// actions and the policy uses it to name the alternatives in a refusal (I7).
//
// A request in needs_information has exactly one destination for its requester
// (the state the question interrupted) and only closes for the team: the
// answer is the way back, and a team that wants the request in triaging again
// has not asked anything.
func AllowedRequestTransitions(snapshot RequestSnapshot, capability RequestCapability) []domain.RequestStatus {
	if capability == RequestCapabilityNone {
		return nil
	}
	from, err := domain.NormalizeRequestStatus(string(snapshot.State))
	if err != nil {
		return nil
	}
	if from == domain.RequestStatusNeedsInformation && capability == RequestCapabilityRequester {
		if prior, ok := answerDestination(snapshot); ok {
			return []domain.RequestStatus{prior}
		}
		return nil
	}
	var out []domain.RequestStatus
	for _, to := range domain.RequestStatuses {
		if to == from {
			continue
		}
		move, ok := requestMoveFor(from, to)
		if !ok || !capability.canMake(move.min) {
			continue
		}
		out = append(out, to)
	}
	return out
}

// ---- The graph ----

// requestMoveKind is what a legal move must carry as evidence, and what a
// request.invalid refusal is about when the evidence does not.
type requestMoveKind int

const (
	// requestMovePlain: the move itself is the whole act.
	requestMovePlain requestMoveKind = iota
	// requestMoveQuestion: asking the requester something (I5).
	requestMoveQuestion
	// requestMoveReject: declining with a reason (I3).
	requestMoveReject
	// requestMoveDuplicate: folding into a canonical (I3, D7).
	requestMoveDuplicate
	// requestMoveDelivery: stating the outcome exists (I2).
	requestMoveDelivery
)

// requestMove is one edge of the approved lifecycle. min is the least
// capability that may make it; reopen marks D6's reopen moves; detach marks the
// moves out of duplicate, which must clear duplicate_of in the same write.
type requestMove struct {
	from   domain.RequestStatus
	to     domain.RequestStatus
	min    RequestCapability
	kind   requestMoveKind
	reopen bool
	detach bool
}

// requestMoves is the approved graph, edge by edge. It is a table rather than a
// switch so the whole lifecycle can be read at once, and so a missing edge is a
// missing row a reviewer can see rather than a branch nobody notices.
//
// The edges below are read off the approved contract rather than added to it,
// and where the contract is silent the reading is the permissive one, because a
// restriction nobody approved is as much a product decision as a relaxation:
//
//   - `rejected` and `duplicate` are reachable from every non-terminal working
//     state (contract §1: "plus terminal rejected and duplicate", with no
//     precondition on the source). That includes in_progress: "blocked by a
//     security review" is discovered while work is under way, not before it.
//   - After delivery the only move is acceptance or a reopen (contract §4:
//     "Requester accepted is the requester alone, from delivered only; anything
//     else is a reopen"), so delivered → rejected does not exist.
//   - needs_information is asked from the working states only (contract §1's
//     diagram puts it between triaging and accepted), so new → needs_information
//     does not exist: nothing has been triaged to ask about yet.
//
// TestRequestTransitions_ProductionGraphMatchesTheContract pins the exact set
// against a table transcribed separately, so adding or dropping an edge is a
// visible diff on both sides rather than a drift.
var requestMoves = []requestMove{
	// The working chain (contract §1), the team's day-to-day triage.
	{from: domain.RequestStatusNew, to: domain.RequestStatusTriaging, min: RequestCapabilityTeam},
	{from: domain.RequestStatusTriaging, to: domain.RequestStatusAccepted, min: RequestCapabilityTeam},
	{from: domain.RequestStatusAccepted, to: domain.RequestStatusInProgress, min: RequestCapabilityTeam},
	{from: domain.RequestStatusInProgress, to: domain.RequestStatusDelivered, min: RequestCapabilityTeam, kind: requestMoveDelivery},

	// Asking obliges an answer (I5). needs_information is entered from the
	// working chain only; there is nothing to ask about before triage begins.
	{from: domain.RequestStatusTriaging, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam, kind: requestMoveQuestion},
	{from: domain.RequestStatusAccepted, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam, kind: requestMoveQuestion},
	{from: domain.RequestStatusInProgress, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam, kind: requestMoveQuestion},

	// Acceptance is the requester's alone, from delivered only (I1, D5). This
	// is the only row whose minimum is RequestCapabilityRequester, and the
	// capability is re-checked against the request's requester row.
	{from: domain.RequestStatusDelivered, to: domain.RequestStatusRequesterAccepted, min: RequestCapabilityRequester},

	// Reopen is a transition, not a state (D6): the requester reopens from
	// delivered/requester_accepted, the team from rejected/duplicate.
	{from: domain.RequestStatusDelivered, to: domain.RequestStatusTriaging, min: RequestCapabilityRequester, reopen: true},
	{from: domain.RequestStatusRequesterAccepted, to: domain.RequestStatusTriaging, min: RequestCapabilityRequester, reopen: true},
	{from: domain.RequestStatusRejected, to: domain.RequestStatusTriaging, min: RequestCapabilityTeam, reopen: true},
	{from: domain.RequestStatusRejected, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam, kind: requestMoveQuestion, reopen: true},
	// A duplicate reopens detach in the same write (D7 and DetachDuplicate).
	{from: domain.RequestStatusDuplicate, to: domain.RequestStatusTriaging, min: RequestCapabilityTeam, reopen: true, detach: true},
	{from: domain.RequestStatusDuplicate, to: domain.RequestStatusNeedsInformation, min: RequestCapabilityTeam, kind: requestMoveQuestion, reopen: true, detach: true},

	// The team's closes. The approved contract's actor table gives "reject" and
	// "duplicate" to the team lead / org owner-admin row and lists only
	// inbox/detail/comment/claim for a plain member (artifact 36 v2 §2), so both
	// are manager moves — a plain member triages and delivers but does not close.
	// If that reading is ever reversed it is ONE row's `min` changing here, which
	// is why the distinction lives in the table rather than in a clause.
	{from: domain.RequestStatusNew, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject},
	{from: domain.RequestStatusTriaging, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject},
	{from: domain.RequestStatusNeedsInformation, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject},
	{from: domain.RequestStatusAccepted, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject},
	{from: domain.RequestStatusInProgress, to: domain.RequestStatusRejected, min: RequestCapabilityManager, kind: requestMoveReject},
	{from: domain.RequestStatusNew, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate},
	{from: domain.RequestStatusTriaging, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate},
	{from: domain.RequestStatusNeedsInformation, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate},
	{from: domain.RequestStatusAccepted, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate},
	{from: domain.RequestStatusInProgress, to: domain.RequestStatusDuplicate, min: RequestCapabilityManager, kind: requestMoveDuplicate},
}

// requestMoveFor finds the edge, if the approved graph has one. The answer is
// not filtered by capability: the caller checks that, so an edge's existence
// and who may use it stay separate questions (a refusal needs to know whether
// the move is legal at all before it can say who could have made it).
func requestMoveFor(from, to domain.RequestStatus) (requestMove, bool) {
	for _, m := range requestMoves {
		if m.from == from && m.to == to {
			return m, true
		}
	}
	return requestMove{}, false
}

// ---- The requester's answer ----

// answerDestination is where an answer to a question lands: the state the
// request was in when it was asked. Only the three working states qualify —
// nothing asks a question before triage begins, and I5 is explicit that the
// answer does not go forward to delivered.
func answerDestination(snapshot RequestSnapshot) (domain.RequestStatus, bool) {
	switch snapshot.PriorState {
	case domain.RequestStatusTriaging, domain.RequestStatusAccepted, domain.RequestStatusInProgress:
		return snapshot.PriorState, true
	default:
		return "", false
	}
}

// planRequestAnswer validates the requester's way out of needs_information.
func planRequestAnswer(in RequestTransitionInput, from, to domain.RequestStatus) (*RequestTransitionPlan, error) {
	prior, ok := answerDestination(in.Request)
	if !ok {
		if in.Request.PriorState == "" {
			return nil, authz.RequestInvalid("an answer restores the state the question interrupted: the request is in %s but its prior state is unknown, so the caller must supply it from the request's history", from)
		}
		return nil, authz.RequestInvalid("an answer restores the state the question interrupted, and %q is not one it can restore: a request is asked about while it is triaging, accepted or in progress (I5)", in.Request.PriorState)
	}
	if to != prior {
		if to == domain.RequestStatusDelivered || to == domain.RequestStatusRequesterAccepted {
			return nil, authz.RequestInvalid("an answer does not deliver or accept a request: it returns it to %s, where the team continues (I5). Delivery and acceptance are separate acts, by the team and by the requester respectively", prior)
		}
		return nil, refuseRequestMove(from, to, in.Request, in.Capability)
	}
	if err := checkRequestAnswerEvidence(in.Evidence); err != nil {
		return nil, err
	}
	move := requestMove{from: from, to: prior, min: RequestCapabilityRequester}
	return buildRequestTransitionPlan(move, in), nil
}

// ---- Refusals ----

// checkRequesterActor re-checks a declared requester capability against the
// request's own requester row. The capability is an input, so it could be
// passed by a caller that resolved it wrongly (or not at all); acceptance is
// the one act that must not depend on a caller getting that right. A request
// with no requester user — a connector's or service client's submission — has
// nobody who can accept it or answer for it, and says so.
func checkRequesterActor(snapshot RequestSnapshot, actorUserID int) error {
	if snapshot.RequesterUserID == nil {
		return authz.Forbidden("request %d has no requester user (it was submitted by a service or connector), so no one can answer for it, accept it or reopen it on the requester's behalf", snapshot.ID)
	}
	if actorUserID == 0 || actorUserID != *snapshot.RequesterUserID {
		return authz.Forbidden("a requester act is the requester's alone (I1): user %d is not request %d's requester, and no team member, agent, token or connector may act on their behalf", actorUserID, snapshot.ID)
	}
	return nil
}

// checkRequestTrigger refuses everything that is not an explicit act (I8).
func checkRequestTrigger(trigger RequestTrigger) error {
	switch trigger {
	case RequestTriggerExplicit:
		return nil
	case RequestTriggerLinkedWork:
		return authz.RequestInvalid("a linked ticket's completion never moves a request (I8): the request's state changes only when a person or agent decides it does, and a closed, merged or started ticket may at most be shown as a recommendation")
	case RequestTriggerAggregate:
		return authz.RequestInvalid("the outcome aggregate reports on linked work; it never moves the request (I8, E2). Deliver the request explicitly, with its outcome brief and its delivered links")
	case RequestTriggerSweep:
		return authz.RequestInvalid("a clock or background job never moves a request (I8): v1 has no SLA and no automatic transition (D8), and needs_information records needs_info_since so a later clock can exclude the wait rather than act on it")
	case "":
		return authz.RequestInvalid("a transition says what asked for it: pass trigger %q for an explicit act, and nothing else ever moves a request (I8)", string(RequestTriggerExplicit))
	default:
		return authz.RequestInvalid("unknown request transition trigger %q", string(trigger))
	}
}

// refuseRequestMove is the illegal-move refusal (I7): request.invalid, naming
// the states this caller may reach instead.
func refuseRequestMove(from, to domain.RequestStatus, snapshot RequestSnapshot, capability RequestCapability) error {
	next := AllowedRequestTransitions(snapshot, capability)
	if len(next) == 0 {
		return authz.RequestInvalid("a request in %s cannot move to %s, and there is nothing you may move it to from %s", from, to, from)
	}
	return authz.RequestInvalid("a request in %s cannot move to %s: from %s you may move it to %s", from, to, from, requestStatusNames(next))
}

// refuseRequestMoveCapability refuses a move the graph allows but this caller
// may not make. A caller with no standing has already been answered not-found
// in PlanRequestTransition; this is the "you can see it, not do it" case.
func refuseRequestMoveCapability(move requestMove, capability RequestCapability) error {
	if move.to == domain.RequestStatusRequesterAccepted {
		return authz.Forbidden("acceptance is the requester's alone (I1, D5): only the request's own requester may accept a delivery, in the web UI, and being %s confers none of it — no team member, team lead, agent, API token or connector may accept on their behalf", capability)
	}
	return authz.Forbidden("a request in %s may move to %s, but that move belongs to %s, not %s", move.from, move.to, move.min, capability)
}

// ---- Evidence ----

// checkRequestMoveEvidence validates the evidence an edge's kind requires and
// refuses evidence it would not read. Refusing the extra fields is what makes
// the plan trustworthy: if a caller passes an outcome brief to a move that does
// not deliver, the brief was not written, and a policy that silently dropped it
// would let the caller believe it had been.
func checkRequestMoveEvidence(move requestMove, snapshot RequestSnapshot, ev RequestTransitionEvidence) error {
	switch move.kind {
	case requestMovePlain:
		return checkNoExtraEvidence(ev, evidenceUse{})
	case requestMoveQuestion:
		return checkRequestQuestionEvidence(ev)
	case requestMoveReject:
		return checkRequestRejectEvidence(ev)
	case requestMoveDuplicate:
		return checkRequestDuplicateEvidence(snapshot, ev)
	case requestMoveDelivery:
		return checkRequestDeliveryEvidence(ev)
	default:
		return authz.RequestInvalid("request move %s → %s has no evidence rule: this is a bug in the transition table", move.from, move.to)
	}
}

// evidenceUse says which evidence fields one move reads; every other field must
// be empty (see checkNoExtraEvidence).
type evidenceUse struct {
	question  bool
	answer    bool
	reject    bool
	duplicate bool
	delivery  bool
}

// checkNoExtraEvidence refuses evidence a move does not read, naming each field.
func checkNoExtraEvidence(ev RequestTransitionEvidence, use evidenceUse) error {
	var extra []string
	if !use.question && strings.TrimSpace(ev.Question) != "" {
		extra = append(extra, "question")
	}
	if !use.answer && ev.AnswerTo != 0 {
		extra = append(extra, "answer_to")
	}
	if !use.reject && strings.TrimSpace(ev.RejectReason) != "" {
		extra = append(extra, "reject_reason")
	}
	if !use.duplicate && (ev.CanonicalRequestID != 0 || ev.CanonicalIsDuplicate) {
		extra = append(extra, "canonical_request_id")
	}
	if !use.delivery && (strings.TrimSpace(ev.OutcomeBrief) != "" || ev.DeliveredByCount != 0 || ev.NoArtifact) {
		extra = append(extra, "outcome_brief/delivered_by_count/no_artifact")
	}
	if len(extra) == 0 {
		return nil
	}
	return authz.RequestInvalid("%s is not evidence for this move: it does not read it, so it would not be written, and passing it would read as though it had been", strings.Join(extra, ", "))
}

// checkRequestQuestionEvidence requires the question I5 obliges.
func checkRequestQuestionEvidence(ev RequestTransitionEvidence) error {
	if err := checkNoExtraEvidence(ev, evidenceUse{question: true}); err != nil {
		return err
	}
	q, err := domain.NormalizeRequestBrief("question", ev.Question)
	if err != nil {
		return authz.RequestInvalid("%s", err)
	}
	if q == "" {
		return authz.RequestInvalid("entering needs_information asks the requester something: a question is required, because asking obliges an answer (I5)")
	}
	return nil
}

// checkRequestAnswerEvidence requires the question the answer replies to.
func checkRequestAnswerEvidence(ev RequestTransitionEvidence) error {
	if err := checkNoExtraEvidence(ev, evidenceUse{answer: true}); err != nil {
		return err
	}
	if ev.AnswerTo <= 0 {
		return authz.RequestInvalid("an answer names the question comment it answers (answer_to): the answer is what returns the request to the state the question interrupted (I5)")
	}
	return nil
}

// checkRequestRejectEvidence requires the reason I3 and the requester's
// visibility of it imply.
func checkRequestRejectEvidence(ev RequestTransitionEvidence) error {
	if err := checkNoExtraEvidence(ev, evidenceUse{reject: true}); err != nil {
		return err
	}
	reason, err := boundedRequestTransitionText("reject_reason", ev.RejectReason, domain.MaxRequestRejectReasonLen)
	if err != nil {
		return authz.RequestInvalid("%s", err)
	}
	if reason == "" {
		return authz.RequestInvalid("a rejected request carries reject_reason: the requester is shown why it was declined (I3)")
	}
	return nil
}

// checkRequestDuplicateEvidence requires a canonical, refuses a self-fold and
// refuses a canonical that is itself a duplicate (D7).
func checkRequestDuplicateEvidence(snapshot RequestSnapshot, ev RequestTransitionEvidence) error {
	if err := checkNoExtraEvidence(ev, evidenceUse{duplicate: true}); err != nil {
		return err
	}
	if ev.CanonicalRequestID <= 0 {
		return authz.RequestInvalid("a duplicate names the canonical request in duplicate_of: it is what the requester follows from then on (I3)")
	}
	if snapshot.ID != 0 && ev.CanonicalRequestID == snapshot.ID {
		return authz.RequestInvalid("request %d cannot be a duplicate of itself", snapshot.ID)
	}
	if ev.CanonicalIsDuplicate {
		return authz.RequestInvalid("duplicate_of must name a canonical request, and request %d is itself a duplicate: fold to the root of its chain (D7 — a canonical is never a duplicate)", ev.CanonicalRequestID)
	}
	return nil
}

// checkRequestDeliveryEvidence requires I2's pair: an outcome brief, and
// exactly one of a delivered link or an explicit no-artifact record.
func checkRequestDeliveryEvidence(ev RequestTransitionEvidence) error {
	if err := checkNoExtraEvidence(ev, evidenceUse{delivery: true}); err != nil {
		return err
	}
	brief, err := domain.NormalizeRequestBrief("outcome_brief", ev.OutcomeBrief)
	if err != nil {
		return authz.RequestInvalid("%s", err)
	}
	if brief == "" {
		return authz.RequestInvalid("delivered is evidenced: outcome_brief says what is and is not included (I2)")
	}
	if ev.DeliveredByCount < 0 {
		return authz.RequestInvalid("delivered_by_count is a count of delivered_by links and cannot be negative")
	}
	switch {
	case ev.DeliveredByCount > 0 && ev.NoArtifact:
		return authz.RequestInvalid("a delivery has either delivered_by links or an explicit no-artifact record, not both: say which it is, so the requester's view can state it honestly (I2)")
	case ev.DeliveredByCount == 0 && !ev.NoArtifact:
		return authz.RequestInvalid("delivered needs at least one delivered_by link (a task, an artifact or a receipt) or an explicit no-artifact record: a brief alone does not say the outcome exists (I2, D10)")
	}
	return nil
}

// ---- Building the plan ----

// buildRequestTransitionPlan fills the plan for a move that has already passed
// the graph, the capability and the evidence checks. Columns are appended in
// the order of the RequestSet* constants so a plan compares exactly in a test.
func buildRequestTransitionPlan(move requestMove, in RequestTransitionInput) *RequestTransitionPlan {
	plan := &RequestTransitionPlan{From: move.from, To: move.to}
	ev := in.Evidence
	switch move.kind {
	case requestMoveQuestion:
		plan.Question, _ = domain.NormalizeRequestBrief("question", ev.Question) // already validated
	case requestMoveReject:
		plan.RejectReason, _ = boundedRequestTransitionText("reject_reason", ev.RejectReason, domain.MaxRequestRejectReasonLen)
	case requestMoveDuplicate:
		canonical := ev.CanonicalRequestID
		plan.DuplicateOf = &canonical
	case requestMoveDelivery:
		plan.OutcomeBrief, _ = domain.NormalizeRequestBrief("outcome_brief", ev.OutcomeBrief)
	}
	// An answer names the question it answers. Every other move refuses the
	// field (checkNoExtraEvidence), so setting it whenever it is present is the
	// same check stated once.
	plan.AnswerTo = ev.AnswerTo
	if move.reopen {
		plan.Reopened = true
	}
	if move.detach && in.Request.DuplicateOf != nil {
		// Only a request that is actually folded is detached: a duplicate whose
		// pointer is already gone (a retried reopen) applies the state move and
		// no detach, and 053 is satisfied either way.
		plan.ClearDuplicateOf = true
	}
	plan.Set = append(plan.Set, requestTransitionColumns(move)...)
	if move.to == domain.RequestStatusRequesterAccepted {
		actor := in.ActorUserID
		plan.AcceptedBy = &actor
	}
	return plan
}

// requestTransitionColumns lists the columns the move's write must set, in the
// constants' order.
func requestTransitionColumns(move requestMove) []RequestTransitionColumn {
	var out []RequestTransitionColumn
	if move.to == domain.RequestStatusNeedsInformation {
		out = append(out, RequestSetNeedsInfoSince)
	}
	if move.to == domain.RequestStatusDelivered {
		out = append(out, RequestSetDeliveredAt)
	}
	if move.to == domain.RequestStatusRequesterAccepted {
		out = append(out, RequestSetAcceptedAt)
	}
	if move.to == domain.RequestStatusRejected || move.to == domain.RequestStatusDuplicate {
		out = append(out, RequestSetClosedAt)
	}
	if move.reopen {
		out = append(out, RequestSetReopenedAt, RequestSetReopenedCount)
	}
	return out
}

// ---- Small shared bits ----

// requestStatusNames lists states for a refusal message, in the order given.
func requestStatusNames(states []domain.RequestStatus) string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return strings.Join(out, ", ")
}

// boundedRequestTransitionText trims s and refuses it over limit characters,
// naming the field. domain.NormalizeRequestBrief is the exported normalizer for
// the brief-sized fields (question, outcome_brief); reject_reason has its own,
// tighter bound (domain.MaxRequestRejectReasonLen) that no exported normalizer
// covers, so it is measured here. The unit (runes) and the message shape are
// deliberately identical to the domain's, and if domain ever exports its own
// bounded-text helper this should call it rather than keep a second copy.
func boundedRequestTransitionText(field, s string, limit int) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n > limit {
		return "", fmt.Errorf("%s is over %d characters (got %d)", field, limit, n)
	}
	return s, nil
}
