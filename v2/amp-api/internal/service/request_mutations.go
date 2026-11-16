package service

// Team request mutations: the authorized content edit, the triage claim and
// assignee, and one row of discussion (epic 202, story 549, AMP #3959; approved
// product contract artifact 36 v2 §1, §2 and §5; build slices S3 and S4 of
// discoveries/team-inbox-approved-build-slices.md).
//
// Four acts live here, and each answers the same three questions the same way:
// may this caller do this to this request, who is it attributed to, and was
// anything actually written (because a write is audited exactly once and a
// replay is not).
//
//	act                     who                    when
//	----------------------  ---------------------  --------------------------------------
//	EditRequest             the request's own       only while the request is untriaged
//	                        submitter               (state new, no claim, no assignee)
//	ClaimRequest            a team member           an unclaimed request, for themselves
//	ReleaseRequestClaim     the claim's holder      whenever they hold it
//	AssignRequest           a team manager          any request; a transfer clears the claim
//	CommentOnRequest        team or requester       their own voice, in the kinds that voice owns
//
// What is deliberately NOT here, because each is another slice's concern and
// none of it is reachable from these functions:
//
//   - no lifecycle. Nothing in this file writes `state`, `duplicate_of`,
//     `reject_reason`, a state-entry timestamp, `accepted_by` or any other
//     lifecycle column; the transitions belong to the one transition function
//     (service/request_lifecycle.go, #3960). A claim is not a transition: it
//     records who is looking, not where the request is.
//   - no endpoints, no MCP tools, no UI. Those are thin adapters over these
//     methods (slices F2 and I2).
//   - no routing or lineage. `routes_to`/`planned_as`/`delivered_by` are link
//     rows (slice E1) and never metadata.
//   - no read path. Loading a request for a caller is #3958's
//     service/request_views.go; the loader here exists only to authorize a
//     write that is already being attempted.
//
// # Actor and state cannot be spoofed
//
// Every input type in this file is a payload and nothing else. None of them has
// a field for the actor, the request's tenant, its state, its claim, its
// assignee, its revision to be written or its requester: the acting identity is
// resolved from the authenticated principal, the tenant from the team row, the
// current record from the row under a lock, and the revision from the
// repository's compare-and-set. What a caller *can* name is refused rather than
// dropped:
//
//   - a submitter may not send `outcome_brief` or `metadata` at all (see
//     RequestEditInput) — silently ignoring them would read as though they had
//     been written;
//   - the comment kind names a VOICE, and a caller may only speak in its own
//     (see requestCommentVoice); nobody but the system may post a `system` row;
//   - a claim is only ever for the caller, never for a user they name — taking
//     work for someone else is an assignee transfer, which is a manager's act.
//
// # One audit row per write, none for a replay
//
// Every repository write in this file reports whether it changed anything
// (`changed`), and each method audits exactly when it did. A replay (the same
// claim twice, the assignee already set, an empty edit, a comment retried by its
// id) writes nothing, bumps nothing and audits nothing — which is what keeps the
// audit log a count of acts instead of a count of attempts, and why a retried
// request does not look like two people did the same thing.
//
// # Visibility is decided first, and never confirmed
//
// Everything a caller may not see is authz.RequestNotFound: an unknown team, an
// unknown request, another tenant's request and another person's request are one
// answer with one set of bytes (contract §2's 404-never-403). Only a caller who
// can already see the request is told why a particular act is refused.

