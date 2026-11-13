package service

// Team request lifecycle: the write half of the one state machine (epic 202,
// story 548, AMP #3960; approved product contract artifact 36 v2 §1, §2 and §4;
// build slices S5 and S6 of discoveries/team-inbox-approved-build-slices.md).
//
// The decision is service/request_transitions.go's (AMP #3953): a pure function
// that says whether a caller may move a request to a state, on what evidence.
// This file is the only place that acts on that answer. It
//
//  1. loads the request the caller is acting in and resolves the caller's
//     capability from the rows on this call (authz.InboxRole and
//     Checker.RequireTeamManager — nothing cached, contract §2);
//  2. fills the policy's evidence from TYPED things it resolved itself: the
//     delivered_by links actually stored on the request, the canonical request
//     actually read, the question comment actually in the thread, the prior
//     state actually in the history. A caller supplies words and ids it means;
//     it never supplies a count, a URL or a claim that the outcome exists;
//  3. asks PlanRequestTransition, so every refusal (an illegal edge, a
//     capability, a missing reason, a non-explicit trigger) is the policy's
//     words and no second rule exists here;
//  4. applies the plan in ONE transaction (repository.ApplyRequestTransition, or
//     MarkDuplicate for the one fold that owns the duplicate relation row):
//     state, rev + 1, the state-entry columns, the question/answer comment, the
//     duplicate detach and exactly one request_events row, all or nothing;
//  5. audits the act once, after it committed, as `request.transition`.
//
// Five properties this file is responsible for, and where each one lives:
//
//   - Acceptance is the requester's alone (I1, D5). RequestCapabilityFor gives
//     RequestCapabilityRequester only to a person in the web UI whose user id is
//     the request's requester, and the policy re-checks that id. Here, the two
//     named requester entry points (AcceptRequest, AnswerRequest) refuse a
//     non-UI caller BEFORE the request is loaded, so an agent account, an API
//     token, a connector and an "on behalf" agent are refused with the same
//     answer for every id — no existence to leak, and no path at all by which a
//     credential accepts an outcome.
//   - Asking obliges an answer (I5). A team move into needs_information carries
//     its question and the repository writes the comment in the same
//     transaction (there is no way to move first and ask later). An answer
//     names the question it answers, which is verified against this request's
//     own thread, and its destination is the state the question interrupted,
//     read from the request's history because the schema keeps no prior-state
//     column (053).
//   - Reject and duplicate carry what makes them honest (I3, D7). A reject needs
//     its reason; a duplicate names a canonical this service READ — same
//     tenant, not itself, and not a duplicate itself — and the fold itself is
//     MarkDuplicate's write, which flattens the chain and writes the relation.
//   - Reopen is a transition, not a state (D6). The requester reopens from
//     delivered/requester_accepted and the team from rejected/duplicate; every
//     reopen raises reopened_count, and the one that leaves duplicate detaches
//     duplicate_of and the live relation row in the same statement set.
//   - Nothing auto-advances from linked work (I8). This file reads no ticket, no
//     task, no receipt and no aggregate: the only trigger it can produce is
//     RequestTriggerExplicit, and it is the only value it passes. A linked
//     ticket's completion therefore cannot reach a request's state by any path
//     in this process — TestRequestLifecycle_NoTicketStateCanMoveARequest pins
//     the absence structurally.
//
// What this file deliberately does not do — each one another slice's concern:
//
//   - no endpoint, no MCP tool, no UI. This is the service the thin adapters
//     (F2–F4, I2, J1–J6) call; #3960 ships no surface.
//   - no planning, no dispatch, no approval. A delivery links existing work
//     (E1's attach owns writing those links); nothing here creates a ticket or
//     approves one.
//   - no read path. Which view a caller gets (team record versus requester
//     projection) is the read service's (S2/E3).
//   - no concurrent *sweep*. Every move is one caller's explicit act on one
//     request, on the revision it read.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// RequestDeliveryEvidence resolves what a delivery is evidenced by. A delivery
// states that the outcome exists (I2); the evidence is the request's own
// delivered_by relations — tasks, artifacts and receipts whose target was
// verified in this org when the link was written (repository.LinkRequest) — and
// never anything a caller asserts about them.
//
// It is an interface for one reason: the service must be able to prove what it
// counts, and a test must be able to state "these are the links" without writing
// four tables. What it can NOT do is trust a caller: the input type has no
// count, no link and no URL, and countDeliveredBy refuses a row that is not a
// delivered_by relation to a task, artifact or receipt rather than counting it.
type RequestDeliveryEvidence interface {
	// DeliveredByLinks returns the stored delivered_by relations of one request
	// in its tenant. An empty slice is "nothing linked yet", which is a fact the
	// policy distinguishes from "there is nothing to link" (the explicit
	// no-artifact record).
	DeliveredByLinks(ctx context.Context, requestID, orgID, teamID int) ([]domain.RequestLink, error)
}

