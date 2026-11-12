package service

// Team requests: the one authorized read path into a team's inbox (epic 202,
// story 549, AMP #3958; approved product contract artifact 36 v2 §2/§4/§5;
// approved mockups artifact 37 v1 (team side) and artifact 38 v1 (requester
// side); build slice S2 of discoveries/team-inbox-approved-build-slices.md).
//
// Three reads, one decision:
//
//	ListRequests      the team's queue: one keyset page, the per-state counts
//	                  beside it, and (when asked) the caller's own claimed and
//	                  assigned slice of the same queue.
//	GetRequest        one request: the team's whole record, or the requester's
//	                  whitelist projection of the one request that is theirs.
//	ListOwnRequests   a requester's own requests, projected, never the record.
//
// They are one file because they share one question — *what may this caller
// see of this request?* — and that question is answered in one place:
// authz.RequestViewFor (through Checker.InboxRole / Checker.RequestAccess /
// Checker.ViewForLoad), re-read from the rows on every call. Nothing here
// caches a membership, a grant, a role or a view. Losing a team membership or an
// org role, or having a submit grant revoked, takes effect on the very next
// call, which is contract §2's per-request re-check and the same rule
// TeamSubmit makes for submitting.
//
// # Two views, and the record only ever reaches one of them
//
// The team sees `domain.TeamRequest`: every column, as stored. The requester
// sees `domain.RequesterRequestView`, built by ProjectRequesterRequest from
// named inputs — and this file is the read path that decides which of the
// request's comments and files are the requester's to see (the projection's own
// input contract says that decision is ours, not the projection's). A requester
// never receives the record: RequestRead.Projection is the only field this file
// fills for them, and RequestRead.Record is left nil, so a surface cannot
// accidentally serialize a claim, an assignee, a contact snapshot, a duplicate
// pointer or a metadata key to a customer.
//
// # Indistinguishable refusals
//
// Every "you cannot see this" leaves through authz.RequestNotFound: one status,
// one code, one message, whoever asked and whatever they asked for. A request
// in another team, a request that belongs to another requester, a service- or
// connector-submitted request with no requester of yours, and an id or
// reference that never existed are the same bytes. An id walker therefore
// learns nothing, and a requester cannot confirm that a guessed reference is
// taken. The 404 is structural rather than a convention: Checker.ViewForLoad
// returns it for both a failed load and an unauthorized view, so a handler does
// not have to remember to make them match.
//
// # A database failure is not a missing row
//
// A real load failure — the pool closing, a cancelled statement, a dead
// connection — is returned as itself. Only repository.ErrNotFound becomes the
// surface's 404. Reporting a database problem as "no such request" would hide
// an outage behind a legitimate answer, and the read path is exactly where that
// would be least visible.
//
// # Keyset paging, and counts that agree with it
//
// Both lists page by `id DESC` with the last row's id as the cursor, so a page
// stays stable while rows land above it (the ListRequests contract). Every
// total is computed cursor-free, by the same predicate as its page: a queue pill
// counts the queue, and a "Claimed by me" header counts the person's whole
// slice, never the remainder of somebody's scroll. That property is the
// repository's (request_reads.go, request_summaries.go) and this file only has
// to pass one filter to both calls — which is why it builds that filter once.
//
// What this file deliberately does not do — each one another slice's, and none
// of them reachable from here:
//
//   - no HTTP, MCP or UI. Surfaces are thin adapters (§5: "one service layer,
//     thin REST and MCP adapters") and they call these methods rather than
//     re-implementing the decision.
//   - no writes. Nothing here claims, assigns, comments, transitions, links,
//     delivers, accepts or audits; a read never writes an audit row, because the
//     audit log records acts, and looking at a queue is not one.
//   - no lifecycle. Which state a request is in is the record's; what a move
//     means and who may make it is the one transition function's (S5).
//   - no delivery decision. Milestones come from the request's own lifecycle
//     and its first-write-wins timestamps. The details projection also asks the
//     outcome aggregate (request_outcome.go, #3966) for its NOTES — how many
//     items are finished — but the aggregate cannot mark a step done: the
//     projection re-gates every step against the state, so inferring delivery
//     from finished work stays impossible (contract §4, I8).
//   - no signed download grants. The requester's ready files ask a
//     RequestDownloadLinker for their reference (slice G3 ships one); with none
//     configured a ready file degrades to "unavailable", which is the
//     projection's own fail-closed rule and never a broken download link.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- Shapes ----