import (
	"context"
	"errors"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// RequestEditInput is one versioned content edit, exactly as a surface hands it
// over (slice S3; contract §1's "versioned edits (stale write → 409)").
//
// Subject and InputBrief are what a submitter wrote, and the only two fields
// this path can move. OutcomeBrief and Metadata are present so that a payload
// naming them is ANSWERED instead of silently dropped — the adapter decodes one
// JSON body for the request surface, and a field that vanished between the wire
// and the write would tell its author something false. Neither can be written
// here: the outcome brief is the team's statement of what it delivered (set by
// the delivery transition, I2), and metadata is connector labels and routing
// hints, which the team and its integrations own, never the requester.
//
// A nil pointer leaves the field as stored and a pointer to "" clears it (the
// repository's RequestEdit contract); a blank subject is refused by the domain
// rather than stored.
type RequestEditInput struct {
	Subject      *string
	InputBrief   *string
	OutcomeBrief *string
	Metadata     domain.MetadataPatch
}

// RequestDiscussionInput is one row said on a request's thread (slice S3).
//
// Kind names whose voice the row speaks in, and the caller may only use the
// kinds its own voice owns (CommentOnRequest): empty means a plain `comment`.
// Body is the text, bounded like a brief. RetryOfCommentID is the id a previous
// attempt returned, so a retried post answers with the row it already wrote —
// with its original attribution — instead of a second row.
type RequestDiscussionInput struct {
	Kind             domain.RequestCommentKind
	Body             string
	RetryOfCommentID int64
}

// EditRequest applies a submitter's versioned edit to their own request, while
// it is still untriaged, compare-and-set against expectedRev.
//
// "Untriaged" is read off the row, structurally: state `new` with no triage
// claim and no assignee. That is the whole window in which the request is still
// the submitter's alone — the first claim or assignment is a person having
// picked it up, and the first move into triaging is the desk having started. From
// then on the content is fixed (contract §1: "the request itself is immutable"),
// and what a requester wants changed they say in the thread; a team that wants
// the record to say something else has the delivery transition's outcome brief,
// not a rewrite of what was asked.
//
// expectedRev is required, and the requirement is what closes the window between
// the authorization read and the write: this method decides "untriaged" from a
// row it read, and every other write on the request (a claim, a transition, a
// second edit) bumps rev, so a triage that starts in between is a 409 here rather
// than an edit that lands after triage began. A missing or non-positive
// expectedRev is request.invalid naming the field, not a silent "whatever the row
// is now".
//
// changed is false when the edit changed nothing (every named field already had
// that value): the stored row comes back unchanged, rev does not move and nothing
// is audited, so a form saved twice does not make every other holder of the
// revision stale.
func (s *Service) EditRequest(ctx context.Context, p authz.Principal, teamID, requestID, expectedRev int, in RequestEditInput) (*domain.TeamRequest, bool, error) {
	team, req, _, err := s.loadRequestForMutation(ctx, p, teamID, requestID)
	if err != nil {
		return nil, false, err
	}
	if !requestSubmitterIs(p, req) {
		// The caller can see this request (loadRequestForMutation answered 404
		// otherwise), so the honest answer is why not: content edits are the
		// submitter's, even for the team that triages it. Note that this asks the
		// durable requester column and not the read view: a team member who
		// submitted to their own inbox reads the team's view (authz.RequestViewFor's
		// order) but is still the person whose words these are, so they may fix
		// them before anyone picks the request up — while a member who did not
		// submit it may not touch someone else's text either way.
		return nil, false, authz.Forbidden("request %d's content is edited by the person who submitted it: a team routes it (a link), annotates it (a comment) and states the outcome (the delivery), but never rewrites what the requester asked", req.ID)
	}
	if !requestUntriaged(req) {
		return nil, false, authz.Forbidden("request %d is %s and has been picked up for triage, so its content is fixed: say what should change in the thread (a comment), and the team answers there", req.ID, requestPickupWord(req))
	}
	if in.OutcomeBrief != nil {
		return nil, false, authz.RequestInvalid("outcome_brief is the team's statement of what it delivered, not a submitter's field: the team writes it when it delivers the request (I2), and an edit cannot")
	}
	if len(in.Metadata) > 0 {
		return nil, false, authz.RequestInvalid("metadata is connector labels and routing hints the team and its integrations own, not a submitter's field: an edit writes subject and input_brief only")
	}
	if expectedRev < 1 {
		return nil, false, authz.RequestInvalid("expected_rev is required on an edit: send the rev you read (If-Match / expected_rev), so an edit that raced a triage decision is a conflict instead of a stale write")
	}

	// Shaped before the write, so a field the domain refuses is request.invalid
	// naming it rather than a constraint failure reported as a server fault. The
	// repository validates the whole result again (it can see rows this method
	// cannot), and normalizes identically.
	edit := repository.RequestEdit{Subject: in.Subject, InputBrief: in.InputBrief}
	if in.Subject != nil {
		subject, err := domain.NormalizeRequestSubject(*in.Subject)
		if err != nil {
			return nil, false, authz.RequestInvalid("%s", err)
		}
		edit.Subject = &subject
	}
	if in.InputBrief != nil {
		brief, err := domain.NormalizeRequestBrief("input_brief", *in.InputBrief)
		if err != nil {
			return nil, false, authz.RequestInvalid("%s", err)
		}
		edit.InputBrief = &brief
	}

	updated, changed, err := s.Repo.UpdateRequest(ctx, req.ID, req.OrgID, req.TeamID, expectedRev, edit,
		repository.RequestEditActor{Name: p.Label(), UserID: p.UserRef()})
	if err != nil {
		return nil, false, requestMutationRefusal(err)
	}
	if !changed {
		return updated, false, nil
	}
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.update",
		OrgID:      intRef(team.OrgID),
		TargetType: "request",
		TargetID:   idStr(updated.ID),
		TargetName: updated.PublicRef,
		Summary:    "edited request " + updated.PublicRef,
		// Field NAMES and revisions only: a request's audit row is org-visible, and
		// the requester's words belong in the record and the thread, not copied
		// into the log.
		Details: map[string]any{
			"team_id":  team.ID,
			"from_rev": expectedRev,
			"to_rev":   updated.Rev,
			"fields":   requestEditFields(req, updated),
		},
	})
	return updated, true, nil
}