// requestDeliveryEvidenceFromRepo is the production implementation: the request
// links table, read tenant-scoped (a request in another team is ErrNotFound,
// exactly as every other request read answers).
type requestDeliveryEvidenceFromRepo struct{ repo *repository.Repo }

func (e requestDeliveryEvidenceFromRepo) DeliveredByLinks(ctx context.Context, requestID, orgID, teamID int) ([]domain.RequestLink, error) {
	return e.repo.RequestLinks(ctx, requestID, orgID, teamID, domain.RequestLinkDeliveredBy)
}

// requestDeliveryEvidence is the evidence source this Service uses: the injected
// one when a test set it, otherwise the repository.
func (s *Service) requestDeliveryEvidence() RequestDeliveryEvidence {
	if s.RequestEvidence != nil {
		return s.RequestEvidence
	}
	return requestDeliveryEvidenceFromRepo{repo: s.Repo}
}

// TransitionRequestInput is one lifecycle act on one request, as a surface
// receives it. Every field is either the caller's own words, an id it means, or
// the revision it read — there is no field for a count of evidence, a URL, an
// actor, a trigger or a capability, because those are resolved here or by the
// policy.
//
// A field outside the destination's kind is refused by the policy (a question on
// a plain triage step, an outcome brief on a reject): the write never silently
// ignores something the caller thought it was sending.
type TransitionRequestInput struct {
	// RequestID is the request, and TeamID the inbox the caller is acting in.
	// The org is the team's, read from its row: a caller cannot aim a move at
	// another tenant's org, and a request in another team is not found.
	RequestID int
	TeamID    int

	// To is the destination. Empty is allowed for one act only: the requester's
	// answer does not choose where the request goes — it goes back to the state
	// the question interrupted, which is read from the request's history.
	To domain.RequestStatus

	// ExpectedRev is the revision the caller read (an If-Match). Zero means "the
	// revision this call reads", which is honest because the decision is made on
	// that same read; the write still refuses if the row moved in between.
	ExpectedRev int

	// Question is what the team asks the requester, required to enter
	// needs_information (I5) and written as the thread's `question` comment.
	Question string
	// Answer is the requester's reply (it leaves needs_information), and
	// AnswerTo names the question comment it answers. AnswerTo may be 0 when the
	// requester answers the question currently open, which is the newest
	// question in the thread; with no question to answer the act is refused,
	// because an answer with no question is not an answer.
	Answer   string
	AnswerTo int64

	// RejectReason is why the request is declined (I3), shown to the requester.
	RejectReason string
	// CanonicalRequestID is the request a duplicate folds into (I3, D7). The
	// canonical itself is read here: a canonical in another team is not found,
	// and one that is itself a duplicate is refused so a fold lands on the root.
	CanonicalRequestID int

	// OutcomeBrief is the delivery note: what is and is not included (I2).
	OutcomeBrief string
	// NoArtifact is the explicit "the outcome exists and there is nothing to link"
	// record. It is a statement, not an absence: without it a delivery needs at
	// least one stored delivered_by relation, and with links already present
	// setting it is refused ("not both").
	NoArtifact bool
}

// TransitionRequestResult is one applied move.
type TransitionRequestResult struct {
	// Request is the stored row after the move.
	Request *domain.TeamRequest
	// Comment is the question or answer the move appended, or nil when the act
	// carried none.
	Comment *domain.RequestComment
	// Changed is false only for a fold that was already in force (a replay of
	// the same duplicate, which wrote nothing); every other successful move
	// wrote. See repository.MarkDuplicate.
	Changed bool
	// Plan is the decision that was applied, so a surface can report why the
	// request is where it is (and a test can assert the move against the policy
	// rather than against its own idea of the graph).
	Plan *RequestTransitionPlan
}