// RequestQueueInput is one inbox read: which team's queue, the filters the
// adapter's query string carried, the keyset cursor, and whether the caller
// also wants their own claimed/assigned slice of the same queue.
//
// The tenant is a team id only, and deliberately so. The org is read from the
// team row (053's composite foreign key makes the pair one fact), so an adapter
// cannot pair a team with the wrong org, and a caller cannot aim a read at
// another tenant by sending two ids that disagree. It is the same rule
// SubmitRequest follows for intake.
type RequestQueueInput struct {
	// TeamID is the inbox being read. Required.
	TeamID int

	// States matches any of the listed lifecycle states; empty means every
	// state. An unknown value is refused (request.invalid) rather than matched
	// against nothing: a typo that silently returns no rows reads as "the queue
	// is empty", which is the one answer a queue must never invent.
	States []domain.RequestStatus
	// RequesterUserID narrows the queue to one requester's requests — the
	// triager asking "what has this person sent us?". It is a filter on the
	// team's queue, not a visibility decision: the caller still has to be an
	// inbox member to read the queue at all.
	RequesterUserID *int
	// SourceKind narrows to one intake surface (web/mcp/api/connector).
	SourceKind domain.RequestSourceKind
	// CreatedAfter and CreatedBefore are a half-open window on created_at
	// (after is inclusive, before exclusive).
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	// Attached, when set, splits the queue into "has work planned or routed"
	// (true) and "nothing attached yet" (false).
	Attached *bool
	// Query is a case-insensitive substring match over the request's subject,
	// public reference and input brief. Its LIKE wildcards are literal.
	Query string

	// BeforeID pages back; 0 is the first page. Limit 0 means the default
	// (50) and anything above the ceiling (200) is clamped, the contract's
	// bounds.
	BeforeID int
	Limit    int

	// Mine asks for the caller's own slice of this same queue — the requests
	// they hold the triage claim on or own the outcome of. It is the "Claimed
	// by me" panel's data, and it is a slice of the filter above, never a
	// separate queue. A principal with no user row has nothing of its own, so
	// Mine stays nil for it.
	Mine bool
	// MineBeforeID and MineLimit page that slice independently of the queue
	// page; the slice's own total is always cursor-free.
	MineBeforeID int
	MineLimit    int
}

// RequestQueue is one page of a team's queue with the two server aggregates a
// team-inbox screen needs beside it: the per-state counts for the filter (the
// pill row) and, when asked, the caller's own claimed/assigned slice.
//
// The shape is the {items, count, next_before_id} envelope amp_kb_list and
// ListAudit already answer with, plus the aggregates under their own names, so
// a client reads the page and the numbers from one response and cannot render a
// pill that disagrees with the list beneath it.
type RequestQueue struct {
	// Items is the page, newest first.
	Items []domain.TeamRequest `json:"items"`
	// Count is how many rows this page holds (not the filtered total; Totals
	// carries that).
	Count int `json:"count"`
	// NextBeforeID is the cursor for the next page, 0 on the last one.
	NextBeforeID int `json:"next_before_id,omitempty"`

	// Totals is the per-state breakdown of the whole filter with its paging
	// fields cleared: the number on each queue pill, and their sum.
	Totals repository.RequestStateTotals `json:"totals"`

	// Mine is the caller's claimed/assigned slice of the same filter, nil when
	// it was not asked for (or when the caller has no user row to own
	// anything).
	Mine *repository.RequestOwnership `json:"mine,omitempty"`
}

// RequestReadInput names one request for the single-request read. It has three
// fields because the two surfaces address a request differently, and the
// difference is real rather than cosmetic:
//
//   - the team addresses it by serial inside a team's inbox (its detail page
//     lives under that team), so TeamID + RequestID;
//   - a requester holds an opaque reference and never the serial (D1), and has
//     no team to name, so PublicRef alone — or RequestID alone for a surface
//     that kept the id from its own submission's answer.
//
// Exactly one of RequestID and PublicRef names the request. TeamID belongs to
// the team surface; a requester read never carries it, and a read that carries
// it is answered as the team's (which is also what a requester who is a team
// member wants: RequestViewFor gives the team's full view to a triager, even
// when they are also the requester).
type RequestReadInput struct {
	// TeamID is the inbox the request is expected in, 0 for the requester
	// surface. When set, the request is loaded inside that team's tenant, so an
	// id from another team is not found rather than found-and-refused.
	TeamID int
	// RequestID is the request's serial, 0 when PublicRef names it.
	RequestID int
	// PublicRef is the requester's opaque reference (the display form's REQ-
	// prefix is accepted and stripped). Empty when RequestID names it.
	PublicRef string
}