// ClaimRequest takes request requestID for p — the caller, always: a claim says
// "I am looking at it right now", so there is no way to claim on someone else's
// behalf (that is AssignRequest, a manager's explicit transfer).
//
// Only a triager may claim (a team member, a lead, an org owner/admin or a
// global admin — authz.InboxRole's InboxMember, re-read from the rows on this
// call). A claim held by someone else is the surface's 409 request.conflict
// naming the holder, and it writes nothing: there is no expiry and no take-over
// (D4). Taking work from its holder is an assignee transfer, which clears the
// claim in the same write and leaves a history row; a claim steal would leave
// neither.
//
// changed is false when the caller already holds the claim: a retry after a lost
// response answers with the row, keeps the claim's age, bumps no revision and
// audits nothing.
//
// No lifecycle state is consulted or changed: a claim records who is looking, not
// where the request is, so claiming a request that has moved on is bookkeeping,
// not a transition (and the transition policy stays the only thing that moves a
// request). expectRev, when greater than zero, is the caller's compare-and-set and
// is applied only to a call that would change something, exactly as the
// repository documents.
func (s *Service) ClaimRequest(ctx context.Context, p authz.Principal, teamID, requestID int, expectRev int) (*domain.TeamRequest, bool, error) {
	team, req, view, err := s.loadRequestForMutation(ctx, p, teamID, requestID)
	if err != nil {
		return nil, false, err
	}
	if !view.Full() {
		return nil, false, authz.Forbidden("claiming a request is a triager's act: it is a member of the inbox's team saying they are looking at it now, and a requester has no claim to take on their own request")
	}
	if !p.HasUser() {
		return nil, false, authz.RequestInvalid("a claim names the person holding it, and this credential has no AMP user to name")
	}
	updated, changed, err := s.Repo.ClaimRequest(ctx, repository.RequestClaimWrite{
		RequestID:   req.ID,
		OrgID:       req.OrgID,
		TeamID:      req.TeamID,
		ClaimUserID: p.UserID,
		ExpectRev:   expectRev,
		Actor:       p.Label(),
		ActorUserID: p.UserRef(),
	})
	if err != nil {
		return nil, false, requestMutationRefusal(err)
	}
	if !changed {
		return updated, false, nil
	}
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.claim",
		OrgID:      intRef(team.OrgID),
		TargetType: "request",
		TargetID:   idStr(updated.ID),
		TargetName: updated.PublicRef,
		Summary:    "claimed request " + updated.PublicRef,
		Details: map[string]any{
			"team_id":       team.ID,
			"claim_user_id": p.UserID,
			"rev":           updated.Rev,
		},
	})
	return updated, true, nil
}