// TransitionRequest performs one explicit lifecycle act: it loads the request,
// resolves the caller's capability, fills the policy's evidence from typed
// records, decides, and applies the decision atomically.
//
// A refusal costs nothing: an illegal edge, a capability, missing evidence and an
// inapplicable trigger all come back from the policy before the write starts, and
// a write that finds the row has moved is refused rather than applied. A caller
// with no standing on the request is answered with the surface's single not-found
// — the same bytes an unknown id gets — so ids cannot be walked.
func (s *Service) TransitionRequest(ctx context.Context, p authz.Principal, in TransitionRequestInput) (*TransitionRequestResult, error) {
	return s.transitionRequest(ctx, p, in)
}

// AcceptRequest is the requester's acceptance of a delivery (I1, D5): the one
// act no team member, team lead, agent account, API token or connector may
// perform, with no on-behalf variant in v1.
//
// The caller must be a person in the web UI, and that is checked BEFORE the
// request is loaded: an agent or a token is refused with the same answer for
// every id, so the refusal says nothing about whether the request exists, and
// there is no code path by which a credential accepts an outcome. The rest is the
// policy's: only the request's own requester, only from delivered.
func (s *Service) AcceptRequest(ctx context.Context, p authz.Principal, requestID, teamID, expectedRev int) (*TransitionRequestResult, error) {
	if err := requireRequesterUI(p, "Accepting a delivered request"); err != nil {
		return nil, err
	}
	return s.transitionRequest(ctx, p, TransitionRequestInput{
		RequestID:   requestID,
		TeamID:      teamID,
		To:          domain.RequestStatusRequesterAccepted,
		ExpectedRev: expectedRev,
	})
}

// AnswerRequest is the requester's answer to a question (I5): it returns the
// request to the state the question interrupted. Like acceptance it is a person's
// act in the web UI — a credential is not the requester (contract §5 puts no
// answer on the API or MCP surface) — and the destination is read from the
// request's history, never chosen by the caller.
func (s *Service) AnswerRequest(ctx context.Context, p authz.Principal, in TransitionRequestInput) (*TransitionRequestResult, error) {
	if err := requireRequesterUI(p, "Answering a request's question"); err != nil {
		return nil, err
	}
	in.To = ""
	return s.transitionRequest(ctx, p, in)
}

// ReopenRequest is a reopen (D6): the requester reopens from delivered or
// requester_accepted, the team from rejected or duplicate. Which of the two
// applies is the policy's decision; this entry point only refuses a non-person
// caller when the requester's half is the one that would succeed, which it cannot
// know until it has read the request — so it is the general path with the
// destination the reopen lands in.
func (s *Service) ReopenRequest(ctx context.Context, p authz.Principal, requestID, teamID, expectedRev int) (*TransitionRequestResult, error) {
	return s.transitionRequest(ctx, p, TransitionRequestInput{
		RequestID:   requestID,
		TeamID:      teamID,
		To:          domain.RequestStatusTriaging,
		ExpectedRev: expectedRev,
	})
}

// RequesterRequestRef is how a requester names one of their own requests: the
// opaque public reference their submission answered with, or its serial.
// Exactly one is set.
//
// It exists because the two surfaces genuinely address a request differently.
// A triager works from the team's record and has the serial; a requester holds
// only the reference, and the requester projection (domain.RequesterRequestView)
// deliberately carries neither the serial nor the team — it is a read shape, not
// an addressing scheme. The requester acts therefore need a way to name their
// request that is scoped by their own requester column, and that resolution
// belongs here rather than in an adapter: a REST handler that looked a
// reference up itself would be a second tenant-scoped lookup with a second
// definition of what a reference may name (the reason request_mutations.go
// refuses a reference on a team write path instead of searching for it).
//
// There is no team field on purpose. The team is read from the loaded row, which
// the requester column already scoped to this person, so a caller cannot pair a
// reference with someone else's inbox.
type RequesterRequestRef struct {
	// RequestID is the request's serial, 0 when PublicRef names it.
	RequestID int
	// PublicRef is the requester's opaque reference (the display form's REQ-
	// prefix is accepted and stripped). Empty when RequestID names it.
	PublicRef string
}

