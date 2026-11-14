package api

// Team request reads over REST (epic 202, story 549, AMP #3968; build slice F1's
// list half and F2's read half of
// discoveries/team-inbox-approved-build-slices.md; approved product contract
// artifact 36 v2 §2, §4 and §5).
//
//	GET /api/teams/:id/requests             the team's queue: one keyset page, the per-state
//	                                        counts beside it, and (when asked) the caller's own
//	                                        claimed/assigned slice of the same queue
//	GET /api/teams/:id/requests/summary     the same counts without a page (My work's inbox panels)
//	GET /api/teams/:id/requests/:request    one request — by serial or by public reference — with
//	                                        its delivery aggregate
//	GET /api/teams/:id/requests/:request/comments
//	                                        one request's discussion, as the caller's view of that
//	                                        request allows it (AMP #4270)
//	GET /api/teams/:id/requests/:request/events
//	                                        one request's history rows, team view only (AMP #4271)
//	GET /api/teams/:id/requests/:request/links
//	                                        one request's linked work, filterable by kind,
//	                                        team view only (AMP #4272)
//	GET /api/requests                       the caller's OWN requests, projected, never the record
//	GET /api/requests/:request              one of the caller's own, by serial or public reference
//
// Every one of them is a thin adapter over the one authorized read path
// (internal/service/request_views.go, slice S2: ListRequests, GetRequest,
// ListOwnRequests, and the three one-request reads ListRequestComments,
// ListRequestEvents and ListRequestLinks). It decodes a query
// string or a path, maps the transport's own facts onto the service's input and
// encodes the service's answer. It
// decides nothing: who may read what, the tenant, the keyset cursor, the
// cursor-free counts, the requester whitelist and every 404 are the service's,
// so the codes a client branches on are the same ones the MCP reads (slice I1)
// answer with.
//
// # The view matrix is the service's, and these routes cannot widen it
//
//   - an inbox member (team member or lead, an owner or admin of the team's org,
//     a global admin) reads the queue and the record;
//   - a requester — the customer who sent a request — reads their own requests,
//     projected, and nothing else: no queue, no other person's request, no team
//     record;
//   - a submit-scoped service credential reads none of it. submitScopeGate
//     (token_scope.go, AMP #3772) refuses it here before the handler runs, and
//     the capability layer refuses it again, so a route added tomorrow cannot
//     inherit read access by forgetting the first check;
//   - a stranger, a foreign team, and a team id that was never issued are one
//     answer: 404 request.not_found, byte for byte (authz.RequestNotFound), so a
//     probe learns nothing about which teams, requests or references exist.
//
// Registering through deniedMux is what puts the scope gate and the
// access.denied audit in front of all five routes, so a 403 is recorded like
// every other refusal on this surface. A 404 is not: "not found" is the answer
// for something the caller may not see, and filing a prober's enumeration
// attempts is noise, not an access event (decisions/audit-log-v2.md).
//
// # Two shapes, and the record only ever reaches one of them
//
// The queue pages `domain.TeamRequest` records, because its caller is an inbox
// member whose job is to triage them. The requester's own list and detail answer
// `domain.RequesterRequestView` — the whitelist projection, whose type has no
// field for an org, a claim, an assignee, a contact snapshot, a revision, a
// metadata key, a ticket or an agent identity (slice E3). The service never
// fills both halves of a RequestRead, and writeRequestRead refuses to encode a
// record in a requester answer even if it were handed one, so "a requester gets
// the projection" is structural on this surface too rather than a convention
// each handler has to remember.
//
// What this file deliberately does NOT do — each one another slice's concern,
// and none of them reachable from these routes:
//
//   - no writes. Nothing here submits, edits, comments, claims, assigns,
//     transitions, links, delivers or accepts; a read never writes an audit row,
//     because looking at a queue is not an act on it.
//   - no attachment reads. Files are slice G3's (the mint and serve routes
//     already exist in request_download.go); the thread read is here because it
//     is one request's read, and it is the discussion's only read (AMP #4270).
//   - no surrogate counts. Every number here is the service's aggregate over the
//     same predicate as the page it sits beside, so a pill can never describe a
//     different queue than the list beneath it (request_summaries.go).

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// ---- The team's queue ----