// ReleaseRequestClaim ends the claim p holds on request requestID.
//
// It is the holder's act and only the holder's: releasing someone else's claim
// would be the mirror of a steal, and a manager who needs a claim to end clears
// it with an assignee transfer (AssignRequest), which is explicit and leaves a
// history row naming the claim it cleared.
//
// changed is false when the caller holds no claim — the request is already
// released, so the call succeeds and writes nothing (no revision bump, no history
// row, no audit), which is what makes a retry after a lost response safe. A claim
// someone else holds is the 409 request.conflict naming them.
func (s *Service) ReleaseRequestClaim(ctx context.Context, p authz.Principal, teamID, requestID int, expectRev int) (*domain.TeamRequest, bool, error) {
	team, req, view, err := s.loadRequestForMutation(ctx, p, teamID, requestID)
	if err != nil {
		return nil, false, err
	}
	if !view.Full() {
		return nil, false, authz.Forbidden("releasing a claim is the claim holder's act, and a requester holds no claim on the inbox's request")
	}
	if !p.HasUser() {
		return nil, false, authz.RequestInvalid("a claim is released by the user holding it, and this credential has no AMP user to name")
	}
	updated, changed, err := s.Repo.ReleaseClaim(ctx, repository.RequestClaimWrite{
		RequestID:   req.ID,
		OrgID:       req.OrgID,
		TeamID:      req.TeamID,
		ClaimUserID: p.UserID,
		ExpectRev:   expectRev,
		Actor:       p.Label(),
		ActorUserID: p.UserRef(),
	})
	if err != nil {
		return nil, false, requestMutationRefusal(err)
	}
	if !changed {
		return updated, false, nil
	}
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.release",
		OrgID:      intRef(team.OrgID),
		TargetType: "request",
		TargetID:   idStr(updated.ID),
		TargetName: updated.PublicRef,
		Summary:    "released their claim on request " + updated.PublicRef,
		Details: map[string]any{
			"team_id":       team.ID,
			"claim_user_id": p.UserID,
			"rev":           updated.Rev,
		},
	})
	return updated, true, nil
}