// RequestRead is the answer to "let me see this request", and it is exactly one
// of two things:
//
//   - the team's view: Record is the stored request and Projection is nil. Every
//     field is the record's own; triage works from it.
//   - the requester's view: Projection is the whitelist object and Record is
//     nil. Nothing internal has a field to reach here, which is what makes "a
//     private field never reaches a requester" structural rather than a promise.
//
// A caller that wants to be sure which it got reads View (authz.RequestView), or
// simply branches on which field is set; there is no third state and no
// partially-filled one.
type RequestRead struct {
	// View is the decision that produced this answer: "team" or "requester".
	View authz.RequestView
	// Record is the stored request, for the team's view only.
	Record *domain.TeamRequest
	// Projection is the requester's whitelist view, for the requester's own
	// request only.
	Projection *domain.RequesterRequestView
}

// OwnRequestListInput is one requester's list read: the filters the requester
// surface offers over a person's own requests, and the keyset cursor.
//
// There is no tenant and no owner in it: the list is the caller's own requests
// by construction (the repository's own-request reads are scoped by the
// requester column, which comes from the principal), so there is no field here
// that could aim it at somebody else.
type OwnRequestListInput struct {
	// States matches any of the listed lifecycle states; empty means all. The
	// requester surface filters the list into open and closed, which is a
	// filter over the caller's own rows and nothing more.
	States []domain.RequestStatus

	// BeforeID pages back; 0 is the first page. Limit 0 means the default (50),
	// above the ceiling (200) is clamped.
	BeforeID int
	Limit    int
}

// OwnRequestPage is one page of a requester's requests, already projected.
//
// Every item is a domain.RequesterRequestView — never a record — so the list
// surface has nothing internal to leak even by mistake. The rows are projected
// without their thread and their files: those belong to the detail read (one
// request at a time), and a list row does not open them. Milestones are
// per-row because the request's own lifecycle determines them; the projection
// re-gates each one against that lifecycle, so a row cannot claim a step its
// state has not reached.
type OwnRequestPage struct {
	// Items is the page, newest first.
	Items []*domain.RequesterRequestView `json:"items"`
	// Count is how many rows this page holds.
	Count int `json:"count"`
	// NextBeforeID is the cursor for the next page, 0 on the last one.
	NextBeforeID int `json:"next_before_id,omitempty"`
	// Total is how many requests the caller has in total (cursor-free), so a
	// list header never shrinks as someone pages.
	Total int `json:"total"`
}

// RequestDownloadLinker mints the opaque reference a requester's ready file is
// downloaded with — contract §4's "explicitly shared downloads", served through
// a short-lived signed grant (slice G3). The read path calls it only for a file
// the download rules already allow, and it passes the reference through
// untouched; the projection advertises it only on a ready file.
//
// It is an interface here so this slice can ship before G3 does: with no linker
// configured, a ready file degrades to "unavailable" (the projection's own
// fail-closed rule) instead of advertising a download that cannot work. An
// error from it is returned, not swallowed: a read path that hid a failed grant
// would show a requester "unavailable" for a file the team did deliver, and
// nobody would learn that minting is broken.
type RequestDownloadLinker interface {
	// RequesterDownloadRef returns the opaque reference for one attachment of
	// req, or "" when nothing can be minted for it. p is the requester whose
	// own read this is.
	RequesterDownloadRef(ctx context.Context, p authz.Principal, req *domain.TeamRequest, a *domain.RequestAttachment) (string, error)
}

// ---- The queue ----

