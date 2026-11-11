package authz

// Team inbox requests: who may see one request, and the stable request.* codes
// every refusal on the request surface carries (epic 202; approved product
// contract artifact 36 v2 §2 and §5; build slice B4 of
// discoveries/team-inbox-approved-build-slices.md, AMP #3771).
//
// Two decisions live here, and both are deliberately pure so the visibility
// matrix can be tested exhaustively without a database:
//
//   - RequestViewFor / CanViewRequest: what a principal sees of one request.
//     There are exactly two views and they are not interchangeable. The team
//     sees the whole record; the requester sees their own request only, and
//     never another requester's — customer-to-customer visibility is out of v1.
//   - the request.* codes: request.not_found, request.conflict,
//     request.invalid and request.rate_limited, so a client branches on a code
//     rather than on a message that may be reworded.
//
// The rule the whole file serves is the contract's: seeing your own request is
// not seeing the project, and not seeing a request is never confirmed. A
// requester holds no org or team role and no project Level, and needs none —
// their own request is readable with zero project membership. Conversely a
// project Level of Owner on the inbox's own project, reached through some other
// team, confers nothing here: the decision is about the request's team, never
// the project ladder.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- Who may see a request ----

// InboxRole is a principal's standing on one team's inbox, as far as *reading
// requests* goes. Checker.InboxRole resolves it from the rows; the decision
// itself (RequestViewFor) is pure, so the matrix can be tested without a
// database.
//
// There are only two values on purpose. "Can see the inbox" is one question
// with one answer, and the finer distinctions the inbox needs later — who may
// claim, assign, deliver — are separate checks on the service's transition
// path, not a ladder to widen here.
type InboxRole int

const (
	// InboxNone: no standing on the team's inbox. That covers a stranger from
	// another org, a plain org member with no team role, a member of a peer
	// team on the same project, an agent account holding no role of its own —
	// and, deliberately, the holder of a submit grant who is in no team and no
	// org. A grant confers submitting to the inbox, never reading it (contract
	// §2: a requester "cannot read other requests").
	InboxNone InboxRole = iota
	// InboxMember: may read the team's whole inbox — a member or lead of the
	// team, an owner or admin of its org, or a global admin.
	InboxMember
)

func (r InboxRole) String() string {
	if r >= InboxMember {
		return "inbox"
	}
	return "none"
}

// RequestView is what a caller may see of one request. The zero value is "no
// view", so anything that forgets to decide fails closed.
type RequestView struct {
	Mode string // "team", "requester", or "none"
}

// TeamRequestView is the team's view: the whole record, its history, its
// internal fields — what triage works from.
func TeamRequestView() RequestView { return RequestView{Mode: "team"} }

// OwnRequestView is the requester's view of their own request: the whitelist
// projection of the contract's §4, never ticket names, project ids, agent
// identities or audit detail. Which fields that is, is the projection's own
// concern (build slice E3); this value only says *that* the caller is the
// requester, so the read path must project rather than return the record.
func OwnRequestView() RequestView { return RequestView{Mode: "requester"} }

// NoRequestView is "this request is not yours to see". Callers must report it
// exactly as they report a request that does not exist: RequestNotFound.
func NoRequestView() RequestView { return RequestView{Mode: "none"} }

// Full reports whether the view is the team's whole record.
func (v RequestView) Full() bool { return v.Mode == "team" }

// Own reports whether the view is the requester's own projection.
func (v RequestView) Own() bool { return v.Mode == "requester" }

// None reports whether the caller may not see the request at all.
func (v RequestView) None() bool { return v.Mode != "team" && v.Mode != "requester" }

func (v RequestView) String() string {
	if v.None() {
		return "none"
	}
	return v.Mode
}