// AssignRequest makes assignee the request's owner, or leaves it unowned when
// assignee is "none"/"unassigned". Only a team manager may call it — a lead of
// the inbox's team, an owner or admin of its org, or a global admin
// (authz.RequireTeamManager, the same gate team membership uses; contract §2
// gives "assign" to that row and not to a plain member).
//
// A transfer clears any active claim in the same write (the repository's one
// UPDATE), so there is no instant in which a request has a new owner and a stale
// claim, and the history row names both the previous owner and whose claim was
// cleared. That is the approved way work moves between people: explicit,
// attributed, and never a silent claim steal.
//
// The assignee must be able to see the inbox. Handing a request to a user with no
// standing on the team would give them an owner they cannot open, so a
// customer with only a submit grant, or a stranger to the org, is refused with
// request.invalid naming the user — the same rule requireMember applies before a
// ticket changes hands.
//
// expectRev, when greater than zero, is the caller's compare-and-set; changed is
// false when the request already is (assignee, unclaimed), so a retried transfer
// writes and audits nothing.
func (s *Service) AssignRequest(ctx context.Context, p authz.Principal, teamID, requestID int, assignee string, expectRev int) (*domain.TeamRequest, bool, error) {
	team, req, view, err := s.loadRequestForMutation(ctx, p, teamID, requestID)
	if err != nil {
		return nil, false, err
	}
	if !view.Full() {
		return nil, false, authz.Forbidden("assigning a request is the inbox team's act: a requester cannot hand their own request to someone")
	}
	if err := s.Authz.RequireTeamManager(ctx, p, team); err != nil {
		return nil, false, err
	}

	var newOwner *int
	switch ref := strings.ToLower(strings.TrimSpace(assignee)); ref {
	case "":
		return nil, false, authz.RequestInvalid(`assignee is required: a user id or email, "me" to take the request, or "none" to leave it unowned`)
	case "none", "unassigned":
	default:
		id, err := s.Authz.ResolveUser(ctx, p, assignee)
		if err != nil {
			return nil, false, err
		}
		if err := s.requireRequestAssignee(ctx, team, id); err != nil {
			return nil, false, err
		}
		newOwner = &id
	}

	updated, changed, err := s.Repo.SetAssignee(ctx, repository.RequestClaimWrite{
		RequestID:   req.ID,
		OrgID:       req.OrgID,
		TeamID:      req.TeamID,
		Assignee:    newOwner,
		ExpectRev:   expectRev,
		Actor:       p.Label(),
		ActorUserID: p.UserRef(),
	})
	if err != nil {
		return nil, false, requestMutationRefusal(err)
	}
	if !changed {
		return updated, false, nil
	}
	summary := "unassigned request " + updated.PublicRef
	if newOwner != nil {
		summary = "assigned request " + updated.PublicRef + " to " + s.userLabel(ctx, *newOwner)
	}
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.assign",
		OrgID:      intRef(team.OrgID),
		TargetType: "request",
		TargetID:   idStr(updated.ID),
		TargetName: updated.PublicRef,
		Summary:    summary,
		// The change, not the request: where the ownership came from and whose
		// claim the transfer cleared. Ids only — the log names nobody's words.
		Details: map[string]any{
			"team_id":                team.ID,
			"owner_user_id":          newOwner,
			"previous_owner_user_id": req.OwnerUserID,
			"cleared_claim_user_id":  req.ClaimUserID,
			"rev":                    updated.Rev,
		},
	})
	return updated, true, nil
}