// AcceptRequestByRef is AcceptRequest for the way a requester actually holds
// their request: by public reference (or serial), not by the team and serial a
// triager's path carries.
//
// It is the same act and the same rules; only the addressing differs. The
// person-in-the-web-UI check runs FIRST, before the reference is resolved, so an
// agent account, an API token or a submit-only credential learns nothing about
// whether a reference was ever minted — the answer it gets is the same bytes for
// a real reference, an unknown one and someone else's. A requester who is not a
// member of the team passes that check (they are the person) and is then
// refused, if the request is not theirs, by the own-request load: one 404.
func (s *Service) AcceptRequestByRef(ctx context.Context, p authz.Principal, ref RequesterRequestRef, expectedRev int) (*TransitionRequestResult, error) {
	if err := requireRequesterUI(p, "Accepting a delivered request"); err != nil {
		return nil, err
	}
	req, err := s.loadOwnTransitionTarget(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	return s.AcceptRequest(ctx, p, req.ID, req.TeamID, expectedRev)
}

// AnswerRequestByRef is AnswerRequest for the requester's own request, named the
// way they hold it.
//
// Like acceptance it refuses a non-person caller before the reference is
// resolved (the same requireRequesterUI inside AnswerRequest, which runs first
// here because this function does the person check itself before loading) — so a
// credential cannot use a guessed reference to learn anything, and there is no
// path by which an agent answers on a requester's behalf. The destination is
// read from the request's history, never supplied by the caller.
func (s *Service) AnswerRequestByRef(ctx context.Context, p authz.Principal, ref RequesterRequestRef, in TransitionRequestInput) (*TransitionRequestResult, error) {
	if err := requireRequesterUI(p, "Answering a request's question"); err != nil {
		return nil, err
	}
	req, err := s.loadOwnTransitionTarget(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	in.RequestID, in.TeamID = req.ID, req.TeamID
	return s.AnswerRequest(ctx, p, in)
}

// ReopenRequestByRef is ReopenRequest for the requester's own request, named the
// way they hold it. The decision stays the policy's: a requester may reopen from
// delivered or requester_accepted (D6), and a request the team closed is reopened
// by the team, on the team's own path.
//
// Unlike acceptance this is not a person-only act in the policy (a team member
// reopens too), so it does not pre-refuse a credential: a token acting for a
// person who is the requester resolves through RequestCapabilityFor, which is the
// one place that answers "is this caller the requester".
func (s *Service) ReopenRequestByRef(ctx context.Context, p authz.Principal, ref RequesterRequestRef, expectedRev int) (*TransitionRequestResult, error) {
	req, err := s.loadOwnTransitionTarget(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	return s.ReopenRequest(ctx, p, req.ID, req.TeamID, expectedRev)
}

// loadOwnTransitionTarget loads a request the caller is the requester of, from
// the reference they hold.
//
// Three properties, each one load-bearing:
//
//   - No user row means nothing can be theirs, so it is the surface's 404 rather
//     than a lookup scoped by user 0.
//   - The reference's shape is normalized by the domain's own rule (the display
//     form's REQ- prefix is stripped, a value the domain could never have minted
//     is refused request.invalid rather than searched for) — the same
//     normalizeReadRef the reads use, so a write and a read cannot disagree about
//     what a reference may name.
//   - The load is the own-request read, scoped by the caller's own requester
//     column (GetRequestByRequester / …Ref). Another person's reference, a
//     service- or connector-submitted request with no requester, and a reference
//     that was never minted are therefore all ErrNotFound, and the surface's one
//     404 is the same bytes for all three.
func (s *Service) loadOwnTransitionTarget(ctx context.Context, p authz.Principal, ref RequesterRequestRef) (*domain.TeamRequest, error) {
	if !p.HasUser() {
		return nil, authz.RequestNotFound()
	}
	named, err := normalizeReadRef(RequestReadInput{RequestID: ref.RequestID, PublicRef: ref.PublicRef})
	if err != nil {
		return nil, err
	}
	var req *domain.TeamRequest
	if named.id > 0 {
		req, err = s.Repo.GetRequestByRequester(ctx, named.id, p.UserID)
	} else {
		req, err = s.Repo.GetRequestByRequesterRef(ctx, named.publicRef, p.UserID)
	}
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, authz.RequestNotFound()
		}
		return nil, err
	}
	return req, nil
}

// requireRequesterUI refuses a requester act that did not arrive from a person in
// the web UI. It runs before anything is loaded, so it cannot leak whether an id
// exists, and its message names what a caller should do instead rather than
// leaving an agent to guess (authz.UIOnly's house style).
func requireRequesterUI(p authz.Principal, what string) error {
	if p.InUI() && p.HasUser() {
		return nil
	}
	return authz.Forbidden("%s is the requester's own act, done by that person in the AMP web UI (I1, D5): a team member, an agent account, an API token and a connector cannot do it, and v1 has no on-behalf acceptance. Nothing was changed.", what)
}

// ---- The one body ----

// transitionRequest is the single implementation every entry point above shares.
// It is deliberately linear: load, resolve, decide, apply, audit — each step
// handing the next exactly what it established, and no step reading anything the
// one before it did not authorize.
func (s *Service) transitionRequest(ctx context.Context, p authz.Principal, in TransitionRequestInput) (*TransitionRequestResult, error) {
	team, req, capability, err := s.loadTransitionTarget(ctx, p, in)
	if err != nil {
		return nil, err
	}
	// The request's prior state is read once, and only when it can matter: a
	// request waiting on an answer is the one state whose history affects a move.
	// The policy reads it to know where an answer goes (053 keeps no prior-state
	// column on purpose: a second column could disagree with the history).
	var prior domain.RequestStatus
	if req.State == domain.RequestStatusNeedsInformation {
		prior, err = s.Repo.RequestPriorState(ctx, req.ID, req.OrgID, req.TeamID)
		if err != nil {
			return nil, transitionRequestError(req.ID, err)
		}
	}
	to := in.To
	if to == "" {
		// The requester's answer does not choose where the request goes: it goes
		// back to the state the question interrupted. An empty prior state is left
		// empty on purpose — the policy refuses it with its own explanation rather
		// than this file inventing a destination for a waiting request.
		to = prior
	}
	snapshot := RequestSnapshotOf(req)
	snapshot.PriorState = prior
	// Evidence is resolved only when the caller could make this move at all. The
	// order matters for more than tidiness: resolving a duplicate's canonical, or
	// counting a delivery's links, is a read of records the caller may not be
	// entitled to learn about, so a caller whose capability already rules the move
	// out is refused FIRST, from the policy's own words (I7), without this service
	// touching anything else. AllowedRequestTransitions is the policy's read-side
	// answer to exactly this question — the same function that renders a UI's
	// actions — so the two cannot disagree about what "could make this move" means.
	evidence := RequestTransitionEvidence{Trigger: RequestTriggerExplicit}
	if slices.Contains(AllowedRequestTransitions(snapshot, capability), to) {
		evidence, err = s.transitionEvidence(ctx, p, req, capability, to, in)
		if err != nil {
			return nil, err
		}
	}
	expectRev := in.ExpectedRev
	if expectRev <= 0 {
		expectRev = req.Rev
	} else if expectRev != req.Rev {
		// The caller's copy of the request is stale before anything was decided:
		// a 409 it can resolve, not a 404 (it may see the row) and not a write. The
		// cause is the repository's own typed conflict — not a copy of its sentinel —
		// so a surface can read the revision the row is actually at off the error
		// and tell the client what to retry with (the same reason
		// requestMutationRefusal carries the error), while errors.Is still matches
		// ErrConflict through the type's Is method.
		return nil, &codedCause{
			err:   authz.RequestConflict("request %d is at revision %d, not %d: reload it, then decide again", req.ID, req.Rev, expectRev),
			cause: &repository.RequestRevConflict{RequestID: req.ID, Expected: expectRev, Current: req.Rev},
		}
	}
	plan, err := PlanRequestTransition(RequestTransitionInput{
		Request:     snapshot,
		To:          to,
		Capability:  capability,
		ActorUserID: p.UserID,
		Evidence:    evidence,
	})
	if err != nil {
		return nil, err
	}

	applied, comment, changed, err := s.applyTransitionPlan(ctx, p, req, plan, expectRev, in)
	if err != nil {
		return nil, transitionRequestError(req.ID, err)
	}
	// The retention clock is the request's (D3), so a move that closes, reopens or
	// accepts it restamps its files' expiry. It runs after the commit and its
	// failure is logged, never returned: the move already happened, and the purge
	// re-checks the request's state, so an unsynced row is repaired by the next
	// pass rather than wrongly deleted. See request_retention.go.
	s.syncAttachmentRetention(ctx, applied, plan.To, plan.Reopened)
	s.recordTransition(ctx, p, team, applied, plan, comment)
	return &TransitionRequestResult{Request: applied, Comment: comment, Changed: changed, Plan: plan}, nil
}

// loadTransitionTarget loads the inbox the caller is acting in, the request, and
// the caller's capability on it — in that order, because each answer is an input
// to the next and the org comes from the team row rather than from the caller.
//
// The capability is resolved for every call, from the rows (authz.InboxRole and
// RequireTeamManager), which is the contract's per-request re-check: losing a team
// membership or a lead role takes effect on the very next act, with no cache to
// invalidate.
func (s *Service) loadTransitionTarget(ctx context.Context, p authz.Principal, in TransitionRequestInput) (*domain.Team, *domain.TeamRequest, RequestCapability, error) {
	if in.RequestID <= 0 {
		return nil, nil, RequestCapabilityNone, authz.RequestNotFound()
	}
	if in.TeamID <= 0 {
		return nil, nil, RequestCapabilityNone, authz.RequestInvalid("a request act names the inbox team it is performed in")
	}
	team, err := s.Repo.GetTeam(ctx, in.TeamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The same answer an unknown request gets: a caller who may not name a
			// team learns nothing about it, and the request surface has one 404.
			return nil, nil, RequestCapabilityNone, authz.RequestNotFound()
		}
		return nil, nil, RequestCapabilityNone, err
	}
	req, err := s.Repo.GetRequest(ctx, in.RequestID, team.OrgID, team.ID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, RequestCapabilityNone, authz.RequestNotFound()
		}
		return nil, nil, RequestCapabilityNone, err
	}
	role, err := s.Authz.InboxRole(ctx, p, team.ID)
	if err != nil {
		return nil, nil, RequestCapabilityNone, err
	}
	manager, err := isTeamManager(ctx, s.Authz, p, team)
	if err != nil {
		return nil, nil, RequestCapabilityNone, err
	}
	return team, req, RequestCapabilityFor(req, p, role, manager), nil
}