// ListRequests returns one page of teamID's queue, the per-state counts for the
// same filter, and (when asked) the caller's own claimed/assigned slice of it.
//
// The caller must be an inbox member: a member or lead of the team, an owner or
// admin of its org, or a global admin — Checker.InboxRole's whole allow-set,
// re-read from the rows on this call. Anyone else, and any team that does not
// exist, is answered with the surface's single not-found: a stranger cannot
// learn that a team id is taken, and a submit grant confers no queue access
// (contract §2: a requester "cannot read other requests").
//
// One filter, three answers. The page, the pill counts and the personal slice
// are built from the same RequestFilter, so a state filter the caller sent
// narrows all three and a pill cannot describe a different queue than the page
// it sits above. The paging fields differ on purpose: the page honors the
// caller's cursor, while the totals and the slice's own total are cursor-free
// (the repository clears them on a value copy), so neither number shrinks as
// someone scrolls.
func (s *Service) ListRequests(ctx context.Context, p authz.Principal, in RequestQueueInput) (*RequestQueue, error) {
	// A submit-scoped credential reads no inbox, its own team's included
	// (AMP #3772): the scope is exactly one operation, and reading is not it.
	// Refused before anything is loaded, so a narrowed credential cannot probe
	// team ids through this path.
	if err := p.RequireUnscoped("read a team's request queue"); err != nil {
		return nil, err
	}
	team, err := s.readInboxTeam(ctx, p, in.TeamID)
	if err != nil {
		return nil, err
	}
	filter := requestQueueFilter(team, in)

	page, err := s.Repo.ListRequests(ctx, filter)
	if err != nil {
		return nil, err
	}
	// The same filter, paging cleared by the repository on its own copy, so the
	// pills count the queue rather than the rest of this page.
	totals, err := s.Repo.CountRequestsByState(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := &RequestQueue{
		Items:        page.Items,
		Count:        page.Count,
		NextBeforeID: page.NextBeforeID,
		Totals:       totals,
	}
	if in.Mine && p.HasUser() {
		mine := filter
		mine.BeforeID = in.MineBeforeID
		mine.Limit = in.MineLimit
		owned, err := s.Repo.RequestsOwnedBy(ctx, mine, p.UserID)
		if err != nil {
			return nil, err
		}
		out.Mine = owned
	}
	return out, nil
}

// readInboxTeam loads teamID and requires p to be an inbox member, answering
// the surface's not-found for every way that can fail to be true.
//
// The team is loaded first for a reason that is about data rather than
// authorization: its org is the record's other tenant half, and reading it from
// the row is what makes "an adapter cannot pair a team with the wrong org"
// structural. Checker.InboxRole then re-reads the membership, org role and (for
// a global admin) the team row itself, so exactly one implementation decides
// who reads an inbox — the same one the single-request read uses.
//
// A team id that is not a team, and a team the caller has no standing on, are
// the same answer here, because a surface must not let a stranger discover which
// team ids exist.
func (s *Service) readInboxTeam(ctx context.Context, p authz.Principal, teamID int) (*domain.Team, error) {
	if teamID <= 0 {
		return nil, authz.RequestNotFound()
	}
	team, err := s.Repo.GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, authz.RequestNotFound()
		}
		return nil, err
	}
	role, err := s.Authz.InboxRole(ctx, p, team.ID)
	if err != nil {
		return nil, err
	}
	if role < authz.InboxMember {
		return nil, authz.RequestNotFound()
	}
	return team, nil
}

// requestQueueFilter turns the adapter's input into the one RequestFilter every
// read of this call uses. It is a function rather than three inline literals so
// the page, the pills and the personal slice cannot be built from three slightly
// different filters — the bug a pill row gets wrong exactly once.
//
// The tenant comes from the team row (never from the input), and nothing here
// validates the individual filters: the repository owns each term's rules
// (states are normalized and refused there, source kinds are checked against the
// closed set, LIKE wildcards are escaped), so a term added there is honored here
// without this file changing.
func requestQueueFilter(team *domain.Team, in RequestQueueInput) repository.RequestFilter {
	return repository.RequestFilter{
		OrgID:           team.OrgID,
		TeamID:          team.ID,
		States:          in.States,
		RequesterUserID: in.RequesterUserID,
		SourceKind:      in.SourceKind,
		CreatedAfter:    in.CreatedAfter,
		CreatedBefore:   in.CreatedBefore,
		Attached:        in.Attached,
		Query:           in.Query,
		BeforeID:        in.BeforeID,
		Limit:           in.Limit,
	}
}

// ---- One request ----