// CommentOnRequest appends one row to a request's discussion, in the caller's
// own voice, and returns the stored row plus whether this call wrote it.
//
// Who may say what is a VOICE, not a permission ladder, because a discussion row
// is read as "the desk said" or "the requester said" (the requester's view
// attributes every row to "you" or "the desk", E3):
//
//	kind        voice        who can post it
//	----------  -----------  ----------------------------------------------
//	comment     either       a triager, or the request's own requester
//	question    team         a triager only — asking the requester something
//	answer      requester    the request's own requester only
//	system      nobody       written by the system's own paths, never by a caller
//
// A team member could technically post an `answer` and a requester a `question`;
// both are refused, because each would put words in the other party's mouth in a
// thread whose attribution is the record. The team's voice wins for a member who
// also submitted the request, the same order authz.RequestViewFor uses.
//
// A comment changes nothing but itself: no state, no revision, no event (the
// repository guarantees it, and the transition that enters needs_information —
// with its question — is #3960's). So this path can never advance a request,
// whatever it says; a requester's answer that returns a request to the queue is a
// transition, and this row alone is only what was said.
//
// Repeats: RetryOfCommentID makes a retried post answer with the row the earlier
// attempt wrote, unchanged and with its original attribution, and changed is false
// so nothing is audited a second time.
func (s *Service) CommentOnRequest(ctx context.Context, p authz.Principal, teamID, requestID int, in RequestDiscussionInput) (*domain.RequestComment, bool, error) {
	team, req, view, err := s.loadRequestForMutation(ctx, p, teamID, requestID)
	if err != nil {
		return nil, false, err
	}
	kind, err := domain.NormalizeRequestCommentKind(string(in.Kind))
	if err != nil {
		return nil, false, authz.RequestInvalid("%s", err)
	}
	voice := requestVoiceTeam
	if !view.Full() {
		// Own view: the loader already answered 404 for anything else, and Own
		// means the durable requester column matches this principal. Re-checked
		// against the row (not the view) so the property is local to the act.
		if !requestSubmitterIs(p, req) {
			return nil, false, authz.RequestNotFound()
		}
		voice = requestVoiceRequester
	}
	if _, ok := requestCommentVoice(kind); !ok {
		return nil, false, authz.Forbidden("a %s comment is the system's own record of what it did (a submission, an edit note), never something a caller says: post a comment, a question or an answer", kind)
	}
	if !requestCommentVoiceAllowed(kind, voice) {
		return nil, false, authz.Forbidden("%s", requestVoiceRefusal(kind, voice))
	}
	// Validated here so a body the domain refuses is request.invalid naming the
	// field, and so the normalized kind and body are what the repository stores.
	c := &domain.RequestComment{Kind: kind, Body: in.Body}
	if err := c.Validate(); err != nil {
		return nil, false, authz.RequestInvalid("%s", err)
	}

	stored, err := s.Repo.AppendRequestComment(ctx, repository.AppendRequestCommentRequest{
		RequestID: req.ID,
		OrgID:     req.OrgID,
		TeamID:    req.TeamID,
		Kind:      c.Kind,
		Body:      c.Body,
		// The author snapshot is the authenticated principal's, captured now and
		// kept after the account is deleted; nothing in the payload can name a
		// different author (RequestDiscussionInput has no field for one).
		Author:           p.Label(),
		AuthorUserID:     p.UserRef(),
		RetryOfCommentID: in.RetryOfCommentID,
	})
	if err != nil {
		return nil, false, requestMutationRefusal(err)
	}
	// The repository answers a retry that names this request's own comment with
	// that row and writes nothing; any other call inserts, and an inserted row's
	// serial is never the id it was handed (ids are never reused, and a comment
	// cannot be deleted except with its request). So an answer whose id is the
	// retry id is the replay, and it is not audited again.
	if in.RetryOfCommentID > 0 && stored.ID == in.RetryOfCommentID {
		return stored, false, nil
	}
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.comment",
		OrgID:      intRef(team.OrgID),
		TargetType: "request",
		TargetID:   idStr(req.ID),
		TargetName: req.PublicRef,
		Summary:    "commented on request " + req.PublicRef,
		Details: map[string]any{
			"team_id":    team.ID,
			"comment_id": stored.ID,
			"kind":       string(stored.Kind),
		},
	})
	return stored, true, nil
}

// ---- Whose voice a discussion row speaks in ----

// requestVoice is whose words a discussion row is: the desk's or the requester's.
// It is not a permission level — the two are peers, and each owns different kinds
// (see CommentOnRequest's table).
type requestVoice int

const (
	requestVoiceTeam requestVoice = iota
	requestVoiceRequester
)

func (v requestVoice) String() string {
	if v == requestVoiceRequester {
		return "the requester"
	}
	return "a triager"
}

// requestCommentVoice answers which voice each discussion kind belongs to, and ok
// is false for a kind no caller may post at all. It is a closed table read by
// CommentOnRequest, so a kind added to the domain without a decision here is
// refused rather than silently attributed to whoever called.
func requestCommentVoice(kind domain.RequestCommentKind) (voice requestVoice, ok bool) {
	switch kind {
	case domain.RequestCommentDiscussion:
		// A plain comment belongs to both voices; the caller's own voice decides
		// which attribution the row carries, and CommentOnRequest checks the
		// member/requester split before it gets here.
		return requestVoiceTeam, true
	case domain.RequestCommentQuestion:
		return requestVoiceTeam, true
	case domain.RequestCommentAnswer:
		return requestVoiceRequester, true
	default:
		// `system` and anything unknown: the system's own narrative rows are
		// written by the paths that did the thing, never by an API caller.
		return 0, false
	}
}