// isTeamManager reports whether p may manage team. A refusal is the answer
// "no", and it is told apart from a failure by being an authz error: everything
// else (a dead connection, a cancelled context) is returned, so a database
// problem never reads as a person without a lead role.
func isTeamManager(ctx context.Context, checker *authz.Checker, p authz.Principal, team *domain.Team) (bool, error) {
	err := checker.RequireTeamManager(ctx, p, team)
	if err == nil {
		return true, nil
	}
	var coded *authz.Error
	if errors.As(err, &coded) {
		return false, nil
	}
	return false, err
}

// transitionEvidence fills the policy's evidence from typed records, and only
// the fields the destination's kind reads — the policy refuses the rest, and a
// field this file filled that the move does not read would be a bug the caller
// could not see.
//
// Nothing here is derived from text, from a caller's count, or from a ticket:
//
//   - a question is the caller's own words;
//   - an answer names a question comment of this request (an id and nothing
//     else) and carries its words as a comment, which is why the answer text is
//     not evidence at all;
//   - a reject is the caller's reason;
//   - a duplicate names a canonical this function READ from the request table, so
//     CanonicalIsDuplicate is an observation rather than a caller's assertion;
//   - a delivery counts the request's stored delivered_by relations, validated
//     below; the caller's "no artifact" is an explicit statement this function
//     passes through as exactly that.
func (s *Service) transitionEvidence(ctx context.Context, p authz.Principal, req *domain.TeamRequest, capability RequestCapability,
	to domain.RequestStatus, in TransitionRequestInput) (RequestTransitionEvidence, error) {
	ev := RequestTransitionEvidence{Trigger: RequestTriggerExplicit}
	if req.State == domain.RequestStatusNeedsInformation && capability == RequestCapabilityRequester {
		// The answer path reads exactly one field, and the destination is the
		// prior state rather than a choice (see planRequestAnswer).
		answerTo := in.AnswerTo
		if answerTo <= 0 {
			latest, err := s.Repo.RequestLatestQuestion(ctx, req.ID, req.OrgID, req.TeamID)
			if err != nil && !errors.Is(err, repository.ErrNotFound) {
				return ev, transitionRequestError(req.ID, err)
			}
			if latest != nil {
				answerTo = latest.ID
			}
		}
		ev.AnswerTo = answerTo
		return ev, nil
	}
	switch to {
	case domain.RequestStatusNeedsInformation:
		ev.Question = trimRequestTransitionText(in.Question)
	case domain.RequestStatusRejected:
		ev.RejectReason = trimRequestTransitionText(in.RejectReason)
	case domain.RequestStatusDuplicate:
		if in.CanonicalRequestID <= 0 {
			return ev, authz.RequestInvalid("a duplicate names the canonical request it folds into (duplicate_of): that is the request the requester follows from then on (I3)")
		}
		canonical, err := s.Repo.GetRequest(ctx, in.CanonicalRequestID, req.OrgID, req.TeamID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return ev, authz.RequestInvalid("request %d cannot be a duplicate of request %d: the canonical must be a request in this team", req.ID, in.CanonicalRequestID)
			}
			return ev, err
		}
		ev.CanonicalRequestID = canonical.ID
		// Read, not asserted: a canonical is never itself a duplicate (D7), and
		// saying so here is what lets the policy refuse with its own words instead
		// of the caller's claim being taken on trust.
		ev.CanonicalIsDuplicate = canonical.DuplicateOf != nil
	case domain.RequestStatusDelivered:
		count, err := s.countDeliveredBy(ctx, req)
		if err != nil {
			return ev, transitionRequestError(req.ID, err)
		}
		ev.OutcomeBrief = trimRequestTransitionText(in.OutcomeBrief)
		ev.DeliveredByCount = count
		ev.NoArtifact = in.NoArtifact
	}
	return ev, nil
}