// GetRequest is the single authorized read of one request, serving both views.
//
// Order of decisions, and why it is this order:
//
//  1. The scope gate. A submit-scoped credential may submit and nothing else, so
//     it is refused before a request is loaded (AMP #3772).
//  2. Load, scoped by whichever surface named the request. The team surface
//     loads inside the named team's tenant (an id from another team is not
//     found, never found-and-refused). The requester surface loads through the
//     own-request read, which is scoped by the caller's own requester column —
//     so another requester's request, a service- or connector-submitted request
//     with no requester, and an id or reference that never existed are all
//     ErrNotFound.
//  3. One decision, Checker.ViewForLoad: a failed load and an unauthorized view
//     both leave through authz.RequestNotFound, so the two are the same bytes and
//     no handler has to remember to make them match. Any other load error is
//     returned as itself (a database problem is not a missing row).
//  4. Build the one view the decision allows. The team's view is the record; the
//     requester's is the projection, and it is the only field filled for them.
//
// A caller who is both a triager and the requester gets the team's view: the
// decision ranks the team's standing first, because a triager must see the
// record they triage, and the projection is not that record.
func (s *Service) GetRequest(ctx context.Context, p authz.Principal, in RequestReadInput) (*RequestRead, error) {
	if err := p.RequireUnscoped("read a request"); err != nil {
		return nil, err
	}
	ref, err := normalizeReadRef(in)
	if err != nil {
		return nil, err
	}

	var req *domain.TeamRequest
	var loadErr error
	switch {
	case in.TeamID > 0:
		// Team surface: the tenant comes from the team row, so the pair is one
		// fact and a reference or id from another team cannot be reached.
		var team *domain.Team
		team, err = s.readTeamForRead(ctx, in.TeamID)
		if err != nil {
			return nil, err
		}
		if ref.id > 0 {
			req, loadErr = s.Repo.GetRequest(ctx, ref.id, team.OrgID, team.ID)
		} else {
			req, loadErr = s.Repo.GetRequestByRef(ctx, ref.publicRef, team.OrgID, team.ID)
		}
	case ref.id > 0:
		// Requester surface by serial. Without a user row there is nothing the
		// request could belong to, so it is simply not yours — the not-found
		// answer, never a hint that the id exists.
		if !p.HasUser() {
			return nil, authz.RequestNotFound()
		}
		req, loadErr = s.Repo.GetRequestByRequester(ctx, ref.id, p.UserID)
	case ref.publicRef != "":
		if !p.HasUser() {
			return nil, authz.RequestNotFound()
		}
		req, loadErr = s.Repo.GetRequestByRequesterRef(ctx, ref.publicRef, p.UserID)
	}

	view, err := s.Authz.ViewForLoad(ctx, p, req, loadErr)
	if err != nil {
		return nil, err
	}
	return s.buildRequestRead(ctx, p, view, req)
}

// requestReadRef is one way of naming a request: its serial, or the requester's
// opaque reference. Exactly one is set.
type requestReadRef struct {
	id        int
	publicRef string
}

// normalizeReadRef enforces "name the request exactly one way" and normalizes
// the reference through the domain's own shape rule (so REQ-8F2Q1 pasted from
// the UI finds the stored token, and a reference the domain could never have
// minted is refused rather than searched for).
//
// A missing name is a caller bug, not a not-found: an adapter that passed
// neither has nothing to look up, and answering 404 would hide its mistake
// behind a legitimate answer. A request.invalid names the field.
func normalizeReadRef(in RequestReadInput) (requestReadRef, error) {
	hasID, hasRef := in.RequestID > 0, strings.TrimSpace(in.PublicRef) != ""
	switch {
	case hasID && hasRef:
		return requestReadRef{}, authz.RequestInvalid("name the request by its id or by its reference, not both")
	case !hasID && !hasRef:
		return requestReadRef{}, authz.RequestInvalid("name the request by its id or by its reference")
	case hasID:
		return requestReadRef{id: in.RequestID}, nil
	}
	ref, err := domain.NormalizeRequestPublicRef(in.PublicRef)
	if err != nil {
		// The domain's message names the field and never echoes the rejected
		// value back (contract §5: a refusal must not repeat content).
		return requestReadRef{}, authz.RequestInvalid("%s", err)
	}
	return requestReadRef{publicRef: ref}, nil
}

// readTeamForRead loads a team for the team surface of GetRequest. It does not
// decide authorization — Checker.ViewForLoad does that after the request is
// loaded — it only supplies the org half of the tenant and answers the surface's
// not-found for a team id that is not a team.
//
// Loading the team before the request is deliberate: it keeps the tenant
// pairing honest (the org always comes from the row 053's foreign key checks)
// and it means a stranger naming an unknown team costs one lookup rather than a
// scan of the request table.
func (s *Service) readTeamForRead(ctx context.Context, teamID int) (*domain.Team, error) {
	team, err := s.Repo.GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, authz.RequestNotFound()
		}
		return nil, err
	}
	return team, nil
}