// requestCommentVoiceAllowed is the whole rule, stated once for CommentOnRequest
// and for the exhaustive table test: a plain comment may be said by either voice,
// and the two directed kinds may only be said by the voice they belong to.
func requestCommentVoiceAllowed(kind domain.RequestCommentKind, voice requestVoice) bool {
	if kind == domain.RequestCommentDiscussion {
		return true
	}
	want, ok := requestCommentVoice(kind)
	return ok && want == voice
}

// requestVoiceRefusal explains a kind posted out of the caller's voice, naming the
// party whose words it would have been and how to say the same thing legitimately.
func requestVoiceRefusal(kind domain.RequestCommentKind, got requestVoice) string {
	switch kind {
	case domain.RequestCommentQuestion:
		return "a question is the desk asking the requester something, so only " + requestVoiceTeam.String() + " can post one: " +
			"a requester writes a comment, or answers the question the desk asked"
	case domain.RequestCommentAnswer:
		return "an answer is the requester's reply, so only " + requestVoiceRequester.String() + " can post one: " +
			"the team writes a comment, and answering on the requester's behalf is nobody's act (I1)"
	default:
		return "a " + string(kind) + " comment is not yours to post as " + got.String() + ": post a comment instead"
	}
}

// ---- Authorization and loading ----

// loadRequestForMutation loads the request a mutation names and decides whether p
// may see it at all, from the rows on this call.
//
// The order is deliberate: the team first (its org is the other tenant half the
// repository demands, read from the row rather than from the caller), then the
// request scoped by both halves, then the caller's standing. Everything invisible
// leaves as authz.RequestNotFound — an unknown team, an unknown request, another
// team's request and another person's request are one answer, so an id walker
// learns nothing (contract §2). A database failure is never one of those: it is
// returned as itself.
//
// The view it returns is the caller's read view (team, requester's own, or none),
// which is also the capability each act needs: view.Full() is "a triager here",
// and the own view is the submitter's own request. Acts that need more (assign
// needs the manager's gate) ask for it themselves.
func (s *Service) loadRequestForMutation(ctx context.Context, p authz.Principal, teamID, requestID int) (*domain.Team, *domain.TeamRequest, authz.RequestView, error) {
	team, err := s.Repo.GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, authz.NoRequestView(), authz.RequestNotFound()
		}
		return nil, nil, authz.NoRequestView(), err
	}
	req, err := s.Repo.GetRequest(ctx, requestID, team.OrgID, team.ID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, authz.NoRequestView(), authz.RequestNotFound()
		}
		return nil, nil, authz.NoRequestView(), err
	}
	view, err := s.Authz.RequestAccess(ctx, p, req)
	if err != nil {
		return nil, nil, authz.NoRequestView(), err
	}
	if view.None() {
		return nil, nil, authz.NoRequestView(), authz.RequestNotFound()
	}
	return team, req, view, nil
}

// requestSubmitterIs reports whether p is the person the request was submitted by:
// the durable requester column, never a comment author, an audit field or a
// display name.
func requestSubmitterIs(p authz.Principal, req *domain.TeamRequest) bool {
	return p.HasUser() && req.RequesterUserID != nil && *req.RequesterUserID == p.UserID
}

// requestUntriaged reports whether the request is still the submitter's alone:
// intake's state, nobody looking at it, nobody owning it. Each half matters —
// a claim is someone triaging it now, and an assignee is someone owning the
// outcome — and all three are read off the row, so the answer cannot go stale
// within the decision (the edit's compare-and-set covers the gap to the write).
func requestUntriaged(req *domain.TeamRequest) bool {
	return req.State == domain.RequestStatusNew && req.ClaimUserID == nil && req.OwnerUserID == nil
}