// RequestViewFor decides, purely, what view principal me has of r given their
// standing role on r's team.
//
// Order matters and is the whole rule: the team's full view wins, because a
// triager who is also the requester (a team member submitting to their own
// inbox) must see the record they triage, not a projection of it. Otherwise a
// caller sees their own request and nothing else.
//
// "Their own" is the durable requester column, not an audit field: a request
// submitted by a connector or service client has no requester user and belongs
// to nobody's "own" view.
func RequestViewFor(r *domain.TeamRequest, me int, role InboxRole) RequestView {
	if r == nil {
		return NoRequestView()
	}
	if role >= InboxMember {
		return TeamRequestView()
	}
	if me != 0 && r.RequesterUserID != nil && *r.RequesterUserID == me {
		return OwnRequestView()
	}
	return NoRequestView()
}

// CanViewRequest reports whether me may see r at all.
func CanViewRequest(r *domain.TeamRequest, me int, role InboxRole) bool {
	return !RequestViewFor(r, me, role).None()
}

// FilterRequests returns the requests in view, preserving order — the
// FilterTasks counterpart for the inbox list. The team's view keeps every row
// the caller's inbox check already admitted; a requester's own view keeps
// exactly their own requests and nothing else. The result is always a fresh
// slice, never the caller's own, so a list handler can filter and then annotate
// without writing back through it.
func FilterRequests(reqs []domain.TeamRequest, view RequestView, me int) []domain.TeamRequest {
	if view.None() {
		return nil
	}
	out := make([]domain.TeamRequest, 0, len(reqs))
	for i := range reqs {
		if view.Full() || (me != 0 && reqs[i].RequesterUserID != nil && *reqs[i].RequesterUserID == me) {
			out = append(out, reqs[i])
		}
	}
	return out
}

// InboxRole resolves p's standing on teamID's inbox from the rows, on every
// call. It is the read counterpart of TeamSubmit and deliberately the same
// allow-set minus the grant path: a team member or lead, an owner or admin of
// the team's org, or a global admin. A submit grant is not in it — the contract
// gives a requester their own request, never the inbox (§2: a requester
// "cannot read other requests"). Nothing is cached — no allow-set, no
// request-scoped memo — so losing a team membership or an org role takes effect
// on the very next request (contract §2's per-request re-check, the same rule
// TeamSubmit makes for submitting).
//
// An unknown team is InboxNone, not an error: the caller reports a request it
// cannot see and a request that does not exist identically, so there is nothing
// useful — or safe — to distinguish here.
func (c *Checker) InboxRole(ctx context.Context, p Principal, teamID int) (InboxRole, error) {
	// A submit-scoped credential reads no inbox, its own team's included
	// (AMP #3772): the scope suppresses reading the same way it suppresses
	// every other capability.
	if role, scoped := scopedInboxRole(p); scoped {
		return role, nil
	}
	// The team is loaded before anything else, even for a global admin, for the
	// same reason TeamSubmit does it: asking about a team that does not exist
	// must answer "no standing" rather than "global admin, so yes".
	team, err := c.repo.GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return InboxNone, nil
		}
		return InboxNone, err
	}
	if p.Super {
		return InboxMember, nil
	}
	if !p.HasUser() {
		// Local dev before its user row is provisioned (auth disabled): it has
		// no membership of its own, and being Super is the whole of its
		// authority — handled above.
		return InboxNone, nil
	}
	teamRole, err := c.repo.GetTeamRole(ctx, team.ID, p.UserID)
	if err != nil {
		return InboxNone, err
	}
	if teamRole != "" {
		return InboxMember, nil
	}
	// The org role is read for the team's own org, so an owner/admin of some
	// other org reaches nothing here.
	orgRole, err := c.repo.GetOrgRole(ctx, team.OrgID, p.UserID)
	if err != nil {
		return InboxNone, err
	}
	if orgRole == domain.OrgRoleOwner || orgRole == domain.OrgRoleAdmin {
		return InboxMember, nil
	}
	return InboxNone, nil
}

// RequestAccess decides p's view of r, reading their standing on r's team from
// the database on this call. r must already be loaded by the caller.
func (c *Checker) RequestAccess(ctx context.Context, p Principal, r *domain.TeamRequest) (RequestView, error) {
	if r == nil {
		return NoRequestView(), nil
	}
	role, err := c.InboxRole(ctx, p, r.TeamID)
	if err != nil {
		return NoRequestView(), err
	}
	return RequestViewFor(r, p.UserID, role), nil
}