// teamRequestQueue serves GET /api/teams/:id/requests.
func (h *RestHandler) teamRequestQueue(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	in, err := parseRequestQueueQuery(r.URL.Query(), p)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	in.TeamID = teamID
	queue, err := h.svc.ListRequests(r.Context(), p, in)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, queue)
}

// requestQueueSummary is the queue's counts without its page: what My work's
// "Team inboxes" panel reads for each team it lists, and the "Claimed by me ·
// team requests" panel's numbers. It is the same two aggregates the queue
// answers with (RequestQueue.Totals and RequestQueue.Mine), under their own
// names so a caller cannot mistake the answer for a page.
type requestQueueSummary struct {
	// Totals is the per-state breakdown of the filter, cursor-free.
	Totals repository.RequestStateTotals `json:"totals"`
	// Mine is the caller's claimed/assigned slice, nil when it was not asked
	// for (or when the caller has no user row to own anything).
	Mine *repository.RequestOwnership `json:"mine,omitempty"`
}

// teamRequestSummary serves GET /api/teams/:id/requests/summary.
//
// It is the queue read with the page thrown away: the same filters, the same
// authorization and the same aggregates, so the numbers can never come from a
// second implementation. The page it asks for is the smallest the service can
// produce (one row), which is the price of reusing the one authorized read
// path rather than growing a second, divergent one in the repository.
func (h *RestHandler) teamRequestSummary(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	in, err := parseRequestQueueQuery(r.URL.Query(), p)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	in.TeamID = teamID
	// A summary is the counts: no cursor, and the cheapest page there is. The
	// caller's mine_* paging is left alone, because that slice's ids are part of
	// the answer it asked for.
	in.BeforeID, in.Limit = 0, 1
	queue, err := h.svc.ListRequests(r.Context(), p, in)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, requestQueueSummary{Totals: queue.Totals, Mine: queue.Mine})
}

// ---- One request, and the requester's own reads ----

// teamRequestDetail serves GET /api/teams/:id/requests/:request — the team's
// view of one request, addressed by its serial or by its public reference.
//
// The literal `/requests/summary` pattern registered beside this one is more
// specific, so net/http routes that path to teamRequestSummary and never here;
// and a reference can never be the word "summary" anyway (053's shape is 16 to
// 64 characters), so the two cannot be confused either way.
func (h *RestHandler) teamRequestDetail(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	ref, err := requestReadTarget(r.PathValue("request"))
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	ref.TeamID = teamID
	read, err := h.svc.GetRequest(r.Context(), p, ref)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	h.writeRequestRead(w, r, read)
}

// teamRequestComments serves GET /api/teams/:id/requests/:request/comments:
// one request's discussion.
//
// It is the read the POST beside it has always lacked (AMP #4270): the wire
// list of the write route, in the shape the client already types
// (RequestComment: id, request_id, kind, body, author, author_user_id,
// created_at), so the detail page can render the thread without a second shape
// to keep in step.
//
// It decides nothing. The path value is decoded with requestReadTarget — the
// same decoder the detail read uses, so the serial and the requester's opaque
// reference are accepted exactly where they are accepted there — and the service
// runs the same load-and-decide, which is why an unknown team, another team's
// request, another requester's request and a request in another org are one 404
// with one code, byte for byte, rather than a 403 from this route alone.
//
// Two views, one envelope, and exactly one of the two item fields is ever
// filled (the same discipline as writeRequestRead): the team's answer is
// `comments` — every row, including internal remarks; the requester's is
// `shared_comments`, the projection's kind filter and two-word attribution, in
// domain.RequesterComment's shape (kind, from, body, at), which has no field
// for an id, an author's name or a user id. Absent, not null: a client can tell
// "this view has no such list" from "the list is empty", which is the
// distinction the detail page's seam already relies on.
//
// after_id and limit page the team's thread forward. A requester's rows carry no
// id, so the service drops their cursor and their page is the repository's
// bounded first page: the answer says next_after_id is absent rather than
// handing them a number that is not theirs to use.
func (h *RestHandler) teamRequestComments(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	ref, err := requestReadTarget(r.PathValue("request"))
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	in, err := parseRequestThreadQuery(r.URL.Query())
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	ref.TeamID = teamID
	in.RequestReadInput = ref
	thread, err := h.svc.ListRequestComments(r.Context(), p, in)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if thread == nil || thread.View.None() {
		// Unreachable through ListRequestComments, which refuses both. It is
		// here so a future caller that skips the decision fails closed instead
		// of encoding an empty thread as a real answer.
		fail(w, authz.RequestNotFound(), http.StatusNotFound)
		return
	}
	answer := requestThreadAnswer{
		View:        thread.View.String(),
		Count:       thread.Count,
		NextAfterID: thread.NextAfterID,
	}
	if thread.View.Full() {
		answer.RequestID = thread.RequestID
		comments := thread.Comments
		if comments == nil {
			comments = []domain.RequestComment{}
		}
		answer.Comments = &comments
	} else {
		if thread.Comments != nil {
			// Fail closed rather than encode the team's thread to a requester:
			// the service guarantees this, so reaching it means a caller
			// assembled a RequestThread by hand.
			fail(w, authz.RequestNotFound(), http.StatusNotFound)
			return
		}
		shared := thread.Shared
		if shared == nil {
			shared = []domain.RequesterComment{}
		}
		answer.Shared = &shared
	}
	jsonOK(w, answer)
}