// buildRequestRead turns one authorized view and one loaded record into the one
// answer that view allows. It is the only place a RequestRead is assembled, so
// "the requester gets the projection and never the record" holds for both entry
// points rather than for the one somebody remembered.
func (s *Service) buildRequestRead(ctx context.Context, p authz.Principal, view authz.RequestView, req *domain.TeamRequest) (*RequestRead, error) {
	if req == nil || view.None() {
		// Unreachable through ViewForLoad, which refuses both. It is here so a
		// future caller that skips the decision fails closed instead of
		// assembling an empty team view.
		return nil, authz.RequestNotFound()
	}
	if view.Full() {
		return &RequestRead{View: view, Record: req}, nil
	}
	projection, err := s.projectRequesterRequest(ctx, p, req, true)
	if err != nil {
		return nil, err
	}
	return &RequestRead{View: view, Projection: projection}, nil
}

// ---- The requester's own list ----

// ListOwnRequests returns one page of the caller's own requests, projected, with
// the cursor-free total.
//
// "Own" is the durable requester column, so the list is the requests this person
// submitted — not the ones somebody assigned to them, and not their team's
// queue. A team member reading it sees their own submissions; a requester sees
// theirs and nobody else's, because the repository's own-request predicate is an
// equality on that one column and there is no filter in OwnRequestListInput that
// could widen it.
//
// A caller with no AMP user row owns nothing, so the answer is an empty page
// with a zero total rather than a refusal: the surface is "your requests", and
// having none is not an error. A submit-scoped credential is refused, because
// reading is not its one operation.
func (s *Service) ListOwnRequests(ctx context.Context, p authz.Principal, in OwnRequestListInput) (*OwnRequestPage, error) {
	if err := p.RequireUnscoped("read your requests"); err != nil {
		return nil, err
	}
	out := &OwnRequestPage{Items: []*domain.RequesterRequestView{}}
	if !p.HasUser() {
		return out, nil
	}
	filter := repository.OwnRequestFilter{
		RequesterUserID: p.UserID,
		States:          in.States,
		BeforeID:        in.BeforeID,
		Limit:           in.Limit,
	}
	page, err := s.Repo.ListOwnRequests(ctx, filter)
	if err != nil {
		return nil, err
	}
	total, err := s.Repo.CountOwnRequests(ctx, filter)
	if err != nil {
		return nil, err
	}
	// teamNames keeps this to at most one team lookup per team on the page: a
	// requester's requests almost always belong to one desk, and the desk's name
	// is one field rather than a join. A lookup failure is returned rather than
	// answered with an empty name: "the desk" with no name would be a quiet lie
	// about a row that is fine.
	teamNames := map[int]string{}
	for _, req := range page.Items {
		name, ok := teamNames[req.TeamID]
		if !ok {
			team, err := s.Repo.GetTeam(ctx, req.TeamID)
			if err != nil {
				return nil, err
			}
			name = team.Name
			teamNames[req.TeamID] = name
		}
		out.Items = append(out.Items, s.projectRequesterRequestRow(&req, name))
	}
	out.Count = page.Count
	out.NextBeforeID = page.NextBeforeID
	out.Total = total
	return out, nil
}

// ---- The requester's projection, as the read path builds it ----

// projectRequesterRequest builds the requester's view of one request from what
// this read path is allowed to hand over: the record's requester-safe subset,
// the desk's name, the milestones its own lifecycle establishes, the comments
// the team shared, and the files that are this requester's to see.
//
// withThread and withFiles are the one difference between the detail read and a
// list row: a list row does not open a thread or a file list it will not show.
// Everything else — the whitelist, the sanitizing, the fail-closed degradations
// — is ProjectRequesterRequest's, so there is exactly one implementation of what
// a requester may see.
func (s *Service) projectRequesterRequest(ctx context.Context, p authz.Principal, req *domain.TeamRequest, withThread bool) (*domain.RequesterRequestView, error) {
	team, err := s.Repo.GetTeam(ctx, req.TeamID)
	if err != nil {
		// The request cascades with its team (053), so a request whose team
		// cannot be loaded is a real inconsistency — returned, never papered
		// over with an empty desk name.
		return nil, err
	}
	in := RequesterProjectionInput{
		Request:  req,
		TeamName: team.Name,
		// The aggregate is the same one the team rail reads (#3966), so the
		// requester's stepper notes and the desk's progress come from one
		// definition. A read failure here costs the notes, never the view: the
		// steps themselves are the lifecycle's and cannot be wrong.
		Milestones: s.requesterMilestones(ctx, req),
	}
	if withThread {
		comments, err := s.Repo.ListRequestComments(ctx, req.ID, req.OrgID, req.TeamID, 0, 0)
		if err != nil {
			return nil, err
		}
		in.Comments = s.requesterSharedComments(req, comments)
		outputs, err := s.requesterOutputs(ctx, p, req)
		if err != nil {
			return nil, err
		}
		in.Outputs = outputs
	}
	return ProjectRequesterRequest(in), nil
}