// countDeliveredBy answers how many delivered_by relations the request actually
// has, and refuses to count anything that is not one.
//
// The validation is the point. A delivery's evidence is a typed relation whose
// target existed in this org when it was linked (repository.LinkRequest verified
// it, and 055's link kinds restrict the target to a task, an artifact or a
// receipt); a row that does not look like one — whatever produced it — is
// refused rather than counted, because counting it would turn "something is
// attached" into "the outcome exists". There is no path here by which a URL, a
// free-text reference or a caller's tally can evidence a delivery.
func (s *Service) countDeliveredBy(ctx context.Context, req *domain.TeamRequest) (int, error) {
	links, err := s.requestDeliveryEvidence().DeliveredByLinks(ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range links {
		if l.RequestID != req.ID || l.LinkKind != domain.RequestLinkDeliveredBy {
			return 0, fmt.Errorf("request %d: a delivered_by link row is not a delivery evidence record (request %d, kind %q): %w",
				req.ID, l.RequestID, l.LinkKind, repository.ErrRequestTransitionShape)
		}
		switch l.TargetKind {
		case domain.RequestTargetTask, domain.RequestTargetArtifact, domain.RequestTargetReceipt:
		default:
			return 0, fmt.Errorf("request %d: a delivered_by link to a %s is not delivery evidence (D10 allows a task, an artifact or a receipt): %w",
				req.ID, l.TargetKind, repository.ErrRequestTransitionShape)
		}
		n++
	}
	return n, nil
}

// applyTransitionPlan performs the plan. The fold is MarkDuplicate's write (it
// owns the duplicate relation row and the chain flattening); every other move is
// repository.ApplyRequestTransition's single transaction.
func (s *Service) applyTransitionPlan(ctx context.Context, p authz.Principal, req *domain.TeamRequest,
	plan *RequestTransitionPlan, expectRev int, in TransitionRequestInput) (*domain.TeamRequest, *domain.RequestComment, bool, error) {
	actor, actorID := p.Label(), p.UserRef()
	if plan.To == domain.RequestStatusDuplicate && plan.DuplicateOf != nil {
		folded, changed, err := s.Repo.MarkDuplicate(ctx, repository.RequestDuplicateWrite{
			RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID,
			CanonicalID: *plan.DuplicateOf,
			ExpectRev:   expectRev,
			CreatedBy:   actorID,
			Actor:       actor,
			ActorUserID: actorID,
		})
		if err != nil {
			return nil, nil, false, err
		}
		return folded, nil, changed, nil
	}
	w := repository.RequestTransitionWrite{
		RequestID:        req.ID,
		OrgID:            req.OrgID,
		TeamID:           req.TeamID,
		From:             plan.From,
		To:               plan.To,
		ExpectRev:        expectRev,
		Set:              transitionColumnNames(plan),
		OutcomeBrief:     plan.OutcomeBrief,
		RejectReason:     plan.RejectReason,
		NoArtifact:       in.NoArtifact,
		AcceptedBy:       plan.AcceptedBy,
		ClearDuplicateOf: plan.ClearDuplicateOf,
		Actor:            actor,
		ActorUserID:      actorID,
	}
	switch {
	case plan.Question != "":
		w.Comment = &repository.RequestTransitionComment{
			Kind: domain.RequestCommentQuestion, Body: plan.Question,
			Author: actor, AuthorUserID: actorID,
		}
	case plan.AnswerTo > 0:
		w.Comment = &repository.RequestTransitionComment{
			Kind: domain.RequestCommentAnswer, Body: trimRequestTransitionText(in.Answer), AnswerTo: plan.AnswerTo,
			Author: actor, AuthorUserID: actorID,
		}
	}
	applied, comment, err := s.Repo.ApplyRequestTransition(ctx, w)
	if err != nil {
		return nil, nil, false, err
	}
	return applied, comment, true, nil
}

// transitionColumnNames converts the plan's columns to the repository's names.
// They are the same words (the schema's column names) on purpose: the policy
// names what the write must set, and a mapping table between them would be a
// second vocabulary for the same thing.
func transitionColumnNames(plan *RequestTransitionPlan) []string {
	out := make([]string, len(plan.Set))
	for i, c := range plan.Set {
		out[i] = string(c)
	}
	return out
}

// recordTransition appends the one audit row the act gets, after it committed:
// `request.transition`, on the team's org, targeting the request, naming both
// states and — for the two acts whose meaning is easy to lose in a state pair —
// whether the move was a reopen or an acceptance.
//
// It runs after the commit and how it fails is audit.go's rule: the change already
// happened, so a failure is logged rather than returned. The row names the
// principal through the audit writer's usual context (actor, channel, address,
// token); the details add ids and state names, never the question, the answer,
// the reason or the brief — a request's content is requester-visible and belongs
// in its own record, not in the operational log.
func (s *Service) recordTransition(ctx context.Context, p authz.Principal, team *domain.Team,
	req *domain.TeamRequest, plan *RequestTransitionPlan, comment *domain.RequestComment) {
	details := map[string]any{
		"team_id":    team.ID,
		"public_ref": req.PublicRef,
		"from_state": string(plan.From),
		"to_state":   string(plan.To),
		"reopened":   plan.Reopened,
		"rev":        req.Rev,
	}
	if plan.AcceptedBy != nil {
		details["accepted_by"] = *plan.AcceptedBy
	}
	if comment != nil {
		details["comment_id"] = comment.ID
		details["comment_kind"] = string(comment.Kind)
	}
	if plan.DuplicateOf != nil {
		details["duplicate_of"] = *plan.DuplicateOf
	}
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.transition",
		OrgID:      intRef(team.OrgID),
		TargetType: "request",
		TargetID:   idStr(req.ID),
		TargetName: req.PublicRef,
		Summary:    "moved request " + req.PublicRef + " from " + string(plan.From) + " to " + string(plan.To),
		Details:    details,
	})
}