// requestThreadAnswer is one request's discussion, as the caller's view of that
// request allows it.
//
// View says which of the two item fields is set, and exactly one ever is. The
// two shapes are the product's, not this route's invention: Comments is the
// stored row (identical to the POST's own record and to the UI's RequestComment)
// and Shared is the requester's projection of a comment, which can say only
// "you" or "the desk" and carries no id.
type requestThreadAnswer struct {
	// View is the decision that produced this answer: "team" or "requester".
	View string `json:"view"`
	// RequestID is the request the thread belongs to, for the team's answer
	// only. A requester's rows and projection have no field for the internal
	// serial (they address their request by its public reference), so this answer
	// does not hand it to them — the same whitelist rule the rest of the
	// requester's surface follows.
	RequestID int `json:"request_id,omitempty"`
	// Count is how many rows this page holds.
	Count int `json:"count"`
	// NextAfterID is the forward cursor, absent on the last page and on a
	// requester's answer (whose rows carry no id).
	NextAfterID int64 `json:"next_after_id,omitempty"`
	// Comments is the team's whole thread, in append order. Pointer so that an
	// empty thread encodes as [] and a requester's answer omits it entirely.
	Comments *[]domain.RequestComment `json:"comments,omitempty"`
	// Shared is the requester's thread: the shared kinds, attributed "you"/"the
	// desk", with no id, author name or user id.
	Shared *[]domain.RequesterComment `json:"shared_comments,omitempty"`
}

// teamRequestEvents serves GET /api/teams/:id/requests/:request/events: one
// request's history rows.
//
// It is the read `request_events` has never had (AMP #4271). Every act the
// product performs appends one row — the submit, a transition, a claim, a
// release, a transfer, a link or unlink, an edit — and until this route the
// detail page's history rail had nothing to render (the temporary seam in
// v2/ui/src/pages/RequestDetail.tsx).
//
// It decides nothing. The path value is decoded with requestReadTarget, the
// same decoder the detail read and the thread read use, so the serial and the
// requester's opaque reference are accepted exactly where they are accepted
// there; the service runs the same load-and-decide, which is why an unknown
// team, another team's request, another requester's request and a request in
// another org are one 404 with one code, byte for byte.
//
// One view, one list. The history is an internal fact, so the requester's
// projection is refused by the service (it has no field for a history row and
// there is no requester-shaped event type to project into) and this handler
// fails closed if a hand-assembled answer ever carried rows on a non-team view
// — the writeRequestRead discipline, applied here for the same reason.
//
// after_id and limit page the history forward, exactly as they page a thread:
// the rows are append-only, so id order is history order and a forward cursor
// is stable under concurrent appends. The two parameters are decoded by
// parseRequestHistoryQuery, which is parseRequestThreadQuery's rules over the
// same two names — the answer's cursor is an event id, and a caller pages with
// it.
func (h *RestHandler) teamRequestEvents(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	ref, err := requestReadTarget(r.PathValue("request"))
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	in, err := parseRequestHistoryQuery(r.URL.Query())
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	ref.TeamID = teamID
	in.RequestReadInput = ref
	history, err := h.svc.ListRequestEvents(r.Context(), p, in)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if history == nil || !history.View.Full() {
		// Unreachable through ListRequestEvents, which refuses a requester and
		// answers no other view. It is here so a future caller that skips the
		// decision fails closed instead of encoding the history as a real
		// answer for someone the whitelist excludes.
		fail(w, authz.RequestNotFound(), http.StatusNotFound)
		return
	}
	events := history.Events
	if events == nil {
		events = []domain.RequestEvent{}
	}
	jsonOK(w, requestHistoryAnswer{
		View:        history.View.String(),
		RequestID:   history.RequestID,
		Count:       history.Count,
		NextAfterID: history.NextAfterID,
		Events:      &events,
	})
}