// ViewForLoad is the read path's one decision, and the reason the 404s are
// indistinguishable is structural rather than a convention each handler has to
// remember: a request that could not be loaded and a request that is not the
// caller's to see both leave through the same RequestNotFound.
//
// Callers pass whatever the repository returned — a request, repository.ErrNotFound,
// or another error — and get (view, nil) when the caller may see it, the
// surface's single 404 when the answer is "not for you", and the underlying
// error only when something actually went wrong.
func (c *Checker) ViewForLoad(ctx context.Context, p Principal, r *domain.TeamRequest, loadErr error) (RequestView, error) {
	if loadErr != nil {
		if errors.Is(loadErr, repository.ErrNotFound) {
			return NoRequestView(), RequestNotFound()
		}
		return NoRequestView(), loadErr
	}
	view, err := c.RequestAccess(ctx, p, r)
	if err != nil {
		return NoRequestView(), err
	}
	if view.None() {
		return NoRequestView(), RequestNotFound()
	}
	return view, nil
}

// ---- Stable codes on the request surface ----

// Every refusal on the request surface carries one of these codes next to its
// HTTP status, so a client branches on the code instead of parsing a message
// that may be reworded (contract §5, "stable request.* codes"). They are
// resource-prefixed like the remote-control surface's codes, and distinct from
// the ticket surface's, so a client can tell "that request is not there for
// you" from "that ticket is not there for you".
const (
	// CodeRequestNotFound: 404 — the request is not there, or is not the
	// caller's to see. The two are the same answer (see RequestNotFound).
	CodeRequestNotFound = "request.not_found"
	// CodeRequestConflict: 409 — a write whose rev is stale, or a claim
	// someone else won first.
	CodeRequestConflict = "request.conflict"
	// CodeRequestInvalid: 400 — the submission itself is wrong in a way the
	// caller can fix: a missing field, a bound exceeded, a state move that is
	// not legal.
	CodeRequestInvalid = "request.invalid"
	// CodeRequestRateLimited: 429 — too many submissions; wait, then retry.
	CodeRequestRateLimited = "request.rate_limited"
)

// RequestNotFound is the one refusal for a request the caller may not see.
//
// It takes no arguments on purpose. A request that does not exist and another
// team's request must be indistinguishable, or a requester could walk ids and
// learn which ones are taken, in which team, in which state, for whom. So the
// status, the code and the message are the same bytes for every not-found on
// this surface — whoever asked, and whatever they asked for. Callers report a
// nil load, a foreign request and a request outside the caller's view with this
// same error; nothing here may name an id, a ref, a team or a state.
func RequestNotFound() error {
	return Coded(404, CodeRequestNotFound, "request not found")
}

// RequestConflict is a 409: a write whose rev is stale, or a claim that lost
// its race. format says what to do next and may name the current rev or the
// claimant — but only where the caller is already entitled to see them. A
// requester's view never names a triager, so the read path passes a message
// suited to the view it is answering.
func RequestConflict(format string, a ...any) error {
	return Coded(409, CodeRequestConflict, format, a...)
}

// RequestInvalid is a 400: the submission is wrong in a way the caller can fix.
// format names the field and the rule; it should not echo the rejected value
// back, since on a shared inbox that value may be another party's content.
func RequestInvalid(format string, a ...any) error {
	return Coded(400, CodeRequestInvalid, format, a...)
}

// RequestRateLimited is a 429 carrying the wait before the caller may retry.
// format says what was limited ("too many submissions from this address"). The
// wait is in the message; the surface's writer adds the Retry-After header and
// retry_after_s field where it has them (the relay envelope), which is that
// layer's concern, not this one's.
func RequestRateLimited(retryAfterS int, format string, a ...any) error {
	if retryAfterS < 1 {
		retryAfterS = 1
	}
	wait := "try again in " + strconv.Itoa(retryAfterS) + "s"
	if format == "" {
		return Coded(429, CodeRequestRateLimited, "too many requests; %s", wait)
	}
	return Coded(429, CodeRequestRateLimited, "%s; %s", fmt.Sprintf(format, a...), wait)
}