// projectRequesterRequestRow is the list-row variant: the record's whitelist and
// its milestones under the desk's name, with no thread and no files. It exists
// so the list loop reads as one call and cannot accidentally open a thread per
// row, and it is a separate function (rather than a bool flag at the call site)
// so the difference between a row and a detail is visible in the code.
func (s *Service) projectRequesterRequestRow(req *domain.TeamRequest, teamName string) *domain.RequesterRequestView {
	return ProjectRequesterRequest(RequesterProjectionInput{
		Request:    req,
		TeamName:   teamName,
		Milestones: requesterMilestonesFor(req),
	})
}

// requesterMilestonesFor builds the public stepper from the request's own
// lifecycle: the five closed steps, each "done" when the request's state has
// reached it, with the first-write-wins timestamps the record carries.
//
// It counts nothing and reads no linked work. The contract is explicit that a
// closing ticket never flips a request (§4, I8), and the projection re-gates
// every step against the state anyway, so a step can never be announced before
// the request itself got there. That is also why the slice can ship before the
// outcome aggregate (E2) does: the aggregate will add *notes* and evidence to
// these steps, but the question "has this request been delivered?" is answered
// by the request's state, and nothing else may answer it.
//
// A step's At is set only when the record has a timestamp for it. Triage has no
// entry timestamp in 053, so "Being reviewed" is done without a time rather
// than with an invented one.
func requesterMilestonesFor(req *domain.TeamRequest) []domain.RequesterMilestoneInput {
	if req == nil {
		return nil
	}
	created := req.CreatedAt
	out := []domain.RequesterMilestoneInput{
		{Key: domain.RequesterMilestoneReceived, Done: true, At: &created},
		{Key: domain.RequesterMilestoneReviewing},
		{Key: domain.RequesterMilestoneInProgress},
		{Key: domain.RequesterMilestoneDelivered, At: req.DeliveredAt},
		{Key: domain.RequesterMilestoneAccepted, At: req.AcceptedAt},
	}
	// Done is asked of the same closed rule the projection enforces, so this
	// list cannot say "done" for a state the projection would then have to undo:
	// one definition of what a step means, used twice.
	for i := range out {
		out[i].Done = domain.RequesterMilestoneDoneAllowed(out[i].Key, req.State)
	}
	return out
}

// requesterMilestones is the detail projection's stepper: the same closed steps,
// with the outcome aggregate's notes attached (RequesterMilestonesFor).
//
// The aggregate's read is best-effort on purpose. It adds a sentence about how
// much work is finished; it decides nothing. So a failure to read the linked work
// leaves the stepper exactly as it was before this slice existed — five steps
// gated by the lifecycle — rather than failing a requester's read of their own
// request. The list rows deliberately do NOT pay for this read (one aggregate per
// row would turn a page into N queries); a row's stepper is the lifecycle's.
func (s *Service) requesterMilestones(ctx context.Context, req *domain.TeamRequest) []domain.RequesterMilestoneInput {
	if req == nil {
		return nil
	}
	var out *RequestOutcome
	if facts, err := s.Repo.RequestOutcomeFactsFor(ctx, req.ID, req.OrgID, req.TeamID); err == nil {
		out = BuildRequestOutcome(requestOutcomeInput(facts))
	}
	return RequesterMilestonesFor(req, out)
}