// transitionRequestError translates the answers a write can come back with into
// the surface's stable codes, and leaves every other failure as itself so a dead
// connection or a cancelled context still reads as what it is.
//
//   - a lost revision and a state that moved are both request.conflict (409),
//     because both are "the row changed since you read it, reload and decide
//     again" — the caller can resolve either, and neither wrote anything;
//   - a delivery with no evidence and an answer naming no question are
//     request.invalid (400): the act is wrong in a way the caller can fix;
//   - a row this tenant cannot name is the surface's single not-found.
func transitionRequestError(requestID int, err error) error {
	var revConflict *repository.RequestRevConflict
	var stateConflict *repository.RequestStateConflict
	switch {
	case errors.As(err, &revConflict):
		return &codedCause{
			err:   authz.RequestConflict("request %d is at revision %d, not %d: it changed since it was read, so reload it and decide again", requestID, revConflict.Current, revConflict.Expected),
			cause: err,
		}
	case errors.As(err, &stateConflict):
		return &codedCause{
			err:   authz.RequestConflict("request %d is now %s, not %s: it moved since this act was decided, so reload it and decide again", requestID, stateConflict.Current, stateConflict.Expected),
			cause: err,
		}
	case errors.Is(err, repository.ErrRequestDeliveryUnEvidenced):
		return authz.RequestInvalid("request %d is delivered only when the outcome exists: link what delivered it (a task, an artifact or a receipt), or say explicitly that there is nothing to link (I2). %s", requestID, err)
	case errors.Is(err, repository.ErrRequestAnswerTarget):
		return authz.RequestInvalid("an answer names the question it answers, and that question must be one of this request's own: %s", err)
	case errors.Is(err, repository.ErrRequestTransitionShape):
		return authz.RequestInvalid("%s", err)
	case errors.Is(err, repository.ErrNotFound):
		return authz.RequestNotFound()
	}
	return fmt.Errorf("transition request %d: %w", requestID, err)
}

// trimRequestTransitionText is the one normalization this file does on caller
// text before the policy sees it: surrounding whitespace is not part of a
// question, an answer or a reason, and a field that is only whitespace is empty
// (which each of them refuses in its own words).
func trimRequestTransitionText(s string) string { return strings.TrimSpace(s) }