// requestHistoryAnswer is one request's history page.
//
// Events is field-for-field the UI's existing `RequestEvent`
// (v2/ui/src/types/index.ts) — domain.RequestEvent's JSON tags are that type's
// fields — so this route adds nothing to the client's shapes. It is a pointer so
// that an empty history encodes as [] rather than null: "this request has no
// acts yet" is an answer a rail must be able to render, and null reads as "not
// served".
type requestHistoryAnswer struct {
	// View is the decision that produced this answer: "team" today.
	View string `json:"view"`
	// RequestID is the request the history belongs to, so a caller can pin the
	// answer to the request it asked about.
	RequestID int `json:"request_id"`
	// Count is how many rows this page holds.
	Count int `json:"count"`
	// NextAfterID is the forward cursor, absent on the last page.
	NextAfterID int64 `json:"next_after_id,omitempty"`
	// Events is the page, in history order (by id).
	Events *[]domain.RequestEvent `json:"events,omitempty"`
}

// teamRequestLinks serves GET /api/teams/:id/requests/:request/links: one
// request's linked work — the project it is routed to, the epics/stories/tickets
// it is planned as, and the tasks/artifacts/receipts that delivered it.
//
// It is the read the POST beside it has always lacked (AMP #4272): the detail
// design draws the planning rail and the deliver panel's evidence list from these
// rows, and until this route the page reconstructed only the route row from
// `outcome.routed_project_id` and stated the rest as a gap (the temporary seam in
// v2/ui/src/pages/RequestDetail.tsx).
//
// It decides nothing. The path value is decoded with requestReadTarget — the same
// decoder the detail read and its two sibling reads use, so the serial and the
// requester's opaque reference are accepted exactly where they are accepted there
// — and the service runs the same load-and-decide, which is why an unknown team,
// another team's request, another requester's request and a request in another
// org are one 404 with one code, byte for byte.
//
// One view, one list. The lineage is an internal fact (a link row names a ticket
// serial, a project id and the triager who attached it), so the requester's view
// is refused by the service and this handler fails closed if a hand-assembled
// answer ever carried rows on a non-team view — the writeRequestRead discipline,
// applied here for the same reason. The requester's own summary of the same facts
// is the projection's `milestones`, which the detail read already serves.
//
// # The optional kind filter reuses the repository's variadic
//
// `?kind=planned_as` (and a repeated or comma-joined list) narrows the answer.
// The values are normalized and refused by domain.NormalizeRequestLinkKind before
// the service is called, because the repository's own refusal is an uncoded error
// and a client branching on `request.invalid` should not have to tell a typo from
// a server fault (parseRequestLinksQuery's note). No kind means every kind, which
// is the rail's question. There is no cursor: the list is bounded by what planning
// and delivery attached, and a page nobody needs is a page a client can miss.
func (h *RestHandler) teamRequestLinks(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	ref, err := requestReadTarget(r.PathValue("request"))
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	in, err := parseRequestLinksQuery(r.URL.Query())
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	ref.TeamID = teamID
	in.RequestReadInput = ref
	links, err := h.svc.ListRequestLinks(r.Context(), p, in)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if links == nil || !links.View.Full() {
		// Unreachable through ListRequestLinks, which refuses a requester and
		// answers no other view. It is here so a future caller that skips the
		// decision fails closed instead of encoding the lineage as a real answer
		// for someone the whitelist excludes.
		fail(w, authz.RequestNotFound(), http.StatusNotFound)
		return
	}
	rows := links.Links
	if rows == nil {
		rows = []domain.RequestLink{}
	}
	jsonOK(w, requestLinksAnswer{
		View:      links.View.String(),
		RequestID: links.RequestID,
		Count:     links.Count,
		Links:     &rows,
	})
}