// requesterSharedComments decides which of a request's comments the requester
// sees, and attributes each one.
//
// The team's thread is not the requester's thread: a plain `comment` is the desk
// talking among itself (triage notes, routing reasoning, "I'll take this one"),
// and 054 has no per-row sharing column, so a read path has to make the call
// rather than pass everything through. The rule this function implements is the
// narrow one:
//
//   - a `question` is the desk asking *this requester* something (I5 makes it
//     the prerequisite of needs_information) — shared by definition;
//   - an `answer` is what the requester said back — theirs by definition, and
//     gating it on the author would hide their own words from them;
//   - a plain `comment` is shared only when the requester wrote it, so their own
//     remarks come back to them and nobody else's internal notes do;
//   - a `system` comment is the record's own voice (a transition's note) and is
//     never shared; the projection has a state word and timestamps for that, and
//     a system row can name internal actors.
//
// Attribution collapses to the projection's two words: "you" when the stored
// author is this requester, "desk" for everyone else — an owner, a triager, an
// agent, a worker or a connector's service client. Which person on the team
// wrote it is the team's business (contract §4).
func (s *Service) requesterSharedComments(req *domain.TeamRequest, comments []domain.RequestComment) []domain.RequesterCommentInput {
	if len(comments) == 0 {
		return nil
	}
	requester := 0
	if req.RequesterUserID != nil {
		requester = *req.RequesterUserID
	}
	out := make([]domain.RequesterCommentInput, 0, len(comments))
	for _, c := range comments {
		fromRequester := requester != 0 && c.AuthorUserID != nil && *c.AuthorUserID == requester
		switch c.Kind {
		case domain.RequestCommentQuestion, domain.RequestCommentAnswer:
			// Shared: the desk asked, or the requester replied.
		case domain.RequestCommentDiscussion:
			if !fromRequester {
				continue
			}
		default:
			// `system` (and anything the schema's CHECK does not name, which
			// cannot be stored) is not a requester-visible remark.
			continue
		}
		out = append(out, domain.RequesterCommentInput{
			Kind:          c.Kind,
			Body:          c.Body,
			FromRequester: fromRequester,
			At:            c.CreatedAt,
		})
	}
	return out
}

// requesterOutputs is the files on a request that its requester may see, in the
// projection's three-word vocabulary and with a download reference only when the
// file may actually be downloaded.
//
// v1 shares one source: the requester's own uploads (uploaded_by is this
// requester). The team's delivered outputs are an explicit share the team makes
// when it delivers (E2's delivered_by links to artifacts and receipts, then G3's
// grants); nothing in this slice may infer that a file of the team's is the
// requester's to see, so nothing is shared by accident.
//
// The status mapping is domain.RequestAttachmentDownloadRefusal's, not a second
// opinion: a file that function refuses is not "ready" here, whatever the reason
// — infected and purged are unavailable (nothing can serve them and nothing will
// change that), a pending scan and a lead's hold are preparing (the team can
// still make it ready). The reference itself is minted by the configured
// RequestDownloadLinker and only for a file the rules allow; with no linker
// there is no reference, and the projection degrades the file rather than
// advertising a download that cannot work.
func (s *Service) requesterOutputs(ctx context.Context, p authz.Principal, req *domain.TeamRequest) ([]domain.RequesterOutputInput, error) {
	files, err := s.Repo.ListRequestAttachments(ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	now := s.now()
	out := make([]domain.RequesterOutputInput, 0, len(files))
	for i := range files {
		a := &files[i]
		// Own uploads only: `uploaded_by` is NULL when the uploader's account
		// was deleted, and a file nobody is known to have uploaded is not
		// shared with a requester on the strength of a guess.
		if a.UploadedBy == nil || *a.UploadedBy != p.UserID {
			continue
		}
		status := domain.RequesterOutputUnavailable
		ref := ""
		switch domain.RequestAttachmentDownloadRefusal(a, now) {
		case domain.RequestAttachmentServed:
			// The download rules allow it, so the only thing left to decide is
			// whether a reference can be minted. With no linker configured (or a
			// linker that minted nothing) nothing can serve the bytes, so the
			// honest word is "unavailable", not "ready": the projection would
			// degrade it anyway (its advertise-only-a-working-download rule), and
			// saying so here keeps this path's own claim true as well.
			if s.RequestDownloads != nil {
				minted, err := s.RequestDownloads.RequesterDownloadRef(ctx, p, req, a)
				if err != nil {
					return nil, err
				}
				if strings.TrimSpace(minted) != "" {
					status, ref = domain.RequesterOutputReady, minted
				}
			}
		case domain.RequestAttachmentRefusedPending, domain.RequestAttachmentRefusedHeld:
			// Nothing is wrong with the file; the team can still make it ready.
			status = domain.RequesterOutputPreparing
		}
		created := a.CreatedAt
		out = append(out, domain.RequesterOutputInput{
			Source:      domain.RequesterOutputFromRequester,
			Name:        a.FileName,
			SizeBytes:   a.SizeBytes,
			Status:      status,
			At:          &created,
			DownloadRef: ref,
		})
	}
	return out, nil
}