// requestPickupWord says how a request has left the submitter's hands, for the
// refusal an edit after triage gets. It names only what the submitter can already
// see on their own request.
func requestPickupWord(req *domain.TeamRequest) string {
	switch {
	case req.State != domain.RequestStatusNew:
		return "in " + string(req.State)
	case req.OwnerUserID != nil:
		return "assigned to someone"
	default:
		return "claimed for triage"
	}
}

// requireRequestAssignee refuses an assignee who could not open the request they
// are being given work on: a user with no standing on the inbox (a customer with
// only a submit grant, a stranger to the org), or a deactivated account. The
// question is asked as that user — authz.InboxRole reads their rows — so the
// answer is the same one their own next read would get, and no project access is
// consulted: a request is not a ticket.
func (s *Service) requireRequestAssignee(ctx context.Context, team *domain.Team, userID int) error {
	u, err := s.Repo.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return authz.NotFound("user %d not found", userID)
		}
		return err
	}
	if u.DisabledAt != nil {
		return authz.RequestInvalid("user %d (%s) is deactivated and can't be given a request", userID, u.Email)
	}
	role, err := s.Authz.InboxRole(ctx, authz.Principal{UserID: userID}, team.ID)
	if err != nil {
		return err
	}
	if role < authz.InboxMember {
		return authz.RequestInvalid("user %d (%s) has no access to team %d's inbox, so they could not open the request they were given; add them to the team first", userID, u.Email, team.ID)
	}
	return nil
}

// requestEditFields names the content fields an edit moved, from the row as it was
// read and as it was written. Only the two fields a submitter can write are
// compared: outcome_brief and metadata are refused before the write, so they
// cannot move here (and the audit says what the act changed, not what the row
// contains).
func requestEditFields(before, after *domain.TeamRequest) []string {
	var fields []string
	if before.Subject != after.Subject {
		fields = append(fields, "subject")
	}
	if before.InputBrief != after.InputBrief {
		fields = append(fields, "input_brief")
	}
	return fields
}

// requestMutationRefusal translates the answers a repository write in this file can
// come back with into the request surface's stable codes, and leaves every other
// failure as itself so a dead connection or a cancelled context still reads as
// what it is.
//
// A conflict keeps the repository's ErrConflict identity alongside its coded
// shape (codedCause, request_submit.go), so a caller that only branches on
// "conflict" and one that branches on request.conflict both work — the same
// choice the submission path made, so the request surface has one conflict shape.
func requestMutationRefusal(err error) error {
	var rev *repository.RequestRevConflict
	if errors.As(err, &rev) {
		return &codedCause{
			err: authz.RequestConflict("request %d is at rev %d, not rev %d: it changed since it was read, so reload it and reapply",
				rev.RequestID, rev.Current, rev.Expected),
			// The repository's own conflict rides along, not a copy of its sentinel:
			// errors.As still finds the *RequestRevConflict (so a surface can report
			// the revision it must retry with) and errors.Is still matches
			// ErrConflict through its Is method.
			cause: err,
		}
	}
	var held *repository.RequestClaimHeld
	if errors.As(err, &held) {
		return &codedCause{
			err: authz.RequestConflict("request %d is claimed by user %d: a claim is released by its holder or cleared by an assignee transfer, never taken",
				held.RequestID, held.Holder),
			// Same reason: the holder stays readable from the error.
			cause: err,
		}
	}
	if errors.Is(err, repository.ErrRequestClaimant) {
		return authz.RequestInvalid("a claim write names the person the claim belongs to: %s", err)
	}
	if errors.Is(err, repository.ErrNotFound) {
		// A row that vanished between the read and the write, or another tenant's
		// id: the request surface's one 404, never the repository's id-naming
		// message.
		return authz.RequestNotFound()
	}
	if errors.Is(err, repository.ErrRequestTenantScope) {
		return authz.RequestInvalid("a request write needs its org and team: %s", err)
	}
	return err
}