// requestLinksAnswer is one request's linked work.
//
// Links is field-for-field the UI's existing `RequestLink`
// (v2/ui/src/types/index.ts) — domain.RequestLink's JSON tags are that type's
// fields: id, request_id, link_kind, target_kind, target_id, created_by,
// created_at. So this route adds nothing to the client's shapes, and the UI's
// `ResolvedRequestLink` is that type plus an OPTIONAL resolved `target` (its
// title, project, approval and tombstone facts), which a client fills from the
// target's own read — it is not this answer's to invent.
//
// It is a pointer so that a request with nothing attached encodes as [] rather
// than null: "nothing is attached yet" is an answer the planning rail must be able
// to render ("Not routed yet"), and null reads as "not served".
type requestLinksAnswer struct {
	// View is the decision that produced this answer: "team" today.
	View string `json:"view"`
	// RequestID is the request the lineage belongs to, so a caller can pin the
	// answer to the request it asked about.
	RequestID int `json:"request_id"`
	// Count is how many rows this answer holds, so a rail can say "3 planned
	// items" without counting.
	Count int `json:"count"`
	// Links is the whole lineage, in attach order (by id), tombstones included.
	Links *[]domain.RequestLink `json:"links,omitempty"`
}

// ownRequests serves GET /api/requests: the caller's own requests, projected.
func (h *RestHandler) ownRequests(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	in, err := parseOwnRequestQuery(r.URL.Query())
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	page, err := h.svc.ListOwnRequests(r.Context(), p, in)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, page)
}

// ownRequestDetail serves GET /api/requests/:request: one request the caller
// submitted, addressed by its serial or by the public reference it was answered
// with at submission.
func (h *RestHandler) ownRequestDetail(w http.ResponseWriter, r *http.Request) {
	ref, err := requestReadTarget(r.PathValue("request"))
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	// No TeamID: this is the requester surface, and the service loads it through
	// the own-request read, which is scoped by the caller's own requester column.
	// Another person's request, a service- or connector-submitted request with no
	// requester, and a reference that was never minted are all the same 404.
	read, err := h.svc.GetRequest(r.Context(), p, ref)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	h.writeRequestRead(w, r, read)
}

// requestReadAnswer is one request's answer on either surface.
//
// View says which of the two objects below is set, and exactly one ever is:
// "team" carries the stored record (triage works from it) with the delivery
// aggregate beside it; "requester" carries the whitelist projection and nothing
// else. The two live in one envelope so a client branches on one field, while
// the construction below refuses to fill more than the decision allows.
type requestReadAnswer struct {
	// View is the decision that produced this answer: "team" or "requester".
	View string `json:"view"`
	// Request is the stored record, for the team's view only.
	Request *domain.TeamRequest `json:"request,omitempty"`
	// Outcome is how the request's linked work actually stands (slice E2): the
	// counts the triage rail shows and the delivery gate the Deliver panel
	// disables its button on. Team view only — it names work items.
	Outcome *service.RequestOutcome `json:"outcome,omitempty"`
	// Projection is the requester's whitelist view of their own request. It has
	// no field for anything internal, which is why answering with it is safe
	// even though this type also has a Request field.
	Projection *domain.RequesterRequestView `json:"projection,omitempty"`
}

// writeRequestRead encodes one authorized read as the one answer its view
// allows. It is the only place a request read becomes JSON on this surface, so
// "the requester gets the projection and never the record" holds for both entry
// points rather than for the one somebody remembered.
//
// The team answer also carries the request's outcome aggregate. That read is not
// best-effort here: the aggregate is what the delivery panel gates on, and a
// rail that silently showed "nothing planned" because a query failed would be
// worse than an error.
func (h *RestHandler) writeRequestRead(w http.ResponseWriter, r *http.Request, read *service.RequestRead) {
	if read == nil || read.View.None() {
		// Unreachable through GetRequest, which refuses both. It is here so a
		// future caller that skips the decision fails closed instead of encoding
		// an empty answer.
		fail(w, authz.RequestNotFound(), http.StatusNotFound)
		return
	}
	if read.View.Full() {
		if read.Record == nil {
			fail(w, authz.RequestNotFound(), http.StatusNotFound)
			return
		}
		_, outcome, err := h.svc.RequestOutcome(r.Context(), read.Record.ID, read.Record.OrgID, read.Record.TeamID)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, requestReadAnswer{View: read.View.String(), Request: read.Record, Outcome: outcome})
		return
	}
	if read.Record != nil || read.Projection == nil {
		// Fail closed rather than encode a record to a requester: the service
		// guarantees this pair, so reaching it means a caller assembled a
		// RequestRead by hand. Answering 500 is the honest response; answering
		// with the record would be the leak this check exists to prevent.
		fail(w, authz.RequestNotFound(), http.StatusNotFound)
		return
	}
	jsonOK(w, requestReadAnswer{View: read.View.String(), Projection: read.Projection})
}

// ---- Query and path decoding ----

// requestPathTeam reads the :id path value of a team's request route. A value
// that is not a whole number is a malformed path, not a request-surface refusal:
// it names no team at all, and it is deliberately not a request.* code — the same
// answer teamSub and the submission route give.
//
// A number that is not a team (zero, or one that was never issued) is NOT
// refused here: it is passed to the service, which answers the surface's single
// 404. That keeps "this team does not exist" and "this team is not yours" one
// wall of bytes, which a 400 for id 0 would break.
func requestPathTeam(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonErr(w, errorf("the team id in the path must be a whole number"), http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// requestReadTarget turns the :request path value into the service's way of
// naming a request: its serial when the value is a positive whole number, its
// opaque public reference otherwise. Both are accepted because the two surfaces
// genuinely address a request differently — a triager has the serial, a
// requester holds only the reference their submission answered with — and the
// service normalizes and validates the reference (the display form's REQ- prefix
// is stripped, and a value the domain could never have minted is refused with
// request.invalid rather than searched for).
//
// A value that parses as neither a serial nor a legal reference shape is the
// caller's mistake, so it is a 400 and never a 404: a 404 would report "no such
// request" for a value that could not name one.
func requestReadTarget(segment string) (service.RequestReadInput, error) {
	segment = strings.TrimSpace(segment)
	if segment == "" {
		return service.RequestReadInput{}, authz.RequestInvalid("name the request by its serial or by its public reference")
	}
	if n, err := strconv.Atoi(segment); err == nil && n > 0 {
		return service.RequestReadInput{RequestID: n}, nil
	}
	return service.RequestReadInput{PublicRef: segment}, nil
}

// parseRequestQueueQuery maps the queue's query string onto the service's input.
//
// Every refusal here is authz.RequestInvalid, so the client gets the stable
// request.invalid code next to a message naming the parameter: a filter that
// silently changed meaning would be worse than a 400, and a filter silently
// dropped is exactly the bug the KB's "filter given, filter ignored" note
// records (architecture/request-queue-summaries.md).
//
// These checks are the adapter's, not a second validation layer: the repository
// owns each term's rules and refuses a bad value too. The adapter checks first
// only so the refusal is *coded* — the repository's errors carry no
// request.* code, and a client branching on the code should not have to tell a
// typo'd state from a server fault.
func parseRequestQueueQuery(q url.Values, p authz.Principal) (service.RequestQueueInput, error) {
	var in service.RequestQueueInput
	states, err := requestQueryStates(q)
	if err != nil {
		return in, err
	}
	in.States = states

	if v := strings.TrimSpace(q.Get("submitter")); v != "" {
		if strings.EqualFold(v, "me") {
			if !p.HasUser() {
				return in, authz.RequestInvalid("submitter=me needs a signed-in AMP user")
			}
			id := p.UserID
			in.RequesterUserID = &id
		} else {
			id, err := strconv.Atoi(v)
			if err != nil || id <= 0 {
				return in, authz.RequestInvalid("submitter must be a user id or \"me\"")
			}
			in.RequesterUserID = &id
		}
	}

	if v := strings.TrimSpace(q.Get("source_kind")); v != "" {
		kind := domain.RequestSourceKind(strings.ToLower(v))
		if !slices.Contains(domain.RequestSourceKinds, kind) {
			return in, authz.RequestInvalid("source_kind must be one of %s", requestNames(domain.RequestSourceKinds))
		}
		in.SourceKind = kind
	}

	if in.CreatedAfter, err = requestQueryTime(q, "created_after"); err != nil {
		return in, err
	}
	if in.CreatedBefore, err = requestQueryTime(q, "created_before"); err != nil {
		return in, err
	}
	if in.Attached, err = requestQueryBool(q, "attached"); err != nil {
		return in, err
	}
	in.Query = q.Get("q")

	if in.BeforeID, err = requestQueryCount(q, "before_id"); err != nil {
		return in, err
	}
	if in.Limit, err = requestQueryCount(q, "limit"); err != nil {
		return in, err
	}
	if in.Mine, err = requestQueryFlag(q, "mine"); err != nil {
		return in, err
	}
	if in.MineBeforeID, err = requestQueryCount(q, "mine_before_id"); err != nil {
		return in, err
	}
	if in.MineLimit, err = requestQueryCount(q, "mine_limit"); err != nil {
		return in, err
	}
	return in, nil
}

// parseRequestThreadQuery maps the discussion read's query string onto the
// service's input: the forward cursor and the page size, nothing else. A term
// the read does not have is a term that could not be aimed at somebody else's
// rows, which is why there is no filter here — a thread is one request's.
//
// A negative value is refused rather than read as "unset" (requestQueryCount's
// rule): quietly reinterpreting -5 as the default is not an answer a pager can
// act on, and the client would page forever.
func parseRequestThreadQuery(q url.Values) (service.RequestThreadInput, error) {
	var in service.RequestThreadInput
	if v := strings.TrimSpace(q.Get("after_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return in, authz.RequestInvalid("after_id must be a whole number")
		}
		if n < 0 {
			return in, authz.RequestInvalid("after_id must be zero or more")
		}
		in.AfterID = n
	}
	limit, err := requestQueryCount(q, "limit")
	if err != nil {
		return in, err
	}
	in.Limit = limit
	return in, nil
}

// parseRequestHistoryQuery maps the history read's query string onto the
// service's input: the forward cursor and the page size, nothing else.
//
// It has the thread decoder's two rules for the two same-named parameters, and
// deliberately no third parameter: a history is one request's, so a filter term
// could only ever be aimed at rows this read already scoped. A negative value is
// refused rather than read as "unset" (requestQueryCount's rule) — quietly
// reinterpreting -5 as the default is not an answer a pager can act on.
func parseRequestHistoryQuery(q url.Values) (service.RequestHistoryInput, error) {
	var in service.RequestHistoryInput
	if v := strings.TrimSpace(q.Get("after_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return in, authz.RequestInvalid("after_id must be a whole number")
		}
		if n < 0 {
			return in, authz.RequestInvalid("after_id must be zero or more")
		}
		in.AfterID = n
	}
	limit, err := requestQueryCount(q, "limit")
	if err != nil {
		return in, err
	}
	in.Limit = limit
	return in, nil
}

// parseRequestLinksQuery maps the links read's query string onto the service's
// input: the optional `kind` filter and nothing else.
//
// `kind` may be repeated (`?kind=planned_as&kind=delivered_by`) or comma-joined
// (`?kind=planned_as,delivered_by`), the shape `state` has on the queue and the
// one every filter on this surface uses, so a client does not learn a second
// spelling for the same idea. Every value is normalized through
// domain.NormalizeRequestLinkKind, so `PLANNED_AS` and ` planned_as ` mean the
// kind, and an unknown word is a coded 400 naming the closed set — never a filter
// silently dropped, which would answer "this request has nothing planned" for a
// request that does. That is the "filter given, filter ignored" bug
// architecture/request-queue-summaries.md records, and the reason this decoder
// refuses rather than tolerates.
//
// A duplicate kind is a caller repeating itself, not an error: the repository's
// `link_kind = ANY($3)` answers the same rows either way, and refusing would
// punish a client that merged two filters. The list is passed through as the
// caller wrote it, deduplicated in order, so the answer cannot change with the
// request's spelling of it.
//
// There is deliberately no cursor and no page size: see teamRequestLinks.
func parseRequestLinksQuery(q url.Values) (service.RequestLinksInput, error) {
	var in service.RequestLinksInput
	seen := map[domain.RequestLinkKind]bool{}
	for _, raw := range q["kind"] {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				return in, authz.RequestInvalid("kind must name a link relation; omit it to list every relation")
			}
			kind, err := domain.NormalizeRequestLinkKind(part)
			if err != nil {
				// The domain's message names the closed set and never echoes the
				// rejected value back (contract §5: a refusal must not repeat
				// content).
				return in, authz.RequestInvalid("%s", err)
			}
			if seen[kind] {
				continue
			}
			seen[kind] = true
			in.Kinds = append(in.Kinds, kind)
		}
	}
	return in, nil
}

// parseOwnRequestQuery maps the requester's own-list query string. It is
// deliberately smaller than the queue's: a requester has no tenant to name, no
// claim or assignment, no "attached" question and no search across other
// people's words — every term the service's OwnRequestListInput does not have is
// a term that could not be aimed at somebody else's rows.
func parseOwnRequestQuery(q url.Values) (service.OwnRequestListInput, error) {
	var in service.OwnRequestListInput
	states, err := requestQueryStates(q)
	if err != nil {
		return in, err
	}
	in.States = states
	if in.BeforeID, err = requestQueryCount(q, "before_id"); err != nil {
		return in, err
	}
	if in.Limit, err = requestQueryCount(q, "limit"); err != nil {
		return in, err
	}
	return in, nil
}

// requestQueryStates reads the `state` filter, which may be repeated
// (?state=new&state=triaging) or comma-separated (?state=new,triaging). Values
// are normalized through the domain's own rule — so " Triaging " is triaging —
// and an unknown or blank value is refused rather than matched against nothing:
// a typo that returned an empty queue would read as "the team has no requests",
// the one answer a queue must never invent.
func requestQueryStates(q url.Values) ([]domain.RequestStatus, error) {
	var out []domain.RequestStatus
	for _, raw := range q["state"] {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				return nil, authz.RequestInvalid("state must name a lifecycle state; omit it to list every state")
			}
			st, err := domain.NormalizeRequestStatus(part)
			if err != nil {
				// The domain's message names the closed set and never echoes the
				// rejected value back.
				return nil, authz.RequestInvalid("%s", err)
			}
			out = append(out, st)
		}
	}
	return out, nil
}

// requestQueryTime reads an RFC 3339 timestamp parameter (the ListAudit and
// project-report convention), nil when it is absent.
func requestQueryTime(q url.Values, name string) (*time.Time, error) {
	v := strings.TrimSpace(q.Get(name))
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, authz.RequestInvalid("%s must be an RFC 3339 timestamp", name)
	}
	return &t, nil
}

// requestQueryBool reads a true/false parameter, nil when it is absent.
func requestQueryBool(q url.Values, name string) (*bool, error) {
	v := strings.TrimSpace(q.Get(name))
	if v == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, authz.RequestInvalid("%s must be true or false", name)
	}
	return &b, nil
}

// requestQueryFlag reads a presence flag: any of true/1/t/yes means set, an
// absent value is false, and anything else is refused rather than read as false
// (a caller that sent mine=maybe should be told, not silently given a queue).
func requestQueryFlag(q url.Values, name string) (bool, error) {
	v := strings.TrimSpace(q.Get(name))
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, authz.RequestInvalid("%s must be true or false", name)
	}
	return b, nil
}

// requestQueryCount reads a whole non-negative number parameter (a cursor or a
// page size). Absent is 0, which every paging field reads as "unset". A negative
// value is refused: the repository would read it as unset too, and quietly
// reinterpreting -5 as the default is not an answer a pager can act on.
func requestQueryCount(q url.Values, name string) (int, error) {
	v := strings.TrimSpace(q.Get(name))
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, authz.RequestInvalid("%s must be a whole number", name)
	}
	if n < 0 {
		return 0, authz.RequestInvalid("%s must be zero or more", name)
	}
	return n, nil
}

// requestNames lists a typed vocabulary's values for a refusal message, the same
// wording the domain's own validators use.
func requestNames[T ~string](names []T) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return strings.Join(out, ", ")
}
